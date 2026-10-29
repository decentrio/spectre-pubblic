package services

import (
	"context"
	"fmt"
	"log"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"

	client "relayer/client"
)

// L2TimeoutUpdate is the attestor-authenticated L2 state a Cosmos timeout
// needs. Payload advances the L2 wasm client on Cosmos; Height and Timestamp
// identify the exact L2 block from which the non-membership proof is taken.
//
// It deliberately contains only wire-neutral values. The attestor header
// builder lives in chain/l2rollup, which already depends on services, so putting
// its concrete type here would create an import cycle.
type L2TimeoutUpdate struct {
	Payload   []byte
	Height    uint64
	Timestamp uint64
}

// L2TimeoutRecovery supplies the L2-specific operations the shared Cosmos
// timeout scanner cannot own without depending on the attestor adapter. The
// callbacks must build their update and proof for the same L2 router and block.
type L2TimeoutRecovery struct {
	ClientID                string
	BuildUpdate             func(context.Context) (L2TimeoutUpdate, error)
	BuildNonMembershipProof func(context.Context, channeltypesv2.Packet, uint64) ([]byte, error)
}

// This file exposes the timeout-recovery surface so the chain-adapter relay path
// (package relay) can reuse the (subtle) scanners and pending-tracker bookkeeping
// instead of reimplementing them. These are thin wrappers over the unexported
// originals — no logic is duplicated.

// ScanCosmosTimeouts detects Cosmos-origin packets that expired on ETH without a
// receipt and relays their MsgTimeout back to Cosmos. Safe to call periodically;
// it purges/prunes the pending tracker and recovers from panics internally. stdCtx
// aborts the in-scan Cosmos catch-up wait promptly on shutdown.
func (s *Services) ScanCosmosTimeouts(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, ethClientID string) {
	s.scanForCosmosTimeouts(stdCtx, cosmos, evm, ethClientID)
}

// ScanCosmosL2Timeouts detects Cosmos-origin packets that expired on an
// attestor-backed L2 without a receipt. It advances the paired L2 wasm client
// and submits MsgTimeout in one Cosmos batch. It must be used for OP/Base and
// other L2 wasm clients; ScanCosmosTimeouts is the Ethereum beacon-client path.
func (s *Services) ScanCosmosL2Timeouts(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, recovery L2TimeoutRecovery) {
	s.scanForCosmosL2Timeouts(stdCtx, cosmos, evm, recovery)
}

// ScanEthTimeouts detects ETH-origin packets that expired on Cosmos without a
// receipt and relays their MsgTimeout back to ETH. Same periodic-call contract as
// ScanCosmosTimeouts.
func (s *Services) ScanEthTimeouts(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, routerClientID string) {
	s.scanForEthTimeouts(stdCtx, evmTimeoutDeps{cosmos: cosmos, evm: evm, routerClientID: routerClientID})
}

// ScanL2Timeouts detects L2-origin packets that expired on Cosmos without a
// receipt and relays their timeoutPacket back to the L2 ICS26Router. It uses the
// same Cosmos non-membership proof path as ScanEthTimeouts, but drains the L2
// pending tracker so it never interferes with the legacy ETH source tracker.
func (s *Services) ScanL2Timeouts(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, routerClientID string) {
	s.scanForL2Timeouts(stdCtx, evmTimeoutDeps{cosmos: cosmos, evm: evm, routerClientID: routerClientID})
}

// TrackCosmosPending records a Cosmos-origin packet just recv-relayed to ETH so
// ScanCosmosTimeouts can later refund it if it expires undelivered. False means
// the durable state write failed and the source event must be retried.
func (s *Services) TrackCosmosPending(packet channeltypesv2.Packet, blockNumber uint64) bool {
	return s.BatchBuilder.PendingTracker.Add(packet, blockNumber)
}

// CosmosPendingSettled reports whether a terminal Cosmos event has durably
// closed this exact packet. The relay module checks it around tracking so a
// terminal event cannot be undone by an in-flight batch adding the packet back.
func (s *Services) CosmosPendingSettled(packet channeltypesv2.Packet) bool {
	return s.BatchBuilder.PendingTracker.IsSettled(packet)
}

// MarkCosmosPendingSettled records a terminal Cosmos event and removes its
// matching pending entry atomically. False means persistence failed; callers
// should log the failure, but must not relay a packet already known settled.
func (s *Services) MarkCosmosPendingSettled(packet channeltypesv2.Packet) bool {
	if err := s.BatchBuilder.PendingTracker.MarkSettled(packet); err != nil {
		log.Printf("ERROR [PendingTracker][ATTENTION] failed to persist terminal Cosmos settlement seq=%d: %v", packet.Sequence, err)
		return false
	}
	return true
}

// UntrackCosmosPending removes a Cosmos-origin packet from the pending tracker
// once it has been successfully received on ETH — it can no longer time out, so
// the timeout scanner need not keep querying its receipt. If persistence fails,
// the packet remains tracked and the failure is logged for operator attention.
func (s *Services) UntrackCosmosPending(packet channeltypesv2.Packet) {
	if err := s.BatchBuilder.PendingTracker.RemovePacketIfCurrent(packet); err != nil {
		log.Printf("ERROR [PendingTracker][ATTENTION] failed to persist removal of settled Cosmos packet seq=%d: %v", packet.Sequence, err)
	}
}

// TrackL2Pending records an L2-origin packet observed on the L2 source path so
// ScanL2Timeouts can refund it if Cosmos never receives it before timeout. False
// means the durable state write failed and the source event must be retried.
func (s *Services) TrackL2Pending(packet channeltypesv2.Packet, blockNumber uint64) bool {
	return s.BatchBuilder.L2PendingTracker.Add(packet, blockNumber)
}

// UntrackL2Pending removes an L2-origin packet once the L2->Cosmos receive relay
// succeeds; after a Cosmos receipt exists, a timeout refund must not be attempted.
// If persistence fails, the packet remains tracked and the failure is logged for
// operator attention.
func (s *Services) UntrackL2Pending(packet channeltypesv2.Packet) {
	if err := s.BatchBuilder.L2PendingTracker.RemovePacketIfCurrent(packet); err != nil {
		log.Printf("ERROR [PendingTracker][ATTENTION] failed to persist removal of settled L2 packet seq=%d: %v", packet.Sequence, err)
	}
}

// Worker exposes the shared Worker (TxHandler + Prover) so the chain adapters —
// which wrap the existing Build/Send pipeline — can be constructed with the same
// worker the Services instance uses. Read-only handle; the adapters do not mutate
// it.
func (s *Services) Worker() *Worker { return s.worker }

// CosmosConfig exposes the resolved Cosmos-source config (ProofType, TrustLevel,
// BatchConfig, …) so the adapter wiring can build the groth16 client-update
// builder from the same resolved config the rest of the path uses.
func (s *Services) CosmosConfig() Config { return s.cosmosConfig }

// RotatePinnedSet force-rotates the on-chain SpectreClient's pinned
// validator set (RefreshCosmosClient with forceRotation=true), regardless of how
// much overlap remains. This is the guaranteed rotation, run on a fixed cadence
// by relay.WithPeriodicUpdate: the per-packet and expiry-driven
// refresh paths only rotate when overlap has already decayed to the threshold, so
// during a quiet period (no packet traffic) the pinned set could otherwise decay
// below the 2/3 quorum needed to update OR rotate the client — bricking it. This
// call keeps the pinned set fresh independent of packet flow. Side-effect-free on
// the adapter's cursors (the expiry is driven by ClientExpiresAt, not a seeded
// timestamp), so it only submits the rotation tx.
func (s *Services) RotatePinnedSet(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint, routerClientID string) error {
	lightBlock, err := s.worker.RefreshCosmosClient(stdCtx, cosmos, evm, routerClientID, s.cosmosConfig.FetchTimeout, s.cosmosConfig.RotationThreshold, s.cosmosConfig.ProofType, s.cosmosConfig.TrustLevel)
	if err == nil && lightBlock != nil {
		s.ObserveCosmosOnEVMUpdate(lightBlock.SignedHeader.Header.Time)
	}
	return err
}

// PinnedSetRotationInterval derives the force-rotation cadence from the on-chain
// trusting period exactly as the legacy routine did (min of the configured
// refresh interval and trustingPeriod minus a safety margin), so the pinned set
// is refreshed well within the window where it stays above quorum.
func (s *Services) PinnedSetRotationInterval(stdCtx context.Context, evm EVMEndpoint) (time.Duration, error) {
	readCtx, cancel := fetchCtx(stdCtx, s.cosmosConfig.FetchTimeout)
	defer cancel()
	clientState, err := fetchOnChainClientState(readCtx, evm)
	if err != nil {
		return 0, err
	}
	trustingPeriod := time.Duration(clientState.TrustingPeriod) * time.Second
	return deriveCosmosRefreshInterval(s.cosmosConfig, trustingPeriod)
}

// PinnedSetRotationDueIn reports how long until the next pinned-set rotation is
// due, based on the ON-CHAIN pinned snapshot timestamp (not process uptime, and
// not the latest height): it is the remaining time until snapshotTime +
// interval, clamped to >= 0. A client that is
// already at or past its rotation interval at startup returns 0 (rotate now),
// mirroring the legacy seedCosmosClientFreshness behavior — so a relayer restart
// near the rotation deadline does not wait a full fresh interval before the first
// rotation, which could let the pinned set decay below quorum.
func (s *Services) PinnedSetRotationDueIn(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint) (time.Duration, error) {
	interval, err := s.PinnedSetRotationInterval(stdCtx, evm)
	if err != nil {
		return 0, err
	}
	readCtx, cancel := fetchCtx(stdCtx, s.cosmosConfig.FetchTimeout)
	trustedHeight, err := FetchOnChainTrustedHeight(readCtx, evm)
	cancel()
	if err != nil {
		return 0, err
	}
	lightCtx, cancelLight := fetchCtx(stdCtx, s.cosmosConfig.FetchTimeout)
	defer cancelLight()
	lightBlock, err := client.GetLightBlock(lightCtx, cosmos.CosmosClient(), trustedHeight)
	if err != nil {
		return 0, err
	}
	trustedTime := lightBlock.SignedHeader.Header.Time
	if trustedTime.IsZero() {
		return 0, fmt.Errorf("zero trusted light block timestamp at height %d", trustedHeight)
	}
	// The update-age metric tracks the latest height: that is when the client
	// last moved, whatever the pinned set did.
	s.ObserveCosmosOnEVMUpdate(trustedTime)

	// The rotation deadline tracks the pinned snapshot: it is what the contract
	// expires, and app-state updates advance the latest height without moving
	// it. Anchoring on trustedTime let a restart after a few packet updates push
	// the first rotation past the snapshot's real expiry (#541).
	snapCtx, cancelSnap := fetchCtx(stdCtx, s.cosmosConfig.FetchTimeout)
	snapshotTime, supported, err := fetchPinnedSnapshotTime(snapCtx, evm)
	cancelSnap()
	if err != nil {
		return 0, err
	}
	anchor := trustedTime
	if supported {
		anchor = snapshotTime
	}
	return rotationDueIn(anchor, interval, time.Now()), nil
}

// rotationDueIn is the time left until anchor+interval, clamped to zero so an
// overdue rotation fires immediately.
func rotationDueIn(anchor time.Time, interval time.Duration, now time.Time) time.Duration {
	due := anchor.Add(interval).Sub(now)
	if due < 0 {
		return 0
	}
	return due
}

// SeedEVMOnCosmosUpdate records the timestamp already trusted by the on-chain
// beacon client so the first queue report is useful even before packet traffic
// or a refresh advances it in this process.
func (s *Services) SeedEVMOnCosmosUpdate(stdCtx context.Context, cosmos CosmosEndpoint, ethClientID string) error {
	readCtx, cancel := fetchCtx(stdCtx, s.cosmosConfig.FetchTimeout)
	defer cancel()
	state, err := client.GetEthereumClientState(readCtx, cosmos.CosmosClient(), ethClientID)
	if err != nil {
		return err
	}
	trustedTimestamp := state.ComputeTimestampAtSlot(state.LatestSlot)
	if trustedTimestamp == 0 {
		return fmt.Errorf("zero trusted EVM timestamp at slot %d", state.LatestSlot)
	}
	s.ObserveEVMOnCosmosUpdate(time.Unix(int64(trustedTimestamp), 0))
	return nil
}
