//! A self-contained, signed Ethereum light client chain for tests.
//!
//! The recorded fixtures are signed by the real sync committee, so a test cannot choose how many
//! members sign an update. This chain is signed by a small committee whose secret keys the test
//! holds. The signatures are real BLS signatures (milagro) over the real signing root; only the
//! beacon data around them is synthetic.
//!
//! The chain's own updates lie in sync committee period 0. [`SyntheticChain::next_period_update`]
//! builds the one kind of update that crosses into period 1, signed by the next committee.

use alloy_primitives::{Bytes, FixedBytes, B256};
use ethereum_types::consensus::{
    bls::{BlsPublicKey, BlsSignature},
    domain::{compute_domain, DomainType},
    fork::{Fork, ForkParameters, Version},
    light_client_header::{
        BeaconBlockHeader, ExecutionPayloadHeader, LightClientHeader, LightClientUpdate,
    },
    merkle::{floorlog2, EXECUTION_PAYLOAD_GINDEX, FINALIZED_ROOT_GINDEX_ELECTRA},
    signing_data::compute_signing_root,
    sync_committee::{SyncAggregate, SyncCommittee},
};
use sha2::{Digest, Sha256};
use tree_hash::TreeHash;

use crate::{
    client_state::ClientState, consensus_state::ConsensusState,
    sync_protocol_helpers::get_subtree_index, test_utils::bls_verifier::aggreagate,
};

/// The number of members of the synthetic sync committee.
pub const COMMITTEE_SIZE: usize = 8;
/// The slot of the consensus state the chain is anchored at.
pub const ANCHOR_SLOT: u64 = 100;
/// How far an update finalizes past the slot it is built on.
pub const FINALIZED_SLOT_STEP: u64 = 100;
/// The last slot of sync committee period 0 with the chain's parameters (32 * 256 slots).
pub const PERIOD_END_SLOT: u64 = 8192;

const SECONDS_PER_SLOT: u64 = 12;
/// How far the attested header of an update is past its finalized header.
const ATTESTED_SLOT_STEP: u64 = 64;

/// A synthetic chain with an 8-member sync committee, and a distinct 8-member next committee.
pub struct SyntheticChain {
    secret_keys: Vec<milagro_bls::SecretKey>,
    committee: SyncCommittee,
    next_secret_keys: Vec<milagro_bls::SecretKey>,
    next_committee: SyncCommittee,
}

impl Default for SyntheticChain {
    fn default() -> Self {
        Self::new()
    }
}

impl SyntheticChain {
    /// Creates the chain with a deterministic committee.
    ///
    /// # Panics
    /// Panics if a deterministic key cannot be derived, which would be a bug in the test setup.
    #[must_use]
    pub fn new() -> Self {
        let (secret_keys, committee) = derive_committee(1);
        let (next_secret_keys, next_committee) = derive_committee(101);

        Self {
            secret_keys,
            committee,
            next_secret_keys,
            next_committee,
        }
    }

    /// The full sync committee.
    #[must_use]
    pub const fn committee(&self) -> &SyncCommittee {
        &self.committee
    }

    /// The full next sync committee, the one the consensus state at [`ANCHOR_SLOT`] knows by its
    /// summary and that takes over in period 1.
    #[must_use]
    pub const fn next_committee(&self) -> &SyncCommittee {
        &self.next_committee
    }

    /// A client state whose latest slot is [`ANCHOR_SLOT`].
    ///
    /// `min_sync_committee_participants` is set to a third of the committee, rounded up, which is
    /// what the relayer configures.
    #[must_use]
    pub fn client_state(&self) -> ClientState {
        let fork = |version: u8| Fork {
            version: Version::from([version, 0, 0, 0]),
            epoch: 0,
        };
        ClientState {
            chain_id: 1,
            genesis_validators_root: B256::repeat_byte(7),
            min_sync_committee_participants: (COMMITTEE_SIZE as u64).div_ceil(3),
            sync_committee_size: COMMITTEE_SIZE as u64,
            seconds_per_slot: SECONDS_PER_SLOT,
            slots_per_epoch: 32,
            epochs_per_sync_committee_period: 256,
            latest_slot: ANCHOR_SLOT,
            fork_parameters: ForkParameters {
                altair: fork(1),
                bellatrix: fork(2),
                capella: fork(3),
                deneb: fork(4),
                electra: fork(5),
                ..ForkParameters::default()
            },
            ..ClientState::default()
        }
    }

    /// The consensus state at [`ANCHOR_SLOT`], with the synthetic committee as the current one.
    #[must_use]
    pub fn consensus_state(&self) -> ConsensusState {
        ConsensusState {
            slot: ANCHOR_SLOT,
            state_root: B256::repeat_byte(1),
            timestamp: ANCHOR_SLOT * SECONDS_PER_SLOT,
            current_sync_committee: self.committee.to_summarized_sync_committee(),
            next_sync_committee: Some(self.next_committee.to_summarized_sync_committee()),
        }
    }

    /// A timestamp past every slot the chain signs, including the ones in period 1, so no update
    /// is from the future.
    #[must_use]
    pub const fn current_timestamp(&self) -> u64 {
        (PERIOD_END_SLOT + 1_000) * SECONDS_PER_SLOT
    }

    /// An update on top of the [`ANCHOR_SLOT`] consensus state, signed by the first `signers`
    /// committee members. See [`Self::update_after`].
    #[must_use]
    pub fn update(&self, execution_state_root: B256, signers: usize) -> LightClientUpdate {
        self.update_after(ANCHOR_SLOT, execution_state_root, signers)
    }

    /// An update on top of the consensus state at `trusted_slot`, attesting an execution state
    /// root, signed by the first `signers` committee members.
    ///
    /// The update finalizes [`FINALIZED_SLOT_STEP`] slots past `trusted_slot`, so feeding the
    /// finalized slot of one update to the next builds a chain of updates. Two updates that
    /// differ only in `execution_state_root` attest to conflicting execution states at one slot.
    ///
    /// # Panics
    /// Panics if `signers` exceeds [`COMMITTEE_SIZE`], or if the update would leave period 0.
    #[must_use]
    pub fn update_after(
        &self,
        trusted_slot: u64,
        execution_state_root: B256,
        signers: usize,
    ) -> LightClientUpdate {
        let finalized_slot = trusted_slot + FINALIZED_SLOT_STEP;
        let attested_slot = finalized_slot + ATTESTED_SLOT_STEP;
        let signature_slot = attested_slot + 1;
        assert!(
            signature_slot < PERIOD_END_SLOT,
            "the update would leave sync committee period 0"
        );
        self.signed_update(
            &self.secret_keys,
            [finalized_slot, attested_slot, signature_slot],
            execution_state_root,
            signers,
        )
    }

    /// An update whose slots are in period 1, signed by the first `signers` members of the *next*
    /// committee, which is how the current committee of period 1 authenticates itself to a client
    /// whose consensus state is still in period 0 (`ActiveSyncCommittee::Next`).
    ///
    /// It finalizes at `PERIOD_END_SLOT + FINALIZED_SLOT_STEP`. Two of them that differ only in
    /// `execution_state_root` attest to conflicting execution states at one slot.
    ///
    /// # Panics
    /// Panics if `signers` exceeds [`COMMITTEE_SIZE`].
    #[must_use]
    pub fn next_period_update(
        &self,
        execution_state_root: B256,
        signers: usize,
    ) -> LightClientUpdate {
        let finalized_slot = PERIOD_END_SLOT + FINALIZED_SLOT_STEP;
        let attested_slot = finalized_slot + ATTESTED_SLOT_STEP;
        self.signed_update(
            &self.next_secret_keys,
            [finalized_slot, attested_slot, attested_slot + 1],
            execution_state_root,
            signers,
        )
    }

    fn signed_update(
        &self,
        secret_keys: &[milagro_bls::SecretKey],
        [finalized_slot, attested_slot, signature_slot]: [u64; 3],
        execution_state_root: B256,
        signers: usize,
    ) -> LightClientUpdate {
        assert!(
            signers <= COMMITTEE_SIZE,
            "more signers than committee members"
        );

        let finalized_header = header(finalized_slot, B256::ZERO, B256::repeat_byte(0xaa));
        let finality_branch = branch(floorlog2(FINALIZED_ROOT_GINDEX_ELECTRA));
        let attested_state_root = fold(
            finalized_header.beacon.tree_hash_root(),
            &finality_branch,
            get_subtree_index(FINALIZED_ROOT_GINDEX_ELECTRA),
        );
        let attested_header = header(attested_slot, attested_state_root, execution_state_root);

        let client_state = self.client_state();
        let fork_version = client_state
            .fork_parameters
            .compute_fork_version(client_state.compute_epoch_at_slot(signature_slot - 1));
        let domain = compute_domain(
            DomainType::SYNC_COMMITTEE,
            Some(fork_version),
            Some(client_state.genesis_validators_root),
            client_state.fork_parameters.genesis_fork_version,
        );
        let signing_root = compute_signing_root(&attested_header.beacon, domain);

        let signatures = secret_keys[..signers]
            .iter()
            .map(|sk| milagro_bls::Signature::new(signing_root.as_slice(), sk))
            .collect::<Vec<_>>();
        let signature_refs = signatures.iter().collect::<Vec<_>>();
        let aggregate = milagro_bls::AggregateSignature::aggregate(&signature_refs);

        let bits = u8::try_from((1_u16 << signers) - 1).unwrap();

        LightClientUpdate {
            attested_header,
            next_sync_committee: None,
            next_sync_committee_branch: None,
            finalized_header,
            finality_branch,
            sync_aggregate: SyncAggregate {
                sync_committee_bits: Bytes::from(vec![bits]),
                sync_committee_signature: BlsSignature::from(aggregate.as_bytes()),
            },
            signature_slot,
        }
    }
}

/// A committee of [`COMMITTEE_SIZE`] deterministic keys, derived from `first_seed..`.
fn derive_committee(first_seed: u8) -> (Vec<milagro_bls::SecretKey>, SyncCommittee) {
    let secret_keys = (first_seed..)
        .take(COMMITTEE_SIZE)
        .map(|seed| milagro_bls::SecretKey::key_generate(&[seed; 32], &[]).unwrap())
        .collect::<Vec<_>>();
    let pubkeys = secret_keys
        .iter()
        .map(|sk| FixedBytes::from(milagro_bls::PublicKey::from_secret_key(sk).as_bytes()))
        .collect::<Vec<BlsPublicKey>>();
    let aggregate_pubkey = aggreagate(&pubkeys).unwrap();

    (
        secret_keys,
        SyncCommittee {
            pubkeys,
            aggregate_pubkey,
        },
    )
}

/// A pre-Gloas-format header whose body root commits to an execution header with the given state root.
fn header(slot: u64, state_root: B256, execution_state_root: B256) -> LightClientHeader {
    let execution = ExecutionPayloadHeader {
        state_root: execution_state_root,
        block_number: slot,
        timestamp: slot * SECONDS_PER_SLOT,
        ..ExecutionPayloadHeader::default()
    };
    let execution_branch = branch(floorlog2(EXECUTION_PAYLOAD_GINDEX));
    let body_root = fold(
        execution.tree_hash_root(),
        &execution_branch,
        get_subtree_index(EXECUTION_PAYLOAD_GINDEX),
    );

    LightClientHeader {
        beacon: BeaconBlockHeader {
            slot,
            state_root,
            body_root,
            ..BeaconBlockHeader::default()
        },
        execution: Some(execution),
        execution_block_hash: None,
        execution_branch,
    }
}

/// An arbitrary but fixed Merkle branch of `depth` nodes.
fn branch(depth: usize) -> Vec<B256> {
    (1..=depth)
        .map(|i| B256::repeat_byte(u8::try_from(i).unwrap()))
        .collect()
}

/// Folds `leaf` up `branch`, the inverse of `validate_merkle_branch`.
fn fold(leaf: B256, branch: &[B256], index: u64) -> B256 {
    branch.iter().enumerate().fold(leaf, |value, (i, node)| {
        let mut hasher = Sha256::new();
        if (index >> i) & 1 == 1 {
            hasher.update(node);
            hasher.update(value);
        } else {
            hasher.update(value);
            hasher.update(node);
        }
        B256::from_slice(&hasher.finalize())
    })
}
