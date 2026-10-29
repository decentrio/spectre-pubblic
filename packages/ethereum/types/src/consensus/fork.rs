//! This module defines types related to forks in Ethereum.

use alloy_primitives::{aliases::B32, B256};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use tree_hash::TreeHash;
use tree_hash_derive::TreeHash;

/// Type alias Etheruem Version which is a fixed 4 byte array
pub type Version = B32;

/// The fork data
#[derive(Serialize, Deserialize, JsonSchema, PartialEq, Eq, Clone, Debug, Default)]
pub struct Fork {
    /// The version of the fork
    #[schemars(with = "String")]
    pub version: Version,
    /// The epoch at which this fork is activated
    pub epoch: u64,
}

/// The fork data
#[derive(Serialize, Deserialize, PartialEq, Clone, Debug, Default, TreeHash)]
struct ForkData {
    /// The current version
    pub current_version: Version,
    /// The genesis validators root
    pub genesis_validators_root: B256,
}

/// The fork parameters
#[derive(Serialize, Deserialize, JsonSchema, PartialEq, Eq, Clone, Debug)]
#[allow(clippy::module_name_repetitions)]
pub struct ForkParameters {
    /// The genesis fork version
    #[schemars(with = "String")]
    pub genesis_fork_version: Version,
    /// The genesis slot
    pub genesis_slot: u64,
    /// The altair fork
    pub altair: Fork,
    /// The bellatrix fork
    pub bellatrix: Fork,
    /// The capella fork
    pub capella: Fork,
    /// The deneb fork
    pub deneb: Fork,
    /// The electra fork
    pub electra: Fork,
    /// The fulu (Fusaka) fork. On a chain that has not scheduled Fulu, `epoch`
    /// must be `u64::MAX` so `compute_fork_version` never selects it. Absent input
    /// (pre-Fulu client states / fixtures) deserializes to that "not scheduled"
    /// value, so an Electra-only chain behaves exactly as before.
    #[serde(default = "fork_not_scheduled")]
    pub fulu: Fork,
    /// The gloas (Glamsterdam) fork. Same convention as `fulu`: `epoch` is `u64::MAX` on a
    /// chain that has not scheduled Gloas, and absent input deserializes to that value.
    #[serde(default = "fork_not_scheduled")]
    pub gloas: Fork,
}

/// The default `fulu`/`gloas` fork for inputs that predate that fork's support: a zero version
/// pinned to `u64::MAX` so `compute_fork_version` never selects it.
const fn fork_not_scheduled() -> Fork {
    Fork {
        version: Version::ZERO,
        epoch: u64::MAX,
    }
}

// Hand-written so `fulu` and `gloas` default to the not-scheduled sentinel (`u64::MAX`) rather
// than `Fork::default()`'s epoch 0. A derived Default would make
// `ForkParameters::default().compute_fork_version(_)` return the zero Gloas version
// for every epoch, since the newest arm is checked first.
impl Default for ForkParameters {
    fn default() -> Self {
        Self {
            genesis_fork_version: Version::default(),
            genesis_slot: 0,
            altair: Fork::default(),
            bellatrix: Fork::default(),
            capella: Fork::default(),
            deneb: Fork::default(),
            electra: Fork::default(),
            fulu: fork_not_scheduled(),
            gloas: fork_not_scheduled(),
        }
    }
}

impl ForkParameters {
    /// Returns the fork version based on the `epoch`.
    /// [See in consensus-spec](https://github.com/ethereum/consensus-specs/blob/dev/specs/capella/fork.md#modified-compute_fork_version)
    #[must_use]
    pub const fn compute_fork_version(&self, epoch: u64) -> Version {
        match epoch {
            _ if epoch >= self.gloas.epoch => self.gloas.version,
            _ if epoch >= self.fulu.epoch => self.fulu.version,
            _ if epoch >= self.electra.epoch => self.electra.version,
            _ if epoch >= self.deneb.epoch => self.deneb.version,
            _ if epoch >= self.capella.epoch => self.capella.version,
            _ if epoch >= self.bellatrix.epoch => self.bellatrix.version,
            _ if epoch >= self.altair.epoch => self.altair.version,
            _ => self.genesis_fork_version,
        }
    }
}

/// Return the 32-byte fork data root for the `current_version` and `genesis_validators_root`.
/// This is used primarily in signature domains to avoid collisions across forks/chains.
///
/// [See in consensus-spec](https://github.com/ethereum/consensus-specs/blob/dev/specs/phase0/beacon-chain.md#compute_fork_data_root)
#[must_use]
pub fn compute_fork_data_root(current_version: Version, genesis_validators_root: B256) -> B256 {
    let fork_data = ForkData {
        current_version,
        genesis_validators_root,
    };

    fork_data.tree_hash_root()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn v(n: u8) -> Version {
        Version::from([n, 0, 0, 0])
    }

    fn params(electra_epoch: u64, fulu_epoch: u64) -> ForkParameters {
        params_with_gloas(electra_epoch, fulu_epoch, u64::MAX)
    }

    fn params_with_gloas(electra_epoch: u64, fulu_epoch: u64, gloas_epoch: u64) -> ForkParameters {
        ForkParameters {
            genesis_fork_version: v(0),
            genesis_slot: 0,
            altair: Fork {
                version: v(1),
                epoch: 0,
            },
            bellatrix: Fork {
                version: v(2),
                epoch: 0,
            },
            capella: Fork {
                version: v(3),
                epoch: 0,
            },
            deneb: Fork {
                version: v(4),
                epoch: 0,
            },
            electra: Fork {
                version: v(5),
                epoch: electra_epoch,
            },
            fulu: Fork {
                version: v(6),
                epoch: fulu_epoch,
            },
            gloas: Fork {
                version: v(7),
                epoch: gloas_epoch,
            },
        }
    }

    #[test]
    fn compute_fork_version_selects_fulu_at_and_past_its_epoch() {
        let p = params(100, 200);
        assert_eq!(
            p.compute_fork_version(150),
            v(5),
            "between electra and fulu -> electra"
        );
        assert_eq!(p.compute_fork_version(200), v(6), "at fulu -> fulu");
        assert_eq!(p.compute_fork_version(999), v(6), "past fulu -> fulu");
    }

    #[test]
    fn compute_fork_version_ignores_unscheduled_fulu() {
        // fulu.epoch = u64::MAX means "not scheduled": electra stays active forever.
        let p = params(100, u64::MAX);
        assert_eq!(p.compute_fork_version(1_000_000), v(5));
    }

    #[test]
    fn deserializes_without_fulu_to_never() {
        // A pre-Fulu client state omits "fulu"; it must default to epoch u64::MAX so
        // compute_fork_version never selects it.
        let json = r#"{
            "genesis_fork_version": "0x00000000",
            "genesis_slot": 0,
            "altair": {"version":"0x01000000","epoch":0},
            "bellatrix": {"version":"0x02000000","epoch":0},
            "capella": {"version":"0x03000000","epoch":0},
            "deneb": {"version":"0x04000000","epoch":0},
            "electra": {"version":"0x05000000","epoch":100}
        }"#;
        let p: ForkParameters = serde_json::from_str(json).unwrap();
        assert_eq!(p.fulu.epoch, u64::MAX);
        assert_eq!(p.compute_fork_version(1_000_000), p.electra.version);
    }

    #[test]
    fn compute_fork_version_selects_gloas_at_and_past_its_epoch() {
        let p = params_with_gloas(100, 200, 300);
        assert_eq!(
            p.compute_fork_version(250),
            v(6),
            "between fulu and gloas -> fulu"
        );
        assert_eq!(
            p.compute_fork_version(299),
            v(6),
            "just before gloas -> fulu"
        );
        assert_eq!(p.compute_fork_version(300), v(7), "at gloas -> gloas");
        assert_eq!(p.compute_fork_version(999), v(7), "past gloas -> gloas");
    }

    #[test]
    fn compute_fork_version_ignores_unscheduled_gloas() {
        let p = params_with_gloas(100, 200, u64::MAX);
        assert_eq!(p.compute_fork_version(1_000_000), v(6));
    }

    #[test]
    fn deserializes_without_gloas_to_never() {
        // A pre-Gloas client state omits "gloas"; it must default to epoch u64::MAX so
        // compute_fork_version never selects it.
        let json = r#"{
            "genesis_fork_version": "0x00000000",
            "genesis_slot": 0,
            "altair": {"version":"0x01000000","epoch":0},
            "bellatrix": {"version":"0x02000000","epoch":0},
            "capella": {"version":"0x03000000","epoch":0},
            "deneb": {"version":"0x04000000","epoch":0},
            "electra": {"version":"0x05000000","epoch":100},
            "fulu": {"version":"0x06000000","epoch":200}
        }"#;
        let p: ForkParameters = serde_json::from_str(json).unwrap();
        assert_eq!(p.gloas.epoch, u64::MAX);
        assert_eq!(p.compute_fork_version(1_000_000), p.fulu.version);
    }
}
