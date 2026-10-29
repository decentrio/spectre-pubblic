use cosmwasm_std::{testing::MockApi, Api, Binary};

use crate::msg::{
    CheckForMisbehaviourResult, ClientMessage, IbcHeight, MigrateMsg, SignedAttestedL2Header,
    StatusResult, SudoMsg, TimestampAtHeightResult, UpdateStateResult,
};
use crate::{
    canonical_header::ExecutionHeaderFork, error::Error, state::AttestorConfig,
    verification::build_attestation_statement,
};

#[test]
fn decodes_the_shared_go_to_rust_signed_client_message_fixture() {
    let fixture = include_bytes!("../../../test/fixtures/wasm-contracts/l2-client-message.json");
    let message: ClientMessage<SignedAttestedL2Header> = serde_json::from_slice(fixture).unwrap();
    let ClientMessage::Header(header) = message else {
        panic!("shared fixture must contain a header");
    };
    assert_eq!(header.l2_header.number, 42);
    assert_eq!(header.router_proof.proof, vec![vec![1, 2], vec![3]]);
    assert_eq!(header.attestor_signature.len(), 1);
}

#[test]
fn shared_signed_header_has_a_valid_indexed_certificate() {
    let fixture = include_bytes!("../../../test/fixtures/wasm-contracts/l2-client-message.json");
    let signed: ClientMessage<SignedAttestedL2Header> = serde_json::from_slice(fixture).unwrap();
    let ClientMessage::Header(signed) = signed else {
        panic!("shared fixture must contain a header");
    };
    let public_key = Binary::from_base64("iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w=").unwrap();
    let attestors = AttestorConfig {
        public_keys: vec![public_key.clone()],
        threshold: 1,
    };
    let block_hash: [u8; 32] = signed
        .l2_header
        .hash(ExecutionHeaderFork::Prague)
        .unwrap()
        .into();
    let statement = build_attestation_statement(
        11_155_420,
        [
            0x64, 0x52, 0x80, 0x88, 0x57, 0x49, 0xdc, 0x97, 0xea, 0x46, 0x1d, 0xe2, 0x80, 0xeb,
            0x32, 0x73, 0xc9, 0x1d, 0x36, 0xdf,
        ],
        attestors.set_hash().unwrap(),
        signed.l2_header.number,
        block_hash,
        signed.l2_header.state_root.into(),
    );
    let certificate = &signed.attestor_signature[0];
    assert_eq!(certificate.attestor_index, 0);
    assert!(MockApi::default()
        .ed25519_verify(
            &statement,
            certificate.signature.as_slice(),
            public_key.as_slice()
        )
        .unwrap());

    let json = serde_json::to_string(&ClientMessage::Header(signed.clone())).unwrap();
    assert!(json.contains(r#""attestor_signature":["#));
    assert!(!json.contains(r#""signatures""#));
    assert!(json.contains(r#""proof":[[1,2],[3]]"#));
    let decoded: ClientMessage<SignedAttestedL2Header> = serde_json::from_str(&json).unwrap();
    assert_eq!(decoded, ClientMessage::Header(signed));
}

#[test]
fn unsigned_header_does_not_decode_as_signed_header() {
    let fixture =
        include_bytes!("../../../test/fixtures/wasm-contracts/l2-client-message-unsigned.json");
    assert!(serde_json::from_slice::<ClientMessage<SignedAttestedL2Header>>(fixture).is_err());
}

#[test]
fn migrate_wire_is_strict_snake_case() {
    assert_eq!(
        serde_json::to_string(&MigrateMsg::KeepAttestors {}).unwrap(),
        r#"{"keep_attestors":{}}"#
    );
    assert_eq!(
        serde_json::to_string(&MigrateMsg::ReplaceAttestors {
            public_keys: vec![Binary::from(vec![1; 32])],
            threshold: 1,
        })
        .unwrap(),
        r#"{"replace_attestors":{"public_keys":["AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="],"threshold":1}}"#
    );
    assert!(serde_json::from_slice::<MigrateMsg>(
        br#"{"replace_attestors":{"public_keys":["AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="],"threshold":1,"unexpected":true}}"#,
    )
    .is_err());
}

#[test]
fn decodes_the_flat_host_membership_payload() {
    let message: SudoMsg = serde_json::from_slice(
        br#"{"verify_membership":{"height":{"revision_number":0,"revision_height":42},"delay_time_period":1,"delay_block_period":2,"proof":"AQ==","merkle_path":{"key_path":["Y2xpZW50LTA=" ]},"value":"Ag=="}}"#,
    )
    .unwrap();

    match message {
        SudoMsg::VerifyMembership {
            height,
            delay_time_period,
            delay_block_period,
            proof,
            merkle_path,
            value,
        } => {
            assert_eq!(height.revision_height, 42);
            assert_eq!(delay_time_period, 1);
            assert_eq!(delay_block_period, 2);
            assert_eq!(proof, Binary::from([1]));
            assert_eq!(merkle_path.key_path, vec![Binary::from(b"client-0")]);
            assert_eq!(value, Binary::from([2]));
        }
        _ => panic!("unexpected sudo message"),
    }
}

#[test]
fn serializes_host_result_wrappers() {
    assert_eq!(
        serde_json::to_string(&UpdateStateResult {
            heights: vec![IbcHeight {
                revision_number: 0,
                revision_height: 42,
            }],
        })
        .unwrap(),
        r#"{"heights":[{"revision_number":0,"revision_height":42}]}"#
    );
    assert_eq!(
        serde_json::to_string(&CheckForMisbehaviourResult {
            found_misbehaviour: true,
        })
        .unwrap(),
        r#"{"found_misbehaviour":true}"#
    );
    assert_eq!(
        serde_json::to_string(&TimestampAtHeightResult { timestamp: 7 }).unwrap(),
        r#"{"timestamp":7}"#
    );
    assert_eq!(
        serde_json::to_string(&StatusResult {
            status: "Active".into(),
        })
        .unwrap(),
        r#"{"status":"Active"}"#
    );
}

#[test]
fn decodes_misbehaviour_client_message_envelopes() {
    let message: ClientMessage<u64> =
        serde_json::from_slice(br#"{"type":"misbehaviour","value":{"header_1":7,"header_2":7}}"#)
            .unwrap();
    assert_eq!(
        message,
        ClientMessage::Misbehaviour {
            header_1: 7,
            header_2: 7,
        }
    );
}

#[test]
fn decodes_unsupported_lifecycle_messages_without_losing_payloads() {
    let upgrade: SudoMsg = serde_json::from_slice(
        br#"{"verify_upgrade_and_update_state":{"upgrade_client_state":"AQ==","upgrade_consensus_state":"Ag==","proof_upgrade_client":"Aw==","proof_upgrade_consensus_state":"BA=="}}"#,
    )
    .unwrap();
    assert!(matches!(
        upgrade,
        SudoMsg::VerifyUpgradeAndUpdateState {
            upgrade_client_state,
            upgrade_consensus_state,
            proof_upgrade_client,
            proof_upgrade_consensus_state,
        } if upgrade_client_state.as_slice() == [1]
            && upgrade_consensus_state.as_slice() == [2]
            && proof_upgrade_client.as_slice() == [3]
            && proof_upgrade_consensus_state.as_slice() == [4]
    ));

    let recovery: SudoMsg = serde_json::from_slice(br#"{"migrate_client_store":{}}"#).unwrap();
    assert!(matches!(recovery, SudoMsg::MigrateClientStore {}));
}

#[test]
fn unsupported_operation_error_text_is_stable() {
    assert_eq!(
        Error::UnsupportedNonZeroDelay {
            delay_time_period: 7,
            delay_block_period: 3,
        }
        .to_string(),
        "non-zero delay is unsupported: delay_time_period=7, delay_block_period=3"
    );
    assert_eq!(
        Error::UnsupportedLifecycleOperation {
            operation: "verify_upgrade_and_update_state",
        }
        .to_string(),
        "lifecycle operation is unsupported: verify_upgrade_and_update_state"
    );
}
