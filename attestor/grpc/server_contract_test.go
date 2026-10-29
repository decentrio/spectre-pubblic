package grpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"attestor/core"
	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakePorts struct {
	status      core.FeedStatus
	statusErr   error
	frontier    core.AttestedRoot
	found       bool
	feedErr     error
	verdict     core.SignedBlockIdentityVerdict
	verifyErr   error
	lastRequest core.BlockIdentityRequest
	updates     chan core.FrontierUpdate
	watchErr    error
}

func (f *fakePorts) AttestedUpTo(context.Context, core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	return f.frontier.Clone(), f.found, f.feedErr
}

func (f *fakePorts) AttestedRootAtOrBelow(_ context.Context, height uint64, _ core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	if !f.found || f.frontier.L2BlockNumber > height {
		return core.AttestedRoot{}, false, f.feedErr
	}
	return f.frontier.Clone(), true, f.feedErr
}

func (f *fakePorts) VerifyStateRoot(_ context.Context, request core.BlockIdentityRequest) (core.SignedBlockIdentityVerdict, error) {
	f.lastRequest = request
	if !request.RunMode.Valid() {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorInvalidArgument, "invalid mode", nil)
	}
	return f.verdict.Clone(), f.verifyErr
}

func (f *fakePorts) Status(context.Context) (core.FeedStatus, error) {
	return f.status, f.statusErr
}

func (f *fakePorts) WatchFrontier(context.Context, core.WatchRequest) (<-chan core.FrontierUpdate, error) {
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	if f.updates == nil {
		return nil, core.NewError(core.ErrorUnavailable, "watch unavailable", nil)
	}
	return f.updates, nil
}

func newContractClient(t *testing.T, service *Server) attestorpb.AttestorServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := googlegrpc.NewServer()
	service.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := googlegrpc.NewClient(
		"passthrough:///attestor-contract",
		googlegrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return attestorpb.NewAttestorServiceClient(connection)
}

func newContractServer(t *testing.T, ports core.Ports) *Server {
	t.Helper()
	service, err := New(map[string]core.Ports{"chain-a": ports})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

func TestNewRejectsEmptyOrIncompleteRoutes(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New accepted an empty routing table")
	}
	if _, err := New(map[string]core.Ports{"chain-a": {}}); err == nil {
		t.Fatal("New accepted a route without StatusReader")
	}
}

func TestAdapterContractQueriesAndProvenance(t *testing.T) {
	gameIndex := uint64(0)
	ports := &fakePorts{
		status:   core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true, ReplicaSeen: true, ReplicaFinalized: 100, Pending: 2},
		frontier: core.AttestedRoot{L2BlockNumber: 100, Root: make([]byte, 32), Source: "game", GameIndex: &gameIndex, AttestedAt: time.Unix(1000, 0)},
		found:    true,
	}
	client := newContractClient(t, newContractServer(t, core.Ports{Feed: ports, Status: ports}))
	info, err := client.Info(context.Background(), &attestorpb.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(info.GetChains()) != 1 || info.GetChains()[0].GetPendingGames() != 2 || !info.GetChains()[0].GetReplicaSeen() {
		t.Fatalf("Info = %+v", info)
	}
	frontier, err := client.AttestedUpTo(context.Background(), &attestorpb.AttestedUpToRequest{SrcChain: "chain-a"})
	if err != nil {
		t.Fatalf("AttestedUpTo: %v", err)
	}
	if !frontier.GetFound() || frontier.GetRoot().GetGameIndex() != 0 {
		t.Fatalf("AttestedUpTo = %+v", frontier)
	}
	missing, err := client.AttestedRootAtOrBelow(context.Background(), &attestorpb.AttestedRootAtOrBelowRequest{SrcChain: "chain-a", L2BlockNumber: 99})
	if err != nil || missing.GetFound() {
		t.Fatalf("AttestedRootAtOrBelow = (%+v, %v), want not found", missing, err)
	}
}

// src_chain is a routing and trust-boundary input. The server never infers it
// from a configured route: production relayers must name the attestor feed they
// intend to consume.
func TestAdapterContractRequiresSrcChainByDefault(t *testing.T) {
	ports := &fakePorts{
		status:   core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true},
		frontier: core.AttestedRoot{L2BlockNumber: 100, Root: make([]byte, 32)},
		found:    true,
		verdict:  core.SignedBlockIdentityVerdict{Valid: true, BlockNumber: 100, Signature: make([]byte, signatureLength)},
	}
	client := newContractClient(t, newContractServer(t, core.Ports{Feed: ports, Verifier: ports, Status: ports}))

	for _, srcChain := range []string{"", " \t"} {
		if _, err := client.AttestedUpTo(context.Background(), &attestorpb.AttestedUpToRequest{SrcChain: srcChain}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("AttestedUpTo src_chain %q code = %v, want InvalidArgument", srcChain, status.Code(err))
		}
		if _, err := client.AttestedRootAtOrBelow(context.Background(), &attestorpb.AttestedRootAtOrBelowRequest{SrcChain: srcChain}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("AttestedRootAtOrBelow src_chain %q code = %v, want InvalidArgument", srcChain, status.Code(err))
		}
		if _, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{
			SrcChain:          srcChain,
			ExpectedStateRoot: make([]byte, 32),
			RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("VerifyStateRoot src_chain %q code = %v, want InvalidArgument", srcChain, status.Code(err))
		}
	}
}

func TestAdapterContractVerifyErrorMatrix(t *testing.T) {
	validVerdict := core.SignedBlockIdentityVerdict{Valid: true, BlockNumber: 100, Signature: make([]byte, signatureLength)}
	ports := &fakePorts{
		status:  core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true},
		verdict: validVerdict,
	}
	client := newContractClient(t, newContractServer(t, core.Ports{Verifier: ports, Status: ports}))
	request := &attestorpb.VerifyStateRootRequest{
		SrcChain:          "chain-a",
		BlockNumber:       100,
		ExpectedStateRoot: make([]byte, 32),
		RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
		L2Router:          make([]byte, 20),
		AttestorSetHash:   make([]byte, 32),
	}

	if _, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{SrcChain: "chain-a", ExpectedStateRoot: []byte{1}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed root code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{SrcChain: "missing", ExpectedStateRoot: make([]byte, 32), RunMode: attestorpb.RunMode_RUN_MODE_FINALIZED}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown chain code = %v, want NotFound", status.Code(err))
	}

	ports.verifyErr = core.NewError(core.ErrorFailedPrecondition, "replica lag", nil)
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("lag code = %v, want FailedPrecondition", status.Code(err))
	}
	ports.verifyErr = core.NewError(core.ErrorUnavailable, "replica unavailable", errors.New("rpc down"))
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.Unavailable {
		t.Fatalf("RPC code = %v, want Unavailable", status.Code(err))
	}
	ports.verifyErr = nil
	ports.verdict = core.SignedBlockIdentityVerdict{Valid: false, BlockNumber: 100}
	response, err := client.VerifyStateRoot(context.Background(), request)
	if err != nil || response.GetValid() || len(response.GetAttestationSignature()) != 0 {
		t.Fatalf("mismatch response = (%+v, %v)", response, err)
	}
	// A positive verdict with no signature is a PLUGIN BUG, not a precondition
	// that clears on its own, so it must not share a code with "the replica has
	// not reached this block". The consumer classifies FailedPrecondition as
	// retryable; a bug retried at the L2 poll cadence is retried forever.
	ports.verdict = core.SignedBlockIdentityVerdict{Valid: true, BlockNumber: 100}
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.Internal {
		t.Fatalf("unsigned positive code = %v, want Internal", status.Code(err))
	}
	ports.verdict = validVerdict
	response, err = client.VerifyStateRoot(context.Background(), request)
	if err != nil || !response.GetValid() || len(response.GetAttestationSignature()) != signatureLength {
		t.Fatalf("signed response = (%+v, %v)", response, err)
	}
	if got := ports.lastRequest; got.L2Router != [20]byte{} || got.AttestorSetHash != [32]byte{} {
		t.Fatalf("zero signing context was not forwarded: %+v", got)
	}
}

func TestAdapterContractVerifyRequiresAndForwardsSigningContext(t *testing.T) {
	ports := &fakePorts{
		status:  core.FeedStatus{SrcChain: "chain-a"},
		verdict: core.SignedBlockIdentityVerdict{Valid: true, Signature: make([]byte, signatureLength)},
	}
	client := newContractClient(t, newContractServer(t, core.Ports{Verifier: ports, Status: ports}))
	request := &attestorpb.VerifyStateRootRequest{
		SrcChain: "chain-a", ExpectedStateRoot: make([]byte, 32), RunMode: attestorpb.RunMode_RUN_MODE_SAFE,
		L2Router: []byte{1, 2, 3}, AttestorSetHash: make([]byte, 32),
	}
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("short router code = %v, want InvalidArgument", status.Code(err))
	}
	request.L2Router = make([]byte, 20)
	request.L2Router[19] = 1
	request.AttestorSetHash[31] = 2
	if _, err := client.VerifyStateRoot(context.Background(), request); err != nil {
		t.Fatalf("verify with signing context: %v", err)
	}
	if got := ports.lastRequest; got.L2Router[19] != 1 || got.AttestorSetHash[31] != 2 {
		t.Fatalf("signing context = router=%x set_hash=%x, want request values", got.L2Router, got.AttestorSetHash)
	}
}

func TestAdapterContractWatchCapability(t *testing.T) {
	ports := &fakePorts{status: core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true}}
	withoutWatch := newContractClient(t, newContractServer(t, core.Ports{Status: ports}))
	stream, err := withoutWatch.WatchAttested(context.Background(), &attestorpb.WatchAttestedRequest{SrcChain: "chain-a"})
	if err != nil {
		t.Fatalf("WatchAttested start: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unimplemented {
		t.Fatalf("missing watcher code = %v, want Unimplemented", status.Code(err))

	}
	ports.updates = make(chan core.FrontierUpdate, 1)
	ports.updates <- core.FrontierUpdate{Found: true, Root: core.AttestedRoot{L2BlockNumber: 3, Root: make([]byte, 32)}}
	withWatch := newContractClient(t, newContractServer(t, core.Ports{Status: ports, Watcher: ports}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err = withWatch.WatchAttested(ctx, &attestorpb.WatchAttestedRequest{SrcChain: "chain-a"})
	if err != nil {
		t.Fatalf("WatchAttested: %v", err)
	}
	update, err := stream.Recv()
	if err != nil || update.GetRoot().GetL2BlockNumber() != 3 {
		t.Fatalf("watch update = (%+v, %v)", update, err)
	}
}

// Reported by @neitdung on #473. FailedPrecondition used to answer for a missing
// verifier, a missing feed, an absent chain table and an unsigned positive
// verdict as well as for a replica that has not reached the target block. The
// consumer classifies FailedPrecondition as retryable, so every one of those
// permanent conditions was retried at the L2 poll cadence forever and the
// permanent-failure hold could never fire.
//
// These are CONFIGURATION failures: a route wired without the port it is being
// asked for, or a daemon with no routes at all. They never clear on their own.
// The matching relayer half is TestServerCodesClassifyIntoTheRightOutcome in
// relayer/chain/l2rollup/attestorgrpc.
func TestCapabilityAndRouteFailuresDoNotLookLikeReplicaLag(t *testing.T) {
	ports := &fakePorts{status: core.FeedStatus{AttestationHead: core.RunModeFinalized}}

	// A route wired with a Status reader but no Feed or Verifier: reachable, and
	// unable to answer, permanently.
	partial := core.Ports{Status: ports}
	service, err := New(map[string]core.Ports{"chain-a": partial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := newContractClient(t, service)

	cases := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{
			name: "AttestedUpTo on a route with no commitment feed",
			call: func() error {
				_, err := client.AttestedUpTo(context.Background(), &attestorpb.AttestedUpToRequest{SrcChain: "chain-a"})
				return err
			},
			want: codes.Unimplemented,
		},
		{
			name: "AttestedRootAtOrBelow on a route with no commitment feed",
			call: func() error {
				_, err := client.AttestedRootAtOrBelow(context.Background(),
					&attestorpb.AttestedRootAtOrBelowRequest{SrcChain: "chain-a", L2BlockNumber: 10})
				return err
			},
			want: codes.Unimplemented,
		},
		{
			name: "VerifyStateRoot on a route with no block verifier",
			call: func() error {
				_, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{
					SrcChain:          "chain-a",
					BlockNumber:       10,
					ExpectedStateRoot: make([]byte, 32),
					RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
				})
				return err
			},
			want: codes.Unimplemented,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a route without the port answered successfully")
			}
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v; %v is what a replica that has not caught up returns, "+
					"so the relayer would retry this configuration mistake forever", got, tc.want, codes.FailedPrecondition)
			}
		})
	}
}
