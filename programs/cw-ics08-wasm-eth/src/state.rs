//! State management for the Ethereum light client

use cosmwasm_std::{Order, Storage};
use ethereum_light_client::client_state::ClientState as EthClientState;
use ethereum_light_client::consensus_state::ConsensusState as EthConsensusState;
use ibc_proto::{
    google::protobuf::Any,
    ibc::lightclients::wasm::v1::{
        ClientState as WasmClientState, ConsensusState as WasmConsensusState,
    },
};
use prost::Message;

use crate::ContractError;

/// The store key used by `ibc-go` to store the client state
pub const HOST_CLIENT_STATE_KEY: &str = "clientState";
/// The store key used by `ibc-go` to store the consensus states
pub const HOST_CONSENSUS_STATES_KEY: &str = "consensusStates";

/// The key used to store the consensus states by height
#[must_use]
pub fn consensus_db_key(slot: u64) -> String {
    format!("{}/{}-{}", HOST_CONSENSUS_STATES_KEY, 0, slot)
}

/// Removes every stored consensus state.
///
/// Only the keys under `consensusStates/` are touched; the client state and the contract version
/// are left alone.
pub fn clear_consensus_states(storage: &mut dyn Storage) {
    // The range is `[consensusStates/, consensusStates0)`: '0' is the byte after '/', so it covers
    // exactly the keys that have the `consensusStates/` prefix.
    let start = format!("{HOST_CONSENSUS_STATES_KEY}/");
    let end = format!("{HOST_CONSENSUS_STATES_KEY}0");
    // The keys are collected first because the storage cannot be written while it is iterated.
    let keys = storage
        .range_keys(
            Some(start.as_bytes()),
            Some(end.as_bytes()),
            Order::Ascending,
        )
        .collect::<Vec<_>>();
    for key in keys {
        storage.remove(&key);
    }
}

/// Get the Wasm client state
/// # Errors
/// Returns an error if the client state is not found or cannot be deserialized
/// # Returns
/// The Wasm client state
#[allow(clippy::module_name_repetitions)]
pub fn get_wasm_client_state(storage: &dyn Storage) -> Result<WasmClientState, ContractError> {
    let wasm_client_state_any_bz = storage
        .get(HOST_CLIENT_STATE_KEY.as_bytes())
        .ok_or(ContractError::ClientStateNotFound)?;
    let wasm_client_state_any = Any::decode(wasm_client_state_any_bz.as_slice())?;

    Ok(WasmClientState::decode(
        wasm_client_state_any.value.as_slice(),
    )?)
}

/// Get the Ethereum client state
/// # Errors
/// Returns an error if the client state is not found or cannot be deserialized
/// # Returns
/// The Ethereum client state
#[allow(clippy::module_name_repetitions)]
pub fn get_eth_client_state(storage: &dyn Storage) -> Result<EthClientState, ContractError> {
    let wasm_client_state = get_wasm_client_state(storage)?;
    Ok(serde_json::from_slice(&wasm_client_state.data)?)
}

/// Get the Ethereum consensus state at a given height
/// # Errors
/// Returns an error if the consensus state is not found or cannot be deserialized
/// # Returns
/// The Ethereum consensus state
#[allow(clippy::module_name_repetitions)]
pub fn get_eth_consensus_state(
    storage: &dyn Storage,
    slot: u64,
) -> Result<EthConsensusState, ContractError> {
    let wasm_consensus_state_any_bz = storage
        .get(consensus_db_key(slot).as_bytes())
        .ok_or(ContractError::ConsensusStateNotFound)?;
    let wasm_consensus_state_any = Any::decode(wasm_consensus_state_any_bz.as_slice())?;
    let wasm_consensus_state =
        WasmConsensusState::decode(wasm_consensus_state_any.value.as_slice())?;

    Ok(serde_json::from_slice(&wasm_consensus_state.data)?)
}

/// Checks that a header is anchored at the latest consensus state of the client.
///
/// A header names the consensus state it is verified against (`trusted_slot`), but `UpdateState`
/// applies the result on top of the latest one. Verifying against an older consensus state would
/// let the keys of a past sync committee, which carry no stake any more, forge a root, and write
/// the header at a slot that is already stored. Every consensus state older than the latest is
/// therefore refused as an anchor. Misbehaviour evidence writes nothing and has its own rule, see
/// `ethereum_light_client::misbehaviour::verify_misbehaviour`.
/// # Errors
/// Returns an error if `trusted_slot` is not the latest slot of the client
pub const fn ensure_trusted_slot_is_latest(
    client_state: &EthClientState,
    trusted_slot: u64,
) -> Result<(), ContractError> {
    if trusted_slot != client_state.latest_slot {
        return Err(ContractError::TrustedSlotNotLatest {
            trusted_slot,
            latest_slot: client_state.latest_slot,
        });
    }
    Ok(())
}

/// Get the consensus state a header is verified against
/// # Errors
/// Returns an error if `trusted_slot` is not the latest slot of the client, or if its consensus
/// state is not found or cannot be deserialized
pub fn get_trusted_consensus_state(
    storage: &dyn Storage,
    client_state: &EthClientState,
    trusted_slot: u64,
) -> Result<EthConsensusState, ContractError> {
    ensure_trusted_slot_is_latest(client_state, trusted_slot)?;
    get_eth_consensus_state(storage, trusted_slot)
}

/// Store the consensus state
/// # Errors
/// Returns an error if the consensus state cannot be serialized into an Any
#[allow(clippy::module_name_repetitions)]
pub fn store_consensus_state(
    storage: &mut dyn Storage,
    wasm_consensus_state: &WasmConsensusState,
    slot: u64,
) -> Result<(), ContractError> {
    let encoded = encode_consensus_state(wasm_consensus_state)?;
    storage.set(consensus_db_key(slot).as_bytes(), &encoded);

    Ok(())
}

/// Store the client state
/// # Errors
/// Returns an error if the client state cannot be serialized into an Any
#[allow(clippy::module_name_repetitions)]
pub fn store_client_state(
    storage: &mut dyn Storage,
    wasm_client_state: &WasmClientState,
) -> Result<(), ContractError> {
    let encoded = encode_client_state(wasm_client_state)?;
    storage.set(HOST_CLIENT_STATE_KEY.as_bytes(), &encoded);

    Ok(())
}

/// Encodes a client envelope without mutating storage.
///
/// # Errors
/// Returns a protobuf encoding error if the host envelope cannot be encoded.
pub fn encode_client_state(wasm_client_state: &WasmClientState) -> Result<Vec<u8>, ContractError> {
    Ok(Any::from_msg(wasm_client_state)?.encode_to_vec())
}

/// Encodes a consensus envelope without mutating storage.
///
/// # Errors
/// Returns a protobuf encoding error if the host envelope cannot be encoded.
pub fn encode_consensus_state(
    wasm_consensus_state: &WasmConsensusState,
) -> Result<Vec<u8>, ContractError> {
    Ok(Any::from_msg(wasm_consensus_state)?.encode_to_vec())
}
