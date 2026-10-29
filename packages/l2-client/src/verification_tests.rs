use alloy_primitives::{Address, Bloom, Bytes, FixedBytes, B256, U256};
use cosmwasm_std::{
    testing::MockApi, Addr, Api, Binary, CanonicalAddr, RecoverPubkeyError, StdResult,
    VerificationError,
};
use ed25519_zebra::{SigningKey, VerificationKeyBytes};

use crate::{
    canonical_header::{CanonicalEvmHeader, ExecutionHeaderFork},
    error::Error,
    msg::{EvmAccountProof, IndexedAttestorSignature, SignedAttestedL2Header},
    state::{AttestorConfig, ClientState, CommonProfile, RuntimeProfile},
    verification::{
        build_attestation_statement, verify_authenticated_header, verify_authenticated_misbehaviour,
    },
};

#[derive(Clone, Debug, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
struct Profile(CommonProfile);

impl RuntimeProfile for Profile {
    fn common(&self) -> &CommonProfile {
        &self.0
    }

    fn expected_profile_version() -> &'static str {
        "verification_test"
    }
}

fn profile() -> Profile {
    Profile(CommonProfile {
        l2_chain_id: 11_155_420,
        l2_router: "0x645280885749dc97ea461de280eb3273c91d36df"
            .parse()
            .unwrap(),
        commitment_slot: B256::with_last_byte(2),
        profile_version: "verification_test".into(),
        l2_header_fork: ExecutionHeaderFork::Prague,
    })
}

fn header() -> SignedAttestedL2Header {
    SignedAttestedL2Header {
        l2_header: CanonicalEvmHeader {
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
        },
        router_proof: EvmAccountProof { proof: vec![] },
        attestor_signature: vec![],
    }
}

fn sorted_signers(count: usize) -> Vec<(Binary, SigningKey)> {
    let mut signers = (1..=count)
        .map(|index| {
            let signer = SigningKey::from([u8::try_from(index).unwrap(); 32]);
            let public: [u8; 32] = VerificationKeyBytes::from(&signer).into();
            (Binary::from(public), signer)
        })
        .collect::<Vec<_>>();
    signers.sort_by(|left, right| left.0.as_slice().cmp(right.0.as_slice()));
    signers
}

fn client(signers: &[(Binary, SigningKey)], threshold: u16) -> ClientState<Profile> {
    ClientState {
        latest_height: 6,
        frozen_height: None,
        profile: profile(),
        attestors: AttestorConfig {
            public_keys: signers.iter().map(|(key, _)| key.clone()).collect(),
            threshold,
        },
    }
}

fn statement(client: &ClientState<Profile>, header: &SignedAttestedL2Header) -> [u8; 164] {
    let common = client.profile.common();
    let mut router = [0_u8; 20];
    router.copy_from_slice(common.l2_router.as_slice());
    build_attestation_statement(
        common.l2_chain_id,
        router,
        client.attestors.set_hash().unwrap(),
        header.l2_header.number,
        header.l2_header.hash(common.l2_header_fork).unwrap().into(),
        header.l2_header.state_root.into(),
    )
}

fn sign(
    client: &ClientState<Profile>,
    header: &mut SignedAttestedL2Header,
    signers: &[(Binary, SigningKey)],
    indices: &[u16],
) {
    let message = statement(client, header);
    header.attestor_signature = indices
        .iter()
        .map(|index| IndexedAttestorSignature {
            attestor_index: *index,
            signature: Binary::from(<[u8; 64]>::from(
                signers[usize::from(*index)].1.sign(&message),
            )),
        })
        .collect();
}

#[test]
fn authenticates_one_of_one_and_two_of_three_before_router_proof() {
    for (members, threshold, indices) in [(1, 1, vec![0]), (3, 2, vec![0, 2])] {
        let signers = sorted_signers(members);
        let client = client(&signers, threshold);
        let mut header = header();
        sign(&client, &mut header, &signers, &indices);

        assert!(matches!(
            verify_authenticated_header(&MockApi::default(), &client, &header),
            Err(Error::Proof(_))
        ));
    }
}

#[test]
fn rejects_certificate_structure_before_any_crypto_or_trie_work() {
    let signers = sorted_signers(3);
    let client = client(&signers, 2);
    let mut valid = header();
    sign(&client, &mut valid, &signers, &[0, 2]);

    let mut empty = valid.clone();
    empty.attestor_signature.clear();
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &empty),
        Err(Error::UnsupportedUnsignedHeader)
    ));

    let mut under = valid.clone();
    under.attestor_signature.pop();
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &under),
        Err(Error::InvalidAttestorSignatureCount)
    ));

    let mut over = valid.clone();
    over.attestor_signature
        .push(over.attestor_signature[0].clone());
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &over),
        Err(Error::InvalidAttestorSignatureCount)
    ));

    let mut duplicate = valid.clone();
    duplicate.attestor_signature[1].attestor_index = 0;
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &duplicate),
        Err(Error::DuplicateOrUnsortedSignatureIndex)
    ));

    let mut unsorted = valid.clone();
    unsorted.attestor_signature.swap(0, 1);
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &unsorted),
        Err(Error::DuplicateOrUnsortedSignatureIndex)
    ));

    let mut out_of_range = valid.clone();
    out_of_range.attestor_signature[1].attestor_index = 3;
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &out_of_range),
        Err(Error::InvalidAttestorIndex)
    ));

    let mut bad_length = valid.clone();
    bad_length.attestor_signature[0].signature = Binary::from(vec![0; 63]);
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &bad_length),
        Err(Error::InvalidAttestorSignatureLength)
    ));

    let mut long_signature = valid;
    long_signature.attestor_signature[0].signature = Binary::from(vec![0; 65]);
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &long_signature),
        Err(Error::InvalidAttestorSignatureLength)
    ));
}

#[test]
fn rejects_zero_height_before_signature_or_trie_work() {
    let signers = sorted_signers(1);
    let client = client(&signers, 1);
    let mut invalid = header();
    sign(&client, &mut invalid, &signers, &[0]);
    invalid.l2_header.number = 0;
    invalid.router_proof.proof = vec![vec![0xff]];

    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &invalid),
        Err(Error::ZeroHeight)
    ));
}

#[test]
fn negative_fixture_names_every_locked_failure_and_precedence_case() {
    #[derive(serde::Deserialize)]
    struct Fixture {
        schema: String,
        applies_to_profiles: Vec<String>,
        cases: Vec<Case>,
    }
    #[derive(serde::Deserialize)]
    struct Case {
        name: String,
        expected_error: String,
        must_precede_proof: bool,
    }

    let fixture: Fixture = serde_json::from_str(include_str!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../test/fixtures/wasm-contracts/l2-attestation-negative.json"
    )))
    .unwrap();
    assert_eq!(fixture.schema, "spectre_l2_attestation_negative_v1");
    assert_eq!(
        fixture.applies_to_profiles,
        ["op_attestor_v1", "base_attestor_v1", "arbitrum_attestor_v1"]
    );

    let expected = [
        ("missing_signatures", "UnsupportedUnsignedHeader", true),
        ("zero_height", "ZeroHeight", true),
        ("too_few_signatures", "InvalidAttestorSignatureCount", true),
        ("too_many_signatures", "InvalidAttestorSignatureCount", true),
        ("duplicate_index", "DuplicateOrUnsortedSignatureIndex", true),
        ("unsorted_index", "DuplicateOrUnsortedSignatureIndex", true),
        ("out_of_range_index", "InvalidAttestorIndex", true),
        ("short_signature", "InvalidAttestorSignatureLength", true),
        ("long_signature", "InvalidAttestorSignatureLength", true),
        (
            "wrong_signature",
            "AttestorSignatureVerificationFailed",
            true,
        ),
        ("wrong_key", "AttestorSignatureVerificationFailed", true),
        ("wrong_set", "AttestorSignatureVerificationFailed", true),
        ("wrong_chain", "AttestorSignatureVerificationFailed", true),
        ("wrong_router", "AttestorSignatureVerificationFailed", true),
        ("wrong_block", "AttestorSignatureVerificationFailed", true),
        (
            "wrong_state_root",
            "AttestorSignatureVerificationFailed",
            true,
        ),
        (
            "invalid_signature_and_malformed_proof",
            "AttestorSignatureVerificationFailed",
            true,
        ),
        ("valid_signature_and_malformed_proof", "Proof", false),
    ];
    let actual = fixture
        .cases
        .iter()
        .map(|case| {
            (
                case.name.as_str(),
                case.expected_error.as_str(),
                case.must_precede_proof,
            )
        })
        .collect::<Vec<_>>();
    assert_eq!(actual, expected);
}

#[test]
fn every_context_or_signature_mutation_fails_before_malformed_proof() {
    let signers = sorted_signers(1);
    let client = client(&signers, 1);
    let mut valid = header();
    sign(&client, &mut valid, &signers, &[0]);
    valid.router_proof.proof = vec![vec![0xff]];

    let mut cases = vec![];
    let mut bad_signature = valid.clone();
    let mut signature = bad_signature.attestor_signature[0].signature.to_vec();
    signature[0] ^= 1;
    bad_signature.attestor_signature[0].signature = Binary::from(signature);
    cases.push((client.clone(), bad_signature));

    let mut wrong_block = valid.clone();
    wrong_block.l2_header.gas_used += 1;
    cases.push((client.clone(), wrong_block));

    let mut wrong_height = valid.clone();
    wrong_height.l2_header.number += 1;
    cases.push((client.clone(), wrong_height));

    let mut wrong_state_root = valid.clone();
    wrong_state_root.l2_header.state_root = B256::with_last_byte(98);
    cases.push((client.clone(), wrong_state_root));

    let mut wrong_chain = client.clone();
    wrong_chain.profile.0.l2_chain_id += 1;
    cases.push((wrong_chain, valid.clone()));

    let mut wrong_router = client.clone();
    wrong_router.profile.0.l2_router = Address::with_last_byte(99);
    cases.push((wrong_router, valid.clone()));

    let other_signers = sorted_signers(2);
    let wrong_set = self::client(&other_signers[0..1], 1);
    cases.push((wrong_set, valid.clone()));

    let mut wrong_key = client.clone();
    wrong_key.attestors.public_keys[0] = Binary::from([0x55; 32]);
    cases.push((wrong_key, valid));

    for (client, header) in cases {
        assert!(matches!(
            verify_authenticated_header(&MockApi::default(), &client, &header),
            Err(Error::AttestorSignatureVerificationFailed)
        ));
    }
}

#[test]
fn misbehaviour_authenticates_both_certificates_before_either_proof() {
    let signers = sorted_signers(1);
    let client = client(&signers, 1);
    let mut first = header();
    sign(&client, &mut first, &signers, &[0]);
    first.router_proof.proof = vec![vec![0xff]];

    let mut second = header();
    second.l2_header.state_root = B256::with_last_byte(99);
    sign(&client, &mut second, &signers, &[0]);
    let mut signature = second.attestor_signature[0].signature.to_vec();
    signature[0] ^= 1;
    second.attestor_signature[0].signature = Binary::from(signature);

    assert!(matches!(
        verify_authenticated_misbehaviour(&MockApi::default(), &client, &first, &second),
        Err(Error::AttestorSignatureVerificationFailed)
    ));
}

#[test]
fn crypto_host_errors_and_false_share_one_stable_error() {
    let signers = sorted_signers(1);
    let client = client(&signers, 1);
    let mut header = header();
    sign(&client, &mut header, &signers, &[0]);

    let mut false_signature = header.clone();
    let mut signature = false_signature.attestor_signature[0].signature.to_vec();
    signature[0] ^= 1;
    false_signature.attestor_signature[0].signature = Binary::from(signature);
    assert!(matches!(
        verify_authenticated_header(&MockApi::default(), &client, &false_signature),
        Err(Error::AttestorSignatureVerificationFailed)
    ));
    assert!(matches!(
        verify_authenticated_header(&CryptoErrorApi, &client, &header),
        Err(Error::AttestorSignatureVerificationFailed)
    ));
}

struct CryptoErrorApi;

impl Api for CryptoErrorApi {
    fn addr_validate(&self, human: &str) -> StdResult<Addr> {
        MockApi::default().addr_validate(human)
    }

    fn addr_canonicalize(&self, human: &str) -> StdResult<CanonicalAddr> {
        MockApi::default().addr_canonicalize(human)
    }

    fn addr_humanize(&self, canonical: &CanonicalAddr) -> StdResult<Addr> {
        MockApi::default().addr_humanize(canonical)
    }

    fn secp256k1_verify(
        &self,
        message_hash: &[u8],
        signature: &[u8],
        public_key: &[u8],
    ) -> Result<bool, VerificationError> {
        MockApi::default().secp256k1_verify(message_hash, signature, public_key)
    }

    fn secp256k1_recover_pubkey(
        &self,
        message_hash: &[u8],
        signature: &[u8],
        recovery_param: u8,
    ) -> Result<Vec<u8>, RecoverPubkeyError> {
        MockApi::default().secp256k1_recover_pubkey(message_hash, signature, recovery_param)
    }

    fn ed25519_verify(
        &self,
        _message: &[u8],
        _signature: &[u8],
        _public_key: &[u8],
    ) -> Result<bool, VerificationError> {
        Err(VerificationError::GenericErr)
    }

    fn ed25519_batch_verify(
        &self,
        messages: &[&[u8]],
        signatures: &[&[u8]],
        public_keys: &[&[u8]],
    ) -> Result<bool, VerificationError> {
        MockApi::default().ed25519_batch_verify(messages, signatures, public_keys)
    }

    fn debug(&self, message: &str) {
        MockApi::default().debug(message);
    }
}
