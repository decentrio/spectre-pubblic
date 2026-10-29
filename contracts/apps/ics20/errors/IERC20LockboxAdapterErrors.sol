// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IERC20LockboxAdapterErrors
/// @notice Interface for the errors of the ERC20 lockbox adapter
interface IERC20LockboxAdapterErrors {
    /// @notice Unauthorized function call
    /// @param caller The caller of the function
    error AdapterUnauthorized(address caller);

    /// @notice The address is not a registered escrow
    /// @param account The address that was expected to be a registered escrow
    error AdapterNotEscrow(address account);

    /// @notice The adapter is not a user-transferable ERC20
    error AdapterNotTransferable();

    /// @notice The underlying reserve cannot cover the requested release
    /// @param requested The amount of the underlying token requested, in underlying units
    /// @param available The amount of the underlying token held by the adapter, in underlying units
    error AdapterInsufficientReserve(uint256 requested, uint256 available);

    /// @notice The underlying/denom decimal relationship cannot be represented without truncation or overflow
    /// @param denomDecimals The decimals of the IBC denom
    /// @param underlyingDecimals The decimals of the underlying token
    error AdapterInvalidDecimals(uint8 denomDecimals, uint8 underlyingDecimals);

    /// @notice The declared underlying decimals do not match the underlying token
    /// @param reported The decimals reported by the underlying token
    /// @param declared The decimals declared at deployment
    error AdapterUnderlyingDecimalsMismatch(uint8 reported, uint8 declared);

    /// @notice The underlying token did not credit the adapter with the expected amount
    /// @param expected The expected balance increase, in underlying units
    /// @param actual The observed balance increase, in underlying units
    error AdapterUnexpectedUnderlyingBalance(uint256 expected, uint256 actual);

    /// @notice An underlying transfer debited or credited an unexpected amount
    /// @param expected The amount that should have moved, in underlying units
    /// @param debited The amount debited from the adapter, in underlying units
    /// @param credited The amount credited to the receiver, in underlying units
    error AdapterUnexpectedUnderlyingTransfer(uint256 expected, uint256 debited, uint256 credited);

    /// @notice An address that must contain contract code does not
    /// @param account The address without contract code
    error AdapterNotContract(address account);

    /// @notice A contract does not expose the escrow interface expected by the adapter
    /// @param escrow The invalid escrow contract
    error AdapterInvalidEscrow(address escrow);

    /// @notice An escrow is bound to a different ICS20 contract
    /// @param escrow The escrow contract
    /// @param expected The ICS20 contract configured on the adapter
    /// @param actual The ICS20 contract configured on the escrow
    error AdapterEscrowICS20Mismatch(address escrow, address expected, address actual);

    /// @notice A zero address was provided where a contract address is required
    error AdapterZeroAddress();
}
