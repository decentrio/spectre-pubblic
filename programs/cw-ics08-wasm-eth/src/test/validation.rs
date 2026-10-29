//! What a bootstrap or a fork parameter migration may put into a client, and that a rejected one
//! writes nothing.

use cosmwasm_std::{
    coins,
    testing::{message_info, mock_env, MockApi, MockQuerier, MockStorage},
    OwnedDeps, Storage,
};
use ethereum_light_client::{
    client_state::ClientState, consensus_state::ConsensusState,
    test_utils::synthetic::SyntheticChain,
};
use ethereum_types::consensus::fork::{Fork, ForkParameters, Version};

use crate::{
    contract::{instantiate, migrate},
    custom_query::EthereumCustomQuery,
    msg::{InstantiateMsg, MigrateMsg, Migration},
    state::HOST_CLIENT_STATE_KEY,
    test::helpers::mk_deps,
    ContractError,
};

type Deps = OwnedDeps<MockStorage, MockApi, MockQuerier<EthereumCustomQuery>, EthereumCustomQuery>;

fn bootstrap(client_state: &ClientState, consensus_state: &ConsensusState) -> InstantiateMsg {
    InstantiateMsg {
        client_state: serde_json::to_vec(client_state).unwrap().into(),
        consensus_state: serde_json::to_vec(consensus_state).unwrap().into(),
        checksum: b"checksum".into(),
    }
}

fn instantiate_msg(deps: &mut Deps, msg: InstantiateMsg) -> Result<(), ContractError> {
    let creator = deps.api.addr_make("creator");
    instantiate(
        deps.as_mut(),
        mock_env(),
        message_info(&creator, &coins(1, "uatom")),
        msg,
    )
    .map(|_| ())
}

fn fork(version: u8, epoch: u64) -> Fork {
    Fork {
        version: Version::from([version, 0, 0, 0]),
        epoch,
    }
}

#[test]
fn a_valid_bootstrap_is_accepted() {
    let chain = SyntheticChain::new();
    let mut deps = mk_deps();

    instantiate_msg(
        &mut deps,
        bootstrap(&chain.client_state(), &chain.consensus_state()),
    )
    .unwrap();

    assert!(deps.storage.get(HOST_CLIENT_STATE_KEY.as_bytes()).is_some());
}

#[test]
fn a_bootstrap_with_unusable_parameters_is_rejected_without_writing() {
    let chain = SyntheticChain::new();
    let valid = chain.client_state();
    let size = valid.sync_committee_size;

    let cases: Vec<(&str, ClientState)> = vec![
        (
            "no committee members",
            ClientState {
                sync_committee_size: 0,
                ..valid.clone()
            },
        ),
        (
            "no participants required",
            ClientState {
                min_sync_committee_participants: 0,
                ..valid.clone()
            },
        ),
        (
            "more participants than members",
            ClientState {
                min_sync_committee_participants: size + 1,
                ..valid.clone()
            },
        ),
        (
            "already frozen",
            ClientState {
                is_frozen: true,
                ..valid
            },
        ),
    ];
    for (name, client_state) in cases {
        let mut deps = mk_deps();

        let error = instantiate_msg(
            &mut deps,
            bootstrap(&client_state, &chain.consensus_state()),
        )
        .unwrap_err();

        assert!(
            matches!(error, ContractError::InvalidClientState(_)),
            "{name}: {error}"
        );
        assert!(deps.storage.get(b"contract_info").is_none(), "{name}");
        assert!(deps.storage.get(b"clientState").is_none(), "{name}");
    }
}

#[test]
fn a_bootstrap_without_the_next_sync_committee_is_rejected_without_writing() {
    let chain = SyntheticChain::new();
    let mut deps = mk_deps();
    let consensus_state = ConsensusState {
        next_sync_committee: None,
        ..chain.consensus_state()
    };

    let error = instantiate_msg(
        &mut deps,
        bootstrap(&chain.client_state(), &consensus_state),
    )
    .unwrap_err();

    assert!(matches!(error, ContractError::InvalidConsensusState(_)));
    assert!(deps.storage.get(b"contract_info").is_none());
    assert!(deps.storage.get(b"clientState").is_none());
}

#[test]
fn a_bootstrap_with_forks_out_of_order_is_rejected_without_writing() {
    let chain = SyntheticChain::new();
    let mut client_state = chain.client_state();
    // Deneb would activate after Electra.
    client_state.fork_parameters.deneb = fork(4, 10);
    let mut deps = mk_deps();

    let error = instantiate_msg(
        &mut deps,
        bootstrap(&client_state, &chain.consensus_state()),
    )
    .unwrap_err();

    assert!(matches!(
        error,
        ContractError::InvalidForkParameters {
            fork: "electra",
            ..
        }
    ));
    assert!(deps.storage.get(b"clientState").is_none());
}

/// A client at `latest_slot` 100, which is epoch 3, so every fork up to Electra is active and Fulu
/// and Gloas are not scheduled.
fn running_client() -> (Deps, ClientState) {
    let chain = SyntheticChain::new();
    let client_state = chain.client_state();
    let mut deps = mk_deps();
    instantiate_msg(
        &mut deps,
        bootstrap(&client_state, &chain.consensus_state()),
    )
    .unwrap();
    (deps, client_state)
}

fn update_fork_parameters(
    deps: &mut Deps,
    fork_parameters: ForkParameters,
) -> Result<(), ContractError> {
    migrate(
        deps.as_mut(),
        mock_env(),
        MigrateMsg {
            migration: Migration::UpdateForkParameters(fork_parameters),
        },
    )
    .map(|_| ())
}

#[test]
fn an_active_fork_cannot_be_changed() {
    let (mut deps, client_state) = running_client();
    let before = deps.storage.get(HOST_CLIENT_STATE_KEY.as_bytes()).unwrap();
    let current = client_state.fork_parameters;

    let cases: Vec<(&str, ForkParameters)> = vec![
        (
            "version of an active fork",
            ForkParameters {
                deneb: fork(9, current.deneb.epoch),
                ..current.clone()
            },
        ),
        (
            "activation epoch of an active fork",
            ForkParameters {
                electra: fork(5, 5000),
                ..current.clone()
            },
        ),
        (
            "genesis fork version",
            ForkParameters {
                genesis_fork_version: Version::from([9, 0, 0, 0]),
                ..current
            },
        ),
    ];
    for (name, fork_parameters) in cases {
        let error = update_fork_parameters(&mut deps, fork_parameters).unwrap_err();

        assert!(
            matches!(error, ContractError::InvalidForkParameters { .. }),
            "{name}: {error}"
        );
        assert_eq!(
            deps.storage.get(HOST_CLIENT_STATE_KEY.as_bytes()).unwrap(),
            before,
            "{name}"
        );
    }
}

#[test]
fn a_fork_that_has_not_activated_cannot_move_into_the_past() {
    let (mut deps, client_state) = running_client();
    let before = deps.storage.get(HOST_CLIENT_STATE_KEY.as_bytes()).unwrap();

    // The client's latest slot is in epoch 3, so Fulu at epoch 2 would rewrite history.
    let error = update_fork_parameters(
        &mut deps,
        ForkParameters {
            fulu: fork(6, 2),
            ..client_state.fork_parameters
        },
    )
    .unwrap_err();

    assert!(matches!(
        error,
        ContractError::InvalidForkParameters { fork: "fulu", .. }
    ));
    assert_eq!(
        deps.storage.get(HOST_CLIENT_STATE_KEY.as_bytes()).unwrap(),
        before
    );
}

#[test]
fn fork_parameters_out_of_order_are_rejected() {
    let (mut deps, client_state) = running_client();

    let error = update_fork_parameters(
        &mut deps,
        ForkParameters {
            fulu: fork(6, 5000),
            gloas: fork(7, 4000),
            ..client_state.fork_parameters
        },
    )
    .unwrap_err();

    assert!(matches!(
        error,
        ContractError::InvalidForkParameters { fork: "gloas", .. }
    ));
}

#[test]
fn a_fork_can_be_scheduled_and_rescheduled_before_it_activates() {
    let (mut deps, client_state) = running_client();

    update_fork_parameters(
        &mut deps,
        ForkParameters {
            fulu: fork(6, 5000),
            ..client_state.fork_parameters.clone()
        },
    )
    .unwrap();
    update_fork_parameters(
        &mut deps,
        ForkParameters {
            fulu: fork(6, 6000),
            gloas: fork(7, 9000),
            ..client_state.fork_parameters
        },
    )
    .unwrap();
}
