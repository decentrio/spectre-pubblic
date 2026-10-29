//! This module implements the sync protocol helpers defined in [consensus-specs](https://github.com/ethereum/consensus-specs)

use alloy_primitives::B256;
use ethereum_types::consensus::{
    light_client_header::LightClientHeader,
    merkle::{
        floorlog2, CURRENT_SYNC_COMMITTEE_GINDEX_ELECTRA, CURRENT_SYNC_COMMITTEE_GINDEX_GLOAS,
        EXECUTION_BLOCK_HASH_GINDEX_DENEB, EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
        EXECUTION_PAYLOAD_GINDEX, FINALIZED_ROOT_GINDEX_ELECTRA, FINALIZED_ROOT_GINDEX_GLOAS,
        NEXT_SYNC_COMMITTEE_GINDEX_ELECTRA, NEXT_SYNC_COMMITTEE_GINDEX_GLOAS,
    },
};
use tree_hash::TreeHash;

use crate::{
    client_state::ClientState,
    error::EthereumIBCError,
    trie::{is_valid_normalized_merkle_branch, validate_merkle_branch},
};

/// Checks that `branch` has exactly the depth of `gindex`.
///
/// The light client spec types each branch with the exact length of its fork, so a branch of any
/// other length is malformed rather than merely un-normalized.
/// # Errors
/// Returns an error if the branch length differs from `floorlog2(gindex)`.
pub const fn ensure_branch_length(
    name: &'static str,
    branch: &[B256],
    gindex: u64,
) -> Result<(), EthereumIBCError> {
    let expected = floorlog2(gindex);
    ensure!(
        branch.len() == expected,
        EthereumIBCError::InvalidBranchLength {
            name,
            expected,
            found: branch.len(),
        }
    );
    Ok(())
}

// See spec: https://github.com/ethereum/consensus-specs/blob/dev/specs/gloas/light-client/sync-protocol.md#modified-finalized_root_gindex_at_slot
/// Returns the finalized root gindex at the given slot.
/// # Errors
/// Returns an error if the epoch is in a non-supported fork.
pub fn finalized_root_gindex_at_slot(
    client_state: &ClientState,
    slot: u64,
) -> Result<u64, EthereumIBCError> {
    let epoch = client_state.compute_epoch_at_slot(slot);
    client_state.verify_supported_fork_at_epoch(epoch)?;

    if client_state.is_gloas_epoch(epoch) {
        return Ok(FINALIZED_ROOT_GINDEX_GLOAS);
    }
    Ok(FINALIZED_ROOT_GINDEX_ELECTRA)
}

// See spec: https://github.com/ethereum/consensus-specs/blob/dev/specs/gloas/light-client/sync-protocol.md#modified-current_sync_committee_gindex_at_slot
/// Returns the current sync committee gindex at the given slot.
/// # Errors
/// Returns an error if the epoch is in a non-supported fork.
pub fn current_sync_committee_gindex_at_slot(
    client_state: &ClientState,
    slot: u64,
) -> Result<u64, EthereumIBCError> {
    let epoch = client_state.compute_epoch_at_slot(slot);
    client_state.verify_supported_fork_at_epoch(epoch)?;

    if client_state.is_gloas_epoch(epoch) {
        return Ok(CURRENT_SYNC_COMMITTEE_GINDEX_GLOAS);
    }
    Ok(CURRENT_SYNC_COMMITTEE_GINDEX_ELECTRA)
}

// See spec: https://github.com/ethereum/consensus-specs/blob/dev/specs/gloas/light-client/sync-protocol.md#modified-next_sync_committee_gindex_at_slot
/// Returns the next sync committee gindex at the given slot.
/// # Errors
/// Returns an error if the epoch is in a non-supported fork.
pub fn next_sync_committee_gindex_at_slot(
    client_state: &ClientState,
    slot: u64,
) -> Result<u64, EthereumIBCError> {
    let epoch = client_state.compute_epoch_at_slot(slot);
    client_state.verify_supported_fork_at_epoch(epoch)?;

    if client_state.is_gloas_epoch(epoch) {
        return Ok(NEXT_SYNC_COMMITTEE_GINDEX_GLOAS);
    }
    Ok(NEXT_SYNC_COMMITTEE_GINDEX_ELECTRA)
}

// See spec: https://github.com/ethereum/consensus-specs/blob/dev/specs/electra/light-client/sync-protocol.md#modified-get_lc_execution_root
/// Returns the execution root of a pre-Gloas-format light client header: the tree hash root of its
/// full execution payload header.
///
/// Gloas-format headers carry the execution block hash directly and have no execution root.
/// # Errors
/// Returns an error if the epoch is in a non-supported fork or the header carries no execution
/// payload header.
pub fn get_lc_execution_root(
    client_state: &ClientState,
    header: &LightClientHeader,
) -> Result<B256, EthereumIBCError> {
    let epoch = client_state.compute_epoch_at_slot(header.beacon.slot);
    client_state.verify_supported_fork_at_epoch(epoch)?;

    header
        .execution
        .as_ref()
        .map(TreeHash::tree_hash_root)
        .ok_or(EthereumIBCError::InvalidLightClientHeaderFormat)
}

// See spec: https://github.com/ethereum/consensus-specs/blob/dev/specs/gloas/light-client/sync-protocol.md#modified-is_valid_light_client_header
/// Validates a light client header.
///
/// A header must set exactly one of `execution` (pre-Gloas format) and `execution_block_hash`
/// (Gloas format). A full execution payload header is rejected at or after the Gloas fork, since
/// Gloas blocks no longer carry the payload. A block hash is proven against the body root with the
/// gindex of the header's own epoch: `EXECUTION_BLOCK_HASH_GINDEX_GLOAS` from Gloas, otherwise
/// `EXECUTION_BLOCK_HASH_GINDEX_DENEB` (every supported fork is Electra or later).
/// # Errors
/// Returns an error if the header cannot be validated.
pub fn is_valid_light_client_header(
    client_state: &ClientState,
    header: &LightClientHeader,
) -> Result<(), EthereumIBCError> {
    let epoch = client_state.compute_epoch_at_slot(header.beacon.slot);

    client_state.verify_supported_fork_at_epoch(epoch)?;

    match (&header.execution, header.execution_block_hash) {
        (Some(_), None) => {
            ensure!(
                !client_state.is_gloas_epoch(epoch),
                EthereumIBCError::ExecutionPayloadHeaderAfterGloas
            );
            ensure_branch_length(
                "execution branch",
                &header.execution_branch,
                EXECUTION_PAYLOAD_GINDEX,
            )?;
            validate_merkle_branch(
                get_lc_execution_root(client_state, header)?,
                header.execution_branch.clone(),
                floorlog2(EXECUTION_PAYLOAD_GINDEX),
                get_subtree_index(EXECUTION_PAYLOAD_GINDEX),
                header.beacon.body_root,
            )
        }
        (None, Some(execution_block_hash)) => {
            // Every Gloas-format header carries the Gloas-length branch, whatever its own epoch:
            // headers that predate Gloas arrive with leading zero nodes, which
            // `is_valid_normalized_merkle_branch` checks and skips.
            ensure_branch_length(
                "execution branch",
                &header.execution_branch,
                EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
            )?;
            let gindex = if client_state.is_gloas_epoch(epoch) {
                EXECUTION_BLOCK_HASH_GINDEX_GLOAS
            } else {
                EXECUTION_BLOCK_HASH_GINDEX_DENEB
            };
            is_valid_normalized_merkle_branch(
                execution_block_hash,
                &header.execution_branch,
                gindex,
                header.beacon.body_root,
            )
        }
        _ => Err(EthereumIBCError::InvalidLightClientHeaderFormat),
    }
}

// See spec: <https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/light-client/sync-protocol.md#get_subtree_index>
/// Values that are constant across all configurations.
#[must_use]
pub const fn get_subtree_index(idx: u64) -> u64 {
    idx % 2_u64.pow(idx.ilog2())
}

// See spec: <https://github.com/ethereum/consensus-specs/blob/ff99bc03d6da29d9ef6e055bdb8500e1b2942f1e/specs/electra/light-client/fork.md#L26>
/// Normalize a merkle branch to a depth for given gindex.
/// # Errors
/// Returns an error if the merkle branch is larger than the calculated depth for the given gindex.
pub fn normalize_merkle_branch(
    branch: &[B256],
    gindex: u64,
) -> Result<Vec<B256>, EthereumIBCError> {
    let depth = floorlog2(gindex);
    let num_extra =
        depth
            .checked_sub(branch.len())
            .ok_or(EthereumIBCError::MerkleBranchTooLong {
                maximum: depth,
                found: branch.len(),
            })?;

    // TODO: Switch to std::iter::repeat_n when cosmwasm supports rust 1.85 (https://github.com/CosmWasm/cosmwasm/issues/2292)
    Ok(vec![B256::default(); num_extra]
        .into_iter()
        .chain(branch.to_vec())
        .collect())
}

#[cfg(test)]
mod test {
    use alloy_primitives::B256;

    use crate::{error::EthereumIBCError, sync_protocol_helpers::normalize_merkle_branch};

    #[test]
    fn test_nomralize_merkle_branch() {
        let branch = vec![B256::from_slice(
            &hex::decode("75d7411cb01daad167713b5a9b7219670f0e500653cbbcd45cfe1bfe04222459")
                .unwrap(),
        )];
        let gindex = 4;
        let normalized = normalize_merkle_branch(&branch, gindex).unwrap();

        let expected_branch = vec![B256::default(), branch[0]];
        assert_eq!(normalized, expected_branch);
    }

    #[test]
    fn test_normalize_merkle_branch_with_no_extra() {
        let branch = vec![B256::default(); 3];
        let gindex = 8;
        let normalized = normalize_merkle_branch(&branch, gindex).unwrap();

        assert_eq!(normalized, branch);
    }

    #[test]
    fn test_normalize_merkle_branch_rejects_invalid_branch() {
        assert!(matches!(
            normalize_merkle_branch(&[B256::default(); 3], 2),
            Err(EthereumIBCError::MerkleBranchTooLong {
                maximum: 1,
                found: 3,
            })
        ));
    }
}

#[cfg(test)]
mod gloas_test {
    use alloy_primitives::B256;
    use ethereum_types::consensus::{
        fork::{Fork, ForkParameters},
        light_client_header::{BeaconBlockHeader, ExecutionPayloadHeader, LightClientHeader},
        merkle::{floorlog2, EXECUTION_BLOCK_HASH_GINDEX_DENEB, EXECUTION_BLOCK_HASH_GINDEX_GLOAS},
    };
    use sha2::{Digest, Sha256};

    use super::{
        current_sync_committee_gindex_at_slot, finalized_root_gindex_at_slot, get_subtree_index,
        is_valid_light_client_header, next_sync_committee_gindex_at_slot,
    };
    use crate::{client_state::ClientState, error::EthereumIBCError};

    const SLOTS_PER_EPOCH: u64 = 32;
    const GLOAS_EPOCH: u64 = 100;
    const GLOAS_SLOT: u64 = GLOAS_EPOCH * SLOTS_PER_EPOCH;

    fn client_state(gloas_epoch: u64) -> ClientState {
        ClientState {
            slots_per_epoch: SLOTS_PER_EPOCH,
            fork_parameters: ForkParameters {
                electra: Fork {
                    epoch: 0,
                    ..Default::default()
                },
                fulu: Fork {
                    epoch: 50,
                    ..Default::default()
                },
                gloas: Fork {
                    epoch: gloas_epoch,
                    ..Default::default()
                },
                ..Default::default()
            },
            ..Default::default()
        }
    }

    /// Folds `leaf` up `branch`, the inverse of `validate_merkle_branch`.
    fn fold(leaf: B256, branch: &[B256], index: u64) -> B256 {
        let mut value = leaf;
        for (i, node) in branch.iter().enumerate() {
            let mut hasher = Sha256::new();
            if (index >> i) & 1 == 1 {
                hasher.update(node);
                hasher.update(value);
            } else {
                hasher.update(value);
                hasher.update(node);
            }
            value = B256::from_slice(&hasher.finalize());
        }
        value
    }

    fn branch(depth: usize) -> Vec<B256> {
        (0..depth)
            .map(|i| B256::repeat_byte(u8::try_from(i + 1).unwrap()))
            .collect()
    }

    /// A Gloas-format header whose body root commits to `block_hash` at `gindex`.
    fn hash_header(
        slot: u64,
        block_hash: B256,
        gindex: u64,
        branch_len: usize,
    ) -> LightClientHeader {
        let depth = floorlog2(gindex);
        let real = branch(depth);
        let body_root = fold(block_hash, &real, get_subtree_index(gindex));
        let mut execution_branch = vec![B256::ZERO; branch_len - depth];
        execution_branch.extend(real);
        LightClientHeader {
            beacon: BeaconBlockHeader {
                slot,
                body_root,
                ..Default::default()
            },
            execution: None,
            execution_block_hash: Some(block_hash),
            execution_branch,
        }
    }

    #[test]
    fn gindices_switch_at_the_gloas_epoch() {
        let cs = client_state(GLOAS_EPOCH);
        let before = GLOAS_SLOT - 1;
        assert_eq!(finalized_root_gindex_at_slot(&cs, before).unwrap(), 169);
        assert_eq!(
            current_sync_committee_gindex_at_slot(&cs, before).unwrap(),
            86
        );
        assert_eq!(next_sync_committee_gindex_at_slot(&cs, before).unwrap(), 87);
        assert_eq!(finalized_root_gindex_at_slot(&cs, GLOAS_SLOT).unwrap(), 735);
        assert_eq!(
            current_sync_committee_gindex_at_slot(&cs, GLOAS_SLOT).unwrap(),
            2945
        );
        assert_eq!(
            next_sync_committee_gindex_at_slot(&cs, GLOAS_SLOT).unwrap(),
            2946
        );
    }

    #[test]
    fn gindices_stay_on_electra_when_gloas_is_unscheduled() {
        let cs = client_state(u64::MAX);
        assert_eq!(
            finalized_root_gindex_at_slot(&cs, GLOAS_SLOT * 10).unwrap(),
            169
        );
    }

    #[test]
    fn gloas_header_with_valid_block_hash_proof_is_accepted() {
        let cs = client_state(GLOAS_EPOCH);
        let header = hash_header(
            GLOAS_SLOT,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
            11,
        );
        assert_eq!(header.execution_branch.len(), 11);
        is_valid_light_client_header(&cs, &header).unwrap();
    }

    #[test]
    fn gloas_header_with_wrong_block_hash_is_rejected() {
        let cs = client_state(GLOAS_EPOCH);
        let mut header = hash_header(
            GLOAS_SLOT,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
            11,
        );
        header.execution_block_hash = Some(B256::repeat_byte(0x43));
        assert!(matches!(
            is_valid_light_client_header(&cs, &header),
            Err(EthereumIBCError::InvalidMerkleBranch(_))
        ));
    }

    #[test]
    fn gloas_epoch_header_cannot_reuse_a_deneb_depth_proof() {
        // A proof built for the pre-Gloas gindex must not validate at a Gloas slot.
        let cs = client_state(GLOAS_EPOCH);
        let header = hash_header(
            GLOAS_SLOT,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_DENEB,
            11,
        );
        assert!(is_valid_light_client_header(&cs, &header).is_err());
    }

    #[test]
    fn pre_gloas_epoch_accepts_a_normalized_block_hash_proof() {
        // Gloas-format light client data also covers headers that predate Gloas; those are proven
        // at the Deneb gindex and arrive padded to the Gloas branch length.
        let cs = client_state(GLOAS_EPOCH);
        let slot = 60 * SLOTS_PER_EPOCH;
        let header = hash_header(
            slot,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_DENEB,
            11,
        );
        assert_eq!(&header.execution_branch[..2], &[B256::ZERO; 2]);
        is_valid_light_client_header(&cs, &header).unwrap();
    }

    #[test]
    fn pre_gloas_epoch_rejects_nonzero_padding_in_the_normalized_branch() {
        let cs = client_state(GLOAS_EPOCH);
        let slot = 60 * SLOTS_PER_EPOCH;
        let mut header = hash_header(
            slot,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_DENEB,
            11,
        );
        header.execution_branch[0] = B256::repeat_byte(0x99);
        assert!(matches!(
            is_valid_light_client_header(&cs, &header),
            Err(EthereumIBCError::InvalidNormalizedMerkleBranch { .. })
        ));
    }

    #[test]
    fn block_hash_branch_must_have_the_gloas_length() {
        let cs = client_state(GLOAS_EPOCH);
        let slot = 60 * SLOTS_PER_EPOCH;
        // A Deneb-depth branch (9) is the pre-Gloas container length, not the Gloas one.
        let header = hash_header(
            slot,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_DENEB,
            9,
        );
        assert!(matches!(
            is_valid_light_client_header(&cs, &header),
            Err(EthereumIBCError::InvalidBranchLength {
                expected: 11,
                found: 9,
                ..
            })
        ));
    }

    #[test]
    fn over_long_branch_is_rejected() {
        let cs = client_state(GLOAS_EPOCH);
        let mut header = hash_header(
            GLOAS_SLOT,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
            11,
        );
        header.execution_branch.insert(0, B256::ZERO);
        assert!(matches!(
            is_valid_light_client_header(&cs, &header),
            Err(EthereumIBCError::InvalidBranchLength {
                expected: 11,
                found: 12,
                ..
            })
        ));
    }

    #[test]
    fn execution_payload_header_is_rejected_from_the_gloas_epoch() {
        let cs = client_state(GLOAS_EPOCH);
        let header = LightClientHeader {
            beacon: BeaconBlockHeader {
                slot: GLOAS_SLOT,
                ..Default::default()
            },
            execution: Some(ExecutionPayloadHeader::default()),
            execution_block_hash: None,
            execution_branch: vec![B256::ZERO; 4],
        };
        assert_eq!(
            is_valid_light_client_header(&cs, &header),
            Err(EthereumIBCError::ExecutionPayloadHeaderAfterGloas)
        );
    }

    #[test]
    fn header_must_set_exactly_one_execution_field() {
        let cs = client_state(GLOAS_EPOCH);
        let mut neither = hash_header(
            GLOAS_SLOT,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
            11,
        );
        neither.execution_block_hash = None;
        assert_eq!(
            is_valid_light_client_header(&cs, &neither),
            Err(EthereumIBCError::InvalidLightClientHeaderFormat)
        );

        let mut both = hash_header(
            GLOAS_SLOT,
            B256::repeat_byte(0x42),
            EXECUTION_BLOCK_HASH_GINDEX_GLOAS,
            11,
        );
        both.execution = Some(ExecutionPayloadHeader::default());
        assert_eq!(
            is_valid_light_client_header(&cs, &both),
            Err(EthereumIBCError::InvalidLightClientHeaderFormat)
        );
    }
}
