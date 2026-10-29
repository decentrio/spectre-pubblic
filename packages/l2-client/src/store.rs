//! 08-wasm host storage helpers for L2 client and consensus state envelopes.

use cosmwasm_std::Storage;
use ibc_proto::{
    google::protobuf::Any,
    ibc::lightclients::wasm::v1::{
        ClientState as WasmClientState, ConsensusState as WasmConsensusState,
    },
};
use prost::{Message, Name};

use crate::error::Error;

/// Host key where ibc-go stores the outer Wasm client state.
pub const HOST_CLIENT_STATE_KEY: &str = "clientState";
/// Host key prefix where ibc-go stores outer Wasm consensus states.
pub const HOST_CONSENSUS_STATES_KEY: &str = "consensusStates";

/// Returns the host storage key for a revision-zero L2 consensus state.
#[must_use]
pub fn consensus_db_key(height: u64) -> String {
    format!("{HOST_CONSENSUS_STATES_KEY}/0-{height}")
}

/// Loads the outer Wasm client state from host-owned storage.
pub fn get_wasm_client_state(storage: &dyn Storage) -> Result<WasmClientState, Error> {
    let bytes = storage
        .get(HOST_CLIENT_STATE_KEY.as_bytes())
        .ok_or(Error::ClientStateMissing)?;
    let envelope = Any::decode(bytes.as_slice())?;
    if envelope.type_url != WasmClientState::type_url() {
        return Err(Error::InvalidHostEnvelopeType);
    }
    WasmClientState::decode(envelope.value.as_slice()).map_err(Into::into)
}

/// Stores the outer Wasm client state in host-owned storage.
pub fn store_wasm_client_state(
    storage: &mut dyn Storage,
    client_state: &WasmClientState,
) -> Result<(), Error> {
    let envelope = Any::from_msg(client_state)?;
    storage.set(
        HOST_CLIENT_STATE_KEY.as_bytes(),
        envelope.encode_to_vec().as_slice(),
    );
    Ok(())
}

/// Loads an outer Wasm consensus state at an exact revision-zero L2 height.
pub fn get_wasm_consensus_state(
    storage: &dyn Storage,
    height: u64,
) -> Result<WasmConsensusState, Error> {
    let bytes = storage
        .get(consensus_db_key(height).as_bytes())
        .ok_or(Error::ConsensusStateMissing(height))?;
    let envelope = Any::decode(bytes.as_slice())?;
    if envelope.type_url != WasmConsensusState::type_url() {
        return Err(Error::InvalidHostEnvelopeType);
    }
    WasmConsensusState::decode(envelope.value.as_slice()).map_err(Into::into)
}

/// Loads an outer Wasm consensus state when the exact height exists.
pub fn may_get_wasm_consensus_state(
    storage: &dyn Storage,
    height: u64,
) -> Result<Option<WasmConsensusState>, Error> {
    let Some(bytes) = storage.get(consensus_db_key(height).as_bytes()) else {
        return Ok(None);
    };
    let envelope = Any::decode(bytes.as_slice())?;
    if envelope.type_url != WasmConsensusState::type_url() {
        return Err(Error::InvalidHostEnvelopeType);
    }
    Ok(Some(WasmConsensusState::decode(envelope.value.as_slice())?))
}

/// Stores an outer Wasm consensus state at an exact revision-zero L2 height.
pub fn store_wasm_consensus_state(
    storage: &mut dyn Storage,
    height: u64,
    consensus_state: &WasmConsensusState,
) -> Result<(), Error> {
    let envelope = Any::from_msg(consensus_state)?;
    storage.set(
        consensus_db_key(height).as_bytes(),
        envelope.encode_to_vec().as_slice(),
    );
    Ok(())
}
