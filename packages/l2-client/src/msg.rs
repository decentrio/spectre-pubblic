//! Shared wire messages and EVM proof containers.

use alloy_primitives::B256;
use cosmwasm_std::Binary;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::canonical_header::CanonicalEvmHeader;

/// Mandatory signed L2 update.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct SignedAttestedL2Header {
    /// Canonical L2 execution header.
    pub l2_header: CanonicalEvmHeader,
    /// Proof of the configured router account against the execution state root.
    pub router_proof: EvmAccountProof,
    /// Exactly-threshold signatures, strictly increasing by attestor index.
    ///
    /// The singular field name is retained from the original development wire
    /// contract even though quorum authentication makes its value a list.
    pub attestor_signature: Vec<IndexedAttestorSignature>,
}

/// One Ed25519 signature bound to its active-set index.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct IndexedAttestorSignature {
    pub attestor_index: u16,
    /// Raw 64-byte Ed25519 signature, encoded as base64 in JSON.
    pub signature: Binary,
}

/// Governance-hosted attestor migration operation.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub enum MigrateMsg {
    /// Validate the current client without rewriting it.
    KeepAttestors {},
    /// Atomically replace only the active attestor set.
    ReplaceAttestors {
        public_keys: Vec<Binary>,
        threshold: u16,
    },
}

/// Direct client-creation message.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct InstantiateMsg {
    pub client_state: Binary,
    pub consensus_state: Binary,
    pub checksum: Binary,
}

/// Host IBC height.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct IbcHeight {
    pub revision_number: u64,
    pub revision_height: u64,
}

/// Host Merkle path.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct MerklePath {
    pub key_path: Vec<Binary>,
}

/// Host sudo operations.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub enum SudoMsg {
    UpdateState {
        client_message: Binary,
    },
    UpdateStateOnMisbehaviour {
        client_message: Binary,
    },
    VerifyMembership {
        height: IbcHeight,
        delay_time_period: u64,
        delay_block_period: u64,
        proof: Binary,
        merkle_path: MerklePath,
        value: Binary,
    },
    VerifyNonMembership {
        height: IbcHeight,
        delay_time_period: u64,
        delay_block_period: u64,
        proof: Binary,
        merkle_path: MerklePath,
    },
    VerifyUpgradeAndUpdateState {
        upgrade_client_state: Binary,
        upgrade_consensus_state: Binary,
        proof_upgrade_client: Binary,
        proof_upgrade_consensus_state: Binary,
    },
    MigrateClientStore {},
}

/// Update result returned to the host.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct UpdateStateResult {
    pub heights: Vec<IbcHeight>,
}
/// Misbehaviour query result.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct CheckForMisbehaviourResult {
    pub found_misbehaviour: bool,
}
/// Timestamp query result.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct TimestampAtHeightResult {
    pub timestamp: u64,
}
/// Status query result.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct StatusResult {
    pub status: String,
}

/// Read-only host queries.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize, JsonSchema)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub enum QueryMsg {
    VerifyClientMessage { client_message: Binary },
    CheckForMisbehaviour { client_message: Binary },
    TimestampAtHeight { height: IbcHeight },
    Status {},
}

/// Bounded EVM account proof.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct EvmAccountProof {
    pub proof: Vec<Vec<u8>>,
}

/// Bounded EVM storage proof.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct EvmStorageProof {
    #[schemars(with = "String")]
    pub key: B256,
    pub value: Vec<u8>,
    pub proof: Vec<Vec<u8>>,
}

/// One unambiguous client-message envelope.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(
    tag = "type",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum ClientMessage<Header> {
    Header(Header),
    Misbehaviour { header_1: Header, header_2: Header },
}
