package status

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"relayer/relay"
	"relayer/services"
)

// Registry collects the snapshot sources of every engine in this process, so one
// endpoint answers for the whole relayer instead of one per goroutine.
//
// A process serves one relay path (A1), so in practice this holds that path's
// modules and its one services instance. It is a registry rather than a fixed
// pair because a path is more than one module -- Cosmos->ETH and its ETH->Cosmos
// return leg are two -- and because the engines that build them are three
// separate functions that should not have to know about each other.
//
// The zero value is usable, and a nil *Registry answers with an empty snapshot:
// status is off by default, and an engine must not need to know whether anyone
// is listening.
type Registry struct {
	mu     sync.Mutex
	paths  []func(context.Context) relay.Status
	queues []registeredQueues
	// reactivators are the write half, registered alongside the queues they act
	// on, and keyed by the SAME engine name the queues are stamped with.
	//
	// They used to be an unkeyed list offered to each engine in turn, first
	// success wins. Reported by @neitdung: that makes registration order the
	// tie-breaker for a state-changing operation, and an operator acting on the
	// second entry status printed could silently mutate the first.
	reactivators []registeredReactivator
}

type registeredQueues struct {
	engine string
	queues func(time.Time) []services.QueueStatus
}

type registeredReactivator struct {
	engine     string
	reactivate func(direction, sourceClient string, sequence uint64) error
}

// DeadLetterSelector names exactly one dead-lettered packet: which engine, which
// of that engine's trackers, and which packet in it.
//
// All four fields are required. A packet identity is (source client, sequence)
// and client ids are chain-local, so those two alone do not identify a row in a
// process that registers more than one tracker -- which every process does, since
// one services instance holds three.
type DeadLetterSelector struct {
	Path         string `json:"path"`
	Direction    string `json:"direction"`
	SourceClient string `json:"source_client"`
	Sequence     uint64 `json:"sequence"`
}

// Validate refuses a selector that does not name a packet, before anything is
// looked up and long before anything is written.
func (s DeadLetterSelector) Validate() error {
	var missing []string
	if s.Path == "" {
		missing = append(missing, "path")
	}
	if s.Direction == "" {
		missing = append(missing, "direction")
	}
	if s.SourceClient == "" {
		missing = append(missing, "source_client")
	}
	if s.Sequence == 0 {
		missing = append(missing, "a non-zero sequence")
	}
	if len(missing) > 0 {
		return fmt.Errorf("name the packet: %s, as printed by `relayer status`", strings.Join(missing, ", "))
	}
	return nil
}

func (s DeadLetterSelector) String() string {
	return fmt.Sprintf("%s/%s/%s/%d", s.Path, s.Direction, s.SourceClient, s.Sequence)
}

// AddPath registers one relay module's view of itself.
func (r *Registry) AddPath(path func(context.Context) relay.Status) {
	if r == nil || path == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
}

// AddQueues registers one services instance's queue view. Call it once per
// engine: the two modules of a bidirectional path share one services instance,
// and registering per module would report its queues twice.
func (r *Registry) AddQueues(engine string, queues func(time.Time) []services.QueueStatus) {
	if r == nil || queues == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queues = append(r.queues, registeredQueues{engine: engine, queues: queues})
}

// AddReactivator registers one services instance's dead-letter write path. Call
// it with AddQueues: what status lists is what a reactivation must be able to
// act on, so registering one without the other leaves an operator reading a
// packet id nothing will accept.
func (r *Registry) AddReactivator(engine string, reactivate func(direction, sourceClient string, sequence uint64) error) {
	if r == nil || reactivate == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactivators = append(r.reactivators, registeredReactivator{engine: engine, reactivate: reactivate})
}

// Reactivate returns one dead-lettered packet to the timeout scan set, in the
// engine and tracker the selector NAMES.
//
// It resolves before it mutates, and refuses both zero matches and more than
// one. The previous version offered the packet to every registered engine in
// registration order and returned on the first success -- reported by
// @neitdung: for a write, registration order is not a tie-breaker, and an
// operator acting on the second entry `relayer status` printed could silently
// give a fresh retry budget to the first while the packet they meant stayed
// dead-lettered.
//
// A zero-match error names the engines that ARE registered, because the likely
// mistake is a typo in a name the operator just read.
func (r *Registry) Reactivate(sel DeadLetterSelector) error {
	if err := sel.Validate(); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("no relay engine is registered to reactivate %s", sel)
	}
	r.mu.Lock()
	reactivators := append([]registeredReactivator(nil), r.reactivators...)
	r.mu.Unlock()

	var matches []registeredReactivator
	var known []string
	for _, candidate := range reactivators {
		known = append(known, candidate.engine)
		if candidate.engine == sel.Path {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].reactivate(sel.Direction, sel.SourceClient, sel.Sequence)
	case 0:
		if len(known) == 0 {
			return fmt.Errorf("no relay engine is registered to reactivate %s", sel)
		}
		return fmt.Errorf("no relay path named %q; this process serves %s", sel.Path, strings.Join(known, ", "))
	default:
		// Registration is the process's own doing, so this is a bug here rather
		// than operator error -- and it is the ambiguity the selector exists to
		// remove, so it must not be resolved by picking one.
		return fmt.Errorf("%d relay paths are registered as %q; the name must identify one", len(matches), sel.Path)
	}
}

// Snapshot asks every registered source, in registration order.
//
// The registered functions are called WITHOUT the registry lock: each one takes
// its own short locks and one of them makes a bounded RPC, and holding this lock
// across that would let a slow endpoint block a second concurrent request.
func (r *Registry) Snapshot(ctx context.Context) Snapshot {
	snap := Snapshot{Now: time.Now()}
	if r == nil {
		return snap
	}

	r.mu.Lock()
	paths := append([]func(context.Context) relay.Status(nil), r.paths...)
	queues := append([]registeredQueues(nil), r.queues...)
	r.mu.Unlock()

	for _, path := range paths {
		snap.Paths = append(snap.Paths, path(ctx))
	}
	for _, registered := range queues {
		reported := registered.queues(snap.Now)
		for i := range reported {
			// Stamped here, not in services: an engine does not know what the
			// process called it, and the name is what a reactivation must quote
			// back. What status prints is what the write route accepts.
			reported[i].Path = registered.engine
		}
		snap.Queues = append(snap.Queues, reported...)
	}
	return snap
}
