package evm

import (
	"context"
	"testing"

	"relayer/chain"
	"relayer/rpcmock"
	"relayer/services"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// PacketSettled asks the router the question that matches the event: a
// receipt for a receive, the ABSENCE of the send's commitment for an
// acknowledgement. Asking the receipt question of an ack would look at a path
// that lives on the other chain.
func TestPacketSettled_AsksPerEventType(t *testing.T) {
	node := rpcmock.NewEVMNode(t)
	router := ethcommon.HexToAddress("0x000000000000000000000000000000000000c26e")
	d := &Destination{evm: services.EVMEndpoint{Client: node.Client(t), Contracts: services.EVMContracts{Router: router}}}

	toEVM := channeltypesv2.Packet{Sequence: 3, SourceClient: "08-wasm-0", DestinationClient: "cosmoshub-1"}
	fromEVM := channeltypesv2.Packet{Sequence: 5, SourceClient: "cosmoshub-1", DestinationClient: "08-wasm-0"}
	rawTo, _ := toEVM.Marshal()
	rawFrom, _ := fromEVM.Marshal()

	settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.SendPacket, Sequence: 3, Raw: rawTo})
	if err != nil || settled {
		t.Fatalf("send with no receipt: settled=%v err=%v, want unsettled", settled, err)
	}
	node.SetStorage(router, services.EthIBCStorageKey(services.EthPath("cosmoshub-1", 3, services.EthPathReceipt)), ethcommon.HexToHash("0x01"))
	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.SendPacket, Sequence: 3, Raw: rawTo}); err != nil || !settled {
		t.Fatalf("send with a receipt: settled=%v err=%v, want settled", settled, err)
	}

	// The ack's send commitment is still on the router: not settled.
	node.SetStorage(router, services.EthIBCStorageKey(services.EthPath("cosmoshub-1", 5, services.EthPathCommitment)), ethcommon.HexToHash("0x02"))
	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.AckPacket, Sequence: 5, Raw: rawFrom}); err != nil || settled {
		t.Fatalf("ack while the commitment stands: settled=%v err=%v, want unsettled", settled, err)
	}
	node.SetStorage(router, services.EthIBCStorageKey(services.EthPath("cosmoshub-1", 5, services.EthPathCommitment)), ethcommon.Hash{})
	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.AckPacket, Sequence: 5, Raw: rawFrom}); err != nil || !settled {
		t.Fatalf("ack after the commitment is deleted: settled=%v err=%v, want settled", settled, err)
	}
}
