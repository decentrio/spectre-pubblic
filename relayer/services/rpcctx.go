package services

import (
	"context"
	"time"
)

// defaultFetchTimeout is the fallback when the caller's fetch timeout is unset
// (a zero-value Config in a test, or a config file that omits fetch_timeout).
// It matches the configured default in NewConfig.
const defaultFetchTimeout = 15 * time.Second

// fetchCtx bounds one short RPC call — a header read, a storage read, a status
// query — at the caller's configured fetch timeout.
//
// It takes the duration rather than a Config because #274 dissolved the
// process-wide context: the scoped dependencies carry a fetchTimeout directly,
// and passing that keeps this helper usable from all of them without
// reintroducing a shared config object.
//
// This is the per-call budget, distinct from the transport-level ceiling in
// client.DefaultRPCTimeout. The transport bound exists so nothing can hang
// forever; this one exists so a call we expect to be fast fails fast, instead of
// holding a relay loop for two minutes before anyone learns the node is sick.
//
// parent should be the loop's context wherever one is in scope, so shutdown
// cancels in-flight calls. Passing context.Background() is acceptable only where
// no context reaches the call site yet (the subscribers — see RLY-15 in #286);
// those calls are still bounded, just not cancellable.
func fetchCtx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultFetchTimeout
	}
	return context.WithTimeout(parent, timeout)
}
