package services

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/cosmos/gogoproto/proto"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// PacketCommitment is one outstanding entry of a Cosmos packet-commitment
// store: the sequence, and the commitment hash that says WHICH packet that
// sequence currently names.
//
// The hash is carried rather than dropped because a sequence alone does not
// identify a packet to anything outside the chain's own store. The commitment is
// what the chain will accept a proof against, so it is also the only thing that
// can confirm a transaction found for that sequence is the packet still
// outstanding and not an older one that happens to share the sequence.
type PacketCommitment struct {
	Sequence uint64
	Data     []byte
}

const packetCommitmentsQueryPath = "/ibc.core.channel.v2.Query/PacketCommitments"

// CosmosCommitmentPageSize caps one page of the commitment query. The healthy
// answer is a handful of sequences, so the page size only matters on a chain
// with a real backlog -- where paging is exactly what stops one query from
// returning everything at once.
const CosmosCommitmentPageSize = 100

// SequenceKey renders a sequence as the paginator key for it: the commitment
// store is prefixed per client and keyed by the 8-byte big-endian sequence, so
// this is the "start here" the SDK iterator understands.
func SequenceKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}

// CosmosCommitmentWindow returns at most limit outstanding commitments for
// clientID, starting after cursor and wrapping to the oldest.
//
// It ASKS FOR THE WINDOW rather than paging the whole set and slicing it. The
// difference is the whole cost of the pass. The commitment store is keyed by the
// big-endian sequence, and the SDK's paginator starts its iterator at the key it
// is given, so "the N sequences after the cursor" is two page queries -- against
// a backlog of any size. Paging everything first made the query cost
// proportional to the backlog: a chain with 100k outstanding commitments spent a
// thousand serial ABCI round-trips before relaying a single packet.
//
// The rotation is the point of the cursor. Taking the oldest N every pass looks
// fair and is not: a prefix that cannot resolve -- pruned history, or packets
// already delivered and waiting on an acknowledgement, which keep their
// commitment -- occupies the whole window forever, and every later packet is
// never even looked at. Rotating means an unresolvable prefix costs one pass,
// not all of them. The caller records the last sequence returned as the next
// cursor.
//
// Shared by every source that enumerates a Cosmos commitment store: the Cosmos
// source for its own sends, and the EVM and L2 sources for the acknowledgements
// they owe those sends.
func CosmosCommitmentWindow(ctx context.Context, endpoint CosmosEndpoint, clientID string, cursor uint64, limit int) ([]PacketCommitment, error) {
	// After the cursor: the packets this pass has not looked at most recently.
	window, err := PageCosmosPacketCommitments(ctx, endpoint, clientID, SequenceKey(cursor+1), limit, 0)
	if err != nil {
		return nil, err
	}
	// Short means the tail ran out, so wrap to the oldest. Bounded by the cursor
	// so the two segments cannot overlap: everything taken above is beyond it.
	if len(window) < limit && cursor > 0 {
		head, err := PageCosmosPacketCommitments(ctx, endpoint, clientID, nil, limit-len(window), cursor)
		if err != nil {
			return nil, err
		}
		window = append(window, head...)
	}
	return window, nil
}

// PageCosmosPacketCommitments reads up to want commitments for clientID starting
// at startKey, stopping early at stopAfter when that is non-zero.
//
// stopAfter is what keeps a wrap segment from re-reading what the segment after
// the cursor already took.
func PageCosmosPacketCommitments(ctx context.Context, endpoint CosmosEndpoint, clientID string, startKey []byte, want int, stopAfter uint64) ([]PacketCommitment, error) {
	if want <= 0 {
		return nil, nil
	}
	var (
		out     []PacketCommitment
		nextKey = startKey
	)
	for {
		limit := min(want-len(out), CosmosCommitmentPageSize)
		request := &channeltypesv2.QueryPacketCommitmentsRequest{
			ClientId:   clientID,
			Pagination: &query.PageRequest{Key: nextKey, Limit: uint64(limit)},
		}
		raw, err := proto.Marshal(request)
		if err != nil {
			return nil, fmt.Errorf("marshal packet-commitments request: %w", err)
		}
		result, err := endpoint.CosmosClient().ABCIQuery(ctx, packetCommitmentsQueryPath, raw)
		if err != nil {
			return nil, fmt.Errorf("packet commitments: %w", err)
		}
		if result.Response.Code != 0 {
			return nil, fmt.Errorf("packet commitments: code %d: %s", result.Response.Code, result.Response.Log)
		}
		var response channeltypesv2.QueryPacketCommitmentsResponse
		if err := proto.Unmarshal(result.Response.Value, &response); err != nil {
			return nil, fmt.Errorf("decode packet-commitments response: %w", err)
		}
		for _, commitment := range response.Commitments {
			if stopAfter > 0 && commitment.Sequence > stopAfter {
				return out, nil
			}
			out = append(out, PacketCommitment{Sequence: commitment.Sequence, Data: commitment.Data})
			if len(out) == want {
				return out, nil
			}
		}
		if response.Pagination == nil || len(response.Pagination.NextKey) == 0 {
			return out, nil
		}
		nextKey = response.Pagination.NextKey
	}
}
