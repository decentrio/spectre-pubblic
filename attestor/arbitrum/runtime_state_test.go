package arbitrum

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestRunModeUsesFinalizedNaming(t *testing.T) {
	mode, err := RunMode("finalized").Normalize()
	if err != nil {
		t.Fatalf("normalize finalized mode: %v", err)
	}
	if mode != RunModeFinalized {
		t.Fatalf("normalized mode: got %q want %q", mode, RunModeFinalized)
	}

	if _, err := RunMode("finalize").Normalize(); err == nil {
		t.Fatal("legacy finalize mode must be rejected")
	}
	if _, err := RunMode("").Normalize(); err == nil {
		t.Fatal("empty internal run mode must be rejected")
	}
}

func TestRuntimeStateTracksFinalizedConsistency(t *testing.T) {
	reader := newRuntimeTestReader()
	reader.setHeader(testHeader(1, 0x01))
	reader.setHeader(testHeader(2, 0x02))
	reader.setHeader(testHeader(3, 0x03))
	reader.setHeader(testHeader(4, 0x04))
	reader.setHeads(3, 2, 1)

	state, err := NewRuntimeState(reader)
	if err != nil {
		t.Fatalf("create runtime state: %v", err)
	}
	checks, err := state.Refresh(context.Background())
	if err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("initial refresh produced checks: %+v", checks)
	}

	reader.setHeads(3, 2, 2)
	checks, err = state.Refresh(context.Background())
	if err != nil {
		t.Fatalf("finalize height 2: %v", err)
	}
	// Startup records only the current unsafe and safe heads. Height 2 was the
	// initial safe head but was already below the initial unsafe head, so its
	// consistency result is intentionally incomplete instead of triggering a
	// historical public-RPC backfill.
	if len(checks) != 1 ||
		checks[0].UnsafeObserved ||
		!checks[0].SafeObserved ||
		!checks[0].SafeMatches ||
		checks[0].Consistent() {
		t.Fatalf("height 2 bootstrap consistency: %+v", checks)
	}

	// Height 3 was first observed with root 0x03 while unsafe. Before it
	// becomes safe, replace the canonical header with root 0x33 to model an
	// unsafe reorg. The first unsafe observation must be retained so finalization
	// reports the mismatch while the safe observation matches.
	reader.setHeader(testHeader(3, 0x33))
	reader.setHeads(4, 3, 2)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("record reorged safe height 3: %v", err)
	}

	reader.setHeads(4, 3, 3)
	checks, err = state.Refresh(context.Background())
	if err != nil {
		t.Fatalf("finalize height 3: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("height 3 checks: got %d want 1", len(checks))
	}
	check := checks[0]
	if !check.UnsafeObserved || !check.SafeObserved {
		t.Fatalf("height 3 observations missing: %+v", check)
	}
	if check.UnsafeMatches {
		t.Fatalf("reorged unsafe root unexpectedly matched: %+v", check)
	}
	if !check.SafeMatches {
		t.Fatalf("safe root did not match final root: %+v", check)
	}
	if check.Consistent() {
		t.Fatalf("mismatched finalization reported consistent: %+v", check)
	}

	snapshot := state.Snapshot()
	if !snapshot.UnsafeObserved || snapshot.Unsafe.BlockNumber != 4 {
		t.Fatalf("unsafe snapshot: %+v", snapshot)
	}
	if !snapshot.SafeObserved || snapshot.Safe.BlockNumber != 3 {
		t.Fatalf("safe snapshot: %+v", snapshot)
	}
	if !snapshot.FinalizedSeen || snapshot.Finalized.BlockNumber != 3 {
		t.Fatalf("finalized snapshot: %+v", snapshot)
	}
	if !snapshot.HasLastFinalizedCheck || snapshot.LastFinalizedCheck.Finalized.BlockNumber != 3 {
		t.Fatalf("finalized check snapshot: %+v", snapshot)
	}
}

func TestRuntimeStateRejectsInconsistentHeadOrdering(t *testing.T) {
	reader := newRuntimeTestReader()
	reader.setHeader(testHeader(1, 0x01))
	reader.setHeader(testHeader(2, 0x02))
	reader.setHeads(1, 2, 1)

	state, err := NewRuntimeState(reader)
	if err != nil {
		t.Fatalf("create runtime state: %v", err)
	}
	if _, err := state.Refresh(context.Background()); err == nil {
		t.Fatal("expected safe-above-unsafe ordering error")
	}
}

func TestRuntimeStateMarksFinalizedHeadRegression(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 5; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(5, 5, 4)
	state, err := NewRuntimeState(reader)
	if err != nil {
		t.Fatalf("create runtime state: %v", err)
	}
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	reader.setHeads(5, 5, 3)
	checks, err := state.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh after finalized-head regression: %v", err)
	}
	if len(checks) != 1 || !checks[0].FinalizedReorg ||
		checks[0].Finalized.BlockNumber != 3 || checks[0].PreviousFinalized.BlockNumber != 4 {
		t.Fatalf("regression checks = %+v", checks)
	}
	snapshot := state.Snapshot()
	if !snapshot.FinalizedSeen || snapshot.Finalized.BlockNumber != 3 ||
		!snapshot.LastFinalizedCheck.FinalizedReorg {
		t.Fatalf("snapshot after finalized-head regression = %+v", snapshot)
	}
}

func TestResolveCanonicalBlockHashHonorsConfirmationMode(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 5; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(5, 5, 4)
	state, err := NewRuntimeState(reader)
	if err != nil {
		t.Fatalf("create runtime state: %v", err)
	}

	blockFive := reader.header(5)
	commitment, err := state.ResolveCanonicalBlockHash(
		context.Background(),
		blockFive.Hash(),
		RunModeSafe,
	)
	if err != nil {
		t.Fatalf("resolve safe block: %v", err)
	}
	if commitment.BlockNumber != 5 || commitment.StateRoot != blockFive.Root {
		t.Fatalf("safe commitment: %+v", commitment)
	}

	_, err = state.ResolveCanonicalBlockHash(
		context.Background(),
		blockFive.Hash(),
		RunModeFinalized,
	)
	if !errors.Is(err, ErrCommitmentNotReady) {
		t.Fatalf("block above finalized head: got %v want not ready", err)
	}
}

func TestResolveCanonicalBlockHashRejectsReorgedHeader(t *testing.T) {
	reader := newRuntimeTestReader()
	oldHeader := testHeader(3, 0x03)
	reader.setHeader(oldHeader)
	reader.setHeader(testHeader(4, 0x04))
	reader.setHeads(4, 3, 3)
	reader.setHeader(testHeader(3, 0x33))

	state, err := NewRuntimeState(reader)
	if err != nil {
		t.Fatalf("create runtime state: %v", err)
	}
	_, err = state.ResolveCanonicalBlockHash(
		context.Background(),
		oldHeader.Hash(),
		RunModeSafe,
	)
	if !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("reorged block: got %v want mismatch", err)
	}
}

func TestNitroBlockNotFoundRecognizesRPCText(t *testing.T) {
	for _, err := range []error{
		ethereum.NotFound,
		fmt.Errorf("safe block not found"),
		fmt.Errorf("finalized block not found"),
	} {
		if !isNitroBlockNotFound(err) {
			t.Fatalf("expected block-not-found match for %v", err)
		}
	}
	if isNitroBlockNotFound(fmt.Errorf("connection reset")) {
		t.Fatal("unexpected block-not-found match")
	}
}

func newCappedRuntimeState(
	t *testing.T,
	reader *runtimeTestReader,
	maxBlocks uint64,
	concurrency int,
) *RuntimeState {
	t.Helper()
	state, err := NewRuntimeStateWithConfig(reader, RuntimeStateConfig{
		BackfillMaxBlocks:   maxBlocks,
		BackfillConcurrency: concurrency,
	})
	if err != nil {
		t.Fatalf("create runtime state: %v", err)
	}
	return state
}

func TestRuntimeStateCapsUnsafeAndSafeBackfill(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 30; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(10, 10, 10)
	state := newCappedRuntimeState(t, reader, 4, 2)

	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	baseline := reader.fetchCallCount()

	// Unsafe and safe both jump 20 blocks; each walk must fetch only the
	// newest 4 (head via tag + 3 numeric lookups each).
	reader.setHeads(30, 30, 10)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("capped refresh: %v", err)
	}
	if fetched := reader.fetchCallCount() - baseline; fetched != 6 {
		t.Fatalf("numeric fetches during capped refresh: got %d want 6", fetched)
	}
	for _, mode := range []RunMode{RunModeUnsafe, RunModeSafe} {
		for _, height := range []uint64{27, 28, 29, 30} {
			if _, ok := state.observed[mode][height]; !ok {
				t.Fatalf("%s height %d missing from capped window", mode, height)
			}
		}
		for _, height := range []uint64{12, 20, 26} {
			if _, ok := state.observed[mode][height]; ok {
				t.Fatalf("%s height %d recorded despite cap", mode, height)
			}
		}
	}

	// Finalizing into the gap: heights below the capped window report the
	// incomplete shape, heights inside it report full observations.
	reader.setHeads(30, 30, 28)
	checks, err := state.Refresh(context.Background())
	if err != nil {
		t.Fatalf("finalize into gap: %v", err)
	}
	if len(checks) != 4 {
		t.Fatalf("finalized checks: got %d want 4 (capped)", len(checks))
	}
	for index, check := range checks {
		wantHeight := uint64(25 + index)
		if check.Finalized.BlockNumber != wantHeight {
			t.Fatalf("check %d height: got %d want %d", index, check.Finalized.BlockNumber, wantHeight)
		}
		wantObserved := wantHeight >= 27
		if check.UnsafeObserved != wantObserved || check.SafeObserved != wantObserved {
			t.Fatalf("check %d observations: %+v", index, check)
		}
		if wantObserved && (!check.UnsafeMatches || !check.SafeMatches) {
			t.Fatalf("check %d in-window mismatch: %+v", index, check)
		}
	}

	snapshot := state.Snapshot()
	if snapshot.Unsafe.BlockNumber != 30 || snapshot.Safe.BlockNumber != 30 || snapshot.Finalized.BlockNumber != 28 {
		t.Fatalf("heads after capped refreshes: %+v", snapshot)
	}
}

func TestRuntimeStateCapsFinalizedBackfill(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 30; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(30, 20, 10)
	state := newCappedRuntimeState(t, reader, 4, 2)

	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	// Finalized jumps 20 blocks; only the newest 4 produce consistency checks.
	reader.setHeads(30, 30, 30)
	checks, err := state.Refresh(context.Background())
	if err != nil {
		t.Fatalf("capped finalized refresh: %v", err)
	}
	if len(checks) != 4 {
		t.Fatalf("finalized checks: got %d want 4 (capped)", len(checks))
	}
	for index, check := range checks {
		if want := uint64(27 + index); check.Finalized.BlockNumber != want {
			t.Fatalf("check %d height: got %d want %d", index, check.Finalized.BlockNumber, want)
		}
	}
	for mode, observations := range state.observed {
		if len(observations) != 0 {
			t.Fatalf("%s observations survived pruning at finalized head: %v", mode, observations)
		}
	}
}

func TestRuntimeStateBackfillConcurrencyBounded(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 20; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(10, 10, 10)
	state := newCappedRuntimeState(t, reader, 0, 2)

	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	gate := make(chan struct{})
	reader.setGate(gate)
	reader.setHeads(18, 10, 10)

	refreshDone := make(chan error, 1)
	go func() {
		_, err := state.Refresh(context.Background())
		refreshDone <- err
	}()

	deadline := time.After(5 * time.Second)
	for reader.inFlightFetches() < 2 {
		select {
		case <-deadline:
			t.Fatal("backfill never reached the concurrency limit")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if max := reader.maxConcurrentFetches(); max != 2 {
		t.Fatalf("concurrent fetch high-water: got %d want 2", max)
	}

	close(gate)
	if err := <-refreshDone; err != nil {
		t.Fatalf("gated refresh: %v", err)
	}
	for height := uint64(11); height <= 18; height++ {
		commitment, ok := state.observed[RunModeUnsafe][height]
		if !ok || commitment.StateRoot != testHeader(height, byte(height)).Root {
			t.Fatalf("unsafe observation at %d: ok=%t commitment=%+v", height, ok, commitment)
		}
	}
}

func TestRuntimeStateBackfillErrorCancelsAndPreservesState(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 20; height++ {
		if height == 11 {
			continue // first height of the walk is missing and fails immediately
		}
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(10, 5, 5)
	state := newCappedRuntimeState(t, reader, 0, 2)

	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	gate := make(chan struct{})
	reader.setGate(gate)
	reader.setHeads(20, 5, 5)
	if _, err := state.Refresh(context.Background()); err == nil {
		t.Fatal("refresh with a missing height must fail")
	}

	snapshot := state.Snapshot()
	if snapshot.Unsafe.BlockNumber != 10 || snapshot.Safe.BlockNumber != 5 || snapshot.Finalized.BlockNumber != 5 {
		t.Fatalf("heads advanced on failed refresh: %+v", snapshot)
	}
	for mode, observations := range state.observed {
		for height := range observations {
			if height > 10 {
				t.Fatalf("failed refresh leaked %s observation at %d", mode, height)
			}
		}
	}

	// The retry resumes from the old previous heads once the height exists.
	reader.setHeader(testHeader(11, 0x11))
	close(gate)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("retry refresh: %v", err)
	}
	for height := uint64(11); height <= 20; height++ {
		if _, ok := state.observed[RunModeUnsafe][height]; !ok {
			t.Fatalf("retry did not record unsafe height %d", height)
		}
	}
}

type testRPCError struct{ code int }

func (e testRPCError) Error() string  { return fmt.Sprintf("rpc error %d", e.code) }
func (e testRPCError) ErrorCode() int { return e.code }

func TestIsRateLimitedMatchesWrapped429(t *testing.T) {
	tooMany := rpc.HTTPError{StatusCode: 429, Status: "429 Too Many Requests"}
	if !IsRateLimited(fmt.Errorf("query Nitro block 7: %w", tooMany)) {
		t.Fatal("wrapped HTTP 429 not recognised as rate limited")
	}
	if !IsRateLimited(fmt.Errorf("query Nitro block 7: %w", testRPCError{code: 429})) {
		t.Fatal("in-band JSON-RPC 429 not recognised as rate limited")
	}
	if IsRateLimited(fmt.Errorf("query Nitro block 7: %w", rpc.HTTPError{StatusCode: 500})) {
		t.Fatal("HTTP 500 misclassified as rate limited")
	}
	if IsRateLimited(errors.New("connection reset")) {
		t.Fatal("plain error misclassified as rate limited")
	}
}

func TestRuntimeStateBackfillRetriesRateLimitedFetch(t *testing.T) {
	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 20; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(10, 10, 10)
	state := newCappedRuntimeState(t, reader, 0, 2)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	// Height 15 answers 429 once before succeeding; the walk must ride it out
	// instead of discarding the whole range.
	reader.setRateLimit(15, 1)
	reader.setHeads(18, 10, 10)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh across a transient 429: %v", err)
	}
	for height := uint64(11); height <= 18; height++ {
		if _, ok := state.observed[RunModeUnsafe][height]; !ok {
			t.Fatalf("unsafe height %d missing after retried walk", height)
		}
	}
}

func TestRuntimeStateLogsBackfillBehindBeforeCap(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 300; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(10, 10, 10)
	// Cap 1024 -> behind threshold 64; a 90-block jump is behind, not capped.
	state := newCappedRuntimeState(t, reader, 1024, 8)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	reader.setHeads(100, 10, 10)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("behind refresh: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "attestor unsafe backfill behind: span=90") {
		t.Fatalf("behind log missing in:\n%s", logged)
	}
	if strings.Contains(logged, "backfill capped") {
		t.Fatalf("cap log fired below the cap:\n%s", logged)
	}
	// Every height was still observed — behind is a warning, not a skip.
	for height := uint64(11); height <= 100; height++ {
		if _, ok := state.observed[RunModeUnsafe][height]; !ok {
			t.Fatalf("behind walk skipped height %d", height)
		}
	}
}

func TestRuntimeStateLogsCappedBackfill(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	reader := newRuntimeTestReader()
	for height := uint64(1); height <= 30; height++ {
		reader.setHeader(testHeader(height, byte(height)))
	}
	reader.setHeads(10, 10, 10)
	state := newCappedRuntimeState(t, reader, 4, 2)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	reader.setHeads(30, 30, 10)
	if _, err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("capped refresh: %v", err)
	}

	logged := buf.String()
	for _, want := range []string{
		"attestor unsafe backfill capped:",
		"attestor safe backfill capped:",
		"skipped_blocks=16",
		"skipped_from=11",
		"skipped_to=26",
		"fetch_from=27",
		"head=30",
		"cap=4",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("capped backfill log missing %q in:\n%s", want, logged)
		}
	}
}

func testHeader(height uint64, root byte) *types.Header {
	return &types.Header{
		Number: new(big.Int).SetUint64(height),
		Root:   [32]byte{31: root},
	}
}

type runtimeTestReader struct {
	mu        sync.RWMutex
	unsafe    uint64
	safe      uint64
	finalized uint64
	headers   map[uint64]*types.Header
	byHash    map[common.Hash]*types.Header

	// gate, when non-nil, blocks numeric-height lookups of seeded headers
	// until it is closed, making concurrency and cancellation observable.
	gate        chan struct{}
	fetchCalls  int
	inFlight    int
	maxInFlight int

	// rateLimit maps a height to how many times it still answers HTTP 429
	// before succeeding.
	rateLimit map[uint64]int
}

func (r *runtimeTestReader) setRateLimit(height uint64, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rateLimit == nil {
		r.rateLimit = make(map[uint64]int)
	}
	r.rateLimit[height] = count
}

func (r *runtimeTestReader) consumeRateLimit(height uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rateLimit[height] <= 0 {
		return false
	}
	r.rateLimit[height]--
	return true
}

func newRuntimeTestReader() *runtimeTestReader {
	return &runtimeTestReader{
		headers: make(map[uint64]*types.Header),
		byHash:  make(map[common.Hash]*types.Header),
	}
}

func (r *runtimeTestReader) setHeads(unsafe, safe, finalized uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unsafe = unsafe
	r.safe = safe
	r.finalized = finalized
}

func (r *runtimeTestReader) setHeader(header *types.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers[header.Number.Uint64()] = types.CopyHeader(header)
	r.byHash[header.Hash()] = types.CopyHeader(header)
}

func (r *runtimeTestReader) header(height uint64) *types.Header {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return types.CopyHeader(r.headers[height])
}

func (r *runtimeTestReader) setGate(gate chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = gate
}

func (r *runtimeTestReader) fetchCallCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fetchCalls
}

func (r *runtimeTestReader) inFlightFetches() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.inFlight
}

func (r *runtimeTestReader) maxConcurrentFetches() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.maxInFlight
}

func (r *runtimeTestReader) beginFetch(height uint64) (chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetchCalls++
	r.inFlight++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
	_, exists := r.headers[height]
	return r.gate, r.gate != nil && exists
}

func (r *runtimeTestReader) endFetch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
}

func (r *runtimeTestReader) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	r.mu.RLock()
	var height uint64
	numeric := false
	switch {
	case number == nil:
		height = r.unsafe
	case number.Int64() == int64(rpc.SafeBlockNumber):
		height = r.safe
	case number.Int64() == int64(rpc.FinalizedBlockNumber):
		height = r.finalized
	case number.Sign() >= 0 && number.IsUint64():
		height = number.Uint64()
		numeric = true
	default:
		r.mu.RUnlock()
		return nil, fmt.Errorf("unsupported block selector %s", number)
	}
	r.mu.RUnlock()

	if numeric {
		gate, gated := r.beginFetch(height)
		defer r.endFetch()
		if gated {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if r.consumeRateLimit(height) {
			return nil, rpc.HTTPError{StatusCode: 429, Status: "429 Too Many Requests"}
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	header, ok := r.headers[height]
	if !ok {
		return nil, fmt.Errorf("header %d not configured", height)
	}
	return types.CopyHeader(header), nil
}

func (r *runtimeTestReader) HeaderByHash(
	_ context.Context,
	hash common.Hash,
) (*types.Header, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	header, ok := r.byHash[hash]
	if !ok {
		return nil, ethereum.NotFound
	}
	return types.CopyHeader(header), nil
}
