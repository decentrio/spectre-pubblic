package arbitrum

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	attestortypes "attestor/types"
	"attestor/types/redact"

	"github.com/ethereum/go-ethereum/common"
)

// DaemonConfig contains the gRPC listener, L1 RollupCore source, and
// external Nitro RPC endpoints.
type DaemonConfig struct {
	GRPCListenAddress          string       `json:"grpc_listen_address"`
	MetricsListenAddress       string       `json:"metrics_listen_address"`
	RuntimePollInterval        string       `json:"runtime_poll_interval"`
	RuntimeBackfillMaxBlocks   uint64       `json:"runtime_backfill_max_blocks"`
	RuntimeBackfillConcurrency uint64       `json:"runtime_backfill_concurrency"`
	L1IngestHead               L1IngestHead `json:"l1_ingest_head"`
	AttestationHead            RunMode      `json:"attestation_head"`
	DisableDerivedRoots        bool         `json:"disable_derived_roots"`
	DerivedGapBlocks           uint64       `json:"derived_attestation_gap_blocks"`
	MaxDerivedRoots            uint64       `json:"max_derived_roots"`
	SrcChain                   string       `json:"src_chain"`
	L1RPCURL                   string       `json:"l1_rpc_url"`
	L1ChainID                  uint64       `json:"l1_chain_id"`
	L2ChainID                  uint64       `json:"l2_chain_id"`
	AttestationSigningKey      string       `json:"attestation_signing_key"`
	RollupCoreAddress          string       `json:"rollup_core_address"`
	AssertionsMappingSlot      string       `json:"assertions_mapping_slot"`
	AssertionStatusOffset      uint8        `json:"assertion_status_offset"`
	AssertionStartBlock        uint64       `json:"assertion_start_block"`
	AssertionPollInterval      string       `json:"assertion_poll_interval"`
	AssertionCallTimeout       string       `json:"assertion_call_timeout"`
	AssertionMaxBlockRange     uint64       `json:"assertion_max_block_range"`
	AttestorStatePath          string       `json:"attestor_state_path"`
	NitroRPCURL                string       `json:"nitro_rpc_url"`
	NitroWSURL                 string       `json:"nitro_ws_url"`
}

// LoadDaemonConfig reads, validates, and resolves filesystem paths relative to
// the directory containing the configuration file.
func LoadDaemonConfig(path string) (DaemonConfig, error) {
	if path == "" {
		return DaemonConfig{}, errors.New("attestor config path must not be empty")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return DaemonConfig{}, fmt.Errorf("resolve attestor config path %q: %w", path, err)
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return DaemonConfig{}, fmt.Errorf("read attestor config %q: %w", absolutePath, err)
	}
	var config DaemonConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return DaemonConfig{}, fmt.Errorf("decode attestor config %q: %w", absolutePath, err)
	}
	if err := rejectManagedNitroFields(data); err != nil {
		return DaemonConfig{}, fmt.Errorf("decode attestor config %q: %w", absolutePath, err)
	}

	base := filepath.Dir(absolutePath)
	config.AttestorStatePath = resolveConfigPath(base, config.AttestorStatePath)
	if err := config.Validate(); err != nil {
		return DaemonConfig{}, fmt.Errorf("validate attestor config %q: %w", absolutePath, err)
	}
	return config, nil
}

func rejectManagedNitroFields(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range []string{
		"nitro_binary_path",
		"nitro_binary_sha256",
		"nitro_arguments",
		"nitro_work_dir",
		"nitro_data_dir",
		"nitro_ipc_path",
		"nitro_startup_timeout",
		"nitro_shutdown_timeout",
	} {
		if _, found := fields[field]; found {
			return fmt.Errorf(
				"%s is no longer supported; run Nitro separately and configure nitro_rpc_url and nitro_ws_url",
				field,
			)
		}
	}
	return nil
}

// Validate checks the gRPC listener and external RPC settings.
func (c DaemonConfig) Validate() error {
	if err := validateListenAddress(c.GRPCListenAddress); err != nil {
		return err
	}
	if err := validateMetricsListenAddress(c.MetricsListenAddress); err != nil {
		return err
	}
	if _, err := c.RuntimePollDuration(); err != nil {
		return err
	}
	if _, err := c.L1IngestHead.Normalize(); err != nil {
		return err
	}
	if _, err := c.NormalizedAttestationHead(); err != nil {
		return err
	}
	if c.SrcChain == "" {
		return errors.New("src_chain is required")
	}
	if err := validateRPCURL(c.L1RPCURL, "l1_rpc_url", "http", "https", "ws", "wss"); err != nil {
		return err
	}
	if c.L1ChainID == 0 {
		return errors.New("l1_chain_id must be greater than zero")
	}
	if c.L2ChainID == 0 {
		return errors.New("l2_chain_id must be greater than zero")
	}
	if strings.TrimSpace(c.AttestationSigningKey) == "" {
		return errors.New("attestation_signing_key is required")
	}
	if !common.IsHexAddress(c.RollupCoreAddress) ||
		common.HexToAddress(c.RollupCoreAddress) == (common.Address{}) {
		return errors.New("rollup_core_address must be a non-zero EVM address")
	}
	if _, err := decodeHash32(c.AssertionsMappingSlot, "assertions_mapping_slot"); err != nil {
		return err
	}
	if c.AssertionStatusOffset >= 32 {
		return errors.New("assertion_status_offset must be between 0 and 31")
	}
	if c.AssertionStartBlock == 0 {
		return errors.New("assertion_start_block must be a non-zero RollupCore log scan start block")
	}
	if _, err := c.AssertionPollDuration(); err != nil {
		return err
	}
	if _, err := c.AssertionCallTimeoutDuration(); err != nil {
		return err
	}
	if c.AttestorStatePath == "" || !filepath.IsAbs(c.AttestorStatePath) {
		return errors.New("attestor_state_path must resolve to an absolute path")
	}
	if err := validateRPCURL(c.NitroRPCURL, "nitro_rpc_url", "http", "https"); err != nil {
		return err
	}
	if err := validateRPCURL(c.NitroWSURL, "nitro_ws_url", "ws", "wss"); err != nil {
		return err
	}
	return nil
}

// NormalizedAttestationHead returns the Nitro head used for self-derived
// attestations. The conservative default mirrors the OP attestor.
func (c DaemonConfig) NormalizedAttestationHead() (RunMode, error) {
	if c.AttestationHead == "" {
		return RunModeFinalized, nil
	}
	mode, err := c.AttestationHead.Normalize()
	if err != nil {
		return "", fmt.Errorf("attestation_head: %w", err)
	}
	return mode, nil
}

// DerivedAttestationGap returns the minimum L2 block distance between
// self-derived feed entries.
func (c DaemonConfig) DerivedAttestationGap() uint64 {
	const defaultDerivedGapBlocks = uint64(150)
	if c.DerivedGapBlocks == 0 {
		return defaultDerivedGapBlocks
	}
	return c.DerivedGapBlocks
}

// DerivedRootLimit returns the maximum number of confirmed derived entries
// retained in the feed. Assertion-backed entries and provisional derived
// entries are never pruned.
func (c DaemonConfig) DerivedRootLimit() uint64 {
	const defaultMaxDerivedRoots = uint64(1_000)
	if c.MaxDerivedRoots == 0 {
		return defaultMaxDerivedRoots
	}
	return c.MaxDerivedRoots
}

// RuntimePollDuration returns the fallback interval for reconciling Nitro's
// unsafe, safe, and finalized heads when no new-head event is received. An
// omitted interval defaults to 15 seconds.
func (c DaemonConfig) RuntimePollDuration() (time.Duration, error) {
	const defaultRuntimePollInterval = 15 * time.Second
	if c.RuntimePollInterval == "" {
		return defaultRuntimePollInterval, nil
	}
	return parsePositiveDuration(c.RuntimePollInterval, "runtime_poll_interval")
}

// BackfillMaxBlocks returns the maximum L2 heights fetched per unsafe, safe,
// or finalized range in one runtime refresh. When the tracker is behind by
// more, the oldest heights are skipped and reported as unobserved. An omitted
// value defaults to 2048, roughly one L1 finality epoch of Arbitrum blocks.
func (c DaemonConfig) BackfillMaxBlocks() uint64 {
	if c.RuntimeBackfillMaxBlocks == 0 {
		return defaultBackfillMaxBlocks
	}
	return c.RuntimeBackfillMaxBlocks
}

// BackfillConcurrency returns the bound on concurrent Nitro header fetches
// during a runtime backfill. An omitted value defaults to 8 — enough to
// outpace Arbitrum block production at typical public-RPC latency while
// staying under free-tier concurrent-request limits. Raise it on a dedicated
// endpoint; lower it if the endpoint returns 429s.
func (c DaemonConfig) BackfillConcurrency() uint64 {
	if c.RuntimeBackfillConcurrency == 0 {
		return uint64(defaultBackfillConcurrency)
	}
	return c.RuntimeBackfillConcurrency
}

// AssertionPollDuration returns the finalized-L1 assertion polling interval.
// An omitted interval defaults to 12 seconds.
func (c DaemonConfig) AssertionPollDuration() (time.Duration, error) {
	const defaultAssertionPollInterval = 12 * time.Second
	if c.AssertionPollInterval == "" {
		return defaultAssertionPollInterval, nil
	}
	return parsePositiveDuration(c.AssertionPollInterval, "assertion_poll_interval")
}

// AssertionCallTimeoutDuration returns the deadline for one upstream call the
// assertion loop makes to L1 or Nitro. An omitted value defaults to
// attestortypes.DefaultCallTimeout.
func (c DaemonConfig) AssertionCallTimeoutDuration() (time.Duration, error) {
	if c.AssertionCallTimeout == "" {
		return attestortypes.DefaultCallTimeout, nil
	}
	return parsePositiveDuration(c.AssertionCallTimeout, "assertion_call_timeout")
}

// AssertionBlockRange returns the maximum number of finalized L1 blocks read
// in one log query.
func (c DaemonConfig) AssertionBlockRange() uint64 {
	const defaultAssertionMaxBlockRange = uint64(2_000)
	if c.AssertionMaxBlockRange == 0 {
		return defaultAssertionMaxBlockRange
	}
	return c.AssertionMaxBlockRange
}

func validateListenAddress(address string) error {
	if address == "" {
		return errors.New("grpc_listen_address is required")
	}
	_, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse grpc_listen_address: %w", err)
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return errors.New("grpc_listen_address must contain a port between 1 and 65535")
	}
	return nil
}

func validateMetricsListenAddress(address string) error {
	if address == "" {
		return errors.New("metrics_listen_address is required")
	}
	_, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse metrics_listen_address: %w", err)
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return errors.New("metrics_listen_address must contain a port between 1 and 65535")
	}
	return nil
}

func validateRPCURL(raw, field string, allowedSchemes ...string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", field)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", field, redact.Error(err))
	}
	allowed := false
	for _, scheme := range allowedSchemes {
		if parsed.Scheme == scheme {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%s must use %s", field, strings.Join(allowedSchemes, " or "))
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s must include a host", field)
	}
	return nil
}

func decodeHash32(raw, field string) (common.Hash, error) {
	var value common.Hash
	encoded := strings.TrimPrefix(raw, "0x")
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return value, fmt.Errorf("decode %s: %w", field, err)
	}
	if len(decoded) != common.HashLength {
		return value, fmt.Errorf(
			"%s must contain exactly %d bytes, got %d",
			field,
			common.HashLength,
			len(decoded),
		)
	}
	copy(value[:], decoded)
	return value, nil
}

func parsePositiveDuration(raw, field string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is required", field)
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", field, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", field)
	}
	return duration, nil
}

func resolveConfigPath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}
