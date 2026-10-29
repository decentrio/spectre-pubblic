package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"relayer/chain"
	"relayer/chain/ethnative"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/subscriber"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
)

// Destination is the Cosmos implementation of chain.Destination for the ETH->Cosmos
// direction: it hosts the Ethereum beacon light client, a native module of the chain
// (ibc.lightclients.spectre.v1, see chain/ethnative), and is where the beacon update
// + ETH-origin packets are submitted. It holds the worker, Cosmos
// endpoint, and Ethereum client ID needed by that path; it is constructed by the
// wiring, not the cfg-only registry.
type Destination struct {
	worker   *services.Worker
	cosmos   services.CosmosEndpoint
	clientID string
}

type atomicCosmosTxHandler interface {
	SendCosmosTxBatchAtomic(context.Context, services.CosmosEndpoint, []any) error
}

// NewDestination wires the Cosmos destination to the worker and Cosmos endpoint.
func NewDestination(worker *services.Worker, cosmos services.CosmosEndpoint, clientID string) *Destination {
	return &Destination{worker: worker, cosmos: cosmos, clientID: clientID}
}

func (d *Destination) Chain() chain.ChainType { return chain.Cosmos }

func (d *Destination) SupportsUpdatePacketFolding() bool {
	if d.worker == nil || d.worker.TxHandler == nil {
		return false
	}
	_, ok := d.worker.TxHandler.(atomicCosmosTxHandler)
	return ok
}

// UpdateClient converts each beacon JSON header to the protobuf Header of the native
// light client, waits for the Cosmos node to reach the latest signature slot, then
// submits the ordered MsgUpdateClient batch. This preserves the legacy pre-submit timing guard
// without carrying relayer-only metadata inside the payload.
func (d *Destination) UpdateClient(ctx context.Context, clientID string, update chain.ClientUpdate) error {
	if err := ctx.Err(); err != nil {
		return err // shutting down — do not start a client-update tx
	}
	if len(update.Payloads) == 0 {
		return nil // client already current
	}
	if clientID == "" {
		clientID = d.clientID
	}
	signer, err := d.worker.TxHandler.CosmosSignerAddress()
	if err != nil {
		return fmt.Errorf("cosmos dest: signer address: %w", err)
	}
	sigSlot, err := highestBeaconSignatureSlot(update.Payloads)
	if err != nil {
		return fmt.Errorf("cosmos dest: decode beacon update: %w", err)
	}
	msgs, err := buildBeaconUpdateMessages(signer, clientID, update.Payloads)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ethClientState, err := relayerclient.GetEthereumClientState(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return fmt.Errorf("cosmos dest: eth client state: %w", err)
	}
	if err := d.worker.WaitForCosmosCatchUp(ctx, d.cosmos, ethClientState, sigSlot); err != nil {
		return fmt.Errorf("cosmos dest: wait for chain catch-up: %w", err)
	}
	if err := d.worker.TxHandler.SendCosmosTxBatch(ctx, d.cosmos, msgs); err != nil {
		return fmt.Errorf("cosmos dest: submit beacon update (exec block %d): %w", update.Height, err)
	}
	return nil
}

func buildBeaconUpdateMessages(signer, clientID string, payloads [][]byte) ([]any, error) {
	msgs := make([]any, 0, len(payloads))
	for i, payload := range payloads {
		msg, err := ethnative.BuildUpdateClient(signer, clientID, payload)
		if err != nil {
			return nil, fmt.Errorf("cosmos dest: wrap beacon update %d: %w", i, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// highestBeaconSignatureSlot reads the timing prerequisite from the same JSON
// payload that is converted into the header the light client verifies. The last header normally has the latest
// slot, but taking the maximum makes ordered period-crossing batches explicit.
func highestBeaconSignatureSlot(payloads [][]byte) (uint64, error) {
	var latest uint64
	for i, payload := range payloads {
		var header relayerclient.EthereumHeader
		if err := json.Unmarshal(payload, &header); err != nil {
			return 0, fmt.Errorf("header %d: unmarshal JSON: %w", i, err)
		}
		slot, err := strconv.ParseUint(header.ConsensusUpdate.SignatureSlot, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("header %d: parse signature slot: %w", i, err)
		}
		if slot > latest {
			latest = slot
		}
	}
	return latest, nil
}

// highestBeaconFinalizedSlot returns the destination consensus height installed
// by the ordered beacon-update payloads. Packet messages folded behind those
// updates must name this slot even though it is not on-chain yet.
func highestBeaconFinalizedSlot(payloads [][]byte) (uint64, error) {
	var latest uint64
	for i, payload := range payloads {
		var header relayerclient.EthereumHeader
		if err := json.Unmarshal(payload, &header); err != nil {
			return 0, fmt.Errorf("header %d: unmarshal JSON: %w", i, err)
		}
		slot, err := strconv.ParseUint(header.ConsensusUpdate.FinalizedHeader.Beacon.Slot, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("header %d: parse finalized slot: %w", i, err)
		}
		if slot > latest {
			latest = slot
		}
	}
	return latest, nil
}

// RelayPackets builds the Cosmos-bound IBC messages (recv/ack) from the given
// ETH-origin packets + storage proofs and submits them via SendCosmosTxBatch.
//
// The declared proof height is the on-chain client's latest slot, whose
// execution block the ETH source is expected to have built the proof against.
// That used to be an assumption resting on both sides reading the same client
// state; requireProofsBuiltAt now checks it, because the module's view of the
// destination client can lag the chain (another relayer, a manual update-client)
// and the resulting failure surfaces as an opaque trie error from inside the light client.
//
// ETH-origin timeouts are handled by the async scanner, not this path.
// buildPacketMessages emits exactly one message per packet, in order, and
// SendCosmosTxBatch commits sub-batches front to back, so the message count it
// reports as committed is also the packet prefix that was delivered.
func (d *Destination) RelayPackets(ctx context.Context, packets []chain.RelayPacket) (int, error) {
	if len(packets) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err // shutting down — do not start a packet tx
	}
	signerAddr, err := d.worker.TxHandler.CosmosSignerAddress()
	if err != nil {
		return 0, fmt.Errorf("cosmos dest: signer address: %w", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ethClientState, err := relayerclient.GetEthereumClientState(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return 0, fmt.Errorf("cosmos dest: eth client state: %w", err)
	}
	if err := requireProofsBuiltAt(packets, ethClientState.LatestExecutionBlockNumber); err != nil {
		return 0, err
	}
	proofHeight := clienttypes.Height{RevisionNumber: 0, RevisionHeight: ethClientState.LatestSlot}
	msgs, err := buildPacketMessages(signerAddr, proofHeight, packets)
	if err != nil {
		return 0, err
	}
	return d.sendPacketBatch(ctx, msgs)
}

// RelayWithUpdate submits the ordered beacon updates and all packet messages in
// one Cosmos transaction. Packet proof heights use the finalized slot installed
// by the update rather than querying the still-stale on-chain client state.
func (d *Destination) RelayWithUpdate(ctx context.Context, clientID string, update chain.ClientUpdate, packets []chain.RelayPacket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(update.Payloads) == 0 {
		return fmt.Errorf("cosmos dest: folded relay requires a client update payload")
	}
	if d.worker == nil || d.worker.TxHandler == nil {
		return fmt.Errorf("cosmos dest: transaction handler is not configured")
	}
	atomicSender, ok := d.worker.TxHandler.(atomicCosmosTxHandler)
	if !ok {
		return fmt.Errorf("cosmos dest: transaction handler does not support atomic update/packet batches")
	}
	if clientID == "" {
		clientID = d.clientID
	}
	signer, err := d.worker.TxHandler.CosmosSignerAddress()
	if err != nil {
		return fmt.Errorf("cosmos dest: signer address: %w", err)
	}
	updateMsgs, err := buildBeaconUpdateMessages(signer, clientID, update.Payloads)
	if err != nil {
		return err
	}
	sigSlot, err := highestBeaconSignatureSlot(update.Payloads)
	if err != nil {
		return fmt.Errorf("cosmos dest: decode beacon update: %w", err)
	}
	finalizedSlot, err := highestBeaconFinalizedSlot(update.Payloads)
	if err != nil {
		return fmt.Errorf("cosmos dest: decode beacon update: %w", err)
	}
	// update.Height and finalizedSlot are the execution block and the beacon slot of
	// the SAME finalized header (chain/evm.BeaconBuilder), so a packet proven at
	// update.Height verifies against the consensus state this update installs.
	if err := requireProofsBuiltAt(packets, update.Height); err != nil {
		return err
	}
	packetMsgs, err := buildPacketMessages(signer, clienttypes.Height{RevisionNumber: 0, RevisionHeight: finalizedSlot}, packets)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ethClientState, err := relayerclient.GetEthereumClientState(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return fmt.Errorf("cosmos dest: eth client state: %w", err)
	}
	if err := d.worker.WaitForCosmosCatchUp(ctx, d.cosmos, ethClientState, sigSlot); err != nil {
		return fmt.Errorf("cosmos dest: wait for chain catch-up: %w", err)
	}

	msgs := make([]any, 0, len(updateMsgs)+len(packetMsgs))
	msgs = append(msgs, updateMsgs...)
	msgs = append(msgs, packetMsgs...)
	if err := atomicSender.SendCosmosTxBatchAtomic(ctx, d.cosmos, msgs); err != nil {
		if errors.Is(err, services.ErrPermanentRelayFailure) {
			return chain.Permanent(err)
		}
		return err
	}
	return nil
}

// requireProofsBuiltAt fails when the proofs were built against a different
// execution block than the consensus height the messages are about to declare.
//
// The two are chosen in different places -- the source builds the proof at the
// height the module hands it, the destination names the consensus height it is
// installing or already trusts -- so nothing but this check ties them together.
// When they drift apart the packet still submits, and the light client rejects it
// with "get trie node failed: Invalid state root": a message about the trie, from
// inside the light client, that names neither height. Failing here instead names both.
//
// Transient on purpose: the module re-queues, and the next flush rebuilds the
// proof against a client state it has re-read.
func requireProofsBuiltAt(packets []chain.RelayPacket, execBlock uint64) error {
	for _, rp := range packets {
		if rp.Height != execBlock {
			return fmt.Errorf(
				"cosmos dest: proof for seq=%d was built at execution block %d but this batch declares the consensus height at block %d; not submitting a proof that cannot verify",
				rp.Sequence, rp.Height, execBlock)
		}
	}
	return nil
}

func buildPacketMessages(signer string, proofHeight clienttypes.Height, packets []chain.RelayPacket) ([]any, error) {
	msgs := make([]any, 0, len(packets))
	for _, rp := range packets {
		var pkt channeltypesv2.Packet
		if err := pkt.Unmarshal(rp.Packet); err != nil {
			// A malformed packet is deterministic — it will never decode on retry.
			return nil, chain.Permanent(fmt.Errorf("cosmos dest: decode packet: %w", err))
		}
		// The source hands over the relayer's JSON storage proof; the light client
		// verifies the protobuf one. The conversion is deterministic, so a proof that
		// does not convert will never convert on retry either.
		proof, err := ethnative.Proof(rp.Proof)
		if err != nil {
			return nil, chain.Permanent(fmt.Errorf("cosmos dest: convert proof (seq=%d): %w", pkt.Sequence, err))
		}
		switch rp.Type {
		case chain.SendPacket:
			msgs = append(msgs, &channeltypesv2.MsgRecvPacket{
				Packet:          pkt,
				ProofCommitment: proof,
				ProofHeight:     proofHeight,
				Signer:          signer,
			})
		case chain.AckPacket:
			msgs = append(msgs, &channeltypesv2.MsgAcknowledgement{
				Packet:          pkt,
				Acknowledgement: channeltypesv2.Acknowledgement{AppAcknowledgements: rp.AckBytes},
				ProofAcked:      proof,
				ProofHeight:     proofHeight,
				Signer:          signer,
			})
		default:
			return nil, chain.Permanent(fmt.Errorf("cosmos dest: unsupported packet type %d (seq=%d)", rp.Type, pkt.Sequence))
		}
	}
	return msgs, nil
}

// sendPacketBatch returns how many of msgs are on chain. A split batch can
// commit its leading sub-batches and then fail, and SendCosmosTxBatch reports
// that count in BatchPartialError; dropping it here is what made the caller
// re-queue -- and re-submit -- packets that were already delivered.
func (d *Destination) sendPacketBatch(ctx context.Context, msgs []any) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	// A deterministic Cosmos DeliverTx revert cannot succeed on retry — surface it
	// as chain.Permanent so the module DROPS the batch rather than re-queueing it
	// forever (each retry re-runs the beacon client update and drains gas). CheckTx/
	// broadcast/RPC errors stay transient and are re-queued.
	if err := d.worker.TxHandler.SendCosmosTxBatch(ctx, d.cosmos, msgs); err != nil {
		committed := services.CommittedBeforeFailure(err)
		if errors.Is(err, services.ErrPermanentRelayFailure) {
			return committed, chain.Permanent(err)
		}
		return committed, err
	}
	return len(msgs), nil
}

// HasPacketReceipt reports whether an ETH-origin packet was already delivered on
// Cosmos, via the same receipt ABCI query the ETH recovery path and the L2
// destination use. It used to be a "not wired" stub because nothing in the
// module read it; the enumeration backstop does (dropSettled), so an answer
// that is always an error would have relayed every enumerated receive twice.
func (d *Destination) HasPacketReceipt(ctx context.Context, packet []byte) (bool, error) {
	if d.cosmos.CosmosClient() == nil {
		return false, fmt.Errorf("cosmos dest: no Cosmos client to query receipts on")
	}
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return false, fmt.Errorf("cosmos dest: decode packet: %w", err)
	}
	return subscriber.HasCosmosPacketReceipt(ctx, d.cosmos, pkt)
}

// ClientExpiresAt returns a conservative expiry for the ETH light client on Cosmos. Unlike a Tendermint client there is no single trusting period; the
// client must be advanced within ~2 sync-committee periods of its latest tracked
// slot. We report one period after that slot's time — deliberately early so the
// refresh routine never lets it lapse (a wrong-too-late value would expire the
// client, the failure class we care about most).
func (d *Destination) ClientExpiresAt(ctx context.Context, _ string) (time.Time, time.Duration, error) {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cs, err := relayerclient.GetEthereumClientState(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("cosmos dest: eth client state: %w", err)
	}
	expiresAt, period := ethClientExpiry(cs)
	return expiresAt, period, nil
}

// ethClientExpiry derives the conservative expiry of the Ethereum light client
// from its on-chain state.
//
// A beacon client has no single trusting period the way a Tendermint client does:
// it must be advanced within about two sync-committee periods of its latest
// tracked slot. This reports ONE period after that slot's time, which is
// deliberately early -- the refresh routine then acts sooner than strictly
// necessary, and being wrong in the other direction lets the client lapse, which
// is the failure that costs the most.
//
// A zero period means the client state is misconfigured (any of the three factors
// unset). Reporting the zero time rather than an error is what the module reads as
// "no expiry", so it falls back to the periodic refresh instead of treating a
// malformed client state as an expiry emergency.
func ethClientExpiry(cs *relayerclient.EthereumClientState) (time.Time, time.Duration) {
	periodSecs := cs.EpochsPerSyncCommitteePeriod * cs.SlotsPerEpoch * cs.SecondsPerSlot
	if periodSecs == 0 {
		return time.Time{}, 0
	}
	// One sync-committee period stands in for the trusting period here: it is the
	// window the client must be advanced within, and it is what the expiry above
	// is measured from.
	return time.Unix(int64(cs.ComputeTimestampAtSlot(cs.LatestSlot)+periodSecs), 0),
		time.Duration(periodSecs) * time.Second
}

// ClientActive asks ibc-go for the client's status. Anything but Active --
// Frozen or Expired -- is a client that rejects every packet message.
var _ chain.ClientStatusChecker = (*Destination)(nil)

func (d *Destination) ClientActive(ctx context.Context) (bool, string, error) {
	if d.cosmos.CosmosClient() == nil {
		return false, "", fmt.Errorf("cosmos dest: no Cosmos client to query client status on")
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, err := relayerclient.GetClientStatus(readCtx, d.cosmos.CosmosClient(), d.clientID)
	if err != nil {
		return false, "", fmt.Errorf("cosmos dest: client status: %w", err)
	}
	return status == ibcexported.Active.String(), status, nil
}
