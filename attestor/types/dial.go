package types

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"attestor/types/redact"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// DefaultRPCTimeout is the transport-level ceiling for one HTTP JSON-RPC
// request. It is a liveness backstop: a reachable node can accept a request
// and never answer, and go-ethereum does not apply a client timeout itself.
const DefaultRPCTimeout = 2 * time.Minute

// DialRPC connects to a JSON-RPC endpoint. HTTP(S) endpoints receive a
// transport timeout; WebSocket endpoints do not, because their connections are
// intentionally long-lived and do not use http.Client. A call on such a
// connection is bounded only by the context it is made with.
//
// An error from here can quote rawURL, and an RPC provider's API key travels in
// the URL, so callers redact it (redact.Error) before it is logged or returned.
func DialRPC(ctx context.Context, rawURL string, timeout time.Duration) (*rpc.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", redact.Error(err))
	}

	var opts []rpc.ClientOption
	if u.Scheme == "http" || u.Scheme == "https" {
		opts = append(opts, rpc.WithHTTPClient(&http.Client{Timeout: timeout}))
	}
	return rpc.DialOptions(ctx, rawURL, opts...)
}

// DialEthRPC connects to an Ethereum-family JSON-RPC endpoint with DialRPC's
// transport timeout. Its errors name the endpoint by scheme and host only.
func DialEthRPC(ctx context.Context, rawURL string, timeout time.Duration) (*ethclient.Client, error) {
	rpcClient, err := DialRPC(ctx, rawURL, timeout)
	if err != nil {
		return nil, fmt.Errorf("eth rpc %s: %w", redact.URL(rawURL), redact.Error(err))
	}
	return ethclient.NewClient(rpcClient), nil
}
