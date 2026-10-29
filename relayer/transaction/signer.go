package transaction

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"relayer/keys"
)

// Signer supplies the relayer's ordinary Ethereum and Cosmos signing keys.
// Implementations are installed before the first send, which keeps key-source
// selection auditable and gives a future external signer one integration point.
type Signer interface {
	EthKey() (*ecdsa.PrivateKey, error)
	CosmosKeyBytes() ([]byte, error)
}

type envSigner struct {
	ethOnce sync.Once
	ethKey  *ecdsa.PrivateKey
	ethErr  error

	cosmosOnce  sync.Once
	cosmosBytes []byte
	cosmosErr   error
}

// NewEnvSigner returns the default signer backed by ETH_PRIVATE_KEY and
// COSMOS_PRIVATE_KEY. Each key is parsed at most once.
func NewEnvSigner() Signer { return &envSigner{} }

func (s *envSigner) EthKey() (*ecdsa.PrivateKey, error) {
	s.ethOnce.Do(func() {
		raw := os.Getenv("ETH_PRIVATE_KEY")
		if raw == "" {
			s.ethErr = fmt.Errorf("ETH_PRIVATE_KEY environment variable is required in .env file")
			return
		}
		s.ethKey, s.ethErr = keys.RestoreKey(raw)
		if s.ethErr != nil {
			s.ethErr = fmt.Errorf("failed to restore ETH private key: %w", s.ethErr)
		}
	})
	return s.ethKey, s.ethErr
}

func (s *envSigner) CosmosKeyBytes() ([]byte, error) {
	s.cosmosOnce.Do(func() {
		raw := strings.TrimPrefix(os.Getenv("COSMOS_PRIVATE_KEY"), "0x")
		if raw == "" {
			s.cosmosErr = fmt.Errorf("COSMOS_PRIVATE_KEY environment variable is required in .env file")
			return
		}
		s.cosmosBytes, s.cosmosErr = hex.DecodeString(raw)
		if s.cosmosErr != nil {
			s.cosmosErr = fmt.Errorf("failed to decode COSMOS_PRIVATE_KEY: %w", s.cosmosErr)
		}
	})
	if s.cosmosErr != nil {
		return nil, s.cosmosErr
	}
	out := make([]byte, len(s.cosmosBytes))
	copy(out, s.cosmosBytes)
	return out, nil
}
