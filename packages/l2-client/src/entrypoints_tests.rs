use alloy_primitives::{Address, Bloom, Bytes, FixedBytes, B256, U256};
use cosmwasm_std::{
    testing::{mock_dependencies, mock_env},
    Api, Binary, Storage,
};
use serde::{Deserialize, Serialize};

use crate::{
    canonical_header::{CanonicalEvmHeader, ExecutionHeaderFork},
    entrypoints,
    error::Error,
    msg::{
        ClientMessage, EvmAccountProof, IndexedAttestorSignature, MigrateMsg, QueryMsg,
        SignedAttestedL2Header, SudoMsg,
    },
    runtime,
    state::{
        AttestorConfig, ClientState, CommonProfile, ConsensusState, Header, Height, RuntimeProfile,
    },
    verification::AuthenticatedHeader,
    L2LightClient,
};

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
struct Profile(CommonProfile);

impl RuntimeProfile for Profile {
    fn common(&self) -> &CommonProfile {
        &self.0
    }

    fn expected_profile_version() -> &'static str {
        "entrypoint_test"
    }
}

fn storage_snapshot(storage: &dyn Storage, heights: &[u64]) -> Vec<Option<Vec<u8>>> {
    std::iter::once(storage.get(crate::store::HOST_CLIENT_STATE_KEY.as_bytes()))
        .chain(
            heights
                .iter()
                .map(|height| storage.get(crate::store::consensus_db_key(*height).as_bytes())),
        )
        .collect()
}

struct Adapter;

impl L2LightClient for Adapter {
    type Profile = Profile;

    fn verify(
        _api: &dyn Api,
        _client: &ClientState<Profile>,
        header: &SignedAttestedL2Header,
    ) -> Result<AuthenticatedHeader, Error> {
        Ok(AuthenticatedHeader::from_test_header(normalize(header)))
    }

    fn verify_misbehaviour(
        api: &dyn Api,
        client: &ClientState<Profile>,
        first: &SignedAttestedL2Header,
        second: &SignedAttestedL2Header,
    ) -> Result<(AuthenticatedHeader, AuthenticatedHeader), Error> {
        Ok((
            Self::verify(api, client, first)?,
            Self::verify(api, client, second)?,
        ))
    }
}

fn profile() -> Profile {
    Profile(CommonProfile {
        l2_chain_id: 2,
        l2_router: Address::with_last_byte(1),
        commitment_slot: B256::with_last_byte(2),
        profile_version: "entrypoint_test".into(),
        l2_header_fork: ExecutionHeaderFork::London,
    })
}

fn attestors() -> AttestorConfig {
    AttestorConfig {
        public_keys: vec![Binary::from(vec![1; 32])],
        threshold: 1,
    }
}

fn attested(block: u8) -> SignedAttestedL2Header {
    SignedAttestedL2Header {
        l2_header: CanonicalEvmHeader {
            parent_hash: B256::with_last_byte(4),
            ommers_hash: B256::with_last_byte(2),
            beneficiary: Address::with_last_byte(3),
            state_root: B256::with_last_byte(block),
            transactions_root: B256::with_last_byte(block + 1),
            receipts_root: B256::with_last_byte(6),
            logs_bloom: Bloom::ZERO,
            difficulty: U256::ZERO,
            number: 5,
            gas_limit: 30_000_000,
            gas_used: 21_000,
            timestamp: 1_700_000_000,
            extra_data: Bytes::new(),
            mix_hash: B256::with_last_byte(block + 2),
            nonce: FixedBytes::ZERO,
            base_fee_per_gas: Some(U256::from(9)),
            withdrawals_root: None,
            blob_gas_used: None,
            excess_blob_gas: None,
            parent_beacon_block_root: None,
            requests_hash: None,
        },
        router_proof: EvmAccountProof { proof: vec![] },
        attestor_signature: vec![IndexedAttestorSignature {
            attestor_index: 0,
            signature: Binary::from(vec![block; 64]),
        }],
    }
}

fn normalize(header: &SignedAttestedL2Header) -> Header {
    Header {
        height: Height::new(header.l2_header.number).unwrap(),
        state_root: header.l2_header.state_root,
        router_storage_root: header.l2_header.transactions_root,
        timestamp_seconds: header.l2_header.timestamp,
        l2_block_hash: header.l2_header.mix_hash,
        parent_hash: header.l2_header.parent_hash,
    }
}

#[derive(Serialize)]
struct HistoricalUnsignedV1Header {
    l2_header: CanonicalEvmHeader,
    router_proof: EvmAccountProof,
}

fn unsigned(block: u8) -> HistoricalUnsignedV1Header {
    let signed = attested(block);
    HistoricalUnsignedV1Header {
        l2_header: signed.l2_header,
        router_proof: signed.router_proof,
    }
}

#[test]
fn verify_client_message_rejects_non_conflicting_misbehaviour_envelopes() {
    let mut deps = mock_dependencies();
    let trusted = normalize(&attested(5));
    let client = ClientState {
        latest_height: 5,
        frozen_height: None,
        profile: profile(),
        attestors: attestors(),
    };
    let consensus: ConsensusState = trusted.consensus_state(0).unwrap();
    runtime::instantiate(deps.as_mut().storage, &client, &consensus, vec![9], 10).unwrap();

    let identical = attested(5);
    let mut different_height = attested(8);
    different_height.l2_header.number = 6;

    for message in [
        ClientMessage::Misbehaviour {
            header_1: identical.clone(),
            header_2: identical,
        },
        ClientMessage::Misbehaviour {
            header_1: attested(5),
            header_2: different_height,
        },
    ] {
        let client_message = Binary::from(serde_json::to_vec(&message).unwrap());

        assert!(matches!(
            entrypoints::query::<Adapter>(
                deps.as_ref(),
                QueryMsg::VerifyClientMessage { client_message }
            ),
            Err(Error::InvalidHeader("headers do not prove misbehaviour"))
        ));
    }
}

/// Every host path must refuse the unsigned-v1 wire before invoking an adapter.
#[test]
fn host_paths_reject_conflicts_without_authenticated_attestations() {
    let mut deps = mock_dependencies();
    let trusted = normalize(&attested(5));
    let client = ClientState {
        latest_height: 5,
        frozen_height: None,
        profile: profile(),
        attestors: attestors(),
    };
    let consensus: ConsensusState = trusted.consensus_state(0).unwrap();
    runtime::instantiate(deps.as_mut().storage, &client, &consensus, vec![9], 10).unwrap();

    let client_message = Binary::from(
        serde_json::to_vec(&ClientMessage::Misbehaviour {
            header_1: unsigned(5),
            header_2: unsigned(9),
        })
        .unwrap(),
    );

    for error in [
        entrypoints::query::<Adapter>(
            deps.as_ref(),
            QueryMsg::VerifyClientMessage {
                client_message: client_message.clone(),
            },
        )
        .unwrap_err(),
        entrypoints::query::<Adapter>(
            deps.as_ref(),
            QueryMsg::CheckForMisbehaviour {
                client_message: client_message.clone(),
            },
        )
        .unwrap_err(),
        entrypoints::sudo::<Adapter>(
            deps.as_mut(),
            &mock_env(),
            SudoMsg::UpdateStateOnMisbehaviour { client_message },
        )
        .unwrap_err(),
    ] {
        assert!(
            matches!(error, Error::UnsupportedUnsignedHeader),
            "expected an unsigned-v1 header to be refused, got {error:?}"
        );
    }
    assert_eq!(
        runtime::client_state::<Profile>(deps.as_ref().storage)
            .unwrap()
            .frozen_height,
        None,
        "unsigned evidence must not freeze the client"
    );
}

#[test]
fn non_zero_delay_precedes_path_height_state_and_proof_validation() {
    let mut deps = mock_dependencies();
    let before = storage_snapshot(deps.as_ref().storage, &[0, 5]);

    for (message, expected_time, expected_blocks) in [
        (
            SudoMsg::VerifyMembership {
                height: crate::msg::IbcHeight {
                    revision_number: 9,
                    revision_height: 0,
                },
                delay_time_period: 7,
                delay_block_period: 0,
                proof: Binary::from(b"not-json"),
                merkle_path: crate::msg::MerklePath { key_path: vec![] },
                value: Binary::default(),
            },
            7,
            0,
        ),
        (
            SudoMsg::VerifyNonMembership {
                height: crate::msg::IbcHeight {
                    revision_number: 9,
                    revision_height: 0,
                },
                delay_time_period: 0,
                delay_block_period: 3,
                proof: Binary::from(b"not-json"),
                merkle_path: crate::msg::MerklePath { key_path: vec![] },
            },
            0,
            3,
        ),
    ] {
        assert!(matches!(
            entrypoints::sudo::<Adapter>(deps.as_mut(), &mock_env(), message),
            Err(Error::UnsupportedNonZeroDelay {
                delay_time_period,
                delay_block_period,
            }) if delay_time_period == expected_time && delay_block_period == expected_blocks
        ));
        assert_eq!(storage_snapshot(deps.as_ref().storage, &[0, 5]), before);
    }
}

#[test]
fn zero_delay_validates_height_and_state_before_path_and_proof() {
    let mut deps = mock_dependencies();
    let malformed = Binary::from(b"not-json");

    let invalid_height = SudoMsg::VerifyMembership {
        height: crate::msg::IbcHeight {
            revision_number: 4,
            revision_height: 0,
        },
        delay_time_period: 0,
        delay_block_period: 0,
        proof: malformed.clone(),
        merkle_path: crate::msg::MerklePath { key_path: vec![] },
        value: Binary::default(),
    };
    assert!(matches!(
        entrypoints::sudo::<Adapter>(deps.as_mut(), &mock_env(), invalid_height),
        Err(Error::InvalidRevision(4))
    ));

    let trusted = normalize(&attested(5));
    let client = ClientState {
        latest_height: 5,
        frozen_height: None,
        profile: profile(),
        attestors: attestors(),
    };
    let consensus = trusted.consensus_state(0).unwrap();
    runtime::instantiate(deps.as_mut().storage, &client, &consensus, vec![9], 10).unwrap();

    let missing_state = SudoMsg::VerifyNonMembership {
        height: crate::msg::IbcHeight {
            revision_number: 0,
            revision_height: 6,
        },
        delay_time_period: 0,
        delay_block_period: 0,
        proof: malformed,
        merkle_path: crate::msg::MerklePath { key_path: vec![] },
    };
    assert!(matches!(
        entrypoints::sudo::<Adapter>(deps.as_mut(), &mock_env(), missing_state),
        Err(Error::ConsensusStateMissing(6))
    ));
}

#[test]
fn status_bytes_and_frozen_update_outcome_are_stable() {
    for (frozen_height, expected) in [
        (None, br#"{"status":"Active"}"#.as_slice()),
        (Some(5), br#"{"status":"Frozen"}"#.as_slice()),
    ] {
        let mut deps = mock_dependencies();
        let trusted = normalize(&attested(5));
        let client = ClientState {
            latest_height: 5,
            frozen_height,
            profile: profile(),
            attestors: attestors(),
        };
        let consensus = trusted.consensus_state(0).unwrap();
        runtime::instantiate(deps.as_mut().storage, &client, &consensus, vec![9], 10).unwrap();

        let status = entrypoints::query::<Adapter>(deps.as_ref(), QueryMsg::Status {}).unwrap();
        assert_eq!(status.as_slice(), expected);

        if let Some(height) = frozen_height {
            let before = storage_snapshot(deps.as_ref().storage, &[5, 6]);
            let client_message =
                Binary::from(serde_json::to_vec(&ClientMessage::Header(attested(6))).unwrap());
            assert!(matches!(
                entrypoints::sudo::<Adapter>(
                    deps.as_mut(),
                    &mock_env(),
                    SudoMsg::UpdateState { client_message },
                ),
                Err(Error::Frozen(actual)) if actual == height
            ));
            assert_eq!(storage_snapshot(deps.as_ref().storage, &[5, 6]), before);

            let proof = SudoMsg::VerifyMembership {
                height: crate::msg::IbcHeight {
                    revision_number: 0,
                    revision_height: 5,
                },
                delay_time_period: 0,
                delay_block_period: 0,
                proof: Binary::from(b"not-json"),
                merkle_path: crate::msg::MerklePath { key_path: vec![] },
                value: Binary::default(),
            };
            assert!(matches!(
                entrypoints::sudo::<Adapter>(deps.as_mut(), &mock_env(), proof),
                Err(Error::Frozen(actual)) if actual == height
            ));

            let repeated_freeze = Binary::from(
                serde_json::to_vec(&ClientMessage::Misbehaviour {
                    header_1: attested(5),
                    header_2: attested(9),
                })
                .unwrap(),
            );
            assert!(matches!(
                entrypoints::sudo::<Adapter>(
                    deps.as_mut(),
                    &mock_env(),
                    SudoMsg::UpdateStateOnMisbehaviour {
                        client_message: repeated_freeze,
                    },
                ),
                Err(Error::Frozen(actual)) if actual == height
            ));
            assert_eq!(storage_snapshot(deps.as_ref().storage, &[5, 6]), before);
        }
    }
}

#[test]
fn migrate_entrypoint_rotates_the_attestor_set_without_response_side_effects() {
    let mut deps = mock_dependencies();
    let trusted = normalize(&attested(5));
    let client = ClientState {
        latest_height: 5,
        frozen_height: None,
        profile: profile(),
        attestors: attestors(),
    };
    runtime::instantiate(
        deps.as_mut().storage,
        &client,
        &trusted.consensus_state(0).unwrap(),
        vec![9],
        10,
    )
    .unwrap();

    let response = entrypoints::migrate::<Adapter>(
        deps.as_mut(),
        MigrateMsg::ReplaceAttestors {
            public_keys: vec![Binary::from(vec![1; 32]), Binary::from(vec![2; 32])],
            threshold: 2,
        },
    )
    .unwrap();
    assert!(response.attributes.is_empty());
    assert!(response.events.is_empty());
    assert!(response.messages.is_empty());
    assert!(response.data.is_none());
    assert_eq!(
        runtime::client_state::<Profile>(deps.as_ref().storage)
            .unwrap()
            .attestors
            .threshold,
        2
    );
}
