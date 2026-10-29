package relay

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"relayer/chain"
)

// waitListLimit caps how many packets the waiting log names individually. A
// backlog of hundreds must not produce a log line of hundreds of entries; the
// oldest few plus a count answer the operator's question ("is my packet in
// there, and how long has it been stuck?") without the noise.
const waitListLimit = 5

// waitEntryTTL bounds the tracker's memory independently of which handleBatch
// exit path ran. Every still-waiting packet is re-observed at most
// waitingBackoffMax (15s) apart, so an entry untouched for an hour belongs to a
// packet that left the queue through a path that did not clear it — drop it
// rather than leaking one entry per such packet for the process's lifetime.
const waitEntryTTL = time.Hour

// waitKey identifies a waiting packet within one relay path. Sequence alone is
// not enough: a packet's recv and its returning ack share a sequence and can
// both be observed on the same path, and they wait for different reasons.
//
// The client id is deliberately absent: one Module drives one source→destination
// path, so it is constant here. It would also not help the case it looks like it
// should — create-clients-eth repoints an existing router client via
// migrateClient, so sequences restart under the SAME client id. That case is
// handled by waitEntry.height instead.
type waitKey struct {
	typ chain.EventType
	seq uint64
}

type waitEntry struct {
	since    time.Time // first flush this packet was observed waiting
	lastSeen time.Time // most recent flush, for TTL purging only
	// height is the source height the packet was emitted at. Sequences restart —
	// create-clients-eth repoints an existing client with migrateClient, so the
	// source begins again at sequence 1 under the same client id. A later packet
	// reusing a sequence is a different packet, emitted at a different height, and
	// must start its own clock rather than inherit the retired one's age.
	//
	// This lives in the entry, not the key, because clearPacket identifies packets
	// by chain.RelayPacket, whose Height is the PROOF height (module.go builds it
	// from proofHeight, shared across the batch) — not the emission height. Keying
	// on it would make every clear miss.
	height uint64
	// proofSince is when this packet's FIRST proof attempt failed, and it is
	// deliberately not `since`.
	//
	// The two clocks answer different questions. `since` is the total wait, which
	// is what the waiting log wants: a packet parked at the finality gate has been
	// waiting, whatever the reason. But a packet can sit there for half an hour
	// before a proof is ever attempted, and reusing that age for the proof ladder
	// makes the very first failure claim it "has failed to prove for 30m" and jump
	// straight to STUCK -- burning four of the five thresholds before a single
	// proof ran, so the next line is an hour away. That silences exactly the early
	// window the ladder exists to create.
	//
	// Zero until the first proof failure. Reset with the entry, so a relayed or
	// dropped packet -- or a reused sequence -- starts a fresh proof clock.
	proofSince time.Time
	// reported is the highest proof-failure threshold already logged for this
	// packet (see proofFailureThreshold). It lives here so it shares the entry's
	// lifecycle: cleared when the packet relays or is dropped, purged with the
	// entry on TTL. A separate map would leak entries the clear paths do not know
	// to visit.
	reported int
}

// waitTracker records how long each packet has been parked waiting, so the
// waiting log can report an age rather than only a count.
//
// Why an age matters: without one, a packet waiting normally for finality and a
// packet stuck forever produce byte-identical log lines. The age is the only
// signal that separates "the L1 has not finalized yet" from "this ack is never
// coming back", which is precisely the question an operator asks when a return
// leg goes quiet.
//
// Ownership: entries are written only from the source's subscribe goroutine (the
// single caller of handleBatch). The mutex is held anyway so a future status
// command can read the set from another goroutine without a data race.
type waitTracker struct {
	mu      sync.Mutex
	entries map[waitKey]waitEntry
	now     func() time.Time // injectable for tests
}

func newWaitTracker() *waitTracker {
	return &waitTracker{entries: make(map[waitKey]waitEntry), now: time.Now}
}

// observe records that e is still waiting and returns how long it has been. The
// first observation reports 0, not a fabricated age — the tracker cannot know
// how long the packet waited before the process started, and reporting a guess
// would be worse than reporting nothing.
func (w *waitTracker) observe(e chain.Event) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	key := waitKey{typ: e.Type, seq: e.Sequence}
	entry, ok := w.entries[key]
	if !ok || entry.height != e.Height {
		// Either the first sighting, or a different packet that reused the sequence.
		entry = waitEntry{since: now, height: e.Height}
	}
	entry.lastSeen = now
	w.entries[key] = entry
	return now.Sub(entry.since)
}

// clearPacket forgets a packet that is no longer waiting — it was relayed, or
// dropped as permanently dead. Clearing something never observed is a no-op, so
// callers need not check first.
func (w *waitTracker) clearPacket(p chain.RelayPacket) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.entries, waitKey{typ: p.Type, seq: p.Sequence})
}

// clear is clearPacket for an event that never reached the RelayPacket stage
// (dropped at proof time).
func (w *waitTracker) clear(e chain.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.entries, waitKey{typ: e.Type, seq: e.Sequence})
}

// purgeStale drops entries no flush has touched for waitEntryTTL. See the const
// comment for why this backstop exists on top of clear.
func (w *waitTracker) purgeStale() {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := w.now().Add(-waitEntryTTL)
	for key, entry := range w.entries {
		if entry.lastSeen.Before(cutoff) {
			delete(w.entries, key)
		}
	}
}

// waiting pairs an event with how long it has been parked, for the log summary.
type waiting struct {
	event chain.Event
	age   time.Duration
}

// describeWaiting renders the waiting set as "ack seq=1 h=21 for 4m12s, ...",
// longest-waiting first and capped at waitListLimit, so the entry most likely to
// be stuck is always the one shown. Returns "" for an empty set.
func describeWaiting(items []waiting) string {
	if len(items) == 0 {
		return ""
	}
	sorted := make([]waiting, len(items))
	copy(sorted, items)
	// Insertion sort by descending age: the list is at most a batch's worth of
	// packets, and this keeps equal ages in observation order.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].age > sorted[j-1].age; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	shown := sorted
	if len(shown) > waitListLimit {
		shown = shown[:waitListLimit]
	}
	parts := make([]string, 0, len(shown))
	for _, it := range shown {
		parts = append(parts, fmt.Sprintf("%s seq=%d h=%d for %s",
			it.event.Type, it.event.Sequence, it.event.Height, it.age.Round(time.Second)))
	}
	out := strings.Join(parts, ", ")
	if len(sorted) > len(shown) {
		out += fmt.Sprintf(", +%d more", len(sorted)-len(shown))
	}
	return out
}

// proofFailureThresholds is the escalation ladder for a packet whose proof keeps
// failing. Every failure re-queues, so without a ladder the loop logs the same
// line every flush -- 296 byte-identical lines in the run that prompted this,
// with nothing to separate "waiting for finality" from "this will never work".
//
// The steps widen because the two questions change with age. In the first minute
// an operator wants to see the error at all; after a quarter of an hour they want
// to know it is still the same one.
var proofFailureThresholds = []time.Duration{
	0,
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

// proofFailureStuckAfter is where the wording changes. Past it the line stops
// describing a retry and says the packet is stuck, because by then it is: every
// transient cause this path has (an RPC blip, an unfinalized source height, a
// client that has not caught up) resolves in far less.
const proofFailureStuckAfter = 15 * time.Minute

// proofFailureThreshold returns how many reporting thresholds age has crossed.
// Past the last one it keeps counting hourly, so a packet stuck overnight leaves
// a trail rather than one line and then silence.
func proofFailureThreshold(age time.Duration) int {
	crossed := 0
	for _, t := range proofFailureThresholds {
		if age >= t {
			crossed++
		}
	}
	if last := proofFailureThresholds[len(proofFailureThresholds)-1]; age >= last {
		crossed += int((age - last) / time.Hour)
	}
	return crossed
}

// observeProofFailure records that e's proof failed again and reports whether
// this failure is worth a log line -- the first one always is, and afterwards
// only when the age crosses a new threshold.
//
// The age returned is measured from the first PROOF failure (waitEntry.proofSince),
// not from the first time the packet was seen waiting, so the caller logs how long
// proving has been failing rather than how long the packet has existed.
func (w *waitTracker) observeProofFailure(e chain.Event) (time.Duration, bool) {
	// Keep the entry alive for the TTL sweep, and let it handle a reused sequence
	// (which resets proofSince along with the rest of the entry). Its return value
	// is the total wait, which is not the clock this ladder runs on.
	w.observe(e)

	w.mu.Lock()
	defer w.mu.Unlock()
	key := waitKey{typ: e.Type, seq: e.Sequence}
	entry, ok := w.entries[key]
	if !ok {
		return 0, true // observe() just wrote it; a missing entry means a racing clear
	}
	now := w.now()
	if entry.proofSince.IsZero() {
		entry.proofSince = now
	}
	age := now.Sub(entry.proofSince)
	report := false
	if threshold := proofFailureThreshold(age); threshold > entry.reported {
		entry.reported = threshold
		report = true
	}
	w.entries[key] = entry
	return age, report
}
