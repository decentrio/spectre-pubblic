// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IERC20LockboxAdapter
/// @notice Interface for the ERC20 lockbox adapter, which lets an already-deployed, non-mintable
/// ERC20 serve as the Ethereum-side representation of a Cosmos-native IBC denom.
interface IERC20LockboxAdapter {
    /// @notice Emitted when a release drops the remaining capacity below the configured threshold
    /// @param capacity The remaining capacity after the release, in denom units
    /// @param threshold The configured low-reserve threshold, in denom units
    event ReserveLow(uint256 capacity, uint256 threshold);

    /// @notice Emitted when an escrow contract is registered
    /// @param escrow The escrow contract address
    event EscrowRegistered(address indexed escrow);

    /// @notice Emitted when the low-reserve threshold is updated
    /// @param threshold The new threshold, in denom units
    event LowReserveThresholdSet(uint256 threshold);

    /// @notice Registers an escrow contract that is allowed to release the underlying token
    /// @dev The escrow must be a contract whose `ics20()` binding matches the adapter's ICS20.
    /// @param escrow The escrow contract address, as returned by `ICS20Transfer.getEscrow`
    function addEscrow(address escrow) external;

    /// @notice Sets the capacity level below which `ReserveLow` is emitted
    /// @param threshold The threshold in denom units, or zero to disable the alert
    function setLowReserveThreshold(uint256 threshold) external;

    /// @notice Returns the underlying token held by the adapter
    /// @return The reserve balance, in underlying units
    function reserve() external view returns (uint256);

    /// @notice Returns how much more of the denom can be bridged in before the reserve is exhausted
    /// @return The remaining capacity, in denom units
    function capacity() external view returns (uint256);

    /// @notice Returns whether an address is a registered escrow
    /// @param account The address to check
    /// @return True if the address is a registered escrow
    function isEscrow(address account) external view returns (bool);
}
