// The relayer's config, in three files that read and write one structure:
// config.go holds the schema, loading, and the one-path-per-process census;
// config_validate.go holds everything that can prove a loaded config wrong
// before a relay loop opens (parse-time field checks, the create-clients
// preflight, the startup probes); config_edit.go holds the in-place JSON editor
// create-clients writes ids back with. Adding a key touches the schema here
// and, if it needs judging, config_validate.go.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"relayer/chain"
	"relayer/services"
	"strings"
	"time"
)

const (
	flagConfigPath     = "config"
	flagOnlyOnce       = "only-once"
	flagProofType      = "proof-type"
	flagOutput         = "output"
	flagOutputPath     = "output-path"
	flagTrustLevel     = "trust-level"
	flagTrustingPeriod = "trusting-period"
	flagClockDrift     = "clock-drift"
	flagTrustedBlock   = "trusted-block"
	flagForceRotation  = "force-rotation"
	flagTargetHeight   = "target-height"
	flagMaxHops        = "max-hops"
	flagBenchmark      = "benchmark"
	configFilePerm     = 0o600
)

// --- Config types for JSON config file ---

type cosmosToEthConfig struct {
	TmRpcUrl              string `json:"tm_rpc_url"`
	ICS26Address          string `json:"ics26_address"`
	ICS26ClientID         string `json:"ics26_client_id"`
	CosmosWasmClientID    string `json:"cosmos_wasm_client_id"`
	EthRpcUrl             string `json:"eth_rpc_url"`
	EthWsUrl              string `json:"eth_ws_url"`
	SpectreClient         string `json:"spectre_client"`
	SignatureVerifier     string `json:"signature_verifier"`
	Membership            string `json:"membership"`
	Misbehaviour          string `json:"misbehaviour"`
	UpdateClient          string `json:"update_client"`
	TrustingPeriod        uint32 `json:"trusting_period"`
	TrustLevel            string `json:"trust_level"`
	ProofType             string `json:"proof_type"`
	ClockDrift            uint32 `json:"clock_drift"`
	BeaconFinalityRetries uint32 `json:"beacon_finality_retries"`
	AppHashWaitRetries    uint32 `json:"app_hash_wait_retries"`
	AppHashWaitInterval   uint64 `json:"app_hash_wait_interval_seconds"`
	FetchTimeout          uint64 `json:"fetch_timeout"`
	RotationThreshold     string `json:"rotation_threshold"`
	RefreshInterval       uint64 `json:"refresh_interval_seconds"`
	// RouterDeployBlock is the block ics26_address was deployed at. Optional:
	// when set, the packet-flush backstop resolves an outstanding packet's event
	// with one eth_getLogs over [router_deploy_block, head]; when unset it walks
	// bounded windows back from the head.
	RouterDeployBlock uint64 `json:"router_deploy_block,omitempty"`
}

type ethToCosmosConfig struct {
	// BeaconUrl is the only field consumed for the ETH→Cosmos direction; the
	// Cosmos RPC, Eth RPC and ICS26 address all come from the cosmos_to_eth
	// module (used for both directions).
	BeaconUrl string `json:"eth_beacon_api_url"`
}

type configModule struct {
	// Name is a free-form human label (logs / diagnostics). For legacy configs it
	// doubles as the direction selector when dst_chain is absent (back-compat).
	Name string `json:"name"`
	// SrcChain / DstChain are chain kinds (chain.ChainType: "cosmos", "ethereum",
	// "opstack", "arbitrum") in the canonical schema; the (src, dst) pair selects
	// the relay direction. In a legacy config (no dst_chain) src_chain instead
	// carries the ics26_client_id fallback.
	SrcChain string          `json:"src_chain"`
	DstChain string          `json:"dst_chain"`
	Builder  string          `json:"builder"`
	Config   json.RawMessage `json:"config"`
}

// moduleDirection is the relay direction a config module resolves to.
type moduleDirection string

const (
	dirCosmosToEth moduleDirection = "cosmos_to_eth"
	dirEthToCosmos moduleDirection = "eth_to_cosmos"
	dirL2ToCosmos  moduleDirection = "l2_to_cosmos"
	dirCosmosToL2  moduleDirection = "cosmos_to_l2"
)

// isChainKind reports whether s names a known chain family (chain.ChainType).
func isChainKind(s string) bool {
	switch chain.ChainType(s) {
	case chain.Cosmos, chain.Ethereum, chain.OPStack, chain.Arbitrum:
		return true
	default:
		return false
	}
}

// classifyModule resolves a module's relay direction. A module is CANONICAL when
// BOTH src_chain and dst_chain are known chain kinds — the (src, dst) pair selects
// the direction through the table below, so a new direction is added by extending
// the table + its adapter, not by editing a name switch. Otherwise the module is
// LEGACY and dispatches on `name`; this is deliberate back-compat, because an old
// config may carry a junk dst_chain (a since-removed field) or a src_chain that is
// really the ics26_client_id — neither is a valid chain kind, so such a module
// correctly stays on the legacy path. isLegacy keeps the src_chain ->
// ics26_client_id fallback that the canonical schema no longer overloads. An
// unrecognized module is a hard error — never silently dropped.
func classifyModule(m configModule) (dir moduleDirection, isLegacy bool, err error) {
	if isChainKind(m.SrcChain) && isChainKind(m.DstChain) {
		src := chain.ChainType(m.SrcChain)
		dst := chain.ChainType(m.DstChain)
		switch {
		case src == chain.Cosmos && dst == chain.Ethereum:
			return dirCosmosToEth, false, nil
		case src == chain.Ethereum && dst == chain.Cosmos:
			return dirEthToCosmos, false, nil
		case (src == chain.OPStack || src == chain.Arbitrum) && dst == chain.Cosmos:
			return dirL2ToCosmos, false, nil
		case src == chain.Cosmos && (dst == chain.OPStack || dst == chain.Arbitrum):
			return dirCosmosToL2, false, nil
		default:
			return "", false, fmt.Errorf("module %q: unsupported direction src_chain=%q dst_chain=%q", m.Name, m.SrcChain, m.DstChain)
		}
	}
	switch m.Name {
	case string(dirCosmosToEth):
		return dirCosmosToEth, true, nil
	case string(dirEthToCosmos):
		return dirEthToCosmos, true, nil
	default:
		return "", false, fmt.Errorf(
			"module %q: unrecognized module; use a legacy name (cosmos_to_eth / eth_to_cosmos) or declare src_chain + dst_chain as chain kinds (cosmos, ethereum, opstack, arbitrum)", m.Name)
	}
}

type batchConfig struct {
	BatchSize          uint8  `json:"batch_size"`
	BatchPeriodSeconds uint64 `json:"batch_period_seconds"`
}

// jsonConfig still has no `server` member. One existed — log_level, address,
// port — and nothing ever read any of it: the relayer opened no listener, so
// there was no HTTP endpoint and no way for log_level to affect anything. Configs
// that still carry the block keep loading, because encoding/json ignores unknown
// members and nothing here sets DisallowUnknownFields (pinned by
// TestLoadConfig/ignores legacy server block).
//
// status_address is the one member that now HAS a reader, and it is flat rather
// than a block for exactly that reason: a block invites unread neighbours, which
// is how the last one came to describe a server that did not exist.
type jsonConfig struct {
	Batch                 batchConfig      `json:"batch"`
	DeprecatedBatchConfig *json.RawMessage `json:"batch_config"`
	Modules               []configModule   `json:"modules"`
	// StatusAddress is where `relayer start` serves its read-only status
	// snapshot. Empty (the default) means it binds nothing.
	StatusAddress string `json:"status_address"`
	// StatusAllowControl adds the one write route to that endpoint, which
	// returns a dead-lettered packet to the timeout scan set. Off by default and
	// separate from the address on purpose: turning on a way to READ what is stuck
	// must not also turn on a way to change it.
	StatusAllowControl bool `json:"status_allow_control"`
}

// appConfig is one relay path: one process serves exactly one source →
// destination pair (A1), and loadConfig rejects a file declaring more at parse
// time (rejectExtraRelayPaths). So each direction is a single value with a
// presence flag, never a list -- there is nothing to iterate, select from, or
// disambiguate with a --source flag, and every command reads the field for the
// direction it wants.
//
// The presence flags matter because a zero value is a legal-looking config: an
// absent module and a configured module with every field empty must fail
// differently (nothing to relay vs. a field to fill in).
type appConfig struct {
	// HasCosmosToEth / CosmosToEthConfig: the Cosmos→ETH source. The same
	// module also drives the ETH→Cosmos leg (runAdapterEngine), with
	// eth_to_cosmos supplying only the beacon URL.
	HasCosmosToEth    bool
	CosmosToEthConfig cosmosToEthConfig
	// HasCosmosToL2 / CosmosToL2Config: the Cosmos→L2 destination. Same schema
	// as cosmos_to_eth (the groth16 path covers Cosmos → any EVM), pointed at an
	// L2 rollup's exec RPC + its deployed SpectreClient/ICS26Router.
	// CosmosToL2Name is the module's label, for startup diagnostics.
	HasCosmosToL2    bool
	CosmosToL2Config cosmosToEthConfig
	CosmosToL2Name   string
	// HasEthToCosmos records whether the direction was explicitly configured.
	// It distinguishes an absent module from a configured module with an empty
	// beacon endpoint, which must fail validation instead of silently disabling
	// the ETH→Cosmos relay.
	HasEthToCosmos    bool
	EthToCosmosConfig ethToCosmosConfig
	// HasL2ToCosmos / L2ToCosmosConfig: the L2 (opstack/arbitrum) → Cosmos
	// source, the return leg of a Cosmos↔L2 path.
	HasL2ToCosmos    bool
	L2ToCosmosConfig l2ToCosmosConfig
	BatchConfig      services.BatchConfig
	// StatusAddress is the bind address for the read-only status endpoint, empty
	// when it is off. Off is the default: adding a status feature must not make a
	// relayer that was never asked to bind a port start listening.
	StatusAddress string
	// StatusAllowControl enables the endpoint's single write route.
	StatusAllowControl bool
}

// loadConfig reads a config that is expected to be complete — every client already
// created and recorded. This is what `start`, `update-client` and `create-clients-eth`
// want.
func loadConfig(configPath string) (*appConfig, error) {
	return loadConfigWith(configPath, true)
}

// loadConfigForClientCreation reads a config that is about to have its L2 wasm
// clients created, so their ids are legitimately still empty (#309). Only
// create-clients-cosmos uses this: it is the command that fills them in.
func loadConfigForClientCreation(configPath string) (*appConfig, error) {
	return loadConfigWith(configPath, false)
}

func loadConfigWith(configPath string, requireL2WasmClientID bool) (*appConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var jc jsonConfig
	if err := json.Unmarshal(data, &jc); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	if jc.DeprecatedBatchConfig != nil {
		return nil, fmt.Errorf("batch_config is deprecated; use batch")
	}

	// One relay path per file (A1), decided before any module is parsed in full:
	// a file with two paths is refused for that reason alone, not for a missing
	// field in the second one. Everything below therefore parses each direction
	// into a single value.
	census, err := censusRelayPaths(jc.Modules)
	if err != nil {
		return nil, err
	}
	if err := rejectExtraRelayPaths(configPath, census); err != nil {
		return nil, err
	}

	var (
		c2e, c2l2         cosmosToEthConfig
		c2eName, c2l2Name string
		hasC2E, hasC2L2   bool
		l2                l2ToCosmosConfig
		hasL2             bool
	)
	var e2c ethToCosmosConfig
	var hasEthToCosmos bool
	batch := services.DefaultConfig().BatchConfig
	if jc.Batch.BatchSize != 0 {
		batch.BatchSize = jc.Batch.BatchSize
	}
	if jc.Batch.BatchPeriodSeconds != 0 {
		batch.BatchPeriods = time.Duration(jc.Batch.BatchPeriodSeconds) * time.Second
	}
	for _, m := range jc.Modules {
		dir, isLegacy, err := classifyModule(m)
		if err != nil {
			return nil, err
		}
		switch dir {
		case dirCosmosToEth:
			var one cosmosToEthConfig
			if err := json.Unmarshal(m.Config, &one); err != nil {
				return nil, fmt.Errorf("module %q: parse cosmos_to_eth config: %w", m.Name, err)
			}
			// Legacy configs overloaded src_chain as the ics26_client_id fallback;
			// the canonical schema keeps src_chain as the chain kind, so only fall
			// back for a legacy module.
			if one.ICS26ClientID == "" && isLegacy {
				one.ICS26ClientID = m.SrcChain
			}
			c2e, c2eName, hasC2E = one, m.Name, true
		case dirEthToCosmos:
			hasEthToCosmos = true
			if err := json.Unmarshal(m.Config, &e2c); err != nil {
				return nil, fmt.Errorf("module %q: parse eth_to_cosmos config: %w", m.Name, err)
			}
		case dirCosmosToL2:
			// Same schema as cosmos_to_eth, pointed at the L2 (groth16 → any EVM).
			var one cosmosToEthConfig
			if err := json.Unmarshal(m.Config, &one); err != nil {
				return nil, fmt.Errorf("module %q: parse cosmos_to_l2 config: %w", m.Name, err)
			}
			c2l2, c2l2Name, hasC2L2 = one, m.Name, true
		case dirL2ToCosmos:
			var one l2ToCosmosConfig
			if err := json.Unmarshal(m.Config, &one); err != nil {
				return nil, fmt.Errorf("module %q: parse l2_to_cosmos config: %w", m.Name, err)
			}
			one.kind = chain.ChainType(m.SrcChain) // opstack | arbitrum
			if err := one.validateWith(requireL2WasmClientID); err != nil {
				return nil, fmt.Errorf("module %q: %w", m.Name, err)
			}
			if _, err := parseHeadKind(one.HeadKind); err != nil {
				return nil, fmt.Errorf("module %q: %w", m.Name, err)
			}
			l2, hasL2 = one, true
		}
	}

	if hasC2E {
		if err := validateCosmosToEthConfig(c2e, moduleFieldPrefix(c2eName, dirCosmosToEth)); err != nil {
			return nil, err
		}
	}

	// Validate eth_to_cosmos config if populated. Only eth_beacon_api_url is
	// consumed for the ETH→Cosmos direction.
	//
	// eth_ws_url is deliberately NOT required here — see validateRelayStartupConfig,
	// which `start` calls. loadConfig is shared with create-clients-cosmos,
	// create-clients-eth and update-client, none of which ever read EthWsUrl
	// (build_source.go is the only consumer, and it runs from `start` alone), so
	// rejecting a missing websocket at load time blocks client creation on a
	// field that command will not use.
	if hasEthToCosmos {
		if e2c.BeaconUrl == "" {
			return nil, fmt.Errorf("eth_to_cosmos is configured but eth_to_cosmos.eth_beacon_api_url is empty; the ETH→Cosmos direction reads finality and light-client updates from the beacon REST API and cannot run without it")
		}
		if err := validateURL(e2c.BeaconUrl, "eth_to_cosmos.eth_beacon_api_url"); err != nil {
			return nil, err
		}
	}

	// Same schema and checks as cosmos_to_eth for the L2 destination.
	if hasC2L2 {
		if err := validateCosmosToEthConfig(c2l2, moduleFieldPrefix(c2l2Name, dirCosmosToL2)); err != nil {
			return nil, err
		}
	}

	return &appConfig{
		HasCosmosToEth:     hasC2E,
		CosmosToEthConfig:  c2e,
		HasCosmosToL2:      hasC2L2,
		CosmosToL2Config:   c2l2,
		CosmosToL2Name:     c2l2Name,
		HasEthToCosmos:     hasEthToCosmos,
		EthToCosmosConfig:  e2c,
		HasL2ToCosmos:      hasL2,
		L2ToCosmosConfig:   l2,
		BatchConfig:        batch,
		StatusAddress:      jc.StatusAddress,
		StatusAllowControl: jc.StatusAllowControl,
	}, nil
}

// relayPathCensus is what rejectExtraRelayPaths needs and nothing more: how many
// paths the file declares and what to call each one.
type relayPathCensus struct {
	// declared names every relay module, by the field an operator edits it by.
	declared []string
	// Legs, not paths: a cosmos_to_l2 destination and an l2_to_cosmos source pair
	// up into one Cosmos↔L2 path, so those two are counted separately and
	// combined by rejectExtraRelayPaths.
	cosmosToEth, cosmosToL2Legs, l2ToCosmosLegs int
}

// censusRelayPaths reads only each module's direction and identity, before any
// module is parsed in full. A malformed config body is left for the full parse
// to report, in the words that name the field; here it just means "no id".
func censusRelayPaths(modules []configModule) (relayPathCensus, error) {
	var c relayPathCensus
	for _, m := range modules {
		dir, isLegacy, err := classifyModule(m)
		if err != nil {
			return c, err
		}
		var id struct {
			ICS26ClientID    string `json:"ics26_client_id"`
			AttestorSrcChain string `json:"attestor_src_chain"`
		}
		if len(m.Config) > 0 {
			_ = json.Unmarshal(m.Config, &id)
		}
		switch dir {
		case dirCosmosToEth:
			// Legacy configs overloaded src_chain as the ics26_client_id fallback.
			if id.ICS26ClientID == "" && isLegacy {
				id.ICS26ClientID = m.SrcChain
			}
			c.cosmosToEth++
			c.declared = append(c.declared, fmt.Sprintf("cosmos_to_eth ics26_client_id=%q", id.ICS26ClientID))
		case dirCosmosToL2:
			c.cosmosToL2Legs++
			c.declared = append(c.declared, fmt.Sprintf("cosmos_to_l2 ics26_client_id=%q%s", id.ICS26ClientID, moduleNameSuffix(m.Name)))
		case dirL2ToCosmos:
			c.l2ToCosmosLegs++
			c.declared = append(c.declared, fmt.Sprintf("l2_to_cosmos attestor_src_chain=%q", id.AttestorSrcChain))
		}
	}
	return c, nil
}

// rejectExtraRelayPaths is where one-process-one-pair is enforced: at parse
// time, before an appConfig exists, so nothing downstream ever holds more than
// one module per direction -- no lists, no loops that run at most once, no
// --source flag to pick among them.
//
// The rule is not a simplification for its own sake — running several paths in one
// process is what forces every shared structure between them: one nonce cache
// across unrelated chains (#320), one error path where a single dead loop takes
// the others down (#321), and one state directory two paths write into. A process per
// path removes all three by construction, so the blast radius of a failure is that
// path alone.
//
// A Cosmos→L2 destination and its L2→Cosmos return leg are ONE pair: they relay
// opposite directions of the same path, and the return leg resolves its timeout
// path from the forward one (findL2TimeoutReturnPath). So the number of Cosmos↔L2
// paths is however many legs the longer side declares. The Cosmos↔ETH pair needs no
// equivalent grouping — runAdapterEngine already drives both directions from a
// single cosmos_to_eth module, with eth_to_cosmos supplying only the beacon URL.
//
// The error names every path the file declares, individually rather than paired
// up: which forward leg a return leg belongs to is resolved at runtime from the
// L2 chain id, so pairing them here by file order would name the wrong partner
// exactly when the counts disagree — the case this error exists for. And it
// shows the way out, because telling the operator what is wrong without the
// split command leaves them editing the config at random.
func rejectExtraRelayPaths(configPath string, c relayPathCensus) error {
	if c.cosmosToEth+max(c.cosmosToL2Legs, c.l2ToCosmosLegs) <= 1 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s declares more than one relay path, but one relayer process serves exactly one source → destination pair.\n", configPath)
	b.WriteString("declared:\n")
	for _, d := range c.declared {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	b.WriteString("a cosmos_to_l2 destination and its l2_to_cosmos return leg are one path; everything else is a path of its own.\n")
	b.WriteString("keep one path per config file and start one process per file:\n")
	b.WriteString("  ./relayer start --config <one-path>.json")
	return fmt.Errorf("%s", b.String())
}

// resolveBeaconURL returns the L1 beacon endpoint to create/advance the Ethereum
// light client with. Only the eth_to_cosmos module declares one now: L2 sources no
// longer pin an Ethereum client, so an L2-only deployment needs no beacon at all.
func resolveBeaconURL(cfg *appConfig) string {
	if cfg.EthToCosmosConfig.BeaconUrl != "" {
		return cfg.EthToCosmosConfig.BeaconUrl
	}
	return ""
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func roleManagerOrDefault(cfg *appConfig) string {
	return envOrDefault("ROLE_MANAGER", cfg.CosmosToEthConfig.ICS26Address)
}

func cosmosRouterClientIDOrDefault(cfg *appConfig) string {
	return envOrDefault("ICS26_CLIENT_ID", cfg.CosmosToEthConfig.ICS26ClientID)
}

func cosmosWasmClientIDOrDefault(cfg *appConfig) string {
	return envOrDefault("COSMOS_WASM_CLIENT_ID", cfg.CosmosToEthConfig.CosmosWasmClientID)
}

// selectCosmosSource returns a shallow copy of cfg whose singular
// CosmosToEthConfig is the Cosmos→EVM module the file declares, so the
// create-clients and submit-misbehaviour flows (preflight, run funcs, config
// write-back) read one field whichever direction they serve.
//
// This is the promotion half of what used to be --source selection. The
// disambiguation half is gone with the lists: one path per file means there is
// nothing to pick among. The promotion stays because a cosmos_to_l2 entry is the
// same config struct pointed at the L2 exec RPC -- its ETH side (the SpectreClient
// + ICS26Router on the L2) is created by the same create-clients-eth flow -- and
// that flow reads CosmosToEthConfig. With neither module declared the config is
// returned as is; the command's own preflight then reports the empty endpoint.
func selectCosmosSource(cfg *appConfig) *appConfig {
	if cfg.HasCosmosToEth || !cfg.HasCosmosToL2 {
		return cfg
	}
	out := *cfg
	out.CosmosToEthConfig = cfg.CosmosToL2Config
	return &out
}

// relayPathID names the single relay path this process serves, compactly enough
// to sit on every log line.
//
// A1 makes this well defined: start refuses a config declaring more than one
// path, so the answer is a process-level constant and can be stamped once as a
// log prefix. Direction is NOT a process-level constant and deliberately absent
// here -- a path is bidirectional (cmd/run_adapters.go runs a cosmos->eth and an
// eth->cosmos module side by side), so the direction belongs on the labels of
// the code that actually knows it.
//
// The client id is part of the identity because three processes all relaying
// cosmos->eth from different source clients is a valid deployment, and their
// logs are merged by journald or Loki with no file boundary left to tell them
// apart. That merge is the problem this solves.
func relayPathID(cfg *appConfig) string {
	switch {
	case cfg.HasCosmosToEth:
		return "cosmos<->eth/" + cfg.CosmosToEthConfig.ICS26ClientID
	case cfg.HasCosmosToL2:
		// Prefer the rollup's own name over a generic "l2": the return leg names
		// the chain family (opstack / arbitrum / base) and an operator running two
		// rollups needs to tell them apart.
		kind := "l2"
		if cfg.HasL2ToCosmos && cfg.L2ToCosmosConfig.AttestorSrcChain != "" {
			kind = cfg.L2ToCosmosConfig.AttestorSrcChain
		}
		return "cosmos<->" + kind + "/" + cfg.CosmosToL2Config.ICS26ClientID
	case cfg.HasL2ToCosmos:
		return cfg.L2ToCosmosConfig.AttestorSrcChain + "->cosmos"
	}
	return "relayer"
}

// moduleNameSuffix appends the config module's label when it has one, so an
// operator who labelled their modules sees the label they wrote.
func moduleNameSuffix(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return fmt.Sprintf(" (module %q)", name)
}
