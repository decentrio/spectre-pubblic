// Package wasmclient builds the common Cosmos 08-wasm MsgUpdateClient envelope.
package wasmclient

import (
	"fmt"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
)

// BuildUpdateClient wraps data, which must already be the JSON understood by an
// 08-wasm client, in a MsgUpdateClient. It deliberately does not inspect or
// transform the data so source builders and the on-chain client share one
// meaningful payload representation.
func BuildUpdateClient(signer, clientID string, data []byte) (*clienttypes.MsgUpdateClient, error) {
	clientMessage := &ibcwasmtypes.ClientMessage{Data: data}
	anyMsg, err := codectypes.NewAnyWithValue(clientMessage)
	if err != nil {
		return nil, fmt.Errorf("wrap wasm client message: %w", err)
	}
	return &clienttypes.MsgUpdateClient{
		ClientId:      clientID,
		ClientMessage: anyMsg,
		Signer:        signer,
	}, nil
}
