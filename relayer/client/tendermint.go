// This file talks to a CometBFT RPC endpoint and nothing else: light blocks,
// validators, genesis, proofs.
//
// The Solidity ABI boundary that used to share this file now lives in encode.go.
// The two Cosmos client-state queries that used to sit in ethereum.go live here
// instead: they take an *rpchttp.HTTP, so by this package's own rule -- one file
// per counterparty -- this is where they belong.
package client

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/big"
	"slices"

	misbehaviourContract "relayer/bindings/Misbehaviour"
	spectreContract "relayer/bindings/SpectreClient"
	updateClientContract "relayer/bindings/UpdateClient"
	"relayer/chain/ethnative/spectrepb"

	"github.com/cometbft/cometbft/p2p"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	commettypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/cosmos/gogoproto/proto"
	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	commitmenttypes "github.com/cosmos/ibc-go/v10/modules/core/23-commitment/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
)

const (
	MAX_TOTAL_VOTING_POWER = math.MaxInt64 / 8
	ProofType_EXIST        = 0
	ProofType_NON_EXIST    = 1
)

type LightBlock struct {
	SignedHeader commettypes.SignedHeader
	ValSet       commettypes.ValidatorSet
	NextValSet   commettypes.ValidatorSet
	PeerId       p2p.ID
	BlockHeight  int64
}

func (b *LightBlock) IntoHeader(trustedBlock LightBlock) (updateClientContract.ISpectreTendermintMsgsHeader, error) {
	if b == nil {
		return updateClientContract.ISpectreTendermintMsgsHeader{}, fmt.Errorf("light block is nil")
	}
	revisionNumer := clienttypes.ParseChainID(trustedBlock.SignedHeader.ChainID)
	trustedHeight, err := nonNegativeInt64ToUint64("trusted block height", trustedBlock.BlockHeight)
	if err != nil {
		return updateClientContract.ISpectreTendermintMsgsHeader{}, err
	}
	headerHeight, err := nonNegativeInt64ToUint64("proposed block height", b.BlockHeight)
	if err != nil {
		return updateClientContract.ISpectreTendermintMsgsHeader{}, err
	}
	commitHeight, err := nonNegativeInt64ToUint64("commit height", b.SignedHeader.Commit.Height)
	if err != nil {
		return updateClientContract.ISpectreTendermintMsgsHeader{}, err
	}

	commitSigs := []updateClientContract.ISpectreTendermintMsgsCommitSig{}
	for _, sig := range b.SignedHeader.Commit.Signatures {
		// The validator address ties a commit slot to the validator it belongs to, so the
		// on-chain quorum check can confirm that the slot a proof cites is the same validator
		// whose pinned-set voting power it claims (ZK-09). CometBFT derives it the same way
		// Solidity does — the first 20 bytes of sha256 over the Ed25519 pubkey. ABSENT slots
		// carry no address and copy as zero, which is fine: only active signers are checked.
		var valAddr [20]byte
		copy(valAddr[:], sig.ValidatorAddress)

		// CometBFT: 0=UNKNOWN, 1=ABSENT, 2=COMMIT, 3=NIL
		// Solidity:  0=UNKNOWN, 1=ABSENT, 2=COMMIT, 3=NIL
		commitSigs = append(commitSigs, updateClientContract.ISpectreTendermintMsgsCommitSig{
			Flag:             uint8(sig.BlockIDFlag),
			ValidatorAddress: valAddr,
		})
	}

	header := updateClientContract.ISpectreTendermintMsgsHeader{
		TrustedHeight: updateClientContract.IICS02ClientMsgsHeight{
			RevisionNumber: revisionNumer,
			RevisionHeight: trustedHeight,
		},
		SignedHeader: updateClientContract.ISpectreTendermintMsgsSignedHeader{
			Header: updateClientContract.ISpectreTendermintMsgsBlockHeader{
				Version: updateClientContract.ISpectreTendermintMsgsVersion{
					BlockVersion: b.SignedHeader.Version.Block,
					AppVersion:   b.SignedHeader.Version.App,
				},
				ChainId:        b.SignedHeader.ChainID,
				Height:         headerHeight,
				Time:           big.NewInt(b.SignedHeader.Time.UnixNano()),
				HasLastBlockId: !b.SignedHeader.LastBlockID.IsZero(),
				LastBlockId: updateClientContract.ISpectreTendermintMsgsBlockId{
					HashData: bytesToBytes32(b.SignedHeader.LastBlockID.Hash),
					PartSetHeader: updateClientContract.ISpectreTendermintMsgsPartSetHeader{
						Total:    b.SignedHeader.LastBlockID.PartSetHeader.Total,
						HashData: bytesToBytes32(b.SignedHeader.LastBlockID.PartSetHeader.Hash),
					},
				},
				HasLastCommitHash:  b.SignedHeader.LastCommitHash != nil,
				LastCommitHash:     bytesToBytes32(b.SignedHeader.LastCommitHash),
				HasDataHash:        b.SignedHeader.DataHash != nil,
				DataHash:           bytesToBytes32(b.SignedHeader.DataHash),
				ValidatorsHash:     bytesToBytes32(b.SignedHeader.ValidatorsHash),
				NextValidatorsHash: bytesToBytes32(b.SignedHeader.NextValidatorsHash),
				ConsensusHash:      bytesToBytes32(b.SignedHeader.ConsensusHash),
				AppHash:            bytesToBytes32(b.SignedHeader.AppHash),
				HasLastResultsHash: b.SignedHeader.LastResultsHash != nil,
				LastResultsHash:    bytesToBytes32(b.SignedHeader.LastResultsHash),
				HasEvidenceHash:    b.SignedHeader.EvidenceHash != nil,
				EvidenceHash:       bytesToBytes32(b.SignedHeader.EvidenceHash),
				ProposerAddress:    b.SignedHeader.ProposerAddress,
			},
			Commit: updateClientContract.ISpectreTendermintMsgsBlockCommit{
				Height: commitHeight,
				Round:  uint32(b.SignedHeader.Commit.Round),
				BlockId: updateClientContract.ISpectreTendermintMsgsBlockId{
					HashData: bytesToBytes32(b.SignedHeader.Commit.BlockID.Hash),
					PartSetHeader: updateClientContract.ISpectreTendermintMsgsPartSetHeader{
						Total:    b.SignedHeader.Commit.BlockID.PartSetHeader.Total,
						HashData: bytesToBytes32(b.SignedHeader.Commit.BlockID.PartSetHeader.Hash),
					},
				},
				CommitSigs: commitSigs,
			},
		},
	}

	return header, nil
}

type SpectreClientGenesis struct {
	TrustedClientState        ClientState
	TrustedConsensusState     updateClientContract.ISpectreTendermintMsgsConsensusState
	InitialPinnedValidatorSet spectreContract.ISpectreTendermintMsgsValidatorSet
}

// DefaultClockDrift is the allowed gap (in seconds) between the proven
// consensus-state timestamp and the verifying chain's block time. It must be
// generous enough to cover relay latency (proof gen + destination block time +
// queueing); too small a value makes the light client reject otherwise-valid
// updates with ProofIsTooOld. The same value is stored in the client state at
// creation and drives the on-chain freshness window in `_requireFreshness`
// (ProofIsInTheFuture / ProofIsTooOld).
const DefaultClockDrift uint32 = 30

func GetGenesis(client *rpchttp.HTTP, trustedBlock int64, trustingPeriod uint32, trustLevel string, _ string, clockDrift uint32) (*SpectreClientGenesis, error) {
	status, err := client.Status(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	if trustedBlock == 0 {
		// get latest block
		trustedBlock = status.SyncInfo.LatestBlockHeight
	}

	// GetGenesis is a one-shot CLI path with no context of its own (see the
	// client.Status call above); the ctx-less RPC is explicit at the call site
	// rather than hidden behind a wrapper.
	trustedLightBlock, err := GetLightBlock(context.Background(), client, trustedBlock)
	if err != nil {
		return nil, fmt.Errorf("failed to get light block: %w", err)
	}

	unbondingPeriod, err := GetUnbondingTime(client)
	if err != nil {
		return nil, fmt.Errorf("failed to get unbonding time: %w", err)
	}

	return spectreClientGenesisFromLightBlock(trustedLightBlock, unbondingPeriod, trustingPeriod, trustLevel, clockDrift)
}

func spectreClientGenesisFromLightBlock(trustedLightBlock *LightBlock, unbondingPeriod float64, trustingPeriod uint32, trustLevel string, clockDrift uint32) (*SpectreClientGenesis, error) {
	if trustedLightBlock == nil {
		return nil, fmt.Errorf("trusted light block is nil")
	}
	if clockDrift == 0 {
		clockDrift = DefaultClockDrift
	}
	if trustingPeriod == 0 {
		trustingPeriod = uint32(unbondingPeriod * 2 / 3)
	}

	if trustingPeriod > uint32(unbondingPeriod) {
		return nil, fmt.Errorf("trusting period %d cannot be greater than unbonding period %d", trustingPeriod, uint32(unbondingPeriod))
	}

	chainId := trustedLightBlock.SignedHeader.Header.ChainID
	revision := clienttypes.ParseChainID(chainId)

	trustThreshold, err := ParseTrustThreshold(trustLevel)
	if err != nil {
		return nil, fmt.Errorf("failed to parse trust level: %w", err)
	}

	latestRevisionHeight, err := nonNegativeInt64ToUint64("trusted light block header height", trustedLightBlock.SignedHeader.Header.Height)
	if err != nil {
		return nil, err
	}

	clientState := ClientState{
		ChainId:    chainId,
		TrustLevel: trustThreshold,
		LatestHeight: updateClientContract.IICS02ClientMsgsHeight{
			RevisionNumber: revision,
			RevisionHeight: latestRevisionHeight,
		},
		IsFrozen:        false,
		TrustingPeriod:  trustingPeriod,
		UnbondingPeriod: uint32(unbondingPeriod),
		ClockDrift:      clockDrift,
	}

	consensusState, err := ConsensusStateFromLightBlock(trustedLightBlock)
	if err != nil {
		return nil, err
	}

	initialPinnedValidatorSet, err := ValidatorSetToContract(trustedLightBlock.NextValSet, "initial pinned")
	if err != nil {
		return nil, err
	}

	genesis := SpectreClientGenesis{
		TrustedClientState:        clientState,
		TrustedConsensusState:     consensusState,
		InitialPinnedValidatorSet: initialPinnedValidatorSet,
	}
	return &genesis, nil
}

// ConsensusStateFromLightBlock reconstructs the Spectre consensus state committed by a
// CometBFT light block. Its ABI encoding is the value SpectreClient hashes and stores for
// the corresponding revision height.
func ConsensusStateFromLightBlock(
	lightBlock *LightBlock,
) (updateClientContract.ISpectreTendermintMsgsConsensusState, error) {
	if lightBlock == nil {
		return updateClientContract.ISpectreTendermintMsgsConsensusState{}, fmt.Errorf("light block is nil")
	}

	return updateClientContract.ISpectreTendermintMsgsConsensusState{
		Timestamp:          big.NewInt(lightBlock.SignedHeader.Header.Time.UnixNano()),
		Root:               bytesToBytes32(lightBlock.SignedHeader.Header.AppHash),
		NextValidatorsHash: bytesToBytes32(lightBlock.SignedHeader.NextValidatorsHash),
	}, nil
}

func GetUnbondingTime(client *rpchttp.HTTP) (float64, error) {

	// Query path for staking parameters
	queryPath := "/cosmos.staking.v1beta1.Query/Params"

	// Make ABCI query
	result, err := client.ABCIQuery(context.Background(), queryPath, nil)
	if err != nil {
		return 0, fmt.Errorf("ABCI query failed: %w", err)
	}

	if result.Response.Code != 0 {
		return 0, fmt.Errorf("query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}

	// Create codec for decoding
	interfaceRegistry := codectypes.NewInterfaceRegistry()
	cdc := codec.NewProtoCodec(interfaceRegistry)

	// Decode the response
	var params stakingtypes.QueryParamsResponse
	if err := cdc.Unmarshal(result.Response.Value, &params); err != nil {
		return 0, fmt.Errorf("failed to unmarshal params: %w", err)
	}
	return params.Params.UnbondingTime.Seconds(), nil
}

func GetLatestLightBlock(ctx context.Context, client *rpchttp.HTTP) (*LightBlock, error) {
	status, err := client.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	return GetLightBlock(ctx, client, status.SyncInfo.LatestBlockHeight)
}

// cometBFTMaxPerPage is the largest page size the CometBFT /validators RPC
// honors (rpc/core/env.go maxPerPage). Requesting this many per page minimizes
// round-trips while still reading the full set.
const cometBFTMaxPerPage = 100

// validatorsPager is the subset of the CometBFT RPC client used to page the
// validator set. *rpchttp.HTTP satisfies it; tests supply a fake.
type validatorsPager interface {
	Validators(ctx context.Context, height *int64, page, perPage *int) (*coretypes.ResultValidators, error)
}

// fetchAllValidators returns the complete validator set at the given height.
//
// The CometBFT /validators RPC paginates and defaults to perPage=30 when perPage
// is nil (rpc/core/env.go validatePerPage). Calling it once with nil therefore
// silently truncates the set to the first 30 validators on any chain with more
// than 30 — the assembled set then has the wrong hash and signer extraction
// fails for indices >= 30. We page explicitly and loop until the reported Total
// is collected (issue #105).
func fetchAllValidators(ctx context.Context, client validatorsPager, height int64) ([]*commettypes.Validator, error) {
	var collected []*commettypes.Validator
	perPage := cometBFTMaxPerPage
	for page := 1; ; page++ {
		p := page
		resp, err := client.Validators(ctx, &height, &p, &perPage)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch validators (height %d, page %d): %w", height, page, err)
		}
		collected = append(collected, resp.Validators...)
		// Stop once we have the full set, or defensively if a page is empty.
		if len(collected) >= resp.Total || len(resp.Validators) == 0 {
			break
		}
	}
	return collected, nil
}

// GetLightBlock fetches a complete light block, propagating cancellation
// through every CometBFT request.
func GetLightBlock(ctx context.Context, client *rpchttp.HTTP, height int64) (*LightBlock, error) {
	status, err := client.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	peerId := status.NodeInfo.ID()
	commitResp, err := client.Commit(ctx, &height)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch trusted commit: %w", err)
	}

	signedHeader := commitResp.SignedHeader
	proposerAddr := signedHeader.Header.ProposerAddress
	var proposer *commettypes.Validator
	validators, err := fetchAllValidators(ctx, client, height)
	if err != nil {
		return nil, err
	}

	for _, resp := range validators {
		if resp != nil {
			if slices.Equal(resp.Address.Bytes(), proposerAddr.Bytes()) {
				proposer = resp
				break
			}
		}
	}

	valSet := commettypes.NewValidatorSet(validators)
	valSet.Proposer = proposer
	// Guard against a truncated/incomplete fetch: the assembled set must hash to
	// the value committed in the header, otherwise the on-chain validator-set
	// hash check would fail and signer indices would be misaligned (issue #105).
	if !bytes.Equal(valSet.Hash(), signedHeader.Header.ValidatorsHash) {
		return nil, fmt.Errorf(
			"validator set hash mismatch at height %d: assembled %X != header ValidatorsHash %X (got %d validators)",
			height, valSet.Hash(), signedHeader.Header.ValidatorsHash.Bytes(), len(validators))
	}

	nextHeight := height + 1
	nextValidators, err := fetchAllValidators(ctx, client, nextHeight)
	if err != nil {
		return nil, err
	}

	for _, resp := range nextValidators {
		if resp != nil {
			if slices.Equal(resp.Address.Bytes(), proposerAddr.Bytes()) {
				proposer = resp
				break
			}
		}
	}

	nextValSet := commettypes.NewValidatorSet(nextValidators)
	nextValSet.Proposer = proposer
	if !bytes.Equal(nextValSet.Hash(), signedHeader.Header.NextValidatorsHash) {
		return nil, fmt.Errorf(
			"next validator set hash mismatch at height %d: assembled %X != header NextValidatorsHash %X (got %d validators)",
			nextHeight, nextValSet.Hash(), signedHeader.Header.NextValidatorsHash.Bytes(), len(nextValidators))
	}

	return &LightBlock{
		SignedHeader: signedHeader,
		ValSet:       *valSet,
		NextValSet:   *nextValSet,
		PeerId:       peerId,
		BlockHeight:  height,
	}, nil

}

func ProvePath(ctx context.Context, client *rpchttp.HTTP, height int64, path [][]byte) ([]byte, *commitmenttypes.MerkleProof, error) {
	queryPath := fmt.Sprintf("store/%s/key", string(path[0]))
	request := slices.Concat(path[1:]...)

	// Make ABCI query
	result, err := client.ABCIQueryWithOptions(ctx, queryPath, request, rpcclient.ABCIQueryOptions{
		// Proof height should be the block before the target block.
		Height: height - 1,
		Prove:  true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ABCI query failed: %w", err)
	}

	if result.Response.Code != 0 {
		return nil, nil, fmt.Errorf("query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}

	if result.Response.Height != height-1 {
		return nil, nil, fmt.Errorf("proof height mismatch")
	}

	if !slices.Equal(result.Response.Key, path[1]) {
		return nil, nil, fmt.Errorf("key mismatch")
	}

	proof, err := commitmenttypes.ConvertProofs(result.Response.ProofOps)
	if err != nil {
		return nil, nil, fmt.Errorf("proof could not be retrieved: %w", err)
	}
	if len(proof.Proofs) == 0 {
		return nil, nil, fmt.Errorf("proof is empty")
	}
	return result.Response.Value, &proof, nil
}

// EncodeUpdateConsensusStateMsg abi-encodes a MsgUpdateConsensusState for
// SpectreClient.updateConsensusState(bytes) / ICS26Router.updateConsensusState.
func EncodeUpdateConsensusStateMsg(update updateClientContract.ISpectreClientMsgsMsgUpdateApplicationState, newValidatorSet spectreContract.ISpectreTendermintMsgsValidatorSet) ([]byte, error) {
	args := abi.Arguments{
		{Type: updateConsensusStateMsgType},
	}
	return args.Pack(MsgUpdateConsensusState{Update: update, NewValidatorSet: newValidatorSet})
}

// EncodeMisbehaviourContractMsg encodes the generated Misbehaviour binding
// type. The generated binding uses package-local struct types, so this helper
// shares the same ABI tuple while allowing the services package to preserve
// the exact proof metadata types produced by that binding.
func EncodeMisbehaviourContractMsg(msg misbehaviourContract.ISpectreClientMsgsMsgSubmitMisbehaviour) ([]byte, error) {
	args := abi.Arguments{{Type: misbehaviourMsgType}}
	return args.Pack(msg)
}

func bytesToBytes32(data []byte) [32]byte {
	var result [32]byte
	copy(result[:], data)
	return result
}

// GetEthereumClientState reads the Ethereum client state on Cosmos, propagating
// cancellation into the ABCI query. The destination hosts the light client as a
// native module (ibc.lightclients.spectre.v1), so the client state is the
// protobuf message itself rather than an 08-wasm wrapper around JSON.
func GetEthereumClientState(ctx context.Context, cosmosClient *rpchttp.HTTP, clientID string) (*EthereumClientState, error) {
	queryReq := &clienttypes.QueryClientStateRequest{
		ClientId: clientID,
	}

	reqBytes, err := proto.Marshal(queryReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query request: %w", err)
	}

	result, err := cosmosClient.ABCIQuery(ctx, "/ibc.core.client.v1.Query/ClientState", reqBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to query client state: %w", err)
	}

	if result.Response.Code != 0 {
		return nil, fmt.Errorf("query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}

	var queryResp clienttypes.QueryClientStateResponse
	if err := proto.Unmarshal(result.Response.Value, &queryResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal query response: %w", err)
	}

	if queryResp.ClientState == nil {
		return nil, fmt.Errorf("client %s has no client state", clientID)
	}
	if queryResp.ClientState.TypeUrl != SpectreClientStateTypeURL {
		return nil, fmt.Errorf("client %s has state type %q, not the spectre Ethereum light client (%s)",
			clientID, queryResp.ClientState.TypeUrl, SpectreClientStateTypeURL)
	}
	var clientState spectrepb.ClientState
	if err := proto.Unmarshal(queryResp.ClientState.Value, &clientState); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ethereum client state: %w", err)
	}

	return EthereumClientStateFromProto(&clientState), nil
}

// GetWasmClientLatestHeight reads the LatestHeight of any 08-wasm client on Cosmos
// (ETH beacon, L2 rollup, ...). The wasm ClientState carries LatestHeight directly,
// so this does not decode the client-specific inner Data — for an L2 client the
// revision height IS the L2 block number the client trusts, i.e. the proof height.
func GetWasmClientLatestHeight(ctx context.Context, cosmosClient *rpchttp.HTTP, clientID string) (clienttypes.Height, error) {
	queryReq := &clienttypes.QueryClientStateRequest{ClientId: clientID}
	reqBytes, err := proto.Marshal(queryReq)
	if err != nil {
		return clienttypes.Height{}, fmt.Errorf("marshal client-state query: %w", err)
	}
	result, err := cosmosClient.ABCIQuery(ctx, "/ibc.core.client.v1.Query/ClientState", reqBytes)
	if err != nil {
		return clienttypes.Height{}, fmt.Errorf("query client state %s: %w", clientID, err)
	}
	if result.Response.Code != 0 {
		return clienttypes.Height{}, fmt.Errorf("client-state query %s failed with code %d: %s", clientID, result.Response.Code, result.Response.Log)
	}
	var queryResp clienttypes.QueryClientStateResponse
	if err := proto.Unmarshal(result.Response.Value, &queryResp); err != nil {
		return clienttypes.Height{}, fmt.Errorf("unmarshal client-state response: %w", err)
	}
	var wasmClientState ibcwasmtypes.ClientState
	if err := proto.Unmarshal(queryResp.ClientState.Value, &wasmClientState); err != nil {
		return clienttypes.Height{}, fmt.Errorf("unmarshal wasm client state: %w", err)
	}
	return wasmClientState.LatestHeight, nil
}

// GetClientStatus asks ibc-go for a client's status ("Active", "Frozen",
// "Expired", ...). It is the chain's own answer, so it covers every client type
// -- the ETH beacon and the L2 wasm clients alike -- without decoding the
// client-specific state, and it is the same predicate 04-channel/v2 applies
// before it accepts a packet.
func GetClientStatus(ctx context.Context, cosmosClient *rpchttp.HTTP, clientID string) (string, error) {
	reqBytes, err := proto.Marshal(&clienttypes.QueryClientStatusRequest{ClientId: clientID})
	if err != nil {
		return "", fmt.Errorf("marshal client-status query: %w", err)
	}
	result, err := cosmosClient.ABCIQuery(ctx, "/ibc.core.client.v1.Query/ClientStatus", reqBytes)
	if err != nil {
		return "", fmt.Errorf("query client status %s: %w", clientID, err)
	}
	if result.Response.Code != 0 {
		return "", fmt.Errorf("query client status %s failed with code %d: %s", clientID, result.Response.Code, result.Response.Log)
	}
	var resp clienttypes.QueryClientStatusResponse
	if err := proto.Unmarshal(result.Response.Value, &resp); err != nil {
		return "", fmt.Errorf("unmarshal client status %s: %w", clientID, err)
	}
	return resp.Status, nil
}
