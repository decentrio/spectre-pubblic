//!  packet commitment storage derivation for `ICS26Router`.

use alloy_primitives::{keccak256, B256};

use crate::{
    error::Error,
    evm_proof::{verify_bounded_storage_value, verify_bounded_storage_zero, ProofLimits},
    msg::EvmStorageProof,
};

/// Conservative limits for a single router proof.
pub const PROOF_LIMITS: ProofLimits = ProofLimits {
    max_account_nodes: 64,
    max_storage_nodes: 64,
    max_node_bytes: 32 * 1024,
};
const MAX_COMMITMENT_PATH_BYTES: usize = 256;
const MAX_COMMITMENT_VALUE_BYTES: usize = 32;

/// Derives the Solidity mapping slot for an already-hashed IBC commitment path.
///
/// `IBCStoreStorage.commitments` is its first field, so Solidity computes
/// `keccak256(abi.encode(path_hash, commitment_slot))`.
#[must_use]
pub fn commitment_storage_slot_at(path_hash: B256, commitment_slot: B256) -> B256 {
    let mut preimage = [0_u8; 64];
    preimage[..32].copy_from_slice(path_hash.as_slice());
    preimage[32..].copy_from_slice(commitment_slot.as_slice());
    keccak256(preimage)
}

/// Validates and hashes an `ICS26Router` binary packet path.
///
/// The router encodes these as `clientId || type || sequence_be`, where type is
/// one for commitments, two for receipts, or three for acknowledgements.
pub fn commitment_path_hash(path: &[u8]) -> Result<B256, Error> {
    const PATH_SUFFIX_LENGTH: usize = 9;
    let Some(client_id) = path.get(..path.len().saturating_sub(PATH_SUFFIX_LENGTH)) else {
        return Err(Error::Proof("unsupported IBC commitment path".into()));
    };
    let Some(path_type) = path.get(path.len().saturating_sub(PATH_SUFFIX_LENGTH)) else {
        return Err(Error::Proof("unsupported IBC commitment path".into()));
    };
    if client_id.is_empty()
        || std::str::from_utf8(client_id).is_err()
        || !matches!(path_type, 1..=3)
    {
        return Err(Error::Proof("unsupported IBC commitment path".into()));
    }
    Ok(keccak256(path))
}

/// Verifies a router commitment against the stored storage root.
pub fn verify_membership(
    storage_root: B256,
    commitment_slot: B256,
    path: &[u8],
    value: &[u8],
    proof: &EvmStorageProof,
) -> Result<(), Error> {
    validate_packet_bounds(path, value, proof)?;
    let expected_key = commitment_storage_slot_at(commitment_path_hash(path)?, commitment_slot);
    if proof.key != expected_key || proof.value != value {
        return Err(Error::Proof(
            "commitment proof key or value does not match request".into(),
        ));
    }
    let root: [u8; 32] = storage_root.into();
    verify_bounded_storage_value(PROOF_LIMITS, &root, proof)
}

/// Verifies a router commitment is absent or encoded as the EVM zero value.
pub fn verify_non_membership(
    storage_root: B256,
    commitment_slot: B256,
    path: &[u8],
    proof: &EvmStorageProof,
) -> Result<(), Error> {
    validate_packet_bounds(path, &[], proof)?;
    let expected_key = commitment_storage_slot_at(commitment_path_hash(path)?, commitment_slot);
    if proof.key != expected_key {
        return Err(Error::Proof(
            "commitment proof key does not match request".into(),
        ));
    }
    let root: [u8; 32] = storage_root.into();
    verify_bounded_storage_zero(PROOF_LIMITS, &root, proof)
}

fn validate_packet_bounds(path: &[u8], value: &[u8], proof: &EvmStorageProof) -> Result<(), Error> {
    if path.len() > MAX_COMMITMENT_PATH_BYTES {
        return Err(Error::ProofLimit {
            limit: "IBC commitment path byte length",
            maximum: MAX_COMMITMENT_PATH_BYTES,
        });
    }
    if value.len() > MAX_COMMITMENT_VALUE_BYTES || proof.value.len() > MAX_COMMITMENT_VALUE_BYTES {
        return Err(Error::ProofLimit {
            limit: "IBC commitment value byte length",
            maximum: MAX_COMMITMENT_VALUE_BYTES,
        });
    }
    PROOF_LIMITS.validate_storage(proof)
}
