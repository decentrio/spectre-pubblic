// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,gas-custom-errors

/// @notice Deploys and registers an `ERC20LockboxAdapter` for one IBC denom, following the order in
/// docs/ERC20_LOCKBOX_ADAPTER.md#deployment. Every irreversible step (`setCustomERC20`, funding, activation) comes
/// after every check, and funding comes last.
///
/// Environment:
///   ICS20_TRANSFER        ICS20Transfer proxy
///   UNDERLYING            the already-deployed ERC20 to lock and release
///   CLIENT_ID             router client id on THIS chain the denom arrives on (the `destinationClient`)
///   BASE_DENOM            the denom exactly as the counterparty puts it in the packet, e.g. `uatom` or, for a
///                         voucher on the counterparty, its full trace such as `transfer/channel-536/uusdc`
///   DENOM_DECIMALS        the Cosmos exponent (6 for `uatom`); Ethereum cannot verify it, check the counterparty
///                         bank metadata before running
///   UNDERLYING_DECIMALS   optional; read from `decimals()` when unset
///   RATE_LIMIT            optional; escrow rate limit for the adapter in denom units; required when the escrow
///                         launch gate is on or ACTIVATE_ESCROW is set
///   LOW_RESERVE_THRESHOLD optional; capacity in denom units below which the adapter emits `ReserveLow`
///   RESERVE_AMOUNT        optional; underlying units moved from the broadcaster to the adapter, after every check;
///                         must be a multiple of the adapter's `scale`
///   ACTIVATE_ESCROW       optional; call `activateEscrow` last (needed for a client behind the launch gate)
///   ALLOW_ACTIVE_CLIENT   optional; set only after pausing packet processing for a client that is already active
///
/// The broadcaster must hold, with zero execution delay: ADMIN_ROLE (adapter setup, `createEscrow`, `activateEscrow`),
/// ERC20_CUSTOMIZER_ROLE (`setCustomERC20`) and, when RATE_LIMIT is set, RATE_LIMITER_ROLE. The script checks this up
/// front and reverts before sending anything. A deployment whose AccessManager sits behind a timelock must run the same
/// calls through governance instead.
import { stdJson } from "forge-std/StdJson.sol";
import { Script } from "forge-std/Script.sol";
import { IERC20 } from "@openzeppelin-contracts/token/ERC20/IERC20.sol";
import { SafeERC20 } from "@openzeppelin-contracts/token/ERC20/utils/SafeERC20.sol";
import { IERC20Metadata } from "@openzeppelin-contracts/token/ERC20/extensions/IERC20Metadata.sol";
import { IAccessManaged } from "@openzeppelin-contracts/access/manager/IAccessManaged.sol";
import { IAccessManager } from "@openzeppelin-contracts/access/manager/IAccessManager.sol";
import { SafeCast } from "@openzeppelin-contracts/utils/math/SafeCast.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";

import { ERC20LockboxAdapter } from "contracts/apps/ics20/ERC20LockboxAdapter.sol";
import { IICS20Transfer, IICS20TransferAccessControlled } from "contracts/apps/ics20/interfaces/IICS20Transfer.sol";
import { IICS20Errors } from "contracts/apps/ics20/errors/IICS20Errors.sol";
import { IRateLimit } from "contracts/apps/ics20/interfaces/IRateLimit.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { IBCIdentifiers } from "contracts/shared/access/IBCIdentifiers.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";

contract ERC20LockboxAdapterDeploy is Script {
    using stdJson for string;
    using SafeERC20 for IERC20;

    /// @dev `10 ** exponent` must fit in a uint256; mirrors the adapter constructor.
    uint256 internal constant MAX_SCALE_EXPONENT = 77;

    struct Config {
        address ics20;
        address underlying;
        string clientId;
        string baseDenom;
        uint8 denomDecimals;
        uint8 underlyingDecimals;
        uint256 rateLimit;
        uint256 lowReserveThreshold;
        uint256 reserveAmount;
        bool activateEscrow;
        bool allowActiveClient;
    }

    /// @notice Entry point for `forge script`: reads the configuration from the environment.
    function run() public returns (string memory) {
        return execute(_loadConfig());
    }

    /// @notice Runs the deployment for an explicit configuration. Split from `run` so tests do not depend on
    /// process-wide environment variables.
    function execute(Config memory cfg) public returns (string memory) {
        string memory denom = _denomPath(cfg.clientId, cfg.baseDenom);
        IICS20Transfer ics20 = IICS20Transfer(cfg.ics20);

        _validate(cfg, denom);

        vm.startBroadcast();
        (, address broadcaster,) = vm.readCallers();
        IAccessManager authority = IAccessManager(IAccessManaged(cfg.ics20).authority());

        // 1. The client's escrow must exist before the adapter can be bound to it. Creating it is harmless
        //    and idempotent per client id; everything irreversible happens later.
        address escrow = ics20.getEscrow(cfg.clientId);
        _requireAuthority(authority, cfg, broadcaster, escrow == address(0));
        if (escrow == address(0)) {
            escrow = IICS20TransferAccessControlled(cfg.ics20).createEscrow(cfg.clientId);
        }
        require(escrow != address(0), "escrow was not created");

        // 2. Deploy and bind. The adapter is a fresh AccessManager target, so its restricted setters
        //    default to ADMIN_ROLE, which the authority check above required.
        ERC20LockboxAdapter adapter = new ERC20LockboxAdapter(
            cfg.ics20, cfg.underlying, cfg.denomDecimals, cfg.underlyingDecimals, address(authority)
        );
        adapter.addEscrow(escrow);
        if (cfg.lowReserveThreshold != 0) {
            adapter.setLowReserveThreshold(cfg.lowReserveThreshold);
        }
        if (cfg.rateLimit != 0) {
            IRateLimit(escrow).setRateLimit(address(adapter), cfg.rateLimit);
        }

        // 3. Verify every value before the one-shot registration.
        require(adapter.ics20() == cfg.ics20, "adapter ics20 mismatch");
        require(address(adapter.underlying()) == cfg.underlying, "adapter underlying mismatch");
        require(adapter.decimals() == cfg.denomDecimals, "adapter denom decimals mismatch");
        require(adapter.scale() == 10 ** (uint256(cfg.underlyingDecimals) - uint256(cfg.denomDecimals)), "bad scale");
        require(adapter.isEscrow(escrow), "escrow not bound");
        require(IRateLimit(escrow).getRateLimit(address(adapter)) == cfg.rateLimit, "rate limit not installed");

        // 4. Register. This cannot be undone, and the denom path is the only thing the core looks up by.
        IICS20TransferAccessControlled(cfg.ics20).setCustomERC20(denom, address(adapter));
        require(ics20.ibcERC20Contract(denom) == address(adapter), "denom -> adapter mapping missing");
        require(
            keccak256(bytes(ics20.ibcERC20Denom(address(adapter)))) == keccak256(bytes(denom)),
            "adapter -> denom mapping missing"
        );

        // 5. Fund last, by a plain transfer, and verify the exact balance (rejects fee-on-transfer tokens).
        if (cfg.reserveAmount != 0) {
            uint256 before = adapter.reserve();
            // safeTransfer: tokens such as USDT succeed without returning a bool, which a typed call cannot decode.
            IERC20(cfg.underlying).safeTransfer(address(adapter), cfg.reserveAmount);
            require(adapter.reserve() == before + cfg.reserveAmount, "reserve funded with a different amount");
        }

        // 6. Activate only now that every invariant, mapping and balance has been verified.
        if (cfg.activateEscrow) {
            address[] memory tokens = new address[](1);
            tokens[0] = address(adapter);
            IICS20TransferAccessControlled(cfg.ics20).activateEscrow(cfg.clientId, tokens);
            require(ics20.isEscrowActive(cfg.clientId), "escrow not active");
        }
        vm.stopBroadcast();

        string memory json = "json";
        json.serialize("adapter", vm.toString(address(adapter)));
        json.serialize("underlying", vm.toString(cfg.underlying));
        json.serialize("escrow", vm.toString(escrow));
        json.serialize("denom", denom);
        json.serialize("scale", vm.toString(adapter.scale()));
        json.serialize("reserve", vm.toString(adapter.reserve()));
        return json.serialize("capacity", vm.toString(adapter.capacity()));
    }

    function _loadConfig() internal view returns (Config memory cfg) {
        cfg.ics20 = vm.envAddress("ICS20_TRANSFER");
        cfg.underlying = vm.envAddress("UNDERLYING");
        cfg.clientId = vm.envString("CLIENT_ID");
        cfg.baseDenom = vm.envString("BASE_DENOM");

        cfg.denomDecimals = SafeCast.toUint8(vm.envUint("DENOM_DECIMALS"));

        cfg.underlyingDecimals =
            _resolveUnderlyingDecimals(cfg.underlying, vm.envOr("UNDERLYING_DECIMALS", type(uint256).max));

        cfg.rateLimit = vm.envOr("RATE_LIMIT", uint256(0));
        cfg.lowReserveThreshold = vm.envOr("LOW_RESERVE_THRESHOLD", uint256(0));
        cfg.reserveAmount = vm.envOr("RESERVE_AMOUNT", uint256(0));
        cfg.activateEscrow = vm.envOr("ACTIVATE_ESCROW", false);
        cfg.allowActiveClient = vm.envOr("ALLOW_ACTIVE_CLIENT", false);
    }

    /// @dev `declared == type(uint256).max` means "not set": read the token's own `decimals()`. The adapter
    /// constructor still cross-checks a declared value against the token when the token exposes it.
    function _resolveUnderlyingDecimals(address underlying, uint256 declared) internal view returns (uint8) {
        if (declared != type(uint256).max) {
            return SafeCast.toUint8(declared);
        }
        require(underlying.code.length != 0, "underlying has no code");
        try IERC20Metadata(underlying).decimals() returns (uint8 reported) {
            return reported;
        } catch {
            revert("underlying does not expose decimals(): set UNDERLYING_DECIMALS");
        }
    }

    /// @dev Built exactly as `ICS20Transfer.onRecvPacket` builds the destination denom
    /// (`getDenomPrefix(destPort, destinationClient) ++ denom`), so the registered key cannot drift from the one the
    /// core looks up. A wrong key leaves the adapter unused and a default voucher gets deployed instead.
    function _denomPath(string memory clientId, string memory baseDenom) internal pure returns (string memory) {
        return string(abi.encodePacked(ICS20Lib.DEFAULT_PORT_ID, "/", clientId, "/", baseDenom));
    }

    function _validate(Config memory cfg, string memory denom) internal view {
        require(cfg.ics20.code.length != 0, "ICS20_TRANSFER has no code");
        require(cfg.underlying.code.length != 0, "UNDERLYING has no code");
        require(bytes(cfg.clientId).length != 0, "empty CLIENT_ID");
        // Same rule as the production launch config. The default `client-<seq>` namespace is handed out by the
        // permissionless `addClient`, so an adapter bound to such an id could be inherited by whoever registers it
        // first. `/` would make `transfer/<client>/<denom>` ambiguous, and an id the router cannot accept would leave
        // a funded adapter that never receives a packet. This runs before anything is sent, reserve funding included.
        require(
            IBCIdentifiers.validateCustomIBCIdentifier(bytes(cfg.clientId)), "CLIENT_ID must be a custom IBC identifier"
        );
        require(bytes(cfg.baseDenom).length != 0, "empty BASE_DENOM");
        require(cfg.underlyingDecimals >= cfg.denomDecimals, "underlying decimals below denom decimals");
        uint256 exponent = uint256(cfg.underlyingDecimals) - uint256(cfg.denomDecimals);
        require(exponent <= MAX_SCALE_EXPONENT, "decimals gap too large");
        require(cfg.reserveAmount % (10 ** exponent) == 0, "RESERVE_AMOUNT is not a multiple of scale");

        IICS20Transfer ics20 = IICS20Transfer(cfg.ics20);
        // `ibcERC20Contract` reverts with `ICS20DenomNotFound` for an unregistered denom rather than returning zero.
        // Any other revert is not "free": surface it instead of treating the denom as available.
        try ics20.ibcERC20Contract(denom) returns (address) {
            revert("denom already has a registered token");
        } catch (bytes memory reason) {
            require(
                reason.length >= 4 && bytes4(reason) == IICS20Errors.ICS20DenomNotFound.selector,
                "unexpected revert while checking the denom"
            );
        }

        // A client that is already live may have taken traffic: an inbound packet that landed before the mapping
        // would have created the default voucher, and a half-installed adapter would bounce the next ones.
        if (ics20.isEscrowActive(cfg.clientId)) {
            require(cfg.allowActiveClient, "client is active: pause packet processing, then set ALLOW_ACTIVE_CLIENT");
        }
        if (ics20.requiresPrecreatedEscrows() || cfg.activateEscrow) {
            require(cfg.rateLimit != 0, "RATE_LIMIT is required by the launch gate / ACTIVATE_ESCROW");
        }
    }

    function _requireAuthority(
        IAccessManager authority,
        Config memory cfg,
        address broadcaster,
        bool needsCreateEscrow
    )
        internal
        view
    {
        (bool admin, uint32 adminDelay) = authority.hasRole(IBCRolesLib.ADMIN_ROLE, broadcaster);
        require(admin && adminDelay == 0, "broadcaster needs ADMIN_ROLE with zero delay");
        if (needsCreateEscrow) {
            _requireCallable(authority, broadcaster, cfg.ics20, IICS20TransferAccessControlled.createEscrow.selector);
        }
        _requireCallable(authority, broadcaster, cfg.ics20, IICS20TransferAccessControlled.setCustomERC20.selector);
        if (cfg.activateEscrow) {
            _requireCallable(authority, broadcaster, cfg.ics20, IICS20TransferAccessControlled.activateEscrow.selector);
        }
        if (cfg.rateLimit != 0) {
            // `setRateLimit` checks the global role itself rather than using `restricted`.
            (bool limiter, uint32 limiterDelay) = authority.hasRole(IBCRolesLib.RATE_LIMITER_ROLE, broadcaster);
            require(limiter && limiterDelay == 0, "broadcaster needs RATE_LIMITER_ROLE with zero delay");
        }
        if (cfg.reserveAmount != 0) {
            require(
                IERC20(cfg.underlying).balanceOf(broadcaster) >= cfg.reserveAmount,
                "broadcaster holds less underlying than RESERVE_AMOUNT"
            );
        }
    }

    function _requireCallable(IAccessManager authority, address caller, address target, bytes4 selector) internal view {
        (bool immediate,) = authority.canCall(caller, target, selector);
        require(
            immediate,
            string.concat("broadcaster cannot call ", Strings.toHexString(uint256(uint32(selector)), 4), " immediately")
        );
    }
}
