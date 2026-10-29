package chain

import "errors"

// ErrRetryable marks a transient relay failure: the event is still valid but
// could not be relayed right now — an RPC blip, the source state not yet
// provable (Cosmos AppHash lag, ETH finality behind the packet block), or a
// destination not yet caught up. The relay module re-queues events whose handler
// returns a Retryable error so a brief infrastructure hiccup never permanently
// drops a valid packet (the re-queue behavior of the legacy handlers, expressed
// generically).
//
// Only an explicitly Permanent-tagged error (see ErrPermanent) is dropped;
// everything else — a Retryable error OR a plain untagged error — is re-queued,
// so the default on an unclassified failure is never to lose a valid packet. A
// dropped (permanent) packet that later expires is still refunded by the timeout
// scanner, so funds stay safe.
var ErrRetryable = errors.New("retryable relay failure")

// IsRetryable reports whether err (or anything it wraps) is an ErrRetryable.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrRetryable)
}

// ErrPermanent marks a relay failure as deterministic: retrying cannot succeed
// because the packet is dead, not the infrastructure. Two cases:
//   - the packet has TIMED OUT (its receive can never be accepted; the async
//     timeout scanner refunds it on the source chain instead), or
//   - an on-chain message reverted deterministically (status=0 revert).
//
// The relay module DROPS an event whose handler returns a Permanent error rather
// than re-queueing it, so a dead packet cannot spin forever — each retry re-runs a
// full (expensive) client update, so an unbounded retry drains the signer's gas.
// Funds are still safe: a timed-out packet is refunded by the timeout scanner,
// which is fed independently of this drop.
var ErrPermanent = errors.New("permanent relay failure")

// IsPermanent reports whether err (or anything it wraps) is an ErrPermanent.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrPermanent)
}
