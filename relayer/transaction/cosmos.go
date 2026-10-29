// This file is the Cosmos half of the transaction package: every send goes
// through the account-sequence block under cosmosMu.
//
// See ethereum.go for why the two halves are separate files rather than sections
// of one, and why both deliberately stay over the line limit.
package transaction

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"relayer/chain"
	services "relayer/services"
	utils "relayer/utils"

	sdkmath "cosmossdk.io/math"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkbech32 "github.com/cosmos/cosmos-sdk/types/bech32"
	txservice "github.com/cosmos/cosmos-sdk/types/tx"
	sdksigning "github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/gogoproto/proto"
	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	clienttypesv2 "github.com/cosmos/ibc-go/v10/modules/core/02-client/v2/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	exported "github.com/cosmos/ibc-go/v10/modules/core/exported"
)

// CreateWasmClient creates an 08-wasm client (an L2 rollup client) from an already-built
// wasm ClientState/ConsensusState.
func (h *Handler) CreateWasmClient(stdCtx context.Context, endpoint services.CosmosEndpoint, clientState exported.ClientState, consensusState exported.ConsensusState, counterpartyClientID string) (string, error) {
	return h.createClient(stdCtx, endpoint, counterpartyClientID, func(signer string) (*clienttypes.MsgCreateClient, error) {
		return clienttypes.NewMsgCreateClient(clientState, consensusState, signer)
	})
}

// CreateClientFromAny creates a client from an already-packed client and consensus state. The
// native Ethereum light client uses it: its states are protobuf messages of a module the
// relayer does not link, so they cannot be passed as ibc-go exported.ClientState values.
func (h *Handler) CreateClientFromAny(stdCtx context.Context, endpoint services.CosmosEndpoint, clientState, consensusState *codectypes.Any, counterpartyClientID string) (string, error) {
	return h.createClient(stdCtx, endpoint, counterpartyClientID, func(signer string) (*clienttypes.MsgCreateClient, error) {
		return &clienttypes.MsgCreateClient{ClientState: clientState, ConsensusState: consensusState, Signer: signer}, nil
	})
}

// createClient submits MsgCreateClient, built by buildMsg for the signer, and registers the
// counterparty. See CreateWasmClient and CreateClientFromAny.
func (h *Handler) createClient(stdCtx context.Context, endpoint services.CosmosEndpoint, counterpartyClientID string, buildMsg func(signer string) (*clienttypes.MsgCreateClient, error)) (string, error) {
	log.Printf("[CreateEthClient] starting")
	// counterpartyClientID is the client on the counterparty chain that tracks Cosmos.
	// It is a config value known upfront (registration is only a naming binding in
	// ICS26Router — the counterparty light client need not exist yet), so each caller
	// passes its own: the ETH beacon client passes the ETH-side router client id, an
	// L2 bootstrap passes the L2-side client id. An empty value skips registration.

	privKeyBytes, err := h.keySigner().CosmosKeyBytes()
	if err != nil {
		return "", fmt.Errorf("failed to decode private key: %w", err)
	}

	privKey := secp256k1.PrivKey{Key: privKeyBytes}
	signerAddr, err := cosmosSignerBech32(privKey)
	if err != nil {
		return "", err
	}
	log.Printf("[CreateEthClient] signer: %s", signerAddr)

	// Get chain configuration from environment
	chainID := os.Getenv("COSMOS_CHAIN_ID")
	if chainID == "" {
		return "", fmt.Errorf("COSMOS_CHAIN_ID environment variable is required in .env file")
	}

	// Get gas and fee configuration
	// Default gas limit for MsgCreateClient with wasm payload.
	gasLimit, err := envUint64("COSMOS_GAS_LIMIT", 1500000)
	if err != nil {
		return "", err
	}

	feeDenom := os.Getenv("COSMOS_FEE_DENOM")
	if feeDenom == "" {
		feeDenom = "stake" // Default fee denom
	}

	feeAmount, _, err := envInt64("COSMOS_FEE_AMOUNT", 10000000)
	if err != nil {
		return "", err
	}
	log.Printf("[CreateEthClient] gas config: gasLimit=%d fee=%d%s", gasLimit, feeAmount, feeDenom)

	// Creating and registering the client can broadcast two Cosmos transactions.
	// Keep both sequence reads and broadcasts exclusive with relay batches so a
	// concurrent relay cannot sign with the same account sequence.
	h.cosmosMu.Lock()
	defer h.cosmosMu.Unlock()

	// Query account info (account number and sequence) from the chain
	log.Printf("[CreateEthClient] querying cosmos account info")
	accountNumber, sequence, err := h.queryAccountInfo(stdCtx, endpoint, signerAddr)
	if err != nil {
		return "", fmt.Errorf("failed to query account info: %w", err)
	}
	log.Printf("[CreateEthClient] account info: accountNumber=%d sequence=%d", accountNumber, sequence)

	// Setup encoding config
	interfaceRegistry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(interfaceRegistry)
	authtypes.RegisterInterfaces(interfaceRegistry)
	channeltypesv2.RegisterInterfaces(interfaceRegistry)
	clienttypes.RegisterInterfaces(interfaceRegistry)
	ibcwasmtypes.RegisterInterfaces(interfaceRegistry)
	cdc := codec.NewProtoCodec(interfaceRegistry)
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)

	msg, err := buildMsg(signerAddr)
	if err != nil {
		return "", err
	}
	log.Printf("[CreateEthClient] MsgCreateClient built")

	// Build the transaction
	txBuilder := txConfig.NewTxBuilder()
	if err := txBuilder.SetMsgs(msg); err != nil {
		return "", fmt.Errorf("failed to set messages: %w", err)
	}

	txBuilder.SetGasLimit(gasLimit)
	txBuilder.SetFeeAmount(sdk.NewCoins(sdk.NewCoin(feeDenom, sdkmath.NewInt(feeAmount))))

	// First, set an empty signature to populate signer info for sign bytes generation
	pubKey := privKey.PubKey()
	emptySig := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: nil,
		},
		Sequence: sequence,
	}
	if err := txBuilder.SetSignatures(emptySig); err != nil {
		return "", fmt.Errorf("failed to set empty signature: %w", err)
	}

	// Create signer data
	signerData := authsigning.SignerData{
		Address:       signerAddr,
		ChainID:       chainID,
		AccountNumber: accountNumber,
		Sequence:      sequence,
		PubKey:        pubKey,
	}

	// Get sign bytes using the adapter function
	signBytes, err := authsigning.GetSignBytesAdapter(
		stdCtx,
		txConfig.SignModeHandler(),
		sdksigning.SignMode_SIGN_MODE_DIRECT,
		signerData,
		txBuilder.GetTx(),
	)
	if err != nil {
		return "", fmt.Errorf("failed to get sign bytes: %w", err)
	}

	// Sign the bytes
	sigRaw, err := privKey.Sign(signBytes)
	if err != nil {
		return "", fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Set the actual signature
	sigV2 := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: sigRaw,
		},
		Sequence: sequence,
	}

	if err := txBuilder.SetSignatures(sigV2); err != nil {
		return "", fmt.Errorf("failed to set signatures: %w", err)
	}

	// Encode the transaction
	txBytes, err := txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return "", fmt.Errorf("failed to encode transaction: %w", err)
	}

	// Broadcast the transaction
	log.Printf("[CreateEthClient] broadcasting MsgCreateClient")
	bctx, bcancel := context.WithTimeout(stdCtx, cosmosRPCTimeout)
	result, err := endpoint.CosmosClient().BroadcastTxSync(bctx, txBytes)
	bcancel()
	if err != nil {
		return "", fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	if result.Code != 0 {
		return "", fmt.Errorf("transaction failed with code %d: %s", result.Code, result.Log)
	}

	log.Printf("[CreateEthClient] MsgCreateClient broadcast successfully. Hash: %s", result.Hash.String())

	// Wait for MsgCreateClient tx and extract the new client ID from events
	log.Printf("[CreateEthClient] waiting for MsgCreateClient tx result")
	txResult, err := h.waitForTxResult(stdCtx, endpoint, result.Hash, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("failed waiting for MsgCreateClient tx: %w", err)
	}

	if txResult.TxResult.Code != 0 {
		return "", fmt.Errorf("MsgCreateClient tx failed at DeliverTx: code=%d log=%s", txResult.TxResult.Code, txResult.TxResult.Log)
	}

	newClientID := extractClientID(txResult)
	if newClientID == "" {
		return "", fmt.Errorf("MsgCreateClient tx confirmed but client_id not found in events")
	}
	log.Printf("[CreateEthClient] new client ID: %s", newClientID)

	// No counterparty id given (e.g. an L2 bootstrap whose L2-side client id is not yet
	// configured): the client is created, its counterparty registered later.
	if counterpartyClientID == "" {
		return newClientID, nil
	}

	// Build and broadcast MsgRegisterCounterparty as a separate transaction
	registerMsg := clienttypesv2.NewMsgRegisterCounterparty(
		newClientID,
		[][]byte{[]byte("")},
		counterpartyClientID,
		signerAddr,
	)
	log.Printf("[CreateEthClient] MsgRegisterCounterparty built for clientID=%s", newClientID)

	// Re-query account info (sequence incremented after first tx)
	log.Printf("[CreateEthClient] querying cosmos account info for register counterparty")
	accountNumber, sequence, err = h.queryAccountInfo(stdCtx, endpoint, signerAddr)
	if err != nil {
		return "", fmt.Errorf("failed to query account info for register counterparty: %w", err)
	}
	log.Printf("[CreateEthClient] register counterparty account info: accountNumber=%d sequence=%d", accountNumber, sequence)

	txBuilder2 := txConfig.NewTxBuilder()
	if err := txBuilder2.SetMsgs(registerMsg); err != nil {
		return "", fmt.Errorf("failed to set register counterparty message: %w", err)
	}
	txBuilder2.SetGasLimit(gasLimit)
	txBuilder2.SetFeeAmount(sdk.NewCoins(sdk.NewCoin(feeDenom, sdkmath.NewInt(feeAmount))))

	emptySig2 := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: nil,
		},
		Sequence: sequence,
	}
	if err := txBuilder2.SetSignatures(emptySig2); err != nil {
		return "", fmt.Errorf("failed to set empty signature: %w", err)
	}

	signerData2 := authsigning.SignerData{
		Address:       signerAddr,
		ChainID:       chainID,
		AccountNumber: accountNumber,
		Sequence:      sequence,
		PubKey:        pubKey,
	}

	signBytes2, err := authsigning.GetSignBytesAdapter(
		stdCtx,
		txConfig.SignModeHandler(),
		sdksigning.SignMode_SIGN_MODE_DIRECT,
		signerData2,
		txBuilder2.GetTx(),
	)
	if err != nil {
		return "", fmt.Errorf("failed to get sign bytes for register counterparty: %w", err)
	}

	sigRaw2, err := privKey.Sign(signBytes2)
	if err != nil {
		return "", fmt.Errorf("failed to sign register counterparty tx: %w", err)
	}

	sigV22 := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: sigRaw2,
		},
		Sequence: sequence,
	}
	if err := txBuilder2.SetSignatures(sigV22); err != nil {
		return "", fmt.Errorf("failed to set signatures: %w", err)
	}

	txBytes2, err := txConfig.TxEncoder()(txBuilder2.GetTx())
	if err != nil {
		return "", fmt.Errorf("failed to encode register counterparty tx: %w", err)
	}

	log.Printf("[CreateEthClient] broadcasting MsgRegisterCounterparty")
	bctx2, bcancel2 := context.WithTimeout(stdCtx, cosmosRPCTimeout)
	result2, err := endpoint.CosmosClient().BroadcastTxSync(bctx2, txBytes2)
	bcancel2()
	if err != nil {
		return "", fmt.Errorf("failed to broadcast register counterparty tx: %w", err)
	}

	if result2.Code != 0 {
		return "", fmt.Errorf("register counterparty tx failed with code %d: %s", result2.Code, result2.Log)
	}

	log.Printf("[CreateEthClient] MsgRegisterCounterparty broadcast successfully. Hash: %s", result2.Hash.String())

	// Wait for the counterparty registration to be committed before returning. The
	// caller may immediately build another tx from this same account (e.g.
	// create-clients-cosmos creating an L2 wasm client right after the L1 one), and
	// that tx queries the account sequence — a still-uncommitted registration hands
	// back the pre-registration sequence and the next tx fails with
	// "account sequence mismatch".
	log.Printf("[CreateEthClient] waiting for MsgRegisterCounterparty tx result")
	txResult2, err := h.waitForTxResult(stdCtx, endpoint, result2.Hash, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("failed waiting for MsgRegisterCounterparty tx: %w", err)
	}
	if txResult2.TxResult.Code != 0 {
		return "", fmt.Errorf("register counterparty tx failed with code %d: %s",
			txResult2.TxResult.Code, txResult2.TxResult.Log)
	}

	return newClientID, nil
}

// extractClientID parses the client_id from the MsgCreateClient tx result events.
func extractClientID(txResult *coretypes.ResultTx) string {
	if txResult == nil {
		return ""
	}
	for _, event := range txResult.TxResult.Events {
		// Try all events — different ibc-go versions use different event types
		// (e.g. "create_client", "ibc_client", or attributes on generic events)
		for _, attr := range event.Attributes {
			if attr.Key == "client_id" {
				return attr.Value
			}
		}
	}
	return ""
}

// cosmosBech32Prefix returns the bech32 account prefix of the target Cosmos
// chain: COSMOS_ADDRESS_PREFIX, defaulting to "cosmos" (issue #220). Addresses
// are rendered with bech32.ConvertAndEncode instead of sdk.AccAddress.String()
// so the SDK's process-global bech32 config is never consulted or mutated —
// a future per-source prefix only needs to thread a string here.
func cosmosBech32Prefix() string {
	if prefix := os.Getenv("COSMOS_ADDRESS_PREFIX"); prefix != "" {
		return prefix
	}
	return "cosmos"
}

// cosmosSignerBech32 encodes the signer account address derived from privKey
// with the configured bech32 prefix.
func cosmosSignerBech32(privKey secp256k1.PrivKey) (string, error) {
	prefix := cosmosBech32Prefix()
	addr, err := sdkbech32.ConvertAndEncode(prefix, privKey.PubKey().Address())
	if err != nil {
		return "", fmt.Errorf("failed to bech32-encode signer address with prefix %q: %w", prefix, err)
	}
	// ConvertAndEncode normalizes the HRP but does not validate its characters.
	// Decode the result so startup rejects an address that Cosmos clients cannot
	// subsequently parse.
	if _, _, err := sdkbech32.DecodeAndConvert(addr); err != nil {
		return "", fmt.Errorf("invalid Cosmos signer address prefix %q: %w", prefix, err)
	}
	return addr, nil
}

// CosmosSignerAddress returns the bech32 address derived from COSMOS_PRIVATE_KEY.
// Used to populate the Signer field in Cosmos messages before batching them.
func (h *Handler) CosmosSignerAddress() (string, error) {
	privKeyBytes, err := h.keySigner().CosmosKeyBytes()
	if err != nil {
		return "", fmt.Errorf("failed to decode COSMOS_PRIVATE_KEY: %w", err)
	}
	return cosmosSignerBech32(secp256k1.PrivKey{Key: privKeyBytes})
}

func cloneCosmosSDKMsgWithSigner(msg any, signer string, index int) (sdk.Msg, error) {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		if index >= 0 {
			return nil, fmt.Errorf("message %d must be a proto.Message", index)
		}
		return nil, fmt.Errorf("message must be a proto.Message")
	}
	clonedProto := proto.Clone(protoMsg)
	if clonedProto == nil {
		if index >= 0 {
			return nil, fmt.Errorf("message %d cloned to nil", index)
		}
		return nil, fmt.Errorf("message cloned to nil")
	}
	sdkMsg, ok := clonedProto.(sdk.Msg)
	if !ok {
		if index >= 0 {
			return nil, fmt.Errorf("message %d does not implement sdk.Msg interface", index)
		}
		return nil, fmt.Errorf("message does not implement sdk.Msg interface")
	}
	fillEmptyCosmosSigner(sdkMsg, signer)
	return sdkMsg, nil
}

func fillEmptyCosmosSigner(msg sdk.Msg, signer string) {
	switch m := msg.(type) {
	case *clienttypes.MsgUpdateClient:
		if m.Signer == "" {
			m.Signer = signer
		}
	case *channeltypesv2.MsgRecvPacket:
		if m.Signer == "" {
			m.Signer = signer
		}
	case *channeltypesv2.MsgAcknowledgement:
		if m.Signer == "" {
			m.Signer = signer
		}
	case *channeltypesv2.MsgTimeout:
		if m.Signer == "" {
			m.Signer = signer
		}
	}
}

// isCosmosDuplicatePacketError returns true when the ABCI response indicates the
// packet(s) were already processed, so retrying the identical tx can never
// succeed and it is safe to drop. Matches two deterministic duplicate signals:
//
//	codespace "channelv2", code 11 = ErrAcknowledgementExists (ack already written)
//	codespace "channelv2", code 12 = ErrNoOpMsg (duplicate-delivery signal)
//	codespace "channel",   code 22 = ErrRedundantTx — the RedundantRelayDecorator
//	  ante handler rejects a tx whose packet messages are ALL already processed
//	  (raised at CheckTx). Without dropping this the relayer re-queues the tx and
//	  re-submits it forever, each retry re-running a full client update — draining
//	  gas — even though it can never stop being redundant.
//
// Substring matching on log messages is avoided because "commitment not found" can
// mean a packet was never sent — not just a duplicate — and would silently drop it.
func isCosmosDuplicatePacketError(codespace string, code uint32) bool {
	return (codespace == "channelv2" && (code == 11 || code == 12)) ||
		(codespace == "channel" && code == 22)
}

// isCosmosClientNotActiveError matches ibc-go's 02-client refusals that are about
// the light client, not the message:
//
//	codespace "client", code 5  = ErrClientFrozen
//	codespace "client", code 29 = ErrClientNotActive -- raised by 04-channel/v2
//	  RecvPacket/Acknowledgement and by the membership and update paths whenever
//	  the client's status is not Active (Frozen or Expired)
//
// Neither says anything about the packet: it succeeds once the client is
// recovered. Classifying them as permanent made the relay module drop every
// in-flight packet (#402).
func isCosmosClientNotActiveError(codespace string, code uint32) bool {
	return codespace == "client" && (code == 5 || code == 29)
}

// classifyCosmosFailure picks the sentinel a deterministic ABCI failure carries:
// ErrClientNotActive for a client that cannot accept anything, otherwise
// fallback.
func classifyCosmosFailure(codespace string, code uint32, fallback error) error {
	if isCosmosClientNotActiveError(codespace, code) {
		return services.ErrClientNotActive
	}
	return fallback
}

// duplicateDropIsSafe reports whether a duplicate response establishes that
// the entire submitted batch is already complete. A Cosmos transaction is
// atomic, so this is only true for a single-message batch.
func duplicateDropIsSafe(msgCount int) bool { return msgCount == 1 }

// newCosmosPreDeliverFailure preserves ABCI metadata and marks deterministic
// duplicate/redundant packet failures as permanent even when they are observed
// before DeliverTx. This matters for atomic update+packet batches: they cannot
// split inside the transaction handler, so the relay module must receive a
// permanent error and isolate the packets itself.
func newCosmosPreDeliverFailure(stage string, code uint32, codespace, failureLog string, data []byte) error {
	failure := &services.CosmosTxFailure{
		Stage:     stage,
		Code:      code,
		Codespace: codespace,
		Log:       failureLog,
		Data:      data,
	}
	if isCosmosDuplicatePacketError(codespace, code) {
		failure.Err = services.ErrPermanentRelayFailure
	}
	if isCosmosClientNotActiveError(codespace, code) {
		failure.Err = services.ErrClientNotActive
	}
	return failure
}

// splitCosmosBatchAfterDuplicate isolates a duplicate from its siblings. A
// Cosmos transaction is atomic, so a duplicate response for a multi-message
// transaction does not establish that its other messages were delivered. The
// returned success count must remain a prefix because BatchPartialError users
// settle only msgs[:SucceededCount].
func (h *Handler) splitCosmosBatchAfterDuplicate(
	stdCtx context.Context,
	svcCtx services.CosmosEndpoint,
	sdkMsgs []sdk.Msg,
	accountNumber, sequence uint64,
) (uint64, int, error) {
	return splitCosmosBatchAfterDuplicateWith(
		h.sendCosmosTxBatchWithSplitting, stdCtx, svcCtx, sdkMsgs, accountNumber, sequence,
	)
}

// cosmosSubBatchSender is the half-submitting seam used by the duplicate split.
// Production passes Handler.sendCosmosTxBatchWithSplitting; unit tests pass a
// fake so the prefix and sequence invariants do not require a Cosmos node.
type cosmosSubBatchSender func(
	stdCtx context.Context,
	svcCtx services.CosmosEndpoint,
	sdkMsgs []sdk.Msg,
	accountNumber, sequence uint64,
	allowSplit bool,
) (uint64, int, error)

func splitCosmosBatchAfterDuplicateWith(
	send cosmosSubBatchSender,
	stdCtx context.Context,
	svcCtx services.CosmosEndpoint,
	sdkMsgs []sdk.Msg,
	accountNumber, sequence uint64,
) (uint64, int, error) {
	mid := len(sdkMsgs) / 2
	nextSequence, succeeded, err := send(
		stdCtx, svcCtx, sdkMsgs[:mid], accountNumber, sequence, true,
	)
	if err != nil {
		log.Printf("WARN [SendCosmosTx] first half after duplicate split failed: %v", err)
		return nextSequence, succeeded, err
	}
	finalSequence, succeededSecond, err := send(
		stdCtx, svcCtx, sdkMsgs[mid:], accountNumber, nextSequence, true,
	)
	return finalSequence, succeeded + succeededSecond, err
}

// simulateMsgs builds a transaction with the given messages, signs it with an empty signature, and simulates its gas consumption.
func (h *Handler) simulateMsgs(stdCtx context.Context, svcCtx services.CosmosEndpoint, sdkMsgs []sdk.Msg, sequence uint64) (uint64, error) {
	// Setup encoding config
	interfaceRegistry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(interfaceRegistry)
	authtypes.RegisterInterfaces(interfaceRegistry)
	channeltypesv2.RegisterInterfaces(interfaceRegistry)
	cdc := codec.NewProtoCodec(interfaceRegistry)
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)

	// Get key details to construct the empty signature through the signer seam.
	privKeyBytes, err := h.keySigner().CosmosKeyBytes()
	if err != nil {
		return 0, fmt.Errorf("failed to decode private key: %w", err)
	}
	privKey := secp256k1.PrivKey{Key: privKeyBytes}
	pubKey := privKey.PubKey()

	// Build the transaction
	txBuilder := txConfig.NewTxBuilder()
	if err := txBuilder.SetMsgs(sdkMsgs...); err != nil {
		return 0, fmt.Errorf("failed to set messages: %w", err)
	}

	// We set a reasonable gas limit and empty fee for simulation
	txBuilder.SetGasLimit(20000000) // generous gas limit for simulation
	feeDenom := os.Getenv("COSMOS_FEE_DENOM")
	if feeDenom == "" {
		feeDenom = "stake"
	}
	txBuilder.SetFeeAmount(sdk.NewCoins(sdk.NewCoin(feeDenom, sdkmath.NewInt(0))))

	// Set empty signature
	emptySig := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: nil,
		},
		Sequence: sequence,
	}
	if err := txBuilder.SetSignatures(emptySig); err != nil {
		return 0, fmt.Errorf("failed to set empty signature: %w", err)
	}

	// Encode the transaction
	txBytes, err := txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return 0, fmt.Errorf("failed to encode transaction: %w", err)
	}

	// Create and marshal simulation request
	simReq := &txservice.SimulateRequest{
		TxBytes: txBytes,
	}
	simReqBytes, err := proto.Marshal(simReq)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal simulate request: %w", err)
	}

	// Query simulate endpoint
	qctx, qcancel := context.WithTimeout(stdCtx, cosmosRPCTimeout)
	result, err := svcCtx.CosmosClient().ABCIQuery(qctx, "/cosmos.tx.v1beta1.Service/Simulate", simReqBytes)
	qcancel()
	if err != nil {
		return 0, fmt.Errorf("simulate ABCI query failed: %w", err)
	}

	if result.Response.Code != 0 {
		return 0, newCosmosPreDeliverFailure(
			services.CosmosStageSimulation, result.Response.Code, result.Response.Codespace, result.Response.Log, nil,
		)
	}

	var simResp txservice.SimulateResponse
	if err := proto.Unmarshal(result.Response.Value, &simResp); err != nil {
		return 0, fmt.Errorf("failed to unmarshal simulate response: %w", err)
	}

	if simResp.GasInfo == nil {
		return 0, fmt.Errorf("gas info is nil in simulation response")
	}

	return simResp.GasInfo.GasUsed, nil
}

// sendCosmosTxBatchWithSplitting handles gas simulation, clamping, fee scaling,
// and optional recursive batch splitting. When allowSplit is false it returns
// before broadcasting if the batch cannot be submitted as one transaction.
func (h *Handler) sendCosmosTxBatchWithSplitting(stdCtx context.Context, svcCtx services.CosmosEndpoint, sdkMsgs []sdk.Msg, accountNumber, sequence uint64, allowSplit bool) (uint64, int, error) {
	return h.sendCosmosTxBatchAtHeadroom(stdCtx, svcCtx, sdkMsgs, accountNumber, sequence, allowSplit, 0)
}

func (h *Handler) sendCosmosTxBatchAtHeadroom(stdCtx context.Context, svcCtx services.CosmosEndpoint, sdkMsgs []sdk.Msg, accountNumber, sequence uint64, allowSplit bool, headroomStep int) (uint64, int, error) {
	if len(sdkMsgs) == 0 {
		return sequence, 0, nil
	}

	// Get block gas limit
	var maxBlockGas uint64 = 0
	if params, err := svcCtx.CosmosClient().ConsensusParams(stdCtx, nil); err == nil && params != nil {
		if params.ConsensusParams.Block.MaxGas > 0 {
			maxBlockGas = uint64(params.ConsensusParams.Block.MaxGas)
		}
	}

	shouldSplit := false
	var finalGasLimit uint64
	// unscaledGasLimit is finalGasLimit BEFORE applyCosmosGasHeadroom and before
	// the block clamp. The out-of-gas ladder is anchored on it, not on
	// finalGasLimit: anchoring on the scaled value applies the headroom factor a
	// second time, and once finalGasLimit has been clamped to maxBlockGas every
	// rung flattens onto the clamp, so the ladder reports itself exhausted while
	// real headroom steps remain. It is also what makes the rung the ladder names
	// equal the gas the retry actually computes at headroomStep+1.
	var unscaledGasLimit uint64

	// Simulate gas consumption for the messages in the batch.
	if len(sdkMsgs) > 0 {
		simulatedGas, err := h.simulateMsgs(stdCtx, svcCtx, sdkMsgs, sequence)
		if err != nil {
			log.Printf("WARN [SendCosmosTx] Simulation failed for batch of size %d: %v", len(sdkMsgs), err)
			// A client that is not Active rejects every message in the batch, so
			// splitting or broadcasting on fallback gas only pays fees to hear the
			// same answer from DeliverTx.
			if errors.Is(err, services.ErrClientNotActive) {
				return sequence, 0, err
			}
			if !allowSplit {
				return sequence, 0, fmt.Errorf("[SendCosmosTx] simulation failed; refusing to split atomic batch: %w", err)
			}
			if len(sdkMsgs) > 1 {
				log.Printf("[SendCosmosTx] Splitting batch...")
				shouldSplit = true
			}
		} else {
			// Apply the current finite gas-headroom factor.
			unscaledGasLimit = simulatedGas
			adjustedGas := applyCosmosGasHeadroom(simulatedGas, headroomStep)
			if maxBlockGas > 0 && adjustedGas >= maxBlockGas {
				log.Printf("WARN [SendCosmosTx] Adjusted gas %d exceeds max block gas %d for batch of size %d", adjustedGas, maxBlockGas, len(sdkMsgs))
				if len(sdkMsgs) > 1 {
					if !allowSplit {
						return sequence, 0, fmt.Errorf("[SendCosmosTx] adjusted gas %d exceeds max block gas %d; refusing to split atomic batch", adjustedGas, maxBlockGas)
					}
					log.Printf("[SendCosmosTx] Splitting batch...")
					shouldSplit = true
				} else {
					// Single message exceeds block limit; clamp it to max block gas.
					finalGasLimit = maxBlockGas
				}
			} else {
				finalGasLimit = adjustedGas
			}
		}
	}

	if shouldSplit {
		mid := len(sdkMsgs) / 2
		nextSeq, succ1, err := h.sendCosmosTxBatchWithSplitting(stdCtx, svcCtx, sdkMsgs[:mid], accountNumber, sequence, true)
		if err != nil {
			return sequence, succ1, err
		}
		nextSeq, succ2, err := h.sendCosmosTxBatchWithSplitting(stdCtx, svcCtx, sdkMsgs[mid:], accountNumber, nextSeq, true)
		if err != nil {
			return nextSeq, succ1 + succ2, err
		}
		return nextSeq, succ1 + succ2, nil
	}

	// If simulation wasn't run or failed, calculate the fallback gas limit
	if finalGasLimit == 0 {
		baseGas, err := envUint64("COSMOS_GAS_LIMIT", 200000)
		if err != nil {
			return sequence, 0, err
		}
		// MsgUpdateClient requires significantly more gas due to wasm verification
		for _, msg := range sdkMsgs {
			if _, ok := msg.(*clienttypes.MsgUpdateClient); ok {
				if baseGas < 2000000 {
					baseGas = 2000000
				}
				break
			}
		}

		// Guard against gasLimit overflow: baseGas * len(sdkMsgs)
		if len(sdkMsgs) > 0 && baseGas > (1<<64-1)/uint64(len(sdkMsgs)) {
			finalGasLimit = 1<<64 - 1
		} else {
			finalGasLimit = baseGas * uint64(len(sdkMsgs))
		}
		unscaledGasLimit = finalGasLimit
		finalGasLimit = applyCosmosGasHeadroom(finalGasLimit, headroomStep)
	}

	// Clamp to block gas limit and guard against zero limit
	if maxBlockGas > 0 && finalGasLimit > maxBlockGas {
		finalGasLimit = maxBlockGas
	}

	feeDenom := os.Getenv("COSMOS_FEE_DENOM")
	if feeDenom == "" {
		feeDenom = "stake"
	}

	// Set fee amount: default to matching gas limit
	feeAmount := int64(finalGasLimit)
	// Same parse and the same negative guard as the create-client path: this one
	// builds its own coin, so a guard on only one of the two leaves the panic
	// reachable.
	//
	// Branch on WHETHER THE VARIABLE WAS SET, not on the value. Branching on
	// baseFee > 0 made COSMOS_FEE_AMOUNT=0 -- a valid setting on a chain with no
	// minimum gas price -- indistinguishable from leaving it unset, so the
	// override was dropped and the default gas-matching fee was paid instead.
	baseFee, feeOverridden, err := envInt64("COSMOS_FEE_AMOUNT", 0)
	if err != nil {
		return sequence, 0, err
	}
	if feeOverridden {
		// Guard against feeAmount overflow: baseFee * len(sdkMsgs)
		if len(sdkMsgs) > 0 && baseFee > (1<<63-1)/int64(len(sdkMsgs)) {
			feeAmount = 1<<63 - 1
		} else {
			feeAmount = baseFee * int64(len(sdkMsgs))
		}
		feeAmount = applyCosmosFeeHeadroom(feeAmount, headroomStep)
	}

	// Clamp feeAmount if gasLimit was clamped to block limit
	if maxBlockGas > 0 && feeAmount > int64(maxBlockGas) {
		feeAmount = int64(maxBlockGas)
	}

	// Build, sign, and broadcast through the signer seam.
	privKeyBytes, err := h.keySigner().CosmosKeyBytes()
	if err != nil {
		return sequence, 0, fmt.Errorf("failed to decode private key: %w", err)
	}
	privKey := secp256k1.PrivKey{Key: privKeyBytes}
	pubKey := privKey.PubKey()
	signerAddr, err := cosmosSignerBech32(privKey)
	if err != nil {
		return sequence, 0, err
	}
	chainID := os.Getenv("COSMOS_CHAIN_ID")
	if chainID == "" {
		return sequence, 0, fmt.Errorf("COSMOS_CHAIN_ID environment variable is required")
	}

	interfaceRegistry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(interfaceRegistry)
	authtypes.RegisterInterfaces(interfaceRegistry)
	channeltypesv2.RegisterInterfaces(interfaceRegistry)
	cdc := codec.NewProtoCodec(interfaceRegistry)
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)

	txBuilder := txConfig.NewTxBuilder()
	if err := txBuilder.SetMsgs(sdkMsgs...); err != nil {
		return sequence, 0, fmt.Errorf("failed to set messages: %w", err)
	}

	txBuilder.SetGasLimit(finalGasLimit)
	txBuilder.SetFeeAmount(sdk.NewCoins(sdk.NewCoin(feeDenom, sdkmath.NewInt(feeAmount))))

	emptySig := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: nil,
		},
		Sequence: sequence,
	}
	if err := txBuilder.SetSignatures(emptySig); err != nil {
		return sequence, 0, fmt.Errorf("failed to set empty signature: %w", err)
	}

	signerData := authsigning.SignerData{
		Address:       signerAddr,
		ChainID:       chainID,
		AccountNumber: accountNumber,
		Sequence:      sequence,
		PubKey:        pubKey,
	}

	signBytes, err := authsigning.GetSignBytesAdapter(
		stdCtx,
		txConfig.SignModeHandler(),
		sdksigning.SignMode_SIGN_MODE_DIRECT,
		signerData,
		txBuilder.GetTx(),
	)
	if err != nil {
		return sequence, 0, fmt.Errorf("failed to get sign bytes: %w", err)
	}

	sigRaw, err := privKey.Sign(signBytes)
	if err != nil {
		return sequence, 0, fmt.Errorf("failed to sign transaction: %w", err)
	}

	sigV2 := sdksigning.SignatureV2{
		PubKey: pubKey,
		Data: &sdksigning.SingleSignatureData{
			SignMode:  sdksigning.SignMode_SIGN_MODE_DIRECT,
			Signature: sigRaw,
		},
		Sequence: sequence,
	}

	if err := txBuilder.SetSignatures(sigV2); err != nil {
		return sequence, 0, fmt.Errorf("failed to set signatures: %w", err)
	}

	txBytes, err := txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return sequence, 0, fmt.Errorf("failed to encode transaction: %w", err)
	}

	benchEnabled := utils.BenchEnabled()
	var broadcastStart time.Time
	if benchEnabled {
		broadcastStart = time.Now()
	}

	bctx, bcancel := context.WithTimeout(stdCtx, cosmosRPCTimeout)
	syncResult, err := svcCtx.CosmosClient().BroadcastTxSync(bctx, txBytes)
	bcancel()
	if err != nil {
		return sequence, 0, fmt.Errorf("failed to broadcast transaction: %w", err)
	}
	if syncResult.Code != 0 {
		log.Printf("WARN [SendCosmosTx] CheckTx FAILED: code=%d codespace=%s log=%s data=%x",
			syncResult.Code, syncResult.Codespace, syncResult.Log, syncResult.Data)
		if cosmosOutOfGas(syncResult.Codespace, syncResult.Code) {
			// CheckTx rejects the transaction before inclusion, so its sequence
			// remains available. The next attempt must still change: split a
			// splittable batch or advance the finite headroom ladder.
			cause := &services.CosmosTxFailure{
				Stage:     "CheckTx",
				Code:      syncResult.Code,
				Codespace: syncResult.Codespace,
				Log:       syncResult.Log,
				Data:      syncResult.Data,
				Err:       services.ErrPermanentRelayFailure,
			}
			plan, at := planCosmosOutOfGas(cause, len(sdkMsgs), headroomStep, unscaledGasLimit, maxBlockGas, allowSplit)
			next, bottom, ok := chain.Climb(plan, at)
			if !ok {
				log.Printf("ERROR [SendCosmosTx] CheckTx out-of-gas batch cannot receive more gas; reporting permanent")
				return sequence, 0, bottom
			}
			switch next.What {
			case knobBatchSize:
				log.Printf("[SendCosmosTx] splitting CheckTx out-of-gas batch of %d to %d", len(sdkMsgs), next.To)
				return h.splitCosmosBatchAfterDuplicate(stdCtx, svcCtx, sdkMsgs, accountNumber, sequence)
			default:
				log.Printf("[SendCosmosTx] retrying CheckTx out-of-gas batch at %s", next)
				return h.sendCosmosTxBatchAtHeadroom(stdCtx, svcCtx, sdkMsgs, accountNumber, sequence, allowSplit, headroomStep+1)
			}
		}
		if isCosmosDuplicatePacketError(syncResult.Codespace, syncResult.Code) {
			if duplicateDropIsSafe(len(sdkMsgs)) {
				log.Printf("[SendCosmosTx] duplicate packet (codespace=%s code=%d), dropping", syncResult.Codespace, syncResult.Code)
				return sequence, 1, nil
			}
			if allowSplit {
				log.Printf("[SendCosmosTx] duplicate packet in batch of %d; splitting to isolate it", len(sdkMsgs))
				return h.splitCosmosBatchAfterDuplicate(stdCtx, svcCtx, sdkMsgs, accountNumber, sequence)
			}
		}
		return sequence, 0, newCosmosPreDeliverFailure(
			"CheckTx", syncResult.Code, syncResult.Codespace, syncResult.Log, syncResult.Data,
		)
	}

	txResult, err := h.waitForTxResult(stdCtx, svcCtx, syncResult.Hash, cosmosInclusionTimeout)
	if err != nil {
		return sequence, 0, fmt.Errorf("failed to confirm transaction inclusion: %w", err)
	}

	var broadcastDur time.Duration
	if benchEnabled {
		broadcastDur = time.Since(broadcastStart)
	}

	if txResult.TxResult.Code != 0 {
		log.Printf("WARN [SendCosmosTx] DeliverTx FAILED: code=%d codespace=%s log=%s data=%x",
			txResult.TxResult.Code, txResult.TxResult.Codespace, txResult.TxResult.Log, txResult.TxResult.Data)
		if isCosmosDuplicatePacketError(txResult.TxResult.Codespace, txResult.TxResult.Code) {
			if duplicateDropIsSafe(len(sdkMsgs)) {
				log.Printf("[SendCosmosTx] duplicate packet (codespace=%s code=%d), dropping", txResult.TxResult.Codespace, txResult.TxResult.Code)
				return sequence + 1, 1, nil
			}
			if allowSplit {
				// DeliverTx was included, so its account sequence was consumed even
				// though message execution reverted.
				log.Printf("[SendCosmosTx] duplicate packet in batch of %d; splitting to isolate it", len(sdkMsgs))
				return h.splitCosmosBatchAfterDuplicate(stdCtx, svcCtx, sdkMsgs, accountNumber, sequence+1)
			}
		}
		if cosmosOutOfGas(txResult.TxResult.Codespace, txResult.TxResult.Code) {
			// DeliverTx consumed the account sequence. A transient result must also
			// change the next attempt, or an underestimated batch repeats forever.
			nextSequence := sequence + 1
			cause := &services.CosmosTxFailure{
				Stage:     "DeliverTx",
				Code:      txResult.TxResult.Code,
				Codespace: txResult.TxResult.Codespace,
				Log:       txResult.TxResult.Log,
				Data:      txResult.TxResult.Data,
				Err:       services.ErrPermanentRelayFailure,
			}
			plan, at := planCosmosOutOfGas(cause, len(sdkMsgs), headroomStep, unscaledGasLimit, maxBlockGas, allowSplit)
			next, bottom, ok := chain.Climb(plan, at)
			if !ok {
				log.Printf("ERROR [SendCosmosTx] out-of-gas batch cannot receive more gas; reporting permanent")
				return nextSequence, 0, bottom
			}
			switch next.What {
			case knobBatchSize:
				log.Printf("[SendCosmosTx] splitting included out-of-gas batch of %d to %d", len(sdkMsgs), next.To)
				return h.splitCosmosBatchAfterDuplicate(stdCtx, svcCtx, sdkMsgs, accountNumber, nextSequence)
			default:
				log.Printf("[SendCosmosTx] retrying included out-of-gas batch at %s", next)
				return h.sendCosmosTxBatchAtHeadroom(stdCtx, svcCtx, sdkMsgs, accountNumber, nextSequence, allowSplit, headroomStep+1)
			}
		}
		return sequence, 0, &services.CosmosTxFailure{
			Stage:     "DeliverTx",
			Code:      txResult.TxResult.Code,
			Codespace: txResult.TxResult.Codespace,
			Log:       txResult.TxResult.Log,
			Data:      txResult.TxResult.Data,
			Err: classifyCosmosFailure(txResult.TxResult.Codespace, txResult.TxResult.Code,
				services.ErrPermanentRelayFailure),
		}
	}

	log.Printf("[SendCosmosTx] Tx confirmed at height %d hash=%s (msgs=%d)", txResult.Height, txResult.Hash.String(), len(sdkMsgs))
	if benchEnabled {
		log.Printf("[bench] cosmos batch msgs=%d gasWanted=%d gasUsed=%d broadcast=%s height=%d hash=%s",
			len(sdkMsgs), txResult.TxResult.GasWanted, txResult.TxResult.GasUsed,
			broadcastDur, txResult.Height, txResult.Hash.String())
	}

	return sequence + 1, len(sdkMsgs), nil
}

// SendCosmosTxBatch sends multiple messages in a single Cosmos transaction.
//
// NOTE on Non-Atomicity:
// If a batch is large or simulation indicates it would exceed the block gas limit,
// the batch is recursively split into smaller sub-batches and sent as MULTIPLE separate transactions.
// These transactions are executed sequentially, and the account sequence is updated accordingly.
// If an earlier sub-batch succeeds but a later one fails, an error is returned.
//
// Retry Behavior and Idempotency:
// Callers should be aware that the overall batch operation is not atomic. On failure,
// the relayer's main loop will retry. However, because the loop is state-based, it queries
// the current chain state at the start of each iteration. Any messages/packets successfully
// committed by the earlier succeeded sub-batches will not be included in the retried batch.
// Downstream Cosmos modules/contracts are idempotent and tolerate already-processed packets safely.
func (h *Handler) SendCosmosTxBatch(stdCtx context.Context, svcCtx services.CosmosEndpoint, msgs []any) error {
	return h.sendCosmosTxBatch(stdCtx, svcCtx, msgs, true)
}

// SendCosmosTxBatchAtomic sends every message in exactly one Cosmos
// transaction. Unlike SendCosmosTxBatch it refuses to recursively split on a
// failed simulation or block-gas overflow, preserving update+packet atomicity.
func (h *Handler) SendCosmosTxBatchAtomic(stdCtx context.Context, svcCtx services.CosmosEndpoint, msgs []any) error {
	return h.sendCosmosTxBatch(stdCtx, svcCtx, msgs, false)
}

func (h *Handler) sendCosmosTxBatch(stdCtx context.Context, svcCtx services.CosmosEndpoint, msgs []any, allowSplit bool) error {
	if len(msgs) == 0 {
		return nil
	}

	benchEnabled := utils.BenchEnabled()
	var benchStart time.Time
	if benchEnabled {
		benchStart = time.Now()
	}

	privKeyBytes, err := h.keySigner().CosmosKeyBytes()
	if err != nil {
		return fmt.Errorf("failed to decode private key: %w", err)
	}

	privKey := secp256k1.PrivKey{Key: privKeyBytes}
	signerAddr, err := cosmosSignerBech32(privKey)
	if err != nil {
		return err
	}

	// Convert all messages to sdk.Msg, filling empty Signer fields
	var sdkMsgs []sdk.Msg
	for i, msg := range msgs {
		sdkMsg, err := cloneCosmosSDKMsgWithSigner(msg, signerAddr, i)
		if err != nil {
			return err
		}
		sdkMsgs = append(sdkMsgs, sdkMsg)
	}

	// Serialize Cosmos sequence use across relay goroutines. A split batch may
	// submit multiple txs, so the lock covers the whole sequence chain.
	h.cosmosMu.Lock()
	defer h.cosmosMu.Unlock()

	// Query account info (account number and sequence) from the chain
	accountNumber, sequence, err := h.queryAccountInfo(stdCtx, svcCtx, signerAddr)
	if err != nil {
		return fmt.Errorf("failed to query account info: %w", err)
	}

	_, succCount, err := h.sendCosmosTxBatchWithSplitting(stdCtx, svcCtx, sdkMsgs, accountNumber, sequence, allowSplit)
	if benchEnabled {
		log.Printf("[bench] cosmos batch msgs=%d total=%s", len(msgs), time.Since(benchStart))
	}
	if err != nil {
		if succCount > 0 {
			return &services.BatchPartialError{
				SucceededCount: succCount,
				Err:            err,
			}
		}
		return err
	}
	return nil
}

// queryAccountInfo queries the account number and sequence for the given address
func (h *Handler) queryAccountInfo(stdCtx context.Context, svcCtx services.CosmosEndpoint, address string) (uint64, uint64, error) {
	// Build the query request
	queryReq := &authtypes.QueryAccountRequest{
		Address: address,
	}

	reqBytes, err := proto.Marshal(queryReq)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to marshal query request: %w", err)
	}

	// Query path for auth account
	queryPath := "/cosmos.auth.v1beta1.Query/Account"

	// Make ABCI query
	qctx, qcancel := context.WithTimeout(stdCtx, cosmosRPCTimeout)
	result, err := svcCtx.CosmosClient().ABCIQuery(qctx, queryPath, reqBytes)
	qcancel()
	if err != nil {
		return 0, 0, fmt.Errorf("ABCI query failed: %w", err)
	}

	if result.Response.Code != 0 {
		return 0, 0, fmt.Errorf("query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}

	// Setup interface registry to decode the account
	interfaceRegistry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(interfaceRegistry)
	authtypes.RegisterInterfaces(interfaceRegistry)

	// Decode the response
	var queryResp authtypes.QueryAccountResponse
	if err := proto.Unmarshal(result.Response.Value, &queryResp); err != nil {
		return 0, 0, fmt.Errorf("failed to unmarshal query response: %w", err)
	}

	// Unpack the account from Any type
	var account sdk.AccountI
	if err := interfaceRegistry.UnpackAny(queryResp.Account, &account); err != nil {
		return 0, 0, fmt.Errorf("failed to unpack account: %w", err)
	}

	return account.GetAccountNumber(), account.GetSequence(), nil
}

// waitForTxResult polls the chain until the transaction with the given hash is included in a block or the timeout expires.
func (h *Handler) waitForTxResult(stdCtx context.Context, svcCtx services.CosmosEndpoint, txHash []byte, timeout time.Duration) (*coretypes.ResultTx, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Abort promptly on shutdown. Without this the poll loop runs until the local
		// timeout (up to cosmosInclusionTimeout) — and because SendCosmosTxBatch holds
		// cosmosMu across this call, that would stall shutdown for the whole window.
		if err := stdCtx.Err(); err != nil {
			return nil, fmt.Errorf("cancelled waiting for tx %X: %w", txHash, err)
		}
		qctx, qcancel := context.WithTimeout(stdCtx, cosmosRPCTimeout)
		result, err := svcCtx.CosmosClient().Tx(qctx, txHash, false)
		qcancel()
		if err == nil && result != nil && result.Height > 0 {
			log.Printf("[SendCosmosTx] Tx %X confirmed at height %d", txHash, result.Height)
			return result, nil
		}
		// Wait before the next poll, but cancel out immediately instead of sleeping a
		// full second past a shutdown signal.
		timer := time.NewTimer(1 * time.Second)
		select {
		case <-stdCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("cancelled waiting for tx %X: %w", txHash, stdCtx.Err())
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("timeout waiting for tx %X to be included in a block", txHash)
}
