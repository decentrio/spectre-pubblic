package host_test

import (
	"context"
	"net"
	"testing"
	"time"

	"attestor/core"
	"attestor/host"
	attestorpb "attestor/types/attestor"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// thirdChainPlugin is intentionally not an OP or Arbitrum type. It proves a
// future plugin only needs core ports plus composition wiring to use the host
// and shared AttestorService.
type thirdChainPlugin struct {
	started chan struct{}
	status  core.FeedStatus
	root    core.AttestedRoot
	verdict core.SignedBlockIdentityVerdict
}

func (p *thirdChainPlugin) Run(ctx context.Context) error {
	close(p.started)
	<-ctx.Done()
	return nil
}

func (p *thirdChainPlugin) Status(context.Context) (core.FeedStatus, error) {
	return p.status, nil
}

func (p *thirdChainPlugin) AttestedUpTo(context.Context, core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	return p.root.Clone(), true, nil
}

func (p *thirdChainPlugin) AttestedRootAtOrBelow(_ context.Context, height uint64, _ core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	if height < p.root.L2BlockNumber {
		return core.AttestedRoot{}, false, nil
	}
	return p.root.Clone(), true, nil
}

func (p *thirdChainPlugin) VerifyStateRoot(context.Context, core.BlockIdentityRequest) (core.SignedBlockIdentityVerdict, error) {
	return p.verdict.Clone(), nil
}

func TestHostExtensionContractWithThirdChainPlugin(t *testing.T) {
	plugin := &thirdChainPlugin{
		started: make(chan struct{}),
		status:  core.FeedStatus{SrcChain: "third-chain", AttestationHead: core.RunModeSafe, Ready: true, ReplicaSeen: true, ReplicaSafe: 42},
		root:    core.AttestedRoot{L2BlockNumber: 42, Root: make([]byte, 32), Source: "third-party"},
		verdict: core.SignedBlockIdentityVerdict{Valid: true, BlockNumber: 42, Signature: make([]byte, 64)},
	}
	listener := bufconn.Listen(1 << 20)
	attestorHost, err := host.New(host.Config{
		Routes: map[string]core.Ports{
			"third-chain": {Feed: plugin, Verifier: plugin, Status: plugin},
		},
		Runners:          []host.RunnerSpec{{Name: "third-plugin-loop", Runner: plugin}},
		Listener:         listener,
		HealthPoll:       time.Millisecond,
		EnableReflection: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- attestorHost.Run(ctx) }()
	<-plugin.started

	connection, err := newHostContractConnection(listener)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("dial host: %v", err)
	}
	defer connection.Close()
	client := attestorpb.NewAttestorServiceClient(connection)
	check := healthpb.NewHealthClient(connection)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, checkErr := check.Check(context.Background(), &healthpb.HealthCheckRequest{Service: attestorpb.AttestorService_ServiceDesc.ServiceName})
		if checkErr == nil && response.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			break
		}
		time.Sleep(time.Millisecond)
	}
	healthResponse, err := check.Check(context.Background(), &healthpb.HealthCheckRequest{Service: attestorpb.AttestorService_ServiceDesc.ServiceName})
	if err != nil || healthResponse.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = (%+v, %v), want SERVING", healthResponse, err)
	}

	info, err := client.Info(context.Background(), &attestorpb.InfoRequest{})
	if err != nil || len(info.GetChains()) != 1 || info.GetChains()[0].GetSrcChain() != "third-chain" {
		t.Fatalf("Info = (%+v, %v)", info, err)
	}
	frontier, err := client.AttestedUpTo(context.Background(), &attestorpb.AttestedUpToRequest{SrcChain: "third-chain"})
	if err != nil || !frontier.GetFound() || frontier.GetRoot().GetL2BlockNumber() != 42 {
		t.Fatalf("AttestedUpTo = (%+v, %v)", frontier, err)
	}
	bounded, err := client.AttestedRootAtOrBelow(context.Background(), &attestorpb.AttestedRootAtOrBelowRequest{SrcChain: "third-chain", L2BlockNumber: 42})
	if err != nil || !bounded.GetFound() {
		t.Fatalf("AttestedRootAtOrBelow = (%+v, %v)", bounded, err)
	}
	verdict, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{
		SrcChain:          "third-chain",
		BlockNumber:       42,
		ExpectedStateRoot: make([]byte, 32),
		RunMode:           attestorpb.RunMode_RUN_MODE_SAFE,
		L2Router:          make([]byte, 20),
		AttestorSetHash:   make([]byte, 32),
	})
	if err != nil || !verdict.GetValid() || len(verdict.GetAttestationSignature()) != 64 {
		t.Fatalf("VerifyStateRoot = (%+v, %v)", verdict, err)
	}
	stream, err := client.WatchAttested(context.Background(), &attestorpb.WatchAttestedRequest{SrcChain: "third-chain"})
	if err != nil {
		t.Fatalf("WatchAttested: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unimplemented {
		t.Fatalf("WatchAttested without watcher = %v, want Unimplemented", status.Code(err))
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancellation: %v", err)
	}
}

func newHostContractConnection(listener *bufconn.Listener) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"passthrough:///attestor-host-contract",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}
