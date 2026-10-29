// Package core defines the domain-neutral boundary between an attestor plugin
// and its transport/lifecycle host. It intentionally carries opaque feed
// values: OP output roots and Arbitrum state roots are not made equivalent by
// sharing these DTOs.
package core

import "time"

// RunMode is the finality level at which a canonical block identity was
// checked and signed.
type RunMode string

const (
	RunModeUnsafe    RunMode = "unsafe"
	RunModeSafe      RunMode = "safe"
	RunModeFinalized RunMode = "finalized"
)

// Valid reports whether the value can be used for block verification.
func (m RunMode) Valid() bool {
	switch m {
	case RunModeUnsafe, RunModeSafe, RunModeFinalized:
		return true
	default:
		return false
	}
}

// AttestationPolicy controls whether a read may return a root that still
// awaits a plugin-specific finalized recheck.
type AttestationPolicy struct {
	IncludeProvisional bool
}

// AttestedRoot is an opaque, already durable feed entry. Root has no generic
// comparison semantics: its bytes are an OP output root for OP Stack and an
// execution state root for Arbitrum.
type AttestedRoot struct {
	L2BlockNumber uint64
	Root          []byte
	Source        string
	GameIndex     *uint64
	AssertionHash []byte
	Provisional   bool
	AttestedAt    time.Time
}

// Clone returns a transport-safe copy with no shared byte slices.
func (r AttestedRoot) Clone() AttestedRoot {
	r.Root = append([]byte(nil), r.Root...)
	r.AssertionHash = append([]byte(nil), r.AssertionHash...)
	if r.GameIndex != nil {
		gameIndex := *r.GameIndex
		r.GameIndex = &gameIndex
	}
	return r
}

// BlockIdentityRequest is the canonical L2 block a relayer candidate must
// match and attest. The router and active-set hash are part of the signature
// statement, binding an otherwise valid L2 block to precisely one wasm client
// configuration. ExpectedBlockHash is optional; ExpectedStateRoot is always
// present once decoded by the transport adapter.
type BlockIdentityRequest struct {
	BlockNumber       uint64
	ExpectedStateRoot [32]byte
	ExpectedBlockHash *[32]byte
	RunMode           RunMode
	L2Router          [20]byte
	AttestorSetHash   [32]byte
}

// SignedBlockIdentityVerdict is a canonical block identity returned by a
// plugin. A positive verdict must have an Ed25519 signature; a mismatch is a
// valid=false result rather than a transport error.
type SignedBlockIdentityVerdict struct {
	Valid       bool
	BlockNumber uint64
	BlockHash   [32]byte
	StateRoot   [32]byte
	Signature   []byte
}

// Clone returns a transport-safe copy with no shared signature bytes.
func (v SignedBlockIdentityVerdict) Clone() SignedBlockIdentityVerdict {
	v.Signature = append([]byte(nil), v.Signature...)
	return v
}

// FeedStatus is a local, read-only health snapshot. It must describe plugin
// readiness, not merely whether the gRPC process has a listening socket.
type FeedStatus struct {
	SrcChain         string
	AttestationHead  RunMode
	Ready            bool
	ReplicaSeen      bool
	ReplicaUnsafe    uint64
	ReplicaSafe      uint64
	ReplicaFinalized uint64
	Pending          uint64
}

// WatchRequest controls an optional frontier stream.
type WatchRequest struct {
	Policy AttestationPolicy
}

// FrontierUpdate is the authoritative frontier at one instant. Found=false
// represents a frontier that disappeared after a provisional revocation.
type FrontierUpdate struct {
	Root  AttestedRoot
	Found bool
}
