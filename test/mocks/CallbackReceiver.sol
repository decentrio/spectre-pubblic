// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable no-empty-blocks

import { IIBCSenderCallbacks } from "contracts/apps/ics20/interfaces/IIBCSenderCallbacks.sol";
import { IIBCAppCallbacks } from "contracts/core/messages/IIBCAppCallbacks.sol";
import { IBCCallbackReceiver } from "contracts/apps/ics20/callbacks/IBCCallbackReceiver.sol";

/// @title CallbackReceiver
/// @notice A contract that implements the IIBCSenderCallbacks interface to receive callbacks from IBC applications.
contract CallbackReceiver is IBCCallbackReceiver {
    /// @inheritdoc IIBCSenderCallbacks
    function onAckPacket(
        bool success,
        IIBCAppCallbacks.OnAcknowledgementPacketCallback calldata msg_
    )
        external
        override
    {
        // Handle the acknowledgement logic here
        // For example, emit an event or update state
    }

    /// @inheritdoc IIBCSenderCallbacks
    function onTimeoutPacket(IIBCAppCallbacks.OnTimeoutPacketCallback calldata msg_) external override {
        // Handle the timeout logic here
        // For example, emit an event or update state
    }
}

contract RevertingCallbackReceiver is IBCCallbackReceiver {
    function onAckPacket(bool, IIBCAppCallbacks.OnAcknowledgementPacketCallback calldata) external pure override {
        revert("ack callback failed");
    }

    function onTimeoutPacket(IIBCAppCallbacks.OnTimeoutPacketCallback calldata) external pure override {
        revert("timeout callback failed");
    }
}

contract GasConsumingCallbackReceiver is IBCCallbackReceiver {
    uint256 private _counter;

    function onAckPacket(bool, IIBCAppCallbacks.OnAcknowledgementPacketCallback calldata) external override {
        while (true) {
            _counter++;
        }
    }

    function onTimeoutPacket(IIBCAppCallbacks.OnTimeoutPacketCallback calldata) external override {
        while (true) {
            _counter++;
        }
    }
}

/// @notice Fails every callback with a huge revert payload, hoping the caller copies and re-emits all of it
contract ReturnBombCallbackReceiver is IBCCallbackReceiver {
    /// @notice The size of the revert payload. Its memory is paid for by this contract within the callback gas limit.
    uint256 public constant BOMB_SIZE = 180_000;
    /// @notice The first bytes of the revert payload, the rest of which is zeroes
    bytes4 public constant MARKER = 0xdeadbeef;

    function onAckPacket(bool, IIBCAppCallbacks.OnAcknowledgementPacketCallback calldata) external pure override {
        _bomb();
    }

    function onTimeoutPacket(IIBCAppCallbacks.OnTimeoutPacketCallback calldata) external pure override {
        _bomb();
    }

    function _bomb() private pure {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            mstore(0, shl(224, 0xdeadbeef))
            revert(0, 180000)
        }
    }
}

/// @notice Announces the gas it was given, so tests can tell whether a callback got its full allowance
/// @dev An event keeps the callback cheap: creating state costs more than the callback allowance
contract GasProbeCallbackReceiver is IBCCallbackReceiver {
    /// @notice Emitted when a callback starts
    /// @param gasAtEntry The gas left at the start of the callback
    event CallbackEntered(uint256 gasAtEntry);

    function onAckPacket(bool, IIBCAppCallbacks.OnAcknowledgementPacketCallback calldata) external override {
        emit CallbackEntered(gasleft());
    }

    function onTimeoutPacket(IIBCAppCallbacks.OnTimeoutPacketCallback calldata) external override {
        emit CallbackEntered(gasleft());
    }
}
