package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"relayer/workers"
	workergroup "relayer/workers"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The path travels as a field, standard-library lines included. Two things would
// break that: never routing the standard logger into the scoped logger, and
// routing it while the old Lmsgprefix prefix and LstdFlags timestamp stayed set,
// which would put both inside every message a second time -- the timestamp zap
// already writes, and a path the field already carries.
func TestStampRelayPath_CarriesThePathOnStdlibLines(t *testing.T) {
	origFlags, origPrefix, origOut := log.Flags(), log.Prefix(), log.Writer()
	t.Cleanup(func() {
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
		log.SetOutput(origOut)
	})

	core, logs := observer.New(zapcore.InfoLevel)
	stampRelayPath(zap.New(core), "cosmos<->eth/cosmoshub-1")
	log.Printf("[Subscribe] send_packet received: seq=%d", 7)

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: the standard logger is not routed into the scoped one", len(entries))
	}
	if got, want := entries[0].Message, "[Subscribe] send_packet received: seq=7"; got != want {
		t.Fatalf("message = %q, want %q: the text must arrive alone, without a timestamp or "+
			"path prefix of the standard logger's own", got, want)
	}
	got, ok := entries[0].ContextMap()[relayPathLogField]
	if !ok {
		t.Fatalf("the line carries no %q field: %+v; a merged log cannot attribute it to a relay path",
			relayPathLogField, entries[0].ContextMap())
	}
	if got != "cosmos<->eth/cosmoshub-1" {
		t.Fatalf("%s = %v, want the relay path", relayPathLogField, got)
	}
}

// The other half of the same call: the injected zap logger must carry the path
// too. Found in review -- the first version stamped only log.Default(), so every
// lifecycle line Start emits through zap ("Relayer started", "shutdown
// requested", "clients stopped") stayed unattributable in a merged log, and the
// test then in place only looked at log.Prefix() so it could not see that.
func TestStampRelayPath_ScopesTheZapLoggerToo(t *testing.T) {
	origFlags, origPrefix, origOut := log.Flags(), log.Prefix(), log.Writer()
	t.Cleanup(func() {
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
		log.SetOutput(origOut)
	})
	log.SetOutput(&bytes.Buffer{})

	core, logs := observer.New(zapcore.InfoLevel)
	scoped := stampRelayPath(zap.New(core), "cosmos<->eth/cosmoshub-1")
	scoped.Info("Relayer started")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got, ok := entries[0].ContextMap()[relayPathLogField]
	if !ok {
		t.Fatalf("the zap line carries no %q field: %+v; a merged log cannot attribute it "+
			"to a relay path", relayPathLogField, entries[0].ContextMap())
	}
	if got != "cosmos<->eth/cosmoshub-1" {
		t.Fatalf("%s = %v, want the relay path", relayPathLogField, got)
	}
}

// stringLiterals returns the contents of each double-quoted literal on a line,
// so a check meant for format strings cannot read the code around them.
//
// Escapes are honoured only far enough to keep \" from ending a literal early;
// this reads Go source for one narrow property, not to compile it.
func stringLiterals(line string) []string {
	var out []string
	inside, escaped := false, false
	start := 0
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inside:
			escaped = true
		case r == '"':
			if inside {
				out = append(out, line[start:i])
			} else {
				start = i + 1
			}
			inside = !inside
		}
	}
	return out
}

// Two label families are the target: [<direction> <Work>] and the closed
// lowercase list. Neither can contain "][", which is how a caller fakes a second
// tier when the function it calls is missing a parameter (relayer/subscriber
// passed "SubscribeCosmos][recovery" as a label). This fails on the forgery, not
// on the confusing log line it produces months later.
//
// [ATTENTION] is exempt, and is the one exemption. It is not a forged label: it
// is a fixed severity tier this repository already uses in 12 files, arriving
// over #377, #395, #415 and #457, and it is written the way this test asks for --
// the label is a parameter, the tier a literal after it. Forbidding it would mean
// rewriting a settled convention to satisfy a gate aimed at something else.
func TestNoLogLabelForgesASecondTier(t *testing.T) {
	root := ".."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "bindings" || name == "third_party" || name == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(line, "][") || strings.HasPrefix(trimmed, "//") {
				// A comment describing a past forgery is not one.
				continue
			}
			// "][" is ordinary Go outside a string literal (`map[k][]byte`,
			// `x[i][j]`). Only a quoted occurrence is a forged label, so the
			// literals are extracted and nothing else on the line is read.
			//
			// Splitting on the first quote and scanning the remainder is not the
			// same thing, and the difference is a false positive: on
			//   dst.UpdateClient(ctx, "client-0", chain.ClientUpdate{Payloads: [][]byte{{1}}})
			// the [][]byte sits after a string literal and was reported as a
			// forged label.
			for _, literal := range stringLiterals(line) {
				// Drop the one allowed tier before looking, so "[%s][ATTENTION][x]"
				// is still caught: only the exemption itself is removed.
				if rest := strings.ReplaceAll(literal, "][ATTENTION]", "]"); strings.Contains(rest, "][") {
					offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, trimmed))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("log labels forging a second tier:\n%s\n\nA label that needs a second level needs a "+
			"parameter, not a closing bracket in the middle of the format string.", strings.Join(offenders, "\n"))
	}
}

// The two noise words are gone and must stay gone: the direction is already the
// label (or the process prefix), so "relay" and "adapter" said nothing.
func TestNoLogLabelRepeatsTheLayerName(t *testing.T) {
	for _, banned := range []string{`"[relay `, `"[adapter `, `"\n[relay `, `"\n[adapter `} {
		out, err := exec.Command("grep", "-rl", "--include=*.go", banned, "..").Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			t.Errorf("label %q is back in:\n%s", strings.TrimSpace(banned), out)
		}
	}
}

// The point of E3 is one concept, one name. Four names for the timeout scanner
// and three for the Ethereum sender is not a formatting problem: an operator
// grepping [SendEthTx] silently missed a third of the sender's own lines.
//
// This pins the closed set. Adding a label is deliberate and cheap -- add it
// here too. Re-inventing a name for something already named is what this stops.
func TestLogLabelsStayOnTheAgreedList(t *testing.T) {
	allowed := map[string]bool{
		// [<direction> <Work>] -- direction is literal where the code knows it,
		// and "%s" where it comes from the module's own name.
		"eth->cosmos Subscribe": true, "l2->cosmos Subscribe": true,
		"cosmos->eth Relay": true, "cosmos->l2 Relay": true, "l2->cosmos Relay": true,
		"cosmos->eth UpdateClient": true, "cosmos->l2 UpdateClient": true,
		"eth->cosmos UpdateClient": true,
		"%s Relay":                 true, "%s UpdateClient": true,

		// [<Work>] -- the emitting code serves more than one direction, so the
		// chain stays in the name: it is the only thing left that separates them.
		"SubscribeCosmos": true, "SendEthTx": true, "SendCosmosTx": true,
		"CreateEthClient": true, "CreateCosmosClient": true,
		"UpdateCosmosClient": true, "UpdateEthClient": true, "RefreshCosmosClient": true,
		"CosmosTimeoutScan": true, "%sTimeoutScan": true,
		"Misbehaviour": true, "PendingTracker": true,
		// Added on purpose, which is what the message below asks for. #452's
		// enumeration backstop and #454's owed-ack ledger both landed after the
		// E3 renaming pass ran, so their labels were never put through it:
		// FlushCosmos is chain-qualified for the same reason SubscribeCosmos is
		// (cosmos.NewSource serves both cosmos->eth and cosmos->l2), and AckWatch
		// replaces a bare [adapter], which is one of the two noise words E3
		// removed.
		"FlushCosmos": true, "AckWatch": true,
		// The enumeration backstop's EVM halves, one per router this process reads:
		// [eth->cosmos Flush] for the L1 source, [l2->cosmos Flush] for an L2 source, each
		// built for exactly one direction; FlushCosmos serves both and stays chain-qualified.
		"eth->cosmos Flush": true, "l2->cosmos Flush": true, "l2->cosmos %s": true,

		// The closed lowercase list: shared by every path, so a direction would
		// be a lie.
		// `create-clients` sits here for the same reason as `start`: it names a CLI
		// command that serves both directions, so a direction in the label would be
		// a lie.
		"bench": true, "prover": true, "BatchBuilder": true, "QueueState": true, "start": true,
		// The status endpoint serves the whole process, so a direction would be a
		// lie -- the same reason start and create-clients sit here.
		"status":         true,
		"create-clients": true,

		// Deleted by #417 along with the unbound-header fallback it belongs to;
		// touching it here would only conflict with that PR.
		"%s": true,
	}

	// A level token (logbridge) may stand in front of the label, and the label is
	// still what the line is about. Without the optional group every line that gains
	// a token would silently drop out of this check, which would stay green while
	// covering less.
	labelRe := regexp.MustCompile(`\.(?:Printf|Println|Fatalf|Infof|Warnf|Errorf|Debugf)\("(?:(?:DEBUG|WARN|ERROR) )?(?:\\n)?\[([^\]"]+)\]`)
	var offenders []string
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == "bindings" || n == "third_party" || n == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, m := range labelRe.FindAllStringSubmatch(line, -1) {
				if !allowed[m[1]] {
					offenders = append(offenders, fmt.Sprintf("%s:%d: [%s]", path, i+1, m[1]))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("log labels outside the agreed set:\n%s\n\nEither reuse the name this concept already "+
			"has, or add the new one to this list on purpose.", strings.Join(offenders, "\n"))
	}
}

// The per-path half of the same contract: the goroutine's own cleanup must
// stand down too. It used to be an unconditional `defer cleanup()`, which fired
// the instant the engine returned -- and onceCleanup made it the winning call,
// so the central skip never got a say.
func TestReleaseOrHold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantClosed bool
		wantFlag   bool
	}{
		{name: "a clean stop closes the clients", err: nil, wantClosed: true},
		{name: "an ordinary failure still closes them", err: errors.New("rpc broke"), wantClosed: true},
		{
			name: "a stuck worker holds them open and raises the flag",
			err:  errWrap(workergroup.ErrWorkersStillRunning),
			// no close, and the central cleanups must stand down as well
			wantFlag: true,
		},
		{
			name:     "a stuck worker joined with a real failure still holds them",
			err:      errors.Join(errors.New("proof failed"), errWrap(workergroup.ErrWorkersStillRunning)),
			wantFlag: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := false
			var flag atomic.Bool
			releaseOrHold(zap.NewNop(), func() { closed = true }, &flag, tc.err, "cosmos_to_eth", "client-0")

			if closed != tc.wantClosed {
				t.Fatalf("clients closed = %v, want %v", closed, tc.wantClosed)
			}
			if flag.Load() != tc.wantFlag {
				t.Fatalf("workersStuck = %v, want %v: the central cleanups read this, so a "+
					"missed flag closes the same clients a moment later", flag.Load(), tc.wantFlag)
			}
		})
	}

	// P1c, found in review. stopRelaysAndCleanup already refuses to close clients
	// when its own wait overruns, for a stated reason: "a goroutine that has not
	// returned may still be using them, and closing underneath it trades a slow
	// shutdown for a crash". Its wait cannot see a worker orphaned INSIDE a source,
	// because the engine returns as soon as its own drain times out and the
	// orphaned goroutine is in no WaitGroup. The wait then succeeds and the
	// cleanups run over a live worker -- the same crash, one level down.
	t.Run("cleanup is held when a source could not stop its workers", func(t *testing.T) {
		t.Run("held when a source reports stuck workers", func(t *testing.T) {
			cleanupRan := false
			err := stopRelaysAndCleanup(
				func() {}, func() {},
				[]func(){func() { cleanupRan = true }},
				func() bool { return true },
			)
			if cleanupRan {
				t.Fatal("closed the clients while a worker was still using them; " +
					"that is the crash the drain-overrun branch already refuses to cause")
			}
			if err == nil {
				t.Fatal("a shutdown that left clients open must be reported, or it looks clean")
			}
			if !strings.Contains(err.Error(), "leaving clients open") {
				t.Fatalf("error %q does not say what was skipped or why", err)
			}
		})

		t.Run("runs when every worker stopped", func(t *testing.T) {
			cleanupRan := false
			err := stopRelaysAndCleanup(
				func() {}, func() {},
				[]func(){func() { cleanupRan = true }},
				func() bool { return false },
			)
			if err != nil {
				t.Fatalf("clean shutdown reported an error: %v", err)
			}
			if !cleanupRan {
				t.Fatal("clients were never closed on a clean shutdown; the guard is too eager " +
					"and every ordinary stop now leaks its connections")
			}
		})
	})
}

func errWrap(err error) error { return errors.Join(errors.New("cosmos source"), err) }

// awaitRelayExit decides the process exit code. The SIGTERM branch is the one
// that was wrong: a module reports the workers it could not drain, that error is
// recorded, and the branch returned nil anyway -- a stuck worker looked exactly
// like a clean stop.
func TestAwaitRelayExit(t *testing.T) {
	logger := zap.NewNop()
	noopStop := func() error { return nil }

	t.Run("clean stop returns nil", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		if err := awaitRelayExit(context.Background(), done, newEngineErrors(func() {}), noopStop, logger); err != nil {
			t.Fatalf("clean stop returned %v", err)
		}
	})

	// The failure reaches this function through done, not through an arm of its
	// own: recording cancels the sibling engines, so every engine returns and the
	// wait group closes done. The assertion is that "all engines returned" no
	// longer reads as a clean exit when one of them returned because it failed.
	t.Run("an engine failure is returned", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		engineErrs := newEngineErrors(func() {})
		engineErrs.record(errors.New("cosmos_to_eth source \"cosmos-0\": boom"))

		err := awaitRelayExit(context.Background(), done, engineErrs, noopStop, logger)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("engine failure = %v, want the engine's own error", err)
		}
	})

	t.Run("SIGTERM surfaces a recorded drain failure", func(t *testing.T) {
		// Exactly the shape relay.Module.Run produces on an overrun.
		const stuck = `relay cosmos->eth: shutdown drain timed out after 30s; workers still running: [cosmos-subscribe]`
		engineErrs := newEngineErrors(func() {})
		engineErrs.record(errors.New(stuck))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := awaitRelayExit(ctx, make(chan struct{}), engineErrs, noopStop, logger)
		if err == nil {
			t.Fatal("SIGTERM with a stuck worker exited cleanly; that is the bug this branch had")
		}
		if !strings.Contains(err.Error(), "cosmos-subscribe") {
			t.Fatalf("error %q does not name the worker that is stuck", err)
		}
	})

	t.Run("SIGTERM surfaces a shutdown that overran its budget", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stopErr := errors.New("relay engines did not drain within 30s")

		err := awaitRelayExit(ctx, make(chan struct{}), newEngineErrors(func() {}),
			func() error { return stopErr }, logger)
		if !errors.Is(err, stopErr) {
			t.Fatalf("SIGTERM returned %v, want the shutdown failure", err)
		}
	})

	// The control for the two above: an ordinary SIGTERM with nothing wrong exits
	// 0. Without it, "return the error" is satisfied by always returning one.
	t.Run("an ordinary SIGTERM exits cleanly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := awaitRelayExit(ctx, make(chan struct{}), newEngineErrors(func() {}), noopStop, logger); err != nil {
			t.Fatalf("ordinary SIGTERM returned %v, want nil", err)
		}
	})

	// P1b: awaitRelayExit existed, was documented as the fix, was tested -- and
	// Start never called it, so the SIGTERM ending still dropped a recorded failure
	// and returned nil. This pins the contract awaitRelayExit implements, so the
	// wiring test below has something to be the wiring OF.
	t.Run("reports a failure recorded during shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // SIGTERM already landed

		engineErrs := newEngineErrors(func() {})
		engineErrs.record(errors.New("cosmos source: workers still running"))

		err := awaitRelayExit(ctx, make(chan struct{}), engineErrs,
			func() error { return nil }, zap.NewNop())
		if err == nil {
			t.Fatal("exit code 0 with a recorded failure: the relayer reports success " +
				"and the operator never learns a worker was stuck")
		}
	})
}

func TestStopRelaysAndCleanup(t *testing.T) {
	t.Run("client cleanup runs after relay drain", func(t *testing.T) {
		var order []string
		stopRelaysAndCleanup(
			func() { order = append(order, "cancel") },
			func() { order = append(order, "drain") },
			[]func(){func() { order = append(order, "cleanup") }},
			nil,
		)
		want := []string{"cancel", "drain", "cleanup"}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Fatalf("order = %v, want %v", order, want)
		}
	})

	// A worker that will not return must not hold the process. Before this, wait()
	// was called bare: the relayer sat holding its Cosmos account sequence and its
	// ETH nonce, and an operator restarting it collided with both.
	t.Run("gives up on a stuck drain", func(t *testing.T) {
		withShortShutdownBudgets(t, 40*time.Millisecond, time.Second)

		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		cleanupRan := false

		start := time.Now()
		err := stopRelaysAndCleanup(
			func() {},
			func() { <-release }, // the worker that refuses to exit
			[]func(){func() { cleanupRan = true }},
			nil,
		)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("a drain that never returned was reported as a clean shutdown")
		}
		if !strings.Contains(err.Error(), "drain") {
			t.Fatalf("error %q does not say which phase overran", err)
		}
		if elapsed > 2*shutdownDrainBudget {
			t.Fatalf("waited %s for a %s budget", elapsed, shutdownDrainBudget)
		}
		// Deliberate: a goroutine that has not returned may still be using these
		// clients, and closing underneath it trades a slow shutdown for a crash.
		if cleanupRan {
			t.Fatal("clients were closed while a worker was still running")
		}
	})

	// The cleanup phase is bounded too -- cosmosClient.Stop() and ethClient.Close()
	// talk to sockets, so neither is guaranteed to return.
	t.Run("gives up on a stuck cleanup", func(t *testing.T) {
		withShortShutdownBudgets(t, time.Second, 40*time.Millisecond)

		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		err := stopRelaysAndCleanup(func() {}, func() {}, []func(){func() { <-release }}, nil)
		if err == nil {
			t.Fatal("a cleanup that never returned was reported as a clean shutdown")
		}
		if !strings.Contains(err.Error(), "cleanup") {
			t.Fatalf("error %q does not say which phase overran", err)
		}
	})

	// The control case: nothing is stuck, so nothing is reported. Without this the
	// tests above are satisfied by a function that always errors.
	t.Run("reports nothing on a clean stop", func(t *testing.T) {
		withShortShutdownBudgets(t, time.Second, time.Second)

		if err := stopRelaysAndCleanup(func() {}, func() {}, []func(){func() {}}, nil); err != nil {
			t.Fatalf("a clean shutdown reported %v", err)
		}
	})
}

func TestIsShutdownErr(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{context.Canceled, true},
		{context.DeadlineExceeded, true},
		{fmt.Errorf("wrapped: %w", context.Canceled), true},
		{errors.New("request was cancelled by server"), false},
		{nil, false},
	} {
		if got := isShutdownErr(tc.err); got != tc.want {
			t.Fatalf("isShutdownErr(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// withShortShutdownBudgets runs the same code path in milliseconds. The budgets
// are the only thing shortened -- every branch under test is the production one.
func withShortShutdownBudgets(t *testing.T, drain, cleanup time.Duration) {
	t.Helper()
	origDrain, origCleanup := shutdownDrainBudget, shutdownCleanupBudget
	shutdownDrainBudget, shutdownCleanupBudget = drain, cleanup
	t.Cleanup(func() { shutdownDrainBudget, shutdownCleanupBudget = origDrain, origCleanup })
}

// The budget an operator is promised. Nothing reads this at runtime, so a
// change to either half would otherwise be invisible until a SIGTERM in
// production took longer than the runbook says it can.
func TestShutdownBudget_IsFortyFiveSeconds(t *testing.T) {
	if shutdownBudget != 45*time.Second {
		t.Fatalf("shutdown budget = %s, want 45s (docs and the runbook state this number)", shutdownBudget)
	}
	if shutdownDrainBudget != workers.ShutdownDrainTimeout {
		t.Fatalf("drain budget = %s, want the per-module timeout %s: modules drain in parallel, so the "+
			"two are the same bound and must not drift", shutdownDrainBudget, workers.ShutdownDrainTimeout)
	}
}
