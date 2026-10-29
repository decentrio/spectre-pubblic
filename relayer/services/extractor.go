package services

import (
	"crypto/ed25519"
	"fmt"
	"sort"

	relayerclient "relayer/client"
	"relayer/prover/witness"

	"github.com/cometbft/cometbft/types"
)

// ExtractValidatorSignatures lives in services rather than the prover package
// because signature selection is relayer policy (greedy quorum, pinned-set
// allowedIndices) and it needs cometbft + the RPC client; the prover only
// consumes the resulting witness.ValidatorSignature slice.
//
// ExtractValidatorSignatures collects non-absent, locally-verified commit
// signatures until their cumulative voting power exceeds 2/3 of
// TotalVotingPower. Candidates are sorted by power for selection, while the
// selected proof slots are returned in validator-index order.
//
// allowedIndices restricts which validator slots may be selected. When nil,
// all signing validators are candidates. Pass the set of cached signer indices
// to guarantee the proof only references indices already cached on-chain.
//
// Returns an error if no quorum can be reached or the required signer count
// would exceed the largest configured bucket.
func ExtractValidatorSignatures(
	lightBlock *relayerclient.LightBlock,
	chainID string,
	maxBucket int,
	allowedIndicesOpt ...map[uint32]bool,
) (*ExtractorResult, error) {
	var allowedIndices map[uint32]bool
	if len(allowedIndicesOpt) > 0 {
		allowedIndices = allowedIndicesOpt[0]
	}

	if lightBlock == nil {
		return nil, fmt.Errorf("light block is nil")
	}
	commit := lightBlock.SignedHeader.Commit
	if commit == nil {
		return nil, fmt.Errorf("commit is nil")
	}
	validators := lightBlock.ValSet
	if len(validators.Validators) == 0 {
		return nil, fmt.Errorf("validator set is empty")
	}

	candidates := make([]witness.ValidatorSignature, 0, len(commit.Signatures))
	for i, sig := range commit.Signatures {
		if sig.BlockIDFlag == types.BlockIDFlagAbsent {
			continue
		}
		if allowedIndices != nil && !allowedIndices[uint32(i)] {
			continue
		}
		if i >= len(validators.Validators) {
			return nil, fmt.Errorf("validator index %d out of range (have %d validators)", i, len(validators.Validators))
		}
		validator := validators.Validators[i]
		pubKeyBytes := validator.PubKey.Bytes()
		if len(pubKeyBytes) != ed25519.PublicKeySize {
			continue
		}
		if len(sig.Signature) != ed25519.SignatureSize {
			continue
		}
		voteData := commit.VoteSignBytes(chainID, int32(i))
		if !ed25519.Verify(pubKeyBytes, voteData, sig.Signature) {
			continue
		}
		candidates = append(candidates, witness.ValidatorSignature{
			Signature:   sig.Signature,
			PublicKey:   pubKeyBytes,
			Index:       i,
			Power:       validator.VotingPower,
			SignedBytes: voteData,
			Active:      true,
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no valid non-absent signatures found in commit")
	}

	// Greedy by voting power so the smallest prefix hits quorum and the
	// smallest bucket can be used.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Power > candidates[j].Power
	})

	totalPower := validators.TotalVotingPower()
	quorum := totalPower*2/3 + 1

	var accumulated int64
	cutoff := len(candidates)
	for i, c := range candidates {
		accumulated += c.Power
		if accumulated >= quorum {
			cutoff = i + 1
			break
		}
	}
	if accumulated < quorum {
		return nil, fmt.Errorf("insufficient voting power: have %d, need %d of %d", accumulated, quorum, totalPower)
	}

	allCandidates := append([]witness.ValidatorSignature(nil), candidates...)
	selected := append([]witness.ValidatorSignature(nil), candidates[:cutoff]...)
	sortSelectedSignaturesByIndex(selected)
	if len(selected) > maxBucket {
		return nil, fmt.Errorf("quorum requires %d signers but largest available bucket is %d", len(selected), maxBucket)
	}

	return &ExtractorResult{
		Candidates: allCandidates,
		Signatures: selected,
	}, nil
}

func sortSelectedSignaturesByIndex(sigs []witness.ValidatorSignature) {
	sort.Slice(sigs, func(i, j int) bool {
		return sigs[i].Index < sigs[j].Index
	})
}

// ExtractorResult bundles the valid signer candidates and the selected signer
// set. Selection policy lives in the relayer; this type is the shape it hands
// back.
type ExtractorResult struct {
	Candidates []witness.ValidatorSignature
	Signatures []witness.ValidatorSignature
}
