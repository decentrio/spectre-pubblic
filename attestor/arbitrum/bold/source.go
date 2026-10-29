// Package bold ingests BoLD assertions from RollupCore and checks their
// committed L2 blocks against the configured Nitro RPC endpoint.
package bold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	arbitrum "attestor/arbitrum"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	// AssertionStatusNone means the RollupCore object no longer exists.
	AssertionStatusNone uint8 = 0
	// AssertionStatusPending means the RollupCore object remains challengeable.
	AssertionStatusPending uint8 = 1
	// AssertionStatusConfirmed means RollupCore has irreversibly confirmed it.
	AssertionStatusConfirmed uint8 = 2

	assertionStatusNone      = AssertionStatusNone
	assertionStatusPending   = AssertionStatusPending
	assertionStatusConfirmed = AssertionStatusConfirmed
	maxMachineStatus         = uint8(2)

	assertionCreatedDataWords   = 25
	assertionConfirmedDataWords = 2
	abiWordSize                 = 32
)

var (
	assertionCreatedTopic = crypto.Keccak256Hash([]byte(
		"AssertionCreated(bytes32,bytes32,((bytes32,bytes32,(bytes32,uint256,address,uint64,uint64)),((bytes32[2],uint64[2]),uint8,bytes32),((bytes32[2],uint64[2]),uint8,bytes32)),bytes32,uint256,bytes32,uint256,address,uint64)",
	))
	assertionConfirmedTopic = crypto.Keccak256Hash([]byte(
		"AssertionConfirmed(bytes32,bytes32,bytes32)",
	))
	chainIDSelector             = crypto.Keccak256([]byte("chainId()"))[:4]
	errZeroAssertionL2BlockHash = errors.New("assertion has zero L2 block hash")
)

// L1Client is the Ethereum RPC surface required by RollupCoreSource.
type L1Client interface {
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
	ChainID(context.Context) (*big.Int, error)
	FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error)
	CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error)
	StorageAt(context.Context, common.Address, common.Hash, *big.Int) ([]byte, error)
	StorageAtHash(context.Context, common.Address, common.Hash, common.Hash) ([]byte, error)
}

// ConfirmedAssertion is one finalized-L1 AssertionConfirmed event.
type ConfirmedAssertion struct {
	AssertionHash common.Hash
	L2BlockHash   common.Hash
	L1BlockNumber uint64
}

// L1Block identifies a fixed Ethereum snapshot.
type L1Block struct {
	Number uint64
	Hash   common.Hash
}

// AssertionSource is the testable RollupCore input consumed by the attestation loop.
type AssertionSource interface {
	Head(context.Context, arbitrum.L1IngestHead) (L1Block, error)
	CanonicalHash(context.Context, uint64) (common.Hash, error)
	AssertionStatusAtHash(context.Context, arbitrum.ProposedAssertion, common.Hash) (uint8, error)
	FinalizedBlockNumber(context.Context) (uint64, error)
	ChainIDs(context.Context, uint64) (*big.Int, *big.Int, error)
	Assertions(
		context.Context,
		uint64,
		uint64,
	) ([]arbitrum.ProposedAssertion, []ConfirmedAssertion, error)
	AssertionStatus(context.Context, arbitrum.ProposedAssertion, uint64) (uint8, error)
}

// RollupCoreSource reads a single Arbitrum RollupCore contract through an L1
// execution RPC. The caller selects fixed block numbers or hashes for reads.
type RollupCoreSource struct {
	client                L1Client
	address               common.Address
	assertionsMappingSlot common.Hash
	assertionStatusOffset uint8
}

// NewRollupCoreSource constructs a finalized-L1 assertion source.
func NewRollupCoreSource(
	client L1Client,
	address common.Address,
	assertionsMappingSlot common.Hash,
	assertionStatusOffset uint8,
) (*RollupCoreSource, error) {
	if client == nil {
		return nil, errors.New("L1 client must not be nil")
	}
	if address == (common.Address{}) {
		return nil, errors.New("RollupCore address must not be zero")
	}
	if assertionStatusOffset >= abiWordSize {
		return nil, fmt.Errorf(
			"assertion status offset %d is outside a storage word",
			assertionStatusOffset,
		)
	}
	return &RollupCoreSource{
		client:                client,
		address:               address,
		assertionsMappingSlot: assertionsMappingSlot,
		assertionStatusOffset: assertionStatusOffset,
	}, nil
}

// FinalizedBlockNumber returns the L1 execution block covered by Ethereum's
// finalized tag.
func (s *RollupCoreSource) FinalizedBlockNumber(ctx context.Context) (uint64, error) {
	header, err := s.client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if errors.Is(err, ethereum.NotFound) || (err == nil && header == nil) {
		return 0, fmt.Errorf("finalized L1 block was not found: %w", ethereum.NotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("query finalized L1 block: %w", err)
	}
	if header.Number == nil || !header.Number.IsUint64() {
		return 0, errors.New("finalized L1 block number is not a uint64")
	}
	return header.Number.Uint64(), nil
}

// Head resolves a tag once; callers must not repeatedly read a moving tag.
func (s *RollupCoreSource) Head(ctx context.Context, head arbitrum.L1IngestHead) (L1Block, error) {
	head, err := head.Normalize()
	if err != nil {
		return L1Block{}, err
	}
	tag := rpc.FinalizedBlockNumber
	switch head {
	case arbitrum.L1IngestSafe:
		tag = rpc.SafeBlockNumber
	case arbitrum.L1IngestLatest:
		tag = rpc.LatestBlockNumber
	}
	header, err := s.client.HeaderByNumber(ctx, big.NewInt(int64(tag)))
	if err != nil {
		return L1Block{}, fmt.Errorf("query %s L1 head: %w", head, err)
	}
	if header == nil || header.Number == nil || !header.Number.IsUint64() {
		return L1Block{}, fmt.Errorf("%s L1 head has invalid block number", head)
	}
	return L1Block{Number: header.Number.Uint64(), Hash: header.Hash()}, nil
}

// CanonicalHash returns the current canonical Ethereum hash at the given height.
func (s *RollupCoreSource) CanonicalHash(ctx context.Context, number uint64) (common.Hash, error) {
	header, err := s.client.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
	if err != nil {
		return common.Hash{}, fmt.Errorf("query canonical L1 block %d: %w", number, err)
	}
	if header == nil || header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != number {
		return common.Hash{}, fmt.Errorf("canonical L1 block %d has invalid header", number)
	}
	return header.Hash(), nil
}

// AssertionStatusAtHash pins speculative status reads to the ingest snapshot.
func (s *RollupCoreSource) AssertionStatusAtHash(ctx context.Context, proposal arbitrum.ProposedAssertion, hash common.Hash) (uint8, error) {
	preimage := append(proposal.AssertionHash.Bytes(), s.assertionsMappingSlot.Bytes()...)
	output, err := s.client.StorageAtHash(ctx, s.address, crypto.Keccak256Hash(preimage), hash)
	if err != nil {
		return 0, fmt.Errorf("query assertion %s status at L1 hash %s: %w", proposal.AssertionHash, hash, err)
	}
	return s.decodeAssertionStatus(proposal.AssertionHash, output)
}

// ChainIDs returns the connected Ethereum L1 chain ID and RollupCore's
// configured Arbitrum L2 chain ID at the supplied finalized L1 block.
func (s *RollupCoreSource) ChainIDs(
	ctx context.Context,
	finalizedL1Block uint64,
) (*big.Int, *big.Int, error) {
	l1ChainID, err := s.client.ChainID(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("query L1 chain ID: %w", err)
	}
	if l1ChainID == nil {
		return nil, nil, errors.New("L1 chain ID is nil")
	}
	output, err := s.client.CallContract(ctx, ethereum.CallMsg{
		To:   &s.address,
		Data: append([]byte(nil), chainIDSelector...),
	}, new(big.Int).SetUint64(finalizedL1Block))
	if err != nil {
		return nil, nil, fmt.Errorf("query RollupCore chainId: %w", err)
	}
	if len(output) != abiWordSize {
		return nil, nil, fmt.Errorf(
			"RollupCore chainId returned %d bytes, want %d",
			len(output),
			abiWordSize,
		)
	}
	return new(big.Int).Set(l1ChainID), new(big.Int).SetBytes(output), nil
}

// Assertions reads and validates AssertionCreated and AssertionConfirmed logs
// from one inclusive L1 block range.
func (s *RollupCoreSource) Assertions(
	ctx context.Context,
	fromL1Block uint64,
	toL1Block uint64,
) ([]arbitrum.ProposedAssertion, []ConfirmedAssertion, error) {
	if fromL1Block > toL1Block {
		return nil, nil, fmt.Errorf(
			"invalid assertion log range %d..%d",
			fromL1Block,
			toL1Block,
		)
	}
	logs, err := s.client.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromL1Block),
		ToBlock:   new(big.Int).SetUint64(toL1Block),
		Addresses: []common.Address{s.address},
		Topics:    [][]common.Hash{{assertionCreatedTopic, assertionConfirmedTopic}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf(
			"filter RollupCore assertion logs %d..%d: %w",
			fromL1Block,
			toL1Block,
			err,
		)
	}

	var proposals []arbitrum.ProposedAssertion
	var confirmations []ConfirmedAssertion
	for _, event := range logs {
		if event.Removed {
			return nil, nil, fmt.Errorf(
				"RollupCore log at L1 block %d was marked removed",
				event.BlockNumber,
			)
		}
		if len(event.Topics) == 0 {
			return nil, nil, fmt.Errorf(
				"RollupCore log at L1 block %d has no event topic",
				event.BlockNumber,
			)
		}
		switch event.Topics[0] {
		case assertionCreatedTopic:
			proposal, parseErr := parseAssertionCreated(event)
			if parseErr != nil {
				if errors.Is(parseErr, errZeroAssertionL2BlockHash) {
					continue
				}
				return nil, nil, parseErr
			}
			proposals = append(proposals, proposal)
		case assertionConfirmedTopic:
			confirmation, parseErr := parseAssertionConfirmed(event)
			if parseErr != nil {
				return nil, nil, parseErr
			}
			confirmations = append(confirmations, confirmation)
		default:
			return nil, nil, fmt.Errorf(
				"unexpected RollupCore event topic %s",
				event.Topics[0],
			)
		}
	}
	return proposals, confirmations, nil
}

// AssertionStatus reads the packed AssertionNode.status byte at one finalized
// L1 block. A direct storage read is required because getAssertion reverts once
// an assertion is destroyed instead of returning AssertionStatus.NoAssertion.
func (s *RollupCoreSource) AssertionStatus(
	ctx context.Context,
	proposal arbitrum.ProposedAssertion,
	finalizedL1Block uint64,
) (uint8, error) {
	assertionHash := proposal.AssertionHash
	preimage := make([]byte, common.HashLength*2)
	copy(preimage[:common.HashLength], assertionHash[:])
	copy(preimage[common.HashLength:], s.assertionsMappingSlot[:])
	storageKey := crypto.Keccak256Hash(preimage)
	output, err := s.client.StorageAt(
		ctx,
		s.address,
		storageKey,
		new(big.Int).SetUint64(finalizedL1Block),
	)
	if err != nil {
		return 0, fmt.Errorf("query assertion %s status: %w", assertionHash, err)
	}
	return s.decodeAssertionStatus(assertionHash, output)
}

func (s *RollupCoreSource) decodeAssertionStatus(assertionHash common.Hash, output []byte) (uint8, error) {
	if len(output) != abiWordSize {
		return 0, fmt.Errorf(
			"assertion %s status slot returned %d bytes, want %d",
			assertionHash,
			len(output),
			abiWordSize,
		)
	}
	status := output[abiWordSize-1-int(s.assertionStatusOffset)]
	if status > assertionStatusConfirmed {
		return 0, fmt.Errorf("assertion %s has unknown status %d", assertionHash, status)
	}
	return status, nil
}

func parseAssertionCreated(event types.Log) (arbitrum.ProposedAssertion, error) {
	if len(event.Topics) != 3 {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated at L1 block %d has %d topics, want 3",
			event.BlockNumber,
			len(event.Topics),
		)
	}
	if len(event.Data) != assertionCreatedDataWords*abiWordSize {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated at L1 block %d has %d data bytes, want %d",
			event.BlockNumber,
			len(event.Data),
			assertionCreatedDataWords*abiWordSize,
		)
	}

	assertionHash := event.Topics[1]
	parentHash := event.Topics[2]
	afterState := event.Data[13*abiWordSize : 19*abiWordSize]
	machineStatusWord := afterState[4*abiWordSize : 5*abiWordSize]
	if !bytes.Equal(machineStatusWord[:abiWordSize-1], make([]byte, abiWordSize-1)) {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated %s machine status is not a uint8",
			assertionHash,
		)
	}
	machineStatus := machineStatusWord[abiWordSize-1]
	if machineStatus == 0 {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated %s commits to an unfinished machine state",
			assertionHash,
		)
	}
	if machineStatus > maxMachineStatus {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated %s has unknown machine status %d",
			assertionHash,
			machineStatus,
		)
	}

	l2BlockHash := common.BytesToHash(afterState[:abiWordSize])
	if l2BlockHash == (common.Hash{}) {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated %s has a zero L2 block hash: %w",
			assertionHash,
			errZeroAssertionL2BlockHash,
		)
	}
	afterInboxBatchAcc := common.BytesToHash(
		event.Data[19*abiWordSize : 20*abiWordSize],
	)
	afterStateHash := crypto.Keccak256Hash(afterState)
	calculatedHash := crypto.Keccak256Hash(
		parentHash[:],
		afterStateHash[:],
		afterInboxBatchAcc[:],
	)
	if calculatedHash != assertionHash {
		return arbitrum.ProposedAssertion{}, fmt.Errorf(
			"AssertionCreated hash mismatch at L1 block %d: event=%s calculated=%s",
			event.BlockNumber,
			assertionHash,
			calculatedHash,
		)
	}
	return arbitrum.ProposedAssertion{
		AssertionHash:    assertionHash,
		ParentHash:       parentHash,
		L2BlockHash:      l2BlockHash,
		InboxAccumulator: afterInboxBatchAcc,
		L1BlockNumber:    event.BlockNumber,
	}, nil
}

func parseAssertionConfirmed(event types.Log) (ConfirmedAssertion, error) {
	if len(event.Topics) != 2 {
		return ConfirmedAssertion{}, fmt.Errorf(
			"AssertionConfirmed at L1 block %d has %d topics, want 2",
			event.BlockNumber,
			len(event.Topics),
		)
	}
	if len(event.Data) != assertionConfirmedDataWords*abiWordSize {
		return ConfirmedAssertion{}, fmt.Errorf(
			"AssertionConfirmed at L1 block %d has %d data bytes, want %d",
			event.BlockNumber,
			len(event.Data),
			assertionConfirmedDataWords*abiWordSize,
		)
	}
	l2BlockHash := common.BytesToHash(event.Data[:abiWordSize])
	if l2BlockHash == (common.Hash{}) {
		return ConfirmedAssertion{}, fmt.Errorf(
			"AssertionConfirmed %s has a zero L2 block hash",
			event.Topics[1],
		)
	}
	return ConfirmedAssertion{
		AssertionHash: event.Topics[1],
		L2BlockHash:   l2BlockHash,
		L1BlockNumber: event.BlockNumber,
	}, nil
}
