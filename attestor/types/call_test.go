package types

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestCallBoundWithoutTimeoutLeavesTheContextAlone(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		parent := context.Background()
		err := CallBound{Timeout: timeout}.Do(parent, "call", func(ctx context.Context) error {
			if ctx != parent {
				t.Fatalf("timeout %s: call got a derived context, want the caller's", timeout)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("timeout %s: %v", timeout, err)
		}
	}
}

func TestCallBoundCutsACallThatNeverReturns(t *testing.T) {
	started := time.Now()
	err := CallBound{Timeout: 50 * time.Millisecond}.Do(context.Background(), "L1 AssertionStatus", func(ctx context.Context) error {
		<-ctx.Done() // a call parked on a connection that never answers
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("Do returned nil for a call cut by its deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %q does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "L1 AssertionStatus exceeded its 50ms deadline") {
		t.Fatalf("error %q does not name the call and its deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("call was cut after %s, want about 50ms", elapsed)
	}
}

func TestCallBoundReturnsParentCancellationUnlabelled(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	err := CallBound{Timeout: time.Hour}.Do(parent, "call", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "deadline") {
		t.Fatalf("shutdown reported as a call deadline: %q", err)
	}
}

func TestCallBoundDoesNotBlameTheDeadlineForATransportTimeout(t *testing.T) {
	// An HTTP client timeout also satisfies errors.Is(err, DeadlineExceeded),
	// while the call's context is still live.
	transport := errors.New("Post: Client.Timeout exceeded")
	err := CallBound{Timeout: time.Hour}.Do(context.Background(), "call", func(context.Context) error {
		return errors.Join(transport, context.DeadlineExceeded)
	})
	if err == nil || strings.Contains(err.Error(), "exceeded its") {
		t.Fatalf("transport timeout reported as %v, want it returned as is", err)
	}
}

func TestCallBoundPassesThroughOrdinaryResults(t *testing.T) {
	bound := CallBound{Timeout: time.Hour}
	if err := bound.Do(context.Background(), "call", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("success became %v", err)
	}
	failure := errors.New("upstream said no")
	if err := bound.Do(context.Background(), "call", func(context.Context) error { return failure }); err != failure {
		t.Fatalf("failure became %v, want it returned as is", err)
	}
}

func TestCallReturnsTheValueOfTheCall(t *testing.T) {
	bound := CallBound{Timeout: time.Hour}
	got, err := Call(context.Background(), bound, "call", func(context.Context) (uint64, error) { return 42, nil })
	if err != nil || got != 42 {
		t.Fatalf("Call = (%d, %v), want (42, nil)", got, err)
	}
	failure := errors.New("upstream said no")
	if _, err := Call(context.Background(), bound, "call", func(context.Context) (uint64, error) { return 0, failure }); err != failure {
		t.Fatalf("Call error = %v, want it returned as is", err)
	}
}

// The beat is how a runner tells a pass that is making progress from a loop that
// is parked, so it must fire for every way a call can end.
func TestCallBoundBeatsWhenACallReturns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		call    func(context.Context) error
	}{
		{"success", time.Hour, func(context.Context) error { return nil }},
		{"failure", time.Hour, func(context.Context) error { return errors.New("no") }},
		{"cut by its deadline", 20 * time.Millisecond, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{"no deadline configured", 0, func(context.Context) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var beats atomic.Int32
			_ = CallBound{Timeout: tc.timeout, Beat: func() { beats.Add(1) }}.Do(context.Background(), "call", tc.call)
			if got := beats.Load(); got != 1 {
				t.Fatalf("%d beats, want 1", got)
			}
		})
	}
}

func TestFeedStaleAfterCoversOneCallAndTwoTicks(t *testing.T) {
	if got := FeedStaleAfter(5*time.Minute, 30*time.Second); got != 6*time.Minute {
		t.Fatalf("FeedStaleAfter = %s, want 6m0s", got)
	}
}

func TestHeartbeatAliveWindow(t *testing.T) {
	const maxAge = time.Minute
	start := time.Unix(1_000_000, 0)
	var beat Heartbeat

	if beat.Alive(start, maxAge) {
		t.Fatal("a heartbeat that never beat is alive")
	}
	beat.Beat(start)
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"at the beat", start, true},
		{"just inside the window", start.Add(maxAge), true},
		{"just outside the window", start.Add(maxAge + time.Nanosecond), false},
	} {
		if got := beat.Alive(tc.now, maxAge); got != tc.want {
			t.Errorf("%s: Alive = %t, want %t", tc.name, got, tc.want)
		}
	}
	beat.Beat(start.Add(10 * time.Minute))
	if !beat.Alive(start.Add(10*time.Minute+time.Second), maxAge) {
		t.Fatal("a fresh beat did not revive the heartbeat")
	}
}

// silentBlockNumberService receives the request and never answers it, as a
// WebSocket peer that keeps its connection up and drops one response does.
type silentBlockNumberService struct{}

func (silentBlockNumberService) BlockNumber(ctx context.Context) (hexutil.Uint64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

// DialRPC puts no timeout on a WebSocket connection, so a call on one that is
// never answered is bounded only by the context it is made with. That is what
// makes the call bound the thing that frees a feed runner, for the endpoints a
// transport timeout cannot reach.
func TestCallBoundFreesACallOnAWebSocketThatNeverAnswers(t *testing.T) {
	rpcServer := rpc.NewServer()
	t.Cleanup(rpcServer.Stop)
	if err := rpcServer.RegisterName("eth", silentBlockNumberService{}); err != nil {
		t.Fatalf("register RPC service: %v", err)
	}
	server := httptest.NewServer(rpcServer.WebsocketHandler(nil))
	t.Cleanup(server.Close)

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialRPC(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("DialRPC: %v", err)
	}
	t.Cleanup(client.Close)

	started := time.Now()
	err = CallBound{Timeout: 100 * time.Millisecond}.Do(context.Background(), "eth_blockNumber", func(ctx context.Context) error {
		var blockNumber hexutil.Uint64
		return client.CallContext(ctx, &blockNumber, "eth_blockNumber")
	})
	if err == nil || !strings.Contains(err.Error(), "eth_blockNumber exceeded its 100ms deadline") {
		t.Fatalf("call on a silent WebSocket = %v, want the call deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the call was freed after %s, want about 100ms", elapsed)
	}

	// The same call with only the dial's 50ms transport timeout in play, and no
	// call bound, is not freed by it: that timeout does not apply to a
	// WebSocket. The caller's own context is the only bound.
	probe, probeCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer probeCancel()
	var blockNumber hexutil.Uint64
	probeStarted := time.Now()
	err = client.CallContext(probe, &blockNumber, "eth_blockNumber")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call bounded by the caller's context = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(probeStarted); elapsed < 250*time.Millisecond {
		t.Fatalf("the call returned after %s: the dial's transport timeout freed a WebSocket call", elapsed)
	}
}
