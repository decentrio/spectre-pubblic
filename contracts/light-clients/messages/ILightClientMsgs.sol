// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";

/// @title LightClient Messages
/// @notice Interface defining light client messages
interface ILightClientMsgs {
    /// @notice Message for querying the membership of a key-value pair in the Merkle root at a given height.
    struct MsgVerifyMembership {
        IICS02ClientMsgs.Height height;
        IMembershipMsgs.KVPair[] kvPairs;
        IMembershipMsgs.MerkleProof[] merkleProofs;
        bytes32 appHash;
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState;
        IMembershipMsgs.MembershipType membershipType;
        bytes[] path;
        bytes value;
    }

    /// @notice Message for querying the non-membership of a key in the Merkle root at a given height.
    struct MsgVerifyNonMembership {
        IICS02ClientMsgs.Height height;
        IMembershipMsgs.KVPair[] kvPairs;
        IMembershipMsgs.MerkleProof[] merkleProofs;
        bytes32 appHash;
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState;
        IMembershipMsgs.MembershipType membershipType;
        bytes[] path;
    }

    /// @notice The result of an update operation
    enum UpdateResult {
        /// The update was successful
        Update,
        /// A misbehaviour was detected
        Misbehaviour,
        /// Client is already up to date
        NoOp
    }
}
