//! This module contains all the message types used in Solidity IBC Eureka.
//! In case some message types are not found in the `ics26` module nor the `groth16_ics07` module,
//! they are defined here.

use ibc_client_tendermint_types::ConsensusState as ICS07TendermintConsensusState;
use ibc_core_commitment_types::{commitment::CommitmentRoot, merkle::MerklePath};
use tendermint::{hash::Algorithm, Time};
use tendermint_light_client_verifier::types::Hash;
use time::OffsetDateTime;

alloy_sol_types::sol!("../../contracts/core/messages/IICS26RouterMsgs.sol");
alloy_sol_types::sol!("../../contracts/core/messages/IICS02ClientMsgs.sol");
alloy_sol_types::sol!("../../contracts/apps/ics20/messages/IICS20TransferMsgs.sol");
alloy_sol_types::sol!("../../contracts/core/messages/IIBCAppCallbacks.sol");

alloy_sol_types::sol!("../../contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol");
alloy_sol_types::sol!("../../contracts/light-clients/spectre/messages/IGroth16Msgs.sol");
alloy_sol_types::sol!("../../contracts/light-clients/spectre/messages/IMembershipMsgs.sol");

#[cfg(feature = "rpc")]
impl IGroth16Msgs::Groth16Proof {
    /// Create a new [`Groth16Proof`] instance.
    ///
    /// # Panics
    /// Panics if the vkey is not a valid hex string, or if the bytes cannot be decoded.
    #[must_use]
    pub fn new(vkey: &str, proof: Vec<u8>, public_values: Vec<u8>) -> Self {
        let stripped = vkey.strip_prefix("0x").expect("failed to strip prefix");
        let vkey_bytes: [u8; 32] = hex::decode(stripped)
            .expect("failed to decode vkey")
            .try_into()
            .expect("invalid vkey length");
        Self {
            vKey: vkey_bytes.into(),
            proof: proof.into(),
            publicValues: public_values.into(),
        }
    }
}

#[allow(clippy::fallible_impl_from)]
impl From<ICS07TendermintConsensusState> for ISpectreTendermintMsgs::ConsensusState {
    fn from(ics07_tendermint_consensus_state: ICS07TendermintConsensusState) -> Self {
        let root: [u8; 32] = ics07_tendermint_consensus_state
            .root
            .into_vec()
            .try_into()
            .unwrap();
        let next_validators_hash: [u8; 32] = ics07_tendermint_consensus_state
            .next_validators_hash
            .as_bytes()
            .try_into()
            .unwrap();
        Self {
            #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
            timestamp: ics07_tendermint_consensus_state
                .timestamp
                .unix_timestamp_nanos() as u128,
            root: root.into(),
            nextValidatorsHash: next_validators_hash.into(),
        }
    }
}

#[allow(clippy::fallible_impl_from)]
impl From<ISpectreTendermintMsgs::ConsensusState> for ICS07TendermintConsensusState {
    fn from(consensus_state: ISpectreTendermintMsgs::ConsensusState) -> Self {
        let time = OffsetDateTime::from_unix_timestamp_nanos(
            consensus_state.timestamp.try_into().unwrap(),
        )
        .unwrap();
        let seconds = time.unix_timestamp();
        let nanos = time.nanosecond();
        Self {
            timestamp: Time::from_unix_timestamp(seconds, nanos).unwrap(),
            root: CommitmentRoot::from_bytes(&consensus_state.root.0),
            next_validators_hash: Hash::from_bytes(
                Algorithm::Sha256,
                &consensus_state.nextValidatorsHash.0,
            )
            .unwrap(),
        }
    }
}

impl From<IMembershipMsgs::KVPair> for (MerklePath, Vec<u8>) {
    fn from(kv_pair: IMembershipMsgs::KVPair) -> Self {
        (
            MerklePath {
                key_path: kv_pair
                    .path
                    .into_iter()
                    .map(Vec::from)
                    .map(Into::into)
                    .collect(),
            },
            kv_pair.value.into(),
        )
    }
}

impl From<ibc_core_client_types::Height> for IICS02ClientMsgs::Height {
    fn from(height: ibc_core_client_types::Height) -> Self {
        Self {
            revisionNumber: height.revision_number(),
            revisionHeight: height.revision_height(),
        }
    }
}
