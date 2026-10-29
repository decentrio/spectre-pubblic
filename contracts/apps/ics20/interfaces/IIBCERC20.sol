// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IMintableAndBurnable } from "contracts/apps/ics20/interfaces/IMintableAndBurnable.sol";

/// @title IIBCERC20
/// @notice The interface for received tokens, deployed on receive by ICS20
interface IIBCERC20 is IMintableAndBurnable {
    /// @notice Get the full denom path of the token
    /// @return The full path of the token's denom
    function fullDenomPath() external view returns (string memory);

    /// @notice Set custom display metadata for the token
    /// @dev This can only be called once by the owning ICS20 contract.
    /// @param name_ The custom token name
    /// @param symbol_ The custom token symbol
    /// @param decimals_ The custom token decimals
    function setMetadata(string calldata name_, string calldata symbol_, uint8 decimals_) external;

    /// @notice Get the escrow contract address
    /// @return The escrow contract address
    function escrow() external view returns (address);

    /// @notice Get the ICS20 contract address
    /// @return The ICS20 contract address
    function ics20() external view returns (address);

    /// @notice Initializes the IBCERC20 contract
    /// @dev This function is meant to be called by a proxy
    /// @param ics20_ The ICS20 contract address
    /// @param escrow_ The escrow contract address, can burn and mint tokens
    /// @param fullDenomPath_ The full IBC denom path for this token
    function initialize(address ics20_, address escrow_, string calldata fullDenomPath_) external;

    /// @notice Emitted when custom token metadata is set
    /// @param name_ The custom token name
    /// @param symbol_ The custom token symbol
    /// @param decimals_ The custom token decimals
    event IBCERC20MetadataSet(string name_, string symbol_, uint8 decimals_);
}
