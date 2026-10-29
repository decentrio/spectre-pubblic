package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"

	"attestor/core"
	attestorpb "attestor/types/attestor"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// leakyKey stands for an RPC provider API key in the upstream endpoint's URL.
const leakyKey = "DUMMY-KEY-0123456789"

// transportFailure is what net/http reports when an upstream connection drops:
// the full URL, of which it strips only the userinfo password.
func transportFailure() error {
	return fmt.Errorf("optimism_syncStatus failed: %w", &url.Error{
		Op:  "Post",
		URL: "http://127.0.0.1:9545/rpc?apikey=" + leakyKey,
		Err: io.EOF,
	})
}

func assertNoLeak(t *testing.T, what string, err error, wantCode codes.Code, wantMessage string) {
	t.Helper()
	if status.Code(err) != wantCode {
		t.Fatalf("%s: code = %v, want %v (err %v)", what, status.Code(err), wantCode, err)
	}
	message := status.Convert(err).Message()
	if message != wantMessage {
		t.Fatalf("%s: message = %q, want exactly %q", what, message, wantMessage)
	}
	for _, leaked := range []string{leakyKey, "apikey", "127.0.0.1", "9545", "EOF", "http://"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("%s: caller-visible error %q carries %q", what, err, leaked)
		}
	}
}

// The API is unauthenticated, so anything an error carries is readable by every
// peer that can reach it. A classified error's cause is where the upstream
// transport error, and the endpoint URL it quotes, lives.
func TestNoRPCReturnsTheCauseOfAnUpstreamFailure(t *testing.T) {
	cause := transportFailure()
	ports := &fakePorts{
		status:    core.FeedStatus{SrcChain: "chain-a", AttestationHead: core.RunModeFinalized, Ready: true},
		statusErr: core.NewError(core.ErrorUnavailable, "read replica status", cause),
	}
	client := newContractClient(t, newContractServer(t, core.Ports{Status: ports, Feed: ports, Verifier: ports, Watcher: ports}))
	ctx := context.Background()

	_, err := client.Info(ctx, &attestorpb.InfoRequest{})
	assertNoLeak(t, "Info", err, codes.Unavailable, "read replica status")

	ports.statusErr = nil
	ports.feedErr = core.NewError(core.ErrorUnavailable, "read the attested feed", cause)
	_, err = client.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{SrcChain: "chain-a"})
	assertNoLeak(t, "AttestedUpTo", err, codes.Unavailable, "read the attested feed")
	_, err = client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{SrcChain: "chain-a", L2BlockNumber: 1})
	assertNoLeak(t, "AttestedRootAtOrBelow", err, codes.Unavailable, "read the attested feed")

	ports.verifyErr = core.NewError(core.ErrorUnavailable, "read replica finalized head", cause)
	_, err = client.VerifyStateRoot(ctx, &attestorpb.VerifyStateRootRequest{
		SrcChain:          "chain-a",
		ExpectedStateRoot: make([]byte, 32),
		L2Router:          make([]byte, 20),
		AttestorSetHash:   make([]byte, 32),
		RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
	})
	assertNoLeak(t, "VerifyStateRoot", err, codes.Unavailable, "read replica finalized head")

	ports.watchErr = core.NewError(core.ErrorUnavailable, "watch the attested feed", cause)
	stream, err := client.WatchAttested(ctx, &attestorpb.WatchAttestedRequest{SrcChain: "chain-a"})
	if err != nil {
		t.Fatalf("WatchAttested: %v", err)
	}
	_, err = stream.Recv()
	assertNoLeak(t, "WatchAttested", err, codes.Unavailable, "watch the attested feed")
}

// An error with no classified message gets a fixed one for its class: the text
// of an unclassified error is by definition not something its author vetted.
func TestUnclassifiedErrorsGetAFixedMessage(t *testing.T) {
	cause := transportFailure()
	for _, tc := range []struct {
		name    string
		err     error
		code    codes.Code
		message string
	}{
		{"unclassified", fmt.Errorf("boom: %w", cause), codes.Internal, "internal error"},
		{"classified without a message", core.NewError(core.ErrorUnavailable, "", cause), codes.Unavailable, "upstream unavailable"},
		{"invalid argument", core.NewError(core.ErrorInvalidArgument, "", cause), codes.InvalidArgument, "invalid argument"},
		{"not found", core.NewError(core.ErrorNotFound, "", cause), codes.NotFound, "not found"},
		{"failed precondition", core.NewError(core.ErrorFailedPrecondition, "", cause), codes.FailedPrecondition, "failed precondition"},
		{"unimplemented", core.NewError(core.ErrorUnimplemented, "", cause), codes.Unimplemented, "not implemented"},
		{"internal", core.NewError(core.ErrorInternal, "", cause), codes.Internal, "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ports := &fakePorts{verifyErr: tc.err}
			client := newContractClient(t, newContractServer(t, core.Ports{Status: ports, Verifier: ports}))
			_, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{
				SrcChain:          "chain-a",
				ExpectedStateRoot: make([]byte, 32),
				L2Router:          make([]byte, 20),
				AttestorSetHash:   make([]byte, 32),
				RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
			})
			assertNoLeak(t, tc.name, err, tc.code, tc.message)
		})
	}
}

type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *capturedLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// What the caller no longer gets is not lost: the server logs the cause, with
// the key redacted, so an operator can still see why the upstream failed.
func TestTheCauseIsLoggedRedactedNotDiscarded(t *testing.T) {
	var logs capturedLog
	ports := &fakePorts{}
	service, err := New(map[string]core.Ports{"chain-a": {Status: ports, Verifier: ports}}, WithErrorLog(logs.logf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := newContractClient(t, service)
	request := &attestorpb.VerifyStateRootRequest{
		SrcChain:          "chain-a",
		ExpectedStateRoot: make([]byte, 32),
		L2Router:          make([]byte, 20),
		AttestorSetHash:   make([]byte, 32),
		RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
	}

	ports.verifyErr = core.NewError(core.ErrorUnavailable, "read replica finalized head", transportFailure())
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.Unavailable {
		t.Fatalf("VerifyStateRoot code = %v", status.Code(err))
	}
	lines := logs.snapshot()
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want 1: %q", len(lines), lines)
	}
	for _, want := range []string{"VerifyStateRoot", "read replica finalized head", "optimism_syncStatus failed", "http://127.0.0.1:9545", "EOF"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %q is missing %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], leakyKey) || strings.Contains(lines[0], "apikey") {
		t.Fatalf("log line %q carries the key", lines[0])
	}

	// An error whose whole text the caller already sees has nothing to add.
	ports.verifyErr = core.NewError(core.ErrorFailedPrecondition, "replica lag", nil)
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("VerifyStateRoot code = %v", status.Code(err))
	}
	if got := logs.snapshot(); len(got) != 1 {
		t.Fatalf("an error with no hidden cause was logged: %q", got)
	}

	// An unclassified error is logged too: its text is all hidden.
	ports.verifyErr = errors.New("unclassified failure")
	if _, err := client.VerifyStateRoot(context.Background(), request); status.Code(err) != codes.Internal {
		t.Fatalf("VerifyStateRoot code = %v", status.Code(err))
	}
	if got := logs.snapshot(); len(got) != 2 || !strings.Contains(got[1], "unclassified failure") {
		t.Fatalf("unclassified error log = %q", got)
	}
}

// The server log is where the cause goes now, so it is where a key that a
// redaction missed would be written. The query shapes are the ones a text scan
// for the end of a URL can get wrong.
func TestTheLoggedCauseIsRedactedForAnyQueryShape(t *testing.T) {
	for _, query := range []string{
		"apikey=" + leakyKey,
		"label='node'&apikey=" + leakyKey,
		"a=(b);c,d!*&apikey=" + leakyKey,
		`q="a"&apikey=` + leakyKey,
		"x='1'&apikey=" + leakyKey + "&y='2'",
	} {
		t.Run(query, func(t *testing.T) {
			var logs capturedLog
			ports := &fakePorts{verifyErr: core.NewError(core.ErrorUnavailable, "read replica finalized head",
				fmt.Errorf("optimism_syncStatus failed: %w", &url.Error{Op: "Post", URL: "http://127.0.0.1:9545/rpc?" + query, Err: io.EOF}))}
			service, err := New(map[string]core.Ports{"chain-a": {Status: ports, Verifier: ports}}, WithErrorLog(logs.logf))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			client := newContractClient(t, service)
			if _, err := client.VerifyStateRoot(context.Background(), &attestorpb.VerifyStateRootRequest{
				SrcChain:          "chain-a",
				ExpectedStateRoot: make([]byte, 32),
				L2Router:          make([]byte, 20),
				AttestorSetHash:   make([]byte, 32),
				RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
			}); status.Code(err) != codes.Unavailable {
				t.Fatalf("VerifyStateRoot code = %v", status.Code(err))
			}
			lines := logs.snapshot()
			if len(lines) != 1 {
				t.Fatalf("%d log lines, want 1: %q", len(lines), lines)
			}
			if !strings.Contains(lines[0], `Post "http://127.0.0.1:9545": EOF`) {
				t.Errorf("log line %q should name the host and the failure", lines[0])
			}
			for _, leaked := range []string{leakyKey, "apikey", "node", "/rpc"} {
				if strings.Contains(lines[0], leaked) {
					t.Errorf("log line %q carries %q", lines[0], leaked)
				}
			}
		})
	}
}
