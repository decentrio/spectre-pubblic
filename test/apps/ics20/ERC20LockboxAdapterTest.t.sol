// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length,max-states-count

import { Test } from "forge-std/Test.sol";

import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";
import { IICS20TransferMsgs } from "contracts/apps/ics20/messages/IICS20TransferMsgs.sol";
import { IIBCAppCallbacks } from "contracts/core/messages/IIBCAppCallbacks.sol";

import { IICS26Router } from "contracts/core/interfaces/IICS26Router.sol";
import { IERC20LockboxAdapter } from "contracts/apps/ics20/interfaces/IERC20LockboxAdapter.sol";
import { IERC20LockboxAdapterErrors } from "contracts/apps/ics20/errors/IERC20LockboxAdapterErrors.sol";
import { IRateLimitErrors } from "contracts/apps/ics20/errors/IRateLimitErrors.sol";
import { ISignatureTransfer } from "@uniswap/permit2/src/interfaces/ISignatureTransfer.sol";

import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { ERC20LockboxAdapter } from "contracts/apps/ics20/ERC20LockboxAdapter.sol";
import { Escrow } from "contracts/apps/ics20/Escrow.sol";
import { IBCERC20 } from "contracts/apps/ics20/IBCERC20.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { ICS24Host } from "contracts/core/libraries/ICS24Host.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IntegrationEnv } from "test/utils/IntegrationEnv.sol";
import { PermitSignature } from "test/utils/PermitSignature.sol";

import { ERC20 } from "@openzeppelin-contracts/token/ERC20/ERC20.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";

/// @notice A minimal, already-deployed ERC20: fixed supply minted once, no mint or burn.
contract FixedSupplyERC20 is ERC20 {
    constructor(uint256 supply) ERC20("Fixed Supply Token", "FIXED") {
        _mint(msg.sender, supply);
    }
}

/// @notice A token whose transfer fee can be enabled after the adapter reserve is funded.
contract ToggleFeeERC20 is ERC20 {
    bool public feeEnabled;

    constructor(uint256 supply) ERC20("Toggle Fee Token", "FEE") {
        _mint(msg.sender, supply);
    }

    function setFeeEnabled(bool enabled) external {
        feeEnabled = enabled;
    }

    function _update(address from, address to, uint256 value) internal override {
        if (feeEnabled && from != address(0) && to != address(0) && value != 0) {
            super._update(from, to, value - 1);
            super._update(from, address(0), 1);
        } else {
            super._update(from, to, value);
        }
    }
}

contract NoEscrowInterface { }

contract BoundEscrow {
    address public immutable ics20;

    constructor(address ics20_) {
        ics20 = ics20_;
    }
}

contract ERC20LockboxAdapterTest is Test {
    ICS20Transfer public ics20Transfer;
    ERC20LockboxAdapter public adapter;
    FixedSupplyERC20 public underlying;
    AccessManager public accessManager;
    address public escrow;

    IntegrationEnv public env = new IntegrationEnv();
    PermitSignature public permitHelper = new PermitSignature();

    address public ics26 = makeAddr("ics26router");
    address public treasury = makeAddr("treasury");

    string public constant CLIENT = "client-0";
    string public constant FORWARD_CLIENT = "client-1";
    string public constant COUNTERPARTY = "client-99";
    string public constant FORWARD_COUNTERPARTY = "client-forward-counterparty";
    string public constant BASE_DENOM = "uatom";
    string public constant DENOM_PATH = "transfer/client-0/uatom";
    string public constant FEE_BASE_DENOM = "ufee";
    string public constant FEE_DENOM_PATH = "transfer/client-0/ufee";

    uint8 public constant DENOM_DECIMALS = 6;
    uint8 public constant UNDERLYING_DECIMALS = 18;
    uint256 public constant SCALE = 10 ** uint256(UNDERLYING_DECIMALS - DENOM_DECIMALS);
    uint256 public constant FIXED_SUPPLY = 1_000_000e18;
    uint256 public constant INITIAL_RESERVE = 500_000e18;

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

        vm.prank(treasury);
        underlying = new FixedSupplyERC20(FIXED_SUPPLY);

        adapter = new ERC20LockboxAdapter(
            address(ics20Transfer), address(underlying), DENOM_DECIMALS, UNDERLYING_DECIMALS, address(accessManager)
        );

        // Production governance pre-creates the escrow before configuring tokens and activation.
        escrow = ics20Transfer.createEscrow(CLIENT);

        adapter.addEscrow(escrow);
        ics20Transfer.setCustomERC20(DENOM_PATH, address(adapter));

        vm.prank(treasury);
        assertTrue(underlying.transfer(address(adapter), INITIAL_RESERVE));
    }

    // ==================== setup ====================

    function test_success_setup() public view {
        assertEq(ics20Transfer.ibcERC20Contract(DENOM_PATH), address(adapter), "denom not mapped to adapter");
        assertEq(ics20Transfer.ibcERC20Denom(address(adapter)), DENOM_PATH, "reverse mapping missing");
        assertEq(adapter.scale(), SCALE, "scale mismatch");
        assertEq(adapter.decimals(), DENOM_DECIMALS, "adapter reports denom decimals");
        assertEq(adapter.reserve(), INITIAL_RESERVE, "reserve mismatch");
        assertEq(adapter.capacity(), INITIAL_RESERVE / SCALE, "capacity mismatch");
        assert(adapter.isEscrow(escrow));
    }

    function test_failure_deployWithBadDecimals() public {
        // underlying with fewer decimals than the denom would truncate user funds
        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterInvalidDecimals.selector, 18, 6));
        new ERC20LockboxAdapter(address(ics20Transfer), address(underlying), 18, 6, address(accessManager));

        // declared decimals must match what the underlying reports
        vm.expectRevert(
            abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterUnderlyingDecimalsMismatch.selector, 18, 8)
        );
        new ERC20LockboxAdapter(address(ics20Transfer), address(underlying), 6, 8, address(accessManager));

        // 10 ** 78 cannot be represented in a uint256.
        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterInvalidDecimals.selector, 0, 78));
        new ERC20LockboxAdapter(address(ics20Transfer), address(underlying), 0, 78, address(accessManager));
    }

    function test_failure_deployWithNonContractDependency() public {
        address notContract = makeAddr("notContract");

        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotContract.selector, notContract));
        new ERC20LockboxAdapter(
            notContract, address(underlying), DENOM_DECIMALS, UNDERLYING_DECIMALS, address(accessManager)
        );

        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotContract.selector, notContract));
        new ERC20LockboxAdapter(
            address(ics20Transfer), notContract, DENOM_DECIMALS, UNDERLYING_DECIMALS, address(accessManager)
        );

        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotContract.selector, notContract));
        new ERC20LockboxAdapter(
            address(ics20Transfer), address(underlying), DENOM_DECIMALS, UNDERLYING_DECIMALS, notContract
        );
    }

    function test_failure_addInvalidOrMismatchedEscrow() public {
        address notContract = makeAddr("notEscrowContract");
        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotContract.selector, notContract));
        adapter.addEscrow(notContract);

        NoEscrowInterface invalidEscrow = new NoEscrowInterface();
        vm.expectRevert(
            abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterInvalidEscrow.selector, address(invalidEscrow))
        );
        adapter.addEscrow(address(invalidEscrow));

        BoundEscrow mismatchedEscrow = new BoundEscrow(makeAddr("otherICS20"));
        vm.expectRevert(
            abi.encodeWithSelector(
                IERC20LockboxAdapterErrors.AdapterEscrowICS20Mismatch.selector,
                address(mismatchedEscrow),
                address(ics20Transfer),
                mismatchedEscrow.ics20()
            )
        );
        adapter.addEscrow(address(mismatchedEscrow));
    }

    function test_success_worksBehindEscrowLaunchGate() public {
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, address(this), 0);
        Escrow(escrow).setRateLimit(address(adapter), INITIAL_RESERVE / SCALE);

        address[] memory tokens = new address[](1);
        tokens[0] = address(adapter);
        ics20Transfer.activateEscrow(CLIENT, tokens);
        ics20Transfer.enableEscrowLaunchGate();

        address receiver = makeAddr("gatedReceiver");
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, 100e6, 1));

        assertTrue(ics20Transfer.requiresPrecreatedEscrows());
        assertTrue(ics20Transfer.isEscrowActive(CLIENT));
        assertEq(underlying.balanceOf(receiver), 100e6 * SCALE);
    }

    // ==================== receive: Cosmos -> Ethereum ====================

    function testFuzz_success_recv(uint256 amount) public {
        amount = bound(amount, 1, INITIAL_RESERVE / SCALE);
        address receiver = makeAddr("receiver");

        vm.prank(ics26);
        bytes memory ack = ics20Transfer.onRecvPacket(_recvCallback(receiver, amount, 1));
        assertEq(ack, ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON, "unexpected ack");

        // the receiver holds the real, pre-existing token — not a voucher
        assertEq(underlying.balanceOf(receiver), amount * SCALE, "receiver underlying mismatch");
        assertEq(adapter.reserve(), INITIAL_RESERVE - amount * SCALE, "reserve not drawn down");
        // the escrow never holds the underlying, and its adapter balance is cleared
        assertEq(underlying.balanceOf(escrow), 0, "escrow must not hold the underlying");
        assertEq(adapter.balanceOf(escrow), 0, "escrow adapter balance must be cleared");
        assertEq(adapter.totalSupply(), 0, "adapter supply must be cleared");
    }

    function test_success_recvScalesDecimals() public {
        address receiver = makeAddr("receiver");

        // 1.5 ATOM expressed in denom units (6 decimals)
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, 1_500_000, 1));

        assertEq(underlying.balanceOf(receiver), 1.5e18, "1.5 denom units must become 1.5e18 underlying");
    }

    function test_failure_recvExceedsReserve() public {
        address receiver = makeAddr("receiver");
        uint256 amount = (INITIAL_RESERVE / SCALE) + 1;

        vm.expectRevert(
            abi.encodeWithSelector(
                IERC20LockboxAdapterErrors.AdapterInsufficientReserve.selector, amount * SCALE, INITIAL_RESERVE
            )
        );
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, amount, 1));
    }

    function test_failure_amountScalingOverflowIsAtomic() public {
        address receiver = makeAddr("overflowReceiver");
        uint256 amount = type(uint256).max / SCALE + 1;
        bytes memory arithmeticPanic = abi.encodeWithSignature("Panic(uint256)", 0x11);

        vm.expectRevert(arithmeticPanic);
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, amount, 1));

        assertEq(underlying.balanceOf(receiver), 0, "overflow must not release underlying");
        assertEq(adapter.reserve(), INITIAL_RESERVE, "overflow must not change reserve");
        assertEq(adapter.balanceOf(escrow), 0, "overflow must roll back escrow credit");
        assertEq(adapter.totalSupply(), 0, "overflow must roll back supply");

        vm.expectRevert(arithmeticPanic);
        vm.prank(receiver);
        ics20Transfer.sendTransfer(_sendMsg(amount));

        assertEq(adapter.reserve(), INITIAL_RESERVE, "send overflow must not change reserve");
        assertEq(adapter.balanceOf(escrow), 0, "send overflow must not create escrow credit");
    }

    function test_success_recvEmitsReserveLow() public {
        uint256 threshold = 100e6; // 100 ATOM of remaining capacity
        adapter.setLowReserveThreshold(threshold);

        address receiver = makeAddr("receiver");
        uint256 toThreshold = adapter.capacity() - threshold;

        // one unit above the threshold: no alert
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, toThreshold - 1, 1));

        vm.expectEmit(true, true, true, true, address(adapter));
        emit IERC20LockboxAdapter.ReserveLow(threshold - 1, threshold);
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, 2, 2));
    }

    function test_failure_recvRejectsFeeOnRelease() public {
        ToggleFeeERC20 feeToken = new ToggleFeeERC20(1000e18);
        ERC20LockboxAdapter feeAdapter = new ERC20LockboxAdapter(
            address(ics20Transfer), address(feeToken), DENOM_DECIMALS, UNDERLYING_DECIMALS, address(accessManager)
        );
        feeAdapter.addEscrow(escrow);
        ics20Transfer.setCustomERC20(FEE_DENOM_PATH, address(feeAdapter));

        uint256 reserveFunding = 100e18;
        assertTrue(feeToken.transfer(address(feeAdapter), reserveFunding));
        feeToken.setFeeEnabled(true);

        address receiver = makeAddr("feeReceiver");
        uint256 amount = 1e6;
        uint256 required = amount * SCALE;
        vm.expectRevert(
            abi.encodeWithSelector(
                IERC20LockboxAdapterErrors.AdapterUnexpectedUnderlyingTransfer.selector,
                required,
                required,
                required - 1
            )
        );
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallbackForDenom(receiver, amount, 1, FEE_BASE_DENOM, CLIENT));

        assertEq(feeToken.balanceOf(receiver), 0, "failed release must not short-pay receiver");
        assertEq(feeAdapter.reserve(), reserveFunding, "failed release must restore reserve");
    }

    function test_failure_sendRejectsFeeOnLock() public {
        ToggleFeeERC20 feeToken = new ToggleFeeERC20(1000e18);
        ERC20LockboxAdapter feeAdapter = new ERC20LockboxAdapter(
            address(ics20Transfer), address(feeToken), DENOM_DECIMALS, UNDERLYING_DECIMALS, address(accessManager)
        );
        feeAdapter.addEscrow(escrow);

        address user = makeAddr("feeSender");
        uint256 amount = 1e6;
        uint256 required = amount * SCALE;
        assertTrue(feeToken.transfer(user, required));

        vm.prank(user);
        feeToken.approve(address(feeAdapter), required);
        feeToken.setFeeEnabled(true);

        vm.expectRevert(
            abi.encodeWithSelector(
                IERC20LockboxAdapterErrors.AdapterUnexpectedUnderlyingBalance.selector, required, required - 1
            )
        );
        vm.prank(address(ics20Transfer));
        feeAdapter.transferFrom(user, escrow, amount);

        assertEq(feeToken.balanceOf(user), required, "failed lock must not charge user");
        assertEq(feeAdapter.reserve(), 0, "failed lock must not change reserve");
        assertEq(feeAdapter.balanceOf(escrow), 0, "failed lock must not create escrow credit");
    }

    // ==================== send: Ethereum -> Cosmos ====================

    function testFuzz_success_roundTrip(uint256 amount) public {
        amount = bound(amount, 1, INITIAL_RESERVE / SCALE);
        address user = makeAddr("user");

        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(user, amount, 1));
        assertEq(underlying.balanceOf(user), amount * SCALE, "user did not receive the underlying");

        _sendBack(user, amount);

        assertEq(underlying.balanceOf(user), 0, "user underlying not locked");
        assertEq(adapter.reserve(), INITIAL_RESERVE, "reserve not restored");
        assertEq(adapter.balanceOf(escrow), 0, "escrow adapter balance must be cleared after burn");
        assertEq(adapter.totalSupply(), 0, "adapter supply must be cleared after burn");
    }

    function test_failure_sendWithoutUnderlyingApproval() public {
        address user = makeAddr("user");

        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(user, 100e6, 1));

        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(uint64(1)));
        // no approve of the adapter on the underlying
        vm.prank(user);
        vm.expectRevert();
        ics20Transfer.sendTransfer(_sendMsg(100e6));
    }

    function test_failure_sendTransferWithPermit2IsUnsupportedAndDoesNotLock() public {
        uint256 amount = 100e6;
        (address user, uint256 privateKey) = makeAddrAndKey("permitUser");
        vm.prank(treasury);
        assertTrue(underlying.transfer(user, amount * SCALE));

        vm.prank(user);
        underlying.approve(address(adapter), amount * SCALE);

        ISignatureTransfer.PermitTransferFrom memory permit = ISignatureTransfer.PermitTransferFrom({
            permitted: ISignatureTransfer.TokenPermissions({ token: address(adapter), amount: amount }),
            nonce: 1,
            deadline: block.timestamp + 1 hours
        });
        bytes memory signature = permitHelper.getPermitTransferSignature(
            permit, privateKey, address(ics20Transfer), ISignatureTransfer(env.permit2()).DOMAIN_SEPARATOR()
        );

        vm.expectRevert("TRANSFER_FROM_FAILED");
        vm.prank(user);
        ics20Transfer.sendTransferWithPermit2(_sendMsg(amount), permit, signature);

        assertEq(underlying.balanceOf(user), amount * SCALE, "permit2 failure must not lock underlying");
        assertEq(adapter.reserve(), INITIAL_RESERVE, "permit2 failure must not change reserve");
        assertEq(adapter.balanceOf(escrow), 0, "permit2 failure must not leave escrow credit");
    }

    function test_success_forwardAndReturnClearsPersistentEscrowCredit() public {
        uint256 amount = 100e6;
        address user = _fundForwardUser(amount);
        address forwardEscrow = _createAndRegisterEscrow(FORWARD_CLIENT);

        _sendVia(user, amount, FORWARD_CLIENT, 7);

        assertEq(underlying.balanceOf(user), 0, "forwarded underlying must be locked");
        assertEq(adapter.reserve(), INITIAL_RESERVE + amount * SCALE, "forwarding must grow reserve");
        assertEq(adapter.balanceOf(forwardEscrow), amount, "forward escrow credit must persist");
        assertEq(adapter.totalSupply(), amount, "persistent credit must be represented in supply");

        string memory returningDenom = string.concat("transfer/", FORWARD_COUNTERPARTY, "/", DENOM_PATH);
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(
            _recvCallbackForDenom(user, amount, 8, returningDenom, FORWARD_CLIENT, FORWARD_COUNTERPARTY)
        );

        assertEq(underlying.balanceOf(user), amount * SCALE, "return must release underlying");
        assertEq(adapter.reserve(), INITIAL_RESERVE, "return must restore reserve");
        assertEq(adapter.balanceOf(forwardEscrow), 0, "return must clear forward escrow credit");
        assertEq(adapter.totalSupply(), 0, "return must clear adapter supply");
    }

    function test_success_forwardErrorAckClearsPersistentEscrowCredit() public {
        uint256 amount = 100e6;
        address user = _fundForwardUser(amount);
        address forwardEscrow = _createAndRegisterEscrow(FORWARD_CLIENT);

        _sendVia(user, amount, FORWARD_CLIENT, 7);
        assertEq(adapter.balanceOf(forwardEscrow), amount, "forward escrow credit must persist");

        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(
            IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                sourceClient: FORWARD_CLIENT,
                destinationClient: FORWARD_COUNTERPARTY,
                sequence: 7,
                payload: _sendPayload(user, amount),
                acknowledgement: ICS24Host.UNIVERSAL_ERROR_ACK,
                relayer: makeAddr("forwardRelayer")
            })
        );

        assertEq(underlying.balanceOf(user), amount * SCALE, "forward refund must release underlying");
        assertEq(adapter.reserve(), INITIAL_RESERVE, "forward refund must restore reserve");
        assertEq(adapter.balanceOf(forwardEscrow), 0, "forward refund must clear escrow credit");
        assertEq(adapter.totalSupply(), 0, "forward refund must clear adapter supply");
    }

    // ==================== refunds ====================

    function test_success_refundOnErrorAck() public {
        address user = makeAddr("user");
        uint256 amount = 250e6;

        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(user, amount, 1));
        _sendBack(user, amount);
        assertEq(underlying.balanceOf(user), 0, "underlying should be locked");

        IIBCAppCallbacks.OnAcknowledgementPacketCallback memory ackMsg = IIBCAppCallbacks.OnAcknowledgementPacketCallback({
            sourceClient: CLIENT,
            destinationClient: COUNTERPARTY,
            sequence: 1,
            payload: _sendPayload(user, amount),
            acknowledgement: ICS24Host.UNIVERSAL_ERROR_ACK,
            relayer: makeAddr("relayer")
        });

        vm.prank(ics26);
        ics20Transfer.onAcknowledgementPacket(ackMsg);

        assertEq(underlying.balanceOf(user), amount * SCALE, "refund did not return the underlying");
        assertEq(adapter.reserve(), INITIAL_RESERVE - amount * SCALE, "reserve not drawn for the refund");
    }

    function test_success_refundOnTimeout() public {
        address user = makeAddr("user");
        uint256 amount = 250e6;

        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(user, amount, 1));
        _sendBack(user, amount);

        IIBCAppCallbacks.OnTimeoutPacketCallback memory timeoutMsg = IIBCAppCallbacks.OnTimeoutPacketCallback({
            sourceClient: CLIENT,
            destinationClient: COUNTERPARTY,
            sequence: 1,
            payload: _sendPayload(user, amount),
            relayer: makeAddr("relayer")
        });

        vm.prank(ics26);
        ics20Transfer.onTimeoutPacket(timeoutMsg);

        assertEq(underlying.balanceOf(user), amount * SCALE, "timeout refund did not return the underlying");
    }

    // ==================== access control on the ERC20 face ====================

    function test_failure_notTransferable() public {
        address user = makeAddr("user");

        vm.expectRevert(IERC20LockboxAdapterErrors.AdapterNotTransferable.selector);
        vm.prank(user);
        adapter.approve(user, 1);

        // a non-escrow cannot release the reserve
        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotEscrow.selector, user));
        vm.prank(user);
        // forge-lint: disable-next-line(erc20-unchecked-transfer)
        adapter.transfer(user, 1);

        // only the ICS20 contract may mint or pull
        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterUnauthorized.selector, user));
        vm.prank(user);
        adapter.mint(escrow, 1);

        // burn is called by an escrow and always burns that escrow's own credit
        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotEscrow.selector, user));
        vm.prank(user);
        adapter.burn(1);

        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterUnauthorized.selector, user));
        vm.prank(user);
        // forge-lint: disable-next-line(erc20-unchecked-transfer)
        adapter.transferFrom(user, escrow, 1);
    }

    function test_failure_mintToNonEscrow() public {
        address notEscrow = makeAddr("notEscrow");

        vm.expectRevert(abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterNotEscrow.selector, notEscrow));
        vm.prank(address(ics20Transfer));
        adapter.mint(notEscrow, 1);
    }

    function test_success_allowanceOnlyForICS20() public {
        assertEq(adapter.allowance(makeAddr("user"), address(ics20Transfer)), type(uint256).max, "ics20 allowance");
        assertEq(adapter.allowance(makeAddr("user"), makeAddr("other")), 0, "no allowance for anyone else");
    }

    function test_failure_restrictedFunctionsUnauthorized() public {
        address user = makeAddr("user");

        vm.startPrank(user);
        vm.expectRevert();
        adapter.addEscrow(user);
        vm.expectRevert();
        adapter.setLowReserveThreshold(1);
        vm.stopPrank();
    }

    function test_failure_legacyOperatorEntryPointsAreAbsent() public {
        _assertSelectorAbsent(abi.encodeWithSignature("deposit(uint256)", 1));
        _assertSelectorAbsent(abi.encodeWithSignature("requestWithdrawal(address,uint256)", treasury, 1));
        _assertSelectorAbsent(abi.encodeWithSignature("executeWithdrawal()"));
        _assertSelectorAbsent(abi.encodeWithSignature("cancelWithdrawal()"));
        _assertSelectorAbsent(abi.encodeWithSignature("withdrawable()"));
        _assertSelectorAbsent(abi.encodeWithSignature("pendingWithdrawal()"));
        _assertSelectorAbsent(abi.encodeWithSignature("withdrawalDelay()"));
        _assertSelectorAbsent(abi.encodeWithSignature("totalDeposited()"));
        _assertSelectorAbsent(abi.encodeWithSignature("totalWithdrawn()"));
        _assertSelectorAbsent(abi.encodeWithSignature("totalLockedIn()"));
        _assertSelectorAbsent(abi.encodeWithSignature("totalReleased()"));
    }

    // ==================== direct reserve funding ====================

    function test_success_directTopUpIncreasesCapacity() public {
        address donor = makeAddr("donor");
        vm.prank(treasury);
        assertTrue(underlying.transfer(donor, 1000e18));

        vm.prank(donor);
        assertTrue(underlying.transfer(address(adapter), 1000e18));

        assertEq(adapter.reserve(), INITIAL_RESERVE + 1000e18, "reserve not topped up");
        assertEq(adapter.capacity(), (INITIAL_RESERVE + 1000e18) / SCALE, "capacity not increased");
    }

    function test_success_capacityExcludesSubScaleReserveDust() public {
        uint256 capacityBefore = adapter.capacity();

        vm.startPrank(treasury);
        assertTrue(underlying.transfer(address(adapter), SCALE - 1));
        assertEq(adapter.capacity(), capacityBefore, "sub-scale residue must not add capacity");

        assertTrue(underlying.transfer(address(adapter), 1));
        vm.stopPrank();

        assertEq(adapter.capacity(), capacityBefore + 1, "a whole denom unit must add capacity");
    }

    // ==================== rate limiting ====================

    function test_success_rateLimitAppliesToAdapter() public {
        // rate limits are keyed on the registered token and denominated in denom units
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, address(this), 0);
        Escrow(escrow).setRateLimit(address(adapter), 100e6);

        address receiver = makeAddr("receiver");
        vm.expectRevert(abi.encodeWithSelector(IRateLimitErrors.RateLimitExceeded.selector, 100e6, 101e6));
        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, 101e6, 1));

        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(receiver, 100e6, 2));
        assertEq(underlying.balanceOf(receiver), 100e6 * SCALE, "receiver should be paid up to the limit");
    }

    function test_success_errorAckRestoresExactRateLimitCredit() public {
        _assertRefundRestoresRateLimitCredit(false);
    }

    function test_success_timeoutRestoresExactRateLimitCredit() public {
        _assertRefundRestoresRateLimitCredit(true);
    }

    // ==================== helpers ====================

    function _assertSelectorAbsent(bytes memory callData) private {
        (bool success, bytes memory returnData) = address(adapter).call(callData);
        assertFalse(success, "legacy operator selector must not be callable");
        assertEq(returnData.length, 0, "legacy operator selector must not exist");
    }

    /// @notice Sends `amount` (denom units) of the adapter denom back to Cosmos on behalf of `user`.
    function _sendBack(address user, uint256 amount) private {
        _sendVia(user, amount, CLIENT, 1);
    }

    function _sendVia(address user, uint256 amount, string memory sourceClient, uint64 sequence) private {
        vm.mockCall(ics26, IICS26Router.sendPacket.selector, abi.encode(sequence));
        vm.startPrank(user);
        underlying.approve(address(adapter), amount * SCALE);
        ics20Transfer.sendTransfer(_sendMsg(amount, sourceClient));
        vm.stopPrank();
        vm.clearMockedCalls();
    }

    function _sendMsg(uint256 amount) private view returns (IICS20TransferMsgs.SendTransferMsg memory) {
        return _sendMsg(amount, CLIENT);
    }

    function _sendMsg(
        uint256 amount,
        string memory sourceClient
    )
        private
        view
        returns (IICS20TransferMsgs.SendTransferMsg memory)
    {
        return IICS20TransferMsgs.SendTransferMsg({
            denom: address(adapter),
            amount: amount,
            receiver: "cosmos1receiver",
            sourceClient: sourceClient,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            timeoutTimestamp: uint64(block.timestamp + 1 hours),
            memo: ""
        });
    }

    function _fundForwardUser(uint256 amount) private returns (address user) {
        user = makeAddr("forwardUser");
        vm.prank(treasury);
        assertTrue(underlying.transfer(user, amount * SCALE));
    }

    function _createAndRegisterEscrow(string memory clientId) private returns (address newEscrow) {
        newEscrow = ics20Transfer.createEscrow(clientId);
        adapter.addEscrow(newEscrow);
    }

    function _assertRefundRestoresRateLimitCredit(bool timeout) private {
        uint256 receivedAmount = 500e6;
        uint256 sentAmount = 200e6;
        uint64 sequence = 11;
        address user = makeAddr(timeout ? "timeoutRateUser" : "ackRateUser");

        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, address(this), 0);
        Escrow(escrow).setRateLimit(address(adapter), 1000e6);

        vm.prank(ics26);
        ics20Transfer.onRecvPacket(_recvCallback(user, receivedAmount, 1));
        assertEq(Escrow(escrow).getDailyUsage(address(adapter)), receivedAmount, "receive usage mismatch");

        _sendVia(user, sentAmount, CLIENT, sequence);
        assertEq(
            Escrow(escrow).getDailyUsage(address(adapter)), receivedAmount - sentAmount, "send must remove exact usage"
        );

        if (timeout) {
            vm.prank(ics26);
            ics20Transfer.onTimeoutPacket(
                IIBCAppCallbacks.OnTimeoutPacketCallback({
                    sourceClient: CLIENT,
                    destinationClient: COUNTERPARTY,
                    sequence: sequence,
                    payload: _sendPayload(user, sentAmount),
                    relayer: makeAddr("timeoutRateRelayer")
                })
            );
        } else {
            vm.prank(ics26);
            ics20Transfer.onAcknowledgementPacket(
                IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                    sourceClient: CLIENT,
                    destinationClient: COUNTERPARTY,
                    sequence: sequence,
                    payload: _sendPayload(user, sentAmount),
                    acknowledgement: ICS24Host.UNIVERSAL_ERROR_ACK,
                    relayer: makeAddr("ackRateRelayer")
                })
            );
        }

        assertEq(Escrow(escrow).getDailyUsage(address(adapter)), receivedAmount, "refund usage mismatch");
        assertEq(underlying.balanceOf(user), receivedAmount * SCALE, "refund underlying mismatch");
        assertEq(adapter.reserve(), INITIAL_RESERVE - receivedAmount * SCALE, "refund reserve mismatch");
    }

    /// @notice The payload ICS20Transfer produces when the adapter denom leaves Ethereum.
    function _sendPayload(address sender, uint256 amount) private pure returns (IICS26RouterMsgs.Payload memory) {
        return IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: DENOM_PATH,
                    amount: amount,
                    sender: Strings.toHexString(sender),
                    receiver: "cosmos1receiver",
                    memo: ""
                })
            )
        });
    }

    /// @notice A packet carrying the Cosmos-native base denom into Ethereum.
    function _recvCallback(
        address receiver,
        uint256 amount,
        uint64 sequence
    )
        private
        pure
        returns (IIBCAppCallbacks.OnRecvPacketCallback memory)
    {
        return _recvCallbackForDenom(receiver, amount, sequence, BASE_DENOM, CLIENT, COUNTERPARTY);
    }

    function _recvCallbackForDenom(
        address receiver,
        uint256 amount,
        uint64 sequence,
        string memory denom,
        string memory destinationClient
    )
        private
        pure
        returns (IIBCAppCallbacks.OnRecvPacketCallback memory)
    {
        return _recvCallbackForDenom(receiver, amount, sequence, denom, destinationClient, COUNTERPARTY);
    }

    function _recvCallbackForDenom(
        address receiver,
        uint256 amount,
        uint64 sequence,
        string memory denom,
        string memory destinationClient,
        string memory sourceClient
    )
        private
        pure
        returns (IIBCAppCallbacks.OnRecvPacketCallback memory)
    {
        return IIBCAppCallbacks.OnRecvPacketCallback({
            sourceClient: sourceClient,
            destinationClient: destinationClient,
            sequence: sequence,
            payload: IICS26RouterMsgs.Payload({
                sourcePort: ICS20Lib.DEFAULT_PORT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                version: ICS20Lib.ICS20_VERSION,
                encoding: ICS20Lib.ICS20_ENCODING,
                value: abi.encode(
                    IICS20TransferMsgs.FungibleTokenPacketData({
                        denom: denom,
                        amount: amount,
                        sender: "cosmos1sender",
                        receiver: Strings.toHexString(receiver),
                        memo: ""
                    })
                )
            }),
            relayer: address(0)
        });
    }
}
