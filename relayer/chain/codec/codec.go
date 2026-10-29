// Package codec holds the gob codec for the opaque Cosmos-to-EVM client
// update payload. The beacon and L2-to-Cosmos adapters carry their wasm JSON
// client-message bytes directly.
package codec

import (
	"bytes"
	"encoding/gob"
	"fmt"

	spectreContract "relayer/bindings/SpectreClient"
	updateclientContract "relayer/bindings/UpdateClient"
)

// --- Cosmos -> ETH (groth16) update ---

type cosmosUpdate struct {
	Kind      int // services.ClientUpdateKind, kept as int so codec stays leaf
	AppMsg    updateclientContract.ISpectreClientMsgsMsgUpdateApplicationState
	NewValSet spectreContract.ISpectreTendermintMsgsValidatorSet
}

// EncodeCosmosUpdate encodes a Cosmos->ETH SpectreClient update. kind carries the
// services.ClientUpdateKind value.
func EncodeCosmosUpdate(kind int, appMsg updateclientContract.ISpectreClientMsgsMsgUpdateApplicationState, newValSet spectreContract.ISpectreTendermintMsgsValidatorSet) ([]byte, error) {
	return encode(cosmosUpdate{Kind: kind, AppMsg: appMsg, NewValSet: newValSet})
}

// DecodeCosmosUpdate reverses EncodeCosmosUpdate.
func DecodeCosmosUpdate(payload []byte) (kind int, appMsg updateclientContract.ISpectreClientMsgsMsgUpdateApplicationState, newValSet spectreContract.ISpectreTendermintMsgsValidatorSet, err error) {
	var u cosmosUpdate
	if derr := decode(payload, &u); derr != nil {
		return 0, appMsg, newValSet, fmt.Errorf("codec: decode cosmos update: %w", derr)
	}
	return u.Kind, u.AppMsg, u.NewValSet, nil
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decode(payload []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(payload)).Decode(v)
}
