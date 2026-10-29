// This file holds the optional-capability surface: the callback types a caller
// supplies and the With* functions that install them. They are separated from
// module.go because they are the package's API to its callers, while module.go
// is how the path runs -- two different readers, and the option list is what a
// new relay path is configured from.
package relay

import (
	"context"
	"time"
)

// ScanFunc runs one timeout-recovery sweep for this path: it detects packets that
// were recv-relayed but expired undelivered and submits their MsgTimeout back to
// the source chain. It is called periodically and must handle its own errors (the
// wrapped scanners log and recover internally), so a transient RPC failure never
// stops the loop.
type ScanFunc func(ctx context.Context)

// TrackFunc records a recv-relayed packet — proto-marshaled bytes plus the source
// height it was observed at — so the path's ScanFunc can later refund it if it is
// never delivered. It returns false when the tracker could not durably record
// the packet; the module then re-queues the event without attempting its relay.
// Add is idempotent by packet identity.
type TrackFunc func(packet []byte, height uint64) bool

// UntrackFunc removes a packet (by its proto-marshaled bytes) from the pending
// tracker once the destination confirms delivery. Called after a successful relay
// so the timeout scanner stops considering a packet that can no longer time out.
type UntrackFunc func(packet []byte)

// PendingSettledFunc reports whether a terminal source event has already closed
// this exact packet in the durable pending tracker. It is intentionally separate
// from chain.Destination settlement checks: this closes the local handoff race,
// where a terminal event and the batch handler run concurrently.
type PendingSettledFunc func(packet []byte) bool

// MarkPendingSettledFunc persists a settlement learned from a destination check.
// It returns false only when durable persistence failed; callers still skip the
// known-settled packet and log the degraded persistence separately.
type MarkPendingSettledFunc func(packet []byte) bool

// ClientUpdateObserver records the source-chain timestamp trusted by a client
// after an update is submitted or the builder confirms it is current.
type ClientUpdateObserver func(trustedAt time.Time)

// PeriodicUpdateFunc runs one periodic destination-client update on a fixed
// cadence, independent of packet flow and of the expiry-driven refresh. It
// exists for clients whose freshness need is not captured by ClientExpiresAt
// alone — notably the SpectreClient pinned validator set, which must be rotated
// before its overlap decays below quorum even during quiet periods. It returns an
// error so the loop can retry sooner on failure.
type PeriodicUpdateFunc func(ctx context.Context) error

// Option configures optional Module capabilities (timeout scanning, pending-packet
// tracking) without breaking the base NewModule contract.
type Option func(*Module)

// WithTimeoutScanner runs scan every interval until Run's context is cancelled.
// interval <= 0 falls back to defaultScanInterval.
func WithTimeoutScanner(interval time.Duration, scan ScanFunc) Option {
	return func(m *Module) {
		if interval <= 0 {
			interval = defaultScanInterval
		}
		m.scan = scan
		m.scanInterval = interval
	}
}

// WithPacketTracker records every recv-relayed (SendPacket) packet via track, so
// the path's timeout scanner can refund it if delivery never completes. Tracking
// happens regardless of relay outcome, so a packet that fails to relay and then
// times out is still refunded. untrack removes a packet once the relay succeeds —
// after delivery it can no longer time out, so keeping it in the tracker only
// bloats it and makes the scanner query a receipt it will always find. untrack
// may be nil (no removal hook).
func WithPacketTracker(track TrackFunc, untrack UntrackFunc) Option {
	return func(m *Module) {
		m.track = track
		m.untrack = untrack
	}
}

// WithPendingSettlementGuard wires the durable terminal-tombstone operations for
// a packet tracker. It is paired with WithPacketTracker on paths whose source can
// emit a terminal event while a packet is already handed to handleBatch.
func WithPendingSettlementGuard(isSettled PendingSettledFunc, markSettled MarkPendingSettledFunc) Option {
	return func(m *Module) {
		m.pendingSettled = isSettled
		m.markPendingSettled = markSettled
	}
}

// WithClientUpdateObserver exposes successful client progress to observability
// without coupling the generic relay loop to a particular reporter.
func WithClientUpdateObserver(observer ClientUpdateObserver) Option {
	return func(m *Module) { m.observeClientUpdate = observer }
}

// WithPeriodicUpdate runs a forced client update every interval (independent of
// the expiry-driven refresh) to keep the destination client fresh in ways
// ClientExpiresAt does not capture — e.g. the SpectreClient pinned-set rotation.
// interval <= 0 falls back to defaultPeriodicUpdateInterval.
//
// initialDelay is how long to wait before the FIRST update, derived from the
// client's on-chain freshness (time until it is next due), not process uptime.
// A restart near the rotation deadline therefore fires promptly instead of
// waiting a whole fresh interval — the guarantee that keeps the pinned set above
// quorum. A negative initialDelay is treated as 0 (due now).
func WithPeriodicUpdate(interval, initialDelay time.Duration, periodicUpdate PeriodicUpdateFunc) Option {
	return func(m *Module) {
		if interval <= 0 {
			interval = defaultPeriodicUpdateInterval
		}
		if initialDelay < 0 {
			initialDelay = 0
		}
		m.periodicUpdate = periodicUpdate
		m.periodicUpdateInterval = interval
		m.periodicUpdateInitialDelay = initialDelay
	}
}

// AckDueFunc records that a receive relay succeeded and its acknowledgement is
// now owed. False means the durable write failed; the caller must not treat the
// packet as recorded.
type AckDueFunc func(packet []byte) bool

// AckSettledFunc closes an owed-acknowledgement record.
type AckSettledFunc func(packet []byte)

// OverdueAcksFunc lists packets whose acknowledgement has been owed longer than
// the configured threshold, oldest first.
type OverdueAcksFunc func(threshold time.Duration) []string

// WithAckWatch names the acknowledgements that never came back.
//
// waitTracker can only follow packets it OBSERVED. An acknowledgement written
// while the relayer was down, or outside the scan window, produces no event to
// wait on -- so a missing ack is indistinguishable from one that has simply not
// arrived yet, and the packet's escrow stays locked in silence. Recording the
// debt at the moment the receive relay succeeds is what makes it nameable
// later.
//
// The record must be DURABLE or it says nothing after the restart that is the
// most likely reason the ack was missed in the first place.
//
// interval <= 0 falls back to defaultAckWatchInterval; threshold <= 0 to
// defaultAckOverdueAfter.
func WithAckWatch(interval, threshold time.Duration, due AckDueFunc, settled AckSettledFunc, overdue OverdueAcksFunc) Option {
	return func(m *Module) {
		if interval <= 0 {
			interval = defaultAckWatchInterval
		}
		if threshold <= 0 {
			threshold = defaultAckOverdueAfter
		}
		m.ackDue = due
		m.ackSettled = settled
		m.overdueAcks = overdue
		m.ackWatchInterval = interval
		m.ackOverdueAfter = threshold
	}
}

// WithPacketFlush periodically asks the source to ENUMERATE its outstanding
// packets and relays whatever the destination has not settled. It is a no-op
// unless the source implements chain.PacketLister.
//
// This is the backstop that block scanning cannot be: a scan can only find
// packets inside the window it scans, so a lost cursor, a long outage, or a
// second relayer joining a running path all leave older packets invisible. A
// query finds a packet at any age.
//
// interval <= 0 falls back to defaultFlushInterval. It is minutes, not seconds:
// every pass is real queries against both chains, and because it finds packets
// at any age there is nothing to gain from running it often.
func WithPacketFlush(interval time.Duration) Option {
	return func(m *Module) {
		if interval <= 0 {
			interval = defaultFlushInterval
		}
		m.flushInterval = interval
	}
}
