package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"attestor/types/attestation"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"

	"relayer/chain"
	"relayer/chain/l2rollup"
	"relayer/chain/l2rollup/attestorgrpc"
	relayerclient "relayer/client"
	"relayer/relay"
	"relayer/services"
	"relayer/status"
)

// l2ToCosmosConfig is one L2->Cosmos source's `config` block. It is self-contained:
// the L1/L2 execution RPCs, the Cosmos RPC hosting the L2 wasm client, the attestor
// sidecar address + its src_chain key (#240), the two client ids (the wasm client on
// Cosmos and the ICS26Router client on the L2), the head policy, and the rollup
// profile (whose common.l2_router the membership proofs are taken against).
type l2ToCosmosConfig struct {
	// L1RpcUrl, OpNodeRpcUrl and EthBeaconAPIURL are gone with the settlement path:
	// the client no longer verifies anything against L1, so this module never dials
	// one. That also retires the pinned-Ethereum-client dependency (#276) for L2
	// sources — there is no client here that has to be kept advancing.
	L2RpcUrl string `json:"l2_rpc_url"`
	TmRpcUrl string `json:"tm_rpc_url"`

	AttestorEndpoints []l2AttestorEndpointConfig `json:"attestor_endpoints"`
	AttestorSrcChain  string                     `json:"attestor_src_chain"`
	// Attestors is the exact immutable key set stored in the paired wasm client.
	// Every endpoint below names one key by its canonical set index.
	Attestors       attestation.AttestorConfig `json:"attestors"`
	L2WasmClientID  string                     `json:"l2_wasm_client_id"`
	L2ICS26ClientID string                     `json:"l2_ics26_client_id"`
	HeadKind        string                     `json:"head_kind"`
	// IncludeProvisional accepts attestor roots whose irreversible-finality check has
	// not passed yet; what such a root can still lose depends on its source (see
	// docs/L2_CLIENTS.md, "Provisional attestations"). It is deliberately its own
	// field: it used to be inferred from head_kind, which conflated two independent
	// axes — head_kind selects which L2 head is read, provisional is about whether
	// the attestor's finality re-check has completed. Defaults to true for anything
	// but finalized, an accepted trade-off documented in the same section.
	IncludeProvisional *bool `json:"include_provisional,omitempty"`
	// LogScanChunk caps the block span of a single eth_getLogs on the L2 packet
	// scan. 0 (the default) issues one call per range, which is right for any
	// provider that does not cap the span. It covered the L1 assertion scan too
	// until #347 removed that path; only the L2 scan is left to bound.
	//
	// It lives here rather than in rollup_profile because the profile is forwarded
	// verbatim into MsgCreateClient and the Rust verifier parses it with
	// deny_unknown_fields — a relayer-only knob added there would break client
	// creation.
	//
	// Known caps: Alchemy free tier 10, drpc 10_000.
	LogScanChunk uint64 `json:"log_scan_chunk,omitempty"`
	// RouterDeployBlock is the block the L2 ICS26 router was deployed at.
	// Optional: when set, the packet-flush backstop resolves an outstanding
	// packet's event with one eth_getLogs over [router_deploy_block, head]; when
	// unset it walks bounded windows back from the head. Relayer-only, so it lives
	// here and not in rollup_profile for the reason given above.
	RouterDeployBlock uint64          `json:"router_deploy_block,omitempty"`
	RollupProfile     json.RawMessage `json:"rollup_profile"`

	// kind is the chain family (opstack/arbitrum) resolved from the module's
	// src_chain by loadConfig; it selects the per-L2 header builder.
	kind chain.ChainType
}

// l2AttestorEndpointConfig maps a reachable attestor daemon to its immutable
// ClientState public-key index. Keeping the key in one canonical set prevents a
// typo from making the relayer sign for a different client than it bootstrapped.
type l2AttestorEndpointConfig struct {
	Address       string `json:"address"`
	AttestorIndex uint16 `json:"attestor_index"`
}

// validate checks everything the relay path needs, including the id of an L2 wasm
// client that must already exist on Cosmos.
func (c l2ToCosmosConfig) validate() error {
	return c.validateWith(true)
}

// validateForClientCreation is validate minus the one field create-clients-cosmos
// is about to produce. That command creates the L2 wasm client and writes its id
// back into this module, so demanding the id up front made a fresh config
// unusable: the only way forward was to invent a plausible one ("08-wasm-1"),
// which the command then overwrote — pure ceremony, and a wrong guess was accepted
// just as silently as a right one (#309).
//
// The exemption is deliberately this narrow. Every other field is needed to reach
// a chain at all, and failing on those before spending an on-chain MsgCreateClient
// is the point of validating here.
func (c l2ToCosmosConfig) validateForClientCreation() error {
	return c.validateWith(false)
}

func (c l2ToCosmosConfig) validateWith(requireWasmClientID bool) error {
	required := map[string]string{
		"l2_rpc_url": c.L2RpcUrl, "tm_rpc_url": c.TmRpcUrl,
		"attestor_src_chain": c.AttestorSrcChain,
		"l2_ics26_client_id": c.L2ICS26ClientID,
	}
	if requireWasmClientID {
		required["l2_wasm_client_id"] = c.L2WasmClientID
	}
	for name, v := range required {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("l2_to_cosmos config: %s is required", name)
		}
	}
	if len(c.RollupProfile) == 0 {
		return fmt.Errorf("l2_to_cosmos config: rollup_profile is required")
	}
	if _, err := parseHeadKind(c.HeadKind); err != nil {
		return err
	}
	if _, err := l2RouterFromProfile(c.RollupProfile); err != nil {
		return err
	}
	if _, err := l2ChainIDFromProfile(c.RollupProfile); err != nil {
		return fmt.Errorf("l2_to_cosmos config: %w", err)
	}
	if err := c.Attestors.Validate(); err != nil {
		return fmt.Errorf("l2_to_cosmos config: invalid attestors: %w", err)
	}
	if len(c.AttestorEndpoints) < int(c.Attestors.Threshold) {
		return fmt.Errorf("l2_to_cosmos config: %d attestor_endpoints cannot satisfy threshold %d", len(c.AttestorEndpoints), c.Attestors.Threshold)
	}
	seen := make(map[uint16]struct{}, len(c.AttestorEndpoints))
	for _, endpoint := range c.AttestorEndpoints {
		if endpoint.Address == "" {
			return fmt.Errorf("l2_to_cosmos config: attestor endpoint address is required")
		}
		if int(endpoint.AttestorIndex) >= len(c.Attestors.PublicKeys) {
			return fmt.Errorf("l2_to_cosmos config: attestor endpoint index %d is outside attestors.public_keys", endpoint.AttestorIndex)
		}
		if _, duplicate := seen[endpoint.AttestorIndex]; duplicate {
			return fmt.Errorf("l2_to_cosmos config: duplicate attestor endpoint index %d", endpoint.AttestorIndex)
		}
		seen[endpoint.AttestorIndex] = struct{}{}
	}
	return nil
}

// parseHeadKind maps the config string to the anti-reorg head policy (default Safe).
func parseHeadKind(s string) (l2rollup.HeadKind, error) {
	switch s {
	case "", "safe":
		return l2rollup.Safe, nil
	case "unsafe":
		return l2rollup.Unsafe, nil
	case "finalized":
		return l2rollup.Finalized, nil
	default:
		return 0, fmt.Errorf("l2_to_cosmos config: head_kind %q must be unsafe|safe|finalized", s)
	}
}

// ethClientBeacon names one module's view of a Cosmos-side Ethereum light client:

func l2RouterFromProfile(profile json.RawMessage) (common.Address, error) {
	var pc struct {
		Common struct {
			L2Router string `json:"l2_router"`
		} `json:"common"`
	}
	if err := json.Unmarshal(profile, &pc); err != nil {
		return common.Address{}, fmt.Errorf("l2_to_cosmos config: parse rollup_profile.common: %w", err)
	}
	if !common.IsHexAddress(pc.Common.L2Router) {
		return common.Address{}, fmt.Errorf("l2_to_cosmos config: rollup_profile.common.l2_router %q is not a valid hex address", pc.Common.L2Router)
	}
	return common.HexToAddress(pc.Common.L2Router), nil
}

// buildL2ToCosmosModule constructs one L2->Cosmos relay module: an l2rollup Source
// (gated on the attestor), a Cosmos Destination hosting the L2 wasm client, the
// per-L2 header builder, and timeout recovery through the matching Cosmos->L2
// SpectreClient/ICS26Router return path. It returns the module, the paired
// Cosmos->L2 timeout recovery callbacks, and a cleanup that
// closes the dialed clients. The caller runs module.Run(ctx) on its own goroutine.
// stateDir may be empty, which keeps the scan cursor in memory only. `start`
// always supplies one; the tests do not.
func buildL2ToCosmosModule(logger *zap.Logger, cfg l2ToCosmosConfig, txHandler services.TransactionHandler, timeoutReturn l2TimeoutReturnPath, stateDir string) (*relay.Module, services.L2TimeoutRecovery, func(), error) {
	// Before anything dials: a retired env key that silently sizes nothing is the
	// failure NFR 10 forbids. Checked here rather than globally because the key
	// only ever affected this path, so a deployment without an l2_to_cosmos
	// module has nothing to correct.
	if err := l2rollup.RejectRetiredLookbackEnv(); err != nil {
		return nil, services.L2TimeoutRecovery{}, nil, err
	}
	headKind, err := parseHeadKind(cfg.HeadKind)
	if err != nil {
		return nil, services.L2TimeoutRecovery{}, nil, err
	}
	router, err := l2RouterFromProfile(cfg.RollupProfile)
	if err != nil {
		return nil, services.L2TimeoutRecovery{}, nil, err
	}
	if err := timeoutReturn.validate(); err != nil {
		return nil, services.L2TimeoutRecovery{}, nil, err
	}
	chainID, err := l2ChainIDFromProfile(cfg.RollupProfile)
	if err != nil {
		return nil, services.L2TimeoutRecovery{}, nil, fmt.Errorf("l2_to_cosmos config: %w", err)
	}
	includeProvisional, warning := resolveIncludeProvisional(headKind, cfg.IncludeProvisional)
	if warning != "" {
		logger.Sugar().Warn(warning)
	}

	// Read before dialling anything. A corrupt cursor file is fatal (B6/G4), and
	// it must be fatal here, where there is nothing open to unwind -- not after
	// three clients and an attestor connection are already up.
	var recoveryState *services.RecoveryStateStore
	if stateDir != "" {
		recoveryState, err = services.LoadRecoveryState(filepath.Join(stateDir, services.RecoveryCursorsFileName))
		if err != nil {
			return nil, services.L2TimeoutRecovery{}, nil, fmt.Errorf("load L2 recovery state: %w", err)
		}
	}

	l2, err := relayerclient.DialEthRPC(context.Background(), cfg.L2RpcUrl, relayerclient.DefaultRPCTimeout)
	if err != nil {
		return nil, services.L2TimeoutRecovery{}, nil, fmt.Errorf("dial L2 rpc: %w", err)
	}
	cosmosClient, err := relayerclient.DialCosmosRPC(cfg.TmRpcUrl, "/websocket", relayerclient.DefaultRPCTimeout)
	if err != nil {
		l2.Close()
		return nil, services.L2TimeoutRecovery{}, nil, fmt.Errorf("create Cosmos rpc client: %w", err)
	}
	if err := cosmosClient.Start(); err != nil {
		l2.Close()
		return nil, services.L2TimeoutRecovery{}, nil, fmt.Errorf("start Cosmos rpc client: %w", err)
	}
	attestors := make([]*attestorgrpc.Client, 0, len(cfg.AttestorEndpoints))
	signingAttestors := make([]l2rollup.SigningAttestor, 0, len(cfg.AttestorEndpoints))
	for _, endpoint := range cfg.AttestorEndpoints {
		attestor, err := attestorgrpc.Dial(endpoint.Address)
		if err != nil {
			l2.Close()
			if stopErr := cosmosClient.Stop(); stopErr != nil {
				log.Printf("WARN failed to terminate cosmos client: %v", stopErr)
			}
			for _, opened := range attestors {
				_ = opened.Close()
			}
			return nil, services.L2TimeoutRecovery{}, nil, fmt.Errorf("dial attestor index %d (%s): %w", endpoint.AttestorIndex, endpoint.Address, err)
		}
		attestors = append(attestors, attestor)
		signingAttestors = append(signingAttestors, l2rollup.SigningAttestor{Index: endpoint.AttestorIndex, Client: attestor})
	}

	// Cosmos-side context for the destination (it uses only CosmosClient + the
	// signer via the tx handler); the L2 exec client fills the eth slot unused here.
	svcCtx := services.CosmosEndpoint{Client: cosmosClient}
	worker := services.NewWorker(txHandler, nil)

	// One builder for every chain: the header is the canonical L2 block plus a router
	// account proof, and nothing about that is chain-specific any more. It takes the
	// same endpoint set the source gates heights on, so the block it packages is
	// bound to a threshold of replicas that independently recognise that height.
	headerBuilder, err := l2rollup.NewAttestedHeaderBuilder(l2, router, chainID, cfg.Attestors, signingAttestors, cfg.AttestorSrcChain, headKind.RunMode(), fmt.Sprintf("l2-%s", cfg.kind))
	if err != nil {
		l2.Close()
		if stopErr := cosmosClient.Stop(); stopErr != nil {
			log.Printf("WARN failed to terminate cosmos client: %v", stopErr)
		}
		for _, opened := range attestors {
			_ = opened.Close()
		}
		return nil, services.L2TimeoutRecovery{}, nil, err
	}
	frontier, err := l2rollup.NewQuorumAttestationFrontier(signingAttestors, cfg.Attestors.Threshold)
	if err != nil {
		l2.Close()
		if stopErr := cosmosClient.Stop(); stopErr != nil {
			log.Printf("WARN failed to terminate cosmos client: %v", stopErr)
		}
		for _, opened := range attestors {
			_ = opened.Close()
		}
		return nil, services.L2TimeoutRecovery{}, nil, err
	}
	trackL2Pending, untrackL2Pending := l2PendingTrackerHooks(timeoutReturn.svc)
	source := l2rollup.NewSource(cfg.kind, l2, headKind, cfg.L2ICS26ClientID, cfg.L2WasmClientID, cfg.AttestorSrcChain, router, frontier, includeProvisional).
		WithLogScanChunk(cfg.LogScanChunk).
		WithRouterDeployBlock(cfg.RouterDeployBlock).
		// AckPacket / TimeoutPacket on the L2 close a packet's lifecycle. Reading
		// them lets the tracker drop a packet another relayer settled without
		// waiting for a timeout scan to query for it. Same hook the module uses to
		// untrack after its own successful relay -- one settlement path, two ways
		// of learning about it.
		WithSettleHook(untrackL2Pending).
		// The enumeration backstop skips a send already past its timeout, so the
		// module's track hook never sees it; the backstop registers it itself so
		// the timeout scanner can refund it.
		WithPendingTracker(trackL2Pending).
		// Reconcile against what Cosmos already records, so a packet another
		// relayer settled is not proven a second time.
		WithCosmosPacketState(l2rollup.NewCosmosPacketState(svcCtx)).
		// The acknowledged half also closes the owed-acknowledgement record. The
		// same Services instance backs both directions of this pair, which is what
		// lets a debt one side recorded be settled from the other.
		WithAckSettleHook(settleL2OwedAck(timeoutReturn.svc))
	if recoveryState != nil {
		// The scan cursor survives a restart only when the caller gave this path
		// a state directory; `start` always does, the tests do not.
		source = source.WithRecoveryState(recoveryState)
	}
	dest := l2rollup.NewDestination(worker, svcCtx, cfg.L2WasmClientID)
	builder := l2rollup.NewAttestedBuilder(headerBuilder)

	ackDue, ackSettled := ackWatchHooks(timeoutReturn.svc)
	module := relay.NewModule(
		fmt.Sprintf("%s->cosmos", cfg.kind),
		cfg.L2ICS26ClientID,
		source, dest, builder,
		// The destination client here is the L2's wasm client on Cosmos, so its
		// age is the "evm-on-cosmos" figure -- same slot the ETH path fills.
		relay.WithClientUpdateObserver(timeoutReturn.svc.ObserveEVMOnCosmosUpdate),
		relay.WithTimeoutScanner(0, func(c context.Context) {
			timeoutReturn.svc.ScanL2Timeouts(c, timeoutReturn.deps.Cosmos, timeoutReturn.deps.EVM, timeoutReturn.deps.IDs.CosmosOnEVM)
		}),
		relay.WithPacketTracker(trackL2Pending, untrackL2Pending),
		// The mirror of the cosmos->l2 hooks, over the SAME Services: that leg
		// records what a delivered receive owes, this one settles it when the
		// acknowledgement returns. The overdue reporter lives here rather than on
		// the outbound leg because this module exists exactly when the pair is
		// complete -- a forward-only deployment has no return leg and must not
		// alarm about acknowledgements it was never going to relay.
		relay.WithAckWatch(0, 0, ackDue, ackSettled, timeoutReturn.svc.OverdueAcks),
		// The enumeration backstop: L2 sends the L2 router still holds a
		// commitment for, and acknowledgements it wrote for Cosmos sends that
		// Cosmos still holds a commitment for. The L2 scan window is the
		// shortest of the four paths (#308), so this is the leg that needs it most.
		relay.WithPacketFlush(0),
	)
	logger.Sugar().Infof("l2->cosmos source: %s (attestors=%d threshold=%d src_chain=%s wasm_client=%s head=%s)",
		cfg.kind, len(cfg.AttestorEndpoints), cfg.Attestors.Threshold, cfg.AttestorSrcChain, cfg.L2WasmClientID, cfg.HeadKind)

	cleanup := func() {
		l2.Close()
		if err := cosmosClient.Stop(); err != nil {
			log.Printf("WARN failed to terminate cosmos client: %v", err)
		}
		for _, attestor := range attestors {
			if err := attestor.Close(); err != nil {
				log.Printf("WARN failed to close attestor client: %v", err)
			}
		}
	}
	timeoutRecovery := newL2TimeoutRecovery(cfg, headerBuilder, frontier, l2, router, includeProvisional)
	return module, timeoutRecovery, cleanup, nil
}

// newL2TimeoutRecovery binds the Cosmos->L2 timeout scanner to the same
// attestors, L2 router, and wasm client as the L2->Cosmos relay. Reusing this
// construction is essential: a timeout proof is accepted only against the
// exact attested block whose update is included in its Cosmos transaction.
func newL2TimeoutRecovery(cfg l2ToCosmosConfig, headerBuilder l2rollup.HeaderBuilder, frontier l2rollup.AttestationFrontier, l2 *ethclient.Client, router common.Address, includeProvisional bool) services.L2TimeoutRecovery {
	return services.L2TimeoutRecovery{
		ClientID: cfg.L2WasmClientID,
		BuildUpdate: func(stdCtx context.Context) (services.L2TimeoutUpdate, error) {
			ctx, cancel := context.WithTimeout(stdCtx, 90*time.Second)
			defer cancel()
			root, found, err := frontier.AttestedUpTo(ctx, cfg.AttestorSrcChain, includeProvisional)
			if err != nil {
				return services.L2TimeoutUpdate{}, fmt.Errorf("attested L2 frontier: %w", err)
			}
			if !found || root == nil || root.GetL2BlockNumber() == 0 {
				return services.L2TimeoutUpdate{}, fmt.Errorf("attestor has no relayable L2 height for %q", cfg.AttestorSrcChain)
			}
			requestedHeight := root.GetL2BlockNumber()
			msg, committedHeight, err := headerBuilder.BuildHeader(ctx, l2rollup.HeaderRequest{Height: requestedHeight})
			if err != nil {
				return services.L2TimeoutUpdate{}, fmt.Errorf("attested L2 header at %d: %w", requestedHeight, err)
			}
			if committedHeight != requestedHeight {
				return services.L2TimeoutUpdate{}, fmt.Errorf("attested L2 header committed height %d, want %d", committedHeight, requestedHeight)
			}
			payload, err := msg.EncodeClientMessage()
			if err != nil {
				return services.L2TimeoutUpdate{}, fmt.Errorf("encode attested L2 header at %d: %w", committedHeight, err)
			}
			trusted, ok := msg.(interface{ TrustedAt() time.Time })
			if !ok {
				return services.L2TimeoutUpdate{}, fmt.Errorf("attested L2 header at %d does not report its timestamp", committedHeight)
			}
			timestamp := trusted.TrustedAt().Unix()
			if timestamp <= 0 {
				return services.L2TimeoutUpdate{}, fmt.Errorf("attested L2 header at %d has invalid timestamp %d", committedHeight, timestamp)
			}
			return services.L2TimeoutUpdate{Payload: payload, Height: committedHeight, Timestamp: uint64(timestamp)}, nil
		},
		BuildNonMembershipProof: func(ctx context.Context, packet channeltypesv2.Packet, height uint64) ([]byte, error) {
			return l2rollup.NonMembershipProof(ctx, l2, router, packet, height)
		},
	}
}

// resolveIncludeProvisional applies the documented default while retaining the
// explicit override. A finalized replica head and the attestor's provisional
// frontier are independent policies: an override changes the latter only.
func resolveIncludeProvisional(headKind l2rollup.HeadKind, override *bool) (bool, string) {
	includeProvisional := headKind != l2rollup.Finalized
	if override != nil {
		includeProvisional = *override
	}
	if headKind == l2rollup.Finalized && includeProvisional {
		return true, "l2->cosmos finalized head_kind with include_provisional=true accepts provisional attestor roots for relayability; state-root verification remains finalized"
	}
	return includeProvisional, ""
}

// settleL2OwedAck closes an owed-acknowledgement record from a terminal L2
// AckPacket, whoever relayed it.
func settleL2OwedAck(svc *services.Services) func(raw []byte) {
	return func(raw []byte) {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [AckWatch] settle owed ack from terminal L2 event: decode packet: %v", err)
			return
		}
		svc.ClearAckDue(pkt)
	}
}

func l2PendingTrackerHooks(svc *services.Services) (relay.TrackFunc, relay.UntrackFunc) {
	trackL2Pending := func(raw []byte, height uint64) bool {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [l2->cosmos Relay] track pending: decode packet: %v", err)
			return false
		}
		return svc.TrackL2Pending(pkt, height)
	}
	untrackL2Pending := func(raw []byte) {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("ERROR [l2->cosmos Relay] untrack pending: decode packet: %v", err)
			return
		}
		svc.UntrackL2Pending(pkt)
	}
	return trackL2Pending, untrackL2Pending
}

// runL2Engine drives one L2->Cosmos module until ctx is cancelled. A cancelled
// context is a clean shutdown, not a relay failure.
func runL2Engine(ctx context.Context, module *relay.Module, reg *status.Registry) error {
	reg.AddPath(module.Status)
	return module.Run(ctx)
}
