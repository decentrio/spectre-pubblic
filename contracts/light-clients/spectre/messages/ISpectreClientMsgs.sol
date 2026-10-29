// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";

/// @title Spectre Client Messages
/// @notice Message and output types for the SpectreClient light client and its modules.
interface ISpectreClientMsgs {
    /// @notice One batched Ed25519 Groth16 proof over a single signed header.
    /// @dev The relayer picks the smallest registered `bucket` that fits the signers needed to reach
    ///      2/3 voting power; padding slots carry `active[i] = false`. The checked in-repo prover
    ///      manifest currently enables N=4 only. Larger buckets require matching prover artifacts,
    ///      verifier deployment, and `SignatureVerifier.setBucket` registration.
    /// @param proof The Groth16 proof (8 uint256s).
    /// @param commitments The proof commitments (2 uint256s).
    /// @param commitmentPok The proof of knowledge for commitments (2 uint256s).
    /// @param bucket The validator-count bucket this proof was generated against.
    /// @param signerIndices Validator indices in the header commit's commitSigs. Length == bucket.
    /// @param pinnedValidatorIndices Validator indices in the pinned validator set. Length == bucket.
    /// @param signerPubkeys Compressed Ed25519 public keys per slot, matching pinnedValidatorIndices. Length == bucket.
    /// @param active Per-slot real-signer flag (true = real validator, false = dummy padding). Length == bucket.
    struct BatchProof {
        uint256[8] proof;
        uint256[2] commitments;
        uint256[2] commitmentPok;
        uint16 bucket;
        uint32[] signerIndices;
        uint32[] pinnedValidatorIndices;
        bytes32[] signerPubkeys;
        bool[] active;
    }

    /// @notice Advances the block state (appHash) against the already-pinned validator set.
    /// @param trustedConsensusState The trusted consensus state the header is verified against.
    /// @param proposedHeader The proposed header with validator signatures.
    /// @param time The current time in unix nanoseconds.
    /// @param proof The batched Ed25519 signature proof over the proposed header.
    struct MsgUpdateApplicationState {
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState;
        ISpectreTendermintMsgs.Header proposedHeader;
        uint128 time;
        BatchProof proof;
    }

    /// @notice Rotates the pinned validator set and advances the consensus state.
    /// @param update The application-state update (header, proof, trusted consensus state).
    /// @param newValidatorSet The new validator set to pin; its hash must equal the header's nextValidatorsHash.
    struct MsgUpdateConsensusState {
        MsgUpdateApplicationState update;
        ISpectreTendermintMsgs.ValidatorSet newValidatorSet;
    }

    /// @notice Two conflicting signed headers at the same height, each proof-backed.
    struct Misbehaviour {
        ISpectreTendermintMsgs.Header header1;
        ISpectreTendermintMsgs.Header header2;
    }

    /// @notice Submits misbehaviour (two conflicting signed headers) to freeze the client.
    /// @param misbehaviour The two conflicting headers.
    /// @param trustedConsensusState1 The trusted consensus state header1 is verified against.
    /// @param trustedConsensusState2 The trusted consensus state header2 is verified against.
    /// @param time The current time in unix nanoseconds.
    /// @param proof1 The batched signature proof over header1.
    /// @param proof2 The batched signature proof over header2.
    struct MsgSubmitMisbehaviour {
        Misbehaviour misbehaviour;
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState1;
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState2;
        uint128 time;
        BatchProof proof1;
        BatchProof proof2;
    }

    /// @notice The result of validating a header inside the UpdateClient module.
    /// @param trustedConsensusState The trusted consensus state the header was verified against.
    /// @param newConsensusState The new consensus state derived from the verified header.
    /// @param trustedHeight The trusted height.
    /// @param newHeight The new height.
    struct VerifyHeaderOutput {
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState;
        ISpectreTendermintMsgs.ConsensusState newConsensusState;
        IICS02ClientMsgs.Height trustedHeight;
        IICS02ClientMsgs.Height newHeight;
    }
}
