//! Solidity types for `solidity-ibc-eureka`

#![deny(clippy::nursery, clippy::pedantic, warnings, unused_crate_dependencies)]

pub mod groth16_ics07;
pub mod ics26;
pub mod msgs;

#[derive(thiserror::Error, Debug)]
#[allow(missing_docs)]
pub enum FromStrError {
    #[error("unsupported zk algorithm: {0}")]
    UnsupportedZkAlgorithm(String),
}
