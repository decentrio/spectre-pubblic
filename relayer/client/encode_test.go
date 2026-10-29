package client

import (
	"bytes"
	"encoding/json"
	"math/big"
	updateClientContract "relayer/bindings/UpdateClient"
	"strings"
	"testing"

	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"

	commettypes "github.com/cometbft/cometbft/types"
	ics23 "github.com/cosmos/ics23/go"
)

// TestEncodeUpdateApplicationStateMsg_MatchesGeneratedABI checks that the manual
// ABI tuple (updateApplicationStateMsgType) produces bytes that the abigen-
// generated UpdateClient.verifyHeader input tuple can round-trip — i.e. the
// hand-built encoding matches the generated ABI exactly.
// fullyPopulatedApplicationStateMsg is the fixture both update encoders share.
// Shared on purpose: two copies drift, and the field a copy forgets to set is
// exactly the field that then goes unchecked -- CommitSigs was left empty here
// once, and the validatorAddress bug rode through green tests to chain.
func fullyPopulatedApplicationStateMsg() updateClientContract.ISpectreClientMsgsMsgUpdateApplicationState {
	var validatorsHash [32]byte
	validatorsHash[0] = 0x88
	validatorsHash[1] = 0xbe

	zero8 := [8]*big.Int{}
	for i := range zero8 {
		zero8[i] = big.NewInt(0)
	}
	zero2 := [2]*big.Int{big.NewInt(0), big.NewInt(0)}

	msg := updateClientContract.ISpectreClientMsgsMsgUpdateApplicationState{
		TrustedConsensusState: updateClientContract.ISpectreTendermintMsgsConsensusState{
			Timestamp:          big.NewInt(1700000000000000000),
			NextValidatorsHash: validatorsHash,
		},
		ProposedHeader: updateClientContract.ISpectreTendermintMsgsHeader{
			SignedHeader: updateClientContract.ISpectreTendermintMsgsSignedHeader{
				Header: updateClientContract.ISpectreTendermintMsgsBlockHeader{
					ChainId:            "test-0",
					Time:               big.NewInt(1700000001000000000),
					ValidatorsHash:     validatorsHash,
					NextValidatorsHash: validatorsHash,
				},
				Commit: updateClientContract.ISpectreTendermintMsgsBlockCommit{
					Height: 58,
					CommitSigs: []updateClientContract.ISpectreTendermintMsgsCommitSig{{
						Flag:             2,
						ValidatorAddress: [20]byte{0x12, 0x34, 0x56, 0x78},
					}},
				},
			},
			TrustedHeight: updateClientContract.IICS02ClientMsgsHeight{
				RevisionNumber: 0,
				RevisionHeight: 19,
			},
		},
		Time: big.NewInt(1700000002000000000),
		Proof: updateClientContract.ISpectreClientMsgsBatchProof{
			Proof:                  zero8,
			Commitments:            zero2,
			CommitmentPok:          zero2,
			Bucket:                 16,
			SignerIndices:          []uint32{0, 1},
			PinnedValidatorIndices: []uint32{7, 8},
			SignerPubkeys:          [][32]byte{{0x01}, {0x02}},
			Active:                 []bool{true, true},
		},
	}
	return msg
}

func TestEncodeUpdateApplicationStateMsg_MatchesGeneratedABI(t *testing.T) {
	var validatorsHash [32]byte
	validatorsHash[0] = 0x88
	validatorsHash[1] = 0xbe
	msg := fullyPopulatedApplicationStateMsg()

	encoded, err := EncodeUpdateApplicationStateMsg(msg)
	if err != nil {
		t.Fatalf("encode update application state msg: %v", err)
	}

	contractABI, err := updateClientContract.ContractUpdateClientMetaData.GetAbi()
	if err != nil {
		t.Fatalf("parse generated ABI: %v", err)
	}
	if got, want := updateApplicationStateMsgType.String(), contractABI.Methods["verifyHeader"].Inputs[0].Type.String(); got != want {
		t.Fatalf("manual ABI tuple diverged from the generated contract ABI\n got=%s\nwant=%s", got, want)
	}
	unpacked, err := contractABI.Methods["verifyHeader"].Inputs.Unpack(encoded)
	if err != nil {
		t.Fatalf("generated ABI failed to unpack encoded msg: %v", err)
	}
	if len(unpacked) != 1 {
		t.Fatalf("unpacked %d values, want 1", len(unpacked))
	}

	jsonBytes, err := json.Marshal(unpacked[0])
	if err != nil {
		t.Fatalf("marshal unpacked tuple: %v", err)
	}
	var decoded updateClientContract.ISpectreClientMsgsMsgUpdateApplicationState
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("unmarshal decoded update msg: %v", err)
	}
	if decoded.ProposedHeader.SignedHeader.Header.ValidatorsHash != validatorsHash {
		t.Fatalf(
			"validatorsHash decoded incorrectly: got %x want %x",
			decoded.ProposedHeader.SignedHeader.Header.ValidatorsHash,
			validatorsHash,
		)
	}
	if len(decoded.Proof.PinnedValidatorIndices) != 2 || decoded.Proof.PinnedValidatorIndices[0] != 7 || decoded.Proof.PinnedValidatorIndices[1] != 8 {
		t.Fatalf("pinned validator indices decoded incorrectly: %+v", decoded.Proof.PinnedValidatorIndices)
	}
	if got := decoded.ProposedHeader.SignedHeader.Commit.CommitSigs; len(got) != 1 || got[0].Flag != 2 || got[0].ValidatorAddress != ([20]byte{0x12, 0x34, 0x56, 0x78}) {
		t.Fatalf("commit signatures decoded incorrectly: %+v", got)
	}
}

func TestEncodeConsensusState(t *testing.T) {
	t.Run("valid consensus state encodes without error", func(t *testing.T) {
		consensusState := updateClientContract.ISpectreTendermintMsgsConsensusState{
			Timestamp:          big.NewInt(1700000000000),
			Root:               [32]byte{0x01, 0x02, 0x03},
			NextValidatorsHash: [32]byte{0x04, 0x05, 0x06},
		}
		encoded, err := EncodeConsensusState(consensusState)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(encoded) == 0 {
			t.Fatal("encoded result should not be empty")
		}
	})
}

func TestDecodeClientState(t *testing.T) {
	original := ClientState{
		ChainId: "cosmoshub-4",
		TrustLevel: TrustThreshold{
			Numerator:   2,
			Denominator: 3,
		},
		LatestHeight: updateClientContract.IICS02ClientMsgsHeight{
			RevisionNumber: 4,
			RevisionHeight: 100,
		},
		TrustingPeriod:  1209600,
		UnbondingPeriod: 1814400,
		IsFrozen:        false,
		ClockDrift:      15,
	}

	encoded, err := EncodeClientState(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeClientState(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.ChainId != original.ChainId {
		t.Errorf("ChainId: got %q, want %q", decoded.ChainId, original.ChainId)
	}
	if decoded.TrustLevel.Numerator != original.TrustLevel.Numerator {
		t.Errorf("TrustLevel.Numerator: got %d, want %d", decoded.TrustLevel.Numerator, original.TrustLevel.Numerator)
	}
	if decoded.TrustLevel.Denominator != original.TrustLevel.Denominator {
		t.Errorf("TrustLevel.Denominator: got %d, want %d", decoded.TrustLevel.Denominator, original.TrustLevel.Denominator)
	}
	if decoded.LatestHeight.RevisionNumber != original.LatestHeight.RevisionNumber {
		t.Errorf("LatestHeight.RevisionNumber: got %d, want %d", decoded.LatestHeight.RevisionNumber, original.LatestHeight.RevisionNumber)
	}
	if decoded.LatestHeight.RevisionHeight != original.LatestHeight.RevisionHeight {
		t.Errorf("LatestHeight.RevisionHeight: got %d, want %d", decoded.LatestHeight.RevisionHeight, original.LatestHeight.RevisionHeight)
	}
	if decoded.TrustingPeriod != original.TrustingPeriod {
		t.Errorf("TrustingPeriod: got %d, want %d", decoded.TrustingPeriod, original.TrustingPeriod)
	}
	if decoded.UnbondingPeriod != original.UnbondingPeriod {
		t.Errorf("UnbondingPeriod: got %d, want %d", decoded.UnbondingPeriod, original.UnbondingPeriod)
	}
	if decoded.IsFrozen != original.IsFrozen {
		t.Errorf("IsFrozen: got %v, want %v", decoded.IsFrozen, original.IsFrozen)
	}
	if decoded.ClockDrift != original.ClockDrift {
		t.Errorf("ClockDrift: got %d, want %d", decoded.ClockDrift, original.ClockDrift)
	}
}

func TestEncodeClientState(t *testing.T) {
	t.Run("valid client state encodes without error", func(t *testing.T) {
		clientState := ClientState{
			ChainId: "cosmoshub-4",
			TrustLevel: TrustThreshold{
				Numerator:   2,
				Denominator: 3,
			},
			LatestHeight: updateClientContract.IICS02ClientMsgsHeight{
				RevisionNumber: 4,
				RevisionHeight: 100,
			},
			TrustingPeriod:  1209600,
			UnbondingPeriod: 1814400,
			IsFrozen:        false,
			ClockDrift:      15,
		}
		encoded, err := EncodeClientState(clientState)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(encoded) == 0 {
			t.Fatal("encoded result should not be empty")
		}
	})
}

func TestParseInnerOp(t *testing.T) {
	t.Run("nil input returns zero value", func(t *testing.T) {
		result := ParseInnerOp(nil)
		if result.HashOp != 0 {
			t.Errorf("HashOp: got %d, want 0", result.HashOp)
		}
		if result.Prefix != nil {
			t.Errorf("Prefix: got %v, want nil", result.Prefix)
		}
		if result.Suffix != nil {
			t.Errorf("Suffix: got %v, want nil", result.Suffix)
		}
	})

	t.Run("valid inner op", func(t *testing.T) {
		innerOp := &ics23.InnerOp{
			Hash:   ics23.HashOp_SHA256,
			Prefix: []byte{0x01},
			Suffix: []byte{0x02},
		}
		result := ParseInnerOp(innerOp)
		if result.HashOp != uint8(ics23.HashOp_SHA256) {
			t.Errorf("HashOp: got %d, want %d", result.HashOp, uint8(ics23.HashOp_SHA256))
		}
		if len(result.Prefix) != 1 || result.Prefix[0] != 0x01 {
			t.Errorf("Prefix: got %v, want [0x01]", result.Prefix)
		}
		if len(result.Suffix) != 1 || result.Suffix[0] != 0x02 {
			t.Errorf("Suffix: got %v, want [0x02]", result.Suffix)
		}
	})
}

func TestParseLeafOp(t *testing.T) {
	t.Run("nil input returns zero value", func(t *testing.T) {
		result := ParseLeafOp(nil)
		if result.HashOp != 0 {
			t.Errorf("HashOp: got %d, want 0", result.HashOp)
		}
		if result.PrehashKey != 0 {
			t.Errorf("PrehashKey: got %d, want 0", result.PrehashKey)
		}
		if result.PrehashValue != 0 {
			t.Errorf("PrehashValue: got %d, want 0", result.PrehashValue)
		}
		if result.Prefix != nil {
			t.Errorf("Prefix: got %v, want nil", result.Prefix)
		}
	})

	t.Run("valid leaf op", func(t *testing.T) {
		leafOp := &ics23.LeafOp{
			Hash:         ics23.HashOp_SHA256,
			PrehashKey:   ics23.HashOp_NO_HASH,
			PrehashValue: ics23.HashOp_SHA256,
			Prefix:       []byte{0x00},
		}
		result := ParseLeafOp(leafOp)
		if result.HashOp != uint8(ics23.HashOp_SHA256) {
			t.Errorf("HashOp: got %d, want %d", result.HashOp, uint8(ics23.HashOp_SHA256))
		}
		if result.PrehashKey != uint8(ics23.HashOp_NO_HASH) {
			t.Errorf("PrehashKey: got %d, want %d", result.PrehashKey, uint8(ics23.HashOp_NO_HASH))
		}
		if result.PrehashValue != uint8(ics23.HashOp_SHA256) {
			t.Errorf("PrehashValue: got %d, want %d", result.PrehashValue, uint8(ics23.HashOp_SHA256))
		}
		if len(result.Prefix) != 1 || result.Prefix[0] != 0x00 {
			t.Errorf("Prefix: got %v, want [0x00]", result.Prefix)
		}
	})
}

func TestParseTrustThreshold(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantNum uint8
		wantDen uint8
		wantErr bool
	}{
		{name: "valid 2/3", input: "2/3", wantNum: 2, wantDen: 3},
		{name: "valid 1/3", input: "1/3", wantNum: 1, wantDen: 3},
		{name: "invalid format dash", input: "2-3", wantErr: true},
		{name: "invalid format no separator", input: "abc", wantErr: true},
		{name: "invalid numerator", input: "abc/3", wantErr: true},
		{name: "invalid denominator", input: "2/abc", wantErr: true},
		{name: "zero denominator", input: "2/0", wantErr: true},
		{name: "numerator exceeds uint8", input: "300/6", wantErr: true},
		{name: "denominator exceeds uint8", input: "2/256", wantErr: true},
		{name: "uint8 max accepted", input: "255/255", wantNum: 255, wantDen: 255},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTrustThreshold(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tc.input, err)
			}
			if got.Numerator != tc.wantNum {
				t.Errorf("numerator: got %d, want %d", got.Numerator, tc.wantNum)
			}
			if got.Denominator != tc.wantDen {
				t.Errorf("denominator: got %d, want %d", got.Denominator, tc.wantDen)
			}
		})
	}
}

func TestNonNegativeInt64ToUint64(t *testing.T) {
	if got, err := nonNegativeInt64ToUint64("height", 5); err != nil || got != 5 {
		t.Fatalf("5 -> (%d, %v), want (5, nil)", got, err)
	}
	if got, err := nonNegativeInt64ToUint64("height", 0); err != nil || got != 0 {
		t.Fatalf("0 -> (%d, %v), want (0, nil)", got, err)
	}
	if _, err := nonNegativeInt64ToUint64("height", -1); err == nil || !strings.Contains(err.Error(), "height") {
		t.Fatalf("-1 must be refused and the error must name the field, got %v", err)
	}
}

func TestVotingPowerToUint64(t *testing.T) {
	if got, err := votingPowerToUint64("power", MAX_TOTAL_VOTING_POWER); err != nil || got != uint64(MAX_TOTAL_VOTING_POWER) {
		t.Fatalf("the maximum is allowed: got (%d, %v)", got, err)
	}
	if _, err := votingPowerToUint64("power", MAX_TOTAL_VOTING_POWER+1); err == nil || !strings.Contains(err.Error(), "exceeds max total voting power") {
		t.Fatalf("one above the maximum must be refused, got %v", err)
	}
	if _, err := votingPowerToUint64("power", -7); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("a negative power must be refused, got %v", err)
	}
}

func TestValidatorInfoToContract(t *testing.T) {
	if info, err := validatorInfoToContract("v", nil); err != nil || info.ValAddress != nil || info.VotingPower != 0 || info.PubKey != [32]byte{} {
		t.Fatalf("a nil validator maps to the zero value without error, got (%+v, %v)", info, err)
	}
	key := cmted25519.GenPrivKey().PubKey()
	val := &commettypes.Validator{Address: key.Address(), PubKey: key, VotingPower: 42, ProposerPriority: -3}
	info, err := validatorInfoToContract("v", val)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(info.ValAddress, key.Address()) || info.PubKey != bytesToBytes32(key.Bytes()) || info.VotingPower != 42 || info.ProposerPriority != -3 {
		t.Fatalf("fields not carried: %+v", info)
	}
	val.VotingPower = -1
	if _, err := validatorInfoToContract("v", val); err == nil || !strings.Contains(err.Error(), "v voting power") {
		t.Fatalf("a negative voting power must be refused with the validator named, got %v", err)
	}
}

func TestParseCommitmentProof(t *testing.T) {
	if _, err := ParseCommitmentProof(nil); err == nil {
		t.Fatal("a nil proof must be refused")
	}
	exist := &ics23.ExistenceProof{Key: []byte("k"), Value: []byte("v"), Leaf: &ics23.LeafOp{Hash: ics23.HashOp_SHA256}, Path: []*ics23.InnerOp{{Hash: ics23.HashOp_SHA256, Prefix: []byte{1}}, {Hash: ics23.HashOp_SHA256, Suffix: []byte{2}}}}
	parsed, err := ParseCommitmentProof(&ics23.CommitmentProof{Proof: &ics23.CommitmentProof_Exist{Exist: exist}})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ProofType != ProofType_EXIST || string(parsed.ExistenceProof.Key) != "k" || string(parsed.ExistenceProof.Value) != "v" || len(parsed.ExistenceProof.Path) != 2 {
		t.Fatalf("existence proof not carried: %+v", parsed)
	}
	if _, err := ParseCommitmentProof(&ics23.CommitmentProof{Proof: &ics23.CommitmentProof_Nonexist{Nonexist: &ics23.NonExistenceProof{Key: []byte("k")}}}); err == nil {
		t.Fatal("a non-existence proof with neither neighbour must be refused")
	}
	parsed, err = ParseCommitmentProof(&ics23.CommitmentProof{Proof: &ics23.CommitmentProof_Nonexist{Nonexist: &ics23.NonExistenceProof{Key: []byte("k"), Left: exist}}})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ProofType != ProofType_NON_EXIST || !parsed.NonExistenceProof.HasLeft || parsed.NonExistenceProof.HasRight || string(parsed.NonExistenceProof.Left.Key) != "k" {
		t.Fatalf("left-only non-existence proof not carried: %+v", parsed.NonExistenceProof)
	}
}
