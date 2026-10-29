// The start command: build the configured relay path and run it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"time"

	"relayer/relay"
	"relayer/services"
	"relayer/status"
	"relayer/transaction"
	utils "relayer/utils"
)

// Start starts the relay service loop.
// It loads config from a JSON file, connects to Cosmos and Ethereum,
// and subscribes to send_packet events to relay packets.
// statusBindGrace is how long start waits to see whether the status endpoint
// bound before carrying on. Listening either works immediately or fails
// immediately, so this is long enough to catch "port already in use" and short
// enough that it never delays a healthy start noticeably.
const statusBindGrace = 200 * time.Millisecond

func Start(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "start the relay loop",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := cmd.Flags().GetString(flagConfigPath)
			if err != nil {
				return fmt.Errorf("failed to get config path: %w", err)
			}

			// Load .env for prover paths, private keys, etc.
			_ = godotenv.Load()

			benchmarkFlag, err := cmd.Flags().GetBool(flagBenchmark)
			if err != nil {
				return fmt.Errorf("failed to get benchmark flag: %w", err)
			}
			utils.SetBenchEnabled(benchmarkFlag || utils.BenchEnabled())
			if utils.BenchEnabled() {
				log.Printf("[bench] enabled: detailed gas/timing logs are active")
			}

			if err := validateStartupKeys(); err != nil {
				return err
			}

			runCtx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSignals()

			// Load JSON config
			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			// Checks that only apply to relaying, so they live here rather than in
			// the shared loadConfig. Run before the prover load: a config error
			// should surface immediately, not after the bucket registry is read.
			// One process, one source → destination pair: loadConfig has already
			// refused a file declaring more than one (rejectExtraRelayPaths), so
			// what is left is that it declares one at all -- `start` with nothing
			// to relay is a config mistake, not an idle process.
			if err := requireRelayPath(cfg, configPath); err != nil {
				return err
			}
			if err := validateRelayStartupConfig(cfg); err != nil {
				return err
			}
			// Before anything opens a state directory: state written by the previous
			// layout is not read by this build, and starting anyway would look like a
			// clean first run while pending packets quietly stopped being tracked.
			if err := checkLegacyStateLayout(configPath); err != nil {
				return err
			}

			// Stamp the path onto every log line from here on. A1 makes this a
			// process-level constant, and the merge point (journald, Loki) is where
			// several relayer processes lose the file boundary that used to tell
			// their lines apart.
			//
			// The boundary it sits on is "answerable from the file" versus "needs a
			// read". The two checks above report a config the operator can read back
			// from the file, so a label would add nothing; everything below -- the
			// chain-id and deployment probes, the signer lock, the prover, the relay
			// loops -- describes live state, and in a merged log that is where the
			// line stops saying which process it came from. relayPathID is safe here
			// because loadConfig and requireRelayPath have proved there is exactly one.
			//
			// Shadowing `logger` is deliberate: every builder and lifecycle line
			// below takes it from here, so the scoped logger reaches them without
			// anyone having to remember to pass the right one.
			logger := stampRelayPath(logger, relayPathID(cfg))

			// After the stamp, not before: TestStart/stamps the relay path before it can fail
			// holds that EVERY zap line start emits carries relay_path, and a line
			// above the stamp would be the first exception. Putting this one line
			// below costs nothing the two checks above do not already cover --
			// validateSingleRelayPair names configPath in all three of its errors --
			// and it still runs before anything that needs a network read, which is
			// where a run against the wrong file starts reporting live state the
			// operator cannot find in the file they were looking at.
			//
			// (validateRelayStartupConfig's error names the client id but not the
			// file; that gap predates this and is not this change's to close.)
			logConfigTarget(logger, "start", configPath, cfg)

			// Third: everything that needs a read to prove wrong — the state
			// directory, the Cosmos chain id, the deployed contracts and the client
			// ids. Last of the three because the two above are answerable from the
			// file alone, and reporting a chain disagreement for a config that
			// declares two paths would name the wrong problem. runCtx, not
			// cmd.Context(): each probe wraps what it is given in a timeout, so on
			// cmd.Context() a Ctrl-C during a hung endpoint would wait the full
			// budget out per endpoint.
			// The state directory this process will actually write into, derived
			// the same way the loops below derive it. A1 has already proved there
			// is exactly one path, so there is exactly one directory to judge.
			if err := validateStartupConfig(runCtx, cfg, startupStateDir(configPath, cfg)); err != nil {
				return err
			}
			// Last of the startup gates, because it is the only one that takes
			// something: an exclusive lock on this process's Cosmos signing
			// address. Running it after the read-only checks means a config
			// mistake is reported without ever contending for the lock.
			releaseSigner, err := acquireCosmosSignerLock()
			if err != nil {
				return err
			}
			defer releaseSigner()

			// Dial the prover once and share the client across the relay loops; its
			// calls are safe to make concurrently, and the prover serializes the
			// proving itself. Only a Cosmos-sourced path (to Ethereum or to an L2)
			// proves anything: a config without one never needs the prover, so it is
			// neither required nor dialled. A path that does need it fails here, with
			// the address in the message, rather than at its first client update.
			var p services.Prover
			if cfg.HasCosmosToEth || cfg.HasCosmosToL2 {
				proverConn, err := dialProver(runCtx, cmd, logger)
				if err != nil {
					return err
				}
				defer proverConn.Close()
				p = proverConn
			}

			// One path: loadConfig refused a file with more than one, and
			// requireRelayPath proved there is at least one, so each direction
			// below is a single value behind its presence flag -- nothing to loop
			// over, nothing to pick from.
			//
			// validateL2TimeoutReturnPathConfigs is gone with #328: the return path
			// is now resolved for the dest by matching the L2 chain id at build time
			// (l2TimeoutReturnPathConfigForDest), so a separate up-front pass would
			// only duplicate it. This check stays because it answers a different
			// question -- whether the l2_to_cosmos source's configured chain id is
			// the chain its RPC actually serves -- and it must run before anything
			// dials.
			// runCtx, not cmd.Context(): the probe wraps whatever it is given in a
			// 10s timeout, so on cmd.Context() a Ctrl-C during a hung L2 RPC waited
			// out the full 10s before the process could exit.
			if cfg.HasL2ToCosmos {
				if err := validateL2ChainID(runCtx, cfg.L2ToCosmosConfig); err != nil {
					return err
				}
			}
			// EVM nonces are per (chain id, sender), and the Handler mutex only
			// serializes allocations in this process. Resolve every EVM endpoint's
			// actual chain id and hold those cross-process locks before any builder
			// can submit. A non-answer is fatal here: starting without a chain id
			// would silently mean starting without this guard.
			releaseEVMSignerLocks, err := acquireEVMSignerLocks(runCtx, cfg)
			if err != nil {
				return err
			}
			defer releaseEVMSignerLocks()
			// Env overrides (ICS26_CLIENT_ID, COSMOS_WASM_CLIENT_ID, ROLE_MANAGER)
			// name a single source, and a process runs exactly one — so they can
			// only ever apply to the source that is running; effectiveRelayIDs and
			// effectiveRoleManager apply them unconditionally for that reason.
			//
			// They reach the cosmos_to_eth builder only. buildCosmosToL2Dest does
			// not consult the environment, so evmDeploymentsInConfig must not apply
			// them to the cosmos_to_l2 entry either -- see the two branches there.

			// One shared TransactionHandler across all sources. What sharing buys is
			// the mutex, not the cache: two sources can target the SAME (chain,
			// signer) pair, and only a single h.mu can serialize their nonce
			// allocation. Per-source handlers would each take their own lock and
			// hand the same nonce to both.
			//
			// Sharing the cache is safe because it is keyed by (chainID, address)
			// — see transaction.evmNonceKey — so sources submitting to DIFFERENT
			// EVM chains keep separate nonces for the same signer. Before that
			// keying the cache was a single counter, and a nonce allocated against
			// one L2 was reused against another as a future nonce: accepted into
			// the queued pool, never minable, and never surfaced as an error (#320).
			//
			// The Cosmos side re-queries the account sequence fresh under cosmosMu,
			// so sharing is safe there too.
			txHandler := &transaction.Handler{}

			// One independent relay loop per Cosmos→ETH source. Each has its own
			// Tendermint RPC, SpectreClient and router client id; they share the
			// prover, the TransactionHandler, the ETH beacon endpoint, and the
			// same ICS26Router (ETH events are partitioned by the per-source
			// router client id filter).
			var wg sync.WaitGroup
			// A path runs at most two engines: the forward leg, plus the return leg
			// for Cosmos↔L2. Cosmos↔ETH is one engine — runAdapterEngine drives both
			// of its directions internally.
			// Held for the life of the process: the lock IS the descriptor, so these
			// are collected early and never deferred into a shorter scope. The
			// kernel releases them at exit, including on SIGKILL.
			var stateLocks []*os.File
			// Released only here, when RunE returns and the process is leaving.
			// This is not tidiness: os.File carries a finalizer that closes the
			// descriptor once the value is unreachable, and closing it releases the
			// flock. A slice that is only ever appended to is exactly the shape the
			// collector is free to reclaim, which would silently hand the directory
			// to a second process while this one is still running.
			defer func() {
				for _, lock := range stateLocks {
					_ = lock.Close()
				}
			}()

			const maxEnginesPerPath = 2
			cleanups := make([]func(), 0, maxEnginesPerPath)
			relayCtx, cancelRelays := context.WithCancel(runCtx)
			defer cancelRelays()

			// One path per process (#418), so an engine failure is THE failure: no
			// other path is left to keep serving. What used to be here was a
			// buffered channel, a select arm on it, and a nested select to prefer a
			// buffered error over a coincident done -- a gathering layer sized for
			// many unrelated loops in one process, which is the arrangement #418
			// removed.
			//
			// The collection stays, because a Cosmos<->L2 path still runs two
			// engines. First error wins and cancels its sibling: the return leg has
			// nothing to do once the forward leg is gone, and cancelling is what
			// keeps the teardown as prompt as the channel made it. See engineErrors.
			engineErrs := newEngineErrors(cancelRelays)
			recordEngineErr := engineErrs.record
			var l2ReturnPaths []l2TimeoutReturnPathConfig
			onceCleanup := func(cleanup func()) func() {
				var once sync.Once
				return func() {
					if cleanup != nil {
						once.Do(cleanup)
					}
				}
			}
			// Set by any engine whose source gave up on its inner drain. Read by
			// stopRelaysAndCleanup, which then leaves the clients open.
			var workersStuck atomic.Bool
			// Returns the error rather than swallowing it: awaitRelayExit decides
			// whether a shutdown overrun is the thing to report or is outranked by
			// the failure that caused the shutdown.
			stopAndCleanup := func() error {
				return stopRelaysAndCleanup(cancelRelays, wg.Wait, cleanups, workersStuck.Load)
			}

			// One endpoint answers for the whole process, so the engines below
			// register with it as they start. It binds nothing unless the config
			// asked for an address, and a failure to bind stops the relayer rather
			// than leaving an operator to discover during an incident that the thing
			// they were told to query was never listening.
			statusReg := &status.Registry{}
			statusErrCh := make(chan error, 1)
			if cfg.StatusAddress != "" {
				var controls *status.Controls
				if cfg.StatusAllowControl {
					controls = &status.Controls{Reactivate: statusReg.Reactivate}
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := status.Serve(relayCtx, cfg.StatusAddress, statusReg.Snapshot, controls); err != nil {
						statusErrCh <- err
					}
				}()
				select {
				case err := <-statusErrCh:
					stopAndCleanup()
					return err
				case <-time.After(statusBindGrace):
				}
			}
			if cfg.HasCosmosToEth {
				c2e := cfg.CosmosToEthConfig
				// The EFFECTIVE id, the one the builder below will relay with. Naming
				// the directory from the raw config id let the lock and the cursors
				// describe a different relay pair than the process actually served.
				// Resolved ONCE. The same value names the directory below and is
				// handed to the builder, so the lock, the cursors and the pair being
				// relayed cannot describe different clients.
				ids := effectiveRelayIDs(c2e)
				stateDir := relayStateDir(configPath, "cosmos-to-eth", ids)
				// Claim the directory before dialling anything. A second process
				// with the same identity must stop here, not after it has opened
				// sockets and begun advancing the cursors this one owns.
				lock, err := lockStateDir(stateDir)
				if err != nil {
					stopAndCleanup()
					return err
				}
				stateLocks = append(stateLocks, lock)
				svc, deps, cleanup, err := buildCosmosToEthSource(
					logger, c2e, cfg.EthToCosmosConfig, cfg.BatchConfig, p, txHandler,
					buildCosmosToEthSourceOptions{
						ids:                ids,
						roleManager:        effectiveRoleManager(c2e),
						startSubscriptions: true,
						relayStateDir:      stateDir,
						// start drives the eth->cosmos direction, so a missing
						// cosmos_wasm_client_id must fail here rather than at the
						// first packet that needs it. Unrelated to the state
						// directory above, and it arrived after this branch was
						// cut, so it is kept rather than replaced.
						relaysEVMToCosmos: true,
					},
				)
				if err != nil {
					stopAndCleanup()
					return fmt.Errorf("cosmos_to_eth source %q: %w", c2e.ICS26ClientID, err)
				}
				cleanup = onceCleanup(cleanup)
				cleanups = append(cleanups, cleanup)
				wg.Add(1)
				go func(svc *services.Services, deps services.RelayDeps, cleanup func()) {
					defer wg.Done()
					// Chain-adapter RelayModule engine — the sole relay engine.
					err := runAdapterEngine(relayCtx, svc, deps, statusReg)
					releaseOrHold(logger, cleanup, &workersStuck, err, "cosmos_to_eth", deps.IDs.CosmosOnEVM)
					if err != nil {
						recordEngineErr(fmt.Errorf("cosmos_to_eth source %q: %w", deps.IDs.CosmosOnEVM, err))
					}
				}(svc, deps, cleanup)
			}

			// The Cosmos→L2 destination: the same groth16 pipeline as Cosmos→ETH
			// pointed at the L2's SpectreClient/ICS26Router, with no reverse beacon
			// direction.
			//
			// Built before anything launches: first resolve the destination's return
			// path and match the l2_to_cosmos source to it, then start the goroutines
			// below. This way a return-path mismatch aborts before any Cosmos→L2
			// connection opens, instead of after the relay loop is already
			// dialing/subscribing.
			var l2Dest *builtL2Dest
			if cfg.HasCosmosToL2 {
				d := cfg.CosmosToL2Config
				l2StateDir := relayStateDir(configPath, "cosmos-to-l2", relayClientIDs{
					router: d.ICS26ClientID,
					wasm:   d.CosmosWasmClientID,
				})
				l2Lock, err := lockStateDir(l2StateDir)
				if err != nil {
					stopAndCleanup()
					return err
				}
				stateLocks = append(stateLocks, l2Lock)
				svc, deps, cleanup, err := buildCosmosToL2Dest(
					logger, d, cfg.BatchConfig, p, txHandler,
					l2StateDir,
				)
				if err != nil {
					stopAndCleanup()
					return fmt.Errorf("cosmos_to_l2 dest %q: %w", d.ICS26ClientID, err)
				}
				cleanup = onceCleanup(cleanup)
				cleanups = append(cleanups, cleanup)
				// Only resolve the return-path chain id when there is an
				// l2_to_cosmos source to match it against; otherwise a
				// forward-only deployment pays an avoidable L2 RPC round
				// trip (and startup-abort risk) for a path nothing consults.
				if cfg.HasL2ToCosmos {
					returnPath, err := l2TimeoutReturnPathConfigForDest(runCtx, cfg.CosmosToL2Name, d, l2TimeoutReturnPath{svc: svc, deps: deps})
					if err != nil {
						stopAndCleanup()
						return fmt.Errorf("cosmos_to_l2 dest %q: %w", d.ICS26ClientID, err)
					}
					l2ReturnPaths = append(l2ReturnPaths, returnPath)
				}
				// Not launched here: nothing dials, subscribes or submits until the
				// source below has built and matched. hasReturnLeg is deliberately
				// NOT set here either: whether the l2_to_cosmos source resolves to
				// THIS destination is only known once it has matched.
				l2Dest = &builtL2Dest{svc: svc, deps: deps, cleanup: cleanup}
			}

			// The L2->Cosmos source (opstack/arbitrum): its own L1/L2/Cosmos clients
			// + attestor sidecar, sharing the TransactionHandler (same Cosmos signer
			// -> shared sequence path).
			var l2Source *relay.Module
			var l2SourceCleanup func()
			if cfg.HasL2ToCosmos {
				src := cfg.L2ToCosmosConfig
				timeoutReturn, err := findL2TimeoutReturnPath(runCtx, src, l2ReturnPaths)
				if err != nil {
					stopAndCleanup()
					return fmt.Errorf("l2_to_cosmos source %q: %w", src.AttestorSrcChain, err)
				}
				l2SrcStateDir := relayStateDir(configPath, "l2-to-cosmos", relayClientIDs{
					router: src.L2ICS26ClientID,
					wasm:   src.L2WasmClientID,
				})
				l2SrcLock, err := lockStateDir(l2SrcStateDir)
				if err != nil {
					stopAndCleanup()
					return err
				}
				stateLocks = append(stateLocks, l2SrcLock)
				module, timeoutRecovery, cleanup, err := buildL2ToCosmosModule(logger, src, txHandler, timeoutReturn, l2SrcStateDir)
				if err != nil {
					stopAndCleanup()
					return fmt.Errorf("l2_to_cosmos source %q: %w", src.AttestorSrcChain, err)
				}
				cleanup = onceCleanup(cleanup)
				cleanups = append(cleanups, cleanup)
				l2Source, l2SourceCleanup = module, cleanup
				// Now that the source has matched, the destination knows whether
				// anything will settle what it records. timeoutReturn carries that
				// destination's own *services.Services, and sharing that instance is
				// the whole mechanism: one side records the debt, the other clears it
				// from the same ledger.
				if l2Dest != nil {
					l2Dest.hasReturnLeg = destHasReturnLeg(l2Dest.svc, []*services.Services{timeoutReturn.svc})
					if l2Dest.hasReturnLeg {
						l2Dest.timeoutRecovery = &timeoutRecovery
					}
				}
			}

			// Both halves built and matched cleanly — only now start
			// dialing/subscribing/submitting.
			if l2Dest != nil {
				wg.Add(1)
				go func(d builtL2Dest) {
					defer wg.Done()
					err := runCosmosToL2Engine(relayCtx, d.svc, d.deps, statusReg, d.timeoutRecovery, d.hasReturnLeg)
					releaseOrHold(logger, d.cleanup, &workersStuck, err, "cosmos_to_l2", d.deps.IDs.CosmosOnEVM)
					if err != nil {
						recordEngineErr(fmt.Errorf("cosmos_to_l2 dest %q: %w", d.deps.IDs.CosmosOnEVM, err))
					}
				}(*l2Dest)
			}
			if l2Source != nil {
				srcChain := cfg.L2ToCosmosConfig.AttestorSrcChain
				wg.Add(1)
				go func(module *relay.Module, cleanup func()) {
					defer wg.Done()
					err := runL2Engine(relayCtx, module, statusReg)
					releaseOrHold(logger, cleanup, &workersStuck, err, "l2_to_cosmos", srcChain)
					if err != nil {
						recordEngineErr(fmt.Errorf("l2_to_cosmos source %q: %w", srcChain, err))
					}
				}(l2Source, l2SourceCleanup)
			}

			logger.Sugar().Infof("Relayer started: relaying %s", relayPathID(cfg))
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			// awaitRelayExit rather than an inline select. The docstring on it
			// already said it was extracted "so all three endings can be tested" --
			// and then nothing called it, so Start kept the ending that drops a
			// recorded failure and exits 0. Found in review.
			return awaitRelayExit(runCtx, done, engineErrs, stopAndCleanup, logger)
		},
	}
	cmd.Flags().String(flagConfigPath, "config.json", "path to JSON config file")
	registerProverFlag(cmd)
	cmd.Flags().Bool(flagBenchmark, false, "enable detailed benchmark gas/timing logs (or set RELAYER_BENCHMARK=1)")
	return cmd
}

// startupStateDir names the one state directory this process will use, or "" if
// the config declares no path this build can resolve one for.
//
// It exists so validateStartupConfig can judge the directory that will really be
// written rather than the retired RELAYER_RECOVERY_STATE_FILE location. The
// derivation is the same relayStateDir the loops use, which is the point: a
// check against a different directory would pass while the real one failed.
func startupStateDir(configPath string, cfg *appConfig) string {
	switch {
	case cfg.HasCosmosToEth:
		return relayStateDir(configPath, "cosmos-to-eth", effectiveRelayIDs(cfg.CosmosToEthConfig))
	case cfg.HasCosmosToL2:
		d := cfg.CosmosToL2Config
		return relayStateDir(configPath, "cosmos-to-l2", relayClientIDs{
			router: d.ICS26ClientID, wasm: d.CosmosWasmClientID,
		})
	case cfg.HasL2ToCosmos:
		src := cfg.L2ToCosmosConfig
		return relayStateDir(configPath, "l2-to-cosmos", relayClientIDs{
			router: src.L2ICS26ClientID, wasm: src.L2WasmClientID,
		})
	}
	return ""
}
