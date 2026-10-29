package services

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	updateclientContract "relayer/bindings/UpdateClient"
	relayerclient "relayer/client"
	"relayer/prover"
	"relayer/prover/witness"

	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	cmttypes "github.com/cometbft/cometbft/types"
	tenderminttypes "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"
)

func TestUpdateCommitToMisbehaviour_PreservesValidatorAddress(t *testing.T) {
	wantAddress := [20]byte{0x01, 0x23, 0x45, 0x67, 0x89}
	converted := updateCommitToMisbehaviour(updateclientContract.ISpectreTendermintMsgsBlockCommit{
		CommitSigs: []updateclientContract.ISpectreTendermintMsgsCommitSig{{
			Flag:             2,
			ValidatorAddress: wantAddress,
		}},
	})

	if len(converted.CommitSigs) != 1 {
		t.Fatalf("commit signatures = %d, want 1", len(converted.CommitSigs))
	}
	if got := converted.CommitSigs[0]; got.Flag != 2 || got.ValidatorAddress != wantAddress {
		t.Fatalf("converted signature = %+v, want flag=2 validatorAddress=%x", got, wantAddress)
	}
}

func TestMisbehaviourEvidenceFromCandidates_DetectsPinnedSetEquivocation(t *testing.T) {
	pubkey0 := [32]byte{0x01}
	pubkey1 := [32]byte{0x02}
	pubkey2 := [32]byte{0x03}
	pinned := pinnedCosmosValidatorSet{
		indices:      []uint32{0, 1, 2},
		pubkeys:      [][32]byte{pubkey0, pubkey1, pubkey2},
		votingPowers: []uint64{40, 35, 25},
		totalPower:   100,
	}

	header1 := misbehaviourHeaderCandidate{
		height:    42,
		blockHash: [32]byte{0xAA},
		candidates: []witness.ValidatorSignature{
			{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
			{Index: 1, PublicKey: pubkey1[:], Power: 35, Active: true},
		},
	}
	header2 := misbehaviourHeaderCandidate{
		height:    42,
		blockHash: [32]byte{0xBB},
		candidates: []witness.ValidatorSignature{
			{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
			{Index: 2, PublicKey: pubkey2[:], Power: 25, Active: true},
			{Index: 1, PublicKey: pubkey1[:], Power: 35, Active: true},
		},
	}

	evidence, err := misbehaviourEvidenceFromCandidates(header1, header2, pinned, pinned, 4)
	if err != nil {
		t.Fatalf("build evidence: %v", err)
	}
	if evidence == nil {
		t.Fatal("expected same-height distinct block hashes with pinned quorum to produce evidence")
	}
	if len(evidence.sigs1) != 2 || len(evidence.sigs2) != 2 {
		t.Fatalf("selected signatures = %d/%d, want 2/2", len(evidence.sigs1), len(evidence.sigs2))
	}
	if _, err := misbehaviourEvidenceFromCandidates(header1, header1, pinned, pinned, 4); err == nil {
		t.Fatal("same block hash at the same height must be rejected")
	}
}

func TestMisbehaviourEvidenceFromCandidates_IgnoresConflictsWithoutPinnedQuorum(t *testing.T) {
	pubkey0 := [32]byte{0x01}
	pubkey1 := [32]byte{0x02}
	pubkey2 := [32]byte{0x03}
	pinned := pinnedCosmosValidatorSet{
		indices:      []uint32{0, 1, 2},
		pubkeys:      [][32]byte{pubkey0, pubkey1, pubkey2},
		votingPowers: []uint64{40, 35, 25},
		totalPower:   100,
	}

	header1 := misbehaviourHeaderCandidate{
		height:    99,
		blockHash: [32]byte{0xAA},
		candidates: []witness.ValidatorSignature{
			{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
			{Index: 1, PublicKey: pubkey1[:], Power: 35, Active: true},
		},
	}
	header2 := misbehaviourHeaderCandidate{
		height:    99,
		blockHash: [32]byte{0xBB},
		candidates: []witness.ValidatorSignature{
			{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
		},
	}

	if _, err := misbehaviourEvidenceFromCandidates(header1, header2, pinned, pinned, 4); err == nil {
		t.Fatal("conflicting headers without pinned quorum must be rejected")
	}
}

func TestMisbehaviourBatchProof_BuildsPinnedMetadata(t *testing.T) {
	pubkey0 := [32]byte{0x01}
	pubkey1 := [32]byte{0x02}
	pinned := pinnedCosmosValidatorSet{
		indices:      []uint32{0, 1},
		pubkeys:      [][32]byte{pubkey0, pubkey1},
		votingPowers: []uint64{50, 50},
		totalPower:   100,
	}
	headerProof := prover.ProofResult{
		Bucket: 2,
		PaddedSigs: []witness.ValidatorSignature{
			{Index: 7, PublicKey: pubkey1[:], Active: true},
			{Index: 0, PublicKey: make([]byte, 32), Active: false},
		},
		Proof:         [8]*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3), big.NewInt(4), big.NewInt(5), big.NewInt(6), big.NewInt(7), big.NewInt(8)},
		Commitments:   [2]*big.Int{big.NewInt(9), big.NewInt(10)},
		CommitmentPok: [2]*big.Int{big.NewInt(11), big.NewInt(12)},
	}

	got, err := misbehaviourBatchProof(headerProof, pinned)
	if err != nil {
		t.Fatalf("misbehaviourBatchProof: %v", err)
	}
	if got.Bucket != 2 {
		t.Fatalf("bucket = %d, want 2", got.Bucket)
	}
	if len(got.SignerIndices) != 2 || got.SignerIndices[0] != 7 {
		t.Fatalf("signer indices = %+v", got.SignerIndices)
	}
	if len(got.PinnedValidatorIndices) != 2 || got.PinnedValidatorIndices[0] != 1 {
		t.Fatalf("pinned indices = %+v", got.PinnedValidatorIndices)
	}
	if len(got.Active) != 2 || !got.Active[0] || got.Active[1] {
		t.Fatalf("active flags = %+v", got.Active)
	}
}

// --- struct conversion: the field-dropping class ---

// updateHeaderToMisbehaviourHeader and its three helpers copy a header from one
// generated binding's types into another's, field by field. Twenty-odd
// assignments written out by hand, and nothing about a missing one fails to
// compile: the destination field simply stays zero, the message encodes, and the
// contract rejects it on chain.
//
// That is not hypothetical here. The validatorAddress bug was exactly this --
// a field that never made it across, past a green test suite, discovered only
// when the relayer reverted every packet.
//
// So this does not check a list of fields, which would repeat the same omission
// risk one level up. It fills EVERY field of the source with a distinct non-zero
// value by reflection, converts, and asserts nothing on the far side is still
// zero. Add a field to the Solidity struct and forget the assignment, and this
// fails without anyone having to remember to extend the test.
func TestUpdateBlockHeaderToMisbehaviour_CarriesEveryField(t *testing.T) {
	var src updateclientContract.ISpectreTendermintMsgsBlockHeader
	fillNonZero(t, reflect.ValueOf(&src).Elem(), "BlockHeader")
	// Guard the fixture before trusting the comparison: equal values make a swap
	// undetectable no matter how the comparison is written.
	assertFixtureValuesAreDistinct(t, reflect.ValueOf(src), "BlockHeader")

	got := updateBlockHeaderToMisbehaviour(src)
	assertFieldsCarriedAcross(t, reflect.ValueOf(src), reflect.ValueOf(got), "BlockHeader", nil)
}

// Same argument one level up: the outer header carries the signed header, the
// commit and the trusted height.
func TestUpdateHeaderToMisbehaviourHeader_CarriesEveryField(t *testing.T) {
	var src updateclientContract.ISpectreTendermintMsgsHeader
	fillNonZero(t, reflect.ValueOf(&src).Elem(), "Header")
	// Guard the fixture before trusting the comparison: equal values make a swap
	// undetectable no matter how the comparison is written.
	assertFixtureValuesAreDistinct(t, reflect.ValueOf(src), "Header")

	got := updateHeaderToMisbehaviourHeader(src)
	assertFieldsCarriedAcross(t, reflect.ValueOf(src), reflect.ValueOf(got), "Header", nil)
}

// fillNonZero writes a distinct non-zero value into every field of v, recursing
// into nested structs and giving slices one filled element.
//
// Distinct values matter as much as non-zero ones: if two fields were filled
// identically, a conversion that swapped them would still pass.
//
// The first version derived numbers from len(path)+1, which is not distinct --
// it is distinct per path LENGTH. .LastCommitHash and .ValidatorsHash are both
// fifteen characters, so both [32]byte fields came out identical and swapping
// the two production assignments passed. Array elements were worse: every
// element of one array shared the field's path, so all 32 bytes were the same
// number. Review caught it, twice in the same family.
//
// So the value is now a counter, incremented at every leaf. Two leaves cannot
// collide by construction, whatever their names happen to be, and
// assertFixtureValuesAreDistinct proves it rather than trusting this comment.
func fillNonZero(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	next := uint64(0)
	fillDistinct(t, v, path, &next)
}

func fillDistinct(t *testing.T, v reflect.Value, path string, next *uint64) {
	t.Helper()
	take := func() uint64 {
		*next++
		return *next
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(big.Int{}) {
			// #nosec G115 -- counter, bounded by the field count of a test fixture
			v.Set(reflect.ValueOf(*big.NewInt(int64(take()))))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if !v.Field(i).CanSet() {
				continue
			}
			fillDistinct(t, v.Field(i), path+"."+v.Type().Field(i).Name, next)
		}
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillDistinct(t, v.Elem(), path, next)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillDistinct(t, elem, path+"[0]", next)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Array:
		// A [32]byte is ONE value, not 32. Handing each element its own counter
		// would need 32 distinct bytes per hash and wrap at 256, putting the
		// collision back. Writing the counter's big-endian bytes across the array
		// keeps whole arrays distinct: the encoding is injective in the counter.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			var seed [8]byte
			binary.BigEndian.PutUint64(seed[:], take())
			for i := 0; i < v.Len(); i++ {
				v.Index(i).SetUint(uint64(seed[i%len(seed)]))
			}
			return
		}
		for i := 0; i < v.Len(); i++ {
			fillDistinct(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i), next)
		}
	case reflect.String:
		v.SetString(path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// #nosec G115 -- counter, bounded by the field count of a test fixture
		v.SetInt(int64(take()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(take())
	default:
		t.Fatalf("fillNonZero does not handle %s at %s; extend it rather than skipping the field", v.Kind(), path)
	}
}

// assertFieldsCarriedAcross walks a source and a destination struct in parallel
// and reports every field whose value did not survive the conversion.
//
// It compares VALUES, not merely non-zeroness. The first version of these tests
// only checked that nothing on the destination was zero, and that is a weaker
// property than it looks: swapping ValidatorsHash and NextValidatorsHash leaves
// both fields non-zero, passed cleanly, and corrupts the binding between a header
// and the validator set it commits to. Raised in review on this PR, reproduced,
// and fixed here.
//
// The two structs come from different generated packages, so fields are matched
// by NAME rather than by position. Position would silently pair the wrong fields
// the moment the two definitions diverge -- which is the drift this exists to
// catch.
// renames maps a source field name to the destination name it is copied into,
// for conversions that do not keep the name. Only conversions that rename need
// it; the rest pass nil. Keys are leaf field names, not paths, which is enough
// while no two levels of one conversion rename the same name differently.
func assertFieldsCarriedAcross(t *testing.T, src, dst reflect.Value, path string, renames map[string]string) {
	t.Helper()
	switch src.Kind() {
	case reflect.Struct:
		if src.Type() == reflect.TypeOf(big.Int{}) {
			from, to := src.Interface().(big.Int), dst.Interface().(big.Int)
			if from.Cmp(&to) != 0 {
				t.Errorf("%s = %s, want %s", path, to.String(), from.String())
			}
			return
		}
		for i := 0; i < src.NumField(); i++ {
			name := src.Type().Field(i).Name
			dstName := name
			if renamed, ok := renames[name]; ok {
				dstName = renamed
			}
			dstField := dst.FieldByName(dstName)
			if !dstField.IsValid() {
				t.Errorf("%s.%s has no counterpart %q in %s, so the conversion cannot carry it",
					path, name, dstName, dst.Type())
				continue
			}
			assertFieldsCarriedAcross(t, src.Field(i), dstField, path+"."+name, renames)
		}
	case reflect.Slice:
		if dst.Len() != src.Len() {
			t.Errorf("%s has %d elements, want %d", path, dst.Len(), src.Len())
			return
		}
		for i := 0; i < src.Len(); i++ {
			assertFieldsCarriedAcross(t, src.Index(i), dst.Index(i), fmt.Sprintf("%s[%d]", path, i), renames)
		}
	case reflect.Ptr:
		if src.IsNil() != dst.IsNil() {
			t.Errorf("%s: source nil=%v, destination nil=%v", path, src.IsNil(), dst.IsNil())
			return
		}
		if !src.IsNil() {
			assertFieldsCarriedAcross(t, src.Elem(), dst.Elem(), path, renames)
		}
	default:
		if !reflect.DeepEqual(src.Interface(), dst.Interface()) {
			t.Errorf("%s = %v, want %v", path, dst.Interface(), src.Interface())
		}
	}
}

// assertFixtureValuesAreDistinct proves the property fillNonZero claims, rather
// than leaving it to a comment that was wrong twice.
//
// Two same-typed fields holding the same value make every swap between them
// invisible, and no amount of care in the comparison helper can recover that --
// the two sides genuinely are equal. This walks the filled fixture and fails on
// the first collision, naming both paths.
func assertFixtureValuesAreDistinct(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	seen := map[string]string{}
	var walk func(reflect.Value, string)
	walk = func(v reflect.Value, path string) {
		record := func() {
			key := fmt.Sprintf("%s|%v", v.Type(), v.Interface())
			if first, dup := seen[key]; dup {
				t.Errorf("%s and %s hold the same %s value; a conversion swapping them cannot be detected",
					first, path, v.Type())
				return
			}
			seen[key] = path
		}
		switch v.Kind() {
		case reflect.Struct:
			if v.Type() == reflect.TypeOf(big.Int{}) {
				record()
				return
			}
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		case reflect.Ptr:
			if !v.IsNil() {
				walk(v.Elem(), path)
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i))
			}
		case reflect.Array:
			// Compared whole, matching how fillNonZero fills it and how a swap
			// would move it.
			record()
		case reflect.Bool:
			// Every bool is true by construction; distinctness is meaningless and
			// a swap between two bools is not detectable by value at all.
		default:
			record()
		}
	}
	walk(v, path)
}

// assertNoZeroFields reports every field left at its zero value. It reports all
// of them rather than stopping at the first, so one run names the whole gap.
func assertNoZeroFields(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(big.Int{}) {
			n := v.Interface().(big.Int)
			if n.Sign() == 0 {
				t.Errorf("%s was not carried across the conversion", path)
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			assertNoZeroFields(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Ptr:
		if v.IsNil() {
			t.Errorf("%s is nil after conversion", path)
			return
		}
		assertNoZeroFields(t, v.Elem(), path)
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty after conversion", path)
			return
		}
		for i := 0; i < v.Len(); i++ {
			assertNoZeroFields(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Array:
		if v.IsZero() {
			t.Errorf("%s is all zero after conversion", path)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is zero after conversion", path)
		}
	}
}

// --- the trusted-set binding ---

// misbehaviourValSet builds a deterministic validator set. Distinct names give
// distinct keys, which is what lets a test say "a different set" and mean it.
func misbehaviourValSet(t *testing.T, name string, powers ...int64) *cmttypes.ValidatorSet {
	t.Helper()
	validators := make([]*cmttypes.Validator, len(powers))
	for i, power := range powers {
		secret := []byte(fmt.Sprintf("%s-%d", name, i))
		validators[i] = cmttypes.NewValidator(cmted25519.GenPrivKeyFromSecret(secret).PubKey(), power)
	}
	return cmttypes.NewValidatorSet(validators)
}

// trustedHeaderFor wraps a validator set as the TrustedValidators of an IBC
// Tendermint header, and returns a trusted light block whose nextValidatorsHash
// commits to nextHash.
func trustedHeaderFor(t *testing.T, trusted *cmttypes.ValidatorSet, nextHash []byte) (*tenderminttypes.Header, *relayerclient.LightBlock) {
	t.Helper()
	proto, err := trusted.ToProto()
	if err != nil {
		t.Fatalf("validator set to proto: %v", err)
	}
	header := &tenderminttypes.Header{TrustedValidators: proto}
	block := &relayerclient.LightBlock{
		SignedHeader: cmttypes.SignedHeader{
			Header: &cmttypes.Header{NextValidatorsHash: nextHash},
		},
	}
	return header, block
}

// pinnedSetFromTrustedValidators turns the validator set carried IN the evidence
// into the pinned set that quorum is then measured against. That set arrives from
// whoever submitted the evidence, so the only thing standing between a forged set
// and a successful misbehaviour claim is the hash check here: the set must hash
// to the nextValidatorsHash of a trusted consensus state already on chain.
//
// Without it a submitter could attach a validator set of their own making, meet
// "quorum" trivially, and have honest validators recorded as equivocating.
func TestPinnedSetFromTrustedValidators(t *testing.T) {
	t.Run("accepts a set matching the trusted nextValidatorsHash", func(t *testing.T) {
		valSet := misbehaviourValSet(t, "trusted", 40, 35, 25)
		header, block := trustedHeaderFor(t, valSet, valSet.Hash())

		pinned, err := pinnedSetFromTrustedValidators(header, block)
		if err != nil {
			t.Fatalf("a set that hashes to the trusted next-validators hash was rejected: %v", err)
		}
		if got := len(pinned.pubkeys); got != 3 {
			t.Fatalf("pinned %d validators, want 3", got)
		}
		if pinned.totalPower != 100 {
			t.Fatalf("total power = %d, want 100", pinned.totalPower)
		}
		// Indices are positional, and the quorum selection later indexes into
		// pubkeys by them: a permutation here would prove the wrong signers.
		for i, idx := range pinned.indices {
			if idx != uint32(i) {
				t.Fatalf("indices[%d] = %d, want %d", i, idx, i)
			}
		}
		for i, want := range valSet.Validators {
			if got := pinned.pubkeys[i]; got != [32]byte(want.PubKey.Bytes()) {
				t.Errorf("pubkey %d = %x, want %x", i, got, want.PubKey.Bytes())
			}
			if got := pinned.votingPowers[i]; got != uint64(want.VotingPower) {
				t.Errorf("voting power %d = %d, want %d", i, got, want.VotingPower)
			}
		}
	})

	t.Run("rejects a set that does not match the trusted hash", func(t *testing.T) {
		// The forged set: well-formed, but not the one the chain committed to.
		forged := misbehaviourValSet(t, "forged", 100)
		honest := misbehaviourValSet(t, "trusted", 40, 35, 25)
		header, block := trustedHeaderFor(t, forged, honest.Hash())

		if _, err := pinnedSetFromTrustedValidators(header, block); err == nil {
			t.Fatal("accepted a validator set the trusted consensus state does not commit to")
		}
	})

	t.Run("rejects missing inputs", func(t *testing.T) {
		valSet := misbehaviourValSet(t, "trusted", 10)
		header, block := trustedHeaderFor(t, valSet, valSet.Hash())

		for name, call := range map[string]func() error{
			"nil header": func() error {
				_, err := pinnedSetFromTrustedValidators(nil, block)
				return err
			},
			"header without trusted validators": func() error {
				_, err := pinnedSetFromTrustedValidators(&tenderminttypes.Header{}, block)
				return err
			},
			"nil trusted light block": func() error {
				_, err := pinnedSetFromTrustedValidators(header, nil)
				return err
			},
			"trusted light block without a header": func() error {
				_, err := pinnedSetFromTrustedValidators(header, &relayerclient.LightBlock{})
				return err
			},
		} {
			if err := call(); err == nil {
				t.Errorf("%s: accepted, want an error", name)
			}
		}
	})
}

// --- the guards on the way in ---

// misbehaviourCandidateFromLightBlock is where a malformed light block is meant
// to stop. Each guard exists because the field it checks is dereferenced further
// down: without them the failure is a panic inside the relayer rather than a
// rejected submission, and a panic on this path takes the process down while
// evidence is being prepared.
//
// Each case asserts WHICH guard fired, not merely that something failed. The
// first version of this test checked only that an error came back, and every
// input was malformed in more than one way -- so removing a guard just moved the
// failure to the next one and the test stayed green.
func TestMisbehaviourCandidateFromLightBlock_RejectsMalformedInput(t *testing.T) {
	// valid is well-formed as far as this function's guards are concerned; each
	// case below breaks exactly one thing.
	valid := func() *relayerclient.LightBlock {
		return &relayerclient.LightBlock{
			BlockHeight: 42,
			SignedHeader: cmttypes.SignedHeader{
				Commit: &cmttypes.Commit{
					BlockID: cmttypes.BlockID{Hash: bytes.Repeat([]byte{0x11}, 32)},
				},
			},
		}
	}

	tests := []struct {
		name    string
		block   func() *relayerclient.LightBlock
		wantErr string
	}{
		{"nil light block", func() *relayerclient.LightBlock { return nil }, "light block is nil"},
		{"negative height", func() *relayerclient.LightBlock {
			b := valid()
			b.BlockHeight = -1
			return b
		}, "negative light block height"},
		{"nil commit", func() *relayerclient.LightBlock {
			b := valid()
			b.SignedHeader.Commit = nil
			return b
		}, "commit is nil"},
		{"empty block hash", func() *relayerclient.LightBlock {
			b := valid()
			b.SignedHeader.Commit.BlockID.Hash = nil
			return b
		}, "commit block hash is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := misbehaviourCandidateFromLightBlock(tt.block(), "chain-a", 4)
			if err == nil {
				t.Fatal("accepted a light block it cannot build a candidate from")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("a different guard fired: got %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// misbehaviourConsensusState is the trusted state the contract checks the
// evidence against. It reads three fields off the light block, and the guard
// exists because two of them sit behind a pointer.
func TestMisbehaviourConsensusState(t *testing.T) {
	t.Run("carries timestamp, root and next validators hash", func(t *testing.T) {
		when := time.Unix(1_700_000_000, 123)
		appHash := bytes.Repeat([]byte{0xab}, 32)
		nextHash := bytes.Repeat([]byte{0xcd}, 32)
		block := &relayerclient.LightBlock{
			SignedHeader: cmttypes.SignedHeader{
				Header: &cmttypes.Header{Time: when, AppHash: appHash, NextValidatorsHash: nextHash},
			},
		}

		got, err := misbehaviourConsensusState(block)
		if err != nil {
			t.Fatalf("build consensus state: %v", err)
		}
		// Nanoseconds, not seconds: the contract compares this against a
		// nanosecond timestamp, and a seconds value would read as 1970.
		if got.Timestamp.Int64() != when.UnixNano() {
			t.Errorf("timestamp = %d, want %d (nanoseconds)", got.Timestamp.Int64(), when.UnixNano())
		}
		if got.Root != [32]byte(appHash) {
			t.Errorf("root = %x, want the app hash %x", got.Root, appHash)
		}
		if got.NextValidatorsHash != [32]byte(nextHash) {
			t.Errorf("nextValidatorsHash = %x, want %x", got.NextValidatorsHash, nextHash)
		}
	})

	t.Run("rejects a light block with no header", func(t *testing.T) {
		for name, block := range map[string]*relayerclient.LightBlock{
			"nil light block": nil,
			"no header":       {},
		} {
			if _, err := misbehaviourConsensusState(block); err == nil {
				t.Errorf("%s: accepted, want an error", name)
			}
		}
	})
}

// lightBlockFromTendermintHeader converts evidence that arrived over the wire.
// Both halves can fail on malformed input, and the third check exists because
// SignedHeaderFromProto can succeed while leaving Header nil -- which the caller
// then dereferences.
//
// The last case is the reason this test asserts on the message: it is reachable
// only with a signed header that carries a commit but no header, and any input
// malformed in a coarser way fails earlier, leaving that branch unexercised.
func TestLightBlockFromTendermintHeader_RejectsMalformedInput(t *testing.T) {
	// An EMPTY signed header is the one that reaches the third guard:
	// SignedHeaderFromProto returns no error for it and leaves Header nil. Adding
	// a commit makes it fail validation earlier instead, which is how an earlier
	// version of this test left that branch unexercised.
	validValSet, err := misbehaviourValSet(t, "trusted", 10).ToProto()
	if err != nil {
		t.Fatalf("validator set to proto: %v", err)
	}

	tests := []struct {
		name    string
		header  *tenderminttypes.Header
		wantErr string
	}{
		{"nil header", nil, "header is nil"},
		{"no validator set", &tenderminttypes.Header{SignedHeader: &cmtproto.SignedHeader{}}, "validator set"},
		{
			"signed header carries no header",
			&tenderminttypes.Header{SignedHeader: &cmtproto.SignedHeader{}, ValidatorSet: validValSet},
			"signed header is missing its header",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := lightBlockFromTendermintHeader(tt.header)
			if err == nil {
				t.Fatal("accepted a header it cannot convert")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("a different guard fired: got %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
