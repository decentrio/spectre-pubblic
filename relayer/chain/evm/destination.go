// Package evm adapts an EVM chain (Ethereum L1, and the L2s Arbitrum/Optimism/
// Base) as a chain.Destination — where a Cosmos-sourced SpectreClient update is
// submitted. It wraps the existing services/transaction code.
package evm

import (
	"context"
	"errors"
	"fmt"
	"time"

	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/chain"
	"relayer/chain/codec"
	"relayer/services"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// Destination is the EVM implementation of chain.Destination for a chain that
// hosts a SpectreClient (ZK Tendermint) light client of Cosmos. It holds the
// shared worker plus the EVM/Cosmos endpoints needed by transaction submission
// and expiry reads; it is constructed by the wiring, not the cfg-only registry.
type Destination struct {
	worker   *services.Worker
	cosmos   services.CosmosEndpoint
	evm      services.EVMEndpoint
	clientID string
}

// NewDestination wires the EVM destination to the worker and its EVM endpoint.
func NewDestination(worker *services.Worker, cosmos services.CosmosEndpoint, evm services.EVMEndpoint, clientID string) *Destination {
	return &Destination{worker: worker, cosmos: cosmos, evm: evm, clientID: clientID}
}

func (d *Destination) Chain() chain.ChainType { return chain.Ethereum }

// SupportsUpdatePacketFolding reports whether the ICS26Router is also the proof
// submitter. Only that deployment shape can express the client update as an
// inner router call in the same multicall as the packets.
func (d *Destination) SupportsUpdatePacketFolding() bool {
	return d.evm.RouterManagesProofSubmission()
}

// UpdateClient decodes the Cosmos->ETH update payload back into the typed message
// and submits it. SendEthTx routes updateApplicationState vs updateConsensusState
// off the result's Kind, exactly as the legacy RefreshCosmosClient path did.
func (d *Destination) UpdateClient(ctx context.Context, _ string, update chain.ClientUpdate) error {
	if err := ctx.Err(); err != nil {
		return err // shutting down — do not start a client-update tx
	}
	result, err := decodeClientUpdate(update)
	if err != nil {
		return err
	}
	if err := d.worker.TxHandler.SendEthTx(ctx, d.evm, d.clientID, result); err != nil {
		return fmt.Errorf("evm: submit client update (height %d): %w", update.Height, err)
	}
	return nil
}

func decodeClientUpdate(update chain.ClientUpdate) (services.CosmosClientUpdateBuildResult, error) {
	if len(update.Payloads) != 1 {
		return services.CosmosClientUpdateBuildResult{}, fmt.Errorf("evm: expected exactly one client update payload, got %d", len(update.Payloads))
	}
	kind, appMsg, newValSet, err := codec.DecodeCosmosUpdate(update.Payloads[0])
	if err != nil {
		return services.CosmosClientUpdateBuildResult{}, fmt.Errorf("evm: decode client update: %w", err)
	}
	return services.CosmosClientUpdateBuildResult{
		Kind:      services.ClientUpdateKind(kind),
		AppMsg:    appMsg,
		NewValSet: newValSet,
		HasMsg:    true,
	}, nil
}

// RelayPackets builds the concrete ICS26 messages (recv/ack/timeout) from the
// given source packets + injected proofs and submits them via SendEthTxBatch.
// The proofs are already built by the source (Membership/NonMembershipProof), so
// this does no proof generation.
// Every packet rides in one ICS26Router multicall, so this destination has no
// partial state to report: committed is len(packets) or zero, never in between.
func (d *Destination) RelayPackets(ctx context.Context, packets []chain.RelayPacket) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err // shutting down — do not start a packet tx
	}
	msgs, err := buildPacketMessages(packets)
	if err != nil {
		return 0, err
	}
	if err := d.sendPacketBatch(ctx, msgs); err != nil {
		return 0, err
	}
	return len(packets), nil
}

// RelayWithUpdate prepends the decoded Cosmos-client update to the packet calls
// and submits one ICS26Router multicall. SendEthTxBatch preserves message order,
// so packet proofs are checked only after the update has installed their target
// consensus state.
func (d *Destination) RelayWithUpdate(ctx context.Context, _ string, update chain.ClientUpdate, packets []chain.RelayPacket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !d.SupportsUpdatePacketFolding() {
		return fmt.Errorf("evm: update/packet folding requires ICS26Router to be the proof submitter")
	}
	result, err := decodeClientUpdate(update)
	if err != nil {
		return err
	}
	packetMsgs, err := buildPacketMessages(packets)
	if err != nil {
		return err
	}
	msgs := make([]any, 0, 1+len(packetMsgs))
	msgs = append(msgs, result)
	msgs = append(msgs, packetMsgs...)
	return d.sendPacketBatch(ctx, msgs)
}

func buildPacketMessages(packets []chain.RelayPacket) ([]any, error) {
	msgs := make([]any, 0, len(packets))
	for _, rp := range packets {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(rp.Packet); err != nil {
			// A malformed packet is deterministic — it will never decode on retry.
			return nil, chain.Permanent(fmt.Errorf("evm: decode packet: %w", err))
		}
		ethPkt := services.ToEthPacket(pkt)
		switch rp.Type {
		case chain.SendPacket:
			msgs = append(msgs, contractICS26Router.IICS26RouterMsgsMsgRecvPacket{
				Packet:        ethPkt,
				MembershipMsg: rp.Proof,
			})
		case chain.AckPacket:
			if len(rp.AckBytes) == 0 {
				return nil, chain.Permanent(fmt.Errorf("evm: ack packet seq=%d missing acknowledgement bytes", pkt.Sequence))
			}
			msgs = append(msgs, contractICS26Router.IICS26RouterMsgsMsgAckPacket{
				Packet:          ethPkt,
				Acknowledgement: rp.AckBytes[0], // ETH MsgAckPacket takes a single ack
				MembershipMsg:   rp.Proof,
			})
		case chain.TimeoutPacket:
			msgs = append(msgs, contractICS26Router.IICS26RouterMsgsMsgTimeoutPacket{
				Packet:           ethPkt,
				NonMembershipMsg: rp.Proof,
			})
		default:
			return nil, chain.Permanent(fmt.Errorf("evm: unknown packet type %d (seq=%d)", rp.Type, pkt.Sequence))
		}
	}
	return msgs, nil
}

func (d *Destination) sendPacketBatch(ctx context.Context, msgs []any) error {
	if len(msgs) == 0 {
		return nil
	}
	// A deterministic on-chain revert (status=0) cannot succeed on retry — surface
	// it as chain.Permanent so the module DROPS the batch instead of re-queueing it
	// forever (each retry re-runs the client update and drains gas). Everything else
	// (nonce, RPC, broadcast) stays transient and is re-queued.
	if err := d.worker.TxHandler.SendEthTxBatch(ctx, d.evm, d.clientID, msgs); err != nil {
		if errors.Is(err, services.ErrPermanentRelayFailure) {
			return chain.Permanent(err)
		}
		return err
	}
	return nil
}

// HasPacketReceipt reports whether a packet was already delivered on ETH. The
// packet is the proto-marshaled channeltypesv2.Packet (the convention shared
// with Event.Raw and RelayPackets).
func (d *Destination) HasPacketReceipt(ctx context.Context, packet []byte) (bool, error) {
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return false, fmt.Errorf("evm: decode packet: %w", err)
	}
	return services.HasEthPacketReceipt(ctx, d.evm, pkt)
}

// ClientActive reports whether the SpectreClient-of-Cosmos that the router
// routes this client id to accepts messages: false once it is frozen, and false
// when a migration has re-pointed the id away from the configured contract (see
// services.CosmosClientStatus).
var _ chain.ClientStatusChecker = (*Destination)(nil)

func (d *Destination) ClientActive(ctx context.Context) (bool, string, error) {
	status, err := services.CosmosClientStatus(ctx, d.evm, d.clientID)
	if err != nil {
		return false, "", fmt.Errorf("evm: client status: %w", err)
	}
	return status == "Active", status, nil
}

// ClientExpiresAt returns when the SpectreClient-of-Cosmos on this chain expires
// (trusted consensus timestamp + trusting period), so the RelayModule refresh
// routine can advance the client before it lapses.
func (d *Destination) ClientExpiresAt(ctx context.Context, _ string) (time.Time, time.Duration, error) {
	return services.CosmosClientExpiry(ctx, d.cosmos, d.evm)
}
