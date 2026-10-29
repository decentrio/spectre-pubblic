package opstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	attestortypes "attestor/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type fakeGames struct {
	games          []ProposedRoot
	finalizedGames []ProposedRoot
	errAt          map[uint64]error
	countErr       error
	finalizedBlock uint64
	finalizedErr   error
}

func (f *fakeGames) GameCount(_ context.Context) (uint64, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	return uint64(len(f.games)), nil
}

func (f *fakeGames) GameAtIndex(_ context.Context, idx uint64) (ProposedRoot, error) {
	if err, ok := f.errAt[idx]; ok {
		return ProposedRoot{}, err
	}
	if idx >= uint64(len(f.games)) {
		return ProposedRoot{}, errors.New("index out of range")
	}
	return f.games[idx], nil
}

func (f *fakeGames) FinalizedView(context.Context) (gameSource, uint64, error) {
	if f.finalizedErr != nil {
		return nil, 0, f.finalizedErr
	}
	if f.finalizedGames != nil {
		view := *f
		view.games = f.finalizedGames
		view.finalizedGames = nil
		return &view, f.finalizedBlock, nil
	}
	return f, f.finalizedBlock, nil
}

type fakeReplica struct {
	status      SyncStatus
	statusErr   error
	outputs     map[uint64][32]byte
	outputErr   map[uint64]error
	commitments map[uint64]L2Commitment
}

func (f *fakeReplica) SyncStatus(_ context.Context) (SyncStatus, error) {
	if f.statusErr != nil {
		return SyncStatus{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeReplica) OutputAtBlock(_ context.Context, l2Block uint64) ([32]byte, error) {
	if err, ok := f.outputErr[l2Block]; ok {
		return [32]byte{}, err
	}
	root, ok := f.outputs[l2Block]
	if !ok {
		return [32]byte{}, errors.New("no output for block")
	}
	return root, nil
}

// commitments overrides what CommitmentAt returns; when a block is absent it
// falls back to the output-root map so existing tests need no new fixtures.
func (f *fakeReplica) CommitmentAt(ctx context.Context, l2Block uint64) (L2Commitment, error) {
	if c, ok := f.commitments[l2Block]; ok {
		return c, nil
	}
	root, err := f.OutputAtBlock(ctx, l2Block)
	if err != nil {
		return L2Commitment{}, err
	}
	return L2Commitment{BlockNumber: l2Block, OutputRoot: root}, nil
}

type fakeHook struct {
	calls []uint64 // game indices
	err   error
}

func (f *fakeHook) OnMismatch(_ context.Context, game ProposedRoot, _ [32]byte) error {
	f.calls = append(f.calls, game.GameIndex)
	return f.err
}

func root(b byte) [32]byte {
	var r [32]byte
	r[0] = b
	return r
}

func game(idx uint64, gameType uint32, l2Block uint64, claim [32]byte) ProposedRoot {
	return ProposedRoot{
		GameIndex:     idx,
		GameAddress:   common.BytesToAddress([]byte{byte(idx + 1)}),
		GameType:      gameType,
		RootClaim:     claim,
		L2BlockNumber: l2Block,
		L1Timestamp:   1000 + idx,
	}
}

func newTestAttestor(t *testing.T, head Head, games *fakeGames, rep *fakeReplica, hook ChallengeHook) *OpStackAttestor {
	t.Helper()
	store, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	store.Bootstrap(0)
	if hook == nil {
		hook = &fakeHook{}
	}
	cfg := Config{
		SrcChain:           "op-test",
		L1RpcUrl:           "http://localhost:1",
		OpNodeRpcUrl:       "http://localhost:2",
		DisputeGameFactory: common.BytesToAddress([]byte{0xfa}),
		RespectedGameType:  0,
		AttestationHead:    head,
		StatePath:          store.path,
		// Verdict-focused tests disable self-derived attestation; derived
		// tests flip this off explicitly.
		DisableDerivedRoots: true,
	}
	cfg.applyDefaults()
	a := New(cfg, games, rep, store, hook, nil, zap.NewNop().Sugar())
	a.now = func() time.Time { return time.Unix(5000, 0) }
	return a
}

func TestRunOnceMatchAttests(t *testing.T) {
	claim := root(0xaa)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 150, UnsafeL2: 160, FinalizedL2: 150}, outputs: map[uint64][32]byte{100: claim}}
	hook := &fakeHook{}
	a := newTestAttestor(t, HeadSafe, games, rep, hook)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if got := a.store.Pending(); len(got) != 0 {
		t.Fatalf("pending = %d entries, want 0", len(got))
	}
	cur, ok := a.ConfirmedUpTo()
	if !ok || cur.Height != 100 || cur.ID != claim {
		t.Fatalf("ConfirmedUpTo = (%+v, %t), want height 100 root %x", cur, ok, claim)
	}
	if len(hook.calls) != 0 {
		t.Fatalf("hook called %d times on a match, want 0", len(hook.calls))
	}
}

func TestIngestUsesOnlyTheFinalizedFactoryView(t *testing.T) {
	claim := root(0xaa)
	games := &fakeGames{
		// This is visible at latest L1 but absent from the finalized snapshot.
		games:          []ProposedRoot{game(0, 0, 100, claim)},
		finalizedGames: []ProposedRoot{},
		finalizedBlock: 500,
	}
	rep := &fakeReplica{status: SyncStatus{FinalizedL2: 100}, outputs: map[uint64][32]byte{100: claim}}
	a := newTestAttestor(t, HeadFinalized, games, rep, nil)
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce at finalized view 500: %v", err)
	}
	if got := a.store.NextGameIndex(); got != 0 {
		t.Fatalf("cursor after latest-only game = %d, want 0", got)
	}
	if got := a.store.LastFinalizedL1Block(); got != 500 {
		t.Fatalf("persisted finalized L1 marker = %d, want 500", got)
	}
	if _, found := a.store.HighestAttestedAtOrBelow(100, true); found {
		t.Fatal("latest-only game was published before its L1 evidence finalized")
	}
	// A latest-L1 reorg can replace the still-unfinalized object repeatedly.
	// Neither replacement may advance the cursor or become provenance for the
	// feed while the finalized view remains unchanged.
	latestReplacement := root(0xbb)
	games.games = []ProposedRoot{game(0, 0, 100, latestReplacement)}
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce after latest-only factory reorg: %v", err)
	}
	if got := a.store.NextGameIndex(); got != 0 {
		t.Fatalf("cursor after latest-only factory reorg = %d, want 0", got)
	}
	if _, found := a.store.HighestAttestedAtOrBelow(100, true); found {
		t.Fatal("latest-only replacement became provenance for the feed before finalization")
	}

	// Advancing the finalized view makes the exact game available once.
	games.finalizedGames = []ProposedRoot{game(0, 0, 100, claim)}
	games.finalizedBlock = 501
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce at finalized view 501: %v", err)
	}
	if got := a.store.NextGameIndex(); got != 1 {
		t.Fatalf("cursor after finalized game = %d, want 1", got)
	}
	if got := a.store.LastFinalizedL1Block(); got != 501 {
		t.Fatalf("persisted finalized L1 marker = %d, want 501", got)
	}
	root, found := a.store.HighestAttestedAtOrBelow(100, false)
	if !found || root.GameIndex != 0 || root.Root != claim {
		t.Fatalf("finalized game root = (%+v, %t)", root, found)
	}
}

func TestIngestPersistsBatchOnce(t *testing.T) {
	claim := root(0xaa)
	games := &fakeGames{games: []ProposedRoot{
		game(0, 0, 100, claim),
		game(1, 0, 101, claim),
		game(2, 0, 102, claim),
	}}
	a := newTestAttestor(t, HeadFinalized, games, nil, nil)
	fs := realStateFileSystem()
	createTemp := fs.createTemp
	persisted := 0
	fs.createTemp = func(dir, pattern string) (stateFile, error) {
		persisted++
		return createTemp(dir, pattern)
	}
	a.store.fs = fs

	if err := a.ingest(context.Background()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := a.store.NextGameIndex(); got != 3 {
		t.Fatalf("ingest cursor = %d, want 3", got)
	}
	if persisted != 1 {
		t.Fatalf("ingest persisted %d times, want one batch commit", persisted)
	}
}

func TestRunOnceMismatchRecordsAndHooks(t *testing.T) {
	claim := root(0xaa)
	derived := root(0xab) // single-byte divergence
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 150, FinalizedL2: 150}, outputs: map[uint64][32]byte{100: derived}}
	hook := &fakeHook{}
	a := newTestAttestor(t, HeadSafe, games, rep, hook)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if _, ok := a.ConfirmedUpTo(); ok {
		t.Fatal("mismatch must not enter the attested feed")
	}
	if len(a.store.Pending()) != 0 {
		t.Fatal("mismatch is a terminal verdict; game must leave pending")
	}
	if len(hook.calls) != 1 || hook.calls[0] != 0 {
		t.Fatalf("hook calls = %v, want exactly [0]", hook.calls)
	}
	mismatches := a.store.state.Mismatches
	if len(mismatches) != 1 || mismatches[0].LocalRoot != derived {
		t.Fatalf("mismatch record = %+v, want one entry with local root %x", mismatches, derived)
	}
}

func TestMismatchIsRetainedWhenChallengeHookFails(t *testing.T) {
	claim := root(0xaa)
	local := root(0xab)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	replica := &fakeReplica{
		status:  SyncStatus{SafeL2: 100, FinalizedL2: 100},
		outputs: map[uint64][32]byte{100: local},
	}
	hook := &fakeHook{err: errors.New("challenge transaction rejected")}
	a := newTestAttestor(t, HeadSafe, games, replica, hook)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: mismatch hook errors must not erase verdict: %v", err)
	}
	if got := a.store.state.Mismatches; len(got) != 1 || got[0].LocalRoot != local {
		t.Fatalf("mismatches after hook failure = %+v", got)
	}
	if pending := a.store.Pending(); len(pending) != 0 {
		t.Fatalf("pending after hook failure = %+v", pending)
	}
}

// hangingHook never returns until its context ends, like a challenge
// transaction submitted to an endpoint that does not answer.
type hangingHook struct{}

func (hangingHook) OnMismatch(ctx context.Context, _ ProposedRoot, _ [32]byte) error {
	<-ctx.Done()
	return ctx.Err()
}

// The hook is called with the loop's context and, once it submits a challenge
// transaction, will make network calls. A hook that never returns must be cut
// like any other call: the verdict stays recorded, the failure is logged, and the
// pass goes on.
func TestHungChallengeHookIsCutAndTheVerdictKept(t *testing.T) {
	claim := root(0xaa)
	local := root(0xab)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	replica := &fakeReplica{
		status:  SyncStatus{SafeL2: 100, FinalizedL2: 100},
		outputs: map[uint64][32]byte{100: local},
	}
	a := newTestAttestor(t, HeadSafe, games, replica, hangingHook{})
	a.cfg.CallTimeout = 50 * time.Millisecond
	core, logs := observer.New(zapcore.ErrorLevel)
	a.logger = zap.New(core).Sugar()

	done := make(chan error, 1)
	go func() { done <- a.runOnce(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runOnce: a cut hook must not fail the pass: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung challenge hook parked the pass")
	}
	if got := a.store.state.Mismatches; len(got) != 1 || got[0].LocalRoot != local {
		t.Fatalf("mismatches after a cut hook = %+v", got)
	}
	failure := logs.FilterMessageSnippet("challenge hook failed")
	if failure.Len() != 1 || !strings.Contains(failure.All()[0].Message, "challenge hook exceeded its 50ms deadline") {
		t.Fatalf("hook failure log = %v, want one line naming the hook and its deadline", failure.All())
	}
}

func TestIngestErrorStopsCursor(t *testing.T) {
	claim := root(0x01)
	games := &fakeGames{
		games: []ProposedRoot{game(0, 0, 100, claim), game(1, 0, 200, claim), game(2, 0, 300, claim)},
		errAt: map[uint64]error{1: errors.New("rpc down")},
	}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 1000, FinalizedL2: 1000}, outputs: map[uint64][32]byte{100: claim}}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err == nil {
		t.Fatal("runOnce must surface the ingest error")
	}
	if got := a.store.NextGameIndex(); got != 1 {
		t.Fatalf("ingest cursor = %d, want 1 (stopped at the failed index)", got)
	}
	if got := a.store.Pending(); len(got) != 1 || got[0].GameIndex != 0 {
		t.Fatalf("pending = %+v, want only game 0", got)
	}

	// Recovery: the error clears, the scan resumes exactly at index 1.
	delete(games.errAt, 1)
	rep.outputs[200] = claim
	rep.outputs[300] = claim
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce after recovery: %v", err)
	}
	if got := a.store.NextGameIndex(); got != 3 {
		t.Fatalf("ingest cursor = %d after recovery, want 3", got)
	}
	if cur, ok := a.ConfirmedUpTo(); !ok || cur.Height != 300 {
		t.Fatalf("ConfirmedUpTo = (%+v, %t), want height 300", cur, ok)
	}
}

func TestGameCountErrorStopsBeforeAnyStateChange(t *testing.T) {
	games := &fakeGames{countErr: errors.New("factory unavailable")}
	replica := &fakeReplica{}
	a := newTestAttestor(t, HeadFinalized, games, replica, nil)

	if err := a.runOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "factory unavailable") {
		t.Fatalf("runOnce error = %v, want GameCount failure", err)
	}
	if got := a.store.NextGameIndex(); got != 0 {
		t.Fatalf("ingest cursor = %d, want 0", got)
	}
	if pending := a.store.Pending(); len(pending) != 0 {
		t.Fatalf("pending after GameCount failure = %+v", pending)
	}
}

func TestReadOnlyReplicaFailureMatrix(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	withoutReplica := New(Config{SrcChain: "op-test"}, nil, nil, store, nil, nil, zap.NewNop().Sugar())
	if _, err := withoutReplica.HeadAt(context.Background(), HeadSafe); !errors.Is(err, ErrNoReplica) {
		t.Fatalf("HeadAt without replica error = %v, want ErrNoReplica", err)
	}
	if _, err := withoutReplica.CommitmentAt(context.Background(), 100); !errors.Is(err, ErrNoReplica) {
		t.Fatalf("CommitmentAt without replica error = %v, want ErrNoReplica", err)
	}

	replica := &fakeReplica{statusErr: errors.New("sync status unavailable")}
	withReplica := New(Config{SrcChain: "op-test"}, nil, replica, store, nil, nil, zap.NewNop().Sugar())
	if _, err := withReplica.HeadAt(context.Background(), HeadSafe); err == nil || !strings.Contains(err.Error(), "sync status unavailable") {
		t.Fatalf("HeadAt sync error = %v", err)
	}
	replica.statusErr = nil
	replica.status = SyncStatus{SafeL2: 100}
	if _, err := withReplica.HeadAt(context.Background(), Head("unknown")); err == nil || !strings.Contains(err.Error(), "unknown attestation head") {
		t.Fatalf("HeadAt invalid head error = %v", err)
	}
	replica.outputErr = map[uint64]error{100: errors.New("commitment unavailable")}
	if _, err := withReplica.CommitmentAt(context.Background(), 100); err == nil || !strings.Contains(err.Error(), "commitment unavailable") {
		t.Fatalf("CommitmentAt error = %v", err)
	}
}

func TestReplicaLagKeepsPending(t *testing.T) {
	claim := root(0x02)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 500, claim)}}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 400, UnsafeL2: 450, FinalizedL2: 400}} // safe head behind the proposal
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("a lagging replica is not an error: %v", err)
	}
	if got := a.store.Pending(); len(got) != 1 {
		t.Fatalf("pending = %d entries, want 1 (undecided while safe head lags)", len(got))
	}
	if _, ok := a.ConfirmedUpTo(); ok {
		t.Fatal("nothing may be affirmed while the safe head lags")
	}
}

func TestReplicaErrorKeepsPendingAndFails(t *testing.T) {
	claim := root(0x03)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	rep := &fakeReplica{
		status:    SyncStatus{SafeL2: 150, FinalizedL2: 150},
		outputErr: map[uint64]error{100: errors.New("op-node down")},
	}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err == nil {
		t.Fatal("replica output error must fail the pass")
	}
	if got := a.store.Pending(); len(got) != 1 {
		t.Fatalf("pending = %d entries, want 1 (no verdict on error)", len(got))
	}
	if _, ok := a.ConfirmedUpTo(); ok {
		t.Fatal("nothing may be affirmed on a replica error")
	}
}

func TestSyncStatusErrorDecidesNothing(t *testing.T) {
	claim := root(0x04)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	rep := &fakeReplica{statusErr: errors.New("sync status down")}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err == nil {
		t.Fatal("sync status error must fail the pass")
	}
	if got := a.store.Pending(); len(got) != 1 {
		t.Fatalf("pending = %d entries, want 1", len(got))
	}
}

func TestUnrespectedGameTypeSkipped(t *testing.T) {
	claim := root(0x05)
	games := &fakeGames{games: []ProposedRoot{
		game(0, 42, 100, claim), // wrong type
		game(1, 0, 200, claim),
	}}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 1000, FinalizedL2: 1000}, outputs: map[uint64][32]byte{200: claim}}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if got := a.store.NextGameIndex(); got != 2 {
		t.Fatalf("cursor = %d, want 2 (skipped game still advances ingest)", got)
	}
	cur, ok := a.ConfirmedUpTo()
	if !ok || cur.Height != 200 {
		t.Fatalf("ConfirmedUpTo = (%+v, %t), want height 200 only", cur, ok)
	}
}

func TestUnsafeProvisionalThenConfirmed(t *testing.T) {
	claim := root(0x06)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 500, claim)}}
	// Unsafe head covers the block, finalized head does not: provisional verdict.
	rep := &fakeReplica{status: SyncStatus{SafeL2: 400, UnsafeL2: 600, FinalizedL2: 400}, outputs: map[uint64][32]byte{500: claim}}
	a := newTestAttestor(t, HeadUnsafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if _, ok := a.ConfirmedUpTo(); ok {
		t.Fatal("provisional roots must not count as attested by default")
	}
	if cur, ok := a.store.AttestedUpTo(true); !ok || cur.Height != 500 {
		t.Fatalf("includeProvisional AttestedUpTo = (%+v, %t), want height 500", cur, ok)
	}
	if got := a.store.RecheckList(); len(got) != 1 || !got[0].ProvisionalMatch {
		t.Fatalf("recheck list = %+v, want one provisional match", got)
	}

	// Finalized head catches up and agrees: provisional flag clears.
	rep.status.SafeL2 = 600
	rep.status.FinalizedL2 = 600
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (recheck): %v", err)
	}
	if cur, ok := a.ConfirmedUpTo(); !ok || cur.Height != 500 {
		t.Fatalf("ConfirmedUpTo after confirmation = (%+v, %t), want height 500", cur, ok)
	}
	if got := a.store.RecheckList(); len(got) != 0 {
		t.Fatalf("recheck list not drained: %+v", got)
	}
}

func TestUnsafeProvisionalMatchDivergesAtSafe(t *testing.T) {
	claim := root(0x07)
	divergent := root(0x08)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 500, claim)}}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 400, UnsafeL2: 600, FinalizedL2: 400}, outputs: map[uint64][32]byte{500: claim}}
	hook := &fakeHook{}
	a := newTestAttestor(t, HeadUnsafe, games, rep, hook)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	// Finalized derivation disagrees with what the sequencer gossiped.
	rep.outputs[500] = divergent
	rep.status.SafeL2 = 600
	rep.status.FinalizedL2 = 600
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (recheck): %v", err)
	}
	if _, ok := a.store.AttestedUpTo(true); ok {
		t.Fatal("diverged provisional match must be removed from the feed")
	}
	if len(a.store.state.Mismatches) != 1 {
		t.Fatalf("mismatches = %+v, want the corrected safe-head verdict", a.store.state.Mismatches)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook calls = %v, want exactly one (from the authoritative safe verdict)", hook.calls)
	}
	if got := a.store.RecheckList(); len(got) != 0 {
		t.Fatalf("recheck list not drained: %+v", got)
	}
}

func TestUnsafeProvisionalMismatchConfirmedLegitAtSafe(t *testing.T) {
	claim := root(0x09)
	gossiped := root(0x0a)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 500, claim)}}
	// Gossip disagrees with the claim → provisional mismatch...
	rep := &fakeReplica{status: SyncStatus{SafeL2: 400, UnsafeL2: 600, FinalizedL2: 400}, outputs: map[uint64][32]byte{500: gossiped}}
	hook := &fakeHook{}
	a := newTestAttestor(t, HeadUnsafe, games, rep, hook)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("provisional mismatch should raise the hook once, got %v", hook.calls)
	}
	// ...but honest finalized derivation agrees with the claim: proposer was
	// honest, the sequencer gossip diverged.
	rep.outputs[500] = claim
	rep.status.SafeL2 = 600
	rep.status.FinalizedL2 = 600
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (recheck): %v", err)
	}
	cur, ok := a.ConfirmedUpTo()
	if !ok || cur.Height != 500 || cur.ID != claim {
		t.Fatalf("ConfirmedUpTo = (%+v, %t), want confirmed height 500", cur, ok)
	}
	if len(a.store.state.Mismatches) != 0 {
		t.Fatalf("provisional mismatch must be superseded, got %+v", a.store.state.Mismatches)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("no additional hook call on a safe-head match, got %v", hook.calls)
	}
}

func TestSafeModeNeverPopulatesRecheck(t *testing.T) {
	claim := root(0x0b)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 100, claim)}}
	rep := &fakeReplica{status: SyncStatus{SafeL2: 150, UnsafeL2: 200, FinalizedL2: 150}, outputs: map[uint64][32]byte{100: claim}}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if got := a.store.RecheckList(); len(got) != 0 {
		t.Fatalf("safe mode populated the recheck list: %+v", got)
	}
	feed := a.store.state.Attested
	if len(feed) != 1 || feed[0].Provisional {
		t.Fatalf("feed = %+v, want one non-provisional entry", feed)
	}
}

func TestBootstrapStartIndex(t *testing.T) {
	games := &fakeGames{games: []ProposedRoot{
		{GameIndex: 0, L1Timestamp: 100},
		{GameIndex: 1, L1Timestamp: 200},
		{GameIndex: 2, L1Timestamp: 300},
		{GameIndex: 3, L1Timestamp: 400},
	}}
	ctx := context.Background()
	cases := []struct {
		cutoff uint64
		want   uint64
	}{
		{cutoff: 0, want: 0},
		{cutoff: 150, want: 1},
		{cutoff: 300, want: 2},
		{cutoff: 999, want: 4}, // everything older → start at the live edge
	}
	for _, tc := range cases {
		got, err := bootstrapStartIndex(ctx, games, 4, tc.cutoff)
		if err != nil {
			t.Fatalf("bootstrapStartIndex(cutoff=%d): %v", tc.cutoff, err)
		}
		if got != tc.want {
			t.Errorf("bootstrapStartIndex(cutoff=%d) = %d, want %d", tc.cutoff, got, tc.want)
		}
	}
	if got, err := bootstrapStartIndex(ctx, games, 0, 100); err != nil || got != 0 {
		t.Errorf("empty factory: got (%d, %v), want (0, nil)", got, err)
	}
}

func TestSafeVerdictAboveFinalizedIsProvisional(t *testing.T) {
	claim := root(0x0c)
	games := &fakeGames{games: []ProposedRoot{game(0, 0, 500, claim)}}
	// Safe covers the block, finalized does not: verdict must be provisional
	// (the safe head can still reorg with L1).
	rep := &fakeReplica{status: SyncStatus{SafeL2: 600, UnsafeL2: 700, FinalizedL2: 400}, outputs: map[uint64][32]byte{500: claim}}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if _, ok := a.ConfirmedUpTo(); ok {
		t.Fatal("safe verdict above finalized must not count as confirmed")
	}
	if cur, ok := a.store.AttestedUpTo(true); !ok || cur.Height != 500 {
		t.Fatalf("provisional AttestedUpTo = (%+v, %t), want height 500", cur, ok)
	}
	if got := a.store.RecheckList(); len(got) != 1 {
		t.Fatalf("recheck list = %+v, want one entry", got)
	}

	// Finalized catches up and agrees.
	rep.status.FinalizedL2 = 600
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (recheck): %v", err)
	}
	if cur, ok := a.ConfirmedUpTo(); !ok || cur.Height != 500 {
		t.Fatalf("ConfirmedUpTo after confirmation = (%+v, %t), want height 500", cur, ok)
	}
}

func TestDerivedAttestationConfirmedAtFinalized(t *testing.T) {
	rootA, rootB := root(0x20), root(0x21)
	games := &fakeGames{}
	rep := &fakeReplica{
		status:  SyncStatus{SafeL2: 1000, UnsafeL2: 1000, FinalizedL2: 1000},
		outputs: map[uint64][32]byte{1000: rootA, 1200: rootB},
	}
	a := newTestAttestor(t, HeadFinalized, games, rep, nil)
	a.cfg.DisableDerivedRoots = false

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	cur, ok := a.ConfirmedUpTo()
	if !ok || cur.Height != 1000 || cur.ID != rootA {
		t.Fatalf("ConfirmedUpTo = (%+v, %t), want confirmed derived root at 1000", cur, ok)
	}

	// Head advances less than the gap: no new derived root.
	rep.status.FinalizedL2 = 1000 + a.cfg.DerivedGapBlocks - 1
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (below gap): %v", err)
	}
	if cur, _ := a.ConfirmedUpTo(); cur.Height != 1000 {
		t.Fatalf("derived attested below the gap: %+v", cur)
	}

	// Head advances past the gap: new derived root.
	rep.status.FinalizedL2 = 1200
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (past gap): %v", err)
	}
	if cur, _ := a.ConfirmedUpTo(); cur.Height != 1200 || cur.ID != rootB {
		t.Fatalf("ConfirmedUpTo = %+v, want derived root at 1200", cur)
	}
}

func TestDerivedProvisionalReverify(t *testing.T) {
	rootA := root(0x22)
	games := &fakeGames{}
	// Safe head ahead of finalized: derived attestation is provisional.
	rep := &fakeReplica{
		status:  SyncStatus{SafeL2: 1000, UnsafeL2: 1100, FinalizedL2: 800},
		outputs: map[uint64][32]byte{1000: rootA},
	}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)
	a.cfg.DisableDerivedRoots = false

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if _, ok := a.ConfirmedUpTo(); ok {
		t.Fatal("provisional derived root must not count as confirmed")
	}
	if cur, ok := a.store.AttestedUpTo(true); !ok || cur.Height != 1000 {
		t.Fatalf("provisional AttestedUpTo = (%+v, %t), want 1000", cur, ok)
	}

	// Finalized covers the block, derivation agrees: confirmed in place.
	rep.status.FinalizedL2 = 1000
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (reverify): %v", err)
	}
	if cur, ok := a.ConfirmedUpTo(); !ok || cur.Height != 1000 || cur.ID != rootA {
		t.Fatalf("ConfirmedUpTo after reverify = (%+v, %t), want confirmed 1000", cur, ok)
	}
}

func TestDerivedDivergenceCorrected(t *testing.T) {
	provisionalRoot, finalRoot := root(0x23), root(0x24)
	games := &fakeGames{}
	rep := &fakeReplica{
		status:  SyncStatus{SafeL2: 1000, UnsafeL2: 1100, FinalizedL2: 800},
		outputs: map[uint64][32]byte{1000: provisionalRoot},
	}
	a := newTestAttestor(t, HeadSafe, games, rep, nil)
	a.cfg.DisableDerivedRoots = false

	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	// L1 reorg scenario: the finalized chain derives a different root.
	rep.outputs[1000] = finalRoot
	rep.status.FinalizedL2 = 1000
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce (reverify): %v", err)
	}
	cur, ok := a.ConfirmedUpTo()
	if !ok || cur.Height != 1000 || cur.ID != finalRoot {
		t.Fatalf("ConfirmedUpTo = (%+v, %t), want corrected finalized root %x", cur, ok, finalRoot)
	}
	if got := a.store.DerivedProvisionalAtOrBelow(1000); len(got) != 0 {
		t.Fatalf("provisional derived entries not drained: %+v", got)
	}
}

func TestPruneDerivedKeepsGamesAndProvisional(t *testing.T) {
	s, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(4000, 0)
	s.RecordMatch(testGame(0, 50, root(0x30)), now, false) // game entry, never pruned
	s.AppendDerived(100, root(0x31), now, false)
	s.AppendDerived(200, root(0x32), now, false)
	s.AppendDerived(300, root(0x33), now, false)
	s.AppendDerived(400, root(0x34), now, true) // provisional, never pruned

	if removed := s.PruneDerived(2); removed != 1 {
		t.Fatalf("PruneDerived removed %d, want 1", removed)
	}
	// The derived entry at 100 is gone; the query falls back to the game
	// entry at 50, which pruning never touches.
	if got, ok := s.HighestAttestedAtOrBelow(100, false); !ok || got.L2BlockNumber != 50 || got.Source != SourceGame {
		t.Fatalf("at 100 after prune: (%+v, %t), want the game entry at 50", got, ok)
	}
	if got, ok := s.HighestAttestedAtOrBelow(250, false); !ok || got.L2BlockNumber != 200 {
		t.Fatalf("at 250 after prune: (%+v, %t), want the surviving derived entry at 200", got, ok)
	}
	if cur, ok := s.AttestedUpTo(true); !ok || cur.Height != 400 {
		t.Fatalf("provisional derived entry must survive pruning, got (%+v, %t)", cur, ok)
	}
	if removed := s.PruneDerived(2); removed != 0 {
		t.Fatalf("second prune removed %d, want 0", removed)
	}
}

// hangingReplica parks SyncStatus until its context ends, like a connection
// whose response never arrives.
type hangingReplica struct{ fakeReplica }

func (h *hangingReplica) SyncStatus(ctx context.Context) (SyncStatus, error) {
	<-ctx.Done()
	return SyncStatus{}, ctx.Err()
}

// stallingReplica serves its first SyncStatus and parks on every later one.
type stallingReplica struct {
	fakeReplica
	calls atomic.Int32
}

func (s *stallingReplica) SyncStatus(ctx context.Context) (SyncStatus, error) {
	if s.calls.Add(1) == 1 {
		return s.status, nil
	}
	<-ctx.Done()
	return SyncStatus{}, ctx.Err()
}

// hangingGames parks the finalized factory view until its context ends.
type hangingGames struct{ fakeGames }

func (h *hangingGames) FinalizedView(ctx context.Context) (gameSource, uint64, error) {
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

// testClock is a clock the test advances by hand; it is read by the run loop's
// goroutine and the test's, so it is atomic.
type testClock struct{ ns atomic.Int64 }

func newTestClock(at time.Time) *testClock {
	c := &testClock{}
	c.ns.Store(at.UnixNano())
	return c
}

func (c *testClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *testClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func captureWarnings(a *OpStackAttestor) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.WarnLevel)
	a.logger = zap.New(core).Sugar()
	return logs
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func counterTotal(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var total float64
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
		return total
	}
	return 0
}

func stopRun(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A call that never returns used to park the single feed goroutine forever:
// no error, no backoff, no log, no metric. The call bound turns it into an
// ordinary failed pass, and the log line names the call that was cut.
func TestHungOpNodeCallIsCutAndLogged(t *testing.T) {
	a := newTestAttestor(t, HeadSafe, &fakeGames{}, &fakeReplica{}, nil)
	a.replica = &hangingReplica{}
	a.cfg.CallTimeout = 50 * time.Millisecond
	a.cfg.PollInterval = 10 * time.Millisecond
	logs := captureWarnings(a)
	registry := prometheus.NewRegistry()
	a.metrics = NewMetrics(registry)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer stopRun(t, cancel, done)

	waitUntil(t, "the hung call to be cut and logged", func() bool {
		return logs.FilterMessageSnippet("attestation pass failed").Len() == 1
	})
	message := logs.FilterMessageSnippet("attestation pass failed").All()[0].Message
	if !strings.Contains(message, "op-node SyncStatus exceeded its 50ms deadline") ||
		!strings.Contains(message, "nothing affirmed, retrying in 1m0s") {
		t.Fatalf("log line %q does not name the call, its deadline and the backoff", message)
	}
	if got := counterTotal(t, registry, "spectre_op_attestor_pass_failures_total"); got != 1 {
		t.Fatalf("pass failure counter = %v, want 1", got)
	}
}

func TestRunBoundsTheBootstrapOfAFreshStore(t *testing.T) {
	a := newTestAttestor(t, HeadSafe, &fakeGames{}, &fakeReplica{}, nil)
	fresh, err := LoadStore(filepath.Join(t.TempDir(), "fresh.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	a.store = fresh
	a.games = &hangingGames{}
	a.cfg.CallTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "bootstrap failed") ||
			!strings.Contains(err.Error(), "factory FinalizedView exceeded its 50ms deadline") {
			t.Fatalf("Run error = %v, want a bootstrap failure naming the call and its deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung bootstrap parked Run")
	}
}

// RunnerAlive is what tells a parked loop from a healthy one: LastSyncStatus
// keeps returning the last snapshot either way, so health that read only that
// stayed SERVING over a frozen feed.
func TestRunnerAliveTellsAParkedLoopFromAHealthyOne(t *testing.T) {
	rep := &stallingReplica{}
	rep.status = SyncStatus{SafeL2: 150, UnsafeL2: 160, FinalizedL2: 150}
	a := newTestAttestor(t, HeadSafe, &fakeGames{}, &fakeReplica{}, nil)
	a.replica = rep
	clock := newTestClock(time.Unix(5000, 0))
	a.now = clock.now
	// A long call bound keeps the second pass parked in its first call for the
	// whole test, so what is measured is the heartbeat and not the bound.
	a.cfg.CallTimeout = time.Hour
	a.cfg.PollInterval = 10 * time.Millisecond
	staleAfter := time.Hour + 20*time.Millisecond

	if a.RunnerAlive() {
		t.Fatal("a loop that never started is alive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer stopRun(t, cancel, done)

	waitUntil(t, "the first pass to record a snapshot and the loop to beat", func() bool {
		_, found := a.LastSyncStatus()
		return found && a.RunnerAlive()
	})
	waitUntil(t, "the second pass to park", func() bool { return rep.calls.Load() >= 2 })

	clock.advance(30 * time.Minute)
	if !a.RunnerAlive() {
		t.Fatal("a call that has run for less than the call bound read as a dead loop")
	}
	clock.advance(staleAfter)
	if _, found := a.LastSyncStatus(); !found {
		t.Fatal("the snapshot disappeared")
	}
	if a.RunnerAlive() {
		t.Fatal("a loop parked beyond the call bound and two ticks still reads as alive")
	}
}

// A pass that fails is not a parked loop: it is reported by the log and the
// failure counter, and it backs off, but the loop keeps coming back around.
func TestRunnerStaysAliveWhilePassesFail(t *testing.T) {
	a := newTestAttestor(t, HeadSafe, &fakeGames{}, &fakeReplica{statusErr: errors.New("op-node unavailable")}, nil)
	clock := newTestClock(time.Unix(5000, 0))
	a.now = clock.now
	// staleAfter is about 1s, well under the 1m failure backoff, so the loop
	// is only alive through the backoff if it beats without running a pass.
	a.cfg.CallTimeout = time.Second
	a.cfg.PollInterval = 10 * time.Millisecond
	logs := captureWarnings(a)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer stopRun(t, cancel, done)

	waitUntil(t, "the failing pass to be logged", func() bool {
		return logs.FilterMessageSnippet("attestation pass failed").Len() == 1
	})
	clock.advance(30 * time.Second) // inside the 1m backoff: ticks arrive, no pass runs
	// The advance left the last beat 30s old, far past staleAfter: only a beat
	// at the new time makes the loop alive again.
	waitUntil(t, "the loop to beat again during the backoff", a.RunnerAlive)
	if got := logs.FilterMessageSnippet("attestation pass failed").Len(); got != 1 {
		t.Fatalf("%d passes failed, want 1: the backoff should have held the next pass back", got)
	}
	if _, found := a.LastSyncStatus(); found {
		t.Fatal("a failed pass recorded a snapshot")
	}
}

// hookedReplica runs a hook inside each OutputAtBlock call, before the output is
// read: the place a slow endpoint, or one that advances a test clock, is stood
// in for.
type hookedReplica struct {
	fakeReplica
	hook  func(ctx context.Context, call int) error
	calls atomic.Int32
}

func (h *hookedReplica) OutputAtBlock(ctx context.Context, l2Block uint64) ([32]byte, error) {
	call := int(h.calls.Add(1))
	if h.hook != nil {
		if err := h.hook(ctx, call); err != nil {
			return [32]byte{}, err
		}
	}
	return h.fakeReplica.OutputAtBlock(ctx, l2Block)
}

// newPendingGames builds an attestor with n ingested games that the replica
// already covers, so a pass decides each of them with one OutputAtBlock call.
func newPendingGames(t *testing.T, n int) (*OpStackAttestor, *hookedReplica) {
	t.Helper()
	games := &fakeGames{}
	outputs := map[uint64][32]byte{}
	for i := 0; i < n; i++ {
		claim := root(byte(i + 1))
		games.games = append(games.games, game(uint64(i), 0, 100+uint64(i), claim))
		outputs[100+uint64(i)] = claim
	}
	replica := &hookedReplica{fakeReplica: fakeReplica{
		status:  SyncStatus{SafeL2: 200, UnsafeL2: 200, FinalizedL2: 200},
		outputs: outputs,
	}}
	a := newTestAttestor(t, HeadSafe, games, &fakeReplica{}, nil)
	a.replica = replica
	return a, replica
}

// A pass is a loop over work the runner does not control, and it can take far
// longer than any one call. Each call here is well inside its bound; only their
// sum is longer than one call's bound. A deadline on the whole pass would cut it
// and send it to the failure backoff, with the games behind the cut waiting for
// the next pass.
func TestALongPassOfSlowCallsIsNotCut(t *testing.T) {
	const games = 10
	a, replica := newPendingGames(t, games)
	a.cfg.CallTimeout = 100 * time.Millisecond
	a.cfg.PollInterval = time.Hour // only the immediate first pass runs
	replica.hook = func(ctx context.Context, _ int) error {
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	logs := captureWarnings(a)

	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer stopRun(t, cancel, done)

	waitUntil(t, "every game to be decided in the first pass", func() bool {
		return a.store.NextGameIndex() == games && len(a.store.Pending()) == 0
	})
	if took := time.Since(started); took <= a.cfg.CallTimeout {
		t.Fatalf("the pass took %s, no longer than one call's bound %s: the test proves nothing", took, a.cfg.CallTimeout)
	}
	if got := logs.FilterMessageSnippet("attestation pass failed").Len(); got != 0 {
		t.Fatalf("%d pass failures logged for a slow but healthy replica", got)
	}
}

// A pass can legitimately run far longer than the staleness window. It stays
// alive because the runner beats as each call returns, not only when the loop
// comes back around.
func TestRunnerStaysAliveThroughALongPassOfSlowCalls(t *testing.T) {
	const games = 10
	a, replica := newPendingGames(t, games)
	clock := newTestClock(time.Unix(5000, 0))
	a.now = clock.now
	a.cfg.CallTimeout = time.Hour
	a.cfg.PollInterval = 10 * time.Millisecond
	staleAfter := attestortypes.FeedStaleAfter(a.cfg.CallTimeout, a.cfg.PollInterval)

	const perCall = 40 * time.Minute // each call "takes" this long on the test clock
	var sampled []bool
	replica.hook = func(context.Context, int) error {
		clock.advance(perCall)
		sampled = append(sampled, a.RunnerAlive()) // while the call is in flight
		return nil
	}

	a.heartbeat.Beat(a.now()) // the loop has started
	startedAt := clock.now()
	if err := a.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if span := clock.now().Sub(startedAt); span <= 2*staleAfter {
		t.Fatalf("the pass spanned %s on the test clock, not longer than twice the %s window: the test proves nothing", span, staleAfter)
	}
	if len(sampled) < games {
		t.Fatalf("%d calls sampled, want at least %d", len(sampled), games)
	}
	for call, alive := range sampled {
		if !alive {
			t.Fatalf("the loop read as dead during call %d of a pass whose calls each returned inside the window", call+1)
		}
	}
	if !a.RunnerAlive() {
		t.Fatal("the loop reads as dead after a pass whose last call just returned")
	}
}

// The pass failure line is where a transport error is logged at run time, and
// net/http quotes the full endpoint URL in it; an RPC provider's API key is in
// that URL.
func TestPassFailureLogDoesNotQuoteTheEndpointKey(t *testing.T) {
	transport := &url.Error{Op: "Post", URL: "http://127.0.0.1:9545/rpc?apikey=SECRET-KEY", Err: io.EOF}
	a := newTestAttestor(t, HeadSafe, &fakeGames{}, &fakeReplica{statusErr: fmt.Errorf("optimism_syncStatus failed: %w", transport)}, nil)
	a.cfg.PollInterval = 10 * time.Millisecond
	logs := captureWarnings(a)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer stopRun(t, cancel, done)

	waitUntil(t, "the failing pass to be logged", func() bool {
		return logs.FilterMessageSnippet("attestation pass failed").Len() == 1
	})
	message := logs.FilterMessageSnippet("attestation pass failed").All()[0].Message
	if strings.Contains(message, "SECRET-KEY") || strings.Contains(message, "apikey") {
		t.Fatalf("log line %q carries the endpoint key", message)
	}
	if !strings.Contains(message, `Post "http://127.0.0.1:9545": EOF`) {
		t.Fatalf("log line %q should still name the host and the failure", message)
	}
}
