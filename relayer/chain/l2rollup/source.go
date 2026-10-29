package l2rollup

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	attestorpb "attestor/types/attestor"
	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// HeadKind is the L2 confirmation policy — the single anti-reorg knob — using the
// OP Stack / attestor head vocabulary (unsafe / safe / finalized). The on-chain L2
// light client authenticates the exact L2 block identity with its pinned attestor
// set; this chooses the replica head those attestors must answer against. Finality
// is not in the 164-byte statement, so config validation requires a distinct key
// set for every finality tier.
type HeadKind int

const (
	// Unsafe relays at the latest L2 block (skip-finality): lowest latency, exposed
	// to L2 reorgs / sequencer equivocation.
	Unsafe HeadKind = iota
	// Safe relays at the sequencer-safe head (batch posted to L1, not yet finalized).
	Safe
	// Finalized relays only at L2 blocks whose L1 batch is finalized: safe, highest
	// latency.
	Finalized
)

func (k HeadKind) validate() error {
	switch k {
	case Unsafe, Safe, Finalized:
		return nil
	default:
		return fmt.Errorf("l2 source: invalid head kind %d", k)
	}
}

// RunMode maps the configured head kind onto the attestor replica head a
// VerifyStateRoot request must be answered against, so the block the builder binds to
// is judged at the same finality the source gated the height on.
func (k HeadKind) RunMode() attestorpb.RunMode {
	switch k {
	case Safe:
		return attestorpb.RunMode_RUN_MODE_SAFE
	case Finalized:
		return attestorpb.RunMode_RUN_MODE_FINALIZED
	default:
		return attestorpb.RunMode_RUN_MODE_UNSAFE
	}
}

func (k HeadKind) String() string {
	switch k {
	case Unsafe:
		return "unsafe"
	case Safe:
		return "safe"
	case Finalized:
		return "finalized"
	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}

// Source is the EVM-L2 chain.Source (L2->Cosmos). An L2 is EVM, so packet
// membership proofs are the same eth_getProof account+storage proofs the ETH L1
// source builds — verified by the L2 light client against the L2 world state_root
// (account proof) then the IBC-handler storage_root (storage proof), per Dũng P2.
// The L2 difference is entirely the finality policy here.
type Source struct {
	chainType          chain.ChainType // OPStack or Arbitrum (for Chain())
	eth                *ethclient.Client
	headKind           HeadKind
	l2ClientID         string            // the ICS26Router client id on the L2 (event partition key)
	cosmosWasmClientID string            // the Cosmos wasm client id paired with l2ClientID
	router             ethcommon.Address // the L2 ICS26Router address (from rollup_profile.common.l2_router)

	// attestor gates RelayableHeight on a quorum of the configured attestors.
	// OP re-derives from L1; Arbitrum unsafe explicitly trusts the configured
	// Nitro node. When nil, RelayableHeight falls back to the raw L2 head.
	// srcChainID is the attestor's src_chain key (distinct from the on-L2
	// client id).
	attestor   AttestationFrontier
	srcChainID string

	// includeProvisional decides whether a verdict the attestor has not yet
	// confirmed at its chain-specific irreversible frontier may be relayed. It
	// is a separate axis from headKind and must stay one: headKind selects which
	// L2 head the source reads
	// (unsafe/safe/finalized), while provisional is about whether the attestor's
	// own finality re-check has completed for that root. Deriving one from the other
	// made "safe" silently imply "accept provisional", with no way to ask for safe
	// without it.
	includeProvisional bool

	// cosmosState reconciles scanned events against what Cosmos already records,
	// so a packet another relayer settled is not proven a second time. nil skips
	// reconciliation entirely (one-shot and test callers).
	cosmosState CosmosPacketState

	// recovery persists the scan cursor across restarts. nil keeps the cursor in
	// memory only, which is what the tests and any one-shot caller want.
	recovery *services.RecoveryStateStore

	// settle is called for terminal L2 events; see WithSettleHook. nil means the
	// source drops them, which is what it did before terminal events were read.
	settle func(packet []byte)

	// settleAck is called for the ACKNOWLEDGED half of those, and closes the
	// owed-acknowledgement record. Separate from settle because a timeout is also
	// terminal and settles no debt. nil means no ledger is running.
	settleAck func(packet []byte)

	// track registers a send with the pending tracker; see WithPendingTracker.
	// nil means the enumeration backstop cannot register what it skips.
	track func(packet []byte, height uint64) bool

	// logScanChunk caps the block span of a single eth_getLogs. 0 means "one call
	// for the whole range", which is what every provider that does not cap the span
	// wants. Providers that do cap it vary by three orders of magnitude (Alchemy's
	// free tier allows 10 blocks, drpc 10_000), and the failure is a plain 400 that
	// names neither the setting nor a workable value — so this is configuration,
	// not a constant.
	logScanChunk uint64

	// routerDeployBlock is the block the L2 router was deployed at; 0 when not
	// configured. The flush's log lookups start there (see evmflush.Router).
	routerDeployBlock uint64

	// flush is the enumeration backstop's cursors and log-range policy; see
	// flush.go. Touched only from UnrelayedPackets, which the relay module calls
	// on one goroutine.
	flush flushState
}

// Source satisfies chain.Source.
var _ chain.Source = (*Source)(nil)

// NewSource wires an L2 source. router is the L2 ICS26Router address (the packet
// membership proofs are taken against its storage_root). attestor may be nil
// (raw-head interim); when set, srcChainID identifies this L2 to the frontier.
func NewSource(chainType chain.ChainType, eth *ethclient.Client, headKind HeadKind, l2ClientID, cosmosWasmClientID, srcChainID string, router ethcommon.Address, attestor AttestationFrontier, includeProvisional bool) *Source {
	return &Source{
		chainType:          chainType,
		eth:                eth,
		headKind:           headKind,
		l2ClientID:         l2ClientID,
		cosmosWasmClientID: cosmosWasmClientID,
		router:             router,
		attestor:           attestor,
		srcChainID:         srcChainID,
		includeProvisional: includeProvisional,
	}
}

// WithRecoveryState gives this source a durable scan cursor. Without one the
// cursor lives only in Subscribe's stack frame, so a restart resumes at
// head - L2_STARTUP_LOOKBACK_BLOCKS and anything older is never rescanned --
// an acknowledgement written before that window strands its packet, and its
// escrow with it, silently (#308).
//
// An option rather than a constructor parameter because it is genuinely
// optional: the one-shot and test paths run without durable state, and
// NewSource already carries nine arguments.
func (s *Source) WithRecoveryState(store *services.RecoveryStateStore) *Source {
	s.recovery = store
	return s
}

// WithCosmosPacketState lets the scan reconcile against Cosmos before relaying:
// an event whose work is already settled there is dropped rather than proven
// again. Without it the L2 path is the only one of the four that relays purely on
// what it saw in a block, with no on-chain check that the work is still needed --
// so a second relayer settling a packet leaves this one proving it anyway.
//
// An option, not a constructor parameter, for the same reason as
// WithRecoveryState: the one-shot and test paths have no Cosmos endpoint.
func (s *Source) WithCosmosPacketState(state CosmosPacketState) *Source {
	s.cosmosState = state
	return s
}

// WithLogScanChunk caps the block span of each eth_getLogs this source issues.
// A zero or unset value keeps the single-call behaviour.
// WithSettleHook registers what to do when the L2 emits a TERMINAL packet event
// -- AckPacket or TimeoutPacket. Those close a packet's lifecycle rather than
// creating relay work, so they never become a chain.Event: handing one to the
// relay loop would produce an empty "relay" of a packet with nothing left to do.
//
// It takes the proto-marshaled packet, the same form chain.Event.Raw carries and
// the same the module's untrack hook already takes, so one settlement path
// serves both ways of learning that a packet is done.
//
// The hook is how the source settles the pending tracker without owning it. It
// matters because the alternative is finding out by QUERY: today an ack another
// relayer submitted leaves our packet in the tracker until a timeout scan looks,
// and the doc's acceptance for this is explicit -- another relayer settles the
// packet and our tracker drops it with no query at all.
//
// Optional; a source built without it simply ignores terminal events, which is
// the behaviour every L2 source had before this existed.
func (s *Source) WithSettleHook(settle func(packet []byte)) *Source {
	s.settle = settle
	return s
}

// WithAckSettleHook registers what to do when the L2 emits a terminal
// ACKNOWLEDGEMENT, as opposed to any terminal event.
//
// Reported by @DongLieu: WithSettleHook was wired to the pending tracker alone,
// so an acknowledgement another relayer submitted dropped this process's pending
// record and left its owed-acknowledgement debt untouched -- durable, surviving
// restart, and reported overdue forever for a packet that is settled on-chain.
func (s *Source) WithAckSettleHook(settle func(packet []byte)) *Source {
	s.settleAck = settle
	return s
}

// WithPendingTracker lets the enumeration backstop register a send it is about
// to skip. The backstop drops a send already past its timeout instead of
// relaying it, leaving the refund to the timeout scanner -- but the scanner reads
// only the pending tracker, and a send the block scan missed was never put there.
// Sends the backstop DOES return are tracked by the module's own hook; this one
// covers the ones it does not.
//
// It takes the proto-marshaled packet and its block height, the form of the
// module's relay.TrackFunc, and reports false when the durable write failed.
//
// Optional; without it an expired send found only by the backstop is not refunded.
func (s *Source) WithPendingTracker(track func(packet []byte, height uint64) bool) *Source {
	s.track = track
	return s
}

func (s *Source) WithLogScanChunk(n uint64) *Source {
	s.logScanChunk = n
	return s
}

// WithRouterDeployBlock sets the block the L2 router was deployed at, so the
// enumeration backstop asks for the router's whole history in one log query
// instead of walking bounded windows back from the head. 0 keeps the walk.
func (s *Source) WithRouterDeployBlock(n uint64) *Source {
	s.routerDeployBlock = n
	return s
}

func (s *Source) Chain() chain.ChainType { return s.chainType }

// LatestHeight is the latest L2 block visible at the configured head tag — how far
// the subscriber can see packets. It is NOT the trust gate (that is RelayableHeight).
func (s *Source) LatestHeight(ctx context.Context) (uint64, error) { return s.head(ctx) }

// RelayableHeight is the highest L2 height a packet may be proven at. With an
// attestor it is the attestor's policy-selected frontier (AttestedUpTo);
// includeProvisional accepts the chain-specific provisional frontier. On Arbitrum
// unsafe this explicitly means trusting the configured Nitro node before any L1
// assertion exists. Without an attestor it degrades to the raw L2 head
// (skip-finality). A not-yet-attested source returns 0 — nothing is relayable yet,
// so the module waits rather than relaying an unverified height.
func (s *Source) RelayableHeight(ctx context.Context) (uint64, error) {
	if s.attestor == nil {
		return s.head(ctx)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	root, found, err := s.attestor.AttestedUpTo(cctx, s.srcChainID, s.includeProvisional)
	if err != nil {
		return 0, classifyAttestorFailure("RelayableHeight",
			fmt.Errorf("l2 source: attested-up-to (kind=%d): %w", s.headKind, err))
	}
	if !found {
		// found=false is NOT a failure: the attestor has simply not attested
		// anything yet. It is the same "not yet" as a RelayableHeight that has not
		// caught up, so it must not be classified, logged as an error, or charged to
		// any retry budget — the module waits.
		return 0, nil
	}
	return root.GetL2BlockNumber(), nil
}

// head reads the L2 head at the configured head-kind tag.
func (s *Source) head(ctx context.Context) (uint64, error) {
	if err := s.headKind.validate(); err != nil {
		return 0, err
	}
	tag := big.NewInt(int64(rpc.LatestBlockNumber)) // Unsafe
	switch s.headKind {
	case Safe:
		tag = big.NewInt(int64(rpc.SafeBlockNumber))
	case Finalized:
		tag = big.NewInt(int64(rpc.FinalizedBlockNumber))
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	h, err := s.eth.HeaderByNumber(cctx, tag)
	if err != nil {
		return 0, fmt.Errorf("l2 source: head (kind=%d): %w", s.headKind, err)
	}
	return h.Number.Uint64(), nil
}

// QueryHeader returns the target L2 height as an 8-byte big-endian integer for the
// L2 client-update builder.
//
// It used to prefix the selected head kind, so the builder could refuse to assemble a
// Safe or Finalized request from provisional evidence. The header carries no head kind
// now, and the selection has already been applied upstream: headKind picks which
// attestor frontier RelayableHeight gates on, so by the time a height reaches here it
// has passed that gate. Re-sending the kind would only let the builder second-guess a
// decision already made.
func (s *Source) QueryHeader(_ context.Context, height uint64) ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, height)
	return b, nil
}

// Subscribe (in subscribe.go) streams L2 ICS26Router packet events to the handler.

// MembershipProof builds the L2 eth_getProof (account proof vs the L2 world
// state_root, storage proof vs the ICS26Router storage_root) that the packet
// commitment (send) or acknowledgement exists at height — the exact mechanics of the
// ETH L1 source, reused against the L2 router. height is the L2 block the module
// advanced the destination L2 wasm client to (proofHeight = m.lastHeight), so no
// Cosmos-side read is needed here. Do NOT use a Cosmos app_hash — the two EVM roots
// are the L2 world state_root and the router account storage_root (Dũng P2).
func (s *Source) MembershipProof(ctx context.Context, packet []byte, height uint64, eventType chain.EventType) ([]byte, error) {
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return nil, fmt.Errorf("l2 source: decode packet: %w", err)
	}
	var clientID string
	var pathType byte
	switch eventType {
	case chain.SendPacket:
		// An L2->Cosmos send past its timeout can never be received on Cosmos, so
		// report it permanent and let the module DROP it. The async L2 timeout
		// scanner, fed by the pending tracker before this proof step, refunds it on
		// the L2 instead. Cosmos has ~wall-clock BFT time, so compare against
		// time.Now like the ETH path.
		if pkt.TimeoutTimestamp > 0 && uint64(time.Now().Unix()) >= pkt.TimeoutTimestamp {
			return nil, chain.Permanent(fmt.Errorf("l2 source: send seq=%d timed out; deferred to timeout scanner", pkt.Sequence))
		}
		clientID, pathType = pkt.SourceClient, 1 // packet commitment
	case chain.AckPacket:
		clientID, pathType = pkt.DestinationClient, 3 // ack
	default:
		return nil, fmt.Errorf("l2 source: MembershipProof: unsupported event type %d", eventType)
	}
	path := services.EthPath(clientID, pkt.Sequence, pathType)
	return l2StorageProof(ctx, s.eth, s.router, path, height)
}

// l2StorageProof proves one ICS26Router commitment slot at an L2 height, in the shape
// the L2 wasm client expects.
//
// This is NOT the Ethereum L1 membership proof (client.GetEthMembershipProof). That
// one wraps an account proof and a storage proof together and hex-encodes both,
// because the ETH light client re-derives the account from the L1 state root. The L2
// client already knows the router's storage root — it authenticated it through the
// rollup header's router_proof — so it wants a bare `EvmStorageProof` and rejects
// anything else outright:
//
//	unknown field `account_proof`, expected one of `key`, `value`, `proof`
//
// (serde `deny_unknown_fields`). Reusing the L1 shape here made every acknowledgement
// fail that way.
//
// The value is the full 32-byte storage word, not the minimal big-endian form
// eth_getProof reports: the client compares it byte-for-byte against the commitment
// the IBC host expects, which is a full 32-byte hash. The trie check is unaffected
// either way — the verifier strips leading zeros before RLP-encoding.
func l2StorageProof(ctx context.Context, l2 *ethclient.Client, router ethcommon.Address, path []byte, height uint64) ([]byte, error) {
	slot := ethcommon.HexToHash(services.ICS26_IBC_STORAGE_SLOT)
	storageKey := crypto.Keccak256Hash(crypto.Keccak256(path), slot.Bytes())

	raw, err := relayerclient.EthGetProof(ctx, l2, router, []ethcommon.Hash{storageKey}, new(big.Int).SetUint64(height))
	if err != nil {
		return nil, fmt.Errorf("l2 source: prove router slot at height %d: %w", height, err)
	}
	if len(raw.Storage) == 0 {
		return nil, fmt.Errorf("l2 source: eth_getProof returned no storage proof for key %s", storageKey.Hex())
	}
	sp := raw.Storage[0]

	var value [32]byte
	if len(sp.Value) > len(value) {
		return nil, fmt.Errorf("l2 source: storage value at %s is %d bytes, want <= 32", storageKey.Hex(), len(sp.Value))
	}
	copy(value[len(value)-len(sp.Value):], sp.Value) // left-pad to the full word

	// An all-zero word is an empty slot, not a commitment. Marshalling it anyway
	// produces a proof the L2 wasm client rejects, which costs a transaction and
	// reads as permanent -- dropping a packet whose only problem was being proven
	// at a height the destination client had not reached yet.
	if value == ([32]byte{}) {
		return nil, fmt.Errorf("l2 source: commitment slot %s is empty at height %d: %w",
			storageKey.Hex(), height, relayerclient.ErrCommitmentAbsent)
	}

	return json.Marshal(EvmStorageProof{
		Key:   hexBytes(storageKey.Bytes()),
		Value: byteList(value[:]),
		Proof: byteMatrix(sp.Proof),
	})
}

// NonMembershipProof proves that the L2 router has not written the receipt for
// a Cosmos-origin packet at height. It emits the bare EvmStorageProof expected
// by the attestor-backed L2 wasm client; the Ethereum beacon proof envelope is
// incompatible with that client.
//
// This is exported for the Cosmos->L2 timeout scanner. The normal L2 source
// path only proves membership because its own timeouts are settled on the L2.
func NonMembershipProof(ctx context.Context, l2 *ethclient.Client, router ethcommon.Address, packet channeltypesv2.Packet, height uint64) ([]byte, error) {
	path := services.EthPath(packet.DestinationClient, packet.Sequence, 2)
	storageKey := crypto.Keccak256Hash(crypto.Keccak256(path), ethcommon.HexToHash(services.ICS26_IBC_STORAGE_SLOT).Bytes())
	raw, err := relayerclient.EthGetProof(ctx, l2, router, []ethcommon.Hash{storageKey}, new(big.Int).SetUint64(height))
	if err != nil {
		return nil, fmt.Errorf("l2 source: prove absent receipt at height %d: %w", height, err)
	}
	if len(raw.Storage) == 0 {
		return nil, fmt.Errorf("l2 source: eth_getProof returned no storage proof for receipt key %s", storageKey.Hex())
	}
	sp := raw.Storage[0]
	if len(sp.Value) != 0 {
		return nil, fmt.Errorf("l2 source: receipt slot %s is populated at height %d: %w", storageKey.Hex(), height, relayerclient.ErrPacketAlreadyReceived)
	}
	return json.Marshal(EvmStorageProof{
		Key:   hexBytes(storageKey.Bytes()),
		Value: byteList(sp.Value),
		Proof: byteMatrix(sp.Proof),
	})
}

// Source.NonMembershipProof is unused on the L2 source relay path: L2-origin packet
// timeouts are handled by the async timeout scanner (as on the ETH source), not the
// Subscribe->relay flow, so no TimeoutPacket event reaches here.
func (s *Source) NonMembershipProof(_ context.Context, _ []byte, _ uint64) ([]byte, error) {
	return nil, fmt.Errorf("l2 source: NonMembershipProof unused (timeouts are scanner-handled)")
}
