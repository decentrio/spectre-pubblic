// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";

/// @title IMisbehaviour
/// @notice Interface for the Misbehaviour module. Validates two conflicting signed headers and
///         their signature proofs; invoked via delegatecall by SpectreClient.
interface IMisbehaviour {
    /// @notice Validates misbehaviour (two conflicting signed headers) and their signature proofs.
    /// @param msg_ The misbehaviour submission message.
    function verifyMisbehaviour(ISpectreClientMsgs.MsgSubmitMisbehaviour calldata msg_) external;
}
