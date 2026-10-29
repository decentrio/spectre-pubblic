package services

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	misbehaviourContract "relayer/bindings/Misbehaviour"
	updateclientContract "relayer/bindings/UpdateClient"
	relayerclient "relayer/client"
	"relayer/prover"
	"relayer/prover/witness"

	cmttypes "github.com/cometbft/cometbft/types"
	tenderminttypes "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"
)

type misbehaviourHeaderCandidate struct {
	height     uint64
	blockHash  [32]byte
	lightBlock *relayerclient.LightBlock
	candidates []witness.ValidatorSignature
}

type cosmosMisbehaviourEvidence struct {
	header1 *relayerclient.LightBlock
	header2 *relayerclient.LightBlock
	sigs1   []witness.ValidatorSignature
	sigs2   []witness.ValidatorSignature
}

// PreparedCosmosMisbehaviour is the proof-backed message ready for either
// direct submission or wrapping as router/SpectreClient calldata for a dry run.
type PreparedCosmosMisbehaviour struct {
	EncodedMessage []byte
	Height         uint64
	BlockHash1     [32]byte
	BlockHash2     [32]byte
}

func misbehaviourHeaderTimestampNanos(
	stdCtx context.Context,
	evm EVMEndpoint,
	fetchTimeout time.Duration,
) (*big.Int, error) {
	queryCtx, cancel := fetchCtx(stdCtx, fetchTimeout)
	defer cancel()
	header, err := evm.EthClient().HeaderByNumber(queryCtx, nil)
	if err != nil {
		return nil, err
	}
	if header == nil || header.Time == 0 {
		return nil, fmt.Errorf("latest ethereum header is nil or has zero timestamp")
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(header.Time), big.NewInt(1_000_000_000)), nil
}

func misbehaviourCandidateFromLightBlock(lightBlock *relayerclient.LightBlock, chainID string, maxBucket int) (misbehaviourHeaderCandidate, error) {
	if lightBlock == nil {
		return misbehaviourHeaderCandidate{}, fmt.Errorf("light block is nil")
	}
	if lightBlock.BlockHeight < 0 {
		return misbehaviourHeaderCandidate{}, fmt.Errorf("negative light block height %d", lightBlock.BlockHeight)
	}
	if lightBlock.SignedHeader.Commit == nil {
		return misbehaviourHeaderCandidate{}, fmt.Errorf("commit is nil")
	}
	if len(lightBlock.SignedHeader.Commit.BlockID.Hash) == 0 {
		return misbehaviourHeaderCandidate{}, fmt.Errorf("commit block hash is empty")
	}

	extracted, err := ExtractValidatorSignatures(lightBlock, chainID, maxBucket)
	if err != nil {
		return misbehaviourHeaderCandidate{}, err
	}
	return misbehaviourHeaderCandidate{
		height:     uint64(lightBlock.BlockHeight),
		blockHash:  bytesToBytes32(lightBlock.SignedHeader.Commit.BlockID.Hash),
		lightBlock: lightBlock,
		candidates: extracted.Candidates,
	}, nil
}

func misbehaviourEvidenceFromCandidates(
	first misbehaviourHeaderCandidate,
	second misbehaviourHeaderCandidate,
	firstPinned pinnedCosmosValidatorSet,
	secondPinned pinnedCosmosValidatorSet,
	maxBucket int,
) (*cosmosMisbehaviourEvidence, error) {
	if first.height != second.height {
		return nil, fmt.Errorf("conflicting header heights differ: %d != %d", first.height, second.height)
	}
	if first.blockHash == second.blockHash {
		return nil, fmt.Errorf("misbehaviour not detected: block hashes are equal at height %d", first.height)
	}
	firstSelected, err := selectSignaturesForPinnedSet(first.candidates, firstPinned, maxBucket)
	if err != nil {
		return nil, fmt.Errorf("header1 pinned-set quorum: %w", err)
	}
	secondSelected, err := selectSignaturesForPinnedSet(second.candidates, secondPinned, maxBucket)
	if err != nil {
		return nil, fmt.Errorf("header2 pinned-set quorum: %w", err)
	}
	return &cosmosMisbehaviourEvidence{
		header1: first.lightBlock,
		header2: second.lightBlock,
		sigs1:   firstSelected,
		sigs2:   secondSelected,
	}, nil
}

func lightBlockFromTendermintHeader(header *tenderminttypes.Header) (*relayerclient.LightBlock, error) {
	if header == nil {
		return nil, fmt.Errorf("header is nil")
	}
	signedHeader, err := cmttypes.SignedHeaderFromProto(header.SignedHeader)
	if err != nil {
		return nil, fmt.Errorf("signed header: %w", err)
	}
	validatorSet, err := cmttypes.ValidatorSetFromProto(header.ValidatorSet)
	if err != nil {
		return nil, fmt.Errorf("validator set: %w", err)
	}
	if signedHeader.Header == nil {
		return nil, fmt.Errorf("signed header is missing its header")
	}
	return &relayerclient.LightBlock{
		SignedHeader: *signedHeader,
		ValSet:       *validatorSet,
		BlockHeight:  signedHeader.Height,
	}, nil
}

func pinnedSetFromTrustedValidators(
	header *tenderminttypes.Header,
	trustedLightBlock *relayerclient.LightBlock,
) (pinnedCosmosValidatorSet, error) {
	if header == nil || header.TrustedValidators == nil {
		return pinnedCosmosValidatorSet{}, fmt.Errorf("trusted validator set is missing")
	}
	validatorSet, err := cmttypes.ValidatorSetFromProto(header.TrustedValidators)
	if err != nil {
		return pinnedCosmosValidatorSet{}, fmt.Errorf("trusted validator set: %w", err)
	}
	if trustedLightBlock == nil || trustedLightBlock.SignedHeader.Header == nil {
		return pinnedCosmosValidatorSet{}, fmt.Errorf("trusted light block is missing")
	}
	if !bytes.Equal(validatorSet.Hash(), trustedLightBlock.SignedHeader.NextValidatorsHash) {
		return pinnedCosmosValidatorSet{}, fmt.Errorf(
			"trusted validator hash %X does not match trusted consensus nextValidatorsHash %X",
			validatorSet.Hash(), trustedLightBlock.SignedHeader.NextValidatorsHash,
		)
	}

	indices := make([]uint32, len(validatorSet.Validators))
	pubkeys := make([][32]byte, len(validatorSet.Validators))
	votingPowers := make([]uint64, len(validatorSet.Validators))
	var totalPower int64
	for i, validator := range validatorSet.Validators {
		if validator == nil || validator.PubKey == nil {
			return pinnedCosmosValidatorSet{}, fmt.Errorf("trusted validator %d has no public key", i)
		}
		key := validator.PubKey.Bytes()
		if len(key) != 32 {
			return pinnedCosmosValidatorSet{}, fmt.Errorf("trusted validator %d public key length is %d, want 32", i, len(key))
		}
		if validator.VotingPower <= 0 {
			return pinnedCosmosValidatorSet{}, fmt.Errorf("trusted validator %d has non-positive voting power %d", i, validator.VotingPower)
		}
		indices[i] = uint32(i)
		pubkeys[i] = bytesToBytes32(key)
		votingPowers[i] = uint64(validator.VotingPower)
		totalPower += validator.VotingPower
	}
	return pinnedCosmosValidatorSet{
		indices: indices, pubkeys: pubkeys, votingPowers: votingPowers, totalPower: totalPower,
	}, nil
}

func trustedLightBlockForHeader(
	stdCtx context.Context,
	cosmos CosmosEndpoint,
	fetchTimeout time.Duration,
	header *tenderminttypes.Header,
) (*relayerclient.LightBlock, error) {
	if header == nil || header.Header == nil {
		return nil, fmt.Errorf("trusted height and header are required")
	}
	height := header.TrustedHeight.RevisionHeight
	if height > uint64(^uint64(0)>>1) {
		return nil, fmt.Errorf("trusted height %d overflows int64", height)
	}
	queryCtx, cancel := fetchCtx(stdCtx, fetchTimeout)
	defer cancel()
	trusted, err := relayerclient.GetLightBlock(queryCtx, cosmos.CosmosClient(), int64(height))
	if err != nil {
		return nil, fmt.Errorf("fetch trusted light block %d: %w", height, err)
	}
	if trusted.BlockHeight != int64(height) {
		return nil, fmt.Errorf("trusted light block height = %d, want %d", trusted.BlockHeight, height)
	}
	if trusted.SignedHeader.ChainID != header.Header.ChainID {
		return nil, fmt.Errorf("trusted light block chain_id %q does not match header chain_id %q", trusted.SignedHeader.ChainID, header.Header.ChainID)
	}
	return trusted, nil
}

// PrepareCosmosMisbehaviour validates standard IBC Tendermint misbehaviour
// evidence, selects proof-backed quorum against each header's trusted validator
// set, and builds the encoded SpectreClient message without broadcasting it.
func (s *Services) PrepareCosmosMisbehaviour(
	stdCtx context.Context,
	cosmos CosmosEndpoint,
	evm EVMEndpoint,
	cosmosRouterClientID string,
	input *tenderminttypes.Misbehaviour,
) (PreparedCosmosMisbehaviour, error) {
	if err := stdCtx.Err(); err != nil {
		return PreparedCosmosMisbehaviour{}, err
	}
	if s == nil || s.worker == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("misbehaviour services are not initialized")
	}
	if input == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("misbehaviour evidence is nil")
	}
	if input.Header1 == nil || input.Header2 == nil || input.Header1.SignedHeader == nil || input.Header2.SignedHeader == nil ||
		input.Header1.SignedHeader.Header == nil || input.Header2.SignedHeader.Header == nil ||
		input.Header1.SignedHeader.Commit == nil || input.Header2.SignedHeader.Commit == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("misbehaviour evidence must contain two complete signed headers")
	}
	if err := input.ValidateBasic(); err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("invalid Tendermint misbehaviour evidence: %w", err)
	}
	if input.ClientId != cosmosRouterClientID {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf(
			"evidence client_id %q does not match configured ics26_client_id %q",
			input.ClientId, cosmosRouterClientID,
		)
	}
	if cosmos.CosmosClient() == nil || evm.EthClient() == nil || evm.SpectreClientContract() == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("Cosmos RPC, EVM RPC, and SpectreClient address are required")
	}
	queryCtx, cancel := fetchCtx(stdCtx, s.cosmosConfig.FetchTimeout)
	clientState, err := fetchOnChainClientState(queryCtx, evm)
	cancel()
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("on-chain client state: %w", err)
	}
	if clientState.IsFrozen {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("SpectreClient is already frozen")
	}
	chainID := input.Header1.Header.ChainID
	if chainID != clientState.ChainId {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("evidence chain_id %q does not match on-chain chain_id %q", chainID, clientState.ChainId)
	}
	if input.Header1.Header.ChainID != input.Header2.Header.ChainID {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("evidence headers have different chain IDs")
	}
	if input.Header1.Header.Height != input.Header2.Header.Height {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf(
			"standalone Spectre misbehaviour requires equal heights: %d != %d",
			input.Header1.Header.Height, input.Header2.Header.Height,
		)
	}
	if bytes.Equal(input.Header1.Commit.BlockID.Hash, input.Header2.Commit.BlockID.Hash) {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("misbehaviour not detected: block hashes are equal")
	}

	firstLightBlock, err := lightBlockFromTendermintHeader(input.Header1)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header1: %w", err)
	}
	secondLightBlock, err := lightBlockFromTendermintHeader(input.Header2)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header2: %w", err)
	}
	trustedFirst, err := trustedLightBlockForHeader(stdCtx, cosmos, s.cosmosConfig.FetchTimeout, input.Header1)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header1: %w", err)
	}
	trustedSecond, err := trustedLightBlockForHeader(stdCtx, cosmos, s.cosmosConfig.FetchTimeout, input.Header2)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header2: %w", err)
	}
	firstPinned, err := pinnedSetFromTrustedValidators(input.Header1, trustedFirst)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header1: %w", err)
	}
	secondPinned, err := pinnedSetFromTrustedValidators(input.Header2, trustedSecond)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header2: %w", err)
	}
	firstCandidate, err := misbehaviourCandidateFromLightBlock(firstLightBlock, chainID, s.worker.Prover.MaxBucket())
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header1 signatures: %w", err)
	}
	secondCandidate, err := misbehaviourCandidateFromLightBlock(secondLightBlock, chainID, s.worker.Prover.MaxBucket())
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("header2 signatures: %w", err)
	}
	evidence, err := misbehaviourEvidenceFromCandidates(firstCandidate, secondCandidate, firstPinned, secondPinned, s.worker.Prover.MaxBucket())
	if err != nil {
		return PreparedCosmosMisbehaviour{}, err
	}
	return s.worker.BuildCosmosMisbehaviour(stdCtx, evm, s.cosmosConfig.FetchTimeout, trustedFirst, trustedSecond, evidence, firstPinned, secondPinned)
}

// SubmitCosmosMisbehaviourEvidence prepares and broadcasts standard IBC
// Tendermint evidence with the dedicated misbehaviour signer.
func (s *Services) SubmitCosmosMisbehaviourEvidence(
	stdCtx context.Context,
	cosmos CosmosEndpoint,
	evm EVMEndpoint,
	cosmosRouterClientID string,
	input *tenderminttypes.Misbehaviour,
) (PreparedCosmosMisbehaviour, error) {
	prepared, err := s.PrepareCosmosMisbehaviour(stdCtx, cosmos, evm, cosmosRouterClientID, input)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, err
	}
	if s.worker.TxHandler == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: nil tx handler")
	}
	if err := s.worker.TxHandler.SubmitMisbehaviour(stdCtx, evm, cosmosRouterClientID, prepared.EncodedMessage); err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: tx: %w", err)
	}
	return prepared, nil
}

// BuildCosmosMisbehaviour builds and ABI-encodes the two proof-backed headers.
func (w *Worker) BuildCosmosMisbehaviour(
	stdCtx context.Context,
	evm EVMEndpoint,
	fetchTimeout time.Duration,
	trustedFirst *relayerclient.LightBlock,
	trustedSecond *relayerclient.LightBlock,
	evidence *cosmosMisbehaviourEvidence,
	firstPinned pinnedCosmosValidatorSet,
	secondPinned pinnedCosmosValidatorSet,
) (PreparedCosmosMisbehaviour, error) {
	if w == nil || w.Prover == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: nil prover")
	}
	if evidence == nil || evidence.header1 == nil || evidence.header2 == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: incomplete evidence")
	}
	if trustedFirst == nil || trustedSecond == nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: trusted light blocks are required")
	}
	if trustedFirst.BlockHeight >= evidence.header1.BlockHeight || trustedSecond.BlockHeight >= evidence.header2.BlockHeight {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf(
			"submit misbehaviour: trusted heights %d/%d must be below conflicting heights %d/%d",
			trustedFirst.BlockHeight, trustedSecond.BlockHeight, evidence.header1.BlockHeight, evidence.header2.BlockHeight,
		)
	}
	if len(evidence.sigs1) == 0 || len(evidence.sigs2) == 0 {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: missing selected pinned-set signatures")
	}

	// Misbehaviour is two independent proof jobs — one per conflicting header —
	// composed here (the prover has no misbehaviour-specific API). The path is
	// rare and inherently synchronous, so it submits both and waits on each.
	log.Printf("[Misbehaviour] generating proofs for conflicting headers height=%d sigs=%d/%d",
		evidence.header1.BlockHeight, len(evidence.sigs1), len(evidence.sigs2))
	header1Result, err := w.generateProof(stdCtx, evidence.sigs1)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: header1 proof: %w", err)
	}
	header2Result, err := w.generateProof(stdCtx, evidence.sigs2)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: header2 proof: %w", err)
	}

	header1, err := lightBlockToMisbehaviourHeader(evidence.header1, trustedFirst)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: header1: %w", err)
	}
	header2, err := lightBlockToMisbehaviourHeader(evidence.header2, trustedSecond)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: header2: %w", err)
	}
	trustedConsensusState1, err := misbehaviourConsensusState(trustedFirst)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: trusted consensus state1: %w", err)
	}
	trustedConsensusState2, err := misbehaviourConsensusState(trustedSecond)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: trusted consensus state2: %w", err)
	}
	now, err := misbehaviourHeaderTimestampNanos(stdCtx, evm, fetchTimeout)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: ethereum timestamp: %w", err)
	}
	proof1, err := misbehaviourBatchProof(header1Result, firstPinned)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: proof1 metadata: %w", err)
	}
	proof2, err := misbehaviourBatchProof(header2Result, secondPinned)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: proof2 metadata: %w", err)
	}

	msg := misbehaviourContract.ISpectreClientMsgsMsgSubmitMisbehaviour{
		Misbehaviour: misbehaviourContract.ISpectreClientMsgsMisbehaviour{
			Header1: header1,
			Header2: header2,
		},
		TrustedConsensusState1: trustedConsensusState1,
		TrustedConsensusState2: trustedConsensusState2,
		Time:                   now,
		Proof1:                 proof1,
		Proof2:                 proof2,
	}
	encoded, err := relayerclient.EncodeMisbehaviourContractMsg(msg)
	if err != nil {
		return PreparedCosmosMisbehaviour{}, fmt.Errorf("submit misbehaviour: encode msg: %w", err)
	}
	return PreparedCosmosMisbehaviour{
		EncodedMessage: encoded,
		Height:         uint64(evidence.header1.BlockHeight),
		BlockHash1:     bytesToBytes32(evidence.header1.SignedHeader.Commit.BlockID.Hash),
		BlockHash2:     bytesToBytes32(evidence.header2.SignedHeader.Commit.BlockID.Hash),
	}, nil
}

func lightBlockToMisbehaviourHeader(
	lightBlock *relayerclient.LightBlock,
	trustedLightBlock *relayerclient.LightBlock,
) (misbehaviourContract.ISpectreTendermintMsgsHeader, error) {
	updateHeader, err := lightBlock.IntoHeader(*trustedLightBlock)
	if err != nil {
		return misbehaviourContract.ISpectreTendermintMsgsHeader{}, err
	}
	return updateHeaderToMisbehaviourHeader(updateHeader), nil
}

func misbehaviourConsensusState(lightBlock *relayerclient.LightBlock) (misbehaviourContract.ISpectreTendermintMsgsConsensusState, error) {
	if lightBlock == nil || lightBlock.SignedHeader.Header == nil {
		return misbehaviourContract.ISpectreTendermintMsgsConsensusState{}, fmt.Errorf("missing light block header")
	}
	return misbehaviourContract.ISpectreTendermintMsgsConsensusState{
		Timestamp:          big.NewInt(lightBlock.SignedHeader.Header.Time.UnixNano()),
		Root:               bytesToBytes32(lightBlock.SignedHeader.Header.AppHash),
		NextValidatorsHash: bytesToBytes32(lightBlock.SignedHeader.NextValidatorsHash),
	}, nil
}

func misbehaviourBatchProof(
	headerProof prover.ProofResult,
	pinnedSet pinnedCosmosValidatorSet,
) (misbehaviourContract.ISpectreClientMsgsBatchProof, error) {
	if headerProof.Bucket <= 0 {
		return misbehaviourContract.ISpectreClientMsgsBatchProof{}, fmt.Errorf("invalid bucket %d", headerProof.Bucket)
	}
	if len(headerProof.PaddedSigs) != headerProof.Bucket {
		return misbehaviourContract.ISpectreClientMsgsBatchProof{}, fmt.Errorf(
			"padded signature count %d does not match bucket %d", len(headerProof.PaddedSigs), headerProof.Bucket)
	}

	signerIndices := make([]uint32, headerProof.Bucket)
	signerPubkeys := make([][32]byte, headerProof.Bucket)
	active := make([]bool, headerProof.Bucket)
	pinnedValidatorIndices := make([]uint32, headerProof.Bucket)
	pinnedIndexByPubkey := pinnedSet.indexByPubkey()
	for i, sig := range headerProof.PaddedSigs {
		signerIndices[i] = uint32(maxInt(sig.Index, 0))
		pubkey := bytesToBytes32(sig.PublicKey)
		if sig.Active {
			pinnedIdx, ok := pinnedIndexByPubkey[pubkey]
			if !ok {
				return misbehaviourContract.ISpectreClientMsgsBatchProof{}, fmt.Errorf("active signer at slot %d is not in pinned validator set", i)
			}
			pinnedValidatorIndices[i] = pinnedIdx
		}
		signerPubkeys[i] = pubkey
		active[i] = sig.Active
	}

	return misbehaviourContract.ISpectreClientMsgsBatchProof{
		Proof:                  headerProof.Proof,
		Commitments:            headerProof.Commitments,
		CommitmentPok:          headerProof.CommitmentPok,
		Bucket:                 uint16(headerProof.Bucket),
		SignerIndices:          signerIndices,
		PinnedValidatorIndices: pinnedValidatorIndices,
		SignerPubkeys:          signerPubkeys,
		Active:                 active,
	}, nil
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func updateHeaderToMisbehaviourHeader(h updateclientContract.ISpectreTendermintMsgsHeader) misbehaviourContract.ISpectreTendermintMsgsHeader {
	return misbehaviourContract.ISpectreTendermintMsgsHeader{
		SignedHeader: misbehaviourContract.ISpectreTendermintMsgsSignedHeader{
			Header: updateBlockHeaderToMisbehaviour(h.SignedHeader.Header),
			Commit: updateCommitToMisbehaviour(h.SignedHeader.Commit),
		},
		TrustedHeight: misbehaviourContract.IICS02ClientMsgsHeight{
			RevisionNumber: h.TrustedHeight.RevisionNumber,
			RevisionHeight: h.TrustedHeight.RevisionHeight,
		},
	}
}

func updateBlockHeaderToMisbehaviour(h updateclientContract.ISpectreTendermintMsgsBlockHeader) misbehaviourContract.ISpectreTendermintMsgsBlockHeader {
	return misbehaviourContract.ISpectreTendermintMsgsBlockHeader{
		Version: misbehaviourContract.ISpectreTendermintMsgsVersion{
			BlockVersion: h.Version.BlockVersion,
			AppVersion:   h.Version.AppVersion,
		},
		ChainId:            h.ChainId,
		Height:             h.Height,
		Time:               h.Time,
		HasLastBlockId:     h.HasLastBlockId,
		LastBlockId:        updateBlockIDToMisbehaviour(h.LastBlockId),
		HasLastCommitHash:  h.HasLastCommitHash,
		LastCommitHash:     h.LastCommitHash,
		HasDataHash:        h.HasDataHash,
		DataHash:           h.DataHash,
		ValidatorsHash:     h.ValidatorsHash,
		NextValidatorsHash: h.NextValidatorsHash,
		ConsensusHash:      h.ConsensusHash,
		AppHash:            h.AppHash,
		HasLastResultsHash: h.HasLastResultsHash,
		LastResultsHash:    h.LastResultsHash,
		HasEvidenceHash:    h.HasEvidenceHash,
		EvidenceHash:       h.EvidenceHash,
		ProposerAddress:    h.ProposerAddress,
	}
}

func updateCommitToMisbehaviour(c updateclientContract.ISpectreTendermintMsgsBlockCommit) misbehaviourContract.ISpectreTendermintMsgsBlockCommit {
	sigs := make([]misbehaviourContract.ISpectreTendermintMsgsCommitSig, len(c.CommitSigs))
	for i, sig := range c.CommitSigs {
		sigs[i] = misbehaviourContract.ISpectreTendermintMsgsCommitSig{
			Flag:             sig.Flag,
			ValidatorAddress: sig.ValidatorAddress,
		}
	}
	return misbehaviourContract.ISpectreTendermintMsgsBlockCommit{
		Height:     c.Height,
		Round:      c.Round,
		BlockId:    updateBlockIDToMisbehaviour(c.BlockId),
		CommitSigs: sigs,
	}
}

func updateBlockIDToMisbehaviour(id updateclientContract.ISpectreTendermintMsgsBlockId) misbehaviourContract.ISpectreTendermintMsgsBlockId {
	return misbehaviourContract.ISpectreTendermintMsgsBlockId{
		HashData: id.HashData,
		PartSetHeader: misbehaviourContract.ISpectreTendermintMsgsPartSetHeader{
			Total:    id.PartSetHeader.Total,
			HashData: id.PartSetHeader.HashData,
		},
	}
}
