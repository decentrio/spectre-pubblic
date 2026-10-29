// Package l2rollup holds the relayer adapters for the L2->Cosmos path (Arbitrum /
// OP-Stack). The Cosmos-side wasm client authenticates a canonical L2 block identity
// with its pinned Ed25519 threshold set, then verifies the router account proof
// against that authenticated state root. This Destination mirrors the existing
// beacon (ETH->Cosmos) Cosmos destination almost exactly:
//
//   - UpdateClient submits MsgUpdateClient wrapping a wasm ClientMessage whose Data
//     is the signed L2 header (the builder produces it). No ETH-first ordering step
//     is needed: the attestor-trusted L2 client has no Ethereum-client dependency.
//   - RelayPackets submits the standard channeltypesv2 recv / ack / timeout
//     messages, which the client maps to VerifyMembership / VerifyNonMembership.
//     The L2 client does not decode packets.
package l2rollup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"relayer/chain"
	"relayer/chain/wasmclient"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/subscriber"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
)

// Destination satisfies chain.Destination, and the folding half of it: the relay
// engine folds automatically once an adapter advertises the capability.
var _ chain.Destination = (*Destination)(nil)
var _ chain.FoldingDestination = (*Destination)(nil)
var _ chain.ClientStatusChecker = (*Destination)(nil)

// atomicCosmosTxHandler is the submission guarantee folding needs: one
// transaction, no recursive splitting. A split batch would land the update
// without its packets, or land some packets against a client the same failed
// batch never advanced.
type atomicCosmosTxHandler interface {
	SendCosmosTxBatchAtomic(context.Context, services.CosmosEndpoint, []any) error
}

// isPermanentCosmosFailure reports whether a Cosmos submit error is a
// deterministic on-chain revert (drop) vs a transient one (retry) — same
// classification the Cosmos beacon destination uses.
func isPermanentCosmosFailure(err error) bool {
	return errors.Is(err, services.ErrPermanentRelayFailure)
}

// Destination is the Cosmos side of the L2->Cosmos path: it hosts the L2 wasm
// light client and is where the L2 client update + L2-origin packets are
// submitted. Chain() is Cosmos (this is the destination chain).
type Destination struct {
	worker   *services.Worker
	cosmos   services.CosmosEndpoint
	clientID string // the L2 wasm light-client id on Cosmos
}

// NewDestination wires the L2 Cosmos destination. clientID is the L2 wasm client.
func NewDestination(worker *services.Worker, cosmos services.CosmosEndpoint, clientID string) *Destination {
	return &Destination{worker: worker, cosmos: cosmos, clientID: clientID}
}

func (d *Destination) Chain() chain.ChainType { return chain.Cosmos }

// UpdateClient submits MsgUpdateClient for the L2 wasm client, wrapping the L2
// header (the sole update payload, produced by the L2 builder) in a wasm
// ClientMessage.
//
// No ETH-first ordering step is needed: the header builder assembles its L1 proofs
// against the ETH client's already-trusted L1 block (EthClientLatestSlotAndBlock), so
// every L2 update verifies against the ETH state the client already holds. The ETH
// client's freshness bounds how recent an L2 update can be, but advancing it is not a
// prerequisite for this submit to verify.
func (d *Destination) UpdateClient(ctx context.Context, _ string, update chain.ClientUpdate) error {
	if err := ctx.Err(); err != nil {
		return err // shutting down — do not start a client-update tx
	}
	if len(update.Payloads) != 1 {
		return fmt.Errorf("l2 dest: expected exactly one client update payload, got %d", len(update.Payloads))
	}
	signer, err := d.worker.TxHandler.CosmosSignerAddress()
	if err != nil {
		return fmt.Errorf("l2 dest: signer address: %w", err)
	}
	msg, err := wasmclient.BuildUpdateClient(signer, d.clientID, update.Payloads[0])
	if err != nil {
		return fmt.Errorf("l2 dest: build update (height %d): %w", update.Height, err)
	}
	if err := d.worker.TxHandler.SendCosmosTxBatch(ctx, d.cosmos, []any{msg}); err != nil {
		return fmt.Errorf("l2 dest: submit update (height %d): %w", update.Height, err)
	}
	return nil
}

// RelayPackets builds the standard channeltypesv2 messages (recv / ack / timeout)
// from the L2-origin packets + storage proofs and submits them. Same wrappers as
// the existing Cosmos beacon destination; only the proof height differs (the L2
// client's latest tracked height instead of the beacon slot).
//
// One message per packet, in slice order, and SendCosmosTxBatch commits
// sub-batches front to back -- so the count it reports is the packet prefix that
// was delivered. This destination submits to Cosmos, so unlike the EVM one it
// really can stop half way.
func (d *Destination) RelayPackets(ctx context.Context, packets []chain.RelayPacket) (int, error) {
	if len(packets) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err // shutting down — do not start a packet tx
	}
	signer, err := d.worker.TxHandler.CosmosSignerAddress()
	if err != nil {
		return 0, fmt.Errorf("l2 dest: signer address: %w", err)
	}
	proofHeight, err := d.proofHeight(ctx)
	if err != nil {
		return 0, err
	}

	msgs, err := buildPacketMessages(signer, proofHeight, packets)
	if err != nil {
		return 0, err
	}
	if err := d.worker.TxHandler.SendCosmosTxBatch(ctx, d.cosmos, msgs); err != nil {
		committed := services.CommittedBeforeFailure(err)
		if isPermanentCosmosFailure(err) {
			return committed, chain.Permanent(err)
		}
		return committed, err
	}
	return len(packets), nil
}

// buildPacketMessages turns proven packets into the Cosmos messages that carry
// them. Shared by the folded and unfolded paths so the two cannot disagree about
// what a packet message looks like -- which is how the ETH path's folded batches
// came to declare a height their proofs were not built at.
func buildPacketMessages(signer string, proofHeight clienttypes.Height, packets []chain.RelayPacket) ([]any, error) {
	msgs := make([]any, 0, len(packets))
	for _, rp := range packets {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(rp.Packet); err != nil {
			// A malformed packet is deterministic; it will not decode on retry.
			return nil, chain.Permanent(fmt.Errorf("l2 dest: decode packet: %w", err))
		}
		switch rp.Type {
		case chain.SendPacket:
			msgs = append(msgs, &channeltypesv2.MsgRecvPacket{
				Packet:          pkt,
				ProofCommitment: rp.Proof,
				ProofHeight:     proofHeight,
				Signer:          signer,
			})
		case chain.AckPacket:
			msgs = append(msgs, &channeltypesv2.MsgAcknowledgement{
				Packet:          pkt,
				Acknowledgement: channeltypesv2.Acknowledgement{AppAcknowledgements: rp.AckBytes},
				ProofAcked:      rp.Proof,
				ProofHeight:     proofHeight,
				Signer:          signer,
			})
		case chain.TimeoutPacket:
			msgs = append(msgs, &channeltypesv2.MsgTimeout{
				Packet:          pkt,
				ProofUnreceived: rp.Proof,
				ProofHeight:     proofHeight,
				Signer:          signer,
			})
		default:
			return nil, chain.Permanent(fmt.Errorf("l2 dest: unsupported packet type %d (seq=%d)", rp.Type, pkt.Sequence))
		}
	}
	return msgs, nil
}

// requireProofsBuiltAt refuses a batch whose proofs were built against a height
// this submission does not install.
//
// This is the expensive half of folding, not the folding itself. Unfolded, the
// proof height is READ from the on-chain client, so it is true by construction.
// Folded, the update is not on chain yet: the client still reports the old
// height while the packet messages declare the one the update is about to
// install, and the two numbers are now chosen in different places with nothing
// tying them together.
//
// The ETH->Cosmos path shipped exactly that mistake and it cost a night (#443).
// It surfaced as "get trie node failed: Invalid state root" from inside CosmWasm,
// naming no height at all -- and because the atomic batch refuses to split, the
// client update died with the packets, turning "a packet is stuck" into "nothing
// advances".
func requireProofsBuiltAt(packets []chain.RelayPacket, height uint64) error {
	for _, rp := range packets {
		if rp.Height != height {
			return fmt.Errorf(
				"l2 dest: proof for seq=%d was built at L2 height %d but this batch installs height %d; not submitting a proof that cannot verify",
				rp.Sequence, rp.Height, height)
		}
	}
	return nil
}

// SupportsUpdatePacketFolding reports whether the transaction handler can submit
// the update and its packets as ONE Cosmos transaction. Without that guarantee a
// split would land the client update and lose the packets, or the reverse.
func (d *Destination) SupportsUpdatePacketFolding() bool {
	if d.worker == nil || d.worker.TxHandler == nil {
		return false
	}
	_, ok := d.worker.TxHandler.(atomicCosmosTxHandler)
	return ok
}

// RelayWithUpdate submits the L2 header update and every packet message in one
// Cosmos transaction.
//
// This path is why the L2 leg cost TWO transactions per batch while the other
// three cost one -- on the leg with the highest latency of the four. Packet
// proofs target the height this update installs rather than the height the
// on-chain client still reports, which is only sound because requireProofsBuiltAt
// checks it first.
func (d *Destination) RelayWithUpdate(ctx context.Context, _ string, update chain.ClientUpdate, packets []chain.RelayPacket) error {
	if err := ctx.Err(); err != nil {
		return err // shutting down — do not start a folded tx
	}
	if len(update.Payloads) != 1 {
		return fmt.Errorf("l2 dest: folded relay expects exactly one client update payload, got %d", len(update.Payloads))
	}
	if d.worker == nil || d.worker.TxHandler == nil {
		return fmt.Errorf("l2 dest: transaction handler is not configured")
	}
	atomicSender, ok := d.worker.TxHandler.(atomicCosmosTxHandler)
	if !ok {
		return fmt.Errorf("l2 dest: transaction handler does not support atomic update/packet batches")
	}
	signer, err := d.worker.TxHandler.CosmosSignerAddress()
	if err != nil {
		return fmt.Errorf("l2 dest: signer address: %w", err)
	}

	// Before building anything: a mismatch here is not worth a transaction, and
	// the chain's own complaint about it names no height.
	if err := requireProofsBuiltAt(packets, update.Height); err != nil {
		return err
	}

	// The module's client-id argument names the source client on the L2 router.
	// This Cosmos destination must always update its own Wasm client instead.
	// Passing the source id here produces a validly encoded MsgUpdateClient that
	// Cosmos rejects because no such client is registered in its light-client
	// router.
	updateMsg, err := wasmclient.BuildUpdateClient(signer, d.clientID, update.Payloads[0])
	if err != nil {
		return err
	}
	packetMsgs, err := buildPacketMessages(
		signer, clienttypes.Height{RevisionNumber: 0, RevisionHeight: update.Height}, packets)
	if err != nil {
		return err
	}

	// Update first: the packet proofs verify against the state it installs, and
	// SendCosmosTxBatchAtomic preserves order.
	msgs := make([]any, 0, 1+len(packetMsgs))
	msgs = append(msgs, updateMsg)
	msgs = append(msgs, packetMsgs...)

	if err := atomicSender.SendCosmosTxBatchAtomic(ctx, d.cosmos, msgs); err != nil {
		if isPermanentCosmosFailure(err) {
			return chain.Permanent(err)
		}
		return err
	}
	return nil
}

// proofHeight is the L2 client's latest tracked height — the height the packet
// proofs must be verified against. The wasm ClientState carries LatestHeight
// directly; for the L2 client its revision height is the L2 block number.
func (d *Destination) proofHeight(ctx context.Context) (clienttypes.Height, error) {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	h, err := relayerclient.GetWasmClientLatestHeight(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return clienttypes.Height{}, fmt.Errorf("l2 dest: read L2 client latest height: %w", err)
	}
	return h, nil
}

// HasPacketReceipt reports whether an L2-origin packet was already delivered on
// Cosmos, via the same Cosmos receipt ABCI query the ETH recovery path uses.
func (d *Destination) HasPacketReceipt(ctx context.Context, packet []byte) (bool, error) {
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return false, fmt.Errorf("l2 dest: decode packet: %w", err)
	}
	return subscriber.HasCosmosPacketReceipt(ctx, d.cosmos, pkt)
}

// ClientExpiresAt reports when the L2 wasm client would expire on its own timer.
// Unlike a Tendermint client, an optimistic L2 client has NO independent trusting
// period: it advances per-packet via header updates and its trust roots in the
// SHARED L1 (Ethereum) client, whose freshness the ETH path refreshes. So the L2
// client has no self-expiry timer — return a far-future time so the anti-expiry
// refresh routine never force-updates it (its freshness is the L1 client's).
func (d *Destination) ClientExpiresAt(_ context.Context, _ string) (time.Time, time.Duration, error) {
	// No self-expiry, so no trusting period to size a margin against.
	return time.Now().Add(100 * 365 * 24 * time.Hour), 0, nil
}

// ClientActive asks ibc-go for the client's status. Anything but Active --
// Frozen or Expired -- is a client that rejects every packet message.
func (d *Destination) ClientActive(ctx context.Context) (bool, string, error) {
	if d.cosmos.CosmosClient() == nil {
		return false, "", fmt.Errorf("l2 dest: no Cosmos client to query client status on")
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, err := relayerclient.GetClientStatus(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return false, "", fmt.Errorf("l2 dest: client status: %w", err)
	}
	return status == ibcexported.Active.String(), status, nil
}
