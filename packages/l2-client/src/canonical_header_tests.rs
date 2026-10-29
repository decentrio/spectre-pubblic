//! Tests for strict canonical L2 header validation and hashing.

use alloy_primitives::{Address, Bloom, Bytes, FixedBytes, B256, U256};

use crate::{
    canonical_header::{CanonicalEvmHeader, ExecutionHeaderFork},
    error::Error,
};

fn header() -> CanonicalEvmHeader {
    CanonicalEvmHeader {
        parent_hash: B256::with_last_byte(1),
        ommers_hash: B256::with_last_byte(2),
        beneficiary: Address::with_last_byte(3),
        state_root: B256::with_last_byte(4),
        transactions_root: B256::with_last_byte(5),
        receipts_root: B256::with_last_byte(6),
        logs_bloom: Bloom::ZERO,
        difficulty: U256::ZERO,
        number: 7,
        gas_limit: 30_000_000,
        gas_used: 21_000,
        timestamp: 1_700_000_000,
        extra_data: Bytes::new(),
        mix_hash: B256::with_last_byte(8),
        nonce: FixedBytes::ZERO,
        base_fee_per_gas: Some(U256::from(9)),
        withdrawals_root: Some(B256::with_last_byte(10)),
        blob_gas_used: Some(11),
        excess_blob_gas: Some(12),
        parent_beacon_block_root: Some(B256::with_last_byte(13)),
        requests_hash: Some(B256::with_last_byte(14)),
    }
}

#[test]
fn hashes_the_canonical_prague_field_order_deterministically() {
    let header = header();
    let rlp = header.rlp_bytes(ExecutionHeaderFork::Prague).unwrap();
    assert!(rlp.first().is_some_and(|prefix| prefix >= &0xf8));
    // This vector is deliberately fixed so accidental changes to RLP ordering
    // (including optional post-Cancun fields) cannot silently self-confirm.
    assert_eq!(
        header.hash(ExecutionHeaderFork::Prague).unwrap(),
        "0x22ffbd4be33e618a3687c5b7dd88e333b466a0dd8d197528970c4a287ec42f6e"
            .parse::<B256>()
            .unwrap()
    );
}

#[test]
fn rejects_partial_cancun_fields_and_oversized_extra_data() {
    let mut incomplete = header();
    incomplete.excess_blob_gas = None;
    assert!(matches!(
        incomplete.validate_for_fork(ExecutionHeaderFork::Cancun),
        Err(Error::InvalidEvmHeader(
            "optional fields do not match the pinned fork"
        ))
    ));

    let mut oversized = header();
    oversized.extra_data = Bytes::from(vec![0; CanonicalEvmHeader::MAX_EXTRA_DATA_BYTES + 1]);
    assert!(matches!(
        oversized.validate_for_fork(ExecutionHeaderFork::Prague),
        Err(Error::InvalidEvmHeader("extra_data exceeds 32 bytes"))
    ));
}

#[test]
fn rejects_fields_from_a_later_fork() {
    assert!(matches!(
        header().validate_for_fork(ExecutionHeaderFork::London),
        Err(Error::InvalidEvmHeader(
            "optional fields do not match the pinned fork"
        ))
    ));
}
