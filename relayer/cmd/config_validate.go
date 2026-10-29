package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	routerContract "relayer/bindings/ICS26Router"
	tendermintClient "relayer/client"
	"relayer/services"
	"relayer/transaction"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Everything that can prove a loaded config wrong before a relay loop opens:
// parse-time field checks, the create-clients preflight, and the startup probes.
// Split from config.go (E6): schema and loading live there, judging lives here.

// validateRelayStartupConfig rejects a config that would start the relay loop
// half-dead. It is called by `start` only — NOT by loadConfig, which the
// client-creation commands share.
//
// Configuring eth_to_cosmos states the intent to relay that direction, and it
// cannot run without a websocket: SubscribeEth needs eth_subscribe, which HTTP
// cannot serve. Without this check the relayer starts, logs a single
// "eth websocket URL is not configured" line while the prover is still loading,
// and then runs half-dead — the forward direction works, nothing ever picks up an
// acknowledgement, and no later line says why. Fail before the prover load instead.
//
// The eth->cosmos leg subscribes with the cosmos_to_eth module's own eth_ws_url
// (build_source.go passes c2e.EthWsUrl into its context), so that is the field
// checked.
func validateRelayStartupConfig(cfg *appConfig) error {
	if cfg == nil || !cfg.HasEthToCosmos || !cfg.HasCosmosToEth {
		return nil
	}
	if cfg.CosmosToEthConfig.EthWsUrl != "" {
		return nil
	}
	return fmt.Errorf(
		"eth_to_cosmos is configured but cosmos_to_eth (ics26_client_id %q) has an empty eth_ws_url; "+
			"the ETH→Cosmos direction subscribes to ICS26Router events over eth_subscribe "+
			"and cannot run without a ws:// or wss:// endpoint",
		cfg.CosmosToEthConfig.ICS26ClientID)
}

func moduleFieldPrefix(moduleName string, fallback moduleDirection) string {
	if name := strings.TrimSpace(moduleName); name != "" {
		return name
	}
	return string(fallback)
}

// validateCosmosToEthConfig checks the URLs and hex addresses of one Cosmos→ETH
// source or Cosmos→L2 destination. Empty (unpopulated) sources pass so a config
// with only an eth_to_cosmos module still loads.
func validateCosmosToEthConfig(c2e cosmosToEthConfig, fieldPrefix string) error {
	if c2e.TmRpcUrl == "" && c2e.EthRpcUrl == "" && c2e.ICS26Address == "" {
		return nil
	}
	if err := validateURL(c2e.TmRpcUrl, fieldPrefix+".tm_rpc_url"); err != nil {
		return err
	}
	if err := validateURL(c2e.EthRpcUrl, fieldPrefix+".eth_rpc_url"); err != nil {
		return err
	}
	if c2e.EthWsUrl != "" {
		if err := validateURL(c2e.EthWsUrl, fieldPrefix+".eth_ws_url"); err != nil {
			return err
		}
	}
	if err := validateHexAddress(c2e.ICS26Address, fieldPrefix+".ics26_address"); err != nil {
		return err
	}
	// An empty address is accepted for every field here on purpose. Only the
	// SpectreClient deploy reads membership, misbehaviour and update_client, so a
	// relay-only config that names an already-deployed spectre_client must still
	// load. A value that is set is checked for shape here and for code by
	// validateEVMDeployments at start. Where an empty module does matter,
	// create-clients-eth refuses it (checkSpectreModulesBeforeDeploy, #568).
	for _, f := range []struct {
		val, name string
	}{
		{c2e.SpectreClient, fieldPrefix + ".spectre_client"},
		{c2e.SignatureVerifier, fieldPrefix + ".signature_verifier"},
		{c2e.Membership, fieldPrefix + ".membership"},
		{c2e.Misbehaviour, fieldPrefix + ".misbehaviour"},
		{c2e.UpdateClient, fieldPrefix + ".update_client"},
	} {
		if f.val != "" {
			if err := validateHexAddress(f.val, f.name); err != nil {
				return err
			}
		}
	}
	if c2e.RotationThreshold != "" {
		if _, err := services.ParseRotationThreshold(c2e.RotationThreshold); err != nil {
			return fmt.Errorf("%s.rotation_threshold: %w", fieldPrefix, err)
		}
	}
	return nil
}

func validateURL(rawURL, fieldName string) error {
	if rawURL == "" {
		return fmt.Errorf("%s is empty", fieldName)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q for %s: %w", rawURL, fieldName, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("URL %q for %s is missing scheme", rawURL, fieldName)
	}
	return nil
}

func validateHexAddress(addr, fieldName string) error {
	if addr == "" {
		return fmt.Errorf("%s is empty", fieldName)
	}
	if !common.IsHexAddress(addr) {
		return fmt.Errorf("invalid hex address %q for %s", addr, fieldName)
	}
	return nil
}

func preflightCreateClients(cfg *appConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ethClient, err := tendermintClient.DialEthRPC(ctx, cfg.CosmosToEthConfig.EthRpcUrl, tendermintClient.DefaultRPCTimeout)
	if err != nil {
		return fmt.Errorf("ethereum rpc unavailable at %s: %w", cfg.CosmosToEthConfig.EthRpcUrl, err)
	}
	defer ethClient.Close()

	chainID, err := ethClient.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("ethereum rpc not responding at %s: %w", cfg.CosmosToEthConfig.EthRpcUrl, err)
	}

	cosmosClient, err := tendermintClient.DialCosmosRPC(cfg.CosmosToEthConfig.TmRpcUrl, "/websocket", tendermintClient.DefaultRPCTimeout)
	if err != nil {
		return fmt.Errorf("failed to create cosmos rpc client for %s: %w", cfg.CosmosToEthConfig.TmRpcUrl, err)
	}
	status, err := cosmosClient.Status(ctx)
	if err != nil {
		return fmt.Errorf("cosmos rpc unavailable at %s: %w", cfg.CosmosToEthConfig.TmRpcUrl, err)
	}

	if beaconURL := resolveBeaconURL(cfg); beaconURL != "" {
		if _, err := tendermintClient.GetBeaconGenesis(ctx, beaconURL); err != nil {
			return fmt.Errorf("beacon api unavailable at %s: %w", beaconURL, err)
		}
	}

	log.Printf("[create-clients] preflight OK: cosmos_height=%d eth_chain_id=%s", status.SyncInfo.LatestBlockHeight, chainID.String())
	return nil
}

// validateStartupKeys uses the transaction signer seam so startup cannot retain
// a separate environment-only signing-key path.
func validateStartupKeys() error {
	return (&transaction.Handler{}).ValidateKeys()
}

// requireRelayPath is `start`'s half of the one-path rule: loadConfig has already
// refused a file with more than one path (rejectExtraRelayPaths), so what is left
// to check is that there is one at all. `start` with nothing to relay is a config
// mistake, not an idle process.
//
// This runs before the prover load so a config mistake surfaces at once rather than
// after the bucket registry is read.
func requireRelayPath(cfg *appConfig, configPath string) error {
	if cfg == nil || !(cfg.HasCosmosToEth || cfg.HasCosmosToL2 || cfg.HasL2ToCosmos) {
		return fmt.Errorf("no relay source configured in %s (need a cosmos_to_eth, cosmos_to_l2, or l2_to_cosmos module)", configPath)
	}
	return nil
}

// validateStartupConfig runs every check that is cheap, read-only, and able to
// prove this config wrong before the relay loops open.
//
// It exists because the alternative is the most expensive failure shape there
// is: the process starts, loads the prover, relays for hours, and only then
// reveals that ics26_client_id names a client the router never heard of — by
// which time packets are already half-relayed. Everything checked here costs one
// read and no gas, so none of it has a reason to surface later.
//
// Two rules decide what belongs, and both are load-bearing:
//
//   - A DEFINITE DISAGREEMENT is fatal. The endpoint answered, and the answer
//     contradicts the config. No amount of retrying fixes that.
//   - A NON-ANSWERING endpoint is not. That is an availability problem, not a
//     configuration one; the relay loops already retry, and refusing to boot on
//     a blip would turn it into an outage. Same doctrine verifyL2ChainID
//     follows, stated there and reused here.
//
// Waiting states — beacon finality, the attestor frontier — are deliberately out
// of scope. They are operational conditions, not wrong config, and blocking
// startup on them would make a healthy cold start look like a broken one.
//
// Every finding is collected before returning rather than failing on the first.
// A config with three wrong keys should take one run to diagnose, not three.
//
// It runs on a config loadConfig has already reduced to one path, so each
// direction below is a single value behind a presence flag. An absent direction
// is the normal shape for the one this process does not serve.
//
// The attestor half lives in validateAttestorHeads (attestor_head_check.go).
// It could not be written until #441 settled the attestor's startup contract —
// the capabilities list it was going to read never shipped, replaced by the
// per-header Ed25519 attestations — and now reads the one field that contract
// does expose: the head each feed gates on.
func validateStartupConfig(stdCtx context.Context, cfg *appConfig, stateDir string) error {
	if cfg == nil {
		return nil
	}

	var findings []configFinding
	findings = append(findings, validateStateDir(stateDir)...)
	findings = append(findings, validateCosmosChainID(stdCtx, cfg)...)
	findings = append(findings, validateEVMDeployments(stdCtx, cfg)...)
	findings = append(findings, validateAttestorHeads(stdCtx, cfg)...)

	if len(findings) == 0 {
		log.Printf("[start] config validated")
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "config validation found %d problem(s); the relayer stops here rather than "+
		"surfacing them at the first packet:", len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "\n  - %s: %s", f.key, f.detail)
	}
	return fmt.Errorf("%s", b.String())
}

// configFinding is one definite disagreement between the config and the world,
// named by the config key or environment variable at fault.
type configFinding struct {
	key    string
	detail string
}

// configProbeTimeout bounds one read taken while validating. Same value and same
// reason as l2ChainIDProbeTimeout: a rate-limited public endpoint can take
// seconds to answer, and a slow answer must not be mistaken for a wrong one.
const configProbeTimeout = 10 * time.Second

// stateDirProbeFile is the basename written and immediately removed to prove the
// state directory is writable. It carries the pid so two relayers probing the
// same directory cannot delete each other's probe.
const stateDirProbeFile = ".relayer-write-probe"

// validateStateDir proves the recovery-cursor directory can actually be written
// before anything depends on it.
//
// An unwritable state directory does not stop the relayer today: the cursor save
// fails per tick, deep inside the recovery path, and the process keeps relaying
// with an in-memory cursor that dies with it. The next restart then silently
// falls back to the lookback window — the exact event-loss shape persisted
// cursors exist to close.
//
// This creates the directory rather than inspecting an ancestor's mode bits.
// Reading permissions and predicting the outcome is a guess, and guessing is the
// failure this check removes; the run creates that same directory moments later
// anyway. Nothing on a chain changes, and nothing another process can observe
// beyond a directory that was about to exist.
// stateDir is the per-path directory this process will write into, which the
// caller derives from the config (relayStateDir). It used to be read from
// services.RecoveryStatePath and RELAYER_RECOVERY_STATE_FILE; this branch moved
// the cursor into the per-path directory and checkLegacyStateLayout now REFUSES
// that variable outright, so validating the old location would be checking a
// path nothing writes.
func validateStateDir(stateDir string) []configFinding {
	if stateDir == "" {
		return nil // no path resolved yet: nothing to judge, and nothing is written either
	}
	path := filepath.Join(stateDir, services.RecoveryCursorsFileName)
	key := "state directory"
	dir := stateDir

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return []configFinding{{
			key: key,
			detail: fmt.Sprintf("recovery state file %q needs directory %q, which cannot be created: %v. "+
				"Without it every recovery cursor is lost on restart and the relayer falls back to its "+
				"lookback window, which is the event-loss window the cursors exist to close", path, dir, err),
		}}
	}

	probe := filepath.Join(dir, fmt.Sprintf("%s.%d", stateDirProbeFile, os.Getpid()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return []configFinding{{
			key: key,
			detail: fmt.Sprintf("directory %q for recovery state file %q is not writable: %v. "+
				"Recovery cursors would silently fail to persist and every restart would fall back "+
				"to the lookback window", dir, path, err),
		}}
	}
	closeErr := f.Close()
	removeErr := os.Remove(probe)
	if closeErr != nil || removeErr != nil {
		return []configFinding{{
			key: key,
			detail: fmt.Sprintf("directory %q for recovery state file %q accepted a file but not its "+
				"completion (close: %v, remove: %v)", dir, path, closeErr, removeErr),
		}}
	}
	return nil
}

// validateCosmosChainID compares COSMOS_CHAIN_ID against the chain the configured
// Cosmos endpoint actually serves.
//
// The variable is read only when a Cosmos transaction is built
// (transaction/cosmos.go), so today a wrong or missing value surfaces at the
// first Cosmos send — after the packet was received, the proof was built and the
// fee was budgeted.
//
// A Cosmos↔L2 path declares tm_rpc_url on both of its legs, pointing at the same
// chain, so the endpoints are de-duplicated and each distinct one is asked once.
func validateCosmosChainID(stdCtx context.Context, cfg *appConfig) []configFinding {
	urls := cosmosEndpointsInConfig(cfg)
	if len(urls) == 0 {
		return nil
	}

	want := strings.TrimSpace(os.Getenv("COSMOS_CHAIN_ID"))
	if want == "" {
		return []configFinding{{
			key: "COSMOS_CHAIN_ID",
			detail: "not set, but this config relays through Cosmos. Every Cosmos transaction is signed " +
				"with it, so the relayer would start, receive packets, build proofs, and only then fail " +
				"on the first send",
		}}
	}

	var findings []configFinding
	for _, url := range urls {
		got, ok := probeCosmosNetwork(stdCtx, url)
		if !ok {
			log.Printf("WARN [start] could not read the chain id from %s; skipping the COSMOS_CHAIN_ID check "+
				"for that endpoint (declared %q). The relay loops keep retrying it.", url, want)
			continue
		}
		if got != want {
			findings = append(findings, configFinding{
				key: "COSMOS_CHAIN_ID / tm_rpc_url",
				detail: fmt.Sprintf("COSMOS_CHAIN_ID is %q but tm_rpc_url %s serves chain %q. Transactions "+
					"signed for %q are rejected by %q, so no Cosmos send from this process can succeed",
					want, url, got, want, got),
			})
			continue
		}
		log.Printf("[start] cosmos chain id verified for %s: %s", url, got)
	}
	return findings
}

// cosmosEndpointsInConfig returns each distinct tm_rpc_url in the config, in file
// order. Every relay direction here has a Cosmos side, so each declared
// direction contributes one.
func cosmosEndpointsInConfig(cfg *appConfig) []string {
	var urls []string
	seen := map[string]bool{}
	add := func(u string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		urls = append(urls, u)
	}
	if cfg.HasCosmosToEth {
		add(cfg.CosmosToEthConfig.TmRpcUrl)
	}
	if cfg.HasCosmosToL2 {
		add(cfg.CosmosToL2Config.TmRpcUrl)
	}
	if cfg.HasL2ToCosmos {
		add(cfg.L2ToCosmosConfig.TmRpcUrl)
	}
	return urls
}

// probeCosmosNetwork returns the chain id the endpoint reports. ok is false when
// it did not answer at all — the tolerated case, kept distinct from an empty
// answer so "no answer" is never compared as if it were one.
func probeCosmosNetwork(stdCtx context.Context, url string) (network string, ok bool) {
	ctx, cancel := context.WithTimeout(stdCtx, configProbeTimeout)
	defer cancel()

	c, err := tendermintClient.DialCosmosRPC(url, "/websocket", configProbeTimeout)
	if err != nil {
		return "", false
	}
	status, err := c.Status(ctx)
	if err != nil || status == nil {
		return "", false
	}
	return status.NodeInfo.Network, true
}

// evmDeployment is one EVM endpoint and everything this config claims is
// deployed on it.
type evmDeployment struct {
	// spectreAddr and wasmClientID are what the router's registration must agree
	// with. Empty means "not declared for this leg", and the check is skipped.
	spectreAddr  string
	wasmClientID string
	label        string
	rpcURL       string
	routerAddr   string
	routerCliID  string
	// routerKey and clientIDKey are the config keys the router address and the
	// client id came from. They differ per module kind — ics26_address /
	// ics26_client_id on the outbound legs, rollup_profile.common.l2_router /
	// l2_ics26_client_id on the return leg — and a finding must name the key the
	// operator will grep for, not the one this file happens to know.
	routerKey   string
	clientIDKey string
	// addresses maps a config key to the address it names. Only well-formed,
	// non-empty addresses are listed: an unset optional address is a different
	// question, already answered by the required-field checks in loadConfig.
	addresses map[string]string
}

// validateEVMDeployments proves that every address this config points at holds
// code, and that each router knows the client id its module relays through.
//
// Both failures are invisible until the first packet and then
// indistinguishable: a call to an address with no code and a call for an
// unknown client id both come back as an opaque revert with no reason string.
// Reading eth_getCode and one getClient view call costs nothing and separates
// them.
func validateEVMDeployments(stdCtx context.Context, cfg *appConfig) []configFinding {
	var findings []configFinding

	for _, d := range evmDeploymentsInConfig(cfg) {
		ctx, cancel := context.WithTimeout(stdCtx, configProbeTimeout)
		client, err := tendermintClient.DialEthRPC(ctx, d.rpcURL, configProbeTimeout)
		if err != nil {
			cancel()
			log.Printf("WARN [start] could not dial %s for %s; skipping its contract checks. "+
				"The relay loops keep retrying it.", d.rpcURL, d.label)
			continue
		}

		reachable := true
		for _, key := range sortedKeys(d.addresses) {
			addr := d.addresses[key]
			code, err := client.CodeAt(ctx, common.HexToAddress(addr), nil)
			if err != nil {
				// One unanswered read means the endpoint stopped answering, not
				// that the contract is missing. Stop probing it rather than
				// reporting every remaining address as absent.
				reachable = false
				log.Printf("WARN [start] %s: could not read code at %s (%s) from %s; skipping the remaining "+
					"contract checks for this endpoint", d.label, key, addr, d.rpcURL)
				break
			}
			if len(code) == 0 {
				findings = append(findings, configFinding{
					key: fmt.Sprintf("%s.%s", d.label, key),
					detail: fmt.Sprintf("%s holds no code on %s. Every call the relayer makes to it returns "+
						"empty, which reaches the log as an unexplained revert at the first packet",
						addr, d.rpcURL),
				})
			}
		}

		if reachable {
			findings = append(findings, validateRouterClientID(ctx, client, d, findings)...)
		}
		client.Close()
		cancel()
	}
	return findings
}

// validateRouterClientID asks the router for the client id this module relays
// through. It is skipped when the router itself was already reported as
// code-less, because "the router has no code" and "the router does not know this
// client" would then be one fact reported twice, sending an operator looking for
// a second problem.
// validateRouterClientID asks the router whether it knows d.routerCliID, and
// whether the client it resolves to is wired to THIS deployment.
//
// found is read-only and is used for one thing: suppressing the check when the
// router address itself is already a finding. The return value carries only
// what this function discovered, because the caller appends it -- returning
// found would report every earlier finding a second time. It did: a module with
// five missing contracts came out as ten problems, the router check handing
// back the five it was given.
func validateRouterClientID(ctx context.Context, client *ethclient.Client, d evmDeployment, found []configFinding) []configFinding {
	if d.routerAddr == "" || d.routerCliID == "" {
		return nil
	}
	routerKey := fmt.Sprintf("%s.%s", d.label, d.routerKey)
	for _, f := range found {
		if f.key == routerKey {
			return nil
		}
	}

	caller, err := routerContract.NewContractICS26RouterCaller(common.HexToAddress(d.routerAddr), client)
	if err != nil {
		return nil
	}
	addr, err := caller.GetClient(&bind.CallOpts{Context: ctx}, d.routerCliID)
	if err == nil {
		log.Printf("[start] %s: router %s resolves client id %q to %s",
			d.label, d.routerAddr, d.routerCliID, addr.Hex())
		// Resolving is not the same as being wired to THIS deployment. A client id
		// registered against a different light client, or counterparty-wired to a
		// different Cosmos client, resolves perfectly well and then sends every
		// update and proof at one client while the router verifies another. The
		// pure half of that decision already exists and is tested; only the RPC to
		// feed it was missing here.
		if d.spectreAddr == "" || d.wasmClientID == "" {
			return nil
		}
		// The address half first, and unconditionally: it is already in hand, and a
		// counterparty read that fails must not hide a mismatch we have already
		// proved. Reading the counterparty is a second RPC and gets the usual
		// treatment for a call that did not answer -- logged, not fatal.
		expected := common.HexToAddress(d.spectreAddr)
		counterparty := ""
		if cp, cpErr := caller.GetCounterparty(&bind.CallOpts{Context: ctx}, d.routerCliID); cpErr == nil {
			counterparty = cp.ClientId
		} else if addr == expected {
			log.Printf("WARN [start] %s: could not read the counterparty for client id %q (%v); "+
				"skipping the counterparty half of the wiring check",
				d.label, d.routerCliID, cpErr)
			return nil
		} else {
			counterparty = d.wasmClientID // let the address mismatch be the finding
		}
		if wErr := routerWiringIsReusable(
			routerWiring{client: addr, counterparty: counterparty},
			expected, d.wasmClientID,
		); wErr != nil {
			return []configFinding{{
				key: d.clientIDKey,
				detail: fmt.Sprintf("%s: the router at %s knows client id %q, but %s",
					d.label, d.routerAddr, d.routerCliID, wErr),
			}}
		}
		return nil
	}

	// getClient never returns the zero address: it reverts with
	// IBCClientNotFound (contracts/core/client-registry/ICS02ClientUpgradeable.sol:78-80). So a
	// revert IS the answer, and a transport error is not — telling them apart is
	// what keeps this check inside the doctrine on validateStartupConfig.
	if !services.IsEVMRevert(err) {
		log.Printf("WARN [start] %s: could not ask router %s for client id %q (%v); skipping that check. "+
			"The relay loops keep retrying it.", d.label, d.routerAddr, d.routerCliID, err)
		return nil
	}
	return []configFinding{{
		key: fmt.Sprintf("%s.%s", d.label, d.clientIDKey),
		detail: fmt.Sprintf("router %s on %s has no client registered under id %q (%v). Every packet this "+
			"module relays is addressed to that id, so none of them can be delivered",
			d.routerAddr, d.rpcURL, d.routerCliID, err),
	}}
}

// evmDeploymentsInConfig lists one entry per configured EVM endpoint.
//
// cosmos_to_eth and cosmos_to_l2 share a struct and a contract set — to the
// relayer an L2 is just another EVM chain hosting a SpectreClient and a router —
// so they are gathered identically and only the label differs. The l2_to_cosmos
// return leg reaches the same two things on the L2, it only names them
// differently: the address comes from rollup_profile.common.l2_router rather
// than an ics26_address key, and the endpoint is l2_rpc_url. Checking only the
// outbound legs would leave the return path with the failure this validation
// exists to remove, which is the one-sided fix this repo keeps finding.
func evmDeploymentsInConfig(cfg *appConfig) []evmDeployment {
	var out []evmDeployment
	// routerCliID and wasmClientID are passed in rather than derived here,
	// because which of them the RELAY will use differs by path and validating
	// the other one proves nothing. See the two loops below.
	gather := func(c cosmosToEthConfig, label, routerCliID, wasmClientID string) {
		if c.EthRpcUrl == "" {
			return
		}
		addrs := map[string]string{}
		for key, addr := range map[string]string{
			"ics26_address":      c.ICS26Address,
			"spectre_client":     c.SpectreClient,
			"signature_verifier": c.SignatureVerifier,
			"membership":         c.Membership,
			"misbehaviour":       c.Misbehaviour,
			"update_client":      c.UpdateClient,
		} {
			if common.IsHexAddress(addr) {
				addrs[key] = addr
			}
		}
		if len(addrs) == 0 {
			return
		}
		out = append(out, evmDeployment{
			label:        label,
			rpcURL:       c.EthRpcUrl,
			routerAddr:   addrs["ics26_address"],
			routerCliID:  routerCliID,
			routerKey:    "ics26_address",
			clientIDKey:  "ics26_client_id",
			spectreAddr:  addrs["spectre_client"],
			wasmClientID: wasmClientID,
			addresses:    addrs,
		})
	}
	if cfg.HasCosmosToEth {
		c := cfg.CosmosToEthConfig
		// The EFFECTIVE ids, not the ones in the file: buildSource applies these
		// same overrides (build_source.go:90-100), so validating the raw value
		// proves a config startup then does not use. A stale ICS26_CLIENT_ID in
		// .env would pass here and fail at the first packet -- the exact failure
		// this whole function exists to move forward.
		gather(c, "cosmos_to_eth",
			envOrDefault("ICS26_CLIENT_ID", c.ICS26ClientID),
			envOrDefault("COSMOS_WASM_CLIENT_ID", c.CosmosWasmClientID))
	}
	if cfg.HasCosmosToL2 {
		c := cfg.CosmosToL2Config
		// NOT overridden, and the asymmetry is the point: buildCosmosToL2Dest
		// reads c2l.ICS26ClientID directly and never consults the environment
		// (build_cosmos_to_l2.go:57,112,125). Applying the override here validated
		// an id this path will never use -- so a stale value in .env failed a
		// perfectly good JSON config, and a correct one hid a bad JSON id. Found
		// in review, and it is the mirror of the bug the cosmos_to_eth branch
		// above fixes: the rule is that validation reads whatever the BUILDER
		// reads, per path.
		gather(c, "cosmos_to_l2", c.ICS26ClientID, c.CosmosWasmClientID)
	}
	if src := cfg.L2ToCosmosConfig; cfg.HasL2ToCosmos && src.L2RpcUrl != "" {
		router, err := l2RouterFromProfile(src.RollupProfile)
		if err != nil {
			// A malformed profile is already a load-time error; nothing to add.
			return out
		}
		out = append(out, evmDeployment{
			label:       "l2_to_cosmos",
			rpcURL:      src.L2RpcUrl,
			routerAddr:  router.Hex(),
			routerCliID: src.L2ICS26ClientID,
			routerKey:   "rollup_profile.common.l2_router",
			clientIDKey: "l2_ics26_client_id",
			addresses:   map[string]string{"rollup_profile.common.l2_router": router.Hex()},
		})
	}
	return out
}

// sortedKeys keeps findings and log lines in a stable order, so two runs against
// the same broken config produce the same message.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
