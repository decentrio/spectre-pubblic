package main

import "sync"

// engineErrors collects the failure of the one relay path this process serves.
//
// It replaces a buffered channel, a two-arm select and a nested select that
// preferred a buffered error over a coincident done signal. That machinery was
// sized for many unrelated relay loops in one process, which is the arrangement
// A1 removed: a process now serves one path, so an engine failure is THE
// failure and there is nothing left to keep serving.
//
// A Cosmos<->L2 path still runs two engines -- the forward leg and the return
// leg -- so the collection is not gone, only the layer. First error wins, and
// recording one cancels the sibling: the return leg has nothing to do once the
// forward leg is gone, and cancelling keeps the teardown as prompt as the
// channel used to make it.
type engineErrors struct {
	once   sync.Once
	cancel func()

	mu  sync.Mutex
	err error
}

func newEngineErrors(cancel func()) *engineErrors {
	return &engineErrors{cancel: cancel}
}

// record keeps the first error and stops the rest of the path. Later errors are
// dropped on purpose: they are overwhelmingly the sibling reporting the
// cancellation this call caused, and reporting that instead of the cause would
// name the symptom.
func (e *engineErrors) record(err error) {
	if err == nil {
		return
	}
	e.once.Do(func() {
		e.mu.Lock()
		e.err = err
		e.mu.Unlock()
		if e.cancel != nil {
			e.cancel()
		}
	})
}

// first returns the failure, or nil for a clean exit. Safe to call once every
// engine has returned; the mutex is held anyway so a future reader elsewhere
// cannot race.
func (e *engineErrors) first() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}
