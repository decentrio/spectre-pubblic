package services

import (
	"context"
	"fmt"
	"log"
	spectreContract "relayer/bindings/SpectreClient"
	client "relayer/client"
	"relayer/prover"
	"relayer/prover/witness"
	"time"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// read abi json file once in runtime
var tendermintAbiJson *abi.ABI

var initErr error

func init() {
	tendermintAbiJson, initErr = spectreContract.ContractSpectreClientMetaData.GetAbi()
	if initErr != nil {
		log.Fatal(initErr)
	}
}

type TransactionHandler interface {
	CreateCosmosClientContract(stdCtx context.Context, endpoint EVMEndpoint, clientIDs ClientIDs, clientState []byte, consensusState spectreContract.ISpectreTendermintMsgsConsensusState, initialPinnedValidatorSet client.ContractValidatorSet) (ethcommon.Address, error)
	// CreateWasmClient submits MsgCreateClient for any 08-wasm client (ETH beacon,
	// L2 rollup, ...) from an already-built wasm ClientState/ConsensusState and returns
	// the auto-assigned client id. counterpartyClientID is the client on the
	// counterparty chain that tracks Cosmos, registered inline (a naming binding); an
	// empty value skips registration (register it later once its id is known).
	CreateWasmClient(stdCtx context.Context, endpoint CosmosEndpoint, clientState ibcexported.ClientState, consensusState ibcexported.ConsensusState, counterpartyClientID string) (string, error)
	// CreateClientFromAny is CreateWasmClient for a client whose states are already packed into
	// Any values: the native Ethereum light client (ibc.lightclients.spectre.v1), whose messages
	// the relayer builds but whose module it does not link.
	CreateClientFromAny(stdCtx context.Context, endpoint CosmosEndpoint, clientState, consensusState *codectypes.Any, counterpartyClientID string) (string, error)
	SendEthTx(stdCtx context.Context, endpoint EVMEndpoint, cosmosRouterClientID string, msg any) error
	SendEthTxBatch(stdCtx context.Context, endpoint EVMEndpoint, cosmosRouterClientID string, msgs []any) error
	SubmitMisbehaviour(stdCtx context.Context, endpoint EVMEndpoint, cosmosRouterClientID string, misbehaviourMsg []byte) error
	SendCosmosTxBatch(stdCtx context.Context, endpoint CosmosEndpoint, msgs []any) error
	CosmosSignerAddress() (string, error)
}

// Prover is the async proof surface implemented by *prover.Service: submit
// returns a deterministic ProofID immediately, GetProof reads the outcome on
// the relayer's own cadence, WaitProof is the blocking convenience for
// one-shot CLI flows. Retry/timeout policy belongs to the caller — see
// submitAndAwaitProof for the relay-side mapping of the error taxonomy.
type Prover interface {
	SubmitProof(sigs []witness.ValidatorSignature) (prover.ProofID, error)
	// MaxBucket is the largest bucket the engine can prove — the ceiling
	// signature selection must respect.
	MaxBucket() int
	GetProof(id prover.ProofID) (prover.ProofJob, error)
	WaitProof(ctx context.Context, id prover.ProofID) (prover.ProofResult, error)
}

// Services holds the shared pieces the chain adapters reuse: the Worker (tx
// handler + prover), the resolved Cosmos-source config, and the BatchBuilder the
// subscriber pushes into and the adapters drain.
type Services struct {
	worker        *Worker
	cosmosConfig  Config
	BatchBuilder  *BatchBuilder
	clientUpdates clientUpdateTimes
	recovery      *RecoveryStateStore

	// ackDue is the retry ledger for owed-acknowledgement records. Zero value is
	// usable, so neither constructor has to build it. See ackdue.go for why it
	// lives here and not in relay.Module.
	ackDue ackDueLedger
}

type evmTimeoutDeps struct {
	cosmos         CosmosEndpoint
	evm            EVMEndpoint
	routerClientID string
}

func New(txHandler TransactionHandler, prover Prover, cosmosConfig Config, recovery ...*RecoveryStateStore) *Services {
	var recoveryStore *RecoveryStateStore
	if len(recovery) > 0 {
		recoveryStore = recovery[0]
	}
	svc := &Services{
		cosmosConfig: cosmosConfig,
		worker:       NewWorker(txHandler, prover),
		BatchBuilder: NewBatchBuilder(),
		recovery:     recoveryStore,
	}
	svc.BatchBuilder.WithOwedAckSettler(svc.ClearAckDue)
	return svc
}

// NewOneShot builds Services for operator commands. Proof jobs use WaitProof;
// long-running relay services use New and poll GetProof on their own cadence.
func NewOneShot(txHandler TransactionHandler, prover Prover, cosmosConfig Config) *Services {
	svc := New(txHandler, prover, cosmosConfig)
	svc.worker.awaitProof = submitAndWaitProof
	return svc
}

// RecoveryState returns the durable recovery store wired for this source. It is
// nil for one-shot commands and source types that do not use gap recovery.
func (s *Services) RecoveryState() *RecoveryStateStore { return s.recovery }

// NewWithPendingState is the production relay constructor. It rehydrates
// pending packets and timeout tombstones before a Services value can be handed
// to source subscriptions or recovery loops.
func NewWithPendingState(txHandler TransactionHandler, prover Prover, cosmosConfig Config, stateDir string, recovery ...*RecoveryStateStore) (*Services, error) {
	batchBuilder, err := NewPersistentBatchBuilder(stateDir)
	if err != nil {
		return nil, fmt.Errorf("restore pending packet trackers: %w", err)
	}
	var recoveryStore *RecoveryStateStore
	if len(recovery) > 0 {
		recoveryStore = recovery[0]
	}
	svc := &Services{
		cosmosConfig: cosmosConfig,
		worker:       NewWorker(txHandler, prover),
		BatchBuilder: batchBuilder,
		recovery:     recoveryStore,
	}
	svc.BatchBuilder.WithOwedAckSettler(svc.ClearAckDue)
	return svc, nil
}

// CosmosClientExpiry returns when the on-chain Cosmos SpectreClient expires: the
// pinned validator-set snapshot's consensus timestamp plus the trusting period.
// Unlike seedCosmosClientFreshness this is side-effect-free (no timestamp
// mutation; one log line ever, for a pre-#541 client), so the relay module's
// refresh routine can poll it periodically.
//
// The snapshot, not the latest height, is the anchor: updateApplicationState
// advances latestHeight without touching the snapshot, so anchoring on the
// latest header reported a client as healthy for hours after the contract had
// already started rejecting every update (#541). The snapshot is pinned at or
// below latestHeight, so its bound is never the looser of the two.
func CosmosClientExpiry(stdCtx context.Context, cosmos CosmosEndpoint, evm EVMEndpoint) (time.Time, time.Duration, error) {
	readCtx, cancelRead := fetchCtx(stdCtx, defaultFetchTimeout)
	clientState, err := fetchOnChainClientState(readCtx, evm)
	cancelRead()
	if err != nil {
		return time.Time{}, 0, err
	}
	period := time.Duration(clientState.TrustingPeriod) * time.Second

	snapCtx, cancelSnap := fetchCtx(stdCtx, defaultFetchTimeout)
	snapshotTime, supported, err := fetchPinnedSnapshotTime(snapCtx, evm)
	cancelSnap()
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("cosmos client expiry: %w", err)
	}
	if supported {
		return tendermintClientExpiry(snapshotTime, clientState.TrustingPeriod), period, nil
	}

	// Pre-#541 client: no snapshot getter, so the latest height's header time
	// is the best anchor available.
	trustedHeight, err := clientStateRevisionHeightInt64(clientState)
	if err != nil {
		return time.Time{}, 0, err
	}
	lightCtx, cancelLight := fetchCtx(stdCtx, defaultFetchTimeout)
	defer cancelLight()
	lightBlock, err := client.GetLightBlock(lightCtx, cosmos.CosmosClient(), trustedHeight)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("cosmos client expiry: trusted light block %d: %w", trustedHeight, err)
	}
	return tendermintClientExpiry(lightBlock.SignedHeader.Header.Time, clientState.TrustingPeriod), period, nil
}

// tendermintClientExpiry derives when the Tendermint light client on the EVM side
// stops accepting updates: the trusting period measured from the trusted anchor's
// time (the pinned snapshot, see CosmosClientExpiry), not from now.
//
// Measuring from now would report a client as healthy forever, since "now" always
// moves with the check. The clock that matters belongs to the block the client
// currently trusts.
//
// This is the mirror of ethClientExpiry in chain/cosmos, and the two are shaped
// differently on purpose: a Tendermint client has one explicit trusting period,
// while a beacon client has none and its expiry has to be inferred from sync
// committee geometry.
func tendermintClientExpiry(trustedHeaderTime time.Time, trustingPeriodSeconds uint32) time.Time {
	return trustedHeaderTime.Add(time.Duration(trustingPeriodSeconds) * time.Second)
}

func deriveCosmosRefreshInterval(cfg Config, trustingPeriod time.Duration) (time.Duration, error) {
	if trustingPeriod <= 0 {
		return 0, fmt.Errorf("on-chain trusting period must be positive: %s", trustingPeriod)
	}
	configured := cfg.RefreshInterval
	if configured == 0 {
		configured = DEFAULT_REFRESH_INTERVAL
	}
	margin := refreshSafetyMargin(trustingPeriod)
	maxInterval := trustingPeriod - margin
	if maxInterval <= 0 {
		return 0, fmt.Errorf("on-chain trusting period %s is too short for refresh safety margin %s", trustingPeriod, margin)
	}
	if cfg.RefreshIntervalConfigured && configured >= maxInterval {
		return 0, fmt.Errorf(
			"configured refresh interval %s must be less than on-chain trusting period %s minus safety margin %s",
			configured, trustingPeriod, margin,
		)
	}
	if configured >= maxInterval {
		log.Printf("[start] Default refresh interval %s exceeds safe interval %s; deriving from on-chain trusting period %s",
			configured, maxInterval, trustingPeriod)
		return maxInterval, nil
	}
	return configured, nil
}

// RefreshSafetyMargin is exported because the relay module needs the same rule
// the refresh-interval calculation already uses: a margin that is a FRACTION of
// the trusting period, not a fixed number of minutes. Two rules for one concept
// is how the module ended up with a hard-coded 30 minutes that has no relation
// to any client's real trusting period.
func RefreshSafetyMargin(trustingPeriod time.Duration) time.Duration {
	return refreshSafetyMargin(trustingPeriod)
}

func refreshSafetyMargin(trustingPeriod time.Duration) time.Duration {
	margin := DEFAULT_REFRESH_SAFETY_MARGIN
	maxMargin := trustingPeriod / 4
	if maxMargin < MIN_REFRESH_SAFETY_MARGIN {
		return maxMargin
	}
	if margin > maxMargin {
		margin = maxMargin
	}
	if margin < MIN_REFRESH_SAFETY_MARGIN {
		margin = MIN_REFRESH_SAFETY_MARGIN
	}
	return margin
}

const pendingTrackerMaxAge = 1 * time.Hour

type evmTimeoutScanOptions struct {
	tag                  string
	tracker              *PendingPacketTracker
	prepareTimeouts      func(context.Context, evmTimeoutDeps) (*client.LightBlock, bool)
	hasPendingCommitment func(context.Context, evmTimeoutDeps, channeltypesv2.Packet) (bool, error)
	timeoutSend          func(context.Context, evmTimeoutDeps, EthPacket, *client.LightBlock) timeoutSendOutcome
}

func NewWorker(txHandler TransactionHandler, prover Prover) *Worker {
	return &Worker{
		TxHandler:  txHandler,
		Prover:     prover,
		awaitProof: submitAndAwaitProof,
	}
}

// NewOneShotWorker is the operator-command variant: it blocks through
// WaitProof rather than inheriting the relay loop's polling cadence.
func NewOneShotWorker(txHandler TransactionHandler, prover Prover) *Worker {
	return &Worker{
		TxHandler:  txHandler,
		Prover:     prover,
		awaitProof: submitAndWaitProof,
	}
}
