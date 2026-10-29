package bold

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	arbitrum "attestor/arbitrum"
	attestortypes "attestor/types"

	"github.com/ethereum/go-ethereum/common"
)

func TestAssertionAttestorPromotesOnlyAfterRollupAndNitroFinalization(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 100)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	proposal := arbitrum.ProposedAssertion{
		AssertionHash:    common.HexToHash("0x01"),
		ParentHash:       common.HexToHash("0x02"),
		L2BlockHash:      common.HexToHash("0x03"),
		InboxAccumulator: common.HexToHash("0x04"),
		L1BlockNumber:    100,
	}
	source := &assertionTestSource{
		finalizedBlock: 100,
		proposals:      []arbitrum.ProposedAssertion{proposal},
		status:         assertionStatusPending,
	}
	commitment := arbitrum.BlockCommitment{
		BlockNumber: 500,
		BlockHash:   proposal.L2BlockHash,
		StateRoot:   common.HexToHash("0x05"),
	}
	resolver := &assertionTestResolver{
		results: map[arbitrum.RunMode]arbitrum.BlockCommitment{
			arbitrum.RunModeSafe: commitment,
		},
		errs: map[arbitrum.RunMode]error{
			arbitrum.RunModeFinalized: fmt.Errorf(
				"%w: finalized head has not reached block",
				arbitrum.ErrCommitmentNotReady,
			),
		},
	}
	loop, err := NewAssertionAttestor(
		source,
		resolver,
		store,
		AssertionAttestorConfig{PollInterval: time.Second, MaxL1BlockRange: 10},
	)
	if err != nil {
		t.Fatalf("create assertion attestor: %v", err)
	}
	loop.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	if err := loop.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync pending assertion: %v", err)
	}
	if _, found := store.HighestAttested(false); found {
		t.Fatal("pending assertion entered finalized frontier")
	}
	root, found := store.HighestAttested(true)
	if !found || !root.Provisional || root.L2BlockNumber != 500 {
		t.Fatalf("provisional frontier: found=%t root=%+v", found, root)
	}

	source.status = assertionStatusConfirmed
	resolver.errs[arbitrum.RunModeFinalized] = nil
	resolver.results[arbitrum.RunModeFinalized] = commitment
	if err := loop.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync confirmed assertion: %v", err)
	}
	root, found = store.HighestAttested(false)
	if !found || root.Provisional || root.AssertionHash != proposal.AssertionHash {
		t.Fatalf("confirmed frontier: found=%t root=%+v", found, root)
	}
}

func TestAssertionAttestorExcludesNitroMismatch(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 10)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	proposal := arbitrum.ProposedAssertion{
		AssertionHash: common.HexToHash("0x11"),
		L2BlockHash:   common.HexToHash("0x12"),
	}
	source := &assertionTestSource{
		finalizedBlock: 10,
		proposals:      []arbitrum.ProposedAssertion{proposal},
		status:         assertionStatusPending,
	}
	resolver := &assertionTestResolver{
		errs: map[arbitrum.RunMode]error{
			arbitrum.RunModeSafe: fmt.Errorf(
				"%w: assertion block is not canonical",
				arbitrum.ErrCommitmentMismatch,
			),
		},
	}
	loop, err := NewAssertionAttestor(
		source,
		resolver,
		store,
		AssertionAttestorConfig{PollInterval: time.Second},
	)
	if err != nil {
		t.Fatalf("create assertion attestor: %v", err)
	}
	if err := loop.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync mismatched assertion: %v", err)
	}
	if _, found := store.HighestAttested(true); found {
		t.Fatal("mismatched assertion entered feed")
	}
	mismatches := store.Mismatches()
	if len(mismatches) != 1 || mismatches[0].AssertionHash != proposal.AssertionHash {
		t.Fatalf("mismatch records: %+v", mismatches)
	}
}

func TestNewAssertionAttestorFailureMatrix(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	source := &assertionTestSource{}
	resolver := &assertionTestResolver{}

	for _, tc := range []struct {
		name string
		make func() (AssertionSource, CommitmentResolver, *arbitrum.AttestedRootStore, AssertionAttestorConfig)
		want string
	}{
		{
			name: "nil source",
			make: func() (AssertionSource, CommitmentResolver, *arbitrum.AttestedRootStore, AssertionAttestorConfig) {
				return nil, resolver, store, AssertionAttestorConfig{}
			},
			want: "source",
		},
		{
			name: "nil resolver",
			make: func() (AssertionSource, CommitmentResolver, *arbitrum.AttestedRootStore, AssertionAttestorConfig) {
				return source, nil, store, AssertionAttestorConfig{}
			},
			want: "resolver",
		},
		{
			name: "nil store",
			make: func() (AssertionSource, CommitmentResolver, *arbitrum.AttestedRootStore, AssertionAttestorConfig) {
				return source, resolver, nil, AssertionAttestorConfig{}
			},
			want: "store",
		},
		{
			name: "negative poll interval",
			make: func() (AssertionSource, CommitmentResolver, *arbitrum.AttestedRootStore, AssertionAttestorConfig) {
				return source, resolver, store, AssertionAttestorConfig{PollInterval: -time.Second}
			},
			want: "poll interval",
		},
		{
			name: "negative call timeout",
			make: func() (AssertionSource, CommitmentResolver, *arbitrum.AttestedRootStore, AssertionAttestorConfig) {
				return source, resolver, store, AssertionAttestorConfig{CallTimeout: -time.Second}
			},
			want: "call timeout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, resolver, store, config := tc.make()
			_, err := NewAssertionAttestor(source, resolver, store, config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewAssertionAttestor error = %v, want %q", err, tc.want)
			}
		})
	}

	loop, err := NewAssertionAttestor(source, resolver, store, AssertionAttestorConfig{})
	if err != nil {
		t.Fatalf("NewAssertionAttestor defaults: %v", err)
	}
	if loop.config.PollInterval != defaultAssertionPollInterval ||
		loop.config.CallTimeout != attestortypes.DefaultCallTimeout ||
		loop.config.MaxL1BlockRange != defaultMaxL1BlockRange {
		t.Fatalf("defaults = %+v", loop.config)
	}
}

func TestAssertionAttestorValidateChainIDsFailureMatrix(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*assertionTestSource)
		want   string
	}{
		{
			name:   "finalized height error",
			mutate: func(s *assertionTestSource) { s.finalizedErr = errors.New("finalized unavailable") },
			want:   "finalized unavailable",
		},
		{
			name:   "chain query error",
			mutate: func(s *assertionTestSource) { s.chainIDsErr = errors.New("chain id unavailable") },
			want:   "chain id unavailable",
		},
		{
			name:   "nil L1 chain id",
			mutate: func(s *assertionTestSource) { s.l1ChainID = big.NewInt(0); s.returnNilL1 = true },
			want:   "nil chain ID",
		},
		{
			name:   "nil L2 chain id",
			mutate: func(s *assertionTestSource) { s.l2ChainID = big.NewInt(0); s.returnNilL2 = true },
			want:   "nil chain ID",
		},
		{
			name:   "wrong L1 chain id",
			mutate: func(s *assertionTestSource) { s.l1ChainID = big.NewInt(2) },
			want:   "L1 chain ID mismatch",
		},
		{
			name:   "wrong L2 chain id",
			mutate: func(s *assertionTestSource) { s.l2ChainID = big.NewInt(42_162) },
			want:   "RollupCore L2 chain ID mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &assertionTestSource{finalizedBlock: 100}
			tc.mutate(source)
			loop, err := NewAssertionAttestor(source, &assertionTestResolver{}, store, AssertionAttestorConfig{})
			if err != nil {
				t.Fatalf("NewAssertionAttestor: %v", err)
			}
			err = loop.ValidateChainIDs(context.Background(), 1, 42_161)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateChainIDs error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAssertionAttestorSyncFailureMatrix(t *testing.T) {
	proposal := arbitrum.ProposedAssertion{
		AssertionHash: common.HexToHash("0x21"),
		L2BlockHash:   common.HexToHash("0x22"),
		L1BlockNumber: 10,
	}
	commitment := arbitrum.BlockCommitment{
		BlockNumber: 10,
		BlockHash:   proposal.L2BlockHash,
		StateRoot:   common.HexToHash("0x23"),
	}

	for _, tc := range []struct {
		name       string
		configure  func(*assertionTestSource, *assertionTestResolver)
		prepare    func(*arbitrum.AttestedRootStore)
		want       string
		wantCursor uint64
	}{
		{
			name: "finalized height error",
			configure: func(s *assertionTestSource, _ *assertionTestResolver) {
				s.finalizedErr = errors.New("finalized unavailable")
			},
			want:       "finalized unavailable",
			wantCursor: 1,
		},
		{
			name: "assertion range error",
			configure: func(s *assertionTestSource, _ *assertionTestResolver) {
				s.assertionsErr = errors.New("logs unavailable")
			},
			want:       "logs unavailable",
			wantCursor: 1,
		},
		{
			name: "assertion status error",
			configure: func(s *assertionTestSource, _ *assertionTestResolver) {
				s.statusErr = errors.New("status unavailable")
			},
			want:       "status unavailable",
			wantCursor: 11,
		},
		{
			name: "unsupported assertion status",
			configure: func(s *assertionTestSource, _ *assertionTestResolver) {
				s.status = assertionStatusConfirmed + 1
			},
			want:       "unsupported status",
			wantCursor: 11,
		},
		{
			name: "confirmed event contradicts pending status",
			configure: func(s *assertionTestSource, _ *assertionTestResolver) {
				s.status = assertionStatusPending
				s.confirmations = []ConfirmedAssertion{{AssertionHash: proposal.AssertionHash, L2BlockHash: proposal.L2BlockHash}}
			},
			want:       "finalized confirmation event but pending status",
			wantCursor: 11,
		},
		{
			name: "nitro resolver error",
			configure: func(_ *assertionTestSource, r *assertionTestResolver) {
				r.errs = map[arbitrum.RunMode]error{arbitrum.RunModeSafe: errors.New("Nitro unavailable")}
			},
			want:       "Nitro unavailable",
			wantCursor: 11,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 1)
			if err != nil {
				t.Fatalf("create store: %v", err)
			}
			source := &assertionTestSource{
				finalizedBlock: 10,
				proposals:      []arbitrum.ProposedAssertion{proposal},
				status:         assertionStatusPending,
			}
			resolver := &assertionTestResolver{results: map[arbitrum.RunMode]arbitrum.BlockCommitment{arbitrum.RunModeSafe: commitment}}
			tc.configure(source, resolver)
			if tc.prepare != nil {
				tc.prepare(store)
			}
			loop, err := NewAssertionAttestor(source, resolver, store, AssertionAttestorConfig{PollInterval: time.Second})
			if err != nil {
				t.Fatalf("NewAssertionAttestor: %v", err)
			}
			err = loop.SyncOnce(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SyncOnce error = %v, want %q", err, tc.want)
			}
			if got := store.NextL1Block(); got != tc.wantCursor {
				t.Fatalf("next L1 block after failure = %d, want %d", got, tc.wantCursor)
			}
		})
	}
}

func TestRunWithObserverSuppressesObserverOnContextCancellation(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 10)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	source := &assertionTestSource{finalizedErr: context.Canceled}
	resolver := &assertionTestResolver{}
	loop, err := NewAssertionAttestor(
		source,
		resolver,
		store,
		AssertionAttestorConfig{PollInterval: time.Hour, MaxL1BlockRange: 10},
	)
	if err != nil {
		t.Fatalf("create assertion attestor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	observed := 0
	loop.RunWithObserver(ctx, func(error) { observed++ })

	if observed != 0 {
		t.Fatalf("observer called %d times on a canceled context; want 0 (clean shutdown must not count as a failure)", observed)
	}
}

func TestRunWithObserverReportsRealFailures(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 10)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	syncErr := errors.New("finalized unavailable")
	source := &assertionTestSource{finalizedErr: syncErr}
	resolver := &assertionTestResolver{}
	loop, err := NewAssertionAttestor(
		source,
		resolver,
		store,
		AssertionAttestorConfig{PollInterval: time.Hour, MaxL1BlockRange: 10},
	)
	if err != nil {
		t.Fatalf("create assertion attestor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	observations := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.RunWithObserver(ctx, func(err error) { observations <- err })
	}()

	select {
	case observedErr := <-observations:
		if !errors.Is(observedErr, syncErr) {
			t.Fatalf("observer error = %v, want %v", observedErr, syncErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the initial sync's observation")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RunWithObserver to return after cancellation")
	}
}

func TestAssertionAttestorRemovesAssertionWhenRollupNoLongerReportsIt(t *testing.T) {
	store, err := arbitrum.NewAttestedRootStore("arbitrum-one", 11)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	proposal := arbitrum.ProposedAssertion{
		AssertionHash: common.HexToHash("0x31"),
		L2BlockHash:   common.HexToHash("0x32"),
		L1BlockNumber: 10,
	}
	if err := store.RecordProposal(proposal); err != nil {
		t.Fatalf("record proposal: %v", err)
	}
	source := &assertionTestSource{finalizedBlock: 10, status: assertionStatusNone}
	loop, err := NewAssertionAttestor(source, &assertionTestResolver{}, store, AssertionAttestorConfig{})
	if err != nil {
		t.Fatalf("NewAssertionAttestor: %v", err)
	}
	if err := loop.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if proposals := store.Proposals(); len(proposals) != 0 {
		t.Fatalf("proposals after removal = %+v", proposals)
	}
}

type assertionTestSource struct {
	finalizedBlock uint64
	proposals      []arbitrum.ProposedAssertion
	confirmations  []ConfirmedAssertion
	status         uint8

	finalizedErr  error
	chainIDsErr   error
	assertionsErr error
	statusErr     error
	l1ChainID     *big.Int
	l2ChainID     *big.Int
	returnNilL1   bool
	returnNilL2   bool
}

func (s *assertionTestSource) FinalizedBlockNumber(context.Context) (uint64, error) {
	return s.finalizedBlock, s.finalizedErr
}

func (s *assertionTestSource) ChainIDs(
	context.Context,
	uint64,
) (*big.Int, *big.Int, error) {
	if s.chainIDsErr != nil {
		return nil, nil, s.chainIDsErr
	}
	if s.returnNilL1 || s.returnNilL2 {
		var l1ChainID, l2ChainID *big.Int
		if !s.returnNilL1 {
			l1ChainID = s.l1ChainID
			if l1ChainID == nil {
				l1ChainID = big.NewInt(1)
			}
		}
		if !s.returnNilL2 {
			l2ChainID = s.l2ChainID
			if l2ChainID == nil {
				l2ChainID = big.NewInt(42_161)
			}
		}
		return l1ChainID, l2ChainID, nil
	}
	l1ChainID, l2ChainID := s.l1ChainID, s.l2ChainID
	if l1ChainID == nil {
		l1ChainID = big.NewInt(1)
	}
	if l2ChainID == nil {
		l2ChainID = big.NewInt(42_161)
	}
	return l1ChainID, l2ChainID, nil
}

func (s *assertionTestSource) Assertions(
	context.Context,
	uint64,
	uint64,
) ([]arbitrum.ProposedAssertion, []ConfirmedAssertion, error) {
	if s.assertionsErr != nil {
		return nil, nil, s.assertionsErr
	}
	proposals := append([]arbitrum.ProposedAssertion(nil), s.proposals...)
	confirmations := append([]ConfirmedAssertion(nil), s.confirmations...)
	s.proposals = nil
	s.confirmations = nil
	return proposals, confirmations, nil
}

func (s *assertionTestSource) AssertionStatus(
	context.Context,
	arbitrum.ProposedAssertion,
	uint64,
) (uint8, error) {
	return s.status, s.statusErr
}

type assertionTestResolver struct {
	results map[arbitrum.RunMode]arbitrum.BlockCommitment
	errs    map[arbitrum.RunMode]error
}

func (r *assertionTestResolver) ResolveCanonicalBlockHash(
	_ context.Context,
	_ common.Hash,
	mode arbitrum.RunMode,
) (arbitrum.BlockCommitment, error) {
	return r.results[mode], r.errs[mode]
}

func (s *assertionTestSource) Head(_ context.Context, _ arbitrum.L1IngestHead) (L1Block, error) {
	return L1Block{Number: s.finalizedBlock, Hash: common.BigToHash(new(big.Int).SetUint64(s.finalizedBlock))}, s.finalizedErr
}
func (s *assertionTestSource) CanonicalHash(_ context.Context, n uint64) (common.Hash, error) {
	return common.BigToHash(new(big.Int).SetUint64(n)), nil
}
func (s *assertionTestSource) AssertionStatusAtHash(ctx context.Context, p arbitrum.ProposedAssertion, _ common.Hash) (uint8, error) {
	return s.AssertionStatus(ctx, p, s.finalizedBlock)
}
