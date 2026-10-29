package client

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
)

// Gloas light client headers carry no execution payload header, only
// execution_block_hash (consensus-specs gloas/light-client/sync-protocol.md). The wasm client
// therefore takes the execution state root, timestamp and block number from an RLP-encoded
// execution block header shipped beside the update, trusting it only because it hashes to the
// proven block hash. The helpers here fetch that header and reproduce the client's checks so a
// bad header is caught before a transaction is spent on it.

// maxExecutionHeaderRLPBytes mirrors MAX_EXECUTION_HEADER_RLP_BYTES in
// packages/ethereum/light-client/src/execution.rs.
const maxExecutionHeaderRLPBytes = 2048

// Positions of the items the light client reads; stable in every fork since genesis.
const (
	executionHeaderStateRootIndex = 3
	executionHeaderNumberIndex    = 8
	executionHeaderTimestampIndex = 11
)

// ExecutionSummary is what the Ethereum light client stores per finalized header.
type ExecutionSummary struct {
	StateRoot   string
	Timestamp   uint64
	BlockNumber uint64
}

// RawHeaderSource returns the RLP encoding of an execution block header.
type RawHeaderSource interface {
	RawHeaderByHash(ctx context.Context, hash common.Hash) ([]byte, error)
}

// IsGloasFormat reports whether the header carries an execution block hash instead of a full
// execution payload header.
func (h LightClientHeader) IsGloasFormat() bool {
	return h.Execution == nil && h.ExecutionBlockHash != ""
}

// ResolveExecution returns the execution summary of a light client header and, for a Gloas-format
// header, the RLP execution header the wasm client needs to derive it. The RLP is nil for a
// pre-Gloas-format header, whose payload header already carries the values.
func ResolveExecution(ctx context.Context, source RawHeaderSource, header LightClientHeader) (ExecutionSummary, []byte, error) {
	switch {
	case header.Execution != nil && header.ExecutionBlockHash == "":
		blockNumber, err := strconv.ParseUint(header.Execution.BlockNumber, 10, 64)
		if err != nil {
			return ExecutionSummary{}, nil, fmt.Errorf("parse execution block number: %w", err)
		}
		timestamp, err := strconv.ParseUint(header.Execution.Timestamp, 10, 64)
		if err != nil {
			return ExecutionSummary{}, nil, fmt.Errorf("parse execution timestamp: %w", err)
		}
		return ExecutionSummary{StateRoot: header.Execution.StateRoot, Timestamp: timestamp, BlockNumber: blockNumber}, nil, nil
	case header.IsGloasFormat():
		if source == nil {
			return ExecutionSummary{}, nil, fmt.Errorf("light client header is in the Gloas format but no execution node is configured to fetch its execution header")
		}
		blockHash := common.HexToHash(header.ExecutionBlockHash)
		raw, err := source.RawHeaderByHash(ctx, blockHash)
		if err != nil {
			return ExecutionSummary{}, nil, fmt.Errorf("fetch execution header %s: %w", blockHash, err)
		}
		summary, err := DecodeExecutionHeaderRLP(raw, blockHash)
		if err != nil {
			return ExecutionSummary{}, nil, err
		}
		return summary, raw, nil
	default:
		return ExecutionSummary{}, nil, fmt.Errorf("light client header must set exactly one of execution and execution_block_hash")
	}
}

// DecodeExecutionHeaderRLP checks that raw hashes to expectedHash and reads the state root,
// number and timestamp out of it, as the wasm client does. Only the first twelve items are read:
// fields later forks append are bound by the hash and need no handling.
func DecodeExecutionHeaderRLP(raw []byte, expectedHash common.Hash) (ExecutionSummary, error) {
	if len(raw) > maxExecutionHeaderRLPBytes {
		return ExecutionSummary{}, fmt.Errorf("execution header rlp is %d bytes, exceeding the %d byte limit", len(raw), maxExecutionHeaderRLPBytes)
	}
	if got := crypto.Keccak256Hash(raw); got != expectedHash {
		return ExecutionSummary{}, fmt.Errorf("execution header rlp hashes to %s, but the proven execution block hash is %s", got, expectedHash)
	}

	payload, rest, err := rlp.SplitList(raw)
	if err != nil {
		return ExecutionSummary{}, fmt.Errorf("execution header rlp: %w", err)
	}
	if len(rest) != 0 {
		return ExecutionSummary{}, fmt.Errorf("execution header rlp has %d trailing bytes", len(rest))
	}

	var (
		stateRoot   common.Hash
		blockNumber uint64
		timestamp   uint64
	)
	for index := 0; index <= executionHeaderTimestampIndex; index++ {
		kind, content, tail, err := rlp.Split(payload)
		if err != nil {
			return ExecutionSummary{}, fmt.Errorf("execution header rlp item %d: %w", index, err)
		}
		if kind == rlp.List {
			return ExecutionSummary{}, fmt.Errorf("execution header rlp item %d is a list", index)
		}
		item := payload[:len(payload)-len(tail)]
		payload = tail

		switch index {
		case executionHeaderStateRootIndex:
			if len(content) != common.HashLength {
				return ExecutionSummary{}, fmt.Errorf("execution header state root is %d bytes", len(content))
			}
			stateRoot = common.BytesToHash(content)
		case executionHeaderNumberIndex:
			if err := rlp.DecodeBytes(item, &blockNumber); err != nil {
				return ExecutionSummary{}, fmt.Errorf("execution header number: %w", err)
			}
		case executionHeaderTimestampIndex:
			if err := rlp.DecodeBytes(item, &timestamp); err != nil {
				return ExecutionSummary{}, fmt.Errorf("execution header timestamp: %w", err)
			}
		}
	}

	return ExecutionSummary{StateRoot: stateRoot.Hex(), Timestamp: timestamp, BlockNumber: blockNumber}, nil
}

// FinalizedExecutionBlockNumber returns the execution block number a light client header
// finalizes. A pre-Gloas-format header states it; a Gloas-format header names only a block hash,
// so the number is read from the node, which is much cheaper than fetching and verifying the full
// RLP header and enough for height gating.
func FinalizedExecutionBlockNumber(ctx context.Context, client *ethclient.Client, header LightClientHeader) (uint64, error) {
	if header.Execution != nil && header.ExecutionBlockHash == "" {
		return strconv.ParseUint(header.Execution.BlockNumber, 10, 64)
	}
	if !header.IsGloasFormat() {
		return 0, fmt.Errorf("light client header must set exactly one of execution and execution_block_hash")
	}
	if client == nil {
		return 0, fmt.Errorf("light client header is in the Gloas format but no execution node is configured")
	}
	blockHash := common.HexToHash(header.ExecutionBlockHash)
	execHeader, err := client.HeaderByHash(ctx, blockHash)
	if err != nil {
		return 0, fmt.Errorf("fetch execution header %s: %w", blockHash, err)
	}
	return execHeader.Number.Uint64(), nil
}

// EthRawHeaderSource fetches raw execution headers from an Ethereum JSON-RPC endpoint.
type EthRawHeaderSource struct {
	Client *ethclient.Client
}

// RawHeaderByHash asks the node for the stored RLP header with debug_getRawHeader, which
// returns the exact bytes the block hash commits to. A node without the debug namespace is
// served by re-encoding the decoded header, accepted only if it still hashes to the requested
// hash, i.e. when the client library knows every field of the current fork.
func (s EthRawHeaderSource) RawHeaderByHash(ctx context.Context, hash common.Hash) ([]byte, error) {
	var raw hexutil.Bytes
	rpcErr := s.Client.Client().CallContext(ctx, &raw, "debug_getRawHeader", hash)
	if rpcErr == nil && len(raw) > 0 {
		return raw, nil
	}

	header, err := s.Client.HeaderByHash(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("debug_getRawHeader failed (%v) and eth_getBlockByHash failed: %w", rpcErr, err)
	}
	encoded, err := rlp.EncodeToBytes(header)
	if err != nil {
		return nil, fmt.Errorf("re-encode execution header: %w", err)
	}
	if !bytes.Equal(crypto.Keccak256(encoded), hash.Bytes()) {
		return nil, fmt.Errorf("debug_getRawHeader is unavailable (%v) and the re-encoded header does not hash to %s: the node has fields this relayer's go-ethereum does not know; point the relayer at a node with the debug namespace", rpcErr, hash)
	}
	return encoded, nil
}
