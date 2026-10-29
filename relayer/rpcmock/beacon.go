package rpcmock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// BeaconNode is a REST stub for an Ethereum consensus-layer (beacon) API.
//
// The beacon side is plain HTTP+JSON, so unlike the two JSON-RPC stubs a test can
// hand it the exact document a real node returns and the relayer's own decoding
// runs against it unchanged. That is the value here: the response shapes are
// large and fork-sensitive, and decoding is where mistakes in them show up.
//
// A path with no registered response answers 404, so a test that reaches an
// endpoint it did not set up fails there rather than on a confusing decode error
// further along.
type BeaconNode struct {
	server *httptest.Server

	mu        sync.Mutex
	responses map[string]beaconReply
	requests  []string
}

type beaconReply struct {
	status int
	body   string
}

// NewBeaconNode starts a stub beacon API and stops it when the test finishes.
func NewBeaconNode(t testing.TB) *BeaconNode {
	t.Helper()
	n := &BeaconNode{responses: make(map[string]beaconReply)}
	n.server = httptest.NewServer(n)
	t.Cleanup(n.server.Close)
	return n
}

// URL is the base address to pass wherever production code takes a beaconAPIURL.
func (n *BeaconNode) URL() string { return n.server.URL }

// Respond registers the JSON body served for a path. Matching ignores the query
// string, so one registration covers every parameterisation of an endpoint; when
// the query itself is the subject, assert on it with LastQuery.
func (n *BeaconNode) Respond(path, body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.responses[path] = beaconReply{status: http.StatusOK, body: body}
}

// RespondJSON marshals v and registers it, for a test that would rather build a
// value than write out a JSON literal.
func (n *BeaconNode) RespondJSON(t testing.TB, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal stub response for %s: %v", path, err)
	}
	n.Respond(path, string(raw))
}

// Fail registers a non-200 reply — the shape a beacon node returns for a period
// or block it no longer retains, which is a routine condition, not an outage.
func (n *BeaconNode) Fail(path string, status int, body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.responses[path] = beaconReply{status: status, body: body}
}

// Requests returns every request line received, in order, as "path?query".
func (n *BeaconNode) Requests() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.requests...)
}

// LastQuery returns the parsed query of the most recent request to path, or nil
// if that path was never requested. It answers "what parameters did the relayer
// compute", which is a different question from "what did the stub reply".
func (n *BeaconNode) LastQuery(path string) url.Values {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.requests) - 1; i >= 0; i-- {
		reqPath, rawQuery, _ := strings.Cut(n.requests[i], "?")
		if reqPath != path {
			continue
		}
		values, err := url.ParseQuery(rawQuery)
		if err != nil {
			return nil
		}
		return values
	}
	return nil
}

func (n *BeaconNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	line := r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}

	n.mu.Lock()
	n.requests = append(n.requests, line)
	reply, ok := n.responses[r.URL.Path]
	n.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"code":404,"message":"no stub response registered for %s"}`, r.URL.Path)
		return
	}
	w.WriteHeader(reply.status)
	_, _ = w.Write([]byte(reply.body))
}
