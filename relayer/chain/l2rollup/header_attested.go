package l2rollup

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math/big"
	"sort"

	"attestor/types/attestation"
	attestorpb "attestor/types/attestor"
	"relayer/chain"
	relayerclient "relayer/client"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// attestedHeaderBuilder assembles the only header shape the attestor-trusted L2
// clients accept: the canonical L2 execution header at the target height, plus an
// account proof of that chain's ICS26Router against the state root that header
// declares.
//
// It is chain-agnostic on purpose. The per-chain builders it replaces existed to
// gather settlement evidence — DisputeGameFactory games for OP-Stack, RollupCore
// assertions for Arbitrum, each pinned to the L1 block the shared Ethereum client
// trusted. None of that is verified any more: the three verifier crates are now
// identical adapters differing only in `profile_version`, so there is nothing left
// to branch on.
//
// The client verifies the indexed signatures before it touches the router proof.
// Source.RelayableHeight decides HOW FAR the relay may advance; this builder asks
// independent attestors to sign WHAT it packages at that exact height.
type attestedHeaderBuilder struct {
	l2        *ethclient.Client // L2 exec: l2_header + router eth_getProof
	router    ethcommon.Address // the L2 ICS26Router (rollup_profile.common.l2_router)
	chainID   uint64            // profile.common.l2_chain_id
	attestors []SigningAttestor // ordered by immutable wasm attestor index
	set       attestation.AttestorConfig
	setHash   [attestation.HashLength]byte
	srcChain  string             // attestor src_chain key, same as the source's
	runMode   attestorpb.RunMode // replica head the attestor must answer against
	name      string             // registry builder name, for logs and errors
}

// SigningAttestor is one reachable attestor endpoint assigned to its immutable
// ClientState public-key index. It never carries a private key.
type SigningAttestor struct {
	Index  uint16
	Client AttestorClient
}

// NewAttestedHeaderBuilder wires the builder to all configured signing attestors.
// The endpoint list may contain more than the threshold to provide availability;
// each emitted update carries exactly threshold valid signatures in index order.
func NewAttestedHeaderBuilder(l2 *ethclient.Client, router ethcommon.Address, chainID uint64, set attestation.AttestorConfig, attestors []SigningAttestor, srcChain string, runMode attestorpb.RunMode, name string) (HeaderBuilder, error) {
	if l2 == nil {
		return nil, fmt.Errorf("%s: L2 client must not be nil", name)
	}
	if chainID == 0 {
		return nil, fmt.Errorf("%s: L2 chain ID must not be zero", name)
	}
	if srcChain == "" {
		return nil, fmt.Errorf("%s: attestor src_chain must not be empty", name)
	}
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("%s: invalid attestor set: %w", name, err)
	}
	if len(attestors) < int(set.Threshold) {
		return nil, fmt.Errorf("%s: %d attestor endpoints cannot satisfy threshold %d", name, len(attestors), set.Threshold)
	}
	ordered := append([]SigningAttestor(nil), attestors...)
	seen := make(map[uint16]struct{}, len(ordered))
	for _, endpoint := range ordered {
		if endpoint.Client == nil {
			return nil, fmt.Errorf("%s: attestor endpoint at index %d is nil", name, endpoint.Index)
		}
		if int(endpoint.Index) >= len(set.PublicKeys) {
			return nil, fmt.Errorf("%s: attestor endpoint index %d is outside the configured set", name, endpoint.Index)
		}
		if _, duplicate := seen[endpoint.Index]; duplicate {
			return nil, fmt.Errorf("%s: duplicate attestor endpoint index %d", name, endpoint.Index)
		}
		seen[endpoint.Index] = struct{}{}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	setHash, err := set.SetHash()
	if err != nil {
		return nil, fmt.Errorf("%s: derive attestor set hash: %w", name, err)
	}
	return &attestedHeaderBuilder{l2: l2, router: router, chainID: chainID, set: set, setHash: setHash, attestors: ordered, srcChain: srcChain, runMode: runMode, name: name}, nil
}

func (a *attestedHeaderBuilder) Name() string { return a.name }

// BuildHeader returns the header for exactly request.Height. Unlike the settlement
// builders, there is no step-down: those had to commit whatever L2 block the newest
// proven game or assertion covered, which could be far below the request. Here the
// header IS the canonical block at the requested height, so the committed height
// always equals the requested one and the caller advances by it directly.
func (a *attestedHeaderBuilder) BuildHeader(ctx context.Context, request HeaderRequest) (ClientMessage, uint64, error) {
	height := new(big.Int).SetUint64(request.Height)

	l2Header, err := a.l2.HeaderByNumber(ctx, height)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: L2 header at %d: %w", a.name, request.Height, err)
	}
	signatures, err := a.bindToAttestation(ctx, request.Height, l2Header)
	if err != nil {
		return nil, 0, err
	}
	routerProof, err := relayerclient.EthGetProof(ctx, a.l2, a.router, nil, height)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: router proof at L2 height %d: %w", a.name, request.Height, err)
	}

	return &SignedAttestedL2Header{
		L2Header:          toCanonicalHeader(l2Header),
		RouterProof:       EvmAccountProof{Proof: routerProof.AccountProof},
		AttestorSignature: signatures,
	}, request.Height, nil
}

// bindToAttestation refuses a header the attestor's replica does not recognise as the
// canonical block at that height. state root AND block hash are both sent: the state
// root is what the client will trust, the block hash is what makes the answer specific
// to one block rather than to any block sharing a state root.
//
// Endpoints are queried concurrently. Individual causes retain their typed
// classification, while the aggregate is permanent only when no recoverable
// endpoint can bring the valid count up to the configured threshold.
func (a *attestedHeaderBuilder) bindToAttestation(ctx context.Context, height uint64, l2Header *types.Header) ([]IndexedAttestorSignature, error) {
	stateRoot := l2Header.Root
	blockHash := l2Header.Hash()
	statement, err := attestation.SigningBytes(a.chainID, a.router.Bytes(), a.setHash[:], height, blockHash.Bytes(), stateRoot.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s: build attestation statement: %w", a.name, err)
	}
	request := VerificationRequest{
		SrcChain: a.srcChain, BlockNumber: height, StateRoot: stateRoot.Bytes(), BlockHash: blockHash.Bytes(), RunMode: a.runMode, AttestorSetHash: a.setHash,
	}
	copy(request.L2Router[:], a.router.Bytes())

	signatures := make([]IndexedAttestorSignature, 0, a.set.Threshold)
	failures := make([]attestorFailure, 0, len(a.attestors))
	type result struct {
		index   uint16
		verdict SignedVerdict
		err     error
	}
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(a.attestors))
	for _, endpoint := range a.attestors {
		go func(endpoint SigningAttestor) {
			verdict, err := endpoint.Client.VerifyStateRoot(queryCtx, request)
			results <- result{index: endpoint.Index, verdict: verdict, err: err}
		}(endpoint)
	}
	for range a.attestors {
		var one result
		select {
		case <-ctx.Done():
			return nil, chain.Transient(fmt.Errorf("%s: attestor signature quorum: %w", a.name, ctx.Err()))
		case one = <-results:
		}
		if one.err != nil {
			failures = append(failures, newAttestorFailure("Attest", one.index, one.err))
			continue
		}
		if !one.verdict.Valid {
			failures = append(failures, newAttestorFailure("Attest", one.index, fmt.Errorf("%w: did not recognise L2 block %d", ErrAttestorDivergence, height)))
			continue
		}
		if one.verdict.BlockNumber != height || string(one.verdict.BlockHash) != string(blockHash.Bytes()) || string(one.verdict.StateRoot) != string(stateRoot.Bytes()) {
			failures = append(failures, newAttestorFailure("Attest", one.index, fmt.Errorf("%w: returned a different block identity", ErrAttestorDivergence)))
			continue
		}
		if len(one.verdict.Signature) != attestation.SignatureLength || !ed25519.Verify(ed25519.PublicKey(a.set.PublicKeys[one.index]), statement, one.verdict.Signature) {
			failures = append(failures, newAttestorFailure("Attest", one.index, fmt.Errorf("%w: returned an invalid signature", ErrAttestorSignature)))
			continue
		}
		signatures = append(signatures, IndexedAttestorSignature{AttestorIndex: one.index, Signature: append([]byte(nil), one.verdict.Signature...)})
		if len(signatures) == int(a.set.Threshold) {
			cancel()
			sort.Slice(signatures, func(i, j int) bool { return signatures[i].AttestorIndex < signatures[j].AttestorIndex })
			return signatures, nil
		}
	}
	return nil, quorumFailure(a.name, fmt.Sprintf("valid attestor signatures for L2 block %d", height), len(signatures), 0, int(a.set.Threshold), failures)
}
