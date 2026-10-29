// SPDX-License-Identifier: MIT
pragma solidity ^0.8.4;

/// @title ISignatureVerifier - Ed25519 signature verification via Groth16 proof
/// @notice Wrapper that recomputes the hash-aggregate witness commitment from
///         calldata and forwards that 32-byte digest, packed into two public
///         field elements, to the underlying Groth16 verifier selected by
///         bucket.
interface ISignatureVerifier {
    /// @notice Shared CanonicalVote fields the witness commitment binds across
    ///         every validator signing the same Tendermint block. The on-chain
    ///         witness commits only the common prefix (Type|Height), whether the
    ///         round field is present, and the 32-byte block hash. Timestamp,
    ///         chainID and the BlockID part-set header are left out of the hash —
    ///         the in-circuit Ed25519 verify still binds the full signed bytes,
    ///         and chainID is enforced separately on-chain (header.chainId ==
    ///         clientState.chainId).
    struct SharedBlock {
        uint64 height;
        uint64 round;
        bytes32 blockIDHash;
    }

    /// @notice Verify a batch of Ed25519 signatures via a single Groth16 proof.
    /// @dev The relayer picks `bucket` as the smallest circuit that fits the number of signers
    ///      needed to reach 2/3 voting power. Slots beyond the true signer count are padded with
    ///      deterministic dummy signatures whose `active[i] = false`. Those dummy signatures are
    ///      real signatures over dummy bytes and are verified and aggregated by the circuit like
    ///      any other slot — the circuit does NOT gate inactive slots out of the ECIP aggregate.
    ///      Exclusion happens on-chain only: the quorum check skips slots with `active[i] = false`,
    ///      so they carry no voting power. `active` is bound into the witness commitment, which is
    ///      what stops calldata from re-labelling a padding slot as a signer.
    /// @param bucket Validator-count bucket (selects which per-bucket Groth16Verifier to dispatch to)
    /// @param proof The Groth16 proof (8 uint256s: Ar, Bs, Krs)
    /// @param commitments The proof commitments (2 uint256s)
    /// @param commitmentPok The proof of knowledge for commitments (2 uint256s)
    /// @param pubkeys Per-slot Ed25519 compressed public key. Length == bucket.
    /// @param active Per-slot real-signer flag (true = real validator, false = dummy padding). Length == bucket.
    /// @param shared Common-prefix CanonicalVote fields shared across all slots.
    function verifyBatchProof(
        uint16 bucket,
        uint256[8] calldata proof,
        uint256[2] calldata commitments,
        uint256[2] calldata commitmentPok,
        bytes32[] calldata pubkeys,
        bool[] calldata active,
        SharedBlock calldata shared
    )
        external
        returns (bool);
}
