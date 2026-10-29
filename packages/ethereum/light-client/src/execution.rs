//! Execution block headers for Gloas-format light client data.
//!
//! The Gloas light client header no longer carries the execution payload header: it proves only
//! `execution_block_hash`. The execution state root, timestamp and block number that the IBC
//! light client stores in its consensus state therefore come from the RLP-encoded execution block
//! header supplied alongside the update, and are trusted only because
//! `keccak256(rlp) == execution_block_hash`, a hash the beacon block body proves.

use alloy_primitives::{keccak256, B256};
use alloy_rlp::{Decodable, Header as RlpHeader};

use crate::error::EthereumIBCError;

/// Upper bound on the RLP-encoded execution block header, enforced before hashing.
///
/// A post-Prague header is about 600 bytes, with `extra_data` capped at 32 bytes. The bound leaves
/// room for fields later forks add to the header.
pub const MAX_EXECUTION_HEADER_RLP_BYTES: usize = 2048;

/// Index of the `stateRoot` item in an execution block header.
const STATE_ROOT_INDEX: usize = 3;
/// Index of the `number` item in an execution block header.
const NUMBER_INDEX: usize = 8;
/// Index of the `timestamp` item in an execution block header.
const TIMESTAMP_INDEX: usize = 11;

/// The execution header fields the light client consumes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ExecutionBlockSummary {
    /// The execution state root.
    pub state_root: B256,
    /// The execution block timestamp in seconds.
    pub timestamp: u64,
    /// The execution block number.
    pub block_number: u64,
}

/// Decodes the summary out of an RLP-encoded execution block header and checks that the header
/// hashes to `expected_block_hash`.
///
/// Only the first twelve items are read: they are the same in every fork since genesis. Fields
/// appended by later forks need no handling here, because the hash check binds the whole encoding.
/// # Errors
/// Returns an error if the encoding is too large, malformed, or does not hash to
/// `expected_block_hash`.
pub fn verify_execution_header_rlp(
    rlp: &[u8],
    expected_block_hash: B256,
) -> Result<ExecutionBlockSummary, EthereumIBCError> {
    ensure!(
        rlp.len() <= MAX_EXECUTION_HEADER_RLP_BYTES,
        EthereumIBCError::ResourceLimit {
            resource: "execution header rlp",
            maximum: MAX_EXECUTION_HEADER_RLP_BYTES,
            found: rlp.len(),
        }
    );

    let found = keccak256(rlp);
    ensure!(
        found == expected_block_hash,
        EthereumIBCError::ExecutionHeaderHashMismatch {
            expected: expected_block_hash,
            found,
        }
    );

    decode_summary(rlp)
}

fn decode_summary(rlp: &[u8]) -> Result<ExecutionBlockSummary, EthereumIBCError> {
    let mut buf = rlp;
    let list = RlpHeader::decode(&mut buf)
        .map_err(|_| EthereumIBCError::InvalidExecutionHeaderRlp("malformed list header"))?;
    ensure!(
        list.list,
        EthereumIBCError::InvalidExecutionHeaderRlp("not a list")
    );
    ensure!(
        list.payload_length == buf.len(),
        EthereumIBCError::InvalidExecutionHeaderRlp("list length does not match the encoding")
    );

    let mut state_root = None;
    let mut block_number = None;
    let mut timestamp = None;

    for index in 0..=TIMESTAMP_INDEX {
        let item = next_item(&mut buf)?;
        match index {
            STATE_ROOT_INDEX => {
                state_root = Some(B256::decode(&mut &item[..]).map_err(|_| {
                    EthereumIBCError::InvalidExecutionHeaderRlp("invalid state root")
                })?);
            }
            NUMBER_INDEX => {
                block_number = Some(u64::decode(&mut &item[..]).map_err(|_| {
                    EthereumIBCError::InvalidExecutionHeaderRlp("invalid block number")
                })?);
            }
            TIMESTAMP_INDEX => {
                timestamp = Some(u64::decode(&mut &item[..]).map_err(|_| {
                    EthereumIBCError::InvalidExecutionHeaderRlp("invalid timestamp")
                })?);
            }
            _ => {}
        }
    }

    match (state_root, block_number, timestamp) {
        (Some(state_root), Some(block_number), Some(timestamp)) => Ok(ExecutionBlockSummary {
            state_root,
            timestamp,
            block_number,
        }),
        _ => Err(EthereumIBCError::InvalidExecutionHeaderRlp(
            "missing header items",
        )),
    }
}

/// Splits the next RLP string item (header included) off the front of `buf`.
fn next_item<'a>(buf: &mut &'a [u8]) -> Result<&'a [u8], EthereumIBCError> {
    let before: &'a [u8] = buf;
    let header = RlpHeader::decode(buf)
        .map_err(|_| EthereumIBCError::InvalidExecutionHeaderRlp("malformed item header"))?;
    ensure!(
        !header.list,
        EthereumIBCError::InvalidExecutionHeaderRlp("unexpected nested list")
    );
    ensure!(
        buf.len() >= header.payload_length,
        EthereumIBCError::InvalidExecutionHeaderRlp("truncated item")
    );
    let consumed = (before.len() - buf.len()) + header.payload_length;
    *buf = &buf[header.payload_length..];
    Ok(&before[..consumed])
}

/// Builds an RLP execution header for tests.
#[cfg(test)]
pub(crate) mod test_support {
    use alloy_primitives::{Address, Bloom, Bytes, B256, U256};
    use alloy_rlp::{Encodable, Header as RlpHeader};

    /// Encodes a header with the Cancun field set, which is a strict prefix of every later one,
    /// followed by `extra_fields` additional 32-byte items.
    pub fn encode_header(
        state_root: B256,
        number: u64,
        timestamp: u64,
        extra_fields: usize,
    ) -> Vec<u8> {
        let mut payload = Vec::new();
        B256::repeat_byte(1).encode(&mut payload); // parentHash
        B256::repeat_byte(2).encode(&mut payload); // ommersHash
        Address::repeat_byte(3).encode(&mut payload); // coinbase
        state_root.encode(&mut payload); // stateRoot
        B256::repeat_byte(4).encode(&mut payload); // transactionsRoot
        B256::repeat_byte(5).encode(&mut payload); // receiptsRoot
        Bloom::ZERO.encode(&mut payload); // logsBloom
        U256::ZERO.encode(&mut payload); // difficulty
        number.encode(&mut payload); // number
        30_000_000_u64.encode(&mut payload); // gasLimit
        21_000_u64.encode(&mut payload); // gasUsed
        timestamp.encode(&mut payload); // timestamp
        Bytes::from_static(b"spectre").encode(&mut payload); // extraData
        B256::repeat_byte(6).encode(&mut payload); // mixHash
        [0u8; 8].encode(&mut payload); // nonce
        7_u64.encode(&mut payload); // baseFeePerGas
        for i in 0..extra_fields {
            B256::repeat_byte(u8::try_from(10 + i).unwrap()).encode(&mut payload);
        }

        let mut out = Vec::new();
        RlpHeader {
            list: true,
            payload_length: payload.len(),
        }
        .encode(&mut out);
        out.extend_from_slice(&payload);
        out
    }
}

#[cfg(test)]
mod test {
    use alloy_primitives::{keccak256, B256};
    use alloy_rlp::{Encodable, Header as RlpHeader};

    use super::{test_support::encode_header, *};

    #[test]
    fn decodes_summary_and_binds_hash() {
        let state_root = B256::repeat_byte(0xaa);
        let rlp = encode_header(state_root, 1_234_567, 1_790_000_000, 0);
        let summary = verify_execution_header_rlp(&rlp, keccak256(&rlp)).unwrap();
        assert_eq!(
            summary,
            ExecutionBlockSummary {
                state_root,
                timestamp: 1_790_000_000,
                block_number: 1_234_567,
            }
        );
    }

    /// A Prague header encoded by go-ethereum (`types.Header`, see
    /// `relayer/client/execution_header_test.go`), so the Rust decoder is checked against an
    /// encoder written by someone else, not just against this crate's test encoder.
    #[test]
    fn decodes_a_header_encoded_by_go_ethereum() {
        let rlp = hex::decode("f90267a00000000000000000000000000000000000000000000000000000000000000002a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347940000000000000000000000000000000000000003a00000000000000000000000000000000000000000000000000000000000aabbcca056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421b9010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000808312d6878401c9c380825208846ab13b808773706563747265a0000000000000000000000000000000000000000000000000000000000000000088000000000000000007a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b4218080a00000000000000000000000000000000000000000000000000000000000000001a0e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855").unwrap();
        let block_hash = B256::from_slice(
            &hex::decode("3625bad2dba02404047a4b10e276786b5c0a1db5b3e55210816c1864317a31ba")
                .unwrap(),
        );
        let summary = verify_execution_header_rlp(&rlp, block_hash).unwrap();
        assert_eq!(
            summary,
            ExecutionBlockSummary {
                state_root: B256::from_slice(
                    &hex::decode(
                        "0000000000000000000000000000000000000000000000000000000000aabbcc"
                    )
                    .unwrap()
                ),
                timestamp: 1_790_000_000,
                block_number: 1_234_567,
            }
        );
    }

    #[test]
    fn accepts_fields_appended_by_later_forks() {
        let rlp = encode_header(B256::repeat_byte(0xbb), 9, 10, 4);
        let summary = verify_execution_header_rlp(&rlp, keccak256(&rlp)).unwrap();
        assert_eq!(summary.block_number, 9);
    }

    #[test]
    fn rejects_hash_mismatch() {
        let rlp = encode_header(B256::repeat_byte(0xaa), 1, 2, 0);
        let wrong = B256::repeat_byte(0xff);
        assert_eq!(
            verify_execution_header_rlp(&rlp, wrong),
            Err(EthereumIBCError::ExecutionHeaderHashMismatch {
                expected: wrong,
                found: keccak256(&rlp),
            })
        );
    }

    #[test]
    fn a_tampered_state_root_changes_the_hash() {
        let honest = encode_header(B256::repeat_byte(0xaa), 1, 2, 0);
        let forged = encode_header(B256::repeat_byte(0xab), 1, 2, 0);
        // A relayer cannot swap in a different state root under the proven block hash.
        assert!(matches!(
            verify_execution_header_rlp(&forged, keccak256(&honest)),
            Err(EthereumIBCError::ExecutionHeaderHashMismatch { .. })
        ));
    }

    #[test]
    fn rejects_oversized_input_before_hashing() {
        let rlp = vec![0u8; MAX_EXECUTION_HEADER_RLP_BYTES + 1];
        assert!(matches!(
            verify_execution_header_rlp(&rlp, B256::ZERO),
            Err(EthereumIBCError::ResourceLimit { .. })
        ));
    }

    #[test]
    fn rejects_malformed_encodings_even_with_a_matching_hash() {
        // not a list
        let not_list = vec![0x83, 1, 2, 3];
        assert!(matches!(
            verify_execution_header_rlp(&not_list, keccak256(&not_list)),
            Err(EthereumIBCError::InvalidExecutionHeaderRlp(_))
        ));

        // list with too few items
        let mut short = Vec::new();
        B256::ZERO.encode(&mut short);
        let mut rlp = Vec::new();
        RlpHeader {
            list: true,
            payload_length: short.len(),
        }
        .encode(&mut rlp);
        rlp.extend_from_slice(&short);
        assert!(matches!(
            verify_execution_header_rlp(&rlp, keccak256(&rlp)),
            Err(EthereumIBCError::InvalidExecutionHeaderRlp(_))
        ));

        // trailing bytes after the list
        let mut trailing = encode_header(B256::repeat_byte(1), 1, 2, 0);
        trailing.push(0x00);
        assert!(matches!(
            verify_execution_header_rlp(&trailing, keccak256(&trailing)),
            Err(EthereumIBCError::InvalidExecutionHeaderRlp(_))
        ));
    }
}
