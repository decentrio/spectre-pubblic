package rpcmock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	cmtjson "github.com/cometbft/cometbft/libs/json"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
)

// CosmosNode is a CometBFT JSON-RPC stub.
//
// Unlike EVMNode it models no chain semantics, and that is deliberate. The
// Cosmos replies the relayer consumes are proto-encoded ABCI payloads wrapped in
// CometBFT's own JSON codec; a stub that tried to synthesise them generically
// would encode this package's guess at the format, and every test built on it
// would agree with that guess. So the test supplies the reply for the one method
// it is about, and gets a real *rpchttp.HTTP pointed at it.
//
// What this removes is the boilerplate that made each package write its own:
// the server, the codec, the dial, the cleanup.
type CosmosNode struct {
	server *httptest.Server

	mu       sync.Mutex
	handlers map[string]func(params json.RawMessage) (any, error)
	calls    map[string]int
}

// NewCosmosNode starts a stub CometBFT node and stops it when the test finishes.
// A method with no registered handler answers "method not found", so a test that
// reaches an unexpected endpoint fails there rather than on a confusing decode
// error further along.
func NewCosmosNode(t testing.TB) *CosmosNode {
	t.Helper()
	n := &CosmosNode{
		handlers: make(map[string]func(json.RawMessage) (any, error)),
		calls:    make(map[string]int),
	}
	n.server = httptest.NewServer(n)
	t.Cleanup(n.server.Close)
	return n
}

// Handle registers the reply for one JSON-RPC method. The value returned is
// marshalled with CometBFT's codec, which is what the real node uses and what
// rpchttp expects on the way back.
func (n *CosmosNode) Handle(method string, h func(params json.RawMessage) (any, error)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[method] = h
}

// URL is the stub's address, for a caller that dials it itself — the shape a
// config field takes, where the value under test is the URL rather than the
// client built from it.
func (n *CosmosNode) URL() string { return n.server.URL }

// Client dials the stub and returns a real *rpchttp.HTTP. The caller wraps it in
// whatever endpoint struct it needs; this package deliberately imports nothing
// from the relayer, so that a package the endpoint type depends on can use it
// without an import cycle.
func (n *CosmosNode) Client(t testing.TB) *rpchttp.HTTP {
	t.Helper()
	client, err := rpchttp.New(n.server.URL, "/websocket")
	if err != nil {
		t.Fatalf("create CometBFT client against stub: %v", err)
	}
	return client
}

// Calls counts requests to a method by name.
func (n *CosmosNode) Calls(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[method]
}

type cosmosRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type cosmosResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (n *CosmosNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req cosmosRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	n.mu.Lock()
	n.calls[req.Method]++
	handler, ok := n.handlers[req.Method]
	n.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		writeCosmosError(w, req.ID, "method not found: "+req.Method)
		return
	}
	result, err := handler(req.Params)
	if err != nil {
		writeCosmosError(w, req.ID, err.Error())
		return
	}
	raw, err := cmtjson.Marshal(result)
	if err != nil {
		writeCosmosError(w, req.ID, "marshal result: "+err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(cosmosResponse{JSONRPC: "2.0", ID: req.ID, Result: raw})
}

func writeCosmosError(w http.ResponseWriter, id json.RawMessage, message string) {
	_ = json.NewEncoder(w).Encode(cosmosResponse{
		JSONRPC: "2.0", ID: id, Error: &rpcError{Code: -32601, Message: message},
	})
}
