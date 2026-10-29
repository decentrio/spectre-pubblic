// Package ethnative builds the Cosmos messages of the Ethereum light client when
// the destination chain hosts it as a native ibc-go light client module
// (ibc.lightclients.spectre.v1) rather than as a CosmWasm contract behind
// 08-wasm. There is no ClientMessage wrapper and no JSON on chain: client state,
// consensus state, headers and storage proofs are the protobuf messages of
// spectrepb.
//
// The builders still work on the relayer's JSON (client.EthereumHeader and the
// proofs client.GetEthMembershipProof produces) and convert at this boundary, so
// the code that fetches beacon data and builds proofs does not depend on how the
// destination verifies them.
package ethnative

import (
	"encoding/json"
	"fmt"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"

	relayerclient "relayer/client"
)

// ClientType is the ibc-go client type of the module, and so the prefix of the
// client ids it is assigned ("spectre-0", ...).
const ClientType = "spectre"

// PackClientStates packs the bootstrap client and consensus state of a new
// client into the Any values MsgCreateClient carries.
func PackClientStates(clientState *relayerclient.EthereumClientState, consensusState *relayerclient.EthereumConsensusState) (clientStateAny, consensusStateAny *codectypes.Any, err error) {
	cs, err := clientState.ToProto()
	if err != nil {
		return nil, nil, fmt.Errorf("convert client state: %w", err)
	}
	cons, err := consensusState.ToProto()
	if err != nil {
		return nil, nil, fmt.Errorf("convert consensus state: %w", err)
	}
	clientStateAny, err = codectypes.NewAnyWithValue(cs)
	if err != nil {
		return nil, nil, fmt.Errorf("pack client state: %w", err)
	}
	consensusStateAny, err = codectypes.NewAnyWithValue(cons)
	if err != nil {
		return nil, nil, fmt.Errorf("pack consensus state: %w", err)
	}
	return clientStateAny, consensusStateAny, nil
}

// BuildUpdateClient wraps a header, in the relayer's JSON form, in a
// MsgUpdateClient carrying the protobuf Header the native module verifies.
func BuildUpdateClient(signer, clientID string, headerJSON []byte) (*clienttypes.MsgUpdateClient, error) {
	var header relayerclient.EthereumHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("decode ethereum header json: %w", err)
	}
	pb, err := header.ToProto()
	if err != nil {
		return nil, fmt.Errorf("convert ethereum header: %w", err)
	}
	clientMessage, err := codectypes.NewAnyWithValue(pb)
	if err != nil {
		return nil, fmt.Errorf("pack ethereum header: %w", err)
	}
	return &clienttypes.MsgUpdateClient{
		ClientId:      clientID,
		ClientMessage: clientMessage,
		Signer:        signer,
	}, nil
}

// Proof converts a storage proof in the relayer's JSON form (the output of
// client.GetEthMembershipProof and client.GetEthNonMembershipProof) to the
// protobuf MembershipProof the native module verifies, for the proof fields of
// MsgRecvPacket, MsgAcknowledgement and MsgTimeout.
func Proof(proofJSON []byte) ([]byte, error) {
	return relayerclient.MembershipProofFromJSON(proofJSON)
}
