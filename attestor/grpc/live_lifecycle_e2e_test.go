package grpc

// This suite observes a dedicated, freshly started attestor route for minutes:
// derived-root cadence, provisional-to-confirmed promotion, retention, and the
// settlement-backed feed (OP games, Arbitrum assertions). It reads the route's
// own state file and log, so it must never be pointed at a shared attestor —
// scripts/local/test_attestor_lifecycle_e2e.sh starts an isolated daemon with a
// temporary state directory and removes it afterwards.
//
// Request validation and gRPC error mapping are covered by
// server_contract_test.go and are deliberately not repeated here.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	lifecycleFamily      = "ATTESTOR_E2E_LIFECYCLE_FAMILY"
	lifecycleHead        = "ATTESTOR_E2E_LIFECYCLE_HEAD"
	lifecycleStatePath   = "ATTESTOR_E2E_LIFECYCLE_STATE_PATH"
	lifecycleLogPath     = "ATTESTOR_E2E_LIFECYCLE_LOG_PATH"
	lifecycleGapBlocks   = "ATTESTOR_E2E_LIFECYCLE_DERIVED_GAP_BLOCKS"
	lifecycleMaxDerived  = "ATTESTOR_E2E_LIFECYCLE_MAX_DERIVED_ROOTS"
	lifecycleL1RPCURL    = "ATTESTOR_E2E_LIFECYCLE_L1_RPC_URL"
	lifecycleOpNodeURL   = "ATTESTOR_E2E_LIFECYCLE_OP_NODE_RPC_URL"
	lifecycleFactory     = "ATTESTOR_E2E_LIFECYCLE_DISPUTE_GAME_FACTORY"
	lifecycleRollupCore  = "ATTESTOR_E2E_LIFECYCLE_ROLLUP_CORE"
	lifecycleTimeout     = "ATTESTOR_E2E_LIFECYCLE_TIMEOUT"
	lifecycleWatchWindow = "ATTESTOR_E2E_LIFECYCLE_DERIVED_ROOTS"

	// maxRetentionCap is the largest max_derived_roots for which L4 runs; a
	// larger cap would take hours of derived roots to exercise pruning.
	maxRetentionCap = 10

	familyOP       = "op"
	familyBase     = "base"
	familyArbitrum = "arbitrum"

	// ABI selectors (cast sig) for the read-only L1 provenance checks. They are
	// spelled out so this module does not take a go-ethereum dependency.
	selectorGameCount        = "0x4d1975b4" // gameCount()
	selectorGameAtIndex      = "0xbb8aa1fc" // gameAtIndex(uint256)
	selectorRootClaim        = "0xbcef3b55" // rootClaim()
	selectorL2SequenceNumber = "0x99735e32" // l2SequenceNumber()
	selectorL2BlockNumber    = "0x8b85902b" // l2BlockNumber()
)

type lifecycleConfig struct {
	live         liveE2EConfig
	family       string
	head         string
	statePath    string
	logPath      string
	gapBlocks    uint64
	maxDerived   uint64
	derivedRoots int
	l1RPCURL     string
	opNodeRPCURL string
	factory      string
	rollupCore   string
}

// stateEntry is the family-neutral view of one persisted feed entry. OP stores
// roots as JSON byte arrays and Arbitrum as hex strings; both decode here.
type stateEntry struct {
	L2BlockNumber uint64
	Root          []byte
	Source        string
	GameIndex     uint64
	AssertionHash []byte
	Provisional   bool
	L2BlockHash   []byte
	L1BlockNumber uint64
	Confirmed     bool
	AttestedAt    time.Time
}

type stateSnapshot struct {
	SrcChain      string
	NextL1Block   uint64
	NextGameIndex uint64
	Pending       int
	Proposals     []stateEntry
	Attested      []stateEntry
	Mismatches    int
}

// observedRoot is one frontier sample with the moment the suite saw it.
type observedRoot struct {
	root *attestorpb.AttestedRoot
	seen time.Time
}

// TestLiveAttestorFeedLifecycleE2E covers the time-dependent feed behaviour
// tracked by issue #522. Every subtest logs an EVIDENCE line pairing the gRPC
// result with the metric, log line, state-file entry, or L1/L2 RPC read that
// corroborates it.
func TestLiveAttestorFeedLifecycleE2E(t *testing.T) {
	config, enabled, err := loadLifecycleConfig()
	if !enabled {
		t.Skipf("set %s (via scripts/local/test_attestor_lifecycle_e2e.sh) to run against an isolated attestor route", lifecycleFamily)
	}
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.live.timeout)
	defer cancel()

	connection, err := googlegrpc.NewClient(
		config.live.grpcAddress,
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial attestor %s: %v", config.live.grpcAddress, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := attestorpb.NewAttestorServiceClient(connection)
	src := config.live.sourceChain

	divergenceAtStart := divergenceCount(t, ctx, config)

	if !t.Run("L0_isolated_route_identity", func(t *testing.T) {
		chain := waitForReplica(t, ctx, client, src)
		if chain.GetAttestationHead() != config.head {
			t.Fatalf("Info attestation_head = %q, want the isolated route's %q", chain.GetAttestationHead(), config.head)
		}
		state := waitForState(t, ctx, config, func(stateSnapshot) bool { return true })
		if state.SrcChain != src {
			t.Fatalf("state file %s src_chain = %q, want %q", config.statePath, state.SrcChain, src)
		}
		evidence(t, "route src=%s head=%s replica unsafe=%d safe=%d finalized=%d state=%s",
			src, chain.GetAttestationHead(), chain.GetReplicaUnsafeL2(), chain.GetReplicaSafeL2(),
			chain.GetReplicaFinalizedL2(), config.statePath)
	}) {
		return
	}

	var derived []observedRoot
	if !t.Run("L1_derived_cadence_and_watch_semantics", func(t *testing.T) {
		derived = observeDerivedCadence(t, ctx, client, config)
	}) {
		return
	}

	t.Run("L2_provisional_promotion_keeps_identity", func(t *testing.T) {
		if config.head == "finalized" {
			t.Skip("a finalized attestation head never records provisional roots; L3 covers it")
		}
		var candidate *attestorpb.AttestedRoot
		// Only a derived root: an Arbitrum assertion is provisional independently
		// of the head and is promoted on its own schedule (L6).
		for _, sample := range derived {
			if sample.root.GetProvisional() && sample.root.GetSource() == "derived" {
				candidate = sample.root
				break
			}
		}
		if candidate == nil {
			t.Fatalf("observed %d derived roots at head %s and none was provisional", len(derived), config.head)
		}
		assertPromotion(t, ctx, client, config, candidate, "derived")
		requireNoDivergence(t, ctx, config, divergenceAtStart)
	})

	t.Run("L3_finalized_head_has_no_provisional_entries", func(t *testing.T) {
		if config.head != "finalized" {
			t.Skipf("attestation head is %s; L2 covers provisional promotion", config.head)
		}
		// attestation_head gates derived roots (and OP game verdicts) only. An
		// Arbitrum assertion is resolved at the safe head and stays provisional
		// until its L1 confirmation and finalized-Nitro recheck, whatever the
		// head (attestor/arbitrum/bold/attestor.go resolveProposal), so it is
		// the one provisional entry a finalized Arbitrum route may hold.
		allowed := func(source string, provisional bool) bool {
			return !provisional || (config.family == familyArbitrum && source == "assertion")
		}
		for _, sample := range derived {
			if !allowed(sample.root.GetSource(), sample.root.GetProvisional()) {
				t.Fatalf("finalized head served provisional %s root at l2 block %d", sample.root.GetSource(), sample.root.GetL2BlockNumber())
			}
		}
		withProvisional := waitForLiveFrontier(t, ctx, client, src, true)
		confirmed := waitForLiveFrontier(t, ctx, client, src, false)
		if withProvisional.GetL2BlockNumber() < confirmed.GetL2BlockNumber() ||
			!allowed(withProvisional.GetSource(), withProvisional.GetProvisional()) {
			t.Fatalf("include_provisional frontier %+v, confirmed frontier %+v", withProvisional, confirmed)
		}
		state := readStateOrFail(t, config)
		provisionalAssertions := 0
		for _, entry := range state.Attested {
			if !allowed(entry.Source, entry.Provisional) {
				t.Fatalf("state file holds provisional %s entry at l2 block %d", entry.Source, entry.L2BlockNumber)
			}
			if entry.Provisional {
				provisionalAssertions++
			}
		}
		var proof string
		if config.family == familyArbitrum {
			// roots_provisional also counts assertions, so it is not a derived
			// signal here; every derived attestation logs its provisional flag.
			derivedProvisional := "head=finalized provisional=true"
			if strings.Contains(readLogOrFail(t, config), derivedProvisional) {
				t.Fatalf("attestor log %s records a provisional derived root at the finalized head", config.logPath)
			}
			proof = fmt.Sprintf("no %q derived log line; %d provisional assertion entries (allowed)", derivedProvisional, provisionalAssertions)
		} else {
			const metric = "spectre_op_attestor_roots_provisional_total"
			value, _ := scrapeMetricOrFail(t, ctx, config, metric)
			if value != 0 {
				t.Fatalf("%s = %v, want 0 at a finalized head", metric, value)
			}
			proof = fmt.Sprintf("%s=%v", metric, value)
		}
		evidence(t, "finalized head: %d observed roots and %d state entries have no provisional derived/game entry; frontier=%d; %s",
			len(derived), len(state.Attested), confirmed.GetL2BlockNumber(), proof)
	})

	t.Run("L4_retention_prunes_only_confirmed_derived_roots", func(t *testing.T) {
		if config.maxDerived > maxRetentionCap {
			// A small cap prunes the provisional-phase promotion candidate in the
			// same pass that confirms it, so the script runs retention only in
			// the finalized phase.
			t.Skipf("max_derived_roots=%d; retention runs on a route with a cap <= %d", config.maxDerived, maxRetentionCap)
		}
		assertRetention(t, ctx, client, config)
	})

	switch config.family {
	case familyOP, familyBase:
		t.Run("L5_op_game_ingestion_and_provenance", func(t *testing.T) {
			assertOPGames(t, ctx, client, config)
		})
	case familyArbitrum:
		t.Run("L6_arbitrum_assertion_ingestion_promotion_and_cursor", func(t *testing.T) {
			assertArbitrumAssertions(t, ctx, client, config)
		})
	}

	t.Run("L7_no_head_divergence_during_run", func(t *testing.T) {
		requireNoDivergence(t, ctx, config, divergenceAtStart)
	})
}

func loadLifecycleConfig() (lifecycleConfig, bool, error) {
	family := strings.TrimSpace(os.Getenv(lifecycleFamily))
	if family == "" {
		return lifecycleConfig{}, false, nil
	}
	live, enabled, err := loadLiveE2EConfig()
	if !enabled {
		return lifecycleConfig{}, true, fmt.Errorf("%s is required when %s is set", liveE2EGRPCAddress, lifecycleFamily)
	}
	if err != nil {
		return lifecycleConfig{}, true, err
	}
	config := lifecycleConfig{
		live:         live,
		family:       family,
		head:         strings.TrimSpace(os.Getenv(lifecycleHead)),
		statePath:    strings.TrimSpace(os.Getenv(lifecycleStatePath)),
		logPath:      strings.TrimSpace(os.Getenv(lifecycleLogPath)),
		l1RPCURL:     strings.TrimSpace(os.Getenv(lifecycleL1RPCURL)),
		opNodeRPCURL: strings.TrimSpace(os.Getenv(lifecycleOpNodeURL)),
		factory:      strings.TrimSpace(os.Getenv(lifecycleFactory)),
		rollupCore:   strings.TrimSpace(os.Getenv(lifecycleRollupCore)),
		derivedRoots: 4,
	}
	switch family {
	case familyOP, familyBase:
		if config.opNodeRPCURL == "" || config.factory == "" {
			return config, true, fmt.Errorf("%s and %s are required for %s", lifecycleOpNodeURL, lifecycleFactory, family)
		}
	case familyArbitrum:
		if config.rollupCore == "" {
			return config, true, fmt.Errorf("%s is required for arbitrum", lifecycleRollupCore)
		}
	default:
		return config, true, fmt.Errorf("%s = %q, want op, base, or arbitrum", lifecycleFamily, family)
	}
	if _, ok := runModeForHead(config.head); !ok {
		return config, true, fmt.Errorf("%s = %q, want unsafe, safe, or finalized", lifecycleHead, config.head)
	}
	if config.statePath == "" || config.logPath == "" || config.l1RPCURL == "" {
		return config, true, fmt.Errorf("%s, %s, and %s are required", lifecycleStatePath, lifecycleLogPath, lifecycleL1RPCURL)
	}
	if config.live.metricsURL == "" {
		return config, true, fmt.Errorf("%s is required: every lifecycle assertion needs metric evidence", liveE2EMetricsURL)
	}
	for name, target := range map[string]*uint64{lifecycleGapBlocks: &config.gapBlocks, lifecycleMaxDerived: &config.maxDerived} {
		value, parseErr := strconv.ParseUint(strings.TrimSpace(os.Getenv(name)), 10, 64)
		if parseErr != nil || value == 0 {
			return config, true, fmt.Errorf("%s must be a positive integer, got %q", name, os.Getenv(name))
		}
		*target = value
	}
	if value := strings.TrimSpace(os.Getenv(lifecycleWatchWindow)); value != "" {
		count, parseErr := strconv.Atoi(value)
		if parseErr != nil || count < 2 {
			return config, true, fmt.Errorf("%s must be an integer >= 2, got %q", lifecycleWatchWindow, value)
		}
		config.derivedRoots = count
	}
	// Promotion waits for L1 finality and the replica's finalized head, which
	// is minutes on a local devnet even with fast slots.
	config.live.timeout = 30 * time.Minute
	if value := strings.TrimSpace(os.Getenv(lifecycleTimeout)); value != "" {
		timeout, parseErr := time.ParseDuration(value)
		if parseErr != nil || timeout <= 0 {
			return config, true, fmt.Errorf("%s must be a positive duration, got %q", lifecycleTimeout, value)
		}
		config.live.timeout = timeout
	}
	return config, true, nil
}

func evidence(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("EVIDENCE %s: %s", t.Name(), fmt.Sprintf(format, args...))
}

func waitForReplica(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, src string) *attestorpb.ChainInfo {
	t.Helper()
	for {
		info, err := client.Info(ctx, &attestorpb.InfoRequest{})
		if err == nil {
			if chain := chainInfoForSource(info, src); chain != nil && chain.GetReplicaSeen() {
				return chain
			}
		}
		if !sleepCtx(ctx, time.Second) {
			t.Fatalf("attestor never reported replica_seen for %s (last error: %v)", src, err)
		}
	}
}

// observeDerivedCadence follows the provisional frontier until it has seen
// config.derivedRoots distinct derived heights. OP and Base serve it through
// WatchAttested (both policies at once); Arbitrum declares WatchAttested
// unimplemented, so the same frontier is polled with AttestedUpTo instead.
func observeDerivedCadence(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig) []observedRoot {
	t.Helper()
	src := config.live.sourceChain
	derivedBefore := derivedCount(t, ctx, config)

	var samples []observedRoot
	if config.family == familyArbitrum {
		stream, err := client.WatchAttested(ctx, &attestorpb.WatchAttestedRequest{SrcChain: src, IncludeProvisional: true})
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("Arbitrum WatchAttested error = %v, want Unimplemented (frontier is polled instead)", err)
		}
		samples = pollDerivedFrontier(t, ctx, client, config)
	} else {
		samples = watchDerivedFrontier(t, ctx, client, config)
	}

	var heights []uint64
	for _, sample := range samples {
		root := sample.root
		if root.GetSource() != "derived" {
			continue
		}
		if len(heights) > 0 {
			previous := heights[len(heights)-1]
			if root.GetL2BlockNumber() == previous {
				continue
			}
			if root.GetL2BlockNumber() < previous+config.gapBlocks {
				t.Fatalf("derived roots at l2 blocks %d then %d are closer than the configured gap %d",
					previous, root.GetL2BlockNumber(), config.gapBlocks)
			}
		}
		heights = append(heights, root.GetL2BlockNumber())
	}
	if len(heights) < config.derivedRoots {
		t.Fatalf("observed %d distinct derived heights, want %d", len(heights), config.derivedRoots)
	}

	// Corroborate every observed derived height in the daemon's own log, and
	// the derived total in metrics (OP) or log line count (Arbitrum).
	for _, height := range heights {
		waitForLogLine(t, ctx, config, derivedLogLine(config, height))
	}
	derivedAfter := derivedCount(t, ctx, config)
	if derivedAfter-derivedBefore < uint64(len(heights)-1) {
		t.Fatalf("%s grew by %d while %d new derived heights were observed",
			derivedCountSource(config), derivedAfter-derivedBefore, len(heights)-1)
	}
	evidence(t, "derived heights %v (gap >= %d, head=%s); log lines present; %s %d -> %d",
		heights, config.gapBlocks, config.head, derivedCountSource(config), derivedBefore, derivedAfter)
	return samples
}

// watchDerivedFrontier runs a provisional and a confirmed WatchAttested stream
// side by side and checks the documented update semantics on both.
func watchDerivedFrontier(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig) []observedRoot {
	t.Helper()
	src := config.live.sourceChain
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()

	confirmedDone := make(chan error, 1)
	confirmedCount := 0
	go func() {
		stream, err := client.WatchAttested(watchCtx, &attestorpb.WatchAttestedRequest{SrcChain: src})
		if err != nil {
			confirmedDone <- fmt.Errorf("open confirmed watch: %w", err)
			return
		}
		var previous *attestorpb.AttestedRoot
		for {
			update, err := stream.Recv()
			if err != nil {
				if watchCtx.Err() != nil {
					confirmedDone <- nil
				} else {
					confirmedDone <- fmt.Errorf("confirmed watch: %w", err)
				}
				return
			}
			root := update.GetRoot()
			if root.GetProvisional() {
				confirmedDone <- fmt.Errorf("confirmed watch streamed provisional root at %d", root.GetL2BlockNumber())
				return
			}
			if previous != nil && root.GetL2BlockNumber() < previous.GetL2BlockNumber() {
				confirmedDone <- fmt.Errorf("confirmed watch frontier regressed %d -> %d", previous.GetL2BlockNumber(), root.GetL2BlockNumber())
				return
			}
			if previous != nil && sameRootIdentity(previous, root) {
				confirmedDone <- fmt.Errorf("confirmed watch re-sent unchanged frontier %d", root.GetL2BlockNumber())
				return
			}
			previous = root
			confirmedCount++
		}
	}()

	stream, err := client.WatchAttested(watchCtx, &attestorpb.WatchAttestedRequest{SrcChain: src, IncludeProvisional: true})
	if err != nil {
		t.Fatalf("open provisional watch: %v", err)
	}
	var samples []observedRoot
	var previous *attestorpb.AttestedRoot
	distinct := map[uint64]bool{}
	for len(distinct) < config.derivedRoots {
		update, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("provisional watch after %d updates: %v", len(samples), recvErr)
		}
		root := update.GetRoot()
		assertLiveRoot(t, "watch update", root)
		if previous != nil && sameRootIdentity(previous, root) {
			t.Fatalf("provisional watch re-sent unchanged frontier at %d", root.GetL2BlockNumber())
		}
		if previous != nil && root.GetL2BlockNumber() < previous.GetL2BlockNumber() {
			// The contract allows a lower frontier only after a revocation, which
			// is a divergence the run must have recorded.
			t.Fatalf("provisional watch frontier regressed %d -> %d on a healthy devnet",
				previous.GetL2BlockNumber(), root.GetL2BlockNumber())
		}
		// Every streamed frontier is served by the unary API at its height.
		atHeight, queryErr := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
			SrcChain: src, L2BlockNumber: root.GetL2BlockNumber(), IncludeProvisional: true,
		})
		if queryErr != nil {
			t.Fatalf("AttestedRootAtOrBelow(%d): %v", root.GetL2BlockNumber(), queryErr)
		}
		if !atHeight.GetFound() || atHeight.GetRoot().GetL2BlockNumber() != root.GetL2BlockNumber() ||
			!bytes.Equal(atHeight.GetRoot().GetRoot(), root.GetRoot()) {
			t.Fatalf("watched frontier %d/%x is not what AttestedRootAtOrBelow serves: %+v",
				root.GetL2BlockNumber(), root.GetRoot(), atHeight)
		}
		samples = append(samples, observedRoot{root: root, seen: time.Now()})
		if root.GetSource() == "derived" {
			distinct[root.GetL2BlockNumber()] = true
		}
		previous = root
	}
	stopWatch()
	if err := <-confirmedDone; err != nil {
		t.Fatal(err)
	}
	evidence(t, "provisional watch: %d updates, no duplicates, monotone; confirmed watch: %d updates, never provisional",
		len(samples), confirmedCount)
	return samples
}

func pollDerivedFrontier(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig) []observedRoot {
	t.Helper()
	var samples []observedRoot
	var previous *attestorpb.AttestedRoot
	distinct := map[uint64]bool{}
	for len(distinct) < config.derivedRoots {
		response, err := client.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{
			SrcChain: config.live.sourceChain, IncludeProvisional: true,
		})
		if err != nil {
			t.Fatalf("AttestedUpTo: %v", err)
		}
		if response.GetFound() && (previous == nil || !sameRootIdentity(previous, response.GetRoot())) {
			root := response.GetRoot()
			assertLiveRoot(t, "polled frontier", root)
			if previous != nil && root.GetL2BlockNumber() < previous.GetL2BlockNumber() {
				t.Fatalf("polled frontier regressed %d -> %d on a healthy devnet",
					previous.GetL2BlockNumber(), root.GetL2BlockNumber())
			}
			samples = append(samples, observedRoot{root: root, seen: time.Now()})
			if root.GetSource() == "derived" {
				distinct[root.GetL2BlockNumber()] = true
			}
			previous = root
		}
		if !sleepCtx(ctx, time.Second) {
			t.Fatalf("saw %d distinct derived heights before %v", len(distinct), ctx.Err())
		}
	}
	return samples
}

// assertPromotion waits until the confirmed policy serves the exact entry that
// was first observed provisionally: same height, root, source, provenance and
// attested_at. A correction would change the root or attested_at; a revocation
// would make the height disappear.
func assertPromotion(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig, candidate *attestorpb.AttestedRoot, source string) {
	t.Helper()
	src := config.live.sourceChain
	height := candidate.GetL2BlockNumber()
	started := time.Now()
	for {
		response, err := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
			SrcChain: src, L2BlockNumber: height,
		})
		if err != nil {
			t.Fatalf("AttestedRootAtOrBelow(%d, confirmed): %v", height, err)
		}
		root := response.GetRoot()
		if response.GetFound() && root.GetL2BlockNumber() == height && root.GetSource() == source {
			if root.GetProvisional() {
				t.Fatalf("confirmed policy returned provisional root at %d", height)
			}
			if !bytes.Equal(root.GetRoot(), candidate.GetRoot()) {
				t.Fatalf("promotion at %d changed root %x -> %x", height, candidate.GetRoot(), root.GetRoot())
			}
			if root.GetAttestedAt() != candidate.GetAttestedAt() {
				t.Fatalf("promotion at %d changed attested_at %d -> %d (a correction, not a confirmation)",
					height, candidate.GetAttestedAt(), root.GetAttestedAt())
			}
			if !sameProvenance(root, candidate) {
				t.Fatalf("promotion at %d changed provenance %v -> %v", height, candidate.GetProvenance(), root.GetProvenance())
			}
			info, infoErr := client.Info(ctx, &attestorpb.InfoRequest{})
			if infoErr != nil {
				t.Fatalf("Info after promotion: %v", infoErr)
			}
			chain := chainInfoForSource(info, src)
			if chain == nil || chain.GetReplicaFinalizedL2() < height {
				t.Fatalf("root at %d confirmed while replica finalized head is %d", height, chain.GetReplicaFinalizedL2())
			}
			line := confirmLogLine(config, source, root)
			if line != "" {
				waitForLogLine(t, ctx, config, line)
			}
			evidence(t, "%s root at l2 block %d promoted after %s: root=%x attested_at=%d unchanged; replica finalized=%d; log %q",
				source, height, time.Since(started).Round(time.Second), root.GetRoot(), root.GetAttestedAt(),
				chain.GetReplicaFinalizedL2(), line)
			return
		}
		if response.GetFound() && root.GetL2BlockNumber() > height {
			t.Fatalf("AttestedRootAtOrBelow(%d) returned higher block %d", height, root.GetL2BlockNumber())
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("provisional %s root at %d was not promoted before %v", source, height, ctx.Err())
		}
	}
}

// assertStateSettlementPromotion verifies a settlement entry directly from the
// durable state when a same-height derived root wins the oracle-shaped gRPC
// selection. The gRPC API intentionally exposes one root per height, so a
// collision cannot make a game or assertion disappear from this coverage.
func assertStateSettlementPromotion(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig, candidate stateEntry) {
	t.Helper()
	key := settlementKey(candidate)
	started := time.Now()
	for {
		state := readStateOrFail(t, config)
		found := false
		for _, entry := range state.Attested {
			if settlementKey(entry) != key {
				continue
			}
			found = true
			if !bytes.Equal(entry.Root, candidate.Root) || !entry.AttestedAt.Equal(candidate.AttestedAt) {
				t.Fatalf("promotion changed persisted %s entry %s: root %x -> %x, attested_at %s -> %s",
					candidate.Source, key, candidate.Root, entry.Root, candidate.AttestedAt, entry.AttestedAt)
			}
			if entry.Provisional {
				break
			}
			if entry.Source == "assertion" && !entry.Confirmed {
				t.Fatalf("promoted assertion %x is not marked assertion_confirmed in state", entry.AssertionHash)
			}
			info, err := client.Info(ctx, &attestorpb.InfoRequest{})
			if err != nil {
				t.Fatalf("Info after persisted %s promotion: %v", candidate.Source, err)
			}
			chain := chainInfoForSource(info, config.live.sourceChain)
			finalized := uint64(0)
			if chain != nil {
				finalized = chain.GetReplicaFinalizedL2()
			}
			if chain == nil || finalized < entry.L2BlockNumber {
				t.Fatalf("persisted %s root at %d confirmed while replica finalized head is %d",
					candidate.Source, entry.L2BlockNumber, finalized)
			}
			line := ""
			if entry.Source == "game" {
				line = fmt.Sprintf("finalized head confirmed provisional verdict for game %d (match=true)", entry.GameIndex)
			}
			if line != "" {
				waitForLogLine(t, ctx, config, line)
			}
			evidence(t, "persisted %s root at l2 block %d promoted after %s: root=%x attested_at=%s unchanged; replica finalized=%d; log %q",
				candidate.Source, entry.L2BlockNumber, time.Since(started).Round(time.Second), entry.Root, entry.AttestedAt,
				finalized, line)
			return
		}
		if !found {
			t.Fatalf("provisional persisted %s entry %s disappeared before promotion (revoked or corrected)", candidate.Source, key)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("provisional persisted %s entry %s was not promoted before %v", candidate.Source, key, ctx.Err())
		}
	}
}

// assertRetention samples the isolated state file until pruning has happened,
// then proves through gRPC that a pruned confirmed derived height is gone while
// every retained entry, and every settlement-backed entry, is still served.
func assertRetention(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig) {
	t.Helper()
	src := config.live.sourceChain
	everConfirmed := map[uint64][]byte{}
	settlement := map[string]stateEntry{}
	var state stateSnapshot
	var pruned []uint64
	for {
		state = readStateOrFail(t, config)
		var confirmedDerived uint64
		current := map[uint64]bool{}
		for _, entry := range state.Attested {
			switch {
			case entry.Source == "derived" && !entry.Provisional:
				confirmedDerived++
				current[entry.L2BlockNumber] = true
				everConfirmed[entry.L2BlockNumber] = entry.Root
			case entry.Source != "derived":
				settlement[settlementKey(entry)] = entry
			}
		}
		if confirmedDerived > config.maxDerived {
			t.Fatalf("state file retains %d confirmed derived roots, cap is %d", confirmedDerived, config.maxDerived)
		}
		for key, entry := range settlement {
			if !stateHasSettlement(state, key) {
				t.Fatalf("%s entry at l2 block %d disappeared; retention must never prune settlement-backed roots", entry.Source, entry.L2BlockNumber)
			}
		}
		pruned = pruned[:0]
		for height := range everConfirmed {
			if !current[height] {
				pruned = append(pruned, height)
			}
		}
		if len(pruned) >= 2 {
			break
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("no confirmed derived root was pruned before %v (cap %d, %d confirmed seen)", ctx.Err(), config.maxDerived, len(everConfirmed))
		}
	}

	for _, height := range pruned {
		response, err := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{SrcChain: src, L2BlockNumber: height})
		if err != nil {
			t.Fatalf("AttestedRootAtOrBelow(pruned %d): %v", height, err)
		}
		if response.GetFound() && response.GetRoot().GetL2BlockNumber() == height && response.GetRoot().GetSource() == "derived" {
			t.Fatalf("pruned derived root at %d is still served: %+v", height, response.GetRoot())
		}
	}
	for _, entry := range state.Attested {
		if entry.Source != "derived" || entry.Provisional {
			continue
		}
		response, err := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{SrcChain: src, L2BlockNumber: entry.L2BlockNumber})
		if err != nil {
			t.Fatalf("AttestedRootAtOrBelow(retained %d): %v", entry.L2BlockNumber, err)
		}
		if !response.GetFound() || response.GetRoot().GetL2BlockNumber() != entry.L2BlockNumber {
			// A later sample may have pruned it since; only a lower answer while
			// the state still holds it would be wrong, and that is re-read here.
			if stillRetained(t, config, entry.L2BlockNumber) {
				t.Fatalf("retained derived root at %d not served: %+v", entry.L2BlockNumber, response)
			}
		}
	}

	// The OP daemon logs pruning at debug level, which its production logger
	// drops, so for both families the proof is the number of derived roots
	// ever attested exceeding what is retained; Arbitrum also logs the prune.
	derivedTotal := derivedCount(t, ctx, config)
	retained := 0
	for _, entry := range state.Attested {
		if entry.Source == "derived" {
			retained++
		}
	}
	if derivedTotal <= uint64(retained) {
		t.Fatalf("%s = %d but %d derived roots retained; nothing was pruned", derivedCountSource(config), derivedTotal, retained)
	}
	if config.family == familyArbitrum {
		waitForLogLine(t, ctx, config, "Arbitrum pruned ")
	}
	evidence(t, "cap=%d: pruned heights %v no longer served; %s=%d > %d retained; %d settlement entries retained",
		config.maxDerived, pruned, derivedCountSource(config), derivedTotal, retained, len(settlement))
}

// assertOPGames checks the game-backed feed of an OP Stack route. The OP devnet
// proposer posts a game every 60s, so at least one game is required. The Base
// devnet runs no proposer: with gameCount == 0 the expectation is an empty game
// feed and zero ingested games, never a fabricated "game" source.
func assertOPGames(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig) {
	t.Helper()
	src := config.live.sourceChain
	count, err := ethCallUint(ctx, config.l1RPCURL, config.factory, selectorGameCount, "finalized")
	if err != nil {
		t.Fatalf("DisputeGameFactory.gameCount at finalized L1: %v", err)
	}
	if count == 0 {
		if config.family == familyOP {
			t.Fatal("OP devnet factory has no finalized game; the proposer should post one every 60s")
		}
		state := readStateOrFail(t, config)
		for _, entry := range state.Attested {
			if entry.Source == "game" {
				t.Fatalf("Base feed holds a game entry at %d although the factory has no games", entry.L2BlockNumber)
			}
		}
		ingested, _ := scrapeMetricOrFail(t, ctx, config, "spectre_op_attestor_games_ingested_total")
		pending, _ := scrapeMetricOrFail(t, ctx, config, "spectre_op_attestor_pending_games")
		if ingested != 0 || pending != 0 || state.Pending != 0 {
			t.Fatalf("Base with no games: games_ingested_total=%v pending_games=%v state pending=%d, want 0", ingested, pending, state.Pending)
		}
		evidence(t, "%s factory %s gameCount=0 at finalized L1: feed is derived-only, games_ingested_total=0, pending_games=0",
			config.family, config.factory)
		return
	}

	var game stateEntry
	waitForState(t, ctx, config, func(state stateSnapshot) bool {
		for _, entry := range state.Attested {
			if entry.Source == "game" {
				game = entry
				return true
			}
		}
		return false
	})

	served, err := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
		SrcChain: src, L2BlockNumber: game.L2BlockNumber, IncludeProvisional: true,
	})
	if err != nil {
		t.Fatalf("AttestedRootAtOrBelow(game l2 %d): %v", game.L2BlockNumber, err)
	}
	root := served.GetRoot()
	if !served.GetFound() || root.GetL2BlockNumber() != game.L2BlockNumber {
		t.Fatalf("game at l2 %d not served: %+v", game.L2BlockNumber, served)
	}
	servedGame := root.GetSource() == "game"
	if servedGame {
		index, ok := root.GetProvenance().(*attestorpb.AttestedRoot_GameIndex)
		if !ok || index.GameIndex != game.GameIndex || !bytes.Equal(root.GetRoot(), game.Root) {
			t.Fatalf("served game %+v does not match state game %d root %x", root, game.GameIndex, game.Root)
		}
	} else if root.GetSource() != "derived" {
		t.Fatalf("game %d at l2 %d is persisted, but the feed serves unexpected %+v", game.GameIndex, game.L2BlockNumber, root)
	} else if !bytes.Equal(root.GetRoot(), game.Root) {
		t.Fatalf("derived root at game %d collision height %d = %x, want game root %x",
			game.GameIndex, game.L2BlockNumber, root.GetRoot(), game.Root)
	} else {
		evidence(t, "game %d at l2 %d collides with the served derived root; validating the persisted game entry directly",
			game.GameIndex, game.L2BlockNumber)
	}

	// L1 provenance: the factory entry at that index, read at finalized L1,
	// commits to exactly this root and L2 block.
	proxyWord, err := ethCall(ctx, config.l1RPCURL, config.factory, selectorGameAtIndex+uint256Arg(game.GameIndex), "finalized")
	if err != nil || len(proxyWord) < 96 {
		t.Fatalf("gameAtIndex(%d): %v (%d bytes)", game.GameIndex, err, len(proxyWord))
	}
	proxy := "0x" + hex.EncodeToString(proxyWord[64+12:96])
	claim, err := ethCall(ctx, config.l1RPCURL, proxy, selectorRootClaim, "finalized")
	if err != nil || !bytes.Equal(claim, game.Root) {
		t.Fatalf("game %d proxy %s rootClaim = %x (%v), persisted root %x", game.GameIndex, proxy, claim, err, game.Root)
	}
	l2Height, err := ethCallUint(ctx, config.l1RPCURL, proxy, selectorL2SequenceNumber, "finalized")
	if err != nil {
		l2Height, err = ethCallUint(ctx, config.l1RPCURL, proxy, selectorL2BlockNumber, "finalized")
	}
	if err != nil || l2Height != game.L2BlockNumber {
		t.Fatalf("game %d proxy %s L2 height = %d (%v), feed %d", game.GameIndex, proxy, l2Height, err, game.L2BlockNumber)
	}
	// Replica provenance: honest derivation produces the same output root.
	var output struct {
		OutputRoot string `json:"outputRoot"`
	}
	if err := liveRPC(ctx, config.opNodeRPCURL, "optimism_outputAtBlock", []any{fmt.Sprintf("0x%x", game.L2BlockNumber)}, &output); err != nil {
		t.Fatalf("optimism_outputAtBlock(%d): %v", game.L2BlockNumber, err)
	}
	if replicaRoot, err := decodeExactHex(output.OutputRoot, 32); err != nil || !bytes.Equal(replicaRoot, game.Root) {
		t.Fatalf("replica output root at %d = %s (%v), persisted game root %x", game.L2BlockNumber, output.OutputRoot, err, game.Root)
	}

	ingested, _ := scrapeMetricOrFail(t, ctx, config, "spectre_op_attestor_games_ingested_total")
	matched, _ := scrapeMetricOrFail(t, ctx, config, "spectre_op_attestor_roots_matched_total")
	mismatched, _ := scrapeMetricOrFail(t, ctx, config, "spectre_op_attestor_roots_mismatched_total")
	if ingested < 1 || matched < 1 || mismatched != 0 {
		t.Fatalf("games_ingested_total=%v roots_matched_total=%v roots_mismatched_total=%v, want >=1, >=1, 0", ingested, matched, mismatched)
	}
	line := fmt.Sprintf("attested game %d (l2 block %d", game.GameIndex, game.L2BlockNumber)
	waitForLogLine(t, ctx, config, line)
	evidence(t, "game %d proxy %s: persisted root %x at l2 %d == L1 rootClaim == replica outputAtBlock; served_by_grpc=%t; provisional=%t; games_ingested=%v matched=%v mismatched=%v",
		game.GameIndex, proxy, game.Root, game.L2BlockNumber, servedGame, game.Provisional, ingested, matched, mismatched)

	if game.Provisional {
		if servedGame {
			assertPromotion(t, ctx, client, config, root, "game")
		}
		assertStateSettlementPromotion(t, ctx, client, config, game)
	}
}

// assertArbitrumAssertions checks assertion ingestion, promotion, and that the
// durable L1 cursor never runs past finalized L1.
func assertArbitrumAssertions(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, config lifecycleConfig) {
	t.Helper()
	src := config.live.sourceChain
	const cursorMetric = "spectre_arbitrum_attestor_assertion_next_l1_block"

	checkCursor := func() (uint64, uint64) {
		cursor, _ := scrapeMetricOrFail(t, ctx, config, cursorMetric)
		state := readStateOrFail(t, config)
		// Read finalized L1 after the cursor: finality only advances, so a
		// cursor read first can never be legitimately ahead of it.
		finalized, err := l1FinalizedBlock(ctx, config.l1RPCURL)
		if err != nil {
			t.Fatalf("read finalized L1 block: %v", err)
		}
		if uint64(cursor) > finalized+1 || state.NextL1Block > finalized+1 {
			t.Fatalf("assertion cursor metric=%v state=%d is beyond finalized L1 %d + 1", cursor, state.NextL1Block, finalized)
		}
		for _, entry := range append(append([]stateEntry(nil), state.Proposals...), state.Attested...) {
			if entry.L1BlockNumber > finalized {
				t.Fatalf("assertion %x ingested from L1 block %d above finalized %d", entry.AssertionHash, entry.L1BlockNumber, finalized)
			}
		}
		return uint64(cursor), finalized
	}

	initialCursor, _ := checkCursor()
	var assertion stateEntry
	for {
		checkCursor()
		state := readStateOrFail(t, config)
		found := false
		for _, entry := range state.Attested {
			if entry.Source == "assertion" {
				assertion = entry
				found = true
				break
			}
		}
		if found {
			break
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("no assertion attested before %v", ctx.Err())
		}
	}

	served, err := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
		SrcChain: src, L2BlockNumber: assertion.L2BlockNumber, IncludeProvisional: true,
	})
	if err != nil {
		t.Fatalf("AttestedRootAtOrBelow(assertion l2 %d): %v", assertion.L2BlockNumber, err)
	}
	root := served.GetRoot()
	if !served.GetFound() || root.GetL2BlockNumber() != assertion.L2BlockNumber || !bytes.Equal(root.GetRoot(), assertion.Root) {
		t.Fatalf("assertion at l2 %d not served: %+v", assertion.L2BlockNumber, served)
	}
	servedAssertion := root.GetSource() == "assertion"
	if servedAssertion {
		if !bytes.Equal(root.GetAssertionHash(), assertion.AssertionHash) {
			t.Fatalf("served assertion hash %x, state %x", root.GetAssertionHash(), assertion.AssertionHash)
		}
	} else if root.GetSource() != "derived" {
		t.Fatalf("assertion %x at l2 %d is persisted, but the feed serves unexpected %+v", assertion.AssertionHash, assertion.L2BlockNumber, root)
	} else {
		evidence(t, "assertion %x at l2 %d collides with the served derived root; validating the persisted assertion directly",
			assertion.AssertionHash, assertion.L2BlockNumber)
	}

	// L1 provenance: RollupCore emitted an event for this assertion hash in the
	// recorded finalized L1 block.
	var logs []json.RawMessage
	filter := map[string]any{
		"fromBlock": fmt.Sprintf("0x%x", assertion.L1BlockNumber),
		"toBlock":   fmt.Sprintf("0x%x", assertion.L1BlockNumber),
		"address":   config.rollupCore,
		"topics":    []any{nil, "0x" + hex.EncodeToString(assertion.AssertionHash)},
	}
	if err := liveRPC(ctx, config.l1RPCURL, "eth_getLogs", []any{filter}, &logs); err != nil || len(logs) == 0 {
		t.Fatalf("RollupCore %s has no log for assertion %x in L1 block %d (%v)", config.rollupCore, assertion.AssertionHash, assertion.L1BlockNumber, err)
	}
	// L2 provenance: the committed block hash is canonical on Nitro and its
	// header carries the served state root.
	header, err := getL2Header(ctx, config.live.l2RPCURL, assertion.L2BlockNumber)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(header.hash, assertion.L2BlockHash) || !bytes.Equal(header.stateRoot, assertion.Root) {
		t.Fatalf("Nitro block %d hash=%x root=%x, assertion commits hash=%x root=%x",
			assertion.L2BlockNumber, header.hash, header.stateRoot, assertion.L2BlockHash, assertion.Root)
	}
	evidence(t, "assertion %x from L1 block %d (log present) commits Nitro block %d hash=%x root=%x; served_by_grpc=%t; provisional=%t",
		assertion.AssertionHash, assertion.L1BlockNumber, assertion.L2BlockNumber, header.hash, assertion.Root, servedAssertion, assertion.Provisional)

	if assertion.Provisional {
		if servedAssertion {
			assertPromotion(t, ctx, client, config, root, "assertion")
		}
		assertStateSettlementPromotion(t, ctx, client, config, assertion)
	}

	// The cursor must keep following finalized L1 (one epoch is ~64s on the
	// devnet), never jumping past it.
	finalCursor, finalized := checkCursor()
	for finalCursor <= initialCursor {
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("assertion cursor did not advance past %d before %v", initialCursor, ctx.Err())
		}
		finalCursor, finalized = checkCursor()
	}
	pending, _ := scrapeMetricOrFail(t, ctx, config, "spectre_arbitrum_attestor_assertions_pending")
	evidence(t, "%s %d -> %d, always <= finalized L1 + 1 (finalized=%d); assertions_pending=%v",
		cursorMetric, initialCursor, finalCursor, finalized, pending)
}

func requireNoDivergence(t *testing.T, ctx context.Context, config lifecycleConfig, before float64) {
	t.Helper()
	now := divergenceCount(t, ctx, config)
	if now != before {
		t.Fatalf("%s changed %v -> %v: a provisional root was corrected during the run", divergenceMetric(config), before, now)
	}
	evidence(t, "%s stayed at %v", divergenceMetric(config), now)
}

func divergenceCount(t *testing.T, ctx context.Context, config lifecycleConfig) float64 {
	t.Helper()
	value, _ := scrapeMetricOrFail(t, ctx, config, divergenceMetric(config))
	return value
}

func divergenceMetric(config lifecycleConfig) string {
	if config.family == familyArbitrum {
		return "spectre_arbitrum_attestor_finalized_divergence_total"
	}
	return "spectre_op_attestor_head_divergence_total"
}

// derivedCount is how many derived attestations the route has made since it
// started. The OP daemon exports a counter; the Arbitrum daemon has none, so
// its log lines are counted instead.
func derivedCount(t *testing.T, ctx context.Context, config lifecycleConfig) uint64 {
	t.Helper()
	if config.family == familyArbitrum {
		return uint64(strings.Count(readLogOrFail(t, config), "Arbitrum derived root attested: "))
	}
	value, _ := scrapeMetricOrFail(t, ctx, config, "spectre_op_attestor_roots_derived_total")
	return uint64(value)
}

func derivedCountSource(config lifecycleConfig) string {
	if config.family == familyArbitrum {
		return "log count of \"Arbitrum derived root attested\""
	}
	return "spectre_op_attestor_roots_derived_total"
}

func derivedLogLine(config lifecycleConfig, height uint64) string {
	if config.family == familyArbitrum {
		return fmt.Sprintf("Arbitrum derived root attested: l2_block=%d ", height)
	}
	return fmt.Sprintf("derived root attested at l2 block %d ", height)
}

func confirmLogLine(config lifecycleConfig, source string, root *attestorpb.AttestedRoot) string {
	switch {
	case source == "derived" && config.family == familyArbitrum:
		return fmt.Sprintf("Arbitrum finalized head confirmed derived root: l2_block=%d ", root.GetL2BlockNumber())
	case source == "derived":
		return fmt.Sprintf("finalized head confirmed derived root at l2 block %d", root.GetL2BlockNumber())
	case source == "game":
		return fmt.Sprintf("finalized head confirmed provisional verdict for game %d (match=true)", root.GetGameIndex())
	default:
		// Assertion promotion is silent in the log; state assertion_confirmed
		// and the confirmed gRPC policy are its evidence.
		return ""
	}
}

func sameRootIdentity(a, b *attestorpb.AttestedRoot) bool {
	return a.GetL2BlockNumber() == b.GetL2BlockNumber() && a.GetProvisional() == b.GetProvisional() &&
		bytes.Equal(a.GetRoot(), b.GetRoot())
}

func sameProvenance(a, b *attestorpb.AttestedRoot) bool {
	return a.GetSource() == b.GetSource() && a.GetGameIndex() == b.GetGameIndex() &&
		bytes.Equal(a.GetAssertionHash(), b.GetAssertionHash())
}

func settlementKey(entry stateEntry) string {
	return fmt.Sprintf("%s/%d/%d/%x", entry.Source, entry.L2BlockNumber, entry.GameIndex, entry.AssertionHash)
}

func stateHasSettlement(state stateSnapshot, key string) bool {
	for _, entry := range state.Attested {
		if entry.Source != "derived" && settlementKey(entry) == key {
			return true
		}
	}
	return false
}

func stillRetained(t *testing.T, config lifecycleConfig, height uint64) bool {
	t.Helper()
	for _, entry := range readStateOrFail(t, config).Attested {
		if entry.Source == "derived" && entry.L2BlockNumber == height {
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// ---------------------------------------------------------------- state file

func waitForState(t *testing.T, ctx context.Context, config lifecycleConfig, ready func(stateSnapshot) bool) stateSnapshot {
	t.Helper()
	for {
		state, err := readState(config)
		if err == nil && ready(state) {
			return state
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("state file %s never reached the expected condition (last error: %v)", config.statePath, err)
		}
	}
}

func readStateOrFail(t *testing.T, config lifecycleConfig) stateSnapshot {
	t.Helper()
	state, err := readState(config)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func readState(config lifecycleConfig) (stateSnapshot, error) {
	data, err := os.ReadFile(config.statePath)
	if err != nil {
		return stateSnapshot{}, fmt.Errorf("read state file: %w", err)
	}
	type rawEntry struct {
		L2BlockNumber      uint64          `json:"l2_block_number"`
		Root               json.RawMessage `json:"root"`
		Source             string          `json:"source"`
		GameIndex          uint64          `json:"game_index"`
		AssertionHash      json.RawMessage `json:"assertion_hash"`
		Provisional        bool            `json:"provisional"`
		L2BlockHash        json.RawMessage `json:"l2_block_hash"`
		L1BlockNumber      uint64          `json:"l1_block_number"`
		AssertionConfirmed bool            `json:"assertion_confirmed"`
		Confirmed          bool            `json:"confirmed"`
		AttestedAt         time.Time       `json:"attested_at"`
	}
	var raw struct {
		SrcChain      string     `json:"src_chain"`
		NextL1Block   uint64     `json:"next_l1_block"`
		NextGameIndex uint64     `json:"next_game_index"`
		Pending       []rawEntry `json:"pending"`
		Proposals     []rawEntry `json:"proposals"`
		Attested      []rawEntry `json:"attested"`
		Mismatches    []any      `json:"mismatches"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return stateSnapshot{}, fmt.Errorf("decode state file %s: %w", config.statePath, err)
	}
	convert := func(entries []rawEntry) ([]stateEntry, error) {
		out := make([]stateEntry, 0, len(entries))
		for _, entry := range entries {
			root, err := decodeStateBytes(entry.Root)
			if err != nil {
				return nil, fmt.Errorf("root at %d: %w", entry.L2BlockNumber, err)
			}
			assertionHash, err := decodeStateBytes(entry.AssertionHash)
			if err != nil {
				return nil, fmt.Errorf("assertion hash at %d: %w", entry.L2BlockNumber, err)
			}
			blockHash, err := decodeStateBytes(entry.L2BlockHash)
			if err != nil {
				return nil, fmt.Errorf("block hash at %d: %w", entry.L2BlockNumber, err)
			}
			source := entry.Source
			if source == "" && config.family != familyArbitrum {
				source = "game" // legacy OP state files omitted the source
			}
			out = append(out, stateEntry{
				L2BlockNumber: entry.L2BlockNumber,
				Root:          root,
				Source:        source,
				GameIndex:     entry.GameIndex,
				AssertionHash: assertionHash,
				Provisional:   entry.Provisional,
				L2BlockHash:   blockHash,
				L1BlockNumber: entry.L1BlockNumber,
				Confirmed:     entry.AssertionConfirmed || entry.Confirmed,
				AttestedAt:    entry.AttestedAt,
			})
		}
		return out, nil
	}
	attested, err := convert(raw.Attested)
	if err != nil {
		return stateSnapshot{}, err
	}
	proposals, err := convert(raw.Proposals)
	if err != nil {
		return stateSnapshot{}, err
	}
	return stateSnapshot{
		SrcChain:      raw.SrcChain,
		NextL1Block:   raw.NextL1Block,
		NextGameIndex: raw.NextGameIndex,
		Pending:       len(raw.Pending),
		Proposals:     proposals,
		Attested:      attested,
		Mismatches:    len(raw.Mismatches),
	}, nil
}

// decodeStateBytes accepts a JSON byte array ([32]byte in OP state) or a hex
// string (common.Hash in Arbitrum state). Absent fields decode to nil.
func decodeStateBytes(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var values []int
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, err
		}
		out := make([]byte, len(values))
		for index, value := range values {
			if value < 0 || value > 255 {
				return nil, fmt.Errorf("byte %d out of range: %d", index, value)
			}
			out[index] = byte(value)
		}
		return out, nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(text, "0x"))
}

// waitForLogLine polls the daemon log: a line is written just after the store
// commit that already made the entry visible over gRPC.
func waitForLogLine(t *testing.T, ctx context.Context, config lifecycleConfig, line string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(readLogOrFail(t, config), line) {
		if time.Now().After(deadline) || !sleepCtx(ctx, time.Second) {
			t.Fatalf("attestor log %s has no line containing %q", config.logPath, line)
		}
	}
}

func readLogOrFail(t *testing.T, config lifecycleConfig) string {
	t.Helper()
	data, err := os.ReadFile(config.logPath)
	if err != nil {
		t.Fatalf("read attestor log: %v", err)
	}
	return string(data)
}

// ------------------------------------------------------------------- metrics

// scrapeMetricOrFail returns the sample for name labelled with this route's
// chain. An absent sample reads as 0 (counter vectors appear only after their
// first increment); found reports whether it was present.
func scrapeMetricOrFail(t *testing.T, ctx context.Context, config lifecycleConfig, name string) (float64, bool) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, config.live.metricsURL, nil)
	if err != nil {
		t.Fatalf("build metrics request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET metrics: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status %s: %v", response.Status, err)
	}
	value, found, err := metricSample(string(body), name, config.live.sourceChain)
	if err != nil {
		t.Fatal(err)
	}
	return value, found
}

func metricSample(body, name, chain string) (float64, bool, error) {
	label := fmt.Sprintf(`chain="%s"`, chain)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") || !strings.Contains(line, label) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, false, fmt.Errorf("parse metric line %q: %w", line, err)
		}
		return value, true, nil
	}
	return 0, false, nil
}

// --------------------------------------------------------------- L1/L2 reads

func liveRPC(ctx context.Context, url, method string, params []any, result any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer response.Body.Close()
	var decoded struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if decoded.Error != nil {
		return fmt.Errorf("%s error %d: %s", method, decoded.Error.Code, decoded.Error.Message)
	}
	if len(decoded.Result) == 0 || string(decoded.Result) == "null" {
		return errors.New(method + " returned null")
	}
	return json.Unmarshal(decoded.Result, result)
}

func ethCall(ctx context.Context, url, to, data, block string) ([]byte, error) {
	var result string
	call := map[string]any{"to": to, "data": data}
	if err := liveRPC(ctx, url, "eth_call", []any{call, block}, &result); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(result, "0x"))
}

func ethCallUint(ctx context.Context, url, to, data, block string) (uint64, error) {
	word, err := ethCall(ctx, url, to, data, block)
	if err != nil {
		return 0, err
	}
	if len(word) != 32 {
		return 0, fmt.Errorf("eth_call returned %d bytes, want one word", len(word))
	}
	value := new(big.Int).SetBytes(word)
	if !value.IsUint64() {
		return 0, fmt.Errorf("value %s overflows uint64", value)
	}
	return value.Uint64(), nil
}

func uint256Arg(value uint64) string {
	return fmt.Sprintf("%064x", value)
}

func l1FinalizedBlock(ctx context.Context, url string) (uint64, error) {
	var header struct {
		Number string `json:"number"`
	}
	if err := liveRPC(ctx, url, "eth_getBlockByNumber", []any{"finalized", false}, &header); err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(header.Number, "0x"), 16, 64)
}

// TestLifecycleEvidenceParsers pins the two pure parsers the live suite relies
// on, so a format slip fails here rather than as a confusing live timeout.
func TestLifecycleEvidenceParsers(t *testing.T) {
	body := strings.Join([]string{
		`# HELP spectre_op_attestor_roots_derived_total Self-derived output roots.`,
		`spectre_op_attestor_roots_derived_total{chain="other"} 9`,
		`spectre_op_attestor_roots_derived_total{chain="opdev"} 7`,
		`spectre_op_attestor_roots_derived_total_extra{chain="opdev"} 1`,
	}, "\n")
	if value, found, err := metricSample(body, "spectre_op_attestor_roots_derived_total", "opdev"); err != nil || !found || value != 7 {
		t.Fatalf("metricSample = %v, %t, %v; want 7, true, nil", value, found, err)
	}
	if value, found, err := metricSample(body, "spectre_op_attestor_head_divergence_total", "opdev"); err != nil || found || value != 0 {
		t.Fatalf("absent metric = %v, %t, %v; want 0, false, nil", value, found, err)
	}

	asArray, err := decodeStateBytes(json.RawMessage(`[1,2,255]`))
	if err != nil || !bytes.Equal(asArray, []byte{1, 2, 255}) {
		t.Fatalf("OP byte array = %x, %v", asArray, err)
	}
	asHex, err := decodeStateBytes(json.RawMessage(`"0x01ff"`))
	if err != nil || !bytes.Equal(asHex, []byte{1, 255}) {
		t.Fatalf("Arbitrum hex = %x, %v", asHex, err)
	}
	if absent, err := decodeStateBytes(nil); err != nil || absent != nil {
		t.Fatalf("absent = %x, %v", absent, err)
	}
	if _, err := decodeStateBytes(json.RawMessage(`[256]`)); err == nil {
		t.Fatal("out-of-range byte decoded")
	}
}
