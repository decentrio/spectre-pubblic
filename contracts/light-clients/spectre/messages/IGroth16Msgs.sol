// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title Groth16 Messages
/// @notice This interface defines the structure of messages used in the gnark program.
interface IGroth16Msgs {
    /// @notice The Groth16 proof that can be submitted to the Groth16Verifier contract.
    /// @dev vKey must be verified before sending this to the gnark program.
    /// @param vKey The verification key for the program.
    /// @param publicValues The public values for the program.
    /// @param proof The proof for the program.
    struct Groth16Proof {
        bytes32 vKey;
        bytes publicValues;
        bytes proof;
    }
}
