package relay

import (
	"sync"
	"time"
)

// Worker names. They are constants because Run passes them to the worker group
// and the loops pass them to the heartbeat, and a typo in either would produce a
// loop that reports under a name nothing else uses -- visible only as a heartbeat
// that never arrives for a worker that is running fine.
// OWED: the subscriber does not beat. Its loop belongs to the adapter, so the
// module can only see it deliver a batch -- and "delivered recently" is traffic,
// not liveness. Recording it here would put exactly the confusion this type
// removes back into the output: a quiet chain would show a stale subscriber and
// read as wedged. A traffic-independent subscriber tick needs a hook on the
// adapter side, which is where #468 is already working.
const (
	workerSubscriber     = "subscriber"
	workerRefresh        = "refresh"
	workerTimeoutScan    = "timeout-scan"
	workerPeriodicUpdate = "periodic-update"
	workerPacketFlush    = "packet-flush"
)

// heartbeats records when each loop last completed a pass.
//
// The signal is "a tick finished", NOT "the chain moved". Those look identical
// on a busy chain and opposite on a quiet one: Arbitrum Nitro only produces
// blocks when there are transactions, so a head that has not advanced is a
// perfectly healthy idle chain, and a liveness rule built on head movement
// restarts a relayer that is doing nothing wrong -- losing whatever ack it was
// half way through.
//
// It exists because telling a WEDGED loop from an IDLE one currently requires
// `kill -QUIT` to dump goroutines, and that dump kills the process. An operator
// diagnosing a quiet direction should not have to end the run to find out
// whether it is stuck.
type heartbeats struct {
	mu   sync.Mutex
	last map[string]heartbeat
	now  func() time.Time // injectable for tests
}

// heartbeat is one loop's last completed pass.
type heartbeat struct {
	At time.Time
	// Note is what the pass did, when that is cheap to say and useful to read:
	// "advanced to 4210" beats a bare timestamp when the question is whether the
	// loop is turning or merely alive.
	Note string
}

func newHeartbeats() *heartbeats {
	return &heartbeats{last: make(map[string]heartbeat), now: time.Now}
}

// record marks a completed pass. A pass that FAILED still counts: the loop ran,
// which is what this measures. Whether the work succeeded is a separate question
// that the loop's own log and the rest of the status snapshot answer.
func (h *heartbeats) record(worker, note string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[worker] = heartbeat{At: h.now(), Note: note}
}

// snapshot lists the loops that have completed at least one pass, least recently
// beaten first -- the one most likely to be wedged reads first.
func (h *heartbeats) snapshot() []LoopHealth {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	now := h.now()
	out := make([]LoopHealth, 0, len(h.last))
	for worker, beat := range h.last {
		since := now.Sub(beat.At)
		out = append(out, LoopHealth{
			Worker:    worker,
			LastTick:  beat.At,
			Since:     since,
			SinceText: since.Round(time.Second).String(),
			Note:      beat.Note,
		})
	}
	h.mu.Unlock()

	sortLoopsByStalest(out)
	return out
}
