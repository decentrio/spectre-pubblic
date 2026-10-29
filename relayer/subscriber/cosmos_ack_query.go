package subscriber

import (
	"context"

	"relayer/services"
	"relayer/utils"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// HasCosmosPacketAcknowledgement reports whether Cosmos has written an
// acknowledgement for a packet it received. The ack is stored under the packet's
// DESTINATION client -- the counterparty's light client on Cosmos -- with the
// ICS-24 acknowledgement path type, the same way the receipt is stored beside it.
//
// Exported beside HasCosmosPacketReceipt and HasPendingCosmosPacketCommitment
// because the enumeration backstop asks all three of the same store: "was it
// received", "was it acknowledged", and "is the send still open".
func HasCosmosPacketAcknowledgement(stdCtx context.Context, endpoint services.CosmosEndpoint, packet channeltypesv2.Packet) (bool, error) {
	return HasCosmosPacketAcknowledgementFor(stdCtx, endpoint, packet.DestinationClient, packet.Sequence)
}

// HasCosmosPacketAcknowledgementFor is the same question by (client, sequence),
// for a caller that knows a packet is outstanding on the counterparty before it
// has the packet's bytes -- which is exactly the enumeration case.
func HasCosmosPacketAcknowledgementFor(stdCtx context.Context, endpoint services.CosmosEndpoint, destClient string, sequence uint64) (bool, error) {
	return hasCosmosIBCPathValue(stdCtx, endpoint, utils.IBCPath(destClient, sequence, []byte{3}))
}
