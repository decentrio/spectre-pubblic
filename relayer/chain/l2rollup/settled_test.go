package l2rollup

import (
	"context"
	"encoding/json"
	"testing"

	"relayer/chain"
	"relayer/rpcmock"
	"relayer/services"
	"relayer/utils"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func TestPacketSettled_AsksPerEventType(t *testing.T) {
	node := rpcmock.NewCosmosNode(t)
	present := map[string]bool{}
	node.Handle("abci_query", func(params json.RawMessage) (any, error) {
		var q struct {
			Path string            `json:"path"`
			Data cmtbytes.HexBytes `json:"data"`
		}
		if err := cmtjson.Unmarshal(params, &q); err != nil {
			return nil, err
		}
		var value []byte
		if present[string(q.Data)] {
			value = []byte{1}
		}
		return &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Value: value}}, nil
	})
	d := &Destination{cosmos: services.CosmosEndpoint{Client: node.Client(t)}, clientID: "08-wasm-3"}

	toCosmos := channeltypesv2.Packet{Sequence: 3, SourceClient: "client-0", DestinationClient: "08-wasm-3"}
	fromCosmos := channeltypesv2.Packet{Sequence: 5, SourceClient: "08-wasm-3", DestinationClient: "client-0"}
	rawTo, _ := toCosmos.Marshal()
	rawFrom, _ := fromCosmos.Marshal()

	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.SendPacket, Raw: rawTo}); err != nil || settled {
		t.Fatalf("send with no receipt: settled=%v err=%v, want unsettled", settled, err)
	}
	present[string(utils.IBCPath("08-wasm-3", 3, []byte{2})[1])] = true
	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.SendPacket, Raw: rawTo}); err != nil || !settled {
		t.Fatalf("send with a receipt: settled=%v err=%v, want settled", settled, err)
	}

	present[string(utils.IBCCommitmentPath(fromCosmos, []byte{1})[1])] = true
	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.AckPacket, Raw: rawFrom}); err != nil || settled {
		t.Fatalf("ack while the commitment stands: settled=%v err=%v, want unsettled", settled, err)
	}
	delete(present, string(utils.IBCCommitmentPath(fromCosmos, []byte{1})[1]))
	if settled, err := d.PacketSettled(context.Background(), chain.Event{Type: chain.AckPacket, Raw: rawFrom}); err != nil || !settled {
		t.Fatalf("ack after the commitment is deleted: settled=%v err=%v, want settled", settled, err)
	}
}
