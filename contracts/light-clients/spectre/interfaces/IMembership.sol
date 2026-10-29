// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";

/// @title IMembership
/// @notice Interface for the Membership module (ICS-23 Merkle proof verification).
///         Stateless and pure math over calldata; invoked via staticcall by SpectreClient.
interface IMembership {
    /// @notice Verifies ICS-23 (non)membership proofs against the trusted appHash root.
    function verifyMembership(
        bytes32 appHash,
        IMembershipMsgs.KVPair[] calldata kvPairs,
        IMembershipMsgs.MerkleProof[] calldata merkleProofs
    )
        external
        view;
}
