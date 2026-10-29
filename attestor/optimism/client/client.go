// Package client is the relayer-side handle to the attestor sidecar. It
// mirrors the read interface of the in-process AttestedRootStore so a future
// op_to_cosmos module can consume the feed the same way whether the attestor
// runs in-process or as a sidecar next to the replica.
package client

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"attestor/optimism"
	attestorpb "attestor/types/attestor"
)

// AttestedRoot is one feed entry as seen by a consumer.
type AttestedRoot struct {
	L2BlockNumber uint64
	Root          [32]byte
	// Source is "derived" (self-computed by the attestor) or "game"
	// (a DisputeGameFactory proposal confirmed by replay).
	Source string
	// GameIndex is meaningful only when Source is "game".
	GameIndex uint64
	// Provisional roots await the attestor's finalized-head recheck.
	Provisional bool
	AttestedAt  time.Time
}

// Client talks to one attestor sidecar (plaintext gRPC — the sidecar is
// expected to sit on localhost or a private network next to the relayer).
type Client struct {
	conn *grpc.ClientConn
	svc  attestorpb.AttestorServiceClient
}

// Dial connects to the sidecar at target (host:port).
func Dial(target string) (*Client, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to dial attestor sidecar %s: %w", target, err)
	}
	return &Client{conn: conn, svc: attestorpb.NewAttestorServiceClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func fromPB(pb *attestorpb.AttestedRoot) (AttestedRoot, error) {
	if len(pb.GetRoot()) != 32 {
		return AttestedRoot{}, fmt.Errorf("attestor returned %d-byte root, want 32", len(pb.GetRoot()))
	}
	var root [32]byte
	copy(root[:], pb.GetRoot())
	return AttestedRoot{
		L2BlockNumber: pb.GetL2BlockNumber(),
		Root:          root,
		Source:        pb.GetSource(),
		GameIndex:     pb.GetGameIndex(),
		Provisional:   pb.GetProvisional(),
		AttestedAt:    time.Unix(pb.GetAttestedAt(), 0),
	}, nil
}

// Info returns the sidecar's per-chain status.
func (c *Client) Info(ctx context.Context) ([]*attestorpb.ChainInfo, error) {
	resp, err := c.svc.Info(ctx, &attestorpb.InfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("attestor Info failed: %w", err)
	}
	return resp.GetChains(), nil
}

// AttestedUpTo returns the attested frontier for a chain as a cursor; ok is
// false when nothing qualifying has been attested yet.
func (c *Client) AttestedUpTo(ctx context.Context, srcChain string, includeProvisional bool) (attestor.Cursor, bool, error) {
	resp, err := c.svc.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{
		SrcChain:           srcChain,
		IncludeProvisional: includeProvisional,
	})
	if err != nil {
		return attestor.Cursor{}, false, fmt.Errorf("attestor AttestedUpTo(%s) failed: %w", srcChain, err)
	}
	if !resp.GetFound() {
		return attestor.Cursor{}, false, nil
	}
	entry, err := fromPB(resp.GetRoot())
	if err != nil {
		return attestor.Cursor{}, false, err
	}
	return attestor.Cursor{Height: entry.L2BlockNumber, ID: entry.Root}, true, nil
}

// AttestedRootAtOrBelow returns the highest attested root at or below l2Block
// — the query the relay path uses to pick a root covering a packet's height.
func (c *Client) AttestedRootAtOrBelow(ctx context.Context, srcChain string, l2Block uint64, includeProvisional bool) (AttestedRoot, bool, error) {
	resp, err := c.svc.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
		SrcChain:           srcChain,
		L2BlockNumber:      l2Block,
		IncludeProvisional: includeProvisional,
	})
	if err != nil {
		return AttestedRoot{}, false, fmt.Errorf("attestor AttestedRootAtOrBelow(%s, %d) failed: %w", srcChain, l2Block, err)
	}
	if !resp.GetFound() {
		return AttestedRoot{}, false, nil
	}
	entry, err := fromPB(resp.GetRoot())
	if err != nil {
		return AttestedRoot{}, false, err
	}
	return entry, true, nil
}

// WatchAttested streams frontier updates to fn until ctx is done or the
// stream fails. Each entry is the authoritative frontier at that moment: with
// includeProvisional it may repeat a height with a corrected root or cleared
// provisional flag, or retreat after a provisional revocation. fn returning an
// error stops the watch and propagates it.
func (c *Client) WatchAttested(ctx context.Context, srcChain string, includeProvisional bool, fn func(AttestedRoot) error) error {
	stream, err := c.svc.WatchAttested(ctx, &attestorpb.WatchAttestedRequest{
		SrcChain:           srcChain,
		IncludeProvisional: includeProvisional,
	})
	if err != nil {
		return fmt.Errorf("attestor WatchAttested(%s) failed: %w", srcChain, err)
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("attestor watch stream (%s) ended: %w", srcChain, err)
		}
		entry, err := fromPB(msg.GetRoot())
		if err != nil {
			return err
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
}
