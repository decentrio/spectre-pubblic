package transaction

import (
	"testing"
)

func TestEnvSigner_CosmosKeyBytesAreCopied(t *testing.T) {
	t.Setenv("COSMOS_PRIVATE_KEY", testCosmosPrivKeyHex)
	signer := NewEnvSigner()
	first, err := signer.CosmosKeyBytes()
	if err != nil {
		t.Fatalf("first CosmosKeyBytes: %v", err)
	}
	first[0] ^= 0xff
	second, err := signer.CosmosKeyBytes()
	if err != nil {
		t.Fatalf("second CosmosKeyBytes: %v", err)
	}
	if first[0] == second[0] {
		t.Fatal("CosmosKeyBytes returned shared backing storage")
	}
}
