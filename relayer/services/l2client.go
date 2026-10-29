package services

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"attestor/types/attestation"
	relayerclient "relayer/client"

	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
)

// decodeHexPrefixed decodes a 0x-optional hex string into bytes.
func decodeHexPrefixed(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid hex %q: %w", s, err)
	}
	return b, nil
}

// L2ClientParams is the immutable configuration needed to bootstrap one L2 rollup
// wasm light client on Cosmos (Arbitrum / Base / Optimism). The bootstrap roots are
// read from the L2 chain (see relayerclient.GetL2BootstrapState); the rollup profile
// (which carries the L1 client id + checksum, l2 chain id, router address, commitment
// slot, and the rollup-specific verifier fields) is operator-supplied verbatim.
type L2ClientParams struct {
	// WasmChecksum is the hex checksum of the L2 client's own stored wasm code.
	WasmChecksum string
	// RollupProfile is the full ICS-08 L2 verifier Profile JSON (the `common` block
	// plus the rollup-specific finality fields), supplied verbatim from config and
	// embedded as ClientState.profile. Rollup-specific, so it is opaque here.
	RollupProfile json.RawMessage
	// Bootstrap is the trusted initial state read from the L2 chain.
	Bootstrap relayerclient.L2BootstrapState
	// CounterpartyClientID is the L2-side client (on the rollup's ICS26Router) that
	// tracks Cosmos, registered inline as this client's counterparty. Empty when the
	// L2-side client id is not yet known, in which case registration is deferred.
	CounterpartyClientID string
	// Attestors is the immutable, canonically ordered Ed25519 set that signs every
	// header update. It is part of ClientState, not relayer-only configuration.
	Attestors attestation.AttestorConfig
}

// The JSON shapes below mirror the ICS-08 CosmWasm L2 client types
// (packages/l2-client/src/state.rs). serde uses deny_unknown_fields, so field names
// must match exactly. instantiate takes the client state and consensus state
// DIRECTLY (no Bootstrap envelope):
//   - client_state Data = the complete ClientState<Profile>, including
//     finality and freshness policy
//   - consensus_state Data = the complete ConsensusState, including L2 block
//     identity and conservative Unsafe/Pending bootstrap metadata
//
// The B256 fields serialize as 0x-prefixed lowercase hex (alloy default);
// timestamp_nanos = timestamp_seconds * 1e9 (matches Header::consensus_state).

// l2ClientStateJSON mirrors l2-client `ClientState`. The policy fields are gone with
// the finality taxonomy: the client stores no levels. Each update instead carries
// signatures that bind the exact L2 block identity to this immutable attestor set.
type l2ClientStateJSON struct {
	LatestHeight uint64                     `json:"latest_height"`
	FrozenHeight *uint64                    `json:"frozen_height"`
	Profile      json.RawMessage            `json:"profile"`
	Attestors    attestation.AttestorConfig `json:"attestors"`
}

// l2ConsensusStateJSON mirrors l2-client `ConsensusState`. The settlement provenance
// (l1_origin_*, evidence_hash, rollup_commitment) and the finality taxonomy
// (finality_level, proposal_status, finality_reached_at) are gone: the client verifies
// no L1 evidence and records no attestation metadata, and carrying fields it does not
// read makes the wire contract overstate what is checked.
type l2ConsensusStateJSON struct {
	StateRoot      string `json:"state_root"`       // B256 0x-hex
	IBCStorageRoot string `json:"ibc_storage_root"` // B256 0x-hex
	TimestampNanos uint64 `json:"timestamp_nanos"`
	L2Height       uint64 `json:"l2_height"`
	L2BlockHash    string `json:"l2_block_hash"`
	ParentHash     string `json:"parent_hash"`
	// FirstAcceptedAt is replaced by the wasm instantiate entrypoint with Cosmos
	// block time; it is sent as zero.
	FirstAcceptedAt uint64 `json:"first_accepted_at"`
}

// BuildL2WasmClientState assembles the wasm ClientState + ConsensusState the L2
// MsgCreateClient submits: the client-state Data is ClientState<Profile> (the
// operator's verifier profile + the bootstrap height), the consensus-state Data is
// the bootstrap roots read from the L2 chain (ibc_storage_root = the router account
// storage root; timestamp in nanos).
func BuildL2WasmClientState(p L2ClientParams) (ibcexported.ClientState, ibcexported.ConsensusState, error) {
	checksumBz, err := decodeHexPrefixed(p.WasmChecksum)
	if err != nil {
		return nil, nil, fmt.Errorf("l2 client: decode wasm checksum: %w", err)
	}
	if len(p.RollupProfile) == 0 {
		return nil, nil, fmt.Errorf("l2 client: rollup profile is required")
	}
	if err := p.Attestors.Validate(); err != nil {
		return nil, nil, fmt.Errorf("l2 client: invalid attestor set: %w", err)
	}
	if p.Bootstrap.TimestampSeconds > ^uint64(0)/1_000_000_000 {
		return nil, nil, fmt.Errorf("l2 client: bootstrap timestamp overflows nanoseconds")
	}
	clientState := l2ClientStateJSON{
		LatestHeight: p.Bootstrap.Height,
		FrozenHeight: nil,
		Profile:      p.RollupProfile,
		Attestors:    p.Attestors,
	}
	clientStateBz, err := json.Marshal(clientState)
	if err != nil {
		return nil, nil, fmt.Errorf("l2 client: marshal client state: %w", err)
	}

	consensus := l2ConsensusStateJSON{
		StateRoot:       p.Bootstrap.StateRoot.Hex(),
		IBCStorageRoot:  p.Bootstrap.RouterStorageRoot.Hex(),
		TimestampNanos:  p.Bootstrap.TimestampSeconds * 1_000_000_000,
		L2Height:        p.Bootstrap.Height,
		L2BlockHash:     p.Bootstrap.BlockHash.Hex(),
		ParentHash:      p.Bootstrap.ParentHash.Hex(),
		FirstAcceptedAt: 0,
	}
	consensusBz, err := json.Marshal(consensus)
	if err != nil {
		return nil, nil, fmt.Errorf("l2 client: marshal consensus state: %w", err)
	}

	wasmClientState := &ibcwasmtypes.ClientState{
		Data:     clientStateBz,
		Checksum: checksumBz,
		LatestHeight: clienttypes.Height{
			RevisionNumber: 0,
			RevisionHeight: p.Bootstrap.Height,
		},
	}
	wasmConsensusState := &ibcwasmtypes.ConsensusState{Data: consensusBz}
	return wasmClientState, wasmConsensusState, nil
}

// CreateL2Client bootstraps one L2 rollup wasm light client on Cosmos and returns
// its auto-assigned client id. It goes through the shared generic wasm-create tx path
// (the same one CreateEthClient uses), passing p.CounterpartyClientID (the L2-side
// client that tracks Cosmos) so it is registered inline like the ETH beacon client.
// When that id is not yet configured it is empty, and counterparty registration is
// deferred to a later step (registering the ETH source's router id here would write a
// wrong, durable mapping).
func (w *Worker) CreateL2Client(stdCtx context.Context, endpoint CosmosEndpoint, p L2ClientParams) (string, error) {
	clientState, consensusState, err := BuildL2WasmClientState(p)
	if err != nil {
		return "", err
	}
	return w.TxHandler.CreateWasmClient(stdCtx, endpoint, clientState, consensusState, p.CounterpartyClientID)
}
