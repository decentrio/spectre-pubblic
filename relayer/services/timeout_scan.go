package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/chain/ethnative"
	client "relayer/client"
	"time"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// The timeout scanners: one per source chain, each asking whether a tracked
// send has expired on its destination and, if so, submitting the timeout on the
// source. Split from services.go (E6): construction and client expiry stay
// there, scanning lives here, the retry ledger the scanners charge is
// timeout_retry.go.

func pendingPacketsTimedOutAtTimestamp(pending []pendingPacketInfo, timestamp uint64) []pendingPacketInfo {
	if timestamp == 0 {
		return nil
	}

	expired := make([]pendingPacketInfo, 0, len(pending))
	for _, info := range pending {
		if info.Packet.TimeoutTimestamp > 0 && timestamp >= info.Packet.TimeoutTimestamp {
			expired = append(expired, info)
		}
	}
	return expired
}

// ShouldRelayCosmosTimeoutToEth decides whether a CosmosTimeout event needs a
// follow-up ICS26Router.timeoutPacket on the ETH side.
//
// IBC v2 timeout is submitted to the *source* chain: it deletes the source-side
// packet commitment and refunds the sender. For a Cosmos→ETH packet, that
// MsgTimeout fires on Cosmos and the destination (ETH) never held a commitment
// for the packet — calling ETH's timeoutPacket would be a NoOp at best and a
// wasted Groth16 proof + tx in any case.
//
// The Cosmos-originated case is identified by `destination_client` matching the
// router client ID we know ETH uses for this Cosmos chain (`ICS26ClientID` in
// the config). When the destination is *not* our Cosmos chain's ETH-side ID,
// the packet came from ETH and ETH still owns a commitment that needs the
// timeoutPacket call to delete + refund.
//
// NOTE: this predicate previously compared `source_client` against the same
// router ID, which never matched for Cosmos-originated packets (their
// source_client is the Cosmos-side client name e.g. "08-wasm-0"). That fired
// the no-op ETH relay for every Cosmos→ETH timeout — wasted gas at best, and
// blocked the relay loop entirely whenever the ETH-side updateClient was
// failing for unrelated reasons.
func ShouldRelayCosmosTimeoutToEth(packet *channeltypesv2.Packet, cosmosRouterClientID string) bool {
	if packet == nil || cosmosRouterClientID == "" {
		return false
	}
	return packet.DestinationClient != cosmosRouterClientID
}

// buildCosmosTimeoutMsg takes stdCtx so the eth_getProof it issues is bound to
// the scan's lifetime. Without it the scanner could block forever on an
// unresponsive node, and because relay.Module runs the scan synchronously that
// stops timeout recovery entirely rather than just delaying one packet.
func (s *Services) buildCosmosTimeoutMsg(stdCtx context.Context, evm EVMEndpoint, packet channeltypesv2.Packet, ethClientState *client.EthereumClientState) (*channeltypesv2.MsgTimeout, error) {
	receiptPath := EthPath(packet.DestinationClient, packet.Sequence, 2)
	proofCtx, cancel := fetchCtx(stdCtx, defaultFetchTimeout)
	defer cancel()
	proofJSON, err := client.GetEthNonMembershipProof(
		proofCtx, evm.EthClient(), evm.Contracts.Router, receiptPath, ethcommon.HexToHash(ICS26_IBC_STORAGE_SLOT), new(big.Int).SetUint64(ethClientState.LatestExecutionBlockNumber))
	if err != nil {
		return nil, fmt.Errorf("failed to get ETH non-membership proof: %w", err)
	}
	// The light client verifies the protobuf proof, not the relayer's JSON one.
	proofBytes, err := ethnative.Proof(proofJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ETH non-membership proof: %w", err)
	}

	return channeltypesv2.NewMsgTimeout(
		packet,
		proofBytes,
		clienttypes.Height{RevisionNumber: 0, RevisionHeight: ethClientState.LatestSlot},
		"",
	), nil
}

// updateMsgsFailed reports whether a failed [update..., timeout...] batch
// stopped before any timeout message could execute. Such a failure belongs to
// the shared client-update prefix and must defer every packet without charging
// its packet-attributable retry budget.
func updateMsgsFailed(err error, updateMsgCount int) bool {
	if updateMsgCount == 0 {
		return false
	}
	var partialErr *BatchPartialError
	if errors.As(err, &partialErr) {
		return partialErr.SucceededCount < updateMsgCount
	}
	return true
}

// handleCosmosTimeoutBatchFailure preserves any successful prefix, never charges
// packets behind an update-prefix failure, and isolates permanent timeout
// failures into singleton submissions so only the actual poison packet is charged.
func (s *Services) handleCosmosTimeoutBatchFailure(stdCtx context.Context, cosmos CosmosEndpoint, tracker *PendingPacketTracker, updateMsgs, timeoutMsgs []any, processed []pendingPacketInfo, sendErr error) {
	remainingInfos := processed
	remainingMsgs := timeoutMsgs
	if updateMsgsFailed(sendErr, len(updateMsgs)) {
		deferTimeoutRetries(tracker, remainingInfos, time.Now(), "CosmosTimeout")
		return
	}
	var partialErr *BatchPartialError
	if errors.As(sendErr, &partialErr) {
		succeeded := partialErr.SucceededCount - len(updateMsgs)
		if succeeded > len(remainingInfos) {
			succeeded = len(remainingInfos)
		}
		for _, info := range remainingInfos[:succeeded] {
			if err := tracker.RemoveIfCurrent(info); err != nil {
				log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] successful timeout completion was not durable: %v", err)
				return
			}
		}
		remainingInfos = remainingInfos[succeeded:]
		remainingMsgs = remainingMsgs[succeeded:]
	}
	if len(remainingInfos) == 0 {
		return
	}
	if !errors.Is(sendErr, ErrPermanentRelayFailure) {
		deferTimeoutRetries(tracker, remainingInfos, time.Now(), "CosmosTimeout")
		return
	}
	for i, info := range remainingInfos {
		err := s.worker.TxHandler.SendCosmosTxBatch(stdCtx, cosmos, []any{remainingMsgs[i]})
		if err == nil {
			if err := tracker.RemoveIfCurrent(info); err != nil {
				log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] successful timeout completion was not durable: %v", err)
				return
			}
			continue
		}
		if errors.Is(err, ErrPermanentRelayFailure) {
			if !chargeTimeoutFailure(tracker, info, time.Now(), "Cosmos") {
				return
			}
		} else {
			if !deferTimeoutRetries(tracker, []pendingPacketInfo{info}, time.Now(), "CosmosTimeout") {
				return
			}
		}
	}
}

// resolveCosmosTimeoutProofState keeps the shared-prerequisite deferral beside
// both failure exits. Every packet in the scan depends on the same update and
// client state, so neither failure is attributable to an individual packet;
// all expired packets back off without consuming their permanent-failure budget.
func (s *Services) resolveCosmosTimeoutProofState(
	tracker *PendingPacketTracker,
	expired []pendingPacketInfo,
	buildUpdate func() (*EthClientUpdateResult, error),
	fetchClientState func() (*client.EthereumClientState, error),
) (*EthClientUpdateResult, *client.EthereumClientState, bool) {
	updateResult, err := buildUpdate()
	if err != nil {
		log.Printf("WARN [CosmosTimeoutScan] Failed to build ETH client update headers: %v; deferring %d timeout(s) without charging", err, len(expired))
		deferTimeoutRetries(tracker, expired, time.Now(), "CosmosTimeout")
		return nil, nil, false
	}

	ethClientState := updateResult.EthClientState
	if ethClientState == nil {
		ethClientState, err = fetchClientState()
		if err != nil {
			log.Printf("WARN [CosmosTimeoutScan] Failed to get ETH client state: %v; deferring %d timeout(s) without charging", err, len(expired))
			deferTimeoutRetries(tracker, expired, time.Now(), "CosmosTimeout")
			return nil, nil, false
		}
	}
	return updateResult, ethClientState, true
}

func (s *Services) scanForCosmosTimeouts(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, ethClientID string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR [CosmosTimeoutScan] Panic recovered: %v", r)
		}
	}()

	tracker := s.BatchBuilder.PendingTracker
	if allowed, err := tracker.TimeoutRetriesAllowed(); !allowed {
		if err != nil {
			log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] timeout retry circuit breaker remains open: %v", err)
		}
		return
	}
	if err := tracker.PurgeStaleWithoutTimeout(pendingTrackerMaxAge); err != nil {
		log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist pending-state cleanup: %v", err)
		return
	}
	now := time.Now()
	pending := tracker.GetDue(now)
	if len(pending) == 0 {
		return
	}

	headerCtx, cancelHeader := fetchCtx(stdCtx, defaultFetchTimeout)
	ethHeader, err := evm.EthClient().HeaderByNumber(headerCtx, nil)
	cancelHeader()
	if err != nil {
		log.Printf("WARN [CosmosTimeoutScan] Failed to get ETH block header: %v", err)
		deferTimeoutRetries(tracker, pending, time.Now(), "CosmosTimeout")
		return
	}
	expired := pendingPacketsTimedOutAtTimestamp(pending, ethHeader.Time)
	if len(expired) == 0 {
		return
	}

	unreceived := make([]pendingPacketInfo, 0, len(expired))
	for _, info := range expired {
		received, err := HasEthPacketReceipt(stdCtx, evm, info.Packet)
		if err != nil {
			log.Printf("WARN [CosmosTimeoutScan] src=%s seq=%d: failed to check ETH packet receipt: %v", info.Packet.SourceClient, info.Packet.Sequence, err)
			if !deferTimeoutRetries(tracker, []pendingPacketInfo{info}, time.Now(), "CosmosTimeout") {
				return
			}
			continue
		}
		if received {
			if err := tracker.RemoveIfCurrent(info); err != nil {
				log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist received packet removal: %v", err)
				return
			}
			log.Printf("[CosmosTimeoutScan] src=%s seq=%d: ETH receipt already exists, removed from pending tracker", info.Packet.SourceClient, info.Packet.Sequence)
			continue
		}
		unreceived = append(unreceived, info)
	}
	expired = unreceived
	if len(expired) == 0 {
		return
	}

	updateResult, ethClientState, ok := s.resolveCosmosTimeoutProofState(
		tracker,
		expired,
		func() (*EthClientUpdateResult, error) {
			return s.worker.BuildEthClientUpdateHeaders(stdCtx, cosmos, evm, ethClientID)
		},
		func() (*client.EthereumClientState, error) {
			readCtx, cancel := fetchCtx(stdCtx, defaultFetchTimeout)
			defer cancel()
			return client.GetEthereumClientState(readCtx, cosmos.CosmosClient(), ethClientID)
		},
	)
	if !ok {
		return
	}
	if len(updateResult.Headers) == 0 && updateResult.ProofTimestamp > 0 {
		s.ObserveEVMOnCosmosUpdate(time.Unix(int64(updateResult.ProofTimestamp), 0))
	}

	expired = pendingPacketsTimedOutAtTimestamp(expired, updateResult.ProofTimestamp)
	if len(expired) == 0 {
		// The shared proof/update path succeeded; a packet merely is not due at
		// this proof timestamp, so stale outage deferrals must not keep it stuck.
		if err := tracker.ClearDeferralsIfCurrentBatch(pending); err != nil {
			log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist not-due state: %v", err)
		}
		return
	}

	timeoutMsgs := make([]any, 0, len(expired))
	processed := make([]pendingPacketInfo, 0, len(expired))
	for _, info := range expired {
		msgTimeout, err := s.buildCosmosTimeoutMsg(stdCtx, evm, info.Packet, ethClientState)
		if err != nil {
			if errors.Is(err, client.ErrPacketAlreadyReceived) {
				if err := tracker.RemoveIfCurrent(info); err != nil {
					log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist received packet removal: %v", err)
					return
				}
				log.Printf("[CosmosTimeoutScan] src=%s seq=%d: receipt exists at proof height, removed from pending tracker", info.Packet.SourceClient, info.Packet.Sequence)
				continue
			}
			log.Printf("WARN [CosmosTimeoutScan] src=%s seq=%d: %v", info.Packet.SourceClient, info.Packet.Sequence, err)
			if !deferTimeoutRetries(tracker, []pendingPacketInfo{info}, time.Now(), "CosmosTimeout") {
				return
			}
			continue
		}
		timeoutMsgs = append(timeoutMsgs, msgTimeout)
		processed = append(processed, info)
	}
	if len(timeoutMsgs) == 0 {
		return
	}
	if err := tracker.ClearDeferralsIfCurrentBatch(processed); err != nil {
		log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist recovered prerequisite state: %v", err)
		return
	}

	updateMsgs := make([]any, 0, len(updateResult.Headers))
	for i, header := range updateResult.Headers {
		msg, err := ethnative.BuildUpdateClient("", ethClientID, header)
		if err != nil {
			log.Printf("WARN [CosmosTimeoutScan] Failed to wrap ETH update header %d: %v", i, err)
			deferTimeoutRetries(tracker, processed, time.Now(), "CosmosTimeout")
			return
		}
		updateMsgs = append(updateMsgs, msg)
	}
	if len(updateMsgs) > 0 {
		if err := s.worker.WaitForCosmosCatchUp(stdCtx, cosmos, ethClientState, updateResult.SigSlot); err != nil {
			log.Printf("WARN [CosmosTimeoutScan] target chain did not catch up: %v", err)
			deferTimeoutRetries(tracker, processed, time.Now(), "CosmosTimeout")
			return
		}
	}

	batchMsgs := append(append(make([]any, 0, len(updateMsgs)+len(timeoutMsgs)), updateMsgs...), timeoutMsgs...)
	if err := s.worker.TxHandler.SendCosmosTxBatch(stdCtx, cosmos, batchMsgs); err != nil {
		log.Printf("WARN [CosmosTimeoutScan] SendCosmosTxBatch failed: %v", err)
		s.handleCosmosTimeoutBatchFailure(stdCtx, cosmos, tracker, updateMsgs, timeoutMsgs, processed, err)
		return
	}
	for _, info := range processed {
		if err := tracker.RemoveIfCurrent(info); err != nil {
			log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] timeout succeeded but completion state was not durable: %v", err)
			return
		}
		log.Printf("[CosmosTimeoutScan] src=%s seq=%d: timeout relay completed", info.Packet.SourceClient, info.Packet.Sequence)
	}
	if updateResult.ProofTimestamp > 0 {
		s.ObserveEVMOnCosmosUpdate(time.Unix(int64(updateResult.ProofTimestamp), 0))
	}
}

func (s *Services) scanForEVMTimeouts(stdCtx context.Context, deps evmTimeoutDeps, opts evmTimeoutScanOptions) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR [%sTimeoutScan] Panic recovered: %v", opts.tag, r)
		}
	}()
	if opts.tracker == nil {
		log.Printf("ERROR [%sTimeoutScan] pending tracker is nil", opts.tag)
		return
	}
	if allowed, err := opts.tracker.TimeoutRetriesAllowed(); !allowed {
		if err != nil {
			log.Printf("ERROR [%sTimeoutScan][ATTENTION] timeout retry circuit breaker remains open: %v", opts.tag, err)
		}
		return
	}
	if err := opts.tracker.PurgeStaleWithoutTimeout(pendingTrackerMaxAge); err != nil {
		log.Printf("ERROR [%sTimeoutScan][ATTENTION] failed to persist pending-state cleanup: %v", opts.tag, err)
		return
	}
	now := time.Now()
	pending := opts.tracker.GetDue(now)
	if len(pending) == 0 {
		return
	}
	expired := pendingPacketsTimedOutAtTimestamp(pending, uint64(now.Unix()))
	if len(expired) == 0 {
		return
	}
	log.Printf("[%sTimeoutScan] Found %d locally-expired EVM-origin packet(s)", opts.tag, len(expired))
	candidates := make([]pendingPacketInfo, 0, len(expired))
	for _, info := range expired {
		pendingCommitment, err := opts.hasPendingCommitment(stdCtx, deps, info.Packet)
		if err != nil {
			log.Printf("WARN [%sTimeoutScan] src=%s seq=%d: failed to check EVM packet commitment: %v", opts.tag, info.Packet.SourceClient, info.Packet.Sequence, err)
			if !applyTimeoutOutcome(opts.tracker, info, timeoutDeferred, time.Now(), opts.tag+"Timeout") {
				return
			}
			continue
		}
		if !pendingCommitment {
			if err := opts.tracker.RemoveIfCurrent(info); err != nil {
				log.Printf("ERROR [%sTimeoutScan][ATTENTION] failed to persist cleared commitment: %v", opts.tag, err)
				return
			}
			log.Printf("[%sTimeoutScan] src=%s seq=%d: EVM commitment already cleared, removed from pending tracker", opts.tag, info.Packet.SourceClient, info.Packet.Sequence)
			continue
		}
		candidates = append(candidates, info)
	}
	if len(candidates) == 0 {
		return
	}

	// The Cosmos client update is a shared prerequisite for every timeout proof
	// in this scan. Resolve it once: a backlog of N expired packets must not
	// trigger N expensive update/proof attempts when that prerequisite is down.
	latestLightBlock, ok := opts.prepareTimeouts(stdCtx, deps)
	if !ok {
		deferTimeoutRetries(opts.tracker, candidates, time.Now(), opts.tag+"Timeout")
		return
	}
	if err := opts.tracker.ClearDeferralsIfCurrentBatch(candidates); err != nil {
		log.Printf("ERROR [%sTimeoutScan][ATTENTION] failed to persist recovered prerequisite state: %v", opts.tag, err)
		return
	}
	for _, info := range candidates {
		// The shared client update just succeeded. Any prior shared-outage
		// deferrals no longer describe this packet, so let a subsequent packet-
		// specific not-due or transient result start at the one-minute backoff.
		packet := info.Packet
		outcome := opts.timeoutSend(stdCtx, deps, EthPacket{Type: EthSend, Packet: &packet, BlockNumber: info.BlockNumber}, latestLightBlock)
		if !applyTimeoutOutcome(opts.tracker, info, outcome, time.Now(), opts.tag+"Timeout") {
			return
		}
	}
}

func (s *Services) scanForL2Timeouts(stdCtx context.Context, deps evmTimeoutDeps) {
	s.scanForEVMTimeouts(stdCtx, deps, evmTimeoutScanOptions{
		tag:     "L2",
		tracker: s.BatchBuilder.L2PendingTracker,
		prepareTimeouts: func(c context.Context, scanCtx evmTimeoutDeps) (*client.LightBlock, bool) {
			return s.updateCosmosClientForEth(c, scanCtx, "L2Timeout")
		},
		hasPendingCommitment: func(c context.Context, scanCtx evmTimeoutDeps, packet channeltypesv2.Packet) (bool, error) {
			return HasPendingEthPacketCommitment(c, scanCtx.evm, packet)
		},
		timeoutSend: func(c context.Context, scanCtx evmTimeoutDeps, packet EthPacket, lightBlock *client.LightBlock) timeoutSendOutcome {
			return s.timeoutEVMSend(c, scanCtx, packet, "L2", lightBlock)
		},
	})
}

func (s *Services) scanForEthTimeouts(stdCtx context.Context, deps evmTimeoutDeps) {
	s.scanForEVMTimeouts(stdCtx, deps, evmTimeoutScanOptions{
		tag:     "Eth",
		tracker: s.BatchBuilder.EthPendingTracker,
		prepareTimeouts: func(c context.Context, scanCtx evmTimeoutDeps) (*client.LightBlock, bool) {
			return s.updateCosmosClientForEth(c, scanCtx, "EthTimeout")
		},
		hasPendingCommitment: func(c context.Context, scanCtx evmTimeoutDeps, packet channeltypesv2.Packet) (bool, error) {
			return HasPendingEthPacketCommitment(c, scanCtx.evm, packet)
		},
		timeoutSend: func(c context.Context, scanCtx evmTimeoutDeps, packet EthPacket, lightBlock *client.LightBlock) timeoutSendOutcome {
			return s.timeoutEVMSend(c, scanCtx, packet, "Eth", lightBlock)
		},
	})
}

func (s *Services) timeoutEVMSend(stdCtx context.Context, deps evmTimeoutDeps, packet EthPacket, tag string, latestLightBlock *client.LightBlock) timeoutSendOutcome {
	log.Printf("[%sTimeoutScan] src=%s seq=%d: packet expired, preparing timeout proof", tag, packet.Packet.SourceClient, packet.Packet.Sequence)
	counterpartyTime := uint64(latestLightBlock.SignedHeader.Header.Time.Unix())
	if counterpartyTime < packet.Packet.TimeoutTimestamp {
		log.Printf("[%sTimeoutScan] src=%s seq=%d: counterparty time %d < timeout %d, skipping", tag, packet.Packet.SourceClient, packet.Packet.Sequence, counterpartyTime, packet.Packet.TimeoutTimestamp)
		return timeoutNotDue
	}

	calldata, err := CosmosNonMembership(stdCtx, deps.cosmos, *packet.Packet, packet.Packet.DestinationClient, []byte{2}, latestLightBlock)
	if err != nil {
		log.Printf("WARN [%sTimeoutScan] src=%s seq=%d: %v", tag, packet.Packet.SourceClient, packet.Packet.Sequence, err)
		return timeoutOutcomeForError(err)
	}
	msgTimeoutPacket := contractICS26Router.IICS26RouterMsgsMsgTimeoutPacket{
		Packet:           ToEthPacket(*packet.Packet),
		NonMembershipMsg: calldata,
	}
	if err := s.worker.TxHandler.SendEthTx(stdCtx, deps.evm, deps.routerClientID, msgTimeoutPacket); err != nil {
		log.Printf("WARN [%sTimeoutScan] src=%s seq=%d: SendEthTx failed: %v", tag, packet.Packet.SourceClient, packet.Packet.Sequence, err)
		return timeoutOutcomeForError(err)
	}
	log.Printf("[%sTimeoutScan] src=%s seq=%d: relay completed", tag, packet.Packet.SourceClient, packet.Packet.Sequence)
	return timeoutSent
}

func (s *Services) updateCosmosClientForEth(stdCtx context.Context, deps evmTimeoutDeps, tag string) (*client.LightBlock, bool) {
	latestLightBlock, err := s.worker.UpdateCosmosClient(stdCtx, deps.cosmos, deps.evm, deps.routerClientID, s.cosmosConfig.FetchTimeout, s.cosmosConfig.RotationThreshold, s.cosmosConfig.ProofType, 0, s.cosmosConfig.TrustLevel, false, 0)
	if err != nil {
		log.Printf("WARN [%sTimeoutScan] Failed to update cosmos light client: %v", tag, err)
		return nil, false
	}
	if latestLightBlock == nil {
		log.Printf("WARN [%sTimeoutScan] Failed to update cosmos light client: latestLightBlock is nil", tag)
		return nil, false
	}
	s.ObserveCosmosOnEVMUpdate(latestLightBlock.SignedHeader.Header.Time)

	return latestLightBlock, true
}
