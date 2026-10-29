package client

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	cmtlog "github.com/cometbft/cometbft/libs/log"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	jsonrpcclient "github.com/cometbft/cometbft/rpc/jsonrpc/client"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"relayer/logbridge"
)

// DefaultRPCTimeout is the transport-level ceiling on a single JSON-RPC round
// trip. It is a LIVENESS BACKSTOP, not a latency target: neither library we
// dial supplies one of its own, so without it a node that accepts the TCP
// connection and then stops answering blocks the caller forever.
//
// That is not hypothetical here. The relay loops drive one direction on a
// single goroutine, so one unanswered request wedges that whole direction with
// no error, no retry and nothing in the log — the first row of the failure
// table in the local-devnet runbook. See GetEthMembershipProof for the same
// hazard caught at a call site.
//
// It is deliberately much larger than Config.FetchTimeout (15s). FetchTimeout
// bounds calls we expect to be fast; this bounds every call, including ones
// that are legitimately slow — a wide eth_getLogs scan, an eth_getProof against
// an archive node, a paginated TxSearch over a long recovery range. Set it low
// enough to be a useful SLO and it starts cancelling healthy work, which is a
// worse failure than the one it prevents. Two minutes is far beyond any healthy
// response and far below "forever".
const DefaultRPCTimeout = 2 * time.Minute

// DialCosmosRPC connects to a CometBFT node with a bounded HTTP client.
//
// The timeout applies ONLY to the HTTP JSON-RPC calls (Status, TxSearch,
// ABCIQuery, Commit, Validators, Block). It does not reach the WebSocket
// subscription: NewWithClient passes the http.Client to the JSON-RPC caller and
// builds WSEvents separately via newWSEvents, which dials its own long-lived
// connection (cometbft rpc/client/http/http.go:132-152). A timeout that killed
// the subscription would silently undo the live-path fix in #377, so this
// separation is load-bearing rather than incidental.
func DialCosmosRPC(remote, wsEndpoint string, timeout time.Duration) (*rpchttp.HTTP, error) {
	httpClient, err := jsonrpcclient.DefaultHTTPClient(remote)
	if err != nil {
		return nil, fmt.Errorf("cosmos rpc %s: build http client: %w", remote, err)
	}
	httpClient.Timeout = timeout
	c, err := rpchttp.NewWithClient(remote, wsEndpoint, httpClient)
	if err != nil {
		return nil, fmt.Errorf("cosmos rpc %s: %w", remote, err)
	}
	c.SetLogger(cometLogger{})
	return c, nil
}

// cometLogger routes CometBFT's structured logger into the standard logger the
// rest of the relayer writes to.
//
// It is not cosmetic. The two failures that decide whether the live
// subscription is alive are reported ONLY through this interface, and the
// client's logger is a nop until SetLogger is called — which nothing in this
// repository did:
//
//	rpc/client/http/http.go, WSEvents.redoSubscriptionsAfter
//	    "Failed to resubscribe"                                     <- resubscribe gave up
//	rpc/client/http/http.go, WSEvents.eventListener
//	    "wanted to publish ResultEvent, but out channel is full"    <- an event was DROPPED
//
// Both are silent losses: neither closes the Go channel the subscriber selects
// on, so without this adapter a subscription can stop delivering and leave no
// trace in any log. See cosmosLiveEventBuffer in relayer/subscriber/subscriber.go for the
// drop, and the resubscribe path for the other.
//
// Scope note: rpchttp builds its inner WSClient before SetLogger can reach it
// (newWSEvents calls w.ws.SetLogger(w.Logger) with the nop), so this covers the
// WSEvents layer — where both lines above live — and not the transport layer
// below it.
type cometLogger struct{ fields []interface{} }

var _ cmtlog.Logger = cometLogger{}

// Debug is dropped: CometBFT emits per-message debug output on this path, which
// would bury the relayer's own log without adding a decision an operator makes.
func (cometLogger) Debug(string, ...interface{}) {}

func (l cometLogger) Info(msg string, keyvals ...interface{}) { l.print("", msg, keyvals) }

// Error is the whole reason this adapter exists, so it must arrive as an error:
// the two lines in the comment above are an abandoned resubscribe and a dropped
// event. It used to carry the word in the middle of the text, which a log viewer
// does not read as a level; the token has to lead the message for logbridge.
func (l cometLogger) Error(msg string, keyvals ...interface{}) {
	l.print(logbridge.Error, msg, keyvals)
}

func (l cometLogger) With(keyvals ...interface{}) cmtlog.Logger {
	// Copy rather than append in place: the receiver's backing array may be
	// shared with another logger derived from the same parent.
	fields := make([]interface{}, 0, len(l.fields)+len(keyvals))
	fields = append(fields, l.fields...)
	fields = append(fields, keyvals...)
	return cometLogger{fields: fields}
}

// print writes one line. token is a logbridge level token, or empty for info.
func (l cometLogger) print(token, msg string, keyvals []interface{}) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s[cometbft] %s", token, msg)
	all := make([]interface{}, 0, len(l.fields)+len(keyvals))
	all = append(all, l.fields...)
	all = append(all, keyvals...)
	for i := 0; i+1 < len(all); i += 2 {
		fmt.Fprintf(&b, " %v=%v", all[i], all[i+1])
	}
	if len(all)%2 == 1 {
		fmt.Fprintf(&b, " %v=(MISSING)", all[len(all)-1])
	}
	log.Print(b.String())
}

// DialEthRPC connects to an Ethereum-family JSON-RPC endpoint, applying the
// timeout to HTTP(S) endpoints only.
//
// WebSocket endpoints are returned unbounded on purpose. A ws:// connection is
// long-lived by design — an event subscription is meant to stay open for the
// process's lifetime — so an http.Client deadline is both inapplicable (geth
// routes ws/wss through newClientTransportWS, which never reads cfg.httpClient
// — rpc/client.go:206-214) and wrong in intent. Liveness on the WS path is
// already covered: the subscription surfaces failures on sub.Err(), which
// SubscribeEth turns into a reconnect.
func DialEthRPC(ctx context.Context, rawurl string, timeout time.Duration) (*ethclient.Client, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("eth rpc %s: parse url: %w", rawurl, err)
	}

	var opts []rpc.ClientOption
	if u.Scheme == "http" || u.Scheme == "https" {
		opts = append(opts, rpc.WithHTTPClient(&http.Client{Timeout: timeout}))
	}

	rpcClient, err := rpc.DialOptions(ctx, rawurl, opts...)
	if err != nil {
		return nil, fmt.Errorf("eth rpc %s: %w", rawurl, err)
	}
	return ethclient.NewClient(rpcClient), nil
}
