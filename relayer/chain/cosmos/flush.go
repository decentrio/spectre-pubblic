package cosmos

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"relayer/chain"
	"relayer/chain/evmflush"
	"relayer/services"
	"relayer/subscriber"

	"github.com/cosmos/gogoproto/proto"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"

	abcitypes "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
)

// flushQueryTimeout bounds one enumeration pass. It is generous because the pass
// runs every few minutes and a paginated commitment query against a busy chain
// is legitimately slow; the point of the bound is that a hung endpoint cannot
// wedge the flush worker, not to enforce a latency target.
const flushQueryTimeout = 60 * time.Second

// flushSequenceCap bounds how many outstanding sequences one pass will resolve
// into packets. Each sequence costs a TxSearch, so an unbounded backlog would
// turn a backstop into a stampede. The remainder is not lost: the cursor makes
// the next pass start where this one stopped.
//
// The cap bounds the COMMITMENT QUERY too, which is the part that used to be
// unbounded: the pass asks for a window of this size starting after the cursor
// rather than paging the whole set and slicing it. See outstandingCommitments.
const flushSequenceCap = 200

// flushTxSearchResults is how many transactions one sequence lookup will scan.
//
// One would do if every node matched a multi-condition query per EVENT, which
// CometBFT's kv indexer does since v0.38 -- its index key carries the event
// sequence, so `packet_sequence=N AND packet_source_client=X` must be satisfied
// by one event. That is a property of the NODE's indexer, not of this query: a
// psql-indexed node, or kv entries written in the legacy format (which record
// event sequence 0 for every attribute), match the two conditions across
// DIFFERENT events of the same transaction. Such a transaction then comes back
// first and carries no matching event, and asking for a single result would let
// it hide the real one. Scanning a few costs nothing on the healthy path, where
// the first result is the answer.
const flushTxSearchResults = 10

// flushTxSearchMaxResults bounds how far one sequence lookup will page.
//
// Scanning one page was not enough. On exactly the nodes the comment above
// describes, the stale cross-matches can FILL a page and leave the live send at
// result 11 or later -- the lookup then reports "not found", the cursor moves
// on, and the next pass asks the identical question and gets the identical
// answer. The packet is outstanding forever while every pass says there is
// nothing to do. Found in review.
//
// Paging to exhaustion is the other wrong answer: TotalCount on a
// cross-matching index is whatever that index chose to conflate, and this
// lookup runs once per outstanding sequence, up to flushSequenceCap of them per
// pass. A cap keeps the pass bounded, which is the property the commitment
// enumeration was just fixed to have. Reaching it is reported rather than
// silent, because "we looked at 100 transactions for one sequence" is a broken
// index and the operator is the only one who can fix it.
const flushTxSearchMaxResults = 100

// UnrelayedPackets enumerates the packets this Cosmos source has sent that are
// still outstanding, so the relay module can find work it never observed.
//
// It implements chain.PacketLister; see that interface for why enumeration
// exists alongside block scanning.
//
// An outstanding packet COMMITMENT is the right signal: Cosmos deletes it when
// the packet is acknowledged or timed out, so its presence means the packet's
// lifecycle has not closed. It does NOT mean the destination never received it
// -- a delivered packet whose ack is still in flight still has a commitment --
// which is why the caller filters on the destination's receipt rather than this
// query pretending to know.
//
// Two legs, both answering "is it still open on the SENDING chain and not yet
// settled on this one":
//
//   - sends: this chain still holds the packet's commitment (above);
//   - acknowledgements: the EVM router still holds a send's commitment AND
//     this chain has written an acknowledgement for it. Enumerating the acks
//     this chain wrote says nothing on its own about whether the counterparty
//     consumed them; the router's commitment is what says that, so the router
//     is probed (evmflush) and this chain is asked only about those sequences.
//
// A leg whose query fails is reported and skipped rather than failing the pass:
// the other leg's packets are exactly what the pass exists to recover.
func (s *Source) UnrelayedPackets(ctx context.Context) ([]chain.Event, error) {
	queryCtx, cancel := context.WithTimeout(ctx, flushQueryTimeout)
	defer cancel()

	sends, err := s.unrelayedSends(queryCtx)
	if err != nil {
		return nil, err
	}
	acks, err := s.unrelayedAcks(queryCtx)
	if err != nil {
		s.logger.Printf("WARN [FlushCosmos] acknowledgement leg: %v; the send leg's %d packet(s) still stand", err, len(sends))
	}
	return append(sends, acks...), nil
}

func (s *Source) unrelayedSends(ctx context.Context) ([]chain.Event, error) {
	window, err := s.outstandingCommitments(ctx)
	if err != nil {
		return nil, err
	}
	if len(window) == 0 {
		return nil, nil
	}
	s.advanceFlushCursor(window)

	events := make([]chain.Event, 0, len(window))
	for _, commitment := range window {
		event, found, err := s.sendEventForCommitment(ctx, commitment)
		if err != nil {
			// One unresolvable sequence must not discard the rest: the others are
			// exactly the packets this pass exists to recover.
			s.logger.Printf("WARN [FlushCosmos] resolve send packet seq=%d: %v", commitment.sequence, err)
			continue
		}
		if !found {
			// The commitment exists but no indexed send_packet tx carries the packet
			// it commits to. On a pruned node that is expected for an old packet, and
			// it is worth saying so once per pass rather than silently returning a
			// shorter list.
			s.logger.Printf("WARN [FlushCosmos] commitment for seq=%d has no indexed send_packet tx matching it (pruned history?)",
				commitment.sequence)
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

// unrelayedAcks lists the acknowledgements this chain has written for EVM sends
// whose commitment the router still holds -- acks the router has not received
// back.
//
// The router's commitments are the enumerable side here, by probe: an EVM->Cosmos
// packet is committed on the router under ITS client for Cosmos, and
// acknowledged here under THIS chain's client for the EVM. Only the sequences
// the probe finds still open are asked about, so the pass costs one batched
// storage read plus one ABCI query per open send.
func (s *Source) unrelayedAcks(ctx context.Context) ([]chain.Event, error) {
	if s.evm.EthClient() == nil || s.evm.RouterContract() == nil {
		return nil, nil
	}
	router := evmflush.Router{Client: s.evm.EthClient(), Address: *s.evm.RouterContract(), Label: "[FlushCosmos]", Logger: s.logger}
	prev, err := router.PrevSequenceSend(ctx, s.ids.CosmosOnEVM)
	if err != nil {
		return nil, err
	}
	lo, hi, ok := evmflush.Window(prev, s.ackProbeCursor, flushSequenceCap)
	if !ok {
		return nil, nil
	}
	open, err := router.OutstandingSlots(ctx, evmflush.Sequences(lo, hi), func(seq uint64) []byte {
		return services.EthPath(s.ids.CosmosOnEVM, seq, services.EthPathCommitment)
	})
	if err != nil {
		return nil, err
	}
	// The cursor moves only once the probe has answered. A failed probe leaves
	// the window to the next pass; advancing first would skip these sequences
	// for a whole rotation on one RPC blip.
	s.ackProbeCursor = lo

	var events []chain.Event
	for _, slot := range open {
		written, err := subscriber.HasCosmosPacketAcknowledgementFor(ctx, s.cosmos, s.ids.EVMOnCosmos, slot.Sequence)
		if err != nil {
			s.logger.Printf("WARN [FlushCosmos] ack seq=%d: %v", slot.Sequence, err)
			continue
		}
		if !written {
			continue // not received here yet, or received and not acknowledged: the other direction's work
		}
		event, found, err := s.ackEventForCommitment(ctx, slot)
		if err != nil {
			s.logger.Printf("WARN [FlushCosmos] resolve ack seq=%d: %v", slot.Sequence, err)
			continue
		}
		if !found {
			s.logger.Printf("WARN [FlushCosmos] acknowledgement for seq=%d has no indexed write_acknowledgement tx matching the router's commitment (pruned history?)",
				slot.Sequence)
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

// flushCommitment is one outstanding commitment: the sequence to resolve, and
// the commitment hash that says WHICH packet that sequence currently names.
//
// The hash is carried rather than dropped because a sequence alone does not
// identify a packet to anything outside the chain's own store. The commitment is
// what the chain will accept a proof against, so it is also the only thing that
// can confirm the transaction this pass finds is the packet still outstanding
// and not an older one that happens to share the sequence.
type flushCommitment struct {
	sequence uint64
	data     []byte
}

// outstandingCommitments returns at most flushSequenceCap commitments, starting
// after the sequence the previous pass stopped at and wrapping to the oldest.
//
// The window query itself lives in services.CosmosCommitmentWindow, because the
// EVM and L2 sources page the same store for the acknowledgements they owe --
// and the reason it asks for a WINDOW rather than paging everything is recorded
// there. What this keeps is the cost of the pass: the cap bounds the query and
// the TxSearch half alike.
//
// What it gives up is the backlog total, which used to be logged. Counting is
// exactly the unbounded work being removed, and a number nobody can act on is
// not worth a thousand queries.
func (s *Source) outstandingCommitments(ctx context.Context) ([]flushCommitment, error) {
	page, err := services.CosmosCommitmentWindow(ctx, s.cosmos, s.commitmentQueryClientID(), s.flushCursor, flushSequenceCap)
	if err != nil {
		return nil, fmt.Errorf("cosmos source: %w", err)
	}
	window := make([]flushCommitment, 0, len(page))
	for _, c := range page {
		window = append(window, flushCommitment{sequence: c.Sequence, data: c.Data})
	}
	if len(window) == flushSequenceCap {
		s.logger.Printf("[FlushCosmos] resolving %d outstanding commitments on %s this pass (seq %d..%d); "+
			"the window is full, so more may remain for the next pass",
			len(window), s.commitmentQueryClientID(), window[0].sequence, window[len(window)-1].sequence)
	}
	return window, nil
}

// sequenceKey is the paginator key for a sequence; see services.SequenceKey.
func sequenceKey(sequence uint64) []byte { return services.SequenceKey(sequence) }

// sendEventForCommitment finds the indexed send_packet transaction for one
// outstanding commitment and rebuilds the event the subscriber would have
// delivered.
//
// It searches by sequence rather than by height because the whole point of the
// enumeration path is that the height is unknown -- if it were known, the block
// scan would already have covered it. What comes back is then checked against
// the commitment, because the sequence is what was searched for and the
// commitment is what says which packet that sequence currently means.
func (s *Source) sendEventForCommitment(ctx context.Context, commitment flushCommitment) (chain.Event, bool, error) {
	q := fmt.Sprintf("send_packet.packet_sequence=%d AND send_packet.packet_source_client='%s'",
		commitment.sequence, s.commitmentQueryClientID())
	return s.findIndexedEvent(ctx, q, commitment.sequence, func(tx *coretypes.ResultTx) (chain.Event, bool) {
		packet, ok := matchSendEvent(tx.TxResult.Events, s.commitmentQueryClientID(), commitment)
		if !ok {
			return chain.Event{}, false
		}
		return chain.Event{
			Type:     chain.SendPacket,
			Height:   uint64(tx.Height),
			Sequence: packet.Sequence,
			ClientID: packet.DestinationClient,
			Raw:      mustMarshalPacket(packet),
		}, true
	})
}

// ackEventForCommitment finds the indexed write_acknowledgement transaction for
// an EVM send the router still holds a commitment for, and rebuilds the event
// the subscriber would have delivered. Same search shape as the send leg, same
// three checks -- sequence, client, and the commitment the ROUTER holds, which
// is the same hash this chain computed when it verified the packet.
func (s *Source) ackEventForCommitment(ctx context.Context, slot evmflush.Slot) (chain.Event, bool, error) {
	destClient := s.ids.EVMOnCosmos
	q := fmt.Sprintf("write_acknowledgement.packet_sequence=%d AND write_acknowledgement.packet_dest_client='%s'",
		slot.Sequence, destClient)
	return s.findIndexedEvent(ctx, q, slot.Sequence, func(tx *coretypes.ResultTx) (chain.Event, bool) {
		packet, acks, ok := matchWriteAckEvent(tx.TxResult.Events, destClient, slot)
		if !ok {
			return chain.Event{}, false
		}
		return chain.Event{
			Type:     chain.AckPacket,
			Height:   uint64(tx.Height),
			Sequence: packet.Sequence,
			ClientID: packet.DestinationClient,
			Raw:      mustMarshalPacket(packet),
			AckBytes: acks,
		}, true
	})
}

// findIndexedEvent pages one TxSearch until match accepts a transaction, with
// the bounds flushTxSearchResults and flushTxSearchMaxResults explain.
func (s *Source) findIndexedEvent(ctx context.Context, q string, sequence uint64, match func(*coretypes.ResultTx) (chain.Event, bool)) (chain.Event, bool, error) {
	perPage := flushTxSearchResults
	scanned := 0
	for page := 1; ; page++ {
		result, err := s.cosmos.CosmosClient().TxSearch(ctx, q, false, &page, &perPage, "asc")
		if err != nil {
			return chain.Event{}, false, fmt.Errorf("tx search page %d: %w", page, err)
		}
		for _, tx := range result.Txs {
			if event, ok := match(tx); ok {
				return event, true, nil
			}
		}
		scanned += len(result.Txs)
		// An empty page ends it regardless of what TotalCount claims: a node that
		// keeps reporting more than it will return must not spin this loop.
		if len(result.Txs) == 0 || scanned >= result.TotalCount {
			return chain.Event{}, false, nil
		}
		if scanned >= flushTxSearchMaxResults {
			log.Printf("WARN [FlushCosmos] sequence %d on %s: scanned %d of %d indexed transactions without "+
				"finding the commitment the chain holds; giving up on this sequence for this pass. "+
				"That many matches for one (sequence, client) pair means the node's transaction "+
				"index is conflating events -- a psql index, or kv entries in the legacy format",
				sequence, s.commitmentQueryClientID(), scanned, result.TotalCount)
			return chain.Event{}, false, nil
		}
	}
}

// matchWriteAckEvent returns the write_acknowledgement event whose packet is the
// one the router's commitment names, with its application acknowledgements.
// The three checks are the send leg's (see matchSendEvent); the commitment here
// is the ROUTER's, read from its storage, and it is the same hash this chain
// computed when it verified the packet's membership proof.
func matchWriteAckEvent(events []abcitypes.Event, destClient string, slot evmflush.Slot) (channeltypesv2.Packet, [][]byte, bool) {
	for _, event := range events {
		if event.Type != "write_acknowledgement" {
			continue
		}
		packet, ok := packetFromSendEvent(event.Attributes) // same encoded_packet_hex attribute
		if !ok || packet.Sequence != slot.Sequence || packet.DestinationClient != destClient {
			continue
		}
		if !bytes.Equal(channeltypesv2.CommitPacket(packet), slot.Value[:]) {
			continue
		}
		acks, ok := acknowledgementsFromEvent(event.Attributes)
		if !ok {
			continue
		}
		return packet, acks, true
	}
	return channeltypesv2.Packet{}, nil, false
}

// acknowledgementsFromEvent decodes the hex-encoded Acknowledgement an ibc-go v2
// write_acknowledgement event carries into its application acknowledgements.
// An empty list is not an acknowledgement the EVM can consume, so it is a miss.
func acknowledgementsFromEvent(attributes []abcitypes.EventAttribute) ([][]byte, bool) {
	for _, attribute := range attributes {
		if attribute.Key != "encoded_acknowledgement_hex" {
			continue
		}
		raw, err := hex.DecodeString(attribute.Value)
		if err != nil {
			return nil, false
		}
		var ack channeltypesv2.Acknowledgement
		if err := proto.Unmarshal(raw, &ack); err != nil {
			return nil, false
		}
		if len(ack.AppAcknowledgements) == 0 {
			return nil, false
		}
		return ack.AppAcknowledgements, true
	}
	return nil, false
}

// matchSendEvent returns the send_packet event whose packet is the one this
// commitment commits to.
//
// Three checks, and each closes a hole the others do not. TxSearch matches
// TRANSACTIONS, and the event list it hands back is the whole transaction's,
// unfiltered -- the query decides which transactions come back, never which
// events within one.
//
//   - SEQUENCE, because a transaction can carry several sends.
//   - SOURCE CLIENT, because sequences are per-client: seq=N exists independently
//     on every client, so a sequence-only match would answer with whichever
//     client's send appeared first in the transaction.
//   - COMMITMENT, because the first two identify a POSITION, not a packet. The
//     chain reuses a position: a client migration keeps a client id and restarts
//     sequences, and the transaction index keeps the old send. Ordering by "asc"
//     then hands back the OLDEST transaction at that position, so the flush would
//     rebuild a packet that closed long ago -- relaying something already settled
//     while the packet actually outstanding stays undiscovered, every pass,
//     identically. Comparing CommitPacket against the commitment the chain holds
//     right now is what distinguishes the two, and it is the same hash the chain
//     would check a proof against.
func matchSendEvent(events []abcitypes.Event, sourceClient string, commitment flushCommitment) (channeltypesv2.Packet, bool) {
	for _, event := range events {
		if event.Type != "send_packet" {
			continue
		}
		packet, ok := packetFromSendEvent(event.Attributes)
		if !ok || packet.Sequence != commitment.sequence || packet.SourceClient != sourceClient {
			continue
		}
		if !bytes.Equal(channeltypesv2.CommitPacket(packet), commitment.data) {
			continue
		}
		return packet, true
	}
	return channeltypesv2.Packet{}, false
}

// packetFromSendEvent decodes the hex-encoded packet an ibc-go v2 send_packet
// event carries. The encoded form is the contract the subscriber already
// consumes; decoding it here keeps one wire format rather than two.
func packetFromSendEvent(attributes []abcitypes.EventAttribute) (channeltypesv2.Packet, bool) {
	for _, attribute := range attributes {
		if attribute.Key != "encoded_packet_hex" {
			continue
		}
		raw, err := hex.DecodeString(attribute.Value)
		if err != nil {
			return channeltypesv2.Packet{}, false
		}
		var packet channeltypesv2.Packet
		if err := proto.Unmarshal(raw, &packet); err != nil {
			return channeltypesv2.Packet{}, false
		}
		return packet, true
	}
	return channeltypesv2.Packet{}, false
}

// mustMarshalPacket re-encodes a packet that was just decoded from the chain's
// own event, so a failure here would mean the packet round-tripped differently
// than it was produced. Returning nil rather than panicking keeps a malformed
// event from taking the process down; handleBatch rejects an empty payload.
func mustMarshalPacket(packet channeltypesv2.Packet) []byte {
	raw, err := proto.Marshal(&packet)
	if err != nil {
		return nil
	}
	return raw
}

var _ chain.PacketLister = (*Source)(nil)

// commitmentQueryClientID names the client a Cosmos send commitment is stored
// under, which is the packet's LOCAL source client.
//
// For a Cosmos->EVM packet that is EVMOnCosmos -- the EVM light client living on
// Cosmos -- and NOT CosmosOnEVM, which is how the EVM router knows Cosmos and is
// the packet's DESTINATION client. Getting it backwards is not a partial
// failure: the query returns an empty set, so the enumeration backstop reports
// nothing outstanding on precisely the path it was built for, and looks healthy
// doing it. It is a named method so the choice is testable rather than repeated
// as a field access at each call site.
func (s *Source) commitmentQueryClientID() string { return s.ids.EVMOnCosmos }

// advanceFlushCursor records where this pass stopped, so the next one starts
// after it.
//
// The rotation is the point. Taking the oldest flushSequenceCap every pass looks
// fair and is not: a prefix that cannot resolve -- pruned history, or packets
// already delivered and waiting on an acknowledgement, which keep their
// commitment -- occupies the whole window forever, and every later packet is
// never even looked at. Rotating means an unresolvable prefix costs one pass,
// not all of them.
//
// A set that fits inside one window ends every pass at the same last sequence,
// so the cursor parks there and the wrap hands the whole set back next time.
// That is the intended behaviour for a healthy chain, not a stalled cursor.
func (s *Source) advanceFlushCursor(window []flushCommitment) {
	if len(window) == 0 {
		return
	}
	s.flushCursor = window[len(window)-1].sequence
}
