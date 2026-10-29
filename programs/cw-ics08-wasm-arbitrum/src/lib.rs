//! Arbitrum ICS-08 Wasm client entrypoint crate.

#![deny(
    clippy::nursery,
    clippy::pedantic,
    warnings,
    missing_docs,
    unused_crate_dependencies
)]

use cosmwasm_std as _;
use l2_arbitrum as _;
use l2_client as _;

pub mod contract;
pub mod msg;
