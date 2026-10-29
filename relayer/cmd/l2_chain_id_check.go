package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
)

// l2ChainIDProbeTimeout bounds one eth_chainId probe. Generous on purpose: a
// rate-limited public endpoint can take seconds to answer, and a slow answer
// must not be mistaken for a wrong one.
const l2ChainIDProbeTimeout = 10 * time.Second

// l2ChainIDFromProfile reads rollup_profile.common.l2_chain_id.
//
// The Rust verifier's CommonProfile declares the field non-optional under
// deny_unknown_fields, so a profile without it cannot create a client — treat a
// missing or zero value as a config error rather than skipping the check.
func l2ChainIDFromProfile(profile json.RawMessage) (uint64, error) {
	var pc struct {
		Common struct {
			L2ChainID uint64 `json:"l2_chain_id"`
		} `json:"common"`
	}
	if err := json.Unmarshal(profile, &pc); err != nil {
		return 0, fmt.Errorf("parse rollup_profile.common: %w", err)
	}
	if pc.Common.L2ChainID == 0 {
		return 0, fmt.Errorf("rollup_profile.common.l2_chain_id is missing or zero")
	}
	return pc.Common.L2ChainID, nil
}

// validateL2ChainID checks the l2_to_cosmos module's declared l2_chain_id
// against the chain its l2_rpc_url actually serves.
//
// Until this existed the field was inert: nothing in the relayer read it, and
// nothing in the Rust verifier reads it either (it is declared in CommonProfile
// and never consulted). Transactions take their chain id from eth_chainId on the
// RPC itself, so a profile naming the devnet while the RPC pointed at Arbitrum
// Sepolia relayed happily against Sepolia — with the config, the created
// client's committed profile, and the docs all saying otherwise. `l2_chain_id`
// is documented as "must be the chain the relayer's l2_rpc_url actually serves"
// (relayer/op-l2-config.example.json) and listed as a hand-filled field for
// every L2 in docs/E2E.md, so an operator reasonably assumes a wrong value is
// caught. This is what catches it.
//
// A NON-ANSWERING endpoint does not fail this configuration comparison. That is
// an availability problem, not evidence that rollup_profile is wrong, so this
// check reports it as unknown rather than as a mismatch. The startup EVM signer
// lock separately requires eth_chainId before it can identify the nonce domain;
// it will stop startup rather than silently run without that lock. Only a
// definite disagreement here — the endpoint answered, and answered with a
// different chain — is a configuration error, because no amount of retrying
// fixes it and every packet relayed meanwhile is relayed against the wrong chain.
func validateL2ChainID(stdCtx context.Context, src l2ToCosmosConfig) error {
	want, err := l2ChainIDFromProfile(src.RollupProfile)
	if err != nil {
		return fmt.Errorf("l2_to_cosmos config (attestor_src_chain %q): %w", src.AttestorSrcChain, err)
	}

	verified, err := verifyL2ChainID(stdCtx, l2ChainIDCheck{
		label:            fmt.Sprintf("l2_to_cosmos config (attestor_src_chain %q)", src.AttestorSrcChain),
		rpcURL:           src.L2RpcUrl,
		want:             want,
		tolerateNoAnswer: true,
	})
	if err != nil {
		return err
	}
	// Exactly one of these two lines. Reporting the skip and then reporting
	// success unconditionally left an operator reading "skipping the
	// l2_chain_id check" immediately followed by "l2_chain_id verified",
	// which is the one thing the log must not say.
	if !verified {
		log.Printf("WARN [start] could not read eth_chainId from %s; skipping the l2_chain_id check for %q "+
			"(declared %d). The EVM signer lock still needs that answer, so startup will stop rather than run without its nonce guard.",
			src.L2RpcUrl, src.AttestorSrcChain, want)
		return nil
	}
	log.Printf("[start] l2_chain_id verified for %q: %d", src.AttestorSrcChain, want)
	return nil
}

// l2ChainIDCheck is one declared-vs-served comparison.
//
// tolerateNoAnswer differs by caller on purpose, because "the endpoint did not
// answer" means different things to each:
//
//   - start (true): an unreachable node is not evidence of a profile mismatch,
//     so this comparison reports it as unverified rather than wrong. The EVM
//     signer lock is a separate startup gate and must still resolve the chain id
//     before the relayer can submit safely.
//   - create-clients (false): the command is about to commit this profile into
//     an on-chain client that nothing can later repair, and it dials the very
//     same RPC moments afterwards for the bootstrap roots. It cannot succeed
//     with an unreachable endpoint anyway, so failing here costs nothing and
//     says why in one line instead of failing obscurely one step later.
type l2ChainIDCheck struct {
	label            string
	rpcURL           string
	want             uint64
	tolerateNoAnswer bool
}

// verifyL2ChainID reports a definite disagreement between the declared chain id
// and the one the endpoint serves.
//
// verified says whether a comparison actually happened. A tolerated no-answer
// returns (false, nil): no disagreement was found because none could be looked
// for. Callers must not announce success on that branch — reporting "verified"
// after logging "skipping" tells an operator the opposite of what occurred.
func verifyL2ChainID(stdCtx context.Context, c l2ChainIDCheck) (verified bool, err error) {
	got := probeL2ChainID(stdCtx, c.rpcURL)
	if got == nil {
		// "The node did not answer" and "we were told to stop" arrive here as the
		// same nil, and they are opposites. Tolerating a cancellation lets `start`
		// walk past this check into the rest of its initialisation after a Ctrl-C,
		// and fail later on something unrelated -- which is what the caller's own
		// signal context was threaded in to prevent.
		if err := stdCtx.Err(); err != nil {
			return false, fmt.Errorf("%s: cancelled while reading eth_chainId from %s: %w",
				c.label, c.rpcURL, err)
		}
		if c.tolerateNoAnswer {
			return false, nil
		}
		return false, fmt.Errorf(
			"%s: could not read eth_chainId from %s, so rollup_profile.common.l2_chain_id (%d) cannot be "+
				"verified. This command is about to commit that profile into a light client that cannot be "+
				"corrected afterwards, so it stops rather than guess. Fix the endpoint and re-run",
			c.label, c.rpcURL, c.want)
	}

	// Compared as big.Int rather than through uint64. An answer above the uint64
	// range used to be folded into "no answer", so a value that PROVABLY cannot
	// match the declared id (the profile field is uint64) took the tolerate
	// branch and startup continued — the opposite of the rule this check exists
	// to enforce.
	if got.Cmp(new(big.Int).SetUint64(c.want)) != 0 {
		return false, fmt.Errorf(
			"%s: rollup_profile.common.l2_chain_id is %d but l2_rpc_url %s serves chain %s. Transactions are "+
				"signed with the chain id reported by the RPC, so this relayer would act on chain %s while its "+
				"client profile, and anything reading the config, says chain %d. Fix whichever is wrong",
			c.label, c.want, c.rpcURL, got, got, c.want)
	}
	return true, nil
}

// probeL2ChainID returns the chain id the endpoint reports, or nil when it did
// not answer at all. Only the nil case means "no answer" — any value it did
// report is returned verbatim, so the caller can tell "wrong chain" from "no
// chain" even when the value is out of uint64 range.
func probeL2ChainID(stdCtx context.Context, rpcURL string) *big.Int {
	ctx, cancel := context.WithTimeout(stdCtx, l2ChainIDProbeTimeout)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil
	}
	defer client.Close()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil
	}
	return chainID
}

// validateL2ClientChainID verifies an --l2-config profile against the chain its
// RPC serves, and returns the declared id on success.
//
// It exists as its own function so the create-clients path's behaviour is
// testable without standing up Cosmos: it composes exactly what that path needs
// (parse the profile, probe the endpoint, compare, refuse on no answer), against
// a real l2ClientConfig rather than a hand-picked chain id.
func validateL2ClientChainID(stdCtx context.Context, l2cfg *l2ClientConfig) (uint64, error) {
	want, err := l2ChainIDFromProfile(l2cfg.RollupProfile)
	if err != nil {
		return 0, fmt.Errorf("l2-config: %w", err)
	}
	// tolerateNoAnswer is false here, so a nil error means the comparison ran.
	if _, err := verifyL2ChainID(stdCtx, l2ChainIDCheck{
		label:  "l2-config",
		rpcURL: l2cfg.L2RPCURL,
		want:   want,
	}); err != nil {
		return 0, err
	}
	return want, nil
}
