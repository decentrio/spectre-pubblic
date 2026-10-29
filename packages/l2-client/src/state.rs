//! State models for authenticated L2 clients.

use alloy_primitives::{Address, B256};
use cosmwasm_std::Binary;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{canonical_header::ExecutionHeaderFork, error::Error};

/// Maximum active Ed25519 attestors for one client instance.
pub const MAX_ATTESTORS: usize = 32;
/// Raw Ed25519 public-key length.
pub const ED25519_PUBLIC_KEY_LENGTH: usize = 32;

/// Compressed Ed25519 encodings that decode to a point of order 1, 2, 4 or 8.
///
/// A key from this list satisfies the verification equation for signatures nobody made: the
/// identity, for one, "signs" every statement under ZIP-215. There are five such `y` values and
/// each is valid with either sign bit. A test derives this table from `curve25519-dalek`.
pub(crate) const SMALL_ORDER_ED25519_KEYS: [[u8; ED25519_PUBLIC_KEY_LENGTH]; 10] = [
    hex_key("0000000000000000000000000000000000000000000000000000000000000000"),
    hex_key("0000000000000000000000000000000000000000000000000000000000000080"),
    hex_key("0100000000000000000000000000000000000000000000000000000000000000"),
    hex_key("0100000000000000000000000000000000000000000000000000000000000080"),
    hex_key("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05"),
    hex_key("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85"),
    hex_key("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"),
    hex_key("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa"),
    hex_key("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"),
    hex_key("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
];

/// Decodes a 64-digit hex string at compile time.
const fn hex_key(hex: &str) -> [u8; ED25519_PUBLIC_KEY_LENGTH] {
    const fn digit(byte: u8) -> u8 {
        match byte {
            b'0'..=b'9' => byte - b'0',
            b'a'..=b'f' => byte - b'a' + 10,
            _ => panic!("not a lowercase hex digit"),
        }
    }

    let hex = hex.as_bytes();
    assert!(hex.len() == 2 * ED25519_PUBLIC_KEY_LENGTH);
    let mut key = [0; ED25519_PUBLIC_KEY_LENGTH];
    let mut index = 0;
    while index < ED25519_PUBLIC_KEY_LENGTH {
        key[index] = digit(hex[2 * index]) << 4 | digit(hex[2 * index + 1]);
        index += 1;
    }
    key
}

/// Whether the low 255 bits of the key, its `y` coordinate, are below the field prime `2^255 - 19`.
///
/// RFC 8032 requires it, but ZIP-215 verification, which `ed25519_verify` follows, also accepts
/// `y` in `[p, 2^255)`, a second encoding of the point with `y - p`.
pub(crate) fn has_canonical_y(key: &[u8]) -> bool {
    let Some((&top, rest)) = key.split_last() else {
        return false;
    };
    // `p` is 0x7fff...ffed, so y >= p exactly when the top byte is 0x7f (sign bit aside), every
    // byte below it is 0xff, and the lowest byte is at least 0xed.
    let is_all_ones = rest[1..].iter().all(|&byte| byte == 0xff);
    !(top & 0x7f == 0x7f && is_all_ones && rest[0] >= 0xed)
}

/// Active, canonically ordered Ed25519 attestor set.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct AttestorConfig {
    /// Raw 32-byte keys, strictly increasing in lexicographic byte order.
    pub public_keys: Vec<Binary>,
    /// Exact number of indexed signatures required on every update.
    pub threshold: u16,
}

impl AttestorConfig {
    /// Validates size, threshold, key length, ordering, and uniqueness.
    pub fn validate(&self) -> Result<(), Error> {
        if self.public_keys.is_empty() {
            return Err(Error::InvalidAttestorSet);
        }
        if self.public_keys.len() > MAX_ATTESTORS {
            return Err(Error::TooManyAttestors);
        }
        if self.threshold == 0 || usize::from(self.threshold) > self.public_keys.len() {
            return Err(Error::InvalidAttestorThreshold);
        }
        if self
            .public_keys
            .iter()
            .any(|key| key.len() != ED25519_PUBLIC_KEY_LENGTH)
        {
            return Err(Error::InvalidAttestorPublicKeyLength);
        }
        if self
            .public_keys
            .windows(2)
            .any(|pair| pair[0].as_slice() >= pair[1].as_slice())
        {
            return Err(Error::InvalidAttestorSet);
        }
        Ok(())
    }

    /// Validates a set that is about to be installed, at instantiation or on rotation.
    ///
    /// On top of [`Self::validate`], every key must be a canonical Ed25519 encoding of a point
    /// that does not have small order. Stricter rules are not part of [`Self::validate`], which
    /// runs on every call, because a client that already holds such a key must keep working
    /// until governance rotates the set.
    pub fn validate_for_install(&self) -> Result<(), Error> {
        self.validate()?;
        if self.public_keys.iter().any(|key| {
            !has_canonical_y(key)
                || SMALL_ORDER_ED25519_KEYS
                    .iter()
                    .any(|weak| weak == key.as_slice())
        }) {
            return Err(Error::InvalidAttestorPublicKey);
        }
        Ok(())
    }

    /// Derives SHA-256(u16be(threshold) || u16be(count) || sorted keys).
    pub fn set_hash(&self) -> Result<[u8; 32], Error> {
        self.validate()?;
        let count = u16::try_from(self.public_keys.len()).map_err(|_| Error::TooManyAttestors)?;
        let mut hasher = Sha256::new();
        hasher.update(self.threshold.to_be_bytes());
        hasher.update(count.to_be_bytes());
        for key in &self.public_keys {
            hasher.update(key.as_slice());
        }
        Ok(hasher.finalize().into())
    }
}

/// Immutable deployment data needed for local L2 proof verification.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct CommonProfile {
    /// L2 execution chain identifier.
    pub l2_chain_id: u64,
    /// L2 `ICS26Router` address.
    #[schemars(with = "String")]
    pub l2_router: Address,
    /// Solidity commitment mapping slot.
    #[schemars(with = "String")]
    pub commitment_slot: B256,
    /// Artifact-specific profile version.
    pub profile_version: String,
    /// Canonical execution-header fork.
    pub l2_header_fork: ExecutionHeaderFork,
}

/// Access to an artifact-specific runtime profile.
pub trait RuntimeProfile {
    /// Returns shared profile fields.
    fn common(&self) -> &CommonProfile;
    /// Returns the only profile version accepted by this artifact.
    fn expected_profile_version() -> &'static str;
}

/// Revision-zero IBC height.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct Height {
    /// Must be zero.
    pub revision_number: u64,
    /// Nonzero L2 block number.
    pub revision_height: u64,
}

impl Height {
    /// Builds a revision-zero height.
    pub const fn new(revision_height: u64) -> Result<Self, Error> {
        if revision_height == 0 {
            return Err(Error::ZeroHeight);
        }
        Ok(Self {
            revision_number: 0,
            revision_height,
        })
    }

    /// Validates the supported height space.
    pub const fn validate(self) -> Result<(), Error> {
        if self.revision_number != 0 {
            return Err(Error::InvalidRevision(self.revision_number));
        }
        if self.revision_height == 0 {
            return Err(Error::ZeroHeight);
        }
        Ok(())
    }
}

/// State retained for one attested L2 block.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct ConsensusState {
    #[schemars(with = "String")]
    pub state_root: B256,
    #[schemars(with = "String")]
    pub ibc_storage_root: B256,
    pub timestamp_nanos: u64,
    pub l2_height: u64,
    #[schemars(with = "String")]
    pub l2_block_hash: B256,
    #[schemars(with = "String")]
    pub parent_hash: B256,
    pub first_accepted_at: u64,
}

impl ConsensusState {
    /// Converts seconds to nanoseconds with overflow checking.
    pub fn timestamp_nanos_from_seconds(seconds: u64) -> Result<u64, Error> {
        seconds
            .checked_mul(1_000_000_000)
            .ok_or(Error::TimestampOverflow)
    }

    /// Validates bootstrap and stored-state invariants.
    pub fn validate(&self) -> Result<(), Error> {
        Height::new(self.l2_height)?;
        if self.state_root.is_zero()
            || self.ibc_storage_root.is_zero()
            || self.l2_block_hash.is_zero()
        {
            return Err(Error::InvalidHeader(
                "consensus roots and block hash must be non-zero",
            ));
        }
        Ok(())
    }

    /// Returns whether another state disagrees on block identity.
    #[must_use]
    pub fn conflicts_with(&self, other: &Self) -> bool {
        self.l2_block_hash != other.l2_block_hash
            || self.parent_hash != other.parent_hash
            || self.state_root != other.state_root
            || self.ibc_storage_root != other.ibc_storage_root
    }
}

/// Locally verified normalized update.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct Header {
    pub height: Height,
    #[schemars(with = "String")]
    pub state_root: B256,
    #[schemars(with = "String")]
    pub router_storage_root: B256,
    pub timestamp_seconds: u64,
    #[schemars(with = "String")]
    pub l2_block_hash: B256,
    #[schemars(with = "String")]
    pub parent_hash: B256,
}

impl Header {
    /// Converts the verified header into stored consensus state.
    pub fn consensus_state(&self, accepted_at: u64) -> Result<ConsensusState, Error> {
        self.height.validate()?;
        let state = ConsensusState {
            state_root: self.state_root,
            ibc_storage_root: self.router_storage_root,
            timestamp_nanos: ConsensusState::timestamp_nanos_from_seconds(self.timestamp_seconds)?,
            l2_height: self.height.revision_height,
            l2_block_hash: self.l2_block_hash,
            parent_hash: self.parent_hash,
            first_accepted_at: accepted_at,
        };
        state.validate()?;
        Ok(state)
    }

    /// Checks for a well-formed, same-height state conflict.
    pub fn conflicts_with(&self, other: &Self) -> Result<bool, Error> {
        Ok(self.height == other.height
            && self
                .consensus_state(0)?
                .conflicts_with(&other.consensus_state(0)?))
    }
}

/// Active L2 client state.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct ClientState<Profile> {
    pub latest_height: u64,
    pub frozen_height: Option<u64>,
    pub profile: Profile,
    pub attestors: AttestorConfig,
}

impl<Profile: RuntimeProfile> ClientState<Profile> {
    /// Validates bootstrap and loaded client/profile invariants.
    pub fn validate(&self) -> Result<(), Error> {
        Height::new(self.latest_height)?;
        self.attestors.validate()?;
        if self.frozen_height == Some(0) {
            return Err(Error::ZeroHeight);
        }
        let common = self.profile.common();
        if common.profile_version != Profile::expected_profile_version() {
            return Err(Error::InvalidProfileVersion);
        }
        if common.l2_chain_id == 0 {
            return Err(Error::InvalidHeader("L2 chain ID must be non-zero"));
        }
        if common.l2_router.is_zero() {
            return Err(Error::InvalidHeader("L2 router address must be non-zero"));
        }
        Ok(())
    }
}
