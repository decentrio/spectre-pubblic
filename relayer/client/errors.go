package client

import "errors"

// ErrPacketAlreadyReceived marks the proof result that is evidence about a
// packet rather than the endpoint: the timeout receipt slot is non-empty, so
// the destination already received it and no timeout can be valid.
var ErrPacketAlreadyReceived = errors.New("packet already received on the destination")

// ErrCommitmentAbsent is the mirror of ErrPacketAlreadyReceived, and the answer
// to what a missing commitment means: the membership slot is EMPTY at the height
// asked about, so no membership proof exists to build.
//
// It is TRANSIENT, deliberately. Two things produce it, and neither is fixed by
// dropping the packet:
//
//   - the proof height is behind the packet. Packets are proven at the height the
//     DESTINATION's client trusts, not the height the event was seen at, so a
//     client that lags the event proves against a state where the commitment does
//     not exist yet. It appears as the client advances (#443 is this, one layer up).
//   - the packet was already settled on the source, in which case the terminal
//     event listeners and the on-chain reconciliation remove it from the queue,
//     and the timeout scanner refunds anything they miss.
//
// Treating it as permanent would drop a packet that is merely early. The cost of
// treating it as transient is bounded because it is caught BEFORE a transaction:
// what used to happen was a proof of zero submitted to the chain, one wasted
// transaction per attempt, and a revert that reads as permanent -- dropping a
// valid packet after paying to do so.
var ErrCommitmentAbsent = errors.New("commitment absent at the proof height")
