// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable gas-custom-errors

import { Test } from "forge-std/Test.sol";

import { IICS20TransferMsgs } from "contracts/apps/ics20/messages/IICS20TransferMsgs.sol";
import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";

import { IERC20 } from "@openzeppelin-contracts/token/ERC20/IERC20.sol";
import { IICS26RouterErrors } from "contracts/core/errors/IICS26RouterErrors.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";

import { IbcImpl } from "test/utils/IbcImpl.sol";
import { IntegrationEnv } from "test/utils/IntegrationEnv.sol";
import { RecvPacketReentrantERC20 } from "test/mocks/RecvPacketReentrantERC20.sol";
import { TestHelper } from "test/utils/TestHelper.sol";

/// @notice Both `IbcImpl` stacks map the router's relayer selectors to `PUBLIC_ROLE` (`pubRelay = true`), which is the
/// only deployment shape in which an arbitrary token contract may call `recvPacket`.
contract ReentrantRecvPacketTest is Test {
    IbcImpl public ibcImplA;
    IbcImpl public ibcImplB;

    TestHelper public th = new TestHelper();
    IntegrationEnv public integrationEnv = new IntegrationEnv();

    function setUp() public {
        ibcImplA = new IbcImpl(integrationEnv.permit2());
        ibcImplB = new IbcImpl(integrationEnv.permit2());

        ibcImplA.addCounterpartyImpl(ibcImplB, th.FIRST_CLIENT_ID());
        ibcImplB.addCounterpartyImpl(ibcImplA, th.FIRST_CLIENT_ID());
    }

    /// @dev A hostile token re-enters `recvPacket` with a victim's packet while the ICS20 lock is held. The rejection
    /// from the lock must abort the relay as a whole: recording it as the packet's outcome would commit the universal
    /// error acknowledgement next to the receipt and make the packet undeliverable for every honest relayer.
    function test_failure_reentrantRecvPacketIsNotRecordedAsAnErrorAck() public {
        uint256 amount = 1000;
        address victim = integrationEnv.createAndFundUser(amount);
        address receiver = makeAddr("receiver");
        IICS26RouterMsgs.Packet memory victimPacket =
            ibcImplB.sendTransferAsUser(integrationEnv.erc20(), victim, Strings.toHexString(receiver), amount);

        RecvPacketReentrantERC20 hostileToken = new RecvPacketReentrantERC20();
        address attacker = makeAddr("attacker");
        hostileToken.mint(attacker, 1);
        hostileToken.arm(
            ibcImplA.ics26Router(),
            IICS26RouterMsgs.MsgRecvPacket({ packet: victimPacket, membershipMsg: ibcImplA.emptyMembershipMsg() })
        );

        _sendTransferWithHostileToken(attacker, hostileToken);

        // The re-entrant relay was rejected as a whole, with the router's empty-reason failure
        assertFalse(hostileToken.reentrantRelaySucceeded(), "re-entrant relay must not succeed");
        assertEq(
            hostileToken.reentrantRelayRevertData(),
            abi.encodeWithSelector(IICS26RouterErrors.IBCFailedCallback.selector),
            "re-entrant relay must abort instead of reporting an app error"
        );

        // Nothing was recorded for the victim's packet: no receipt and no (error) acknowledgement
        assertEq(
            ibcImplA.relayerHelper().queryPacketReceipt(victimPacket.destClient, victimPacket.sequence),
            bytes32(0),
            "receipt must not be written"
        );
        assertEq(
            ibcImplA.relayerHelper().queryAckCommitment(victimPacket.destClient, victimPacket.sequence),
            bytes32(0),
            "acknowledgement must not be committed"
        );
        assertFalse(ibcImplA.relayerHelper().isPacketReceived(victimPacket));

        // An honest relay still delivers the packet and the source chain settles it with the success ack
        bytes[] memory acks = ibcImplA.recvPacket(victimPacket);
        assertEq(acks, th.SINGLE_SUCCESS_ACK(), "honest relay must succeed");
        assertTrue(ibcImplA.relayerHelper().isPacketReceiveSuccessful(victimPacket));

        string memory voucherDenom = string.concat(
            ICS20Lib.DEFAULT_PORT_ID,
            "/",
            th.FIRST_CLIENT_ID(),
            "/",
            Strings.toHexString(address(integrationEnv.erc20()))
        );
        assertEq(
            IERC20(ibcImplA.ics20Transfer().ibcERC20Contract(voucherDenom)).balanceOf(receiver),
            amount,
            "receiver must be minted the voucher"
        );
        ibcImplB.ackPacket(victimPacket, acks);
    }

    function _sendTransferWithHostileToken(address attacker, RecvPacketReentrantERC20 hostileToken) private {
        vm.startPrank(attacker);
        hostileToken.approve(address(ibcImplA.ics20Transfer()), 1);
        ibcImplA.ics20Transfer()
            .sendTransfer(
                IICS20TransferMsgs.SendTransferMsg({
                    denom: address(hostileToken),
                    amount: 1,
                    receiver: "attacker-receiver",
                    sourceClient: th.FIRST_CLIENT_ID(),
                    destPort: ICS20Lib.DEFAULT_PORT_ID,
                    timeoutTimestamp: uint64(block.timestamp + 10 minutes),
                    memo: ""
                })
            );
        vm.stopPrank();
    }
}
