package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"

	relayerclient "relayer/client"
	"relayer/services"
)

// buildCosmosToEthSource builds one Cosmos→ETH source: its own Tendermint RPC,
// SpectreClient and router client id, sharing the passed-in prover and the ETH
// beacon endpoint from the single eth_to_cosmos module. It returns the built
// Services, the composition-root dependencies, and a cleanup func that stops
// the chain clients.
//
// The caller runs runAdapterEngine(ctx, svc, deps) — which blocks until an error
// or shutdown — on its own goroutine, so N sources relay independently. Their ETH
// event streams don't cross-feed: SubscribeEth filters ICS26Router logs by the
// per-source router client id.
//
// buildCosmosToEthSourceOptions separates the long-running subscription setup
// from one-shot operator commands. Incident commands use only HTTP RPCs and
// must not depend on websocket infrastructure they never consume.
type buildCosmosToEthSourceOptions struct {
	// ids are the EFFECTIVE client ids this source will relay with, resolved by
	// the caller. They are passed rather than re-derived here on purpose: the
	// caller also names relayStateDir from them, and a second derivation is a
	// second chance to disagree. Start used to name the directory from the raw
	// config id while this builder applied the environment overrides, so the
	// lock and the cursors described a pair the process was not serving. Found
	// in review. One derivation, two consumers, no way to diverge.
	ids relayClientIDs
	// roleManager is the effective role-manager address, resolved by the caller
	// for the same reason ids are. Not part of the relay identity, but subject to
	// the same allow/disable decision.
	roleManager        string
	startSubscriptions bool
	relayStateDir      string

	// relaysEVMToCosmos says this caller will drive the eth->cosmos direction,
	// which is the only consumer of cosmos_wasm_client_id -- the id of the
	// Ethereum light client ON Cosmos.
	//
	// It is a property of the CALLER, not of the config, and treating it as the
	// latter broke submit-misbehaviour for a cosmos_to_l2 source: selectCosmosSource
	// promotes the L2 module, cosmos_to_l2 deliberately carries no
	// cosmos_wasm_client_id, and the builder rejected it for a field the command
	// never reads. The evidence was refused before it was looked at.
	//
	// Default false, so a caller opts IN to the requirement. That is the safe
	// direction here: a caller that forgets it fails at the point of use with the
	// id it wanted, whereas defaulting to true would reject valid L2
	// configurations again -- the failure this field exists to remove.
	relaysEVMToCosmos bool
}

// relayClientIDs is one cosmos_to_eth path's identity after overrides.
type relayClientIDs struct {
	router string
	wasm   string
}

// relayStateDir is where one process keeps everything it must not lose across a
// restart: the recovery cursors and the pending-packet trackers, in one place.
//
// Three properties, each for a reason:
//
//   - Rooted at the CONFIG's directory, not the working directory. Running
//     `start` from two different directories with the same config must reach the
//     same state, and a cwd-relative root is the surest way to get two.
//   - A pure function of identity (absolute config path, direction, and BOTH
//     client ids of the pair). Same identity, same directory, always; different
//     identity, different directory, with nothing to coordinate.
//   - A digest rather than the raw client ids, which may contain characters that
//     are not valid in a path component.
//
// Before this the two mechanisms disagreed on all three: cursors lived under a
// cwd-relative root in one file keyed by source id, trackers beside the config
// in a per-identity directory. Changing directory moved half the state.
//
// The pair, not the router alone. Reported by @neitdung: the directory was named
// from the router id while the builder was handed {router, wasm}, so holding the
// config and the router fixed and changing only COSMOS_WASM_CLIENT_ID served a
// DIFFERENT relay path out of the same directory -- the same lock, the same
// cursors, the same pending trackers. On restart the new wasm client inherits a
// cursor that skips its own older events, and pending packets from the previous
// pair replay under the new destination. "A pure function of the relay identity"
// was stated in this comment while half the identity was missing from the input.
func relayStateDir(configPath, relayKind string, ids relayClientIDs) string {
	canonical := canonicalConfigPath(configPath)
	sum := relayIdentityDigest(canonical, relayKind, ids)
	return filepath.Join(filepath.Dir(canonical), ".spectre-state", fmt.Sprintf("%s-%x", relayKind, sum[:8]))
}

// relayIdentityDigest frames each field by LENGTH rather than separating them
// with a byte.
//
// A separator is only unambiguous while no field can contain it. These fields
// come from a JSON config, so nothing stops an operator -- or a generator -- from
// putting one there, and a separator-based digest lets two distinct identities
// collide into one directory: exactly the duplicate-owner case the lock exists to
// prevent, and it would appear as two processes quietly sharing one cursor rather
// than as an error. Length framing has no such input.
func relayIdentityDigest(canonicalPath, relayKind string, ids relayClientIDs) [sha256.Size]byte {
	h := sha256.New()
	for _, field := range []string{canonicalPath, relayKind, ids.router, ids.wasm} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		h.Write(length[:])
		h.Write([]byte(field))
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// canonicalConfigPath resolves configPath to one name the filesystem agrees on,
// so two ways of naming the same file cannot produce two state directories.
//
// Both the ROOT and the HASH now come from it, which they did not: the hash used
// filepath.Abs while the root used filepath.Dir(configPath) unchanged, so a
// relative --config produced a cwd-relative root -- the exact thing the comment
// above warns against.
//
// filepath.Abs alone is not enough, because it does not resolve symlinks, and
// that is not a corner case. On macOS /var is a symlink to /private/var, so
// running `--config config.json` from the config's own directory and passing the
// same file by absolute path already produced two different digests AND two
// different roots for one config. Two state directories mean two lock files, and
// two locks that never contend mean the duplicate-owner case this file exists to
// prevent.
//
// A path that cannot be resolved falls back to the absolute form rather than
// failing: the caller has already loaded the config, so this is a filesystem
// oddity rather than a missing file, and refusing to start over it would be worse
// than a slightly less canonical name.
func canonicalConfigPath(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// Legacy state locations, kept only so startup can REFUSE them by name.
const (
	legacyTrackerDirName  = ".fast-ibc-state"
	legacyCursorDirName   = ".relayer-state"
	legacyCursorFileEnvar = "RELAYER_RECOVERY_STATE_FILE"
)

// checkLegacyStateLayout refuses to start when state from the previous layout is
// present, instead of silently ignoring it.
//
// The new binary reads neither the old tracker directory nor the old cursor file,
// so an upgrade would begin with an empty tracker and an empty cursor set and say
// nothing about it. Everything outside the startup lookback then falls out of
// timeout tracking, and a packet whose refund depended on that tracker is simply
// not refunded.
//
// It does not migrate. Nothing is deployed outside devnets, so a converter for a
// layout with no users would be machinery with no purpose -- and a converter
// never exercised on real state is a worse promise than an error. What matters is
// that the change is VISIBLE: an operator who sees this clears stale devnet state
// in one command, and one who cannot afford to is told before the relayer starts
// rather than after a packet is missed.
func checkLegacyStateLayout(configPath string) error {
	var found []string
	if path := os.Getenv(legacyCursorFileEnvar); path != "" {
		found = append(found, fmt.Sprintf("%s=%s (no longer read)", legacyCursorFileEnvar, path))
	}
	// Both parents of the config, RAW and canonical, and the raw one first.
	//
	// The previous layout joined the tracker directory onto
	// filepath.Dir(configPath) with no Abs and no EvalSymlinks (see 47bbc901), so
	// legacy state sits beside the config path AS SPELLED on the command line.
	// Canonicalising this lookup -- which the identity derivation above rightly
	// does -- looks in the wrong place whenever the two differ: an operator who
	// ran the old build through a config symlink, and upgrades through that same
	// symlink, would have the guard resolve to the target's parent, find nothing,
	// and start with an empty tracker. That is the silent upgrade loss this guard
	// exists to make loud. Found in review.
	//
	// legacyCursorDirName stays relative on purpose: the old default was the bare
	// ".relayer-state/recovery-cursors.json" (e60b69ad), resolved against the
	// process working directory, so that is where to look for it.
	candidates := []string{
		filepath.Join(filepath.Dir(configPath), legacyTrackerDirName),
		filepath.Join(filepath.Dir(canonicalConfigPath(configPath)), legacyTrackerDirName),
		legacyCursorDirName,
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		// Dedupe by RESOLVED form but report the spelling: the same directory
		// reached two ways must not be listed as two problems, and the operator is
		// best served by the name they typed.
		//
		// The key has to resolve symlinks, not merely absolutise. The first version
		// used filepath.Abs and its own dedupe test caught it: on macOS
		// t.TempDir() hands back /var/folders/... which IS a symlink to
		// /private/var/folders/..., so the raw and canonical parents were one
		// directory spelled two ways and both survived. Deduping a symlink-driven
		// pair with a function that ignores symlinks is the same mistake this
		// guard is about, one level down.
		key := candidate
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			key = resolved
		} else if abs, absErr := filepath.Abs(candidate); absErr == nil {
			// EvalSymlinks fails on a path that does not exist, which the os.Stat
			// below is about to reject anyway; Abs keeps the key stable meanwhile.
			key = filepath.Clean(abs)
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, err := os.Stat(candidate); err == nil {
			found = append(found, candidate)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(
		"state from the previous layout is still present: %s. This build keeps one state directory "+
			"per relay path under .spectre-state beside the config and does not read the old "+
			"locations, so starting would silently begin with an empty pending tracker and an empty "+
			"cursor set -- packets older than the startup lookback would stop being tracked for "+
			"timeout refunds. There is no migration: remove the listed paths (and unset %s) to start "+
			"fresh, which is what a devnet wants, or relay them out with the previous build first",
		strings.Join(found, ", "), legacyCursorFileEnvar)
}

// effectiveRelayIDs resolves the client ids a cosmos_to_eth path will actually
// RUN with, which is not always the pair written in the file: buildCosmosToEthSource
// applies ICS26_CLIENT_ID and COSMOS_WASM_CLIENT_ID over the config.
//
// The overrides apply unconditionally because a process serves one path, so the
// environment can only ever speak for the source that is running. The flag that
// used to gate them existed for a --source selection out of a multi-source
// file, where a stale variable could speak for a source the operator had not
// picked; both the selection and the file shape are gone (A7).
//
// It exists because two consumers must agree and they used to disagree. Start
// named the state directory -- the lock, the cursors, the pending trackers --
// from the RAW config id while the builder relayed the effective one. Two
// consequences, both found in review:
//
//   - change an override and restart, and the process relays a different pair
//     while reusing the previous pair's persisted cursors and pending work;
//   - two configs whose raw ids differ but whose effective ids are the same take
//     two DIFFERENT locks and both start on one relay path, which is the
//     duplicate-owner case this directory scheme exists to prevent.
//
// Deriving both from one function is what makes "the directory is a pure
// function of the relay identity" true rather than merely stated.
func effectiveRelayIDs(c2e cosmosToEthConfig) relayClientIDs {
	return relayClientIDs{
		router: envOrDefault("ICS26_CLIENT_ID", c2e.ICS26ClientID),
		wasm:   envOrDefault("COSMOS_WASM_CLIENT_ID", c2e.CosmosWasmClientID),
	}
}

// effectiveRoleManager resolves the role-manager address the same way.
//
// Getting it wrong is not cosmetic. RouterManagesProofSubmission compares this
// address with the router to decide whether proofs go through ICS26Router or
// straight to SpectreClient, so a stale value routes incident evidence down the
// wrong contract path.
//
// It lives beside effectiveRelayIDs rather than inside the builder so that
// EVERY value the environment may override for a source is decided in one place.
// The builder reading one of them itself is how this regressed once: the
// identity honoured the override rule and this did not.
func effectiveRoleManager(c2e cosmosToEthConfig) string {
	return envOrDefault("ROLE_MANAGER", c2e.ICS26Address)
}

func buildCosmosToEthSource(
	logger *zap.Logger,
	c2e cosmosToEthConfig,
	e2c ethToCosmosConfig,
	batchCfg services.BatchConfig,
	p services.Prover,
	txHandler services.TransactionHandler,
	options buildCosmosToEthSourceOptions,
) (*services.Services, services.RelayDeps, func(), error) {
	var zero services.RelayDeps

	// Everything that can be decided from config alone is decided BEFORE any
	// client is dialled or started. A validation failure below this block has no
	// resource to release; one above it would have to unwind a dialled ETH client
	// and a started Cosmos WS subscription, and the cleanup closure that knows how
	// to do that is not built until the end of this function.
	// Resolved by the CALLER, not read from the environment here: an explicitly
	// selected source must be protected from a stale process environment, and one
	// derivation of these values is what makes the state directory and the relay
	// identity the same thing (effectiveRelayIDs).
	cosmosRouterClientID, cosmosWasmClientID := options.ids.router, options.ids.wasm
	roleManager := options.roleManager
	// Guarded by the caller's DIRECTION rather than required unconditionally:
	// submit-misbehaviour on a cosmos_to_l2 source, which deliberately carries no
	// cosmos_wasm_client_id, was otherwise refused for a field it never reads.
	if options.relaysEVMToCosmos && cosmosWasmClientID == "" {
		return nil, zero, nil, fmt.Errorf(
			"cosmos_wasm_client_id is required in cosmos_to_eth config to relay eth->cosmos")
	}
	if cosmosRouterClientID == "" {
		return nil, zero, nil, fmt.Errorf("cosmos router client ID (ICS26_CLIENT_ID or ics26_client_id) is required and cannot be empty")
	}
	// SpectreClient is already deployed; this is its address, not a request to deploy.
	if c2e.SpectreClient == "" {
		return nil, zero, nil, fmt.Errorf("spectre_client address is required in cosmos_to_eth config")
	}
	if options.startSubscriptions && c2e.EthWsUrl != "" {
		if !strings.HasPrefix(c2e.EthWsUrl, "ws://") && !strings.HasPrefix(c2e.EthWsUrl, "wss://") {
			return nil, zero, nil, fmt.Errorf("eth_ws_url must use ws:// or wss://, got: %s", c2e.EthWsUrl)
		}
	}
	// Parses FETCH_TIMEOUT, so it can fail on a malformed override.
	cosmosConfig, err := buildCosmosConfig(c2e, batchCfg)
	if err != nil {
		return nil, zero, nil, err
	}

	// Connect to Ethereum (HTTP for queries)
	ethClient, err := relayerclient.DialEthRPC(context.Background(), c2e.EthRpcUrl, relayerclient.DefaultRPCTimeout)
	if err != nil {
		return nil, zero, nil, fmt.Errorf("failed to connect to Ethereum: %w", err)
	}

	// Connect to Ethereum (WS for subscriptions)
	var ethWsClient *ethclient.Client
	if options.startSubscriptions && c2e.EthWsUrl != "" {
		ethWsClient, err = relayerclient.DialEthRPC(context.Background(), c2e.EthWsUrl, relayerclient.DefaultRPCTimeout)
		if err != nil {
			ethClient.Close()
			return nil, zero, nil, fmt.Errorf("failed to connect to Ethereum WS: %w", err)
		}
	}

	// Connect to Cosmos
	cosmosClient, err := relayerclient.DialCosmosRPC(c2e.TmRpcUrl, "/websocket", relayerclient.DefaultRPCTimeout)
	if err != nil {
		ethClient.Close()
		if ethWsClient != nil {
			ethWsClient.Close()
		}
		return nil, zero, nil, fmt.Errorf("failed to create Cosmos RPC client: %w", err)
	}

	// Assemble the scoped chain dependencies, including the beacon API.
	// Set SpectreClient address (already deployed)
	if c2e.SpectreClient == "" {
		return nil, zero, nil, fmt.Errorf("spectre_client address is required in cosmos_to_eth config")
	}
	if options.startSubscriptions {
		if err := cosmosClient.Start(); err != nil {
			ethClient.Close()
			if ethWsClient != nil {
				ethWsClient.Close()
			}
			return nil, zero, nil, fmt.Errorf("failed to start Cosmos WS client: %w", err)
		}
	}

	deps := services.RelayDeps{
		Cosmos: services.CosmosEndpoint{Client: cosmosClient},
		EVM: services.EVMEndpoint{
			Client: ethClient, WSURL: c2e.EthWsUrl, BeaconAPIURL: e2c.BeaconUrl,
			Contracts: services.EVMContracts{
				Router: common.HexToAddress(c2e.ICS26Address), SignatureVerifier: common.HexToAddress(c2e.SignatureVerifier),
				Membership: common.HexToAddress(c2e.Membership), Misbehaviour: common.HexToAddress(c2e.Misbehaviour),
				UpdateClient: common.HexToAddress(c2e.UpdateClient), RoleManager: common.HexToAddress(roleManager),
				SpectreClient: common.HexToAddress(c2e.SpectreClient),
			},
		},
		IDs:    services.ClientIDs{CosmosOnEVM: cosmosRouterClientID, EVMOnCosmos: cosmosWasmClientID},
		Config: cosmosConfig, Logger: log.Default(),
	}
	cleanup := func() {
		if options.startSubscriptions {
			if err := cosmosClient.Stop(); err != nil {
				log.Printf("WARN failed to terminate cosmos client: %v", err)
			}
		}
		ethClient.Close()
		if ethWsClient != nil {
			ethWsClient.Close()
		}
	}

	if options.startSubscriptions {
		logger.Sugar().Infof("source %q: subscribing to events (spectre_client=%s tm=%s)",
			cosmosRouterClientID, c2e.SpectreClient, c2e.TmRpcUrl)
	} else {
		logger.Sugar().Infof("source %q: configured one-shot RPC context (spectre_client=%s tm=%s)",
			cosmosRouterClientID, c2e.SpectreClient, c2e.TmRpcUrl)
	}

	var recoveryState *services.RecoveryStateStore
	if options.startSubscriptions && options.relayStateDir != "" {
		recoveryState, err = services.LoadRecoveryState(
			filepath.Join(options.relayStateDir, services.RecoveryCursorsFileName))
		if err != nil {
			cleanup()
			return nil, zero, nil, fmt.Errorf("load recovery state: %w", err)
		}
	}
	var svc *services.Services
	if options.relayStateDir != "" {
		svc, err = services.NewWithPendingState(txHandler, p, cosmosConfig, options.relayStateDir, recoveryState)
		if err != nil {
			cleanup()
			return nil, zero, nil, err
		}
	} else {
		if options.startSubscriptions {
			svc = services.New(txHandler, p, cosmosConfig, recoveryState)
		} else {
			svc = services.NewOneShot(txHandler, p, cosmosConfig)
		}
	}
	return svc, deps, cleanup, nil
}

// buildCosmosConfig layers the per-source overrides from c2e (and the global
// FETCH_TIMEOUT env) onto the service defaults.
// envSecondsDuration reads an optional whole-second duration override.
//
// It returns an error rather than falling back, because falling back is what the
// old code did: `if d, err := strconv.Atoi(envVal); err == nil && d > 0` treated
// BOTH a parse failure and a non-positive value as "not set". FETCH_TIMEOUT=30s
// -- the way a Go duration is normally written, and the natural guess for a
// field that is a time.Duration -- was silently discarded, and the operator got
// the default while believing they had set a deadline.
//
// That matters more here than for a gas knob: FetchTimeout is what bounds the
// Cosmos RPC calls, and an RPC without the deadline the operator asked for is
// the failure mode that wedged a whole relay direction before.
func envSecondsDuration(name string) (time.Duration, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s: %w (whole seconds, e.g. 30)", name, err)
	}
	if seconds <= 0 {
		return 0, false, fmt.Errorf("invalid %s: %d seconds is not a usable timeout", name, seconds)
	}
	d, err := secondsToDuration(name, uint64(seconds))
	if err != nil {
		return 0, false, err
	}
	return d, true, nil
}

// maxDurationSeconds is the largest whole-second value a time.Duration can hold.
// time.Duration is an int64 of NANOseconds, so the usable range is roughly 292
// years and anything past it wraps.
const maxDurationSeconds = uint64(math.MaxInt64 / int64(time.Second))

// secondsToDuration converts operator-supplied whole seconds, refusing the ones
// that do not fit.
//
// The multiplication is the trap: strconv accepts 9223372036854775807 happily
// and `time.Duration(seconds) * time.Second` then wraps to -1s. A negative
// timeout is not merely wrong, it is wrong in two different ways depending on
// who reads it -- context.WithTimeout cancels immediately, while
// services.fetchCtx substitutes its 15s default. One accepted setting, two
// behaviours, neither the one the operator asked for.
func secondsToDuration(name string, seconds uint64) (time.Duration, error) {
	if seconds > maxDurationSeconds {
		return 0, fmt.Errorf("invalid %s: %d seconds exceeds the maximum representable duration (%d seconds)",
			name, seconds, maxDurationSeconds)
	}
	// #nosec G115 -- bounded by maxDurationSeconds immediately above
	return time.Duration(seconds) * time.Second, nil
}

func buildCosmosConfig(c2e cosmosToEthConfig, batchCfg services.BatchConfig) (services.Config, error) {
	cfg := services.DefaultConfig()
	if c2e.TrustingPeriod != 0 {
		cfg.TrustingPeriod = c2e.TrustingPeriod
	}
	if c2e.TrustLevel != "" {
		cfg.TrustLevel = c2e.TrustLevel
	}
	if c2e.ProofType != "" {
		cfg.ProofType = c2e.ProofType
	}
	if c2e.ClockDrift != 0 {
		cfg.ClockDrift = c2e.ClockDrift
	}
	if c2e.BeaconFinalityRetries != 0 {
		cfg.BeaconFinalityRetries = c2e.BeaconFinalityRetries
	}
	if c2e.AppHashWaitRetries != 0 {
		cfg.AppHashWaitRetries = c2e.AppHashWaitRetries
	}
	// Same overflow trap as the environment override below, same origin: an
	// operator typing a number. Config JSON is no safer than an env var.
	if c2e.AppHashWaitInterval != 0 {
		d, err := secondsToDuration("app_hash_wait_interval_seconds", c2e.AppHashWaitInterval)
		if err != nil {
			return services.Config{}, err
		}
		cfg.AppHashWaitInterval = d
	}
	if c2e.FetchTimeout != 0 {
		d, err := secondsToDuration("fetch_timeout", c2e.FetchTimeout)
		if err != nil {
			return services.Config{}, err
		}
		cfg.FetchTimeout = d
	}
	if c2e.RotationThreshold != "" {
		cfg.RotationThreshold = c2e.RotationThreshold
	}
	cfg.RouterDeployBlock = c2e.RouterDeployBlock
	if c2e.RefreshInterval != 0 {
		d, err := secondsToDuration("refresh_interval_seconds", c2e.RefreshInterval)
		if err != nil {
			return services.Config{}, err
		}
		cfg.RefreshInterval = d
		cfg.RefreshIntervalConfigured = true
	}
	timeout, set, err := envSecondsDuration("FETCH_TIMEOUT")
	if err != nil {
		return services.Config{}, err
	}
	if set {
		cfg.FetchTimeout = timeout
	}
	cfg.BatchConfig = batchCfg
	return cfg, nil
}
