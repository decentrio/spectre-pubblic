//! Strict canonical EVM execution-header encoding for pinned L2 fork configurations.

use alloy_primitives::{keccak256, Address, Bloom, Bytes, FixedBytes, B256, U256};
use alloy_rlp::Header as RlpHeader;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::error::Error;

/// Fork-specific field set accepted for a canonical execution header.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum ExecutionHeaderFork {
    /// EIP-1559 fields through the London fork.
    London,
    /// Adds the withdrawals root.
    Shanghai,
    /// Adds EIP-4844 blob fields and the parent beacon block root.
    Cancun,
    /// Adds the requests hash.
    Prague,
}

/// Complete EVM execution header used to bind rollup commitments to an L2 state root.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct CanonicalEvmHeader {
    /// Parent block hash.
    #[schemars(with = "String")]
    pub parent_hash: B256,
    /// Ommer-list hash.
    #[schemars(with = "String")]
    pub ommers_hash: B256,
    /// Block beneficiary.
    #[schemars(with = "String")]
    pub beneficiary: Address,
    /// L2 execution state root.
    #[schemars(with = "String")]
    pub state_root: B256,
    /// Transaction trie root.
    #[schemars(with = "String")]
    pub transactions_root: B256,
    /// Receipt trie root.
    #[schemars(with = "String")]
    pub receipts_root: B256,
    /// 256-byte log bloom.
    #[schemars(with = "String")]
    pub logs_bloom: Bloom,
    /// PoW difficulty retained in post-merge headers for canonical compatibility.
    #[schemars(with = "String")]
    pub difficulty: U256,
    /// L2 block number.
    pub number: u64,
    /// Block gas limit.
    pub gas_limit: u64,
    /// Gas used by the block.
    pub gas_used: u64,
    /// Unix timestamp in seconds.
    pub timestamp: u64,
    /// Opaque extra data, bounded to the Ethereum protocol maximum.
    #[schemars(with = "String")]
    pub extra_data: Bytes,
    /// Mix hash / post-merge prev-randao field.
    #[schemars(with = "String")]
    pub mix_hash: B256,
    /// Eight-byte nonce.
    #[schemars(with = "String")]
    pub nonce: FixedBytes<8>,
    /// EIP-1559 base fee.
    #[schemars(with = "Option<String>")]
    pub base_fee_per_gas: Option<U256>,
    /// Shanghai withdrawals root.
    #[schemars(with = "Option<String>")]
    pub withdrawals_root: Option<B256>,
    /// Cancun blob gas used.
    pub blob_gas_used: Option<u64>,
    /// Cancun excess blob gas.
    pub excess_blob_gas: Option<u64>,
    /// Cancun parent beacon block root.
    #[schemars(with = "Option<String>")]
    pub parent_beacon_block_root: Option<B256>,
    /// Prague requests hash.
    #[schemars(with = "Option<String>")]
    pub requests_hash: Option<B256>,
}

impl CanonicalEvmHeader {
    /// Ethereum's consensus maximum extra-data size.
    pub const MAX_EXTRA_DATA_BYTES: usize = 32;

    /// Returns the checked block number.
    pub const fn number(&self) -> u64 {
        self.number
    }

    /// Returns the checked Unix timestamp in seconds.
    pub const fn timestamp(&self) -> u64 {
        self.timestamp
    }

    /// Returns the execution state root committed by this header.
    pub const fn state_root(&self) -> B256 {
        self.state_root
    }

    /// Validates the fork-specific optional-field combination and fixed protocol limits.
    pub fn validate_for_fork(&self, fork: ExecutionHeaderFork) -> Result<(), Error> {
        if self.extra_data.len() > Self::MAX_EXTRA_DATA_BYTES {
            return Err(Error::InvalidEvmHeader("extra_data exceeds 32 bytes"));
        }
        if self.gas_used > self.gas_limit {
            return Err(Error::InvalidEvmHeader("gas_used exceeds gas_limit"));
        }

        let london = self.base_fee_per_gas.is_some();
        let shanghai = self.withdrawals_root.is_some();
        let cancun = self.blob_gas_used.is_some()
            && self.excess_blob_gas.is_some()
            && self.parent_beacon_block_root.is_some();
        let partial_cancun = self.blob_gas_used.is_some()
            || self.excess_blob_gas.is_some()
            || self.parent_beacon_block_root.is_some();

        let valid = match fork {
            ExecutionHeaderFork::London => {
                london && !shanghai && !partial_cancun && self.requests_hash.is_none()
            }
            ExecutionHeaderFork::Shanghai => {
                london && shanghai && !partial_cancun && self.requests_hash.is_none()
            }
            ExecutionHeaderFork::Cancun => {
                london && shanghai && cancun && self.requests_hash.is_none()
            }
            ExecutionHeaderFork::Prague => {
                london && shanghai && cancun && self.requests_hash.is_some()
            }
        };

        if !valid {
            return Err(Error::InvalidEvmHeader(
                "optional fields do not match the pinned fork",
            ));
        }
        Ok(())
    }

    /// RLP-encodes this header in the field order selected by the pinned fork.
    pub fn rlp_bytes(&self, fork: ExecutionHeaderFork) -> Result<Vec<u8>, Error> {
        self.validate_for_fork(fork)?;
        let mut fields = vec![
            alloy_rlp::encode(self.parent_hash),
            alloy_rlp::encode(self.ommers_hash),
            alloy_rlp::encode(self.beneficiary),
            alloy_rlp::encode(self.state_root),
            alloy_rlp::encode(self.transactions_root),
            alloy_rlp::encode(self.receipts_root),
            alloy_rlp::encode(self.logs_bloom),
            alloy_rlp::encode(self.difficulty),
            alloy_rlp::encode(self.number),
            alloy_rlp::encode(self.gas_limit),
            alloy_rlp::encode(self.gas_used),
            alloy_rlp::encode(self.timestamp),
            alloy_rlp::encode(self.extra_data.clone()),
            alloy_rlp::encode(self.mix_hash),
            alloy_rlp::encode(self.nonce),
            alloy_rlp::encode(
                self.base_fee_per_gas
                    .ok_or(Error::InvalidEvmHeader("missing base fee"))?,
            ),
        ];

        if matches!(
            fork,
            ExecutionHeaderFork::Shanghai
                | ExecutionHeaderFork::Cancun
                | ExecutionHeaderFork::Prague
        ) {
            fields.push(alloy_rlp::encode(
                self.withdrawals_root
                    .ok_or(Error::InvalidEvmHeader("missing withdrawals root"))?,
            ));
        }
        if matches!(
            fork,
            ExecutionHeaderFork::Cancun | ExecutionHeaderFork::Prague
        ) {
            fields.push(alloy_rlp::encode(
                self.blob_gas_used
                    .ok_or(Error::InvalidEvmHeader("missing blob gas used"))?,
            ));
            fields.push(alloy_rlp::encode(
                self.excess_blob_gas
                    .ok_or(Error::InvalidEvmHeader("missing excess blob gas"))?,
            ));
            fields
                .push(alloy_rlp::encode(self.parent_beacon_block_root.ok_or(
                    Error::InvalidEvmHeader("missing parent beacon root"),
                )?));
        }
        if matches!(fork, ExecutionHeaderFork::Prague) {
            fields.push(alloy_rlp::encode(
                self.requests_hash
                    .ok_or(Error::InvalidEvmHeader("missing requests hash"))?,
            ));
        }

        let payload_length = fields.iter().map(Vec::len).sum();
        let mut output = Vec::with_capacity(payload_length + 9);
        RlpHeader {
            list: true,
            payload_length,
        }
        .encode(&mut output);
        for field in fields {
            output.extend(field);
        }
        Ok(output)
    }

    /// Returns `keccak256(rlp(header))` for the pinned fork configuration.
    pub fn hash(&self, fork: ExecutionHeaderFork) -> Result<B256, Error> {
        Ok(keccak256(self.rlp_bytes(fork)?))
    }
}
