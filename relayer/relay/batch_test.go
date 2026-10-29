package relay

import (
	"context"
	"errors"
	"fmt"
	"relayer/chain"
	"relayer/logbridge"
	"relayer/services"
	"strings"
	"sync"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func TestHandleBatch(t *testing.T) {
	// TestHandleBatch/waiting log names the packet: the source-gate wait must identify
	// the packet. A bare count cannot tell an operator whether the ack they are
	// looking for is in the queue at all.
	t.Run("waiting log names the packet", func(t *testing.T) {
		src := &mockSource{latest: 100, relayable: 50}
		m := NewModule("arbitrum->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})

		out := captureLog(func() {
			m.handleBatch(context.Background(), []chain.Event{
				{Type: chain.AckPacket, Sequence: 7, Height: 80, Raw: []byte("ack"), AckBytes: [][]byte{{1}}},
			})
		})

		for _, want := range []string{"waiting:", "ack seq=7", "h=80", "source relayable height=50"} {
			if !strings.Contains(out, want) {
				t.Fatalf("waiting log missing %q:\n%s", want, out)
			}
		}
	})

	// TestHandleBatch/age grows at destination gate is the regression guard for the
	// subtle half of this change. A packet that clears the SOURCE gate every flush
	// but parks at the DESTINATION-coverage gate is the exact shape of a stuck L2
	// ack. Clearing the wait entry when it passes the source gate would reset its age
	// to zero on every flush, so the log would report a permanently-stuck packet as
	// freshly arrived — hiding the very condition the age exists to reveal.
	t.Run("age grows at destination gate", func(t *testing.T) {
		src := &mockSource{latest: 100}
		// The builder can only ever advance the client to 50, so a packet at 100
		// clears RelayableHeight but never the destination-coverage check.
		m := NewModule("arbitrum->cosmos", "client-0", src, &mockDest{}, &mockBuilder{forceHeight: 50})

		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		m.waits.now = clk.now

		event := chain.Event{Type: chain.AckPacket, Sequence: 7, Height: 100, Raw: []byte("ack"), AckBytes: [][]byte{{1}}}
		batch := []chain.Event{event}

		m.handleBatch(context.Background(), batch) // first sighting: age 0
		clk.t = clk.t.Add(10 * time.Minute)

		out := captureLog(func() { m.handleBatch(context.Background(), batch) })

		if !strings.Contains(out, "waiting 10m0s") {
			t.Fatalf("age did not accumulate across flushes at the destination gate:\n%s", out)
		}
		if !strings.Contains(out, "ack seq=7") {
			t.Fatalf("destination-gate log did not name the packet:\n%s", out)
		}
	})

	// TestHandleBatch/delivery clears the age: once a packet lands, a later packet
	// reusing its sequence must not inherit the old age. Sequences do restart —
	// create-clients-eth repoints an existing router client with migrateClient, so
	// the source begins again at sequence 1 under the SAME client id.
	//
	// Two independent properties keep that honest, and this asserts both:
	//   - a delivered packet clears its own entry (settleDelivered), and
	//   - a sequence reused at a different source height starts its own clock rather
	//     than inheriting the retired packet's age (see TestWaiting_ReusedSequenceRestartsTheAge).
	t.Run("delivery clears the age", func(t *testing.T) {
		src := &mockSource{latest: 100, relayable: 50}
		m := NewModule("arbitrum->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})

		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		m.waits.now = clk.now

		// Generation 1, seq 7 at height 80: above the relayable frontier, so it parks.
		old := chain.Event{Type: chain.AckPacket, Sequence: 7, Height: 80, Raw: []byte("ack-old"), AckBytes: [][]byte{{1}}}
		m.handleBatch(context.Background(), []chain.Event{old})
		clk.t = clk.t.Add(30 * time.Minute)

		oldKey := waitKey{typ: chain.AckPacket, seq: 7}
		if _, ok := m.waits.entries[oldKey]; !ok {
			t.Fatal("the parked packet should have a wait entry to inherit from")
		}

		// Generation 2 after a client re-creation: the same sequence, but emitted at a
		// height of its own, and low enough to relay immediately.
		fresh := chain.Event{Type: chain.AckPacket, Sequence: 7, Height: 40, Raw: []byte("ack-new"), AckBytes: [][]byte{{1}}}
		m.handleBatch(context.Background(), []chain.Event{fresh})

		if _, ok := m.waits.entries[oldKey]; ok {
			t.Fatal("a delivered packet must not leave a wait entry behind")
		}
	})

	t.Run("proof by type", func(t *testing.T) {
		src := &mockSource{latest: 100} // chain tip well above the packet heights
		dst := &mockDest{}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		// recv/ack packets prove membership (commitment exists)
		if rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 5, Raw: []byte("pkt-a")}}); len(rq) != 0 {
			t.Fatalf("clean send must not re-queue, got %v", rq)
		}
		if src.membershipCalls != 1 || src.nonMembershipCalls != 0 {
			t.Fatalf("send packet must use membership proof: m=%d nm=%d", src.membershipCalls, src.nonMembershipCalls)
		}
		if len(dst.relayed) != 1 || string(dst.relayed[0].Proof) != "membership" {
			t.Fatalf("relayed packet must carry membership proof, got %+v", dst.relayed)
		}

		// timeouts prove non-membership (receipt absent)
		if rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.TimeoutPacket, Height: 6, Raw: []byte("pkt-b")}}); len(rq) != 0 {
			t.Fatalf("clean timeout must not re-queue, got %v", rq)
		}
		if src.nonMembershipCalls != 1 {
			t.Fatalf("timeout must use non-membership proof: nm=%d", src.nonMembershipCalls)
		}
		if len(dst.relayed) != 2 || string(dst.relayed[1].Proof) != "non-membership" {
			t.Fatalf("timeout relayed packet must carry non-membership proof, got %+v", dst.relayed)
		}
	})

	// TestHandleBatch/multicall verifies a multi-event batch folds into ONE
	// RelayPackets call (the multicall that amortizes per-tx intrinsic gas).
	t.Run("multicall", func(t *testing.T) {
		src := &mockSource{latest: 100} // chain tip well above the packet heights
		dst := &mockDest{}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("a")},
			{Type: chain.SendPacket, Height: 9, Raw: []byte("b")},
			{Type: chain.AckPacket, Height: 8, Raw: []byte("c")},
		}
		if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
			t.Fatalf("clean batch must not re-queue, got %v", rq)
		}
		// One client update advancing to the source tip (100), one submit for all 3.
		if len(dst.updates) != 1 || dst.updates[0].Height != 100 {
			t.Fatalf("want single update at source tip 100, got %+v", dst.updates)
		}
		if dst.relayCalls != 1 {
			t.Fatalf("want packets folded into ONE RelayPackets call, got %d calls", dst.relayCalls)
		}
		if len(dst.relayed) != 3 {
			t.Fatalf("want 3 packets relayed, got %d", len(dst.relayed))
		}
	})

	t.Run("folds update and proofs at target height", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &foldingMockDest{enabled: true}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{forceHeight: 80})
		m.lastHeight = 10

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 30, Raw: []byte("a")},
			{Type: chain.AckPacket, Height: 50, Raw: []byte("b")},
		}
		if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
			t.Fatalf("clean folded batch must not re-queue, got %v", rq)
		}
		if dst.foldCalls != 1 || len(dst.foldedUpdates) != 1 || dst.foldedUpdates[0].Height != 80 {
			t.Fatalf("want one folded update at target height 80, calls=%d updates=%+v", dst.foldCalls, dst.foldedUpdates)
		}
		if len(dst.updates) != 0 || dst.relayCalls != 0 {
			t.Fatalf("folded path must not submit standalone txs: updates=%d relays=%d", len(dst.updates), dst.relayCalls)
		}
		if len(src.proofHeights) != 2 || src.proofHeights[0] != 80 || src.proofHeights[1] != 80 {
			t.Fatalf("proofs must target not-yet-on-chain update height 80, got %v", src.proofHeights)
		}
		for _, packet := range dst.foldedPackets[0] {
			if packet.Height != 80 {
				t.Fatalf("folded packet height = %d, want 80", packet.Height)
			}
		}
		if m.lastHeight != 80 {
			t.Fatalf("successful atomic tx must advance lastHeight to 80, got %d", m.lastHeight)
		}
	})

	t.Run("folding capability disabled falls back", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &foldingMockDest{enabled: false}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		if rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 20, Raw: []byte("p")}}); len(rq) != 0 {
			t.Fatalf("fallback batch must not re-queue, got %v", rq)
		}
		if dst.foldCalls != 0 || len(dst.updates) != 1 || dst.relayCalls != 1 {
			t.Fatalf("disabled folding must use two-tx fallback: folds=%d updates=%d relays=%d", dst.foldCalls, len(dst.updates), dst.relayCalls)
		}
	})

	t.Run("fold failure does not advance state", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &foldingMockDest{enabled: true, foldErr: errors.New("rpc unavailable")}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 20, Raw: []byte("p")}})
		if len(rq) != 1 || rq[0] != 0 {
			t.Fatalf("transient folded failure must re-queue packet, got %v", rq)
		}
		if m.lastHeight != 0 {
			t.Fatalf("failed atomic tx must not advance lastHeight, got %d", m.lastHeight)
		}
		if len(dst.updates) != 0 || dst.relayCalls != 0 {
			t.Fatalf("failed folded path must not fall through to standalone txs")
		}
	})

	t.Run("no update needed relays packets only", func(t *testing.T) {
		src := &mockSource{latest: 200}
		dst := &foldingMockDest{enabled: true}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{noPayload: true, forceHeight: 200})

		if rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 180, Raw: []byte("p")}}); len(rq) != 0 {
			t.Fatalf("current-client batch must relay, got %v", rq)
		}
		if dst.foldCalls != 0 || len(dst.updates) != 0 || dst.relayCalls != 1 {
			t.Fatalf("no-update branch must submit packets only: folds=%d updates=%d relays=%d", dst.foldCalls, len(dst.updates), dst.relayCalls)
		}
		if len(src.proofHeights) != 1 || src.proofHeights[0] != 200 {
			t.Fatalf("no-update proof height = %v, want [200]", src.proofHeights)
		}
	})

	t.Run("folded isolation preserves valid siblings", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &foldingMockDest{enabled: true}
		dst.poison = map[string]bool{"bad": true}
		dst.foldFn = func(_ chain.ClientUpdate, packets []chain.RelayPacket) error {
			if len(packets) > 1 {
				return chain.Permanent(errors.New("folded batch reverted"))
			}
			return nil
		}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("good-1")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("bad")},
			{Type: chain.SendPacket, Height: 9, Raw: []byte("good-2")},
		}
		if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
			t.Fatalf("isolation must drop only poison, got requeue %v", rq)
		}
		// One fold: the batch. Isolation no longer re-folds, because the update is
		// installed by the attribution step before any packet is judged -- see
		// TestHandleBatch/folded update rejection does not drop innocent packets for why
		// that step exists.
		if dst.foldCalls != 1 {
			t.Fatalf("folded batch should be attempted once, got %d fold calls", dst.foldCalls)
		}
		if len(dst.updates) != 1 || dst.updates[0].Height != 100 {
			t.Fatalf("the update must be installed on its own before any packet is judged, got %+v", dst.updates)
		}
		if m.lastHeight != 100 {
			t.Fatalf("the standalone update must advance lastHeight, got %d", m.lastHeight)
		}
		if len(dst.relayed) != 2 ||
			string(dst.relayed[0].Packet) != "good-1" || string(dst.relayed[1].Packet) != "good-2" {
			t.Fatalf("both valid siblings must still relay, got %+v", dst.relayed)
		}
	})

	// TestHandleBatch/untracks delivered sends verifies that after a successful relay
	// the module removes each delivered SendPacket from the pending tracker (via the
	// untrack hook) — and only SendPacket, not acks — so the timeout scanner stops
	// considering a packet that can no longer time out.
	t.Run("untracks delivered sends", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{}
		var untracked [][]byte
		m := NewModule("test", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(
				func([]byte, uint64) bool { return true }, // track: no-op for this test
				func(pkt []byte) { untracked = append(untracked, pkt) },
			),
		)

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("send-1")},
			{Type: chain.AckPacket, Height: 8, Raw: []byte("ack-1")},
			{Type: chain.SendPacket, Height: 9, Raw: []byte("send-2")},
		}
		if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
			t.Fatalf("clean batch must not re-queue, got %v", rq)
		}
		if len(untracked) != 2 {
			t.Fatalf("want 2 sends untracked, got %d (%q)", len(untracked), untracked)
		}
		if string(untracked[0]) != "send-1" || string(untracked[1]) != "send-2" {
			t.Fatalf("untracked the wrong packets (acks must not be untracked): %q", untracked)
		}
	})

	// TestHandleBatch/no untrack on relay failure verifies a failed relay does NOT untrack
	// any packet — a packet that was not delivered must stay in the tracker so the
	// timeout scanner can still refund it.
	t.Run("no untrack on relay failure", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{relayErr: errors.New("rpc down")} // transient relay failure
		var untracked [][]byte
		m := NewModule("test", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(
				func([]byte, uint64) bool { return true },
				func(pkt []byte) { untracked = append(untracked, pkt) },
			),
		)

		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 7, Raw: []byte("send-1")}})
		if len(rq) != 1 {
			t.Fatalf("transient relay failure must re-queue the send, got %v", rq)
		}
		if len(untracked) != 0 {
			t.Fatalf("a failed relay must not untrack anything, got %q", untracked)
		}
	})

	t.Run("requeues send when pending tracking fails", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{}
		trackCalls := 0
		m := NewModule("test", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(
				func([]byte, uint64) bool {
					trackCalls++
					return trackCalls > 1
				},
				nil,
			),
		)
		event := chain.Event{Type: chain.SendPacket, Height: 7, Sequence: 9, Raw: []byte("send-1")}

		if got := m.handleBatch(context.Background(), []chain.Event{event}); len(got) != 1 || got[0] != 0 {
			t.Fatalf("failed durable tracking requeue = %v, want [0]", got)
		}
		if dst.relayCalls != 0 {
			t.Fatalf("relay calls after failed durable tracking = %d, want 0", dst.relayCalls)
		}

		if got := m.handleBatch(context.Background(), []chain.Event{event}); len(got) != 0 {
			t.Fatalf("requeue after tracking recovered = %v, want none", got)
		}
		if dst.relayCalls != 1 {
			t.Fatalf("relay calls after tracking recovered = %d, want 1", dst.relayCalls)
		}
	})

	// The source has already handed this packet to the unbuffered batch channel,
	// so it is no longer reachable through DropCosmosQueued. A terminal event now
	// must leave a durable tombstone that handleBatch sees before it tracks, proves,
	// or submits the stale receive.
	t.Run("terminal event after cosmos handoff skips tracking proof and submit", func(t *testing.T) {
		bb := services.NewBatchBuilder()
		packet := channeltypesv2.Packet{
			SourceClient: "cosmos-client", DestinationClient: "eth-client", Sequence: 41,
			TimeoutTimestamp: uint64(time.Now().Add(time.Hour).Unix()),
			Payloads:         []channeltypesv2.Payload{{Value: []byte("handoff")}},
		}
		raw, err := packet.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		bb.AddCosmos(services.CosmosPacket{Type: services.CosmosSend, Packet: &packet, BlockNumber: 7})
		ch := make(chan services.CosmosBatch)
		handedOff := make(chan struct{})
		go func() {
			bb.CheckCosmos(context.Background(), services.BatchConfig{BatchSize: 1}, ch)
			close(handedOff)
		}()
		batch := <-ch
		<-handedOff
		if len(batch.Packets) != 1 {
			t.Fatalf("handoff contains %d packets, want 1", len(batch.Packets))
		}

		// This is SubscribeCosmos observing acknowledge_packet after the handoff.
		if err := bb.PendingTracker.MarkSettled(packet); err != nil {
			t.Fatal(err)
		}
		src := &mockSource{latest: 100}
		dst := &mockDest{}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(func(raw []byte, height uint64) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && bb.PendingTracker.Add(tracked, height)
			}, nil),
			WithPendingSettlementGuard(func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && bb.PendingTracker.IsSettled(tracked)
			}, func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && bb.PendingTracker.MarkSettled(tracked) == nil
			}),
		)

		if requeue := m.handleBatch(context.Background(), []chain.Event{{
			Type: chain.SendPacket, Sequence: packet.Sequence, Height: 7, Raw: raw,
		}}); len(requeue) != 0 {
			t.Fatalf("settled handoff requeue = %v, want none", requeue)
		}
		if src.membershipCalls != 0 || dst.relayCalls != 0 {
			t.Fatalf("settled handoff did proof=%d relay=%d, want neither", src.membershipCalls, dst.relayCalls)
		}
		if got := bb.PendingTracker.Len(); got != 0 {
			t.Fatalf("settled handoff left %d pending tracker entry(ies)", got)
		}
		bb.ReleaseCosmosInFlight(batch)
	})

	t.Run("destination receipt skips before tracking and leaves a tombstone", func(t *testing.T) {
		tracker := services.NewPendingPacketTracker()
		packet := channeltypesv2.Packet{
			SourceClient: "cosmos-client", DestinationClient: "eth-client", Sequence: 42,
			TimeoutTimestamp: 1000, Payloads: []channeltypesv2.Payload{{Value: []byte("received")}},
		}
		raw, err := packet.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		trackCalls := 0
		src := &mockSource{latest: 100}
		dst := &mockDest{hasReceipt: true}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(func(raw []byte, height uint64) bool {
				trackCalls++
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.Add(tracked, height)
			}, nil),
			WithPendingSettlementGuard(func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.IsSettled(tracked)
			}, func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.MarkSettled(tracked) == nil
			}),
		)

		if requeue := m.handleBatch(context.Background(), []chain.Event{{
			Type: chain.SendPacket, Sequence: packet.Sequence, Height: 7, Raw: raw,
		}}); len(requeue) != 0 {
			t.Fatalf("received packet requeue = %v, want none", requeue)
		}
		if trackCalls != 0 || src.membershipCalls != 0 || dst.relayCalls != 0 {
			t.Fatalf("received packet track=%d proof=%d relay=%d, want none", trackCalls, src.membershipCalls, dst.relayCalls)
		}
		if !tracker.IsSettled(packet) {
			t.Fatal("destination receipt did not persist a terminal tombstone")
		}
	})

	// A duplicate inside an otherwise-clean batch must not cost the batch its
	// proof or its submission: the settlement check drops only the one packet
	// another relayer already delivered, and its siblings still fold into a
	// single relay.
	t.Run("one settled packet in a batch does not waste its siblings' proof or submit", func(t *testing.T) {
		tracker := services.NewPendingPacketTracker()
		mkPacket := func(seq uint64) (channeltypesv2.Packet, []byte) {
			p := channeltypesv2.Packet{
				SourceClient: "cosmos-client", DestinationClient: "eth-client", Sequence: seq,
				TimeoutTimestamp: 1000, Payloads: []channeltypesv2.Payload{{Value: []byte(fmt.Sprintf("pkt-%d", seq))}},
			}
			raw, err := p.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			return p, raw
		}
		pkt1, raw1 := mkPacket(1)
		pkt2, raw2 := mkPacket(2) // this one is already delivered by another relayer
		pkt3, raw3 := mkPacket(3)

		trackCalls := 0
		src := &mockSource{latest: 100}
		// packetSettled is asked once per event, in order: false, true, false.
		dst := &mockDest{receiptAnswers: []bool{false, true, false}}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(func(raw []byte, height uint64) bool {
				trackCalls++
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.Add(tracked, height)
			}, nil),
			WithPendingSettlementGuard(func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.IsSettled(tracked)
			}, func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.MarkSettled(tracked) == nil
			}),
		)

		requeue := m.handleBatch(context.Background(), []chain.Event{
			{Type: chain.SendPacket, Sequence: pkt1.Sequence, Height: 7, Raw: raw1},
			{Type: chain.SendPacket, Sequence: pkt2.Sequence, Height: 7, Raw: raw2},
			{Type: chain.SendPacket, Sequence: pkt3.Sequence, Height: 7, Raw: raw3},
		})
		if len(requeue) != 0 {
			t.Fatalf("clean batch with one duplicate requeue = %v, want none", requeue)
		}
		if trackCalls != 2 {
			t.Fatalf("track calls = %d, want 2 (the settled packet must never be tracked)", trackCalls)
		}
		if src.membershipCalls != 2 {
			t.Fatalf("membership proof calls = %d, want 2 (one per unsettled packet)", src.membershipCalls)
		}
		if dst.relayCalls != 1 {
			t.Fatalf("relay calls = %d, want 1 -- the duplicate must not fragment the batch", dst.relayCalls)
		}
		if len(dst.relayed) != 2 {
			t.Fatalf("relayed %d packet(s), want 2 (the duplicate excluded)", len(dst.relayed))
		}
		for _, p := range dst.relayed {
			if p.Sequence == pkt2.Sequence {
				t.Fatalf("duplicate packet seq=%d reached the destination", pkt2.Sequence)
			}
		}
		if !tracker.IsSettled(pkt2) {
			t.Fatal("duplicate packet did not persist a terminal tombstone")
		}
		if tracker.IsSettled(pkt1) || tracker.IsSettled(pkt3) {
			t.Fatal("a sibling packet was wrongly marked settled")
		}
	})

	// A terminal event can land on SubscribeCosmos's goroutine strictly AFTER the
	// second IsSettled read at the top of handleBatchLocked (the one right after
	// Add), AND after the per-packet recheck the proof loop runs just before
	// calling MembershipProof -- landing instead while the proof itself is being
	// built. Before the pre-submission recheck existed, the packet was already in
	// `packets` by the time that happened, so nothing downstream would observe
	// the new tombstone and it would reach RelayPackets as a duplicate receive.
	// MembershipProof is where MarkSettled fires here, so the settlement lands in
	// exactly that gap: after this packet's own proof-loop check already read
	// false, but before submission. The proof is unavoidably spent in this exact
	// window (asserted below); what must not happen is the submission.
	t.Run("terminal event settling mid-proof-build is not submitted as a duplicate", func(t *testing.T) {
		tracker := services.NewPendingPacketTracker()
		packet := channeltypesv2.Packet{
			SourceClient: "cosmos-client", DestinationClient: "eth-client", Sequence: 77,
			TimeoutTimestamp: uint64(time.Now().Add(time.Hour).Unix()),
			Payloads:         []channeltypesv2.Payload{{Value: []byte("mid-proof")}},
		}
		raw, err := packet.Marshal()
		if err != nil {
			t.Fatal(err)
		}

		src := &mockSource{latest: 100}
		// MembershipProof runs strictly after both settlement checks at the top of
		// handleBatchLocked (before tracking, and right after Add) have already
		// read false. This is SubscribeCosmos observing acknowledge_packet on its
		// own goroutine while this handler is mid-proof for the same packet.
		src.onMembershipProof = func(p []byte) {
			if string(p) != string(raw) {
				return
			}
			var tracked channeltypesv2.Packet
			if err := tracked.Unmarshal(p); err != nil {
				t.Fatal(err)
			}
			if err := tracker.MarkSettled(tracked); err != nil {
				t.Fatal(err)
			}
		}
		dst := &mockDest{}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{},
			WithPacketTracker(func(raw []byte, height uint64) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.Add(tracked, height)
			}, nil),
			WithPendingSettlementGuard(func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.IsSettled(tracked)
			}, func(raw []byte) bool {
				var tracked channeltypesv2.Packet
				return tracked.Unmarshal(raw) == nil && tracker.MarkSettled(tracked) == nil
			}),
		)

		if requeue := m.handleBatch(context.Background(), []chain.Event{{
			Type: chain.SendPacket, Sequence: packet.Sequence, Height: 7, Raw: raw,
		}}); len(requeue) != 0 {
			t.Fatalf("settled-mid-proof requeue = %v, want none: it is done, not retryable", requeue)
		}
		if src.membershipCalls != 1 {
			t.Fatalf("membership proof calls = %d, want 1: the proof-loop recheck runs before this packet's own "+
				"proof build and cannot see a settlement landing during that build", src.membershipCalls)
		}
		if dst.relayCalls != 0 || len(dst.relayed) != 0 {
			t.Fatalf("settled-mid-proof reached submission: relayCalls=%d relayed=%+v, want none -- this is the duplicate receive the guard exists to stop",
				dst.relayCalls, dst.relayed)
		}
		if !tracker.IsSettled(packet) {
			t.Fatal("the terminal tombstone set by MarkSettled must still be there")
		}
		if got := tracker.Len(); got != 0 {
			t.Fatalf("the active tracker record must be gone once settled, got %d entries", got)
		}
	})

	// TestHandleBatch/isolates poison packet verifies that when a multi-packet batch
	// hits a permanent (deterministic revert) failure, the module isolates the poison
	// packet by retrying each packet individually — so the poison is dropped alone and
	// its VALID siblings still relay, rather than the whole batch being discarded.
	t.Run("isolates poison packet", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{poison: map[string]bool{"bad": true}}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("good-1")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("bad")},
			{Type: chain.SendPacket, Height: 9, Raw: []byte("good-2")},
		}
		if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
			t.Fatalf("isolation drops only the poison and re-queues nothing, got %v", rq)
		}
		var relayed []string
		for _, p := range dst.relayed {
			relayed = append(relayed, string(p.Packet))
		}
		if len(relayed) != 2 || relayed[0] != "good-1" || relayed[1] != "good-2" {
			t.Fatalf("valid siblings must still relay (poison dropped), got %v", relayed)
		}
	})

	// TestHandleBatch/isolation requeues transient verifies that if an isolated retry
	// hits a TRANSIENT failure, that packet's index is re-queued (not dropped), while
	// the poison packet is still dropped.
	t.Run("isolation requeues transient", func(t *testing.T) {
		src := &mockSource{latest: 100}
		// Whole batch reverts permanently (poison "bad"); when retried alone, "good-1"
		// fails transiently and "bad" fails permanently.
		dst := &transientOnIsolate{poison: "bad", transientFor: "good-1"}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})
		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("good-1")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("bad")},
		}
		rq := m.handleBatch(context.Background(), batch)
		// good-1 (index 0) transient → re-queued; bad (index 1) permanent → dropped.
		if len(rq) != 1 || rq[0] != 0 {
			t.Fatalf("transient isolated retry must re-queue only good-1's index [0], got %v", rq)
		}
	})

	// TestHandleBatch/cancelled context relays nothing verifies that a cancelled context
	// short-circuits handleBatch: everything is re-queued and no proof/relay work runs.
	t.Run("cancelled context relays nothing", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("a")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("b")},
		}
		rq := m.handleBatch(ctx, batch)
		if len(rq) != 2 {
			t.Fatalf("cancelled ctx must re-queue every event, got %v", rq)
		}
		if src.membershipCalls != 0 || dst.relayCalls != 0 || len(dst.updates) != 0 {
			t.Fatalf("cancelled ctx must not run proof/update/relay work: m=%d relay=%d upd=%d",
				src.membershipCalls, dst.relayCalls, len(dst.updates))
		}
	})

	// TestHandleBatch/requeue transient verifies a per-packet transient proof failure
	// re-queues exactly that event's index while the rest still relay.
	t.Run("requeue transient", func(t *testing.T) {
		src := &mockSource{latest: 100, failMembershipOn: map[string]bool{"bad": true}}
		dst := &mockDest{}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 3, Raw: []byte("ok1")},
			{Type: chain.SendPacket, Height: 4, Raw: []byte("bad")},
			{Type: chain.SendPacket, Height: 5, Raw: []byte("ok2")},
		}
		rq := m.handleBatch(context.Background(), batch)
		if len(rq) != 1 || rq[0] != 1 {
			t.Fatalf("want re-queue of index 1 only, got %v", rq)
		}
		if len(dst.relayed) != 2 {
			t.Fatalf("want the 2 good packets relayed, got %d", len(dst.relayed))
		}
	})

	// TestHandleBatch/drop permanent proof verifies a permanent proof failure (a
	// timed-out packet) is DROPPED, not re-queued — the fix for the gas-drain bug
	// where a dead packet spun forever, each retry re-running a client update.
	t.Run("drop permanent proof", func(t *testing.T) {
		src := &mockSource{latest: 100, permanentMembershipOn: map[string]bool{"dead": true}}
		dst := &mockDest{}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 3, Raw: []byte("ok")},
			{Type: chain.SendPacket, Height: 4, Raw: []byte("dead")}, // timed out → permanent
		}
		rq := m.handleBatch(context.Background(), batch)
		if len(rq) != 0 {
			t.Fatalf("permanent packet must be DROPPED, not re-queued, got %v", rq)
		}
		if len(dst.relayed) != 1 || string(dst.relayed[0].Packet) != "ok" {
			t.Fatalf("only the live packet should relay, got %+v", dst.relayed)
		}
	})

	// TestHandleBatch/drop permanent relay verifies a permanent relay (on-chain revert)
	// failure drops the batch instead of re-queueing it forever.
	t.Run("drop permanent relay", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{relayErr: chain.Permanent(errors.New("reverted"))}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 5, Raw: []byte("p")}})
		if len(rq) != 0 {
			t.Fatalf("permanent relay revert must DROP, not re-queue, got %v", rq)
		}
	})

	// TestHandleBatch/requeue transient relay verifies a transient relay failure (RPC,
	// nonce) re-queues the batch (unlike a permanent revert).
	t.Run("requeue transient relay", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{relayErr: errors.New("rpc unavailable")} // not permanent
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 5, Raw: []byte("p")}})
		if len(rq) != 1 {
			t.Fatalf("transient relay failure must re-queue, got %v", rq)
		}
	})

	// TestHandleBatch/precondition skip verifies the RelayableHeight precondition: a
	// packet whose source height is above the relayable height (Cosmos AppHash lag /
	// ETH finality lag) is re-queued WITHOUT any client update or proof — the fix for
	// the re-prove/spam regression where the module burned a proof every flush while
	// waiting.
	t.Run("precondition skip", func(t *testing.T) {
		src := &mockSource{latest: 100, relayable: 50} // tip 100, but only <=50 provable
		dst := &mockDest{}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 80, Raw: []byte("pending")}})
		if len(rq) != 1 || rq[0] != 0 {
			t.Fatalf("packet above relayable height must re-queue, got %v", rq)
		}
		if len(dst.updates) != 0 {
			t.Fatalf("must NOT advance the client for an unprovable batch, got %d updates", len(dst.updates))
		}
		if src.membershipCalls != 0 {
			t.Fatalf("must NOT build a proof for an unprovable batch, got %d", src.membershipCalls)
		}
	})

	// TestHandleBatch/provability guard verifies that when the destination client can
	// only advance to a height below the packet's (the ETH->Cosmos finality-lag case),
	// the packet is re-queued instead of proven against a state that lacks it.
	t.Run("provability guard", func(t *testing.T) {
		src := &mockSource{latest: 100}
		dst := &mockDest{}
		// Builder can only advance the client to height 50 (finality lag), regardless
		// of the requested height.
		m := NewModule("test", "client-0", src, dst, &mockBuilder{forceHeight: 50})

		// Packet observed at execution height 100, but the client only reaches 50.
		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 100, Raw: []byte("future")}})
		if len(rq) != 1 || rq[0] != 0 {
			t.Fatalf("packet above provable height must re-queue, got %v", rq)
		}
		if src.membershipCalls != 0 {
			t.Fatalf("must not attempt a proof for an unprovable packet, got %d", src.membershipCalls)
		}
		if len(dst.relayed) != 0 {
			t.Fatalf("nothing should be relayed, got %d", len(dst.relayed))
		}
	})

	// A permanently-failing packet must leave no wait entry behind, on EVERY terminal
	// path. The folded isolated path was the one that did not clear: a batch fold
	// reverts, isolation retries each packet carrying the update, and a packet that
	// fails permanently there stayed in the tracker until its TTL — so `status` and
	// the waiting log would keep reporting an age for a packet that was already dropped.
	//
	// Asserting on the tracker rather than on a log line is deliberate: the tracker is
	// what a future status command reads, and it is what a fifth terminal path would
	// also have to clear.
	t.Run("folded isolated permanent drop clears the wait", func(t *testing.T) {
		src := &mockSource{relayable: 100}
		dst := &foldingMockDest{enabled: true}
		// The folded batch reverts, the update lands on its own -- so the packets are
		// the poison -- and each isolated singleton then reverts too. Both halves are
		// needed: without the second the packets would simply relay, and this test
		// would assert an empty wait tracker for the wrong reason.
		dst.foldFn = func(_ chain.ClientUpdate, _ []chain.RelayPacket) error {
			return chain.Permanent(errors.New("reverted"))
		}
		dst.relayErr = chain.Permanent(errors.New("reverted"))
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		events := []chain.Event{
			{Type: chain.SendPacket, Height: 10, Sequence: 1, Raw: []byte("a")},
			{Type: chain.SendPacket, Height: 10, Sequence: 2, Raw: []byte("b")},
		}
		// First pass: nothing is relayable yet, so both packets are observed as waiting.
		src.relayable = 5
		if rq := m.handleBatch(context.Background(), events); len(rq) != 2 {
			t.Fatalf("both packets should be re-queued while unrelayable, got %d", len(rq))
		}
		if got := len(m.waits.entries); got != 2 {
			t.Fatalf("expected 2 tracked waits after the first pass, got %d", got)
		}

		// Second pass: relayable, folded batch reverts, isolation drops both permanently.
		src.relayable = 100
		if rq := m.handleBatch(context.Background(), events); len(rq) != 0 {
			t.Fatalf("permanently dropped packets must not be re-queued, got %d", len(rq))
		}
		if got := len(m.waits.entries); got != 0 {
			t.Fatalf("folded isolated drop left %d wait entry/entries behind; every terminal path must clear", got)
		}
		if dst.foldCalls != 1 {
			t.Fatalf("expected one folded batch attempt, got %d", dst.foldCalls)
		}
		if dst.relayCalls != len(events) {
			t.Fatalf("expected one packets-only attempt per packet, got %d for %d packet(s)", dst.relayCalls, len(events))
		}
	})

	// A packet waiting on the source frontier is re-queued every flush — every batch
	// period, on no backoff. A measured OP Sepolia wait produced 67 identical lines in
	// 4m12s, and a default 150-block attestor gap makes ~100 the normal case. The line
	// must therefore report transitions, not repetitions: the same wait says nothing
	// new, a frontier that moved does.
	t.Run("waiting logs only on change", func(t *testing.T) {
		src := &mockSource{latest: 100, relayable: 5}
		m := NewModule("test", "client-0", src, &mockDest{}, &mockBuilder{})
		waiting := []chain.Event{{Type: chain.SendPacket, Height: 50, Raw: []byte("pkt-far")}}

		if rq := m.handleBatch(context.Background(), waiting); len(rq) != 1 {
			t.Fatalf("packet above the relayable height must re-queue, got %v", rq)
		}
		first := m.lastWait
		if !first.logged || first.packets != 1 || first.relayable != 5 || first.description == "" {
			t.Fatalf("first wait must be recorded as logged: %+v", first)
		}

		// Same picture again: nothing to say.
		m.handleBatch(context.Background(), waiting)
		if m.lastWait != first {
			t.Fatalf("an unchanged wait must not update the log state: %+v -> %+v", first, m.lastWait)
		}

		// Frontier advances — that is the signal worth printing.
		src.relayable = 20
		m.handleBatch(context.Background(), waiting)
		if m.lastWait == first {
			t.Fatal("a moving frontier must be reported")
		}
		if m.lastWait.relayable != 20 {
			t.Fatalf("log state must track the new frontier, got %+v", m.lastWait)
		}

		// Once everything is relayable the state resets, so the next wait reports
		// itself even if it looks identical to the one before.
		src.relayable = 100
		if rq := m.handleBatch(context.Background(), waiting); len(rq) != 0 {
			t.Fatalf("packet below the relayable height must not re-queue, got %v", rq)
		}
		if m.lastWait.logged {
			t.Fatalf("a cleared wait must reset the log state, got %+v", m.lastWait)
		}
	})

	// TestHandleBatch/folded update submitted when no packet is provable pins the escape from a loop
	// the relayer could not leave on its own.
	//
	// A folding destination leaves the client update unsubmitted so it can ride in
	// the packet transaction. When every proof fails there are no packets, and the
	// update used to be dropped with them — but the proofs are built at the height
	// the client already trusts, so the client standing still is exactly why they
	// failed:
	//
	//	proofs target the trusted height -> that height falls out of the execution
	//	node's state window -> every proof fails -> no packets -> the update is
	//	dropped -> the client stays put -> the gap only widens
	//
	// Observed on Sepolia: trustedSlot pinned at 10885728 for twenty minutes while
	// finalizedSlot advanced, every pass logging `eth_getProof failed: historical
	// state ... is not available`, and no update ever submitted.
	t.Run("folded update submitted when no packet is provable", func(t *testing.T) {
		src := &mockSource{latest: 200, failMembershipOn: map[string]bool{"p": true}}
		dst := &foldingMockDest{enabled: true}
		b := &mockBuilder{}
		m := NewModule("test", "client-0", src, dst, b)

		rq := m.handleBatch(context.Background(), []chain.Event{
			{Type: chain.SendPacket, Height: 180, Raw: []byte("p")},
		})

		if len(rq) != 1 {
			t.Fatalf("the unprovable packet must be re-queued, got %v", rq)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("the client update must still be submitted with no packets to fold it into, got %d", len(dst.updates))
		}
		if dst.foldCalls != 0 {
			t.Fatalf("RelayWithUpdate is specified to carry packets; with none it must not be called, got %d calls", dst.foldCalls)
		}
		if m.lastHeight != 200 {
			t.Fatalf("lastHeight must advance to the submitted update height, got %d", m.lastHeight)
		}
	})

	// The lock the folded plan holds must be released on this path too, or the next
	// flush deadlocks — a stall that would look exactly like the bug being fixed.
	t.Run("folded update with no packets releases the lock", func(t *testing.T) {
		src := &mockSource{latest: 200, failMembershipOn: map[string]bool{"p": true}}
		dst := &foldingMockDest{enabled: true}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		evts := []chain.Event{{Type: chain.SendPacket, Height: 180, Raw: []byte("p")}}
		m.handleBatch(context.Background(), evts)

		done := make(chan struct{})
		go func() { defer close(done); m.handleBatch(context.Background(), evts) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("second flush blocked: the folded plan's lock was not released after the packet-less update")
		}
	})

	// A failed update must not advance lastHeight — otherwise the module would
	// believe the client covers heights it does not, and build proofs the
	// destination cannot verify.
	t.Run("folded update failure does not advance height", func(t *testing.T) {
		src := &mockSource{latest: 200, failMembershipOn: map[string]bool{"p": true}}
		dst := &foldingMockDest{enabled: true}
		dst.updateErr = errors.New("cosmos rpc down")
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})

		m.handleBatch(context.Background(), []chain.Event{
			{Type: chain.SendPacket, Height: 180, Raw: []byte("p")},
		})

		if m.lastHeight != 0 {
			t.Fatalf("a failed update must leave lastHeight at 0, got %d", m.lastHeight)
		}
	})

	// The ladder is dead code unless handleBatch actually goes through it. This is
	// the production shape: an ack whose proof keeps failing, flushed repeatedly.
	t.Run("escalates a repeated proof failure", func(t *testing.T) {
		src := &mockSource{
			latest:           100,
			relayable:        50, // packet at 80 is not provable yet: it waits first
			failMembershipOn: map[string]bool{"ack": true},
		}
		m := NewModule("eth->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		m.waits.now = clk.now

		batch := []chain.Event{{Type: chain.AckPacket, Sequence: 21, Height: 80, Raw: []byte("ack"), AckBytes: [][]byte{{1}}}}

		// Sixteen minutes parked at the source gate before a proof is ever attempted.
		// This is the normal case on an L2 return leg, and none of it is a proof
		// failure -- so it must not be charged to the ladder below.
		m.handleBatch(context.Background(), batch)
		clk.t = clk.t.Add(16 * time.Minute)
		src.relayable = 100

		first := captureLog(func() { m.handleBatch(context.Background(), batch) })
		if !strings.Contains(first, "ack seq=21") {
			t.Fatalf("the first proof failure must reach the log:\n%s", first)
		}
		if strings.Contains(first, "STUCK") {
			t.Fatalf("the FIRST proof failure was reported as STUCK because it inherited the pre-proof wait:\n%s", first)
		}

		// Ten more flushes within the same threshold: silent. This is the case that
		// produced 296 byte-identical lines.
		repeats := captureLog(func() {
			for i := 0; i < 10; i++ {
				clk.t = clk.t.Add(2 * time.Second)
				m.handleBatch(context.Background(), batch)
			}
		})
		if strings.Contains(repeats, "ack seq=21") {
			t.Fatalf("repeats inside one threshold must be suppressed:\n%s", repeats)
		}

		// Past the stuck threshold the wording has to change: an operator reading
		// "proof for packet" cannot tell a retry from a packet with no way out.
		clk.t = clk.t.Add(proofFailureStuckAfter + time.Minute)
		stuck := captureLog(func() { m.handleBatch(context.Background(), batch) })
		for _, want := range []string{"STUCK", "ack seq=21", "no other exit"} {
			if !strings.Contains(stuck, want) {
				t.Fatalf("the escalated line must contain %q:\n%s", want, stuck)
			}
		}
	})

	// TestHandleBatch/stuck send names the timeout exit: "no other exit" is true of an ack
	// and false of a send. Every source reports an expired send as permanent, the
	// module drops it, and the timeout scanner refunds it -- so telling an operator a
	// send is unrecoverable sends them looking for a problem that resolves itself.
	// The two packet types need different next actions, so they need different lines.
	t.Run("stuck send names the timeout exit", func(t *testing.T) {
		src := &mockSource{
			latest:           100,
			relayable:        100,
			failMembershipOn: map[string]bool{"snd": true},
		}
		m := NewModule("eth->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		m.waits.now = clk.now

		batch := []chain.Event{{Type: chain.SendPacket, Sequence: 21, Height: 80, Raw: []byte("snd")}}

		m.handleBatch(context.Background(), batch)
		clk.t = clk.t.Add(proofFailureStuckAfter + time.Minute)
		stuck := captureLog(func() { m.handleBatch(context.Background(), batch) })

		// A SendPacket prints as "recv": the line names the message the destination
		// will be asked for, not the event's origin (see TestEventType_Names).
		for _, want := range []string{"STUCK", "recv seq=21", "timeout scanner"} {
			if !strings.Contains(stuck, want) {
				t.Fatalf("a stuck send must name the exit it actually has (%q):\n%s", want, stuck)
			}
		}
		if strings.Contains(stuck, "no other exit") {
			t.Fatalf("a send is refunded once past its timeout, so it does have another exit:\n%s", stuck)
		}
	})

	// A permanently-failing source -- a misconfigured attestor route, a malformed
	// request, an RPC the daemon does not implement -- used to be asked again on
	// every pass. The L2 loop re-offers its queued packets every 4s and deliberately
	// does so even when the chain produced no block, so one bad config line became
	// roughly 21,600 attestor calls and log lines a day, each looking like an
	// ordinary transient RPC failure.
	//
	// The hold changes only how often the source is asked. It must not drop a packet
	// and must not skip the durable tracking that pays for the timeout refund.
	t.Run("permanent source failure is held off", func(t *testing.T) {
		events := []chain.Event{{Type: chain.SendPacket, Sequence: 7, Height: 40, Raw: []byte("pkt-7")}}

		// A terminal tombstone is authoritative even when the source cannot answer.
		// These early exits occur after the settlement guard has filtered the batch,
		// so they must retry only the remaining non-terminal events.
		t.Run("settled packets are not requeued on source outage", func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				source         *mockSource
				setHeldOff     bool
				wantSourceCall int
			}{
				{
					name:           "relayable height error",
					source:         &mockSource{latest: 100, relayableErr: errors.New("source unavailable")},
					wantSourceCall: 1,
				},
				{
					name:           "permanent failure hold",
					source:         &mockSource{latest: 100},
					setHeldOff:     true,
					wantSourceCall: 0,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					m := NewModule("l2->cosmos", "client-0", tc.source, &mockDest{}, &mockBuilder{},
						WithPendingSettlementGuard(func([]byte) bool { return true }, nil))
					if tc.setHeldOff {
						m.sourceProbeAt = time.Now().Add(time.Minute)
					}

					if got := m.handleBatch(context.Background(), events); len(got) != 0 {
						t.Fatalf("settled packet requeue = %v, want none", got)
					}
					if tc.source.relayableCalls != tc.wantSourceCall {
						t.Fatalf("RelayableHeight calls = %d, want %d", tc.source.relayableCalls, tc.wantSourceCall)
					}
				})
			}
		})

		t.Run("the source is not asked again during the hold", func(t *testing.T) {
			src := &mockSource{latest: 100, relayableErr: chain.Permanent(errors.New("attestor does not serve this chain"))}
			m := NewModule("l2->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})

			out := captureLog(func() { m.handleBatch(context.Background(), events) })
			if src.relayableCalls != 1 {
				t.Fatalf("first pass asked the source %d times, want 1", src.relayableCalls)
			}
			if !strings.Contains(out, "PERMANENT") {
				t.Fatalf("a permanent source failure must say so, not read as a transient RPC error:\n%s", out)
			}

			for i := 0; i < 5; i++ {
				m.handleBatch(context.Background(), events)
			}
			if src.relayableCalls != 1 {
				t.Fatalf("source asked %d times across 6 passes; the hold is not holding", src.relayableCalls)
			}
		})

		// The hold skips the RPC and the proof, never the durable record: a packet
		// queued while the source is held off must still be tracked, or it can never
		// be refunded when it expires.
		t.Run("packets are still tracked while held off", func(t *testing.T) {
			src := &mockSource{latest: 100, relayableErr: chain.Permanent(errors.New("attestor does not serve this chain"))}
			var tracked [][]byte
			m := NewModule("l2->cosmos", "client-0", src, &mockDest{}, &mockBuilder{},
				WithPacketTracker(func(raw []byte, _ uint64) bool {
					tracked = append(tracked, raw)
					return true
				}, nil))

			m.handleBatch(context.Background(), events) // arms the hold
			m.handleBatch(context.Background(), events) // held off

			if len(tracked) != 2 {
				t.Fatalf("tracked %d packets across two passes, want 2: a packet queued during the hold "+
					"that is never tracked can never be refunded", len(tracked))
			}
		})

		t.Run("the hold clears once the source answers", func(t *testing.T) {
			src := &mockSource{latest: 100, relayableErr: chain.Permanent(errors.New("attestor does not serve this chain"))}
			m := NewModule("l2->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})

			m.handleBatch(context.Background(), events)
			m.sourceProbeAt = time.Now().Add(-time.Second) // the operator fixed the config
			src.relayableErr = nil

			out := captureLog(func() { m.handleBatch(context.Background(), events) })
			if src.relayableCalls != 2 {
				t.Fatalf("source asked %d times; the probe after the hold expired did not happen", src.relayableCalls)
			}
			if !strings.Contains(out, "permanent-failure hold cleared") {
				t.Fatalf("a recovered source must say so:\n%s", out)
			}
			if !m.sourceProbeAt.IsZero() || m.sourceBackoff != 0 {
				t.Fatalf("hold state survived recovery: probeAt=%v backoff=%v", m.sourceProbeAt, m.sourceBackoff)
			}
		})

		// A transient failure must not arm the hold, or a source that is merely
		// unreachable for a moment goes quiet for up to fifteen minutes.
		t.Run("a transient failure does not arm the hold", func(t *testing.T) {
			src := &mockSource{latest: 100, relayableErr: chain.Transient(errors.New("attestor unavailable"))}
			m := NewModule("l2->cosmos", "client-0", src, &mockDest{}, &mockBuilder{})

			m.handleBatch(context.Background(), events)
			m.handleBatch(context.Background(), events)

			if src.relayableCalls != 2 {
				t.Fatalf("source asked %d times across two passes, want 2: a transient failure was treated as permanent", src.relayableCalls)
			}
		})
	})

	// A destination can commit part of a batch and then fail: a Cosmos batch that
	// splits submits its leading sub-batches, and the node can go away between two
	// of them. Those packets are on chain.
	//
	// Before the committed count reached the engine, both endings were wrong for
	// them. The transient ending re-queued the whole batch, so the delivered packets
	// were submitted a second time — and then reverted as duplicates, which the
	// engine reads as permanent and reports as DROP for packets that were in fact
	// delivered. The permanent ending isolated all of them, one wasted transaction
	// each, with the same misleading DROP.
	t.Run("partial commit", func(t *testing.T) {
		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("send-1")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("send-2")},
			{Type: chain.SendPacket, Height: 9, Raw: []byte("send-3")},
		}

		run := func(t *testing.T, dst *mockDest) (requeue []int, untracked [][]byte) {
			t.Helper()
			m := NewModule("test", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{},
				WithPacketTracker(
					func([]byte, uint64) bool { return true },
					func(pkt []byte) { untracked = append(untracked, pkt) },
				),
			)
			return m.handleBatch(context.Background(), batch), untracked
		}

		t.Run("a transient failure re-queues only the uncommitted tail", func(t *testing.T) {
			dst := &mockDest{relayErr: errors.New("node went away"), relayCommit: 2}
			requeue, untracked := run(t, dst)

			if len(requeue) != 1 || requeue[0] != 2 {
				t.Fatalf("requeue = %v, want only index 2; the first two are already on chain", requeue)
			}
			if len(untracked) != 2 {
				t.Fatalf("untracked %d packet(s) (%q), want the 2 that were committed", len(untracked), untracked)
			}
			if dst.relayCalls != 1 {
				t.Fatalf("relay calls = %d, want 1; a transient failure must not start isolating", dst.relayCalls)
			}
		})

		t.Run("a permanent failure isolates only the uncommitted tail", func(t *testing.T) {
			dst := &mockDest{relayErr: chain.Permanent(errors.New("reverted")), relayCommit: 2}
			requeue, untracked := run(t, dst)

			// The tail is a single packet, so it is dropped outright rather than
			// isolated — and the committed two must not be dropped with it.
			if len(requeue) != 0 {
				t.Fatalf("requeue = %v, want none: two landed and the third is permanently dead", requeue)
			}
			if len(untracked) != 2 {
				t.Fatalf("untracked %d packet(s) (%q), want the 2 that were committed", len(untracked), untracked)
			}
			if dst.relayCalls != 1 {
				t.Fatalf("relay calls = %d, want 1; the committed prefix must not be re-submitted for isolation", dst.relayCalls)
			}
		})

		t.Run("a batch that fully committed before the error re-queues nothing", func(t *testing.T) {
			dst := &mockDest{relayErr: errors.New("receipt wait timed out"), relayCommit: 3}
			requeue, untracked := run(t, dst)

			if len(requeue) != 0 {
				t.Fatalf("requeue = %v, want none: every packet is on chain", requeue)
			}
			if len(untracked) != 3 {
				t.Fatalf("untracked %d packet(s), want all 3", len(untracked))
			}
		})

		t.Run("a count larger than the batch is clamped, not indexed with", func(t *testing.T) {
			// An adapter breaking the contract must not take the relay loop down with
			// a slice bounds panic.
			dst := &mockDest{relayErr: errors.New("node went away"), relayCommit: 99}
			requeue, untracked := run(t, dst)

			if len(requeue) != 0 {
				t.Fatalf("requeue = %v, want none after clamping", requeue)
			}
			if len(untracked) != 3 {
				t.Fatalf("untracked %d packet(s), want at most the 3 in the batch", len(untracked))
			}
		})
	})

	// An unresolved abandoned nonce must not be isolated, and must not be dropped.
	//
	// Reported by @DongLieu on #485. The EVM sender marks this failure permanent so
	// nothing resubmits the payload, but permanent already meant "deterministic
	// revert" here, and that ending ISOLATES a multi-packet batch: each packet goes
	// out on its own, against a nonce cache the give-up path just invalidated, so
	// they can take LOWER nonces and mine before the original. The classification
	// meant to prevent a double execution was the thing causing it.
	t.Run("unresolved nonce is neither isolated nor dropped", func(t *testing.T) {
		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("send-1")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("send-2")},
			{Type: chain.SendPacket, Height: 9, Raw: []byte("send-3")},
		}

		// What the adapter actually hands up: chain.Permanent wrapping the sender's
		// error (chain/evm/destination.go:172), so the sentinels stay reachable
		// through it. A fixture that skipped the wrap would test a path production
		// never takes.
		unresolved := chain.Permanent(fmt.Errorf("gave up waiting: %w",
			errors.Join(services.ErrUnresolvedEVMNonce, services.ErrPermanentRelayFailure)))

		var untracked [][]byte
		dst := &mockDest{relayErr: unresolved}
		m := NewModule("test", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{},
			WithPacketTracker(
				func([]byte, uint64) bool { return true },
				func(pkt []byte) { untracked = append(untracked, pkt) },
			),
		)

		requeue := m.handleBatch(context.Background(), batch)

		if dst.relayCalls != 1 {
			t.Fatalf("RelayPackets called %d times, want 1: isolating resubmits every packet against a nonce "+
				"whose outcome is unknown, which is the double execution this state exists to prevent", dst.relayCalls)
		}
		if len(requeue) != 0 {
			t.Fatalf("requeue = %v, want none: a retry is a resubmission too", requeue)
		}
		if len(untracked) != 0 {
			t.Fatalf("untracked %d packet(s); the durable pending record is what settles or refunds them, "+
				"so dropping it strands the packet", len(untracked))
		}
	})

	// The ordinary permanent ending must keep isolating. The guard above is narrow,
	// and a guard that swallowed every permanent failure would trade one bug for the
	// loss of valid siblings that isolation exists to save.
	t.Run("an ordinary permanent failure still isolates", func(t *testing.T) {
		batch := []chain.Event{
			{Type: chain.SendPacket, Height: 7, Raw: []byte("send-1")},
			{Type: chain.SendPacket, Height: 8, Raw: []byte("send-2")},
		}

		dst := &mockDest{relayErr: chain.Permanent(fmt.Errorf("reverted: %w", services.ErrPermanentRelayFailure))}
		m := NewModule("test", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		m.handleBatch(context.Background(), batch)

		if dst.relayCalls < 2 {
			t.Fatalf("RelayPackets called %d times, want the batch plus one per packet: a deterministic revert "+
				"must still be isolated so the poison fails alone", dst.relayCalls)
		}
	})

	// The churn-gap climb has to exist on BOTH sides of prepareBatchUpdate. The
	// unfolded branch delegates to updateClientTo, which loops; the folded branch
	// built exactly one update and returned it, so a client behind a churn gap
	// advanced one hop per flush. With a 30s scan interval and a ceiling of 16 that
	// is eight minutes of re-queueing to cross a gap the unfolded path crosses in
	// one call -- on the packet-driven path, which is the one with packets waiting.
	t.Run("folded path climbs a churn gap in one flush", func(t *testing.T) {
		t.Run("crosses the gap in one flush and folds only the last rung", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &foldingMockDest{enabled: true}
			m := NewModule("test", "client-0", src, dst, &mockBuilder{hops: []uint64{30, 60, 90}})
			m.lastHeight = 10

			batch := []chain.Event{{Type: chain.SendPacket, Height: 90, Raw: []byte("a")}}
			if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
				t.Fatalf("a climbable gap must not re-queue the batch, got %v", rq)
			}

			// The two intermediate hops cannot carry the packets -- each lands below
			// the packet height -- so they go out on their own. Only the rung that
			// reaches the target is folded.
			if len(dst.updates) != 2 || dst.updates[0].Height != 30 || dst.updates[1].Height != 60 {
				t.Fatalf("standalone updates = %+v, want the two intermediate hops at 30 and 60", dst.updates)
			}
			if dst.foldCalls != 1 || len(dst.foldedUpdates) != 1 || dst.foldedUpdates[0].Height != 90 {
				t.Fatalf("want one folded update at the target height 90, calls=%d updates=%+v", dst.foldCalls, dst.foldedUpdates)
			}
			if len(dst.foldedPackets) != 1 || len(dst.foldedPackets[0]) != 1 {
				t.Fatalf("the packet must ride the final tx, got %+v", dst.foldedPackets)
			}
			if m.lastHeight != 90 {
				t.Fatalf("lastHeight = %d, want 90", m.lastHeight)
			}
		})

		// The ceiling is the whole reason the climb is safe to run on the packet path:
		// each hop is a proof and a transaction, so an unbounded one is a gas drain
		// that looks like progress.
		t.Run("stops at the ceiling instead of climbing forever", func(t *testing.T) {
			endless := make([]uint64, chain.MaxClientUpdateHops+5)
			for i := range endless {
				endless[i] = uint64(i + 1)
			}
			src := &mockSource{latest: 1000, relayable: 1000}
			dst := &foldingMockDest{enabled: true}
			m := NewModule("test", "client-0", src, dst, &mockBuilder{hops: endless})

			batch := []chain.Event{{Type: chain.SendPacket, Height: 900, Raw: []byte("a")}}
			if rq := m.handleBatch(context.Background(), batch); len(rq) != 1 {
				t.Fatalf("a batch behind an uncrossable gap must be re-queued, got %v", rq)
			}
			if len(dst.updates) > chain.MaxClientUpdateHops {
				t.Fatalf("submitted %d standalone updates past a ceiling of %d", len(dst.updates), chain.MaxClientUpdateHops)
			}
			if dst.foldCalls != 0 {
				t.Fatalf("nothing reached the target, so nothing may be folded; foldCalls=%d", dst.foldCalls)
			}
		})

		// A hop that installs nothing must end the climb rather than repeat it -- the
		// mirror of the same guard on updateClientTo. Without it a builder stuck on one
		// height burns the whole ceiling in proofs and transactions, per flush, on the
		// packet path.
		t.Run("a hop that does not advance ends the climb", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &foldingMockDest{enabled: true}
			m := NewModule("test", "client-0", src, dst, &mockBuilder{hops: []uint64{50, 50, 50, 50}})

			batch := []chain.Event{{Type: chain.SendPacket, Height: 90, Raw: []byte("a")}}
			if rq := m.handleBatch(context.Background(), batch); len(rq) != 1 {
				t.Fatalf("a packet above a stalled climb must be re-queued, got %v", rq)
			}
			if len(dst.updates) != 1 {
				t.Fatalf("submitted %d updates for a builder stuck at one height, want 1", len(dst.updates))
			}
			if dst.foldCalls != 0 {
				t.Fatalf("the climb never reached the target, so nothing may be folded; foldCalls=%d", dst.foldCalls)
			}
		})

		// An adapter that never hops must behave exactly as it did before the climb
		// existed: one Build, nothing submitted on its own, everything folded.
		t.Run("a builder that never hops is unchanged", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &foldingMockDest{enabled: true}
			b := &mockBuilder{forceHeight: 80}
			m := NewModule("test", "client-0", src, dst, b)
			m.lastHeight = 10

			batch := []chain.Event{{Type: chain.SendPacket, Height: 30, Raw: []byte("a")}}
			if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
				t.Fatalf("re-queued %v", rq)
			}
			if b.built != 1 || len(dst.updates) != 0 || dst.foldCalls != 1 {
				t.Fatalf("built=%d standalone=%d folds=%d; want one build, no standalone tx, one fold",
					b.built, len(dst.updates), dst.foldCalls)
			}
		})
	})

	t.Run("is serialized across callers", func(t *testing.T) {
		source := &overlapProbeSource{}
		m := NewModule("test", "client", source, &mockDest{}, &mockBuilder{})
		events := []chain.Event{{Type: chain.SendPacket, Sequence: 1, Height: 500}}

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.handleBatch(context.Background(), events)
			}()
		}
		wg.Wait()

		if source.overlap.Load() {
			t.Fatal("two handleBatch calls were inside the batch at once; batchMu is not held for the whole of it")
		}
	})

	// Reported by @neitdung. A folded transaction carries the client update AND the
	// packets, so a deterministic rejection of it says nothing about WHICH of the
	// two the chain refused -- but the code read every permanent failure as "the
	// packet is poison". A single packet was therefore dropped outright, and a batch
	// was retried as folded singletons carrying the SAME update, so each failed for
	// the same reason and every packet was dropped in turn.
	//
	// An ack has no independent escape: its counterparty already holds a receipt, so
	// nothing times it out and nothing refunds it. Dropping it on a failure never
	// shown to be its own strands the escrow.
	//
	// The fixture has to reject the update on BOTH paths, folded and standalone --
	// that is what "the update is the problem" means. A fold that fails while the
	// standalone update succeeds is the opposite case, and the last subtest pins
	// that the two are still told apart.
	t.Run("folded update rejection does not drop innocent packets", func(t *testing.T) {
		rejected := func() *foldingMockDest {
			dst := &foldingMockDest{enabled: true, foldErr: chain.Permanent(errors.New("client update rejected"))}
			dst.updateErr = chain.Permanent(errors.New("client update rejected"))
			return dst
		}

		t.Run("a single ack is held, not dropped", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := rejected()
			m := NewModule("test", "client-0", src, dst, &mockBuilder{forceHeight: 80})
			m.lastHeight = 10

			batch := []chain.Event{{Type: chain.AckPacket, Sequence: 7, Height: 30, Raw: []byte("a")}}
			requeue := m.handleBatch(context.Background(), batch)

			if len(requeue) != 1 {
				t.Fatalf("re-queued %v, want the ack held: the update was refused on its own, "+
					"so nothing was ever shown about the packet", requeue)
			}
			if m.lastHeight != 10 {
				t.Fatalf("lastHeight = %d, want 10: an update the chain refused must not be recorded", m.lastHeight)
			}
		})

		// The batch shape is the worse one: isolation would have retried every packet
		// with the same rejected update and dropped all of them.
		t.Run("a whole batch is held, not dropped one by one", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := rejected()
			m := NewModule("test", "client-0", src, dst, &mockBuilder{forceHeight: 80})
			m.lastHeight = 10

			batch := []chain.Event{
				{Type: chain.AckPacket, Sequence: 1, Height: 30, Raw: []byte("a")},
				{Type: chain.AckPacket, Sequence: 2, Height: 31, Raw: []byte("b")},
				{Type: chain.TimeoutPacket, Sequence: 3, Height: 32, Raw: []byte("c")},
			}
			requeue := m.handleBatch(context.Background(), batch)

			if len(requeue) != len(batch) {
				t.Fatalf("re-queued %v, want all %d held", requeue, len(batch))
			}
		})

		// Second round, also @neitdung. The update landing alone clears the UPDATE,
		// and nothing more: the folded transaction was one atomic unit, and a unit can
		// be refused for what the two are together (its gas envelope, its route)
		// while each half passes on its own. The first fix drew the stronger
		// conclusion -- "the update landed, so the packet is poison" -- and dropped a
		// single packet on the folded error without ever sending it by itself. The
		// batch shape went through packet-only isolation; the singleton skipped it.
		t.Run("a packet that fails only in the folded composition is delivered, not dropped", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &foldingMockDest{enabled: true, foldErr: chain.Permanent(errors.New("folded tx reverted"))}
			m := NewModule("test", "client-0", src, dst, &mockBuilder{forceHeight: 80})
			m.lastHeight = 10

			batch := []chain.Event{{Type: chain.AckPacket, Sequence: 7, Height: 30, Raw: []byte("a")}}
			if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
				t.Fatalf("re-queued %v, want nothing outstanding: the packet relays on its own", rq)
			}
			if dst.relayCalls != 1 {
				t.Fatalf("packet-only relay calls = %d, want 1: the folded error says nothing about the packet alone", dst.relayCalls)
			}
			if len(dst.relayed) != 1 || dst.relayed[0].Sequence != 7 {
				t.Fatalf("delivered %+v, want the ack: it was dropped on a failure never shown to be its own", dst.relayed)
			}
			if m.lastHeight != 80 {
				t.Fatalf("lastHeight = %d, want 80: the standalone update installed", m.lastHeight)
			}
			if len(dst.updates) != 1 || dst.updates[0].Height != 80 {
				t.Fatalf("standalone update calls = %+v, want exactly the folded update", dst.updates)
			}
		})

		// The control: a packet that fails on its own too is still dropped -- after
		// that attempt, on that attempt's error, not on the folded one.
		t.Run("a packet that really is poison is dropped once it has failed alone", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &foldingMockDest{enabled: true, foldErr: chain.Permanent(errors.New("folded tx reverted"))}
			dst.poison = map[string]bool{"a": true}
			m := NewModule("test", "client-0", src, dst, &mockBuilder{forceHeight: 80})
			m.lastHeight = 10

			batch := []chain.Event{{Type: chain.AckPacket, Sequence: 7, Height: 30, Raw: []byte("a")}}
			if rq := m.handleBatch(context.Background(), batch); len(rq) != 0 {
				t.Fatalf("re-queued %v; the packet failed on its own, so it is dropped", rq)
			}
			if dst.relayCalls != 1 {
				t.Fatalf("packet-only relay calls = %d, want 1: dropping needs the packet's own failure first", dst.relayCalls)
			}
			if len(dst.relayed) != 0 {
				t.Fatalf("delivered %+v, want nothing: the packet is poison", dst.relayed)
			}
			if m.lastHeight != 80 {
				t.Fatalf("lastHeight = %d, want 80: the standalone update installed", m.lastHeight)
			}
		})
	})
}

// A dropped packet is logged at the level of what is left to do for it. A SendPacket
// still has an exit (the timeout scanner refunds it), so dropping one is routine and
// a warning; an ack or a timeout has none, so dropping one is an error. All three
// DROP sites must say the same, or alerting on level=error pages on an ordinary
// timeout at one of them and stays quiet about a stranded ack at another.
//
// captureLog sees the raw standard-library line, with the token still on it.
func TestDrop_LevelFollowsTheEventType(t *testing.T) {
	dropLine := func(t *testing.T, out string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "DROP packet") {
				return line
			}
		}
		t.Fatalf("no DROP line was logged:\n%s", out)
		return ""
	}

	sites := []struct {
		name string
		run  func(typ chain.EventType) []int
	}{
		{"permanent proof failure", func(typ chain.EventType) []int {
			src := &mockSource{latest: 100, permanentMembershipOn: map[string]bool{"dead": true}}
			m := NewModule("test", "client-0", src, &mockDest{}, &mockBuilder{})
			return m.handleBatch(context.Background(), []chain.Event{{Type: typ, Sequence: 7, Height: 4, Raw: []byte("dead")}})
		}},
		{"single packet reverted on chain", func(typ chain.EventType) []int {
			dst := &mockDest{relayErr: chain.Permanent(errors.New("reverted"))}
			m := NewModule("test", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})
			return m.handleBatch(context.Background(), []chain.Event{{Type: typ, Sequence: 7, Height: 5, Raw: []byte("p")}})
		}},
		{"poison packet isolated from its batch", func(typ chain.EventType) []int {
			dst := &mockDest{poison: map[string]bool{"bad": true}}
			m := NewModule("test", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})
			return m.handleBatch(context.Background(), []chain.Event{
				{Type: typ, Sequence: 6, Height: 4, Raw: []byte("good")},
				{Type: typ, Sequence: 7, Height: 5, Raw: []byte("bad")},
			})
		}},
	}
	types := []struct {
		name string
		typ  chain.EventType
		want string
	}{
		{"send", chain.SendPacket, logbridge.Warn},
		{"ack", chain.AckPacket, logbridge.Error},
	}

	for _, site := range sites {
		for _, c := range types {
			t.Run(site.name+"/"+c.name, func(t *testing.T) {
				var rq []int
				out := captureLog(func() { rq = site.run(c.typ) })
				if len(rq) != 0 {
					t.Fatalf("a permanent failure must drop, not re-queue, got %v", rq)
				}
				line := dropLine(t, out)
				if want := c.want + "[test Relay] DROP packet"; !strings.HasPrefix(line, want) {
					t.Fatalf("a dropped %s packet must be logged at %q, got:\n%s", c.name, strings.TrimSpace(c.want), line)
				}
			})
		}
	}
}

func TestDropToken(t *testing.T) {
	for _, c := range []struct {
		typ  chain.EventType
		want string
	}{
		{chain.SendPacket, logbridge.Warn},
		{chain.AckPacket, logbridge.Error},
		{chain.TimeoutPacket, logbridge.Error},
		// A type this code has never heard of has no known exit, so it is loud.
		{chain.EventType(99), logbridge.Error},
	} {
		if got := dropToken(c.typ); got != c.want {
			t.Errorf("dropToken(%s) = %q, want %q", c.typ, got, c.want)
		}
	}
}
