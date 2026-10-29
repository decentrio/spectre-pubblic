//! Data-only Arbitrum profile for the shared authenticated L2 client.

#![deny(clippy::nursery, clippy::pedantic, warnings, unused_crate_dependencies)]

use l2_client::state::{CommonProfile, RuntimeProfile};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

/// Immutable Arbitrum artifact profile.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct ArbitrumProfile {
    /// Chain-independent L2 verification parameters.
    pub common: CommonProfile,
}

impl RuntimeProfile for ArbitrumProfile {
    fn common(&self) -> &CommonProfile {
        &self.common
    }

    fn expected_profile_version() -> &'static str {
        "arbitrum_attestor_v1"
    }
}

#[cfg(test)]
mod tests {
    use super::ArbitrumProfile;

    const PROFILE_BYTES: &[u8] = b"{\"common\":{\"l2_chain_id\":421614,\"l2_router\":\"0x645280885749dc97ea461de280eb3273c91d36df\",\"commitment_slot\":\"0x1260944489272988d9df285149b5aa1b0f48f2136d6f416159f840a3e0747600\",\"profile_version\":\"arbitrum_attestor_v1\",\"l2_header_fork\":\"london\"}}\n";

    #[test]
    fn profile_preserves_current_payload_bytes() {
        let bytes = include_bytes!("../config/arbitrum-sepolia.json");
        assert_eq!(bytes, PROFILE_BYTES);
        let profile: ArbitrumProfile = serde_json::from_slice(bytes).unwrap();
        assert_eq!(profile.common.profile_version, "arbitrum_attestor_v1");
    }
}
