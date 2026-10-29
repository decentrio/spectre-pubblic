//! This module contains the instantiate helper functions

use cosmwasm_std::{ensure, Storage};
use ethereum_light_client::{
    client_state::ClientState as EthClientState,
    consensus_state::ConsensusState as EthConsensusState,
};
use ethereum_types::consensus::fork::{Fork, ForkParameters};
use ibc_proto::ibc::{
    core::client::v1::Height as IbcProtoHeight,
    lightclients::wasm::v1::{
        ClientState as WasmClientState, ConsensusState as WasmConsensusState,
    },
};

use crate::{
    msg::InstantiateMsg,
    state::{
        clear_consensus_states, consensus_db_key, encode_client_state, encode_consensus_state,
        HOST_CLIENT_STATE_KEY,
    },
    ContractError,
};

/// Fully validated and encoded bootstrap writes.
pub struct PreparedClient {
    client: Vec<u8>,
    consensus_key: Vec<u8>,
    consensus: Vec<u8>,
}

impl PreparedClient {
    /// Commits the already validated bootstrap state without fallible work between writes.
    ///
    /// Every consensus state already in the store is discarded first. `Reinstantiate` is the only
    /// way to unfreeze a client, and it is used after the roots the client accepted can no longer
    /// be trusted. Membership proofs are accepted against any stored consensus state, so keeping
    /// them would keep accepting exactly the roots the recovery was meant to drop. On a fresh
    /// store there is nothing to discard.
    pub fn commit(self, storage: &mut dyn Storage) {
        clear_consensus_states(storage);
        storage.set(HOST_CLIENT_STATE_KEY.as_bytes(), &self.client);
        storage.set(&self.consensus_key, &self.consensus);
    }
}

/// Initializes the client state and consensus state
/// # Errors
/// Will return an error if the client state or consensus state cannot be deserialized.
/// # Panics
/// Will panic if the client state latest height cannot be unwrapped
#[allow(clippy::needless_pass_by_value)]
pub fn prepare_client(msg: InstantiateMsg) -> Result<PreparedClient, ContractError> {
    let client_state_bz: Vec<u8> = msg.client_state.into();
    let client_state: EthClientState = serde_json::from_slice(&client_state_bz)
        .map_err(ContractError::DeserializeClientStateFailed)?;
    let wasm_client_state = WasmClientState {
        checksum: msg.checksum.into(),
        data: client_state_bz,
        latest_height: Some(IbcProtoHeight {
            revision_number: 0,
            revision_height: client_state.latest_slot,
        }),
    };

    let consensus_state_bz: Vec<u8> = msg.consensus_state.into();
    let consensus_state: EthConsensusState = serde_json::from_slice(&consensus_state_bz)
        .map_err(ContractError::DeserializeConsensusStateFailed)?;
    let wasm_consensus_state = WasmConsensusState {
        data: consensus_state_bz,
    };

    ensure!(
        wasm_client_state.latest_height.unwrap().revision_height == client_state.latest_slot,
        ContractError::ClientStateSlotMismatch
    );

    ensure!(
        client_state.latest_slot == consensus_state.slot,
        ContractError::ClientAndConsensusStateMismatch
    );
    ensure!(client_state.latest_slot != 0, ContractError::ZeroHeight);
    ensure!(
        client_state.slots_per_epoch != 0,
        ContractError::InvalidClientState("slots_per_epoch must be non-zero")
    );
    ensure!(
        client_state.epochs_per_sync_committee_period != 0,
        ContractError::InvalidClientState("epochs_per_sync_committee_period must be non-zero")
    );

    ensure!(
        !client_state.is_frozen,
        ContractError::InvalidClientState("a new client state must not be frozen")
    );
    ensure!(
        client_state.sync_committee_size != 0,
        ContractError::InvalidClientState("sync_committee_size must be non-zero")
    );
    ensure!(
        client_state.min_sync_committee_participants != 0
            && client_state.min_sync_committee_participants <= client_state.sync_committee_size,
        ContractError::InvalidClientState(
            "min_sync_committee_participants must be between 1 and sync_committee_size"
        )
    );
    validate_fork_order(&client_state.fork_parameters)?;
    // A client bootstrapped without its next sync committee cannot cross into the next period, and
    // the committee stored from a later update is only trusted to follow the store's own period.
    ensure!(
        consensus_state.next_sync_committee.is_some(),
        ContractError::InvalidConsensusState("next_sync_committee must be set")
    );

    client_state
        .verify_supported_fork_at_epoch(
            client_state.compute_epoch_at_slot(client_state.latest_slot),
        )
        .map_err(ContractError::UnsupportedForkVersion)?;

    Ok(PreparedClient {
        client: encode_client_state(&wasm_client_state)?,
        consensus_key: consensus_db_key(consensus_state.slot).into_bytes(),
        consensus: encode_consensus_state(&wasm_consensus_state)?,
    })
}

/// The forks in activation order, with the name used in errors.
const fn forks(fork_parameters: &ForkParameters) -> [(&'static str, &Fork); 7] {
    [
        ("altair", &fork_parameters.altair),
        ("bellatrix", &fork_parameters.bellatrix),
        ("capella", &fork_parameters.capella),
        ("deneb", &fork_parameters.deneb),
        ("electra", &fork_parameters.electra),
        ("fulu", &fork_parameters.fulu),
        ("gloas", &fork_parameters.gloas),
    ]
}

/// Checks that no fork activates before the one that precedes it.
fn validate_fork_order(fork_parameters: &ForkParameters) -> Result<(), ContractError> {
    for pair in forks(fork_parameters).windows(2) {
        let ((_, previous), (name, fork)) = (pair[0], pair[1]);
        ensure!(
            previous.epoch <= fork.epoch,
            ContractError::InvalidForkParameters {
                fork: name,
                reason: "activates before the fork that precedes it",
            }
        );
    }
    Ok(())
}

/// Validates replacing the fork parameters of a running client.
///
/// The fork version selects the signing domain, so changing one that is already active would make
/// every signature the client has accepted, and every one it will be shown, verify against a
/// different domain. Only forks that have not activated yet can be scheduled or moved, and only to
/// an epoch after the client's latest one.
/// # Errors
/// Returns an error if the new parameters are out of order, change the genesis parameters or a
/// fork that is already active, or move a scheduled fork to an epoch the client has passed.
pub fn validate_fork_parameters_update(
    client_state: &EthClientState,
    new: &ForkParameters,
) -> Result<(), ContractError> {
    validate_fork_order(new)?;

    let current = &client_state.fork_parameters;
    ensure!(
        new.genesis_fork_version == current.genesis_fork_version
            && new.genesis_slot == current.genesis_slot,
        ContractError::InvalidForkParameters {
            fork: "genesis",
            reason: "the genesis fork parameters cannot change",
        }
    );

    let latest_epoch = client_state.compute_epoch_at_slot(client_state.latest_slot);
    for ((name, current), (_, new)) in forks(current).into_iter().zip(forks(new)) {
        if current.epoch <= latest_epoch {
            ensure!(
                current == new,
                ContractError::InvalidForkParameters {
                    fork: name,
                    reason: "an active fork cannot change",
                }
            );
        } else {
            ensure!(
                new.epoch > latest_epoch,
                ContractError::InvalidForkParameters {
                    fork: name,
                    reason: "a fork that has not activated cannot move to an epoch the client has passed",
                }
            );
        }
    }
    Ok(())
}
