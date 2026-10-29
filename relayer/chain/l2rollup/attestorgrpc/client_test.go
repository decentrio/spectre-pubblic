package attestorgrpc

import (
	"attestor/types/attestation"
	attestorpb "attestor/types/attestor"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"relayer/chain/l2rollup"
	"strings"
	"testing"

	"google.golang.org/grpc"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// codeServer answers every RPC with one gRPC code, so a test can name a code and
// read back what the relayer decided about it.
type codeServer struct {
	attestorpb.UnimplementedAttestorServiceServer
	code codes.Code
}

func (s *codeServer) VerifyStateRoot(context.Context, *attestorpb.VerifyStateRootRequest) (*attestorpb.VerifyStateRootResponse, error) {
	return nil, status.Error(s.code, "from the server")
}

func (s *codeServer) Info(context.Context, *attestorpb.InfoRequest) (*attestorpb.InfoResponse, error) {
	if s.code == codes.OK {
		return &attestorpb.InfoResponse{Chains: []*attestorpb.ChainInfo{{SrcChain: "opstack", AttestationHead: "safe"}}}, nil
	}
	return nil, status.Error(s.code, "from the server")
}

func (s *codeServer) AttestedUpTo(context.Context, *attestorpb.AttestedUpToRequest) (*attestorpb.AttestedUpToResponse, error) {
	return nil, status.Error(s.code, "from the server")
}

// dialCodeServer runs a real gRPC server and a real Client over it. The transport
// is the point: mapStatus reads a code off the wire, and a unit test that hands
// it a locally built status asserts the assumption instead of the behaviour.
func dialCodeServer(t *testing.T, code codes.Code) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	attestorpb.RegisterAttestorServiceServer(server, &codeServer{code: code})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{conn: conn, rpc: attestorpb.NewAttestorServiceClient(conn)}
}

// The table this pins is a CONTRACT WITH THE ATTESTOR, not a local preference:
// each code is produced by attestor/grpc for one documented reason.
//
// This is HALF of the classification, and deliberately so -- the two halves live
// in different packages because the seam does:
//
//	code -> sentinel   here, over a real connection (mapStatus)
//	sentinel -> outcome  TestClassifyAttestorFailure in the parent package
//
// classifyAttestorFailure is unexported and attestorgrpc imports l2rollup, so one
// test cannot span both without either widening the API or inverting the
// dependency. Naming the pair is better than doing either.
//
// Reported by @neitdung on #473: every FailedPrecondition was read as "replica
// behind" while the server also returned it for a missing block verifier, a
// missing commitment feed, an unconfigured signer, an absent chain table and an
// unsigned positive verdict. Those are configuration and invariant failures that
// no amount of waiting fixes, so the permanent-failure hold this PR added could
// never fire for any of them. The attestor now gives them disjoint codes, and
// this table is the relayer half of that agreement.
func TestMapStatus_ServerCodesClassifyIntoTheRightOutcome(t *testing.T) {
	cases := []struct {
		name     string
		code     codes.Code
		sentinel error
		// why names the server condition that produces this code, so a reader can
		// find the other side of the contract.
		why string
	}{
		{
			name: "replica has not reached the block", code: codes.FailedPrecondition,
			sentinel: l2rollup.ErrAttestorReplicaBehind,
			why:      "adapter: block N is above the replica head -- the ONLY meaning left for this code",
		},
		{
			name: "route serves no verifier, feed or signer", code: codes.Unimplemented,
			sentinel: l2rollup.ErrAttestorUnimplemented,
			why:      "server: route does not expose a block verifier / commitment feed; adapter: signer is not configured",
		},
		{
			name: "src_chain is not served", code: codes.NotFound,
			sentinel: l2rollup.ErrAttestorUnknownRoute,
			why:      "server: unknown src_chain, and no chains are configured at all",
		},
		{
			name: "the request itself is wrong", code: codes.InvalidArgument,
			sentinel: l2rollup.ErrAttestorBadRequest,
			why:      "server: malformed input or bad run_mode",
		},
		{
			name: "upstream said nothing", code: codes.Unavailable,
			sentinel: l2rollup.ErrAttestorUnavailable,
			why:      "server: L1/L2 RPC did not answer",
		},
		{
			name: "deadline is silence, not a no", code: codes.DeadlineExceeded,
			sentinel: l2rollup.ErrAttestorUnavailable,
			why:      "transport: the attestor said nothing about the block",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := dialCodeServer(t, tc.code)

			// Both RPCs, because they do not share a path: VerifyStateRoot
			// intercepts Unimplemented ahead of mapStatus to report the
			// VerifyStateRoot-specific sentinel (which wraps the general one), so a
			// test that drove only VerifyStateRoot would never execute mapStatus's
			// Unimplemented branch at all. It was mutated to ErrAttestorReplicaBehind
			// and this test stayed green until AttestedUpTo was added here.
			calls := map[string]func() error{
				"VerifyStateRoot": func() error {
					_, err := client.VerifyStateRoot(context.Background(), l2rollup.VerificationRequest{
						SrcChain: "op-sepolia", BlockNumber: 100, StateRoot: make([]byte, 32),
						BlockHash: make([]byte, 32), RunMode: attestorpb.RunMode_RUN_MODE_FINALIZED,
					})
					return err
				},
				"AttestedUpTo": func() error {
					_, _, err := client.AttestedUpTo(context.Background(), "op-sepolia", false)
					return err
				},
			}
			for name, call := range calls {
				err := call()
				if err == nil {
					t.Fatalf("%s: no error for %v (%s)", name, tc.code, tc.why)
				}
				if !errors.Is(err, tc.sentinel) {
					t.Fatalf("%s: %v did not map to %v: %v\n  server side: %s", name, tc.code, tc.sentinel, err, tc.why)
				}
				// The operator has to be able to read the server's own words.
				if !strings.Contains(err.Error(), "from the server") {
					t.Fatalf("%s: the status message was dropped: %v", name, err)
				}
			}
		})
	}
}

// An unmapped code must stay transient: keeping packets and retrying is the safe
// default for a failure nobody has classified, and Internal in particular is the
// server's catch-all for anything unclassified.
func TestMapStatus_AnUnmappedCodeStaysTransient(t *testing.T) {
	client := dialCodeServer(t, codes.Internal)
	_, err := client.VerifyStateRoot(context.Background(), l2rollup.VerificationRequest{
		SrcChain: "op-sepolia", BlockNumber: 100, StateRoot: make([]byte, 32),
		BlockHash: make([]byte, 32), RunMode: attestorpb.RunMode_RUN_MODE_FINALIZED,
	})
	if err == nil {
		t.Fatal("Internal produced no error")
	}
	for _, permanent := range []error{
		l2rollup.ErrAttestorUnimplemented, l2rollup.ErrAttestorUnknownRoute, l2rollup.ErrAttestorBadRequest,
	} {
		if errors.Is(err, permanent) {
			t.Fatalf("Internal was mapped to %v; a catch-all code must not hold a source off", permanent)
		}
	}
}

func TestRequireSrcChain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     string
		want      bool
		permanent bool
	}{
		{name: "configured", value: "op-mainnet"},
		{name: "empty", want: true, permanent: true},
		{name: "whitespace", value: " \t", want: true, permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireSrcChain(tc.value)
			if (err != nil) != tc.want {
				t.Fatalf("requireSrcChain(%q) error = %v, want error=%t", tc.value, err, tc.want)
			}
			if errors.Is(err, l2rollup.ErrAttestorBadRequest) != tc.permanent {
				t.Fatalf("requireSrcChain(%q) permanent = %t, want %t", tc.value, errors.Is(err, l2rollup.ErrAttestorBadRequest), tc.permanent)
			}
		})
	}
}

// Info is read once at startup to prove the attestor serves this L2 at the head
// the relayer will ask for. The table it returns is the attestor's own proto
// shape; an unreachable attestor maps like every other call so the caller can
// tell "did not answer" from "answered wrong".
func TestInfo_ReturnsTheChainTableAndMapsFailures(t *testing.T) {
	chains, err := dialCodeServer(t, codes.OK).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(chains) != 1 || chains[0].GetSrcChain() != "opstack" || chains[0].GetAttestationHead() != "safe" {
		t.Fatalf("Info returned %v, want the server's one opstack/safe entry", chains)
	}

	_, err = dialCodeServer(t, codes.Unavailable).Info(context.Background())
	if !errors.Is(err, l2rollup.ErrAttestorUnavailable) {
		t.Fatalf("Info on an unavailable server = %v, want ErrAttestorUnavailable", err)
	}
}

type contractAttestorServer struct {
	attestorpb.UnimplementedAttestorServiceServer
	frontier   *attestorpb.AttestedUpToResponse
	verify     *attestorpb.VerifyStateRootResponse
	verifyErr  error
	verifySeen *attestorpb.VerifyStateRootRequest
}

func (s *contractAttestorServer) AttestedUpTo(_ context.Context, _ *attestorpb.AttestedUpToRequest) (*attestorpb.AttestedUpToResponse, error) {
	return s.frontier, nil
}

func (s *contractAttestorServer) VerifyStateRoot(_ context.Context, request *attestorpb.VerifyStateRootRequest) (*attestorpb.VerifyStateRootResponse, error) {
	s.verifySeen = request
	if s.verifyErr != nil {
		return nil, s.verifyErr
	}
	return s.verify, nil
}

func newGeneratedContractClient(t *testing.T, server *contractAttestorServer) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := googlegrpc.NewServer()
	attestorpb.RegisterAttestorServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := googlegrpc.NewClient(
		"passthrough:///attestor-contract",
		googlegrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return &Client{conn: connection, rpc: attestorpb.NewAttestorServiceClient(connection)}
}

// TestClient_VerifyStateRootTransportContract pins Client as a pure transport:
// request fields reach the wire unmodified, a valid signature round-trips,
// gRPC status codes propagate, and a bad signature from the attestor passes
// through verbatim rather than being swallowed or normalised — this client
// does not verify signatures itself. The fail-closed boundary that rejects a
// bad signature lives in chain/l2rollup (header_attested.go), not here.
func TestClient_VerifyStateRootTransportContract(t *testing.T) {
	const (
		chainID = uint64(8453)
		height  = uint64(42)
	)
	stateRoot := make([]byte, 32)
	stateRoot[0] = 0xaa
	blockHash := make([]byte, 32)
	blockHash[0] = 0xbb
	var router [attestation.RouterLength]byte
	router[0] = 0xcc
	signer, err := attestation.NewSigner(chainID, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	setHash, err := (attestation.AttestorConfig{PublicKeys: [][]byte{signer.PublicKey()}, Threshold: 1}).SetHash()
	if err != nil {
		t.Fatalf("attestor set hash: %v", err)
	}
	sign := func(l2Router [attestation.RouterLength]byte, attestorSetHash [attestation.HashLength]byte) []byte {
		t.Helper()
		signature, signErr := signer.Sign(l2Router[:], attestorSetHash[:], height, blockHash, stateRoot)
		if signErr != nil {
			t.Fatalf("sign identity: %v", signErr)
		}
		return signature
	}
	statement, err := attestation.SigningBytes(chainID, router[:], setHash[:], height, blockHash, stateRoot)
	if err != nil {
		t.Fatalf("build attestation statement: %v", err)
	}
	response := func(valid bool, signature []byte) *attestorpb.VerifyStateRootResponse {
		return &attestorpb.VerifyStateRootResponse{
			Valid: valid, BlockNumber: height, BlockHash: blockHash, StateRoot: stateRoot, AttestationSignature: signature,
		}
	}
	request := l2rollup.VerificationRequest{
		SrcChain: "base-mainnet", BlockNumber: height, StateRoot: stateRoot, BlockHash: blockHash,
		RunMode: attestorpb.RunMode_RUN_MODE_FINALIZED, L2Router: router, AttestorSetHash: setHash,
	}

	server := &contractAttestorServer{
		frontier: &attestorpb.AttestedUpToResponse{Found: false},
		verify:   response(true, sign(router, setHash)),
	}
	client := newGeneratedContractClient(t, server)
	if root, found, err := client.AttestedUpTo(context.Background(), "base-mainnet", false); err != nil || found || root != nil {
		t.Fatalf("found=false frontier = (%+v, %t, %v)", root, found, err)
	}

	verdict, err := client.VerifyStateRoot(context.Background(), request)
	if err != nil || !verdict.Valid {
		t.Fatalf("valid signed verdict = (%+v, %v)", verdict, err)
	}
	if verdict.BlockNumber != height || string(verdict.StateRoot) != string(stateRoot) || string(verdict.BlockHash) != string(blockHash) {
		t.Fatalf("valid signed verdict identity = %+v", verdict)
	}
	if !ed25519.Verify(signer.PublicKey(), statement, verdict.Signature) {
		t.Fatal("relayer rejected a valid generated signature")
	}
	if got := server.verifySeen; got == nil || got.GetSrcChain() != "base-mainnet" || got.GetBlockNumber() != height ||
		string(got.GetExpectedStateRoot()) != string(stateRoot) || string(got.GetExpectedBlockHash()) != string(blockHash) ||
		got.GetRunMode() != attestorpb.RunMode_RUN_MODE_FINALIZED || string(got.GetL2Router()) != string(router[:]) ||
		string(got.GetAttestorSetHash()) != string(setHash[:]) {
		t.Fatalf("VerifyStateRoot request = %+v", got)
	}

	if t.Run("canonical mismatch", func(t *testing.T) {
		server.verify = response(false, nil)
		server.verifyErr = nil
		result, callErr := client.VerifyStateRoot(context.Background(), request)
		if callErr != nil || result.Valid {
			t.Fatalf("VerifyStateRoot = (%+v, %v), want valid=false", result, callErr)
		}
	}) {
		// Bad-signature responses: the client must pass the signature through
		// exactly as the attestor sent it, neither swallowing nor normalising
		// it, so that the real verifier (outside this package) still sees
		// what was actually on the wire.
		for _, tc := range []struct {
			name      string
			signature []byte
		}{
			{name: "missing signature", signature: nil},
			{name: "wrong signature", signature: make([]byte, attestation.SignatureLength)},
			{name: "wrong attestation context", signature: sign([attestation.RouterLength]byte{0xff}, setHash)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server.verify = response(true, tc.signature)
				server.verifyErr = nil
				result, callErr := client.VerifyStateRoot(context.Background(), request)
				if callErr != nil || !result.Valid {
					t.Fatalf("VerifyStateRoot = (%+v, %v), want valid=true (transport does not verify)", result, callErr)
				}
				if string(result.Signature) != string(tc.signature) {
					t.Fatalf("signature passthrough = %x, want verbatim %x", result.Signature, tc.signature)
				}
			})
		}
	}

	for _, expectedCode := range []codes.Code{codes.FailedPrecondition, codes.Unavailable} {
		server.verifyErr = status.Error(expectedCode, "test upstream failure")
		_, err := client.VerifyStateRoot(context.Background(), request)
		if status.Code(err) != expectedCode {
			t.Fatalf("transport status = %v, want %v (err=%v)", status.Code(err), expectedCode, err)
		}
	}
}
