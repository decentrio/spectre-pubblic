package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveAttestorStartupValidationE2E runs the check `relayer start` performs
// before relaying (validateAttestorHeads) against real attestor processes
// started by scripts/local/test_relayer_attestor_e2e.sh. The Info judgement
// itself is unit-tested (judgeAttestorInfo); this pins that the live daemons'
// answers produce the boot decision an operator relies on.
func TestLiveAttestorStartupValidationE2E(t *testing.T) {
	get := func(name string) string { return strings.TrimSpace(os.Getenv("RELAYER_QUORUM_E2E_" + name)) }
	srcChain := get("SRC_CHAIN")
	if srcChain == "" {
		t.Skip("set RELAYER_QUORUM_E2E_SRC_CHAIN (via scripts/local/test_relayer_attestor_e2e.sh) to run against live attestors")
	}
	head, members, wrongHead, unreachable := get("HEAD"), strings.Split(get("MEMBERS"), ","), get("WRONG_HEAD"), get("UNREACHABLE")
	if head == "" || len(members) != 3 || wrongHead == "" || unreachable == "" {
		t.Fatal("RELAYER_QUORUM_E2E_HEAD, MEMBERS, WRONG_HEAD, and UNREACHABLE are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	configFor := func(src, headKind string, addresses ...string) *appConfig {
		endpoints := make([]l2AttestorEndpointConfig, len(addresses))
		for index, address := range addresses {
			endpoints[index] = l2AttestorEndpointConfig{Address: address, AttestorIndex: uint16(index)}
		}
		return &appConfig{HasL2ToCosmos: true, L2ToCosmosConfig: l2ToCosmosConfig{
			AttestorEndpoints: endpoints, AttestorSrcChain: src, HeadKind: headKind,
		}}
	}
	cases := []struct {
		name     string
		config   *appConfig
		findings []string // expected finding keys, in order
	}{
		{"matching_route_and_head_boots", configFor(srcChain, head, members...), nil},
		{"unreachable_endpoint_is_not_a_finding", configFor(srcChain, head, members[0], members[1], unreachable), nil},
		{"head_kind_mismatch_is_refused", configFor(srcChain, head, members[0], members[1], wrongHead),
			[]string{"attestor_endpoints[2].address / head_kind"}},
		{"unknown_src_chain_is_refused", configFor(srcChain+"-not-served", head, members[0]),
			[]string{"attestor_endpoints[0].address / attestor_src_chain"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			findings := validateAttestorHeads(ctx, testCase.config)
			got := make([]string, len(findings))
			details := make([]string, len(findings))
			for index, finding := range findings {
				got[index] = finding.key
				details[index] = finding.detail
			}
			if strings.Join(got, "|") != strings.Join(testCase.findings, "|") {
				t.Fatalf("findings %q, want %q (details: %q)", got, testCase.findings, details)
			}
			t.Logf("EVIDENCE %s: start would %s; findings=%q", t.Name(), bootDecision(findings), details)
		})
	}
}

func bootDecision(findings []configFinding) string {
	if len(findings) == 0 {
		return "boot"
	}
	return "refuse to boot"
}
