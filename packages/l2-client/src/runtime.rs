//! Storage-backed L2 client lifecycle.

use cosmwasm_std::{to_json_binary, Binary, Storage};
use ibc_proto::{
    google::protobuf::Any,
    ibc::{
        core::client::v1::Height,
        lightclients::wasm::v1::{
            ClientState as WasmClientState, ConsensusState as WasmConsensusState,
        },
    },
};
use prost::Message;
use serde::{de::DeserializeOwned, Deserialize, Serialize};

use crate::{
    error::Error,
    msg::{EvmStorageProof, IbcHeight, MigrateMsg, UpdateStateResult},
    packet,
    state::{AttestorConfig, ClientState, ConsensusState, RuntimeProfile},
    store::{
        get_wasm_client_state, get_wasm_consensus_state, may_get_wasm_consensus_state,
        HOST_CLIENT_STATE_KEY,
    },
    verification::AuthenticatedHeader,
};

/// Fully validated logical effects of one L2 update before host encoding.
#[derive(Debug, PartialEq, Eq)]
pub(crate) struct UpdatePlan<Profile> {
    client: Option<ClientState<Profile>>,
    consensus: Option<(u64, ConsensusState)>,
}

impl<Profile> UpdatePlan<Profile> {
    const fn empty() -> Self {
        Self {
            client: None,
            consensus: None,
        }
    }

    #[cfg(test)]
    pub(crate) const fn client(&self) -> Option<&ClientState<Profile>> {
        self.client.as_ref()
    }

    #[cfg(test)]
    pub(crate) fn consensus(&self) -> Option<(u64, &ConsensusState)> {
        self.consensus
            .as_ref()
            .map(|(height, consensus)| (*height, consensus))
    }
}

/// Complete host-encoded effects of one update.
///
/// Construction can fail, but committing cannot: every protobuf/JSON envelope, key, reported
/// height, and response byte is prepared before the first storage write.
pub(crate) struct StateTransition {
    client_envelope: Option<Vec<u8>>,
    consensus_write: Option<(Vec<u8>, Vec<u8>)>,
    reported_heights: Vec<u64>,
    response_data: Binary,
}

impl StateTransition {
    #[cfg(test)]
    pub(crate) fn client_envelope(&self) -> Option<&[u8]> {
        self.client_envelope.as_deref()
    }

    #[cfg(test)]
    pub(crate) fn consensus_write(&self) -> Option<(&[u8], &[u8])> {
        self.consensus_write
            .as_ref()
            .map(|(key, value)| (key.as_slice(), value.as_slice()))
    }

    #[cfg(test)]
    pub(crate) fn reported_heights(&self) -> &[u64] {
        &self.reported_heights
    }
}

/// Stores explicitly trusted bootstrap state.
pub fn instantiate<Profile>(
    storage: &mut dyn Storage,
    client: &ClientState<Profile>,
    consensus: &ConsensusState,
    checksum: Vec<u8>,
    accepted_at: u64,
) -> Result<(), Error>
where
    Profile: Clone + Serialize + RuntimeProfile,
{
    client.validate()?;
    client.attestors.validate_for_install()?;
    consensus.validate()?;
    if consensus.l2_height != client.latest_height {
        return Err(Error::InvalidHeader(
            "bootstrap consensus height differs from client height",
        ));
    }

    let height = client.latest_height;
    let mut consensus = consensus.clone();
    consensus.first_accepted_at = accepted_at;
    let client = Any::from_msg(&WasmClientState {
        checksum,
        data: serde_json::to_vec(client)?,
        latest_height: Some(Height {
            revision_number: 0,
            revision_height: client.latest_height,
        }),
    })?
    .encode_to_vec();
    let consensus = Any::from_msg(&WasmConsensusState {
        data: serde_json::to_vec(&consensus)?,
    })?
    .encode_to_vec();

    storage.set(HOST_CLIENT_STATE_KEY.as_bytes(), &client);
    storage.set(
        crate::store::consensus_db_key(height).as_bytes(),
        &consensus,
    );
    Ok(())
}

/// Applies the only supported governance migration: keep or atomically replace
/// the active attestor set inside the existing client state.
pub fn migrate<Profile>(storage: &mut dyn Storage, msg: MigrateMsg) -> Result<(), Error>
where
    Profile: DeserializeOwned + Serialize + RuntimeProfile,
{
    let mut client = migration_client_state::<Profile>(storage)?;
    match msg {
        MigrateMsg::KeepAttestors {} => Ok(()),
        MigrateMsg::ReplaceAttestors {
            public_keys,
            threshold,
        } => {
            let replacement = AttestorConfig {
                public_keys,
                threshold,
            };
            replacement.validate_for_install()?;
            client.attestors = replacement;
            client.validate()?;

            // Prepare every fallible decode/encode before the single host-key write.
            // The outer checksum, latest height, profile and frozen state are retained.
            let envelope = prepare_client_state_envelope(storage, &client)?;
            storage.set(HOST_CLIENT_STATE_KEY.as_bytes(), &envelope);
            Ok(())
        }
    }
}

/// One-pass decoder for the current client-state wire shape. `attestors` is optional
/// only while decoding so an older unsigned development state gets the stable typed
/// error below; it is never accepted or promoted by migration.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MigrationClientStateWire<Profile> {
    latest_height: u64,
    frozen_height: Option<u64>,
    profile: Profile,
    attestors: Option<AttestorConfig>,
}

fn migration_client_state<Profile>(storage: &dyn Storage) -> Result<ClientState<Profile>, Error>
where
    Profile: DeserializeOwned + RuntimeProfile,
{
    let envelope = get_wasm_client_state(storage)?;
    let wire: MigrationClientStateWire<Profile> = serde_json::from_slice(&envelope.data)?;
    let client = ClientState {
        latest_height: wire.latest_height,
        frozen_height: wire.frozen_height,
        profile: wire.profile,
        attestors: wire.attestors.ok_or(Error::UnsupportedLegacyClientState)?,
    };
    validate_client_state_envelope(&envelope, client.latest_height)?;
    client.validate()?;
    Ok(client)
}

const fn validate_client_state_envelope(
    envelope: &WasmClientState,
    latest_height: u64,
) -> Result<(), Error> {
    let Some(height) = &envelope.latest_height else {
        return Err(Error::ClientStateHeightMismatch);
    };
    if height.revision_number != 0 || height.revision_height != latest_height {
        return Err(Error::ClientStateHeightMismatch);
    }
    Ok(())
}

/// Loads client state from the host envelope.
pub fn client_state<Profile: DeserializeOwned>(
    storage: &dyn Storage,
) -> Result<ClientState<Profile>, Error> {
    let envelope = get_wasm_client_state(storage)?;
    let client: ClientState<Profile> = serde_json::from_slice(&envelope.data)?;
    let Some(height) = envelope.latest_height else {
        return Err(Error::ClientStateHeightMismatch);
    };
    if height.revision_number != 0 || height.revision_height != client.latest_height {
        return Err(Error::ClientStateHeightMismatch);
    }
    Ok(client)
}

/// Loads an exact consensus state.
pub fn consensus_state(storage: &dyn Storage, height: u64) -> Result<ConsensusState, Error> {
    let state: ConsensusState =
        serde_json::from_slice(&get_wasm_consensus_state(storage, height)?.data)?;
    state.validate()?;
    if state.l2_height != height {
        return Err(Error::ConsensusStateHeightMismatch {
            expected: height,
            found: state.l2_height,
        });
    }
    Ok(state)
}

/// Applies a verified update and returns its already-encoded host response.
pub fn update<Profile>(
    storage: &mut dyn Storage,
    header: &AuthenticatedHeader,
    accepted_at: u64,
) -> Result<Binary, Error>
where
    Profile: DeserializeOwned + Serialize + RuntimeProfile,
{
    let client = client_state::<Profile>(storage)?;
    client.validate()?;
    let height = header.header().height.revision_height;
    let existing = may_load_consensus_state(storage, height)?;
    let previous = if height > 1 {
        may_load_consensus_state(storage, height - 1)?
    } else {
        None
    };
    let next = height
        .checked_add(1)
        .map(|next_height| may_load_consensus_state(storage, next_height))
        .transpose()?
        .flatten();

    let plan = build_update_plan(
        client,
        header,
        existing.as_ref(),
        previous.as_ref(),
        next.as_ref(),
        accepted_at,
    )?;
    let transition = prepare_transition(storage, &plan)?;
    Ok(commit_transition(storage, transition))
}

/// Returns whether two headers constitute actionable, authenticated misbehaviour.
pub fn is_actionable_misbehaviour(
    first: &AuthenticatedHeader,
    second: &AuthenticatedHeader,
) -> Result<bool, Error> {
    first.header().conflicts_with(second.header())
}

/// Applies actionable misbehaviour evidence.
pub fn apply_misbehaviour<Profile>(
    storage: &mut dyn Storage,
    first: &AuthenticatedHeader,
    second: &AuthenticatedHeader,
) -> Result<bool, Error>
where
    Profile: DeserializeOwned + Serialize + RuntimeProfile,
{
    let mut client = client_state::<Profile>(storage)?;
    client.validate()?;
    if let Some(height) = client.frozen_height {
        return Err(Error::Frozen(height));
    }
    let found = is_actionable_misbehaviour(first, second)?;
    if !found {
        return Ok(false);
    }
    client.frozen_height = Some(first.header().height.revision_height);
    store_client_state(storage, &client)?;
    Ok(true)
}

/// Verifies packet membership against an exact stored root.
pub fn verify_membership<Profile: DeserializeOwned + RuntimeProfile>(
    storage: &dyn Storage,
    height: u64,
    paths: &[Binary],
    value: &[u8],
    proof: &[u8],
) -> Result<(), Error> {
    let client = active_client::<Profile>(storage)?;
    let consensus = consensus_state(storage, height)?;
    let path = single_path(paths)?;
    let proof: EvmStorageProof = serde_json::from_slice(proof)?;
    packet::verify_membership(
        consensus.ibc_storage_root,
        client.profile.common().commitment_slot,
        path,
        value,
        &proof,
    )
}

/// Verifies packet non-membership against an exact stored root.
pub fn verify_non_membership<Profile: DeserializeOwned + RuntimeProfile>(
    storage: &dyn Storage,
    height: u64,
    paths: &[Binary],
    proof: &[u8],
) -> Result<(), Error> {
    let client = active_client::<Profile>(storage)?;
    let consensus = consensus_state(storage, height)?;
    let path = single_path(paths)?;
    let proof: EvmStorageProof = serde_json::from_slice(proof)?;
    packet::verify_non_membership(
        consensus.ibc_storage_root,
        client.profile.common().commitment_slot,
        path,
        &proof,
    )
}

fn single_path(paths: &[Binary]) -> Result<&Binary, Error> {
    let [path] = paths else {
        return Err(Error::Proof(
            "exactly one IBC commitment path is required".into(),
        ));
    };
    Ok(path)
}

fn active_client<Profile: DeserializeOwned + RuntimeProfile>(
    storage: &dyn Storage,
) -> Result<ClientState<Profile>, Error> {
    let client = client_state(storage)?;
    client.validate()?;
    if let Some(height) = client.frozen_height {
        return Err(Error::Frozen(height));
    }
    Ok(client)
}

pub(crate) fn build_update_plan<Profile>(
    mut client: ClientState<Profile>,
    authenticated_header: &AuthenticatedHeader,
    existing: Option<&ConsensusState>,
    previous: Option<&ConsensusState>,
    next: Option<&ConsensusState>,
    accepted_at: u64,
) -> Result<UpdatePlan<Profile>, Error> {
    let header = authenticated_header.header();
    if let Some(height) = client.frozen_height {
        return Err(Error::Frozen(height));
    }

    let incoming = header.consensus_state(accepted_at)?;
    let height = header.height.revision_height;

    if existing.is_some_and(|stored| stored.conflicts_with(&incoming)) {
        return Err(Error::StateConflict { height });
    }

    if previous.is_some_and(|state| header.parent_hash != state.l2_block_hash)
        || next.is_some_and(|state| state.parent_hash != header.l2_block_hash)
    {
        return Err(Error::ParentHashMismatch);
    }

    if existing.is_some() {
        // conflicts_with() was false, so every persisted block-identity field matches: this is a
        // re-submission of a block already stored, with nothing to update. First write wins, so
        // the stored state does not depend on arrival order. An honest relayer re-sends the same
        // block routinely (a retry or a cached header), and neither should rewrite anything.
        //
        // The signed-attestation wire format grows this branch into the redesign's promotion and
        // provisional-replacement state machine.
        return Ok(UpdatePlan::empty());
    }

    client.latest_height = client.latest_height.max(height);
    Ok(UpdatePlan {
        client: Some(client),
        consensus: Some((height, incoming)),
    })
}

fn commit_transition(storage: &mut dyn Storage, transition: StateTransition) -> Binary {
    let StateTransition {
        client_envelope,
        consensus_write,
        reported_heights,
        response_data,
    } = transition;
    drop(reported_heights);
    if let Some((key, value)) = consensus_write {
        storage.set(&key, &value);
    }
    if let Some(value) = client_envelope {
        storage.set(HOST_CLIENT_STATE_KEY.as_bytes(), &value);
    }
    response_data
}

pub(crate) fn prepare_transition<Profile: Serialize>(
    storage: &dyn Storage,
    plan: &UpdatePlan<Profile>,
) -> Result<StateTransition, Error> {
    let client_envelope = plan
        .client
        .as_ref()
        .map(|client| -> Result<Vec<u8>, Error> {
            let mut envelope = get_wasm_client_state(storage)?;
            envelope.data = serde_json::to_vec(client)?;
            envelope.latest_height = Some(Height {
                revision_number: 0,
                revision_height: client.latest_height,
            });
            Ok(Any::from_msg(&envelope)?.encode_to_vec())
        })
        .transpose()?;

    let consensus_write = plan
        .consensus
        .as_ref()
        .map(|(height, consensus)| -> Result<(Vec<u8>, Vec<u8>), Error> {
            let envelope = Any::from_msg(&WasmConsensusState {
                data: serde_json::to_vec(consensus)?,
            })?;
            Ok((
                crate::store::consensus_db_key(*height).into_bytes(),
                envelope.encode_to_vec(),
            ))
        })
        .transpose()?;

    let reported_heights = plan
        .consensus
        .as_ref()
        .map(|(height, _)| vec![*height])
        .unwrap_or_default();
    let response_data = to_json_binary(&UpdateStateResult {
        heights: reported_heights
            .iter()
            .copied()
            .map(|revision_height| IbcHeight {
                revision_number: 0,
                revision_height,
            })
            .collect(),
    })?;

    Ok(StateTransition {
        client_envelope,
        consensus_write,
        reported_heights,
        response_data,
    })
}

fn may_load_consensus_state(
    storage: &dyn Storage,
    height: u64,
) -> Result<Option<ConsensusState>, Error> {
    may_get_wasm_consensus_state(storage, height)?
        .map(|state| {
            let state: ConsensusState = serde_json::from_slice(&state.data)?;
            state.validate()?;
            if state.l2_height != height {
                return Err(Error::ConsensusStateHeightMismatch {
                    expected: height,
                    found: state.l2_height,
                });
            }
            Ok(state)
        })
        .transpose()
}

fn store_client_state<Profile: Serialize>(
    storage: &mut dyn Storage,
    client: &ClientState<Profile>,
) -> Result<(), Error> {
    let envelope = prepare_client_state_envelope(storage, client)?;
    storage.set(HOST_CLIENT_STATE_KEY.as_bytes(), &envelope);
    Ok(())
}

fn prepare_client_state_envelope<Profile: Serialize>(
    storage: &dyn Storage,
    client: &ClientState<Profile>,
) -> Result<Vec<u8>, Error> {
    let mut envelope = get_wasm_client_state(storage)?;
    envelope.data = serde_json::to_vec(client)?;
    envelope.latest_height = Some(Height {
        revision_number: 0,
        revision_height: client.latest_height,
    });
    Ok(Any::from_msg(&envelope)?.encode_to_vec())
}
