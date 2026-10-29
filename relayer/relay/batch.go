// This file holds one batch's journey: partition by relayability, prepare the
// client update, prove each packet, submit, and isolate a poison packet on
// failure. It is separated from module.go because handleBatch was 256 lines --
// five times the repo's 50-line limit -- inside a file whose other concern is
// the process lifecycle. C1's rule (every retry branch must change something and
// must be bounded) cannot be expressed while the gas ladder, the floor, and the
// error classification are interleaved in one function.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"relayer/chain"
	"relayer/logbridge"
	"relayer/services"
)

// handleBatch relays one batch of source packets. When the destination supports
// update/packet folding, it builds packet proofs against the update's target
// height and submits the update followed by the packets in one atomic tx. Other
// destinations retain the update-then-packets fallback. It returns the indices
// of events that failed TRANSIENTLY and must be re-queued; successfully-relayed
// and permanently-dropped events are omitted.
func (m *Module) handleBatch(ctx context.Context, events []chain.Event) []int {
	// Serialized: see Module.batchMu. Nothing below re-enters handleBatch --
	// relayIsolated is the only retry path and it calls the destination directly
	// rather than back into this -- so this cannot deadlock on itself.
	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	return m.handleBatchLocked(ctx, events)
}

// handleBatchLocked is handleBatch's body, for callers that must do something
// else under the same lock. flushOnce is the one: its receipt check has to be
// serialized with submission, or a live batch can relay the packet in between
// and the flush submits a duplicate receive.
//
// Callers MUST hold batchMu.
func (m *Module) handleBatchLocked(ctx context.Context, events []chain.Event) []int {
	if len(events) == 0 {
		return nil
	}
	// Honor shutdown: if the context is already cancelled, re-queue everything
	// untouched rather than starting expensive proof/tx work that would run past
	// cancellation. The source re-queues these indices (nothing was relayed).
	if ctx.Err() != nil {
		return allIndices(len(events))
	}

	// Record every recv-relayed packet for timeout recovery BEFORE attempting the
	// relay: even if the update/proof/submit below fails, the scanner must still be
	// able to refund a packet that later expires undelivered. A terminal event can
	// race this handoff, so first consult the durable terminal tombstone and the
	// destination, then check the tombstone again after Add. Only SendPacket opens
	// a receive that can time out; Add is idempotent.
	trackFailed := make([]bool, len(events))
	skippedSettled := make([]bool, len(events))
	var requeue []int
	for i, e := range events {
		if e.Type == chain.SendPacket && m.pendingSettled != nil && m.pendingSettled(e.Raw) {
			log.Printf("[%s Relay] skipping settled packet seq=%d before tracking", m.name, e.Sequence)
			skippedSettled[i] = true
			continue
		}
		settled, err := m.packetSettled(ctx, e)
		if err != nil {
			// An unknown receipt must not be interpreted as delivery: omitting this
			// packet would be worse than a duplicate, because it may be the only
			// relayer able to move the user's funds.
			log.Printf("WARN [%s Relay] settlement check for %s seq=%d: %v; relaying anyway",
				m.name, e.Type, e.Sequence, err)
		} else if settled {
			if e.Type == chain.SendPacket && m.markPendingSettled != nil && !m.markPendingSettled(e.Raw) {
				log.Printf("ERROR [%s Relay][ATTENTION] packet %s seq=%d is settled but its terminal tombstone was not persisted",
					m.name, e.Type, e.Sequence)
			}
			log.Printf("[%s Relay] skipping settled packet %s seq=%d before proof", m.name, e.Type, e.Sequence)
			skippedSettled[i] = true
			continue
		}
		if e.Type == chain.SendPacket && m.track != nil {
			if !m.track(e.Raw, e.Height) {
				log.Printf("WARN [%s Relay] track pending packet seq=%d height=%d: durable write failed; re-queueing", m.name, e.Sequence, e.Height)
				trackFailed[i] = true
				requeue = append(requeue, i)
				continue
			}
			// MarkSettled deletes an active record when it wins after Add. This
			// second read turns the Add/terminal race into a clean skip instead of
			// spending a proof and a reverting transaction.
			if m.pendingSettled != nil && m.pendingSettled(e.Raw) {
				log.Printf("[%s Relay] skipping packet seq=%d: it settled while being tracked", m.name, e.Sequence)
				skippedSettled[i] = true
			}
		}
	}

	// Cheap precondition: the highest source height provable right now. A packet
	// above it (its source state not yet available — Cosmos AppHash H+2 lag, or ETH
	// beacon finality lag) is re-queued WITHOUT any client update or proof, so the
	// module does not burn an expensive Groth16 proof / beacon build every flush
	// while waiting.
	//
	// Held off while the source is in permanent failure. The events are already
	// TRACKED above, so a packet queued during the hold still gets its timeout
	// refund -- the hold skips the RPC and the proof, never the durable record.
	//
	// The destination-client hold works the same way: a frozen client rejects
	// everything, so nothing is proven or submitted until it is active again.
	if m.clientHeldOff(ctx, time.Now()) {
		m.lastWait = waitLogState{}
		return nonSettledIndices(skippedSettled)
	}
	if m.sourceHeldOff(time.Now()) {
		m.lastWait = waitLogState{}
		return nonSettledIndices(skippedSettled)
	}
	relayable, err := m.src.RelayableHeight(ctx)
	if err != nil {
		m.noteSourceFailure(err, "relayable height")
		// Forget the last wait: this flush learned nothing about the frontier, so a
		// wait that resumes identically afterwards is news again rather than a
		// repeat, and the error lines in between do not leave a silent gap.
		m.lastWait = waitLogState{}
		return nonSettledIndices(skippedSettled)
	}
	m.noteSourceHealthy()

	var stalled []waiting // still-waiting packets, with how long each has waited
	provable := make([]chain.Event, 0, len(events))
	provableIdx := make([]int, 0, len(events)) // original index behind provable[j]
	for i, e := range events {
		if trackFailed[i] || skippedSettled[i] {
			continue
		}
		if e.Height > relayable {
			requeue = append(requeue, i) // not yet provable — wait, no expensive work
			stalled = append(stalled, waiting{event: e, age: m.waits.observe(e)})
			continue
		}
		// Deliberately NOT cleared here. Clearing at this gate would reset the age of
		// a packet that clears the source gate every flush but parks at the
		// destination-coverage gate below — the exact shape of a stuck L2 ack, and the
		// case the age exists to expose. An entry is cleared only when the packet
		// stops waiting for good: relayed, or dropped as permanently dead.
		provable = append(provable, e)
		provableIdx = append(provableIdx, i)
	}
	m.waits.purgeStale()
	// Surface the wait so the finality/AppHash lag is visible: relayable moving up
	// shows the source's head advancing toward the packet, and each packet is named
	// (type, sequence, height, age) because the count alone cannot answer the two
	// questions an operator has when a leg goes quiet — is my packet in this queue,
	// and has it been here long enough to be stuck rather than merely waiting?
	//
	// Logged only when that picture CHANGES. This runs once per flush — every batch
	// period, on no backoff — and a wait is normally long. Measured on OP Sepolia:
	// with the attestor's derived-root gap at 150 blocks the frontier sat still for
	// the whole wait, so 67 byte-identical lines collapse to 1; at a gap of 10 it
	// advanced, so 4 lines collapse to 3 — one per real advance.
	//
	// The description is part of the key, not just the counts: a packet whose age
	// crosses a reporting threshold is new information even when the frontier has
	// not moved, and suppressing that would hide the very case the age exists for.
	if len(stalled) > 0 {
		description := describeWaiting(stalled)
		now := waitLogState{packets: len(stalled), relayable: relayable, description: description, logged: true}
		if now != m.lastWait {
			log.Printf("[%s Relay] waiting: %d packet(s) not yet relayable [%s] (source relayable height=%d)",
				m.name, len(stalled), description, relayable)
			m.lastWait = now
		}
	} else {
		m.lastWait = waitLogState{} // next wait reports itself, even if identical to the last one
	}
	if len(provable) == 0 {
		return requeue // nothing relayable this flush — skip the client update entirely
	}

	// Bail before the expensive client update / proof / submit if we are shutting
	// down; re-queue the provable packets so nothing is lost.
	if ctx.Err() != nil {
		return append(requeue, provableIdx...)
	}

	// Prepare the client state that packet proofs will target. A folding destination
	// leaves a needed update unsubmitted until the packet tx; the fallback submits
	// it immediately. The folded plan holds the client-update lock until its atomic
	// tx completes, preventing a refresh update from racing the planned height.
	foldPlan, proofHeight, err := m.prepareBatchUpdate(ctx, relayable)
	if err != nil {
		if m.holdIfClientInactive(ctx, err) {
			return append(requeue, provableIdx...)
		}
		m.noteSourceFailure(err, fmt.Sprintf("update client to height %d", relayable))
		return append(requeue, provableIdx...)
	}
	if foldPlan != nil {
		defer foldPlan.release()
	}

	// Build proofs per packet. A proof-build failure is transient and per-packet:
	// re-queue just that event and keep the rest in the multicall.
	packets := make([]chain.RelayPacket, 0, len(provable))
	relayedIdx := make([]int, 0, len(provable)) // original event index behind packets[j]
	for j, e := range provable {
		i := provableIdx[j]
		// The RelayableHeight partition says the SOURCE can prove e.Height; it says
		// nothing about how far the DESTINATION client actually advanced. Those differ
		// whenever a client update commits a lower height than it was asked for — an
		// L2 update commits the block its attested game covers, which is usually below
		// the requested height. Proving against a client that does not cover the packet
		// is never valid, so re-queue and wait for an update that does.
		//
		// This is logged because it used to be silent: a packet parked here re-ran a
		// full client update every flush, forever, with nothing in the log. Rate-limit
		// via the same cadence as the waiting log if it ever gets noisy.
		if e.Height > proofHeight {
			log.Printf("[%s Relay] waiting: %s seq=%d at height %d not yet covered by the destination client (trusts %d, waiting %s); re-queued",
				m.name, e.Type, e.Sequence, e.Height, proofHeight, m.waits.observe(e).Round(time.Second))
			requeue = append(requeue, i)
			continue
		}

		// Re-check the durable settlement tombstone right before spending a
		// Groth16 proof on this packet. SubscribeCosmos runs on its own goroutine
		// and can call MarkSettled at any point after the two checks at the top
		// of this function ran -- including while an earlier packet in this very
		// loop was being proven. Skipping here, rather than only at submission,
		// is what stops that race from also burning a proof.
		if e.Type == chain.SendPacket && m.pendingSettled != nil && m.pendingSettled(e.Raw) {
			log.Printf("[%s Relay] skipping packet seq=%d: it settled before its proof was built", m.name, e.Sequence)
			m.waits.clear(e)
			continue
		}

		var (
			proof []byte
			err   error
		)
		if e.Type == chain.TimeoutPacket {
			proof, err = m.src.NonMembershipProof(ctx, e.Raw, proofHeight)
		} else {
			proof, err = m.src.MembershipProof(ctx, e.Raw, proofHeight, e.Type)
		}
		if err != nil {
			// A permanent proof failure means the packet is dead (timed out — its
			// commitment can never be proven for a receive). DROP it rather than
			// re-queueing: retrying re-runs a full client update each flush and
			// drains gas, and the timeout scanner (fed independently) refunds it.
			// Any other proof failure is transient (RPC blip, H+2 lag) → re-queue.
			if chain.IsPermanent(err) {
				// Dead for good — forget any recorded wait so the entry does not sit in
				// the tracker until its TTL.
				m.waits.clear(e)
				log.Printf(dropToken(e.Type)+"[%s Relay] DROP packet (%s seq=%d height=%d): %v", m.name, e.Type, e.Sequence, e.Height, err)
				continue
			}
			// Every proof failure here re-queues, so logging one line per flush
			// says the same thing forever. What an operator actually needs is the
			// AGE: a proof failing for ten seconds is an RPC blip, one failing for
			// an hour is a packet nobody will ever deliver -- and the two used to
			// produce byte-identical lines.
			//
			// Whether the packet has another way out depends on its type, and the
			// STUCK line says which -- an operator's next action is different for
			// each, so a single wording would be wrong for one of them.
			//
			// A SendPacket does have one: once it is past its timeout every source
			// reports it permanent (evm/source.go, cosmos/source.go,
			// l2rollup/source.go all return chain.Permanent for an expired send),
			// the branch above drops it, and the timeout scanner refunds it. So
			// "no other exit" would be false for a send -- naming the real exit is
			// more useful than a warning to act.
			//
			// An AckPacket has none: its counterparty packet already has a receipt
			// on the destination, so it can never be timed out either, and its
			// escrow stays locked. A TimeoutPacket that cannot be proven is the
			// same -- nothing else refunds it. Both are worth saying out loud
			// rather than dropping, because dropping would lose the only record
			// that the funds are stranded.
			if age, report := m.waits.observeProofFailure(e); report {
				switch {
				case age < proofFailureStuckAfter:
					log.Printf("WARN [%s Relay] proof for packet (%s seq=%d height=%d, failing for %s): %v",
						m.name, e.Type, e.Sequence, e.Height, age.Round(time.Second), err)
				case e.Type == chain.SendPacket:
					log.Printf("WARN [%s Relay] STUCK: %s seq=%d height=%d has failed to prove for %s; it is refunded by the timeout scanner once past its timeout; last error: %v",
						m.name, e.Type, e.Sequence, e.Height, age.Round(time.Second), err)
				default:
					log.Printf("ERROR [%s Relay] STUCK: %s seq=%d height=%d has failed to prove for %s and has no other exit, so its escrow stays locked; last error: %v",
						m.name, e.Type, e.Sequence, e.Height, age.Round(time.Second), err)
				}
			}
			requeue = append(requeue, i)
			continue
		}
		packets = append(packets, chain.RelayPacket{
			Type: e.Type, Sequence: e.Sequence, Packet: e.Raw,
			Proof: proof, Height: proofHeight, AckBytes: e.AckBytes,
		})
		relayedIdx = append(relayedIdx, i)
	}

	if len(packets) == 0 {
		// Nothing was provable this pass. Submit the folded client update on its
		// own before returning, or the light client never advances again.
		//
		// A folding destination leaves the update unsubmitted so it can ride in
		// the packet tx. When every proof fails, returning here drops it — and the
		// proofs are built at the height the client already trusts, so the client
		// staying put is exactly why they failed. That closes a loop the relayer
		// cannot leave on its own:
		//
		//   proofs target the trusted height  ->  that height falls out of the
		//   execution node's state window  ->  every proof fails  ->  no packets
		//   ->  the update is dropped  ->  the client stays put  ->  the gap only
		//   widens
		//
		// Observed on Sepolia: trustedSlot pinned at 10885728 for 20 minutes while
		// finalizedSlot advanced, every pass logging `eth_getProof failed:
		// historical state ... is not available`, with no update ever submitted.
		// Advancing the client is independent of whether any packet can be
		// relayed, so it must not be conditional on one.
		if foldPlan != nil {
			m.submitFoldedUpdateAlone(ctx, foldPlan)
		}
		return requeue
	}

	// Last cancellation check before the tx: on shutdown, re-queue the proven
	// packets rather than broadcasting during teardown.
	if ctx.Err() != nil {
		return append(requeue, relayedIdx...)
	}

	// Re-check the durable settlement tombstone one more time, immediately before
	// the tx. A terminal event can land on SubscribeCosmos's goroutine at any
	// point during the work above -- climbing the client, building every other
	// packet's proof -- none of which the per-packet check inside that loop can
	// see once IT has already passed for this packet. Filtering here shrinks the
	// remaining window to the submission call itself.
	if m.pendingSettled != nil {
		kept := packets[:0]
		keptIdx := relayedIdx[:0]
		for j, p := range packets {
			if p.Type == chain.SendPacket && m.pendingSettled(p.Packet) {
				log.Printf("[%s Relay] skipping packet seq=%d: it settled while its batch was being prepared", m.name, p.Sequence)
				m.waits.clearPacket(p)
				continue
			}
			kept = append(kept, p)
			keptIdx = append(keptIdx, relayedIdx[j])
		}
		packets, relayedIdx = kept, keptIdx
		if len(packets) == 0 {
			if foldPlan != nil {
				m.submitFoldedUpdateAlone(ctx, foldPlan)
			}
			return requeue
		}
	}

	// Submit the whole batch. A folded plan prepends the not-yet-on-chain client
	// update in the same atomic tx; otherwise the client is already current and the
	// destination submits packets only.
	var committed int
	if foldPlan != nil {
		err = foldPlan.destination.RelayWithUpdate(ctx, m.clientID, foldPlan.update, packets)
		if err == nil {
			committed = len(packets)              // the folded path is one atomic tx: all or nothing
			m.lastHeight = foldPlan.update.Height // foldPlan still holds m.mu
			m.recordClientUpdate(foldPlan.update)
			foldPlan.release()
		}
	} else {
		committed, err = m.dst.RelayPackets(ctx, packets)
	}
	if err == nil {
		m.noteClientActive("")
		m.settleDelivered(packets)
		return requeue
	}
	// A destination that stopped part way has delivered packets[:committed]. Both
	// endings below are wrong for those: re-queueing submits them a second time,
	// and isolating them reports each as DROPPED while it sits delivered on the
	// destination. Settle the prefix and carry on with the tail alone. Only the
	// packets-only path can stop part way -- a folded transaction is atomic, so
	// committed is 0 on its failure and this block is a no-op there.
	if committed > len(packets) {
		log.Printf("ERROR [%s Relay][ATTENTION] destination reported %d of %d packet(s) committed; clamping, the adapter is breaking its contract",
			m.name, committed, len(packets))
		committed = len(packets)
	}
	if committed > 0 {
		log.Printf("WARN [%s Relay] %d of %d packet(s) were committed before the failure at height %d; settling those and retrying the rest",
			m.name, committed, len(packets), proofHeight)
		m.settleDelivered(packets[:committed])
		packets = packets[committed:]
		relayedIdx = relayedIdx[committed:]
		if len(packets) == 0 {
			if foldPlan != nil {
				foldPlan.release()
			}
			return requeue // the whole batch landed; the error is about nothing left to retry
		}
	}

	// The client refused, not the packets: hold the direction and keep them all.
	// Checked before the permanent/transient split, because neither ending is
	// right -- a drop loses the packets, and a plain re-queue retries every flush
	// against a client that will refuse again.
	if isClientNotActive(err) {
		if foldPlan != nil {
			foldPlan.release()
		}
		m.noteClientInactive("", err)
		return append(requeue, relayedIdx...)
	}
	if !chain.IsPermanent(err) {
		// Not permanent is not the same as transient. A Cosmos Simulate rejection
		// is neither -- the SDK hides a frozen client's code behind sdk/6 -- so
		// ask before re-queueing into a client that will refuse again.
		if foldPlan != nil {
			foldPlan.release()
		}
		if m.holdIfClientInactive(ctx, err) {
			return append(requeue, relayedIdx...)
		}
		// Transient (RPC, nonce, broadcast) → re-queue the whole batch for retry.
		log.Printf("WARN [%s Relay] relay %d packet(s) at height %d: %v", m.name, len(packets), proofHeight, err)
		return append(requeue, relayedIdx...)
	}
	// An UNRESOLVED abandoned nonce is permanent, but it is not a revert, and the
	// two endings below are both wrong for it.
	//
	// Reported by @DongLieu on #485. When the EVM sender gives up waiting, the
	// transaction it abandoned may still execute -- a cancellation broadcast to
	// one node is not inclusion, and the account's nonce may already have been
	// consumed by the payload itself. Isolating here resubmits every packet
	// individually against a freshly invalidated nonce cache, so they can take
	// LOWER nonces and mine before the original: the double execution the give-up
	// path exists to prevent, reached through the mechanism meant to prevent it.
	// Dropping is no better -- the packet may be mid-flight, not dead.
	//
	// So neither. The packets keep their durable pending records, which is what
	// settles them when the acknowledgement arrives or refunds them when they time
	// out; this path stops touching them.
	if errors.Is(err, services.ErrUnresolvedEVMNonce) {
		log.Printf("ERROR [%s Relay][ATTENTION] %d packet(s) at height %d left in an unresolved state: %v; "+
			"not retried and not dropped, because the abandoned transaction may still execute. "+
			"They stay in the pending tracker and settle on their acknowledgement or time out.",
			m.name, len(packets), proofHeight, err)
		if foldPlan != nil {
			foldPlan.release() // folding is L2-only, so this is nil here; keep the lock discipline anyway
		}
		return requeue
	}
	// Last check before anything below can drop a packet: a deterministic revert
	// whose cause did not survive to the relayer (no revert data, an unrecognised
	// wrapper) may still be a frozen client. Asking costs one read on a path that
	// is already rare, and a wrong answer here costs every packet in flight.
	if status, inactive := m.destinationClientInactive(ctx); inactive {
		if foldPlan != nil {
			foldPlan.release()
		}
		m.noteClientInactive(status, err)
		return append(requeue, relayedIdx...)
	}
	// A folded transaction carried the client update AND the packets, so a
	// deterministic rejection of it does not say which of the two the chain
	// refused. The poison logic below assumes it does -- and it is only entitled
	// to, for a transaction that contained nothing but packets.
	//
	// Reported by @neitdung: with the update folded in, a rejected UPDATE looked
	// like a poison packet. A single ack was dropped outright, and a larger batch
	// was retried as folded singletons carrying the same update, so every one
	// failed for the same reason and every packet was dropped in turn. An ack and
	// a timeout have no independent escape -- the counterparty already holds a
	// receipt, so nothing times them out and nothing refunds them -- and dropping
	// one on a failure never shown to be its own strands the escrow.
	//
	// So attribute before isolating: submit the update by itself. It lands, or it
	// does not, and either answer is worth one transaction on a path this rare.
	if foldPlan != nil {
		if updateErr := m.dst.UpdateClient(ctx, m.clientID, foldPlan.update); updateErr != nil {
			// The update is what the chain refused. Nothing has been shown about
			// any packet, so nothing may be dropped: hold them all and let the
			// next flush rebuild against whatever the client then trusts.
			foldPlan.release()
			if m.holdIfClientInactive(ctx, updateErr) {
				return append(requeue, relayedIdx...)
			}
			log.Printf("ERROR [%s Relay][ATTENTION] the folded client update at height %d was refused on its own (%v); "+
				"holding %d packet(s) rather than attributing that to them (folded failure: %v)",
				m.name, foldPlan.update.Height, updateErr, len(packets), err)
			return append(requeue, relayedIdx...)
		}
		// The update lands alone. That clears the update, and nothing more: the
		// folded transaction was one atomic unit, and a unit can be refused for
		// what the two are together (its gas envelope, its route) while each half
		// passes on its own. So the packets have still not been tried, and every
		// one of them -- a single one included -- goes through the packets-only
		// path before anything is dropped; a drop there rests on that packet's own
		// failure, not on the folded one. The client now covers the height the
		// proofs were built at, so those sends are valid as built.
		//
		// Reported by @neitdung, second round: the single-packet case skipped
		// this and dropped the packet on the folded error, unattributed.
		m.lastHeight = foldPlan.update.Height
		m.recordClientUpdate(foldPlan.update)
		foldPlan.release()
		log.Printf("WARN [%s Relay] folded transaction at height %d failed (%v) but its client update landed alone; relaying %d packet(s) on their own",
			m.name, proofHeight, err, len(packets))
		return append(requeue, m.relayIsolated(ctx, packets, relayedIdx)...)
	}
	// Permanent (deterministic on-chain revert) of a transaction that carried
	// packets and nothing else. A single packet is the poison — DROP it (the
	// timeout scanner refunds it if it later expires). But a multicall is atomic:
	// one bad packet (timed out / duplicate / already received) reverts the whole
	// tx, so dropping the batch would discard its VALID siblings too. Isolate:
	// retry each packet on its own, so the poison fails (and drops) alone while
	// its siblings still relay. This is the failure path only (rare), so the extra
	// per-packet txs are acceptable to avoid losing valid packets.
	if len(packets) == 1 {
		m.waits.clearPacket(packets[0]) // dead for good — stop reporting a wait for it
		log.Printf(dropToken(packets[0].Type)+"[%s Relay] DROP packet (%s seq=%d) at height %d (permanent): %v",
			m.name, packets[0].Type, packets[0].Sequence, proofHeight, err)
		return requeue
	}
	log.Printf("WARN [%s Relay] permanent batch failure at height %d (%v); isolating %d packet(s) individually", m.name, proofHeight, err, len(packets))
	return append(requeue, m.relayIsolated(ctx, packets, relayedIdx)...)
}

// dropToken is the level of a DROP line. A dropped SendPacket still has an exit:
// the timeout scanner refunds it once it expires, so the drop is the designed
// outcome for a packet that timed out or was already received, and a warning. An
// ack or a timeout that is dropped has no other exit -- the same split the STUCK
// lines make in handleBatchLocked -- and is an error, so alerting on level=error
// does not page on a routine timeout. The token goes ahead of the label because
// logbridge reads it from the start of the message.
func dropToken(t chain.EventType) string {
	if t == chain.SendPacket {
		return logbridge.Warn
	}
	return logbridge.Error
}

// submitFoldedUpdateAlone submits a still-unsubmitted folded client update on
// its own when no packet survives to carry it -- either because every proof
// failed, or because the pre-submission settlement recheck filtered every
// remaining packet out. Advancing the client must not be conditional on a
// packet surviving either path, or a gap that only widens (see the comment at
// the len(packets)==0 call site) can reopen for a second reason.
func (m *Module) submitFoldedUpdateAlone(ctx context.Context, foldPlan *foldedUpdatePlan) {
	// Destination.UpdateClient, not the folding path: RelayWithUpdate is
	// specified to carry packets, and there are none.
	if err := m.dst.UpdateClient(ctx, m.clientID, foldPlan.update); err != nil {
		if m.holdIfClientInactive(ctx, err) {
			return
		}
		log.Printf("WARN [%s Relay] client update (no relayable packets) to height %d: %v",
			m.name, foldPlan.update.Height, err)
		return
	}
	m.lastHeight = foldPlan.update.Height // foldPlan still holds m.mu
	m.recordClientUpdate(foldPlan.update)
	foldPlan.release()
	log.Printf("[%s Relay] advanced client to height %d with no packets to relay",
		m.name, foldPlan.update.Height)
}

type foldedUpdatePlan struct {
	destination chain.FoldingDestination
	update      chain.ClientUpdate
	release     func()
}

// prepareBatchUpdate returns the proof height and, when supported and needed, a
// still-unsubmitted update to fold into the packet tx. A returned folded plan
// owns m.mu until release is called; release is idempotent.
func (m *Module) prepareBatchUpdate(ctx context.Context, height uint64) (*foldedUpdatePlan, uint64, error) {
	folding, ok := m.dst.(chain.FoldingDestination)
	if !ok || !folding.SupportsUpdatePacketFolding() {
		if err := m.updateClientTo(ctx, height); err != nil {
			return nil, 0, err
		}
		m.mu.Lock()
		proofHeight := m.lastHeight
		m.mu.Unlock()
		return nil, proofHeight, nil
	}

	m.mu.Lock()
	locked := true
	release := func() {
		if locked {
			locked = false
			m.mu.Unlock()
		}
	}
	fail := func(err error) (*foldedUpdatePlan, uint64, error) {
		release()
		return nil, 0, err
	}

	update, foldable, err := m.climbToFoldableLocked(ctx, height)
	if err != nil {
		return fail(err)
	}
	if !foldable {
		proofHeight := m.lastHeight
		release()
		return nil, proofHeight, nil
	}
	return &foldedUpdatePlan{destination: folding, update: update, release: release}, update.Height, nil
}

// climbToFoldableLocked advances the client until one more update reaches height,
// and returns that last update STILL UNSUBMITTED so the caller can fold it into
// the packet transaction. foldable is false when there is nothing left to submit:
// the client already covers height, or the builder reported no work.
//
// The climb belongs here and not only in updateClientTo because the two branches
// of prepareBatchUpdate must cross a churn gap the same way. The unfolded branch
// delegates to updateClientTo, which loops; this one built exactly one update, so
// a folding destination behind a gap advanced one hop per flush -- sixteen hops
// at the 30s scan cadence is eight minutes of re-queueing, on the packet-driven
// path, with packets waiting.
//
// An intermediate hop cannot be folded: it lands BELOW the requested height by
// definition, so the packets riding it would declare a proof height the update
// does not install -- the invariant requireProofsBuiltAt exists to hold. Each hop
// therefore goes out on its own, and only the rung that reaches the target is
// handed back to be folded.
//
// The caller holds m.mu, and keeps holding it across the whole climb: an update
// submitted by another goroutine in between would leave the hop chain reasoning
// about a trusted height that no longer holds.
func (m *Module) climbToFoldableLocked(ctx context.Context, height uint64) (chain.ClientUpdate, bool, error) {
	for hop := 0; ; hop++ {
		if hop >= chain.MaxClientUpdateHops {
			return chain.ClientUpdate{}, false, fmt.Errorf(
				"relay %s: client still short of height %d after %d hop(s); the trust overlap has not been re-established",
				m.name, height, hop)
		}
		if height <= m.lastHeight {
			return chain.ClientUpdate{}, false, nil // already covered — nothing to prove
		}
		header, err := m.src.QueryHeader(ctx, height)
		if err != nil {
			return chain.ClientUpdate{}, false, fmt.Errorf("query header at %d: %w", height, err)
		}
		update, err := m.builder.Build(ctx, header)
		if err != nil {
			return chain.ClientUpdate{}, false, fmt.Errorf("build client update: %w", err)
		}
		// An empty payload list means "client already current"; learn the height the
		// builder reports without submitting a tx, exactly as updateClientStepLocked does.
		if len(update.Payloads) == 0 {
			if update.Height > m.lastHeight {
				m.lastHeight = update.Height
			}
			m.recordClientUpdate(update)
			return chain.ClientUpdate{}, false, nil
		}
		// A stale update is skipped without a tx. This is also the anti-spin guard:
		// a hop that installs nothing ends the climb instead of repeating it.
		if update.Height <= m.lastHeight {
			return chain.ClientUpdate{}, false, nil
		}
		if !update.IsHop {
			return update, true, nil
		}
		if err := m.dst.UpdateClient(ctx, m.clientID, update); err != nil {
			return chain.ClientUpdate{}, false, fmt.Errorf("submit client update at %d: %w", update.Height, err)
		}
		m.lastHeight = update.Height
		m.recordClientUpdate(update)
		log.Printf("[%s Relay] client update hop %d landed at %d, short of %d; climbing again",
			m.name, hop, m.lastHeight, height)
	}
}

// relayIsolated re-submits each packet of a permanently-failed batch on its own
// to isolate the poison packet from its valid siblings. Each singleton is
// classified independently: a permanent failure drops that packet alone (the
// timeout scanner refunds it), a transient failure re-queues its index, and a
// success untracks a delivered send. It returns the indices to re-queue.
func (m *Module) relayIsolated(ctx context.Context, packets []chain.RelayPacket, relayedIdx []int) []int {
	var requeue []int
	for j, p := range packets {
		if ctx.Err() != nil { // shutting down — re-queue the rest untouched
			requeue = append(requeue, relayedIdx[j:]...)
			break
		}
		single := []chain.RelayPacket{p}
		// One packet has no prefix to report: a nil error means it landed.
		if _, err := m.dst.RelayPackets(ctx, single); err != nil {
			// The client stopped accepting anything mid-isolation: this packet and
			// every one after it are fine, so hold them all rather than drop.
			if isClientNotActive(err) {
				m.noteClientInactive("", err)
				requeue = append(requeue, relayedIdx[j:]...)
				break
			}
			if chain.IsPermanent(err) {
				if status, inactive := m.destinationClientInactive(ctx); inactive {
					m.noteClientInactive(status, err)
					requeue = append(requeue, relayedIdx[j:]...)
					break
				}
				m.waits.clearPacket(p) // dead for good — stop reporting a wait for it
				log.Printf(dropToken(p.Type)+"[%s Relay] DROP packet (%s seq=%d height=%d, permanent): %v", m.name, p.Type, p.Sequence, p.Height, err)
				continue
			}
			if m.holdIfClientInactive(ctx, err) {
				requeue = append(requeue, relayedIdx[j:]...)
				break
			}
			log.Printf("WARN [%s Relay] isolated relay (%s seq=%d height=%d): %v", m.name, p.Type, p.Sequence, p.Height, err)
			requeue = append(requeue, relayedIdx[j])
			continue
		}
		m.noteClientActive("")
		m.settleDelivered(single)
	}
	return requeue
}

// nonSettledIndices returns the indices that remain retryable after a batch
// pass has identified terminal packets. Source errors and permanent-failure
// holds happen after this filter, so they must never put a known-settled packet
// back into the queue.
func nonSettledIndices(skippedSettled []bool) []int {
	indices := make([]int, 0, len(skippedSettled))
	for i, settled := range skippedSettled {
		if !settled {
			indices = append(indices, i)
		}
	}
	return indices
}

// sourceHeldOff reports whether the source is inside a permanent-failure hold,
// and logs the first tick that ends one so the recovery is visible.
//
// Callers must hold batchMu.
func (m *Module) sourceHeldOff(now time.Time) bool {
	if m.sourceProbeAt.IsZero() {
		return false
	}
	if now.Before(m.sourceProbeAt) {
		return true
	}
	m.sourceProbeAt = time.Time{} // probe this pass; noteSourceFailure re-arms it
	return false
}

// noteSourceFailure logs a source failure and, when it is PERMANENT, holds the
// source off for a widening interval.
//
// The log line says which it was. Without that an operator sees the same
// "relayable height: ..." line for a route the attestor does not serve and for a
// replica that is a second behind, and the first looks like the second for as
// long as it takes someone to read the classifier.
//
// Callers must hold batchMu.
func (m *Module) noteSourceFailure(err error, what string) {
	if !chain.IsPermanent(err) {
		m.noteSourceHealthy() // a transient answer means the source is reachable
		log.Printf("WARN [%s Relay] %s: %v", m.name, what, err)
		return
	}
	// Same 1m->15m ladder the periodic client update uses; one cadence for
	// "this keeps failing, stop asking so often" rather than two.
	m.sourceBackoff = nextPeriodicUpdateBackoff(m.sourceBackoff)
	m.sourceProbeAt = time.Now().Add(m.sourceBackoff)
	log.Printf("ERROR [%s Relay][ATTENTION] %s: %v; this is PERMANENT — no retry resolves it, "+
		"so the source is held off for %s rather than asked every pass. Queued packets are kept and "+
		"still time out normally; fix the configuration to clear it.",
		m.name, what, err, m.sourceBackoff)
}

// noteSourceHealthy clears the hold after the source answers.
//
// Callers must hold batchMu.
func (m *Module) noteSourceHealthy() {
	if m.sourceBackoff == 0 && m.sourceProbeAt.IsZero() {
		return
	}
	log.Printf("[%s Relay] source answered again; permanent-failure hold cleared", m.name)
	m.sourceBackoff = 0
	m.sourceProbeAt = time.Time{}
}
