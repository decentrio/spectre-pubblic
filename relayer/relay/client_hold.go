// This file holds the destination-client hold: what the module does when the
// light client it relays into stops accepting messages at all.
//
// A frozen client is not a property of any packet. Before #402 its rejection
// reached the module as an ordinary permanent revert, so every in-flight packet
// was isolated, rejected again, and dropped -- one transaction each, with only
// "status=0" and "DROP packet" in the log. The packets were not dead: the same
// ones succeed after an unfreeze() or a client migration.
//
// So the module holds the whole direction instead. Nothing is dropped and no
// wait is cleared; queued packets stay queued and tracked (the timeout scanner
// still sees them), and the path resumes by itself once the client is active.
package relay

import (
	"context"
	"errors"
	"log"
	"time"

	"relayer/chain"
	"relayer/services"
)

// isClientNotActive reports whether a destination error is the client refusing,
// not the message.
func isClientNotActive(err error) bool {
	return errors.Is(err, services.ErrClientNotActive)
}

// mayBeClientRefusal reports whether a failure is one an inactive destination
// client could have caused, and so is worth one status query to find out:
//
//   - the sentinel itself;
//   - a deterministic rejection that lost its cause on the way -- an EVM revert
//     whose selector the RPC omitted;
//   - a Cosmos Simulate rejection. The SDK re-wraps every Simulate handler
//     error as sdk/6, so a frozen client's client/29 is unrecognisable there,
//     and the error is not even permanent: without this a folded Cosmos or L2
//     batch on a frozen client re-proves and retries every flush, never held.
//
// Transport failures (RPC down, nonce, broadcast) match none of these, so they
// never cost a query.
func mayBeClientRefusal(err error) bool {
	return isClientNotActive(err) ||
		chain.IsPermanent(err) ||
		errors.Is(err, services.ErrPermanentRelayFailure) ||
		services.IsCosmosSimulationRejection(err)
}

// checkClientAtStartup reads the destination client's status once before the
// workers start, so a relayer restarted against an already-frozen client says so
// in one line instead of discovering it one packet at a time.
//
// A read failure is logged and ignored: it is "do not know", and refusing to
// start on it would turn an RPC blip into an outage.
func (m *Module) checkClientAtStartup(ctx context.Context) {
	checker, ok := m.dst.(chain.ClientStatusChecker)
	if !ok {
		return
	}
	active, status, err := checker.ClientActive(ctx)
	if err != nil {
		log.Printf("WARN [%s Relay] could not read destination client %s status at startup: %v; continuing",
			m.name, m.clientID, err)
		return
	}
	if active {
		return
	}
	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	m.noteClientInactive(status, nil)
}

// destinationClientInactive asks the destination, when it can answer, whether
// its client is inactive. It is the guard in front of every drop: a revert whose
// selector did not survive to the relayer (a wrapper that swallowed it, an RPC
// that strips revert data) must still not be charged against the packet.
//
// "Do not know" answers false, which keeps the pre-existing drop behavior for a
// destination that cannot be asked or does not answer.
func (m *Module) destinationClientInactive(ctx context.Context) (string, bool) {
	checker, ok := m.dst.(chain.ClientStatusChecker)
	if !ok {
		return "", false
	}
	active, status, err := checker.ClientActive(ctx)
	if err != nil {
		log.Printf("WARN [%s Relay] destination client %s status unknown before a drop: %v",
			m.name, m.clientID, err)
		return "", false
	}
	return status, !active
}

// clientHeldOff reports whether the direction is held on an inactive destination
// client. Once the probe interval elapses it re-reads the status when the
// destination can answer; one that cannot is let through for a single pass, and
// a repeat rejection re-arms the hold.
//
// Callers must hold batchMu.
func (m *Module) clientHeldOff(ctx context.Context, now time.Time) bool {
	if !m.clientInactive {
		return false
	}
	if now.Before(m.clientProbeAt) {
		return true
	}
	checker, ok := m.dst.(chain.ClientStatusChecker)
	if !ok {
		return false
	}
	active, status, err := checker.ClientActive(ctx)
	if err != nil {
		// Unknown is not active: keep holding, and ask again at the same spacing.
		m.clientProbeAt = now.Add(m.clientBackoff)
		log.Printf("WARN [%s Relay] destination client %s status unknown while held: %v; re-checking in %s",
			m.name, m.clientID, err, m.clientBackoff)
		return true
	}
	if !active {
		m.noteClientInactive(status, nil)
		return true
	}
	m.noteClientActive(status)
	return false
}

// noteClientInactive arms (or extends) the hold. The first one logs loudly and
// names what the operator needs; repeats log one short line per probe.
//
// Callers must hold batchMu.
func (m *Module) noteClientInactive(status string, cause error) {
	first := !m.clientInactive
	m.clientInactive = true
	m.clientBackoff = nextPeriodicUpdateBackoff(m.clientBackoff)
	m.clientProbeAt = time.Now().Add(m.clientBackoff)
	if status == "" {
		status = "not active"
	}
	held := "destination client " + m.clientID + " is " + status + "; relaying held"
	m.clientHold.Store(&held)
	if !first {
		log.Printf("WARN [%s Relay] destination client %s still %s; holding, re-checking in %s",
			m.name, m.clientID, status, m.clientBackoff)
		return
	}
	reason := ""
	if cause != nil {
		reason = " (" + cause.Error() + ")"
	}
	log.Printf("ERROR [%s Relay][ATTENTION] destination client %s is %s%s. "+
		"No packet will be relayed on this path, and none is dropped: queued packets stay queued and "+
		"tracked, and the timeout scanner still sees them. Relaying resumes by itself once the client "+
		"is unfrozen or migrated. Re-checking in %s.",
		m.name, m.clientID, status, reason, m.clientBackoff)
}

// noteClientActive lifts the hold.
//
// Callers must hold batchMu.
func (m *Module) noteClientActive(status string) {
	if !m.clientInactive {
		return
	}
	m.clientInactive = false
	m.clientBackoff = 0
	m.clientProbeAt = time.Time{}
	m.clientHold.Store(nil)
	if status == "" {
		status = "accepting messages again"
	}
	log.Printf("[%s Relay] destination client %s is %s; hold lifted, relaying resumes", m.name, m.clientID, status)
}

// clientStillHeld is the hold check for loops that run beside handleBatch: the
// expiry refresh and the periodic update. Both submit client updates, which a
// frozen client reverts -- paid for in gas on every attempt -- so they skip
// while the hold is on.
//
// They also probe it. The hold is otherwise re-checked only when a batch runs,
// and a quiet path delivers no batch: without this, a client unfrozen while no
// packet was moving would stay held, and the refresh that keeps it from
// expiring would never run again.
//
// The unheld case is one atomic load and never touches batchMu.
func (m *Module) clientStillHeld(ctx context.Context) bool {
	if m.clientHold.Load() == nil {
		return false
	}
	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	return m.clientHeldOff(ctx, time.Now())
}

// holdIfClientInactive arms the hold when a client-update failure was caused by
// an inactive destination client, and reports whether it did.
//
// The sentinel is the cheap, explicit signal, but it depends on the revert
// selector reaching the relayer, and an RPC that omits revert data turns a
// frozen client's rejection into a bare ErrPermanentRelayFailure. So a
// deterministic rejection without the sentinel also asks the destination, the
// same fallback that guards every packet drop. Transient failures (RPC, nonce)
// never trigger the query.
//
// Callers must hold batchMu.
func (m *Module) holdIfClientInactive(ctx context.Context, err error) bool {
	if isClientNotActive(err) {
		m.noteClientInactive("", err)
		return true
	}
	if !mayBeClientRefusal(err) {
		return false
	}
	status, inactive := m.destinationClientInactive(ctx)
	if !inactive {
		return false
	}
	m.noteClientInactive(status, err)
	return true
}

// armClientHoldFrom is holdIfClientInactive for the loops beside handleBatch --
// the expiry refresh and the periodic update -- which do not hold batchMu.
//
// Without it a client that freezes on a QUIET path is never held: no batch runs
// to notice, so clientStillHeld stays false, both loops keep submitting updates
// that revert on their backoff, and Status never reports HELD.
//
// batchMu is taken only when the failure could be the client's, never on the
// transient failure paths.
func (m *Module) armClientHoldFrom(ctx context.Context, err error) bool {
	if !mayBeClientRefusal(err) {
		return false
	}
	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	return m.holdIfClientInactive(ctx, err)
}
