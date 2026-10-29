package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"relayer/chain/cosmos"
	"relayer/chain/evm"
	"relayer/relay"
	"relayer/services"
	"relayer/status"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// runAdapterEngine drives one source's bidirectional relay through the generic
// relay.Module. Proof generation, gap recovery and timeout scanning stay in
// services and are reached through the chain adapters; the module owns only the
// orchestration.
//
// Two modules run concurrently on a shared child context and the source's shared
// services.BatchBuilder (SubscribeCosmos/SubscribeEth push to disjoint queues, so
// the two drains never collide):
//
//   - Cosmos→ETH ("groth16"): Cosmos source → SpectreClient-on-ETH destination.
//     The subscriber does NOT track Cosmos-origin sends, so the module tracks them
//     (WithPacketTracker) to feed ScanCosmosTimeouts.
//   - ETH→Cosmos ("beacon"): ETH source → 08-wasm-on-Cosmos destination. The ETH
//     subscriber already records EthPendingTracker entries, so the module adds no
//     tracker (avoiding a redundant double-add); ScanEthTimeouts drains that
//     tracker. The enumeration backstop registers the sends it finds itself
//     (evm.Source.unrelayedSends), since those are the ones the subscriber missed.
//
// Engine names. One services instance per engine, and the name is what a
// reactivation addresses -- so they are declared together rather than written as
// literals at the two registration sites, where they could drift apart from each
// other and from what the CLI is told to accept.
const (
	engineCosmosEth = "cosmos<->eth"
	engineCosmosL2  = "cosmos->l2"
)

// The first module to exit cancels the other, and BOTH results are joined into
// the return value. Returning only the first is what hid the failure this engine
// now surfaces: on SIGTERM the unaffected direction usually returns nil first,
// and returning that discarded the other direction's drain failure. Each module
// normalizes cancellation of the run context to nil while preserving real RPC
// deadlines and drain failures, so a clean stop joins two nils and stays nil.
func runAdapterEngine(ctx context.Context, svc *services.Services, deps services.RelayDeps, reg *status.Registry) error {
	worker := svc.Worker()
	bb := svc.BatchBuilder
	cfg := svc.CosmosConfig()

	// Cosmos-origin sends aren't tracked by the subscriber; the module records
	// each one (proto bytes -> Packet) so ScanCosmosTimeouts can refund it, and
	// removes it once the send is received on ETH (it can no longer time out).
	trackCosmosPending := func(raw []byte, height uint64) bool {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [cosmos->eth Relay] track pending: decode packet: %v", err)
			return false
		}
		return svc.TrackCosmosPending(pkt, height)
	}
	untrackCosmosPending := func(raw []byte) {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [cosmos->eth Relay] untrack pending: decode packet: %v", err)
			return
		}
		svc.UntrackCosmosPending(pkt)
	}
	isCosmosPendingSettled, markCosmosPendingSettled := cosmosPendingSettlementGuard(svc, "cosmos->eth")

	// Pinned-set rotation cadence, derived from the on-chain trusting period. A
	// derivation failure is FATAL: an unsafe or underivable interval must be
	// rejected at startup rather than silently replaced by a fixed default that
	// could exceed the trusting period and let the client expire. Fail loud so the
	// operator fixes the config.
	periodicUpdateInterval, err := svc.PinnedSetRotationInterval(ctx, deps.EVM)
	if err != nil {
		if isShutdownErr(err) && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("cosmos->eth: derive pinned-set rotation interval: %w", err)
	}
	// Initial delay from the client's ON-CHAIN freshness (time until next due), so
	// a restart near the rotation deadline fires promptly instead of waiting a full
	// fresh interval. On error, default to 0 (rotate now) — the safe direction:
	// never let the set decay by delaying the first rotation.
	initialRotationDelay, err := svc.PinnedSetRotationDueIn(ctx, deps.Cosmos, deps.EVM)
	if err != nil {
		if isShutdownErr(err) && ctx.Err() != nil {
			return nil
		}
		log.Printf("WARN [cosmos->eth UpdateClient] derive initial rotation delay: %v; rotating on startup", err)
		initialRotationDelay = 0
	}
	if err := svc.SeedEVMOnCosmosUpdate(ctx, deps.Cosmos, deps.IDs.EVMOnCosmos); err != nil {
		log.Printf("WARN [eth->cosmos UpdateClient] seed client-update age: %v", err)
	}

	// A delivered receive makes its acknowledgement owed; a relayed ack closes the
	// debt. Both take the marshaled packet, like the tracker hooks beside them.
	//
	// They are wired to BOTH modules because the two halves land on opposite
	// ones. A Cosmos-origin send is delivered by cosmos->eth (the debt), and the
	// acknowledgement the destination writes for it comes back as an ETH
	// WriteAcknowledgement relayed by eth->cosmos (the settlement) -- and the
	// mirror image for an ETH-origin send. Attaching the pair to one module only
	// meant every module recorded debts that the OTHER module settled, so no debt
	// ever cleared and every returned acknowledgement stayed overdue forever.
	// The tracker behind these hooks is shared (svc.BatchBuilder.AckDueTracker),
	// which is what lets the settlement land from the other side.
	ackDue, ackSettled := ackWatchHooks(svc)

	cosmosToEth := relay.NewModule(
		"cosmos->eth",
		deps.IDs.CosmosOnEVM,
		cosmos.NewSource(deps.Cosmos, deps.EVM, deps.IDs, deps.Config.FetchTimeout, deps.Config.BatchConfig, deps.Logger, bb, svc.RecoveryState()),
		evm.NewDestination(worker, deps.Cosmos, deps.EVM, deps.IDs.CosmosOnEVM),
		cosmos.NewGroth16Builder(worker, deps.Cosmos, deps.EVM, deps.Config.FetchTimeout, deps.Config.RotationThreshold, cfg.ProofType, cfg.TrustLevel),
		relay.WithClientUpdateObserver(svc.ObserveCosmosOnEVMUpdate),
		relay.WithTimeoutScanner(0, func(c context.Context) {
			svc.ScanCosmosTimeouts(c, deps.Cosmos, deps.EVM, deps.IDs.EVMOnCosmos)
		}),
		relay.WithPacketTracker(trackCosmosPending, untrackCosmosPending),
		relay.WithPendingSettlementGuard(isCosmosPendingSettled, markCosmosPendingSettled),
		// Name the acknowledgements that never come back. The wait tracker only
		// follows packets it OBSERVED, so an ack written while this process was
		// down leaves the packet silently unfinished and its escrow locked.
		relay.WithAckWatch(0, 0, ackDue, ackSettled, svc.OverdueAcks),
		// The enumeration backstop. The block scan only ever sees its own window,
		// so a lost cursor, an outage longer than the window, or a second relayer
		// joining this path all leave older packets invisible to it. The Cosmos
		// source lists its outstanding sends and the acknowledgements it wrote
		// for ETH sends; the ETH source below does the mirror.
		relay.WithPacketFlush(0),
		// Force-rotate the pinned validator set on a fixed cadence so it never
		// decays below quorum during a quiet period. ETH->Cosmos needs no
		// equivalent — the beacon client has no pinned set.
		relay.WithPeriodicUpdate(periodicUpdateInterval, initialRotationDelay, func(c context.Context) error {
			return svc.RotatePinnedSet(c, deps.Cosmos, deps.EVM, deps.IDs.CosmosOnEVM)
		}),
	)

	ethToCosmos := relay.NewModule(
		"eth->cosmos",
		deps.IDs.EVMOnCosmos,
		evm.NewSource(deps.Cosmos, deps.EVM, deps.IDs, deps.Config.BatchConfig, deps.Logger, bb, svc.RecoveryState()).
			WithRouterDeployBlock(deps.Config.RouterDeployBlock),
		cosmos.NewDestination(worker, deps.Cosmos, deps.IDs.EVMOnCosmos),
		evm.NewBeaconBuilder(worker, deps.Cosmos, deps.EVM, deps.IDs.EVMOnCosmos),
		relay.WithClientUpdateObserver(svc.ObserveEVMOnCosmosUpdate),
		relay.WithTimeoutScanner(0, func(c context.Context) { svc.ScanEthTimeouts(c, deps.Cosmos, deps.EVM, deps.IDs.CosmosOnEVM) }),
		// The same pair of hooks, and deliberately NO overdue reporter: the
		// tracker is shared, so a second watch loop would report the same set
		// twice. nil leaves the loop unstarted (see Module.Run) while the debt
		// recording and settlement still happen on this side.
		relay.WithAckWatch(0, 0, ackDue, ackSettled, nil),
		// The enumeration backstop for this direction: ETH sends the router still
		// holds a commitment for, and acknowledgements it wrote for Cosmos sends
		// that Cosmos still holds a commitment for.
		relay.WithPacketFlush(0),
	)

	// Both modules, one services instance: the queues are shared, so registering
	// them per module would report the same set twice.
	//
	// engineCosmosEth names the pair, not either direction, because that shared
	// instance is what a reactivation has to address. The name is stamped onto
	// every queue this engine reports, so the string an operator reads in
	// `relayer status` is the string `status reactivate --path` accepts.
	reg.AddPath(cosmosToEth.Status)
	reg.AddPath(ethToCosmos.Status)
	reg.AddQueues(engineCosmosEth, svc.QueueStatus)
	reg.AddReactivator(engineCosmosEth, svc.ReactivateDeadLetter)

	// Child context so the first fatal error stops both modules.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go svc.RunQueueReporter(runCtx)
	errCh := make(chan error, 2)
	go func() { errCh <- cosmosToEth.Run(runCtx) }()
	go func() { errCh <- ethToCosmos.Run(runCtx) }()

	first := <-errCh // first module to exit
	cancel()         // stop the other
	second := <-errCh

	// BOTH results, not just the first -- see the contract on this function.
	// Reading the second only to avoid a goroutine leak is what discarded the
	// other direction's drain failure before it could be recorded.
	return errors.Join(first, second)
}

// cosmosPendingSettlementGuard adapts the durable Cosmos terminal-tombstone
// tracker to the generic relay module. Both Cosmos->EVM engines use it because
// they share SubscribeCosmos and its handoff race.
func cosmosPendingSettlementGuard(svc *services.Services, label string) (relay.PendingSettledFunc, relay.MarkPendingSettledFunc) {
	decode := func(raw []byte) (channeltypesv2.Packet, bool) {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [%s Relay] pending-settlement guard: decode packet: %v", label, err)
			return channeltypesv2.Packet{}, false
		}
		return pkt, true
	}
	return func(raw []byte) bool {
			pkt, ok := decode(raw)
			return ok && svc.CosmosPendingSettled(pkt)
		}, func(raw []byte) bool {
			pkt, ok := decode(raw)
			return ok && svc.MarkCosmosPendingSettled(pkt)
		}
}

// ackWatchHooks builds the record/settle pair for one ledger.
//
// It is a function rather than two closures written at each call site because
// the pair only works when BOTH sides of a relay path close over the SAME
// Services: one direction records the debt a delivered receive owes, the other
// settles it when the acknowledgement comes back. Written out per site, the
// easiest mistake is to hand one direction a different instance, and the symptom
// is silence -- every debt recorded, none ever cleared, every returned ack
// reported overdue forever.
func ackWatchHooks(svc *services.Services) (func([]byte) bool, func([]byte)) {
	due := func(raw []byte) bool {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [AckWatch] record owed ack: decode packet: %v", err)
			return false
		}
		return svc.RecordAckDue(pkt)
	}
	settled := func(raw []byte) {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [AckWatch] settle owed ack: decode packet: %v", err)
			return
		}
		svc.ClearAckDue(pkt)
	}
	return due, settled
}
