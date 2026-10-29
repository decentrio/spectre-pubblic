//! Base authenticated ICS-08 Wasm client entrypoints.

#![allow(missing_docs)]

l2_client::l2_client_entrypoints!(l2_client::StaticL2Client<l2_op_stack::BaseProfile>);

#[cfg(test)]
mod tests {
    use cosmwasm_std::{
        testing::{message_info, mock_dependencies, mock_env},
        Binary, DepsMut, Env, Response,
    };
    use l2_client::{
        error::Error, msg::MigrateMsg, state::RuntimeProfile, L2LightClient, StaticL2Client,
    };
    use l2_op_stack::BaseProfile;

    #[test]
    fn composes_the_shared_kernel_with_the_base_profile() {
        fn assert_composition<Client: L2LightClient<Profile = BaseProfile>>() {}
        assert_composition::<StaticL2Client<BaseProfile>>();
        assert_eq!(BaseProfile::expected_profile_version(), "base_attestor_v1");
        let _: fn(DepsMut, Env, MigrateMsg) -> Result<Response, Error> = super::migrate;
    }

    #[test]
    fn rejects_a_profile_for_a_different_artifact() {
        let mut profile: serde_json::Value = serde_json::from_str(include_str!(
            "../../../packages/l2-op-stack/config/base-sepolia.json"
        ))
        .unwrap();
        profile["common"]["profile_version"] = "op_attestor_v1".into();
        let msg = l2_client::msg::InstantiateMsg {
            client_state: Binary::from(
                serde_json::to_vec(&serde_json::json!({
                    "latest_height": 1,
                    "frozen_height": null,
                    "profile": profile,
                    "attestors": {"public_keys": [Binary::from(vec![1; 32])], "threshold": 1},
                }))
                .unwrap(),
            ),
            consensus_state: Binary::from(valid_consensus_state()),
            checksum: Binary::default(),
        };
        let mut deps = mock_dependencies();
        let sender = deps.api.addr_make("host");

        assert!(matches!(
            super::instantiate(deps.as_mut(), mock_env(), message_info(&sender, &[]), msg),
            Err(l2_client::error::Error::InvalidProfileVersion)
        ));
    }

    fn valid_consensus_state() -> Vec<u8> {
        br#"{"state_root":"0x0000000000000000000000000000000000000000000000000000000000000001","ibc_storage_root":"0x0000000000000000000000000000000000000000000000000000000000000002","timestamp_nanos":1,"l2_height":1,"l2_block_hash":"0x0000000000000000000000000000000000000000000000000000000000000003","parent_hash":"0x0000000000000000000000000000000000000000000000000000000000000004","first_accepted_at":0}"#.to_vec()
    }
}
