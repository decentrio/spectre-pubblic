package core

import "context"

// Runner owns a plugin background loop. Transport must never call Run or use
// it to trigger ingest, refresh, or rechecks.
type Runner interface {
	Run(context.Context) error
}

// FeedReader exposes only durable, read-only feed snapshots.
type FeedReader interface {
	AttestedUpTo(context.Context, AttestationPolicy) (AttestedRoot, bool, error)
	AttestedRootAtOrBelow(context.Context, uint64, AttestationPolicy) (AttestedRoot, bool, error)
}

// BlockVerifier binds a relayer candidate to the plugin's independent L2
// replica and signs the canonical identity only when it matches.
type BlockVerifier interface {
	VerifyStateRoot(context.Context, BlockIdentityRequest) (SignedBlockIdentityVerdict, error)
}

// StatusReader reports plugin-local readiness and diagnostics.
type StatusReader interface {
	Status(context.Context) (FeedStatus, error)
}

// FrontierWatcher is optional. Its absence is the sole reason the shared gRPC
// adapter returns Unimplemented for WatchAttested.
type FrontierWatcher interface {
	WatchFrontier(context.Context, WatchRequest) (<-chan FrontierUpdate, error)
}

// Ports is the immutable route target installed by the composition root. A
// plugin may implement several ports with one object, but the adapter only
// depends on these interfaces and never on a concrete rollup package.
type Ports struct {
	Feed     FeedReader
	Verifier BlockVerifier
	Status   StatusReader
	Watcher  FrontierWatcher
}
