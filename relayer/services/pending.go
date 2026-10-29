package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

type packetKey struct {
	sourceClient string
	sequence     uint64
}

type pendingPacketInfo struct {
	Packet      channeltypesv2.Packet
	ObservedAt  time.Time
	BlockNumber uint64

	// identity is carried by GetDue snapshots. Timeout scanners use it when
	// applying an outcome so work started for an old packet cannot mutate a new
	// packet that reused the same source-client/sequence key in the meantime.
	identity packetIdentity

	// TimeoutAttempts counts how many times a timeout submission for this packet
	// has failed. NotBefore holds it out of the next scans for a growing delay.
	// Without these a deterministically-failing timeout is retried every 30s
	// forever, and every cycle costs a Groth16 proof and a transaction (RLY-06).
	TimeoutAttempts int
	NotBefore       time.Time

	// Deferrals counts back-offs taken for reasons NOT attributable to this
	// packet — a shared prerequisite such as the light-client update that every
	// timeout in the scan depends on. They shape the backoff exactly like
	// TimeoutAttempts but never approach maxTimeoutAttempts, because a packet
	// must not be dead-lettered for an outage it had no part in. See
	// DeferTimeoutRetry.
	Deferrals int

	// DeadLetteredAt is when the packet stopped being retried. Zero until then.
	// The age matters as much as the count: a count says a timeout gave up, the
	// age says how long nobody has looked (RLY-02).
	DeadLetteredAt time.Time
}

// settledPacketInfo is a durable terminal tombstone.  Unlike a dead-letter,
// this says the packet has already completed its lifecycle on-chain and must
// never be tracked or relayed again when a source replay offers it back.
//
// It is keyed by the full packet identity rather than source-client/sequence:
// client migration is allowed to reuse a sequence, and a terminal event for
// the old packet must never suppress the replacement.
type settledPacketInfo struct {
	Packet    channeltypesv2.Packet
	SettledAt time.Time
	identity  packetIdentity
}

// packetIdentity states exactly which packet value owns retry state. The
// source-client/sequence pair is only a lookup key: a re-created client may
// legitimately reuse it. Payloads are digested from their value fields so
// protobuf representation details such as nil versus an empty slice do not
// turn a replay into a new packet and reset its retry budget.
type packetIdentity struct {
	sourceClient      string
	destinationClient string
	sequence          uint64
	timeoutTimestamp  uint64
	payloadsDigest    [sha256.Size]byte
}

func identityOf(info pendingPacketInfo) packetIdentity {
	if info.identity == (packetIdentity{}) {
		return identifyPacket(info.Packet)
	}
	return info.identity
}

func identifyPacket(packet channeltypesv2.Packet) packetIdentity {
	h := sha256.New()
	writeHashUint64(h, uint64(len(packet.Payloads)))
	for _, payload := range packet.Payloads {
		writeHashBytes(h, []byte(payload.SourcePort))
		writeHashBytes(h, []byte(payload.DestinationPort))
		writeHashBytes(h, []byte(payload.Version))
		writeHashBytes(h, []byte(payload.Encoding))
		writeHashBytes(h, payload.Value)
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return packetIdentity{
		sourceClient:      packet.SourceClient,
		destinationClient: packet.DestinationClient,
		sequence:          packet.Sequence,
		timeoutTimestamp:  packet.TimeoutTimestamp,
		payloadsDigest:    digest,
	}
}

func writeHashBytes(h hash.Hash, value []byte) {
	writeHashUint64(h, uint64(len(value)))
	_, _ = h.Write(value) // hash.Hash.Write never returns an error
}

func writeHashUint64(h hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = h.Write(encoded[:]) // hash.Hash.Write never returns an error
}

// Timeout-submission retry policy. A timeout that keeps failing is almost always
// deterministic (a malformed proof, a client that cannot cover the height), so
// retrying it on the raw 30s scan cadence just burns proofs. The backoff grows to
// spread that cost out, and the cap stops it entirely so one dead packet cannot
// occupy the scanner forever.
const (
	timeoutRetryBackoffBase = 1 * time.Minute
	timeoutRetryBackoffMax  = 30 * time.Minute
	maxTimeoutAttempts      = 8

	// deadLetterRetention bounds how long a deterministic timeout failure may
	// suppress retries. A tombstone must outlive a reasonable operator response,
	// but it must not make a re-created client that reuses sequence numbers
	// permanently invisible to timeout recovery.
	deadLetterRetention = 30 * 24 * time.Hour

	// settledPacketRetention bounds terminal tombstones. It is deliberately as
	// long as dead-letter retention: a source restart can replay a substantial
	// recovery window, while an unbounded terminal set would grow for the life of
	// a busy relayer. Full packet identity makes retaining an old tombstone safe
	// across a client migration that reuses a sequence.
	settledPacketRetention = 30 * 24 * time.Hour

	// deferralAlertThreshold is how many un-charged back-offs a packet may take
	// before the scanner says so out loud, and how often it repeats after that.
	//
	// Deferrals never dead-letter, by design: they are outages, and abandoning an
	// escrowed packet because an RPC was down is the failure this retry bound
	// exists to prevent. But "never abandoned" must not mean "never mentioned" —
	// a packet deferring at the 30-minute cap is silently un-timed-out and its
	// funds stay escrowed just the same. Twelve deferrals is past the point where
	// the backoff has saturated, so it is a real stall rather than a blip.
	deferralAlertThreshold = 12
)

// nextTimeoutBackoff doubles per attempt up to the cap: 1m, 2m, 4m … 30m.
func nextTimeoutBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := timeoutRetryBackoffBase << (attempt - 1)
	if d > timeoutRetryBackoffMax || d <= 0 { // <=0 guards shift overflow
		d = timeoutRetryBackoffMax
	}
	return d
}

type PendingPacketTracker struct {
	mtx     sync.Mutex
	packets map[packetKey]pendingPacketInfo

	// statePath is empty for short-lived/test trackers. Production relay
	// trackers set it and synchronously replace the snapshot after every
	// mutation, so restart recovery cannot reset retry budgets or lose
	// tombstones.
	statePath  string
	writeState func(string, []byte) error

	// persistenceErr trips a fail-closed timeout circuit breaker after a
	// state mutation could not be made durable. Leaving the packet due after a
	// rolled-back backoff would repeat expensive timeout work every scan. The
	// scanner probes the snapshot writer before resuming so this remains set
	// until the configured state location is writable again.
	persistenceErr error

	// deadLettered holds packets whose timeout submission exceeded
	// maxTimeoutAttempts. They are kept, not dropped: a packet here has funds
	// escrowed and needs an operator, so it must stay visible.
	deadLettered []pendingPacketInfo

	// deadLetteredByKey is the authoritative tombstone set. A relay source can
	// retry or replay an event after its packet has left packets, so checking only
	// the active map would give an exhausted timeout a fresh retry budget. Keep
	// the tombstone separate from the inspectable queue. Add compares the packet
	// before deduplicating, so a client re-creation that reuses a sequence does
	// not let an old tombstone shadow a different packet forever.
	deadLetteredByKey map[packetKey]pendingPacketInfo

	// settled records terminal source events independently of active packets.
	// A terminal event can arrive while its send is in a handed-off batch, before
	// Add has made an active record. Keeping the terminal fact separately is what
	// lets that later Add refuse the already-settled packet.
	settled map[packetIdentity]settledPacketInfo
}

func NewPendingPacketTracker() *PendingPacketTracker {
	return &PendingPacketTracker{
		packets:           make(map[packetKey]pendingPacketInfo),
		deadLetteredByKey: make(map[packetKey]pendingPacketInfo),
		settled:           make(map[packetIdentity]settledPacketInfo),
	}
}

const pendingPacketStateVersion = 2

type pendingPacketState struct {
	Version      int                 `json:"version"`
	Packets      []pendingPacketInfo `json:"packets"`
	DeadLettered []pendingPacketInfo `json:"dead_lettered"`
	Settled      []settledPacketInfo `json:"settled,omitempty"`
}

// NewPersistentPendingPacketTracker restores the complete active and tombstone
// state before returning. A corrupt/unreadable snapshot is a startup error: an
// empty fallback would silently grant exhausted packets a fresh retry budget.
func NewPersistentPendingPacketTracker(statePath string) (*PendingPacketTracker, error) {
	t := NewPendingPacketTracker()
	t.statePath = statePath
	t.writeState = replaceStateFile
	if err := t.load(); err != nil {
		return nil, err
	}
	// Write once at construction even when no snapshot existed. This verifies
	// the configured location is writable before any subscription or recovery
	// loop can start and create state that only exists in memory.
	if err := t.persistLocked(); err != nil {
		return nil, fmt.Errorf("initialize pending packet state %s: %w", statePath, err)
	}
	return t, nil
}

func (t *PendingPacketTracker) load() error {
	if t.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(t.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pending packet state %s: %w", t.statePath, err)
	}
	var state pendingPacketState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode pending packet state %s: %w", t.statePath, err)
	}
	if state.Version != 1 && state.Version != pendingPacketStateVersion {
		return fmt.Errorf("decode pending packet state %s: unsupported version %d", t.statePath, state.Version)
	}
	for _, info := range state.Packets {
		info.identity = identifyPacket(info.Packet)
		key := packetKey{sourceClient: info.Packet.SourceClient, sequence: info.Packet.Sequence}
		if _, duplicate := t.packets[key]; duplicate {
			return fmt.Errorf("decode pending packet state %s: duplicate active key %s/%d", t.statePath, key.sourceClient, key.sequence)
		}
		t.packets[key] = info
	}
	for _, info := range state.DeadLettered {
		info.identity = identifyPacket(info.Packet)
		key := packetKey{sourceClient: info.Packet.SourceClient, sequence: info.Packet.Sequence}
		if _, duplicate := t.deadLetteredByKey[key]; duplicate {
			return fmt.Errorf("decode pending packet state %s: duplicate tombstone key %s/%d", t.statePath, key.sourceClient, key.sequence)
		}
		if _, active := t.packets[key]; active {
			return fmt.Errorf("decode pending packet state %s: key %s/%d is both active and dead-lettered", t.statePath, key.sourceClient, key.sequence)
		}
		t.deadLettered = append(t.deadLettered, info)
		t.deadLetteredByKey[key] = info
	}
	for _, info := range state.Settled {
		info.identity = identifyPacket(info.Packet)
		if _, duplicate := t.settled[info.identity]; duplicate {
			return fmt.Errorf("decode pending packet state %s: duplicate settled packet %s/%d", t.statePath, info.Packet.SourceClient, info.Packet.Sequence)
		}
		t.settled[info.identity] = info
	}
	return nil
}

// persistLocked atomically replaces the state file while t.mtx is held.
func (t *PendingPacketTracker) persistLocked() error {
	if t.statePath == "" {
		return nil
	}
	state := pendingPacketState{
		Version:      pendingPacketStateVersion,
		Packets:      make([]pendingPacketInfo, 0, len(t.packets)),
		DeadLettered: append([]pendingPacketInfo(nil), t.deadLettered...),
		Settled:      make([]settledPacketInfo, 0, len(t.settled)),
	}
	for _, info := range t.packets {
		state.Packets = append(state.Packets, info)
	}
	for _, info := range t.settled {
		state.Settled = append(state.Settled, info)
	}
	sort.Slice(state.Packets, func(i, j int) bool {
		a, b := state.Packets[i].Packet, state.Packets[j].Packet
		if a.SourceClient != b.SourceClient {
			return a.SourceClient < b.SourceClient
		}
		return a.Sequence < b.Sequence
	})
	sort.Slice(state.Settled, func(i, j int) bool {
		a, b := state.Settled[i].Packet, state.Settled[j].Packet
		if a.SourceClient != b.SourceClient {
			return a.SourceClient < b.SourceClient
		}
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		if a.DestinationClient != b.DestinationClient {
			return a.DestinationClient < b.DestinationClient
		}
		if a.TimeoutTimestamp != b.TimeoutTimestamp {
			return a.TimeoutTimestamp < b.TimeoutTimestamp
		}
		ai, bi := identifyPacket(a), identifyPacket(b)
		return bytes.Compare(ai.payloadsDigest[:], bi.payloadsDigest[:]) < 0
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err == nil {
		writeState := t.writeState
		if writeState == nil {
			writeState = replaceStateFile
		}
		err = writeState(t.statePath, data)
	}
	return err
}

type pendingTrackerSnapshot struct {
	packets           map[packetKey]pendingPacketInfo
	deadLettered      []pendingPacketInfo
	deadLetteredByKey map[packetKey]pendingPacketInfo
	settled           map[packetIdentity]settledPacketInfo
}

func clonePendingPacketMap(source map[packetKey]pendingPacketInfo) map[packetKey]pendingPacketInfo {
	cloned := make(map[packetKey]pendingPacketInfo, len(source))
	for key, info := range source {
		cloned[key] = info
	}
	return cloned
}

func cloneSettledPacketMap(source map[packetIdentity]settledPacketInfo) map[packetIdentity]settledPacketInfo {
	cloned := make(map[packetIdentity]settledPacketInfo, len(source))
	for key, info := range source {
		cloned[key] = info
	}
	return cloned
}

func (t *PendingPacketTracker) snapshotLocked() pendingTrackerSnapshot {
	return pendingTrackerSnapshot{
		packets:           clonePendingPacketMap(t.packets),
		deadLettered:      append([]pendingPacketInfo(nil), t.deadLettered...),
		deadLetteredByKey: clonePendingPacketMap(t.deadLetteredByKey),
		settled:           cloneSettledPacketMap(t.settled),
	}
}

// commitLocked makes an in-memory mutation durable before acknowledging it.
// If the atomic snapshot replacement fails, every tracker view is rolled back
// together. This prevents a later restart from loading an older retry count or
// missing tombstone after the running process already acted as if it persisted.
func (t *PendingPacketTracker) commitLocked(mutate func()) error {
	if t.statePath == "" {
		mutate()
		return nil
	}
	before := t.snapshotLocked()
	mutate()
	if err := t.persistLocked(); err != nil {
		t.packets = before.packets
		t.deadLettered = before.deadLettered
		t.deadLetteredByKey = before.deadLetteredByKey
		t.settled = before.settled
		t.persistenceErr = fmt.Errorf("persist pending packet state %s: %w", t.statePath, err)
		log.Printf("ERROR [PendingTracker][ATTENTION] %v; mutation rolled back and timeout retries are paused", t.persistenceErr)
		return t.persistenceErr
	}
	if t.persistenceErr != nil {
		log.Printf("[PendingTracker] pending packet state recovered at %s; timeout retries may resume", t.statePath)
		t.persistenceErr = nil
	}
	return nil
}

func replaceStateFile(path string, data []byte) error {
	return writeStateFile(path, data)
}

// Add records a packet only after its retry state is durable. It returns false
// when persistence fails and the mutation is rolled back; callers must retry
// the source event instead of relaying a packet that timeout recovery cannot
// see.
func (t *PendingPacketTracker) Add(packet channeltypesv2.Packet, blockNumber uint64) bool {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	key := packetKey{sourceClient: packet.SourceClient, sequence: packet.Sequence}
	identity := identifyPacket(packet)
	// A terminal event may have won the race while this packet sat in a handed-off
	// batch. Treat that as a successful no-op: false means a durable write failed
	// and makes the caller re-queue, which would otherwise turn a settled packet
	// into a retry loop. The relay module asks IsSettled after Add and skips proof
	// and submission in this case.
	if _, settled := t.settled[identity]; settled {
		return true
	}
	// The relay module offers the same source event again on every retry. Once a
	// packet is tracked, its timeout attempts, deferrals and NotBefore deadline
	// are authoritative retry state; replacing the record here would erase that
	// state and let normal re-queueing bypass both backoff and dead-lettering.
	if info, exists := t.packets[key]; exists && info.identity == identity {
		return true
	}
	_, replacesDeadLetter := t.deadLetteredByKey[key]
	if info, dead := t.deadLetteredByKey[key]; dead {
		if info.identity != identity {
			replacesDeadLetter = true
		} else {
			return true
		}
	}
	return t.commitLocked(func() {
		if replacesDeadLetter {
			t.removeDeadLettered(key)
		}
		t.packets[key] = pendingPacketInfo{
			Packet:      packet,
			ObservedAt:  time.Now(),
			BlockNumber: blockNumber,
			identity:    identity,
		}
	}) == nil
}

// MarkSettled records that packet's lifecycle has completed and removes its
// matching active pending record in one durable mutation. It records the
// terminal fact even when no active record exists: that is the handoff race,
// where the packet is between the queue and the eventual Add call.
func (t *PendingPacketTracker) MarkSettled(packet channeltypesv2.Packet) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	identity := identifyPacket(packet)
	key := packetKey{sourceClient: packet.SourceClient, sequence: packet.Sequence}
	active, activeExists := t.packets[key]
	_, alreadySettled := t.settled[identity]
	if alreadySettled && (!activeExists || active.identity != identity) {
		return nil
	}
	return t.commitLocked(func() {
		t.settled[identity] = settledPacketInfo{
			Packet:    packet,
			SettledAt: time.Now(),
			identity:  identity,
		}
		if activeExists && active.identity == identity {
			delete(t.packets, key)
		}
	})
}

// IsSettled reports whether an exact packet identity has a durable terminal
// tombstone. It intentionally does not look at source-client/sequence alone:
// a client migration can reuse that pair for a new packet that must still relay.
func (t *PendingPacketTracker) IsSettled(packet channeltypesv2.Packet) bool {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	_, settled := t.settled[identifyPacket(packet)]
	return settled
}

// removeDeadLettered clears a tombstone that belongs to a different packet
// reusing its source-client/sequence key after a client re-creation. The
// dead-letter slice backs the queue-state gauge, so it must be updated with the
// authoritative tombstone map.
func (t *PendingPacketTracker) removeDeadLettered(key packetKey) {
	delete(t.deadLetteredByKey, key)
	retained := t.deadLettered[:0]
	for _, info := range t.deadLettered {
		if info.Packet.SourceClient == key.sourceClient && info.Packet.Sequence == key.sequence {
			continue
		}
		retained = append(retained, info)
	}
	t.deadLettered = retained
}

// ClearDeferralsIfCurrent resets a packet's deferral count after a cycle that got far
// enough to prove the shared prerequisites are healthy again.
//
// Without it the count only ever grows, so one outage early in a packet's life
// leaves it permanently near the backoff cap and permanently inside the stuck
// gauge — a stale alarm, and a slower retry than the packet deserves.
// It ignores a recovered prerequisite result from a scan whose packet was
// replaced while the scan was in flight.
func (t *PendingPacketTracker) ClearDeferralsIfCurrent(scanned pendingPacketInfo) error {
	return t.ClearDeferralsIfCurrentBatch([]pendingPacketInfo{scanned})
}

// ClearDeferralsIfCurrentBatch resets matching scan snapshots in one durable
// mutation, avoiding one complete snapshot/fsync per packet after a shared
// prerequisite recovers.
func (t *PendingPacketTracker) ClearDeferralsIfCurrentBatch(scanned []pendingPacketInfo) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	updates := make(map[packetKey]pendingPacketInfo)
	for _, snapshot := range scanned {
		key := packetKey{sourceClient: snapshot.Packet.SourceClient, sequence: snapshot.Packet.Sequence}
		info, ok := t.packets[key]
		if !ok || info.identity != identityOf(snapshot) || info.Deferrals == 0 {
			continue
		}
		info.Deferrals = 0
		updates[key] = info
	}
	if len(updates) == 0 {
		return nil
	}
	return t.commitLocked(func() {
		for key, info := range updates {
			t.packets[key] = info
		}
	})
}

// RemovePacketIfCurrent is the packet-carrying counterpart used by terminal
// source events. A delayed acknowledgement/timeout from an old client epoch
// must not delete a replacement packet that reused the same lookup key.
func (t *PendingPacketTracker) RemovePacketIfCurrent(packet channeltypesv2.Packet) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	key := packetKey{sourceClient: packet.SourceClient, sequence: packet.Sequence}
	info, ok := t.packets[key]
	if !ok || info.identity != identifyPacket(packet) {
		return nil
	}
	return t.commitLocked(func() { delete(t.packets, key) })
}

// RemoveSlotIfPresent removes whatever entry occupies (sourceClient, sequence),
// regardless of its identity, and reports whether there was one.
//
// This is deliberately NOT how the pending trackers remove. There, a replayed
// sequence is a DIFFERENT packet that still needs its own timeout, which is
// exactly why RemovePacketIfCurrent compares identity before deleting.
//
// It is the right removal for the owed-acknowledgement ledger, where the slot
// holds a debt rather than a packet awaiting timeout. An acknowledgement for
// (client, sequence) settles that slot, and a debt left there for a superseded
// identity can never be settled by anything else: the packet it belonged to no
// longer exists on the source, so no further acknowledgement will ever name it.
// Left alone it is reported overdue for the life of the state file.
//
// Only ClearAckDue should call this. A caller reaching for it on PendingTracker,
// EthPendingTracker or L2PendingTracker wants RemovePacketIfCurrent instead.
func (t *PendingPacketTracker) RemoveSlotIfPresent(sourceClient string, sequence uint64) (bool, error) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	key := packetKey{sourceClient: sourceClient, sequence: sequence}
	if _, ok := t.packets[key]; !ok {
		return false, nil
	}
	if err := t.commitLocked(func() { delete(t.packets, key) }); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveIfCurrent applies a scanner outcome only when the active packet is the
// exact packet represented by the scan snapshot.
func (t *PendingPacketTracker) RemoveIfCurrent(scanned pendingPacketInfo) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	key := packetKey{sourceClient: scanned.Packet.SourceClient, sequence: scanned.Packet.Sequence}
	info, ok := t.packets[key]
	if !ok || info.identity != identityOf(scanned) {
		return nil
	}
	return t.commitLocked(func() { delete(t.packets, key) })
}

// TimeoutRetriesAllowed is the timeout scanner's circuit-breaker probe. Once a
// retry-state mutation failed, no packet is allowed to start more proof or
// transaction work until the current snapshot can be durably written again.
// A successful probe deliberately still skips this scan; the following scan can
// make a fresh, durable retry-state transition before doing timeout work.
func (t *PendingPacketTracker) TimeoutRetriesAllowed() (bool, error) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	if t.persistenceErr == nil {
		return true, nil
	}
	if err := t.persistLocked(); err != nil {
		t.persistenceErr = fmt.Errorf("persist pending packet state %s: %w", t.statePath, err)
		return false, t.persistenceErr
	}
	log.Printf("[PendingTracker] pending packet state recovered at %s; holding one timeout scan before retrying", t.statePath)
	t.persistenceErr = nil
	return false, nil
}

// PersistenceError exposes the current fail-closed state to queue reporting
// and operational status surfaces. It is nil while tracker mutations are
// durably acknowledged.
func (t *PendingPacketTracker) PersistenceError() error {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	return t.persistenceErr
}

// OlderThan returns the packets observed longer ago than d, oldest first.
//
// It sorts because the caller reports a capped list and the oldest entry is the
// one worth naming: a packet owed an acknowledgement for a day is a different
// problem from one owed it for a minute, and an unsorted cap would hide the
// first behind an arbitrary handful of the second.
func (t *PendingPacketTracker) OlderThan(d time.Duration) []channeltypesv2.Packet {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	cutoff := time.Now().Add(-d)
	due := make([]pendingPacketInfo, 0, len(t.packets))
	for _, info := range t.packets {
		if info.ObservedAt.Before(cutoff) {
			due = append(due, info)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].ObservedAt.Before(due[j].ObservedAt) })
	packets := make([]channeltypesv2.Packet, 0, len(due))
	for _, info := range due {
		packets = append(packets, info.Packet)
	}
	return packets
}

func (t *PendingPacketTracker) GetAll() []pendingPacketInfo {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	result := make([]pendingPacketInfo, 0, len(t.packets))
	for _, info := range t.packets {
		result = append(result, info)
	}
	return result
}

// GetDue returns the tracked packets whose backoff has elapsed. The scanners use
// this instead of GetAll so a packet under backoff is not re-proven every cycle.
func (t *PendingPacketTracker) GetDue(now time.Time) []pendingPacketInfo {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	if t.persistenceErr != nil {
		// A rolled-back retry transition must never look immediately eligible to
		// another caller while the timeout circuit breaker is open.
		return nil
	}
	result := make([]pendingPacketInfo, 0, len(t.packets))
	for _, info := range t.packets {
		if info.NotBefore.After(now) {
			continue
		}
		result = append(result, info)
	}
	return result
}

// RecordTimeoutFailureIfCurrent charges one attempt against a packet whose timeout
// submission failed and pushes it behind a growing backoff. Past
// maxTimeoutAttempts the packet is dead-lettered: removed from the scan set and
// kept for inspection, with a loud log. Reports whether it was dead-lettered.
//
// Dead-lettering a timeout is not the same as dropping a relay — the packet's
// funds are escrowed and only a timeout releases them, so this is an operator
// alarm, not a cleanup. The alternative (retrying forever) spends a Groth16 proof
// every 30s on a submission that has already failed the same way many times.
// It prevents a permanent failure from an old scan charging the retry budget of
// a replacement packet at the same lookup key. A non-nil error means the
// mutation was rolled back and timeout retries are paused.
func (t *PendingPacketTracker) RecordTimeoutFailureIfCurrent(scanned pendingPacketInfo, now time.Time) (bool, error) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	key := packetKey{sourceClient: scanned.Packet.SourceClient, sequence: scanned.Packet.Sequence}
	info, ok := t.packets[key]
	if !ok || info.identity != identityOf(scanned) {
		return false, nil
	}
	return t.recordTimeoutFailureLocked(key, info, now)
}

func (t *PendingPacketTracker) recordTimeoutFailureLocked(key packetKey, info pendingPacketInfo, now time.Time) (bool, error) {
	info.TimeoutAttempts++
	deadLettered := info.TimeoutAttempts > maxTimeoutAttempts
	err := t.commitLocked(func() {
		if deadLettered {
			info.DeadLetteredAt = now
			delete(t.packets, key)
			t.deadLettered = append(t.deadLettered, info)
			t.deadLetteredByKey[key] = info
			return
		}
		info.NotBefore = now.Add(nextTimeoutBackoff(info.TimeoutAttempts))
		t.packets[key] = info
	})
	return err == nil && deadLettered, err
}

// DeferTimeoutRetryIfCurrent backs a packet off WITHOUT charging an attempt.
//
// It is for failures that are not this packet's fault: the light-client update
// every timeout in a scan depends on, a counterparty clock that has not reached
// the packet's timeout yet, an RPC that is down. Those fail identically for
// every packet in the scan, so charging them would dead-letter the whole set for
// one outage — and the packets it would dead-letter are precisely the ones whose
// funds are escrowed and can only be released by a timeout.
//
// The backoff still applies, because the RLY-06 cost problem is real either way:
// without it the scanner re-runs a client update and a Groth16 proof for every
// tracked packet every 30 seconds for as long as the outage lasts. Deferrals
// shape that backoff but are counted separately from TimeoutAttempts and never
// dead-letter, so an infrastructure outage delays a timeout and never abandons
// it.
// Returns the packet's deferral count so the caller can alert on a packet that
// has been deferring long enough to be stuck rather than merely delayed, and 0
// when the packet is no longer tracked. A non-nil error means the mutation was
// rolled back and timeout retries are paused.
// It prevents a transient result from an old scan imposing its backoff and
// stuck count on a replacement packet.
func (t *PendingPacketTracker) DeferTimeoutRetryIfCurrent(scanned pendingPacketInfo, now time.Time) (int, error) {
	counts, err := t.DeferTimeoutRetriesIfCurrent([]pendingPacketInfo{scanned}, now)
	if len(counts) == 0 {
		return 0, err
	}
	return counts[0], err
}

// DeferTimeoutRetriesIfCurrent backs off all still-current scan snapshots with
// one lock and one persisted snapshot. The returned counts align with scanned;
// zero with a nil error means the packet was replaced or removed. A non-nil
// error means the whole mutation was rolled back and timeout retries are
// circuit-broken until persistence recovers.
func (t *PendingPacketTracker) DeferTimeoutRetriesIfCurrent(scanned []pendingPacketInfo, now time.Time) ([]int, error) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	counts := make([]int, len(scanned))
	updates := make(map[packetKey]pendingPacketInfo)
	for i, snapshot := range scanned {
		key := packetKey{sourceClient: snapshot.Packet.SourceClient, sequence: snapshot.Packet.Sequence}
		info, ok := t.packets[key]
		if !ok || info.identity != identityOf(snapshot) {
			continue
		}
		info.Deferrals++
		info.NotBefore = now.Add(nextTimeoutBackoff(info.Deferrals))
		updates[key] = info
		counts[i] = info.Deferrals
	}
	if len(updates) == 0 {
		return counts, nil
	}
	if err := t.commitLocked(func() {
		for key, info := range updates {
			t.packets[key] = info
		}
	}); err != nil {
		clear(counts)
		return counts, err
	}
	return counts, nil
}

// deferralIsStuck reports whether a deferral count has reached the point worth
// alerting on, and repeats every deferralAlertThreshold after that so a stall
// that outlives one log rotation is still visible.
func deferralIsStuck(deferrals int) bool {
	return deferrals >= deferralAlertThreshold && deferrals%deferralAlertThreshold == 0
}

// StuckTimeouts returns how many tracked packets have deferred past
// deferralAlertThreshold, and the highest deferral count among them.
//
// Deferrals never dead-letter, so they never reach the dead-letter gauges — a
// packet can sit at the 30-minute backoff cap indefinitely with every counter
// reading zero. That is the loop this reports: un-timed-out packets whose funds
// are escrowed just the same, distinguished from a healthy queue only by how
// long they have been going nowhere.
func (t *PendingPacketTracker) StuckTimeouts() (count, worst int) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	for _, info := range t.packets {
		if info.Deferrals >= deferralAlertThreshold {
			count++
			if info.Deferrals > worst {
				worst = info.Deferrals
			}
		}
	}
	return count, worst
}

// DeadLetteredTimeouts returns how many packets stopped being retried for
// timeout. Non-zero means escrowed funds need an operator.
func (t *PendingPacketTracker) DeadLetteredTimeouts() int {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	return len(t.deadLettered)
}

// OldestDeadLetteredTimeout returns how long ago the earliest dead-lettered
// timeout gave up, or 0 when there are none. Mirrors BatchBuilder.OldestDeadLetter:
// a count alone cannot distinguish "one packet failed a minute ago" from "three
// have been stranded for a day".
//
// Reads index 0 because DeadLetteredAt is assigned from a monotonically
// advancing clock and entries are only ever appended, so the slice is ordered by
// that field.
func (t *PendingPacketTracker) OldestDeadLetteredTimeout(now time.Time) time.Duration {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	if len(t.deadLettered) == 0 {
		return 0
	}
	return now.Sub(t.deadLettered[0].DeadLetteredAt)
}

// DeadLetterEntry is one packet that stopped being retried, with what an
// operator needs to decide about it: which packet, how long it has been
// stranded, and how many attempts were spent before giving up.
//
// The packet alone was not enough. "Three packets are dead-lettered" and "three
// packets have been dead-lettered since Tuesday" call for different actions, and
// the attempt count is the difference between a deterministic failure and a
// ladder that merely ran out during an outage.
type DeadLetterEntry struct {
	Packet         channeltypesv2.Packet `json:"-"`
	SourceClient   string                `json:"source_client"`
	Sequence       uint64                `json:"sequence"`
	DeadLetteredAt time.Time             `json:"dead_lettered_at"`
	Attempts       int                   `json:"attempts"`
}

// DeadLetteredTimeoutPackets returns the dead-lettered packets without removing
// them, so a status endpoint can list WHICH packets need intervention rather
// than only how many.
//
// The count and the age (DeadLetteredTimeouts, OldestDeadLetteredTimeout) answer
// "is something stuck" and "for how long". Neither answers "which", and the
// difference is not cosmetic: a packet's identity is source client PLUS
// sequence -- see packetIdentity and the note on pendingPacketInfo -- while the
// one log line emitted at dead-letter time carries the sequence alone. With more
// than one client in play, a non-zero gauge and a full log cannot together name
// the packet whose funds are escrowed. This can.
//
// Returned by value: the caller gets a copy it cannot use to mutate tracker
// state, and reading is not allowed to clear the alarm.
func (t *PendingPacketTracker) DeadLetteredTimeoutPackets() []DeadLetterEntry {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	out := make([]DeadLetterEntry, 0, len(t.deadLettered))
	for _, info := range t.deadLettered {
		out = append(out, DeadLetterEntry{
			Packet:         info.Packet,
			SourceClient:   info.Packet.SourceClient,
			Sequence:       info.Packet.Sequence,
			DeadLetteredAt: info.DeadLetteredAt,
			Attempts:       info.TimeoutAttempts,
		})
	}
	return out
}

// ReactivateDeadLetteredTimeout returns one dead-lettered packet to the timeout
// scan set. It is the ONLY way out of that set, deliberately: the dead-letter
// log tells an operator to intervene, and until now the only mechanism for
// intervening was unreachable Go.
//
// The retry budget is reset rather than resumed. A packet reaches the
// dead-letter set by exhausting the ladder, so returning it with the ladder
// still exhausted would spend one scan re-discovering that. Reactivating means a
// person decided the cause is gone -- a client repaired, an endpoint replaced --
// and the packet deserves the same budget a fresh one gets.
//
// It refuses an unknown packet rather than succeeding quietly: an operator who
// mistyped a sequence must learn that here, not by watching nothing happen.
func (t *PendingPacketTracker) ReactivateDeadLetteredTimeout(sourceClient string, sequence uint64) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	key := packetKey{sourceClient: sourceClient, sequence: sequence}
	info, dead := t.deadLetteredByKey[key]
	if !dead {
		return fmt.Errorf("no dead-lettered packet for client %s sequence %d", sourceClient, sequence)
	}
	if _, active := t.packets[key]; active {
		// Both sets holding one key is the invalid state the snapshot decoder
		// already refuses to load. Reactivating into it would make the duplicate
		// permanent instead of visible.
		return fmt.Errorf("packet %s/%d is both active and dead-lettered; refusing to reactivate into an inconsistent tracker",
			sourceClient, sequence)
	}

	return t.commitLocked(func() {
		t.removeDeadLettered(key)
		info.TimeoutAttempts = 0
		info.NotBefore = time.Time{}      // due on the next scan
		info.DeadLetteredAt = time.Time{} // no longer given up on
		t.packets[key] = info
	})
}

func (t *PendingPacketTracker) Len() int {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	return len(t.packets)
}

// PurgeStaleWithoutTimeout removes stale packets without a timeout obligation,
// expires old terminal tombstones, and renews dead letters that have remained
// visible for deadLetterRetention. Renewing, rather than merely deleting their
// tombstones, keeps a timeout recoverable even when the subscriber no longer
// replays its source event.
func (t *PendingPacketTracker) PurgeStaleWithoutTimeout(maxAge time.Duration) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	now := time.Now()
	changed := false
	for _, info := range t.packets {
		if info.Packet.TimeoutTimestamp == 0 && now.Sub(info.ObservedAt) > maxAge {
			changed = true
			break
		}
	}
	if !changed {
		for _, info := range t.settled {
			if now.Sub(info.SettledAt) >= settledPacketRetention {
				changed = true
				break
			}
		}
	}
	if !changed {
		for _, info := range t.deadLettered {
			if now.Sub(info.DeadLetteredAt) >= deadLetterRetention {
				changed = true
				break
			}
		}
	}
	if !changed {
		return nil
	}
	return t.commitLocked(func() {
		t.purgeStaleWithoutTimeoutLocked(now, maxAge)
	})
}

func (t *PendingPacketTracker) purgeStaleWithoutTimeoutLocked(now time.Time, maxAge time.Duration) {
	for key, info := range t.packets {
		if info.Packet.TimeoutTimestamp == 0 && now.Sub(info.ObservedAt) > maxAge {
			delete(t.packets, key)
		}
	}
	for identity, info := range t.settled {
		if now.Sub(info.SettledAt) >= settledPacketRetention {
			delete(t.settled, identity)
		}
	}
	if len(t.deadLettered) == 0 {
		return
	}
	retained := t.deadLettered[:0]
	for _, info := range t.deadLettered {
		if now.Sub(info.DeadLetteredAt) < deadLetterRetention {
			retained = append(retained, info)
			continue
		}
		key := packetKey{sourceClient: info.Packet.SourceClient, sequence: info.Packet.Sequence}
		delete(t.deadLetteredByKey, key)
		info.TimeoutAttempts = 0
		info.Deferrals = 0
		info.NotBefore = time.Time{}
		info.DeadLetteredAt = time.Time{}
		info.ObservedAt = now
		t.packets[key] = info
	}
	t.deadLettered = retained
}
