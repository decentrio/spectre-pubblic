package services

import (
	"context"
	"sync"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func makeCosmosPacket(seq uint64, pktType CosmosPacketType) CosmosPacket {
	return CosmosPacket{
		Type: pktType,
		Packet: &channeltypesv2.Packet{
			Sequence: seq,
		},
	}
}

func makeEthPacket(seq uint64, pktType EthPacketType) EthPacket {
	return EthPacket{
		Type: pktType,
		Packet: &channeltypesv2.Packet{
			Sequence: seq,
		},
	}
}

func TestNewBatchBuilder(t *testing.T) {
	bb := NewBatchBuilder()
	if bb == nil {
		t.Fatal("expected non-nil BatchBuilder")
	}
	if len(bb.cosmosPackets) != 0 {
		t.Fatalf("expected empty cosmos packets, got %d", len(bb.cosmosPackets))
	}
	if len(bb.ethPackets) != 0 {
		t.Fatalf("expected empty eth packets, got %d", len(bb.ethPackets))
	}
	if bb.cosmosTimestamp.IsZero() {
		t.Fatal("expected cosmos timestamp to be set")
	}
	if bb.ethTimestamp.IsZero() {
		t.Fatal("expected eth timestamp to be set")
	}
	if bb.PendingTracker == nil {
		t.Fatal("expected non-nil cosmos pending tracker")
	}
	if bb.EthPendingTracker == nil {
		t.Fatal("expected non-nil eth pending tracker")
	}
	if bb.L2PendingTracker == nil {
		t.Fatal("expected non-nil l2 pending tracker")
	}
}

func TestAddCosmos(t *testing.T) {
	bb := NewBatchBuilder()
	bb.AddCosmos(makeCosmosPacket(1, CosmosSend))

	bb.cosmosMtx.Lock()
	defer bb.cosmosMtx.Unlock()

	if len(bb.cosmosPackets) != 1 {
		t.Fatalf("expected 1 cosmos packet, got %d", len(bb.cosmosPackets))
	}
	if bb.cosmosPackets[0].Packet.Sequence != 1 {
		t.Fatalf("expected sequence 1, got %d", bb.cosmosPackets[0].Packet.Sequence)
	}
	if bb.cosmosPackets[0].Type != CosmosSend {
		t.Fatalf("expected CosmosSend, got %d", bb.cosmosPackets[0].Type)
	}

	t.Run("multiple", func(t *testing.T) {
		bb := NewBatchBuilder()
		count := 5
		for i := 0; i < count; i++ {
			bb.AddCosmos(makeCosmosPacket(uint64(i+1), CosmosSend))
		}

		bb.cosmosMtx.Lock()
		defer bb.cosmosMtx.Unlock()

		if len(bb.cosmosPackets) != count {
			t.Fatalf("expected %d cosmos packets, got %d", count, len(bb.cosmosPackets))
		}
		for i := 0; i < count; i++ {
			if bb.cosmosPackets[i].Packet.Sequence != uint64(i+1) {
				t.Fatalf("packet %d: expected sequence %d, got %d", i, i+1, bb.cosmosPackets[i].Packet.Sequence)
			}
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		bb := NewBatchBuilder()
		goroutines := 50
		packetsPerGoroutine := 20
		total := goroutines * packetsPerGoroutine

		var wg sync.WaitGroup
		wg.Add(goroutines)

		for g := 0; g < goroutines; g++ {
			go func(offset int) {
				defer wg.Done()
				for i := 0; i < packetsPerGoroutine; i++ {
					bb.AddCosmos(makeCosmosPacket(uint64(offset*packetsPerGoroutine+i), CosmosSend))
				}
			}(g)
		}

		wg.Wait()

		bb.cosmosMtx.Lock()
		defer bb.cosmosMtx.Unlock()

		if len(bb.cosmosPackets) != total {
			t.Fatalf("expected %d cosmos packets, got %d", total, len(bb.cosmosPackets))
		}
	})
}

func TestAddEth(t *testing.T) {
	bb := NewBatchBuilder()
	bb.AddEth(makeEthPacket(1, EthSend))

	bb.ethMtx.Lock()
	defer bb.ethMtx.Unlock()

	if len(bb.ethPackets) != 1 {
		t.Fatalf("expected 1 eth packet, got %d", len(bb.ethPackets))
	}
	if bb.ethPackets[0].Packet.Sequence != 1 {
		t.Fatalf("expected sequence 1, got %d", bb.ethPackets[0].Packet.Sequence)
	}
	if bb.ethPackets[0].Type != EthSend {
		t.Fatalf("expected EthSend, got %d", bb.ethPackets[0].Type)
	}

	t.Run("multiple", func(t *testing.T) {
		bb := NewBatchBuilder()
		count := 5
		for i := 0; i < count; i++ {
			bb.AddEth(makeEthPacket(uint64(i+1), EthSend))
		}

		bb.ethMtx.Lock()
		defer bb.ethMtx.Unlock()

		if len(bb.ethPackets) != count {
			t.Fatalf("expected %d eth packets, got %d", count, len(bb.ethPackets))
		}
		for i := 0; i < count; i++ {
			if bb.ethPackets[i].Packet.Sequence != uint64(i+1) {
				t.Fatalf("packet %d: expected sequence %d, got %d", i, i+1, bb.ethPackets[i].Packet.Sequence)
			}
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		bb := NewBatchBuilder()
		goroutines := 50
		packetsPerGoroutine := 20
		total := goroutines * packetsPerGoroutine

		var wg sync.WaitGroup
		wg.Add(goroutines)

		for g := 0; g < goroutines; g++ {
			go func(offset int) {
				defer wg.Done()
				for i := 0; i < packetsPerGoroutine; i++ {
					bb.AddEth(makeEthPacket(uint64(offset*packetsPerGoroutine+i), EthSend))
				}
			}(g)
		}

		wg.Wait()

		bb.ethMtx.Lock()
		defer bb.ethMtx.Unlock()

		if len(bb.ethPackets) != total {
			t.Fatalf("expected %d eth packets, got %d", total, len(bb.ethPackets))
		}
	})
}

func TestPendingPacketsTimedOutAtTimestamp(t *testing.T) {
	pending := []pendingPacketInfo{
		{Packet: channeltypesv2.Packet{Sequence: 1, TimeoutTimestamp: 0}},
		{Packet: channeltypesv2.Packet{Sequence: 2, TimeoutTimestamp: 100}},
		{Packet: channeltypesv2.Packet{Sequence: 3, TimeoutTimestamp: 150}},
	}

	expired := pendingPacketsTimedOutAtTimestamp(pending, 120)
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired packet, got %d", len(expired))
	}
	if expired[0].Packet.Sequence != 2 {
		t.Fatalf("expected sequence 2, got %d", expired[0].Packet.Sequence)
	}
}

func TestCheckCosmos(t *testing.T) {
	t.Run("exceeds batch size", func(t *testing.T) {
		bb := NewBatchBuilder()
		config := BatchConfig{
			BatchSize:    3,
			BatchPeriods: time.Minute * 10,
		}

		for i := 0; i < 5; i++ {
			bb.AddCosmos(makeCosmosPacket(uint64(i+1), CosmosSend))
		}

		ch := make(chan CosmosBatch, 1)
		bb.CheckCosmos(context.Background(), config, ch)

		// Flush chunks at most BatchSize (3) packets per call; the remaining 2
		// stay in the queue for the next tick to chunk-flush again.
		select {
		case batch := <-ch:
			if len(batch.Packets) != 3 {
				t.Fatalf("expected 3 packets in batch (BatchSize chunk), got %d", len(batch.Packets))
			}
		default:
			t.Fatal("expected cosmos batch to be sent")
		}

		bb.cosmosMtx.Lock()
		defer bb.cosmosMtx.Unlock()
		if len(bb.cosmosPackets) != 2 {
			t.Fatalf("expected 2 cosmos packets remaining in queue, got %d", len(bb.cosmosPackets))
		}
	})

	t.Run("below size and before period", func(t *testing.T) {
		bb := NewBatchBuilder()
		config := BatchConfig{
			BatchSize:    10,
			BatchPeriods: time.Minute * 10,
		}

		bb.AddCosmos(makeCosmosPacket(1, CosmosSend))
		bb.AddCosmos(makeCosmosPacket(2, CosmosAck))

		ch := make(chan CosmosBatch, 1)
		bb.CheckCosmos(context.Background(), config, ch)

		select {
		case <-ch:
			t.Fatal("expected no cosmos batch to be sent")
		default:
		}

		bb.cosmosMtx.Lock()
		defer bb.cosmosMtx.Unlock()
		if len(bb.cosmosPackets) != 2 {
			t.Fatalf("expected cosmos packets to remain, got %d", len(bb.cosmosPackets))
		}
	})

	// TestCheckCosmos/returns and restores on cancel pins the property the fix exists
	// for: the chunk is sliced off the queue BEFORE the handoff, so a send that
	// never completes must not take those packets with it.
	//
	// The channel is deliberately unbuffered with no receiver, which is what a
	// shutting-down consumer looks like. Without the ctx guard this blocks forever
	// and the packet is gone from the queue — the test hangs and then fails on the
	// 5s deadline.
	t.Run("returns and restores on cancel", func(t *testing.T) {
		b := NewBatchBuilder()
		b.AddCosmos(cosmosPacketSeq(1))

		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan CosmosBatch) // no receiver, no buffer

		done := make(chan struct{})
		go func() {
			b.CheckCosmos(ctx, cancelTestConfig(), ch)
			close(done)
		}()

		// Let CheckCosmos reach the blocked send, then shut down.
		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("CheckCosmos did not return after cancellation: the send is unguarded")
		}

		b.cosmosMtx.Lock()
		got := len(b.cosmosPackets)
		var seq uint64
		if got == 1 {
			seq = b.cosmosPackets[0].Packet.Sequence
		}
		b.cosmosMtx.Unlock()

		if got != 1 {
			t.Fatalf("packet count after cancelled flush = %d, want 1 (the chunk must go back on the queue)", got)
		}
		if seq != 1 {
			t.Fatalf("restored packet sequence = %d, want 1", seq)
		}
	})

	// TestCheckCosmos/restores chunk ahead of newer arrivals: a restored chunk must go back at
	// the HEAD, so shutdown does not silently reorder the queue relative to packets
	// that arrived while the flush was blocked.
	t.Run("restores chunk ahead of newer arrivals", func(t *testing.T) {
		b := NewBatchBuilder()
		b.AddCosmos(cosmosPacketSeq(1))

		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan CosmosBatch)

		done := make(chan struct{})
		go func() {
			b.CheckCosmos(ctx, cancelTestConfig(), ch)
			close(done)
		}()

		time.Sleep(50 * time.Millisecond)
		b.AddCosmos(cosmosPacketSeq(2)) // arrives while the flush is parked
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("CheckCosmos did not return after cancellation")
		}

		b.cosmosMtx.Lock()
		defer b.cosmosMtx.Unlock()
		if len(b.cosmosPackets) != 2 {
			t.Fatalf("packet count = %d, want 2", len(b.cosmosPackets))
		}
		if got := b.cosmosPackets[0].Packet.Sequence; got != 1 {
			t.Fatalf("head sequence = %d, want 1 (the restored chunk must precede newer arrivals)", got)
		}
	})

	// TestCheckCosmos/still flushes when the consumer is ready guards against the fix
	// turning every flush into a cancellation: a live consumer must still receive.
	t.Run("still flushes when the consumer is ready", func(t *testing.T) {
		b := NewBatchBuilder()
		b.AddCosmos(cosmosPacketSeq(3))

		ch := make(chan CosmosBatch, 1)
		b.CheckCosmos(context.Background(), cancelTestConfig(), ch)

		select {
		case batch := <-ch:
			if len(batch.Packets) != 1 || batch.Packets[0].Packet.Sequence != 3 {
				t.Fatalf("unexpected batch %+v", batch)
			}
		default:
			t.Fatal("no batch delivered to a ready consumer")
		}

		b.cosmosMtx.Lock()
		defer b.cosmosMtx.Unlock()
		if len(b.cosmosPackets) != 0 {
			t.Fatalf("queue = %d after a successful flush, want 0", len(b.cosmosPackets))
		}
	})

	t.Run("cancelled handoff restores queue and clears in flight", func(t *testing.T) {
		bb := NewBatchBuilder()
		packet := makeCosmosPacket(1, CosmosSend)
		packet.BlockNumber = 77
		bb.AddCosmos(packet)
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan CosmosBatch)
		done := make(chan struct{})
		go func() {
			bb.CheckCosmos(ctx, BatchConfig{BatchSize: 1, BatchPeriods: 0}, ch)
			close(done)
		}()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancelled handoff did not return")
		}
		if got := bb.LowestUnsubmittedCosmosHeight(); got != 77 {
			t.Fatalf("restored watermark = %d, want 77", got)
		}
		bb.cosmosMtx.Lock()
		inFlight := len(bb.cosmosInFlight)
		queued := len(bb.cosmosPackets)
		bb.cosmosMtx.Unlock()
		if inFlight != 0 || queued != 1 {
			t.Fatalf("after cancel: inFlight=%d queued=%d", inFlight, queued)
		}
	})
}

func TestCheckEth(t *testing.T) {
	t.Run("exceeds batch size", func(t *testing.T) {
		bb := NewBatchBuilder()
		config := BatchConfig{
			BatchSize:    3,
			BatchPeriods: time.Minute * 10,
		}

		for i := 0; i < 5; i++ {
			bb.AddEth(makeEthPacket(uint64(i+1), EthSend))
		}

		ch := make(chan EthBatch, 1)
		bb.CheckEth(context.Background(), config, ch)

		select {
		case batch := <-ch:
			if len(batch.Packets) != 3 {
				t.Fatalf("expected 3 packets in batch (BatchSize chunk), got %d", len(batch.Packets))
			}
		default:
			t.Fatal("expected eth batch to be sent")
		}

		bb.ethMtx.Lock()
		defer bb.ethMtx.Unlock()
		if len(bb.ethPackets) != 2 {
			t.Fatalf("expected 2 eth packets remaining in queue, got %d", len(bb.ethPackets))
		}
	})

	t.Run("past batch periods", func(t *testing.T) {
		bb := NewBatchBuilder()
		bb.ethTimestamp = time.Now().Add(-time.Minute * 5)

		config := BatchConfig{
			BatchSize:    10,
			BatchPeriods: time.Second,
		}

		bb.AddEth(makeEthPacket(1, EthSend))
		bb.AddEth(makeEthPacket(2, EthWriteAck))

		ch := make(chan EthBatch, 1)
		bb.CheckEth(context.Background(), config, ch)

		select {
		case batch := <-ch:
			if len(batch.Packets) != 2 {
				t.Fatalf("expected 2 packets in batch, got %d", len(batch.Packets))
			}
		default:
			t.Fatal("expected eth batch to be sent")
		}

		bb.ethMtx.Lock()
		defer bb.ethMtx.Unlock()
		if len(bb.ethPackets) != 0 {
			t.Fatalf("expected eth packets to be cleared, got %d", len(bb.ethPackets))
		}
	})

	t.Run("below size and before period", func(t *testing.T) {
		bb := NewBatchBuilder()
		config := BatchConfig{
			BatchSize:    10,
			BatchPeriods: time.Minute * 10,
		}

		bb.AddEth(makeEthPacket(1, EthSend))
		bb.AddEth(makeEthPacket(2, EthWriteAck))

		ch := make(chan EthBatch, 1)
		bb.CheckEth(context.Background(), config, ch)

		select {
		case <-ch:
			t.Fatal("expected no eth batch to be sent")
		default:
		}

		bb.ethMtx.Lock()
		defer bb.ethMtx.Unlock()
		if len(bb.ethPackets) != 2 {
			t.Fatalf("expected eth packets to remain, got %d", len(bb.ethPackets))
		}
	})

	// TestCheckEth/returns and restores on cancel — the ETH mirror of the above. The two
	// paths are edited in lockstep by convention; this keeps that honest.
	t.Run("returns and restores on cancel", func(t *testing.T) {
		b := NewBatchBuilder()
		b.AddEth(ethPacketSeq(7))

		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan EthBatch)

		done := make(chan struct{})
		go func() {
			b.CheckEth(ctx, cancelTestConfig(), ch)
			close(done)
		}()

		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("CheckEth did not return after cancellation: the send is unguarded")
		}

		b.ethMtx.Lock()
		got := len(b.ethPackets)
		var seq uint64
		if got == 1 {
			seq = b.ethPackets[0].Packet.Sequence
		}
		b.ethMtx.Unlock()

		if got != 1 {
			t.Fatalf("packet count after cancelled flush = %d, want 1 (the chunk must go back on the queue)", got)
		}
		if seq != 7 {
			t.Fatalf("restored packet sequence = %d, want 7", seq)
		}
	})

	// TestCheckEth/waiting held back verifies a packet under its NotBefore backoff is
	// NOT flushed (quiet wait), and IS flushed once the backoff elapses.
	t.Run("waiting held back", func(t *testing.T) {
		b := NewBatchBuilder()
		cfg := BatchConfig{BatchSize: 5, BatchPeriods: 0} // period 0 => time-based flush always ready

		// A waiting packet (NotBefore in the future) must be held back: no flush.
		b.RequeueEthWaiting([]EthPacket{waitingEthPkt(1)})
		ch := make(chan EthBatch, 1)
		b.CheckEth(context.Background(), cfg, ch)
		select {
		case <-ch:
			t.Fatal("waiting packet must not flush before its backoff elapses")
		default:
		}

		// Force the backoff to have elapsed and re-check: now it flushes.
		b.ethMtx.Lock()
		for i := range b.ethPackets {
			b.ethPackets[i].NotBefore = time.Now().Add(-time.Second)
		}
		b.ethMtx.Unlock()
		b.CheckEth(context.Background(), cfg, ch)
		select {
		case batch := <-ch:
			if len(batch.Packets) != 1 || batch.Packets[0].Packet.Sequence != 1 {
				t.Fatalf("expected the ready packet, got %+v", batch.Packets)
			}
		default:
			t.Fatal("ready packet must flush once its backoff elapsed")
		}
	})
}

func cancelTestConfig() BatchConfig {
	// BatchSize 1 with a zero period flushes on the first packet, so the test
	// reaches the handoff immediately.
	return BatchConfig{BatchSize: 1, BatchPeriods: 0}
}

func cosmosPacketSeq(seq uint64) CosmosPacket {
	return CosmosPacket{
		Type:   CosmosSend,
		Packet: &channeltypesv2.Packet{Sequence: seq, SourceClient: "08-wasm-0", DestinationClient: "client-0"},
	}
}

func ethPacketSeq(seq uint64) EthPacket {
	return EthPacket{
		Type:   EthSend,
		Packet: &channeltypesv2.Packet{Sequence: seq, SourceClient: "client-0", DestinationClient: "08-wasm-0"},
	}
}

func TestBatchBuilder(t *testing.T) {
	// TestBatchBuilder/cancelled flush strands no packet at production capacity is the case the first
	// round of tests missed, reported by @neitdung.
	//
	// Those tests used an unbuffered channel written by hand, so they only ever
	// exercised the ctx.Done() arm. Production used a 16-slot buffer, where the
	// send ARM WINS: it is ready, so select takes it, the batch lands in the buffer,
	// and the consumer — selecting on the same cancellation — returns without ever
	// receiving it. The chunk was already sliced off the queue, so it existed
	// nowhere. Guarding the send with ctx.Done() does not help at all in that shape.
	//
	// The channel here is built exactly as the source bridges build it, from the
	// same constant, so setting BatchHandoffCapacity back above zero fails this test
	// rather than silently reopening the hole.
	t.Run("cancelled flush strands no packet at production capacity", func(t *testing.T) {
		t.Parallel()

		t.Run("cosmos", func(t *testing.T) {
			t.Parallel()

			b := NewBatchBuilder()
			b.AddCosmos(cosmosPacketSeq(1))

			ctx, cancel := context.WithCancel(context.Background())
			// Exactly what chain/cosmos/source.go builds. No receiver: a consumer
			// that has already returned on shutdown.
			ch := make(chan CosmosBatch, BatchHandoffCapacity)

			done := make(chan struct{})
			go func() {
				b.CheckCosmos(ctx, cancelTestConfig(), ch)
				close(done)
			}()

			time.Sleep(50 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("CheckCosmos did not return after cancellation")
			}

			// The packet must be back in the builder -- not sitting in a channel
			// buffer that nothing will ever read.
			b.cosmosMtx.Lock()
			got := len(b.cosmosPackets)
			b.cosmosMtx.Unlock()
			if got != 1 {
				t.Fatalf("queue holds %d packet(s), want 1; the batch was stranded in the handoff "+
					"(BatchHandoffCapacity = %d must be 0)", got, BatchHandoffCapacity)
			}
		})

		t.Run("eth", func(t *testing.T) {
			t.Parallel()

			b := NewBatchBuilder()
			b.AddEth(ethPacketSeq(7))

			ctx, cancel := context.WithCancel(context.Background())
			ch := make(chan EthBatch, BatchHandoffCapacity)

			done := make(chan struct{})
			go func() {
				b.CheckEth(ctx, cancelTestConfig(), ch)
				close(done)
			}()

			time.Sleep(50 * time.Millisecond)
			cancel()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("CheckEth did not return after cancellation")
			}

			b.ethMtx.Lock()
			got := len(b.ethPackets)
			b.ethMtx.Unlock()
			if got != 1 {
				t.Fatalf("queue holds %d packet(s), want 1; the batch was stranded in the handoff "+
					"(BatchHandoffCapacity = %d must be 0)", got, BatchHandoffCapacity)
			}
		})
	})

	t.Run("lowest unsubmitted includes queue and in flight", func(t *testing.T) {
		bb := NewBatchBuilder()
		a := makeCosmosPacket(1, CosmosSend)
		a.BlockNumber = 300
		b := makeCosmosPacket(2, CosmosSend)
		b.BlockNumber = 200
		bb.AddCosmos(a)
		bb.AddCosmos(b)

		ch := make(chan CosmosBatch, 1)
		bb.CheckCosmos(context.Background(), BatchConfig{BatchSize: 1, BatchPeriods: 0}, ch)
		batch := <-ch
		if got := bb.LowestUnsubmittedCosmosHeight(); got != 200 {
			t.Fatalf("lowest while queued+in-flight = %d, want 200", got)
		}
		bb.ReleaseCosmosInFlight(batch)
		if got := bb.LowestUnsubmittedCosmosHeight(); got != 200 {
			t.Fatalf("lowest after release = %d, want queued height 200", got)
		}
	})

	t.Run("equal in flight lows are counted separately", func(t *testing.T) {
		bb := NewBatchBuilder()
		for seq := uint64(1); seq <= 2; seq++ {
			packet := makeCosmosPacket(seq, CosmosSend)
			packet.BlockNumber = 50
			bb.AddCosmos(packet)
		}
		ch := make(chan CosmosBatch, 2)
		bb.CheckCosmos(context.Background(), BatchConfig{BatchSize: 1, BatchPeriods: 0}, ch)
		bb.CheckCosmos(context.Background(), BatchConfig{BatchSize: 1, BatchPeriods: 0}, ch)
		first, second := <-ch, <-ch
		bb.ReleaseCosmosInFlight(first)
		if got := bb.LowestUnsubmittedCosmosHeight(); got != 50 {
			t.Fatalf("one equal-low chunk still in flight, got %d", got)
		}
		bb.ReleaseCosmosInFlight(second)
		if got := bb.LowestUnsubmittedCosmosHeight(); got != 0 {
			t.Fatalf("all chunks released, got %d", got)
		}
	})
}

func queuedPacket(sequence uint64, src, dst string) CosmosPacket {
	return CosmosPacket{
		Type: CosmosSend,
		Packet: &channeltypesv2.Packet{
			Sequence: sequence, SourceClient: src, DestinationClient: dst,
		},
	}
}

// queuedPayload is the same packet with a distinguishing payload and timeout --
// what makes a REPLAYED sequence a different packet rather than the same one.
func queuedPayload(sequence uint64, src, dst, value string, timeout uint64) CosmosPacket {
	p := queuedPacket(sequence, src, dst)
	p.Packet.TimeoutTimestamp = timeout
	p.Packet.Payloads = []channeltypesv2.Payload{{
		SourcePort: "transfer", DestinationPort: "transfer",
		Version: "ics20-2", Encoding: "application/x-solidity-abi",
		Value: []byte(value),
	}}
	return p
}

// The mirror of the L2 fix. A recovery pass can queue a send and then read the
// acknowledge_packet that closes it, further down the same list. Settling only
// the tracker leaves the queue to relay a finished packet -- and the relay
// re-adds the tracker entry that settling just removed, after the cursor moved
// past the terminal event that would clear it again.
func TestDropCosmosQueued(t *testing.T) {
	newBuilder := func(t *testing.T) *BatchBuilder {
		t.Helper()
		return NewBatchBuilder()
	}

	t.Run("removes the settled packet and reports it", func(t *testing.T) {
		b := newBuilder(t)
		b.AddCosmos(queuedPacket(5, "08-wasm-7", "client-0"))
		b.AddCosmos(queuedPacket(6, "08-wasm-7", "client-0"))

		if !b.DropCosmosQueued(*queuedPacket(5, "08-wasm-7", "client-0").Packet) {
			t.Fatal("a queued packet that settled must be reported as removed")
		}
		cosmos, _ := b.QueueDepths()
		if cosmos != 1 {
			t.Fatalf("queue depth = %d, want 1", cosmos)
		}
	})

	t.Run("a packet that is not queued reports false", func(t *testing.T) {
		b := newBuilder(t)
		b.AddCosmos(queuedPacket(6, "08-wasm-7", "client-0"))
		if b.DropCosmosQueued(*queuedPacket(5, "08-wasm-7", "client-0").Packet) {
			t.Fatal("nothing was queued for that identity; claiming a removal makes the log line a lie")
		}
		if cosmos, _ := b.QueueDepths(); cosmos != 1 {
			t.Fatalf("queue depth = %d, want the untouched 1", cosmos)
		}
	})

	// Sequence alone is not an identity: two clients number their packets
	// independently, so matching on it would drop an unrelated live packet.
	t.Run("the same sequence on another client survives", func(t *testing.T) {
		b := newBuilder(t)
		b.AddCosmos(queuedPacket(5, "08-wasm-7", "client-0"))
		b.AddCosmos(queuedPacket(5, "08-wasm-9", "client-0"))

		b.DropCosmosQueued(*queuedPacket(5, "08-wasm-7", "client-0").Packet)
		if cosmos, _ := b.QueueDepths(); cosmos != 1 {
			t.Fatalf("queue depth = %d, want 1 -- the other client's packet must remain", cosmos)
		}
	})

	// A client migration keeps the client id and restarts sequences from 1 --
	// create-clients-eth repoints the existing client rather than adding one, which
	// is a documented operational step here, not a hypothetical. So seq=N names two
	// different packets over the life of one client, and a stale terminal event for
	// the OLD seq=N must not drop the NEW one: that packet would never be relayed,
	// and nothing downstream is left to notice.
	t.Run("keeps a replayed sequence", func(t *testing.T) {
		b := NewBatchBuilder()
		fresh := queuedPayload(5, "08-wasm-7", "client-0", "new-transfer", 2000)
		b.AddCosmos(fresh)

		stale := queuedPayload(5, "08-wasm-7", "client-0", "old-transfer", 1000)
		if b.DropCosmosQueued(*stale.Packet) {
			t.Fatal("a terminal event for the pre-migration seq=5 claimed to remove the post-migration one")
		}
		if cosmos, _ := b.QueueDepths(); cosmos != 1 {
			t.Fatalf("queue depth = %d, want the replayed packet still queued: it has never been relayed", cosmos)
		}

		// The packet it really names is still removable.
		if !b.DropCosmosQueued(*fresh.Packet) {
			t.Fatal("the queued packet's own identity must still match")
		}
		if cosmos, _ := b.QueueDepths(); cosmos != 0 {
			t.Fatalf("queue depth = %d, want 0", cosmos)
		}
	})
}

func waitingEthPkt(seq uint64) EthPacket {
	return EthPacket{Type: EthSend, Packet: &channeltypesv2.Packet{Sequence: seq, SourceClient: "c"}}
}

// TestNextWaitingBackoff verifies the exponential growth + cap (3s,6s,12s,15s…).
func TestNextWaitingBackoff(t *testing.T) {
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 15 * time.Second, 15 * time.Second}
	for i, w := range want {
		if got := nextWaitingBackoff(i + 1); got != w {
			t.Fatalf("attempt %d: got %s want %s", i+1, got, w)
		}
	}
}

// TestRequeueEthWaiting_GrowsBackoff verifies repeated waiting re-queues grow the
// per-packet delay (so a long finality wait polls ever more sparsely).
func TestRequeueEthWaiting_GrowsBackoff(t *testing.T) {
	b := NewBatchBuilder()
	p := waitingEthPkt(1)
	b.RequeueEthWaiting([]EthPacket{p})
	b.ethMtx.Lock()
	first := b.ethPackets[0].NotBefore
	attempts1 := b.ethPackets[0].waitAttempts
	pk := b.ethPackets[0]
	b.ethPackets = nil
	b.ethMtx.Unlock()
	if attempts1 != 1 {
		t.Fatalf("first waiting re-queue attempts=%d, want 1", attempts1)
	}

	b.RequeueEthWaiting([]EthPacket{pk})
	b.ethMtx.Lock()
	second := b.ethPackets[0].NotBefore
	attempts2 := b.ethPackets[0].waitAttempts
	b.ethMtx.Unlock()
	if attempts2 != 2 {
		t.Fatalf("second waiting re-queue attempts=%d, want 2", attempts2)
	}
	if !second.After(first) {
		t.Fatalf("backoff must grow: second NotBefore %s not after first %s", second, first)
	}
}

func TestRequeueEthWaiting_BeforeReleaseKeepsWatermark(t *testing.T) {
	bb := NewBatchBuilder()
	packet := makeEthPacket(1, EthSend)
	packet.BlockNumber = 400
	bb.AddEth(packet)
	ch := make(chan EthBatch, 1)
	bb.CheckEth(context.Background(), BatchConfig{BatchSize: 1, BatchPeriods: 0}, ch)
	batch := <-ch
	bb.RequeueEthWaiting(batch.Packets)
	bb.ReleaseEthInFlight(batch)
	if got := bb.LowestUnsubmittedEthHeight(); got != 400 {
		t.Fatalf("requeued packet watermark = %d, want 400", got)
	}
}

func TestNewPersistentBatchBuilder_RestoresBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	first, err := NewPersistentBatchBuilder(dir)
	if err != nil {
		t.Fatal(err)
	}
	packet := channeltypesv2.Packet{SourceClient: "l2-client", DestinationClient: "cosmos-client", Sequence: 9, TimeoutTimestamp: 100}
	first.L2PendingTracker.Add(packet, 99)
	first.L2PendingTracker.recordTimeoutFailureForTest(packet.SourceClient, packet.Sequence, time.Now())

	restarted, err := NewPersistentBatchBuilder(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := restarted.L2PendingTracker.GetAll()
	if len(got) != 1 || got[0].TimeoutAttempts != 1 || got[0].BlockNumber != 99 {
		t.Fatalf("batch builder did not restore L2 tracker before return: %#v", got)
	}
	if restarted.cosmosInFlight == nil || restarted.ethInFlight == nil {
		t.Fatal("persistent batch builder left in-flight recovery maps nil")
	}
}
