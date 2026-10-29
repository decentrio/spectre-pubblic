package services

import (
	"fmt"
	"time"

	relayerclient "relayer/client"
)

const DEFAULT_INTERVAL = 5
const DEFAULT_TIME_PERIODS = time.Second * 20 //default 20 seconds

// DEFAULT_CLOCK_DRIFT mirrors relayerclient.DefaultClockDrift (the canonical
// source) so config defaults and genesis generation stay in lock-step.
const DEFAULT_CLOCK_DRIFT uint32 = relayerclient.DefaultClockDrift

// DEFAULT_BEACON_FINALITY_RETRIES caps how many 10s polls the ETH→Cosmos relay
// waits for beacon finality to cover an event block before re-queueing. It must
// exceed Ethereum worst-case finality (~2 epochs ≈ 12.8 min); 90 × 10s = 15 min
// leaves headroom so valid packets are not spuriously re-queued.
const DEFAULT_BEACON_FINALITY_RETRIES uint32 = 90

const DEFAULT_COSMOS_APP_HASH_WAIT_RETRIES uint32 = 30
const DEFAULT_COSMOS_APP_HASH_WAIT_INTERVAL = time.Second

// DEFAULT_ROTATION_THRESHOLD is the pinned-set overlap fraction at or below
// which an update rotates the pinned validator set (updateConsensusState)
// instead of taking the cheap updateApplicationState path. The on-chain
// quorum floor is >2/3 of pinned power and the rotation tx itself must clear
// that same quorum, so the 1/6 buffer above the floor absorbs single-commit
// signature jitter, one large validator exiting between checks, and
// rotation-tx retry latency. Set "1/1" to rotate on any pinned-set change.
const DEFAULT_ROTATION_THRESHOLD = "5/6"

// DEFAULT_REFRESH_INTERVAL is how long the background freshness routines wait
// after the last successful update before advancing the light clients on
// their own (and, for the Cosmos client, force-rotating a stale pinned set).
const DEFAULT_REFRESH_INTERVAL = 24 * time.Hour

const DEFAULT_REFRESH_SAFETY_MARGIN = time.Hour
const MIN_REFRESH_SAFETY_MARGIN = time.Minute

type IntervalType uint8

const (
	blockHeight IntervalType = iota
	timestamp
)

type IntervalConfig struct {
	blockHeight uint8
	blockTime   time.Duration
}

type BatchConfig struct {
	BatchSize    uint8
	BatchPeriods time.Duration
}

type Config struct {
	KeyPath               string
	IntervalParams        IntervalConfig
	IntervalType          IntervalType
	BatchConfig           BatchConfig
	TrustingPeriod        uint32
	TrustLevel            string
	ProofType             string
	ClockDrift            uint32
	BeaconFinalityRetries uint32
	AppHashWaitRetries    uint32
	AppHashWaitInterval   time.Duration
	FetchTimeout          time.Duration
	// RotationThreshold is a fraction string like "5/6": rotate the pinned
	// validator set once its voting-power overlap with the target block's
	// signers is at or below this fraction. Must exceed 2/3 (the quorum
	// floor); "1/1" rotates on any change.
	RotationThreshold string
	// RefreshInterval gates the background freshness routines (see
	// DEFAULT_REFRESH_INTERVAL).
	RefreshInterval           time.Duration
	RefreshIntervalConfigured bool
	// RouterDeployBlock is the block the ICS26Router was deployed at; 0 when not
	// configured. The ETH source's enumeration backstop starts its log lookups
	// there instead of walking bounded windows back from the head.
	RouterDeployBlock uint64
}

func NewConfig(KeyPath string, params IntervalConfig, intervalType IntervalType) Config {
	return Config{
		IntervalParams: params,
		IntervalType:   intervalType,
		FetchTimeout:   time.Second * 15,
	}
}

func DefaultConfig() Config {
	return Config{
		IntervalParams: IntervalConfig{
			blockHeight: DEFAULT_INTERVAL,
			blockTime:   DEFAULT_TIME_PERIODS,
		},
		IntervalType: timestamp,
		BatchConfig: BatchConfig{
			BatchPeriods: time.Second * 3, // default each batch waits for 3 seconds
			BatchSize:    5,               // default 5 packets per batch (multicall gas budget: ~5×2M wasm verify ≈ 10M, fits prod block limit)
		},
		TrustLevel:            "2/3",
		ProofType:             "groth16",
		ClockDrift:            DEFAULT_CLOCK_DRIFT,
		BeaconFinalityRetries: DEFAULT_BEACON_FINALITY_RETRIES,
		AppHashWaitRetries:    DEFAULT_COSMOS_APP_HASH_WAIT_RETRIES,
		AppHashWaitInterval:   DEFAULT_COSMOS_APP_HASH_WAIT_INTERVAL,
		FetchTimeout:          time.Second * 15,
		RotationThreshold:     DEFAULT_ROTATION_THRESHOLD,
		RefreshInterval:       DEFAULT_REFRESH_INTERVAL,
	}
}

// ParseRotationThreshold parses and validates a rotation-threshold fraction.
// An empty value falls back to DEFAULT_ROTATION_THRESHOLD so configs not built
// via DefaultConfig stay safe. The threshold must exceed 2/3 — both update
// kinds need >2/3 of pinned voting power among the target block's signers, so
// a trigger at or below that floor would fire only after the client is
// already unable to prove the rotation — and must not exceed 1.
func ParseRotationThreshold(value string) (relayerclient.TrustThreshold, error) {
	if value == "" {
		value = DEFAULT_ROTATION_THRESHOLD
	}
	threshold, err := relayerclient.ParseTrustThreshold(value)
	if err != nil {
		return relayerclient.TrustThreshold{}, err
	}
	if 3*uint32(threshold.Numerator) <= 2*uint32(threshold.Denominator) {
		return relayerclient.TrustThreshold{}, fmt.Errorf(
			"rotation threshold %s must exceed 2/3 (the pinned-set quorum floor)", value)
	}
	if threshold.Numerator > threshold.Denominator {
		return relayerclient.TrustThreshold{}, fmt.Errorf(
			"rotation threshold %s must not exceed 1", value)
	}
	return threshold, nil
}
