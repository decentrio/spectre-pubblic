package main

import (
	"context"
	"fmt"
	"log"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

const COMETBFT_SEND_PACKET_EVENT = "tm.event = 'Tx'"

type Subscriber struct {
}

func (s *Subscriber) SubscribeCosmos(logger *log.Logger, client rpchttp.HTTP) {
	sub, err := client.WSEvents.Subscribe(context.Background(), "", COMETBFT_SEND_PACKET_EVENT)
	if err != nil {
		logger.Println(err.Error())
	}

	for {
		select {
		case e := <-sub:
			fmt.Println("events: ", e.Events)
			// handle event
			sendPacketEvent := e.Events[channeltypesv2.EventTypeSendPacket]
			if sendPacketEvent == nil {
				continue
			}
			packetHex := e.Events[channeltypesv2.AttributeKeyEncodedPacketHex]
			fmt.Println("packetHex: ", packetHex)
		}
	}
}

func main() {
	subscriber := Subscriber{}

	tendermintRpcClient, err := rpchttp.New("http://127.0.0.1:26657", "/websocket")
	if err != nil {
		panic(fmt.Errorf("failed to create RPC client: %w", err).Error())
	}
	err = tendermintRpcClient.Start()
	if err != nil {
		panic(fmt.Errorf("err starting rpc  client: %w", err).Error())
	}
	log := log.Default()
	fmt.Println("start indexing...")
	subscriber.SubscribeCosmos(log, *tendermintRpcClient)
}
