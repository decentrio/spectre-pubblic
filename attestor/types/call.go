package types

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// DefaultCallTimeout bounds one upstream call a feed runner makes: the OP loop's
// calls to the op-node and the dispute-game factory, and the Arbitrum loop's
// calls to L1 and Nitro. It is the catch-all for transports that carry no
// timeout of their own (WebSocket and IPC connections, where DialRPC applies
// none), and it sits above DefaultRPCTimeout so the HTTP transport timeout still
// fires first on a single hung HTTP request. A call here is a logical call, which
// can be several requests (resolving an assertion's block takes three Nitro
// requests), so the bound covers all of them.
const DefaultCallTimeout = 5 * time.Minute

// CallBound bounds the upstream calls a feed runner makes, one call at a time.
//
// It bounds the call and not the pass, on purpose. A pass is a loop over work
// whose size the runner does not control (every pending assertion, every log
// chunk), and some of that work is examined again from the start on each pass,
// because an assertion that is still pending stays in the list it came from. A
// deadline on the sum of the calls can then be spent on the same prefix every
// time, and starve everything after it: the entries behind the prefix are never
// confirmed or revoked, and the log scan that follows the reconciliation never
// runs. What a deadline has to do is free a call that never returns, and a call is
// what never returns.
type CallBound struct {
	// Timeout is the deadline of one call. Zero or less leaves a call bounded
	// only by the context it is made with.
	Timeout time.Duration
	// Beat, when set, runs when a call returns, whatever its outcome. A runner
	// uses it to tell a pass that is making progress from a loop that is parked.
	Beat func()
}

// Do runs one call under the bound. An error caused by the deadline names the
// call and the deadline, so the operator sees which bound fired rather than a
// bare "context deadline exceeded". The label is added only when the call's own
// context expired: an HTTP transport timeout also satisfies
// errors.Is(err, context.DeadlineExceeded), and must not be reported as this
// deadline. A cancelled parent context is returned as is, for the caller to treat
// as shutdown.
func (b CallBound) Do(ctx context.Context, name string, call func(context.Context) error) error {
	if b.Timeout <= 0 {
		err := call(ctx)
		b.beat()
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	err := call(callCtx)
	b.beat()
	if err != nil && ctx.Err() == nil &&
		errors.Is(callCtx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s exceeded its %s deadline: %w", name, b.Timeout, err)
	}
	return err
}

func (b CallBound) beat() {
	if b.Beat != nil {
		b.Beat()
	}
}

// Call is Do for a call that returns one value.
func Call[T any](ctx context.Context, bound CallBound, name string, call func(context.Context) (T, error)) (T, error) {
	var out T
	err := bound.Do(ctx, name, func(ctx context.Context) error {
		var err error
		out, err = call(ctx)
		return err
	})
	return out, err
}

// FeedStaleAfter is how long a feed runner may go without a beat before it is
// no longer considered alive: one call bounded by the call timeout, then one
// poll tick, plus one more tick of slack. A pass can run far longer than that,
// because the runner beats as each of its calls returns.
func FeedStaleAfter(callTimeout, pollInterval time.Duration) time.Duration {
	return callTimeout + 2*pollInterval
}

// Heartbeat records when a feed runner last made progress: its loop came back
// around, or one of its upstream calls returned. It tells a runner that is
// failing (it still beats, and the failure shows in its log, its failure counter
// and its backoff) from one that is parked on something that never returns (it
// stops beating, and nothing else reports it).
//
// The zero value has never beaten and is not alive. It is safe for concurrent
// use: the runner goroutine beats, readiness probes read.
type Heartbeat struct {
	last atomic.Int64 // UnixNano of the last Beat; 0 until the first one
}

// Beat records that the runner is making progress at now.
func (h *Heartbeat) Beat(now time.Time) {
	h.last.Store(now.UnixNano())
}

// Alive reports whether the last beat is no more than maxAge before now.
func (h *Heartbeat) Alive(now time.Time, maxAge time.Duration) bool {
	last := h.last.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) <= maxAge
}
