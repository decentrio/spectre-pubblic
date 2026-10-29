package main

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// A2/A1 leave one path per process, so an engine failure must still leave the
// process with a non-nil error -- the gathering layer went away, the reporting
// must not.
func TestEngineErrors(t *testing.T) {
	t.Run("a clean run reports nothing", func(t *testing.T) {
		e := newEngineErrors(func() {})
		if err := e.first(); err != nil {
			t.Fatalf("first() = %v on a clean run, want nil", err)
		}
	})

	t.Run("the failure survives to the caller", func(t *testing.T) {
		want := errors.New("cosmos_to_eth source \"client-0\": subscribe: boom")
		e := newEngineErrors(func() {})
		e.record(want)
		if got := e.first(); !errors.Is(got, want) {
			t.Fatalf("first() = %v, want the recorded failure", got)
		}
	})

	// Recording must stop the sibling leg. A Cosmos<->L2 path runs two engines
	// and the return leg has nothing to do once the forward leg is gone; without
	// this the teardown waits for a leg that will never finish on its own.
	t.Run("recording cancels the rest of the path", func(t *testing.T) {
		var cancelled int
		e := newEngineErrors(func() { cancelled++ })
		e.record(errors.New("boom"))
		if cancelled != 1 {
			t.Fatalf("cancel called %d times, want 1", cancelled)
		}
	})

	// The sibling's error is almost always "context canceled" caused by the
	// first one. Reporting it would name the symptom.
	t.Run("the first error wins and cancels once", func(t *testing.T) {
		first := errors.New("the cause")
		var cancelled int
		e := newEngineErrors(func() { cancelled++ })
		e.record(first)
		e.record(errors.New("context canceled"))
		if got := e.first(); !errors.Is(got, first) {
			t.Fatalf("first() = %v, want the cause and not the consequence", got)
		}
		if cancelled != 1 {
			t.Fatalf("cancel called %d times, want 1", cancelled)
		}
	})

	t.Run("a nil error is not a failure", func(t *testing.T) {
		var cancelled int
		e := newEngineErrors(func() { cancelled++ })
		e.record(nil)
		if err := e.first(); err != nil {
			t.Fatalf("recording nil produced %v", err)
		}
		if cancelled != 0 {
			t.Fatal("recording nil cancelled the path")
		}
	})

	// Both engines can fail at the same instant; -race and the once must hold.
	t.Run("concurrent engines", func(t *testing.T) {
		e := newEngineErrors(func() {})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); e.record(errors.New("boom")) }()
		}
		wg.Wait()
		if e.first() == nil {
			t.Fatal("concurrent failures left no error to report")
		}
	})

	// The half of A3's acceptance that awaitRelayExit's own tests cannot reach:
	// "the single path's error still exits the process" needs the engines to HAND
	// their failure to engineErrors in the first place. Removing one of those calls
	// left every test in this package green, which is how a relayer would exit 0 on
	// a dead cosmos_to_eth engine with nothing in the log.
	//
	// Asserted structurally, in the shape TestStart/opts into the wasm client ID requirement
	// already uses here, because the alternative is booting a real relayer: each
	// engine runs on its own goroutine inside the command closure, behind RPC dials
	// a unit test has no way to satisfy.
	//
	// It derives the engine list from the source rather than hard-coding three, so a
	// fourth engine is covered the day it is added instead of the day someone
	// remembers to update a count.
	t.Run("every engine failure is recorded", func(t *testing.T) {
		src, err := os.ReadFile("start.go")
		if err != nil {
			t.Fatalf("read start.go: %v", err)
		}
		body := string(src)

		engineCall := regexp.MustCompile(`err := (run\w*Engine)\(`)
		calls := engineCall.FindAllStringSubmatchIndex(body, -1)
		if len(calls) == 0 {
			t.Fatal("start.go runs no engines; this test no longer describes it")
		}

		for _, c := range calls {
			name := body[c[2]:c[3]]
			// The goroutine body ends at the closing of the `go func(...) {...}(...)`
			// it lives in; the next `}(` is a tight enough bound and does not depend
			// on brace counting.
			tail := body[c[1]:]
			end := strings.Index(tail, "}(")
			if end < 0 {
				end = len(tail)
			}
			if !strings.Contains(tail[:end], "recordEngineErr(") {
				t.Errorf("%s's failure is never handed to engineErrors; the process would exit 0 "+
					"on a dead engine, which is the outcome A3 is about", name)
			}
		}

		// And the collection has to reach the ending that reports it.
		if !strings.Contains(body, "awaitRelayExit(runCtx, done, engineErrs,") {
			t.Error("start does not pass engineErrs to awaitRelayExit, so nothing recorded is ever read")
		}
	})
}
