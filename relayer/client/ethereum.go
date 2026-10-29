// This package names its files after the party being talked to -- tendermint.go,
// beacon.go, dial.go -- so this one holds what talks to an EVM JSON-RPC endpoint
// and nothing else. Every function here takes an *ethclient.Client.
//
// The name stays ethereum.go rather than becoming ethproof.go: naming one file
// by its CONTENT while its neighbours are named by their counterparty leaves the
// next person with no rule to follow. (evm.go was considered and dropped -- the
// package already says "Eth" across its function names, and DialEthRPC opens L2
// connections too. Settling on "EVM" is E2's job, and renaming the file while
// leaving the functions would be half a change.)
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// EthGetProof calls eth_getProof for addr + keys at blockNumber (nil = latest) and
// decodes the hex nodes/values to raw bytes.
//
// ctx is honoured: this used to pass context.Background(), and go-ethereum's HTTP
// client carries no timeout of its own, so a node that accepted the connection and
// then stopped responding blocked the caller forever. The L2 header builders call
// this from the relay loop, which drives one direction on a single goroutine — one
// unanswered proof request wedged that whole direction silently, with nothing in the
// log to say so. Callers must pass a cancellable or deadline-bearing context.
func EthGetProof(ctx context.Context, client *ethclient.Client, addr ethcommon.Address, keys []ethcommon.Hash, blockNumber *big.Int) (*RawEvmProof, error) {
	keyStrs := make([]string, len(keys))
	for i, k := range keys {
		keyStrs[i] = k.Hex()
	}
	var result ethProofResult
	if err := client.Client().CallContext(
		ctx, &result, "eth_getProof",
		addr, keyStrs, toBlockNumArg(blockNumber),
	); err != nil {
		return nil, fmt.Errorf("eth_getProof(%s): %w", addr, err)
	}
	accountNodes, err := decodeHexNodes(result.AccountProof)
	if err != nil {
		return nil, fmt.Errorf("eth_getProof(%s) account proof: %w", addr, err)
	}
	storage := make([]RawStorageProof, len(result.StorageProof))
	for i, sp := range result.StorageProof {
		nodes, err := decodeHexNodes(sp.Proof)
		if err != nil {
			return nil, fmt.Errorf("eth_getProof(%s) storage proof %d: %w", addr, i, err)
		}
		var val []byte
		if sp.Value != nil {
			val = sp.Value.ToInt().Bytes() // minimal big-endian; zero → empty
		}
		storage[i] = RawStorageProof{Key: sp.Key, Value: val, Proof: nodes}
	}
	return &RawEvmProof{AccountProof: accountNodes, StorageHash: result.StorageHash, Storage: storage}, nil
}

// decodeHexNodes decodes a list of 0x-hex MPT node strings to raw bytes.
func decodeHexNodes(nodes []string) ([][]byte, error) {
	out := make([][]byte, len(nodes))
	for i, n := range nodes {
		b, err := hexutil.Decode(n)
		if err != nil {
			return nil, fmt.Errorf("decode node %d: %w", i, err)
		}
		out[i] = b
	}
	return out, nil
}

// GetEthMembershipProof generates a JSON-encoded MembershipProof for MsgAcknowledgement.ProofAcked.
//
// ackPath is the raw IBC path bytes: destClientID + [0x03] + sequence.to_be_bytes(8)
// slot is the ICS26Router ibc_commitment_slot (ICS26_IBC_STORAGE_SLOT constant)
// blockNumber is the ETH block to prove against (use nil for latest)
//
// ctx is honoured, for the reason spelled out on EthGetProof: this is called from
// the ETH->Cosmos relay loop, which drives that direction on a single goroutine,
// so an unanswered eth_getProof used to wedge the whole direction silently.
// Callers must pass a cancellable or deadline-bearing context.
func GetEthMembershipProof(ctx context.Context, client *ethclient.Client, contractAddr ethcommon.Address, ackPath []byte, slot ethcommon.Hash, blockNumber *big.Int) ([]byte, error) {
	// storage_key = keccak256(keccak256(ackPath) ++ slot)
	pathHash := crypto.Keccak256(ackPath)
	storageKey := crypto.Keccak256Hash(pathHash, slot.Bytes())

	var result ethProofResult
	err := client.Client().CallContext(
		ctx,
		&result,
		"eth_getProof",
		contractAddr,
		[]string{storageKey.Hex()},
		toBlockNumArg(blockNumber),
	)
	if err != nil {
		return nil, fmt.Errorf("eth_getProof failed: %w", err)
	}
	if len(result.StorageProof) == 0 {
		return nil, fmt.Errorf("eth_getProof returned no storage proofs")
	}

	sp := result.StorageProof[0]
	valueStr := "0x0"
	if sp.Value != nil {
		valueStr = sp.Value.String()
	}

	// The mirror of the guard GetEthNonMembershipProof already has. An empty slot
	// is not a commitment, and a "proof" of zero is not a proof: it is accepted
	// here, submitted, and rejected on chain, which costs a transaction and reads
	// as a deterministic revert -- so a packet that was merely early gets dropped.
	if sp.Value == nil || sp.Value.ToInt().Sign() == 0 {
		return nil, fmt.Errorf("storage slot empty at key %s for block %s: %w",
			storageKey.Hex(), toBlockNumArg(blockNumber), ErrCommitmentAbsent)
	}

	proof := MembershipProof{
		AccountProof: accountProofData{
			StorageRoot: result.StorageHash.Hex(),
			Proof:       result.AccountProof,
		},
		StorageProof: storageProofData{
			Key:   storageKey.Hex(),
			Value: valueStr,
			Proof: sp.Proof,
		},
	}
	return json.Marshal(proof)
}

func toBlockNumArg(number *big.Int) string {
	if number == nil {
		return "latest"
	}
	return hexutil.EncodeBig(number)
}

// GetL2BootstrapState reads the bootstrap roots for the L2 IBC handler at
// blockNumber (nil = latest): the block's execution state root + timestamp, and
// the ICS26Router account's storage root via eth_getProof (an empty storage-key
// list still returns the account's storageHash).
func GetL2BootstrapState(client *ethclient.Client, routerAddr ethcommon.Address, blockNumber *big.Int) (L2BootstrapState, error) {
	header, err := client.HeaderByNumber(context.Background(), blockNumber)
	if err != nil {
		return L2BootstrapState{}, fmt.Errorf("l2 genesis: header by number: %w", err)
	}
	var proof ethProofResult
	if err := client.Client().CallContext(
		context.Background(), &proof, "eth_getProof",
		routerAddr, []string{}, toBlockNumArg(header.Number),
	); err != nil {
		return L2BootstrapState{}, fmt.Errorf("l2 genesis: eth_getProof(%s): %w", routerAddr, err)
	}
	if proof.StorageHash == (ethcommon.Hash{}) {
		return L2BootstrapState{}, fmt.Errorf("l2 genesis: router %s has no storage root at block %d (not a contract?)", routerAddr, header.Number)
	}
	return L2BootstrapState{
		Height:            header.Number.Uint64(),
		BlockHash:         header.Hash(),
		ParentHash:        header.ParentHash,
		StateRoot:         header.Root,
		RouterStorageRoot: proof.StorageHash,
		TimestampSeconds:  header.Time,
	}, nil
}

// GetEthNonMembershipProof generates a JSON-encoded MembershipProof for MsgTimeout.ProofUnreceived.
//
// It proves that no packet receipt commitment exists at the given IBC path on the ICS26Router contract.
// The storage value at the computed key must be zero (empty), proving the packet was never received.
//
// receiptPath is the raw IBC path bytes: destClientID + [0x02] + sequence.to_be_bytes(8)
// slot is the ICS26Router ibc_commitment_slot (ICS26_IBC_STORAGE_SLOT constant)
// blockNumber is the ETH block to prove against (use nil for latest)
//
// ctx is honoured, for the reason spelled out on EthGetProof. This one is called
// from the timeout scanner, which relay.Module runs synchronously on its scan
// goroutine (module.go scanLoop), so an unanswered eth_getProof used to stop the
// scanner for good — expired packets never refunded, escrow locked indefinitely.
// Callers must pass a cancellable or deadline-bearing context.
func GetEthNonMembershipProof(ctx context.Context, client *ethclient.Client, contractAddr ethcommon.Address, receiptPath []byte, slot ethcommon.Hash, blockNumber *big.Int) ([]byte, error) {
	// storage_key = keccak256(keccak256(receiptPath) ++ slot)
	pathHash := crypto.Keccak256(receiptPath)
	storageKey := crypto.Keccak256Hash(pathHash, slot.Bytes())

	var result ethProofResult
	err := client.Client().CallContext(
		ctx,
		&result,
		"eth_getProof",
		contractAddr,
		[]string{storageKey.Hex()},
		toBlockNumArg(blockNumber),
	)
	if err != nil {
		return nil, fmt.Errorf("eth_getProof failed: %w", err)
	}
	if len(result.StorageProof) == 0 {
		return nil, fmt.Errorf("eth_getProof returned no storage proofs")
	}

	sp := result.StorageProof[0]
	valueStr := "0x0"
	if sp.Value != nil {
		valueStr = sp.Value.String()
	}

	// Verify the value is empty (proving non-membership)
	if sp.Value != nil && sp.Value.ToInt().Cmp(big.NewInt(0)) != 0 {
		return nil, fmt.Errorf("storage slot not empty at key %s: value=%s (expected 0 for non-membership): %w", storageKey.Hex(), valueStr, ErrPacketAlreadyReceived)
	}

	proof := MembershipProof{
		AccountProof: accountProofData{
			StorageRoot: result.StorageHash.Hex(),
			Proof:       result.AccountProof,
		},
		StorageProof: storageProofData{
			Key:   storageKey.Hex(),
			Value: valueStr,
			Proof: sp.Proof,
		},
	}
	return json.Marshal(proof)
}
