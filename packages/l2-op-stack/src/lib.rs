//! Data-only OP Stack profiles for the shared authenticated L2 client.

#![deny(clippy::nursery, clippy::pedantic, warnings, unused_crate_dependencies)]

use l2_client::state::{CommonProfile, RuntimeProfile};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

/// Immutable Optimism artifact profile.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct OpProfile {
    /// Chain-independent L2 verification parameters.
    pub common: CommonProfile,
}

impl RuntimeProfile for OpProfile {
    fn common(&self) -> &CommonProfile {
        &self.common
    }

    fn expected_profile_version() -> &'static str {
        "op_attestor_v1"
    }
}

/// Immutable Base artifact profile.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct BaseProfile {
    /// Chain-independent L2 verification parameters.
    pub common: CommonProfile,
}

impl RuntimeProfile for BaseProfile {
    fn common(&self) -> &CommonProfile {
        &self.common
    }

    fn expected_profile_version() -> &'static str {
        "base_attestor_v1"
    }
}

#[cfg(test)]
mod tests {
    use super::{BaseProfile, OpProfile};

    const OP_PROFILE_BYTES: &[u8] = b"{\"common\":{\"l2_chain_id\":11155420,\"l2_router\":\"0x645280885749dc97ea461de280eb3273c91d36df\",\"commitment_slot\":\"0x1260944489272988d9df285149b5aa1b0f48f2136d6f416159f840a3e0747600\",\"profile_version\":\"op_attestor_v1\",\"l2_header_fork\":\"prague\"}}\n";
    const BASE_PROFILE_BYTES: &[u8] = b"{\"common\":{\"l2_chain_id\":84532,\"l2_router\":\"0x645280885749dc97ea461de280eb3273c91d36df\",\"commitment_slot\":\"0x1260944489272988d9df285149b5aa1b0f48f2136d6f416159f840a3e0747600\",\"profile_version\":\"base_attestor_v1\",\"l2_header_fork\":\"prague\"}}\n";

    #[test]
    fn op_profile_preserves_current_payload_bytes() {
        let bytes = include_bytes!("../config/op-sepolia.json");
        assert_eq!(bytes, OP_PROFILE_BYTES);
        let profile: OpProfile = serde_json::from_slice(bytes).unwrap();
        assert_eq!(profile.common.profile_version, "op_attestor_v1");
    }

    #[test]
    fn base_profile_preserves_current_payload_bytes() {
        let bytes = include_bytes!("../config/base-sepolia.json");
        assert_eq!(bytes, BASE_PROFILE_BYTES);
        let profile: BaseProfile = serde_json::from_slice(bytes).unwrap();
        assert_eq!(profile.common.profile_version, "base_attestor_v1");
    }
}
