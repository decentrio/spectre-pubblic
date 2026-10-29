package services

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/rpc"
)

// ErrPermanentRelayFailure marks a relay failure as deterministic / on-chain:
// the tx was actually included and reverted (ETH receipt.Status==0, or a Cosmos
// DeliverTx non-zero code). Such a failure will recur on retry with the same
// inputs, so the adapter module DROPS the packet (via chain.Permanent) instead
// of re-queueing it forever — a genuinely stuck packet is refunded by the
// timeout scanner once it expires undelivered.
//
// Infrastructure failures (RPC down, beacon finality not yet reached, light-
// client build failure, broadcast timeout, account-sequence mismatch at CheckTx)
// are NOT wrapped with this — they are transient and must be re-queued (with a
// waiting backoff), so a valid packet is never dropped just because the infra
// was briefly unavailable (issue #80 review).
//
// Tx handlers (relayer/transaction) wrap their revert errors with this via
// fmt.Errorf("...: %w", ErrPermanentRelayFailure); callers classify with
// errors.Is.
var ErrPermanentRelayFailure = errors.New("permanent relay failure (on-chain revert)")

// ErrUnresolvedEVMNonce marks a failure whose transaction may STILL EXECUTE.
//
// It is not the same claim as ErrPermanentRelayFailure, and conflating the two
// is what made this necessary. Permanent means "this attempt cannot succeed --
// the chain rejected it deterministically", and relay/batch.go acts on that by
// ISOLATING a multi-packet batch: it resubmits each packet on its own so one
// poison packet does not discard its valid siblings. That is right for a revert
// and wrong here.
//
// Reported by @DongLieu on #485. When the relayer gives up waiting for a
// transaction, the abandoned nonce has one of three outcomes and the node has
// told us none of them: the cancellation replaced it, the original still
// executes, or the original already did. Resubmitting the payload -- as a whole
// batch, as isolated packets, or as a timeout retry -- can run the operation a
// second time while the first is still live, and the invalidated nonce cache
// means the second attempt may take a LOWER nonce and mine first.
//
// So a failure carrying this sentinel means: do not resubmit anything on this
// path until the outcome at that nonce is established. The packets stay in the
// pending tracker, which is the durable record, and settle by their terminal
// event or are refunded by the timeout scanner.
var ErrUnresolvedEVMNonce = errors.New("the outcome of an abandoned EVM nonce is unresolved; the payload must not be resubmitted")

// ErrValidatorCacheRace marks an updateClient revert caused by the validator
// cache changing after the relayer built a cached/delta update. Rebuilding the
// update from fresh on-chain state, or retrying with the full validator set, can
// make progress, so it is treated as transient (re-queued), not permanent.
var ErrValidatorCacheRace = errors.New("validator cache race")

// ErrForcedRotationNotReady marks a forced pinned-set rotation that could not be
// built because the on-chain client already sits at the target height.
// The builder uses the latest trusted height as its proof anchor, so it needs a
// newer source block to build the re-pin. Reporting
// this as success would let the periodic rotation wait a full interval with the
// pinned snapshot unrefreshed (#541). The periodic loop polls for a new block
// without applying transaction-failure backoff: no proof or tx has been sent.
var ErrForcedRotationNotReady = errors.New("forced rotation not ready: client already at target height")

// ErrClientNotActive marks a destination rejection caused by the light client
// itself, not by anything in the submitted payload: the SpectreClient on EVM
// reverted with FrozenClientState(), or ibc-go on Cosmos reported the client as
// frozen or otherwise not Active.
//
// It is deliberately NOT ErrPermanentRelayFailure. Permanent means "this packet
// is dead", and relay/batch.go drops a packet it is told that about. A frozen
// client says nothing about the packet: the same packet succeeds after an
// unfreeze() or a client migration. Dropping it on this error discarded every
// packet in flight, one per transaction, with nothing in the log naming the
// freeze (#402). The relay module holds the whole direction instead.
var ErrClientNotActive = errors.New("destination light client is not active (frozen)")

// CosmosStageSimulation is CosmosTxFailure.Stage for a rejection returned by the
// node's Simulate query: the node executed the messages and refused them.
//
// Its Code and Codespace are NOT the message handler's. Simulate is a gRPC query
// routed through ABCI, and the SDK (v0.53.3) re-wraps the handler error as
// sdk/6 (ErrUnknownRequest), keeping the original only as text in Log. So a
// frozen client's client/29 arrives here as sdk/6, and a code-based classifier
// cannot see it; callers that need the cause must ask the chain instead.
const CosmosStageSimulation = "Simulation"

// IsCosmosSimulationRejection reports whether err carries a Simulate-query
// rejection -- the node ran the messages and refused them -- as opposed to a
// transport failure reaching the node, which carries no ABCI metadata.
func IsCosmosSimulationRejection(err error) bool {
	var failure *CosmosTxFailure
	return errors.As(err, &failure) && failure.Stage == CosmosStageSimulation
}

// CosmosTxFailure preserves ABCI failure metadata across package boundaries so
// relay logic can classify specific failures without scraping formatted strings.
type CosmosTxFailure struct {
	Stage     string
	Code      uint32
	Codespace string
	Log       string
	Data      []byte
	Err       error
}

func (e *CosmosTxFailure) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("cosmos tx failed at %s with code %d", e.Stage, e.Code)
	if e.Codespace != "" {
		msg += fmt.Sprintf(" codespace=%s", e.Codespace)
	}
	if e.Log != "" {
		msg += fmt.Sprintf(": %s", e.Log)
	}
	return msg
}

func (e *CosmosTxFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BatchPartialError is returned when a split batch is partially successful.
// It tracks how many messages at the beginning of the batch were successfully
// committed before a subsequent sub-batch encountered an error.
type BatchPartialError struct {
	SucceededCount int
	Err            error
}

func (e *BatchPartialError) Error() string {
	return fmt.Sprintf("partial batch failure: %d messages succeeded: %v", e.SucceededCount, e.Err)
}

func (e *BatchPartialError) Unwrap() error {
	return e.Err
}

// CommittedBeforeFailure reads the delivered prefix out of a batch failure.
//
// A split batch can commit its leading sub-batches and then fail, and that count
// is the only thing that tells a caller how much of its batch to re-queue.
// Dropping it forces the caller to choose between re-submitting what was already
// delivered and discarding what was not.
//
// Any other error means nothing was delivered: the send either never reached a
// transaction or the single transaction it built failed as a whole. It lives here
// rather than in an adapter because both Cosmos-bound destinations need it and
// both already depend on this package.
func CommittedBeforeFailure(err error) int {
	var partial *BatchPartialError
	if errors.As(err, &partial) && partial.SucceededCount > 0 {
		return partial.SucceededCount
	}
	return 0
}

// IsEVMRevert reports whether the node executed the call and the contract
// reverted, as opposed to the call never reaching or never leaving the node.
//
// The distinction decides whether a failure is fatal or switches a caller onto a
// fallback, so it is made only on signals that mean the EVM ran:
//
//   - JSON-RPC error code 3, which geth, Anvil and Hardhat return for a revert
//     that carries data;
//   - error data that IS revert data: a 0x-prefixed hex string (geth's encoding
//     of the returned bytes) or a string naming the revert (Nethermind and
//     Erigon put "Reverted 0x..." there under their own codes);
//   - "execution reverted" in the message, for a revert that returned nothing.
//
// Non-nil ErrorData alone is NOT one of them. The JSON-RPC spec lets the data
// member carry any server-defined detail, so a -32005 rate limit with
// {"retryAfter": 1} has data too. Treating it as a revert read a throttled
// endpoint as a pre-#541 SpectreClient and silently fell back to the latest-height
// expiry anchor -- the inaccurate one -- instead of retrying (review of d85c8cd).
// Implementing rpc.DataError is weaker still: go-ethereum's jsonError satisfies
// it for every node-returned error.
func IsEVMRevert(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) && rpcErr.ErrorCode() == jsonRPCExecutionError {
		return true
	}
	var dataErr rpc.DataError
	if errors.As(err, &dataErr) && isRevertData(dataErr.ErrorData()) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "execution reverted")
}

// jsonRPCExecutionError is the JSON-RPC error code nodes use for a call the EVM
// executed and the contract reverted, with the revert bytes as data.
const jsonRPCExecutionError = 3

// isRevertData reports whether a JSON-RPC error's data member is revert output
// rather than some other server-defined detail. Only a string can be: revert
// bytes travel hex-encoded, and a structured object is a server's own payload.
func isRevertData(data any) bool {
	str, ok := data.(string)
	if !ok {
		return false
	}
	str = strings.TrimSpace(str)
	if strings.HasPrefix(str, "0x") || strings.HasPrefix(str, "0X") {
		return true
	}
	return strings.Contains(strings.ToLower(str), "revert")
}
