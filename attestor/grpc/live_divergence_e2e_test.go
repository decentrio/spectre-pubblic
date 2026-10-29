package grpc

// This suite drives the attestor's divergence alarms against a live devnet
// (issue #533). Like the fault suite it owns disposable daemons on their own
// ports and state files; the shared attestor and the relayer are never touched.
//
//   D1 (OP)       a real on-chain invalid game: a respected-type game whose
//                 rootClaim is not the replica's output root, created with the
//                 devnet proposer's key. The game stays on L1.
//   D2 (OP)       provisional correction, game and derived paths.
//   D3 (Arbitrum) provisional correction, derived path.
//
// D2 and D3 need the unsafe chain to disagree with what finality later
// settles. The stock Kurtosis sequencers cannot be made to reorg their unsafe
// head on demand, so the divergence is injected one layer down: the route's
// replica RPC goes through an in-process JSON-RPC interposer that, while
// "holding", pins the finalized head it reports at the value seen when the
// hold began and answers every block above it with a deterministic forged
// root. On release it forwards honest answers again, so the daemon's own
// finalized recheck is what discovers the contradiction. The daemon binary,
// its store, metrics, logs and gRPC feed are exercised exactly as deployed;
// only the replica's answers are simulated.
//
// scripts/local/test_attestor_divergence_e2e.sh supplies the environment.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	attestorpb "attestor/types/attestor"
)

const (
	divergenceFamily      = "ATTESTOR_DIVERGENCE_E2E_FAMILY"
	divergenceBinary      = "ATTESTOR_DIVERGENCE_E2E_BINARY"
	divergenceWorkDir     = "ATTESTOR_DIVERGENCE_E2E_WORK_DIR"
	divergenceGRPCPort    = "ATTESTOR_DIVERGENCE_E2E_GRPC_PORT"
	divergenceMetrics     = "ATTESTOR_DIVERGENCE_E2E_METRICS_PORT"
	divergenceTimeout     = "ATTESTOR_DIVERGENCE_E2E_TIMEOUT"
	divergenceDrills      = "ATTESTOR_DIVERGENCE_E2E_DRILLS"
	divergenceProposerKey = "ATTESTOR_DIVERGENCE_E2E_PROPOSER_KEY"

	selectorInitBonds = "0x6593dc6e" // initBonds(uint32)
	selectorGames     = "0x5f0150cb" // games(uint32,bytes32,bytes)

	opDivergenceMetric        = "spectre_op_attestor_head_divergence_total"
	opMismatchMetric          = "spectre_op_attestor_roots_mismatched_total"
	arbitrumDivergenceMetric  = "spectre_arbitrum_attestor_finalized_divergence_total"
	opDerivedDivergenceLine   = "HEAD DIVERGENCE — finalized derivation contradicts provisional derived root"
	opRecheckDivergenceLine   = "HEAD DIVERGENCE — finalized recheck contradicts provisional verdict"
	opMismatchHookLine        = "OUTPUT ROOT MISMATCH"
	arbitrumDivergenceLogLine = "Arbitrum HEAD DIVERGENCE: l2_block="
)

// TestLiveAttestorDivergenceE2E covers issue #533. Every drill logs EVIDENCE
// lines pairing the gRPC verdict with the metric delta and log line.
func TestLiveAttestorDivergenceE2E(t *testing.T) {
	env, enabled, err := loadRouteEnv(divergenceFamily, divergenceBinary, divergenceWorkDir, divergenceGRPCPort, divergenceMetrics)
	if !enabled {
		t.Skipf("set %s (via scripts/local/test_attestor_divergence_e2e.sh) to run the divergence drills", divergenceFamily)
	}
	if err != nil {
		t.Fatal(err)
	}
	env.timeout = time.Hour
	if value := strings.TrimSpace(os.Getenv(divergenceTimeout)); value != "" {
		if env.timeout, err = time.ParseDuration(value); err != nil || env.timeout <= 0 {
			t.Fatalf("%s must be a positive duration, got %q", divergenceTimeout, value)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), env.timeout)
	defer cancel()

	selected := func(name string) bool {
		drills := strings.TrimSpace(os.Getenv(divergenceDrills))
		return drills == "" || drills == "all" || strings.Contains(","+drills+",", ","+name+",")
	}
	switch env.family {
	case familyOP:
		if selected("invalid-game") {
			t.Run("D1_invalid_game_root_claim", func(t *testing.T) {
				assertInvalidGameRoot(t, ctx, env)
			})
		}
		if selected("correction") {
			t.Run("D2_op_provisional_correction", func(t *testing.T) {
				assertOPProvisionalCorrection(t, ctx, env)
			})
		}
	case familyArbitrum:
		if selected("correction") {
			t.Run("D3_arbitrum_provisional_correction", func(t *testing.T) {
				assertArbitrumProvisionalCorrection(t, ctx, env)
			})
		}
	default:
		t.Fatalf("divergence drills run on op or arbitrum, not %q", env.family)
	}
}

// ------------------------------------------------------------ D1 invalid game

func assertInvalidGameRoot(t *testing.T, ctx context.Context, env faultEnv) {
	key := strings.TrimSpace(os.Getenv(divergenceProposerKey))
	if key == "" {
		t.Fatalf("%s is required: the factory only accepts games from the devnet proposer", divergenceProposerKey)
	}
	// A finalized-head route decides the game non-provisionally, so the
	// verdict is terminal and no recheck can rewrite it during the drill.
	route := startFaultRoute(t, ctx, env, "invalid-game", "finalized", env.grpcPort, env.metricsPort)
	route.waitFrontierAbove(t, ctx, 0)

	target := replicaHeads(t, ctx, env.opNodeRPC).finalized
	honest := replicaOutput(t, ctx, env.opNodeRPC, target)
	claim := forgeWord(append([]byte("invalid-game"), honest.output...))
	mismatchedBefore := route.metric(t, ctx, opMismatchMetric)
	offset := route.logOffset(t)

	gameIndex, proxy := createGame(t, ctx, env, key, claim, target)
	evidence(t, "created game %d (%s) at l2 %d: rootClaim %x, replica outputAtBlock %x", gameIndex, proxy, target, claim, honest.output)

	mismatch := waitForOPMismatch(t, ctx, route, func(m opMismatch) bool { return m.GameIndex == gameIndex })
	if !bytes.Equal(mismatch.RootClaim, claim) || !bytes.Equal(mismatch.LocalRoot, honest.output) || mismatch.Provisional {
		t.Fatalf("mismatch record %+v, want claim %x local %x non-provisional", mismatch, claim, honest.output)
	}
	mismatchedAfter := route.metric(t, ctx, opMismatchMetric)
	if mismatchedAfter-mismatchedBefore != 1 {
		t.Fatalf("%s %v -> %v, want +1 for game %d", opMismatchMetric, mismatchedBefore, mismatchedAfter, gameIndex)
	}
	hook := route.waitForLogLineWith(t, ctx, offset, opMismatchHookLine,
		fmt.Sprintf(`"game_index":%d`, gameIndex), "0x"+hex.EncodeToString(claim))

	// The feed never carries the claim: no state entry, and the gRPC feed at
	// the game's height serves the honest root (or a lower one), never it.
	for _, entry := range readStateOrFail(t, route.view).Attested {
		if bytes.Equal(entry.Root, claim) || (entry.Source == "game" && entry.GameIndex == gameIndex) {
			t.Fatalf("feed holds %+v for the invalid game %d", entry, gameIndex)
		}
	}
	served, err := route.client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
		SrcChain: env.srcChain, L2BlockNumber: target, IncludeProvisional: true,
	})
	if err != nil {
		t.Fatalf("AttestedRootAtOrBelow(%d): %v", target, err)
	}
	if bytes.Equal(served.GetRoot().GetRoot(), claim) ||
		(served.GetRoot().GetL2BlockNumber() == target && !bytes.Equal(served.GetRoot().GetRoot(), honest.output)) {
		t.Fatalf("feed at %d serves %+v, want never the claim %x", target, served.GetRoot(), claim)
	}

	// VerifyStateRoot is asked to sign the claimed value as the block's state
	// root and must refuse; the honest header at the same height signs, so the
	// refusal is about the root, not about the block being unavailable.
	header, err := getL2Header(ctx, env.l2RPC, target)
	if err != nil {
		t.Fatal(err)
	}
	forged := l2Header{number: target, hash: header.hash, stateRoot: claim}
	response, err := route.verifyAt(ctx, forged)
	if err != nil {
		t.Fatalf("VerifyStateRoot(claim at %d): %v", target, err)
	}
	assertMismatchedVerdict(t, header, response)
	honestVerdict, err := route.verifyAt(ctx, header)
	if err != nil {
		t.Fatalf("VerifyStateRoot(honest at %d): %v", target, err)
	}
	assertCanonicalSignedVerdict(t, route.verifyConfig(t), header, route.setHash(t), honestVerdict)

	evidence(t, "D1 gRPC: VerifyStateRoot(block %d, state_root=claim) valid=%t signature=%dB canonical_state_root=%x; honest header valid=%t signature=%dB; AttestedRootAtOrBelow(%d) = %s@%d %x",
		target, response.GetValid(), len(response.GetAttestationSignature()), response.GetStateRoot(),
		honestVerdict.GetValid(), len(honestVerdict.GetAttestationSignature()),
		target, served.GetRoot().GetSource(), served.GetRoot().GetL2BlockNumber(), served.GetRoot().GetRoot())
	evidence(t, "D1 metric: %s %v -> %v; state mismatch game=%d claim=%x local=%x provisional=%t",
		opMismatchMetric, mismatchedBefore, mismatchedAfter, mismatch.GameIndex, mismatch.RootClaim, mismatch.LocalRoot, mismatch.Provisional)
	evidence(t, "D1 log: %s", hook)
}

// createGame sends DisputeGameFactory.create with cast (Foundry is a repo
// prerequisite; this module deliberately has no go-ethereum signer) and
// returns the new game's factory index and proxy address.
func createGame(t *testing.T, ctx context.Context, env faultEnv, key string, claim []byte, l2Block uint64) (uint64, string) {
	t.Helper()
	gameType, err := strconv.ParseUint(env.gameType, 10, 32)
	if err != nil {
		t.Fatalf("RESPECTED_GAME_TYPE %q: %v", env.gameType, err)
	}
	bond, err := ethCall(ctx, env.l1RPC, env.factory, selectorInitBonds+uint256Arg(gameType), "latest")
	if err != nil || len(bond) != 32 {
		t.Fatalf("initBonds(%d): %v", gameType, err)
	}
	extraData := "0x" + uint256Arg(l2Block)
	command := exec.CommandContext(ctx, "cast", "send", env.factory, "create(uint32,bytes32,bytes)",
		strconv.FormatUint(gameType, 10), "0x"+hex.EncodeToString(claim), extraData,
		"--value", "0x"+hex.EncodeToString(bytes.TrimLeft(bond, "\x00")),
		"--private-key", key, "--rpc-url", env.l1RPC, "--json")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("cast send create: %v\n%s", err, output)
	}
	var receipt struct {
		Status          string `json:"status"`
		TransactionHash string `json:"transactionHash"`
	}
	if err := json.Unmarshal(output, &receipt); err != nil || receipt.Status != "0x1" {
		t.Fatalf("create receipt %s (%v), want status 0x1", output, err)
	}

	// games(type, claim, extraData) -> (proxy, timestamp); then find its index.
	call := selectorGames + uint256Arg(gameType) + hex.EncodeToString(claim) + uint256Arg(0x60) + uint256Arg(32) + uint256Arg(l2Block)
	word, err := ethCall(ctx, env.l1RPC, env.factory, call, "latest")
	if err != nil || len(word) < 32 {
		t.Fatalf("games(): %v", err)
	}
	proxy := "0x" + hex.EncodeToString(word[12:32])
	count, err := ethCallUint(ctx, env.l1RPC, env.factory, selectorGameCount, "latest")
	if err != nil {
		t.Fatal(err)
	}
	for index := count; index > 0 && count-index < 64; index-- {
		entry, err := ethCall(ctx, env.l1RPC, env.factory, selectorGameAtIndex+uint256Arg(index-1), "latest")
		if err == nil && len(entry) >= 96 && "0x"+hex.EncodeToString(entry[64+12:96]) == proxy {
			evidence(t, "create tx %s mined; game index %d", receipt.TransactionHash, index-1)
			return index - 1, proxy
		}
	}
	t.Fatalf("game proxy %s not found among the last 64 factory entries", proxy)
	return 0, ""
}

// ------------------------------------------------- D2 OP provisional correction

func assertOPProvisionalCorrection(t *testing.T, ctx context.Context, env faultEnv) {
	injector := &divergence{family: familyOP}
	interposer := newRPCInterposer(t, env.opNodeRPC, injector.rewrite)
	routeEnv := env
	routeEnv.opNodeRPC = interposer.url
	route := startFaultRoute(t, ctx, routeEnv, "op-correction", "unsafe", env.grpcPort+10, env.metricsPort+10)
	route.waitFrontierAbove(t, ctx, 0)

	divergenceBefore := route.metric(t, ctx, opDivergenceMetric)
	mismatchedBefore := route.metric(t, ctx, opMismatchMetric)
	hookOffset := route.logOffset(t)
	held := injector.hold(t, ctx, env.opNodeRPC)
	evidence(t, "D2 hold: replica finalized pinned at l2 %d; every outputAtBlock above it is forged", held)

	// Derived path, before the reveal: the forged root is served as a
	// provisional root, confirmed-only readers never see it, and the attestor
	// refuses to sign the sequencer's honest header at that height.
	derived := waitForForgedEntry(t, ctx, route, injector, "derived")
	provisionalView := atOrBelow(t, ctx, route, derived.L2BlockNumber, true)
	if provisionalView.GetL2BlockNumber() != derived.L2BlockNumber || !provisionalView.GetProvisional() ||
		!bytes.Equal(provisionalView.GetRoot(), derived.Root) {
		t.Fatalf("include_provisional view at %d = %+v, want the provisional forged root %x", derived.L2BlockNumber, provisionalView, derived.Root)
	}
	if confirmedView := atOrBelow(t, ctx, route, derived.L2BlockNumber, false); confirmedView != nil &&
		(bytes.Equal(confirmedView.GetRoot(), derived.Root) || confirmedView.GetL2BlockNumber() > held) {
		t.Fatalf("confirmed view at %d = %+v, want a root at or below the held finalized head %d", derived.L2BlockNumber, confirmedView, held)
	}
	header, err := getL2Header(ctx, env.l2RPC, derived.L2BlockNumber)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := route.verifyAt(ctx, header)
	if err != nil {
		t.Fatalf("VerifyStateRoot(%d) while diverged: %v", derived.L2BlockNumber, err)
	}
	if refused.GetValid() || len(refused.GetAttestationSignature()) != 0 || bytes.Equal(refused.GetStateRoot(), header.stateRoot) {
		t.Fatalf("VerifyStateRoot(%d) while diverged = %+v, want valid=false, unsigned, replica's forged state root", derived.L2BlockNumber, refused)
	}

	// Game path: a real proposer game decided while the replica disagrees is
	// a provisional mismatch, which raises the mismatch alarm and the hook.
	game := waitForOPMismatch(t, ctx, route, func(m opMismatch) bool {
		forged, ok := injector.forgedAt(m.L2Block)
		return m.Provisional && ok && bytes.Equal(m.LocalRoot, forged.forged)
	})
	mismatchedDuring := route.metric(t, ctx, opMismatchMetric)
	if mismatchedDuring <= mismatchedBefore {
		t.Fatalf("%s stayed at %v after provisional mismatch on game %d", opMismatchMetric, mismatchedBefore, game.GameIndex)
	}
	hook := route.waitForLogLineWith(t, ctx, hookOffset, opMismatchHookLine, fmt.Sprintf(`"game_index":%d`, game.GameIndex))
	evidence(t, "D2 before reveal: derived l2 %d served provisional root %x (honest %x); VerifyStateRoot(honest header) valid=%t state_root=%x; game %d at l2 %d provisional mismatch; %s %v -> %v",
		derived.L2BlockNumber, derived.Root, mustForged(t, injector, derived.L2BlockNumber).honest, refused.GetValid(), refused.GetStateRoot(),
		game.GameIndex, game.L2Block, opMismatchMetric, mismatchedBefore, mismatchedDuring)
	evidence(t, "D2 hook log: %s", hook)

	// Reveal: the finalized recheck is now authoritative and must correct
	// every forged entry the store holds.
	revealOffset := route.logOffset(t)
	injector.release()
	state := waitForState(t, ctx, route.view, func(state stateSnapshot) bool {
		return len(forgedEntries(injector, state)) == 0 && opGameCorrected(state, game)
	})
	mismatches := readOPMismatches(t, route.view)
	for _, m := range mismatches {
		if m.GameIndex == game.GameIndex {
			t.Fatalf("mismatch record for game %d survived the finalized recheck: %+v", game.GameIndex, m)
		}
	}
	corrected := atOrBelow(t, ctx, route, derived.L2BlockNumber, false)
	honest := mustForged(t, injector, derived.L2BlockNumber).honest
	if corrected == nil || corrected.GetL2BlockNumber() != derived.L2BlockNumber || corrected.GetProvisional() || !bytes.Equal(corrected.GetRoot(), honest) {
		t.Fatalf("confirmed view at %d after reveal = %+v, want honest root %x", derived.L2BlockNumber, corrected, honest)
	}
	signed, err := route.verifyAt(ctx, header)
	if err != nil {
		t.Fatalf("VerifyStateRoot(%d) after reveal: %v", derived.L2BlockNumber, err)
	}
	assertCanonicalSignedVerdict(t, route.verifyConfig(t), header, route.setHash(t), signed)

	derivedLine := route.waitForLogLineWith(t, ctx, revealOffset, opDerivedDivergenceLine, fmt.Sprintf(`"l2_block":%d`, derived.L2BlockNumber))
	recheckLine := route.waitForLogLineWith(t, ctx, revealOffset, opRecheckDivergenceLine, fmt.Sprintf(`"game_index":%d`, game.GameIndex))
	divergenceAfter := route.metric(t, ctx, opDivergenceMetric)
	alarms := route.countLogLines(t, revealOffset, "HEAD DIVERGENCE")
	if divergenceAfter-divergenceBefore != float64(alarms) || alarms < 2 {
		t.Fatalf("%s %v -> %v with %d HEAD DIVERGENCE log lines; want one increment per alarm and at least the derived and game alarms",
			opDivergenceMetric, divergenceBefore, divergenceAfter, alarms)
	}
	if err := injector.failure(); err != nil {
		t.Fatalf("interposer: %v", err)
	}
	evidence(t, "D2 after reveal: AttestedRootAtOrBelow(%d, confirmed) = %x provisional=%t; VerifyStateRoot(honest header) valid=%t signature=%dB; game %d now attested; %d feed entries held forged roots, none remain",
		derived.L2BlockNumber, corrected.GetRoot(), corrected.GetProvisional(), signed.GetValid(), len(signed.GetAttestationSignature()),
		game.GameIndex, injector.forgedCount(state))
	evidence(t, "D2 metric: %s %v -> %v (%d alarms)", opDivergenceMetric, divergenceBefore, divergenceAfter, alarms)
	evidence(t, "D2 log (derived): %s", derivedLine)
	evidence(t, "D2 log (game recheck): %s", recheckLine)
}

func opGameCorrected(state stateSnapshot, game opMismatch) bool {
	for _, entry := range state.Attested {
		if entry.Source == "game" && entry.GameIndex == game.GameIndex && !entry.Provisional && bytes.Equal(entry.Root, game.RootClaim) {
			return true
		}
	}
	return false
}

// ------------------------------------------- D3 Arbitrum provisional correction

func assertArbitrumProvisionalCorrection(t *testing.T, ctx context.Context, env faultEnv) {
	injector := &divergence{family: familyArbitrum}
	interposer := newRPCInterposer(t, env.l2RPC, injector.rewrite)
	routeEnv := env
	routeEnv.l2RPC = interposer.url
	route := startFaultRoute(t, ctx, routeEnv, "arbitrum-correction", "unsafe", env.grpcPort+10, env.metricsPort+10)
	route.waitFrontierAbove(t, ctx, 0)

	divergenceBefore := route.metric(t, ctx, arbitrumDivergenceMetric)
	held := injector.hold(t, ctx, env.l2RPC)
	evidence(t, "D3 hold: Nitro finalized pinned at l2 %d; every header above it carries a forged state root", held)

	derived := waitForForgedEntry(t, ctx, route, injector, "derived")
	provisionalView := atOrBelow(t, ctx, route, derived.L2BlockNumber, true)
	if provisionalView.GetL2BlockNumber() != derived.L2BlockNumber || !provisionalView.GetProvisional() ||
		!bytes.Equal(provisionalView.GetRoot(), derived.Root) {
		t.Fatalf("include_provisional view at %d = %+v, want the provisional forged root %x", derived.L2BlockNumber, provisionalView, derived.Root)
	}
	header, err := getL2Header(ctx, env.l2RPC, derived.L2BlockNumber)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := route.verifyAt(ctx, header)
	if err != nil {
		t.Fatalf("VerifyStateRoot(%d) while diverged: %v", derived.L2BlockNumber, err)
	}
	if refused.GetValid() || len(refused.GetAttestationSignature()) != 0 {
		t.Fatalf("VerifyStateRoot(%d) while diverged = %+v, want valid=false and unsigned", derived.L2BlockNumber, refused)
	}
	evidence(t, "D3 before reveal: derived l2 %d served provisional root %x (honest %x); VerifyStateRoot(honest header) valid=%t state_root=%x",
		derived.L2BlockNumber, derived.Root, header.stateRoot, refused.GetValid(), refused.GetStateRoot())

	revealOffset := route.logOffset(t)
	injector.release()
	state := waitForState(t, ctx, route.view, func(state stateSnapshot) bool {
		return len(forgedEntries(injector, state)) == 0 && stateHasHonestDerived(state, derived.L2BlockNumber, header.stateRoot)
	})
	corrected := atOrBelow(t, ctx, route, derived.L2BlockNumber, false)
	if corrected == nil || corrected.GetL2BlockNumber() != derived.L2BlockNumber || corrected.GetProvisional() || !bytes.Equal(corrected.GetRoot(), header.stateRoot) {
		t.Fatalf("confirmed view at %d after reveal = %+v, want honest root %x", derived.L2BlockNumber, corrected, header.stateRoot)
	}
	signed, err := route.verifyAt(ctx, header)
	if err != nil {
		t.Fatalf("VerifyStateRoot(%d) after reveal: %v", derived.L2BlockNumber, err)
	}
	assertCanonicalSignedVerdict(t, route.verifyConfig(t), header, route.setHash(t), signed)

	line := route.waitForLogLineWith(t, ctx, revealOffset, fmt.Sprintf("%s%d ", arbitrumDivergenceLogLine, derived.L2BlockNumber))
	consistency := route.waitForLogLineWith(t, ctx, revealOffset, "attestor finalized consistency mismatch",
		fmt.Sprintf("height=%d ", derived.L2BlockNumber))
	divergenceAfter := route.metric(t, ctx, arbitrumDivergenceMetric)
	corrections := route.countLogLines(t, revealOffset, arbitrumDivergenceLogLine)
	// One increment per finalized height whose unsafe observation was forged;
	// every corrected derived root is such a height, so the metric covers them.
	if divergenceAfter-divergenceBefore < float64(corrections) || corrections < 1 {
		t.Fatalf("%s %v -> %v with %d derived corrections; want at least one increment per correction",
			arbitrumDivergenceMetric, divergenceBefore, divergenceAfter, corrections)
	}
	if err := injector.failure(); err != nil {
		t.Fatalf("interposer: %v", err)
	}
	evidence(t, "D3 after reveal: AttestedRootAtOrBelow(%d, confirmed) = %x provisional=%t; VerifyStateRoot(honest header) valid=%t signature=%dB; %d forged feed entries, none remain",
		derived.L2BlockNumber, corrected.GetRoot(), corrected.GetProvisional(), signed.GetValid(), len(signed.GetAttestationSignature()), injector.forgedCount(state))
	evidence(t, "D3 metric: %s %v -> %v (%d derived corrections)", arbitrumDivergenceMetric, divergenceBefore, divergenceAfter, corrections)
	evidence(t, "D3 log (derived): %s", line)
	evidence(t, "D3 log (runtime): %s", consistency)
}

func stateHasHonestDerived(state stateSnapshot, height uint64, root []byte) bool {
	for _, entry := range state.Attested {
		if entry.Source == "derived" && entry.L2BlockNumber == height {
			return !entry.Provisional && bytes.Equal(entry.Root, root)
		}
	}
	return false
}

// ------------------------------------------------------------ drill helpers

func (r *faultRoute) verifyConfig(t *testing.T) liveE2EConfig {
	t.Helper()
	key, err := decodeLivePublicKey(r.env.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	config := liveE2EConfig{sourceChain: r.env.srcChain, l2ChainID: r.env.l2ChainID, publicKey: key, router: make([]byte, 20)}
	config.router[19] = 1 // matches verifyAt
	return config
}

func (r *faultRoute) setHash(t *testing.T) []byte {
	t.Helper()
	hash, err := liveSetHash(r.verifyConfig(t).publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// waitForLogLineWith returns the first line written after offset that holds
// every part. Production zap logs are JSON, so a structured field is matched
// as `"name":value` on the same line as its message.
func (r *faultRoute) waitForLogLineWith(t *testing.T, ctx context.Context, offset int, parts ...string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if line, ok := findLogLine(readLogOrFail(t, r.view), offset, parts...); ok {
			return line
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, time.Second) {
			t.Fatalf("attestor log %s has no line after byte %d containing all of %q\n%s", r.view.logPath, offset, parts, tailFile(r.view.logPath, 30))
		}
	}
}

func (r *faultRoute) countLogLines(t *testing.T, offset int, part string) int {
	t.Helper()
	log := readLogOrFail(t, r.view)
	if offset > len(log) {
		return 0
	}
	return strings.Count(log[offset:], part)
}

func findLogLine(log string, offset int, parts ...string) (string, bool) {
	if offset > len(log) {
		return "", false
	}
	for _, line := range strings.Split(log[offset:], "\n") {
		matched := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			return line, true
		}
	}
	return "", false
}

func atOrBelow(t *testing.T, ctx context.Context, route *faultRoute, height uint64, provisional bool) *attestorpb.AttestedRoot {
	t.Helper()
	response, err := route.client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
		SrcChain: route.env.srcChain, L2BlockNumber: height, IncludeProvisional: provisional,
	})
	if err != nil {
		t.Fatalf("AttestedRootAtOrBelow(%d, provisional=%t): %v", height, provisional, err)
	}
	if !response.GetFound() {
		return nil
	}
	return response.GetRoot()
}

// waitForForgedEntry waits for the store to persist a provisional entry whose
// root is one the interposer forged.
func waitForForgedEntry(t *testing.T, ctx context.Context, route *faultRoute, injector *divergence, source string) stateEntry {
	t.Helper()
	var found stateEntry
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	waitForState(t, waitCtx, route.view, func(state stateSnapshot) bool {
		for _, entry := range forgedEntries(injector, state) {
			if entry.Source == source && entry.Provisional {
				found = entry
				return true
			}
		}
		return false
	})
	return found
}

func forgedEntries(injector *divergence, state stateSnapshot) []stateEntry {
	var out []stateEntry
	for _, entry := range state.Attested {
		if forged, ok := injector.forgedAt(entry.L2BlockNumber); ok && bytes.Equal(entry.Root, forged.forged) {
			out = append(out, entry)
		}
	}
	return out
}

func mustForged(t *testing.T, injector *divergence, height uint64) forgery {
	t.Helper()
	forged, ok := injector.forgedAt(height)
	if !ok {
		t.Fatalf("interposer never forged height %d", height)
	}
	return forged
}

type opMismatch struct {
	GameIndex   uint64
	L2Block     uint64
	RootClaim   []byte
	LocalRoot   []byte
	Provisional bool
}

func waitForOPMismatch(t *testing.T, ctx context.Context, route *faultRoute, match func(opMismatch) bool) opMismatch {
	t.Helper()
	// Games are read from finalized L1 and the proposer posts every 60s, so a
	// new game reaches the attestor within a few L1 finality windows.
	deadline := time.Now().Add(15 * time.Minute)
	for {
		mismatches, err := opMismatches(route.view)
		if err == nil {
			for _, m := range mismatches {
				if match(m) {
					return m
				}
			}
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("no matching mismatch record in %s (last error %v)\n%s", route.view.statePath, err, tailFile(route.view.logPath, 30))
		}
	}
}

func readOPMismatches(t *testing.T, config lifecycleConfig) []opMismatch {
	t.Helper()
	mismatches, err := opMismatches(config)
	if err != nil {
		t.Fatal(err)
	}
	return mismatches
}

func opMismatches(config lifecycleConfig) ([]opMismatch, error) {
	data, err := os.ReadFile(config.statePath)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Mismatches []struct {
			Game struct {
				GameIndex     uint64          `json:"game_index"`
				L2BlockNumber uint64          `json:"l2_block_number"`
				RootClaim     json.RawMessage `json:"root_claim"`
			} `json:"game"`
			LocalRoot   json.RawMessage `json:"local_root"`
			Provisional bool            `json:"provisional"`
		} `json:"mismatches"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]opMismatch, 0, len(raw.Mismatches))
	for _, m := range raw.Mismatches {
		claim, err := decodeStateBytes(m.Game.RootClaim)
		if err != nil {
			return nil, err
		}
		local, err := decodeStateBytes(m.LocalRoot)
		if err != nil {
			return nil, err
		}
		out = append(out, opMismatch{GameIndex: m.Game.GameIndex, L2Block: m.Game.L2BlockNumber, RootClaim: claim, LocalRoot: local, Provisional: m.Provisional})
	}
	return out, nil
}

type replicaHeadSet struct{ unsafe, safe, finalized uint64 }

func replicaHeads(t *testing.T, ctx context.Context, opNodeRPC string) replicaHeadSet {
	t.Helper()
	var status struct {
		UnsafeL2    struct{ Number uint64 } `json:"unsafe_l2"`
		SafeL2      struct{ Number uint64 } `json:"safe_l2"`
		FinalizedL2 struct{ Number uint64 } `json:"finalized_l2"`
	}
	if err := liveRPC(ctx, opNodeRPC, "optimism_syncStatus", []any{}, &status); err != nil {
		t.Fatal(err)
	}
	return replicaHeadSet{unsafe: status.UnsafeL2.Number, safe: status.SafeL2.Number, finalized: status.FinalizedL2.Number}
}

type replicaOutputAt struct{ output, stateRoot []byte }

func replicaOutput(t *testing.T, ctx context.Context, opNodeRPC string, height uint64) replicaOutputAt {
	t.Helper()
	var output struct {
		OutputRoot string `json:"outputRoot"`
		StateRoot  string `json:"stateRoot"`
	}
	if err := liveRPC(ctx, opNodeRPC, "optimism_outputAtBlock", []any{fmt.Sprintf("0x%x", height)}, &output); err != nil {
		t.Fatal(err)
	}
	root, err := decodeExactHex(output.OutputRoot, 32)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot, err := decodeExactHex(output.StateRoot, 32)
	if err != nil {
		t.Fatal(err)
	}
	return replicaOutputAt{output: root, stateRoot: stateRoot}
}

// ---------------------------------------------------------------- injection

// forgery is one height's honest and forged root: the output root on OP, the
// header state root on Arbitrum (the value each family's feed stores).
type forgery struct{ honest, forged []byte }

// divergence decides what the interposer rewrites. Its methods run on the
// interposer's HTTP goroutines and on the test goroutine; mu guards all state.
type divergence struct {
	family string

	mu        sync.Mutex
	holding   bool
	held      json.RawMessage // OP: the finalized_l2 block ref; Arbitrum: the finalized header
	heldBlock uint64
	forged    map[uint64]forgery
	err       error
}

// hold pins the finalized head at the upstream's current one and starts
// forging every block above it.
func (d *divergence) hold(t *testing.T, ctx context.Context, upstream string) uint64 {
	t.Helper()
	var held json.RawMessage
	var number uint64
	if d.family == familyArbitrum {
		var header map[string]json.RawMessage
		if err := liveRPC(ctx, upstream, "eth_getBlockByNumber", []any{"finalized", false}, &header); err != nil {
			t.Fatal(err)
		}
		var err error
		if number, err = hexQuantity(header["number"]); err != nil {
			t.Fatal(err)
		}
		if held, err = json.Marshal(header); err != nil {
			t.Fatal(err)
		}
	} else {
		var status map[string]json.RawMessage
		if err := liveRPC(ctx, upstream, "optimism_syncStatus", []any{}, &status); err != nil {
			t.Fatal(err)
		}
		var ref struct{ Number uint64 }
		if err := json.Unmarshal(status["finalized_l2"], &ref); err != nil {
			t.Fatal(err)
		}
		held, number = status["finalized_l2"], ref.Number
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.holding, d.held, d.heldBlock = true, held, number
	if d.forged == nil {
		d.forged = make(map[uint64]forgery)
	}
	return number
}

func (d *divergence) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.holding = false
}

func (d *divergence) forgedAt(height uint64) (forgery, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	forged, ok := d.forged[height]
	return forged, ok
}

// forgedCount reports how many of the state's entries were ever forged; used
// only for evidence after every one of them was corrected.
func (d *divergence) forgedCount(state stateSnapshot) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, entry := range state.Attested {
		if _, ok := d.forged[entry.L2BlockNumber]; ok && entry.L2BlockNumber > d.heldBlock {
			count++
		}
	}
	return count
}

func (d *divergence) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// rewrite is the interposer callback. An error is recorded and the request
// fails upstream-style, so the daemon never receives a half-rewritten reply.
func (d *divergence) rewrite(method string, params []json.RawMessage, result json.RawMessage) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.holding {
		return result, nil
	}
	var out json.RawMessage
	var err error
	switch {
	case d.family == familyOP && method == "optimism_syncStatus":
		out, err = replaceField(result, "finalized_l2", d.held)
	case d.family == familyOP && method == "optimism_outputAtBlock":
		out, err = d.forgeOutputLocked(params, result)
	case d.family == familyArbitrum && method == "eth_getBlockByNumber":
		out, err = d.forgeHeaderLocked(params, result)
	default:
		return result, nil
	}
	if err != nil && d.err == nil {
		d.err = fmt.Errorf("%s: %w", method, err)
	}
	return out, err
}

func (d *divergence) forgeOutputLocked(params []json.RawMessage, result json.RawMessage) (json.RawMessage, error) {
	if len(params) == 0 {
		return nil, errors.New("missing block parameter")
	}
	height, err := hexQuantity(params[0])
	if err != nil {
		return nil, err
	}
	if height <= d.heldBlock {
		return result, nil
	}
	var output map[string]json.RawMessage
	if err := json.Unmarshal(result, &output); err != nil {
		return nil, err
	}
	honest, err := hexBytesField(output, "outputRoot")
	if err != nil {
		return nil, err
	}
	forged := forgeWord(honest)
	d.forged[height] = forgery{honest: honest, forged: forged}
	if output["outputRoot"], err = json.Marshal("0x" + hex.EncodeToString(forged)); err != nil {
		return nil, err
	}
	// A diverged block has its own identity too: rewrite the state root and
	// block hash VerifyStateRoot reads from the same reply.
	stateRoot, err := hexBytesField(output, "stateRoot")
	if err != nil {
		return nil, err
	}
	if output["stateRoot"], err = json.Marshal("0x" + hex.EncodeToString(forgeWord(stateRoot))); err != nil {
		return nil, err
	}
	var ref map[string]json.RawMessage
	if err := json.Unmarshal(output["blockRef"], &ref); err != nil {
		return nil, fmt.Errorf("blockRef: %w", err)
	}
	hash, err := hexBytesField(ref, "hash")
	if err != nil {
		return nil, err
	}
	if ref["hash"], err = json.Marshal("0x" + hex.EncodeToString(forgeWord(hash))); err != nil {
		return nil, err
	}
	if output["blockRef"], err = json.Marshal(ref); err != nil {
		return nil, err
	}
	return json.Marshal(output)
}

func (d *divergence) forgeHeaderLocked(params []json.RawMessage, result json.RawMessage) (json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		return result, nil
	}
	var tag string
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &tag)
	}
	if tag == "finalized" {
		return d.held, nil
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(result, &header); err != nil {
		return nil, err
	}
	height, err := hexQuantity(header["number"])
	if err != nil {
		return nil, err
	}
	if height <= d.heldBlock {
		return result, nil
	}
	// The client recomputes the block hash from the header fields, so a
	// forged state root also yields a different, self-consistent block hash.
	honest, err := hexBytesField(header, "stateRoot")
	if err != nil {
		return nil, err
	}
	forged := forgeWord(honest)
	d.forged[height] = forgery{honest: honest, forged: forged}
	if header["stateRoot"], err = json.Marshal("0x" + hex.EncodeToString(forged)); err != nil {
		return nil, err
	}
	return json.Marshal(header)
}

// forgeWord derives a deterministic 32-byte value from an honest one, so
// every read of the same height returns the same forged answer.
func forgeWord(honest []byte) []byte {
	sum := sha256.Sum256(append([]byte("spectre-divergence-drill"), honest...))
	return sum[:]
}

func replaceField(object json.RawMessage, field string, value json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields[field]; !ok {
		return nil, fmt.Errorf("reply has no %q", field)
	}
	fields[field] = value
	return json.Marshal(fields)
}

func hexBytesField(fields map[string]json.RawMessage, name string) ([]byte, error) {
	var text string
	if err := json.Unmarshal(fields[name], &text); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return decodeExactHex(text, 32)
}

func hexQuantity(raw json.RawMessage) (uint64, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("quantity %s: %w", raw, err)
	}
	return strconv.ParseUint(strings.TrimPrefix(text, "0x"), 16, 64)
}

// ---------------------------------------------------------------- interposer

// rpcInterposer is an HTTP JSON-RPC reverse proxy that passes each result
// through rewrite on its way back. It handles single and batch requests.
type rpcInterposer struct {
	upstream string
	url      string
	server   *httptest.Server
	rewrite  func(method string, params []json.RawMessage, result json.RawMessage) (json.RawMessage, error)
}

func newRPCInterposer(t *testing.T, upstream string, rewrite func(string, []json.RawMessage, json.RawMessage) (json.RawMessage, error)) *rpcInterposer {
	t.Helper()
	p := &rpcInterposer{upstream: upstream, rewrite: rewrite}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	p.url = p.server.URL
	t.Cleanup(p.server.Close)
	return p
}

func (p *rpcInterposer) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		// Close the connection like an unreachable upstream would.
		if hijacker, ok := w.(http.Hijacker); ok {
			if connection, _, hijackErr := hijacker.Hijack(); hijackErr == nil {
				_ = connection.Close()
				return
			}
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(response.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if response.StatusCode == http.StatusOK {
		if reply, err = p.rewriteBody(body, reply); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(reply)
}

type rpcCall struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (p *rpcInterposer) rewriteBody(requestBody, replyBody []byte) ([]byte, error) {
	batch := len(bytes.TrimSpace(requestBody)) > 0 && bytes.TrimSpace(requestBody)[0] == '['
	var calls []rpcCall
	var replies []rpcReply
	if batch {
		if json.Unmarshal(requestBody, &calls) != nil || json.Unmarshal(replyBody, &replies) != nil {
			return replyBody, nil
		}
	} else {
		var call rpcCall
		var reply rpcReply
		if json.Unmarshal(requestBody, &call) != nil || json.Unmarshal(replyBody, &reply) != nil {
			return replyBody, nil
		}
		calls, replies = []rpcCall{call}, []rpcReply{reply}
	}
	byID := make(map[string]rpcCall, len(calls))
	for _, call := range calls {
		byID[string(call.ID)] = call
	}
	for index := range replies {
		reply := &replies[index]
		call, ok := byID[string(reply.ID)]
		if !ok || len(reply.Result) == 0 {
			continue
		}
		rewritten, err := p.rewrite(call.Method, call.Params, reply.Result)
		if err != nil {
			return nil, err
		}
		reply.Result = rewritten
	}
	if batch {
		return json.Marshal(replies)
	}
	return json.Marshal(replies[0])
}

// ------------------------------------------------------------ offline tests

// TestDivergenceInterposerOP pins the OP rewrite rules against a fake op-node:
// nothing changes before the hold, the finalized ref is pinned and blocks
// above it are forged during it, and honest answers return on release.
func TestDivergenceInterposerOP(t *testing.T) {
	finalized := uint64(10)
	upstream := fakeRPC(t, func(method string, params []json.RawMessage) any {
		switch method {
		case "optimism_syncStatus":
			return map[string]any{
				"unsafe_l2":    map[string]any{"number": 40, "hash": hash32(40)},
				"finalized_l2": map[string]any{"number": finalized, "hash": hash32(finalized)},
			}
		case "optimism_outputAtBlock":
			height, _ := hexQuantity(params[0])
			return map[string]any{
				"outputRoot": hash32(height + 1000),
				"stateRoot":  hash32(height + 2000),
				"blockRef":   map[string]any{"number": height, "hash": hash32(height)},
			}
		}
		return nil
	})
	injector := &divergence{family: familyOP}
	interposer := newRPCInterposer(t, upstream, injector.rewrite)
	ctx := context.Background()

	output := func(height uint64) string {
		var out struct{ OutputRoot string }
		if err := liveRPC(ctx, interposer.url, "optimism_outputAtBlock", []any{fmt.Sprintf("0x%x", height)}, &out); err != nil {
			t.Fatal(err)
		}
		return out.OutputRoot
	}
	finalizedSeen := func() uint64 {
		var status struct {
			FinalizedL2 struct{ Number uint64 } `json:"finalized_l2"`
		}
		if err := liveRPC(ctx, interposer.url, "optimism_syncStatus", []any{}, &status); err != nil {
			t.Fatal(err)
		}
		return status.FinalizedL2.Number
	}

	if output(20) != hash32(1020) {
		t.Fatal("rewrote before the hold")
	}
	if held := injector.hold(t, ctx, upstream); held != 10 {
		t.Fatalf("held %d, want 10", held)
	}
	finalized = 30
	if got := finalizedSeen(); got != 10 {
		t.Fatalf("finalized during hold = %d, want pinned 10", got)
	}
	if output(10) != hash32(1010) {
		t.Fatal("forged the held finalized block itself")
	}
	forged := output(20)
	if forged == hash32(1020) || forged != output(20) {
		t.Fatalf("block above the hold = %s, want a stable forged root", forged)
	}
	if record := mustForged(t, injector, 20); "0x"+hex.EncodeToString(record.forged) != forged ||
		"0x"+hex.EncodeToString(record.honest) != hash32(1020) {
		t.Fatalf("forgery record %x/%x does not match served %s", record.honest, record.forged, forged)
	}
	injector.release()
	if finalizedSeen() != 30 || output(20) != hash32(1020) {
		t.Fatal("still rewriting after release")
	}
	if err := injector.failure(); err != nil {
		t.Fatal(err)
	}
}

// TestDivergenceInterposerArbitrum pins the Nitro rules, including a batch
// request: the finalized tag returns the held header, headers above it carry
// a forged state root, and headers at or below it pass through.
func TestDivergenceInterposerArbitrum(t *testing.T) {
	upstream := fakeRPC(t, func(method string, params []json.RawMessage) any {
		if method != "eth_getBlockByNumber" {
			return nil
		}
		var tag string
		_ = json.Unmarshal(params[0], &tag)
		height := uint64(50)
		switch tag {
		case "finalized":
			height = 10
		case "latest":
		default:
			height, _ = hexQuantity(params[0])
		}
		return map[string]any{"number": fmt.Sprintf("0x%x", height), "stateRoot": hash32(height + 500), "hash": hash32(height)}
	})
	injector := &divergence{family: familyArbitrum}
	interposer := newRPCInterposer(t, upstream, injector.rewrite)
	injector.hold(t, context.Background(), upstream)

	batch := `[{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["finalized",false]},` +
		`{"jsonrpc":"2.0","id":2,"method":"eth_getBlockByNumber","params":["0xa",false]},` +
		`{"jsonrpc":"2.0","id":3,"method":"eth_getBlockByNumber","params":["latest",false]}]`
	response, err := http.Post(interposer.url, "application/json", strings.NewReader(batch))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var replies []struct {
		ID     int
		Result struct{ Number, StateRoot string }
	}
	if err := json.NewDecoder(response.Body).Decode(&replies); err != nil || len(replies) != 3 {
		t.Fatalf("batch reply: %v (%d entries)", err, len(replies))
	}
	for _, reply := range replies {
		switch reply.ID {
		case 1, 2:
			if reply.Result.Number != "0xa" || reply.Result.StateRoot != hash32(510) {
				t.Fatalf("reply %d = %+v, want the honest held header", reply.ID, reply.Result)
			}
		case 3:
			if reply.Result.StateRoot == hash32(550) || reply.Result.StateRoot != "0x"+hex.EncodeToString(forgeWord(mustDecode(t, hash32(550)))) {
				t.Fatalf("latest above the hold = %+v, want the forged state root", reply.Result)
			}
		}
	}
}

func TestFindLogLine(t *testing.T) {
	log := "{\"msg\":\"a\",\"l2_block\":5}\n{\"msg\":\"HEAD DIVERGENCE x\",\"l2_block\":7}\n"
	if _, ok := findLogLine(log, 0, "HEAD DIVERGENCE", `"l2_block":5`); ok {
		t.Fatal("matched parts from two different lines")
	}
	if line, ok := findLogLine(log, 0, "HEAD DIVERGENCE", `"l2_block":7`); !ok || !strings.Contains(line, "x") {
		t.Fatalf("line = %q, %t", line, ok)
	}
	if _, ok := findLogLine(log, len(log)-5, "HEAD DIVERGENCE"); ok {
		t.Fatal("matched before the offset")
	}
}

func fakeRPC(t *testing.T, answer func(method string, params []json.RawMessage) any) string {
	t.Helper()
	reply := func(call rpcCall) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": answer(call.Method, call.Params)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var out any
		if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
			var calls []rpcCall
			_ = json.Unmarshal(body, &calls)
			replies := make([]map[string]any, 0, len(calls))
			for _, call := range calls {
				replies = append(replies, reply(call))
			}
			out = replies
		} else {
			var call rpcCall
			_ = json.Unmarshal(body, &call)
			out = reply(call)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func hash32(value uint64) string {
	return fmt.Sprintf("0x%064x", value)
}

func mustDecode(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := decodeExactHex(value, 32)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
