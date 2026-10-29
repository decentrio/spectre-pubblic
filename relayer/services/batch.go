package services

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func (p CosmosPacketType) String() string {
	switch p {
	case CosmosSend:
		return "Send"
	case CosmosAck:
		return "Ack"
	case CosmosTimeout:
		return "Timeout"
	case CosmosAcknowledged:
		return "Acknowledged"
	default:
		return fmt.Sprintf("Unknown(%d)", int(p))
	}
}

func (p EthPacketType) String() string {
	switch p {
	case EthSend:
		return "Send"
	case EthWriteAck:
		return "WriteAck"
	case EthAck:
		return "Ack"
	case EthTimeout:
		return "Timeout"
	default:
		return fmt.Sprintf("Unknown(%d)", int(p))
	}
}

type CosmosPacketType int

const (
	CosmosSend CosmosPacketType = iota
	CosmosAck
	CosmosTimeout
	// CosmosAcknowledged is a TERMINAL event: Cosmos consumed the acknowledgement
	// for a packet it sent, so that packet's lifecycle is closed. It creates no
	// relay work -- it only settles the pending tracker -- and must never reach
	// the batch, or it becomes an empty "relay" of a packet with nothing to do.
	CosmosAcknowledged
)

type EthPacketType int

const (
	EthSend EthPacketType = iota
	EthWriteAck
	EthAck
	EthTimeout
)

type CosmosPacket struct {
	Type        CosmosPacketType
	Packet      *channeltypesv2.Packet
	AckBytes    [][]byte
	BlockNumber uint64
	// NotBefore, when set, holds the packet out of Check* flushes until the time
	// passes — a re-queue backoff for a packet that is not yet relayable (source
	// state lag / finality) so the loop polls it less aggressively than the batch
	// period. Zero (the legacy default) means "ready now". waitAttempts drives the
	// exponential growth of that backoff.
	NotBefore    time.Time
	waitAttempts int
}

type EthPacket struct {
	Type        EthPacketType
	Packet      *channeltypesv2.Packet
	AckBytes    [][]byte
	BlockNumber uint64
	// NotBefore / waitAttempts — see CosmosPacket.
	NotBefore    time.Time
	waitAttempts int
}

type CosmosBatch struct {
	Packets []CosmosPacket
}

type EthBatch struct {
	Packets []EthPacket
}

type BatchBuilder struct {
	cosmosMtx         sync.Mutex
	ethMtx            sync.Mutex
	cosmosTimestamp   time.Time
	ethTimestamp      time.Time
	cosmosPackets     []CosmosPacket
	ethPackets        []EthPacket
	PendingTracker    *PendingPacketTracker
	EthPendingTracker *PendingPacketTracker
	L2PendingTracker  *PendingPacketTracker

	// AckDueTracker records packets whose receive we relayed and whose
	// acknowledgement has not come back. It is the only way to notice an ack the
	// scan window missed: waitTracker follows events it OBSERVED, and an ack
	// nobody saw produces no event to wait on.
	AckDueTracker *PendingPacketTracker

	// settleOwedAck closes the owed-acknowledgement record for a packet whose
	// acknowledgement has arrived BY ANY ROUTE, not only by this process relaying
	// it. The Services constructors wire it to ClearAckDue; nil means no ledger.
	//
	// It lives here rather than on Services because the three places that observe
	// a terminal acknowledgement -- the Cosmos subscriber, the EVM source and the
	// L2 source -- hold a BatchBuilder and not a Services. The clearing has to go
	// through ClearAckDue: cancelling a queued intent and retrying a rolled-back
	// removal are state on Services, so reaching into AckDueTracker directly would
	// leave a settled debt queued for rewriting.
	settleOwedAck func(channeltypesv2.Packet)

	// A flushed chunk is absent from the queue while its handler is running.
	// Keep a multiset of its lowest source heights so durable recovery cursors
	// cannot advance past packets that still only exist in this process.
	cosmosInFlight map[uint64]int
	ethInFlight    map[uint64]int
}

// WithOwedAckSettler installs the hook that closes an owed-acknowledgement
// record. Called once by each Services constructor.
func (b *BatchBuilder) WithOwedAckSettler(settle func(channeltypesv2.Packet)) {
	if b == nil {
		return
	}
	b.settleOwedAck = settle
}

// SettleOwedAck closes the owed-acknowledgement record for a packet whose
// acknowledgement arrived, whoever relayed it.
//
// Reported by @DongLieu: the ledger was cleared only when THIS process relayed
// the AckPacket, while the three terminal-event paths removed the pending record
// and stopped there. Running two relayers is the ordinary case, so the ordinary
// sequence was: this process delivers the receive and records the debt, the other
// submits the acknowledgement, this one observes the terminal event and drops its
// pending record -- and keeps the debt. The packet is settled on-chain and now has
// a receipt, so it can never time out either; the debt is durable, so it survives
// the restart, and the watcher reports it overdue for as long as the state file
// lives. A watcher that names settled packets is worse than no watcher: the real
// overdue entry is then indistinguishable from the noise.
//
// A no-op when no ledger is running, so the terminal paths call it unconditionally.
func (b *BatchBuilder) SettleOwedAck(packet channeltypesv2.Packet) {
	if b == nil || b.settleOwedAck == nil {
		return
	}
	b.settleOwedAck(packet)
}

func NewBatchBuilder() *BatchBuilder {
	now := time.Now()
	return &BatchBuilder{
		cosmosTimestamp:   now,
		ethTimestamp:      now,
		cosmosPackets:     []CosmosPacket{},
		ethPackets:        []EthPacket{},
		PendingTracker:    NewPendingPacketTracker(),
		EthPendingTracker: NewPendingPacketTracker(),
		L2PendingTracker:  NewPendingPacketTracker(),
		// In-memory like its three siblings above. Leaving it nil made every
		// non-persistent setup panic on the first RecordAckDue -- Add takes
		// t.mtx on a nil receiver -- and Services.New uses THIS constructor, so
		// that is the default adapter-engine path, not an exotic one.
		AckDueTracker:  NewPendingPacketTracker(),
		cosmosInFlight: map[uint64]int{},
		ethInFlight:    map[uint64]int{},
	}
}

// NewPersistentBatchBuilder restores all timeout trackers before returning, so
// source subscription and startup recovery cannot replay packets into empty
// retry budgets. Each direction has its own atomically replaced snapshot.
func NewPersistentBatchBuilder(stateDir string) (*BatchBuilder, error) {
	cosmosTracker, err := NewPersistentPendingPacketTracker(filepath.Join(stateDir, "cosmos.json"))
	if err != nil {
		return nil, err
	}
	ethTracker, err := NewPersistentPendingPacketTracker(filepath.Join(stateDir, "eth.json"))
	if err != nil {
		return nil, err
	}
	// A fourth tracker, holding a different KIND of record: not "sent, may need a
	// timeout refund" but "we relayed the receive, the acknowledgement is owed".
	// It reuses the same durable machinery because the requirement is the same --
	// survive a restart -- and ObservedAt is exactly the due-since the overdue
	// check needs.
	ackDueTracker, err := NewPersistentPendingPacketTracker(filepath.Join(stateDir, "awaiting-acks.json"))
	if err != nil {
		return nil, err
	}

	l2Tracker, err := NewPersistentPendingPacketTracker(filepath.Join(stateDir, "l2.json"))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &BatchBuilder{
		cosmosTimestamp:   now,
		ethTimestamp:      now,
		cosmosPackets:     []CosmosPacket{},
		ethPackets:        []EthPacket{},
		PendingTracker:    cosmosTracker,
		EthPendingTracker: ethTracker,
		L2PendingTracker:  l2Tracker,
		AckDueTracker:     ackDueTracker,
		cosmosInFlight:    map[uint64]int{},
		ethInFlight:       map[uint64]int{},
	}, nil
}

func (b *BatchBuilder) AddCosmos(packet CosmosPacket) {
	b.cosmosMtx.Lock()
	b.cosmosPackets = append(b.cosmosPackets, packet)
	count := len(b.cosmosPackets)
	b.cosmosMtx.Unlock()
	log.Printf("[BatchBuilder] Inserted cosmos packet: type=%s seq=%d (batch size: %d)",
		packet.Type, packet.Packet.Sequence, count)
}

// DropCosmosQueued removes a packet from the Cosmos relay queue by identity.
//
// Settling the pending tracker is not enough on its own. A recovery scan can
// read a send and, later in the SAME pass, the acknowledge_packet that closes
// it; the send is already queued by then, and removing only the tracker entry
// leaves the queue to relay a packet whose lifecycle is over -- and the relay
// re-adds the tracker entry that settling just removed, after the scan cursor
// has moved past the terminal event that would clear it again.
//
// It reports whether anything was removed so the caller can say so once rather
// than logging a removal that did not happen.
func (b *BatchBuilder) DropCosmosQueued(packet channeltypesv2.Packet) bool {
	b.cosmosMtx.Lock()
	defer b.cosmosMtx.Unlock()
	// Full identity, not just the client pair and the sequence. A client
	// migration keeps the client id and restarts sequences from 1
	// (create-clients-eth repoints the existing client rather than adding one),
	// so seq=N can name two different packets over the life of one client. A
	// stale terminal event for the OLD seq=N would otherwise silently drop the
	// NEW one from the queue -- never relayed, and nothing left to notice.
	// packetIdentity is the tracker's own answer to exactly this replay, which is
	// why the queue uses it too rather than a weaker key of its own.
	target := identifyPacket(packet)
	kept := b.cosmosPackets[:0:0]
	for _, queued := range b.cosmosPackets {
		if queued.Packet != nil && identifyPacket(*queued.Packet) == target {
			continue
		}
		kept = append(kept, queued)
	}
	removed := len(kept) != len(b.cosmosPackets)
	b.cosmosPackets = kept
	return removed
}

func (b *BatchBuilder) AddEth(packet EthPacket) {
	b.ethMtx.Lock()
	b.ethPackets = append(b.ethPackets, packet)
	count := len(b.ethPackets)
	b.ethMtx.Unlock()
	log.Printf("[BatchBuilder] Inserted eth packet: type=%s seq=%d (batch size: %d)",
		packet.Type, packet.Packet.Sequence, count)
}

// QueueDepths returns the packets waiting in each batch-builder direction.
// A growing value means relay work is arriving faster than the adapters flush it.
func (b *BatchBuilder) QueueDepths() (cosmos, eth int) {
	b.cosmosMtx.Lock()
	cosmos = len(b.cosmosPackets)
	b.cosmosMtx.Unlock()
	b.ethMtx.Lock()
	eth = len(b.ethPackets)
	b.ethMtx.Unlock()
	return cosmos, eth
}

// Waiting-backoff bounds: a packet re-queued because it is not yet relayable
// (source state / finality lag) is held out of flushes with an exponentially
// growing delay, so a quick precondition (Cosmos AppHash H+2, ~1 block) clears
// fast while a long one (ETH beacon finality, minutes) polls sparsely instead of
// every batch period. Bounded so latency after the precondition clears stays low.
const (
	waitingBackoffBase = 3 * time.Second
	waitingBackoffMax  = 15 * time.Second
)

// nextWaitingBackoff returns the delay for the given (already-incremented)
// attempt count: base, 2×base, 4×base, … capped at max.
func nextWaitingBackoff(attempt int) time.Duration {
	d := waitingBackoffBase << (attempt - 1)
	if d > waitingBackoffMax || d <= 0 { // <=0 guards shift overflow
		d = waitingBackoffMax
	}
	return d
}

// RequeueCosmosWaiting re-queues a packet that is valid but NOT YET relayable
// (its source state is not yet provable). It sets a growing NotBefore backoff so
// the loop does not re-attempt (and re-log, and re-hit the RPC) every batch
// period while waiting, then prepends the packet ahead of newer arrivals so a
// re-queued packet is retried before newer work. This is the only re-queue path:
// the adapter module drops permanently-failed packets at the source (the timeout
// scanner refunds them), so there is no retry-budget / dead-letter classification.
func (b *BatchBuilder) RequeueCosmosWaiting(packets []CosmosPacket) {
	if len(packets) == 0 {
		return
	}
	now := time.Now()
	for i := range packets {
		packets[i].waitAttempts++
		packets[i].NotBefore = now.Add(nextWaitingBackoff(packets[i].waitAttempts))
	}
	b.cosmosMtx.Lock()
	b.cosmosPackets = append(packets, b.cosmosPackets...)
	remaining := len(b.cosmosPackets)
	b.cosmosMtx.Unlock()
	log.Printf("DEBUG [BatchBuilder] re-queued %d cosmos packet(s) (waiting) for retry (queue now %d)", len(packets), remaining)
}

// RequeueEthWaiting — see RequeueCosmosWaiting.
func (b *BatchBuilder) RequeueEthWaiting(packets []EthPacket) {
	if len(packets) == 0 {
		return
	}
	now := time.Now()
	for i := range packets {
		packets[i].waitAttempts++
		packets[i].NotBefore = now.Add(nextWaitingBackoff(packets[i].waitAttempts))
	}
	b.ethMtx.Lock()
	b.ethPackets = append(packets, b.ethPackets...)
	remaining := len(b.ethPackets)
	b.ethMtx.Unlock()
	log.Printf("DEBUG [BatchBuilder] re-queued %d eth packet(s) (waiting) for retry (queue now %d)", len(packets), remaining)
}

// BatchHandoffCapacity is the capacity the source bridges must give the channel
// Check* hands batches to. It MUST stay 0.
//
// The chunk is sliced OFF the queue before the handoff, so the packets exist in
// exactly one place: the send. With a buffered channel the send can SUCCEED into
// the buffer while the consumer, selecting on the same cancellation, returns
// without ever receiving it — the chunk is then in neither the queue nor a
// consumer, and nothing restores it. Guarding the send with ctx.Done() does not
// help, because the send arm wins: it is ready.
//
// At capacity 0 the send is a rendezvous. It completes only when a consumer has
// actually taken the batch, and a consumer that has taken it is committed to
// running handleBatch — which re-queues everything when the context is already
// cancelled. So "send completed" and "some goroutine owns these packets" become
// the same fact, which is what makes shutdown lossless.
//
// The cost is only that the producer blocks while the consumer is busy. That is
// backpressure, and it is the correct behaviour: queue depth then shows up in
// the builder, where the logs report it, instead of hiding in a channel.
const BatchHandoffCapacity = 0

// CheckCosmos flushes a ready batch to ch when the size or time trigger fires.
//
// ctx guards the handoff. The chunk is sliced OFF the queue before the send, so
// a send that never completes loses those packets outright — and the send can
// block: ch is a small buffered channel and the consumer holds each batch for as
// long as a relay takes (a Groth16 proof, a tx). On shutdown the consumer stops
// receiving, so an unguarded send would also pin the caller's goroutine past
// cancellation. When ctx wins the race the chunk goes back at the head of the
// queue rather than disappearing.
func (b *BatchBuilder) CheckCosmos(ctx context.Context, config BatchConfig, ch chan<- CosmosBatch) {
	b.cosmosMtx.Lock()

	if len(b.cosmosPackets) == 0 {
		b.cosmosMtx.Unlock()
		return
	}

	// Hold back packets still under their waiting backoff (see CheckEth). Zero
	// NotBefore (legacy default) is always ready, so legacy behavior is unchanged.
	now := time.Now()
	ready, waiting := partitionReadyCosmos(b.cosmosPackets, now)
	if len(ready) == 0 {
		b.cosmosMtx.Unlock()
		return
	}

	flush := false
	reason := ""

	if len(ready) >= int(config.BatchSize) {
		flush = true
		reason = fmt.Sprintf("size limit reached (%d >= %d)", len(ready), config.BatchSize)
	} else if now.After(b.cosmosTimestamp.Add(config.BatchPeriods)) {
		flush = true
		reason = fmt.Sprintf("time limit reached (%v elapsed)", time.Since(b.cosmosTimestamp).Round(time.Millisecond))
	}

	if !flush {
		b.cosmosMtx.Unlock()
		return
	}

	// Slice up to BatchSize so a backlog (queue >> BatchSize) is flushed in
	// fixed-size chunks instead of one oversized multicall that would blow
	// past the EVM 128KB tx size limit.
	chunkSize := int(config.BatchSize)
	if chunkSize <= 0 || chunkSize > len(ready) {
		chunkSize = len(ready)
	}
	chunk := make([]CosmosPacket, chunkSize)
	copy(chunk, ready[:chunkSize])
	b.cosmosPackets = append(ready[chunkSize:], waiting...)
	b.cosmosTimestamp = now
	low := lowestCosmosHeight(chunk)
	if low != 0 {
		b.cosmosInFlight[low]++
	}
	log.Printf("[BatchBuilder] Flushing cosmos batch: %d packets (%s, queue remaining: %d)",
		len(chunk), reason, len(b.cosmosPackets))
	batch := CosmosBatch{Packets: chunk}
	b.cosmosMtx.Unlock()

	select {
	case ch <- batch:
	case <-ctx.Done():
		b.restoreCosmosChunk(chunk)
	}
}

// restoreCosmosChunk puts an un-handed-off chunk back at the HEAD of the queue,
// preserving the order it was flushed in, so a shutdown that interrupts a flush
// leaves the queue exactly as it found it. Deliberately does not touch
// NotBefore/waitAttempts: nothing was attempted, so nothing earned a backoff.
func (b *BatchBuilder) restoreCosmosChunk(chunk []CosmosPacket) {
	if len(chunk) == 0 {
		return
	}
	b.cosmosMtx.Lock()
	b.releaseCosmosInFlightLocked(lowestCosmosHeight(chunk))
	b.cosmosPackets = append(chunk, b.cosmosPackets...)
	remaining := len(b.cosmosPackets)
	b.cosmosMtx.Unlock()
	log.Printf("[BatchBuilder] flush cancelled: returned %d cosmos packet(s) to the queue (queue now %d)", len(chunk), remaining)
}

// ReleaseCosmosInFlight marks a handed-off chunk as submitted, dropped, or
// safely re-queued by its handler.
func (b *BatchBuilder) ReleaseCosmosInFlight(batch CosmosBatch) {
	if len(batch.Packets) == 0 {
		return
	}
	b.cosmosMtx.Lock()
	b.releaseCosmosInFlightLocked(lowestCosmosHeight(batch.Packets))
	b.cosmosMtx.Unlock()
}

func (b *BatchBuilder) releaseCosmosInFlightLocked(low uint64) {
	if low == 0 {
		return
	}
	if b.cosmosInFlight[low] <= 1 {
		delete(b.cosmosInFlight, low)
		return
	}
	b.cosmosInFlight[low]--
}

// partitionReadyCosmos — see partitionReadyEth.
func partitionReadyCosmos(packets []CosmosPacket, now time.Time) (ready, waiting []CosmosPacket) {
	for _, p := range packets {
		if p.NotBefore.IsZero() || !now.Before(p.NotBefore) {
			ready = append(ready, p)
		} else {
			waiting = append(waiting, p)
		}
	}
	return ready, waiting
}

// CheckEth — see CheckCosmos; ctx guards the handoff for the same reason.
func (b *BatchBuilder) CheckEth(ctx context.Context, config BatchConfig, ch chan<- EthBatch) {
	b.ethMtx.Lock()

	if len(b.ethPackets) == 0 {
		b.ethMtx.Unlock()
		return
	}

	// Hold back packets still under their waiting backoff (not yet relayable): only
	// the ready ones are eligible to flush. If none are ready this is a quiet wait
	// — no flush, no log, no downstream RPC. Packets with a zero NotBefore (the
	// legacy default) are always ready, so legacy behavior is unchanged.
	now := time.Now()
	ready, waiting := partitionReadyEth(b.ethPackets, now)
	if len(ready) == 0 {
		b.ethMtx.Unlock()
		return
	}

	flush := false
	reason := ""

	if len(ready) >= int(config.BatchSize) {
		flush = true
		reason = fmt.Sprintf("size limit reached (%d >= %d)", len(ready), config.BatchSize)
	} else if now.After(b.ethTimestamp.Add(config.BatchPeriods)) {
		flush = true
		reason = fmt.Sprintf("time limit reached (%v elapsed)", time.Since(b.ethTimestamp).Round(time.Millisecond))
	}

	if !flush {
		b.ethMtx.Unlock()
		return
	}

	chunkSize := int(config.BatchSize)
	if chunkSize <= 0 || chunkSize > len(ready) {
		chunkSize = len(ready)
	}
	chunk := make([]EthPacket, chunkSize)
	copy(chunk, ready[:chunkSize])
	// Un-flushed ready packets plus the still-waiting ones stay queued.
	b.ethPackets = append(ready[chunkSize:], waiting...)
	b.ethTimestamp = now
	low := lowestEthHeight(chunk)
	if low != 0 {
		b.ethInFlight[low]++
	}
	log.Printf("[BatchBuilder] Flushing eth batch: %d packets (%s, queue remaining: %d)",
		len(chunk), reason, len(b.ethPackets))
	batch := EthBatch{Packets: chunk}
	b.ethMtx.Unlock()

	select {
	case ch <- batch:
	case <-ctx.Done():
		b.restoreEthChunk(chunk)
	}
}

// restoreEthChunk — see restoreCosmosChunk.
func (b *BatchBuilder) restoreEthChunk(chunk []EthPacket) {
	if len(chunk) == 0 {
		return
	}
	b.ethMtx.Lock()
	b.releaseEthInFlightLocked(lowestEthHeight(chunk))
	b.ethPackets = append(chunk, b.ethPackets...)
	remaining := len(b.ethPackets)
	b.ethMtx.Unlock()
	log.Printf("[BatchBuilder] flush cancelled: returned %d eth packet(s) to the queue (queue now %d)", len(chunk), remaining)
}

// ReleaseEthInFlight mirrors ReleaseCosmosInFlight for the ETH queue.
func (b *BatchBuilder) ReleaseEthInFlight(batch EthBatch) {
	if len(batch.Packets) == 0 {
		return
	}
	b.ethMtx.Lock()
	b.releaseEthInFlightLocked(lowestEthHeight(batch.Packets))
	b.ethMtx.Unlock()
}

func (b *BatchBuilder) releaseEthInFlightLocked(low uint64) {
	if low == 0 {
		return
	}
	if b.ethInFlight[low] <= 1 {
		delete(b.ethInFlight, low)
		return
	}
	b.ethInFlight[low]--
}

// LowestUnsubmittedCosmosHeight is the lowest non-zero source height still
// queued or in flight. A recovery cursor must never be persisted above it.
func (b *BatchBuilder) LowestUnsubmittedCosmosHeight() uint64 {
	b.cosmosMtx.Lock()
	defer b.cosmosMtx.Unlock()
	lowest := lowestCosmosHeight(b.cosmosPackets)
	for height := range b.cosmosInFlight {
		if height != 0 && (lowest == 0 || height < lowest) {
			lowest = height
		}
	}
	return lowest
}

// LowestUnsubmittedEthHeight mirrors LowestUnsubmittedCosmosHeight. Both ETH
// recovery streams share this conservative floor because they share one queue.
func (b *BatchBuilder) LowestUnsubmittedEthHeight() uint64 {
	b.ethMtx.Lock()
	defer b.ethMtx.Unlock()
	lowest := lowestEthHeight(b.ethPackets)
	for height := range b.ethInFlight {
		if height != 0 && (lowest == 0 || height < lowest) {
			lowest = height
		}
	}
	return lowest
}

func lowestCosmosHeight(packets []CosmosPacket) uint64 {
	var lowest uint64
	for _, packet := range packets {
		if packet.BlockNumber != 0 && (lowest == 0 || packet.BlockNumber < lowest) {
			lowest = packet.BlockNumber
		}
	}
	return lowest
}

func lowestEthHeight(packets []EthPacket) uint64 {
	var lowest uint64
	for _, packet := range packets {
		if packet.BlockNumber != 0 && (lowest == 0 || packet.BlockNumber < lowest) {
			lowest = packet.BlockNumber
		}
	}
	return lowest
}

// partitionReadyEth splits packets into those eligible to flush now (zero or
// elapsed NotBefore) and those still under a waiting backoff, preserving order
// within each group.
func partitionReadyEth(packets []EthPacket, now time.Time) (ready, waiting []EthPacket) {
	for _, p := range packets {
		if p.NotBefore.IsZero() || !now.Before(p.NotBefore) {
			ready = append(ready, p)
		} else {
			waiting = append(waiting, p)
		}
	}
	return ready, waiting
}
