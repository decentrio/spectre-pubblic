// This file makes the two validator-set decisions: whether the pinned set is
// still usable, and whose signatures get proven.
//
// It is validators.go rather than pinned_set.go because the second decision is
// not about the pinned set at all -- it is choosing the shortest prefix that
// reaches quorum. Every function here takes or returns a validator set, so the
// bare noun is the honest name, and it matches its neighbours (pending.go,
// batch.go, config.go).
//
// The name also marks a boundary with the prover: the relayer, not the prover,
// decides which signatures are proven. Someone working on that boundary should
// find this file by its name.
package services

import (
	"context"
	"fmt"
	"log"
	"math/big"
	spectreContract "relayer/bindings/SpectreClient"
	relayerclient "relayer/client"
	"relayer/prover/witness"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
)

type pinnedCosmosValidatorSet struct {
	indices      []uint32
	pubkeys      [][32]byte
	votingPowers []uint64
	totalPower   int64
}

func (s pinnedCosmosValidatorSet) indexByPubkey() map[[32]byte]uint32 {
	out := make(map[[32]byte]uint32, len(s.pubkeys))
	for i, pubkey := range s.pubkeys {
		out[pubkey] = s.indices[i]
	}
	return out
}

func (s pinnedCosmosValidatorSet) powerByPubkey() map[[32]byte]int64 {
	out := make(map[[32]byte]int64, len(s.pubkeys))
	for i, pubkey := range s.pubkeys {
		out[pubkey] = int64(s.votingPowers[i])
	}
	return out
}

func getPinnedCosmosValidatorSet(stdCtx context.Context, ctx EVMEndpoint) (pinnedCosmosValidatorSet, error) {
	spectre, err := spectreContract.NewContractSpectreClient(*ctx.SpectreClientContract(), ctx.EthClient())
	if err != nil {
		return pinnedCosmosValidatorSet{}, fmt.Errorf("failed to bind SpectreClient: %w", err)
	}
	out, err := spectre.GetPinnedValidatorSet(&bind.CallOpts{Context: stdCtx})
	if err != nil {
		return pinnedCosmosValidatorSet{}, fmt.Errorf("getPinnedValidatorSet: %w", err)
	}
	if len(out.Indices) != len(out.Pubkeys) || len(out.Indices) != len(out.VotingPowers) {
		return pinnedCosmosValidatorSet{}, fmt.Errorf(
			"pinned validator set length mismatch: indices=%d pubkeys=%d powers=%d",
			len(out.Indices), len(out.Pubkeys), len(out.VotingPowers),
		)
	}
	var totalPower int64
	for i, idx := range out.Indices {
		if idx != uint32(i) {
			return pinnedCosmosValidatorSet{}, fmt.Errorf("pinned validator set index mismatch at position %d: got %d", i, idx)
		}
		if out.VotingPowers[i] > uint64(^uint64(0)>>1) {
			return pinnedCosmosValidatorSet{}, fmt.Errorf("pinned validator voting power exceeds int64: index=%d", i)
		}
		totalPower += int64(out.VotingPowers[i])
	}
	return pinnedCosmosValidatorSet{indices: out.Indices, pubkeys: out.Pubkeys, votingPowers: out.VotingPowers, totalPower: totalPower}, nil
}

// pinnedOverlapPower sums the pinned-set voting power held by the target
// block's commit signers — the power available to prove against the pinned
// set. Signers outside the pinned set contribute zero. The measurement is
// per-commit: a pinned validator that merely missed this one block counts as
// zero, which makes the rotation trigger conservative (fires early, not late).
func pinnedOverlapPower(candidates []witness.ValidatorSignature, pinnedSet pinnedCosmosValidatorSet) int64 {
	powerByPubkey := pinnedSet.powerByPubkey()
	var overlap int64
	for _, candidate := range candidates {
		overlap += powerByPubkey[bytesToBytes32(candidate.PublicKey)]
	}
	return overlap
}

// rotationCandidate reports whether an update is considered for the
// updateConsensusState path at all. A changed nextValidatorsHash always is.
// forceRotation (the periodic rotation cadence) is too, even when the hash is
// unchanged: SpectreClient checks the trusting period against the pinned
// snapshot's timestamp, and only updateConsensusState refreshes it. On a chain
// whose validator set never changes, gating rotation on a hash difference
// leaves the snapshot frozen at client creation, and the client expires after
// one trusting period no matter how many updateApplicationState calls land
// (#541).
func rotationCandidate(forceRotation bool, targetNextValHash, pinnedHash [32]byte) bool {
	return forceRotation || targetNextValHash != pinnedHash
}

// forcedRotationBlockedAtHeight reports whether a forced rotation cannot be
// built because the client is already at the target height. Without a forced
// rotation, an up-to-date client simply needs no update; with one, the re-pin is
// still owed. The builder anchors the proof at the latest trusted height, so it
// needs a newer source block and must retry rather than report success (#541).
func forcedRotationBlockedAtHeight(forceRotation bool, trustedHeight, targetHeight int64) bool {
	return forceRotation && trustedHeight == targetHeight
}

// shouldRotatePinnedSet decides whether a rotationCandidate update rotates the
// pinned set (updateConsensusState) or defers and stays on
// updateApplicationState.
// forceRotation (the refresh-routine cadence path) always rotates; otherwise
// rotate once overlap/total ≤ threshold. With threshold "1/1" this rotates on
// any pinned-set change.
func shouldRotatePinnedSet(forceRotation bool, overlapPower, totalPower int64, threshold relayerclient.TrustThreshold) bool {
	if forceRotation {
		return true
	}
	overlap := new(big.Int).Mul(big.NewInt(overlapPower), big.NewInt(int64(threshold.Denominator)))
	total := new(big.Int).Mul(big.NewInt(totalPower), big.NewInt(int64(threshold.Numerator)))
	return overlap.Cmp(total) <= 0
}

func ethLatestHeaderTimestampNanos(stdCtx context.Context, ctx EVMEndpoint, fetchTimeout time.Duration) (*big.Int, error) {
	hctx, cancel := fetchCtx(stdCtx, fetchTimeout)
	defer cancel()

	header, err := ctx.EthClient().HeaderByNumber(hctx, nil)
	if err != nil {
		return nil, err
	}
	if header == nil || header.Time == 0 {
		return nil, fmt.Errorf("latest ethereum header is nil or has zero timestamp")
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(header.Time), big.NewInt(1_000_000_000)), nil
}

func selectSignaturesForPinnedSet(
	candidates []witness.ValidatorSignature,
	pinnedSet pinnedCosmosValidatorSet,
	maxBucket int,
) ([]witness.ValidatorSignature, error) {
	powerByPubkey := pinnedSet.powerByPubkey()
	selected := make([]witness.ValidatorSignature, 0, len(candidates))
	var pinnedAccum int64

	pinnedCandidates := make([]witness.ValidatorSignature, 0, len(candidates))
	for _, candidate := range candidates {
		if powerByPubkey[bytesToBytes32(candidate.PublicKey)] == 0 {
			continue
		}
		pinnedCandidates = append(pinnedCandidates, candidate)
	}
	sort.SliceStable(pinnedCandidates, func(i, j int) bool {
		leftPower := powerByPubkey[bytesToBytes32(pinnedCandidates[i].PublicKey)]
		rightPower := powerByPubkey[bytesToBytes32(pinnedCandidates[j].PublicKey)]
		if leftPower != rightPower {
			return leftPower > rightPower
		}
		return pinnedCandidates[i].Index < pinnedCandidates[j].Index
	})

	total := new(big.Int).SetInt64(pinnedSet.totalPower)
	total.Mul(total, big.NewInt(2))
	for _, candidate := range pinnedCandidates {
		selected = append(selected, candidate)
		pinnedAccum += powerByPubkey[bytesToBytes32(candidate.PublicKey)]
		accum := new(big.Int).SetInt64(pinnedAccum)
		accum.Mul(accum, big.NewInt(3))
		if accum.Cmp(total) > 0 {
			break
		}
	}
	accum := new(big.Int).SetInt64(pinnedAccum)
	accum.Mul(accum, big.NewInt(3))
	if accum.Cmp(total) <= 0 {
		return nil, fmt.Errorf("insufficient pinned-set quorum in proof candidates: have %d of %d", pinnedAccum, pinnedSet.totalPower)
	}
	if len(selected) > maxBucket {
		return nil, fmt.Errorf("pinned-set proof requires %d signers but largest bucket is %d", len(selected), maxBucket)
	}

	sort.Slice(selected, func(i, j int) bool {
		return selected[i].Index < selected[j].Index
	})
	return selected, nil
}

// binarySearchHighestFeasible finds the highest height in (trusted, latest]
// for which feasible returns true, assuming feasibility is non-increasing as
// height moves away from trusted (validator churn accumulates over time; it
// does not spontaneously reverse). feasible must distinguish "infeasible at
// this height" (ok=false, err=nil) from a genuine fetch/RPC error (err!=nil);
// the latter is propagated immediately rather than treated as infeasibility.
// latest itself is assumed already-probed-and-infeasible by the caller and is
// never re-probed here. Returns ok=false if no intermediate height is feasible.
//
// Pure and side-effect free so it's unit-testable without RPC mocking.
func binarySearchHighestFeasible(trusted, latest int64, feasible func(height int64) (ok bool, err error)) (int64, bool, error) {
	if latest <= trusted+1 {
		return 0, false, nil
	}

	lo, hi := trusted, latest
	found := int64(0)
	haveFound := false
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := feasible(mid)
		if err != nil {
			return 0, false, fmt.Errorf("probe height %d: %w", mid, err)
		}
		if ok {
			lo, found, haveFound = mid, mid, true
		} else {
			hi = mid
		}
	}
	return found, haveFound, nil
}

// findHighestFeasibleHop searches for the highest Cosmos height between
// trusted and latest whose commit still carries enough pinned-set signing
// power to clear selectSignaturesForPinnedSet's >2/3 quorum gate. This is the
// RLY-01 multi-hop fallback: used when the direct trusted->latest update no
// longer clears quorum due to validator churn.
func findHighestFeasibleHop(
	stdCtx context.Context,
	ctx cosmosClientDeps,
	trusted, latest int64,
	chainId string,
	pinnedSet pinnedCosmosValidatorSet,
	maxBucket int,
	directQuorumErr error,
) (hopCandidate, error) {
	probed := make(map[int64]struct{})
	var lastQuorumHeight int64
	var lastQuorumErr error

	probe := func(height int64) (hopProbeResult, error) {
		if err := stdCtx.Err(); err != nil {
			return hopProbeResult{}, err
		}
		probed[height] = struct{}{}
		fetchCtx, cancel := fetchCtx(stdCtx, ctx.fetchTimeout)
		defer cancel()
		lb, err := relayerclient.GetLightBlock(fetchCtx, ctx.cosmos.CosmosClient(), height)
		if err != nil {
			return hopProbeResult{}, fmt.Errorf("fetch light block at height %d: %w", height, err)
		}
		extracted, err := ExtractValidatorSignatures(lb, chainId, maxBucket, nil)
		if err != nil {
			return hopProbeResult{}, fmt.Errorf("extract signatures at height %d: %w", height, err)
		}
		selected, err := selectSignaturesForPinnedSet(extracted.Candidates, pinnedSet, maxBucket)
		if err != nil {
			return hopProbeResult{
				lightBlock: lb,
				candidates: extracted.Candidates,
				quorumErr:  err,
			}, nil // infeasible at this height, not a fetch error
		}
		return hopProbeResult{
			lightBlock: lb,
			candidates: extracted.Candidates,
			selected:   selected,
		}, nil
	}

	feasible := func(height int64) (bool, error) {
		result, err := probe(height)
		if err != nil {
			return false, err
		}
		if result.selected == nil {
			lastQuorumHeight = height
			lastQuorumErr = result.quorumErr
			return false, nil
		}
		return true, nil
	}

	height, ok, err := binarySearchHighestFeasible(trusted, latest, feasible)
	if err != nil {
		return hopCandidate{}, fmt.Errorf("hop search: %w", err)
	}
	if !ok {
		for h, sampled := trusted+1, int64(0); h < latest && sampled < hopExhaustionSampleLimit; h++ {
			if _, seen := probed[h]; seen {
				continue
			}
			sampled++
			result, err := probe(h)
			if err != nil {
				return hopCandidate{}, fmt.Errorf("hop search sample height %d: %w", h, err)
			}
			if result.selected != nil {
				log.Printf("WARN RLY01_HOP_NON_MONOTONIC_SAMPLE trusted=%d target=%d hop=%d",
					trusted, latest, h)
				return hopCandidate{
					lightBlock: result.lightBlock,
					candidates: result.candidates,
					selected:   result.selected,
				}, nil
			}
			lastQuorumHeight = h
			lastQuorumErr = result.quorumErr
		}
		msg := fmt.Sprintf("no provable height in (%d, %d]: pinned quorum lost immediately past trusted height", trusted, latest)
		if directQuorumErr != nil {
			msg += fmt.Sprintf("; target quorum shortfall: %v", directQuorumErr)
		}
		if lastQuorumErr != nil {
			msg += fmt.Sprintf("; sampled height %d quorum shortfall: %v", lastQuorumHeight, lastQuorumErr)
		}
		return hopCandidate{}, fmt.Errorf("%s", msg)
	}

	result, err := probe(height)
	if err != nil {
		return hopCandidate{}, fmt.Errorf("re-probe chosen hop height %d: %w", height, err)
	}
	if result.selected == nil {
		return hopCandidate{}, fmt.Errorf("hop height %d unexpectedly infeasible on re-check: %w", height, result.quorumErr)
	}
	log.Printf("RLY01_HOP_FOUND trusted=%d target=%d hop=%d", trusted, latest, height)
	return hopCandidate{lightBlock: result.lightBlock, candidates: result.candidates, selected: result.selected}, nil
}
