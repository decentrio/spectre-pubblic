package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	misbehaviourContract "relayer/bindings/Misbehaviour"
	spectreContract "relayer/bindings/SpectreClient"
	updateClientContract "relayer/bindings/UpdateClient"
	"testing"
	"time"

	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	commettypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
)

func TestEncodeMisbehaviourContractMsg(t *testing.T) {
	proof := misbehaviourContract.ISpectreClientMsgsBatchProof{}
	for i := range proof.Proof {
		proof.Proof[i] = big.NewInt(0)
	}
	for i := range proof.Commitments {
		proof.Commitments[i] = big.NewInt(0)
		proof.CommitmentPok[i] = big.NewInt(0)
	}
	header := misbehaviourContract.ISpectreTendermintMsgsHeader{}
	header.SignedHeader.Header.Time = big.NewInt(0)
	msg := misbehaviourContract.ISpectreClientMsgsMsgSubmitMisbehaviour{
		Misbehaviour: misbehaviourContract.ISpectreClientMsgsMisbehaviour{
			Header1: header,
			Header2: header,
		},
		TrustedConsensusState1: misbehaviourContract.ISpectreTendermintMsgsConsensusState{Timestamp: big.NewInt(1)},
		TrustedConsensusState2: misbehaviourContract.ISpectreTendermintMsgsConsensusState{Timestamp: big.NewInt(2)},
		Time:                   big.NewInt(3),
		Proof1:                 proof,
		Proof2:                 proof,
	}
	encoded, err := EncodeMisbehaviourContractMsg(msg)
	if err != nil {
		t.Fatalf("encode generated misbehaviour contract message: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("expected non-empty ABI payload")
	}
}

func TestBytesToBytes32(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  [32]byte
	}{
		{name: "nil returns zero array", input: nil, want: [32]byte{}},
		{name: "empty returns zero array", input: []byte{}, want: [32]byte{}},
		{
			name:  "short input pads with zeros",
			input: []byte{0xAA, 0xBB},
			want: func() [32]byte {
				var b [32]byte
				b[0], b[1] = 0xAA, 0xBB
				return b
			}(),
		},
		{
			name: "exact 32 bytes copies fully",
			input: func() []byte {
				b := make([]byte, 32)
				for i := range b {
					b[i] = byte(i)
				}
				return b
			}(),
			want: func() [32]byte {
				var b [32]byte
				for i := range b {
					b[i] = byte(i)
				}
				return b
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bytesToBytes32(tc.input)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// EncodeUpdateConsensusStateMsg is the third of three sibling encoders in this
// file, and the only one that had no test. It carries the pinned-set rotation --
// the operation that keeps the light client alive -- and encoding is a
// byte-for-byte contract with Solidity, so a drifted tuple fails on chain, not
// here.
//
// It is also the hardest of the three to check, which is presumably why it was
// skipped. updateConsensusState takes opaque `bytes`, so unlike verifyHeader
// there is no generated tuple to round-trip against. The hand-written tuple is
// still checkable in two halves, because both of its components DO appear as
// typed parameters elsewhere in the generated ABI:
//
//	update           -> UpdateClient.verifyHeader input 0
//	newValidatorSet  -> SpectreClient constructor, initialPinnedValidatorSet
//
// Checking those two and then round-tripping a fully populated value covers what
// can be covered without a chain.
func TestEncodeUpdateConsensusStateMsg(t *testing.T) {
	t.Run("both halves match the generated ABI", func(t *testing.T) {
		components := updateConsensusStateMsgType.TupleElems
		if len(components) != 2 {
			t.Fatalf("tuple has %d components, want 2 (update, newValidatorSet)", len(components))
		}

		updateABI, err := updateClientContract.ContractUpdateClientMetaData.GetAbi()
		if err != nil {
			t.Fatalf("parse generated UpdateClient ABI: %v", err)
		}
		if got, want := components[0].String(), updateABI.Methods["verifyHeader"].Inputs[0].Type.String(); got != want {
			t.Fatalf("the update half diverged from the generated ABI\n got=%s\nwant=%s", got, want)
		}

		spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
		if err != nil {
			t.Fatalf("parse generated SpectreClient ABI: %v", err)
		}
		var valSetParam *abi.Type
		for i := range spectreABI.Constructor.Inputs {
			if spectreABI.Constructor.Inputs[i].Name == "initialPinnedValidatorSet" {
				valSetParam = &spectreABI.Constructor.Inputs[i].Type
			}
		}
		if valSetParam == nil {
			t.Fatal("SpectreClient constructor no longer takes initialPinnedValidatorSet; find the new source of truth for the validator-set tuple")
		}
		if got, want := components[1].String(), valSetParam.String(); got != want {
			t.Fatalf("the newValidatorSet half diverged from the generated ABI\n got=%s\nwant=%s", got, want)
		}
	})

	t.Run("every field survives a round trip", func(t *testing.T) {
		// Populated deliberately: no zero values in nested structs, and both
		// validator entries distinct. A fixture that left CommitSigs empty is
		// exactly how the validatorAddress bug stayed green in the sibling test
		// while the relayer reverted every packet on chain.
		msg := fullyPopulatedApplicationStateMsg()
		valSet := spectreContract.ISpectreTendermintMsgsValidatorSet{
			Validators: []spectreContract.ISpectreTendermintMsgsValidatorInfo{
				{ValAddress: []byte{0xa1, 0xa2}, PubKey: [32]byte{0x11}, VotingPower: 300, ProposerPriority: -7},
				{ValAddress: []byte{0xb1, 0xb2}, PubKey: [32]byte{0x22}, VotingPower: 200, ProposerPriority: 5},
			},
			HasProposer:      true,
			Proposer:         spectreContract.ISpectreTendermintMsgsValidatorInfo{ValAddress: []byte{0xc1}, PubKey: [32]byte{0x33}, VotingPower: 300, ProposerPriority: -7},
			TotalVotingPower: 500,
		}

		encoded, err := EncodeUpdateConsensusStateMsg(msg, valSet)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		args := abi.Arguments{{Type: updateConsensusStateMsgType}}
		unpacked, err := args.Unpack(encoded)
		if err != nil {
			t.Fatalf("unpack: %v", err)
		}
		if len(unpacked) != 1 {
			t.Fatalf("unpacked %d values, want 1", len(unpacked))
		}
		jsonBytes, err := json.Marshal(unpacked[0])
		if err != nil {
			t.Fatalf("marshal unpacked tuple: %v", err)
		}
		var decoded MsgUpdateConsensusState
		if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
			t.Fatalf("unmarshal decoded msg: %v", err)
		}

		// The validator set is the half nothing else in this package checks.
		if got := decoded.NewValidatorSet.TotalVotingPower; got != valSet.TotalVotingPower {
			t.Errorf("totalVotingPower = %d, want %d", got, valSet.TotalVotingPower)
		}
		if !decoded.NewValidatorSet.HasProposer {
			t.Error("hasProposer was lost; the pinned set would be installed with no proposer")
		}
		if got := len(decoded.NewValidatorSet.Validators); got != len(valSet.Validators) {
			t.Fatalf("decoded %d validators, want %d", got, len(valSet.Validators))
		}
		for i, want := range valSet.Validators {
			got := decoded.NewValidatorSet.Validators[i]
			if !bytes.Equal(got.ValAddress, want.ValAddress) {
				t.Errorf("validator %d address = %x, want %x", i, got.ValAddress, want.ValAddress)
			}
			if got.PubKey != want.PubKey {
				t.Errorf("validator %d pubkey = %x, want %x", i, got.PubKey, want.PubKey)
			}
			if got.VotingPower != want.VotingPower {
				t.Errorf("validator %d voting power = %d, want %d", i, got.VotingPower, want.VotingPower)
			}
			// Signed, and negative in this fixture: an unsigned tuple component
			// would decode this as an enormous positive number.
			if got.ProposerPriority != want.ProposerPriority {
				t.Errorf("validator %d proposer priority = %d, want %d", i, got.ProposerPriority, want.ProposerPriority)
			}
		}
		if got := decoded.NewValidatorSet.Proposer.PubKey; got != valSet.Proposer.PubKey {
			t.Errorf("proposer pubkey = %x, want %x", got, valSet.Proposer.PubKey)
		}

		// And the update half must not be disturbed by sitting inside the larger
		// tuple: a component-order mistake shows up here.
		if got, want := decoded.Update.ProposedHeader.SignedHeader.Header.ChainId,
			msg.ProposedHeader.SignedHeader.Header.ChainId; got != want {
			t.Errorf("update chain id = %q, want %q", got, want)
		}
		if len(decoded.Update.ProposedHeader.SignedHeader.Commit.CommitSigs) == 0 {
			t.Error("commit sigs were lost inside the outer tuple")
		} else if got, want := decoded.Update.ProposedHeader.SignedHeader.Commit.CommitSigs[0].ValidatorAddress,
			msg.ProposedHeader.SignedHeader.Commit.CommitSigs[0].ValidatorAddress; got != want {
			t.Errorf("commit sig validator address = %x, want %x", got, want)
		}
	})
}

func TestSpectreClientGenesis_PinsNextValidatorSet(t *testing.T) {
	currentValSet := genesisTestValSet("current", 10, 20, 30)
	nextValSet := genesisTestValSet("next", 10, 20, 30)
	if bytes.Equal(currentValSet.Hash(), nextValSet.Hash()) {
		t.Fatal("test setup requires different current and next validator-set hashes")
	}

	trustedLightBlock := &LightBlock{
		SignedHeader: commettypes.SignedHeader{
			Header: &commettypes.Header{
				ChainID:            "test-chain-1",
				Height:             123,
				Time:               time.Unix(1_700_000_000, 0),
				ValidatorsHash:     currentValSet.Hash(),
				NextValidatorsHash: nextValSet.Hash(),
				AppHash:            []byte("app-hash"),
			},
		},
		ValSet:      currentValSet,
		NextValSet:  nextValSet,
		BlockHeight: 123,
	}

	genesis, err := spectreClientGenesisFromLightBlock(trustedLightBlock, 1_814_400, 0, "2/3", 0)
	if err != nil {
		t.Fatalf("unexpected genesis error: %v", err)
	}

	currentPinned, err := ValidatorSetToContract(currentValSet, "current")
	if err != nil {
		t.Fatalf("current ValidatorSetToContract: %v", err)
	}
	nextPinned, err := ValidatorSetToContract(nextValSet, "next")
	if err != nil {
		t.Fatalf("next ValidatorSetToContract: %v", err)
	}

	if got, want := genesis.TrustedConsensusState.NextValidatorsHash, bytesToBytes32(nextValSet.Hash()); got != want {
		t.Fatalf("next validators hash: got %X, want %X", got, want)
	}
	if got, want := genesis.TrustedConsensusState.Timestamp, big.NewInt(trustedLightBlock.SignedHeader.Header.Time.UnixNano()); got.Cmp(want) != 0 {
		t.Fatalf("consensus timestamp: got %s, want %s", got, want)
	}
	if got, want := genesis.TrustedConsensusState.Root, bytesToBytes32(trustedLightBlock.SignedHeader.Header.AppHash); got != want {
		t.Fatalf("consensus root: got %X, want %X", got, want)
	}
	if !reflect.DeepEqual(genesis.InitialPinnedValidatorSet, nextPinned) {
		t.Fatal("initial pinned validator set should come from the trusted light block's next validator set")
	}
	if reflect.DeepEqual(genesis.InitialPinnedValidatorSet, currentPinned) {
		t.Fatal("initial pinned validator set must not come from the current validator set")
	}
}

func genesisTestValSet(name string, powers ...int64) commettypes.ValidatorSet {
	validators := make([]*commettypes.Validator, len(powers))
	for i, power := range powers {
		secret := []byte(fmt.Sprintf("%s-%d", name, i))
		validators[i] = commettypes.NewValidator(cmted25519.GenPrivKeyFromSecret(secret).PubKey(), power)
	}
	return *commettypes.NewValidatorSet(validators)
}

// fakePager is a test double for the CometBFT validators RPC. It paginates a
// backing slice and records the perPage values it was asked for, so tests can
// assert we never fall back to the nil/default-30 behavior (issue #105).
type fakePager struct {
	all         []*commettypes.Validator
	perPageSeen []int
	failOnPage  int // 0 = never fail
}

func (f *fakePager) Validators(_ context.Context, _ *int64, page, perPage *int) (*coretypes.ResultValidators, error) {
	if perPage == nil {
		return nil, fmt.Errorf("perPage must not be nil (would trigger default-30 truncation)")
	}
	f.perPageSeen = append(f.perPageSeen, *perPage)
	if f.failOnPage != 0 && *page == f.failOnPage {
		return nil, fmt.Errorf("simulated RPC failure on page %d", *page)
	}
	pp := *perPage
	start := min((*page-1)*pp, len(f.all))
	end := min(start+pp, len(f.all))
	return &coretypes.ResultValidators{
		Validators: f.all[start:end],
		Count:      end - start,
		Total:      len(f.all),
	}, nil
}

func makeVals(n int) []*commettypes.Validator {
	vals := make([]*commettypes.Validator, n)
	for i := range vals {
		vals[i] = &commettypes.Validator{
			Address:     fmt.Appendf(nil, "addr-%04d", i),
			VotingPower: int64(i + 1),
		}
	}
	return vals
}

func TestFetchAllValidators_PagesFullSet(t *testing.T) {
	cases := []struct {
		name      string
		total     int
		wantPages int
	}{
		{"empty", 0, 1},
		{"under one page", 17, 1},
		{"exactly 30 (old default boundary)", 30, 1},
		{"just over old default", 31, 1},
		{"exactly one max page", 100, 1},
		{"just over one page", 101, 2},
		{"large set (180 validators)", 180, 2},
		{"multi page (250)", 250, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakePager{all: makeVals(tc.total)}
			got, err := fetchAllValidators(context.Background(), f, 42)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.total {
				t.Fatalf("got %d validators, want full set of %d (truncation regression)", len(got), tc.total)
			}
			if len(f.perPageSeen) != tc.wantPages {
				t.Fatalf("made %d RPC pages, want %d", len(f.perPageSeen), tc.wantPages)
			}
			for i, pp := range f.perPageSeen {
				if pp != cometBFTMaxPerPage {
					t.Fatalf("page %d requested perPage=%d, want explicit %d (nil would default to 30)", i+1, pp, cometBFTMaxPerPage)
				}
			}
		})
	}
}

func TestFetchAllValidators_PropagatesError(t *testing.T) {
	f := &fakePager{all: makeVals(150), failOnPage: 2}
	_, err := fetchAllValidators(context.Background(), f, 42)
	if err == nil {
		t.Fatal("expected error when a page fetch fails, got nil")
	}
}
