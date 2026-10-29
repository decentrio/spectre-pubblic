package services

import (
	"context"
	"errors"
	"log"
	"time"

	"relayer/chain/wasmclient"
	client "relayer/client"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// scanForCosmosL2Timeouts mirrors scanForCosmosTimeouts, but uses the L2 wasm
// client's attested block rather than an Ethereum beacon update. A Cosmos->L2
// timeout is proven against the L2 router storage root installed by that update,
// so the update is submitted first and the MsgTimeouts follow it in order.
// SendCosmosTxBatch may split the batch into several transactions; the update
// then lands first, the L2 client keeps the consensus state at the exact proof
// height for the later timeouts, and handleCosmosTimeoutBatchFailure retires the
// committed timeout prefix while deferring the rest.
func (s *Services) scanForCosmosL2Timeouts(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, recovery L2TimeoutRecovery) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR [CosmosTimeoutScan] L2 panic recovered: %v", r)
		}
	}()

	tracker := s.BatchBuilder.PendingTracker
	if allowed, err := tracker.TimeoutRetriesAllowed(); !allowed {
		if err != nil {
			log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] L2 timeout retry circuit breaker remains open: %v", err)
		}
		return
	}
	if err := tracker.PurgeStaleWithoutTimeout(pendingTrackerMaxAge); err != nil {
		log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist L2 pending-state cleanup: %v", err)
		return
	}
	pending := tracker.GetDue(time.Now())
	if len(pending) == 0 {
		return
	}
	if recovery.ClientID == "" || recovery.BuildUpdate == nil || recovery.BuildNonMembershipProof == nil {
		log.Printf("WARN [CosmosTimeoutScan] L2 timeout recovery is incomplete; deferring %d timeout(s) without charging", len(pending))
		deferTimeoutRetries(tracker, pending, time.Now(), "CosmosL2Timeout")
		return
	}

	// This first head query is only a cheap candidate filter. The attested update
	// below provides the actual proof timestamp and can be lower than the raw L2
	// head, so it is checked again before building timeout messages.
	headerCtx, cancelHeader := fetchCtx(stdCtx, defaultFetchTimeout)
	l2Head, err := evm.EthClient().HeaderByNumber(headerCtx, nil)
	cancelHeader()
	if err != nil {
		log.Printf("WARN [CosmosTimeoutScan] Failed to get L2 block header: %v", err)
		deferTimeoutRetries(tracker, pending, time.Now(), "CosmosL2Timeout")
		return
	}
	expired := pendingPacketsTimedOutAtTimestamp(pending, l2Head.Time)
	if len(expired) == 0 {
		return
	}

	unreceived := make([]pendingPacketInfo, 0, len(expired))
	for _, info := range expired {
		received, err := HasEthPacketReceipt(stdCtx, evm, info.Packet)
		if err != nil {
			log.Printf("WARN [CosmosTimeoutScan] src=%s seq=%d: failed to check L2 packet receipt: %v", info.Packet.SourceClient, info.Packet.Sequence, err)
			if !deferTimeoutRetries(tracker, []pendingPacketInfo{info}, time.Now(), "CosmosL2Timeout") {
				return
			}
			continue
		}
		if received {
			if err := tracker.RemoveIfCurrent(info); err != nil {
				log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist received L2 packet removal: %v", err)
				return
			}
			log.Printf("[CosmosTimeoutScan] src=%s seq=%d: L2 receipt already exists, removed from pending tracker", info.Packet.SourceClient, info.Packet.Sequence)
			continue
		}
		unreceived = append(unreceived, info)
	}
	expired = unreceived
	if len(expired) == 0 {
		return
	}

	update, err := recovery.BuildUpdate(stdCtx)
	if err != nil {
		log.Printf("WARN [CosmosTimeoutScan] Failed to build attested L2 client update: %v; deferring %d timeout(s) without charging", err, len(expired))
		deferTimeoutRetries(tracker, expired, time.Now(), "CosmosL2Timeout")
		return
	}
	if len(update.Payload) == 0 || update.Height == 0 || update.Timestamp == 0 {
		log.Printf("WARN [CosmosTimeoutScan] Invalid attested L2 client update (payload=%d height=%d timestamp=%d); deferring %d timeout(s) without charging", len(update.Payload), update.Height, update.Timestamp, len(expired))
		deferTimeoutRetries(tracker, expired, time.Now(), "CosmosL2Timeout")
		return
	}

	expired = pendingPacketsTimedOutAtTimestamp(expired, update.Timestamp)
	if len(expired) == 0 {
		if err := tracker.ClearDeferralsIfCurrentBatch(pending); err != nil {
			log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist L2 not-due state: %v", err)
		}
		return
	}

	proofHeight := clienttypes.Height{RevisionNumber: 0, RevisionHeight: update.Height}
	timeoutMsgs := make([]any, 0, len(expired))
	processed := make([]pendingPacketInfo, 0, len(expired))
	for _, info := range expired {
		proof, err := recovery.BuildNonMembershipProof(stdCtx, info.Packet, update.Height)
		if err != nil {
			if errors.Is(err, client.ErrPacketAlreadyReceived) {
				if err := tracker.RemoveIfCurrent(info); err != nil {
					log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist received L2 packet removal: %v", err)
					return
				}
				log.Printf("[CosmosTimeoutScan] src=%s seq=%d: receipt exists at attested L2 height, removed from pending tracker", info.Packet.SourceClient, info.Packet.Sequence)
				continue
			}
			log.Printf("WARN [CosmosTimeoutScan] src=%s seq=%d: build L2 non-membership proof: %v", info.Packet.SourceClient, info.Packet.Sequence, err)
			if !deferTimeoutRetries(tracker, []pendingPacketInfo{info}, time.Now(), "CosmosL2Timeout") {
				return
			}
			continue
		}
		timeoutMsgs = append(timeoutMsgs, channeltypesv2.NewMsgTimeout(info.Packet, proof, proofHeight, ""))
		processed = append(processed, info)
	}
	if len(timeoutMsgs) == 0 {
		return
	}
	if err := tracker.ClearDeferralsIfCurrentBatch(processed); err != nil {
		log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] failed to persist recovered L2 prerequisite state: %v", err)
		return
	}

	updateMsg, err := wasmclient.BuildUpdateClient("", recovery.ClientID, update.Payload)
	if err != nil {
		log.Printf("WARN [CosmosTimeoutScan] Failed to wrap attested L2 update: %v", err)
		deferTimeoutRetries(tracker, processed, time.Now(), "CosmosL2Timeout")
		return
	}
	updateMsgs := []any{updateMsg}
	batchMsgs := append(append(make([]any, 0, 1+len(timeoutMsgs)), updateMsgs...), timeoutMsgs...)
	if err := s.worker.TxHandler.SendCosmosTxBatch(stdCtx, cosmos, batchMsgs); err != nil {
		log.Printf("WARN [CosmosTimeoutScan] SendCosmosTxBatch failed for L2 timeout: %v", err)
		s.handleCosmosTimeoutBatchFailure(stdCtx, cosmos, tracker, updateMsgs, timeoutMsgs, processed, err)
		return
	}
	for _, info := range processed {
		if err := tracker.RemoveIfCurrent(info); err != nil {
			log.Printf("ERROR [CosmosTimeoutScan][ATTENTION] L2 timeout succeeded but completion state was not durable: %v", err)
			return
		}
		log.Printf("[CosmosTimeoutScan] src=%s seq=%d: timeout relay completed at L2 height %d", info.Packet.SourceClient, info.Packet.Sequence, update.Height)
	}
	s.ObserveEVMOnCosmosUpdate(time.Unix(int64(update.Timestamp), 0))
}
