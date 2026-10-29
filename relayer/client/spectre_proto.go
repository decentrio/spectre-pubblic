package client

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"relayer/chain/ethnative/spectrepb"
)

// This file converts the relayer's JSON representation of Ethereum light client
// data (the structs in beacon.go, which the beacon API and the Rust wasm client
// share) to the protobuf messages of the spectre light client module that the
// destination Cosmos chain hosts natively (ibc.lightclients.spectre.v1), and back
// for the client state the relayer reads from the chain. The conversion happens
// at the Cosmos boundary only, so everything that builds headers and proofs keeps
// working on the JSON.

// SpectreClientStateTypeURL is the Any type URL of the native Ethereum client state.
const SpectreClientStateTypeURL = "/ibc.lightclients.spectre.v1.ClientState"

// ToProto converts the client state to its protobuf form.
func (cs *EthereumClientState) ToProto() (*spectrepb.ClientState, error) {
	root, err := fixedHex("genesis_validators_root", cs.GenesisValidatorsRoot, 32)
	if err != nil {
		return nil, err
	}
	address, err := fixedHex("ibc_contract_address", cs.IbcContractAddress, 20)
	if err != nil {
		return nil, err
	}
	slot, err := fixedHex("ibc_commitment_slot", cs.IBCCommitmentSlot, 32)
	if err != nil {
		return nil, err
	}
	forks, err := cs.ForkParameters.toProto()
	if err != nil {
		return nil, err
	}
	return &spectrepb.ClientState{
		ChainId:                      cs.ChainID,
		GenesisValidatorsRoot:        root,
		MinSyncCommitteeParticipants: cs.MinSyncCommitteeParticipants,
		SyncCommitteeSize:            cs.SyncCommitteeSize,
		GenesisTime:                  cs.GenesisTime,
		GenesisSlot:                  cs.GenesisSlot,
		ForkParameters:               *forks,
		SecondsPerSlot:               cs.SecondsPerSlot,
		SlotsPerEpoch:                cs.SlotsPerEpoch,
		EpochsPerSyncCommitteePeriod: cs.EpochsPerSyncCommitteePeriod,
		LatestSlot:                   cs.LatestSlot,
		LatestExecutionBlockNumber:   cs.LatestExecutionBlockNumber,
		Frozen:                       cs.IsFrozen,
		IbcContractAddress:           address,
		IbcCommitmentSlot:            slot,
	}, nil
}

// EthereumClientStateFromProto converts the protobuf client state read from the
// chain to the relayer's representation.
func EthereumClientStateFromProto(p *spectrepb.ClientState) *EthereumClientState {
	return &EthereumClientState{
		ChainID:                      p.ChainId,
		EpochsPerSyncCommitteePeriod: p.EpochsPerSyncCommitteePeriod,
		ForkParameters:               forkParametersFromProto(&p.ForkParameters),
		GenesisSlot:                  p.GenesisSlot,
		GenesisTime:                  p.GenesisTime,
		GenesisValidatorsRoot:        hexString(p.GenesisValidatorsRoot),
		IBCCommitmentSlot:            hexString(p.IbcCommitmentSlot),
		IbcContractAddress:           hexString(p.IbcContractAddress),
		IsFrozen:                     p.Frozen,
		LatestExecutionBlockNumber:   p.LatestExecutionBlockNumber,
		LatestSlot:                   p.LatestSlot,
		MinSyncCommitteeParticipants: p.MinSyncCommitteeParticipants,
		SecondsPerSlot:               p.SecondsPerSlot,
		SlotsPerEpoch:                p.SlotsPerEpoch,
		SyncCommitteeSize:            p.SyncCommitteeSize,
	}
}

func (fp *ForkParameters) toProto() (*spectrepb.ForkParameters, error) {
	genesis, err := fixedHex("genesis_fork_version", fp.GenesisForkVersion, 4)
	if err != nil {
		return nil, err
	}
	out := &spectrepb.ForkParameters{GenesisForkVersion: genesis, GenesisSlot: fp.GenesisSlot}
	for _, entry := range []struct {
		name     string
		fork     Fork
		optional bool
		dst      *spectrepb.Fork
	}{
		{"altair", fp.Altair, false, &out.Altair},
		{"bellatrix", fp.Bellatrix, false, &out.Bellatrix},
		{"capella", fp.Capella, false, &out.Capella},
		{"deneb", fp.Deneb, false, &out.Deneb},
		{"electra", fp.Electra, false, &out.Electra},
		// Fulu and Gloas were added to the client state later: a state that omits
		// one is unscheduled, as the Rust client's serde default is.
		{"fulu", fp.Fulu, true, &out.Fulu},
		{"gloas", fp.Gloas, true, &out.Gloas},
	} {
		if entry.optional && entry.fork.Version == "" && entry.fork.Epoch == 0 {
			*entry.dst = spectrepb.Fork{Version: make([]byte, 4), Epoch: ^uint64(0)}
			continue
		}
		version, err := fixedHex(entry.name+" fork version", entry.fork.Version, 4)
		if err != nil {
			return nil, err
		}
		*entry.dst = spectrepb.Fork{Version: version, Epoch: entry.fork.Epoch}
	}
	return out, nil
}

func forkParametersFromProto(p *spectrepb.ForkParameters) ForkParameters {
	fork := func(f spectrepb.Fork) Fork { return Fork{Epoch: f.Epoch, Version: hexString(f.Version)} }
	return ForkParameters{
		Altair:             fork(p.Altair),
		Bellatrix:          fork(p.Bellatrix),
		Capella:            fork(p.Capella),
		Deneb:              fork(p.Deneb),
		Electra:            fork(p.Electra),
		Fulu:               fork(p.Fulu),
		Gloas:              fork(p.Gloas),
		GenesisForkVersion: hexString(p.GenesisForkVersion),
		GenesisSlot:        p.GenesisSlot,
	}
}

// ToProto converts the consensus state to its protobuf form.
func (cs *EthereumConsensusState) ToProto() (*spectrepb.ConsensusState, error) {
	root, err := fixedHex("state_root", cs.StateRoot, 32)
	if err != nil {
		return nil, err
	}
	current, err := cs.CurrentSyncCommittee.toProto()
	if err != nil {
		return nil, fmt.Errorf("current_sync_committee: %w", err)
	}
	out := &spectrepb.ConsensusState{
		Slot:                 cs.Slot,
		StateRoot:            root,
		Timestamp:            cs.Timestamp,
		CurrentSyncCommittee: *current,
	}
	if cs.NextSyncCommittee != nil {
		next, err := cs.NextSyncCommittee.toProto()
		if err != nil {
			return nil, fmt.Errorf("next_sync_committee: %w", err)
		}
		out.NextSyncCommittee = next
	}
	return out, nil
}

func (sc *SummarizedSyncCommittee) toProto() (*spectrepb.SummarizedSyncCommittee, error) {
	hash, err := fixedHex("pubkeys_hash", sc.PubkeysHash, 32)
	if err != nil {
		return nil, err
	}
	aggregate, err := fixedHex("aggregate_pubkey", sc.AggregatePubkey, 48)
	if err != nil {
		return nil, err
	}
	return &spectrepb.SummarizedSyncCommittee{PubkeysHash: hash, AggregatePubkey: aggregate}, nil
}

// ToProto converts the header client message to its protobuf form.
func (h *EthereumHeader) ToProto() (*spectrepb.Header, error) {
	out := &spectrepb.Header{TrustedSlot: h.TrustedSlot}

	switch {
	case h.ActiveSyncCommittee.Current != nil && h.ActiveSyncCommittee.Next == nil:
		committee, err := h.ActiveSyncCommittee.Current.toProto()
		if err != nil {
			return nil, fmt.Errorf("active sync committee: %w", err)
		}
		out.ActiveSyncCommittee = committee
	case h.ActiveSyncCommittee.Next != nil && h.ActiveSyncCommittee.Current == nil:
		committee, err := h.ActiveSyncCommittee.Next.toProto()
		if err != nil {
			return nil, fmt.Errorf("active sync committee: %w", err)
		}
		out.ActiveSyncCommittee = committee
		out.IsNextCommittee = true
	default:
		return nil, fmt.Errorf("active sync committee must be exactly one of Current and Next")
	}

	update, err := h.ConsensusUpdate.toProto()
	if err != nil {
		return nil, fmt.Errorf("consensus update: %w", err)
	}
	out.ConsensusUpdate = *update

	if h.ExecutionHeaderRLP != "" {
		rlp, err := decodeHex(h.ExecutionHeaderRLP)
		if err != nil {
			return nil, fmt.Errorf("execution_header_rlp: %w", err)
		}
		out.ExecutionHeaderRlp = rlp
	}
	return out, nil
}

func (sc *SyncCommittee) toProto() (*spectrepb.SyncCommittee, error) {
	pubkeys, err := hexList("pubkeys", sc.Pubkeys)
	if err != nil {
		return nil, err
	}
	aggregate, err := decodeHex(sc.AggregatePubkey)
	if err != nil {
		return nil, fmt.Errorf("aggregate_pubkey: %w", err)
	}
	return &spectrepb.SyncCommittee{Pubkeys: pubkeys, AggregatePubkey: aggregate}, nil
}

func (u *LightClientUpdate) toProto() (*spectrepb.LightClientUpdate, error) {
	attested, err := u.AttestedHeader.toProto()
	if err != nil {
		return nil, fmt.Errorf("attested_header: %w", err)
	}
	finalized, err := u.FinalizedHeader.toProto()
	if err != nil {
		return nil, fmt.Errorf("finalized_header: %w", err)
	}
	finality, err := hexList("finality_branch", u.FinalityBranch)
	if err != nil {
		return nil, err
	}
	bits, err := decodeHex(u.SyncAggregate.SyncCommitteeBits)
	if err != nil {
		return nil, fmt.Errorf("sync_committee_bits: %w", err)
	}
	signature, err := decodeHex(u.SyncAggregate.SyncCommitteeSignature)
	if err != nil {
		return nil, fmt.Errorf("sync_committee_signature: %w", err)
	}
	signatureSlot, err := parseUint("signature_slot", u.SignatureSlot)
	if err != nil {
		return nil, err
	}
	out := &spectrepb.LightClientUpdate{
		AttestedHeader:  *attested,
		FinalizedHeader: *finalized,
		FinalityBranch:  finality,
		SyncAggregate:   spectrepb.SyncAggregate{SyncCommitteeBits: bits, SyncCommitteeSignature: signature},
		SignatureSlot:   signatureSlot,
	}
	if u.NextSyncCommittee != nil {
		next, err := u.NextSyncCommittee.toProto()
		if err != nil {
			return nil, fmt.Errorf("next_sync_committee: %w", err)
		}
		out.NextSyncCommittee = next
	}
	if len(u.NextSyncCommitteeBranch) > 0 {
		branch, err := hexList("next_sync_committee_branch", u.NextSyncCommitteeBranch)
		if err != nil {
			return nil, err
		}
		out.NextSyncCommitteeBranch = branch
	}
	return out, nil
}

func (h *LightClientHeader) toProto() (*spectrepb.LightClientHeader, error) {
	beacon, err := h.Beacon.toProto()
	if err != nil {
		return nil, fmt.Errorf("beacon: %w", err)
	}
	branch, err := hexList("execution_branch", h.ExecutionBranch)
	if err != nil {
		return nil, err
	}
	out := &spectrepb.LightClientHeader{Beacon: *beacon, ExecutionBranch: branch}
	if h.Execution != nil {
		execution, err := h.Execution.toProto()
		if err != nil {
			return nil, fmt.Errorf("execution: %w", err)
		}
		out.Execution = execution
	}
	if h.ExecutionBlockHash != "" {
		hash, err := decodeHex(h.ExecutionBlockHash)
		if err != nil {
			return nil, fmt.Errorf("execution_block_hash: %w", err)
		}
		out.ExecutionBlockHash = hash
	}
	return out, nil
}

func (b *BeaconBlockHeader) toProto() (*spectrepb.BeaconBlockHeader, error) {
	slot, err := parseUint("slot", b.Slot)
	if err != nil {
		return nil, err
	}
	proposer, err := parseUint("proposer_index", b.ProposerIndex)
	if err != nil {
		return nil, err
	}
	parent, err := decodeHex(b.ParentRoot)
	if err != nil {
		return nil, fmt.Errorf("parent_root: %w", err)
	}
	state, err := decodeHex(b.StateRoot)
	if err != nil {
		return nil, fmt.Errorf("state_root: %w", err)
	}
	body, err := decodeHex(b.BodyRoot)
	if err != nil {
		return nil, fmt.Errorf("body_root: %w", err)
	}
	return &spectrepb.BeaconBlockHeader{
		Slot: slot, ProposerIndex: proposer, ParentRoot: parent, StateRoot: state, BodyRoot: body,
	}, nil
}

func (e *ExecutionPayloadHeader) toProto() (*spectrepb.ExecutionPayloadHeader, error) {
	out := &spectrepb.ExecutionPayloadHeader{}
	for _, field := range []struct {
		name string
		src  string
		dst  *[]byte
	}{
		{"parent_hash", e.ParentHash, &out.ParentHash},
		{"fee_recipient", e.FeeRecipient, &out.FeeRecipient},
		{"state_root", e.StateRoot, &out.StateRoot},
		{"receipts_root", e.ReceiptsRoot, &out.ReceiptsRoot},
		{"logs_bloom", e.LogsBloom, &out.LogsBloom},
		{"prev_randao", e.PrevRandao, &out.PrevRandao},
		{"extra_data", e.ExtraData, &out.ExtraData},
		{"block_hash", e.BlockHash, &out.BlockHash},
		{"transactions_root", e.TransactionsRoot, &out.TransactionsRoot},
		{"withdrawals_root", e.WithdrawalsRoot, &out.WithdrawalsRoot},
	} {
		value, err := decodeHex(field.src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field.name, err)
		}
		*field.dst = value
	}
	for _, field := range []struct {
		name string
		src  string
		dst  *uint64
	}{
		{"block_number", e.BlockNumber, &out.BlockNumber},
		{"gas_limit", e.GasLimit, &out.GasLimit},
		{"gas_used", e.GasUsed, &out.GasUsed},
		{"timestamp", e.Timestamp, &out.Timestamp},
		{"blob_gas_used", e.BlobGasUsed, &out.BlobGasUsed},
		{"excess_blob_gas", e.ExcessBlobGas, &out.ExcessBlobGas},
	} {
		value, err := parseUint(field.name, field.src)
		if err != nil {
			return nil, err
		}
		*field.dst = value
	}
	baseFee, err := uint256BigEndian(e.BaseFeePerGas)
	if err != nil {
		return nil, fmt.Errorf("base_fee_per_gas: %w", err)
	}
	out.BaseFeePerGas = baseFee
	return out, nil
}

// MembershipProofFromJSON converts the relayer's JSON storage proof (the shape
// GetEthMembershipProof and GetEthNonMembershipProof produce) to the protobuf
// MembershipProof the native light client verifies, marshaled.
func MembershipProofFromJSON(raw []byte) ([]byte, error) {
	var in MembershipProof
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode membership proof json: %w", err)
	}
	storageRoot, err := fixedHex("account_proof.storage_root", in.AccountProof.StorageRoot, 32)
	if err != nil {
		return nil, err
	}
	accountNodes, err := hexList("account_proof.proof", in.AccountProof.Proof)
	if err != nil {
		return nil, err
	}
	key, err := fixedHex("storage_proof.key", in.StorageProof.Key, 32)
	if err != nil {
		return nil, err
	}
	value, err := uint256BigEndian(in.StorageProof.Value)
	if err != nil {
		return nil, fmt.Errorf("storage_proof.value: %w", err)
	}
	storageNodes, err := hexList("storage_proof.proof", in.StorageProof.Proof)
	if err != nil {
		return nil, err
	}
	proof := &spectrepb.MembershipProof{
		AccountProof: spectrepb.AccountProof{StorageRoot: storageRoot, Proof: accountNodes},
		StorageProof: spectrepb.StorageProof{Key: key, Value: value, Proof: storageNodes},
	}
	return proof.Marshal()
}

// decodeHex decodes a 0x-prefixed or bare hex string; the empty string is no bytes.
func decodeHex(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return nil, nil
	}
	return hex.DecodeString(s)
}

// fixedHex decodes a hex string that must be exactly want bytes long.
func fixedHex(name, s string, want int) ([]byte, error) {
	b, err := decodeHex(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(b) != want {
		return nil, fmt.Errorf("%s: %d bytes, want %d", name, len(b), want)
	}
	return b, nil
}

func hexList(name string, in []string) ([][]byte, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([][]byte, len(in))
	for i, s := range in {
		b, err := decodeHex(s)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", name, i, err)
		}
		out[i] = b
	}
	return out, nil
}

func hexString(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

func parseUint(name, s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return v, nil
}

// uint256BigEndian converts a decimal or 0x-hex integer to 32 big-endian bytes.
func uint256BigEndian(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	base := 10
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		base = 16
		s = s[2:]
	}
	if s == "" {
		s = "0"
	}
	n, ok := new(big.Int).SetString(s, base)
	if !ok || n.Sign() < 0 || n.BitLen() > 256 {
		return nil, fmt.Errorf("invalid uint256 %q", s)
	}
	out := make([]byte, 32)
	n.FillBytes(out)
	return out, nil
}
