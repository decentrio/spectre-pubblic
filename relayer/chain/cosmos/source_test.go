package cosmos

import (
	"context"
	"relayer/chain"
	"relayer/chain/chaintest"
	"relayer/services"
	"testing"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

const routerClientID = "eth-client-0" // this Cosmos chain's ETH-side router id

func cosmosPacket(t services.CosmosPacketType, src, dst string) services.CosmosPacket {
	p := services.CosmosPacket{
		Type:        t,
		Packet:      &channeltypesv2.Packet{Sequence: 1, SourceClient: src, DestinationClient: dst},
		BlockNumber: 42,
	}
	// Acks carry acknowledgement bytes; without them the mapping skips the event.
	if t == services.CosmosAck {
		p.AckBytes = [][]byte{{0x01}}
	}
	return p
}

func TestCosmosPacketToEvent(t *testing.T) {
	t.Run("timeout filter", func(t *testing.T) {
		tests := []struct {
			name     string
			packet   services.CosmosPacket
			wantOK   bool
			wantType chain.EventType
		}{
			{
				// Cosmos-originated: destination_client == our router id → timeout fires
				// on Cosmos, ETH holds no commitment → must NOT relay to ETH.
				name:   "cosmos-originated timeout dropped",
				packet: cosmosPacket(services.CosmosTimeout, "08-wasm-0", routerClientID),
				wantOK: false,
			},
			{
				// ETH-originated: destination_client != our router id → ETH still owns a
				// commitment that the timeout must delete + refund → relay it.
				name:     "eth-originated timeout relayed",
				packet:   cosmosPacket(services.CosmosTimeout, routerClientID, "08-wasm-0"),
				wantOK:   true,
				wantType: chain.TimeoutPacket,
			},
			{
				name:     "send always relayed",
				packet:   cosmosPacket(services.CosmosSend, routerClientID, "08-wasm-0"),
				wantOK:   true,
				wantType: chain.SendPacket,
			},
			{
				name:     "ack always relayed",
				packet:   cosmosPacket(services.CosmosAck, "08-wasm-0", routerClientID),
				wantOK:   true,
				wantType: chain.AckPacket,
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e, ok := cosmosPacketToEvent(tc.packet, routerClientID)
				if ok != tc.wantOK {
					t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
				}
				if ok && e.Type != tc.wantType {
					t.Fatalf("type = %d, want %d", e.Type, tc.wantType)
				}
			})
		}
	})

	// TestCosmosPacketToEvent/nil packet guards the encode-failure skip.
	t.Run("nil packet", func(t *testing.T) {
		if _, ok := cosmosPacketToEvent(services.CosmosPacket{Type: services.CosmosSend}, routerClientID); ok {
			t.Fatal("nil packet must be skipped")
		}
	})

	// TestCosmosPacketToEvent/empty ack skipped guards the no-ack-bytes skip: an ack
	// without acknowledgement bytes cannot be relayed and must not be emitted.
	t.Run("empty ack skipped", func(t *testing.T) {
		p := services.CosmosPacket{
			Type:   services.CosmosAck,
			Packet: &channeltypesv2.Packet{Sequence: 1, SourceClient: "08-wasm-0", DestinationClient: routerClientID},
			// AckBytes deliberately empty
		}
		if _, ok := cosmosPacketToEvent(p, routerClientID); ok {
			t.Fatal("ack with no acknowledgement bytes must be skipped")
		}
	})
}

// eventsWithOrigins returns two slices that Subscribe treats as index-aligned: the
// handler reports failures as indices into the events slice, and Subscribe
// re-queues orig[idx]. If a skipped packet lands in one slice but not the other,
// every later index shifts by one — and the consequence is not a lost retry but a
// wrong one: the relayer re-queues a packet that already succeeded and forgets the
// one that failed.
//
// The batch below deliberately puts skipped packets FIRST, BETWEEN and LAST, so a
// misalignment cannot hide behind the shape of the input.
func TestEventsWithOrigins_StaysIndexAligned(t *testing.T) {
	seq := func(p services.CosmosPacket, n uint64) services.CosmosPacket {
		p.Packet.Sequence = n
		return p
	}
	ackNoBytes := func(n uint64) services.CosmosPacket {
		p := cosmosPacket(services.CosmosAck, "cosmos-client", routerClientID)
		p.AckBytes = nil // an ack with no bytes cannot be built into a message: skipped
		p.Packet.Sequence = n
		return p
	}

	packets := []services.CosmosPacket{
		ackNoBytes(1), // skipped, first
		seq(cosmosPacket(services.CosmosSend, "cosmos-client", routerClientID), 2),
		{Type: services.CosmosSend, Packet: nil, BlockNumber: 7}, // skipped, nil packet
		seq(cosmosPacket(services.CosmosSend, "cosmos-client", routerClientID), 4),
		seq(cosmosPacket(services.CosmosAck, "cosmos-client", routerClientID), 5),
		ackNoBytes(6), // skipped, last
	}

	events, orig := eventsWithOrigins(packets, routerClientID)

	if len(events) != len(orig) {
		t.Fatalf("slices out of step: %d events vs %d origins", len(events), len(orig))
	}
	if len(events) != 3 {
		t.Fatalf("want the 3 relayable packets, got %d", len(events))
	}
	// The real invariant: for every index the handler could report, the origin at
	// that index is the packet the event was built from.
	for i := range events {
		if orig[i].Packet == nil {
			t.Fatalf("origin %d has no packet", i)
		}
		if events[i].Sequence != orig[i].Packet.Sequence {
			t.Fatalf("index %d maps event seq=%d to origin seq=%d",
				i, events[i].Sequence, orig[i].Packet.Sequence)
		}
	}
	// Spelled out, so a shift by one is named rather than inferred.
	want := []uint64{2, 4, 5}
	for i, w := range want {
		if events[i].Sequence != w {
			t.Fatalf("events[%d].Sequence = %d, want %d (skipped packets must leave BOTH slices)",
				i, events[i].Sequence, w)
		}
	}
}

// A batch where nothing converts must yield two empty slices, not one.
func TestEventsWithOrigins_AllSkipped(t *testing.T) {
	packets := []services.CosmosPacket{
		{Type: services.CosmosSend, Packet: nil},
		{Type: services.CosmosPacketType(99), Packet: &channeltypesv2.Packet{Sequence: 1}},
	}
	events, orig := eventsWithOrigins(packets, routerClientID)
	if len(events) != 0 || len(orig) != 0 {
		t.Fatalf("want both slices empty, got %d events / %d origins", len(events), len(orig))
	}
}

// --- chain.Source contract ---

// A Cosmos packet commitment written at height H is not reflected in the AppHash
// until H+2, so the module must not try to prove anything above latest-2. Getting
// this wrong is not subtle in production but is invisible in a test suite that
// never names the boundary: too large and every relay at the chain tip fails to
// prove, too small and the relayer simply lags.
//
// The clamp matters as much as the subtraction. This is unsigned arithmetic, so
// on a chain that has just started latest-2 would wrap to an enormous height.
func TestRelayableFromLatest(t *testing.T) {
	tests := []struct {
		name   string
		latest uint64
		want   uint64
	}{
		{"genesis", 0, 0},
		{"one block in, still below the lag", 1, 0},
		{"exactly the lag", 2, 0},
		{"first height with something relayable", 3, 1},
		{"steady state", 1000, 998},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relayableFromLatest(tt.latest); got != tt.want {
				t.Fatalf("relayableFromLatest(%d) = %d, want %d (lag is %d)",
					tt.latest, got, tt.want, cosmosAppHashLag)
			}
		})
	}
}

// Packet bytes that do not decode must fail before the source dials anything.
// Reaching the network first turns a permanently broken packet into a retry loop
// against the RPC endpoint, and buries the real cause under a timeout error.
func TestSource_MalformedPacketFailsBeforeAnyRPC(t *testing.T) {
	// A zero-value Source has no Cosmos client: if either call got as far as
	// dialing, it would panic rather than return the decode error asserted below.
	s := &Source{}
	garbage := []byte{0xff, 0xff, 0xff, 0xff}

	if _, err := s.MembershipProof(context.Background(), garbage, 10, chain.SendPacket); err == nil {
		t.Error("MembershipProof accepted undecodable packet bytes")
	}
	if _, err := s.NonMembershipProof(context.Background(), garbage, 10); err == nil {
		t.Error("NonMembershipProof accepted undecodable packet bytes")
	}
}

// The adapter contract suite. Everything asserted here is asserted identically
// on the other two adapters: adding a fourth chain is a package plus this file.
func TestAdapterContract(t *testing.T) {
	chaintest.RunSourceContract(t, chaintest.SourceFixture{Name: "cosmos", Unwired: &Source{}})
	chaintest.RunDestinationContract(t, chaintest.DestinationFixture{Name: "cosmos", Unwired: &Destination{}})
}
