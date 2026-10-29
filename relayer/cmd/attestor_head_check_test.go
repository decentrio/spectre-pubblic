package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"

	attestorpb "attestor/types/attestor"
	"relayer/chain/l2rollup"

	"google.golang.org/grpc"
)

// infoServer answers Info with a fixed chain table and nothing else.
type infoServer struct {
	attestorpb.UnimplementedAttestorServiceServer
	chains []*attestorpb.ChainInfo
}

func (s *infoServer) Info(context.Context, *attestorpb.InfoRequest) (*attestorpb.InfoResponse, error) {
	return &attestorpb.InfoResponse{Chains: s.chains}, nil
}

// bareServer is an attestor build from before Info existed: every RPC is
// Unimplemented.
type bareServer struct {
	attestorpb.UnimplementedAttestorServiceServer
}

func serveBare(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	attestorpb.RegisterAttestorServiceServer(server, &bareServer{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// serveInfo runs a real attestor gRPC server on a loopback port and returns its
// address, because the check dials by address exactly the way start does.
func serveInfo(t *testing.T, chains ...*attestorpb.ChainInfo) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	attestorpb.RegisterAttestorServiceServer(server, &infoServer{chains: chains})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func l2ConfigWithAttestor(srcChain, headKind, address string) *appConfig {
	return &appConfig{
		HasL2ToCosmos: true,
		L2ToCosmosConfig: l2ToCosmosConfig{
			AttestorSrcChain:  srcChain,
			HeadKind:          headKind,
			AttestorEndpoints: []l2AttestorEndpointConfig{{AttestorIndex: 3, Address: address}},
		},
	}
}

func captureStartupLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// The attestor signs only the run_mode it is configured for, so head_kind and
// the feed's attestation_head form nine pairs of which exactly the three equal
// ones can relay. Every pair is judged, with include_provisional on both
// settings, so the table is the whole contract and not a sample of it.
func TestJudgeAttestorInfo_AcceptsOnlyTheThreeEqualPairs(t *testing.T) {
	heads := []l2rollup.HeadKind{l2rollup.Unsafe, l2rollup.Safe, l2rollup.Finalized}
	endpoint := l2AttestorEndpointConfig{AttestorIndex: 3, Address: "127.0.0.1:1"}
	for _, headKind := range heads {
		for _, feed := range heads {
			for _, provisional := range []bool{false, true} {
				name := fmt.Sprintf("head_kind=%s/feed=%s/include_provisional=%t", headKind, feed, provisional)
				t.Run(name, func(t *testing.T) {
					chains := []*attestorpb.ChainInfo{{SrcChain: "opstack", AttestationHead: feed.String()}}
					finding, warning := judgeAttestorInfo(0, endpoint, chains, "opstack", headKind, provisional)

					if headKind != feed {
						if finding == nil {
							t.Fatal("a head the attestor will not sign for passed startup")
						}
						for _, want := range []string{"head_kind", headKind.String(), feed.String(), "attestor 3", "127.0.0.1:1"} {
							if !strings.Contains(finding.key+" "+finding.detail, want) {
								t.Fatalf("finding does not name %q: %+v", want, *finding)
							}
						}
						return
					}
					if finding != nil {
						t.Fatalf("matching heads reported as a problem: %+v", *finding)
					}
					// The knob is a no-op on exactly one pair: a finalized feed has
					// nothing provisional to include.
					wantWarning := feed == l2rollup.Finalized && provisional
					if (warning != "") != wantWarning {
						t.Fatalf("warning = %q, want one: %t", warning, wantWarning)
					}
					if wantWarning && !strings.Contains(warning, "include_provisional") {
						t.Fatalf("warning does not name the knob: %q", warning)
					}
				})
			}
		}
	}
}

// Pointing at an attestor that serves a different L2 is the failure that today
// surfaces as NotFound on the first VerifyStateRoot. The finding lists what the
// attestor does serve so the operator can see which side is wrong.
func TestJudgeAttestorInfo_RejectsAnAttestorNotServingTheSourceChain(t *testing.T) {
	endpoint := l2AttestorEndpointConfig{AttestorIndex: 3, Address: "127.0.0.1:1"}
	chains := []*attestorpb.ChainInfo{
		{SrcChain: "arbitrum", AttestationHead: "safe"},
		{SrcChain: "base", AttestationHead: "safe"},
	}
	finding, warning := judgeAttestorInfo(1, endpoint, chains, "opstack", l2rollup.Safe, true)
	if finding == nil {
		t.Fatal("an attestor that does not serve the source chain passed startup")
	}
	if warning != "" {
		t.Fatalf("a fatal finding also produced a warning: %q", warning)
	}
	for _, want := range []string{"attestor_endpoints[1]", "attestor_src_chain", `"opstack"`, "arbitrum", "base"} {
		if !strings.Contains(finding.key+" "+finding.detail, want) {
			t.Fatalf("finding does not mention %q: %+v", want, *finding)
		}
	}
	// A chain table with nothing in it is the same wrong answer, not "no answer".
	if finding, _ := judgeAttestorInfo(0, endpoint, nil, "opstack", l2rollup.Safe, false); finding == nil {
		t.Fatal("an attestor serving no chains passed startup")
	}
}

func TestValidateAttestorHeads(t *testing.T) {
	t.Run("accepts a match and says so", func(t *testing.T) {
		addr := serveInfo(t, &attestorpb.ChainInfo{SrcChain: "opstack", AttestationHead: "safe"})
		buf := captureStartupLog(t)

		if findings := validateAttestorHeads(context.Background(), l2ConfigWithAttestor("opstack", "safe", addr)); len(findings) != 0 {
			t.Fatalf("a matching attestor reported as a problem: %+v", findings)
		}
		if !strings.Contains(buf.String(), "verified") || strings.Contains(buf.String(), "WARNING") {
			t.Fatalf("log should confirm the match and warn about nothing:\n%s", buf.String())
		}
	})

	// The no-op warning is keyed off the feed's answer, not head_kind: it is the
	// attestor that has nothing provisional to offer.
	t.Run("warns when include_provisional cannot take effect", func(t *testing.T) {
		addr := serveInfo(t, &attestorpb.ChainInfo{SrcChain: "opstack", AttestationHead: "finalized"})
		buf := captureStartupLog(t)
		cfg := l2ConfigWithAttestor("opstack", "finalized", addr)
		on := true
		cfg.L2ToCosmosConfig.IncludeProvisional = &on

		if findings := validateAttestorHeads(context.Background(), cfg); len(findings) != 0 {
			t.Fatalf("a no-op knob is a warning, not a finding: %+v", findings)
		}
		if !strings.Contains(buf.String(), "WARNING") || !strings.Contains(buf.String(), "include_provisional=true has no effect") {
			t.Fatalf("no-op include_provisional was not warned about:\n%s", buf.String())
		}
	})

	// Availability is not evidence the config is wrong. The log must say the check
	// was skipped and must not claim a verification that did not happen.
	t.Run("tolerates an attestor that does not answer", func(t *testing.T) {
		buf := captureStartupLog(t)

		if findings := validateAttestorHeads(context.Background(), l2ConfigWithAttestor("opstack", "safe", "127.0.0.1:1")); len(findings) != 0 {
			t.Fatalf("an unreachable attestor reported as a config problem: %+v", findings)
		}
		if !strings.Contains(buf.String(), "did not answer") || strings.Contains(buf.String(), "verified") {
			t.Fatalf("log should report the skip and nothing else:\n%s", buf.String())
		}
	})

	t.Run("is inert without an L2 source", func(t *testing.T) {
		if findings := validateAttestorHeads(context.Background(), &appConfig{}); findings != nil {
			t.Fatalf("no l2_to_cosmos module, yet findings: %+v", findings)
		}
	})

	// Reported by @DongLieu on #497: a reachable attestor without Info is a
	// different fact from one that is down, and in a mixed-version rollout the
	// operator needs the log to say which. Neither is a finding.
	t.Run("names an attestor without Info", func(t *testing.T) {
		addr := serveBare(t)
		buf := captureStartupLog(t)

		if findings := validateAttestorHeads(context.Background(), l2ConfigWithAttestor("opstack", "safe", addr)); len(findings) != 0 {
			t.Fatalf("an attestor without Info reported as a config problem: %+v", findings)
		}
		out := buf.String()
		if !strings.Contains(out, "does not implement Info") || strings.Contains(out, "did not answer") || strings.Contains(out, "verified") {
			t.Fatalf("log should say the attestor lacks Info, not that it is down or verified:\n%s", out)
		}
	})
}
