// This file holds what both directions share: the Subscriber itself, the
// dependency structs, the recovery-cursor helpers and the event-name constants.
//
// The two directions live in cosmos.go and ethereum.go. They were one 1734-line
// file whose function names already spelled out the symmetry --
// resumeCosmosCursor / resumeEthCursors, subscribeCosmosOnce / subscribeEthOnce,
// recoverCosmosGapToLatest / recoverEthGapToLatest -- while the layout did not,
// so the mirror check that non-functional requirement #5 asks for meant scrolling
// eight hundred lines rather than diffing two files.
package subscriber

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"relayer/services"
)

// Match by IBC v2 event TYPE (send_packet / write_acknowledgement / timeout_packet)
// rather than by message.action. The Rust upstream relayer takes the same approach
// (packages/relayer/lib/src/events/eureka.rs::TryFrom<TmEvent>) — it iterates the tx's
// events and matches event.kind against cosmos_sdk::EVENT_TYPE_*. Using event TYPE
// here means we catch any tx that produces a send_packet (whether triggered by
// gaiad's MsgTransfer wrapper, a direct channeltypesv2.MsgSendPacket from a test
// harness, or any other module that ends up calling the v2 channel keeper), instead
// of being tied to one specific message.action.
const COMETBFT_SEND_PACKET_EVENT = "tm.event = 'Tx' AND send_packet.encoded_packet_hex EXISTS"
const COMETBFT_WRITE_ACK_PACKET_EVENT = "tm.event = 'Tx' AND write_acknowledgement.encoded_packet_hex EXISTS"
const COMETBFT_TIMEOUT_PACKET_EVENT = "tm.event = 'Tx' AND timeout_packet.encoded_packet_hex EXISTS"

// acknowledge_packet is the fourth event and the only TERMINAL one on this side:
// Cosmos emits it when it consumes the acknowledgement for a packet it sent, so
// the packet is settled. Without it the pending tracker only learns of a
// settlement by querying, and a packet another relayer acknowledged sits in the
// tracker until a scan happens to look.
const COMETBFT_ACK_PACKET_EVENT = "tm.event = 'Tx' AND acknowledge_packet.encoded_packet_hex EXISTS"

const EVENT_SEND_PACKET_FIELD = "send_packet.encoded_packet_hex"
const EVENT_WRITE_ACK_PACKET_FIELD = "write_acknowledgement.encoded_packet_hex"
const EVENT_ACKNOWLEDGEMENT_FIELD = "write_acknowledgement.encoded_acknowledgement_hex"
const EVENT_TIMEOUT_PACKET_FIELD = "timeout_packet.encoded_packet_hex"
const EVENT_ACK_PACKET_FIELD = "acknowledge_packet.encoded_packet_hex"
const EVENT_TX_HEIGHT_FIELD = "tx.height"

const ethStartupRecoveryLookbackEnv = "ETH_STARTUP_LOOKBACK_BLOCKS"
const defaultEthStartupRecoveryLookbackBlocks uint64 = 256
const ethSubscriptionReconnectDelay = 2 * time.Second
const ethSeenEventRetentionBlocks uint64 = 2_000

const cosmosStartupRecoveryLookbackEnv = "COSMOS_STARTUP_LOOKBACK_BLOCKS"
const defaultCosmosStartupRecoveryLookbackBlocks uint64 = 256
const cosmosSubscriptionReconnectDelay = 2 * time.Second
const subscriptionCleanupTimeout = 5 * time.Second
const subscriberRPCTimeout = 15 * time.Second
const cosmosRecoveryChunkEnv = "COSMOS_RECOVERY_CHUNK_HEIGHTS"
const defaultCosmosRecoveryChunkHeights uint64 = 256

// cosmosLiveEventBuffer is the per-subscription channel depth requested from
// CometBFT. Passing it is not a tuning nicety -- the default is 1, and a full
// channel makes CometBFT DROP the event:
//
//	// cometbft rpc/client/http/http.go, eventListener
//	select {
//	case out <- *result:
//	default:
//	    w.Logger.Error("wanted to publish ResultEvent, but out channel is full", ...)
//	}
//
// The send is non-blocking, so there is no backpressure, and w.Logger is a nop
// logger unless SetLogger is called -- so a dropped packet leaves no trace
// anywhere. Meanwhile this subscriber's consuming goroutine also runs the 30s
// gap-recovery scan (several paginated TxSearch calls) in the same select, so it
// stops reading for seconds at a time, every 30 seconds.
//
// The Ethereum side does not need this: go-ethereum's generated watcher sends
// blocking and its RPC client queues 20,000 events before failing loudly with
// ErrSubscriptionQueueOverflow, which subscribeEthOnce already surfaces.
//
// A buffer is a mitigation, not a fix -- it still drops once full. The fix is to
// keep slow work off the consuming goroutine; see #376.
const cosmosLiveEventBuffer = 1024

// cosmosLivePathStaleAfter is how long the live subscription may deliver nothing while
// gap recovery is finding packets before that combination is reported.
//
// Recovery is the backstop; the live subscription is meant to be what delivers.
// When recovery starts finding packets the live path should have delivered, the
// live path is failing -- and it fails silently, because a subscription that
// delivers nothing looks exactly like a quiet chain. That is how a Cosmos->Base
// packet was lost unnoticed: every packet for 14 minutes arrived via recovery,
// and nothing said so.
const cosmosLivePathStaleAfter = 2 * time.Minute

// cosmosIndexerLagBlocks is how far behind the head gap recovery stops scanning.
//
// The scan reads two different subsystems and assumes they agree. The range end
// comes from Status.SyncInfo.LatestBlockHeight, which is set when the block
// commits; the results come from TxSearch, which reads the tx indexer -- and the
// indexer is a SEPARATE goroutine consuming the event bus after commit
// (cometbft state/txindex/indexer_service.go). So a tx in the newest block is
// routinely committed but not yet indexed.
//
// That race loses the packet permanently, because the scan then advances the
// cursor past the height it just failed to read:
//
//	latest := Status()                    // 202195, committed
//	TxSearch(..., "tx.height <= 202195")  // indexer has not written 202195 yet -> empty
//	cursor = 202195 + 1                   // 202195 is never scanned again
//
// and it is silent: an empty scan increments nothing, so nothing is logged.
// Observed on a live devnet -- one Cosmos->Base transfer at height 202195 was
// never relayed and never timed out, leaving its funds escrowed.
//
// Two blocks is far more than the indexer needs (its lag is milliseconds) and
// costs the backstop about ten seconds of extra detection delay. Rescanning is
// free: seenEvents dedups, and recovery re-checks on-chain state anyway.
const cosmosIndexerLagBlocks uint64 = 2

// quietScanHeartbeat is how many consecutive find-nothing recovery scans pass
// before one is reported, so a healthy subscriber stays visible (~10 min at the
// 30s interval) without printing every tick.
const quietScanHeartbeat = 20

// advanceQuietScans counts one find-nothing pass and reports whether this is the
// one to log. Shared by both directions so the two heartbeats cannot drift.
func advanceQuietScans(quietScans *uint64) bool {
	*quietScans++
	return *quietScans%quietScanHeartbeat == 0
}

const cosmosTxSearchPerPage = 100
const cosmosSeenEventRetentionHeights uint64 = 2_000

const ethRecoveryChunkEnv = "ETH_RECOVERY_CHUNK_BLOCKS"
const defaultEthRecoveryChunkBlocks uint64 = 256

const cometBFTSendPacketTxSearch = "send_packet.encoded_packet_hex EXISTS"
const cometBFTWriteAckPacketTxSearch = "write_acknowledgement.encoded_packet_hex EXISTS"
const cometBFTTimeoutPacketTxSearch = "timeout_packet.encoded_packet_hex EXISTS"
const cometBFTAckPacketTxSearch = "acknowledge_packet.encoded_packet_hex EXISTS"

// normalizeTimeoutSeconds converts IBC v2 timeout timestamps from nanoseconds to seconds.
// ibc-go stores TimeoutTimestamp in nanoseconds, but the ETH side uses seconds.
// Values < 1e12 are assumed to already be in seconds (year ~33658 CE in seconds).
func normalizeTimeoutSeconds(ts uint64) uint64 {
	if ts >= 1e12 {
		return ts / 1e9
	}
	return ts
}

type Subscriber struct {
	recovery *services.RecoveryStateStore
}

type cosmosDeps struct {
	Cosmos services.CosmosEndpoint
	EVM    services.EVMEndpoint
	IDs    services.ClientIDs
	Logger *log.Logger
}

type ethDeps = cosmosDeps

func NewSubscriber(recovery ...*services.RecoveryStateStore) *Subscriber {
	var store *services.RecoveryStateStore
	if len(recovery) > 0 {
		store = recovery[0]
	}
	return &Subscriber{recovery: store}
}

func clampRecoveryCursor(cursor, lowestUnsubmitted uint64) uint64 {
	if lowestUnsubmitted != 0 && lowestUnsubmitted < cursor {
		return lowestUnsubmitted
	}
	return cursor
}

func recoveryChunkSize(envName string, fallback uint64) uint64 {
	if raw := os.Getenv(envName); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err == nil && value > 0 {
			return value
		}
		log.Printf("WARN [start] ignoring invalid %s=%q; using %d", envName, raw, fallback)
	}
	return fallback
}

func sleepOrDone(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Recovery cursor keys. They name the SCAN each cursor belongs to, and they are
// declared here rather than in services because the stream owns its own key --
// that is what lets a new chain add a key at its own call site instead of a
// field in the shared struct.
//
// One process keeps one cursors.json, so these share a namespace with the keys
// any other source in the same process declares (see chain/l2rollup). Keep them
// distinct.
const (
	cursorCosmos      = "cosmos"
	cursorEthSend     = "eth_send"
	cursorEthWriteAck = "eth_write_ack"
)
