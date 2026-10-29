use alloy_primitives::{Address, B256};
use cosmwasm_std::{testing::MockStorage, Binary, Storage};
use serde::{Deserialize, Serialize, Serializer};

use crate::{
    error::Error,
    msg::{MigrateMsg, UpdateStateResult},
    runtime,
    state::{
        AttestorConfig, ClientState, CommonProfile, ConsensusState, Header, Height, RuntimeProfile,
        MAX_ATTESTORS,
    },
    verification::AuthenticatedHeader,
};

fn update_heights(response: &cosmwasm_std::Binary) -> Vec<u64> {
    serde_json::from_slice::<UpdateStateResult>(response)
        .unwrap()
        .heights
        .into_iter()
        .map(|height| height.revision_height)
        .collect()
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
struct Profile(CommonProfile);

#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
struct FailingSerializeProfile(CommonProfile);

impl Serialize for FailingSerializeProfile {
    fn serialize<SerializerType>(
        &self,
        _serializer: SerializerType,
    ) -> Result<SerializerType::Ok, SerializerType::Error>
    where
        SerializerType: Serializer,
    {
        Err(serde::ser::Error::custom(
            "intentional serialization failure",
        ))
    }
}

impl RuntimeProfile for Profile {
    fn common(&self) -> &CommonProfile {
        &self.0
    }
    fn expected_profile_version() -> &'static str {
        "test_attestor"
    }
}

impl RuntimeProfile for FailingSerializeProfile {
    fn common(&self) -> &CommonProfile {
        &self.0
    }

    fn expected_profile_version() -> &'static str {
        "test_attestor"
    }
}

fn storage_snapshot(storage: &dyn Storage) -> [Option<Vec<u8>>; 3] {
    [
        storage.get(crate::store::HOST_CLIENT_STATE_KEY.as_bytes()),
        storage.get(crate::store::consensus_db_key(5).as_bytes()),
        storage.get(crate::store::consensus_db_key(7).as_bytes()),
    ]
}

fn profile() -> Profile {
    Profile(CommonProfile {
        l2_chain_id: 2,
        l2_router: Address::with_last_byte(1),
        commitment_slot: B256::with_last_byte(2),
        profile_version: "test_attestor".into(),
        l2_header_fork: crate::canonical_header::ExecutionHeaderFork::London,
    })
}

fn attestors() -> AttestorConfig {
    AttestorConfig {
        public_keys: vec![Binary::from(vec![1; 32])],
        threshold: 1,
    }
}

fn header(height: u64, block: u8, parent: B256) -> Header {
    Header {
        height: Height::new(height).unwrap(),
        state_root: B256::with_last_byte(block),
        router_storage_root: B256::with_last_byte(block + 1),
        timestamp_seconds: height,
        l2_block_hash: B256::with_last_byte(block + 2),
        parent_hash: parent,
    }
}

fn authenticated(header: Header) -> AuthenticatedHeader {
    AuthenticatedHeader::from_test_header(header)
}

fn bootstrap() -> (ClientState<Profile>, ConsensusState) {
    let header = header(5, 5, B256::with_last_byte(4));
    (
        ClientState {
            latest_height: 5,
            frozen_height: None,
            profile: profile(),
            attestors: attestors(),
        },
        header.consensus_state(0).unwrap(),
    )
}

#[test]
fn stores_higher_updates_and_historical_backfills_without_rewinding() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();

    assert_eq!(
        update_heights(
            &runtime::update::<Profile>(
                &mut storage,
                &authenticated(header(7, 7, B256::ZERO)),
                11,
            )
            .unwrap(),
        ),
        vec![7]
    );
    assert_eq!(
        update_heights(
            &runtime::update::<Profile>(
                &mut storage,
                &authenticated(header(3, 3, B256::ZERO)),
                12,
            )
            .unwrap(),
        ),
        vec![3]
    );
    assert_eq!(
        runtime::client_state::<Profile>(&storage)
            .unwrap()
            .latest_height,
        7
    );
}

#[test]
fn constructs_explicit_update_effects_before_commit() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let plan = runtime::build_update_plan(
        client,
        &authenticated(header(7, 7, B256::ZERO)),
        None,
        None,
        None,
        11,
    )
    .unwrap();

    assert_eq!(plan.client().unwrap().latest_height, 7);
    let (height, consensus) = plan.consensus().unwrap();
    assert_eq!(height, 7);
    assert_eq!(consensus.first_accepted_at, 11);

    let transition = runtime::prepare_transition(&storage, &plan).unwrap();
    assert!(transition.client_envelope().is_some());
    assert_eq!(
        transition.consensus_write().unwrap().0,
        b"consensusStates/0-7"
    );
    assert_eq!(transition.reported_heights(), &[7]);
}

#[test]
fn validates_every_update_effect_before_writing_any_of_them() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let before = storage_snapshot(&storage);

    assert!(matches!(
        runtime::update::<FailingSerializeProfile>(
            &mut storage,
            &authenticated(header(7, 7, B256::ZERO)),
            11,
        ),
        Err(Error::JsonDecode(_))
    ));
    assert_eq!(storage_snapshot(&storage), before);
}

/// A re-submission of a block already stored is ignored.
///
/// First write wins, so the stored state does not depend on arrival order — an honest relayer
/// re-sends the same block routinely (a retry, a cached header, the same block re-attested at a
/// higher head), and with every root identical there is nothing to update.
#[test]
fn re_submitting_a_stored_block_is_ignored() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let original = runtime::consensus_state(&storage, 5).unwrap();

    let resubmission = header(5, 5, B256::with_last_byte(4));
    for accepted_at in [20, 30, 40] {
        assert_eq!(
            update_heights(
                &runtime::update::<Profile>(
                    &mut storage,
                    &authenticated(resubmission.clone()),
                    accepted_at,
                )
                .unwrap(),
            ),
            Vec::<u64>::new(),
            "a re-submission must not be stored as an update"
        );
        assert_eq!(
            runtime::consensus_state(&storage, 5).unwrap(),
            original,
            "a re-submission rewrote the stored state"
        );
    }
}

#[test]
fn same_height_conflict_is_rejected_without_freezing_or_overwriting() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let original = runtime::consensus_state(&storage, 5).unwrap();

    // Anyone can author contradictory bare headers, so a conflict without authenticated
    // attestations is rejected rather than acted upon: the update fails and the client stays open.
    assert!(matches!(
        runtime::update::<Profile>(
            &mut storage,
            &authenticated(header(5, 8, B256::with_last_byte(4))),
            11,
        ),
        Err(Error::StateConflict { height: 5 })
    ));
    assert_eq!(
        runtime::client_state::<Profile>(&storage)
            .unwrap()
            .frozen_height,
        None,
        "a direct conflicting update must not freeze the client"
    );
    assert_eq!(runtime::consensus_state(&storage, 5).unwrap(), original);

    // And the client keeps working: a later honest update still lands.
    assert_eq!(
        update_heights(
            &runtime::update::<Profile>(
                &mut storage,
                &authenticated(header(6, 6, original.l2_block_hash)),
                12,
            )
            .unwrap(),
        ),
        vec![6]
    );
}

#[test]
fn same_height_conflict_with_a_bad_parent_is_rejected_before_the_parent_check() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    runtime::update::<Profile>(
        &mut storage,
        &authenticated(header(4, 2, B256::with_last_byte(1))),
        11,
    )
    .unwrap();
    let original = runtime::consensus_state(&storage, 5).unwrap();
    let conflicting = header(5, 8, B256::with_last_byte(99));

    // The conflict is detected before the parent-hash check, so the reported error names the
    // conflict rather than the bad parent — the ordering is what makes the message useful.
    assert!(matches!(
        runtime::update::<Profile>(&mut storage, &authenticated(conflicting), 12),
        Err(Error::StateConflict { height: 5 })
    ));
    assert_eq!(
        runtime::client_state::<Profile>(&storage)
            .unwrap()
            .frozen_height,
        None
    );
    assert_eq!(runtime::consensus_state(&storage, 5).unwrap(), original);
}

#[test]
fn parent_links_are_checked_in_both_directions() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let block_five = consensus.l2_block_hash;
    let six = header(6, 6, block_five);
    runtime::update::<Profile>(&mut storage, &authenticated(six), 11).unwrap();

    let bad_seven = header(7, 7, B256::with_last_byte(99));
    assert!(matches!(
        runtime::update::<Profile>(&mut storage, &authenticated(bad_seven), 12),
        Err(Error::ParentHashMismatch)
    ));

    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    runtime::update::<Profile>(
        &mut storage,
        &authenticated(header(7, 7, B256::with_last_byte(99))),
        11,
    )
    .unwrap();
    assert!(matches!(
        runtime::update::<Profile>(
            &mut storage,
            &authenticated(header(6, 6, consensus.l2_block_hash)),
            12,
        ),
        Err(Error::ParentHashMismatch)
    ));
}

#[test]
fn bootstrap_rejects_zero_roots_and_wrong_profile_version() {
    let mut storage = MockStorage::new();
    let (mut client, mut consensus) = bootstrap();
    consensus.state_root = B256::ZERO;
    assert!(matches!(
        runtime::instantiate(&mut storage, &client, &consensus, vec![], 1),
        Err(Error::InvalidHeader(_))
    ));

    let (_, consensus) = bootstrap();
    client.profile.0.profile_version = "wrong".into();
    assert!(matches!(
        runtime::instantiate(&mut storage, &client, &consensus, vec![], 1),
        Err(Error::InvalidProfileVersion)
    ));

    let (mut client, consensus) = bootstrap();
    client.frozen_height = Some(0);
    assert!(matches!(
        runtime::instantiate(&mut storage, &client, &consensus, vec![], 1),
        Err(Error::ZeroHeight)
    ));

    let (mut client, consensus) = bootstrap();
    client.profile.0.l2_chain_id = 0;
    assert!(matches!(
        runtime::instantiate(&mut storage, &client, &consensus, vec![], 1),
        Err(Error::InvalidHeader("L2 chain ID must be non-zero"))
    ));
}

#[test]
fn only_explicit_authenticated_misbehaviour_freezes() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let first = authenticated(header(5, 5, B256::with_last_byte(4)));
    let second = authenticated(header(5, 8, B256::with_last_byte(4)));

    assert!(runtime::apply_misbehaviour::<Profile>(&mut storage, &first, &second).unwrap());
    assert_eq!(
        runtime::client_state::<Profile>(&storage)
            .unwrap()
            .frozen_height,
        Some(5)
    );
}

#[test]
fn keep_attestors_is_byte_identical_and_replace_changes_only_the_inner_set() {
    let mut storage = MockStorage::new();
    let (mut client, consensus) = bootstrap();
    client.frozen_height = Some(4);
    runtime::instantiate(&mut storage, &client, &consensus, vec![9, 8, 7], 10).unwrap();
    let client_key = crate::store::HOST_CLIENT_STATE_KEY.as_bytes();
    let consensus_key = crate::store::consensus_db_key(5);
    let before_client = storage.get(client_key).unwrap();
    let before_consensus = storage.get(consensus_key.as_bytes()).unwrap();
    let before_envelope = crate::store::get_wasm_client_state(&storage).unwrap();

    runtime::migrate::<Profile>(&mut storage, MigrateMsg::KeepAttestors {}).unwrap();
    assert_eq!(storage.get(client_key).unwrap(), before_client);
    assert_eq!(
        storage.get(consensus_key.as_bytes()).unwrap(),
        before_consensus
    );

    let replacement = AttestorConfig {
        public_keys: vec![Binary::from(vec![1; 32]), Binary::from(vec![2; 32])],
        threshold: 2,
    };
    runtime::migrate::<Profile>(
        &mut storage,
        MigrateMsg::ReplaceAttestors {
            public_keys: replacement.public_keys.clone(),
            threshold: replacement.threshold,
        },
    )
    .unwrap();

    let migrated = runtime::client_state::<Profile>(&storage).unwrap();
    assert_eq!(migrated.latest_height, client.latest_height);
    assert_eq!(migrated.frozen_height, client.frozen_height);
    assert_eq!(migrated.profile, client.profile);
    assert_eq!(migrated.attestors, replacement);
    let after_envelope = crate::store::get_wasm_client_state(&storage).unwrap();
    assert_eq!(after_envelope.checksum, before_envelope.checksum);
    assert_eq!(after_envelope.latest_height, before_envelope.latest_height);
    assert_eq!(
        storage.get(consensus_key.as_bytes()).unwrap(),
        before_consensus
    );
}

#[test]
fn missing_attestors_is_rejected_for_every_migration_without_writes() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9, 8, 7], 10).unwrap();
    let client_key = crate::store::HOST_CLIENT_STATE_KEY.as_bytes();
    let consensus_key = crate::store::consensus_db_key(5);
    let mut envelope = crate::store::get_wasm_client_state(&storage).unwrap();
    let mut legacy: serde_json::Value = serde_json::from_slice(&envelope.data).unwrap();
    legacy.as_object_mut().unwrap().remove("attestors");
    envelope.data = serde_json::to_vec(&legacy).unwrap();
    crate::store::store_wasm_client_state(&mut storage, &envelope).unwrap();
    let before = storage_snapshot(&storage);

    let replacement = AttestorConfig {
        public_keys: vec![Binary::from(vec![1; 32]), Binary::from(vec![2; 32])],
        threshold: 2,
    };
    for msg in [
        MigrateMsg::KeepAttestors {},
        MigrateMsg::ReplaceAttestors {
            public_keys: replacement.public_keys.clone(),
            threshold: replacement.threshold,
        },
    ] {
        assert!(matches!(
            runtime::migrate::<Profile>(&mut storage, msg),
            Err(Error::UnsupportedLegacyClientState)
        ));
        assert_eq!(storage_snapshot(&storage), before);
    }
    assert_eq!(storage.get(client_key).unwrap(), before[0].clone().unwrap());
    assert_eq!(
        storage.get(consensus_key.as_bytes()).unwrap(),
        before[1].clone().unwrap()
    );
}

#[test]
fn legacy_profile_version_is_rejected_for_every_migration_without_writes() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let mut envelope = crate::store::get_wasm_client_state(&storage).unwrap();
    let mut legacy: serde_json::Value = serde_json::from_slice(&envelope.data).unwrap();
    legacy["profile"]["profile_version"] = serde_json::Value::String("test_attestor_v1".into());
    envelope.data = serde_json::to_vec(&legacy).unwrap();
    crate::store::store_wasm_client_state(&mut storage, &envelope).unwrap();
    let before = storage_snapshot(&storage);

    for msg in [
        MigrateMsg::KeepAttestors {},
        MigrateMsg::ReplaceAttestors {
            public_keys: vec![Binary::from(vec![2; 32])],
            threshold: 1,
        },
    ] {
        assert!(matches!(
            runtime::migrate::<Profile>(&mut storage, msg),
            Err(Error::InvalidProfileVersion)
        ));
        assert_eq!(storage_snapshot(&storage), before);
    }
}

#[test]
fn invalid_or_unserializable_attestor_replacement_is_atomic() {
    let mut storage = MockStorage::new();
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let before = storage_snapshot(&storage);

    let invalid_replacements = [
        (vec![], 1, "invalid attestor set"),
        (
            (0..=MAX_ATTESTORS)
                .map(|index| Binary::from(vec![u8::try_from(index).unwrap(); 32]))
                .collect(),
            1,
            "attestor set exceeds maximum of 32",
        ),
        (
            vec![Binary::from(vec![1; 32])],
            0,
            "invalid attestor threshold",
        ),
        (
            vec![Binary::from(vec![1; 32])],
            2,
            "invalid attestor threshold",
        ),
        (
            vec![Binary::from(vec![1; 31])],
            1,
            "invalid attestor public key length",
        ),
        (
            vec![Binary::from(vec![1; 32]), Binary::from(vec![1; 32])],
            1,
            "invalid attestor set",
        ),
        (
            vec![Binary::from(vec![2; 32]), Binary::from(vec![1; 32])],
            2,
            "invalid attestor set",
        ),
    ];
    for (public_keys, threshold, expected) in invalid_replacements {
        let error = runtime::migrate::<Profile>(
            &mut storage,
            MigrateMsg::ReplaceAttestors {
                public_keys,
                threshold,
            },
        )
        .unwrap_err();
        assert_eq!(error.to_string(), expected);
        assert_eq!(storage_snapshot(&storage), before);
    }

    assert!(matches!(
        runtime::migrate::<FailingSerializeProfile>(
            &mut storage,
            MigrateMsg::ReplaceAttestors {
                public_keys: vec![Binary::from(vec![2; 32])],
                threshold: 1,
            },
        ),
        Err(Error::JsonDecode(_))
    ));
    assert_eq!(storage_snapshot(&storage), before);
}

#[test]
fn small_order_attestor_keys_are_rejected_at_install_and_rotation_but_not_on_load() {
    let identity = {
        let mut key = vec![0; 32];
        key[0] = 1;
        Binary::from(key)
    };

    // Install: nothing is written.
    let mut storage = MockStorage::new();
    let (mut client, consensus) = bootstrap();
    client.attestors.public_keys = vec![identity.clone()];
    assert!(matches!(
        runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10),
        Err(Error::InvalidAttestorPublicKey)
    ));
    assert_eq!(storage_snapshot(&storage), [None, None, None]);
    // The same client still passes the validation that runs on every call.
    assert!(client.validate().is_ok());

    // Rotation: the stored client is left untouched.
    let (client, consensus) = bootstrap();
    runtime::instantiate(&mut storage, &client, &consensus, vec![9], 10).unwrap();
    let before = storage_snapshot(&storage);
    assert!(matches!(
        runtime::migrate::<Profile>(
            &mut storage,
            MigrateMsg::ReplaceAttestors {
                public_keys: vec![identity],
                threshold: 1,
            },
        ),
        Err(Error::InvalidAttestorPublicKey)
    ));
    assert_eq!(storage_snapshot(&storage), before);
}
