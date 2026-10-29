package opstack

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"attestor/optimism"
	attestortypes "attestor/types"
	"attestor/types/redact"
)

// gameSource and replica are the two upstream dependencies of the attestation
// loop, defined at the consumer so tests can fake them.
type gameSource interface {
	GameCount(ctx context.Context) (uint64, error)
	GameAtIndex(ctx context.Context, index uint64) (ProposedRoot, error)
	// FinalizedView produces one immutable, finalized-L1 view for an ingestion
	// pass. The attestor must never advance its durable factory cursor from a
	// latest-L1 view because that view can disappear in a reorg.
	FinalizedView(context.Context) (gameSource, uint64, error)
}

type replica interface {
	SyncStatus(ctx context.Context) (SyncStatus, error)
	OutputAtBlock(ctx context.Context, l2Block uint64) ([32]byte, error)
	CommitmentAt(ctx context.Context, l2Block uint64) (L2Commitment, error)
}

// OpStackAttestor replays the L2 state transition (via a verify-mode replica
// that derives solely from L1) to attest proposed output roots before anything
// relays against them. It implements attestor.Attestor.
//
// Goroutine ownership: the Run goroutine is the only writer of pending state,
// the ingest cursor, the backoff, and every store mutation; the wake-hint
// goroutine owns nothing shared (it only signals the buffered wake channel);
// external readers use the store's RWMutex-guarded accessors.
type OpStackAttestor struct {
	cfg     Config
	games   gameSource
	replica replica
	store   *AttestedRootStore
	hook    ChallengeHook
	metrics *Metrics
	logger  *zap.SugaredLogger
	backoff attestor.Backoff
	now     func() time.Time
	wake    chan struct{}
	// lastStatus is the most recent successful replica sync status, published
	// for read-only observers (the sidecar gRPC server); nil until the first
	// successful poll.
	lastStatus atomic.Pointer[SyncStatus]
	// heartbeat is beaten by the Run goroutine each time its loop comes back
	// around, and read by the sidecar's readiness probe (see RunnerAlive).
	heartbeat attestortypes.Heartbeat
}

// New assembles an attestor from already-validated config and dialed
// dependencies (see cmd/main.go for the production wiring).
func New(cfg Config, games gameSource, rep replica, store *AttestedRootStore, hook ChallengeHook, metrics *Metrics, logger *zap.SugaredLogger) *OpStackAttestor {
	return &OpStackAttestor{
		cfg:     cfg,
		games:   games,
		replica: rep,
		store:   store,
		hook:    hook,
		metrics: metrics,
		logger:  logger,
		now:     time.Now,
		wake:    make(chan struct{}, 1),
	}
}

func (a *OpStackAttestor) Name() string {
	return "opstack:" + a.cfg.SrcChain
}

// ConfirmedUpTo reports the highest confirmed (non-provisional) attested root.
// It is deliberately not named after the AttestedUpTo RPC: that RPC takes
// include_provisional, while this view never includes provisional entries.
func (a *OpStackAttestor) ConfirmedUpTo() (attestor.Cursor, bool) {
	return a.store.AttestedUpTo(false)
}

// Store exposes the attested-root feed for consumers (the future op_to_cosmos
// module relays only against roots this store affirms).
func (a *OpStackAttestor) Store() *AttestedRootStore {
	return a.store
}

// SrcChain returns the configured source chain identifier.
func (a *OpStackAttestor) SrcChain() string {
	return a.cfg.SrcChain
}

// Head returns the configured gating head.
func (a *OpStackAttestor) Head() Head {
	return a.cfg.AttestationHead
}

// LastSyncStatus returns the most recent successful replica sync status; ok is
// false until the replica has responded once.
func (a *OpStackAttestor) LastSyncStatus() (SyncStatus, bool) {
	if s := a.lastStatus.Load(); s != nil {
		return *s, true
	}
	return SyncStatus{}, false
}

// RunnerAlive reports whether Run's loop is making progress: it has come back
// around, or one of its upstream calls has returned, recently enough. A loop
// parked on something that never returns stops beating, so this goes false even
// though nothing has failed and nothing is logged. A loop whose passes keep
// failing still beats: those failures are visible in the log and the
// pass-failure counter, and the loop backs off. So does a pass that runs for a
// long time over a long list, because it beats as each call returns.
func (a *OpStackAttestor) RunnerAlive() bool {
	return a.heartbeat.Alive(a.now(), attestortypes.FeedStaleAfter(a.cfg.CallTimeout, a.cfg.PollInterval))
}

// ErrNoReplica reports that this attestor was built without a replica, so the
// read-only replica queries below cannot be served. The attestation loop needs
// one, so it only happens in tests.
var ErrNoReplica = errors.New("attestor has no replica client")

// HeadAt returns the replica head for one gating level, read fresh rather than
// from the cached snapshot: a caller verifying a specific block needs to know
// whether the replica covers it now, not at the last poll.
//
// Safe for concurrent use — it only calls the replica, and the Run goroutine
// remains the sole writer of everything else.
func (a *OpStackAttestor) HeadAt(ctx context.Context, head Head) (uint64, error) {
	if a.replica == nil {
		return 0, ErrNoReplica
	}
	status, err := a.replica.SyncStatus(ctx)
	if err != nil {
		return 0, err
	}
	switch head {
	case HeadUnsafe:
		return status.UnsafeL2, nil
	case HeadSafe:
		return status.SafeL2, nil
	case HeadFinalized:
		return status.FinalizedL2, nil
	default:
		return 0, fmt.Errorf("unknown attestation head %q", head)
	}
}

// CommitmentAt returns the replica's canonical commitment at one L2 block.
func (a *OpStackAttestor) CommitmentAt(ctx context.Context, l2Block uint64) (L2Commitment, error) {
	if a.replica == nil {
		return L2Commitment{}, ErrNoReplica
	}
	return a.replica.CommitmentAt(ctx, l2Block)
}

// Run executes the attestation loop until ctx is done: poll ticker plus the
// optional DisputeGameCreated wake hint, with failure backoff. Every upstream
// call the loop makes, the bootstrap that precedes the first pass included, runs
// under cfg.CallTimeout, so no call can park the loop. A call that hits its
// deadline fails the pass like any other error: it is logged, counted and backed
// off, and the next pass resumes from the durable state.
func (a *OpStackAttestor) Run(ctx context.Context) error {
	if a.store.Fresh() {
		if err := a.bootstrap(ctx); err != nil {
			return fmt.Errorf("%s: bootstrap failed: %w", a.Name(), redact.Error(err))
		}
	}
	if a.cfg.L1WsUrl != "" {
		go wakeHintLoop(ctx, a.logger, a.cfg.L1WsUrl, a.cfg.DisputeGameFactory, a.wake)
	}
	a.logger.Infof("%s: attestation loop started (head=%s, poll=%s, factory=%s)",
		a.Name(), a.cfg.AttestationHead, a.cfg.PollInterval, a.cfg.DisputeGameFactory)

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	a.heartbeat.Beat(a.now())
	for {
		if a.backoff.Ready(a.now()) {
			if err := a.runOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				delay := a.backoff.RecordFailure(a.now())
				a.count(func(m *Metrics) { m.PassFailures.WithLabelValues(a.cfg.SrcChain).Inc() })
				a.logger.Warnf("%s: attestation pass failed (nothing affirmed, retrying in %s): %v", a.Name(), delay, redact.Error(err))
			} else {
				a.backoff.RecordSuccess()
			}
		}
		a.heartbeat.Beat(a.now())
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-a.wake:
		}
	}
}

// bootstrap sets the initial ingest cursor on a fresh store: the first game
// created inside the configured L1 lookback window.
func (a *OpStackAttestor) bootstrap(ctx context.Context) error {
	games, _, err := a.finalizedGames(ctx)
	if err != nil {
		return err
	}
	count, err := games.GameCount(ctx)
	if err != nil {
		return err
	}
	cutoff := uint64(0)
	// L1 slots are 12s; the lookback is a bound on history replay, not a
	// precise window, so wall-clock arithmetic is sufficient.
	if lookbackSecs := a.cfg.BootstrapLookbackBlocks * 12; lookbackSecs > 0 {
		nowSecs := uint64(a.now().Unix())
		if nowSecs > lookbackSecs {
			cutoff = nowSecs - lookbackSecs
		}
	}
	start, err := bootstrapStartIndex(ctx, games, count, cutoff)
	if err != nil {
		return err
	}
	if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
		return store.Bootstrap(start), nil
	}); err != nil {
		return err
	}
	a.logger.Infof("%s: bootstrapped ingest cursor at game index %d of %d (lookback %d L1 blocks)",
		a.Name(), start, count, a.cfg.BootstrapLookbackBlocks)
	return nil
}

// gateHead returns the replica head that gates verdicts and derived
// attestations for the configured attestation level.
func (a *OpStackAttestor) gateHead(status SyncStatus) uint64 {
	switch a.cfg.AttestationHead {
	case HeadUnsafe:
		return status.UnsafeL2
	case HeadSafe:
		return status.SafeL2
	default:
		return status.FinalizedL2
	}
}

// runOnce is one attestation pass: ingest new games, read the replica heads,
// resolve provisional entries the finalized head now covers, decide pending
// games, and self-attest the current head. Any error returns with nothing
// affirmed — no cursor advances past a failure, no game leaves pending
// without an explicit verdict, and the feed is only ever appended on a
// verdict or a derived attestation.
func (a *OpStackAttestor) runOnce(ctx context.Context) error {
	if err := a.ingest(ctx); err != nil {
		return err
	}

	status, err := a.loopReplica().SyncStatus(ctx)
	if err != nil {
		a.countReplicaError("rpc")
		return err
	}
	a.lastStatus.Store(&status)
	a.gauge(func(m *Metrics) {
		m.ReplicaFinalizedHead.WithLabelValues(a.cfg.SrcChain).Set(float64(status.FinalizedL2))
		m.ReplicaSafeHead.WithLabelValues(a.cfg.SrcChain).Set(float64(status.SafeL2))
		m.ReplicaUnsafeHead.WithLabelValues(a.cfg.SrcChain).Set(float64(status.UnsafeL2))
	})

	if err := a.resolveRechecks(ctx, status.FinalizedL2); err != nil {
		return err
	}
	if err := a.reverifyDerived(ctx, status.FinalizedL2); err != nil {
		return err
	}
	if err := a.decidePending(ctx, status); err != nil {
		return err
	}
	if err := a.attestDerived(ctx, status); err != nil {
		return err
	}
	a.updateLagMetrics(a.gateHead(status))
	return nil
}

// ingest advances the game-index cursor through the factory list. The cursor
// only ever moves past an index whose fetch succeeded.
func (a *OpStackAttestor) ingest(ctx context.Context) error {
	games, finalizedL1Block, err := a.finalizedGames(ctx)
	if err != nil {
		return err
	}
	count, err := games.GameCount(ctx)
	if err != nil {
		return err
	}
	start := a.store.NextGameIndex()
	if start >= count {
		if finalizedL1Block > a.store.LastFinalizedL1Block() {
			return a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				return store.RecordFinalizedL1Block(finalizedL1Block), nil
			})
		}
		return nil
	}
	ingested := make([]ProposedRoot, 0)
	persist := func() error {
		return a.store.Commit(func(store *AttestedRootStore) (bool, error) {
			store.RecordFinalizedL1Block(finalizedL1Block)
			for offset := range ingested {
				game := &ingested[offset]
				if game.GameType == a.cfg.RespectedGameType {
					store.AdvanceIngest(start+uint64(offset)+1, game)
					continue
				}
				store.AdvanceIngest(start+uint64(offset)+1, nil)
			}
			return true, nil
		})
	}
	report := func() {
		for idx, game := range ingested {
			a.count(func(m *Metrics) { m.GamesIngested.WithLabelValues(a.cfg.SrcChain).Inc() })
			if game.GameType != a.cfg.RespectedGameType {
				a.logger.Debugf("%s: skipping game %d: type %d != respected type %d", a.Name(), start+uint64(idx), game.GameType, a.cfg.RespectedGameType)
				continue
			}
			a.logger.Infof("%s: ingested proposal: game %d, l2 block %d, root %x", a.Name(), start+uint64(idx), game.L2BlockNumber, game.RootClaim)
		}
	}
	for idx := start; idx < count; idx++ {
		game, err := games.GameAtIndex(ctx, idx)
		if err != nil {
			if len(ingested) != 0 {
				if persistErr := persist(); persistErr != nil {
					return fmt.Errorf("persisting ingest through game index %d: %w", idx-1, persistErr)
				}
				report()
			}
			return fmt.Errorf("ingest stopped at game index %d: %w", idx, err)
		}
		ingested = append(ingested, game)
	}
	if err := persist(); err != nil {
		return fmt.Errorf("persisting ingest through game index %d: %w", count-1, err)
	}
	report()
	return nil
}

func (a *OpStackAttestor) finalizedGames(ctx context.Context) (gameSource, uint64, error) {
	if a.games == nil {
		return nil, 0, errors.New("DisputeGameFactory source is not configured")
	}
	view, finalizedL1Block, err := a.loopGames().FinalizedView(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("read finalized DisputeGameFactory view: %w", err)
	}
	if view == nil {
		return nil, 0, errors.New("finalized DisputeGameFactory view is nil")
	}
	return view, finalizedL1Block, nil
}

// decidePending issues verdicts for pending games covered by the gating head.
// A game leaves pending only via an explicit verdict; replica errors keep it
// pending forever (there is no skip-after-N-retries for verification).
func (a *OpStackAttestor) decidePending(ctx context.Context, status SyncStatus) error {
	gate := a.gateHead(status)
	for _, game := range a.store.Pending() {
		if game.L2BlockNumber > gate {
			continue
		}
		local, err := a.loopReplica().OutputAtBlock(ctx, game.L2BlockNumber)
		if err != nil {
			a.countReplicaError("rpc")
			return fmt.Errorf("deciding game %d (l2 block %d): %w", game.GameIndex, game.L2BlockNumber, err)
		}
		// Provisional iff the verdict covers a block the finalized head has
		// not yet re-derived from finalized L1 data.
		provisional := game.L2BlockNumber > status.FinalizedL2
		if err := a.recordVerdict(ctx, game, local, provisional); err != nil {
			return fmt.Errorf("persisting verdict for game %d: %w", game.GameIndex, err)
		}
	}
	return nil
}

// attestDerived self-attests the replica's output root at the gating head —
// the proposal-independent feed: a consumer waits only for the configured
// trust level plus this verification, never for a proposer.
func (a *OpStackAttestor) attestDerived(ctx context.Context, status SyncStatus) error {
	if a.cfg.DisableDerivedRoots {
		return nil
	}
	gate := a.gateHead(status)
	if gate == 0 {
		return nil
	}
	if last, ok := a.store.HighestDerivedBlock(); ok && gate < last+a.cfg.DerivedGapBlocks {
		return nil
	}
	root, err := a.loopReplica().OutputAtBlock(ctx, gate)
	if err != nil {
		a.countReplicaError("rpc")
		return fmt.Errorf("deriving output root at head block %d: %w", gate, err)
	}
	provisional := gate > status.FinalizedL2
	pruned := 0
	if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
		store.AppendDerived(gate, root, a.now(), provisional)
		pruned = store.PruneDerived(int(a.cfg.MaxDerivedRoots))
		return true, nil
	}); err != nil {
		return fmt.Errorf("persisting derived root at head block %d: %w", gate, err)
	}
	a.count(func(m *Metrics) { m.RootsDerived.WithLabelValues(a.cfg.SrcChain).Inc() })
	if provisional {
		a.count(func(m *Metrics) { m.RootsProvisional.WithLabelValues(a.cfg.SrcChain).Inc() })
	}
	a.logger.Infof("%s: derived root attested at l2 block %d (root %x, head=%s, provisional=%t)",
		a.Name(), gate, root, a.cfg.AttestationHead, provisional)
	if pruned > 0 {
		a.logger.Debugf("%s: pruned %d confirmed derived roots beyond cap %d", a.Name(), pruned, a.cfg.MaxDerivedRoots)
	}
	return nil
}

// reverifyDerived re-computes provisional derived roots once the finalized
// head covers them. The finalized result is authoritative: agreement confirms
// the entry, disagreement corrects it and raises the divergence alarm.
func (a *OpStackAttestor) reverifyDerived(ctx context.Context, finalized uint64) error {
	for _, entry := range a.store.DerivedProvisionalAtOrBelow(finalized) {
		root, err := a.loopReplica().OutputAtBlock(ctx, entry.L2BlockNumber)
		if err != nil {
			a.countReplicaError("rpc")
			return fmt.Errorf("reverifying derived root at l2 block %d: %w", entry.L2BlockNumber, err)
		}
		if root == entry.Root {
			var confirmed bool
			if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				confirmed = store.ConfirmDerived(entry.L2BlockNumber)
				return confirmed, nil
			}); err != nil {
				return fmt.Errorf("persisting derived reverification at l2 block %d: %w", entry.L2BlockNumber, err)
			}
			if !confirmed {
				return fmt.Errorf("provisional derived root at l2 block %d disappeared before confirmation", entry.L2BlockNumber)
			}
			a.logger.Infof("%s: finalized head confirmed derived root at l2 block %d", a.Name(), entry.L2BlockNumber)
		} else {
			var corrected bool
			if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				corrected = store.CorrectDerived(entry.L2BlockNumber, root, a.now())
				return corrected, nil
			}); err != nil {
				return fmt.Errorf("persisting derived reverification at l2 block %d: %w", entry.L2BlockNumber, err)
			}
			if !corrected {
				return fmt.Errorf("provisional derived root at l2 block %d disappeared before correction", entry.L2BlockNumber)
			}
			a.count(func(m *Metrics) { m.HeadDivergence.WithLabelValues(a.cfg.SrcChain).Inc() })
			a.logger.Errorw("[opstack attestor] HEAD DIVERGENCE — finalized derivation contradicts provisional derived root",
				"chain", a.cfg.SrcChain,
				"l2_block", entry.L2BlockNumber,
				"provisional_root", fmt.Sprintf("%x", entry.Root),
				"finalized_root", fmt.Sprintf("%x", root),
			)
		}
	}
	return nil
}

func (a *OpStackAttestor) recordVerdict(ctx context.Context, game ProposedRoot, local [32]byte, provisional bool) error {
	now := a.now()
	match := local == game.RootClaim
	if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
		if match {
			store.RecordMatch(game, now, provisional)
			return true, nil
		}
		store.RecordMismatch(game, local, now, provisional)
		return true, nil
	}); err != nil {
		return err
	}
	if match {
		a.count(func(m *Metrics) { m.RootsMatched.WithLabelValues(a.cfg.SrcChain).Inc() })
		a.logger.Infof("%s: attested game %d (l2 block %d, root %x, provisional=%t)",
			a.Name(), game.GameIndex, game.L2BlockNumber, game.RootClaim, provisional)
	} else {
		a.count(func(m *Metrics) { m.RootsMismatched.WithLabelValues(a.cfg.SrcChain).Inc() })
		if err := a.challengeHook(ctx, game, local); err != nil {
			a.logger.Errorf("%s: challenge hook failed for game %d: %v", a.Name(), game.GameIndex, err)
		}
	}
	if provisional {
		a.count(func(m *Metrics) { m.RootsProvisional.WithLabelValues(a.cfg.SrcChain).Inc() })
	}
	return nil
}

// resolveRechecks re-runs provisional game verdicts once the finalized head
// covers their block. The finalized result is always authoritative; a
// contradiction is a head-divergence alarm (sequencer divergence for verdicts
// made at unsafe, an L1 reorg for verdicts made at safe).
func (a *OpStackAttestor) resolveRechecks(ctx context.Context, finalized uint64) error {
	for _, entry := range a.store.RecheckList() {
		if entry.Game.L2BlockNumber > finalized {
			continue
		}
		local, err := a.loopReplica().OutputAtBlock(ctx, entry.Game.L2BlockNumber)
		if err != nil {
			a.countReplicaError("rpc")
			return fmt.Errorf("rechecking game %d (l2 block %d): %w", entry.Game.GameIndex, entry.Game.L2BlockNumber, err)
		}
		finalMatch := local == entry.Game.RootClaim
		if finalMatch == entry.ProvisionalMatch {
			if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				store.ConfirmProvisional(entry.Game.GameIndex)
				return true, nil
			}); err != nil {
				return fmt.Errorf("persisting recheck for game %d: %w", entry.Game.GameIndex, err)
			}
			a.logger.Infof("%s: finalized head confirmed provisional verdict for game %d (match=%t)",
				a.Name(), entry.Game.GameIndex, finalMatch)
		} else {
			if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				store.CorrectProvisional(entry, local, a.now())
				return true, nil
			}); err != nil {
				return fmt.Errorf("persisting recheck for game %d: %w", entry.Game.GameIndex, err)
			}
			a.count(func(m *Metrics) { m.HeadDivergence.WithLabelValues(a.cfg.SrcChain).Inc() })
			a.logger.Errorw("[opstack attestor] HEAD DIVERGENCE — finalized recheck contradicts provisional verdict",
				"chain", a.cfg.SrcChain,
				"game_index", entry.Game.GameIndex,
				"l2_block", entry.Game.L2BlockNumber,
				"provisional_match", entry.ProvisionalMatch,
				"finalized_match", finalMatch,
			)
			if !finalMatch {
				if err := a.challengeHook(ctx, entry.Game, local); err != nil {
					a.logger.Errorf("%s: challenge hook failed for rechecked game %d: %v", a.Name(), entry.Game.GameIndex, err)
				}
			}
		}
	}
	return nil
}

func (a *OpStackAttestor) updateLagMetrics(gate uint64) {
	pending := a.store.Pending()
	var lag uint64
	for _, g := range pending {
		if g.L2BlockNumber > gate {
			if d := g.L2BlockNumber - gate; d > lag {
				lag = d
			}
		}
	}
	a.gauge(func(m *Metrics) {
		m.AttestationLag.WithLabelValues(a.cfg.SrcChain).Set(float64(lag))
		m.PendingGames.WithLabelValues(a.cfg.SrcChain).Set(float64(len(pending)))
	})
}

func (a *OpStackAttestor) countReplicaError(kind string) {
	a.count(func(m *Metrics) { m.ReplicaErrors.WithLabelValues(a.cfg.SrcChain, kind).Inc() })
}

func (a *OpStackAttestor) count(f func(*Metrics)) {
	if a.metrics != nil {
		f(a.metrics)
	}
}

func (a *OpStackAttestor) gauge(f func(*Metrics)) {
	if a.metrics != nil {
		f(a.metrics)
	}
}
