//! Generic entrypoint implementation used by the chain-specific artifacts.

#![allow(clippy::needless_pass_by_value)]

use cosmwasm_std::{to_json_binary, Binary, Deps, DepsMut, Env, Response};

use crate::{
    error::Error,
    msg::{
        CheckForMisbehaviourResult, ClientMessage, IbcHeight, InstantiateMsg, MigrateMsg, QueryMsg,
        SignedAttestedL2Header, StatusResult, SudoMsg, TimestampAtHeightResult,
    },
    runtime,
    state::{ClientState, ConsensusState},
    L2LightClient,
};

/// Initializes a chain-specific artifact using explicitly trusted bootstrap state.
pub fn instantiate<Adapter: L2LightClient>(
    deps: DepsMut,
    env: &Env,
    msg: InstantiateMsg,
) -> Result<Response, Error> {
    let client: ClientState<Adapter::Profile> = serde_json::from_slice(&msg.client_state)?;
    let consensus: ConsensusState = serde_json::from_slice(&msg.consensus_state)?;
    runtime::instantiate(
        deps.storage,
        &client,
        &consensus,
        msg.checksum.to_vec(),
        env.block.time.seconds(),
    )?;
    Ok(Response::default())
}

/// Validates the current client and optionally rotates only its active attestor set.
pub fn migrate<Adapter: L2LightClient>(deps: DepsMut, msg: MigrateMsg) -> Result<Response, Error> {
    runtime::migrate::<Adapter::Profile>(deps.storage, msg)?;
    Ok(Response::default())
}

/// Executes a host sudo operation without emitting attributes, events, or messages.
///
/// No attributes, events, or messages may be attached to this response: ibc-go's 08-wasm keeper
/// rejects a light client that returns any of them ("returning attributes from a contract is not
/// allowed"), and it does so by panicking inside the VM call, so the whole tx fails rather than
/// the update. A light client is a pure state transition; observability has to come from the
/// caller or from querying the client state afterwards.
pub fn sudo<Adapter: L2LightClient>(
    deps: DepsMut,
    env: &Env,
    msg: SudoMsg,
) -> Result<Response, Error> {
    let data = match msg {
        SudoMsg::UpdateState { client_message } => {
            let header = decode_header::<Adapter>(&client_message, deps.as_ref())?;
            runtime::update::<Adapter::Profile>(deps.storage, &header, env.block.time.seconds())?
        }
        SudoMsg::UpdateStateOnMisbehaviour { client_message } => {
            let (first, second) = decode_misbehaviour::<Adapter>(&client_message, deps.as_ref())?;
            if !runtime::apply_misbehaviour::<Adapter::Profile>(deps.storage, &first, &second)? {
                return Err(Error::InvalidHeader("headers do not prove misbehaviour"));
            }
            Binary::default()
        }
        SudoMsg::VerifyMembership {
            height,
            delay_time_period,
            delay_block_period,
            proof,
            merkle_path,
            value,
        } => {
            ensure_zero_delay(delay_time_period, delay_block_period)?;
            validate_height(&height)?;
            runtime::verify_membership::<Adapter::Profile>(
                deps.storage,
                height.revision_height,
                &merkle_path.key_path,
                &value,
                &proof,
            )?;
            Binary::default()
        }
        SudoMsg::VerifyNonMembership {
            height,
            delay_time_period,
            delay_block_period,
            proof,
            merkle_path,
        } => {
            ensure_zero_delay(delay_time_period, delay_block_period)?;
            validate_height(&height)?;
            runtime::verify_non_membership::<Adapter::Profile>(
                deps.storage,
                height.revision_height,
                &merkle_path.key_path,
                &proof,
            )?;
            Binary::default()
        }
        SudoMsg::VerifyUpgradeAndUpdateState { .. } => {
            return Err(Error::UnsupportedLifecycleOperation {
                operation: "verify_upgrade_and_update_state",
            });
        }
        SudoMsg::MigrateClientStore {} => {
            return Err(Error::UnsupportedLifecycleOperation {
                operation: "migrate_client_store",
            });
        }
    };
    Ok(Response::default().set_data(data))
}

const fn ensure_zero_delay(delay_time_period: u64, delay_block_period: u64) -> Result<(), Error> {
    if delay_time_period != 0 || delay_block_period != 0 {
        return Err(Error::UnsupportedNonZeroDelay {
            delay_time_period,
            delay_block_period,
        });
    }
    Ok(())
}

/// Executes a read-only host query.
pub fn query<Adapter: L2LightClient>(deps: Deps, msg: QueryMsg) -> Result<Binary, Error> {
    match msg {
        QueryMsg::VerifyClientMessage { client_message } => {
            match decode_client_message::<Adapter>(&client_message, deps)? {
                ClientMessage::Header(header) => {
                    header.header().consensus_state(0)?;
                }
                ClientMessage::Misbehaviour { header_1, header_2 } => {
                    if !runtime::is_actionable_misbehaviour(&header_1, &header_2)? {
                        return Err(Error::InvalidHeader("headers do not prove misbehaviour"));
                    }
                }
            }
            Ok(Binary::default())
        }
        QueryMsg::CheckForMisbehaviour { client_message } => {
            let (first, second) = decode_misbehaviour::<Adapter>(&client_message, deps)?;
            to_json_binary(&CheckForMisbehaviourResult {
                found_misbehaviour: runtime::is_actionable_misbehaviour(&first, &second)?,
            })
            .map_err(Into::into)
        }
        QueryMsg::TimestampAtHeight { height } => {
            validate_height(&height)?;
            to_json_binary(&TimestampAtHeightResult {
                timestamp: runtime::consensus_state(deps.storage, height.revision_height)?
                    .timestamp_nanos,
            })
            .map_err(Into::into)
        }
        QueryMsg::Status {} => {
            let client = runtime::client_state::<Adapter::Profile>(deps.storage)?;
            client.validate()?;
            to_json_binary(&StatusResult {
                status: if client.frozen_height.is_some() {
                    "Frozen"
                } else {
                    "Active"
                }
                .into(),
            })
            .map_err(Into::into)
        }
    }
}

fn decode_header<Adapter: L2LightClient>(
    data: &Binary,
    deps: Deps,
) -> Result<crate::verification::AuthenticatedHeader, Error> {
    let ClientMessage::Header(header) = decode_client_message::<Adapter>(data, deps)? else {
        return Err(Error::InvalidHeader("header envelope required"));
    };
    Ok(header)
}

fn decode_misbehaviour<Adapter: L2LightClient>(
    data: &Binary,
    deps: Deps,
) -> Result<
    (
        crate::verification::AuthenticatedHeader,
        crate::verification::AuthenticatedHeader,
    ),
    Error,
> {
    let ClientMessage::Misbehaviour { header_1, header_2 } =
        decode_client_message::<Adapter>(data, deps)?
    else {
        return Err(Error::InvalidHeader("misbehaviour envelope required"));
    };
    Ok((header_1, header_2))
}

fn decode_client_message<Adapter: L2LightClient>(
    data: &Binary,
    deps: Deps,
) -> Result<ClientMessage<crate::verification::AuthenticatedHeader>, Error> {
    let value: serde_json::Value = serde_json::from_slice(data)?;
    reject_unsigned_v1(&value)?;
    let message: ClientMessage<SignedAttestedL2Header> = serde_json::from_value(value)?;
    let client = runtime::client_state::<Adapter::Profile>(deps.storage)?;
    client.validate()?;
    if let Some(height) = client.frozen_height {
        return Err(Error::Frozen(height));
    }
    match message {
        ClientMessage::Header(header) => Ok(ClientMessage::Header(Adapter::verify(
            deps.api, &client, &header,
        )?)),
        ClientMessage::Misbehaviour { header_1, header_2 } => {
            let (header_1, header_2) =
                Adapter::verify_misbehaviour(deps.api, &client, &header_1, &header_2)?;
            Ok(ClientMessage::Misbehaviour { header_1, header_2 })
        }
    }
}

fn reject_unsigned_v1(value: &serde_json::Value) -> Result<(), Error> {
    let Some(message_type) = value.get("type").and_then(serde_json::Value::as_str) else {
        return Ok(());
    };
    let Some(payload) = value.get("value") else {
        return Ok(());
    };
    let missing = match message_type {
        "header" => payload.get("attestor_signature").is_none(),
        "misbehaviour" => ["header_1", "header_2"].iter().any(|field| {
            payload
                .get(field)
                .is_some_and(|header| header.get("attestor_signature").is_none())
        }),
        _ => false,
    };
    if missing {
        return Err(Error::UnsupportedUnsignedHeader);
    }
    Ok(())
}

const fn validate_height(height: &IbcHeight) -> Result<(), Error> {
    if height.revision_number != 0 {
        return Err(Error::InvalidRevision(height.revision_number));
    }
    if height.revision_height == 0 {
        return Err(Error::ZeroHeight);
    }
    Ok(())
}
