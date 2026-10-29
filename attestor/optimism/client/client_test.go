package client

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	attestorpb "attestor/types/attestor"
)

type fakeAttestorServiceClient struct {
	infoResponse       *attestorpb.InfoResponse
	infoErr            error
	upToResponse       *attestorpb.AttestedUpToResponse
	upToErr            error
	atOrBelowResponse  *attestorpb.AttestedRootAtOrBelowResponse
	atOrBelowErr       error
	watchErr           error
	watchStream        grpc.ServerStreamingClient[attestorpb.WatchAttestedResponse]
	verifyStateRootErr error
}

func (f *fakeAttestorServiceClient) Info(
	context.Context,
	*attestorpb.InfoRequest,
	...grpc.CallOption,
) (*attestorpb.InfoResponse, error) {
	return f.infoResponse, f.infoErr
}

func (f *fakeAttestorServiceClient) AttestedUpTo(
	context.Context,
	*attestorpb.AttestedUpToRequest,
	...grpc.CallOption,
) (*attestorpb.AttestedUpToResponse, error) {
	return f.upToResponse, f.upToErr
}

func (f *fakeAttestorServiceClient) AttestedRootAtOrBelow(
	context.Context,
	*attestorpb.AttestedRootAtOrBelowRequest,
	...grpc.CallOption,
) (*attestorpb.AttestedRootAtOrBelowResponse, error) {
	return f.atOrBelowResponse, f.atOrBelowErr
}

func (f *fakeAttestorServiceClient) WatchAttested(
	context.Context,
	*attestorpb.WatchAttestedRequest,
	...grpc.CallOption,
) (grpc.ServerStreamingClient[attestorpb.WatchAttestedResponse], error) {
	return f.watchStream, f.watchErr
}

type fakeWatchStream struct {
	ctx      context.Context
	messages []*attestorpb.WatchAttestedResponse
	err      error
}

func (s *fakeWatchStream) Recv() (*attestorpb.WatchAttestedResponse, error) {
	if len(s.messages) != 0 {
		message := s.messages[0]
		s.messages = s.messages[1:]
		return message, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func (s *fakeWatchStream) Header() (metadata.MD, error) { return nil, nil }
func (s *fakeWatchStream) Trailer() metadata.MD         { return nil }
func (s *fakeWatchStream) CloseSend() error             { return nil }
func (s *fakeWatchStream) Context() context.Context     { return s.ctx }
func (s *fakeWatchStream) SendMsg(any) error            { return nil }
func (s *fakeWatchStream) RecvMsg(any) error            { return nil }

func (f *fakeAttestorServiceClient) VerifyStateRoot(
	context.Context,
	*attestorpb.VerifyStateRootRequest,
	...grpc.CallOption,
) (*attestorpb.VerifyStateRootResponse, error) {
	return nil, f.verifyStateRootErr
}

func TestFromPBFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		root []byte
		want string
	}{
		{name: "nil root", want: "0-byte root"},
		{name: "short root", root: make([]byte, 31), want: "31-byte root"},
		{name: "long root", root: make([]byte, 33), want: "33-byte root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fromPB(&attestorpb.AttestedRoot{Root: tc.root})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fromPB error = %v, want %q", err, tc.want)
			}
		})
	}

	root := make([]byte, 32)
	root[0] = 0xab
	entry, err := fromPB(&attestorpb.AttestedRoot{
		L2BlockNumber: 100,
		Root:          root,
		Source:        "derived",
		Provisional:   true,
		AttestedAt:    1234,
	})
	if err != nil {
		t.Fatalf("fromPB valid entry: %v", err)
	}
	if entry.L2BlockNumber != 100 || entry.Root[0] != 0xab || !entry.Provisional || entry.AttestedAt.Unix() != 1234 {
		t.Fatalf("fromPB entry = %+v", entry)
	}
}

func TestClientUnaryFailureMatrix(t *testing.T) {
	ctx := context.Background()
	t.Run("Info RPC error", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{infoErr: errors.New("unavailable")}}
		if _, err := client.Info(ctx); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("Info error = %v", err)
		}
	})
	t.Run("AttestedUpTo RPC error", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{upToErr: errors.New("unavailable")}}
		if _, _, err := client.AttestedUpTo(ctx, "op", false); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("AttestedUpTo error = %v", err)
		}
	})
	t.Run("AttestedUpTo not found", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{upToResponse: &attestorpb.AttestedUpToResponse{Found: false}}}
		if _, found, err := client.AttestedUpTo(ctx, "op", false); err != nil || found {
			t.Fatalf("AttestedUpTo = found=%t err=%v, want not found", found, err)
		}
	})
	t.Run("AttestedUpTo malformed root", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{upToResponse: &attestorpb.AttestedUpToResponse{Found: true, Root: &attestorpb.AttestedRoot{Root: []byte{1}}}}}
		if _, _, err := client.AttestedUpTo(ctx, "op", false); err == nil || !strings.Contains(err.Error(), "1-byte root") {
			t.Fatalf("AttestedUpTo malformed root error = %v", err)
		}
	})
	t.Run("AtOrBelow RPC error", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{atOrBelowErr: errors.New("unavailable")}}
		if _, _, err := client.AttestedRootAtOrBelow(ctx, "op", 100, false); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("AttestedRootAtOrBelow error = %v", err)
		}
	})
	t.Run("AtOrBelow not found", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{atOrBelowResponse: &attestorpb.AttestedRootAtOrBelowResponse{Found: false}}}
		if _, found, err := client.AttestedRootAtOrBelow(ctx, "op", 100, false); err != nil || found {
			t.Fatalf("AttestedRootAtOrBelow = found=%t err=%v, want not found", found, err)
		}
	})
	t.Run("AtOrBelow malformed root", func(t *testing.T) {
		client := &Client{svc: &fakeAttestorServiceClient{atOrBelowResponse: &attestorpb.AttestedRootAtOrBelowResponse{Found: true, Root: &attestorpb.AttestedRoot{Root: []byte{1}}}}}
		if _, _, err := client.AttestedRootAtOrBelow(ctx, "op", 100, false); err == nil || !strings.Contains(err.Error(), "1-byte root") {
			t.Fatalf("AttestedRootAtOrBelow malformed root error = %v", err)
		}
	})
}

func TestWatchAttestedReturnsStartFailure(t *testing.T) {
	client := &Client{svc: &fakeAttestorServiceClient{watchErr: errors.New("stream unavailable")}}
	err := client.WatchAttested(context.Background(), "op", false, func(AttestedRoot) error {
		t.Fatal("watch callback ran after stream start failure")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "stream unavailable") {
		t.Fatalf("WatchAttested error = %v", err)
	}
}

func TestWatchAttestedFailureMatrix(t *testing.T) {
	validRoot := make([]byte, 32)
	validRoot[0] = 0xab
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		stream  *fakeWatchStream
		fn      func(AttestedRoot) error
		wantErr string
	}{
		{
			name: "stream ends unexpectedly",
			ctx:  context.Background(),
			stream: &fakeWatchStream{
				ctx: context.Background(),
			},
			fn:      func(AttestedRoot) error { return nil },
			wantErr: "ended",
		},
		{
			name: "malformed stream root",
			ctx:  context.Background(),
			stream: &fakeWatchStream{
				ctx:      context.Background(),
				messages: []*attestorpb.WatchAttestedResponse{{Root: &attestorpb.AttestedRoot{Root: []byte{1}}}},
			},
			fn:      func(AttestedRoot) error { return nil },
			wantErr: "1-byte root",
		},
		{
			name: "consumer rejects entry",
			ctx:  context.Background(),
			stream: &fakeWatchStream{
				ctx:      context.Background(),
				messages: []*attestorpb.WatchAttestedResponse{{Root: &attestorpb.AttestedRoot{Root: validRoot}}},
			},
			fn:      func(AttestedRoot) error { return errors.New("consumer stopped") },
			wantErr: "consumer stopped",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{svc: &fakeAttestorServiceClient{watchStream: tc.stream}}
			err := client.WatchAttested(tc.ctx, "op", false, tc.fn)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("WatchAttested error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client := &Client{svc: &fakeAttestorServiceClient{watchStream: &fakeWatchStream{ctx: canceled}}}
	if err := client.WatchAttested(canceled, "op", false, func(AttestedRoot) error { return nil }); err != nil {
		t.Fatalf("WatchAttested cancellation error = %v, want nil", err)
	}
}
