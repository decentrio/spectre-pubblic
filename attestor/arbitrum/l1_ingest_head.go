package arbitrum

import "fmt"

// L1IngestHead selects the Ethereum view used to discover BoLD assertions.
// It is independent of the Nitro L2 attestation head.
type L1IngestHead string

const (
	L1IngestFinalized L1IngestHead = "finalized"
	L1IngestSafe      L1IngestHead = "safe"
	L1IngestLatest    L1IngestHead = "latest"
)

// Normalize defaults an empty selector to finalized and validates explicit values.
func (h L1IngestHead) Normalize() (L1IngestHead, error) {
	switch h {
	case "", L1IngestFinalized:
		return L1IngestFinalized, nil
	case L1IngestSafe, L1IngestLatest:
		return h, nil
	default:
		return "", fmt.Errorf("l1_ingest_head must be finalized, safe or latest, got %q", h)
	}
}
