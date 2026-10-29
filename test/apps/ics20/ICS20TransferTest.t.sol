// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length,function-max-lines

import { Test } from "forge-std/Test.sol";
import { Vm } from "forge-std/Vm.sol";

import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";
import { IICS20TransferMsgs } from "contracts/apps/ics20/messages/IICS20TransferMsgs.sol";

import { IICS20Errors } from "contracts/apps/ics20/errors/IICS20Errors.sol";
import { IIBCAppCallbacks } from "contracts/core/messages/IIBCAppCallbacks.sol";
import { IERC20 } from "@openzeppelin-contracts/token/ERC20/IERC20.sol";
import { IERC20Errors } from "@openzeppelin-contracts/interfaces/draft-IERC6093.sol";
import { IICS26Router } from "contracts/core/interfaces/IICS26Router.sol";
import { IICS20Transfer } from "contracts/apps/ics20/interfaces/IICS20Transfer.sol";
import { IIBCSenderCallbacks } from "contracts/apps/ics20/interfaces/IIBCSenderCallbacks.sol";
import { IRateLimit } from "contracts/apps/ics20/interfaces/IRateLimit.sol";

import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { TestERC20, MalfunctioningERC20 } from "test/mocks/TestERC20.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { ICS24Host } from "contracts/core/libraries/ICS24Host.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { Bytes } from "@openzeppelin-contracts/utils/Bytes.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { IBCERC20 } from "contracts/apps/ics20/IBCERC20.sol";
import { Escrow } from "contracts/apps/ics20/Escrow.sol";
import { ISignatureTransfer } from "@uniswap/permit2/src/interfaces/ISignatureTransfer.sol";
import { DeployPermit2 } from "@uniswap/permit2/test/utils/DeployPermit2.sol";
import { PermitSignature } from "test/utils/PermitSignature.sol";
import {
    CallbackReceiver,
    GasConsumingCallbackReceiver,
    GasProbeCallbackReceiver,
    RevertingCallbackReceiver,
    ReturnBombCallbackReceiver
} from "test/mocks/CallbackReceiver.sol";
import { IBCSenderCallbacksLib } from "contracts/apps/ics20/libraries/IBCSenderCallbacksLib.sol";
import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IBCSelectorLib } from "scripts/deployments/IBCSelectorLib.sol";
import { TestHelper } from "test/utils/TestHelper.sol";
import { IntegrationEnv } from "test/utils/IntegrationEnv.sol";

contract ICS20TransferTest is Test, DeployPermit2, PermitSignature {
    ICS20Transfer public ics20Transfer;
    AccessManager public accessManager;

    address public ics26 = makeAddr("ics26router");
    TestHelper public th = new TestHelper();
    IntegrationEnv public env = new IntegrationEnv();

    function setUp() public {
        address escrowLogic = address(new Escrow());
        address ibcERC20Logic = address(new IBCERC20());
        ICS20Transfer ics20TransferLogic = new ICS20Transfer();

        accessManager = new AccessManager(address(this));
        ERC1967Proxy transferProxy = new ERC1967Proxy(
            address(ics20TransferLogic),
            abi.encodeCall(
                ICS20Transfer.initialize, (ics26, escrowLogic, ibcERC20Logic, env.permit2(), address(accessManager))
            )
        );

        ics20Transfer = ICS20Transfer(address(transferProxy));
        assertEq(ics20Transfer.getPermit2(), env.permit2());
        assertEq(ics20Transfer.ics26(), ics26);

        assertEq(ics20Transfer.ibcERC20Denom(address(env.erc20())), "");
    }

    function test_failure_escrowLaunchGateRejectsUnprovisionedClient() public {
        string memory unprovisionedClient = "client-unprovisioned";
        address sender = makeAddr("sender");
        TestERC20 token = env.erc20();

        vm.expectEmit(false, false, false, true, address(ics20Transfer));
        emit IICS20Transfer.ICS20EscrowLaunchGateEnabled();
        ics20Transfer.enableEscrowLaunchGate();
        assertTrue(ics20Transfer.requiresPrecreatedEscrows());

        token.mint(sender, 1);
        vm.prank(sender);
        token.approve(address(ics20Transfer), 1);

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(token),
            amount: 1,
            receiver: "receiver",
            sourceClient: unprovisionedClient,
            destPort: "client-counterparty",
            timeoutTimestamp: uint64(block.timestamp + 1),
            memo: ""
        });

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20EscrowNotProvisioned.selector, unprovisionedClient));
        vm.prank(sender);
        ics20Transfer.sendTransfer(msgSendTransfer);
    }

    function test_success_escrowLaunchGateAllowsProvisionedClient() public {
        string memory provisionedClient = "client-provisioned";
        address sender = makeAddr("sender");
        TestERC20 token = env.erc20();

        address escrow = ics20Transfer.createEscrow(provisionedClient);
        ics20Transfer.enableEscrowLaunchGate();
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, address(this), 0);
        IRateLimit(escrow).setRateLimit(address(token), 1);

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20EscrowNotActive.selector, provisionedClient));
        ics20Transfer.sendTransfer(
            IICS20TransferMsgs.SendTransferMsg({
                denom: address(token),
                amount: 1,
                receiver: "receiver",
                sourceClient: provisionedClient,
                destPort: "client-counterparty",
                timeoutTimestamp: uint64(block.timestamp + 1),
                memo: ""
            })
        );
        ics20Transfer.activateEscrow(provisionedClient, _singleToken(address(token)));
        assertTrue(ics20Transfer.isEscrowActive(provisionedClient));

        token.mint(sender, 1);
        vm.prank(sender);
        token.approve(address(ics20Transfer), 1);
        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(uint64(1)));

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(token),
            amount: 1,
            receiver: "receiver",
            sourceClient: provisionedClient,
            destPort: "client-counterparty",
            timeoutTimestamp: uint64(block.timestamp + 1),
            memo: ""
        });

        vm.prank(sender);
        assertEq(ics20Transfer.sendTransfer(msgSendTransfer), 1);
        assertEq(token.balanceOf(escrow), 1);
    }

    function test_failure_activateEscrowWithoutNonZeroRateLimit() public {
        string memory clientId = "client-pending";
        address escrow = ics20Transfer.createEscrow(clientId);
        address token = address(env.erc20());
        ics20Transfer.enableEscrowLaunchGate();

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20EscrowRateLimitNotSet.selector, clientId, token));
        ics20Transfer.activateEscrow(clientId, _singleToken(token));
        assertFalse(ics20Transfer.isEscrowActive(clientId));
        assertNotEq(escrow, address(0));
    }

    function test_failure_activateEscrowWithoutTokens() public {
        string memory clientId = "client-pending";
        ics20Transfer.createEscrow(clientId);

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20EscrowTokenListEmpty.selector, clientId));
        ics20Transfer.activateEscrow(clientId, new address[](0));
        assertFalse(ics20Transfer.isEscrowActive(clientId));
    }

    function testFuzz_success_sendTransfer(uint256 amount, uint64 seq, uint64 timeoutTimestamp) public {
        vm.assume(amount > 0);

        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();

        address sender = makeAddr("sender");

        IICS26RouterMsgs.Packet memory expPacket = IICS26RouterMsgs.Packet({
            sequence: seq,
            sourceClient: sourceClient,
            destClient: destClient,
            timeoutTimestamp: timeoutTimestamp,
            payloads: new IICS26RouterMsgs.Payload[](1)
        });
        expPacket.payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: Strings.toHexString(address(env.erc20())),
                    amount: amount,
                    sender: Strings.toHexString(sender),
                    receiver: receiver,
                    memo: memo
                })
            )
        });

        vm.startPrank(sender);
        env.erc20().mint(sender, amount);
        env.erc20().approve(address(ics20Transfer), amount);

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(env.erc20()),
            amount: amount,
            receiver: receiver,
            sourceClient: sourceClient,
            destPort: destClient,
            timeoutTimestamp: timeoutTimestamp,
            memo: memo
        });

        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(seq));
        vm.expectCall(
            ics26,
            abi.encodeCall(
                IICS26Router.sendPacket,
                IICS26RouterMsgs.MsgSendPacket({
                    sourceClient: sourceClient, timeoutTimestamp: timeoutTimestamp, payload: expPacket.payloads[0]
                })
            )
        );
        uint64 sequence = ics20Transfer.sendTransfer(msgSendTransfer);
        assertEq(sequence, seq);

        vm.stopPrank();
    }

    function testFuzz_failure_sendTransfer(uint256 amount, uint64 seq, uint64 timeoutTimestamp) public {
        vm.assume(amount > 0);

        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(seq));

        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();

        address sender = makeAddr("sender");

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(env.erc20()),
            amount: amount,
            receiver: receiver,
            sourceClient: sourceClient,
            destPort: destClient,
            timeoutTimestamp: timeoutTimestamp,
            memo: memo
        });

        // ===== Case 1: Test missing approval =====
        vm.expectRevert(
            abi.encodeWithSelector(IERC20Errors.ERC20InsufficientAllowance.selector, address(ics20Transfer), 0, amount)
        );
        vm.prank(sender);
        ics20Transfer.sendTransfer(msgSendTransfer);

        // ===== Case 2: Test insufficient balance =====
        vm.startPrank(sender);
        env.erc20().approve(address(ics20Transfer), amount);
        vm.expectRevert(abi.encodeWithSelector(IERC20Errors.ERC20InsufficientBalance.selector, sender, 0, amount));
        ics20Transfer.sendTransfer(msgSendTransfer);
        vm.stopPrank();

        env.erc20().mint(sender, amount);

        // ===== Case 3: Empty amount =====
        msgSendTransfer.amount = 0;
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAmount.selector, 0));
        vm.prank(sender);
        ics20Transfer.sendTransfer(msgSendTransfer);

        // reset amount
        msgSendTransfer.amount = amount;

        // ===== Case 4: Malfunctioning ERC20 =====
        MalfunctioningERC20 malfunctioningERC20 = new MalfunctioningERC20();
        malfunctioningERC20.mint(sender, amount);
        malfunctioningERC20.setMalfunction(true); // Turn on the malfunctioning behaviour (no update)
        vm.prank(sender);
        malfunctioningERC20.approve(address(ics20Transfer), amount);

        msgSendTransfer.denom = address(malfunctioningERC20);

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20UnexpectedERC20Balance.selector, amount, 0));
        vm.prank(sender);
        ics20Transfer.sendTransfer(msgSendTransfer);
    }

    function testFuzz_success_sendTransferWithPermit2(uint256 amount, uint64 seq, uint64 timeoutTimestamp) public {
        vm.assume(amount > 0);

        address sender = env.createAndFundUser(amount);
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(env.erc20()),
            amount: amount,
            receiver: receiver,
            sourceClient: sourceClient,
            destPort: destClient,
            timeoutTimestamp: timeoutTimestamp,
            memo: memo
        });

        vm.prank(sender);
        env.erc20().approve(env.permit2(), amount);

        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(seq));

        (ISignatureTransfer.PermitTransferFrom memory permit, bytes memory signature) =
            env.getPermitAndSignature(sender, address(ics20Transfer), amount);

        vm.prank(sender);
        uint64 sequence = ics20Transfer.sendTransferWithPermit2(msgSendTransfer, permit, signature);
        assertEq(sequence, seq);
    }

    function test_rateLimitRefundCreditIsPerPacket() public {
        string memory sourceClient = "source-client";
        string memory destClient = "destination-client";
        address sender1 = env.createAndFundUser(1000);
        address sender2 = env.createAndFundUser(2000);
        address sender3 = env.createAndFundUser(100);

        vm.startPrank(sender1);
        env.erc20().approve(address(ics20Transfer), 1000);
        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(uint64(1)));
        ics20Transfer.sendTransfer(
            IICS20TransferMsgs.SendTransferMsg({
                denom: address(env.erc20()),
                amount: 1000,
                receiver: Strings.toHexString(sender1),
                sourceClient: sourceClient,
                destPort: destClient,
                timeoutTimestamp: uint64(block.timestamp + 1 days),
                memo: ""
            })
        );
        vm.stopPrank();

        Escrow escrow = Escrow(ics20Transfer.getEscrow(sourceClient));
        address token = address(env.erc20());
        address rateLimiter = makeAddr("rateLimiter");
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, rateLimiter, 0);
        vm.prank(rateLimiter);
        escrow.setRateLimit(token, 10_000);

        // Simulate a prior outbound withdrawal so the next deposit only removes part of usage.
        vm.prank(address(ics20Transfer));
        escrow.send(IERC20(token), address(this), 500);
        assertEq(escrow.getDailyUsage(address(env.erc20())), 500);

        vm.startPrank(sender2);
        env.erc20().approve(address(ics20Transfer), 2000);
        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(uint64(2)));
        ics20Transfer.sendTransfer(
            IICS20TransferMsgs.SendTransferMsg({
                denom: address(env.erc20()),
                amount: 2000,
                receiver: Strings.toHexString(sender2),
                sourceClient: sourceClient,
                destPort: destClient,
                timeoutTimestamp: uint64(block.timestamp + 1 days),
                memo: ""
            })
        );
        vm.stopPrank();
        assertEq(escrow.getDailyUsage(address(env.erc20())), 0);

        vm.startPrank(sender3);
        env.erc20().approve(address(ics20Transfer), 100);
        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(uint64(3)));
        ics20Transfer.sendTransfer(
            IICS20TransferMsgs.SendTransferMsg({
                denom: address(env.erc20()),
                amount: 100,
                receiver: Strings.toHexString(sender3),
                sourceClient: sourceClient,
                destPort: destClient,
                timeoutTimestamp: uint64(block.timestamp + 1 days),
                memo: ""
            })
        );
        vm.stopPrank();

        // Resolve packets out of order. Packet 2 restores only its 500-unit reduction; packet 3 restores zero.
        IICS26RouterMsgs.Payload memory payload2 = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: Strings.toHexString(address(env.erc20())),
                    amount: 2000,
                    sender: Strings.toHexString(sender2),
                    receiver: Strings.toHexString(sender2),
                    memo: ""
                })
            )
        });
        IICS26RouterMsgs.Payload memory payload3 = payload2;
        payload3.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: 100,
                sender: Strings.toHexString(sender3),
                receiver: Strings.toHexString(sender3),
                memo: ""
            })
        );

        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(
            IIBCAppCallbacks.OnTimeoutPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: 2,
                payload: payload2,
                relayer: address(this)
            })
        );
        assertEq(escrow.getDailyUsage(address(env.erc20())), 500);

        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(
            IIBCAppCallbacks.OnTimeoutPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: 3,
                payload: payload3,
                relayer: address(this)
            })
        );
        assertEq(escrow.getDailyUsage(address(env.erc20())), 500);
    }

    function testFuzz_failure_sendTransferWithPermit2(uint256 amount, uint64 seq, uint64 timeoutTimestamp) public {
        vm.assume(amount > 0);

        address sender = env.createAndFundUser(amount);
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(env.erc20()),
            amount: amount,
            receiver: th.randomString(),
            sourceClient: sourceClient,
            destPort: destClient,
            timeoutTimestamp: timeoutTimestamp,
            memo: th.randomString()
        });

        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(seq));

        (ISignatureTransfer.PermitTransferFrom memory permit, bytes memory signature) =
            env.getPermitAndSignature(sender, address(ics20Transfer), amount);

        // ===== Case 1: Missing Approval =====
        vm.startPrank(sender);
        env.erc20().approve(env.permit2(), 0);

        vm.expectRevert("TRANSFER_FROM_FAILED");
        ics20Transfer.sendTransferWithPermit2(msgSendTransfer, permit, signature);

        vm.stopPrank();
        // ===== Mint and Approve permit2 =====
        env.erc20().approve(env.permit2(), amount);

        // ===== Case 2: Invalid Amount =====
        vm.startPrank(sender);
        msgSendTransfer.amount = 0;
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAmount.selector, 0));
        ics20Transfer.sendTransferWithPermit2(msgSendTransfer, permit, signature);
        // reset amount
        msgSendTransfer.amount = amount;
        vm.stopPrank();

        // ===== Case 4: Invalid Signature =====
        vm.expectRevert();
        vm.prank(sender);
        ics20Transfer.sendTransferWithPermit2(msgSendTransfer, permit, new bytes(65));

        // ===== Case 3: Permit and Token Mismatch =====
        TestERC20 differentERC20 = new TestERC20();
        vm.startPrank(sender);
        differentERC20.mint(sender, amount);
        differentERC20.approve(env.permit2(), amount);
        vm.stopPrank();
        (permit, signature) = env.getPermitAndSignature(sender, address(ics20Transfer), amount, address(differentERC20));
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS20Errors.ICS20Permit2TokenMismatch.selector, address(differentERC20), env.erc20()
            )
        );
        vm.prank(sender);
        ics20Transfer.sendTransferWithPermit2(msgSendTransfer, permit, signature);

        // ===== Case 4: Malfunctioning ERC20 =====
        MalfunctioningERC20 malfunctioningERC20 = new MalfunctioningERC20();
        malfunctioningERC20.mint(sender, amount);
        malfunctioningERC20.setMalfunction(true);
        vm.prank(sender);
        malfunctioningERC20.approve(env.permit2(), amount);
        (permit, signature) =
            env.getPermitAndSignature(sender, address(ics20Transfer), amount, address(malfunctioningERC20));
        msgSendTransfer.denom = address(malfunctioningERC20);

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20UnexpectedERC20Balance.selector, amount, 0));
        vm.prank(sender);
        ics20Transfer.sendTransferWithPermit2(msgSendTransfer, permit, signature);
    }

    function testFuzz_success_sendTransferWithSender(uint256 amount, uint64 seq, uint64 timeoutTimestamp) public {
        vm.assume(amount > 0);

        address sender = makeAddr("sender");
        address customSender = makeAddr("customSender");
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();

        IICS26RouterMsgs.Packet memory expPacket = IICS26RouterMsgs.Packet({
            sequence: seq,
            sourceClient: sourceClient,
            destClient: destClient,
            timeoutTimestamp: timeoutTimestamp,
            payloads: new IICS26RouterMsgs.Payload[](1)
        });
        expPacket.payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: Strings.toHexString(address(env.erc20())),
                    amount: amount,
                    sender: Strings.toHexString(customSender),
                    receiver: receiver,
                    memo: memo
                })
            )
        });

        // give permission to the delegate sender
        accessManager.grantRole(IBCRolesLib.DELEGATE_SENDER_ROLE, sender, 0);
        accessManager.setTargetFunctionRole(
            address(ics20Transfer), IBCSelectorLib.delegateSenderSelectors(), IBCRolesLib.DELEGATE_SENDER_ROLE
        );

        vm.startPrank(sender);

        env.erc20().mint(sender, amount);
        env.erc20().approve(address(ics20Transfer), amount);

        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(env.erc20()),
            amount: amount,
            receiver: receiver,
            sourceClient: sourceClient,
            destPort: expPacket.payloads[0].sourcePort,
            timeoutTimestamp: timeoutTimestamp,
            memo: memo
        });

        vm.expectCall(
            ics26,
            abi.encodeCall(
                IICS26Router.sendPacket,
                IICS26RouterMsgs.MsgSendPacket({
                    sourceClient: msgSendTransfer.sourceClient,
                    timeoutTimestamp: msgSendTransfer.timeoutTimestamp,
                    payload: expPacket.payloads[0]
                })
            )
        );
        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(seq));
        uint64 sequence = ics20Transfer.sendTransferWithSender(msgSendTransfer, customSender);
        assertEq(sequence, seq);

        vm.stopPrank();
    }

    function test_failure_sendTransferWithSenderZeroSender() public {
        address delegate = makeAddr("delegate");
        accessManager.grantRole(IBCRolesLib.DELEGATE_SENDER_ROLE, delegate, 0);
        accessManager.setTargetFunctionRole(
            address(ics20Transfer), IBCSelectorLib.delegateSenderSelectors(), IBCRolesLib.DELEGATE_SENDER_ROLE
        );
        TestERC20 token = env.erc20();
        token.mint(delegate, 1);
        IICS20TransferMsgs.SendTransferMsg memory msgSendTransfer = IICS20TransferMsgs.SendTransferMsg({
            denom: address(token),
            amount: 1,
            receiver: "receiver",
            sourceClient: "client-0",
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            timeoutTimestamp: uint64(block.timestamp + 1),
            memo: ""
        });

        vm.startPrank(delegate);
        token.approve(address(ics20Transfer), 1);
        // A refund to address(0) would revert in the token, leaving the packet and its funds unsettleable
        vm.expectRevert(
            abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, Strings.toHexString(address(0)))
        );
        ics20Transfer.sendTransferWithSender(msgSendTransfer, address(0));
        vm.stopPrank();
        assertEq(token.balanceOf(delegate), 1, "nothing may be taken from the delegate");
    }

    function test_failure_setCustomERC20ForTokenAddressDenom() public {
        address customToken = makeAddr("customToken");
        address nativeToken = address(env.erc20());

        // Refunds of in-flight native packets would be paid in the custom token instead of in the native one
        string memory lowercase = Strings.toHexString(nativeToken);
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20CustomDenomIsAddress.selector, lowercase));
        ics20Transfer.setCustomERC20(lowercase, customToken);

        string memory unprefixed = Strings.toHexString(uint256(uint160(nativeToken)), 20);
        string memory checksummed = Strings.toChecksumHexString(nativeToken);
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20CustomDenomIsAddress.selector, checksummed));
        ics20Transfer.setCustomERC20(checksummed, customToken);
        assertEq(ics20Transfer.ibcERC20Denom(customToken), "", "nothing may be registered");
        assertEq(bytes(unprefixed).length, 42);

        // Any other denom is still accepted
        ics20Transfer.setCustomERC20("transfer/client-0/uatom", customToken);
        assertEq(ics20Transfer.ibcERC20Denom(customToken), "transfer/client-0/uatom");
    }

    function testFuzz_success_onAcknowledgementPacketCallback(
        uint256 amount,
        uint64 seq,
        uint64 timeoutTimestamp
    )
        public
    {
        // override sender
        address sender = address(new CallbackReceiver());

        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IICS26RouterMsgs.Packet memory expPacket = IICS26RouterMsgs.Packet({
            sequence: seq,
            sourceClient: sourceClient,
            destClient: destClient,
            timeoutTimestamp: timeoutTimestamp,
            payloads: new IICS26RouterMsgs.Payload[](1)
        });
        expPacket.payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: Strings.toHexString(address(env.erc20())),
                    amount: amount,
                    sender: Strings.toHexString(sender),
                    receiver: receiver,
                    memo: memo
                })
            )
        });

        // cheat the escrow mapping to not error on finding the escrow
        bytes32 someAddress = keccak256("someAddress");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), someAddress);

        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory callbackMsg =
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: seq,
                payload: expPacket.payloads[0],
                acknowledgement: ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON,
                relayer: relayer
            });

        // Test success ack with callback
        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (true, callbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);

        // Test error ack with callback
        address escrowAddress = address(uint160(uint256(someAddress)));
        callbackMsg.acknowledgement = abi.encodePacked(ICS24Host.UNIVERSAL_ERROR_ACK);
        vm.mockCall(escrowAddress, Escrow.recvCallback.selector, abi.encode(uint256(0)));
        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (false, callbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);

        // Any non-conforming acknowledgement must fail closed and take the refund path.
        callbackMsg.acknowledgement = bytes("legacy-error");
        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (false, callbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
    }

    function testFuzz_success_refundCallbacksWhenPaused(uint256 amount, uint64 seq) public {
        amount = (amount % type(uint128).max) + 1;

        address sender = address(new CallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IICS26RouterMsgs.Payload memory payload = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: Strings.toHexString(address(env.erc20())),
                    amount: amount,
                    sender: Strings.toHexString(sender),
                    receiver: receiver,
                    memo: memo
                })
            )
        });

        Escrow escrowLogic = new Escrow();
        ERC1967Proxy escrowProxy = new ERC1967Proxy(
            address(escrowLogic), abi.encodeCall(Escrow.initialize, (address(ics20Transfer), address(accessManager)))
        );
        address escrowAddress = address(escrowProxy);
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), bytes32(uint256(uint160(escrowAddress))));
        env.erc20().mint(escrowAddress, amount * 2);

        uint256 startingBalance = env.erc20().balanceOf(sender);

        ics20Transfer.pause();
        assert(ics20Transfer.paused());

        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory ackCallbackMsg =
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: seq,
                payload: payload,
                acknowledgement: ICS24Host.UNIVERSAL_ERROR_ACK,
                relayer: relayer
            });

        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (false, ackCallbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(ackCallbackMsg);
        assertEq(env.erc20().balanceOf(sender), startingBalance + amount);

        IIBCAppCallbacks.OnTimeoutPacketCallback memory timeoutCallbackMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: sourceClient, destinationClient: destClient, sequence: seq, payload: payload, relayer: relayer
        });

        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onTimeoutPacket, (timeoutCallbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(timeoutCallbackMsg);
        assertEq(env.erc20().balanceOf(sender), startingBalance + amount * 2);
    }

    function testFuzz_success_onAcknowledgementPacketRevertingCallback(uint256 amount, uint64 seq) public {
        address sender = address(new RevertingCallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory callbackMsg =
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: seq,
                payload: IICS26RouterMsgs.Payload({
                    sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                    destPort: ICS20Lib.DEFAULT_PORT_ID,
                    version: ICS20Lib.ICS20_VERSION,
                    encoding: ICS20Lib.ICS20_ENCODING,
                    value: abi.encode(
                        IICS20TransferMsgs.FungibleTokenPacketData({
                            denom: Strings.toHexString(address(env.erc20())),
                            amount: amount,
                            sender: Strings.toHexString(sender),
                            receiver: receiver,
                            memo: memo
                        })
                    )
                }),
                acknowledgement: ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON,
                relayer: relayer
            });

        bytes memory reason = abi.encodeWithSignature("Error(string)", "ack callback failed");
        vm.expectEmit(true, false, false, true, address(ics20Transfer));
        emit IICS20Transfer.IBCSenderAckPacketCallbackError(sender, reason);

        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
    }

    function testFuzz_success_onAcknowledgementPacketOOGCallback(uint256 amount, uint64 seq) public {
        address sender = address(new GasConsumingCallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory callbackMsg =
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: seq,
                payload: IICS26RouterMsgs.Payload({
                    sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                    destPort: ICS20Lib.DEFAULT_PORT_ID,
                    version: ICS20Lib.ICS20_VERSION,
                    encoding: ICS20Lib.ICS20_ENCODING,
                    value: abi.encode(
                        IICS20TransferMsgs.FungibleTokenPacketData({
                            denom: Strings.toHexString(address(env.erc20())),
                            amount: amount,
                            sender: Strings.toHexString(sender),
                            receiver: receiver,
                            memo: memo
                        })
                    )
                }),
                acknowledgement: ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON,
                relayer: relayer
            });

        vm.expectEmit(true, false, false, true, address(ics20Transfer));
        emit IICS20Transfer.IBCSenderAckPacketCallbackError(sender, bytes(""));

        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket{ gas: 500_000 }(callbackMsg);
    }

    function testFuzz_failure_onAcknowledgementPacket(uint256 amount, uint64 seq) public {
        address sender = makeAddr("sender");
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory callbackMsg =
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: seq,
                payload: IICS26RouterMsgs.Payload({
                    sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                    destPort: ICS20Lib.DEFAULT_PORT_ID,
                    version: ICS20Lib.ICS20_VERSION,
                    encoding: ICS20Lib.ICS20_ENCODING,
                    value: abi.encode(
                        IICS20TransferMsgs.FungibleTokenPacketData({
                            denom: Strings.toHexString(address(env.erc20())),
                            amount: amount,
                            sender: Strings.toHexString(sender),
                            receiver: receiver,
                            memo: memo
                        })
                    )
                }),
                acknowledgement: ICS24Host.UNIVERSAL_ERROR_ACK,
                relayer: relayer
            });

        // cheat the escrow mapping to not error on finding the escrow
        bytes32 someAddress = keccak256("someAddress");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), someAddress);

        // ===== Case 1: Invalid Data =====
        bytes memory data = bytes("invalid");
        callbackMsg.payload.value = data;
        vm.expectRevert();
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
        // reset data
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );

        // ===== Case 2: Invalid contract/denom =====
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: th.INVALID_ID(),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, th.INVALID_ID()));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
        // reset denom
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );

        // ===== Case 4: Denom not found for a non-native token (source trace) =====
        string memory missingDenom =
            string(abi.encodePacked(callbackMsg.payload.sourcePort, "/", callbackMsg.sourceClient, "/", "notfound"));
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: missingDenom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20DenomNotFound.selector, missingDenom));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
        // reset denom
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );

        // ===== Case 5: Invalid Sender =====
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: th.INVALID_ID(),
                receiver: receiver,
                memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, th.INVALID_ID()));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
        // reset sender
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );
    }

    function testFuzz_success_onTimeoutPacketCallback(uint256 amount, uint64 seq) public {
        address sender = address(new CallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnTimeoutPacketCallback memory callbackMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destClient,
            sequence: seq,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: Strings.toHexString(address(env.erc20())),
                        amount: amount,
                        sender: Strings.toHexString(sender),
                        receiver: receiver,
                        memo: memo
                    })
                )
            }),
            relayer: relayer
        });

        // cheat the escrow mapping to not error on finding the escrow
        bytes32 someAddress = keccak256("someAddress");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), someAddress);

        // Test success timeout with callback
        address escrowAddress = address(uint160(uint256(someAddress)));
        vm.mockCall(escrowAddress, Escrow.recvCallback.selector, abi.encode(uint256(0)));
        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onTimeoutPacket, (callbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(callbackMsg);
    }

    function testFuzz_success_onTimeoutPacketRevertingCallback(uint256 amount, uint64 seq) public {
        address sender = address(new RevertingCallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnTimeoutPacketCallback memory callbackMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destClient,
            sequence: seq,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: Strings.toHexString(address(env.erc20())),
                        amount: amount,
                        sender: Strings.toHexString(sender),
                        receiver: receiver,
                        memo: memo
                    })
                )
            }),
            relayer: relayer
        });

        bytes32 someAddress = keccak256("someAddress");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), someAddress);

        address escrowAddress = address(uint160(uint256(someAddress)));
        vm.mockCall(escrowAddress, Escrow.recvCallback.selector, abi.encode(uint256(0)));

        bytes memory reason = abi.encodeWithSignature("Error(string)", "timeout callback failed");
        vm.expectEmit(true, false, false, true, address(ics20Transfer));
        emit IICS20Transfer.IBCSenderTimeoutPacketCallbackError(sender, reason);

        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(callbackMsg);
    }

    function testFuzz_success_onTimeoutPacketOOGCallback(uint256 amount, uint64 seq) public {
        address sender = address(new GasConsumingCallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnTimeoutPacketCallback memory callbackMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destClient,
            sequence: seq,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: Strings.toHexString(address(env.erc20())),
                        amount: amount,
                        sender: Strings.toHexString(sender),
                        receiver: receiver,
                        memo: memo
                    })
                )
            }),
            relayer: relayer
        });

        bytes32 someAddress = keccak256("someAddress");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), someAddress);

        address escrowAddress = address(uint160(uint256(someAddress)));
        vm.mockCall(escrowAddress, Escrow.recvCallback.selector, abi.encode(uint256(0)));

        vm.expectEmit(true, false, false, true, address(ics20Transfer));
        emit IICS20Transfer.IBCSenderTimeoutPacketCallbackError(sender, bytes(""));

        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket{ gas: 500_000 }(callbackMsg);
    }

    function testFuzz_failure_onTimeoutPacket(uint256 amount, uint64 seq) public {
        address sender = makeAddr("sender");
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        IIBCAppCallbacks.OnTimeoutPacketCallback memory callbackMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destClient,
            sequence: seq,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: Strings.toHexString(address(env.erc20())),
                        amount: amount,
                        sender: Strings.toHexString(sender),
                        receiver: receiver,
                        memo: memo
                    })
                )
            }),
            relayer: relayer
        });

        // cheat the escrow mapping to not error on finding the escrow
        bytes32 someAddress = keccak256("someAddress");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), someAddress);

        // ===== Case 1: Invalid Data
        callbackMsg.payload.value = bytes("invalid");
        vm.expectRevert(bytes(""));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(callbackMsg);
        // reset data
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );

        // ===== Case 2: Invalid ERC20 Denom =====
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: th.INVALID_ID(),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, th.INVALID_ID()));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(callbackMsg);
        // reset denom
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );

        // ===== Case 3: Denom not found for a non-native token (source trace) =====
        string memory invalidDenom =
            string(abi.encodePacked(callbackMsg.payload.sourcePort, "/", callbackMsg.sourceClient, "/", "notfound"));
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: invalidDenom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20DenomNotFound.selector, invalidDenom));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(callbackMsg);
        // reset denom
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );

        // ===== Case 4: Invalid Sender =====
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: th.INVALID_ID(),
                receiver: receiver,
                memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, th.INVALID_ID()));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(callbackMsg);
        // reset sender
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: Strings.toHexString(address(env.erc20())),
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );
    }

    function testFuzz_failure_onRecvPacket(uint256 amount, uint64 seq) public {
        vm.assume(amount > 0);

        address sender = makeAddr("sender");
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = Strings.toHexString(makeAddr("receiver"));
        address relayer = makeAddr("relayer");

        string memory denom = string(
            abi.encodePacked(
                ICS20Lib.DEFAULT_PORT_ID, "/", sourceClient, "/", Strings.toHexString(address(env.erc20()))
            )
        );
        IIBCAppCallbacks.OnRecvPacketCallback memory callbackMsg = IIBCAppCallbacks.OnRecvPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destClient,
            sequence: seq,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: denom,
                        amount: amount,
                        sender: Strings.toHexString(sender),
                        receiver: receiver,
                        memo: memo
                    })
                )
            }),
            relayer: relayer
        });

        // ===== Case 1: Invalid Version =====
        callbackMsg.payload.version = th.INVALID_ID();
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS20Errors.ICS20UnexpectedVersion.selector, ICS20Lib.ICS20_VERSION, th.INVALID_ID()
            )
        );
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // Reset version
        callbackMsg.payload.version = ICS20Lib.ICS20_VERSION;

        // ===== Case 2: Invalid Data =====
        callbackMsg.payload.value = bytes("invalid");
        vm.expectRevert(); // here we expect a generic revert caused by the abi.decodePayload function
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset data
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: denom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );

        // ===== Case 3: Invalid Amount =====
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: denom, amount: 0, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAmount.selector, 0));
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset amount
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: denom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );

        // ===== Case 4: Receiver chain is source, but denom is not erc20 address =====
        string memory invalidErc20Denom =
            string(abi.encodePacked(callbackMsg.payload.sourcePort, "/", sourceClient, "/", th.INVALID_ID()));
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: invalidErc20Denom,
                amount: amount,
                sender: Strings.toHexString(sender),
                receiver: receiver,
                memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20DenomNotFound.selector, th.INVALID_ID()));
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset denom
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: denom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );

        // ===== Case 5: Invalid Receiver =====
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: denom, amount: amount, sender: Strings.toHexString(sender), receiver: th.INVALID_ID(), memo: memo
            })
        );
        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, th.INVALID_ID()));
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset receiver
        callbackMsg.payload.value = abi.encode(
            IICS20TransferMsgs.FungibleTokenPacketData({
                denom: denom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
            })
        );

        // ===== Case 6: Invalid Source Port =====
        callbackMsg.payload.sourcePort = th.INVALID_ID();
        vm.expectRevert(
            abi.encodeWithSelector(IICS20Errors.ICS20InvalidPort.selector, ICS20Lib.DEFAULT_PORT_ID, th.INVALID_ID())
        );
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset source port
        callbackMsg.payload.sourcePort = ICS20Lib.DEFAULT_PORT_ID;

        // ===== Case 7: Invalid Dest Port =====
        callbackMsg.payload.destPort = th.INVALID_ID();
        vm.expectRevert(
            abi.encodeWithSelector(IICS20Errors.ICS20InvalidPort.selector, ICS20Lib.DEFAULT_PORT_ID, th.INVALID_ID())
        );
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset dest port
        callbackMsg.payload.destPort = ICS20Lib.DEFAULT_PORT_ID;

        // ===== Case 8: Invalid Encoding =====
        callbackMsg.payload.encoding = th.INVALID_ID();
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS20Errors.ICS20UnexpectedEncoding.selector, ICS20Lib.ICS20_ENCODING, th.INVALID_ID()
            )
        );
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
        // reset encoding
        callbackMsg.payload.encoding = ICS20Lib.ICS20_ENCODING;
    }

    function test_failure_onRecvPacket_zeroAddressReceiver() public {
        address sender = makeAddr("sender");
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();

        string memory denom = string(
            abi.encodePacked(
                ICS20Lib.DEFAULT_PORT_ID, "/", sourceClient, "/", Strings.toHexString(address(env.erc20()))
            )
        );

        string memory zeroAddrStr = Strings.toHexString(address(0));

        IIBCAppCallbacks.OnRecvPacketCallback memory callbackMsg = IIBCAppCallbacks.OnRecvPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destClient,
            sequence: 1,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: denom,
                        amount: 1 ether,
                        sender: Strings.toHexString(sender),
                        receiver: zeroAddrStr,
                        memo: ""
                    })
                )
            }),
            relayer: makeAddr("relayer")
        });

        vm.expectRevert(abi.encodeWithSelector(IICS20Errors.ICS20InvalidAddress.selector, zeroAddrStr));
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(callbackMsg);
    }

    // Tests that the IBCERC20 re-mint refund path (isDestSource = true) works while the contract is paused.
    // The denom carries the transfer/{sourceClient}/ prefix, so _refundTokens mints to escrow then sends to refundee.
    function testFuzz_success_ibcERC20RefundWhenPaused(uint256 amount, uint64 seq) public {
        amount = (amount % type(uint128).max) + 1;

        address sender = address(new CallbackReceiver());
        string memory sourceClient = th.randomString();
        string memory destClient = th.randomString();
        string memory memo = th.randomString();
        string memory receiver = th.randomString();
        address relayer = makeAddr("relayer");

        // Set up escrow for sourceClient
        Escrow escrowLogic = new Escrow();
        ERC1967Proxy escrowProxy = new ERC1967Proxy(
            address(escrowLogic), abi.encodeCall(Escrow.initialize, (address(ics20Transfer), address(accessManager)))
        );
        address escrowAddress = address(escrowProxy);
        vm.store(address(ics20Transfer), _getEscrowMappingSlot(sourceClient), bytes32(uint256(uint160(escrowAddress))));

        // Build prefixed denom: transfer/{sourceClient}/atom  →  isDestSource = true in _refundTokens
        string memory denom = string(abi.encodePacked(ICS20Lib.DEFAULT_PORT_ID, "/", sourceClient, "/atom"));

        // Deploy IBCERC20 for the prefixed denom, tied to the escrow and ics20Transfer
        IBCERC20 ibcERC20Logic = new IBCERC20();
        ERC1967Proxy ibcERC20Proxy = new ERC1967Proxy(
            address(ibcERC20Logic), abi.encodeCall(IBCERC20.initialize, (address(ics20Transfer), escrowAddress, denom))
        );
        address ibcERC20Address = address(ibcERC20Proxy);

        // Wire _ibcERC20Contracts[denom] → ibcERC20Address
        vm.store(
            address(ics20Transfer), _getIBCERC20ContractsMappingSlot(denom), bytes32(uint256(uint160(ibcERC20Address)))
        );

        IICS26RouterMsgs.Payload memory payload = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: denom, amount: amount, sender: Strings.toHexString(sender), receiver: receiver, memo: memo
                })
            )
        });

        ics20Transfer.pause();
        assert(ics20Transfer.paused());

        // === onAcknowledgementPacket with error ack ===
        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory ackCallbackMsg =
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: sourceClient,
                destinationClient: destClient,
                sequence: seq,
                payload: payload,
                acknowledgement: ICS24Host.UNIVERSAL_ERROR_ACK,
                relayer: relayer
            });

        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (false, ackCallbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(ackCallbackMsg);
        assertEq(IBCERC20(ibcERC20Address).balanceOf(sender), amount);

        // === onTimeoutPacket ===
        IIBCAppCallbacks.OnTimeoutPacketCallback memory timeoutCallbackMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: sourceClient, destinationClient: destClient, sequence: seq, payload: payload, relayer: relayer
        });

        vm.expectCall(sender, abi.encodeCall(IIBCSenderCallbacks.onTimeoutPacket, (timeoutCallbackMsg)));
        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(timeoutCallbackMsg);
        assertEq(IBCERC20(ibcERC20Address).balanceOf(sender), amount * 2);
    }

    function test_success_ackCallbackRevertDataIsBounded() public {
        address sender = address(new ReturnBombCallbackReceiver());
        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory callbackMsg = _successAckMsg(sender);
        bytes memory expectedReason =
            _boundedRevertData(sender, abi.encodeCall(IIBCSenderCallbacks.onAckPacket, (true, callbackMsg)));

        vm.recordLogs();
        vm.prank(ics26);
        uint256 gasBefore = gasleft();
        ics20Transfer.onAcknowledgementPacket(callbackMsg);
        uint256 gasUsed = gasBefore - gasleft();

        _assertCallbackError(IICS20Transfer.IBCSenderAckPacketCallbackError.selector, sender, expectedReason, gasUsed);
    }

    function test_success_timeoutCallbackRevertDataIsBounded() public {
        address sender = address(new ReturnBombCallbackReceiver());
        IIBCAppCallbacks.OnTimeoutPacketCallback memory callbackMsg = _timeoutMsg(sender);
        bytes memory expectedReason =
            _boundedRevertData(sender, abi.encodeCall(IIBCSenderCallbacks.onTimeoutPacket, (callbackMsg)));

        vm.recordLogs();
        vm.prank(ics26);
        uint256 gasBefore = gasleft();
        ics20Transfer.onTimeoutPacket(callbackMsg);
        uint256 gasUsed = gasBefore - gasleft();

        _assertCallbackError(
            IICS20Transfer.IBCSenderTimeoutPacketCallbackError.selector, sender, expectedReason, gasUsed
        );
    }

    /// @dev Whenever the acknowledgement is processed the callback ran with its full allowance: a transaction gas
    /// limit that cannot afford that is rejected instead of starving a callback whose failure may not fail the packet
    function test_success_ackCallbackIsNeverStarved() public {
        GasProbeCallbackReceiver probe = new GasProbeCallbackReceiver();
        bytes memory call_ = abi.encodeCall(ICS20Transfer.onAcknowledgementPacket, (_successAckMsg(address(probe))));

        // The first gas limit that processes the ack is a little over 142,000 with this message
        _assertCallbackNeverStarved(call_, probe, 130_000, 155_000, 25);
    }

    function test_success_timeoutCallbackIsNeverStarved() public {
        GasProbeCallbackReceiver probe = new GasProbeCallbackReceiver();
        bytes memory call_ = abi.encodeCall(ICS20Transfer.onTimeoutPacket, (_timeoutMsg(address(probe))));

        _assertCallbackNeverStarved(call_, probe, 100_000, 260_000, 100);
    }

    /// @dev Calls `ics20Transfer` as the router with every gas limit in the range
    function _assertCallbackNeverStarved(
        bytes memory call_,
        GasProbeCallbackReceiver probe,
        uint256 fromGas,
        uint256 toGas,
        uint256 step
    )
        private
    {
        uint256 allowance = IBCSenderCallbacksLib.SENDER_CALLBACK_GAS_LIMIT;
        uint256 processed;
        uint256 rejectedForGas;

        vm.startPrank(ics26);
        for (uint256 gasLimit = fromGas; gasLimit <= toGas; gasLimit += step) {
            vm.recordLogs();
            (bool ok, bytes memory ret) = address(ics20Transfer).call{ gas: gasLimit }(call_);
            Vm.Log[] memory logs = vm.getRecordedLogs();
            (bool ran, uint256 gasAtEntry) = _callbackEntry(logs, address(probe));

            if (ok) {
                ++processed;
                assertTrue(ran, "the callback must run when the packet is processed");
                // The callback starts with what is left of its allowance after the dispatcher
                assertGe(gasAtEntry, allowance - 1000, "the callback was starved");
            } else {
                assertFalse(ran, "the callback must not run when the packet is rejected");
                if (ret.length != 0) {
                    assertEq(bytes4(ret), IICS20Errors.ICS20InsufficientCallbackGas.selector, "unexpected revert");
                    ++rejectedForGas;
                }
            }
        }
        vm.stopPrank();

        assertGt(processed, 0, "no gas limit in the range processed the packet");
        assertGt(rejectedForGas, 0, "no gas limit in the range was rejected for lack of callback gas");
    }

    /// @dev The ICS20 contract cannot tell a callback that failed for lack of gas from one that failed on its own, so
    /// a failing callback must not be able to hide behind a gas limit that is too low
    function test_failure_callbackGasFloorReportsRequirement() public {
        GasProbeCallbackReceiver probe = new GasProbeCallbackReceiver();
        bytes memory call_ = abi.encodeCall(ICS20Transfer.onAcknowledgementPacket, (_successAckMsg(address(probe))));

        // Just enough to reach the floor check, but not to pass it
        vm.prank(ics26);
        (bool ok, bytes memory ret) = address(ics20Transfer).call{ gas: 120_000 }(call_);
        assertFalse(ok);
        assertEq(bytes4(ret), IICS20Errors.ICS20InsufficientCallbackGas.selector);

        (uint256 required, uint256 available) = abi.decode(_stripSelector(ret), (uint256, uint256));
        assertEq(required, IBCSenderCallbacksLib.SENDER_CALLBACK_MIN_GAS, "the floor must be reported");
        assertLt(available, required, "the reported gas must be below the floor");
    }

    function _boundedRevertData(address sender, bytes memory callData) private returns (bytes memory) {
        (bool ok, bytes memory fullRevertData) = sender.call(callData);
        assertFalse(ok);
        assertEq(fullRevertData.length, ReturnBombCallbackReceiver(sender).BOMB_SIZE(), "unexpected bomb size");
        return Bytes.slice(fullRevertData, 0, IBCSenderCallbacksLib.MAX_REVERT_REASON_LENGTH);
    }

    function _assertCallbackError(
        bytes32 eventSelector,
        address sender,
        bytes memory expectedReason,
        uint256 gasUsed
    )
        private
    {
        Vm.Log[] memory logs = vm.getRecordedLogs();
        uint256 found;
        for (uint256 i = 0; i < logs.length; ++i) {
            if (logs[i].topics[0] != eventSelector) {
                continue;
            }
            ++found;
            assertEq(address(uint160(uint256(logs[i].topics[1]))), sender);
            assertEq(abi.decode(logs[i].data, (bytes)), expectedReason, "reason must be the truncated revert data");
        }
        assertEq(found, 1, "exactly one callback error event");
        // Copying and logging the whole payload costs more than a million gas
        assertLt(gasUsed, 400_000, "the callback revert data must not be paid for in full");
    }

    function _callbackEntry(Vm.Log[] memory logs, address probe) private pure returns (bool ran, uint256 gasAtEntry) {
        for (uint256 i = 0; i < logs.length; ++i) {
            if (logs[i].emitter == probe && logs[i].topics[0] == GasProbeCallbackReceiver.CallbackEntered.selector) {
                return (true, abi.decode(logs[i].data, (uint256)));
            }
        }
    }

    function _stripSelector(bytes memory data) private pure returns (bytes memory) {
        return Bytes.slice(data, 4);
    }

    function _callbackPayload(address sender) private view returns (IICS26RouterMsgs.Payload memory) {
        return IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: Strings.toHexString(address(env.erc20())),
                    amount: 1,
                    sender: Strings.toHexString(sender),
                    receiver: "receiver",
                    memo: ""
                })
            )
        });
    }

    function _successAckMsg(address sender) private returns (IIBCAppCallbacks.OnAcknowledgementPacketCallback memory) {
        return IIBCAppCallbacks.OnAcknowledgementPacketCallback({
            sourceClient: "client-0",
            destinationClient: "client-1",
            sequence: 1,
            payload: _callbackPayload(sender),
            acknowledgement: ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON,
            relayer: makeAddr("relayer")
        });
    }

    function _timeoutMsg(address sender) private returns (IIBCAppCallbacks.OnTimeoutPacketCallback memory) {
        // The refund goes to an escrow that is only a stand-in
        bytes32 escrow = keccak256("escrow");
        vm.store(address(ics20Transfer), _getEscrowMappingSlot("client-0"), escrow);
        vm.mockCall(address(uint160(uint256(escrow))), Escrow.recvCallback.selector, abi.encode(uint256(0)));
        return IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: "client-0",
            destinationClient: "client-1",
            sequence: 1,
            payload: _callbackPayload(sender),
            relayer: makeAddr("relayer")
        });
    }

    function _getEscrowMappingSlot(string memory clientId) internal pure returns (bytes32) {
        bytes32 ics20Slot = 0x823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f800;
        return keccak256(abi.encodePacked(clientId, ics20Slot));
    }

    function _singleToken(address token) internal pure returns (address[] memory tokens) {
        tokens = new address[](1);
        tokens[0] = token;
    }

    function _getIBCERC20ContractsMappingSlot(string memory denom) internal pure returns (bytes32) {
        bytes32 ics20Slot = 0x823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f800;
        // _ibcERC20Contracts is field 1 of ICS20TransferStorage (field 0 is _escrows)
        bytes32 ibcERC20ContractsSlot = bytes32(uint256(ics20Slot) + 1);
        return keccak256(abi.encodePacked(denom, ibcERC20ContractsSlot));
    }
}
