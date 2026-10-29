// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";

/// @title Light Client Interface
/// @notice Interface for all IBC light clients to implement.
interface ILightClient {
    /// @notice Advances the block state (appHash) against the already-pinned validator set.
    /// @dev The frequent, cheap update path — verifies the header's signature proof but does not
    ///      re-store the validator set.
    /// @param updateMsg The update message.
    /// @return The result of the update operation
    function updateApplicationState(bytes calldata updateMsg) external returns (ILightClientMsgs.UpdateResult);

    /// @notice Rotates the pinned validator set and advances the consensus state.
    /// @dev The rare, heavy update path — verifies the header's signature proof and re-pins the
    ///      validator set carried by the message.
    /// @param updateMsg The update message.
    /// @return The result of the update operation
    function updateConsensusState(bytes calldata updateMsg) external returns (ILightClientMsgs.UpdateResult);

    /// @notice Querying the membership of a key-value pair
    /// @dev Notice that this message is not view, as it may update the client state for caching purposes.
    /// @param msg_ The membership message
    /// @return The unix timestamp of the verification height in the counterparty chain in seconds.
    function verifyMembership(ILightClientMsgs.MsgVerifyMembership calldata msg_) external returns (uint256);

    /// @notice Querying the non-membership of a key
    /// @dev Notice that this message is not view, as it may update the client state for caching purposes.
    /// @param msg_ The membership message
    /// @return The unix timestamp of the verification height in the counterparty chain in seconds.
    function verifyNonMembership(ILightClientMsgs.MsgVerifyNonMembership calldata msg_) external returns (uint256);

    /// @notice Misbehaviour handling, moves the light client to the frozen state if misbehaviour is detected
    /// @param misbehaviourMsg The misbehaviour message
    function misbehaviour(bytes calldata misbehaviourMsg) external;

    /// @notice Unfreezes a previously frozen client. Governance recovery path.
    function unfreeze() external;

    /// @notice Upgrading the client
    /// @param upgradeMsg The upgrade message
    function upgradeClient(bytes calldata upgradeMsg) external;

    /// @notice Returns the client state.
    /// @return The client state.
    function getClientState() external view returns (bytes memory);
}
