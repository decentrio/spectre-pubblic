// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length

import { Test } from "forge-std/Test.sol";
import { stdJson } from "forge-std/StdJson.sol";

import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";

import { ERC20LockboxAdapterDeploy } from "scripts/ERC20LockboxAdapterDeploy.s.sol";
import { ERC20LockboxAdapter } from "contracts/apps/ics20/ERC20LockboxAdapter.sol";
import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { IICS20TransferAccessControlled } from "contracts/apps/ics20/interfaces/IICS20Transfer.sol";
import { IRateLimit } from "contracts/apps/ics20/interfaces/IRateLimit.sol";
import { Escrow } from "contracts/apps/ics20/Escrow.sol";
import { IBCERC20 } from "contracts/apps/ics20/IBCERC20.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IERC20LockboxAdapterErrors } from "contracts/apps/ics20/errors/IERC20LockboxAdapterErrors.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { IntegrationEnv } from "test/utils/IntegrationEnv.sol";
import { FixedSupplyERC20, ToggleFeeERC20 } from "test/apps/ics20/ERC20LockboxAdapterTest.t.sol";

/// @dev USDT-style token: `transfer`/`transferFrom` succeed but return no data, so a typed `bool` return cannot be
/// decoded. The adapter handles such tokens through SafeERC20; the deploy script must too.
contract NoReturnERC20 {
    uint8 public constant decimals = 18;
    mapping(address account => uint256 balance) public balanceOf;
    mapping(address owner => mapping(address spender => uint256 amount)) public allowance;

    constructor(uint256 supply) {
        balanceOf[msg.sender] = supply;
    }

    function transfer(address to, uint256 amount) external {
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
    }

    function transferFrom(address from, address to, uint256 amount) external {
        allowance[from][msg.sender] -= amount;
        balanceOf[from] -= amount;
        balanceOf[to] += amount;
    }

    function approve(address spender, uint256 amount) external {
        allowance[msg.sender][spender] = amount;
    }
}

contract ERC20LockboxAdapterDeployHarness is ERC20LockboxAdapterDeploy {
    function resolveUnderlyingDecimals(address underlying, uint256 declared) external view returns (uint8) {
        return _resolveUnderlyingDecimals(underlying, declared);
    }

    function denomPath(string memory clientId, string memory baseDenom) external pure returns (string memory) {
        return _denomPath(clientId, baseDenom);
    }
}

/// @dev The script is driven through `execute(Config)` rather than `run()`: `run()` only adds environment parsing,
/// and `vm.setEnv` is process-wide, which would make parallel tests race.
contract ERC20LockboxAdapterDeployTest is Test {
    using stdJson for string;

    ERC20LockboxAdapterDeployHarness internal script;
    ICS20Transfer internal ics20Transfer;
    AccessManager internal accessManager;
    FixedSupplyERC20 internal underlying;

    IntegrationEnv internal env = new IntegrationEnv();
    address internal ics26 = makeAddr("ics26router");

    string internal constant CLIENT = "eth-main";
    string internal constant BASE_DENOM = "uatom";
    string internal constant DENOM_PATH = "transfer/eth-main/uatom";
    uint256 internal constant SCALE = 1e12;
    uint256 internal constant RESERVE = 1000e18;
    address internal constant BROADCASTER = DEFAULT_SENDER;

    function setUp() public {
        // Calls made after `vm.startBroadcast()` come from forge's default sender, not from this contract. This
        // contract stays the AccessManager admin and sets the scene; the broadcaster gets only the roles a real
        // deployer would need.
        accessManager = new AccessManager(address(this));
        accessManager.grantRole(IBCRolesLib.ADMIN_ROLE, BROADCASTER, 0);
        ERC1967Proxy transferProxy = new ERC1967Proxy(
            address(new ICS20Transfer()),
            abi.encodeCall(
                ICS20Transfer.initialize,
                (ics26, address(new Escrow()), address(new IBCERC20()), env.permit2(), address(accessManager))
            )
        );
        ics20Transfer = ICS20Transfer(address(transferProxy));
        vm.prank(BROADCASTER);
        underlying = new FixedSupplyERC20(1_000_000e18);
        script = new ERC20LockboxAdapterDeployHarness();
    }

    function _cfg() internal view returns (ERC20LockboxAdapterDeploy.Config memory cfg) {
        cfg.ics20 = address(ics20Transfer);
        cfg.underlying = address(underlying);
        cfg.clientId = CLIENT;
        cfg.baseDenom = BASE_DENOM;
        cfg.denomDecimals = 6;
        cfg.underlyingDecimals = 18;
    }

    function _adapterOf(string memory json) internal pure returns (ERC20LockboxAdapter) {
        return ERC20LockboxAdapter(json.readAddress(".adapter"));
    }

    // ==================== success ====================

    function test_success_deployWithoutLaunchGate() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.reserveAmount = RESERVE;
        cfg.lowReserveThreshold = 100_000_000;
        cfg.rateLimit = 50_000_000;
        // ADMIN_ROLE does not imply RATE_LIMITER_ROLE.
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, BROADCASTER, 0);

        string memory json = script.execute(cfg);
        ERC20LockboxAdapter adapter = _adapterOf(json);
        address escrow = ics20Transfer.getEscrow(CLIENT);

        assertEq(json.readString(".denom"), DENOM_PATH, "denom path");
        assertEq(json.readAddress(".escrow"), escrow, "escrow in receipt");
        assertEq(ics20Transfer.ibcERC20Contract(DENOM_PATH), address(adapter), "denom -> adapter");
        assertEq(ics20Transfer.ibcERC20Denom(address(adapter)), DENOM_PATH, "adapter -> denom");
        assertTrue(adapter.isEscrow(escrow), "escrow bound");
        assertEq(adapter.scale(), SCALE, "scale");
        assertEq(adapter.decimals(), 6, "denom decimals");
        assertEq(adapter.lowReserveThreshold(), 100_000_000, "threshold");
        assertEq(IRateLimit(escrow).getRateLimit(address(adapter)), 50_000_000, "rate limit");
        assertEq(adapter.reserve(), RESERVE, "reserve");
        assertEq(adapter.capacity(), RESERVE / SCALE, "capacity");
        assertEq(json.readUint(".reserve"), RESERVE, "reserve in receipt");
        assertFalse(ics20Transfer.isEscrowActive(CLIENT), "activation is opt-in");
    }

    function test_success_deployBehindLaunchGateAndActivate() public {
        ics20Transfer.enableEscrowLaunchGate();
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, BROADCASTER, 0);
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.rateLimit = 50_000_000;
        cfg.activateEscrow = true;
        cfg.reserveAmount = RESERVE;

        assertFalse(ics20Transfer.isEscrowActive(CLIENT), "inactive before");
        script.execute(cfg);
        assertTrue(ics20Transfer.isEscrowActive(CLIENT), "active after");
    }

    function test_success_fundsReserveWithTokenThatReturnsNothing() public {
        vm.prank(BROADCASTER);
        NoReturnERC20 token = new NoReturnERC20(1_000_000e18);
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.underlying = address(token);
        cfg.reserveAmount = RESERVE;

        ERC20LockboxAdapter adapter = _adapterOf(script.execute(cfg));

        assertEq(adapter.reserve(), RESERVE, "reserve funded despite the missing bool return");
        assertEq(token.balanceOf(address(adapter)), RESERVE, "tokens actually moved");
    }

    function test_success_reusesExistingEscrow() public {
        address escrow = ics20Transfer.createEscrow(CLIENT);
        string memory json = script.execute(_cfg());
        assertEq(json.readAddress(".escrow"), escrow, "must bind the pre-created escrow");
    }

    function test_success_denomPathMatchesCoreConstruction() public view {
        // `onRecvPacket` registers a counterparty denom as `<destPort>/<destinationClient>/<denom>`; a voucher on the
        // counterparty arrives with its own trace already inside the denom.
        assertEq(script.denomPath("eth-main", "uatom"), "transfer/eth-main/uatom");
        assertEq(
            script.denomPath("eth-main", "transfer/channel-536/uusdc"), "transfer/eth-main/transfer/channel-536/uusdc"
        );
    }

    function test_success_resolveUnderlyingDecimals() public view {
        assertEq(script.resolveUnderlyingDecimals(address(underlying), type(uint256).max), 18, "read from token");
        assertEq(script.resolveUnderlyingDecimals(address(underlying), 8), 8, "declared value wins");
    }

    // ==================== failure: configuration ====================

    function test_failure_resolveUnderlyingDecimalsWithoutDecimals() public {
        address noDecimals = address(new Escrow());
        vm.expectRevert("underlying does not expose decimals(): set UNDERLYING_DECIMALS");
        script.resolveUnderlyingDecimals(noDecimals, type(uint256).max);

        vm.expectRevert("underlying has no code");
        script.resolveUnderlyingDecimals(makeAddr("eoa"), type(uint256).max);
    }

    function test_failure_denomAlreadyRegistered() public {
        script.execute(_cfg());
        vm.expectRevert("denom already has a registered token");
        script.execute(_cfg());
    }

    function test_failure_reserveNotMultipleOfScale() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.reserveAmount = RESERVE + 1;
        vm.expectRevert("RESERVE_AMOUNT is not a multiple of scale");
        script.execute(cfg);
        assertEq(ics20Transfer.getEscrow(CLIENT), address(0), "nothing may be created on a failed preflight");
    }

    function test_failure_launchGateRequiresRateLimit() public {
        ics20Transfer.enableEscrowLaunchGate();
        vm.expectRevert("RATE_LIMIT is required by the launch gate / ACTIVATE_ESCROW");
        script.execute(_cfg());
    }

    function test_failure_activateRequiresRateLimit() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.activateEscrow = true;
        vm.expectRevert("RATE_LIMIT is required by the launch gate / ACTIVATE_ESCROW");
        script.execute(cfg);
    }

    function test_failure_underlyingDecimalsBelowDenomDecimals() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.denomDecimals = 18;
        cfg.underlyingDecimals = 6;
        vm.expectRevert("underlying decimals below denom decimals");
        script.execute(cfg);
    }

    function test_failure_declaredUnderlyingDecimalsMismatch() public {
        // The adapter constructor cross-checks the declared value against the token's own decimals().
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.underlyingDecimals = 8;
        vm.expectRevert(
            abi.encodeWithSelector(IERC20LockboxAdapterErrors.AdapterUnderlyingDecimalsMismatch.selector, 18, 8)
        );
        script.execute(cfg);
    }

    function test_failure_invalidClientIdRejectedBeforeAnyStateChange() public {
        // `client-N` is handed out by the permissionless `addClient`, so a funded adapter bound to it could be
        // inherited by whoever registers that id first; `/` makes the denom path ambiguous; short or odd ids can
        // never be a router client id at all.
        string[6] memory invalid = ["client-1", "channel-0", "a/b", "eth", "eth main", "eth-main/"];
        for (uint256 i = 0; i < invalid.length; ++i) {
            ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
            cfg.clientId = invalid[i];
            cfg.reserveAmount = RESERVE;
            uint256 balanceBefore = underlying.balanceOf(BROADCASTER);

            vm.expectRevert("CLIENT_ID must be a custom IBC identifier");
            script.execute(cfg);

            assertEq(underlying.balanceOf(BROADCASTER), balanceBefore, "no reserve may move");
            assertEq(ics20Transfer.getEscrow(invalid[i]), address(0), "no escrow may be created");
        }
    }

    function test_failure_emptyClientOrDenom() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.clientId = "";
        vm.expectRevert("empty CLIENT_ID");
        script.execute(cfg);

        cfg = _cfg();
        cfg.baseDenom = "";
        vm.expectRevert("empty BASE_DENOM");
        script.execute(cfg);
    }

    function test_failure_activeClientNeedsExplicitAllow() public {
        ics20Transfer.enableEscrowLaunchGate();
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, address(this), 0);
        accessManager.grantRole(IBCRolesLib.RATE_LIMITER_ROLE, BROADCASTER, 0);
        address[] memory tokens = new address[](1);
        tokens[0] = address(underlying);
        address escrow = ics20Transfer.createEscrow(CLIENT);
        IRateLimit(escrow).setRateLimit(tokens[0], 1);
        ics20Transfer.activateEscrow(CLIENT, tokens);

        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.rateLimit = 50_000_000;
        vm.expectRevert("client is active: pause packet processing, then set ALLOW_ACTIVE_CLIENT");
        script.execute(cfg);

        cfg.allowActiveClient = true;
        script.execute(cfg);
        assertTrue(ics20Transfer.ibcERC20Contract(DENOM_PATH) != address(0), "allowed once acknowledged");
    }

    // ==================== failure: authority preflight ====================

    function test_failure_missingCustomizerRoleRevertsBeforeAnyStateChange() public {
        // setCustomERC20 is gated by ERC20_CUSTOMIZER_ROLE, which the broadcaster does not hold.
        bytes4[] memory selectors = new bytes4[](1);
        selectors[0] = IICS20TransferAccessControlled.setCustomERC20.selector;
        accessManager.setTargetFunctionRole(address(ics20Transfer), selectors, IBCRolesLib.ERC20_CUSTOMIZER_ROLE);

        vm.expectRevert(
            bytes(
                string.concat(
                    "broadcaster cannot call ", Strings.toHexString(uint256(uint32(selectors[0])), 4), " immediately"
                )
            )
        );
        script.execute(_cfg());
        assertEq(ics20Transfer.getEscrow(CLIENT), address(0), "no escrow may be created when the preflight fails");
    }

    function test_failure_missingAdminRole() public {
        accessManager.revokeRole(IBCRolesLib.ADMIN_ROLE, BROADCASTER);
        vm.expectRevert("broadcaster needs ADMIN_ROLE with zero delay");
        script.execute(_cfg());
    }

    function test_failure_missingRateLimiterRole() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.rateLimit = 50_000_000;
        vm.expectRevert("broadcaster needs RATE_LIMITER_ROLE with zero delay");
        script.execute(cfg);
    }

    // ==================== failure: funding ====================

    function test_failure_feeOnTransferUnderlyingCannotFundReserve() public {
        vm.prank(BROADCASTER);
        ToggleFeeERC20 feeToken = new ToggleFeeERC20(1_000_000e18);
        feeToken.setFeeEnabled(true);
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.underlying = address(feeToken);
        cfg.reserveAmount = RESERVE;

        vm.expectRevert("reserve funded with a different amount");
        script.execute(cfg);
    }

    function test_failure_broadcasterHoldsTooLittleUnderlying() public {
        ERC20LockboxAdapterDeploy.Config memory cfg = _cfg();
        cfg.reserveAmount = 10_000_000e18;
        vm.expectRevert("broadcaster holds less underlying than RESERVE_AMOUNT");
        script.execute(cfg);
    }
}
