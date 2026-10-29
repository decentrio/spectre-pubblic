// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IGroth16Verifier
/// @notice Common ABI exposed by the generated bucket verifiers.
interface IGroth16Verifier {
    function verifyProof(bytes calldata proof, uint256[2] calldata input) external view;
}
