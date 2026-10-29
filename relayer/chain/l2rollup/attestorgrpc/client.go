// Package attestorgrpc binds the L2 attestor sidecar's gRPC AttestorService
// (#240/#258) to the l2rollup.AttestorClient interface the source gates on. It is a
// thin transport layer: the generated proto AttestedRoot is returned as-is (the
// attestor module owns that shape), so there is no mapping to keep in sync.
package attestorgrpc

import (
	"context"
	"fmt"
	"strings"

	attestorpb "attestor/types/attestor"
	"relayer/chain/l2rollup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// mapStatus translates transport codes into the consumer-side error taxonomy.
// Unknown codes remain unclassified and therefore default to transient.
func mapStatus(err error) error {
	var sentinel error
	switch status.Code(err) {
	case codes.InvalidArgument:
		sentinel = l2rollup.ErrAttestorBadRequest
	case codes.NotFound:
		sentinel = l2rollup.ErrAttestorUnknownRoute
	case codes.FailedPrecondition:
		sentinel = l2rollup.ErrAttestorReplicaBehind
	case codes.Unavailable, codes.DeadlineExceeded:
		sentinel = l2rollup.ErrAttestorUnavailable
	case codes.Unimplemented:
		sentinel = l2rollup.ErrAttestorUnimplemented
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// Client adapts the generated AttestorServiceClient to l2rollup.AttestorClient.
type Client struct {
	conn *grpc.ClientConn
	rpc  attestorpb.AttestorServiceClient
}

var _ l2rollup.AttestorClient = (*Client)(nil)

// Dial connects to the attestor sidecar at addr (host:port). The transport is
// insecure (the sidecar is a co-located trusted process); the caller must Close it.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("attestorgrpc: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, rpc: attestorpb.NewAttestorServiceClient(conn)}, nil
}

// Close releases the gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }

// Info returns every chain the attestor is configured to serve, with the head
// each feed gates on. It is the one call that answers "is this the right
// attestor for this L2, and does it sign at the head this relayer will ask
// for" before any header is built; startup validation reads it once.
func (c *Client) Info(ctx context.Context) ([]*attestorpb.ChainInfo, error) {
	resp, err := c.rpc.Info(ctx, &attestorpb.InfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("attestorgrpc: Info: %w", mapStatus(err))
	}
	return resp.GetChains(), nil
}

// AttestedUpTo returns the attestor's highest independently-re-derived frontier.
func (c *Client) AttestedUpTo(ctx context.Context, srcChain string, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error) {
	if err := requireSrcChain(srcChain); err != nil {
		return nil, false, err
	}
	resp, err := c.rpc.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{
		SrcChain:           srcChain,
		IncludeProvisional: includeProvisional,
	})
	if err != nil {
		return nil, false, fmt.Errorf("attestorgrpc: AttestedUpTo(%s): %w", srcChain, mapStatus(err))
	}
	if !resp.GetFound() {
		return nil, false, nil
	}
	if resp.GetRoot() == nil {
		return nil, false, fmt.Errorf("attestorgrpc: AttestedUpTo(%s) found=true but root is nil", srcChain)
	}
	return resp.GetRoot(), true, nil
}

// AttestedRootAtOrBelow returns the best attested root at or below l2BlockNumber.
func (c *Client) AttestedRootAtOrBelow(ctx context.Context, srcChain string, l2BlockNumber uint64, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error) {
	if err := requireSrcChain(srcChain); err != nil {
		return nil, false, err
	}
	resp, err := c.rpc.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
		SrcChain:           srcChain,
		L2BlockNumber:      l2BlockNumber,
		IncludeProvisional: includeProvisional,
	})
	if err != nil {
		return nil, false, fmt.Errorf("attestorgrpc: AttestedRootAtOrBelow(%s, %d): %w", srcChain, l2BlockNumber, mapStatus(err))
	}
	if !resp.GetFound() {
		return nil, false, nil
	}
	if resp.GetRoot() == nil {
		return nil, false, fmt.Errorf("attestorgrpc: AttestedRootAtOrBelow(%s, %d) found=true but root is nil", srcChain, l2BlockNumber)
	}
	return resp.GetRoot(), true, nil
}

// VerifyStateRoot compares one block identity against the attestor's replica.
func (c *Client) VerifyStateRoot(ctx context.Context, request l2rollup.VerificationRequest) (l2rollup.SignedVerdict, error) {
	if err := requireSrcChain(request.SrcChain); err != nil {
		return l2rollup.SignedVerdict{}, err
	}
	resp, err := c.rpc.VerifyStateRoot(ctx, &attestorpb.VerifyStateRootRequest{
		SrcChain:          request.SrcChain,
		BlockNumber:       request.BlockNumber,
		ExpectedStateRoot: request.StateRoot,
		ExpectedBlockHash: request.BlockHash,
		RunMode:           request.RunMode,
		L2Router:          request.L2Router[:],
		AttestorSetHash:   request.AttestorSetHash[:],
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return l2rollup.SignedVerdict{}, fmt.Errorf("%w: %w", l2rollup.ErrVerifyStateRootUnsupported, err)
		}
		return l2rollup.SignedVerdict{}, fmt.Errorf("attestorgrpc: VerifyStateRoot(%d): %w", request.BlockNumber, mapStatus(err))
	}
	return l2rollup.SignedVerdict{
		Valid:       resp.GetValid(),
		BlockNumber: resp.GetBlockNumber(),
		BlockHash:   append([]byte(nil), resp.GetBlockHash()...),
		StateRoot:   append([]byte(nil), resp.GetStateRoot()...),
		Signature:   append([]byte(nil), resp.GetAttestationSignature()...),
	}, nil
}

func requireSrcChain(srcChain string) error {
	if strings.TrimSpace(srcChain) == "" {
		return fmt.Errorf("%w: attestorgrpc: src_chain is required", l2rollup.ErrAttestorBadRequest)
	}
	return nil
}
