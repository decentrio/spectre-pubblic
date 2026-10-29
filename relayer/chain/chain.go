// Package chain defines the adapter contract that lets one relayer process
// serve many source/destination chains (Cosmos, and EVM L2s such as Arbitrum,
// Optimism, Base) without the core knowing any chain's specifics.
//
// A relay path is a Source (subscribe to events, report heights, build packet
// proofs), a Destination (submit client updates and packets, report client expiry)
// and a Builder (turn a source header into a client update).
//
// Method names follow IBC / relayer convention (UpdateClient, RelayPackets,
// HasPacketReceipt, QueryHeader, LatestHeight) so the contract reads the same
// as any Cosmos relayer.
//
// Payload types are intentionally opaque ([]byte / small structs) so each adapter
// owns its own encoding. Adapters must not leak chain-specific types across this
// boundary — that is the whole point.
package chain

import (
	"context"
	"fmt"
	"time"
)

// ChainType identifies a chain family so the registry can pick an adapter.
// Instances of the same family differ only by config (chain id, RPC, contracts).
type ChainType string

const (
	Cosmos   ChainType = "cosmos"
	Ethereum ChainType = "ethereum" // L1
	OPStack  ChainType = "opstack"  // Optimism + Base (same stack; differ by config/binary)
	Arbitrum ChainType = "arbitrum" // Nitro
)

// EventType classifies a relayable event by its IBC packet-lifecycle message,
// so the core can route it without decoding chain-specific payloads.
type EventType int

const (
	SendPacket EventType = iota
	AckPacket
	TimeoutPacket
)

// String names the event by the message it becomes on the destination, so logs
// distinguish a forward delivery from a returning acknowledgement without the
// reader decoding an integer.
func (t EventType) String() string {
	switch t {
	case SendPacket:
		return "recv"
	case AckPacket:
		return "ack"
	case TimeoutPacket:
		return "timeout"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// Event is a relayable packet observed on a source chain. Raw carries the
// adapter-encoded packet; the core only reads the routing fields.
type Event struct {
	Type   EventType
	Height uint64 // source height the event was observed at (gated by LatestHeight)
	// Sequence is the packet's IBC sequence, duplicated out of Raw purely so the
	// core can name a packet in logs. Decoding Raw for that would put a proto
	// dependency (and adapter-specific knowledge) into the generic relay loop,
	// which is the thing this package exists to avoid — so the adapters, which
	// already hold the decoded packet, copy it in.
	Sequence uint64
	ClientID string   // destination client id this event routes to (event-stream partition key)
	Raw      []byte   // proto-marshaled channeltypesv2.Packet
	AckBytes [][]byte // app acknowledgements, populated only for AckPacket events
}

// ClientUpdate is a light-client update for the destination's client-of-source
// (the payload of an IBC MsgUpdateClient).
//
// Height is exposed so the core can enforce the append-only / monotonic
// invariant generically — reject an update at or below the destination client's
// current height before submitting, which catches reorg-attestations and
// replays without decoding Payloads. (Destination.UpdateClient must still reject
// non-monotonic updates itself; this is defense in depth at the core layer.)
//
// Payloads are adapter-owned, destination-specific update bytes. Most adapters
// emit exactly one payload (a Groth16 proof bundle for Cosmos->X or an
// attestor-signed JSON ClientMessage for L2->Cosmos); beacon updates may need an
// ordered sequence when crossing sync-committee periods. Only the destination
// adapter understands their contents. An empty list means the client is already
// current and no transaction is needed.
type ClientUpdate struct {
	Height    uint64
	Payloads  [][]byte
	TrustedAt time.Time

	// IsHop means this update is an INTERMEDIATE step, not the target: the
	// builder could not reach the requested height in one update and picked a
	// height in between. The caller must ask again from the height this installs.
	//
	// It exists because validator churn can outrun the trust-overlap requirement.
	// When too much of the set has rotated, an update from the trusted height to
	// the live head cannot be verified at all, and the only way across is a chain
	// of updates each of which overlaps the last. Only the builder knows it took
	// that route, so only the builder can say so.
	//
	// Adapters that never hop leave it false, which is the whole of their
	// participation: the engine then does exactly what it did before.
	IsHop bool
}

// MaxClientUpdateHops bounds a chain of intermediate client updates. Each hop
// costs a proof and a transaction, so an unbounded climb is an expensive silent
// loop dressed as progress.
//
// It lives beside IsHop, the field that drives the climb, because every climber
// must agree on it: the relay engine's packet and refresh paths, the periodic
// pinned-set refresh in services, and `update-client --max-hops`. Two constants
// would eventually be two different ceilings, and the operator would have no way
// to tell which one their client stopped at.
const MaxClientUpdateHops = 16

// RelayPacket is a source packet together with the source proof the destination
// needs to submit it. The destination adapter turns it into the concrete IBC
// message (MsgRecvPacket / MsgAcknowledgement / MsgTimeout) and submits it.
type RelayPacket struct {
	Type     EventType // recv (SendPacket), ack, or timeout
	Sequence uint64    // carried from the source Event, for logging and wait bookkeeping
	Packet   []byte    // proto-marshaled channeltypesv2.Packet
	Proof    []byte    // from Source.MembershipProof / NonMembershipProof
	Height   uint64    // the source height the proof is against
	AckBytes [][]byte  // app acknowledgements, for AckPacket only
}

// Source is a chain whose state we prove to a destination.
type Source interface {
	Chain() ChainType

	// Subscribe streams relayable events to handler in BATCHES until ctx is
	// cancelled. It must carry the existing bidirectional gap-recovery guarantees
	// (startup lookback, periodic ticker, reconnect-gap) — reused from the generic
	// subscriber, not reimplemented per chain.
	//
	// CALL ONCE PER PROCESS LIFETIME. Subscribe owns and drains its subscriber and
	// batch-handoff workers before returning; ctx cancellation must interrupt any
	// reconnect wait, live watch, recovery scan, or RPC in flight.
	//
	// Batching (not per-event) is deliberate: it lets the destination fold N
	// packet messages into one multicall (amortizing the ~21k per-tx intrinsic gas
	// and one client-update over the batch) and lets the module perform batch-level
	// waits (source-provability / finality) once instead of per packet.
	//
	// handler returns the indices (into the passed slice) of events that failed
	// TRANSIENTLY and must be re-queued for retry; Subscribe re-queues exactly
	// those. An empty/nil return means every event was handled or permanently
	// dropped (the handler logs permanents). Indices are always valid positions in
	// the slice handler was given. The ctx passed to handler is Subscribe's ctx, so
	// the handler can abort in-flight work (proofs, reads) on shutdown.
	Subscribe(ctx context.Context, handler func(context.Context, []Event) []int) error

	// LatestHeight returns the highest source height that is safe to relay right
	// now. THIS IS THE FINALITY / CONFIRMATION POLICY HOOK — "latest" means the
	// latest height this adapter is willing to prove:
	//   - Cosmos (BFT instant finality): the latest committed height.
	//   - EVM L2, full-finality mode:    the highest L1-finalized L2 height.
	//   - EVM L2, "skip finality" mode:   the latest unsafe/soft head (reorg-exposed).
	// Changing the trust/latency tradeoff = changing only this method.
	LatestHeight(ctx context.Context) (uint64, error)

	// RelayableHeight returns the highest source height whose packets can be PROVEN
	// AND relayed right now. It is the CHEAP precondition the relay module checks
	// before doing any expensive client-update / proof work: a packet above this
	// height is re-queued without burning a proof, instead of proving repeatedly
	// while its source state is not yet available. It encodes each chain's
	// state-availability lag:
	//   - Cosmos: latest committed height minus the AppHash lag — a commitment
	//     written at H is only reflected in the queryable app state at H+2, so
	//     latest-2.
	//   - Ethereum L1: the execution block number of the latest FINALIZED beacon
	//     header — the beacon light client only proves against finalized state.
	// It is always <= LatestHeight.
	RelayableHeight(ctx context.Context) (uint64, error)

	// QueryHeader returns the adapter-owned data a Builder needs to
	// build the update for source state at the given height.
	QueryHeader(ctx context.Context, height uint64) ([]byte, error)

	// MembershipProof proves the packet's commitment EXISTS in the source state at
	// height — for recv/ack. eventType (SendPacket vs AckPacket) selects which
	// membership path is proven (commitment vs ack), which sit at different
	// ICS-24 paths. NonMembershipProof proves the packet receipt is ABSENT — for
	// timeout (a single unambiguous path). Both mirror the existing
	// membershipFn / nonMembershipFn used by the packet relay. packet is the
	// proto-marshaled channeltypesv2.Packet.
	MembershipProof(ctx context.Context, packet []byte, height uint64, eventType EventType) ([]byte, error)
	NonMembershipProof(ctx context.Context, packet []byte, height uint64) ([]byte, error)
}

// Destination is a chain that hosts the light client of some source and where
// we submit updates + packet messages.
type Destination interface {
	Chain() ChainType

	// UpdateClient advances the destination's light-client-of-source. Must be
	// append-only safe: reject a non-monotonic / non-contiguous update rather
	// than corrupt client state.
	UpdateClient(ctx context.Context, clientID string, update ClientUpdate) error

	// RelayPackets builds and submits the concrete IBC packet messages
	// (recv/ack/timeout) from the given source packets + proofs.
	//
	// committed is how many packets, counted from the FRONT of the slice, are on
	// chain when this returns. It is the answer to what a partial failure means:
	// a bare error leaves the caller unable to tell whether to re-queue five
	// packets or two, and re-queueing the ones already delivered submits them
	// again. Returning the count rather than wrapping it in an error type is
	// deliberate -- a caller cannot forget to unwrap what the signature makes it
	// take.
	//
	// Three rules the caller depends on, and every adapter must keep:
	//   - committed is a PREFIX count. Adapters that may commit only part of a
	//     batch must submit in slice order, so packets[:committed] is exactly the
	//     delivered set.
	//   - on a nil error, committed == len(packets).
	//   - an adapter that submits atomically returns 0 with any error. That is
	//     information, not a default: it says nothing was delivered.
	RelayPackets(ctx context.Context, packets []RelayPacket) (committed int, err error)

	// HasPacketReceipt reports whether a packet was already delivered on this
	// chain, so timeout scanners can skip already-settled packets (the #232
	// pre-check, generalized). Prevents advancing state on already-resolved
	// packets.
	//
	// A non-nil error means "DO NOT KNOW", never "not delivered". The caller must
	// stop rather than fall back to the false answer: reading an RPC failure as
	// "no receipt" is the open path to proving a timeout for a packet that was in
	// fact delivered, which refunds the sender for a transfer the receiver got.
	// An adapter that cannot answer must error rather than return false.
	HasPacketReceipt(ctx context.Context, packet []byte) (bool, error)

	// ClientExpiresAt returns when the light-client-of-source on this chain will
	// expire, so the generic refresh routine can schedule updates ahead of it. A
	// zero time means "no expiry" (e.g. a permissioned client with no trusting
	// period).
	//
	// It also returns the trusting period that expiry was derived from. The
	// caller needs it to size its safety margin: a margin fixed in absolute time
	// is either wasteful against a period measured in days or unreachable against
	// one measured in minutes, and every implementation already has the number --
	// it computes the expiry from it. A zero period means "unknown", and the
	// caller falls back to its own default.
	ClientExpiresAt(ctx context.Context, clientID string) (expiresAt time.Time, trustingPeriod time.Duration, err error)
}

// PacketLister is an OPTIONAL Source capability: enumerate packets this source
// has sent that are still outstanding, WITHOUT being told which packets to look
// for.
//
// Every other query in this package is a point lookup by path -- MembershipProof
// takes a packet, HasPacketReceipt takes a packet. They can only answer questions
// about packets the relayer already knows about, which means the relayer cannot
// find what it never saw. Three situations produce exactly that:
//
//   - the persisted cursor is lost or corrupt and the downtime exceeded the
//     startup window, so the rescan starts above the missing packet;
//   - a SECOND relayer is stood up on a path that is already running -- it has no
//     cursor at all, scans one window, and every older packet is invisible to it;
//   - a packet has been in flight longer than the window and nobody relayed it.
//
// The second is not hypothetical: this design deliberately does not assume it is
// the only relayer on a path, and production grants the relay role to several
// independent addresses.
//
// Block scanning and enumeration catch different things -- cosmos/relayer, which
// runs both, documents that its packet queries "can miss things that the block
// scanning performed during standard operation wouldn't". The goal is both, not
// a choice between them.
//
// Cost is asymmetric and that shapes the implementations: Cosmos can enumerate
// commitments in one paginated query, while EVM storage is a mapping whose keys
// cannot be listed, so an EVM implementation must bound a probe with
// nextSequenceSend rather than sweeping 1..N.
//
// Returning candidates is enough: the caller filters what the destination has
// already settled. An implementation must NOT assume it is called on any
// particular cadence.
type PacketLister interface {
	UnrelayedPackets(ctx context.Context) ([]Event, error)
}

// SettlementChecker is an OPTIONAL Destination capability: for an event the
// enumeration backstop found, report whether this destination has already
// settled it, asking the question that matches the event's TYPE.
//
// HasPacketReceipt answers only for a receive: a receipt is what a delivered
// SendPacket leaves behind. It is the wrong question for an acknowledgement,
// whose settled form on the destination is the ABSENCE of the original send's
// commitment -- the destination deletes it when the ack lands -- and a receipt
// check on an ack's packet asks about a receipt that lives on the other chain,
// so it answers "not delivered" forever and the ack is re-submitted every pass.
//
//   - SendPacket    settled ⇔ this destination holds the packet's receipt
//   - AckPacket     settled ⇔ this destination no longer holds the send's commitment
//   - TimeoutPacket settled ⇔ same as AckPacket (a timeout also clears it)
//
// A destination that does not implement it leaves the module with the receipt
// check for receives and no filter for the rest.
type SettlementChecker interface {
	PacketSettled(ctx context.Context, e Event) (bool, error)
}

// ClientStatusChecker is an OPTIONAL Destination capability: report whether the
// destination's light-client-of-source accepts messages at all.
//
// A frozen client rejects every packet for a reason that is not the packet's,
// and the same packet succeeds after the client is unfrozen or migrated. The
// relay module asks this at startup, before it treats any deterministic
// rejection as a dead packet, and while it holds a direction on an inactive
// client -- so a freeze stops the path instead of draining it.
//
// status names the state for logs ("Active", "Frozen", "Expired"). A non-nil
// error means "do not know"; callers must not read it as either answer.
type ClientStatusChecker interface {
	ClientActive(ctx context.Context) (active bool, status string, err error)
}

// FoldingDestination is an optional destination capability for submitting a
// client update followed by packet messages in one atomic transaction. The
// relay module uses it only when SupportsUpdatePacketFolding returns true;
// otherwise it retains Destination's update-then-packets fallback.
//
// update.Payloads is non-empty when RelayWithUpdate is called. Implementations
// must apply the update before the packet messages and revert the whole
// transaction if any message fails.
type FoldingDestination interface {
	SupportsUpdatePacketFolding() bool
	RelayWithUpdate(ctx context.Context, clientID string, update ClientUpdate, packets []RelayPacket) error
}

// Builder turns a source header into a destination-verifiable
// ClientUpdate. The strategy is chosen by what the DESTINATION's light client
// verifies, not one-size-fits-all:
//   - "groth16":     batched Ed25519 ZK proof — the destination runs a ZK
//     Tendermint client (SpectreClient). Cosmos -> any EVM. Relayer generates
//     the proof.
//   - "beacon":      assemble the Ethereum sync-committee + storage/account
//     proof update — the destination runs the 08-wasm beacon light client.
//     ETH L1 -> Cosmos. NO relayer-side proving or re-execution; the wasm
//     client verifies BLS + Merkle itself. This is the existing eth_to_cosmos
//     path.
//   - "l2-op" / "l2-base" / "l2-arbitrum": assemble an attestor-signed header —
//     a threshold of independent attestors sign the canonical L2 header + a
//     router account proof (ed25519), as a JSON ClientMessage. L2 -> Cosmos.
//     The L2 wasm client verifies the attestor signature quorum against its
//     stored attestor public-key set, then the router account proof against the
//     header's state root; it does not verify any L1 rollup-contract witness or
//     defer to the shared ETH client. Source.HeadKind selects the requested
//     latency tier; the request is carried to the builder, and each head_kind
//     uses disjoint attestor keys so selecting an RPC tag alone cannot
//     manufacture Safe or Finalized evidence.
//
// Implemented by cosmos.Groth16Builder, evm.BeaconBuilder and
// l2rollup.AttestedBuilder -- listed so the interface greps to its
// implementations.
type Builder interface {
	Name() string
	Build(ctx context.Context, header []byte) (ClientUpdate, error)
}

// RotatingBuilder is implemented by a Builder whose destination client expires
// from a snapshot that only a heavier update refreshes. BuildRotation is Build
// with that refresh forced, and the engine asks for it on the expiry-driven
// refresh alone.
//
// Implemented by cosmos.Groth16Builder: SpectreClient measures its trusting
// period from the pinned validator-set snapshot, and only updateConsensusState
// moves it. A plain Build takes updateApplicationState whenever the set is
// unchanged or still overlaps enough, so an anti-expiry refresh built that way
// lands and leaves the expiry exactly where it was (#541). Builders without
// such a snapshot (beacon, attested L2) do not implement it, and the refresh
// uses Build.
type RotatingBuilder interface {
	Builder
	BuildRotation(ctx context.Context, header []byte) (ClientUpdate, error)
}
