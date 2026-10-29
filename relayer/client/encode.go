// This file is the Solidity <-> Go encoding boundary: the ABI type definitions,
// the contract-struct conversions, and the Encode/Decode/Parse functions.
//
// It has its own file because that boundary is one of the three most breakable
// in the system, and it used to live inside a file called tendermint.go inside a
// package called client -- somewhere a person editing Encode.sol had no reason
// to open. Now it is findable by file name.
package client

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	spectreContract "relayer/bindings/SpectreClient"
	updateClientContract "relayer/bindings/UpdateClient"

	commettypes "github.com/cometbft/cometbft/types"
	ics23 "github.com/cosmos/ics23/go"
	"github.com/ethereum/go-ethereum/accounts/abi"
)

var clientStateType abi.Type
var consensusStateType abi.Type

// updateApplicationStateMsgType is the ABI tuple for ISpectreClientMsgs.MsgUpdateApplicationState.
// Note: no clientState field (the client reads it from its Store), and the proof
// fields are nested inside a `proof` (BatchProof) sub-tuple.
var updateApplicationStateMsgType abi.Type

// updateConsensusStateMsgType is the ABI tuple for ISpectreClientMsgs.MsgUpdateConsensusState:
// { MsgUpdateApplicationState update; ValidatorSet newValidatorSet }.
var updateConsensusStateMsgType abi.Type

// misbehaviourMsgType is the ABI tuple for
// ISpectreClientMsgs.MsgSubmitMisbehaviour.  It is hand-defined because the
// router accepts the message as opaque bytes rather than exposing a generated
// binding for this nested tuple.
var misbehaviourMsgType abi.Type

// ClientState mirrors ISpectreTendermintMsgs.ClientState. It is defined locally
// because the on-chain client no longer exposes this struct in any ABI (client
// state is passed/returned as opaque `bytes`), so abigen emits no Go type for it.
// Field order/types must match clientStateComponents below.
type ClientState struct {
	ChainId         string
	TrustLevel      TrustThreshold
	LatestHeight    updateClientContract.IICS02ClientMsgsHeight
	TrustingPeriod  uint32
	UnbondingPeriod uint32
	IsFrozen        bool
	ClockDrift      uint32
}

// TrustThreshold mirrors ISpectreTendermintMsgs.TrustThreshold (a Fraction).
type TrustThreshold struct {
	Numerator   uint8
	Denominator uint8
}

func init() {
	clientStateComponents := []abi.ArgumentMarshaling{
		{Name: "chainId", Type: "string"},
		{Name: "trustLevel", Type: "tuple", Components: []abi.ArgumentMarshaling{
			{Name: "numerator", Type: "uint8"},
			{Name: "denominator", Type: "uint8"},
		}},
		{Name: "latestHeight", Type: "tuple", Components: []abi.ArgumentMarshaling{
			{Name: "revisionNumber", Type: "uint64"},
			{Name: "revisionHeight", Type: "uint64"},
		}},
		{Name: "trustingPeriod", Type: "uint32"},
		{Name: "unbondingPeriod", Type: "uint32"},
		{Name: "isFrozen", Type: "bool"},
		{Name: "clockDrift", Type: "uint32"},
	}
	clientStateType, _ = abi.NewType("tuple", "", clientStateComponents)

	consensusStateComponents := []abi.ArgumentMarshaling{
		{Name: "timestamp", Type: "uint128"},
		{Name: "root", Type: "bytes32"},
		{Name: "nextValidatorsHash", Type: "bytes32"},
	}
	consensusStateType, _ = abi.NewType("tuple", "", consensusStateComponents)

	signedHeaderComponents := []abi.ArgumentMarshaling{
		{
			Name: "header",
			Type: "tuple",
			Components: []abi.ArgumentMarshaling{
				{Name: "version", Type: "tuple", Components: []abi.ArgumentMarshaling{
					{Name: "blockVersion", Type: "uint64"},
					{Name: "appVersion", Type: "uint64"},
				}},
				{Name: "chainId", Type: "string"},
				{Name: "height", Type: "uint64"},
				{Name: "time", Type: "uint128"},
				{Name: "hasLastBlockId", Type: "bool"},
				{Name: "lastBlockId", Type: "tuple", Components: []abi.ArgumentMarshaling{
					{Name: "hashData", Type: "bytes32"},
					{Name: "partSetHeader", Type: "tuple", Components: []abi.ArgumentMarshaling{
						{Name: "total", Type: "uint32"},
						{Name: "hashData", Type: "bytes32"},
					}},
				}},
				{Name: "hasLastCommitHash", Type: "bool"},
				{Name: "lastCommitHash", Type: "bytes32"},
				{Name: "hasDataHash", Type: "bool"},
				{Name: "dataHash", Type: "bytes32"},
				{Name: "validatorsHash", Type: "bytes32"},
				{Name: "nextValidatorsHash", Type: "bytes32"},
				{Name: "consensusHash", Type: "bytes32"},
				{Name: "appHash", Type: "bytes32"},
				{Name: "hasLastResultsHash", Type: "bool"},
				{Name: "lastResultsHash", Type: "bytes32"},
				{Name: "hasEvidenceHash", Type: "bool"},
				{Name: "evidenceHash", Type: "bytes32"},
				{Name: "proposerAddress", Type: "bytes"},
			},
		},
		{
			Name: "commit",
			Type: "tuple",
			Components: []abi.ArgumentMarshaling{
				{Name: "height", Type: "uint64"},
				{Name: "round", Type: "uint32"},
				{Name: "blockId", Type: "tuple", Components: []abi.ArgumentMarshaling{
					{Name: "hashData", Type: "bytes32"},
					{Name: "partSetHeader", Type: "tuple", Components: []abi.ArgumentMarshaling{
						{Name: "total", Type: "uint32"},
						{Name: "hashData", Type: "bytes32"},
					}},
				}},
				{Name: "commitSigs", Type: "tuple[]", Components: []abi.ArgumentMarshaling{
					{Name: "flag", Type: "uint8"},
					{Name: "validatorAddress", Type: "bytes20"},
				}},
			},
		},
	}

	headerComponents := []abi.ArgumentMarshaling{
		{Name: "signedHeader", Type: "tuple", Components: signedHeaderComponents},
		{Name: "trustedHeight", Type: "tuple", Components: []abi.ArgumentMarshaling{
			{Name: "revisionNumber", Type: "uint64"},
			{Name: "revisionHeight", Type: "uint64"},
		}},
	}

	// BatchProof sub-tuple, shared by all flows (matches ISpectreClientMsgs.BatchProof).
	batchProofComponents := []abi.ArgumentMarshaling{
		{Name: "proof", Type: "uint256[8]"},
		{Name: "commitments", Type: "uint256[2]"},
		{Name: "commitmentPok", Type: "uint256[2]"},
		{Name: "bucket", Type: "uint16"},
		{Name: "signerIndices", Type: "uint32[]"},
		{Name: "pinnedValidatorIndices", Type: "uint32[]"},
		{Name: "signerPubkeys", Type: "bytes32[]"},
		{Name: "active", Type: "bool[]"},
	}

	// MsgUpdateApplicationState: no clientState field; proof nested as BatchProof.
	applicationStateComponents := []abi.ArgumentMarshaling{
		{Name: "trustedConsensusState", Type: "tuple", Components: consensusStateComponents},
		{Name: "proposedHeader", Type: "tuple", Components: headerComponents},
		{Name: "time", Type: "uint128"},
		{Name: "proof", Type: "tuple", Components: batchProofComponents},
	}
	updateApplicationStateMsgType, _ = abi.NewType("tuple", "", applicationStateComponents)

	// ValidatorSet sub-tuple (matches ISpectreTendermintMsgs.ValidatorSet).
	validatorInfoComponents := []abi.ArgumentMarshaling{
		{Name: "valAddress", Type: "bytes"},
		{Name: "pubKey", Type: "bytes32"},
		{Name: "votingPower", Type: "uint64"},
		{Name: "proposerPriority", Type: "int64"},
	}
	validatorSetComponents := []abi.ArgumentMarshaling{
		{Name: "validators", Type: "tuple[]", Components: validatorInfoComponents},
		{Name: "hasProposer", Type: "bool"},
		{Name: "proposer", Type: "tuple", Components: validatorInfoComponents},
		{Name: "totalVotingPower", Type: "uint64"},
	}

	// MsgUpdateConsensusState: { update: MsgUpdateApplicationState, newValidatorSet: ValidatorSet }.
	updateConsensusStateMsgType, _ = abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "update", Type: "tuple", Components: applicationStateComponents},
		{Name: "newValidatorSet", Type: "tuple", Components: validatorSetComponents},
	})

	misbehaviourMsgType, _ = abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "misbehaviour", Type: "tuple", Components: []abi.ArgumentMarshaling{
			{Name: "header1", Type: "tuple", Components: headerComponents},
			{Name: "header2", Type: "tuple", Components: headerComponents},
		}},
		{Name: "trustedConsensusState1", Type: "tuple", Components: consensusStateComponents},
		{Name: "trustedConsensusState2", Type: "tuple", Components: consensusStateComponents},
		{Name: "time", Type: "uint128"},
		{Name: "proof1", Type: "tuple", Components: batchProofComponents},
		{Name: "proof2", Type: "tuple", Components: batchProofComponents},
	})
}

func nonNegativeInt64ToUint64(name string, value int64) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s cannot be negative: %d", name, value)
	}
	return uint64(value), nil
}

func votingPowerToUint64(name string, value int64) (uint64, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s cannot be negative: %d", name, value)
	}
	if value > MAX_TOTAL_VOTING_POWER {
		return 0, fmt.Errorf("%s %d exceeds max total voting power %d", name, value, MAX_TOTAL_VOTING_POWER)
	}
	return uint64(value), nil
}

func validatorInfoToContract(name string, val *commettypes.Validator) (spectreContract.ISpectreTendermintMsgsValidatorInfo, error) {
	if val == nil {
		return spectreContract.ISpectreTendermintMsgsValidatorInfo{}, nil
	}
	votingPower, err := votingPowerToUint64(name+" voting power", val.VotingPower)
	if err != nil {
		return spectreContract.ISpectreTendermintMsgsValidatorInfo{}, err
	}
	return spectreContract.ISpectreTendermintMsgsValidatorInfo{
		ValAddress:       val.Address,
		PubKey:           bytesToBytes32(val.PubKey.Bytes()),
		VotingPower:      votingPower,
		ProposerPriority: val.ProposerPriority,
	}, nil
}

func ValidatorSetToContract(valSet commettypes.ValidatorSet, name string) (spectreContract.ISpectreTendermintMsgsValidatorSet, error) {
	vals := []spectreContract.ISpectreTendermintMsgsValidatorInfo{}
	for i, val := range valSet.Validators {
		info, err := validatorInfoToContract(fmt.Sprintf("%s validator[%d]", name, i), val)
		if err != nil {
			return spectreContract.ISpectreTendermintMsgsValidatorSet{}, err
		}
		vals = append(vals, info)
	}
	proposer, err := validatorInfoToContract(name+" proposer", valSet.Proposer)
	if err != nil {
		return spectreContract.ISpectreTendermintMsgsValidatorSet{}, err
	}
	totalVotingPower, err := votingPowerToUint64(name+" total voting power", valSet.TotalVotingPower())
	if err != nil {
		return spectreContract.ISpectreTendermintMsgsValidatorSet{}, err
	}
	return spectreContract.ISpectreTendermintMsgsValidatorSet{
		Validators:       vals,
		HasProposer:      valSet.Proposer != nil,
		Proposer:         proposer,
		TotalVotingPower: totalVotingPower,
	}, nil
}

type ContractValidatorSet = spectreContract.ISpectreTendermintMsgsValidatorSet

// parseTrustThreshold parses a trust threshold fraction string like "2/3"
func ParseTrustThreshold(value string) (TrustThreshold, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return TrustThreshold{}, fmt.Errorf("invalid trust threshold format: %s (expected format: 'numerator/denominator')", value)
	}

	// bitSize 8 so values that don't fit the uint8 contract fields are a
	// parse error instead of silently truncating (e.g. "300/6" -> 44/6).
	numerator, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return TrustThreshold{}, fmt.Errorf("invalid numerator: %s", parts[0])
	}

	denominator, err := strconv.ParseUint(parts[1], 10, 8)
	if err != nil {
		return TrustThreshold{}, fmt.Errorf("invalid denominator: %s", parts[1])
	}

	if denominator == 0 {
		return TrustThreshold{}, fmt.Errorf("denominator cannot be zero")
	}

	return TrustThreshold{
		Numerator:   uint8(numerator),
		Denominator: uint8(denominator),
	}, nil
}

func ParseCommitmentProof(proof *ics23.CommitmentProof) (*spectreContract.IMembershipMsgsCommitmentProof, error) {
	if proof == nil {
		return nil, fmt.Errorf("proof is nil")
	}

	var parsedProof *spectreContract.IMembershipMsgsCommitmentProof
	switch p := proof.Proof.(type) {
	case *ics23.CommitmentProof_Exist:
		parsedProof = &spectreContract.IMembershipMsgsCommitmentProof{
			ProofType: ProofType_EXIST,
			ExistenceProof: spectreContract.IMembershipMsgsExistenceProof{
				Key:   p.Exist.Key,
				Value: p.Exist.Value,
				Leaf:  ParseLeafOp(p.Exist.Leaf),
				Path:  []spectreContract.IMembershipMsgsInnerOp{},
			},
			NonExistenceProof: spectreContract.IMembershipMsgsNonExistenceProof{},
		}

		for _, innerOp := range p.Exist.Path {
			parsedProof.ExistenceProof.Path = append(parsedProof.ExistenceProof.Path, ParseInnerOp(innerOp))
		}
	case *ics23.CommitmentProof_Nonexist:
		if p.Nonexist.Left == nil && p.Nonexist.Right == nil {
			return nil, fmt.Errorf("non-existence proof must at least left or right existence proofs")
		}
		parsedProof = &spectreContract.IMembershipMsgsCommitmentProof{
			ProofType:      ProofType_NON_EXIST,
			ExistenceProof: spectreContract.IMembershipMsgsExistenceProof{},
			NonExistenceProof: spectreContract.IMembershipMsgsNonExistenceProof{
				Key:      p.Nonexist.Key,
				HasLeft:  false,
				Left:     spectreContract.IMembershipMsgsExistenceProof{},
				HasRight: false,
				Right:    spectreContract.IMembershipMsgsExistenceProof{},
			},
		}

		if p.Nonexist.Left != nil {
			parsedProof.NonExistenceProof.HasLeft = true
			parsedProof.NonExistenceProof.Left = spectreContract.IMembershipMsgsExistenceProof{
				Key:   p.Nonexist.Left.Key,
				Value: p.Nonexist.Left.Value,
				Leaf:  ParseLeafOp(p.Nonexist.Left.Leaf),
				Path:  []spectreContract.IMembershipMsgsInnerOp{},
			}
			for _, innerOp := range p.Nonexist.Left.Path {
				parsedProof.NonExistenceProof.Left.Path = append(parsedProof.NonExistenceProof.Left.Path, ParseInnerOp(innerOp))
			}
		}

		if p.Nonexist.Right != nil {
			parsedProof.NonExistenceProof.HasRight = true
			parsedProof.NonExistenceProof.Right = spectreContract.IMembershipMsgsExistenceProof{
				Key:   p.Nonexist.Right.Key,
				Value: p.Nonexist.Right.Value,
				Leaf:  ParseLeafOp(p.Nonexist.Right.Leaf),
				Path:  []spectreContract.IMembershipMsgsInnerOp{},
			}
			for _, innerOp := range p.Nonexist.Right.Path {
				parsedProof.NonExistenceProof.Right.Path = append(parsedProof.NonExistenceProof.Right.Path, ParseInnerOp(innerOp))
			}
		}

	case *ics23.CommitmentProof_Batch:
		if len(p.Batch.GetEntries()) == 0 || p.Batch.GetEntries()[0] == nil {
			return nil, fmt.Errorf("batch proof has empty entry")
		}

		if e := p.Batch.GetEntries()[0].GetExist(); e != nil {
			parsedProof = &spectreContract.IMembershipMsgsCommitmentProof{
				ProofType: ProofType_EXIST,
				ExistenceProof: spectreContract.IMembershipMsgsExistenceProof{
					Key:   e.Key,
					Value: e.Value,
					Leaf:  ParseLeafOp(e.Leaf),
					Path:  []spectreContract.IMembershipMsgsInnerOp{},
				},
				NonExistenceProof: spectreContract.IMembershipMsgsNonExistenceProof{},
			}

			for _, innerOp := range e.Path {
				parsedProof.ExistenceProof.Path = append(parsedProof.ExistenceProof.Path, ParseInnerOp(innerOp))
			}
		}

		if n := p.Batch.GetEntries()[0].GetNonexist(); n != nil {
			parsedProof = &spectreContract.IMembershipMsgsCommitmentProof{
				ProofType:      ProofType_NON_EXIST,
				ExistenceProof: spectreContract.IMembershipMsgsExistenceProof{},
				NonExistenceProof: spectreContract.IMembershipMsgsNonExistenceProof{
					Key:      n.Key,
					HasLeft:  false,
					Left:     spectreContract.IMembershipMsgsExistenceProof{},
					HasRight: false,
					Right:    spectreContract.IMembershipMsgsExistenceProof{},
				},
			}

			if n.Left != nil {
				parsedProof.NonExistenceProof.HasLeft = true
				parsedProof.NonExistenceProof.Left = spectreContract.IMembershipMsgsExistenceProof{
					Key:   n.Left.Key,
					Value: n.Left.Value,
					Leaf:  ParseLeafOp(n.Left.Leaf),
					Path:  []spectreContract.IMembershipMsgsInnerOp{},
				}
				for _, innerOp := range n.Left.Path {
					parsedProof.NonExistenceProof.Left.Path = append(parsedProof.NonExistenceProof.Left.Path, ParseInnerOp(innerOp))
				}
			}

			if n.Right != nil {
				parsedProof.NonExistenceProof.HasRight = true
				parsedProof.NonExistenceProof.Right = spectreContract.IMembershipMsgsExistenceProof{
					Key:   n.Right.Key,
					Value: n.Right.Value,
					Leaf:  ParseLeafOp(n.Right.Leaf),
					Path:  []spectreContract.IMembershipMsgsInnerOp{},
				}
				for _, innerOp := range n.Right.Path {
					parsedProof.NonExistenceProof.Right.Path = append(parsedProof.NonExistenceProof.Right.Path, ParseInnerOp(innerOp))
				}
			}
		}
	case *ics23.CommitmentProof_Compressed:
		decompressedProof := ics23.Decompress(proof)
		return ParseCommitmentProof(decompressedProof)
	default:
		return nil, fmt.Errorf("unrecognized proof type")
	}

	return parsedProof, nil
}

func ParseLeafOp(leafOp *ics23.LeafOp) spectreContract.IMembershipMsgsLeafOp {
	if leafOp == nil {
		return spectreContract.IMembershipMsgsLeafOp{}
	}

	return spectreContract.IMembershipMsgsLeafOp{
		HashOp:       uint8(leafOp.Hash),
		PrehashKey:   uint8(leafOp.PrehashKey),
		PrehashValue: uint8(leafOp.PrehashValue),
		Prefix:       leafOp.Prefix,
	}
}

func ParseInnerOp(innerOp *ics23.InnerOp) spectreContract.IMembershipMsgsInnerOp {
	if innerOp == nil {
		return spectreContract.IMembershipMsgsInnerOp{}
	}

	return spectreContract.IMembershipMsgsInnerOp{
		HashOp: uint8(innerOp.Hash),
		Prefix: innerOp.Prefix,
		Suffix: innerOp.Suffix,
	}
}

func EncodeClientState(clientState ClientState) ([]byte, error) {
	args := abi.Arguments{
		{Type: clientStateType},
	}
	encoded, err := args.Pack(clientState)
	return encoded, err
}

func DecodeClientState(data []byte) (ClientState, error) {
	args := abi.Arguments{
		{Type: clientStateType},
	}
	unpacked, err := args.Unpack(data)
	if err != nil {
		return ClientState{}, fmt.Errorf("unpack: %w", err)
	}
	if len(unpacked) == 0 {
		return ClientState{}, fmt.Errorf("no data unpacked")
	}
	// unpacked[0] is an anonymous struct matching the tuple.
	// Use JSON roundtrip to convert to the named target type,
	// since args.Copy maps the tuple to the first field instead of the struct itself.
	jsonBytes, err := json.Marshal(unpacked[0])
	if err != nil {
		return ClientState{}, fmt.Errorf("marshal unpacked tuple: %w", err)
	}
	var clientState ClientState
	if err := json.Unmarshal(jsonBytes, &clientState); err != nil {
		return ClientState{}, fmt.Errorf("unmarshal to client state: %w", err)
	}
	return clientState, nil
}

func EncodeConsensusState(consensusState updateClientContract.ISpectreTendermintMsgsConsensusState) ([]byte, error) {
	args := abi.Arguments{
		{Type: consensusStateType},
	}
	encoded, err := args.Pack(consensusState)
	return encoded, err

}

// EncodeUpdateApplicationStateMsg abi-encodes a MsgUpdateApplicationState for
// SpectreClient.updateApplicationState(bytes) / ICS26Router.updateApplicationState.
func EncodeUpdateApplicationStateMsg(msg updateClientContract.ISpectreClientMsgsMsgUpdateApplicationState) ([]byte, error) {
	args := abi.Arguments{
		{Type: updateApplicationStateMsgType},
	}
	return args.Pack(msg)
}

// MsgUpdateConsensusState mirrors ISpectreClientMsgs.MsgUpdateConsensusState.
// There is no generated Go type for it (nothing takes it as a typed on-chain
// param), so it is hand-defined and packed against updateConsensusStateMsgType.
type MsgUpdateConsensusState struct {
	Update          updateClientContract.ISpectreClientMsgsMsgUpdateApplicationState
	NewValidatorSet spectreContract.ISpectreTendermintMsgsValidatorSet
}
