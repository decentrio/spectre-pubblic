package l2rollup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"relayer/chain"
	"relayer/chain/evmflush"
	relayerclient "relayer/client"
	"relayer/services"
)

// flushState is the enumeration backstop's per-source state: one rotating
// cursor per leg and the log-range policy the lookups narrow in place.
type flushState struct {
	sendCursor uint64 // lowest sequence the last send probe reached; 0 = newest first
	ackCursor  uint64 // last Cosmos commitment sequence the ack leg took
	span       relayerclient.LogSpan
	// seeded records that span took its starting chunk from logScanChunk. The
	// scan loop seeds its own LogSpan the same way; both narrow from there.
	seeded bool
}

// flushQueryTimeout bounds one enumeration pass.
const flushQueryTimeout = 60 * time.Second

// flushSendBudget bounds the send leg inside one pass. Sends run first and
// share the pass's flushQueryTimeout with acknowledgements; a backlog of old
// sends -- each a walk of up to evmflush.MaxLogChunks log queries -- would
// otherwise spend the whole pass and leave the ack leg nothing, pass after
// pass. The ack leg keeps whatever the send leg leaves, at least
// flushQueryTimeout-flushSendBudget. A var only so tests can shorten it.
var flushSendBudget = 40 * time.Second

// cosmosCommitmentLister is the optional half of CosmosPacketState the
// acknowledgement leg needs: page the Cosmos commitment store for the client
// this L2 is paired with. The reconciliation seam asks point questions; this is
// the only enumeration it needs, and a test double that does not implement it
// simply has no acknowledgements to list.
type cosmosCommitmentLister interface {
	CommitmentWindow(ctx context.Context, clientID string, cursor uint64, limit int) ([]services.PacketCommitment, error)
}

func (s cosmosEndpointState) CommitmentWindow(ctx context.Context, clientID string, cursor uint64, limit int) ([]services.PacketCommitment, error) {
	return services.CosmosCommitmentWindow(ctx, s.endpoint, clientID, cursor, limit)
}

// UnrelayedPackets enumerates what this L2 source still owes the Cosmos side.
// It implements chain.PacketLister; the two legs and their bounds are the ones
// the ETH source describes, against the L2's router:
//
//   - sends: the L2 router still holds a commitment under this source's client;
//   - acknowledgements: Cosmos still holds a send's commitment under the paired
//     wasm client AND the L2 router has written an acknowledgement for it.
//
// This is the leg that needs the backstop most: the L2 scan window is the
// shortest of the four paths (#308), and an acknowledgement written on the L2
// while the relayer was down was, before this, gone for good.
func (s *Source) UnrelayedPackets(ctx context.Context) ([]chain.Event, error) {
	if s.eth == nil {
		return nil, fmt.Errorf("l2 source: enumeration needs an L2 client")
	}
	if !s.flush.seeded {
		s.flush.span.Chunk = s.logScanChunk
		s.flush.seeded = true
	}
	queryCtx, cancel := context.WithTimeout(ctx, flushQueryTimeout)
	defer cancel()

	router := evmflush.Router{
		Client: s.eth, Address: s.router, Span: &s.flush.span, DeployBlock: s.routerDeployBlock, Label: "[l2->cosmos Flush]",
	}
	sendCtx, cancelSends := context.WithTimeout(queryCtx, flushSendBudget)
	sends, err := s.unrelayedSends(sendCtx, router)
	// Read before cancelSends, which would turn every Err into Canceled.
	sendBudgetSpent := errors.Is(sendCtx.Err(), context.DeadlineExceeded) && queryCtx.Err() == nil
	cancelSends()
	if err != nil {
		// Only the send leg's own budget ran out -- its probe was slow, and it
		// has left the cursor where it was -- so the ack leg still has its time.
		// Any other failure fails the pass, as before.
		if !sendBudgetSpent {
			return nil, err
		}
		log.Printf("WARN [l2->cosmos Flush] send leg out of time before its probe answered; the cursor stays and the ack leg runs: %v", err)
		sends = nil
	}
	acks, err := s.unrelayedAcks(queryCtx, router)
	if err != nil {
		log.Printf("WARN [l2->cosmos Flush] acknowledgement leg: %v; the send leg's %d packet(s) still stand", err, len(sends))
	}
	return append(sends, acks...), nil
}

func (s *Source) unrelayedSends(ctx context.Context, router evmflush.Router) ([]chain.Event, error) {
	prev, err := router.PrevSequenceSend(ctx, s.l2ClientID)
	if err != nil {
		return nil, err
	}
	lo, hi, ok := evmflush.Window(prev, s.flush.sendCursor, evmflush.ProbeCap)
	if !ok {
		return nil, nil
	}
	slots, err := router.OutstandingSlots(ctx, evmflush.Sequences(lo, hi), func(seq uint64) []byte {
		return services.EthPath(s.l2ClientID, seq, services.EthPathCommitment)
	})
	if err != nil {
		return nil, err
	}
	// The cursor moves only once the probe has answered -- a failed probe leaves
	// the window to the next pass -- and only as far as the lookups got: a pass
	// that runs out of budget resumes at the first send it did not resolve.
	stop := len(slots)
	hold := -1 // a send the pass refused to handle (it could not be tracked), or -1
	var events []chain.Event
	for i, slot := range slots {
		ev, found, err := router.SendLog(ctx, s.l2ClientID, slot.Sequence, slot.Value)
		if err != nil {
			if ctx.Err() != nil {
				stop = i
				log.Printf("WARN [l2->cosmos Flush] send leg out of time at seq=%d with %d of %d send(s) resolved; the next pass resumes there: %v",
					slot.Sequence, i, len(slots), err)
				break
			}
			log.Printf("WARN [l2->cosmos Flush] resolve send seq=%d: %v", slot.Sequence, err)
			continue
		}
		if !found {
			continue
		}
		// l2SendToEvent applies the same destination-client filter the scan does.
		e, ok := l2SendToEvent(ev, s.cosmosWasmClientID)
		if !ok {
			continue
		}
		// Past its timeout it can never be received on Cosmos; the L2 timeout
		// scanner refunds it. The same rule MembershipProof applies, applied here
		// so an old expired send does not cost a proof build per pass -- and so
		// this leg matches the ETH source's.
		//
		// Skipping it must not hide it from that scanner, which reads only the
		// tracker: a send the block scan missed was never registered, so the
		// module's own track hook (which only sees events returned below) cannot
		// have. Register it here. A failed durable write stops the pass at this
		// send, so the cursor does not move past a packet nobody is watching.
		if ev.Packet.TimeoutTimestamp > 0 && uint64(time.Now().Unix()) >= ev.Packet.TimeoutTimestamp {
			if s.track != nil && !s.track(e.Raw, e.Height) {
				hold = i
				log.Printf("WARN [l2->cosmos Flush] track expired send seq=%d from block %d: durable write failed; the next pass resumes there",
					slot.Sequence, e.Height)
				break
			}
			continue
		}
		events = append(events, e)
	}
	if hold >= 0 {
		// Unlike a lookup that ran out of time, this send must be retried even when
		// it is the first: SendCursor would step past it.
		s.flush.sendCursor = evmflush.SendCursorAt(slots, hold)
	} else {
		s.flush.sendCursor = evmflush.SendCursor(lo, slots, stop)
	}
	return events, nil
}

func (s *Source) unrelayedAcks(ctx context.Context, router evmflush.Router) ([]chain.Event, error) {
	lister, ok := s.cosmosState.(cosmosCommitmentLister)
	if !ok || s.cosmosWasmClientID == "" {
		return nil, nil
	}
	// Cosmos->L2 packets are committed on Cosmos under the paired wasm client,
	// and acknowledged on the L2 under this router's client.
	window, err := lister.CommitmentWindow(ctx, s.cosmosWasmClientID, s.flush.ackCursor, evmflush.ProbeCap)
	if err != nil {
		return nil, fmt.Errorf("cosmos commitments: %w", err)
	}
	if len(window) == 0 {
		return nil, nil
	}
	committed := make(map[uint64][]byte, len(window))
	sequences := make([]uint64, 0, len(window))
	for _, c := range window {
		committed[c.Sequence] = c.Data
		sequences = append(sequences, c.Sequence)
	}
	written, err := router.OutstandingSlots(ctx, sequences, func(seq uint64) []byte {
		return services.EthPath(s.l2ClientID, seq, services.EthPathAck)
	})
	if err != nil {
		return nil, err
	}
	// The cursor moves only once the probe has answered -- a failed probe leaves
	// the window to the next pass -- and only as far as the lookups got: a pass
	// that runs out of budget resumes at the first ack it did not resolve.
	stop := len(written)
	var events []chain.Event
	for i, slot := range written {
		ev, found, err := router.WriteAckLog(ctx, s.l2ClientID, slot.Sequence, committed[slot.Sequence])
		if err != nil {
			if ctx.Err() != nil {
				stop = i
				log.Printf("WARN [l2->cosmos Flush] ack leg out of time at seq=%d with %d of %d ack(s) resolved; the next pass resumes there: %v",
					slot.Sequence, i, len(written), err)
				break
			}
			log.Printf("WARN [l2->cosmos Flush] resolve ack seq=%d: %v", slot.Sequence, err)
			continue
		}
		if !found {
			continue
		}
		if e, ok := l2AckToEvent(ev, s.cosmosWasmClientID); ok {
			events = append(events, e)
		}
	}
	s.flush.ackCursor = evmflush.AckCursor(sequences, written, stop)
	return events, nil
}

var _ chain.PacketLister = (*Source)(nil)
