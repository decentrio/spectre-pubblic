package evm

import (
	"context"
	"fmt"

	"relayer/chain"
	"relayer/services"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// PacketSettled implements chain.SettlementChecker for the ETH router.
//
//   - a receive is settled once the router holds the packet's receipt;
//   - an acknowledgement (or timeout) is settled once the router no longer
//     holds the original send's commitment -- ackPacket and timeoutPacket both
//     delete it, and a second submission is answered with Noop.
func (d *Destination) PacketSettled(ctx context.Context, e chain.Event) (bool, error) {
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(e.Raw); err != nil {
		return false, fmt.Errorf("evm: decode packet: %w", err)
	}
	switch e.Type {
	case chain.SendPacket:
		return services.HasEthPacketReceipt(ctx, d.evm, pkt)
	case chain.AckPacket, chain.TimeoutPacket:
		pending, err := services.HasPendingEthPacketCommitment(ctx, d.evm, pkt)
		if err != nil {
			return false, err
		}
		return !pending, nil
	}
	return false, fmt.Errorf("evm: settlement of %s is not defined", e.Type)
}

var _ chain.SettlementChecker = (*Destination)(nil)
