package subscriber

import (
	"context"
	"relayer/services"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func TestNormalizeTimeoutSeconds(t *testing.T) {
	cases := []struct {
		input    uint64
		expected uint64
	}{
		{1_700_000_000_000_000_000, 1_700_000_000}, // 2023 ns → 2023 seconds
		{1_000_000_000_000, 1_000},                 // nanoseconds → seconds
		{1_000, 1_000},                             // already seconds (year 2003), unchanged
		{0, 0},                                     // no timeout, unchanged
	}

	for _, c := range cases {
		got := normalizeTimeoutSeconds(c.input)
		if got != c.expected {
			t.Errorf("normalizeTimeoutSeconds(%d) = %d, want %d", c.input, got, c.expected)
		}
	}
}

// The heartbeat is shared by both directions so they cannot drift: one line every
// quietScanHeartbeat passes, and the counter keeps climbing so the printed number
// is the real streak.
func TestAdvanceQuietScans(t *testing.T) {
	var quiet uint64
	beats := 0
	for i := 1; i <= quietScanHeartbeat*2; i++ {
		if advanceQuietScans(&quiet) {
			beats++
			if quiet%quietScanHeartbeat != 0 {
				t.Fatalf("beat at %d, which is not a multiple of %d", quiet, quietScanHeartbeat)
			}
		}
	}
	if beats != 2 {
		t.Fatalf("beats = %d over %d passes, want 2", beats, quietScanHeartbeat*2)
	}
	if quiet != uint64(quietScanHeartbeat*2) {
		t.Fatalf("counter = %d, want it to keep counting to %d", quiet, quietScanHeartbeat*2)
	}
}

func TestRecoveryChunkSize(t *testing.T) {
	t.Setenv(cosmosRecoveryChunkEnv, "17")
	if got := recoveryChunkSize(cosmosRecoveryChunkEnv, defaultCosmosRecoveryChunkHeights); got != 17 {
		t.Fatalf("chunk size = %d, want 17", got)
	}
	t.Setenv(cosmosRecoveryChunkEnv, "0")
	if got := recoveryChunkSize(cosmosRecoveryChunkEnv, defaultCosmosRecoveryChunkHeights); got != defaultCosmosRecoveryChunkHeights {
		t.Fatalf("invalid chunk fallback = %d", got)
	}
}

func TestSubscriber_ResumesFromPersistedCursors(t *testing.T) {
	sub, store, deps := newRecoveryTestSubscriber(t)
	if err := store.SaveCheckpoint(services.RecoveryCursors{"cosmos": 100, "eth_send": 120, "eth_write_ack": 900}); err != nil {
		t.Fatal(err)
	}
	if got := sub.resumeCosmosCursor(deps, 1000, 256); got != 100 {
		t.Fatalf("cosmos resume = %d, want 100", got)
	}
	send, ack := sub.resumeEthCursors(deps, 1000, 256)
	if send != 120 || ack != 744 {
		t.Fatalf("eth resume = (%d,%d), want (120,744)", send, ack)
	}
}

func TestPersistCosmosCursor_ClampToOutstandingPackets(t *testing.T) {
	sub, store, deps := newRecoveryTestSubscriber(t)
	// Seed progress above the outstanding packets. The safety clamp must be able
	// to rewind an already-persisted cursor, not only constrain its first write.
	if err := store.SaveCheckpoint(services.RecoveryCursors{"cosmos": 90, "eth_send": 100, "eth_write_ack": 100}); err != nil {
		t.Fatal(err)
	}
	bb := services.NewBatchBuilder()
	bb.AddCosmos(services.CosmosPacket{Type: services.CosmosSend, Packet: &channeltypesv2.Packet{Sequence: 1}, BlockNumber: 50})
	bb.AddEth(services.EthPacket{Type: services.EthSend, Packet: &channeltypesv2.Packet{Sequence: 2}, BlockNumber: 60})

	sub.persistCosmosCursor(deps, bb, 100)
	sub.persistEthCursors(deps, bb, 110, 120)
	got, ok := store.Get()
	if !ok {
		t.Fatal("source cursors not saved")
	}
	if got["cosmos"] != 50 || got["eth_send"] != 60 || got["eth_write_ack"] != 60 {
		t.Fatalf("clamped cursors = %+v", got)
	}
}

func TestSleepOrDone_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepOrDone(ctx, time.Minute) {
		t.Fatal("cancelled sleep reported elapsed")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled sleep took %s", elapsed)
	}
}

func TestSleepOrDone_CancellationDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- sleepOrDone(ctx, time.Minute) }()
	cancel()
	select {
	case elapsed := <-done:
		if elapsed {
			t.Fatal("cancelled sleep reported elapsed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect sleep ignored cancellation")
	}
}
