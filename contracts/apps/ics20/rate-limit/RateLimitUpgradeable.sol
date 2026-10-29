// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IRateLimitErrors } from "contracts/apps/ics20/errors/IRateLimitErrors.sol";
import { IRateLimit } from "contracts/apps/ics20/interfaces/IRateLimit.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";

import { IAccessManaged } from "@openzeppelin-contracts/access/manager/IAccessManaged.sol";
import { IAccessManager } from "@openzeppelin-contracts/access/manager/IAccessManager.sol";
import { Math } from "@openzeppelin-contracts/utils/math/Math.sol";
import { AccessManagedUpgradeable } from "@openzeppelin-upgradeable/access/manager/AccessManagedUpgradeable.sol";

/// @title Rate Limit Upgradeable contract
/// @notice This contract is an abstract contract for adding rate limiting to escrow contracts.
/// @dev Rate limits are set per token address by the rate limiter role and are enforced with a rolling window.
/// @dev Rate limits are applied to tokens leaving the escrow contract.
abstract contract RateLimitUpgradeable is IRateLimitErrors, IRateLimit, AccessManagedUpgradeable {
    /// @notice Storage of the RateLimit contract
    /// @dev It's implemented on a custom ERC-7201 namespace to reduce the risk of storage collisions when using with
    /// upgradeable contracts.
    /// @param _rateLimits Mapping of token addresses to their rate limits, 0 means no limit
    /// @param _usage Mapping of token addresses to their current usage
    /// @param _lastUpdate Mapping of token addresses to the timestamp of the last usage update
    struct RateLimitStorage {
        mapping(address token => uint256 limit) _rateLimits;
        mapping(address token => uint256 usage) _usage;
        mapping(address token => uint256 lastUpdate) _lastUpdate;
    }

    /// @notice ERC-7201 slot for the RateLimit storage
    /// @dev keccak256(abi.encode(uint256(keccak256("ibc.storage.RateLimit")) - 1)) & ~bytes32(uint256(0xff))
    bytes32 private constant RATELIMIT_STORAGE_SLOT =
        0xcb05b6cb8e6c87c443cb04d44193d7d46d51c1198725a0ee3478d5baa736c100;

    /// @notice The period for rate limiting
    uint256 private constant RATE_LIMIT_PERIOD = 1 days;

    /// @notice The initializer for the RateLimit contract
    /// @param authority_ The address of the AccessManager contract
    function __RateLimit_init(address authority_) internal onlyInitializing {
        __AccessManaged_init(authority_);
    }

    /// @inheritdoc IRateLimit
    /// @dev DO NOT replace this with the `restricted` modifier. `restricted` authorizes against a
    ///      PER-TARGET function role, and escrows are BeaconProxy instances created per client
    ///      (`ICS20Transfer._createEscrow`). A freshly created escrow therefore has no mapping, so
    ///      `setRateLimit` on it would fall back to `ADMIN_ROLE` and the rate limiter could not
    ///      call it — while `_rateLimits[token] == 0` means "no limit" (see the storage NatSpec
    ///      above), so that escrow would be live and uncapped. Checking the GLOBAL
    ///      `RATE_LIMITER_ROLE` instead makes every escrow configurable the moment it exists,
    ///      whether it was pre-created at deploy time or not. This mirrors the manual per-client
    ///      role check in `ICS02ClientUpgradeable`.
    ///
    ///      A manual check has to re-create by hand the two things the modifier gave for free:
    ///
    ///      1. `isTargetClosed` — the AccessManager kill switch. `restricted` consults it; a bare
    ///         `hasRole` does not, so closing this target would otherwise not disable the function.
    ///      2. `executionDelay == 0` — a delayed role holder is REJECTED rather than allowed
    ///         through. The delay is enforced by `AccessManager.schedule`/`execute`, and this path
    ///         never consumes a scheduled operation, so honouring a non-zero delay here would
    ///         silently ignore it. Only immediate, delay-0 rate limiters are supported.
    ///
    ///      Note that routing through `AccessManager.execute` does NOT work either: it would make
    ///      the manager the `_msgSender()`, and the manager holds no role.
    function setRateLimit(address token, uint256 rateLimit) external {
        IAccessManager manager = IAccessManager(authority());
        require(!manager.isTargetClosed(address(this)), IAccessManaged.AccessManagedUnauthorized(_msgSender()));

        (bool isMember, uint32 executionDelay) = manager.hasRole(IBCRolesLib.RATE_LIMITER_ROLE, _msgSender());
        require(isMember && executionDelay == 0, IAccessManaged.AccessManagedUnauthorized(_msgSender()));

        RateLimitStorage storage $ = _getRateLimitStorage();
        // Usage decays at `limit / period` from its last update. Settling it first keeps the new limit from being
        // applied to the time that has already passed, which would hand back decay that was granted, or take it away.
        if ($._lastUpdate[token] != 0) {
            $._usage[token] = _getCurrentUsage(token);
            $._lastUpdate[token] = block.timestamp;
        }
        $._rateLimits[token] = rateLimit;
    }

    /// @inheritdoc IRateLimit
    function getRateLimit(address token) external view returns (uint256) {
        return _getRateLimitStorage()._rateLimits[token];
    }

    /// @inheritdoc IRateLimit
    function getDailyUsage(address token) external view returns (uint256) {
        return _getCurrentUsage(token);
    }

    /// @notice Checks the rate limit for a token and updates the usage
    /// @param token The token address
    /// @param amount The amount to check against the rate limit
    function _assertAndUpdateRateLimit(address token, uint256 amount) internal {
        RateLimitStorage storage $ = _getRateLimitStorage();

        uint256 rateLimit = $._rateLimits[token];
        if (rateLimit == 0) {
            return;
        }

        uint256 usage = _getCurrentUsage(token) + amount;
        // solhint-disable-next-line gas-strict-inequalities
        require(usage <= rateLimit, RateLimitExceeded(rateLimit, usage));

        $._usage[token] = usage;
        $._lastUpdate[token] = block.timestamp;
    }

    /// @notice Reduces the usage for a token
    /// @dev This function is used in order to track the net usage a token
    /// @param token The token address
    /// @param amount The amount to reduce from the usage
    function _reduceDailyUsage(address token, uint256 amount) internal returns (uint256 usageRemoved) {
        RateLimitStorage storage $ = _getRateLimitStorage();

        uint256 rateLimit = $._rateLimits[token];
        if (rateLimit == 0) {
            return 0;
        }

        uint256 usage = _getCurrentUsage(token);
        if (usage > amount) {
            $._usage[token] = usage - amount;
            usageRemoved = amount;
        } else {
            $._usage[token] = 0;
            usageRemoved = usage;
        }
        $._lastUpdate[token] = block.timestamp;
    }

    /// @notice Increments the usage for a token without checking the rate limit
    /// @dev This function is used to restore usage on refunds, ensuring it doesn't revert.
    ///      `amount` must be the value returned by `_reduceDailyUsage`; in particular, that value is zero
    ///      when rate limiting is disabled. This coupling prevents a refund from creating latent usage
    ///      while the configured limit is zero.
    /// @param token The token address
    /// @param amount The amount to add to the usage
    function _increaseDailyUsageUncapped(address token, uint256 amount) internal {
        if (amount == 0) {
            return;
        }

        RateLimitStorage storage $ = _getRateLimitStorage();

        uint256 usage = _getCurrentUsage(token) + amount;
        $._usage[token] = usage;
        $._lastUpdate[token] = block.timestamp;
    }

    /// @notice Returns the current decayed usage for a token in the rolling window
    /// @param token The token address
    /// @return The current usage (decayed from last update)
    function _getCurrentUsage(address token) internal view returns (uint256) {
        RateLimitStorage storage $ = _getRateLimitStorage();
        uint256 lastUpdate = $._lastUpdate[token];
        if (lastUpdate == 0) {
            return 0;
        }
        uint256 elapsed = block.timestamp - lastUpdate;
        if (elapsed >= RATE_LIMIT_PERIOD) {
            return 0;
        }
        // `elapsed * limit` does not fit in 256 bits for a limit of about 2**256 / 1 days or more, and the
        // multiplication would then revert every send, receive and refund of the token
        uint256 decay = Math.mulDiv(elapsed, $._rateLimits[token], RATE_LIMIT_PERIOD);
        uint256 usage = $._usage[token];
        if (decay >= usage) {
            return 0;
        }
        return usage - decay;
    }

    /// @notice Returns the storage of the RateLimit contract
    /// @return $ The storage of the RateLimit contract
    function _getRateLimitStorage() internal pure returns (RateLimitStorage storage $) {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            $.slot := RATELIMIT_STORAGE_SLOT
        }
    }
}
