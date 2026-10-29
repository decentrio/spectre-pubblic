//! This module defines types related to the Header we use for the Ethereum light client

use alloy_primitives::Bytes;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use ethereum_types::consensus::{
    light_client_header::LightClientUpdate, sync_committee::SyncCommittee,
};

/// The header of a light client update
#[derive(Serialize, Deserialize, JsonSchema, PartialEq, Eq, Clone, Debug)]
pub struct Header {
    /// The active sync committee (untrusted)
    pub active_sync_committee: ActiveSyncCommittee,
    /// The consensus update
    pub consensus_update: LightClientUpdate,
    /// Trusted slot to verify the new update against
    // The client **must** have a consensus state for the provided slot
    pub trusted_slot: u64,
    /// RLP-encoded execution block header of the finalized header (untrusted).
    ///
    /// Required exactly when `consensus_update.finalized_header` is in the Gloas format, where the
    /// light client data proves only the execution block hash. The light client takes the
    /// execution state root, timestamp and block number from this header after checking that it
    /// hashes to that proven block hash.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[schemars(with = "Option<String>")]
    pub execution_header_rlp: Option<Bytes>,
}

/// The active sync committee
#[derive(Serialize, Deserialize, JsonSchema, PartialEq, Eq, Clone, Debug)]
pub enum ActiveSyncCommittee {
    /// The current sync committee
    Current(SyncCommittee),
    /// The next sync committee
    Next(SyncCommittee),
}
