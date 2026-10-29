package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"relayer/services"
	"strconv"
	"time"
)

// The refresh loop: keeps the destination client inside its trusting period
// when no packet traffic would. Split from module.go (E6) -- the engine's
// Run and its other loops stay there; this file is the one loop with its own
// policy (margin, backoff cap, last-known trusting period).

// refreshLoop proactively advances the destination client before it expires,
// covering quiet periods with no packet traffic (the anti-expiry routine).
//
// RETRY POLICY: a failed attempt backs off exponentially, one tick doubling up to
// refreshBackoffMax, and a success or a client that no longer needs refreshing
// clears it. This is a decision, not an omission -- the loop next door states its
// own and this one used to state nothing.
//
// It retries at all because the failure may be an RPC blip. It backs off because
// on the Cosmos->EVM path one attempt is a Groth16 proof plus a transaction, and
// a deterministic failure -- an update that keeps reverting -- would otherwise
// spend thirty of them inside one window, none of which can succeed. The cap is
// far below refreshMargin so the window still gets eight tries: backing off past
// the margin would trade a gas leak for an expired client, which is worse.
//
// WHAT IT SAYS: a run line when a refresh is due and a line when one succeeds,
// both carrying the trust left. Without them "the client has twenty hours" and
// "the client has twenty-five minutes and the last refresh worked" are both
// silence, and an operator cannot tell either from a refresh that is failing.
func (m *Module) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(refreshTick)
	defer ticker.Stop()

	var backoff time.Duration // 0 = nothing has failed
	var retryAt time.Time     // zero = attempt on the next tick
	// The trusting period of the last successful expiry read, so a failing query
	// still sizes its retry against THIS client's margin rather than the default.
	// Zero only before the first successful read, where the default is all there is.
	var lastTrustingPeriod time.Duration
	// The note of the last failed pass, so a tick spent inside the backoff can
	// say what it is waiting out rather than only that it is waiting.
	var lastFailure string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil { // select may pick the tick even when ctx is done
				return
			}
			if time.Now().Before(retryAt) {
				// Inside the backoff from the last failed attempt. The pass still ran,
				// so it still beats -- a backoff of minutes must not read as a wedged
				// loop in `relayer status` -- and the note keeps the failure it is
				// waiting out, so the reason does not vanish behind the wait.
				m.beats.record(workerRefresh, "backing off until "+retryAt.Format(time.RFC3339)+"; last pass: "+lastFailure)
				continue
			}

			// Every ending of this pass beats, including the failures, and the
			// NOTE carries what happened. Reported by @DongLieu: the beat used to
			// sit after the expiry query succeeded, on the argument that "the loop
			// is scheduled" and "the loop can reach the chain" are different
			// claims. They are -- but this measures the first, heartbeat.record
			// says so outright ("a pass that FAILED still counts"), and the second
			// is what the note is for. Against an unreachable RPC every tick took
			// the error path, so no beat was ever recorded and `relayer status`
			// showed the anti-expiry loop as never started: wedged and merely
			// failing look the same again, which is the confusion this type
			// exists to remove -- on the one loop whose liveness matters most
			// exactly when an endpoint is down.
			if m.clientStillHeld(ctx) {
				m.beats.record(workerRefresh, "held: destination client not active")
				continue
			}

			expiresAt, trustingPeriod, err := m.dst.ClientExpiresAt(ctx, m.clientID)
			if err != nil {
				// Sized by the LAST period this loop read, not by zero.
				//
				// Reported by @DongLieu: passing 0 here fell back to the default
				// margin, whose cap is the five-minute ceiling, and my reason for it
				// -- "a client whose real margin is smaller gets a shorter cap on
				// the next pass, once the query answers" -- assumed a pass that
				// answers. Repeated query failures are exactly the case where none
				// does. On a 10-minute trusting period the real margin is 2m30s and
				// the cap would have been five: one retry spanning twice the window
				// the loop exists to work inside. It is the bug this branch set out
				// to fix, surviving in the one path I argued was safe.
				//
				// lastTrustingPeriod is zero only before the first successful read,
				// where the default is all there is.
				backoff, retryAt = m.refreshFailed(backoff, lastTrustingPeriod, "query client expiry", err)
				lastFailure = "query client expiry failed: " + err.Error()
				m.beats.record(workerRefresh, lastFailure)
				continue
			}
			// Remembered for the failure path above. The period is a property of the
			// client, not of this read, so the newest one we have is the best answer
			// available while the query is failing.
			lastTrustingPeriod = trustingPeriod
			if !needsRefresh(expiresAt, time.Now(), trustingPeriod) {
				backoff, retryAt = 0, time.Time{} // healthy: the next failure starts over
				m.beats.record(workerRefresh, "client expires in "+time.Until(expiresAt).Round(time.Second).String())
				continue
			}

			trustLeft := time.Until(expiresAt).Round(time.Second)
			log.Printf("[%s UpdateClient] refresh: client expires in %s; advancing it", m.name, trustLeft)

			latest, err := m.src.LatestHeight(ctx)
			if err != nil {
				backoff, retryAt = m.refreshFailed(backoff, trustingPeriod, "latest source height", err)
				lastFailure = "refresh due, source height unavailable: " + err.Error()
				m.beats.record(workerRefresh, lastFailure)
				continue
			}
			// refreshClientTo, not updateClientTo: on SpectreClient the expiry is
			// the pinned snapshot's, which only a rotation moves. A plain update
			// here lands as updateApplicationState and the client expires anyway.
			if err := m.refreshClientTo(ctx, latest); err != nil {
				// The source has no block past the client yet, so the rotation the
				// refresh owes cannot be built. Nothing failed -- no proof, no tx --
				// so this is not backed off; it stays owed and the next tick, which
				// re-reads the unchanged expiry, tries again. The same case
				// periodicUpdateLoop polls for. Checked before the hold, which must
				// not read a caught-up source as an inactive client.
				if errors.Is(err, services.ErrForcedRotationNotReady) {
					backoff, retryAt = 0, time.Time{}
					lastFailure = "rotation owed, waiting for a source block past " + strconv.FormatUint(latest, 10)
					log.Printf("WARN [%s UpdateClient] refresh: client already at %d but its pinned snapshot is unrotated; "+
						"retrying on the next tick (%s of trust left)", m.name, latest, trustLeft)
					m.beats.record(workerRefresh, lastFailure)
					continue
				}
				// The hold now owns the retry cadence: the next tick skips on
				// clientStillHeld and probes, and the refresh runs again as soon as
				// the client is active -- not after a backoff built from reverts.
				if m.armClientHoldFrom(ctx, err) {
					backoff, retryAt = 0, time.Time{}
					m.beats.record(workerRefresh, "held: destination client not active")
					continue
				}
				backoff, retryAt = m.refreshFailed(backoff, trustingPeriod, fmt.Sprintf("update to %d", latest), err)
				lastFailure = "refresh to " + strconv.FormatUint(latest, 10) + " failed: " + err.Error()
				m.beats.record(workerRefresh, lastFailure)
				continue
			}
			backoff, retryAt = 0, time.Time{}
			log.Printf("[%s UpdateClient] refresh: advanced the client to height %d, with %s of trust left when it started",
				m.name, latest, trustLeft)
			m.beats.record(workerRefresh, "refreshed to "+strconv.FormatUint(latest, 10))
		}
	}
}

// refreshFailed spaces out the next attempt and says when it will be. One place
// decides the policy, so the three ways a refresh can fail cannot grow three
// different ones.
func (m *Module) refreshFailed(backoff, trustingPeriod time.Duration, what string, err error) (time.Duration, time.Time) {
	next := nextRefreshBackoff(backoff, trustingPeriod)
	log.Printf("WARN [%s UpdateClient] refresh: %s: %v; next attempt in %s", m.name, what, err, next)
	return next, time.Now().Add(next)
}

// nextRefreshBackoff advances the spacing after a failed refresh: the first
// failure waits one tick, each further failure doubles, capped at
// refreshBackoffMax. current == 0 means "no prior failure".
func nextRefreshBackoff(current, trustingPeriod time.Duration) time.Duration {
	maxBackoff := refreshBackoffMaxFor(trustingPeriod)
	if current <= 0 {
		return min(refreshTick, maxBackoff)
	}
	return min(current*2, maxBackoff)
}

// refreshMarginFor sizes the margin against the client's own trusting period.
// A zero period means the destination could not report one (an L2 client has no
// self-expiry), and the fixed default stands in.
func refreshMarginFor(trustingPeriod time.Duration) time.Duration {
	if trustingPeriod <= 0 {
		return defaultRefreshMargin
	}
	return services.RefreshSafetyMargin(trustingPeriod)
}

// needsRefresh reports whether the destination client is close enough to expiry
// that the anti-expiry routine must advance it now.
//
// A zero expiry means "never expires" (e.g. a permissioned client) and needs no
// refresh. Otherwise the client is refreshed once it is within refreshMargin of
// expiring — note the direction: MORE than a margin of headroom means there is
// nothing to do yet, and that is the comparison worth pinning, because inverting
// it produces a routine that refreshes only while there is plenty of time and
// goes quiet exactly when the client is about to expire.
func needsRefresh(expiresAt, now time.Time, trustingPeriod time.Duration) bool {
	if expiresAt.IsZero() {
		return false
	}
	return expiresAt.Sub(now) <= refreshMarginFor(trustingPeriod)
}

// refreshBackoffMaxFor bounds how far apart two refresh attempts may drift after
// a failure, AGAINST THE MARGIN THIS CLIENT ACTUALLY HAS. The point of the window
// is to still be trying inside it, and a backoff that could span it turns the
// anti-expiry routine into one that watches the client expire.
//
// A fixed five minutes was safe only while the margin was a fixed thirty. Found
// in review by @neitdung: main now derives the margin from the destination's own
// trusting period (refreshMarginFor -> services.RefreshSafetyMargin, a quarter of
// the period), so the margin is BELOW five minutes for any trusting period at or
// under twenty:
//
//	trusting period   margin     fixed 5m cap
//	4h                1h         fits 8 attempts
//	1h                15m        fits 4
//	20m               5m         one gap spans the whole window
//	10m               2.5m       the first backoff already jumps past it
//	2m                30s        the routine never runs inside the window at all
//
// A quarter of the margin keeps at least four attempts inside the window at every
// period, which is the property the test pins rather than any particular number.
func refreshBackoffMaxFor(trustingPeriod time.Duration) time.Duration {
	cap := refreshMarginFor(trustingPeriod) / 4
	if cap > refreshBackoffCeiling {
		return refreshBackoffCeiling
	}
	if cap < refreshBackoffFloor {
		return refreshBackoffFloor
	}
	return cap
}
