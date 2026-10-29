//! End-to-end checks, through the contract entry points, of which consensus state a client message
//! may be verified against, how much of the sync committee must sign misbehaviour evidence, and
//! which consensus states survive a governance `Reinstantiate`.

use alloy_primitives::B256;
use cosmwasm_std::{
    coins, from_json,
    testing::{message_info, mock_env, MockApi, MockQuerier, MockStorage},
    Binary, Env, OwnedDeps, Storage, Timestamp,
};
use ethereum_light_client::{
    consensus_state::ConsensusState,
    error::EthereumIBCError,
    header::{ActiveSyncCommittee, Header},
    test_utils::synthetic::{
        SyntheticChain, ANCHOR_SLOT, COMMITTEE_SIZE, FINALIZED_SLOT_STEP, PERIOD_END_SLOT,
    },
};
use ibc_proto::ibc::{
    core::client::v1::Height as IbcProtoHeight,
    lightclients::wasm::v1::ConsensusState as WasmConsensusState,
};

use crate::{
    contract::{instantiate, migrate, query, sudo},
    custom_query::EthereumCustomQuery,
    msg::{
        CheckForMisbehaviourMsg, CheckForMisbehaviourResult, EthereumMisbehaviourMsg, Height,
        InstantiateMsg, MerklePath, MigrateMsg, Migration, QueryMsg, SudoMsg, UpdateStateMsg,
        VerifyClientMessageMsg, VerifyMembershipMsg,
    },
    state::{
        consensus_db_key, get_eth_client_state, get_eth_consensus_state, get_wasm_client_state,
        store_client_state, store_consensus_state,
    },
    test::helpers::mk_deps,
    ContractError,
};

type Deps = OwnedDeps<MockStorage, MockApi, MockQuerier<EthereumCustomQuery>, EthereumCustomQuery>;

/// The slots a chain of two updates on top of the bootstrap consensus state ends up at.
const SECOND_SLOT: u64 = ANCHOR_SLOT + FINALIZED_SLOT_STEP;
const THIRD_SLOT: u64 = SECOND_SLOT + FINALIZED_SLOT_STEP;

struct Setup {
    chain: SyntheticChain,
    deps: Deps,
    env: Env,
}

impl Setup {
    /// A client bootstrapped on the synthetic chain, with the clock past every slot it uses.
    fn new() -> Self {
        let chain = SyntheticChain::new();
        let mut deps = mk_deps();
        let mut env = mock_env();
        env.block.time = Timestamp::from_seconds(chain.current_timestamp());
        let creator = deps.api.addr_make("creator");

        instantiate(
            deps.as_mut(),
            env.clone(),
            message_info(&creator, &coins(1, "uatom")),
            InstantiateMsg {
                client_state: serde_json::to_vec(&chain.client_state()).unwrap().into(),
                consensus_state: serde_json::to_vec(&chain.consensus_state()).unwrap().into(),
                checksum: b"checksum".into(),
            },
        )
        .unwrap();

        Self { chain, deps, env }
    }

    /// A client that has advanced twice, so its latest consensus state is at `THIRD_SLOT` and the
    /// bootstrap one at `ANCHOR_SLOT` is two updates old.
    fn advanced() -> Self {
        let mut setup = Self::new();
        for trusted_slot in [ANCHOR_SLOT, SECOND_SLOT] {
            let header = setup.header(trusted_slot, B256::repeat_byte(0x11));
            setup.verify_header(&header).unwrap();
            setup.update_state(&header).unwrap();
        }
        assert_eq!(setup.latest_slot(), THIRD_SLOT);
        setup
    }

    fn header(&self, trusted_slot: u64, execution_state_root: B256) -> Header {
        Header {
            active_sync_committee: ActiveSyncCommittee::Current(self.chain.committee().clone()),
            consensus_update: self.chain.update_after(
                trusted_slot,
                execution_state_root,
                COMMITTEE_SIZE,
            ),
            trusted_slot,
            execution_header_rlp: None,
        }
    }

    /// Misbehaviour evidence anchored at `trusted_slot`, with each update signed by `signers`.
    fn evidence(&self, trusted_slot: u64, signers: usize) -> EthereumMisbehaviourMsg {
        EthereumMisbehaviourMsg {
            trusted_slot,
            sync_committee: ActiveSyncCommittee::Current(self.chain.committee().clone()),
            update_1: self
                .chain
                .update_after(trusted_slot, B256::repeat_byte(0x11), signers),
            update_2: self
                .chain
                .update_after(trusted_slot, B256::repeat_byte(0x22), signers),
        }
    }

    /// Moves the client's latest slot, and a copy of its latest consensus state, to `slot`, keeping
    /// the earlier consensus states. The synthetic chain only signs inside sync committee period 0,
    /// so this is how a test gets a client whose latest slot is in a later period.
    fn move_latest_to(&mut self, slot: u64) {
        let mut client_state = get_eth_client_state(&self.deps.storage).unwrap();
        let previous_latest_slot = client_state.latest_slot;
        client_state.latest_slot = slot;
        let mut wasm_client_state = get_wasm_client_state(&self.deps.storage).unwrap();
        wasm_client_state.data = serde_json::to_vec(&client_state).unwrap();
        wasm_client_state.latest_height = Some(IbcProtoHeight {
            revision_number: 0,
            revision_height: slot,
        });
        store_client_state(&mut self.deps.storage, &wasm_client_state).unwrap();

        let mut consensus_state =
            get_eth_consensus_state(&self.deps.storage, previous_latest_slot).unwrap();
        consensus_state.slot = slot;
        let wasm_consensus_state = WasmConsensusState {
            data: serde_json::to_vec(&consensus_state).unwrap(),
        };
        store_consensus_state(&mut self.deps.storage, &wasm_consensus_state, slot).unwrap();
    }

    /// Moves the client across the period boundary the way a valid rotation header leaves it: the
    /// latest consensus state is at `slot` in period 1, its current committee is the former next
    /// committee, and no next committee is known yet. The earlier consensus states stay.
    fn rotate_to_next_period(&mut self, slot: u64) {
        self.move_latest_to(slot);
        let mut consensus_state = get_eth_consensus_state(&self.deps.storage, slot).unwrap();
        consensus_state.current_sync_committee =
            self.chain.next_committee().to_summarized_sync_committee();
        consensus_state.next_sync_committee = None;
        let wasm_consensus_state = WasmConsensusState {
            data: serde_json::to_vec(&consensus_state).unwrap(),
        };
        store_consensus_state(&mut self.deps.storage, &wasm_consensus_state, slot).unwrap();
    }

    /// Evidence in period 1, anchored at the consensus state at `ANCHOR_SLOT` in period 0 and
    /// signed by the next committee, which that consensus state knows by its summary.
    fn next_period_evidence(&self, signers: usize) -> EthereumMisbehaviourMsg {
        EthereumMisbehaviourMsg {
            trusted_slot: ANCHOR_SLOT,
            sync_committee: ActiveSyncCommittee::Next(self.chain.next_committee().clone()),
            update_1: self
                .chain
                .next_period_update(B256::repeat_byte(0x11), signers),
            update_2: self
                .chain
                .next_period_update(B256::repeat_byte(0x22), signers),
        }
    }

    fn latest_slot(&self) -> u64 {
        get_eth_client_state(&self.deps.storage)
            .unwrap()
            .latest_slot
    }

    fn consensus_state(&self, slot: u64) -> ConsensusState {
        let any = self
            .deps
            .storage
            .get(consensus_db_key(slot).as_bytes())
            .unwrap();
        crate::state::get_eth_consensus_state(&self.deps.storage, slot).unwrap_or_else(|_| {
            panic!("no consensus state at {slot}: {any:?}");
        })
    }

    fn query(&self, msg: QueryMsg) -> Result<Binary, ContractError> {
        query(self.deps.as_ref(), self.env.clone(), msg)
    }

    fn verify_header(&self, header: &Header) -> Result<Binary, ContractError> {
        self.query(QueryMsg::VerifyClientMessage(VerifyClientMessageMsg {
            client_message: serde_json::to_vec(header).unwrap().into(),
        }))
    }

    fn verify_evidence(&self, evidence: &EthereumMisbehaviourMsg) -> Result<Binary, ContractError> {
        self.query(QueryMsg::VerifyClientMessage(VerifyClientMessageMsg {
            client_message: serde_json::to_vec(evidence).unwrap().into(),
        }))
    }

    fn check_evidence(
        &self,
        evidence: &EthereumMisbehaviourMsg,
    ) -> Result<CheckForMisbehaviourResult, ContractError> {
        self.query(QueryMsg::CheckForMisbehaviour(CheckForMisbehaviourMsg {
            client_message: serde_json::to_vec(evidence).unwrap().into(),
        }))
        .map(|bz| from_json(bz).unwrap())
    }

    /// Verifies a garbage membership proof at `slot`. It can only fail with
    /// `ConsensusStateNotFound` when no consensus state is stored there.
    fn verify_membership(&mut self, slot: u64) -> ContractError {
        sudo(
            self.deps.as_mut(),
            self.env.clone(),
            SudoMsg::VerifyMembership(VerifyMembershipMsg {
                height: Height {
                    revision_number: 0,
                    revision_height: slot,
                },
                delay_time_period: 0,
                delay_block_period: 0,
                proof: Binary::default(),
                merkle_path: MerklePath {
                    key_path: vec![Binary::from(b"ibc".as_slice())],
                },
                value: Binary::default(),
            }),
        )
        .unwrap_err()
    }

    fn reinstantiate_at(&mut self, slot: u64) {
        let mut client_state = self.chain.client_state();
        client_state.latest_slot = slot;
        let mut consensus_state = self.chain.consensus_state();
        consensus_state.slot = slot;

        migrate(
            self.deps.as_mut(),
            self.env.clone(),
            MigrateMsg {
                migration: Migration::Reinstantiate(InstantiateMsg {
                    client_state: serde_json::to_vec(&client_state).unwrap().into(),
                    consensus_state: serde_json::to_vec(&consensus_state).unwrap().into(),
                    checksum: b"checksum".into(),
                }),
            },
        )
        .unwrap();
    }

    fn update_state(&mut self, header: &Header) -> Result<(), ContractError> {
        sudo(
            self.deps.as_mut(),
            self.env.clone(),
            SudoMsg::UpdateState(UpdateStateMsg {
                client_message: serde_json::to_vec(header).unwrap().into(),
            }),
        )
        .map(|_| ())
    }
}

#[test]
fn a_chain_of_headers_each_trusting_the_previous_one_is_accepted() {
    // This is how the relayer submits a period crossing: every header trusts the finalized slot of
    // the one before it, which is the latest consensus state by the time it is verified.
    let setup = Setup::advanced();

    assert_eq!(setup.consensus_state(SECOND_SLOT).slot, SECOND_SLOT);
    assert_eq!(setup.consensus_state(THIRD_SLOT).slot, THIRD_SLOT);
}

#[test]
fn a_header_trusting_an_older_consensus_state_is_rejected() {
    let setup = Setup::advanced();
    // Valid against the bootstrap consensus state, and signed by its whole committee.
    let stale = setup.header(ANCHOR_SLOT, B256::repeat_byte(0x33));

    assert!(matches!(
        setup.verify_header(&stale),
        Err(ContractError::TrustedSlotNotLatest {
            trusted_slot: ANCHOR_SLOT,
            latest_slot: THIRD_SLOT,
        })
    ));
}

#[test]
fn a_stale_header_cannot_overwrite_a_stored_consensus_state() {
    let mut setup = Setup::advanced();
    let key = consensus_db_key(SECOND_SLOT);
    let before = setup.deps.storage.get(key.as_bytes()).unwrap();
    // It finalizes SECOND_SLOT again, but with another execution state root.
    let stale = setup.header(ANCHOR_SLOT, B256::repeat_byte(0x33));

    assert!(matches!(
        setup.update_state(&stale),
        Err(ContractError::TrustedSlotNotLatest { .. })
    ));

    assert_eq!(setup.deps.storage.get(key.as_bytes()).unwrap(), before);
    assert_eq!(setup.latest_slot(), THIRD_SLOT);
}

#[test]
fn supermajority_misbehaviour_anchored_at_the_latest_consensus_state_is_found() {
    let setup = Setup::advanced();
    let evidence = setup.evidence(THIRD_SLOT, 6);

    setup.verify_evidence(&evidence).unwrap();
    assert!(setup.check_evidence(&evidence).unwrap().found_misbehaviour);
}

#[test]
fn minority_misbehaviour_cannot_freeze_the_client() {
    let setup = Setup::advanced();
    // 3 of 8 meets min_sync_committee_participants, the only floor the evidence used to face.
    let evidence = setup.evidence(THIRD_SLOT, 3);

    assert!(matches!(
        setup.check_evidence(&evidence),
        Err(ContractError::VerifyClientMessageFailed(
            EthereumIBCError::NotEnoughSignatures
        ))
    ));
    assert!(matches!(
        setup.verify_evidence(&evidence),
        Err(ContractError::VerifyClientMessageFailed(
            EthereumIBCError::NotEnoughSignatures
        ))
    ));
}

#[test]
fn reinstantiate_discards_every_consensus_state_it_did_not_bootstrap() {
    let mut setup = Setup::advanced();
    // The client trusts roots at three slots, and governance restarts it from a slot in between.
    for slot in [ANCHOR_SLOT, SECOND_SLOT, THIRD_SLOT] {
        assert!(matches!(
            setup.verify_membership(slot),
            ContractError::VerifyMembershipFailed(_)
        ));
    }
    let bootstrap_slot = ANCHOR_SLOT + 50;

    setup.reinstantiate_at(bootstrap_slot);

    assert_eq!(setup.latest_slot(), bootstrap_slot);
    for slot in [ANCHOR_SLOT, SECOND_SLOT, THIRD_SLOT] {
        assert!(
            matches!(
                setup.verify_membership(slot),
                ContractError::ConsensusStateNotFound
            ),
            "the consensus state at {slot} must not survive Reinstantiate"
        );
    }
    // The bootstrap one is there: the proof is rejected, not the height.
    assert!(matches!(
        setup.verify_membership(bootstrap_slot),
        ContractError::VerifyMembershipFailed(_)
    ));
}

#[test]
fn reinstantiate_only_touches_keys_under_the_consensus_state_prefix() {
    let mut setup = Setup::advanced();
    // The keys that sort right around the prefix range, and the contract version.
    let neighbours: [&[u8]; 3] = [b"consensusStates", b"consensusStatesX", b"consensusState"];
    for key in neighbours {
        setup.deps.storage.set(key, b"kept");
    }
    let contract_info = setup.deps.storage.get(b"contract_info").unwrap();

    setup.reinstantiate_at(ANCHOR_SLOT + 50);

    for key in neighbours {
        assert_eq!(setup.deps.storage.get(key).unwrap(), b"kept");
    }
    assert_eq!(
        setup.deps.storage.get(b"contract_info").unwrap(),
        contract_info
    );
}

#[test]
fn misbehaviour_stays_reportable_after_the_client_advances_within_its_period() {
    let setup = Setup::advanced();
    // Two fully signed conflicting headers at one attested slot, built when the client was at
    // ANCHOR_SLOT. The client has since moved on twice, inside the same sync committee period.
    let evidence = setup.evidence(ANCHOR_SLOT, COMMITTEE_SIZE);

    setup.verify_evidence(&evidence).unwrap();
    assert!(setup.check_evidence(&evidence).unwrap().found_misbehaviour);
}

#[test]
fn evidence_for_an_earlier_slot_cannot_be_anchored_at_the_latest_consensus_state() {
    let setup = Setup::advanced();
    // The conflicting headers are at an attested slot below the latest finalized slot, so they are
    // not newer than the latest consensus state and cannot be verified against it. That is why
    // evidence may be anchored at an earlier consensus state of the same period.
    let evidence = EthereumMisbehaviourMsg {
        trusted_slot: THIRD_SLOT,
        ..setup.evidence(ANCHOR_SLOT, COMMITTEE_SIZE)
    };

    assert!(matches!(
        setup.check_evidence(&evidence),
        Err(ContractError::VerifyClientMessageFailed(
            EthereumIBCError::IrrelevantUpdate { .. }
        ))
    ));
}

#[test]
fn evidence_signed_by_a_committee_the_client_has_left_is_rejected() {
    let mut setup = Setup::advanced();
    // Signed by the committee of period 0 and valid against the consensus state at ANCHOR_SLOT,
    // which the client still holds.
    let evidence = setup.evidence(ANCHOR_SLOT, COMMITTEE_SIZE);
    assert!(setup.check_evidence(&evidence).unwrap().found_misbehaviour);

    // The client moves into period 1. The committee that signed the evidence has been replaced
    // by the one that was next, so its keys are no longer ones the client trusts.
    setup.rotate_to_next_period(PERIOD_END_SLOT + 100);

    let past_committee = |result: Result<_, ContractError>| {
        matches!(
            result,
            Err(ContractError::VerifyClientMessageFailed(
                EthereumIBCError::MisbehaviourSignedByPastCommittee {
                    signature_period: 0,
                    latest_period: 1,
                }
            ))
        )
    };
    assert!(past_committee(setup.check_evidence(&evidence).map(|_| ())));
    assert!(past_committee(setup.verify_evidence(&evidence).map(|_| ())));
}

#[test]
fn evidence_signed_by_the_current_committee_stays_reportable_across_a_period_rotation() {
    let mut setup = Setup::new();
    // Signed by the committee of period 1 and verified against the consensus state in period 0,
    // which knows that committee as its next one.
    let evidence = setup.next_period_evidence(COMMITTEE_SIZE);
    assert!(setup.check_evidence(&evidence).unwrap().found_misbehaviour);

    // A valid rotation moves the client into period 1, past the evidence's attested slot. The
    // committee that signed it is now the client's current one.
    let rotated_slot = PERIOD_END_SLOT + 400;
    setup.rotate_to_next_period(rotated_slot);

    setup.verify_evidence(&evidence).unwrap();
    assert!(setup.check_evidence(&evidence).unwrap().found_misbehaviour);

    // No consensus state in period 1 is old enough to anchor it, which is why the period 0 one
    // has to keep working. At the latest one the signing committee is the current committee.
    let reanchored = EthereumMisbehaviourMsg {
        trusted_slot: rotated_slot,
        sync_committee: ActiveSyncCommittee::Current(setup.chain.next_committee().clone()),
        ..evidence
    };
    assert!(matches!(
        setup.check_evidence(&reanchored),
        Err(ContractError::VerifyClientMessageFailed(
            EthereumIBCError::IrrelevantUpdate { .. }
        ))
    ));
}
