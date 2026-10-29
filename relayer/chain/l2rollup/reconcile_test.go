package l2rollup

import (
	"context"
	"errors"
	"relayer/chain"
	"sync"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// fakeCosmosState answers the two reconciliation queries independently and
// records which one was asked. Independent answers are the point: a fake that
// returned the same value for both would pass even if alreadyHandled wired the
// send branch to the commitment query, or read either answer backwards.
type fakeCosmosState struct {
	receipt      bool
	receiptErr   error
	commitment   bool
	commitentErr error

	askedReceipt    int
	askedCommitment int
}

func (f *fakeCosmosState) HasPacketReceipt(context.Context, channeltypesv2.Packet) (bool, error) {
	f.askedReceipt++
	return f.receipt, f.receiptErr
}

func (f *fakeCosmosState) HasPacketCommitment(context.Context, channeltypesv2.Packet) (bool, error) {
	f.askedCommitment++
	return f.commitment, f.commitentErr
}

func mustMarshalPacket(t *testing.T) []byte {
	t.Helper()
	packet := channeltypesv2.Packet{
		Sequence:          7,
		SourceClient:      idleTestDestID,
		DestinationClient: idleTestClientID,
		TimeoutTimestamp:  uint64(time.Now().Add(time.Hour).Unix()),
	}
	raw, err := packet.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	return raw
}

func eventOfType(t *testing.T, kind chain.EventType) chain.Event {
	t.Helper()
	return chain.Event{Type: kind, Height: 42, Sequence: 7, Raw: mustMarshalPacket(t)}
}

// A send is settled by a receipt that EXISTS; an acknowledgement by a commitment
// that is GONE. The two branches read their answer in opposite directions, so
// every case here pins both the path queried and the direction it is read.
func TestDropAlreadyHandled(t *testing.T) {
	cases := []struct {
		name      string
		eventType chain.EventType
		state     fakeCosmosState
		wantKept  bool
		wantAsked func(*fakeCosmosState) bool
		why       string
	}{
		{
			name:      "send already received on Cosmos is dropped",
			eventType: chain.SendPacket,
			// The commitment answer is the OPPOSITE verdict, so reading it here
			// instead would keep the event and fail the test.
			state:     fakeCosmosState{receipt: true, commitment: true},
			wantKept:  false,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedReceipt == 1 && f.askedCommitment == 0 },
			why:       "another relayer already delivered this packet; proving it again is wasted work",
		},
		{
			name:      "send not yet received is kept",
			eventType: chain.SendPacket,
			state:     fakeCosmosState{receipt: false, commitment: false},
			wantKept:  true,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedReceipt == 1 },
			why:       "no receipt on Cosmos means the packet is still outstanding",
		},
		{
			name:      "ack whose commitment is gone is dropped",
			eventType: chain.AckPacket,
			// receipt:false is the opposite verdict for the send branch.
			state:     fakeCosmosState{commitment: false, receipt: false},
			wantKept:  false,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedCommitment == 1 && f.askedReceipt == 0 },
			why:       "a cleared commitment means Cosmos already acknowledged or timed out the packet",
		},
		{
			name:      "ack whose commitment is still stored is kept",
			eventType: chain.AckPacket,
			state:     fakeCosmosState{commitment: true, receipt: true},
			wantKept:  true,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedCommitment == 1 },
			why:       "the commitment is still there, so the acknowledgement has not been relayed",
		},
		{
			name:      "receipt query error keeps the event",
			eventType: chain.SendPacket,
			state:     fakeCosmosState{receipt: true, receiptErr: errors.New("rpc down")},
			wantKept:  true,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedReceipt == 1 },
			why:       "fail open: dropping an undecidable event strands the packet and its escrow",
		},
		{
			name:      "commitment query error keeps the event",
			eventType: chain.AckPacket,
			state:     fakeCosmosState{commitment: false, commitentErr: errors.New("rpc down")},
			wantKept:  true,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedCommitment == 1 },
			why:       "fail open, same reason",
		},
		{
			name:      "an event type reconciliation does not understand is kept",
			eventType: chain.TimeoutPacket,
			state:     fakeCosmosState{},
			wantKept:  true,
			wantAsked: func(f *fakeCosmosState) bool { return f.askedReceipt == 0 && f.askedCommitment == 0 },
			why:       "this filter decides what to SKIP, so an unhandled case must fall through to relaying",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			src := &Source{cosmosState: &state}

			kept := src.dropAlreadyHandled(context.Background(), []chain.Event{eventOfType(t, tc.eventType)}, "test")

			if got := len(kept) == 1; got != tc.wantKept {
				t.Fatalf("kept = %v, want %v: %s", got, tc.wantKept, tc.why)
			}
			if !tc.wantAsked(&state) {
				t.Fatalf("wrong Cosmos state queried: receipt asked %d times, commitment %d times",
					state.askedReceipt, state.askedCommitment)
			}
		})
	}

	// Without a Cosmos endpoint the filter must be a no-op, not a drop: the one-shot
	// and test paths build a source without one, and a filter that silently emptied
	// their batch would look like an idle chain.
	t.Run("without cosmos state keeps everything", func(t *testing.T) {
		src := &Source{}
		events := []chain.Event{eventOfType(t, chain.SendPacket), eventOfType(t, chain.AckPacket)}

		if kept := src.dropAlreadyHandled(context.Background(), events, "test"); len(kept) != len(events) {
			t.Fatalf("kept %d of %d events; a source with no Cosmos endpoint must not filter", len(kept), len(events))
		}
	})

	// An undecodable packet cannot be reconciled, so it must be relayed rather than
	// dropped -- the decode failure is ours, not evidence about Cosmos.
	t.Run("undecodable packet is kept", func(t *testing.T) {
		state := &fakeCosmosState{receipt: true}
		src := &Source{cosmosState: state}

		kept := src.dropAlreadyHandled(context.Background(),
			[]chain.Event{{Type: chain.SendPacket, Sequence: 1, Raw: []byte("not a packet")}}, "test")

		if len(kept) != 1 {
			t.Fatal("an event whose packet will not decode must be kept, not dropped")
		}
		if state.askedReceipt != 0 {
			t.Fatal("no query should be issued for a packet that did not decode")
		}
	})
}

// runSubscribeWithCosmosState runs one Subscribe against a quiet node holding a
// single SendPacket log and reports how many events reached the handler.
// Bounded: a hung run fails the test rather than hanging it.
func runSubscribeWithCosmosState(t *testing.T, router ethcommon.Address, state *fakeCosmosState) (int, *fakeCosmosState) {
	t.Helper()
	restore := l2SubscribeInterval
	l2SubscribeInterval = 20 * time.Millisecond
	t.Cleanup(func() { l2SubscribeInterval = restore })
	t.Setenv(l2StartupLookbackEnv, "10")

	node := &idleL2Node{router: router, head: 100, logBlock: 95}
	src := &Source{
		eth: node.start(t), headKind: Unsafe, router: router,
		l2ClientID: idleTestClientID, cosmosWasmClientID: idleTestDestID,
		cosmosState: state,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	offered := make(chan int, 16)
	done := make(chan error, 1)
	go func() {
		done <- src.Subscribe(ctx, func(_ context.Context, batch []chain.Event) []int {
			select {
			case offered <- len(batch):
			default:
			}
			return nil // accept everything, so nothing lands in the pending buffer
		})
	}()

	// Wait for the scan to have happened, then read whatever the handler saw.
	// The scan is the observable step; the handler call, if any, happens in the
	// same iteration immediately after it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(node.scannedRanges()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let that iteration finish
	cancel()
	<-done

	total := 0
	for {
		select {
		case n := <-offered:
			total += n
		default:
			return total, state
		}
	}
}

// settlingState reports no receipt until it is told the packet has been settled,
// standing in for a second relayer delivering the packet while this one waits.
type settlingState struct {
	mu      sync.Mutex
	settled bool
}

func (s *settlingState) settle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled = true
}

func (s *settlingState) HasPacketReceipt(context.Context, channeltypesv2.Packet) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settled, nil
}

func (s *settlingState) HasPacketCommitment(context.Context, channeltypesv2.Packet) (bool, error) {
	return true, nil
}
