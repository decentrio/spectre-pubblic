package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	attestorpb "attestor/types/attestor"
	"relayer/chain/l2rollup"
	"relayer/chain/l2rollup/attestorgrpc"
)

// validateAttestorHeads asks every configured attestor, once, what it serves
// and at which head, and compares the answer with what this relayer is about
// to ask of it.
//
// Two disagreements are fatal, and both would otherwise stay invisible until
// the first header:
//
//   - The attestor does not serve attestor_src_chain. Every VerifyStateRoot
//     would come back NotFound, and the quorum this endpoint was counted
//     towards can never form.
//   - The attestor's attestation_head differs from head_kind. The builder sends
//     head_kind as the run_mode, and the attestor refuses to sign any run_mode
//     but its configured head (attestor/optimism/adapter/adapter.go, arbitrum
//     likewise: "requested run_mode ... does not match configured attestation
//     head"). Not a degraded configuration — one that cannot relay a single
//     packet.
//
// An attestor that does not answer is not a finding. Same doctrine as the other
// probes: availability is not evidence the config is wrong, and the builder
// already holds and retries on ErrAttestorUnavailable.
//
// The include_provisional knob is judged here too, off the FEED's answer rather
// than off head_kind: a feed gating on finalized has no provisional entries to
// include, so the knob is a no-op and the operator who set it believes they
// widened a policy that did not move. That is a warning, not a finding — the
// relay works, it just does not do what the config says it does.
func validateAttestorHeads(stdCtx context.Context, cfg *appConfig) []configFinding {
	if !cfg.HasL2ToCosmos {
		return nil
	}
	src := cfg.L2ToCosmosConfig
	headKind, err := parseHeadKind(src.HeadKind)
	if err != nil {
		return nil // loadConfig already refused this; nothing here to add
	}
	includeProvisional, _ := resolveIncludeProvisional(headKind, src.IncludeProvisional)

	var findings []configFinding
	for i, endpoint := range src.AttestorEndpoints {
		chains, err := probeAttestorInfo(stdCtx, endpoint.Address)
		if errors.Is(err, l2rollup.ErrAttestorUnimplemented) {
			// Reachable, but a build without Info. That is a different fact from
			// "down": it will answer VerifyStateRoot, and nothing here can say at
			// which head, so the log must not read like a transient outage.
			log.Printf("WARN [start] attestor %d (%s) does not implement Info; its attestation_head cannot be checked "+
				"against head_kind %q before the first header. Upgrade it to have the check run at startup.",
				endpoint.AttestorIndex, endpoint.Address, headKind)
			continue
		}
		if err != nil {
			log.Printf("WARN [start] attestor %d (%s) did not answer Info (%v); skipping the attestation_head check for it "+
				"(head_kind %q, attestor_src_chain %q). The header builder keeps retrying it.",
				endpoint.AttestorIndex, endpoint.Address, err, headKind, src.AttestorSrcChain)
			continue
		}
		finding, warning := judgeAttestorInfo(i, endpoint, chains, src.AttestorSrcChain, headKind, includeProvisional)
		if finding != nil {
			findings = append(findings, *finding)
			continue
		}
		if warning != "" {
			log.Printf("WARN [start] WARNING: %s", warning)
		}
		log.Printf("[start] attestor %d (%s) verified for %q: attestation_head=%s matches head_kind, include_provisional=%t",
			endpoint.AttestorIndex, endpoint.Address, src.AttestorSrcChain, headKind, includeProvisional)
	}
	return findings
}

// judgeAttestorInfo is the pure half: one attestor's Info answer against the
// config, as a finding (fatal), a warning (logged), or neither.
//
// i is the endpoint's position in attestor_endpoints, so the finding names the
// key the operator will look for; endpoint.AttestorIndex is the key-set index
// and is reported alongside because that is how the attestor is known
// everywhere else in the log.
func judgeAttestorInfo(i int, endpoint l2AttestorEndpointConfig, chains []*attestorpb.ChainInfo,
	srcChain string, headKind l2rollup.HeadKind, includeProvisional bool) (*configFinding, string) {
	key := fmt.Sprintf("attestor_endpoints[%d].address", i)

	var served []string
	var match *attestorpb.ChainInfo
	for _, c := range chains {
		served = append(served, c.GetSrcChain())
		if c.GetSrcChain() == srcChain {
			match = c
		}
	}
	if match == nil {
		return &configFinding{
			key: key + " / attestor_src_chain",
			detail: fmt.Sprintf("attestor %d at %s serves %q, not attestor_src_chain %q. Every VerifyStateRoot "+
				"to it would be refused as an unknown route, so it can never contribute to the signing quorum",
				endpoint.AttestorIndex, endpoint.Address, served, srcChain),
		}, ""
	}

	feedHead := match.GetAttestationHead()
	if feedHead != headKind.String() {
		return &configFinding{
			key: key + " / head_kind",
			detail: fmt.Sprintf("head_kind is %q but attestor %d at %s gates %q on attestation_head %q. The attestor "+
				"signs only the run_mode it is configured for, so every header this relayer asks it to attest "+
				"would be refused and no packet could be relayed. Make the two agree",
				headKind, endpoint.AttestorIndex, endpoint.Address, srcChain, feedHead),
		}, ""
	}

	if feedHead == l2rollup.Finalized.String() && includeProvisional {
		return nil, fmt.Sprintf("include_provisional=true has no effect: attestor %d at %s gates %q on attestation_head "+
			"%q, so its frontier holds no provisional entries to include. The relay runs at the finalized tier "+
			"regardless of this setting",
			endpoint.AttestorIndex, endpoint.Address, srcChain, feedHead)
	}
	return nil, ""
}

// probeAttestorInfo returns the attestor's chain table, or the error that stood
// in the way — the tolerated case, which the caller reads for WHY (an attestor
// without Info is not an attestor that is down). An answer of zero chains is
// not an error: it is a real, wrong configuration the caller must judge.
func probeAttestorInfo(stdCtx context.Context, address string) ([]*attestorpb.ChainInfo, error) {
	ctx, cancel := context.WithTimeout(stdCtx, configProbeTimeout)
	defer cancel()

	client, err := attestorgrpc.Dial(address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	return client.Info(ctx)
}
