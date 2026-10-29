// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable gas-custom-errors

import { ERC20 } from "@openzeppelin-contracts/token/ERC20/ERC20.sol";

import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";
import { ICS26Router } from "contracts/core/ICS26Router.sol";

/// @title RecvPacketReentrantERC20
/// @notice An attacker-chosen token whose `transferFrom` re-enters `ICS26Router.recvPacket` with a packet that was
/// proven for somebody else. `ICS20Transfer.sendTransfer` calls `transferFrom` while holding its own lock but before
/// the router lock is taken, so the re-entrant relay reaches `ICS20Transfer.onRecvPacket` with the ICS20 lock held.
contract RecvPacketReentrantERC20 is ERC20 {
    ICS26Router private _router;
    bytes private _recvMsg;

    /// @notice Whether the re-entrant `recvPacket` returned instead of reverting
    bool public reentrantRelaySucceeded;
    /// @notice The revert data of the re-entrant `recvPacket`, empty if it did not revert
    bytes public reentrantRelayRevertData;

    constructor() ERC20("Reentrant ERC20", "REENT") { }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    /// @notice Arms the next `transferFrom` to relay `recvMsg` through `router` before moving any tokens
    function arm(ICS26Router router, IICS26RouterMsgs.MsgRecvPacket calldata recvMsg) external {
        _router = router;
        _recvMsg = abi.encode(recvMsg);
    }

    /// @inheritdoc ERC20
    function transferFrom(address from, address to, uint256 value) public override returns (bool) {
        if (_recvMsg.length != 0) {
            IICS26RouterMsgs.MsgRecvPacket memory recvMsg = abi.decode(_recvMsg, (IICS26RouterMsgs.MsgRecvPacket));
            delete _recvMsg;
            // The outcome of the re-entrant relay is recorded rather than propagated: the attacker needs the
            // outer `sendTransfer` to finish so that whatever the relay committed is not rolled back with it.
            try _router.recvPacket(recvMsg) {
                reentrantRelaySucceeded = true;
            } catch (bytes memory reason) {
                reentrantRelayRevertData = reason;
            }
        }
        return super.transferFrom(from, to, value);
    }
}
