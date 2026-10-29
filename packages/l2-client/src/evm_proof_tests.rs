//! Adversarial tests for bounded EVM proof containers.

use alloy_primitives::{keccak256, B256};
use rlp::RlpStream;

use crate::{
    error::Error,
    evm_proof::{verify_bounded_storage_value, ProofLimits},
    msg::EvmStorageProof,
};

fn limits() -> ProofLimits {
    ProofLimits {
        max_account_nodes: 2,
        max_storage_nodes: 2,
        max_node_bytes: 4,
    }
}

fn proof(key: B256, nodes: Vec<Vec<u8>>) -> EvmStorageProof {
    EvmStorageProof {
        key,
        value: Vec::new(),
        proof: nodes,
    }
}

#[test]
fn rejects_excessive_node_count_and_size_before_traversal() {
    let too_many = proof(B256::ZERO, vec![vec![], vec![], vec![]]);
    let too_large = proof(B256::ZERO, vec![vec![0; 5]]);

    assert!(matches!(
        limits().validate_storage(&too_many),
        Err(Error::ProofLimit {
            limit: "proof node count",
            ..
        })
    ));
    assert!(matches!(
        limits().validate_storage(&too_large),
        Err(Error::ProofLimit {
            limit: "RLP node byte length",
            ..
        })
    ));
}

#[test]
fn rejects_malformed_rlp_before_traversal() {
    let malformed = proof(B256::ZERO, vec![vec![0xb8]]);
    assert!(matches!(
        limits().validate_storage(&malformed),
        Err(Error::Proof(message)) if message == "malformed RLP trie node"
    ));
}

#[test]
fn rlp_encodes_decoded_storage_words_before_trie_lookup() {
    let key = B256::with_last_byte(7);
    let value = vec![0x11; 32];
    let mut path = Vec::with_capacity(33);
    path.push(0x20);
    path.extend_from_slice(keccak256(key).as_slice());
    let encoded_value = rlp::encode(&value.as_slice());
    let mut leaf = RlpStream::new_list(2);
    leaf.append(&path);
    leaf.append(&encoded_value.as_ref());
    let leaf = leaf.out().to_vec();
    let root: [u8; 32] = keccak256(&leaf).into();
    let proof = EvmStorageProof {
        key,
        value,
        proof: vec![leaf],
    };

    verify_bounded_storage_value(
        ProofLimits {
            max_account_nodes: 1,
            max_storage_nodes: 1,
            max_node_bytes: 1024,
        },
        &root,
        &proof,
    )
    .unwrap();
}
