// The beacon API and the SSZ helpers it needs. Split out of ethereum.go because
// the beacon chain is a different counterparty from an EVM JSON-RPC endpoint,
// which is the axis this package names its files on.
package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type EthereumClientState struct {
	ChainID                      uint64         `json:"chain_id"`
	EpochsPerSyncCommitteePeriod uint64         `json:"epochs_per_sync_committee_period"`
	ForkParameters               ForkParameters `json:"fork_parameters"`
	GenesisSlot                  uint64         `json:"genesis_slot"`
	GenesisTime                  uint64         `json:"genesis_time"`
	GenesisValidatorsRoot        string         `json:"genesis_validators_root"`
	IBCCommitmentSlot            string         `json:"ibc_commitment_slot"`
	IbcContractAddress           string         `json:"ibc_contract_address"`
	IsFrozen                     bool           `json:"is_frozen"`
	LatestExecutionBlockNumber   uint64         `json:"latest_execution_block_number"`
	LatestSlot                   uint64         `json:"latest_slot"`
	MinSyncCommitteeParticipants uint64         `json:"min_sync_committee_participants"`
	SecondsPerSlot               uint64         `json:"seconds_per_slot"`
	SlotsPerEpoch                uint64         `json:"slots_per_epoch"`
	SyncCommitteeSize            uint64         `json:"sync_committee_size"`
}

type EthereumConsensusState struct {
	Slot                 uint64                   `json:"slot"`
	StateRoot            string                   `json:"state_root"`
	Timestamp            uint64                   `json:"timestamp"`
	CurrentSyncCommittee SummarizedSyncCommittee  `json:"current_sync_committee"`
	NextSyncCommittee    *SummarizedSyncCommittee `json:"next_sync_committee"`
}

type ForkParameters struct {
	Altair             Fork   `json:"altair"`
	Bellatrix          Fork   `json:"bellatrix"`
	Capella            Fork   `json:"capella"`
	Deneb              Fork   `json:"deneb"`
	Electra            Fork   `json:"electra"`
	Fulu               Fork   `json:"fulu"`
	Gloas              Fork   `json:"gloas"`
	GenesisForkVersion string `json:"genesis_fork_version"`
	GenesisSlot        uint64 `json:"genesis_slot"`
}

type Fork struct {
	Epoch   uint64 `json:"epoch"`
	Version string `json:"version"`
}

func (cs *EthereumClientState) ComputeSyncCommitteePeriodAtSlot(slot uint64) uint64 {
	epoch := slot / cs.SlotsPerEpoch
	return epoch / cs.EpochsPerSyncCommitteePeriod
}

// ComputeSlotAtTimestamp returns the slot number for a given unix timestamp.
func (cs *EthereumClientState) ComputeSlotAtTimestamp(timestamp uint64) uint64 {
	if timestamp < cs.GenesisTime {
		return cs.GenesisSlot
	}
	return cs.GenesisSlot + (timestamp-cs.GenesisTime)/cs.SecondsPerSlot
}

// ComputeTimestampAtSlot returns the unix timestamp for a given slot.
func (cs *EthereumClientState) ComputeTimestampAtSlot(slot uint64) uint64 {
	if slot <= cs.GenesisSlot {
		return cs.GenesisTime
	}
	return cs.GenesisTime + (slot-cs.GenesisSlot)*cs.SecondsPerSlot
}

type SyncCommittee struct {
	Pubkeys         []string `json:"pubkeys"`
	AggregatePubkey string   `json:"aggregate_pubkey"`
}

func (sc *SyncCommittee) ToSummarizedSyncCommittee() (*SummarizedSyncCommittee, error) {
	pks := make([][]byte, 0, len(sc.Pubkeys))
	for _, pk := range sc.Pubkeys {
		pkTrimmed := strings.TrimPrefix(pk, "0x")
		pkBytes, err := hex.DecodeString(pkTrimmed)
		if err != nil {
			return nil, err
		}
		if len(pkBytes) != 48 {
			return nil, fmt.Errorf("expected 48-byte BLS public key, got %d bytes", len(pkBytes))
		}
		pks = append(pks, pkBytes)
	}

	pubkeysHash := sszTreeHashBLSPubkeys(pks)
	return &SummarizedSyncCommittee{
		PubkeysHash:     "0x" + hex.EncodeToString(pubkeysHash[:]),
		AggregatePubkey: sc.AggregatePubkey,
	}, nil
}

// sszTreeHashBLSPubkeys computes the SSZ tree hash root of a Vec<FixedBytes<48>>,
// matching the Rust [FixedBytes<48>]::tree_hash_root() (TreeHashType::Vector — no mix_in_length).
//
// Algorithm:
//  1. Each 48-byte pubkey is hashed as a 2-chunk SSZ fixed-vector:
//     leaf = sha256(pk[0:32] || pk[32:48] || zeros[16])
//  2. Merkleize the leaf hashes (pad to next power of two, compute SHA256 binary tree)
//     No length mixing — Vector type, not List.
func sszTreeHashBLSPubkeys(pubkeys [][]byte) [32]byte {
	// Step 1: leaf hash per pubkey (2-chunk SSZ vector hash)
	leaves := make([][32]byte, len(pubkeys))
	for i, pk := range pubkeys {
		var chunk0, chunk1 [32]byte
		copy(chunk0[:], pk[:32])
		copy(chunk1[:], pk[32:]) // 16 bytes of key + 16 implicit zeros
		h := sha256.New()
		h.Write(chunk0[:])
		h.Write(chunk1[:])
		copy(leaves[i][:], h.Sum(nil))
	}

	// Step 2: merkleize (Vector type — no mix_in_length)
	return sszMerkleize(leaves)
}

func sszMerkleize(chunks [][32]byte) [32]byte {
	if len(chunks) == 0 {
		return [32]byte{}
	}
	// Pad to next power of two
	n := sszNextPowerOfTwo(len(chunks))
	padded := make([][32]byte, n)
	copy(padded, chunks)

	// Iteratively hash pairs until one root remains
	for len(padded) > 1 {
		next := make([][32]byte, len(padded)/2)
		for i := range next {
			h := sha256.New()
			h.Write(padded[2*i][:])
			h.Write(padded[2*i+1][:])
			copy(next[i][:], h.Sum(nil))
		}
		padded = next
	}
	return padded[0]
}

func sszNextPowerOfTwo(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}

type SummarizedSyncCommittee struct {
	PubkeysHash     string `json:"pubkeys_hash"`
	AggregatePubkey string `json:"aggregate_pubkey"`
}

// LightClientHeader is a light client header in either wire format: pre-Gloas headers carry the
// full Execution payload header, Gloas headers only ExecutionBlockHash. Exactly one is set.
type LightClientHeader struct {
	Beacon             BeaconBlockHeader       `json:"beacon"`
	Execution          *ExecutionPayloadHeader `json:"execution,omitempty"`
	ExecutionBlockHash string                  `json:"execution_block_hash,omitempty"`
	ExecutionBranch    []string                `json:"execution_branch"`
}

type BeaconBlockHeader struct {
	Slot          string `json:"slot"`
	ProposerIndex string `json:"proposer_index"`
	ParentRoot    string `json:"parent_root"`
	StateRoot     string `json:"state_root"`
	BodyRoot      string `json:"body_root"`
}

type ExecutionPayloadHeader struct {
	ParentHash       string `json:"parent_hash"`
	FeeRecipient     string `json:"fee_recipient"`
	StateRoot        string `json:"state_root"`
	ReceiptsRoot     string `json:"receipts_root"`
	LogsBloom        string `json:"logs_bloom"`
	PrevRandao       string `json:"prev_randao"`
	BlockNumber      string `json:"block_number"`
	GasLimit         string `json:"gas_limit"`
	GasUsed          string `json:"gas_used"`
	Timestamp        string `json:"timestamp"`
	ExtraData        string `json:"extra_data"`
	BaseFeePerGas    string `json:"base_fee_per_gas"`
	BlockHash        string `json:"block_hash"`
	TransactionsRoot string `json:"transactions_root"`
	WithdrawalsRoot  string `json:"withdrawals_root"`
	BlobGasUsed      string `json:"blob_gas_used"`
	ExcessBlobGas    string `json:"excess_blob_gas"`
	// Electra (EIP-7685): hash of the execution requests
	RequestsHash string `json:"requests_hash,omitempty"`
}

type SyncAggregate struct {
	SyncCommitteeBits      string `json:"sync_committee_bits"`
	SyncCommitteeSignature string `json:"sync_committee_signature"`
}

// CountSyncCommitteeParticipants counts the number of set bits in a hex-encoded bitvector.
func CountSyncCommitteeParticipants(bitsHex string) uint64 {
	bitsHex = strings.TrimPrefix(bitsHex, "0x")
	var count uint64
	for _, c := range bitsHex {
		var nibble uint64
		switch {
		case c >= '0' && c <= '9':
			nibble = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			nibble = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			nibble = uint64(c-'A') + 10
		}
		for nibble != 0 {
			count += nibble & 1
			nibble >>= 1
		}
	}
	return count
}

type LightClientUpdate struct {
	AttestedHeader          LightClientHeader `json:"attested_header"`
	NextSyncCommittee       *SyncCommittee    `json:"next_sync_committee"`
	NextSyncCommitteeBranch []string          `json:"next_sync_committee_branch"`
	FinalizedHeader         LightClientHeader `json:"finalized_header"`
	FinalityBranch          []string          `json:"finality_branch"`
	SyncAggregate           SyncAggregate     `json:"sync_aggregate"`
	SignatureSlot           string            `json:"signature_slot"`
}

type LightClientFinalityUpdate struct {
	AttestedHeader  LightClientHeader `json:"attested_header"`
	FinalizedHeader LightClientHeader `json:"finalized_header"`
	FinalityBranch  []string          `json:"finality_branch"`
	SyncAggregate   SyncAggregate     `json:"sync_aggregate"`
	SignatureSlot   string            `json:"signature_slot"`
}

type ActiveSyncCommittee struct {
	Current *SyncCommittee `json:"Current,omitempty"`
	Next    *SyncCommittee `json:"Next,omitempty"`
}

type ExecutionPayload struct {
	ParentHash    string `json:"parent_hash"`
	FeeRecipient  string `json:"fee_recipient"`
	StateRoot     string `json:"state_root"`
	ReceiptsRoot  string `json:"receipts_root"`
	LogsBloom     string `json:"logs_bloom"`
	PrevRandao    string `json:"prev_randao"`
	BlockNumber   string `json:"block_number"`
	GasLimit      string `json:"gas_limit"`
	GasUsed       string `json:"gas_used"`
	Timestamp     string `json:"timestamp"`
	ExtraData     string `json:"extra_data"`
	BaseFeePerGas string `json:"base_fee_per_gas"`
	BlockHash     string `json:"block_hash"`
	BlobGasUsed   string `json:"blob_gas_used"`
	ExcessBlobGas string `json:"excess_blob_gas"`
}

type BeaconBlockBody struct {
	SyncAggregate    SyncAggregate    `json:"sync_aggregate"`
	ExecutionPayload ExecutionPayload `json:"execution_payload"`
}

type BeaconBlockMessage struct {
	Slot          string          `json:"slot"`
	ProposerIndex string          `json:"proposer_index"`
	ParentRoot    string          `json:"parent_root"`
	StateRoot     string          `json:"state_root"`
	Body          BeaconBlockBody `json:"body"`
}
type BeaconGenesis struct {
	GenesisTime           string `json:"genesis_time"`
	GenesisValidatorsRoot string `json:"genesis_validators_root"`
	GenesisForkstring     string `json:"genesis_fork_string"`
}
type BeaconSpec struct {
	// Identity
	ConfigName string `json:"CONFIG_NAME"`
	PresetBase string `json:"PRESET_BASE"`

	// Genesis
	MinGenesisActiveValidatorCount string `json:"MIN_GENESIS_ACTIVE_VALIDATOR_COUNT"`
	MinGenesisTime                 string `json:"MIN_GENESIS_TIME"`
	GenesisDelay                   string `json:"GENESIS_DELAY"`

	// Fork Versions & Epochs
	GenesisForkVersion   string `json:"GENESIS_FORK_VERSION"`
	AltairForkVersion    string `json:"ALTAIR_FORK_VERSION"`
	AltairForkEpoch      string `json:"ALTAIR_FORK_EPOCH"`
	BellatrixForkVersion string `json:"BELLATRIX_FORK_VERSION"`
	BellatrixForkEpoch   string `json:"BELLATRIX_FORK_EPOCH"`
	CapellaForkVersion   string `json:"CAPELLA_FORK_VERSION"`
	CapellaForkEpoch     string `json:"CAPELLA_FORK_EPOCH"`
	DenebForkVersion     string `json:"DENEB_FORK_VERSION"`
	DenebForkEpoch       string `json:"DENEB_FORK_EPOCH"`
	ElectraForkVersion   string `json:"ELECTRA_FORK_VERSION"`
	ElectraForkEpoch     string `json:"ELECTRA_FORK_EPOCH"`
	FuluForkVersion      string `json:"FULU_FORK_VERSION"`
	FuluForkEpoch        string `json:"FULU_FORK_EPOCH"`
	GloasForkVersion     string `json:"GLOAS_FORK_VERSION"`
	GloasForkEpoch       string `json:"GLOAS_FORK_EPOCH"`

	// Time
	SecondsPerSlot      string `json:"SECONDS_PER_SLOT"`
	SecondsPerEth1Block string `json:"SECONDS_PER_ETH1_BLOCK"`

	// Deposit Contract
	DepositChainID         string `json:"DEPOSIT_CHAIN_ID"`
	DepositNetworkID       string `json:"DEPOSIT_NETWORK_ID"`
	DepositContractAddress string `json:"DEPOSIT_CONTRACT_ADDRESS"`

	// Validator / Committee
	SlotsPerEpoch                    string `json:"SLOTS_PER_EPOCH"`
	MaxCommitteesPerSlot             string `json:"MAX_COMMITTEES_PER_SLOT"`
	TargetCommitteeSize              string `json:"TARGET_COMMITTEE_SIZE"`
	MaxValidatorsPerCommittee        string `json:"MAX_VALIDATORS_PER_COMMITTEE"`
	ShuffleRoundCount                string `json:"SHUFFLE_ROUND_COUNT"`
	MinSeedLookahead                 string `json:"MIN_SEED_LOOKAHEAD"`
	MaxSeedLookahead                 string `json:"MAX_SEED_LOOKAHEAD"`
	EpochsPerEth1VotingPeriod        string `json:"EPOCHS_PER_ETH1_VOTING_PERIOD"`
	SlotsPerHistoricalRoot           string `json:"SLOTS_PER_HISTORICAL_ROOT"`
	MinEpochsToInactivityPenalty     string `json:"MIN_EPOCHS_TO_INACTIVITY_PENALTY"`
	EpochsPerHistoricalVector        string `json:"EPOCHS_PER_HISTORICAL_VECTOR"`
	EpochsPerSlashingsVector         string `json:"EPOCHS_PER_SLASHINGS_VECTOR"`
	HistoricalRootsLimit             string `json:"HISTORICAL_ROOTS_LIMIT"`
	ValidatorRegistryLimit           string `json:"VALIDATOR_REGISTRY_LIMIT"`
	MinValidatorWithdrawabilityDelay string `json:"MIN_VALIDATOR_WITHDRAWABILITY_DELAY"`
	ShardCommitteePeriod             string `json:"SHARD_COMMITTEE_PERIOD"`
	EjectionBalance                  string `json:"EJECTION_BALANCE"`
	MinPerEpochChurnLimit            string `json:"MIN_PER_EPOCH_CHURN_LIMIT"`
	MaxPerEpochActivationChurnLimit  string `json:"MAX_PER_EPOCH_ACTIVATION_CHURN_LIMIT"`
	ChurnLimitQuotient               string `json:"CHURN_LIMIT_QUOTIENT"`

	// Balances
	MinDepositAmount                 string `json:"MIN_DEPOSIT_AMOUNT"`
	MaxEffectiveBalance              string `json:"MAX_EFFECTIVE_BALANCE"`
	EffectiveBalanceIncrement        string `json:"EFFECTIVE_BALANCE_INCREMENT"`
	MinActivationBalance             string `json:"MIN_ACTIVATION_BALANCE"`
	MaxEffectiveBalanceElectra       string `json:"MAX_EFFECTIVE_BALANCE_ELECTRA"`
	BalancePerAdditionalCustodyGroup string `json:"BALANCE_PER_ADDITIONAL_CUSTODY_GROUP"`

	// Rewards & Penalties
	BaseRewardFactor                        string `json:"BASE_REWARD_FACTOR"`
	WhistleblowerRewardQuotient             string `json:"WHISTLEBLOWER_REWARD_QUOTIENT"`
	ProposerRewardQuotient                  string `json:"PROPOSER_REWARD_QUOTIENT"`
	InactivityPenaltyQuotient               string `json:"INACTIVITY_PENALTY_QUOTIENT"`
	MinSlashingPenaltyQuotient              string `json:"MIN_SLASHING_PENALTY_QUOTIENT"`
	ProportionalSlashingMultiplier          string `json:"PROPORTIONAL_SLASHING_MULTIPLIER"`
	InactivityPenaltyQuotientAltair         string `json:"INACTIVITY_PENALTY_QUOTIENT_ALTAIR"`
	MinSlashingPenaltyQuotientAltair        string `json:"MIN_SLASHING_PENALTY_QUOTIENT_ALTAIR"`
	ProportionalSlashingMultiplierAltair    string `json:"PROPORTIONAL_SLASHING_MULTIPLIER_ALTAIR"`
	InactivityPenaltyQuotientBellatrix      string `json:"INACTIVITY_PENALTY_QUOTIENT_BELLATRIX"`
	MinSlashingPenaltyQuotientBellatrix     string `json:"MIN_SLASHING_PENALTY_QUOTIENT_BELLATRIX"`
	ProportionalSlashingMultiplierBellatrix string `json:"PROPORTIONAL_SLASHING_MULTIPLIER_BELLATRIX"`
	MinSlashingPenaltyQuotientElectra       string `json:"MIN_SLASHING_PENALTY_QUOTIENT_ELECTRA"`
	WhistleblowerRewardQuotientElectra      string `json:"WHISTLEBLOWER_REWARD_QUOTIENT_ELECTRA"`
	InactivityScoreBias                     string `json:"INACTIVITY_SCORE_BIAS"`
	InactivityScoreRecoveryRate             string `json:"INACTIVITY_SCORE_RECOVERY_RATE"`

	// Sync Committee
	SyncCommitteeSize                    string `json:"SYNC_COMMITTEE_SIZE"`
	EpochsPerSyncCommitteePeriod         string `json:"EPOCHS_PER_SYNC_COMMITTEE_PERIOD"`
	MinSyncCommitteeParticipants         string `json:"MIN_SYNC_COMMITTEE_PARTICIPANTS"`
	SyncCommitteeSubnetCount             string `json:"SYNC_COMMITTEE_SUBNET_COUNT"`
	TargetAggregatorsPerSyncSubcommittee string `json:"TARGET_AGGREGATORS_PER_SYNC_SUBCOMMITTEE"`
	TargetAggregatorsPerCommittee        string `json:"TARGET_AGGREGATORS_PER_COMMITTEE"`

	// Blobs & EIP-4844 / Deneb
	MaxBlobsPerBlock                 string `json:"MAX_BLOBS_PER_BLOCK"`
	MaxBlobCommitmentsPerBlock       string `json:"MAX_BLOB_COMMITMENTS_PER_BLOCK"`
	FieldElementsPerBlob             string `json:"FIELD_ELEMENTS_PER_BLOB"`
	BlobSidecarSubnetCount           string `json:"BLOB_SIDECAR_SUBNET_COUNT"`
	MaxRequestBlobSidecars           string `json:"MAX_REQUEST_BLOB_SIDECARS"`
	MinEpochsForBlobSidecarsRequests string `json:"MIN_EPOCHS_FOR_BLOB_SIDECARS_REQUESTS"`

	// Electra Blobs
	MaxBlobsPerBlockElectra       string `json:"MAX_BLOBS_PER_BLOCK_ELECTRA"`
	BlobSidecarSubnetCountElectra string `json:"BLOB_SIDECAR_SUBNET_COUNT_ELECTRA"`
	MaxRequestBlobSidecarsElectra string `json:"MAX_REQUEST_BLOB_SIDECARS_ELECTRA"`

	// PeerDAS / Data Columns
	NumberOfColumns                        string `json:"NUMBER_OF_COLUMNS"`
	NumberOfCustodyGroups                  string `json:"NUMBER_OF_CUSTODY_GROUPS"`
	DataColumnSidecarSubnetCount           string `json:"DATA_COLUMN_SIDECAR_SUBNET_COUNT"`
	SamplesPerSlot                         string `json:"SAMPLES_PER_SLOT"`
	CustodyRequirement                     string `json:"CUSTODY_REQUIREMENT"`
	ValidatorCustodyRequirement            string `json:"VALIDATOR_CUSTODY_REQUIREMENT"`
	MaxRequestDataColumnSidecars           string `json:"MAX_REQUEST_DATA_COLUMN_SIDECARS"`
	MinEpochsForDataColumnSidecarsRequests string `json:"MIN_EPOCHS_FOR_DATA_COLUMN_SIDECARS_REQUESTS"`
	FieldElementsPerCell                   string `json:"FIELD_ELEMENTS_PER_CELL"`
	FieldElementsPerExtBlob                string `json:"FIELD_ELEMENTS_PER_EXT_BLOB"`
	KzgCommitmentsInclusionProofDepth      string `json:"KZG_COMMITMENTS_INCLUSION_PROOF_DEPTH"`

	// Electra Limits
	PendingDepositsLimit                  string `json:"PENDING_DEPOSITS_LIMIT"`
	PendingPartialWithdrawalsLimit        string `json:"PENDING_PARTIAL_WITHDRAWALS_LIMIT"`
	PendingConsolidationsLimit            string `json:"PENDING_CONSOLIDATIONS_LIMIT"`
	MaxAttesterSlashingsElectra           string `json:"MAX_ATTESTER_SLASHINGS_ELECTRA"`
	MaxAttestationsElectra                string `json:"MAX_ATTESTATIONS_ELECTRA"`
	MaxDepositRequestsPerPayload          string `json:"MAX_DEPOSIT_REQUESTS_PER_PAYLOAD"`
	MaxWithdrawalRequestsPerPayload       string `json:"MAX_WITHDRAWAL_REQUESTS_PER_PAYLOAD"`
	MaxConsolidationRequestsPerPayload    string `json:"MAX_CONSOLIDATION_REQUESTS_PER_PAYLOAD"`
	MaxPendingPartialsPerWithdrawalsSweep string `json:"MAX_PENDING_PARTIALS_PER_WITHDRAWALS_SWEEP"`
	MaxPendingDepositsPerEpoch            string `json:"MAX_PENDING_DEPOSITS_PER_EPOCH"`
	MinPerEpochChurnLimitElectra          string `json:"MIN_PER_EPOCH_CHURN_LIMIT_ELECTRA"`
	MaxPerEpochActivationExitChurnLimit   string `json:"MAX_PER_EPOCH_ACTIVATION_EXIT_CHURN_LIMIT"`
	UnsetDepositRequestsStartIndex        string `json:"UNSET_DEPOSIT_REQUESTS_START_INDEX"`
	FullExitRequestAmount                 string `json:"FULL_EXIT_REQUEST_AMOUNT"`

	// Execution / Payload
	MaxBytesPerTransaction           string `json:"MAX_BYTES_PER_TRANSACTION"`
	MaxTransactionsPerPayload        string `json:"MAX_TRANSACTIONS_PER_PAYLOAD"`
	BytesPerLogsBloom                string `json:"BYTES_PER_LOGS_BLOOM"`
	MaxExtraDataBytes                string `json:"MAX_EXTRA_DATA_BYTES"`
	MaxWithdrawalsPerPayload         string `json:"MAX_WITHDRAWALS_PER_PAYLOAD"`
	MaxValidatorsPerWithdrawalsSweep string `json:"MAX_VALIDATORS_PER_WITHDRAWALS_SWEEP"`
	MaxBlsToExecutionChanges         string `json:"MAX_BLS_TO_EXECUTION_CHANGES"`
	GasLimitAdjustmentFactor         string `json:"GAS_LIMIT_ADJUSTMENT_FACTOR"`
	MaxPayloadSize                   string `json:"MAX_PAYLOAD_SIZE"`

	// Network
	MaxRequestBlocks                  string `json:"MAX_REQUEST_BLOCKS"`
	MaxRequestBlocksDeneb             string `json:"MAX_REQUEST_BLOCKS_DENEB"`
	MinEpochsForBlockRequests         string `json:"MIN_EPOCHS_FOR_BLOCK_REQUESTS"`
	Eth1FollowDistance                string `json:"ETH1_FOLLOW_DISTANCE"`
	SubnetsPerNode                    string `json:"SUBNETS_PER_NODE"`
	AttestationSubnetPrefixBits       string `json:"ATTESTATION_SUBNET_PREFIX_BITS"`
	TtfbTimeout                       string `json:"TTFB_TIMEOUT"`
	RespTimeout                       string `json:"RESP_TIMEOUT"`
	AttestationPropagationSlotRange   string `json:"ATTESTATION_PROPAGATION_SLOT_RANGE"`
	MaximumGossipClockDisparityMillis string `json:"MAXIMUM_GOSSIP_CLOCK_DISPARITY_MILLIS"`
	ProposerScoreBoost                string `json:"PROPOSER_SCORE_BOOST"`

	// Operation Limits
	MaxProposerSlashings         string `json:"MAX_PROPOSER_SLASHINGS"`
	MaxAttesterSlashings         string `json:"MAX_ATTESTER_SLASHINGS"`
	MaxAttestations              string `json:"MAX_ATTESTATIONS"`
	MaxDeposits                  string `json:"MAX_DEPOSITS"`
	MaxVoluntaryExits            string `json:"MAX_VOLUNTARY_EXITS"`
	MinAttestationInclusionDelay string `json:"MIN_ATTESTATION_INCLUSION_DELAY"`
	HysteresisQuotient           string `json:"HYSTERESIS_QUOTIENT"`
	HysteresisDownwardMultiplier string `json:"HYSTERESIS_DOWNWARD_MULTIPLIER"`
	HysteresisUpwardMultiplier   string `json:"HYSTERESIS_UPWARD_MULTIPLIER"`

	// Domains
	DomainBeaconProposer              string `json:"DOMAIN_BEACON_PROPOSER"`
	DomainBeaconAttester              string `json:"DOMAIN_BEACON_ATTESTER"`
	DomainRandao                      string `json:"DOMAIN_RANDAO"`
	DomainDeposit                     string `json:"DOMAIN_DEPOSIT"`
	DomainVoluntaryExit               string `json:"DOMAIN_VOLUNTARY_EXIT"`
	DomainSelectionProof              string `json:"DOMAIN_SELECTION_PROOF"`
	DomainAggregateAndProof           string `json:"DOMAIN_AGGREGATE_AND_PROOF"`
	DomainSyncCommittee               string `json:"DOMAIN_SYNC_COMMITTEE"`
	DomainSyncCommitteeSelectionProof string `json:"DOMAIN_SYNC_COMMITTEE_SELECTION_PROOF"`
	DomainContributionAndProof        string `json:"DOMAIN_CONTRIBUTION_AND_PROOF"`
	DomainApplicationMask             string `json:"DOMAIN_APPLICATION_MASK"`

	// Prefixes & Versioning
	BLSWithdrawalPrefix         string `json:"BLS_WITHDRAWAL_PREFIX"`
	Eth1AddressWithdrawalPrefix string `json:"ETH1_ADDRESS_WITHDRAWAL_PREFIX"`
	CompoundingWithdrawalPrefix string `json:"COMPOUNDING_WITHDRAWAL_PREFIX"`
	VersionedHashVersionKZG     string `json:"VERSIONED_HASH_VERSION_KZG"`

	// Message Domains
	MessageDomainInvalidSnappy string `json:"MESSAGE_DOMAIN_INVALID_SNAPPY"`
	MessageDomainValidSnappy   string `json:"MESSAGE_DOMAIN_VALID_SNAPPY"`

	// Terminal / Merge
	TerminalTotalDifficulty          string `json:"TERMINAL_TOTAL_DIFFICULTY"`
	TerminalBlockHash                string `json:"TERMINAL_BLOCK_HASH"`
	TerminalBlockHashActivationEpoch string `json:"TERMINAL_BLOCK_HASH_ACTIVATION_EPOCH"`

	// Blob Schedule (Fulu)
	BlobSchedule []BlobScheduleEntry `json:"BLOB_SCHEDULE"`
}

type BlobScheduleEntry struct {
	Epoch            string `json:"EPOCH"`
	MaxBlobsPerBlock string `json:"MAX_BLOBS_PER_BLOCK"`
}
type BeaconBlock struct {
	Message   BeaconBlockMessage `json:"message"`
	Signature string             `json:"signature"`
}

type EthereumHeader struct {
	ActiveSyncCommittee ActiveSyncCommittee `json:"active_sync_committee"`
	ConsensusUpdate     LightClientUpdate   `json:"consensus_update"`
	TrustedSlot         uint64              `json:"trusted_slot"`
	// ExecutionHeaderRLP is the hex RLP execution block header of the finalized header. Set
	// exactly when that header is in the Gloas format; see ResolveExecution.
	ExecutionHeaderRLP string `json:"execution_header_rlp,omitempty"`
}

type FinalityUpdateResponse struct {
	Version string                    `json:"version"`
	Data    LightClientFinalityUpdate `json:"data"`
}

type LightClientUpdateResponse struct {
	Data LightClientUpdate `json:"data"`
}

type BeaconGenesisResponse struct {
	Data BeaconGenesis `json:"data"`
}

type BeaconSpecResponse struct {
	Data BeaconSpec `json:"data"`
}

type BeaconBlockResponse struct {
	Version             string      `json:"version"`
	ExecutionOptimistic bool        `json:"execution_optimistic"`
	Finalized           bool        `json:"finalized"`
	Data                BeaconBlock `json:"data"`
}

type BootstrapResponse struct {
	Data struct {
		Header                     LightClientHeader `json:"header"`
		CurrentSyncCommittee       SyncCommittee     `json:"current_sync_committee"`
		CurrentSyncCommitteeBranch []string          `json:"current_sync_committee_branch"`
	} `json:"data"`
}

type BeaconBlockRootResponse struct {
	Data struct {
		Root string `json:"root"`
	} `json:"data"`
}

var beaconHttpClient = &http.Client{
	Timeout: 30 * time.Second,
}

func httpGet[T any](ctx context.Context, url string) (T, error) {
	var result T

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := beaconHttpClient.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result, err
	}

	if resp.StatusCode != 200 {
		return result, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, body)
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return result, err
	}

	return result, nil
}

func GetFinalityUpdate(ctx context.Context, beaconAPIURL string) (*LightClientFinalityUpdate, error) {
	url := fmt.Sprintf("%s/eth/v1/beacon/light_client/finality_update", beaconAPIURL)
	response, err := httpGet[FinalityUpdateResponse](ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get finality update: %w", err)
	}
	return &response.Data, nil
}

func GetLightClientUpdates(ctx context.Context, beaconAPIURL string, startPeriod, count uint64) ([]LightClientUpdate, error) {
	url := fmt.Sprintf("%s/eth/v1/beacon/light_client/updates?start_period=%d&count=%d", beaconAPIURL, startPeriod, count)
	responses, err := httpGet[[]LightClientUpdateResponse](ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get light client updates: %w", err)
	}

	updates := make([]LightClientUpdate, len(responses))
	for i, r := range responses {
		updates[i] = r.Data
	}
	return updates, nil
}

func GetBeaconBlockRoot(ctx context.Context, beaconAPIURL, blockID string) (string, error) {
	url := fmt.Sprintf("%s/eth/v1/beacon/blocks/%s/root", beaconAPIURL, blockID)
	response, err := httpGet[BeaconBlockRootResponse](ctx, url)
	if err != nil {
		return "", fmt.Errorf("failed to get beacon block root: %w", err)
	}
	return response.Data.Root, nil
}

func GetBeaconGenesis(ctx context.Context, beaconAPIURL string) (*BeaconGenesis, error) {
	url := fmt.Sprintf("%s/eth/v1/beacon/genesis", beaconAPIURL)
	response, err := httpGet[BeaconGenesisResponse](ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get beacon genesis: %w", err)
	}
	return &response.Data, nil
}

func GetBeaconSpec(ctx context.Context, beaconAPIURL string) (*BeaconSpec, error) {
	url := fmt.Sprintf("%s/eth/v1/config/spec", beaconAPIURL)
	response, err := httpGet[BeaconSpecResponse](ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get beacon spec: %w", err)
	}
	return &response.Data, nil
}

func GetBeaconBlock(ctx context.Context, beaconAPIURL string, blockID string) (*BeaconBlock, error) {
	url := fmt.Sprintf("%s/eth/v2/beacon/blocks/%s", beaconAPIURL, blockID)
	response, err := httpGet[BeaconBlockResponse](ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get beacon block: %w", err)
	}
	return &response.Data, nil
}

func GetLightClientBootstrap(ctx context.Context, beaconAPIURL, blockRoot string) (*BootstrapResponse, error) {
	url := fmt.Sprintf("%s/eth/v1/beacon/light_client/bootstrap/%s", beaconAPIURL, blockRoot)
	response, err := httpGet[BootstrapResponse](ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get light client bootstrap: %w", err)
	}
	return &response, nil
}

// ToForkParameters maps the beacon spec into the client-state fork schedule. Every
// fork (through Gloas) is passed through with its own version+epoch; the light client
// picks the active version per header via compute_fork_version, so no head epoch is
// needed here.
func (s *BeaconSpec) ToForkParameters() (*ForkParameters, error) {
	altairForkEpoch, err := strconv.ParseUint(s.AltairForkEpoch, 10, 64)
	if err != nil {
		return nil, err
	}
	bellatrixForkEpoch, err := strconv.ParseUint(s.BellatrixForkEpoch, 10, 64)
	if err != nil {
		return nil, err
	}
	capellaForkEpoch, err := strconv.ParseUint(s.CapellaForkEpoch, 10, 64)
	if err != nil {
		return nil, err
	}
	denebForkEpoch, err := strconv.ParseUint(s.DenebForkEpoch, 10, 64)
	if err != nil {
		return nil, err
	}
	electraForkEpoch, err := strconv.ParseUint(s.ElectraForkEpoch, 10, 64)
	if err != nil {
		return nil, err
	}
	// Fulu (Fusaka) fork. A chain that has not scheduled Fulu omits FULU_FORK_* from
	// its beacon spec; map that to epoch ^uint64(0) so the light client's
	// compute_fork_version never selects the Fulu slot and Electra stays active.
	fuluForkEpoch := ^uint64(0)
	if s.FuluForkEpoch != "" {
		fuluForkEpoch, err = strconv.ParseUint(s.FuluForkEpoch, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	// The Rust client's Version is a fixed 4-byte hex, so an empty version string
	// fails to deserialize the whole client state. A pre-Fulu beacon spec omits
	// FULU_FORK_VERSION, so emit the zero version — the slot is never selected anyway
	// (epoch = ^uint64(0)), mirroring the Rust fork_not_scheduled() sentinel.
	fuluForkVersion := s.FuluForkVersion
	if fuluForkVersion == "" {
		fuluForkVersion = "0x00000000"
	}
	// Gloas (Glamsterdam) fork, same convention as Fulu: a spec that omits GLOAS_FORK_* maps to
	// epoch ^uint64(0) and the zero version, so the light client never selects it.
	gloasForkEpoch := ^uint64(0)
	if s.GloasForkEpoch != "" {
		gloasForkEpoch, err = strconv.ParseUint(s.GloasForkEpoch, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	gloasForkVersion := s.GloasForkVersion
	if gloasForkVersion == "" {
		gloasForkVersion = "0x00000000"
	}
	return &ForkParameters{
		GenesisForkVersion: s.GenesisForkVersion,
		GenesisSlot:        0,
		Altair: Fork{
			Version: s.AltairForkVersion,
			Epoch:   altairForkEpoch,
		},
		Bellatrix: Fork{
			Version: s.BellatrixForkVersion,
			Epoch:   bellatrixForkEpoch,
		},
		Capella: Fork{
			Version: s.CapellaForkVersion,
			Epoch:   capellaForkEpoch,
		},
		Deneb: Fork{
			Version: s.DenebForkVersion,
			Epoch:   denebForkEpoch,
		},
		Electra: Fork{
			Version: s.ElectraForkVersion,
			Epoch:   electraForkEpoch,
		},
		Fulu: Fork{
			Version: fuluForkVersion,
			Epoch:   fuluForkEpoch,
		},
		Gloas: Fork{
			Version: gloasForkVersion,
			Epoch:   gloasForkEpoch,
		},
	}, nil
}

// MembershipProof is the JSON format expected by the wasm ETH light client's verify_membership.
// See packages/ethereum/light-client/src/membership.rs.
type MembershipProof struct {
	AccountProof accountProofData `json:"account_proof"`
	StorageProof storageProofData `json:"storage_proof"`
}

type accountProofData struct {
	StorageRoot string   `json:"storage_root"`
	Proof       []string `json:"proof"`
}

type storageProofData struct {
	Key   string   `json:"key"`
	Value string   `json:"value"`
	Proof []string `json:"proof"`
}

// internal types for eth_getProof JSON response
type ethProofResult struct {
	AccountProof []string          `json:"accountProof"`
	StorageHash  ethcommon.Hash    `json:"storageHash"`
	StorageProof []ethStorageProof `json:"storageProof"`
}

type ethStorageProof struct {
	Key   ethcommon.Hash `json:"key"`
	Value *hexutil.Big   `json:"value"`
	Proof []string       `json:"proof"`
}

// RawEvmProof is a decoded eth_getProof result: the account proof MPT nodes and,
// for each requested storage key, its value + storage proof nodes — all as raw
// bytes. Used by the L2 header builder, which needs the exact MPT witness the L2
// wasm verifier RLP-decodes (not the hex-string JSON MembershipProof shape).
type RawEvmProof struct {
	AccountProof [][]byte
	StorageHash  ethcommon.Hash
	Storage      []RawStorageProof
}

// RawStorageProof is one storage slot's decoded proof.
type RawStorageProof struct {
	Key   ethcommon.Hash
	Value []byte // minimal big-endian bytes (Solidity storage value)
	Proof [][]byte
}

// L2BootstrapState is the trusted bootstrap state read directly from an L2 RPC to
// seed an L2 wasm light client's initial consensus state: the execution state
// root, the IBC-handler account's storage root, plus the block height and
// timestamp. (Development-phase: these are read from the L2 node as trusted input;
// production bootstrap will additionally verify them against L1 rollup proofs.)
type L2BootstrapState struct {
	Height            uint64
	BlockHash         ethcommon.Hash
	ParentHash        ethcommon.Hash
	StateRoot         ethcommon.Hash
	RouterStorageRoot ethcommon.Hash
	TimestampSeconds  uint64
}
