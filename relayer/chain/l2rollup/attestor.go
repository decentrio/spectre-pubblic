package l2rollup

import (
	"context"
	"errors"
	"fmt"
	"sort"

	attestorpb "attestor/types/attestor"
	"relayer/chain"
)

type attestorFailure struct {
	cause   error
	outcome chain.Outcome
}

func newAttestorFailure(tag string, index uint16, cause error) attestorFailure {
	indexed := fmt.Errorf("attestor index %d: %w", index, cause)
	classified := classifyAttestorFailure(tag, indexed)
	return attestorFailure{cause: indexed, outcome: classified.Outcome()}
}

// quorumFailure classifies the aggregate by whether retrying the non-permanent
// members could still satisfy the threshold. Raw causes are joined so callers
// retain errors.Is/errors.As access without inheriting contradictory retry tags.
func quorumFailure(tag, description string, valid, waiting, threshold int, failures []attestorFailure) error {
	recoverable := valid + waiting
	causes := make([]error, 0, len(failures))
	for _, failure := range failures {
		causes = append(causes, failure.cause)
		if failure.outcome != chain.OutcomePermanent {
			recoverable++
		}
	}
	cause := errors.Join(causes...)
	err := fmt.Errorf("%s: only %d %s; need %d: %w", tag, valid, description, threshold, cause)
	if recoverable >= threshold {
		return chain.Transient(err)
	}
	return chain.Permanent(err)
}

// AttestationFrontier is the narrow height-gating surface an L2 source needs.
// Keeping it separate from the signing client lets a source aggregate a quorum
// instead of making one endpoint a single point of failure.
type AttestationFrontier interface {
	AttestedUpTo(ctx context.Context, srcChain string, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error)
}

// AttestorClient is the subset of the L2 attestor sidecar's gRPC surface the relayer
// needs to build authenticated headers (AttestorService, #240/#258). It is defined
// HERE, on the consumer side, so source/builders stay unit-testable with a small fake
// instead of a full gRPC server. It returns the generated proto AttestedRoot directly
// — the attestor module is the single source of truth for that shape (attestor/types),
// so no parallel relayer-side struct is kept.
//
// The attestor independently RE-DERIVES L2 state from L1 (OP: a verify-mode op-node
// replica compares each DisputeGameFactory root claim against its own L1-derived
// output root; Arbitrum: a Nitro replay). So an attested root is a trust statement,
// not a relay of the on-chain proposal — which is exactly why RelayableHeight gates
// on it rather than on the raw L2 RPC head.
//
// AttestedUpTo/AttestedRootAtOrBelow are height gates: the relayer reads
// L2BlockNumber from them and nothing else. AttestedRoot.Root is deliberately not
// consumed there because it is chain-specific — an OP output root, but an Arbitrum
// L2 state root — and the chain-agnostic builder has no way to recompute the OP
// form without the settlement machinery the attestor-trusted design removed.
//
// VerificationRequest is both the identity an attestor must independently verify
// and the immutable wasm-client context it must sign. The contract reconstructs
// exactly this statement, so no field may be inferred from a relayer-only default.
type VerificationRequest struct {
	SrcChain        string
	BlockNumber     uint64
	StateRoot       []byte
	BlockHash       []byte
	RunMode         attestorpb.RunMode
	L2Router        [20]byte
	AttestorSetHash [32]byte
}

// SignedVerdict is the canonical block identity returned by one attestor. Valid
// false is a verified mismatch; errors are transport or request failures.
type SignedVerdict struct {
	Valid       bool
	BlockNumber uint64
	BlockHash   []byte
	StateRoot   []byte
	Signature   []byte
}

// VerifyStateRoot is how a built header is authenticated. A positive response
// includes the attestor's Ed25519 signature over the exact wasm-client statement.
type AttestorClient interface {
	AttestationFrontier
	// AttestedUpTo returns the highest L2 block the attestor has independently
	// confirmed for srcChain. includeProvisional accepts Safe-but-not-yet-finalized
	// roots (false = finalized only). found is false before the first attestation.
	AttestedUpTo(ctx context.Context, srcChain string, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error)
	// AttestedRootAtOrBelow returns the best attested root at or below l2BlockNumber —
	// the canonical commitment for a target height. found is false when nothing
	// qualifies at or below the bound.
	AttestedRootAtOrBelow(ctx context.Context, srcChain string, l2BlockNumber uint64, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error)
	VerifyStateRoot(ctx context.Context, request VerificationRequest) (SignedVerdict, error)
}

type quorumAttestationFrontier struct {
	attestors []SigningAttestor
	threshold int
}

// NewQuorumAttestationFrontier builds the source height gate from every configured
// endpoint. Calls run concurrently and return as soon as any threshold endpoints
// report a frontier, so one unavailable endpoint cannot stop a healthy quorum.
func NewQuorumAttestationFrontier(attestors []SigningAttestor, threshold uint16) (AttestationFrontier, error) {
	if threshold == 0 {
		return nil, fmt.Errorf("attestation frontier threshold must not be zero")
	}
	if len(attestors) < int(threshold) {
		return nil, fmt.Errorf("%d attestor endpoints cannot satisfy frontier threshold %d", len(attestors), threshold)
	}
	configured := append([]SigningAttestor(nil), attestors...)
	seen := make(map[uint16]struct{}, len(configured))
	for _, endpoint := range configured {
		if endpoint.Client == nil {
			return nil, fmt.Errorf("attestation frontier endpoint at index %d is nil", endpoint.Index)
		}
		if _, duplicate := seen[endpoint.Index]; duplicate {
			return nil, fmt.Errorf("duplicate attestation frontier endpoint index %d", endpoint.Index)
		}
		seen[endpoint.Index] = struct{}{}
	}
	return &quorumAttestationFrontier{attestors: configured, threshold: int(threshold)}, nil
}

func (q *quorumAttestationFrontier) AttestedUpTo(ctx context.Context, srcChain string, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error) {
	type result struct {
		index uint16
		root  *attestorpb.AttestedRoot
		found bool
		err   error
	}

	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(q.attestors))
	for _, endpoint := range q.attestors {
		go func(endpoint SigningAttestor) {
			root, found, err := endpoint.Client.AttestedUpTo(queryCtx, srcChain, includeProvisional)
			results <- result{index: endpoint.Index, root: root, found: found, err: err}
		}(endpoint)
	}

	frontiers := make([]*attestorpb.AttestedRoot, 0, q.threshold)
	failures := make([]attestorFailure, 0, len(q.attestors))
	waiting := 0
	for range q.attestors {
		var one result
		select {
		case <-ctx.Done():
			return nil, false, chain.Transient(fmt.Errorf("attestor frontier quorum: %w", ctx.Err()))
		case one = <-results:
		}
		if one.err != nil {
			failures = append(failures, newAttestorFailure("Frontier", one.index, one.err))
			continue
		}
		if !one.found {
			waiting++
			continue
		}
		if one.root == nil {
			failures = append(failures, newAttestorFailure("Frontier", one.index, errors.New("returned found=true with a nil frontier")))
			continue
		}
		frontiers = append(frontiers, one.root)
		if len(frontiers) == q.threshold {
			// Every member of this quorum attests through its own reported height.
			// The minimum is therefore the highest height supported by all members
			// of the quorum that completed; a later poll may choose a fresher quorum.
			sort.Slice(frontiers, func(i, j int) bool {
				return frontiers[i].GetL2BlockNumber() < frontiers[j].GetL2BlockNumber()
			})
			return frontiers[0], true, nil
		}
	}
	if len(frontiers)+waiting >= q.threshold {
		// The reachable set has not produced a quorum yet. This is the normal
		// pre-attestation state, so the source waits instead of treating it as an
		// operational failure.
		return nil, false, nil
	}
	return nil, false, quorumFailure("Frontier", "attestor frontiers available", len(frontiers), waiting, q.threshold, failures)
}
