//! This module provides [`update_consensus_state`] function to update the consensus state

use crate::{
    client_state::ClientState,
    consensus_state::ConsensusState,
    error::EthereumIBCError,
    execution::{verify_execution_header_rlp, ExecutionBlockSummary},
    header::Header,
};

/// Returns the execution state root, timestamp and block number of the finalized header.
///
/// A pre-Gloas-format header carries its execution payload header, which its merkle branch
/// already proves. A Gloas-format header carries only the execution block hash, so the values come
/// from `header.execution_header_rlp`, checked against that hash.
///
/// # Errors
/// Returns an error if the finalized header is in the Gloas format and the RLP header is missing
/// or does not hash to the proven block hash, or if the RLP header is supplied for a pre-Gloas
/// format header.
pub fn finalized_execution_summary(
    header: &Header,
) -> Result<ExecutionBlockSummary, EthereumIBCError> {
    let finalized_header = &header.consensus_update.finalized_header;
    match (
        &finalized_header.execution,
        finalized_header.execution_block_hash,
        &header.execution_header_rlp,
    ) {
        (Some(execution), None, None) => Ok(ExecutionBlockSummary {
            state_root: execution.state_root,
            timestamp: execution.timestamp,
            block_number: execution.block_number,
        }),
        (Some(_), None, Some(_)) => Err(EthereumIBCError::UnexpectedExecutionHeaderRlp),
        (None, Some(block_hash), Some(rlp)) => verify_execution_header_rlp(rlp, block_hash),
        (None, Some(_), None) => Err(EthereumIBCError::MissingExecutionHeaderRlp),
        _ => Err(EthereumIBCError::InvalidLightClientHeaderFormat),
    }
}

/// Takes in the current client and consensus state and a new header and returns the updated
/// consensus state and optionally the updated client state (if it needs to be updated)
///
/// # Returns
/// Returns the updated slot, the updated consensus state and optionally the updated client state.
///
/// Current implementation requires the client and consensus states to be updated since historical updates are not allowed.
///
/// # Errors
/// Returns an error if the store period is not equal to the finalized period
#[allow(clippy::module_name_repetitions, clippy::needless_pass_by_value)]
pub fn update_consensus_state(
    current_consensus_state: ConsensusState,
    current_client_state: ClientState,
    header: Header,
) -> Result<(u64, ConsensusState, Option<ClientState>), EthereumIBCError> {
    ensure!(
        current_client_state.latest_slot == current_consensus_state.slot,
        EthereumIBCError::ClientAndConsensusSlotMismatch {
            client_state_slot: current_client_state.latest_slot,
            consensus_state_slot: current_consensus_state.slot
        }
    );

    let store_slot = current_consensus_state.slot;
    let store_period = current_client_state.compute_sync_committee_period_at_slot(store_slot);

    let update_finalized_slot = header.consensus_update.finalized_header.beacon.slot;
    let update_finalized_period =
        current_client_state.compute_sync_committee_period_at_slot(update_finalized_slot);

    let execution = finalized_execution_summary(&header)?;

    let mut new_consensus_state = current_consensus_state.clone();

    if let Some(next_sync_committee) = current_consensus_state.next_sync_committee {
        // sync committee only changes when the period change
        if update_finalized_period == store_period + 1 {
            new_consensus_state.current_sync_committee = next_sync_committee;
            new_consensus_state.next_sync_committee = header
                .consensus_update
                .next_sync_committee
                .map(|c| c.to_summarized_sync_committee());
        }
    } else {
        // if the finalized period is greater, we have to have a next sync committee
        ensure!(
            update_finalized_period == store_period,
            EthereumIBCError::StorePeriodMustBeEqualToFinalizedPeriod
        );
        // The next sync committee in the update is proven against the attested header's state, so
        // it is the committee of the period after the attested one. It is only the committee of the
        // period after the store's if the two periods are the same.
        //
        // After `verify_header` this cannot fail: with no next committee known,
        // `validate_light_client_update` pins the signature period to the store's, and
        // `finalized <= attested < signature` makes the attested period the store's too. The check
        // stays as a guard for a caller that applies an update without having verified it.
        let update_attested_period = current_client_state.compute_sync_committee_period_at_slot(
            header.consensus_update.attested_header.beacon.slot,
        );
        ensure!(
            update_attested_period == store_period,
            EthereumIBCError::StorePeriodMustBeEqualToAttestedPeriod
        );
        new_consensus_state.next_sync_committee = header
            .consensus_update
            .next_sync_committee
            .map(|c| c.to_summarized_sync_committee());
    }

    new_consensus_state.slot = update_finalized_slot;
    new_consensus_state.state_root = execution.state_root;
    new_consensus_state.timestamp = execution.timestamp;

    let new_client_state =
        (update_finalized_slot > current_consensus_state.slot).then_some(ClientState {
            latest_slot: update_finalized_slot,
            latest_execution_block_number: execution.block_number,
            ..current_client_state
        });
    Ok((update_finalized_slot, new_consensus_state, new_client_state))
}

#[cfg(test)]
mod test {
    use alloy_primitives::{keccak256, Bytes, B256};
    use ethereum_types::consensus::{
        light_client_header::{
            BeaconBlockHeader, ExecutionPayloadHeader, LightClientHeader, LightClientUpdate,
        },
        sync_committee::{SummarizedSyncCommittee, SyncCommittee},
    };

    use super::{finalized_execution_summary, update_consensus_state};
    use crate::{
        client_state::ClientState,
        consensus_state::ConsensusState,
        error::EthereumIBCError,
        execution::{test_support::encode_header, ExecutionBlockSummary},
        header::{ActiveSyncCommittee, Header},
    };

    const FINALIZED_SLOT: u64 = 6_400;

    fn header_with(
        execution: Option<ExecutionPayloadHeader>,
        execution_block_hash: Option<B256>,
        rlp: Option<Vec<u8>>,
    ) -> Header {
        Header {
            active_sync_committee: ActiveSyncCommittee::Current(SyncCommittee::default()),
            consensus_update: LightClientUpdate {
                finalized_header: LightClientHeader {
                    beacon: BeaconBlockHeader {
                        slot: FINALIZED_SLOT,
                        ..Default::default()
                    },
                    execution,
                    execution_block_hash,
                    execution_branch: vec![],
                },
                ..Default::default()
            },
            trusted_slot: 0,
            execution_header_rlp: rlp.map(Bytes::from),
        }
    }

    fn pre_gloas_execution() -> ExecutionPayloadHeader {
        ExecutionPayloadHeader {
            state_root: B256::repeat_byte(0x11),
            timestamp: 1_000,
            block_number: 7,
            ..Default::default()
        }
    }

    #[test]
    fn pre_gloas_header_takes_the_execution_state_from_the_payload_header() {
        let header = header_with(Some(pre_gloas_execution()), None, None);
        assert_eq!(
            finalized_execution_summary(&header),
            Ok(ExecutionBlockSummary {
                state_root: B256::repeat_byte(0x11),
                timestamp: 1_000,
                block_number: 7,
            })
        );
    }

    #[test]
    fn gloas_header_takes_the_execution_state_from_the_hashed_rlp() {
        let state_root = B256::repeat_byte(0x22);
        let rlp = encode_header(state_root, 12_345, 2_000, 0);
        let header = header_with(None, Some(keccak256(&rlp)), Some(rlp));
        assert_eq!(
            finalized_execution_summary(&header),
            Ok(ExecutionBlockSummary {
                state_root,
                timestamp: 2_000,
                block_number: 12_345,
            })
        );
    }

    #[test]
    fn gloas_header_rejects_a_state_root_the_block_hash_does_not_commit_to() {
        let honest = encode_header(B256::repeat_byte(0x22), 1, 2, 0);
        let forged = encode_header(B256::repeat_byte(0x23), 1, 2, 0);
        let header = header_with(None, Some(keccak256(&honest)), Some(forged));
        assert!(matches!(
            finalized_execution_summary(&header),
            Err(EthereumIBCError::ExecutionHeaderHashMismatch { .. })
        ));
    }

    #[test]
    fn gloas_header_requires_the_rlp() {
        let header = header_with(None, Some(B256::repeat_byte(0x33)), None);
        assert_eq!(
            finalized_execution_summary(&header),
            Err(EthereumIBCError::MissingExecutionHeaderRlp)
        );
    }

    #[test]
    fn pre_gloas_header_rejects_a_stray_rlp() {
        let rlp = encode_header(B256::repeat_byte(0x22), 1, 2, 0);
        let header = header_with(Some(pre_gloas_execution()), None, Some(rlp));
        assert_eq!(
            finalized_execution_summary(&header),
            Err(EthereumIBCError::UnexpectedExecutionHeaderRlp)
        );
    }

    #[test]
    fn header_with_both_or_neither_execution_field_is_rejected() {
        let neither = header_with(None, None, None);
        assert_eq!(
            finalized_execution_summary(&neither),
            Err(EthereumIBCError::InvalidLightClientHeaderFormat)
        );
        let both = header_with(Some(pre_gloas_execution()), Some(B256::ZERO), None);
        assert_eq!(
            finalized_execution_summary(&both),
            Err(EthereumIBCError::InvalidLightClientHeaderFormat)
        );
    }

    #[test]
    fn update_stores_the_state_root_and_block_number_proven_through_the_block_hash() {
        let state_root = B256::repeat_byte(0x44);
        let rlp = encode_header(state_root, 999, 3_000, 0);
        let header = header_with(None, Some(keccak256(&rlp)), Some(rlp));

        let client_state = ClientState {
            slots_per_epoch: 32,
            epochs_per_sync_committee_period: 256,
            latest_slot: 100,
            latest_execution_block_number: 50,
            ..Default::default()
        };
        let consensus_state = ConsensusState {
            slot: 100,
            state_root: B256::repeat_byte(0x01),
            timestamp: 10,
            current_sync_committee: SummarizedSyncCommittee::default(),
            next_sync_committee: None,
        };

        let (slot, new_consensus_state, new_client_state) =
            update_consensus_state(consensus_state, client_state, header).unwrap();

        assert_eq!(slot, FINALIZED_SLOT);
        assert_eq!(new_consensus_state.state_root, state_root);
        assert_eq!(new_consensus_state.timestamp, 3_000);
        let new_client_state = new_client_state.expect("client state advances with the slot");
        assert_eq!(new_client_state.latest_slot, FINALIZED_SLOT);
        assert_eq!(new_client_state.latest_execution_block_number, 999);
    }
    #[test]
    fn next_sync_committee_is_not_stored_from_an_update_attested_in_a_later_period() {
        use crate::test_utils::synthetic::{SyntheticChain, COMMITTEE_SIZE, PERIOD_END_SLOT};

        let chain = SyntheticChain::new();
        // A client that does not know its next sync committee yet.
        let consensus_state = ConsensusState {
            next_sync_committee: None,
            ..chain.consensus_state()
        };
        let mut update = chain.update(B256::repeat_byte(0x11), COMMITTEE_SIZE);
        // The committee the update carries is the one that follows the attested period, which is
        // not the period after the store's once the attested header is in a later period.
        update.next_sync_committee = Some(chain.committee().clone());
        update.attested_header.beacon.slot = PERIOD_END_SLOT + 1;
        let header = Header {
            active_sync_committee: ActiveSyncCommittee::Current(chain.committee().clone()),
            consensus_update: update,
            trusted_slot: consensus_state.slot,
            execution_header_rlp: None,
        };

        assert!(matches!(
            update_consensus_state(consensus_state, chain.client_state(), header),
            Err(EthereumIBCError::StorePeriodMustBeEqualToAttestedPeriod)
        ));
    }
}
