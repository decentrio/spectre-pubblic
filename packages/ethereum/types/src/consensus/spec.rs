//! This module defines types related to Spec.

use serde::{Deserialize, Serialize};
use serde_with::{serde_as, DisplayFromStr};

use super::fork::{Fork, ForkParameters, Version};

/// The spec type, returned from the beacon api.
#[serde_as]
#[derive(Serialize, Deserialize, PartialEq, Eq, Clone, Debug)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub struct Spec {
    /// The number of seconds per slot.
    #[serde_as(as = "DisplayFromStr")]
    pub seconds_per_slot: u64,
    /// The number of slots per epoch.
    #[serde_as(as = "DisplayFromStr")]
    pub slots_per_epoch: u64,
    /// The number of epochs per sync committee period.
    #[serde_as(as = "DisplayFromStr")]
    pub epochs_per_sync_committee_period: u64,

    /// The size of the sync committee.
    #[serde_as(as = "DisplayFromStr")]
    pub sync_committee_size: u64,

    // Fork Parameters
    /// The genesis fork version.
    pub genesis_fork_version: Version,
    /// The genesis slot.
    #[serde_as(as = "DisplayFromStr")]
    pub genesis_slot: u64,
    /// The altair fork version.
    pub altair_fork_version: Version,
    /// The altair fork epoch.
    #[serde_as(as = "DisplayFromStr")]
    pub altair_fork_epoch: u64,
    /// The bellatrix fork version.
    pub bellatrix_fork_version: Version,
    /// The bellatrix fork epoch.
    #[serde_as(as = "DisplayFromStr")]
    pub bellatrix_fork_epoch: u64,
    /// The capella fork version.
    pub capella_fork_version: Version,
    /// The capella fork epoch.
    #[serde_as(as = "DisplayFromStr")]
    pub capella_fork_epoch: u64,
    /// The deneb fork version.
    pub deneb_fork_version: Version,
    /// The deneb fork epoch.
    #[serde_as(as = "DisplayFromStr")]
    pub deneb_fork_epoch: u64,
    /// The electra fork version.
    pub electra_fork_version: Version,
    /// The electra fork epoch.
    #[serde_as(as = "DisplayFromStr")]
    pub electra_fork_epoch: u64,
    /// The fulu (Fusaka) fork version.
    #[serde(default)]
    pub fulu_fork_version: Version,
    /// The fulu (Fusaka) fork epoch. `u64::MAX` when Fulu is not scheduled; a beacon
    /// spec that omits `FULU_FORK_EPOCH` (pre-Fulu chain) defaults to that value.
    #[serde_as(as = "DisplayFromStr")]
    #[serde(default = "fork_epoch_not_scheduled")]
    pub fulu_fork_epoch: u64,
    /// The gloas (Glamsterdam) fork version.
    #[serde(default)]
    pub gloas_fork_version: Version,
    /// The gloas (Glamsterdam) fork epoch. `u64::MAX` when Gloas is not scheduled; a beacon
    /// spec that omits `GLOAS_FORK_EPOCH` (pre-Gloas chain) defaults to that value.
    #[serde_as(as = "DisplayFromStr")]
    #[serde(default = "fork_epoch_not_scheduled")]
    pub gloas_fork_epoch: u64,
}

/// The `*_FORK_EPOCH` sentinel used when a chain has not scheduled a fork.
const fn fork_epoch_not_scheduled() -> u64 {
    u64::MAX
}

// Hand-written so `fulu_fork_epoch` and `gloas_fork_epoch` default to the not-scheduled
// sentinel (`u64::MAX`) rather than 0, keeping `Spec::default().to_fork_parameters()` from
// selecting the zero Gloas version for every epoch.
impl Default for Spec {
    fn default() -> Self {
        Self {
            seconds_per_slot: 0,
            slots_per_epoch: 0,
            epochs_per_sync_committee_period: 0,
            sync_committee_size: 0,
            genesis_fork_version: Version::default(),
            genesis_slot: 0,
            altair_fork_version: Version::default(),
            altair_fork_epoch: 0,
            bellatrix_fork_version: Version::default(),
            bellatrix_fork_epoch: 0,
            capella_fork_version: Version::default(),
            capella_fork_epoch: 0,
            deneb_fork_version: Version::default(),
            deneb_fork_epoch: 0,
            electra_fork_version: Version::default(),
            electra_fork_epoch: 0,
            fulu_fork_version: Version::default(),
            fulu_fork_epoch: fork_epoch_not_scheduled(),
            gloas_fork_version: Version::default(),
            gloas_fork_epoch: fork_epoch_not_scheduled(),
        }
    }
}

impl Spec {
    /// Returns the number of slots in a sync committee period.
    #[must_use]
    pub const fn period(&self) -> u64 {
        self.epochs_per_sync_committee_period * self.slots_per_epoch
    }

    /// Returns [`ForkParameters`] based on the spec.
    #[must_use]
    pub const fn to_fork_parameters(&self) -> ForkParameters {
        ForkParameters {
            genesis_fork_version: self.genesis_fork_version,
            genesis_slot: self.genesis_slot,
            altair: Fork {
                version: self.altair_fork_version,
                epoch: self.altair_fork_epoch,
            },
            bellatrix: Fork {
                version: self.bellatrix_fork_version,
                epoch: self.bellatrix_fork_epoch,
            },
            capella: Fork {
                version: self.capella_fork_version,
                epoch: self.capella_fork_epoch,
            },
            deneb: Fork {
                version: self.deneb_fork_version,
                epoch: self.deneb_fork_epoch,
            },
            electra: Fork {
                version: self.electra_fork_version,
                epoch: self.electra_fork_epoch,
            },
            fulu: Fork {
                version: self.fulu_fork_version,
                epoch: self.fulu_fork_epoch,
            },
            gloas: Fork {
                version: self.gloas_fork_version,
                epoch: self.gloas_fork_epoch,
            },
        }
    }
}
