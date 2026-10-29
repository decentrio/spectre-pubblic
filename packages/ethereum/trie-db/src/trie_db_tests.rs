//! Regression tests for malformed trie proof handling.

use alloy_primitives::{Address, B256};

use crate::trie_db::verify_account;

#[test]
fn malformed_or_empty_proofs_return_errors_without_panicking() {
    assert!(verify_account(B256::ZERO, Address::ZERO, Vec::<Vec<u8>>::new()).is_err());
    assert!(verify_account(B256::ZERO, Address::ZERO, vec![vec![0xff]]).is_err());
}
