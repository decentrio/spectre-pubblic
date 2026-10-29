package l2rollup

import (
	"context"
	"fmt"

	"relayer/chain"
	"relayer/subscriber"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// PacketSettled implements chain.SettlementChecker for the Cosmos side of an
// L2->Cosmos path. Same store, same two questions as the ETH->Cosmos
// destination: a receive is settled by a receipt, an acknowledgement or
// timeout by the send's commitment being gone.
func (d *Destination) PacketSettled(ctx context.Context, e chain.Event) (bool, error) {
	if d.cosmos.CosmosClient() == nil {
		return false, fmt.Errorf("l2 dest: no Cosmos client to query settlement on")
	}
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(e.Raw); err != nil {
		return false, fmt.Errorf("l2 dest: decode packet: %w", err)
	}
	switch e.Type {
	case chain.SendPacket:
		return subscriber.HasCosmosPacketReceipt(ctx, d.cosmos, pkt)
	case chain.AckPacket, chain.TimeoutPacket:
		pending, err := subscriber.HasPendingCosmosPacketCommitment(ctx, d.cosmos, pkt)
		if err != nil {
			return false, err
		}
		return !pending, nil
	}
	return false, fmt.Errorf("l2 dest: settlement of %s is not defined", e.Type)
}

var _ chain.SettlementChecker = (*Destination)(nil)
