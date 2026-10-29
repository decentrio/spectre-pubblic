// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IMintableAndBurnable
/// @notice Interface for ERC20 tokens to be minted and burned by the ICS20 contract.
interface IMintableAndBurnable {
    /// @notice Mint new tokens to the Escrow contract
    /// @dev This function can only be called by an authorized contract (e.g., ICS20)
    /// @dev This function needs to allow minting tokens to the Escrow contract
    /// @param mintAddress Address to mint tokens to
    /// @param amount Amount of tokens to mint
    function mint(address mintAddress, uint256 amount) external;

    /// @notice Burn `amount` tokens from the caller's own balance
    /// @dev Matches OpenZeppelin's `ERC20Burnable.burn` signature. Implementations are expected to restrict the
    /// caller (e.g. to the Escrow contract) so that only its own balance can ever be burned.
    /// @param amount Amount of tokens to burn
    function burn(uint256 amount) external;
}
