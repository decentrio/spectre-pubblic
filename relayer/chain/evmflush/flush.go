// Package evmflush enumerates the IBC packets an ICS26Router still holds
// commitments or acknowledgements for, WITHOUT being told which packets to look
// for. It is the EVM half of the enumeration backstop (chain.PacketLister); the
// Cosmos half is services.CosmosCommitmentWindow.
//
// Two facts about the router shape everything here:
//
//   - Its store is a mapping. The keys cannot be listed, so "which sequences
//     are outstanding" has to be a PROBE: one storage read per candidate
//     sequence, bounded above by prevSequenceSend (the last sequence the router
//     assigned) and below by a window, never 1..N.
//   - Its events are indexed by client id and sequence. So once a sequence is
//     known to be outstanding, the packet bytes behind it are one eth_getLogs
//     with a topic filter -- which is also what makes the range the provider is
//     asked for the only cost that can grow. The filter matches one log per
//     GENERATION of the client id (a re-created client restarts at 1), and the
//     held commitment says which generation is the live one.
//
// Both the ETH L1 source and the L2 sources sit on this router, and they share
// this code so the two cannot disagree about how a probe is bounded.
package evmflush

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/big"

	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/logbridge"
	"relayer/services"
	"relayer/subscriber"

	contractICS26Router "relayer/bindings/ICS26Router"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// ProbeCap bounds how many sequences one pass probes. The remainder is not
// lost: the cursor makes the next pass continue where this one stopped, and
// wrap to the newest when it reaches the oldest.
const ProbeCap = 200

// probeBatch caps one JSON-RPC batch. Providers cap batch sizes at varying
// numbers; 100 is under every documented limit.
const probeBatch = 100

// MaxLogChunks bounds how far back from the head one log lookup walks. A packet's commitment already proves it
// is outstanding; what this walk is looking for is the event carrying its
// bytes, which was emitted when it was sent -- possibly a very long time ago.
// Walking the whole history of a rollup in provider-sized chunks would be a
// stampede, so the walk gives up, says so, and leaves the packet for a source
// pointed at a provider that serves the range.
const MaxLogChunks = 64

// DefaultLogChunk is the widest span one log lookup asks for when nothing has
// narrowed it and the router's deploy block is unknown: the walk back from the
// head covers at most MaxLogChunks*DefaultLogChunk blocks (about 89 days of L1 at
// 12s blocks). A configured or provider-narrowed LogSpan.Chunk takes precedence.
const DefaultLogChunk = 10_000

// Router is one ICS26Router to enumerate against.
type Router struct {
	Client  *ethclient.Client
	Address ethcommon.Address
	// Span is the log range policy, narrowed in place when the provider refuses
	// a range. Owned by the caller's flush goroutine, like every LogSpan.
	Span *relayerclient.LogSpan
	// DeployBlock is the block the router was deployed at (router_deploy_block),
	// 0 when not configured. No router log exists below it, so when it is known a
	// lookup asks for [DeployBlock, head] in one call -- the whole of the
	// router's history -- instead of walking DefaultLogChunk windows back from
	// the head. A provider that refuses that range narrows it the usual way.
	DeployBlock uint64
	// Label prefixes every log line, e.g. "[eth->cosmos Flush]".
	Label  string
	Logger *log.Logger
}

// Slot is one probed sequence whose slot holds a value: the sequence and the
// 32-byte commitment (or acknowledgement) hash the router stores for it.
//
// The hash is what says WHICH packet the sequence names. The router reuses a
// sequence position after a client is re-created, and the event index keeps the
// old send, so a lookup by sequence alone can rebuild a packet that closed long
// ago. Comparing the rebuilt packet's commitment against this hash is what tells
// the two apart -- the same check the Cosmos source makes against its store.
type Slot struct {
	Sequence uint64
	Value    ethcommon.Hash
}

// Window is the sequence range one pass probes, chosen from the router's
// prevSequenceSend and a rotating cursor.
//
// The cursor is the LOWEST sequence the previous pass probed; the next pass
// continues below it. Zero means "start from the newest", which is also where
// the walk wraps to after reaching sequence 1. Newest-first is deliberate: the
// live path handles what is new, so the packets an enumeration is most likely
// to be needed for are the ones just below the live window -- and a lost cursor
// or a second relayer both want those first.
//
// prev is prevSequenceSend; a router that has never sent has nothing to probe.
func Window(prev, cursor uint64, cap int) (lo, hi uint64, ok bool) {
	if prev == 0 || cap <= 0 {
		return 0, 0, false
	}
	hi = prev
	if cursor > 1 && cursor <= prev {
		hi = cursor - 1
	}
	lo = 1
	if uint64(cap) < hi {
		lo = hi - uint64(cap) + 1
	}
	return lo, hi, true
}

// PrevSequenceSend reads the last sequence the router assigned to a send on
// clientID; see services.EthPrevSequenceSendKey for why it is a storage read.
func (r Router) PrevSequenceSend(ctx context.Context, clientID string) (uint64, error) {
	word, err := r.Client.StorageAt(ctx, r.Address, services.EthPrevSequenceSendKey(clientID), nil)
	if err != nil {
		return 0, fmt.Errorf("%s read prevSequenceSend(%s): %w", r.Label, clientID, err)
	}
	prev, err := services.EthStorageUint64(word)
	if err != nil {
		return 0, fmt.Errorf("%s prevSequenceSend(%s): %w", r.Label, clientID, err)
	}
	return prev, nil
}

// OutstandingSlots probes the router's storage for each sequence and returns the
// ones whose slot is non-zero, in the order given. pathFor builds the ICS-24
// path to probe for a sequence -- the commitment path for sends, the
// acknowledgement path for acks -- so the same probe serves both.
//
// It is batched so that a pass costs one round trip per probeBatch sequences
// rather than one per sequence. A provider that rejects batches is reported,
// not worked around: the pass would otherwise be quietly 100× slower.
func (r Router) OutstandingSlots(ctx context.Context, sequences []uint64, pathFor func(uint64) []byte) ([]Slot, error) {
	var out []Slot
	for start := 0; start < len(sequences); start += probeBatch {
		end := min(start+probeBatch, len(sequences))
		batch := make([]rpc.BatchElem, 0, end-start)
		results := make([]string, end-start)
		for i, seq := range sequences[start:end] {
			batch = append(batch, rpc.BatchElem{
				Method: "eth_getStorageAt",
				Args:   []any{r.Address, services.EthIBCStorageKey(pathFor(seq)), "latest"},
				Result: &results[i],
			})
		}
		if err := r.Client.Client().BatchCallContext(ctx, batch); err != nil {
			return nil, fmt.Errorf("%s probe %d slot(s): %w", r.Label, len(batch), err)
		}
		for i, elem := range batch {
			if elem.Error != nil {
				return nil, fmt.Errorf("%s probe seq=%d: %w", r.Label, sequences[start+i], elem.Error)
			}
			value := ethcommon.HexToHash(results[i])
			if value == (ethcommon.Hash{}) {
				continue
			}
			out = append(out, Slot{Sequence: sequences[start+i], Value: value})
		}
	}
	return out, nil
}

// Sequences lists lo..hi descending, newest first, which is the order the
// probe wants its results in.
func Sequences(lo, hi uint64) []uint64 {
	if hi < lo {
		return nil
	}
	out := make([]uint64, 0, hi-lo+1)
	for seq := hi; ; seq-- {
		out = append(out, seq)
		if seq == lo {
			return out
		}
	}
}

// SendCursor is the send cursor after a pass over the window starting at lo,
// whose outstanding slots (newest first, as OutstandingSlots returns them) were
// resolved up to but not including slots[stop]; stop == len(slots) means the
// pass got through all of them.
//
// A pass that runs out of time mid-window must not skip what it never looked at:
// setting the cursor to lo regardless would leave every unresolved send for a
// whole rotation. So the next pass resumes at slots[stop]. A pass that resolved
// nothing still moves past slots[0], so one lookup slower than the whole budget
// cannot pin the cursor and starve every sequence below it.
func SendCursor(lo uint64, slots []Slot, stop int) uint64 {
	switch {
	case stop >= len(slots):
		return lo
	case stop <= 0:
		return slots[0].Sequence
	default:
		return slots[stop].Sequence + 1 // Window resumes at cursor-1
	}
}

// SendCursorAt is the send cursor for a pass that must retry slots[at] next time,
// even when at is 0. SendCursor moves past slots[0] when a pass resolved nothing,
// because a lookup slower than the whole budget would otherwise pin the cursor;
// that guard is wrong for a send the pass REFUSED to handle (it could not be
// tracked), where skipping it leaves a packet nobody is watching until the
// rotation wraps. The next window's top is slots[at].Sequence.
func SendCursorAt(slots []Slot, at int) uint64 {
	return slots[at].Sequence + 1 // Window resumes at cursor-1
}

// AckCursor is the acknowledgement cursor after a pass over window (the
// sequences services.CosmosCommitmentWindow returned, in its order), whose
// written slots (a subsequence of window, as OutstandingSlots preserves order)
// were resolved up to but not including written[stop]. Same rules as
// SendCursor: resume at the first unresolved slot, but always move past at
// least one.
func AckCursor(window []uint64, written []Slot, stop int) uint64 {
	switch {
	case stop >= len(written):
		return window[len(window)-1]
	case stop <= 0:
		return written[0].Sequence
	default:
		return written[stop].Sequence - 1 // CosmosCommitmentWindow resumes at cursor+1
	}
}

// SendLog finds the SendPacket log the router emitted for the packet whose
// commitment the router holds at (clientID, sequence). Both are indexed topics,
// so the filter is cheap; the range is walked back from the head in bounded
// chunks (see findLog), narrowed the way the scan loop narrows when the provider
// refuses it.
//
// The filter can match MORE than one log: a client re-created on the same id
// restarts its sequences, and the log index keeps every generation's send. Only
// the packet whose commitment equals what the router holds is the current one,
// so every candidate is checked and the older generations are passed over --
// stopping at the first would rebuild a packet that closed long ago, and within
// one chunk the older log is the one that comes back first.
func (r Router) SendLog(ctx context.Context, clientID string, sequence uint64, commitment ethcommon.Hash) (*contractICS26Router.ContractICS26RouterSendPacket, bool, error) {
	filterer, err := contractICS26Router.NewContractICS26RouterFilterer(r.Address, r.Client)
	if err != nil {
		return nil, false, fmt.Errorf("%s bind router filterer: %w", r.Label, err)
	}
	var found *contractICS26Router.ContractICS26RouterSendPacket
	stale := 0
	ok, err := r.findLog(ctx, sequence, func(opts *bind.FilterOpts) (bool, error) {
		it, err := filterer.FilterSendPacket(opts, []string{clientID}, []*big.Int{new(big.Int).SetUint64(sequence)})
		if err != nil {
			return false, err
		}
		defer it.Close()
		for it.Next() {
			if PacketCommitment(it.Event.Packet, it.Event.Sequence) == commitment {
				found = it.Event
				return true, nil
			}
			stale++
		}
		return false, it.Error()
	})
	if err == nil && !ok && stale > 0 {
		r.warnf("seq=%d: %d SendPacket log(s) carry this sequence and none matches the commitment the router holds (re-created client?); skipping",
			sequence, stale)
	}
	return found, ok, err
}

// WriteAckLog finds the WriteAcknowledgement log the router emitted for
// (clientID, sequence), where clientID is the packet's DESTINATION client --
// the one the router received it on -- and commitment is what the SENDING chain
// holds for the packet. Same generation rule as SendLog: the sender's
// commitment decides which of several same-sequence logs is the live one.
func (r Router) WriteAckLog(ctx context.Context, clientID string, sequence uint64, commitment []byte) (*contractICS26Router.ContractICS26RouterWriteAcknowledgement, bool, error) {
	filterer, err := contractICS26Router.NewContractICS26RouterFilterer(r.Address, r.Client)
	if err != nil {
		return nil, false, fmt.Errorf("%s bind router filterer: %w", r.Label, err)
	}
	var found *contractICS26Router.ContractICS26RouterWriteAcknowledgement
	stale := 0
	ok, err := r.findLog(ctx, sequence, func(opts *bind.FilterOpts) (bool, error) {
		it, err := filterer.FilterWriteAcknowledgement(opts, []string{clientID}, []*big.Int{new(big.Int).SetUint64(sequence)})
		if err != nil {
			return false, err
		}
		defer it.Close()
		for it.Next() {
			if bytes.Equal(PacketCommitment(it.Event.Packet, it.Event.Sequence).Bytes(), commitment) {
				found = it.Event
				return true, nil
			}
			stale++
		}
		return false, it.Error()
	})
	if err == nil && !ok && stale > 0 {
		r.warnf("ack seq=%d: %d WriteAcknowledgement log(s) carry this sequence and none matches the commitment the sender holds; skipping",
			sequence, stale)
	}
	return found, ok, err
}

// PacketCommitment is the ICS-24 commitment of a packet as the router logged
// it: the same bytes the router stores in its commitment slot and Cosmos stores
// under the sender's client, so it is the one value both sides can be compared
// on. Solidity's packetCommitmentBytes32 and ibc-go's CommitPacket agree byte
// for byte (services/evm_flush_test.go pins it).
func PacketCommitment(pkt contractICS26Router.IICS26RouterMsgsPacket, sequence *big.Int) ethcommon.Hash {
	return ethcommon.BytesToHash(channeltypesv2.CommitPacket(subscriber.EthPacketToCosmosPacket(pkt, sequence)))
}

// findLog runs one topic-filtered lookup, walking back from the head in chunks.
// With a known DeployBlock the first chunk is [DeployBlock, head]; without one
// it is the newest DefaultLogChunk blocks. Either way the span the provider
// forced, once it has refused one, takes over.
//
// It never asks for [0, head]. The packets an enumeration is for are far more
// likely recent than ancient, a full-range eth_getLogs is the most expensive
// query a node can be asked -- once per probed sequence per pass -- and a node
// with pruned history refuses any range that reaches below its cutoff (#537).
// On a range refusal the span is narrowed on the same ladder the scan loop uses;
// on a pruned-history refusal the walk learns the node's floor and stops there.
// It stops after MaxLogChunks rather than walking a rollup's whole history one
// provider-sized chunk at a time.
func (r Router) findLog(ctx context.Context, sequence uint64, query func(*bind.FilterOpts) (bool, error)) (bool, error) {
	head, err := r.Client.BlockNumber(ctx)
	if err != nil {
		return false, fmt.Errorf("%s head: %w", r.Label, err)
	}
	span := r.Span
	if span == nil {
		span = &relayerclient.LogSpan{}
	}
	from, to := max(r.DeployBlock, span.Floor), head
	if from > to {
		if r.DeployBlock > head {
			r.warnf("seq=%d: router_deploy_block %d is above the head %d; check that it names this chain's router",
				sequence, r.DeployBlock, head)
		}
		return false, nil // the node keeps no history at or below its head
	}
	chunks := 0
	for {
		width := span.Width(from, to)
		if span.Chunk == 0 && r.DeployBlock == 0 && width > DefaultLogChunk {
			width = DefaultLogChunk
		}
		lo := from
		if to-from+1 > width {
			lo = to - width + 1
		}
		opts := &bind.FilterOpts{Start: lo, End: &to, Context: ctx}
		found, err := query(opts)
		if err == nil {
			if found {
				return true, nil
			}
			if lo <= from {
				if from == span.Floor && span.Floor > r.DeployBlock {
					r.warnf("seq=%d: no log between the provider's pruned-history floor %d and the head; "+
						"its event is older than this provider keeps -- point this source at an archive provider to enumerate it",
						sequence, from)
				}
				return false, nil // everything the provider serves has been asked
			}
			chunks++
			if chunks >= MaxLogChunks {
				r.warnf("seq=%d: no log in the newest %d blocks (%d chunks of %d); giving up on it this pass. "+
					"Its commitment says it is outstanding, so the event is older than this provider will serve in one walk -- "+
					"point this source at a provider without a block-range cap to enumerate it",
					sequence, to-lo+1+uint64(chunks-1)*width, chunks, width)
				return false, nil
			}
			to = lo - 1
			continue
		}
		if relayerclient.IsPrunedHistory(err) {
			floor, perr := r.prunedFloor(ctx, lo, to)
			if perr != nil {
				return false, fmt.Errorf("%s seq=%d: locate pruned-history floor in [%d,%d]: %w", r.Label, sequence, lo, to, perr)
			}
			if floor != span.Floor {
				r.warnf("provider has pruned history below block %d; log lookups start there from now on", floor)
			}
			span.Floor, from = floor, floor
			if from > to {
				// [lo,to] was all below the floor and everything above it was
				// already asked.
				r.warnf("seq=%d: no log between the provider's pruned-history floor %d and the head; "+
					"its event is older than this provider keeps -- point this source at an archive provider to enumerate it",
					sequence, floor)
				return false, nil
			}
			continue // re-ask [floor, to]: the part of the refused chunk the node still serves
		}
		if !relayerclient.IsLogRangeRejection(err) {
			return false, fmt.Errorf("%s seq=%d logs [%d,%d]: %w", r.Label, sequence, lo, to, err)
		}
		next, bottom, ok := chain.Climb(
			chain.NeedsChange(err, chain.HalvingLadder(relayerclient.KnobBlocksPerLogRange, width)),
			0, // each rejection re-anchors on the span that was just refused
		)
		if !ok {
			return false, fmt.Errorf("%s seq=%d: %w", r.Label, sequence, bottom)
		}
		span.Chunk = next.To
		r.warnf("provider refused a %d-block log range; narrowing to %d and walking back from %d: %v", width, next.To, to, err)
	}
}

// prunedFloor finds the lowest block in (lo, to+1] the provider still serves logs
// for, given that a query starting at lo was refused as pruned history. to+1
// means the whole of [lo,to] is pruned.
//
// It bisects with single-block probes on a topic nothing emits, so a probe never
// returns logs and never feeds the caller's query callback a range twice: the
// callback sees [floor,to] exactly once, after this returns. A geth node
// rejects on fromBlock alone, before it reads any log, so a probe is cheap.
// The cost is about log2(chunk) calls, paid once per node: the floor is kept on
// the LogSpan, and a node that prunes further is re-detected the same way.
func (r Router) prunedFloor(ctx context.Context, lo, to uint64) (uint64, error) {
	pruned, served := lo, to+1
	for served-pruned > 1 {
		mid := pruned + (served-pruned)/2
		_, err := r.Client.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(mid),
			ToBlock:   new(big.Int).SetUint64(mid),
			Addresses: []ethcommon.Address{r.Address},
			Topics:    [][]ethcommon.Hash{{{}}}, // the zero topic: no event has it
		})
		switch {
		case err == nil:
			served = mid
		case relayerclient.IsPrunedHistory(err):
			pruned = mid
		default:
			return 0, fmt.Errorf("probe block %d: %w", mid, err)
		}
	}
	return served, nil
}

// warnf logs a lookup problem, and every message sent through it is one: a
// provider that cannot answer, history that is gone, a sequence whose logs do not
// match. The level token goes ahead of the label because logbridge reads it from
// the start of the message.
func (r Router) warnf(format string, args ...any) {
	if r.Logger == nil {
		log.Printf(logbridge.Warn+r.Label+" "+format, args...)
		return
	}
	r.Logger.Printf(logbridge.Warn+r.Label+" "+format, args...)
}
