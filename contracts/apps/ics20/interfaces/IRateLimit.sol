// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IRateLimit
/// @notice Interface for a rate limiting contract that manages token usage limits
interface IRateLimit {
    /// @notice Sets the rate limit for a token
    /// @dev The caller must hold the GLOBAL `RATE_LIMITER_ROLE` on the AccessManager, with an
    ///      execution delay of zero. It is not a per-escrow grant: one role holder governs every
    ///      escrow, present and future, because escrows are created per client at runtime and a
    ///      per-target grant could not exist before the escrow does. See
    ///      `RateLimitUpgradeable.setRateLimit` for why this is not the `restricted` modifier.
    ///      Usage that has decayed under the previous limit is settled before the new limit applies.
    /// @param token The token address
    /// @param rateLimit The rate limit to set, where 0 means no limit
    function setRateLimit(address token, uint256 rateLimit) external;

    /// @notice Gets the rate limit for a token
    /// @param token The token address
    /// @return The rate limit for the token
    function getRateLimit(address token) external view returns (uint256);

    /// @notice Gets a token's current usage in the rolling window
    /// @param token The token address
    /// @return The current usage for the token
    function getDailyUsage(address token) external view returns (uint256);
}
