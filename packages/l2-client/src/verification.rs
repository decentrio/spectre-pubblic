//! Local validation shared by every L2 adapter.

use alloy_primitives::B256;
use cosmwasm_std::Api;

use crate::{
    error::Error,
    evm_proof::{verify_bounded_account, ProofLimits},
    msg::SignedAttestedL2Header,
    state::{ClientState, Header, Height, RuntimeProfile},
};

/// SHA-256(UTF8("SPECTRE_L2_ATTESTATION_V1")).
pub const ATTESTATION_PROTOCOL_DOMAIN: [u8; 32] = [
    0x7e, 0xe5, 0x5f, 0xf0, 0xa9, 0xe6, 0x4c, 0x45, 0x5d, 0x44, 0xd7, 0x7f, 0xa7, 0xee, 0x08, 0x39,
    0x45, 0xf8, 0x2a, 0x38, 0xed, 0x7f, 0xb6, 0x38, 0x51, 0x68, 0x54, 0x72, 0x6b, 0xd4, 0x40, 0xad,
];

/// Fixed byte length of the Ed25519 signing statement.
pub const ATTESTATION_STATEMENT_LENGTH: usize = 164;

/// Constructs the raw, fixed-width signing statement without hashing it again.
#[must_use]
pub fn build_attestation_statement(
    l2_chain_id: u64,
    l2_router: [u8; 20],
    attestor_set_hash: [u8; 32],
    l2_block_number: u64,
    l2_block_hash: [u8; 32],
    state_root: [u8; 32],
) -> [u8; ATTESTATION_STATEMENT_LENGTH] {
    let mut statement = [0_u8; ATTESTATION_STATEMENT_LENGTH];
    statement[0..32].copy_from_slice(&ATTESTATION_PROTOCOL_DOMAIN);
    statement[32..40].copy_from_slice(&l2_chain_id.to_be_bytes());
    statement[40..60].copy_from_slice(&l2_router);
    statement[60..92].copy_from_slice(&attestor_set_hash);
    statement[92..100].copy_from_slice(&l2_block_number.to_be_bytes());
    statement[100..132].copy_from_slice(&l2_block_hash);
    statement[132..164].copy_from_slice(&state_root);
    statement
}

/// Conservative bound for a router account proof.
pub const ROUTER_PROOF_LIMITS: ProofLimits = ProofLimits {
    max_account_nodes: 64,
    max_storage_nodes: 64,
    max_node_bytes: 32 * 1024,
};

/// A normalized header that crossed both authentication and proof verification.
///
/// The field and constructor are intentionally private to the common verifier;
/// runtime transitions cannot be invoked with a deserialized or hand-built header.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AuthenticatedHeader {
    header: Header,
}

impl AuthenticatedHeader {
    const fn new(header: Header) -> Self {
        Self { header }
    }

    pub(crate) const fn header(&self) -> &Header {
        &self.header
    }

    #[cfg(test)]
    pub(crate) const fn from_test_header(header: Header) -> Self {
        Self::new(header)
    }
}

struct PreparedAuthentication {
    block_hash: B256,
    statement: [u8; ATTESTATION_STATEMENT_LENGTH],
}

/// Authenticates one signed header, then verifies its router account proof.
pub fn verify_authenticated_header<P: RuntimeProfile>(
    api: &dyn Api,
    client: &ClientState<P>,
    signed_header: &SignedAttestedL2Header,
) -> Result<AuthenticatedHeader, Error> {
    let prepared = prepare_authentication(client, signed_header)?;
    verify_signatures(api, client, signed_header, &prepared)?;
    verify_router_proof(client, signed_header, prepared.block_hash)
}

/// Authenticates both certificates before traversing either router proof.
pub fn verify_authenticated_misbehaviour<P: RuntimeProfile>(
    api: &dyn Api,
    client: &ClientState<P>,
    first: &SignedAttestedL2Header,
    second: &SignedAttestedL2Header,
) -> Result<(AuthenticatedHeader, AuthenticatedHeader), Error> {
    let first_prepared = prepare_authentication(client, first)?;
    let second_prepared = prepare_authentication(client, second)?;
    verify_signatures(api, client, first, &first_prepared)?;
    verify_signatures(api, client, second, &second_prepared)?;
    Ok((
        verify_router_proof(client, first, first_prepared.block_hash)?,
        verify_router_proof(client, second, second_prepared.block_hash)?,
    ))
}

fn prepare_authentication<P: RuntimeProfile>(
    client: &ClientState<P>,
    signed_header: &SignedAttestedL2Header,
) -> Result<PreparedAuthentication, Error> {
    client.validate()?;
    let common = client.profile.common();
    signed_header
        .l2_header
        .validate_for_fork(common.l2_header_fork)?;
    Height::new(signed_header.l2_header.number())?;
    ROUTER_PROOF_LIMITS.validate_account_bounds(&signed_header.router_proof)?;

    if signed_header.attestor_signature.is_empty() {
        return Err(Error::UnsupportedUnsignedHeader);
    }
    if signed_header.attestor_signature.len() != usize::from(client.attestors.threshold) {
        return Err(Error::InvalidAttestorSignatureCount);
    }

    let mut previous_index = None;
    for indexed in &signed_header.attestor_signature {
        let index = usize::from(indexed.attestor_index);
        if index >= client.attestors.public_keys.len() {
            return Err(Error::InvalidAttestorIndex);
        }
        if previous_index.is_some_and(|previous| indexed.attestor_index <= previous) {
            return Err(Error::DuplicateOrUnsortedSignatureIndex);
        }
        if indexed.signature.len() != 64 {
            return Err(Error::InvalidAttestorSignatureLength);
        }
        previous_index = Some(indexed.attestor_index);
    }

    let block_hash = signed_header.l2_header.hash(common.l2_header_fork)?;
    let mut router = [0_u8; 20];
    router.copy_from_slice(common.l2_router.as_slice());
    let statement = build_attestation_statement(
        common.l2_chain_id,
        router,
        client.attestors.set_hash()?,
        signed_header.l2_header.number(),
        block_hash.into(),
        signed_header.l2_header.state_root().into(),
    );

    Ok(PreparedAuthentication {
        block_hash,
        statement,
    })
}

fn verify_signatures<P: RuntimeProfile>(
    api: &dyn Api,
    client: &ClientState<P>,
    signed_header: &SignedAttestedL2Header,
    prepared: &PreparedAuthentication,
) -> Result<(), Error> {
    for indexed in &signed_header.attestor_signature {
        let public_key = &client.attestors.public_keys[usize::from(indexed.attestor_index)];
        if !matches!(
            api.ed25519_verify(
                &prepared.statement,
                indexed.signature.as_slice(),
                public_key.as_slice()
            ),
            Ok(true)
        ) {
            return Err(Error::AttestorSignatureVerificationFailed);
        }
    }

    Ok(())
}

fn verify_router_proof<P: RuntimeProfile>(
    client: &ClientState<P>,
    signed_header: &SignedAttestedL2Header,
    block_hash: B256,
) -> Result<AuthenticatedHeader, Error> {
    let common = client.profile.common();
    let router = verify_bounded_account(
        ROUTER_PROOF_LIMITS,
        signed_header.l2_header.state_root(),
        common.l2_router,
        &signed_header.router_proof,
    )?;

    Ok(AuthenticatedHeader::new(Header {
        height: Height::new(signed_header.l2_header.number())?,
        state_root: signed_header.l2_header.state_root(),
        router_storage_root: B256::from(router.storage_root.0),
        timestamp_seconds: signed_header.l2_header.timestamp(),
        l2_block_hash: block_hash,
        parent_hash: signed_header.l2_header.parent_hash,
    }))
}
