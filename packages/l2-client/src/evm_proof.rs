//! Bounded EVM proof validation before trie traversal.

use alloy_primitives::{Address, B256};
use ethereum_trie_db::trie_db::{
    verify_account, verify_storage_exclusion_proof, verify_storage_inclusion_proof, Account,
};

use crate::{
    error::Error,
    msg::{EvmAccountProof, EvmStorageProof},
};

/// Explicit upper bounds applied before hashing or RLP decoding untrusted proofs.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ProofLimits {
    /// Maximum nodes in an account proof.
    pub max_account_nodes: usize,
    /// Maximum nodes in a storage proof.
    pub max_storage_nodes: usize,
    /// Maximum encoded size of an individual RLP node.
    pub max_node_bytes: usize,
}

impl ProofLimits {
    /// Validates only cheap count and byte-length account-proof bounds.
    /// RLP shape validation remains part of proof verification after authentication.
    pub fn validate_account_bounds(self, proof: &EvmAccountProof) -> Result<(), Error> {
        validate_node_bounds(&proof.proof, self.max_account_nodes, self.max_node_bytes)
    }

    /// Validates an account proof before it is passed to the trie database.
    pub fn validate_account(self, proof: &EvmAccountProof) -> Result<(), Error> {
        validate_nodes(&proof.proof, self.max_account_nodes, self.max_node_bytes)
    }

    /// Validates a storage proof before it is passed to the trie database.
    pub fn validate_storage(self, proof: &EvmStorageProof) -> Result<(), Error> {
        validate_nodes(&proof.proof, self.max_storage_nodes, self.max_node_bytes)
    }
}

/// Verifies a configured account after enforcing caller-supplied proof limits.
pub fn verify_bounded_account(
    limits: ProofLimits,
    state_root: B256,
    address: Address,
    proof: &EvmAccountProof,
) -> Result<Account, Error> {
    limits.validate_account(proof)?;
    verify_account(state_root, address, &proof.proof)
        .map_err(|error| Error::Proof(error.to_string()))
}

/// Verifies an exact storage value after enforcing caller-supplied proof limits.
pub fn verify_bounded_storage_value(
    limits: ProofLimits,
    storage_root: &[u8; 32],
    proof: &EvmStorageProof,
) -> Result<(), Error> {
    limits.validate_storage(proof)?;
    let encoded_value = encode_storage_value(&proof.value)?;
    verify_storage_inclusion_proof(
        storage_root,
        proof.key.as_ref(),
        &encoded_value,
        &proof.proof,
    )
    .map_err(|error| Error::Proof(error.to_string()))
}

/// Verifies the EVM zero-value convention: a slot is absent, or is explicitly RLP-encoded zero.
pub fn verify_bounded_storage_zero(
    limits: ProofLimits,
    storage_root: &[u8; 32],
    proof: &EvmStorageProof,
) -> Result<(), Error> {
    limits.validate_storage(proof)?;

    match verify_storage_exclusion_proof(storage_root, proof.key.as_ref(), &proof.proof) {
        Ok(()) => Ok(()),
        Err(_) => {
            verify_storage_inclusion_proof(storage_root, proof.key.as_ref(), &[0x80], &proof.proof)
                .map_err(|error| Error::Proof(error.to_string()))
        }
    }
}

fn validate_nodes(nodes: &[Vec<u8>], max_nodes: usize, max_node_bytes: usize) -> Result<(), Error> {
    validate_node_bounds(nodes, max_nodes, max_node_bytes)?;
    for node in nodes {
        match rlp::Rlp::new(node).item_count() {
            Ok(2 | 17) => {}
            Ok(_) => return Err(Error::Proof("invalid RLP trie node shape".into())),
            Err(_) => return Err(Error::Proof("malformed RLP trie node".into())),
        }
    }
    Ok(())
}

fn validate_node_bounds(
    nodes: &[Vec<u8>],
    max_nodes: usize,
    max_node_bytes: usize,
) -> Result<(), Error> {
    if nodes.len() > max_nodes {
        return Err(Error::ProofLimit {
            limit: "proof node count",
            maximum: max_nodes,
        });
    }
    if nodes.iter().any(|node| node.len() > max_node_bytes) {
        return Err(Error::ProofLimit {
            limit: "RLP node byte length",
            maximum: max_node_bytes,
        });
    }
    Ok(())
}

fn encode_storage_value(value: &[u8]) -> Result<Vec<u8>, Error> {
    if value.len() > 32 {
        return Err(Error::Proof(
            "decoded EVM storage value exceeds 32 bytes".into(),
        ));
    }
    let first_nonzero = value
        .iter()
        .position(|byte| *byte != 0)
        .unwrap_or(value.len());
    Ok(rlp::encode(&&value[first_nonzero..]).to_vec())
}
