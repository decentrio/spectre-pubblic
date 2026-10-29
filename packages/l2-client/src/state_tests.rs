use cosmwasm_std::{testing::MockApi, Api, Binary};
use serde::Deserialize;
use sha2::{Digest, Sha256};

use crate::{
    error::Error,
    state::{has_canonical_y, AttestorConfig, MAX_ATTESTORS, SMALL_ORDER_ED25519_KEYS},
    verification::{
        build_attestation_statement, ATTESTATION_PROTOCOL_DOMAIN, ATTESTATION_STATEMENT_LENGTH,
    },
};

fn key(byte: u8) -> Binary {
    Binary::from(vec![byte; 32])
}

#[test]
fn accepts_canonical_attestor_sets_at_both_bounds() {
    let one = AttestorConfig {
        public_keys: vec![key(1)],
        threshold: 1,
    };
    assert!(one.validate().is_ok());

    let maximum = AttestorConfig {
        public_keys: (0..MAX_ATTESTORS)
            .map(|index| key(u8::try_from(index).unwrap()))
            .collect(),
        threshold: u16::try_from(MAX_ATTESTORS).unwrap(),
    };
    assert!(maximum.validate().is_ok());
}

#[test]
fn rejects_every_noncanonical_attestor_set_shape() {
    let cases = [
        (
            AttestorConfig {
                public_keys: vec![],
                threshold: 1,
            },
            "invalid attestor set",
        ),
        (
            AttestorConfig {
                public_keys: vec![key(1); MAX_ATTESTORS + 1],
                threshold: 1,
            },
            "attestor set exceeds maximum of 32",
        ),
        (
            AttestorConfig {
                public_keys: vec![key(1)],
                threshold: 0,
            },
            "invalid attestor threshold",
        ),
        (
            AttestorConfig {
                public_keys: vec![key(1)],
                threshold: 2,
            },
            "invalid attestor threshold",
        ),
        (
            AttestorConfig {
                public_keys: vec![Binary::from(vec![1; 31])],
                threshold: 1,
            },
            "invalid attestor public key length",
        ),
        (
            AttestorConfig {
                public_keys: vec![key(1), key(1)],
                threshold: 1,
            },
            "invalid attestor set",
        ),
        (
            AttestorConfig {
                public_keys: vec![key(2), key(1)],
                threshold: 1,
            },
            "invalid attestor set",
        ),
    ];

    for (config, expected) in cases {
        assert_eq!(config.validate().unwrap_err().to_string(), expected);
    }
}

#[test]
fn hashes_threshold_count_and_sorted_raw_keys() {
    let config = AttestorConfig {
        public_keys: vec![key(1), key(2), key(3)],
        threshold: 2,
    };
    assert_eq!(
        hex::encode(config.set_hash().unwrap()),
        "0e46b5cb19c2489cffffa98793641e03fc46f7b4668cebf1e8cce47c4935dbe5"
    );
}

#[test]
fn authentication_error_text_is_stable() {
    let cases = [
        (Error::InvalidAttestorSet, "invalid attestor set"),
        (
            Error::TooManyAttestors,
            "attestor set exceeds maximum of 32",
        ),
        (
            Error::InvalidAttestorThreshold,
            "invalid attestor threshold",
        ),
        (
            Error::InvalidAttestorPublicKeyLength,
            "invalid attestor public key length",
        ),
        (
            Error::InvalidAttestorSignatureCount,
            "invalid attestor signature count",
        ),
        (Error::InvalidAttestorIndex, "invalid attestor index"),
        (
            Error::DuplicateOrUnsortedSignatureIndex,
            "duplicate or unsorted attestor signature index",
        ),
        (
            Error::InvalidAttestorSignatureLength,
            "invalid attestor signature length",
        ),
        (
            Error::AttestorSignatureVerificationFailed,
            "attestor signature verification failed",
        ),
        (
            Error::UnsupportedUnsignedHeader,
            "unsigned L2 headers are unsupported",
        ),
        (
            Error::UnsupportedLegacyClientState,
            "legacy L2 client state is unsupported; create a fresh authenticated client",
        ),
    ];

    for (error, expected) in cases {
        assert_eq!(error.to_string(), expected);
    }
}

#[derive(Deserialize)]
struct VectorFile {
    protocol_domain: String,
    cases: Vec<VectorCase>,
}

#[derive(Deserialize)]
struct VectorCase {
    threshold: u16,
    public_keys: Vec<Binary>,
    attestor_set_hash: String,
    context: VectorContext,
    statement: String,
    signatures: Vec<VectorSignature>,
}

#[derive(Deserialize)]
struct VectorContext {
    l2_chain_id: u64,
    router_address: String,
    block_number: u64,
    block_hash: String,
    state_root: String,
}

#[derive(Deserialize)]
struct VectorSignature {
    attestor_index: u16,
    public_key: Binary,
    signature: Binary,
}

fn fixed<const N: usize>(encoded: &str) -> [u8; N] {
    hex::decode(encoded).unwrap().try_into().unwrap()
}

#[test]
fn v1_domain_and_statement_offsets_are_wire_locked() {
    let router = [0x11; 20];
    let set_hash = [0x22; 32];
    let block_hash = [0x33; 32];
    let state_root = [0x44; 32];
    let statement = build_attestation_statement(
        0x0102_0304_0506_0708,
        router,
        set_hash,
        0x1112_1314_1516_1718,
        block_hash,
        state_root,
    );

    assert_eq!(ATTESTATION_STATEMENT_LENGTH, 164);
    assert_eq!(
        hex::encode(ATTESTATION_PROTOCOL_DOMAIN),
        "7ee55ff0a9e64c455d44d77fa7ee083945f82a38ed7fb638516854726bd440ad"
    );
    assert_eq!(&statement[0..32], &ATTESTATION_PROTOCOL_DOMAIN);
    assert_eq!(&statement[32..40], &0x0102_0304_0506_0708_u64.to_be_bytes());
    assert_eq!(&statement[40..60], &router);
    assert_eq!(&statement[60..92], &set_hash);
    assert_eq!(
        &statement[92..100],
        &0x1112_1314_1516_1718_u64.to_be_bytes()
    );
    assert_eq!(&statement[100..132], &block_hash);
    assert_eq!(&statement[132..164], &state_root);
}

#[test]
fn decodes_every_go_vector_byte_for_byte() {
    let fixture =
        include_bytes!("../../../test/fixtures/wasm-contracts/l2-attestation-vectors.json");
    let vectors: VectorFile = serde_json::from_slice(fixture).unwrap();
    assert_eq!(
        vectors.protocol_domain,
        hex::encode(ATTESTATION_PROTOCOL_DOMAIN)
    );

    for vector in vectors.cases {
        let config = AttestorConfig {
            public_keys: vector.public_keys,
            threshold: vector.threshold,
        };
        let set_hash = config.set_hash().unwrap();
        assert_eq!(hex::encode(set_hash), vector.attestor_set_hash);

        let statement = build_attestation_statement(
            vector.context.l2_chain_id,
            fixed(&vector.context.router_address),
            set_hash,
            vector.context.block_number,
            fixed(&vector.context.block_hash),
            fixed(&vector.context.state_root),
        );
        assert_eq!(statement.len(), ATTESTATION_STATEMENT_LENGTH);
        assert_eq!(hex::encode(statement), vector.statement);
        assert_eq!(vector.signatures.len(), usize::from(vector.threshold));
        for (expected_index, signature) in vector.signatures.into_iter().enumerate() {
            assert_eq!(usize::from(signature.attestor_index), expected_index);
            assert_eq!(signature.public_key, config.public_keys[expected_index]);
            assert_eq!(signature.signature.len(), 64);
            assert!(MockApi::default()
                .ed25519_verify(
                    &statement,
                    signature.signature.as_slice(),
                    signature.public_key.as_slice(),
                )
                .unwrap());
        }
    }
}

fn single_key(key: [u8; 32]) -> AttestorConfig {
    AttestorConfig {
        public_keys: vec![Binary::from(key.to_vec())],
        threshold: 1,
    }
}

/// The 2^255 - 19 field prime, split into the high and low 128 bits of a little-endian integer.
const PRIME: (u128, u128) = ((1 << 127) - 1, u128::MAX - 18);

/// `y >= p`, computed on 128-bit halves instead of byte patterns.
fn y_is_at_least_prime(key: &[u8; 32]) -> bool {
    let mut y = *key;
    y[31] &= 0x7f;
    let low = u128::from_le_bytes(y[..16].try_into().unwrap());
    let high = u128::from_le_bytes(y[16..].try_into().unwrap());
    (high, low) >= PRIME
}

#[test]
fn the_small_order_table_is_exactly_what_curve25519_dalek_decodes_to_small_order() {
    use curve25519_dalek::{constants::EIGHT_TORSION, edwards::CompressedEdwardsY};

    // Every small-order point is in EIGHT_TORSION, and an encoding is either its canonical one or
    // the same with the sign bit flipped. Keep the flipped ones that still decode: x = 0 points
    // accept either sign bit, and the others decode to their negative.
    let mut expected = std::collections::BTreeSet::new();
    for point in EIGHT_TORSION {
        for sign in [0x00_u8, 0x80] {
            let mut encoding = point.compress().to_bytes();
            encoding[31] = (encoding[31] & 0x7f) | sign;
            if let Some(decoded) = CompressedEdwardsY(encoding).decompress() {
                assert!(decoded.is_small_order());
                expected.insert(encoding);
            }
        }
    }

    assert_eq!(
        SMALL_ORDER_ED25519_KEYS
            .iter()
            .copied()
            .collect::<std::collections::BTreeSet<_>>(),
        expected
    );
}

#[test]
fn install_rejects_every_small_order_key_that_validate_still_accepts() {
    for key in SMALL_ORDER_ED25519_KEYS {
        let config = single_key(key);

        // Loading a client that already holds such a key must keep working until it is rotated.
        assert!(config.validate().is_ok());
        assert!(matches!(
            config.validate_for_install(),
            Err(Error::InvalidAttestorPublicKey)
        ));
    }
}

#[test]
fn install_rejects_every_non_canonical_y_encoding() {
    use curve25519_dalek::edwards::CompressedEdwardsY;

    // y in [p, 2^255) is the only range with a second encoding: p + 18 is 0x7f..ff.
    for offset in 0..=18_u8 {
        for sign in [0x00_u8, 0x80] {
            let mut key = [0xff; 32];
            key[0] = 0xed + offset;
            key[31] = 0x7f | sign;

            assert!(!has_canonical_y(&key));
            assert!(matches!(
                single_key(key).validate_for_install(),
                Err(Error::InvalidAttestorPublicKey)
            ));
            // It really is a second encoding: whatever it decodes to re-encodes differently.
            if let Some(point) = CompressedEdwardsY(key).decompress() {
                assert_ne!(point.compress().to_bytes(), key);
            }
        }
    }
}

#[test]
fn canonical_y_check_agrees_with_an_integer_comparison_around_the_prime() {
    let mut keys = vec![[0; 32], [0xff; 32]];
    // Walk both sides of p in the low byte, with every high byte at its boundary value.
    for low in 0xd0..=0xff_u8 {
        for fill in [0xff_u8, 0xfe, 0x00] {
            for top in [0x7f_u8, 0xff, 0x7e] {
                let mut key = [fill; 32];
                key[0] = low;
                key[31] = top;
                keys.push(key);
            }
        }
    }
    // And a spread of arbitrary keys.
    let mut state = Sha256::digest(b"canonical y");
    for _ in 0..512 {
        state = Sha256::digest(state);
        keys.push(state.into());
    }

    for key in keys {
        assert_eq!(
            has_canonical_y(&key),
            !y_is_at_least_prime(&key),
            "{key:02x?}"
        );
    }
    // A key of the wrong length is never canonical.
    assert!(!has_canonical_y(&[]));
}

#[test]
fn install_accepts_real_ed25519_keys() {
    use curve25519_dalek::edwards::CompressedEdwardsY;

    for seed in 1..=64_u8 {
        let signing_key = ed25519_zebra::SigningKey::from([seed; 32]);
        let key: [u8; 32] = ed25519_zebra::VerificationKeyBytes::from(&signing_key).into();

        // A genuine key round-trips through the curve, so it is canonical and not small order.
        let point = CompressedEdwardsY(key).decompress().unwrap();
        assert!(!point.is_small_order());
        assert_eq!(point.compress().to_bytes(), key);

        assert!(single_key(key).validate_for_install().is_ok());
    }
}

#[test]
fn the_identity_key_verifies_a_signature_over_any_message_so_install_must_refuse_it() {
    // R = identity and S = 0 satisfy [S]B = R + [k]A for A = identity whatever k is, which is why
    // such a key must never be an attestor.
    let identity = SMALL_ORDER_ED25519_KEYS[2];
    let mut signature = [0; 64];
    signature[0] = 1;

    for message in [&b"first statement"[..], &b"another statement"[..], &[]] {
        assert!(MockApi::default()
            .ed25519_verify(message, &signature, &identity)
            .unwrap());
    }
    assert!(single_key(identity).validate_for_install().is_err());
}
