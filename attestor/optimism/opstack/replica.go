package opstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	attestortypes "attestor/types"
	"attestor/types/redact"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// SyncStatus carries the replica heads the attestor gates on. Unsafe is
// sequencer gossip; Safe is derived from L1 batch data (reorgable with L1);
// Finalized is derived from finalized L1 data — "tx finality", irreversible.
type SyncStatus struct {
	UnsafeL2    uint64
	SafeL2      uint64
	FinalizedL2 uint64
}

// ReplicaClient talks to the op-node of the verify-mode replica.
type ReplicaClient struct {
	c *rpc.Client
}

// DialReplica connects to the op-node RPC. An HTTP(S) endpoint carries the same
// transport timeout DialEthRPC gives the L1 client: a reachable op-node can
// accept a request and never answer, and go-ethereum applies no client timeout
// itself. A WebSocket or IPC endpoint carries none; the caller's context is
// what bounds a call on it (the run loop's call bound).
func DialReplica(ctx context.Context, url string, timeout time.Duration) (*ReplicaClient, error) {
	c, err := attestortypes.DialRPC(ctx, url, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to dial op-node rpc %s: %w", redact.URL(url), redact.Error(err))
	}
	return &ReplicaClient{c: c}, nil
}

func (r *ReplicaClient) Close() {
	r.c.Close()
}

// L2ChainID reads the rollup's L2 identity from op-node itself. Unlike
// eth_chainId, optimism_rollupConfig belongs to the documented op-node RPC
// surface, so this works with the standard op-node endpoint (port 9545)
// without also requiring an execution-client RPC endpoint.
func (r *ReplicaClient) L2ChainID(ctx context.Context) (*big.Int, error) {
	var config struct {
		L2ChainID json.RawMessage `json:"l2_chain_id"`
	}
	if err := r.c.CallContext(ctx, &config, "optimism_rollupConfig"); err != nil {
		return nil, fmt.Errorf("optimism_rollupConfig failed: %w", err)
	}

	chainID, err := parseRollupChainID(config.L2ChainID)
	if err != nil {
		return nil, fmt.Errorf("optimism_rollupConfig returned invalid l2_chain_id: %w", err)
	}
	return chainID, nil
}

// parseRollupChainID accepts the decimal JSON number returned by op-node and
// the quoted hexadecimal form used by some JSON-RPC tooling.
func parseRollupChainID(raw json.RawMessage) (*big.Int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing value")
	}

	value := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("decode quoted value: %w", err)
		}
	}
	chainID, ok := new(big.Int).SetString(value, 0)
	if !ok {
		return nil, fmt.Errorf("%q is not an integer", value)
	}
	return chainID, nil
}

// l2BlockRef is the subset of op-node's L2BlockRef the attestor reads.
type l2BlockRef struct {
	Number uint64      `json:"number"`
	Hash   common.Hash `json:"hash"`
}

type syncStatusResponse struct {
	UnsafeL2    l2BlockRef `json:"unsafe_l2"`
	SafeL2      l2BlockRef `json:"safe_l2"`
	FinalizedL2 l2BlockRef `json:"finalized_l2"`
}

// SyncStatus returns the replica's current unsafe, safe and finalized L2
// heads via optimism_syncStatus.
func (r *ReplicaClient) SyncStatus(ctx context.Context) (SyncStatus, error) {
	var resp syncStatusResponse
	if err := r.c.CallContext(ctx, &resp, "optimism_syncStatus"); err != nil {
		return SyncStatus{}, fmt.Errorf("optimism_syncStatus failed: %w", err)
	}
	return SyncStatus{
		UnsafeL2:    resp.UnsafeL2.Number,
		SafeL2:      resp.SafeL2.Number,
		FinalizedL2: resp.FinalizedL2.Number,
	}, nil
}

// L2Commitment is the replica's own view of one L2 block: the output root it
// recomputed, plus the block identity that root is derived from.
//
// The relayer needs BlockHash and StateRoot to answer "is the block I am about
// to package the one you hold at this height" (VerifyStateRoot). It cannot use
// OutputRoot for that: the header it builds carries a state root, and turning
// that into an output root would need the message-passer storage root, i.e. a
// second proof against a contract the attestor-trusted client does not verify.
type L2Commitment struct {
	BlockNumber uint64
	BlockHash   common.Hash
	StateRoot   common.Hash
	OutputRoot  [32]byte
}

// outputAtBlockResponse is the subset of optimism_outputAtBlock the attestor
// reads. blockRef and stateRoot ride along in the same response as outputRoot,
// so reading them costs no extra round trip.
type outputAtBlockResponse struct {
	OutputRoot hexutil.Bytes `json:"outputRoot"`
	BlockRef   l2BlockRef    `json:"blockRef"`
	StateRoot  common.Hash   `json:"stateRoot"`
}

// outputAtBlock issues the RPC and validates only what every caller needs.
func (r *ReplicaClient) outputAtBlock(ctx context.Context, l2Block uint64) (outputAtBlockResponse, error) {
	var resp outputAtBlockResponse
	if err := r.c.CallContext(ctx, &resp, "optimism_outputAtBlock", hexutil.Uint64(l2Block)); err != nil {
		return outputAtBlockResponse{}, fmt.Errorf("optimism_outputAtBlock(%d) failed: %w", l2Block, err)
	}
	if len(resp.OutputRoot) != 32 {
		return outputAtBlockResponse{}, fmt.Errorf("optimism_outputAtBlock(%d) returned %d-byte output root, want 32", l2Block, len(resp.OutputRoot))
	}
	return resp, nil
}

// OutputAtBlock returns the replica's self-computed output root at the given
// L2 block via optimism_outputAtBlock.
func (r *ReplicaClient) OutputAtBlock(ctx context.Context, l2Block uint64) ([32]byte, error) {
	resp, err := r.outputAtBlock(ctx, l2Block)
	if err != nil {
		return [32]byte{}, err
	}
	var root [32]byte
	copy(root[:], resp.OutputRoot)
	return root, nil
}

// CommitmentAt returns the replica's canonical block identity at l2Block.
//
// It checks that the reply is *for* the requested block, which OutputAtBlock
// deliberately does not: a caller comparing roots at a height it chose can live
// with an off-by-one, but a caller asking "is this the block you hold at height
// N" cannot — a reply about a different block would be compared as if it were
// about N, and answer valid=false for a perfectly good header.
//
// A block the replica has not derived yet comes back as an RPC error from
// op-node rather than a zero commitment, so "not available" never reads as a
// mismatch.
func (r *ReplicaClient) CommitmentAt(ctx context.Context, l2Block uint64) (L2Commitment, error) {
	resp, err := r.outputAtBlock(ctx, l2Block)
	if err != nil {
		return L2Commitment{}, err
	}
	if resp.BlockRef.Number != l2Block {
		return L2Commitment{}, fmt.Errorf(
			"optimism_outputAtBlock(%d) answered for block %d", l2Block, resp.BlockRef.Number)
	}
	var out L2Commitment
	copy(out.OutputRoot[:], resp.OutputRoot)
	out.BlockNumber = resp.BlockRef.Number
	out.BlockHash = resp.BlockRef.Hash
	out.StateRoot = resp.StateRoot
	return out, nil
}
