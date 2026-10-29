package evm

import (
	"context"
	contractICS26Router "relayer/bindings/ICS26Router"
	spectreContract "relayer/bindings/SpectreClient"
	updateclientContract "relayer/bindings/UpdateClient"
	"relayer/chain"
	"relayer/chain/codec"
	"relayer/services"
	"testing"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

type recordingTxHandler struct {
	services.TransactionHandler
	ethBatches [][]any
}

func (h *recordingTxHandler) SendEthTxBatch(_ context.Context, _ services.EVMEndpoint, _ string, msgs []any) error {
	h.ethBatches = append(h.ethBatches, msgs)
	return nil
}

// mustMarshalPacket returns a proto-marshaled minimal packet for RelayPacket.Packet.
func mustMarshalPacket(t *testing.T) []byte {
	t.Helper()
	pkt := channeltypesv2.Packet{Sequence: 7, SourceClient: "eth-router-0", DestinationClient: "08-wasm-0"}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	return raw
}

// These exercise RelayPackets' input validation — the branches that return before
// SendEthTxBatch, so no worker is needed. A zero-value Destination is sufficient.

func TestRelayPackets(t *testing.T) {
	// TestRelayPackets/ack missing bytes: an ack packet with no acknowledgement bytes
	// cannot build a MsgAckPacket and must error, not silently drop or panic.
	t.Run("ack missing bytes", func(t *testing.T) {
		d := &Destination{}
		_, err := d.RelayPackets(context.Background(), []chain.RelayPacket{{
			Type:   chain.AckPacket,
			Packet: mustMarshalPacket(t),
			// AckBytes deliberately empty
		}})
		if err == nil {
			t.Fatal("ack packet without acknowledgement bytes must error")
		}
	})

	// TestRelayPackets/unknown type guards the default branch (an event type the EVM
	// destination does not build a message for).
	t.Run("unknown type", func(t *testing.T) {
		d := &Destination{}
		_, err := d.RelayPackets(context.Background(), []chain.RelayPacket{{
			Type:   chain.EventType(99),
			Packet: mustMarshalPacket(t),
		}})
		if err == nil {
			t.Fatal("unknown packet type must error")
		}
	})

	// TestRelayPackets/malformed packet guards the packet-decode failure path.
	t.Run("malformed packet", func(t *testing.T) {
		d := &Destination{}
		// A byte string that is not a valid proto packet.
		_, err := d.RelayPackets(context.Background(), []chain.RelayPacket{{
			Type:   chain.SendPacket,
			Packet: []byte{0xff, 0xff, 0xff, 0xff},
		}})
		if err == nil {
			t.Fatal("malformed packet must error")
		}
	})

	// TestRelayPackets/empty: no packets is a no-op (returns nil without touching the
	// worker), matching the module's "nothing to relay" contract.
	t.Run("empty", func(t *testing.T) {
		d := &Destination{}
		if committed, err := d.RelayPackets(context.Background(), nil); err != nil || committed != 0 {
			t.Fatalf("empty relay must be a no-op, got %v", err)
		}
	})
}

// TestUpdateClient_BadPayload guards the codec-decode failure path (returns before
// the eth tx submit, so no worker needed).
func TestUpdateClient_BadPayload(t *testing.T) {
	d := &Destination{}
	err := d.UpdateClient(context.Background(), "", chain.ClientUpdate{Payloads: [][]byte{[]byte("not-a-gob-cosmos-update")}})
	if err == nil {
		t.Fatal("undecodable client update payload must error")
	}
}

func TestUpdateClient_RequiresExactlyOnePayload(t *testing.T) {
	d := &Destination{}
	if err := d.UpdateClient(context.Background(), "", chain.ClientUpdate{}); err == nil {
		t.Fatal("empty payload list must fail")
	}
	if err := d.UpdateClient(context.Background(), "", chain.ClientUpdate{Payloads: [][]byte{[]byte("one"), []byte("two")}}); err == nil {
		t.Fatal("multiple payloads must fail")
	}
}

func TestSupportsUpdatePacketFolding_RequiresRouterProofSubmitter(t *testing.T) {
	const (
		router = "0x0000000000000000000000000000000000000001"
		other  = "0x0000000000000000000000000000000000000002"
	)
	evm := services.EVMEndpoint{Contracts: services.EVMContracts{
		Router:      ethcommon.HexToAddress(router),
		RoleManager: ethcommon.HexToAddress(other),
	}}
	d := &Destination{evm: evm}
	if d.SupportsUpdatePacketFolding() {
		t.Fatal("different role manager must disable folding")
	}
	evm.Contracts.RoleManager = ethcommon.HexToAddress(router)
	d.evm = evm
	if !d.SupportsUpdatePacketFolding() {
		t.Fatal("router proof submitter must enable folding")
	}
}

func TestRelayWithUpdate_SubmitsOneOrderedBatch(t *testing.T) {
	const router = "0x0000000000000000000000000000000000000001"
	evm := services.EVMEndpoint{Contracts: services.EVMContracts{
		Router:      ethcommon.HexToAddress(router),
		RoleManager: ethcommon.HexToAddress(router),
	}}
	handler := &recordingTxHandler{}
	d := &Destination{worker: services.NewWorker(handler, nil), evm: evm, clientID: "client-0"}

	payload, err := codec.EncodeCosmosUpdate(
		int(services.ApplicationUpdate),
		updateclientContract.ISpectreClientMsgsMsgUpdateApplicationState{},
		spectreContract.ISpectreTendermintMsgsValidatorSet{},
	)
	if err != nil {
		t.Fatalf("encode update: %v", err)
	}
	err = d.RelayWithUpdate(context.Background(), "client-0", chain.ClientUpdate{
		Height: 80, Payloads: [][]byte{payload},
	}, []chain.RelayPacket{{
		Type: chain.SendPacket, Packet: mustMarshalPacket(t), Proof: []byte("proof"), Height: 80,
	}})
	if err != nil {
		t.Fatalf("folded relay: %v", err)
	}
	if len(handler.ethBatches) != 1 {
		t.Fatalf("batch submissions = %d, want 1", len(handler.ethBatches))
	}
	batch := handler.ethBatches[0]
	if len(batch) != 2 {
		t.Fatalf("inner message count = %d, want update + packet", len(batch))
	}
	if _, ok := batch[0].(services.CosmosClientUpdateBuildResult); !ok {
		t.Fatalf("first inner message = %T, want CosmosClientUpdateBuildResult", batch[0])
	}
	if _, ok := batch[1].(contractICS26Router.IICS26RouterMsgsMsgRecvPacket); !ok {
		t.Fatalf("second inner message = %T, want MsgRecvPacket", batch[1])
	}
}

// SupportsUpdatePacketFolding and services.EVMEndpoint.RouterManagesProofSubmission
// are one question asked at two layers -- the chain.FoldingDestination contract
// and the deployment shape it depends on. They were separately implemented and
// byte-identical, which is how two things drift: nothing said they had to agree.
//
// This says it. Re-inlining the predicate here is only safe while the two answer
// the same, and the moment they do not, this is red.
func TestSupportsUpdatePacketFolding_AgreesWithTheEndpoint(t *testing.T) {
	addr := func(b byte) ethcommon.Address { return ethcommon.Address{b} }

	for _, tc := range []struct {
		name        string
		roleManager ethcommon.Address
		router      ethcommon.Address
	}{
		{name: "router is the role manager", roleManager: addr(1), router: addr(1)},
		{name: "separate role manager", roleManager: addr(2), router: addr(1)},
		{name: "both unset"},
		{name: "role manager unset", router: addr(1)},
		{name: "router unset", roleManager: addr(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := services.EVMEndpoint{
				Contracts: services.EVMContracts{RoleManager: tc.roleManager, Router: tc.router},
			}
			d := &Destination{evm: endpoint}
			if got, want := d.SupportsUpdatePacketFolding(), endpoint.RouterManagesProofSubmission(); got != want {
				t.Fatalf("folding says %v, the deployment says %v; the two definitions have drifted", got, want)
			}
		})
	}
}
