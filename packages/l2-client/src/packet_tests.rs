use alloy_primitives::{keccak256, B256};
use serde::Deserialize;

use crate::{
    error::Error,
    msg::EvmStorageProof,
    packet::{self, commitment_path_hash, commitment_storage_slot_at},
};

#[test]
fn derives_the_solidity_mapping_slot_from_the_erc7201_namespace() {
    let path_hash = B256::with_last_byte(7);
    let mut preimage = [0_u8; 64];
    preimage[..32].copy_from_slice(path_hash.as_slice());
    let commitment_slot = B256::with_last_byte(9);
    preimage[32..].copy_from_slice(commitment_slot.as_slice());
    assert_eq!(
        commitment_storage_slot_at(path_hash, commitment_slot),
        keccak256(preimage)
    );
}

#[test]
fn matches_the_shared_solidity_storage_layout_fixture() {
    #[derive(Deserialize)]
    struct Fixture {
        path_hash: B256,
        commitment_slot: B256,
        storage_key: B256,
    }

    let fixture: Fixture = serde_json::from_slice(include_bytes!(
        "../../../test/fixtures/wasm-contracts/solidity-storage-layout.json"
    ))
    .unwrap();
    assert_eq!(
        commitment_storage_slot_at(fixture.path_hash, fixture.commitment_slot),
        fixture.storage_key
    );
}

#[test]
fn rejects_empty_commitment_paths() {
    assert!(commitment_path_hash(&[]).is_err());
    assert!(commitment_path_hash(b"commitments/test").is_err());
}

#[test]
fn rejects_oversized_paths_and_values_before_hashing_or_trie_work() {
    let proof = EvmStorageProof {
        key: B256::ZERO,
        value: vec![0; 33],
        proof: vec![],
    };
    assert!(matches!(
        packet::verify_membership(
            B256::with_last_byte(1),
            B256::with_last_byte(2),
            &vec![b'a'; 257],
            &[0; 33],
            &proof,
        ),
        Err(Error::ProofLimit {
            limit: "IBC commitment path byte length",
            maximum: 256,
        })
    ));
}

#[test]
fn accepts_router_binary_packet_paths() {
    for path_type in 1..=3 {
        let mut path = b"08-wasm-7".to_vec();
        path.push(path_type);
        path.extend_from_slice(&42_u64.to_be_bytes());
        assert_eq!(
            commitment_path_hash(&path).unwrap(),
            keccak256(path.as_slice())
        );
    }

    let mut unsupported = b"08-wasm-7".to_vec();
    unsupported.push(4);
    unsupported.extend_from_slice(&42_u64.to_be_bytes());
    assert!(commitment_path_hash(&unsupported).is_err());
    let mut non_utf8_client = vec![0xff, 1];
    non_utf8_client.extend_from_slice(&42_u64.to_be_bytes());
    assert!(commitment_path_hash(&non_utf8_client).is_err());
}
