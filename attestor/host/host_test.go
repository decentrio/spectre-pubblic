package host

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"attestor/core"
	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

type testStatusReader struct {
	mu     sync.RWMutex
	status core.FeedStatus
	err    error
}

func (s *testStatusReader) Status(context.Context) (core.FeedStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.err
}

func (s *testStatusReader) setReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Ready = ready
}

func healthStatus(t *testing.T, host *Host) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	response, err := host.health.Check(context.Background(), &healthpb.HealthCheckRequest{Service: attestorpb.AttestorService_ServiceDesc.ServiceName})
	if err != nil {
		t.Fatalf("health Check: %v", err)
	}
	return response.GetStatus()
}

func waitForHealth(t *testing.T, host *Host, want healthpb.HealthCheckResponse_ServingStatus) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := healthStatus(t, host); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("health = %s, want %s", healthStatus(t, host), want)
}

func TestHostLifecycleAggregatesReadinessAndStopsGracefully(t *testing.T) {
	status := &testStatusReader{status: core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized}}
	started := make(chan struct{})
	host, err := New(Config{
		Routes: map[string]core.Ports{"chain-a": {Status: status}},
		Runners: []RunnerSpec{{Name: "loop", Runner: RunnerFunc(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})}},
		HealthPoll: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	<-started
	waitForHealth(t, host, healthpb.HealthCheckResponse_NOT_SERVING)
	status.setReady(true)
	waitForHealth(t, host, healthpb.HealthCheckResponse_SERVING)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run after cancellation: %v", err)
	}
	if got := healthStatus(t, host); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health after shutdown = %s, want NOT_SERVING", got)
	}
}

func TestHostRunnerFailureFailsClosed(t *testing.T) {
	status := &testStatusReader{status: core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true}}
	host, err := New(Config{
		Routes: map[string]core.Ports{"chain-a": {Status: status}},
		Runners: []RunnerSpec{{Name: "failing", Runner: RunnerFunc(func(context.Context) error {
			return errors.New("synthetic failure")
		})}},
		HealthPoll: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = host.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "synthetic failure") {
		t.Fatalf("Run error = %v, want runner failure", err)
	}
	if got := healthStatus(t, host); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health after runner failure = %s, want NOT_SERVING", got)
	}
}

func TestHostRunnerFailurePublishesNotServingOverGRPCBeforeDrain(t *testing.T) {
	status := &testStatusReader{status: core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true}}
	fail := make(chan struct{})
	started := make(chan struct{})
	listener := bufconn.Listen(1 << 20)
	host, err := New(Config{
		Routes: map[string]core.Ports{
			"chain-a": {
				Status: status,
			},
		},
		Runners: []RunnerSpec{{Name: "failing", Runner: RunnerFunc(func(context.Context) error {
			close(started)
			<-fail
			return errors.New("synthetic failure")
		})}},
		Listener:   listener,
		HealthPoll: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- host.Run(context.Background()) }()
	<-started

	connection, err := googlegrpc.NewClient(
		"passthrough:///attestor-host-health",
		googlegrpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial host: %v", err)
	}
	defer connection.Close()
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	stream, err := healthpb.NewHealthClient(connection).Watch(watchCtx, &healthpb.HealthCheckRequest{Service: attestorpb.AttestorService_ServiceDesc.ServiceName})
	if err != nil {
		t.Fatalf("start health watch: %v", err)
	}
	initial, err := stream.Recv()
	if err != nil || initial.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("initial health = (%+v, %v), want SERVING", initial, err)
	}

	close(fail)
	notServing, err := stream.Recv()
	if err != nil || notServing.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("draining health = (%+v, %v), want NOT_SERVING", notServing, err)
	}

	cancelWatch()
	if err := <-runDone; err == nil || !strings.Contains(err.Error(), "synthetic failure") {
		t.Fatalf("Run error = %v, want runner failure", err)
	}
}

func TestHostRejectsInvalidComposition(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted empty routes")
	}
	status := &testStatusReader{status: core.FeedStatus{SrcChain: "chain-a"}}
	if _, err := New(Config{Routes: map[string]core.Ports{"chain-a": {Status: status}}, Runners: []RunnerSpec{{Name: "", Runner: RunnerFunc(func(context.Context) error { return nil })}}}); err == nil {
		t.Fatal("New accepted unnamed runner")
	}
}
