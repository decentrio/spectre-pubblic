package services

import (
	"encoding/binary"
	"fmt"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// IBC path types, the byte between the client id and the sequence in every
// ICS-24 path the router stores (contracts/core/libraries/ICS24Host.sol).
const (
	EthPathCommitment byte = 1
	EthPathReceipt    byte = 2
	EthPathAck        byte = 3
)

// EthPrevSequenceSendKey is the storage key of
// IBCStoreStorage.prevSequenceSends[clientID] on the router.
//
// The router exposes no getter for it: nextSequenceSend is internal and
// increments as it reads (contracts/core/store/IBCStoreUpgradeable.sol). The
// value is still public state, so it is read the way every other IBC slot on the
// router already is -- straight from storage. IBCStoreStorage puts `commitments`
// at the ERC-7201 base slot and `prevSequenceSends` at base+1, and Solidity keys
// a mapping(string => v) entry at keccak256(bytes(key) ‖ slot).
//
// It is the only upper bound an enumeration of EVM commitments can have:
// storage is a mapping whose keys cannot be listed, so "which sequences are
// outstanding" has to be a bounded probe, and this is what bounds it.
func EthPrevSequenceSendKey(clientID string) ethcommon.Hash {
	slot := new(big.Int).SetBytes(ethcommon.HexToHash(ICS26_IBC_STORAGE_SLOT).Bytes())
	slot.Add(slot, big.NewInt(1))
	return crypto.Keccak256Hash([]byte(clientID), ethcommon.BigToHash(slot).Bytes())
}

// EthStorageUint64 decodes a uint64 that occupies a storage word on its own:
// right-aligned in the 32 bytes, the high 24 zero. Anything else means the slot
// is not the one this reader thinks it is, which is worth an error rather than
// a truncated number.
func EthStorageUint64(word []byte) (uint64, error) {
	if len(word) != 32 {
		return 0, fmt.Errorf("storage word is %d bytes, want 32", len(word))
	}
	for _, b := range word[:24] {
		if b != 0 {
			return 0, fmt.Errorf("storage word %x does not hold a uint64", word)
		}
	}
	return binary.BigEndian.Uint64(word[24:]), nil
}
