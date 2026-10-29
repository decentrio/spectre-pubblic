// main.go is the process entry point and nothing else: wire the commands, run
// one, and shut down cleanly. Each command lives in its own file, following
// submit_misbehaviour.go, which already had that shape.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"relayer/logbridge"
	workergroup "relayer/workers"
)

func isShutdownErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Shutdown budget for the whole process, split between the two phases it has.
//
// shutdownDrainBudget matches workergroup.ShutdownDrainTimeout: modules drain in
// parallel, so the per-module bound is also the bound for all of them together
// and the two must not drift apart. shutdownCleanupBudget covers the client
// close layer (cosmosClient.Stop, ethClient.Close). Together they are the 45
// seconds a SIGTERM is allowed to take.
//
// Widening these is almost always the wrong fix. A drain that overruns means an
// RPC is running without a deadline derived from the relay context, and the
// remedy is to give it one, not to wait longer for it.
// Variables, not constants, only so the shutdown tests can run in milliseconds
// instead of 45 seconds. Production never assigns them.
var (
	shutdownDrainBudget   = workergroup.ShutdownDrainTimeout
	shutdownCleanupBudget = 15 * time.Second
)

// shutdownBudget is the number the operator-facing contract is stated in: a
// SIGTERM finishes, one way or the other, inside this.
const shutdownBudget = workergroup.ShutdownDrainTimeout + 15*time.Second

// stopRelaysAndCleanup cancels the relay context, waits for the engines to
// drain, then closes the clients -- in that order, because closing a client out
// from under a running goroutine turns a clean shutdown into an error cascade.
//
// Each phase is bounded. An unbounded wait here is not a hang that someone
// notices: the process sits there holding its Cosmos account sequence and its
// ETH nonce, and an operator restarting it hits both. Exceeding either budget
// returns an error rather than waiting, so the caller can exit non-zero.
//
// A transaction that has been broadcast but not yet confirmed is NOT waited for
// and NOT cancelled. Waiting reopens an unbounded window; cancelling races a
// transaction that may already be in a block. The packet is in the durable
// pending tracker, and on restart the on-chain receipt query decides whether it
// was delivered -- which is the whole reason those two mechanisms exist. Do not
// "improve" this into a wait.
// workersStuck reports whether any engine gave up on its own inner drain with a
// worker still alive. wait below cannot see that: it waits on the engine
// goroutines, and an engine returns as soon as ITS drain times out, leaving the
// orphaned worker outside every WaitGroup. Without this the wait succeeds and
// the cleanups run underneath a live worker -- the crash the skip above exists
// to avoid, reached one level down. Found in review.
func stopRelaysAndCleanup(cancel func(), wait func(), cleanups []func(), workersStuck func() bool) error {
	cancel()

	if err := runBounded(wait, shutdownDrainBudget); err != nil {
		// The clients are deliberately left open: a goroutine that has not
		// returned may still be using them, and closing underneath it trades a
		// slow shutdown for a crash. Report and let the process exit.
		return fmt.Errorf("relay engines did not drain within %s", shutdownDrainBudget)
	}
	if workersStuck != nil && workersStuck() {
		// Same reason, different level: every engine goroutine has returned, but
		// one of them reported a worker it could not stop.
		return fmt.Errorf("a relay source could not stop its own workers within %s; "+
			"leaving clients open rather than closing them underneath one that is still running",
			workergroup.SourceDrainTimeout)
	}

	if err := runBounded(func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}, shutdownCleanupBudget); err != nil {
		return fmt.Errorf("client cleanup did not finish within %s", shutdownCleanupBudget)
	}
	return nil
}

// awaitRelayExit blocks until the relay path ends and returns the process's exit
// error: nil for a clean stop, non-nil for anything an operator has to act on.
//
// Extracted from Start so all three endings can be tested. The one that used to
// be wrong is unreachable from a unit test while it lives inside the command
// closure, and it was wrong for exactly that reason.
//
// engineErrs holds what the buffered channel used to: an engine can record its
// failure and return without anyone selecting on anything, and recording cancels
// the rest of the path so every engine returns and done closes.
//
// That is why there are two endings here and not three. The channel arm existed
// to observe a failure WHILE siblings were still running; recording now cancels
// them, and their drain is bounded (relay.Module.Run reports the workers that
// overran rather than waiting on them), so the failure reaches the done branch
// promptly instead of needing an arm of its own. The nested select inside done
// goes with it: "all workers returned" and "one of them failed" are one question
// now, and engineErrors.first is the answer.
func awaitRelayExit(
	runCtx context.Context,
	done <-chan struct{},
	engineErrs *engineErrors,
	stopAndCleanup func() error,
	logger *zap.Logger,
) error {
	select {
	case <-done:
		if err := engineErrs.first(); err != nil {
			// An engine failed while running. That error is the actionable one, so a
			// shutdown that also overruns is logged rather than returned in its place.
			logShutdownFailure(logger, stopAndCleanup())
			return err
		}
		return nil

	case <-runCtx.Done():
		// SIGTERM. A module that overran its drain reports which workers are still
		// running (relay.Module.Run), and that error is recorded like any other.
		// This branch did not read it, so a stuck worker produced exit code 0 with
		// "Relayer clients stopped; exiting" as the last line -- the failure was
		// recorded and then dropped, which is the worst of the endings because it
		// looks exactly like success. It asks engineErrors the same question the
		// done branch above asks; the two must stay in step.
		logger.Sugar().Infof("Relayer shutdown requested: %v", runCtx.Err())
		shutdownErr := stopAndCleanup()
		if err := engineErrs.first(); err != nil {
			return err
		}
		if shutdownErr != nil {
			return shutdownErr
		}
		logger.Sugar().Info("Relayer clients stopped; exiting")
		return nil
	}
}

// stampRelayPath labels BOTH loggers with the relay path and returns the
// path-scoped zap logger the caller must use from then on.
//
// The path is a field on every line from here on, standard-library lines
// included: logbridge.Install points the standard logger at the scoped logger,
// and services and subscriber take an injected *log.Logger that the composition
// root fills with log.Default() (cmd/build_source.go, cmd/build_cosmos_to_l2.go),
// so one call reaches every relay-path line. It used to be a text prefix on the
// standard logger, held after the timestamp by Lmsgprefix. As a field it can be
// filtered on, and a viewer no longer has to pick it out of the message.
//
// One function rather than two calls, because the first version stamped only
// log.Default() and every line the injected *zap.Logger emitted after it stayed
// unattributable -- "Relayer started", "shutdown requested", "clients stopped".
// Found in review. Splitting it into a stamp and a separate logger.With() would
// leave the same half-done state one forgotten line away, whereas a function
// that returns the scoped logger cannot be half-used: the caller either takes
// the return value or has nothing to log with.
func stampRelayPath(logger *zap.Logger, pathID string) *zap.Logger {
	scoped := logger.With(zap.String(relayPathLogField, pathID))
	logbridge.Install(scoped)
	return scoped
}

// relayPathLogField is the key the path is logged under, on the zap lines and on
// the standard-library lines bridged into them.
const relayPathLogField = "relay_path"

// logShutdownFailure reports a shutdown that overran its budget on a path whose
// return value is already carrying a more actionable error (a build failure, or
// the loop error that caused the shutdown). Swallowing it silently is what made
// a stuck worker invisible; returning it would hide the cause behind the symptom.
func logShutdownFailure(logger *zap.Logger, err error) {
	if err != nil {
		logger.Sugar().Errorf("Shutdown did not finish cleanly: %v", err)
	}
}

// runBounded runs fn and returns an error if it has not returned within budget.
// fn keeps running in its goroutine afterwards -- there is no way to interrupt an
// arbitrary func() -- so the caller must treat the timeout as "exit now", not as
// "retry".
func runBounded(fn func(), budget time.Duration) error {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

// --- Main ---

func main() {
	settings := resolveLogSettings(os.Args[1:], os.Getenv, stderrIsTerminal())
	zLogger := newLogger(settings, zapcore.Lock(os.Stderr))
	defer zLogger.Sync()
	for _, msg := range settings.ignored {
		zLogger.Warn(msg)
	}
	// One pipeline for the process. The engine logs through the standard logger,
	// which has no levels; this gives every such line a level and the same format as
	// the zap lines, so a viewer sees one stream. `start` re-points it at a
	// path-scoped logger once it knows the path (stampRelayPath).
	logbridge.Install(zLogger)

	logger := zLogger.Sugar()

	rootCmd := &cobra.Command{
		Use:   "relayer [command]",
		Short: "Spectre relayer — relay IBC packets between Cosmos and Ethereum",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}
	registerLogFlags(rootCmd)

	rootCmd.AddCommand(
		Start(zLogger),
		CreateClientsCosmos(zLogger),
		CreateClientsEth(zLogger),
		UpdateClient(zLogger),
		UpdateEthClient(zLogger),
		SubmitMisbehaviour(zLogger),
		Genesis(zLogger),
		Status(zLogger),
	)

	if err := rootCmd.Execute(); err != nil {
		logger.Fatal(err)
	}
}

// releaseOrHold closes one relay path's clients, unless that path just reported
// a worker it could not stop.
//
// The per-goroutine cleanup used to be an unconditional `defer cleanup()`, which
// defeated the whole point of the skip in stopRelaysAndCleanup: an engine
// returns the instant ITS drain times out, so the defer fired while the named
// worker was still using the clients it then closed. onceCleanup made that the
// winning call, so the central skip never got a say. Found in review.
//
// It also raises the flag stopRelaysAndCleanup reads, so the central cleanups --
// which hold the same closure -- stand down too.
func releaseOrHold(
	logger *zap.Logger,
	cleanup func(),
	workersStuck *atomic.Bool,
	err error,
	kind, id string,
) {
	if errors.Is(err, workergroup.ErrWorkersStillRunning) {
		workersStuck.Store(true)
		logger.Sugar().Errorf("%s %q: a worker did not stop, so its clients are left open "+
			"rather than closed underneath it: %v", kind, id, err)
		return
	}
	cleanup()
}
