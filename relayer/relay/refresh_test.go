package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"relayer/chain"
	"relayer/services"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A failing expiry query must keep sizing its retry against THIS client's margin.
//
// Reported by @DongLieu on #487, against the commit that fixed the same class of
// bug one line away. The failure path passed a zero trusting period, which falls
// back to the default margin, whose cap is the five-minute ceiling. On a
// ten-minute trusting period the real margin is 2m30s, so a run of query failures
// could schedule a retry spanning twice the window the loop exists to work
// inside -- and a query that keeps failing is exactly the case where no later
// pass arrives to narrow it.
func TestRefreshBackoffMaxFor_KeepsTheLastKnownTrustingPeriodWhenTheQueryFails(t *testing.T) {
	const trustingPeriod = 10 * time.Minute

	// The rule. A failing query must not fall back to the default margin's cap,
	// because that cap is the larger one for every short trusting period.
	t.Run("the default cap is the wrong size for a short period", func(t *testing.T) {
		margin := refreshMarginFor(trustingPeriod)
		if refreshBackoffMaxFor(0) < margin {
			t.Fatal("the default cap already fits inside this margin; there would be nothing to fix")
		}
		if refreshBackoffMaxFor(trustingPeriod) >= margin {
			t.Fatalf("this client's own cap %s does not fit inside its %s margin",
				refreshBackoffMaxFor(trustingPeriod), margin)
		}
	})

	// The wiring, asserted structurally. Driving the loop to saturation is not
	// practical here: the backoff starts at one tick and doubles, so reaching a
	// 37s cap takes about fifteen failures and the loop SLEEPS each backoff in
	// between -- minutes of wall clock for one assertion. The early spacings are
	// identical whichever period is passed, so a log-reading test passes against
	// the bug; I tried that first and the mutation walked through it.
	//
	// So the assertion is on the call site, the same technique services uses for
	// fsync: the property is invisible from outside at test speed, and a test that
	// cannot see it should say which half it is pinning.
	t.Run("the failure path passes the remembered period, not zero", func(t *testing.T) {
		src, err := os.ReadFile("refresh.go")
		if err != nil {
			t.Fatalf("read refresh.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "func (m *Module) refreshLoop(")
		if start < 0 {
			t.Fatal("refreshLoop not found")
		}
		end := strings.Index(body[start:], "\nfunc ")
		if end < 0 {
			end = len(body) - start
		}
		fn := body[start : start+end]

		if !strings.Contains(fn, "lastTrustingPeriod = trustingPeriod") {
			t.Error("refreshLoop does not remember the period it read, so a failing query has nothing to size with")
		}
		if !strings.Contains(fn, `refreshFailed(backoff, lastTrustingPeriod, "query client expiry"`) {
			t.Error("the expiry-query failure path does not size its retry with the remembered trusting period; " +
				"falling back to the default lets the backoff span the refresh window on a short-lived client")
		}
	})
}

// The constraint C4 states outright, checked at EVERY trusting period rather than
// at the one the fixed margin used to be.
//
// Found in review by @neitdung: the cap was a fixed five minutes, written when
// the margin was a fixed thirty. main derives the margin from the destination's
// own trusting period, so at twenty minutes or less the margin is five minutes or
// less and one backoff step could span the whole window -- the anti-expiry
// routine sitting out exactly the period it exists for. A fixed cap cannot be
// made safe by choosing a smaller number, because the margin has no lower bound
// that a constant can stay under; the cap has to be derived from the same input.
func TestRefreshBackoffMaxFor_StaysInsideTheWindowAtEveryTrustingPeriod(t *testing.T) {
	for _, trustingPeriod := range []time.Duration{
		0, // the destination reports none: the default margin stands in
		30 * time.Second, 2 * time.Minute, 4 * time.Minute, 10 * time.Minute,
		20 * time.Minute, time.Hour, 4 * time.Hour, 14 * 24 * time.Hour,
	} {
		t.Run(trustingPeriod.String(), func(t *testing.T) {
			margin := refreshMarginFor(trustingPeriod)
			cap := refreshBackoffMaxFor(trustingPeriod)
			if cap >= margin {
				t.Fatalf("backoff cap %s is not below the margin %s; one gap could span the window the loop exists to work inside",
					cap, margin)
			}

			attempts := 0
			for elapsed, backoff := time.Duration(0), time.Duration(0); elapsed < margin; attempts++ {
				backoff = nextRefreshBackoff(backoff, trustingPeriod)
				elapsed += backoff
			}
			if attempts < 4 {
				t.Fatalf("only %d attempts fit in the %s window (cap %s); the cap is too high to be an emergency routine",
					attempts, margin, cap)
			}
		})
	}
}

// C4 asks for a decision, not a default: the retry spacing after a failed
// refresh must be stated and bounded. The bound is what makes backing off safe
// here -- the loop exists to act inside the refresh margin, so a backoff that can
// grow past it trades a gas leak for an expired client.
func TestNextRefreshBackoff(t *testing.T) {
	// A long trusting period, so the margin is the default hour and the ceiling
	// is what bounds the backoff. The short-period case is the test below.
	const longPeriod = 24 * time.Hour
	ceiling := refreshBackoffMaxFor(longPeriod)

	t.Run("the first failure waits one tick, then doubles", func(t *testing.T) {
		first := nextRefreshBackoff(0, longPeriod)
		if first != refreshTick {
			t.Fatalf("first backoff = %s, want one tick (%s)", first, refreshTick)
		}
		if second := nextRefreshBackoff(first, longPeriod); second != 2*first {
			t.Fatalf("second backoff = %s, want %s: a retry that waits the same as the last one is not spacing out",
				second, 2*first)
		}
	})

	t.Run("it stops at the cap", func(t *testing.T) {
		if got := nextRefreshBackoff(ceiling, longPeriod); got != ceiling {
			t.Fatalf("backoff grew past the cap to %s, want %s", got, ceiling)
		}
		if got := nextRefreshBackoff(ceiling*4, longPeriod); got != ceiling {
			t.Fatalf("backoff = %s from above the cap, want it clamped to %s", got, ceiling)
		}
	})
}

// refreshLoop is the anti-expiry routine: it acts only on the decision
// needsRefresh makes, and must keep running through a failed query.
//
// No t.Parallel here, deliberately. withFastRefreshTick reassigns a
// package-level var and restores it with t.Cleanup; running these subtests in
// parallel would turn that sequence into a race that -race catches only
// sometimes.
func TestRefreshLoop(t *testing.T) {
	// The decision above is only useful if the loop acts on it. This covers the wiring:
	// a client inside the margin gets an update submitted without any packet traffic.
	t.Run("advances a client near expiry", func(t *testing.T) {
		withFastRefreshTick(t)

		src := &mockSource{latest: 42}
		dst := &mockDest{expiresAt: time.Now().Add(time.Minute)} // well inside the default margin
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.refreshLoop(ctx) }()

		waitFor(t, ctx, func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.lastHeight == 42
		}, "refresh loop never advanced the client to the source's latest height")
		cancel()
		wg.Wait()
	})

	// A client with plenty of headroom must be left alone: refreshing it early spends a
	// proof and a transaction for nothing, on a cadence of once a minute.
	t.Run("leaves a healthy client alone", func(t *testing.T) {
		withFastRefreshTick(t)

		src := &mockSource{latest: 42}
		dst := &mockDest{expiresAt: time.Now().Add(defaultRefreshMargin + time.Hour)}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.refreshLoop(ctx) }()
		time.Sleep(50 * time.Millisecond) // many ticks at 1ms
		cancel()
		wg.Wait()

		if len(dst.updates) != 0 {
			t.Fatalf("refreshed a client that was nowhere near expiry: %+v", dst.updates)
		}
	})

	// A failed expiry query must not be read as "no expiry". The loop logs and retries
	// on the next tick; it must not advance any state, and it must not exit — a refresh
	// loop that dies on one bad RPC leaves the client to expire in silence.
	t.Run("survives a failed expiry query", func(t *testing.T) {
		withFastRefreshTick(t)

		src := &mockSource{latest: 42}
		dst := &mockDest{expiresErr: errors.New("rpc down")}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		done := make(chan struct{})
		go func() { defer wg.Done(); m.refreshLoop(ctx); close(done) }()

		time.Sleep(50 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("refresh loop exited on a failed expiry query; the client is now unattended")
		default:
		}
		if len(dst.updates) != 0 {
			t.Fatalf("submitted an update off a failed expiry query: %+v", dst.updates)
		}
		m.mu.Lock()
		last := m.lastHeight
		m.mu.Unlock()
		if last != 0 {
			t.Fatalf("lastHeight advanced to %d off a failed expiry query", last)
		}
		cancel()
		wg.Wait()
	})

	// The wiring half: the loop must actually space its attempts out. The pure
	// function above can be perfect while the loop calls it and ignores the answer.
	t.Run("backs off when refresh keeps failing", func(t *testing.T) {
		withFastRefreshTick(t) // 1ms, so the backoff doubles in milliseconds

		src := &mockSource{latest: 42}
		dst := &countingExpiryDest{expiresErr: errors.New("rpc down")}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.refreshLoop(ctx) }()

		const window = 300 * time.Millisecond
		time.Sleep(window)
		cancel()
		wg.Wait()

		attempts := dst.calls()
		// Unspaced, a 1ms tick would try ~300 times in this window. Backing off 1, 2,
		// 4, 8 ... reaches 300ms in nine attempts. The bound is loose because the
		// scheduler is not, but it cannot be met by a loop that ignores the backoff.
		if attempts > 30 {
			t.Fatalf("%d attempts in %s: the loop is retrying on every tick, not backing off", attempts, window)
		}
		if attempts < 2 {
			t.Fatalf("%d attempts in %s: the loop stopped retrying altogether", attempts, window)
		}
	})

	// Backing off must not wedge the loop: once the cause clears, the client still
	// gets advanced. A backoff that outlived its failure would be the expired-client
	// outcome the cap exists to prevent, arriving by another route.
	t.Run("recovers after the failure clears", func(t *testing.T) {
		withFastRefreshTick(t)

		src := &mockSource{latest: 42}
		dst := &countingExpiryDest{expiresErr: errors.New("rpc down")}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.refreshLoop(ctx) }()

		waitFor(t, ctx, func() bool { return dst.calls() >= 3 }, "the loop never made its first few attempts")
		dst.heal(time.Now().Add(time.Minute)) // inside refreshMargin, so a refresh is due

		waitFor(t, ctx, func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.lastHeight == 42
		}, "the loop never advanced the client after the failure cleared; the backoff outlived its cause")
		cancel()
		wg.Wait()
	})

	// A backoff that survives its own cause is a slow leak: a client that failed a
	// dozen times last week and recovered would wait the full cap before its next
	// isolated blip is retried, inside a window that exists to keep trying.
	t.Run("backoff does not outlive its cause", func(t *testing.T) {
		withFastRefreshTick(t)

		src := &mockSource{latest: 42}
		dst := &countingExpiryDest{expiresErr: errors.New("rpc down")}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.refreshLoop(ctx) }()

		// Ten failures at a 1ms tick take the backoff to roughly half a second, which
		// is what makes the difference below measurable rather than theoretical.
		waitFor(t, ctx, func() bool { return dst.calls() >= 10 }, "the loop never accumulated a backoff to clear")
		dst.heal(time.Now().Add(time.Minute))
		waitFor(t, ctx, func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.lastHeight == 42
		}, "the loop never recovered")

		// Break it again. With the backoff cleared by the success, the next attempts
		// are one tick apart; carried over, the first would be half a second away.
		before := dst.calls()
		dst.fail(errors.New("rpc down again"))
		deadline, ok := context.WithTimeout(ctx, 300*time.Millisecond)
		defer ok()
		promptly := true
		waitForSoft(deadline, func() bool { return dst.calls() >= before+3 }, &promptly)
		if !promptly {
			t.Fatalf("only %d attempts in 300ms after a fresh failure; the backoff was carried over from the outage that already ended",
				dst.calls()-before)
		}
		cancel()
		wg.Wait()
	})

	// D5: three states, and two of them used to be silence. An operator reading the
	// log could not tell "the client has hours left" from "it has minutes left and
	// the last refresh worked".
	t.Run("says what it is doing", func(t *testing.T) {
		withFastRefreshTick(t)

		var logs bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(oldOutput) })

		src := &mockSource{latest: 42}
		dst := &mockDest{expiresAt: time.Now().Add(time.Minute)} // inside refreshMargin
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.refreshLoop(ctx) }()
		waitFor(t, ctx, func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.lastHeight == 42
		}, "the refresh loop never advanced the client")
		cancel()
		wg.Wait()

		out := logs.String()
		if !strings.Contains(out, "client expires in") {
			t.Errorf("no line saying a refresh was due, so a due refresh is indistinguishable from a healthy client:\n%s", out)
		}
		if !strings.Contains(out, "advanced the client to height 42") {
			t.Errorf("no line saying the refresh succeeded, so success is indistinguishable from failure:\n%s", out)
		}
		if !strings.Contains(out, "of trust left") {
			t.Errorf("the lines carry no remaining lifetime, which is the number an operator is reading for:\n%s", out)
		}
	})

	// A pass that FAILED still counts. heartbeats.record says so outright, and the
	// refresh loop is where it matters most: it is the anti-expiry routine, so the
	// moment an operator needs to know whether it is still turning is exactly the
	// moment its RPC is unreachable.
	//
	// Reported by @DongLieu: the beat sat after the expiry query succeeded, so
	// against a dead endpoint every tick took the error path and no beat was ever
	// recorded. `relayer status` then showed the loop as never started -- wedged and
	// merely failing look identical again, which is the confusion this type removes.
	t.Run("beats on every ending", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			dst  *mockDest
			want string
		}{
			{
				name: "the expiry query fails",
				dst:  &mockDest{expiresErr: errors.New("connection refused")},
				want: "query client expiry failed",
			},
			{
				name: "healthy, nothing to do",
				dst:  &mockDest{expiresAt: time.Now().Add(refreshMarginFor(0) + time.Hour)},
				want: "client expires in",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				withFastRefreshTick(t)

				m := NewModule("test", "client-0", &mockSource{latest: 42}, tc.dst, &mockBuilder{})
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()

				done := make(chan struct{})
				go func() { defer close(done); m.refreshLoop(ctx) }()

				deadline := time.After(3 * time.Second)
				for {
					if note, ok := refreshBeatNote(m); ok {
						if !strings.Contains(note, tc.want) {
							t.Fatalf("refresh note = %q, want it to contain %q", note, tc.want)
						}
						cancel()
						<-done
						return
					}
					select {
					case <-deadline:
						cancel()
						<-done
						t.Fatalf("the refresh loop ran and recorded no heartbeat; status would report it as never started")
					case <-time.After(time.Millisecond):
					}
				}
			})
		}
	})
}

// --- the optional capabilities ---
//
// The With* options are how a path declares what it needs beyond the base relay
// loop. They were all at 0%: nothing pinned that an option actually installs what
// it names, and an option that silently does nothing is invisible -- the module
// runs, relays packets, and quietly never scans for timeouts.

// TestRefreshMarginFor pins the rule itself: the margin is a fraction of the
// client's own trusting period, and it is the SAME rule the refresh-interval
// calculation uses. Two rules for one concept is how the fixed 30 minutes
// survived.
func TestRefreshMarginFor(t *testing.T) {
	for _, tt := range []struct {
		name   string
		period time.Duration
		want   time.Duration
	}{
		{"unknown period falls back", 0, defaultRefreshMargin},
		{"negative period falls back", -time.Hour, defaultRefreshMargin},
		{"a short period gets a quarter of itself", 10 * time.Minute, 150 * time.Second},
		{"a long period is capped at an hour", 14 * 24 * time.Hour, time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshMarginFor(tt.period); got != tt.want {
				t.Fatalf("refreshMarginFor(%s) = %s, want %s", tt.period, got, tt.want)
			}
		})
	}
}

// needsRefresh decides whether the anti-expiry routine acts. The direction of the
// comparison is the whole point: plenty of headroom means do nothing, little
// headroom means refresh now. Inverted, the routine refreshes constantly while the
// client is healthy and falls silent exactly as it approaches expiry — and an
// expired light client stops every relay in that direction.
//
// The boundary cases are listed explicitly because a > / >= slip is invisible in
// the middle of the range.
func TestNeedsRefresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// With no reported trusting period the module keeps its fixed default. This
	// is the L2 case: no self-expiry, so nothing to take a fraction of.
	t.Run("falls back to the default margin when no period is reported", func(t *testing.T) {
		margin := defaultRefreshMargin
		for _, tt := range []struct {
			name      string
			expiresAt time.Time
			want      bool
		}{
			{"no expiry at all", time.Time{}, false},
			{"expiry far beyond the margin", now.Add(margin + time.Hour), false},
			{"one second more headroom than the margin", now.Add(margin + time.Second), false},
			{"exactly the margin", now.Add(margin), true},
			{"one second inside the margin", now.Add(margin - time.Second), true},
			{"about to expire", now.Add(time.Minute), true},
			{"already expired", now.Add(-time.Hour), true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				if got := needsRefresh(tt.expiresAt, now, 0); got != tt.want {
					t.Fatalf("needsRefresh = %v, want %v (headroom %s, margin %s)",
						got, tt.want, tt.expiresAt.Sub(now), margin)
				}
			})
		}
	})

	// The defect this replaces: a margin fixed at 30 minutes against a trusting
	// period SHORTER than 30 minutes makes the condition always true, so the
	// routine refreshes on every tick forever instead of when the client needs it.
	t.Run("a short trusting period does not make every tick a refresh", func(t *testing.T) {
		const period = 10 * time.Minute
		// Eight minutes of headroom out of a ten-minute period is plenty; under the
		// old fixed 30-minute margin this reported "refresh now".
		if needsRefresh(now.Add(8*time.Minute), now, period) {
			t.Fatalf("refreshed with 8m of headroom on a %s period; the margin must scale with the period", period)
		}
		// One minute of headroom out of ten is not.
		if !needsRefresh(now.Add(time.Minute), now, period) {
			t.Fatalf("did not refresh with 1m of headroom on a %s period", period)
		}
	})

	// And the other direction: a period measured in days must not be refreshed
	// only in its last half hour.
	t.Run("a long trusting period gets a proportionally larger margin", func(t *testing.T) {
		const period = 14 * 24 * time.Hour
		if got := refreshMarginFor(period); got <= defaultRefreshMargin {
			t.Fatalf("margin for a %s period = %s, want more than the %s default", period, got, defaultRefreshMargin)
		}
	})
}

// rotatingBuilder is a mockBuilder that also offers the forced build, and
// counts each kind separately so a test can tell which one the engine chose.
type rotatingBuilder struct {
	mockBuilder
	rotations int
}

func (b *rotatingBuilder) BuildRotation(ctx context.Context, header []byte) (chain.ClientUpdate, error) {
	b.rotations++
	b.built-- // counted as a rotation, not a plain build
	return b.mockBuilder.Build(ctx, header)
}

// The expiry-driven refresh must ask for a rotation. On SpectreClient the expiry
// is the pinned snapshot's, and only updateConsensusState moves it: a refresh
// built as a plain update lands as updateApplicationState, succeeds, and leaves
// the client expiring on schedule (#541). Packet-driven updates must NOT pay for
// that -- a rotation per batch is the heavy path on every packet.
func TestRefreshClientTo_ForcesRotation(t *testing.T) {
	t.Run("refresh builds a rotation", func(t *testing.T) {
		b := &rotatingBuilder{}
		dst := &mockDest{}
		m := NewModule("test", "client-0", &mockSource{latest: 50}, dst, b)

		if err := m.refreshClientTo(context.Background(), 50); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if b.rotations != 1 || b.built != 0 {
			t.Fatalf("refresh must build one rotation and no plain update, got rotations=%d built=%d", b.rotations, b.built)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("the rotation must be submitted, got %d updates", len(dst.updates))
		}
	})

	t.Run("a packet-driven update stays on Build", func(t *testing.T) {
		b := &rotatingBuilder{}
		m := NewModule("test", "client-0", &mockSource{latest: 50}, &mockDest{}, b)

		if err := m.updateClientTo(context.Background(), 50); err != nil {
			t.Fatalf("update: %v", err)
		}
		if b.rotations != 0 || b.built != 1 {
			t.Fatalf("updateClientTo must not force a rotation, got rotations=%d built=%d", b.rotations, b.built)
		}
	})

	t.Run("every hop of a refresh climb is a rotation", func(t *testing.T) {
		b := &rotatingBuilder{mockBuilder: mockBuilder{hops: []uint64{20, 35, 50}}}
		m := NewModule("test", "client-0", &mockSource{latest: 50}, &mockDest{}, b)

		if err := m.refreshClientTo(context.Background(), 50); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if b.rotations != 3 || b.built != 0 {
			t.Fatalf("each hop must be built as a rotation, got rotations=%d built=%d", b.rotations, b.built)
		}
	})

	// Beacon and attested-L2 builders have no snapshot to rotate; their plain
	// update already moves the expiry, so the refresh uses it.
	t.Run("a builder with nothing to rotate falls back to Build", func(t *testing.T) {
		b := &mockBuilder{}
		dst := &mockDest{}
		m := NewModule("test", "client-0", &mockSource{latest: 50}, dst, b)

		if err := m.refreshClientTo(context.Background(), 50); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if b.built != 1 || len(dst.updates) != 1 {
			t.Fatalf("want one plain build submitted, got built=%d updates=%d", b.built, len(dst.updates))
		}
	})

	// Pinned on the call site, as above: driving refreshLoop to a due refresh
	// needs a destination near expiry and a ticker, and the property is which
	// method the loop calls.
	t.Run("refreshLoop advances through refreshClientTo", func(t *testing.T) {
		src, err := os.ReadFile("refresh.go")
		if err != nil {
			t.Fatalf("read refresh.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "func (m *Module) refreshLoop(")
		if start < 0 {
			t.Fatal("refreshLoop not found")
		}
		end := strings.Index(body[start:], "\nfunc ")
		if end < 0 {
			end = len(body) - start
		}
		fn := body[start : start+end]
		if !strings.Contains(fn, "m.refreshClientTo(ctx, latest)") {
			t.Error("refreshLoop does not advance the client through refreshClientTo")
		}
		if strings.Contains(fn, "m.updateClientTo(") {
			t.Error("refreshLoop calls updateClientTo, whose update never rotates the pinned set")
		}
	})
}

// Review of 2e26071: a packet update can bring the client to the source head
// without rotating the pinned snapshot. The forced refresh used to stop at
// "height already covered" and return nil before BuildRotation was ever asked,
// so the anti-expiry loop reported "refreshed" with the deadline unchanged.
func TestRefreshClientTo_RotationOwedAtCoveredHeight(t *testing.T) {
	t.Run("a covered height still asks for the rotation, and reports it owed", func(t *testing.T) {
		b := &rotatingBuilder{}
		dst := &mockDest{}
		m := NewModule("test", "client-0", &mockSource{latest: 50}, dst, b)

		if err := m.updateClientTo(context.Background(), 50); err != nil {
			t.Fatalf("packet update: %v", err)
		}
		err := m.refreshClientTo(context.Background(), 50)
		if !errors.Is(err, services.ErrForcedRotationNotReady) {
			t.Fatalf("refresh at a covered height = %v, want ErrForcedRotationNotReady", err)
		}
		if b.rotations != 1 {
			t.Fatalf("refresh must still ask for the rotation, got %d BuildRotation calls", b.rotations)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("only the packet update may be submitted, a rotation that installs nothing must not be; got %d updates", len(dst.updates))
		}
	})

	// The real Groth16 builder says it itself when the source has not moved
	// (services.ErrForcedRotationNotReady); that must reach the loop intact.
	t.Run("the builder's own not-ready error reaches the caller", func(t *testing.T) {
		b := &ownedRotationBuilder{notReady: 1}
		m := NewModule("test", "client-0", &mockSource{latest: 50}, &mockDest{}, b)

		if err := m.refreshClientTo(context.Background(), 50); !errors.Is(err, services.ErrForcedRotationNotReady) {
			t.Fatalf("got %v, want ErrForcedRotationNotReady", err)
		}
	})

	// The short-circuit is kept where nothing is owed: a packet update, and a
	// builder with no snapshot to rotate.
	t.Run("a covered height is still a no-op without a rotation owed", func(t *testing.T) {
		for name, b := range map[string]chain.Builder{"packet update on a rotating builder": &rotatingBuilder{}, "plain builder": &mockBuilder{}} {
			m := NewModule("test", "client-0", &mockSource{latest: 50}, &mockDest{}, b)
			m.lastHeight = 50
			var err error
			if name == "plain builder" {
				err = m.refreshClientTo(context.Background(), 50)
			} else {
				err = m.updateClientTo(context.Background(), 50)
			}
			if err != nil {
				t.Fatalf("%s: covered height = %v, want nil", name, err)
			}
		}
	})
}

// ownedRotationBuilder answers BuildRotation the way the Groth16 pipeline does
// while the source is still at the client's height: ErrForcedRotationNotReady,
// notReady times, then a real rotation one block on. Counted atomically, since
// the loop test reads it while refreshLoop runs.
type ownedRotationBuilder struct {
	notReady  int32
	rotations atomic.Int32
}

func (b *ownedRotationBuilder) Name() string { return "owed-rotation" }

func (b *ownedRotationBuilder) Build(context.Context, []byte) (chain.ClientUpdate, error) {
	return chain.ClientUpdate{}, errors.New("refresh must not take the plain build")
}

func (b *ownedRotationBuilder) BuildRotation(_ context.Context, header []byte) (chain.ClientUpdate, error) {
	if b.rotations.Add(1) <= b.notReady {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: build cosmos update: %w", services.ErrForcedRotationNotReady)
	}
	return chain.ClientUpdate{Height: uint64(header[0]) + 1, Payloads: [][]byte{header}}, nil
}

// The loop half: an owed rotation is neither a success nor a failure. It must
// not log "refreshed", must not grow a failure backoff (nothing failed), and
// must keep retrying until a block lets the rotation land.
func TestRefreshLoop_RetriesAnOwedRotation(t *testing.T) {
	withFastRefreshTick(t)
	b := &ownedRotationBuilder{notReady: 3}
	dst := &mockDest{expiresAt: time.Now(), trustingPeriod: time.Hour} // inside the margin: due
	m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 50}, dst, b)

	var logs bytes.Buffer
	var logMu sync.Mutex
	prevOut := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logs.Write(p) }))
	t.Cleanup(func() { log.SetOutput(prevOut) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); m.refreshLoop(ctx) }()

	waitFor(t, ctx, func() bool { return b.rotations.Load() > 3 }, "refresh never retried the owed rotation")
	cancel()
	<-done

	if len(dst.updates) == 0 {
		t.Fatal("the rotation must land once the source moves")
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	head, _, _ := strings.Cut(out, "advanced the client") // what the loop said while the rotation was owed
	if !strings.Contains(head, "pinned snapshot is unrotated") {
		t.Errorf("the owed rotation must be logged as such, got:\n%s", head)
	}
	if strings.Contains(head, "next attempt in") {
		t.Errorf("an owed rotation is not a failure and must not be backed off, got:\n%s", head)
	}
}

// writerFunc adapts a function to io.Writer, so a test can capture log output
// under its own lock while the loop writes from another goroutine.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
