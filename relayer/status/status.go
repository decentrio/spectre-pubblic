// Package status serves one read-only answer to "what is this relayer doing",
// and reads it back for the CLI.
//
// The relayer binds no port at all today, so an in-flight packet exists nowhere
// but the log: diagnosing a stalled leg means scrolling back through output, or
// querying the source chain by hand for a packet commitment -- which says the
// round trip is incomplete but not WHICH HALF is stuck. The relayer knows that
// and had no way to be asked (#307).
//
// Off by default, GET only, and one narrow write path behind its own setting.
// Everything it serves is already in the log; the difference is that an operator
// can ask for it at the moment they need it rather than at the moment it was
// printed.
//
// NO PROMETHEUS, AND THAT IS A DECISION (slice D, D2 -- the third checkbox of
// #307). This is where /metrics would go, so the reason it is not here belongs
// here too.
//
// The relayer exports no metrics. Its unit of work is a packet, and the question
// asked about a packet is "where is THIS one and how long has it been there" --
// an identity and an age, which is a snapshot rather than a counter. A counter
// answers "how many", which nobody has needed to ask of a process that relays
// one path.
//
// Expect the asymmetry with the attestor to look like an oversight and it is
// not: attestor/optimism DOES serve Prometheus (cmd/main.go, opstack/metrics.go)
// because it is a long-running observer whose health IS a rate -- games
// ingested, roots matched, how far the replica head has fallen behind. Rates are
// the right shape there and the wrong one here.
//
// What would change the decision: several relayer processes under one operator,
// where "which of the twelve is behind" is a question no per-process snapshot
// answers. That is when to revisit, and the counters #151 listed are the
// starting set. Until then, adding an exporter would mean maintaining a second
// description of the same state that nothing reads.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"relayer/relay"
	"relayer/services"
)

// Path is where the snapshot is served and fetched. One path, so there is no
// versioning surface to maintain on a debug endpoint.
const Path = "/status"

// ReactivatePath is the one write route, and it exists only when the operator
// asked for it. Keeping it off Path is the point: the snapshot must stay a read
// that nothing can be smuggled into, so the write is a separate route that is
// simply ABSENT -- 404, not 405 -- when controls are off.
const ReactivatePath = "/reactivate"

const (
	// requestTimeout bounds one served request. It exceeds relay's own probe
	// timeout so a slow probe still produces an answer rather than a dropped
	// connection.
	requestTimeout = 15 * time.Second
	// shutdownGrace bounds how long a shutdown waits for in-flight requests. A
	// status endpoint must never be the reason a relayer takes longer to stop.
	shutdownGrace = 2 * time.Second
)

// Snapshot is the whole answer: every relay path this process drives, and the
// queues behind them.
//
// The two halves are the split an operator cannot make today. A path's Waiting
// set is "not relayable yet, and here is the gate"; a queue's Pending set is
// "relayed, awaiting delivery or a timeout refund".
type Snapshot struct {
	Now    time.Time              `json:"now"`
	Paths  []relay.Status         `json:"paths"`
	Queues []services.QueueStatus `json:"queues"`
}

// SnapshotFunc produces the current answer. It is called once per request, so it
// must not block on the pipeline it describes.
type SnapshotFunc func(context.Context) Snapshot

// Controls is the optional write half. A dead-lettered packet means escrowed
// funds and a log line telling an operator to intervene, and the mechanism for
// intervening has to exist somewhere; nil (the default) means it does not exist
// here.
type Controls struct {
	// Reactivate returns one dead-lettered packet to the timeout scan set. The
	// selector names exactly one tracker; see DeadLetterSelector for why the
	// packet identity alone is not enough.
	Reactivate func(DeadLetterSelector) error
}

// ReactivateRequest is the body of a reactivation. The packet is named the way
// the status output prints it -- path, direction, client, sequence -- so an
// operator can copy the four fields off the row they just read.
type ReactivateRequest = DeadLetterSelector

// Handler serves the snapshot as JSON, and the one control route when ctl is
// non-nil. Path itself is GET only: it exposes packet identities and must never
// grow a way to change them by accident.
func Handler(snap SnapshotFunc, ctl *Controls) http.Handler {
	mux := http.NewServeMux()
	if ctl != nil && ctl.Reactivate != nil {
		mux.HandleFunc(ReactivatePath, reactivateHandler(ctl.Reactivate))
	}
	mux.HandleFunc(Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "status is read-only", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()

		body, err := json.MarshalIndent(snap(ctx), "", "  ")
		if err != nil {
			http.Error(w, "encode status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(body, '\n'))
	})
	return mux
}

// isJSONContentType reports whether the body is declared as JSON, ignoring
// parameters such as "; charset=utf-8". Anything else -- text/plain, a form
// encoding, multipart, or nothing at all -- is one of the three types a browser
// can send without asking permission first.
func isJSONContentType(header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// reactivateHandler is POST-only and validates before acting: a request that
// names no packet must be refused rather than turned into a lookup for the empty
// client at sequence zero.
//
// It also refuses anything a BROWSER could have sent. Reported by @neitdung, and
// the loopback guard does not cover it: refusing a non-loopback bind stops other
// hosts from reaching this route and says nothing about the browser already
// running on this one. A page on any site can POST here as a CORS "simple
// request" -- no preflight, so this server is never asked for permission. The
// same-origin policy hides the RESPONSE from that page; it does not stop the
// state change, and the state change is the entire risk.
//
// Two checks, and each one alone is enough to break the attack:
//
//   - An Origin header means a browser sent it. Operators use curl, which sends
//     none; a page cannot suppress it on a cross-origin POST.
//   - application/json is not a "simple" content type, so requiring it forces a
//     preflight that this server does not answer, and the browser never sends
//     the real request.
//
// Both are here because they fail differently: the first also covers a
// same-origin page served by some future handler, the second also covers a
// non-browser client that got the encoding wrong.
func reactivateHandler(reactivate func(DeadLetterSelector) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "reactivate takes POST", http.StatusMethodNotAllowed)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			log.Printf("WARN [status][ATTENTION] refused a %s request carrying Origin %q: "+
				"this control is for an operator on this host, not for a web page", ReactivatePath, origin)
			http.Error(w, "reactivate does not accept requests from a browser", http.StatusForbidden)
			return
		}
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "reactivate requires Content-Type: application/json", http.StatusUnsupportedMediaType)
			return
		}
		var req ReactivateRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := req.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := reactivate(req); err != nil {
			// The tracker's refusals are all "this packet is not where you think it
			// is" or "the change could not be made durable" -- both are the caller's
			// answer, not an internal detail to swallow.
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		log.Printf("[status] reactivated %s on an operator request", req)
		fmt.Fprintf(w, "reactivated %s\n", req)
	}
}

// Serve runs the endpoint until ctx is cancelled. An empty addr means the
// endpoint is off, which is the default: a relayer that was never asked to bind
// a port must not start listening because a status feature was added.
func Serve(ctx context.Context, addr string, snap SnapshotFunc, ctl *Controls) error {
	if addr == "" {
		return nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("status endpoint on %s: %w", addr, err)
	}
	if err := refuseRemoteControls(listener.Addr(), ctl); err != nil {
		_ = listener.Close()
		return err
	}
	warnIfReachable(listener.Addr())
	return serveOn(ctx, listener, snap, ctl)
}

// serveOn is Serve once the listener exists, so a test can hand it one it can
// kill. Splitting it is what makes "the listener died mid-run" reachable without
// a real network failure.
func serveOn(ctx context.Context, listener net.Listener, snap SnapshotFunc, ctl *Controls) error {
	srv := &http.Server{
		Handler:           Handler(snap, ctl),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// The waiter gets its own cancel so a serve failure can release it. Without
	// that it only ever fires on the PARENT context, and a listener that dies
	// after startup left Serve blocked on <-done until some unrelated shutdown
	// came along -- reported by @neitdung. The one error worth returning
	// immediately was the one that could not get out.
	waitCtx, stopWaiter := context.WithCancel(ctx)
	defer stopWaiter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-waitCtx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(stopCtx)
	}()

	log.Printf("[status] serving %s on http://%s%s", Path, listener.Addr(), Path)
	if ctl != nil && ctl.Reactivate != nil {
		log.Printf("WARN [status][ATTENTION] %s is enabled: this endpoint can change relayer state, not only report it", ReactivatePath)
	}
	err := srv.Serve(listener)
	// Release the waiter before joining it: on a serve failure the parent context
	// is still live, so joining first is the block this fixes. Shutdown on an
	// already-stopped server is a no-op, so the goroutine still ends cleanly.
	stopWaiter()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// refuseRemoteControls stops the WRITE route from being served on an address
// other people can reach.
//
// Reported by @DongLieu: warnIfReachable covers the snapshot, whose risk is
// disclosure, and a warning is the right weight for that. The control route is a
// different thing -- anyone who can connect can reset a dead letter's retry
// budget and requeue it -- and there is no authentication anywhere in this
// package to put in front of it. Serving an unauthenticated mutation to the
// network because a config file said 0.0.0.0 is not a decision an operator makes
// on purpose; it is one they inherit from an example.
//
// Refusing at startup rather than warning is the same posture as the rest of
// this binary: a wrong config stops the process here instead of surfacing at the
// first packet. The read-only endpoint still binds anywhere, so nothing an
// operator uses for diagnosis is taken away.
//
// The way to have controls on a remote host is to reach the loopback listener
// through something that authenticates -- an SSH tunnel, or a proxy that does --
// rather than to publish the route itself.
func refuseRemoteControls(addr net.Addr, ctl *Controls) error {
	if ctl == nil || ctl.Reactivate == nil {
		return nil // read-only: warnIfReachable is the right weight
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP.IsLoopback() {
		return nil
	}
	return fmt.Errorf(
		"status_allow_control is set and the endpoint is bound to %s, which is not loopback: "+
			"%s changes relayer state and nothing here authenticates it, so anyone who can reach "+
			"this address could reactivate dead-lettered packets. Bind status_address to a loopback "+
			"address (127.0.0.1:PORT) and reach it through a tunnel, or turn status_allow_control off",
		addr, ReactivatePath)
}

// warnIfReachable says so when the endpoint is not bound to loopback. It is a
// warning rather than a refusal because binding wider can be deliberate -- a
// container publishing a port has no loopback an operator can reach -- but it
// exposes packet identities and client ids to whoever can connect, and that
// should be a decision someone made rather than one they inherited from an
// example config.
func warnIfReachable(addr net.Addr) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP.IsLoopback() {
		return
	}
	log.Printf("WARN [status][ATTENTION] listening on %s, which is not loopback: the snapshot exposes packet sequences and client ids to anyone who can reach it", addr)
}

// Fetch reads a running relayer's snapshot. Used by the CLI, and the reason
// Snapshot is a plain struct rather than something rendered server-side.
func Fetch(ctx context.Context, addr string) (Snapshot, error) {
	var snap Snapshot

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+Path, nil)
	if err != nil {
		return snap, fmt.Errorf("build status request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return snap, fmt.Errorf("reach the relayer at %s: %w (is it running, and is status_address set?)", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return snap, fmt.Errorf("status endpoint returned %s: %s", resp.Status, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return snap, fmt.Errorf("decode status: %w", err)
	}
	return snap, nil
}

// Reactivate asks a running relayer to return one dead-lettered packet to the
// timeout scan set. The counterpart to Fetch, and the client half of the one
// write route.
func Reactivate(ctx context.Context, addr string, sel DeadLetterSelector) error {
	if err := sel.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(ReactivateRequest(sel))
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+ReactivatePath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build reactivate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach the relayer at %s: %w (is it running, and is status_address set?)", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// The route is absent rather than forbidden when controls are off, so this
		// is the case an operator hits most and it must not read as "no such packet".
		return fmt.Errorf("the relayer at %s serves no write route: set status_allow_control to enable it", addr)
	}
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("reactivate refused (%s): %s", resp.Status, strings.TrimSpace(string(text)))
	}
	return nil
}
