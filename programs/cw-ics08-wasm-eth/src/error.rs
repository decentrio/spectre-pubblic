//! Defines the [`ContractError`] type.

use cosmwasm_std::StdError;
use ethereum_light_client::error::EthereumIBCError;
use thiserror::Error;

#[derive(Error, Debug)]
#[allow(missing_docs, clippy::module_name_repetitions)]
pub enum ContractError {
    #[error("{0}")]
    Std(#[from] StdError),

    #[error("unauthorized")]
    Unauthorized,

    #[error("client state latest height and slot are not equal")]
    ClientStateSlotMismatch,

    #[error("client and consensus state mismatch")]
    ClientAndConsensusStateMismatch,

    #[error("serializing client state failed: {0}")]
    SerializeClientStateFailed(#[source] serde_json::Error),

    #[error("serializing consensus state failed: {0}")]
    SerializeConsensusStateFailed(#[source] serde_json::Error),

    #[error("deserializing client state failed: {0}")]
    DeserializeClientStateFailed(#[source] serde_json::Error),

    #[error("deserializing consensus state failed: {0}")]
    DeserializeConsensusStateFailed(#[source] serde_json::Error),

    #[error("deserializing client message failed: {0}")]
    DeserializeClientMessageFailed(#[source] serde_json::Error),

    #[error("deserializing ethereum misbehaviour message failed: {0}")]
    DeserializeEthMisbehaviourFailed(#[source] serde_json::Error),

    #[error("verify membership failed: {0}")]
    VerifyMembershipFailed(#[source] EthereumIBCError),

    #[error("verify non-membership failed: {0}")]
    VerifyNonMembershipFailed(#[source] EthereumIBCError),

    #[error("verify client message failed: {0}")]
    VerifyClientMessageFailed(#[source] EthereumIBCError),

    #[error("update client state failed: {0}")]
    UpdateClientStateFailed(#[source] EthereumIBCError),

    #[error("unsupported fork version")]
    UnsupportedForkVersion(#[source] EthereumIBCError),

    #[error("invalid Ethereum client state: {0}")]
    InvalidClientState(&'static str),

    #[error("invalid Ethereum consensus state: {0}")]
    InvalidConsensusState(&'static str),

    #[error("invalid fork parameters for {fork}: {reason}")]
    InvalidForkParameters {
        /// The fork the rule failed for.
        fork: &'static str,
        /// The rule that failed.
        reason: &'static str,
    },

    #[error("client state not found")]
    ClientStateNotFound,

    #[error("consensus state not found")]
    ConsensusStateNotFound,

    #[error("trusted slot {trusted_slot} is not the latest slot {latest_slot}")]
    TrustedSlotNotLatest {
        /// The slot the client message names as its trusted consensus state.
        trusted_slot: u64,
        /// The latest slot of the client.
        latest_slot: u64,
    },

    #[error("invalid revision number {0}; only revision zero is supported")]
    InvalidRevision(u64),

    #[error("invalid zero Ethereum height")]
    ZeroHeight,

    #[error("Ethereum timestamp overflow")]
    TimestampOverflow,

    #[error("client is frozen")]
    Frozen,

    // Generic translation errors
    #[error("prost encoding error: {0}")]
    ProstEncodeError(#[from] prost::EncodeError),

    #[error("prost decoding error: {0}")]
    ProstDecodeError(#[from] prost::DecodeError),

    #[error("serde json error: {0}")]
    SerdeJsonError(#[from] serde_json::Error),

    #[error("invalid client message")]
    InvalidClientMessage,

    #[error(
        "non-zero delay is unsupported: delay_time_period={delay_time_period}, delay_block_period={delay_block_period}"
    )]
    UnsupportedNonZeroDelay {
        /// Requested time delay.
        delay_time_period: u64,
        /// Requested block delay.
        delay_block_period: u64,
    },

    #[error("lifecycle operation is unsupported: {operation}")]
    UnsupportedLifecycleOperation {
        /// Stable host operation name.
        operation: &'static str,
    },
}
