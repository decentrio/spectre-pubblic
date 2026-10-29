//! This module provides [`verify_misbehaviour`] function to check for misbehaviour

use alloy_primitives::B256;
use ethereum_types::consensus::light_client_header::{LightClientHeader, LightClientUpdate};

use crate::{
    client_state::ClientState,
    consensus_state::{ConsensusState, TrustedConsensusState},
    error::EthereumIBCError,
    header::ActiveSyncCommittee,
    verify::{validate_light_client_update, validate_light_client_update_bounds, BlsVerify},
};

/// Returns the execution commitments of two attested headers that are comparable with each other.
///
/// Two pre-Gloas-format headers are compared by execution state root, as before. As soon as either
/// header is in the Gloas format, which carries no state root, both are compared by execution
/// block hash. Equal block hashes mean equal execution state, so that comparison is never weaker
/// than comparing state roots.
fn conflicting_execution_roots(
    header_1: &LightClientHeader,
    header_2: &LightClientHeader,
) -> Result<(B256, B256), EthereumIBCError> {
    match (&header_1.execution, &header_2.execution) {
        (Some(execution_1), Some(execution_2)) => {
            Ok((execution_1.state_root, execution_2.state_root))
        }
        _ => Ok((
            execution_block_hash(header_1)?,
            execution_block_hash(header_2)?,
        )),
    }
}

const fn execution_block_hash(header: &LightClientHeader) -> Result<B256, EthereumIBCError> {
    match (&header.execution, header.execution_block_hash) {
        (Some(execution), None) => Ok(execution.block_hash),
        (None, Some(block_hash)) => Ok(block_hash),
        _ => Err(EthereumIBCError::InvalidLightClientHeaderFormat),
    }
}

/// Verifies if a consensus misbehaviour is valid by checking if the two conflicting light client updates are valid.
///
/// Each update must carry the same evidence a header update needs to be accepted: it is bounded,
/// it passes [`validate_light_client_update`], and at least 2/3 of the sync committee signed it.
/// Sync committee signatures are not slashable, so evidence signed by fewer keys than that would
/// let a minority of the committee freeze the client.
///
/// The consensus state the evidence is anchored at may be any the client still holds, because
/// evidence has to be anchored below its attested slot and the client keeps advancing. What the
/// client does not accept is evidence signed by a committee it has left: the signature period of
/// both updates must be the period of the client's latest slot or a later one. The keys of an
/// earlier committee can belong to validators that exited long ago and carry no stake. An anchor
/// from the previous period is still enough for evidence signed by the committee that took over
/// from it, which the anchor knows as its next sync committee.
///
/// * `client_state`: The current client state.
/// * `consensus_state`: The current consensus state (previously verified and stored)
/// * `full_sync_committee`: The full sync committee data. (untrusted)
/// * `update_1`: The first light client update.
/// * `update_2`: The second light client update.
/// * `current_slot`: The slot number computed based on the current timestamp.
/// * `bls_verifier`: BLS verification implementation.
///
/// # Errors
/// Returns an error if the misbehaviour cannot be verified.
#[allow(clippy::module_name_repetitions, clippy::needless_pass_by_value)]
pub fn verify_misbehaviour<V: BlsVerify>(
    client_state: &ClientState,
    consensus_state: &ConsensusState,
    full_sync_committee: &ActiveSyncCommittee,
    update_1: &LightClientUpdate,
    update_2: &LightClientUpdate,
    current_timestamp: u64,
    bls_verifier: V,
) -> Result<(), EthereumIBCError> {
    // Bounds first, as in `verify_header`: nothing below hashes or queries BLS for an oversized update.
    validate_light_client_update_bounds(update_1)?;
    validate_light_client_update_bounds(update_2)?;

    let trusted_consensus_state = TrustedConsensusState::new(
        client_state,
        consensus_state.clone(),
        full_sync_committee.clone(),
        &bls_verifier,
    )?;

    // There is no point to check for misbehaviour when the headers are not for the same height
    let (slot_1, slot_2) = (
        update_1.attested_header.beacon.slot,
        update_2.attested_header.beacon.slot,
    );
    ensure!(
        slot_1 == slot_2,
        EthereumIBCError::MisbehaviourSlotMismatch(slot_1, slot_2)
    );

    let (state_root_1, state_root_2) =
        conflicting_execution_roots(&update_1.attested_header, &update_2.attested_header)?;
    ensure!(
        state_root_1 != state_root_2,
        EthereumIBCError::MisbehaviourStorageRootsMatch(state_root_1)
    );

    let latest_period =
        client_state.compute_sync_committee_period_at_slot(client_state.latest_slot);
    for update in [update_1, update_2] {
        let signature_period =
            client_state.compute_sync_committee_period_at_slot(update.signature_slot);
        ensure!(
            signature_period >= latest_period,
            EthereumIBCError::MisbehaviourSignedByPastCommittee {
                signature_period,
                latest_period,
            }
        );
    }

    let current_slot = client_state
        .compute_slot_at_timestamp(current_timestamp)
        .ok_or(EthereumIBCError::FailedToComputeSlotAtTimestamp {
            timestamp: current_timestamp,
            genesis: client_state.genesis_time,
            seconds_per_slot: client_state.seconds_per_slot,
            genesis_slot: client_state.genesis_slot,
        })?;

    validate_light_client_update::<V>(
        client_state,
        &trusted_consensus_state,
        update_1,
        current_slot,
        &bls_verifier,
    )?;

    validate_light_client_update::<V>(
        client_state,
        &trusted_consensus_state,
        update_2,
        current_slot,
        &bls_verifier,
    )?;

    // Checked after `validate_light_client_update`, which pins the bitvector length to the
    // committee bound to the trusted state, so the 2/3 ratio is taken over the real committee size.
    for update in [update_1, update_2] {
        ensure!(
            update.sync_aggregate.validate_signature_supermajority(),
            EthereumIBCError::NotEnoughSignatures
        );
    }

    Ok(())
}

#[cfg(test)]
#[allow(clippy::pedantic)]
mod test {
    use alloy_primitives::B256;
    use ethereum_types::consensus::{
        bls::{BlsPublicKey, BlsSignature},
        light_client_header::LightClientUpdate,
    };

    use super::*;
    use crate::{
        header::ActiveSyncCommittee,
        test_utils::{
            bls_verifier::{BlsError, TestBlsVerifier},
            synthetic::{SyntheticChain, COMMITTEE_SIZE, PERIOD_END_SLOT},
        },
        verify::BlsVerify,
    };

    struct UnreachableBlsVerifier;

    impl BlsVerify for UnreachableBlsVerifier {
        type Error = BlsError;

        fn fast_aggregate_verify(
            &self,
            _public_keys: &[BlsPublicKey],
            _msg: B256,
            _signature: BlsSignature,
        ) -> Result<(), BlsError> {
            panic!("resource bounds must run before BLS verification")
        }

        fn aggregate(&self, _public_keys: &[BlsPublicKey]) -> Result<BlsPublicKey, BlsError> {
            panic!("resource bounds must run before BLS aggregation")
        }
    }

    /// Two updates that attest conflicting execution state roots at one slot, each signed by the
    /// first `signers` members of the synthetic committee.
    fn conflicting_updates(
        chain: &SyntheticChain,
        signers: usize,
    ) -> (LightClientUpdate, LightClientUpdate) {
        (
            chain.update(B256::repeat_byte(0x11), signers),
            chain.update(B256::repeat_byte(0x22), signers),
        )
    }

    fn verify<V: BlsVerify>(
        chain: &SyntheticChain,
        update_1: &LightClientUpdate,
        update_2: &LightClientUpdate,
        bls_verifier: V,
    ) -> Result<(), EthereumIBCError> {
        verify_misbehaviour(
            &chain.client_state(),
            &chain.consensus_state(),
            &ActiveSyncCommittee::Current(chain.committee().clone()),
            update_1,
            update_2,
            chain.current_timestamp(),
            bls_verifier,
        )
    }

    #[test]
    fn supermajority_evidence_is_accepted() {
        let chain = SyntheticChain::new();
        // 6 of 8 is the smallest signer count with 3 * signers >= 2 * committee.
        let (update_1, update_2) = conflicting_updates(&chain, 6);

        verify(&chain, &update_1, &update_2, TestBlsVerifier).unwrap();
    }

    #[test]
    fn minority_signed_evidence_is_rejected() {
        let chain = SyntheticChain::new();
        let min_participants = chain.client_state().min_sync_committee_participants as usize;

        // 3 of 8 meets min_sync_committee_participants, which is all `validate_light_client_update`
        // asks for, and every count below 6 is short of the 2/3 a header update needs.
        assert_eq!(min_participants, 3);
        for signers in min_participants..6 {
            let (update_1, update_2) = conflicting_updates(&chain, signers);

            assert!(
                matches!(
                    verify(&chain, &update_1, &update_2, TestBlsVerifier),
                    Err(EthereumIBCError::NotEnoughSignatures)
                ),
                "{signers}/{COMMITTEE_SIZE} signers must not freeze the client"
            );
        }
    }

    #[test]
    fn evidence_needs_a_supermajority_on_each_update() {
        let chain = SyntheticChain::new();

        // One honest-looking update is not enough: the other one is the minority-signed forgery.
        let update_1 = chain.update(B256::repeat_byte(0x11), 6);
        let update_2 = chain.update(B256::repeat_byte(0x22), 3);

        assert!(matches!(
            verify(&chain, &update_1, &update_2, TestBlsVerifier),
            Err(EthereumIBCError::NotEnoughSignatures)
        ));
        assert!(matches!(
            verify(&chain, &update_2, &update_1, TestBlsVerifier),
            Err(EthereumIBCError::NotEnoughSignatures)
        ));
    }

    #[test]
    fn identical_execution_roots_are_not_misbehaviour() {
        let chain = SyntheticChain::new();
        let update = chain.update(B256::repeat_byte(0x11), COMMITTEE_SIZE);

        assert!(matches!(
            verify(&chain, &update, &update, TestBlsVerifier),
            Err(EthereumIBCError::MisbehaviourStorageRootsMatch(_))
        ));
    }

    #[test]
    fn oversized_update_fields_fail_before_bls_host_queries() {
        let chain = SyntheticChain::new();
        let (update_1, mut update_2) = conflicting_updates(&chain, COMMITTEE_SIZE);
        update_2
            .attested_header
            .execution
            .as_mut()
            .unwrap()
            .extra_data = vec![0; 33].into();

        assert!(matches!(
            verify(&chain, &update_1, &update_2, UnreachableBlsVerifier),
            Err(EthereumIBCError::ResourceLimit {
                resource: "execution payload extra_data",
                maximum: 32,
                found: 33,
            })
        ));
    }
    /// A client state whose latest slot has moved into period 1, as after a rotation.
    fn client_in_period_1(chain: &SyntheticChain) -> crate::client_state::ClientState {
        crate::client_state::ClientState {
            latest_slot: PERIOD_END_SLOT + 400,
            ..chain.client_state()
        }
    }

    fn verify_with(
        client_state: &crate::client_state::ClientState,
        chain: &SyntheticChain,
        full_sync_committee: &ActiveSyncCommittee,
        update_1: &LightClientUpdate,
        update_2: &LightClientUpdate,
    ) -> Result<(), EthereumIBCError> {
        verify_misbehaviour(
            client_state,
            &chain.consensus_state(),
            full_sync_committee,
            update_1,
            update_2,
            chain.current_timestamp(),
            TestBlsVerifier,
        )
    }

    #[test]
    fn a_previous_period_anchor_authenticates_the_committee_that_took_over() {
        let chain = SyntheticChain::new();
        let (update_1, update_2) = (
            chain.next_period_update(B256::repeat_byte(0x11), COMMITTEE_SIZE),
            chain.next_period_update(B256::repeat_byte(0x22), COMMITTEE_SIZE),
        );

        // The client is in period 1 and the anchor is in period 0. The evidence is signed in
        // period 1 by the committee the anchor knows as its next one.
        verify_with(
            &client_in_period_1(&chain),
            &chain,
            &ActiveSyncCommittee::Next(chain.next_committee().clone()),
            &update_1,
            &update_2,
        )
        .unwrap();
    }

    #[test]
    fn evidence_signed_before_the_latest_period_is_rejected() {
        let chain = SyntheticChain::new();
        let (update_1, update_2) = conflicting_updates(&chain, COMMITTEE_SIZE);

        // Valid against the anchor, signed by the committee of period 0, which the client has left.
        assert_eq!(
            verify_with(
                &client_in_period_1(&chain),
                &chain,
                &ActiveSyncCommittee::Current(chain.committee().clone()),
                &update_1,
                &update_2,
            ),
            Err(EthereumIBCError::MisbehaviourSignedByPastCommittee {
                signature_period: 0,
                latest_period: 1,
            })
        );
        // The same evidence is accepted while the client is still in period 0.
        verify(&chain, &update_1, &update_2, TestBlsVerifier).unwrap();
    }
}
