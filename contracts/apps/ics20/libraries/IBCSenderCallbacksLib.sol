// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IIBCAppCallbacks } from "contracts/core/messages/IIBCAppCallbacks.sol";
import { IIBCSenderCallbacks } from "contracts/apps/ics20/interfaces/IIBCSenderCallbacks.sol";
import { IICS20Errors } from "contracts/apps/ics20/errors/IICS20Errors.sol";

import { ERC165Checker } from "@openzeppelin-contracts/utils/introspection/ERC165Checker.sol";

/// @title IBC Callbacks Library
/// @notice This library provides utility functions for IBC Apps to make callbacks to sender contracts.
library IBCSenderCallbacksLib {
    /// @notice The gas a sender callback is given
    uint256 internal constant SENDER_CALLBACK_GAS_LIMIT = 100_000;
    /// @notice The most revert data kept from a failed sender callback, in bytes
    /// @dev The revert data is chosen by the sender contract and is only logged, so the rest is not worth what
    /// copying and emitting it costs the relayer: 8 gas per byte of log data.
    uint256 internal constant MAX_REVERT_REASON_LENGTH = 256;
    /// @notice The gas that must be left for a sender callback to be made
    /// @dev EIP-150 hands a callee at most 63/64 of the gas left once the call itself is paid for, so the full
    /// allowance needs `SENDER_CALLBACK_GAS_LIMIT * 64 / 63`. On top of that, 2,600 gas for a cold account and the
    /// few opcodes between the check and the call are covered with a margin.
    uint256 internal constant SENDER_CALLBACK_MIN_GAS = SENDER_CALLBACK_GAS_LIMIT * 64 / 63 + 4000;

    /// @notice Emitted when a sender acknowledgement callback reverts or runs out of gas
    /// @param callbackAddress The address of the sender callback contract
    /// @param reason The revert reason, truncated to `MAX_REVERT_REASON_LENGTH` bytes, or empty bytes if unavailable
    event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason);
    /// @notice Emitted when a sender timeout callback reverts or runs out of gas
    /// @param callbackAddress The address of the sender callback contract
    /// @param reason The revert reason, truncated to `MAX_REVERT_REASON_LENGTH` bytes, or empty bytes if unavailable
    event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason);

    /// @notice Checks if the given address implements the IIBCSenderCallbacks interface.
    /// @param sender The address to check
    /// @return bool True if the address implements IIBCSenderCallbacks, false otherwise
    function _supportsCallbacks(address sender) private view returns (bool) {
        return ERC165Checker.supportsInterface(sender, type(IIBCSenderCallbacks).interfaceId);
    }

    /// @notice Make a callback to the sender contract when a packet is acknowledged if it supports IIBCSenderCallbacks.
    /// @dev Reverts if too little gas is left to give the callback its full allowance. A failure of the callback
    /// itself does not revert.
    /// @param callbackAddress The address of the callback contract
    /// @param success Whether the packet was successfully received by the destination chain
    /// @param msg_ The callback message containing details about the acknowledgement
    function ackPacketCallback(
        address callbackAddress,
        bool success,
        IIBCAppCallbacks.OnAcknowledgementPacketCallback calldata msg_
    )
        internal
    {
        if (_supportsCallbacks(callbackAddress)) {
            (bool ok, bytes memory reason) =
                _callSender(callbackAddress, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (success, msg_)));
            if (!ok) {
                emit IBCSenderAckPacketCallbackError(callbackAddress, reason);
            }
        }
    }

    /// @notice Make a callback to the sender contract when a packet times out if it supports IIBCSenderCallbacks.
    /// @dev Reverts if too little gas is left to give the callback its full allowance. A failure of the callback
    /// itself does not revert.
    /// @param callbackAddress The address of the callback contract
    /// @param msg_ The callback message containing details about the timeout
    function timeoutPacketCallback(
        address callbackAddress,
        IIBCAppCallbacks.OnTimeoutPacketCallback calldata msg_
    )
        internal
    {
        if (_supportsCallbacks(callbackAddress)) {
            (bool ok, bytes memory reason) =
                _callSender(callbackAddress, abi.encodeCall(IIBCSenderCallbacks.onTimeoutPacket, (msg_)));
            if (!ok) {
                emit IBCSenderTimeoutPacketCallbackError(callbackAddress, reason);
            }
        }
    }

    /// @notice Calls a sender contract with its gas allowance, keeping a bounded amount of its revert data
    /// @dev A high-level call would copy all of the revert data into memory. Whoever the callee is, it can make
    /// that data as large as its own gas pays for, and the caller then pays to copy and log it.
    /// @param callbackAddress The sender contract
    /// @param data The encoded callback
    /// @return ok Whether the callback returned without reverting
    /// @return reason The revert data of a failed callback, truncated to `MAX_REVERT_REASON_LENGTH` bytes
    function _callSender(address callbackAddress, bytes memory data) private returns (bool ok, bytes memory reason) {
        uint256 available = gasleft();
        require(
            available >= SENDER_CALLBACK_MIN_GAS,
            IICS20Errors.ICS20InsufficientCallbackGas(SENDER_CALLBACK_MIN_GAS, available)
        );

        // solhint-disable-next-line no-inline-assembly
        assembly ("memory-safe") {
            ok := call(SENDER_CALLBACK_GAS_LIMIT, callbackAddress, 0, add(data, 0x20), mload(data), 0, 0)
            if iszero(ok) {
                let size := returndatasize()
                if gt(size, MAX_REVERT_REASON_LENGTH) { size := MAX_REVERT_REASON_LENGTH }
                reason := mload(0x40)
                mstore(reason, size)
                returndatacopy(add(reason, 0x20), 0, size)
                // Allocate the length word and the data, rounded up to a whole word
                mstore(0x40, add(add(reason, 0x20), and(add(size, 0x1f), not(0x1f))))
            }
        }
    }
}
