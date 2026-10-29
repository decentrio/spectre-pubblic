package services

import (
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// The slot the enumeration probes for prevSequenceSend is derived from the
// router's ERC-7201 layout and Solidity's mapping(string => v) key rule; a
// wrong derivation reads zero forever and the backstop reports nothing
// outstanding on every chain. The vectors are Foundry's own:
//
//	cast index string "client-0"  0x…7601 → 0x3f26…c27f
//	cast index string "08-wasm-0" 0x…7601 → 0x16a9…852a
//
// where 0x…7601 is the IBCStore base slot + 1, the position of prevSequenceSends.
func TestEthPrevSequenceSendKey_MatchesSolidityMappingLayout(t *testing.T) {
	for clientID, want := range map[string]string{
		"client-0":  "0x3f26560d696c7c208e3fbf3612fb9dbeae9a1e755ce5029e04347185bdd0c27f",
		"08-wasm-0": "0x16a991a1b20bb64abc8d3aa70fe37a2f84fd1264e4a07b9748e3cb216996852a",
	} {
		if got := EthPrevSequenceSendKey(clientID).Hex(); got != want {
			t.Fatalf("EthPrevSequenceSendKey(%q) = %s, want %s (cast index string)", clientID, got, want)
		}
	}
}

func TestEthStorageUint64(t *testing.T) {
	word := ethcommon.BigToHash(ethcommon.Big1).Bytes()
	word[31] = 0x2a
	got, err := EthStorageUint64(word)
	if err != nil || got != 42 {
		t.Fatalf("EthStorageUint64(…2a) = (%d, %v), want 42: the value is right-aligned in the word", got, err)
	}

	if _, err := EthStorageUint64(word[:31]); err == nil {
		t.Fatal("a 31-byte word must be rejected, not decoded")
	}

	dirty := make([]byte, 32)
	dirty[0] = 1
	if _, err := EthStorageUint64(dirty); err == nil {
		t.Fatal("a word with high bytes set does not hold a uint64 and must be rejected: reading it as one would name the wrong slot silently")
	}
}
