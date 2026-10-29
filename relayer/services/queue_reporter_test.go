package services

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// newQueueReporterTestServices builds the same Services shape the other
// reporter tests use: no chain clients, because nothing here reaches one.
func newQueueReporterTestServices(t *testing.T) *Services {
	t.Helper()
	return New(nil, nil, DefaultConfig())
}

// enqueueQueueReporterPacket makes one counter move, which is all these tests
// need from a "change".
func enqueueQueueReporterPacket(t *testing.T, svc *Services) {
	t.Helper()
	svc.BatchBuilder.PendingTracker.Add(timeoutRetryPacket(1), 10)
}

// captureLog redirects the standard logger for the duration of f and returns
// what was written. The reporter logs through the package logger, which is the
// surface this behaviour is about, so the test reads it rather than restructuring
// production code to hand back strings.
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	flags, out := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetFlags(flags); log.SetOutput(out) })
	f()
	return buf.String()
}

func countLines(s, substr string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func TestReportQueueState(t *testing.T) {
	// An idle relayer printed two [QueueState] lines a minute with nothing to say:
	// 748 lines over a six-hour run in which the queue was empty throughout. The
	// routine lines now report a CHANGE, plus a heartbeat so an idle relayer still
	// proves it is running.
	t.Run("reporter is quiet while nothing changes", func(t *testing.T) {
		svc := newQueueReporterTestServices(t)
		now := time.Unix(1_700_000_000, 0)

		out := captureLog(t, func() {
			var quiet uint64
			last := svc.reportQueueState(now, nil, &quiet)
			// One hour of ticks with an empty, unchanging queue.
			for i := 1; i <= 60; i++ {
				last = svc.reportQueueState(now.Add(time.Duration(i)*time.Minute), &last, &quiet)
			}
		})

		// The first call always reports (last == nil): an operator starting the
		// process must see the state it started in.
		// After that: 60 unchanged ticks, one heartbeat per queueReportQuietHeartbeat.
		want := 1 + 60/queueReportQuietHeartbeat
		if got := countLines(out, "[QueueState] queued("); got != want {
			t.Fatalf("queued() lines = %d, want %d (1 initial + %d heartbeats):\n%s",
				got, want, 60/queueReportQuietHeartbeat, out)
		}
		// Before this change the same hour produced 61 of them.
		if got := countLines(out, "[QueueState] queued("); got >= 61 {
			t.Fatalf("the reporter is still printing every tick: %d lines", got)
		}
	})

	// The client-update ages advance every tick on their own. Including them in the
	// comparison is the mistake D6 documents: the key never repeats, so the dedupe
	// dedupes nothing. They must print with the routine line and never drive it.
	t.Run("reporter ages do not defeat the dedupe", func(t *testing.T) {
		svc := newQueueReporterTestServices(t)
		now := time.Unix(1_700_000_000, 0)
		svc.ObserveCosmosOnEVMUpdate(now.Add(-time.Hour))
		svc.ObserveEVMOnCosmosUpdate(now.Add(-2 * time.Hour))

		out := captureLog(t, func() {
			var quiet uint64
			last := svc.reportQueueState(now, nil, &quiet)
			for i := 1; i < queueReportQuietHeartbeat; i++ {
				last = svc.reportQueueState(now.Add(time.Duration(i)*time.Minute), &last, &quiet)
			}
		})

		if got := countLines(out, "last client update:"); got != 1 {
			t.Fatalf("client-update line printed %d times; the ages change every tick, so "+
				"driving output from them means never de-duplicating:\n%s", got, out)
		}
		if !strings.Contains(out, "cosmos-on-evm=1h0m0s ago") {
			t.Fatalf("the age must still be printed on the line it accompanies:\n%s", out)
		}
	})

	// A change must be reported at once. A relayer that goes from idle to busy and
	// says nothing for ten minutes is worse than one that repeats itself.
	t.Run("reporter reports a change immediately", func(t *testing.T) {
		svc := newQueueReporterTestServices(t)
		now := time.Unix(1_700_000_000, 0)

		out := captureLog(t, func() {
			var quiet uint64
			last := svc.reportQueueState(now, nil, &quiet)
			last = svc.reportQueueState(now.Add(time.Minute), &last, &quiet)
			enqueueQueueReporterPacket(t, svc)
			svc.reportQueueState(now.Add(2*time.Minute), &last, &quiet)
		})

		if got := countLines(out, "[QueueState] queued("); got != 2 {
			t.Fatalf("queued() lines = %d, want 2 (the initial one and the change):\n%s", got, out)
		}
		if !strings.Contains(out, "pending(cosmos=1") {
			t.Fatalf("the change itself was not reported:\n%s", out)
		}
	})

	// A quiet stretch that follows a change must restart from zero, not inherit the
	// old count -- otherwise the heartbeat lands at an arbitrary offset.
	t.Run("reporter resets the quiet count on a change", func(t *testing.T) {
		svc := newQueueReporterTestServices(t)
		now := time.Unix(1_700_000_000, 0)

		var quiet uint64
		last := svc.reportQueueState(now, nil, &quiet)
		for i := 1; i < queueReportQuietHeartbeat; i++ {
			last = svc.reportQueueState(now.Add(time.Duration(i)*time.Minute), &last, &quiet)
		}
		enqueueQueueReporterPacket(t, svc)
		last = svc.reportQueueState(now.Add(time.Hour), &last, &quiet)
		if quiet != 0 {
			t.Fatalf("quiet tick count = %d after a change, want 0", quiet)
		}
	})

	// The merge with #415 added the paused-retry flags to queueState. They belong in
	// counts() by this file's own rule -- they are signal, not something that
	// advances on its own -- and leaving them out would make a persistence outage
	// ENDING invisible: the ATTENTION line simply stops firing, and with the routine
	// lines still suppressed the log would go quiet with nothing saying the state
	// directory came back.
	t.Run("reporter treats paused retries as a change", func(t *testing.T) {
		base := queueState{cosmosQueued: 1}

		paused := base
		paused.cosmosRetryPaused = true
		if base.counts() == paused.counts() {
			t.Fatal("pausing timeout retries is invisible to the change check; " +
				"neither the outage starting nor it ending would reprint the routine lines")
		}

		// The other half of the same rule, which the merge must not break: an age
		// advances on its own, so it must NOT count, or the dedupe dedupes nothing.
		aged := base
		aged.cosmosTimeoutDeadAge = time.Hour
		if base.counts() != aged.counts() {
			t.Fatal("an age difference reached counts(); every tick would look like a change")
		}
	})

	// The merge with #469 brought the counts() dedupe onto this branch after
	// ackDueUnwritten already existed, so the new field had never been considered
	// for it. It belongs there by that file's own rule -- a count of records that
	// failed to reach disk is signal, not something that advances on its own.
	//
	// Leaving it out hides the storage problem CLEARING: the ATTENTION line simply
	// stops firing, and with the routine lines still suppressed, because no other
	// count moved, the log goes quiet with nothing saying the writes started
	// landing again.
	t.Run("reporter treats unwritten ack records as a change", func(t *testing.T) {
		base := queueState{cosmosQueued: 1}

		unwritten := base
		unwritten.ackDueUnwritten = 2
		if base.counts() == unwritten.counts() {
			t.Fatal("owed-acknowledgement records that could not be written are invisible to the change check; " +
				"neither the problem starting nor it clearing would reprint the routine lines")
		}

		// The other half of the rule the merge must not break.
		aged := base
		aged.cosmosTimeoutDeadAge = time.Hour
		if base.counts() != aged.counts() {
			t.Fatal("an age difference reached counts(); every tick would look like a change")
		}
	})
}

func TestQueueState_IncludesStuckAndDeadLetterTimeouts(t *testing.T) {
	svc := New(nil, nil, DefaultConfig())
	packet := timeoutRetryPacket(21)
	svc.BatchBuilder.PendingTracker.Add(packet, 10)
	now := time.Now()
	for range deferralAlertThreshold {
		svc.BatchBuilder.PendingTracker.deferTimeoutRetryForTest(packet.SourceClient, packet.Sequence, now)
	}

	dead := timeoutRetryPacket(22)
	svc.BatchBuilder.EthPendingTracker.Add(dead, 10)
	for attempt := 0; attempt <= maxTimeoutAttempts; attempt++ {
		svc.BatchBuilder.EthPendingTracker.recordTimeoutFailureForTest(dead.SourceClient, dead.Sequence, now)
	}

	state := svc.queueStateAt(now)
	if state.cosmosStuck != 1 || state.cosmosWorstDeferrals != deferralAlertThreshold {
		t.Fatalf("stuck state = %d/%d, want 1/%d", state.cosmosStuck, state.cosmosWorstDeferrals, deferralAlertThreshold)
	}
	if state.ethTimeoutDead != 1 {
		t.Fatalf("eth timeout dead letters = %d, want 1", state.ethTimeoutDead)
	}
}

func TestClientUpdateAges_UseTrustedChainTimestamps(t *testing.T) {
	svc := New(nil, nil, DefaultConfig())
	now := time.Now()
	if _, _, ok := svc.clientUpdateAges(now); ok {
		t.Fatal("client update ages reported before either direction was observed")
	}

	svc.ObserveCosmosOnEVMUpdate(now.Add(-2 * time.Minute))
	svc.ObserveEVMOnCosmosUpdate(now.Add(-3 * time.Minute))
	cosmosAge, evmAge, ok := svc.clientUpdateAges(now)
	if !ok || cosmosAge != 2*time.Minute || evmAge != 3*time.Minute {
		t.Fatalf("client update ages = (%s, %s, %v), want (2m, 3m, true)", cosmosAge, evmAge, ok)
	}

	// An older observation must not make the reported client state regress.
	svc.ObserveCosmosOnEVMUpdate(now.Add(-10 * time.Minute))
	cosmosAge, _, _ = svc.clientUpdateAges(now)
	if cosmosAge != 2*time.Minute {
		t.Fatalf("older observation regressed cosmos-on-evm age to %s", cosmosAge)
	}
}
