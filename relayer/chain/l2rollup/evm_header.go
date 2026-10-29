package l2rollup

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// This file holds the deterministic mapping from a go-ethereum execution header to
// the wire header the L2 client hashes — the part that must match
// packages/l2-client/src/canonical_header.rs byte-for-byte, and so is unit-tested
// rather than only exercised on a devnet.
//
// It used to also carry the OP-Stack settlement helpers (DisputeGameFactory game-list
// slot arithmetic, gameCount(), storage-word address decoding). Those went with the
// settlement builders: the attestor-trusted client verifies no L1 object, so nothing
// reads a factory or a game any more.

// toCanonicalHeader maps a go-ethereum execution header to the CanonicalEvmHeader the
// L2 client hashes. Optional Shanghai/Cancun/Prague fields are carried through exactly
// as present or absent, because validate_for_fork matches that set against the fork
// pinned in the profile and the RLP hashing must reproduce the same block hash.
func toCanonicalHeader(h *types.Header) CanonicalEvmHeader {
	ch := CanonicalEvmHeader{
		ParentHash:       h.ParentHash.Bytes(),
		OmmersHash:       h.UncleHash.Bytes(),
		Beneficiary:      h.Coinbase.Bytes(),
		StateRoot:        h.Root.Bytes(),
		TransactionsRoot: h.TxHash.Bytes(),
		ReceiptsRoot:     h.ReceiptHash.Bytes(),
		LogsBloom:        h.Bloom.Bytes(),
		Difficulty:       u256FromBig(h.Difficulty),
		Number:           h.Number.Uint64(),
		GasLimit:         h.GasLimit,
		GasUsed:          h.GasUsed,
		Timestamp:        h.Time,
		ExtraData:        h.Extra,
		MixHash:          h.MixDigest.Bytes(),
		Nonce:            h.Nonce[:],
	}
	if h.BaseFee != nil {
		bf := u256FromBig(h.BaseFee)
		ch.BaseFeePerGas = &bf
	}
	if h.WithdrawalsHash != nil {
		wr := hexBytes(h.WithdrawalsHash.Bytes())
		ch.WithdrawalsRoot = &wr
	}
	if h.BlobGasUsed != nil {
		v := *h.BlobGasUsed
		ch.BlobGasUsed = &v
	}
	if h.ExcessBlobGas != nil {
		v := *h.ExcessBlobGas
		ch.ExcessBlobGas = &v
	}
	if h.ParentBeaconRoot != nil {
		pb := hexBytes(h.ParentBeaconRoot.Bytes())
		ch.ParentBeaconBlockRoot = &pb
	}
	if h.RequestsHash != nil {
		rh := hexBytes(h.RequestsHash.Bytes())
		ch.RequestsHash = &rh
	}
	return ch
}

// u256FromBig builds the wire u256 from a big.Int (nil → 0).
func u256FromBig(v *big.Int) u256 {
	var u u256
	if v != nil {
		u.Int.Set(v)
	}
	return u
}
