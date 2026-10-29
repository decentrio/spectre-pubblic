// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISignatureVerifier } from "contracts/light-clients/spectre/interfaces/ISignatureVerifier.sol";
import { Encode } from "contracts/light-clients/spectre/libraries/Encode.sol";

/// @title SignatureVerifier
/// @notice Hashes the batch witness with SHA-256 and forwards the 32-byte digest
///         packed into two 128-bit public field elements to the per-bucket gnark
///         Groth16 verifier. One `BucketVerifier` must be registered per
///         supported bucket via `setBucket` before `verifyBatchProof` can succeed
///         for that size.
///
///         As of #199 the witness commitment binds only the common-prefix fields
///         (Type|Height), the vote round, the 32-byte block hash, and the
///         per-slot pubkey + active flag — instead of rebuilding
///         every validator's full canonical vote. The in-circuit Ed25519 verify
///         binds the full signed bytes (timestamp, chainID, part-set header), so
///         dropping them from the on-chain hash costs no security while shrinking
///         both the witness commitment (fewer circuit constraints) and calldata
///         (no per-slot timestamps).
///
///         The in-circuit hash (see relayer/prover/witness/witness.go) reproduces the same
///         byte layout; any drift between this file, witness.go, and
///         BatchCircuit.Define() fails the proof's public-input assertion.
contract SignatureVerifier is ISignatureVerifier {
    /// Witness byte layout (must match relayer/prover/witness/witness.go::encodeWitnessBytes
    /// and BatchCircuit.Define exactly):
    ///   PrefixHead(11) || Round(8) || BlockHash(32)
    ///   || per slot: active(1) || A(32)
    /// PrefixHead = Type(0x08 0x02) || Height(0x11 || sfixed64) — the first 11
    /// bytes of every signer's canonical-vote body. Round = sfixed64 LE, zero
    /// when the round field is absent.
    uint256 constant PREFIX_HEAD_LEN = 11;
    uint256 constant ROUND_LEN = 8;
    uint256 constant WITNESS_HEAD_LEN = PREFIX_HEAD_LEN + ROUND_LEN + 32; // + round + blockHash = 51
    uint256 constant WITNESS_SLOT_LEN = 1 + 32; // active + pubkey

    /// @param verifier Address of the per-bucket gnark-generated Groth16 verifier.
    /// @param selector 4-byte function selector of that verifier's `verifyProof`.
    ///                 Upstream gnark PR #1554 (merged Feb 2026) switched the
    ///                 BN254 Solidity verifier signature from
    ///                 `verifyProof(uint256[8],uint256[2],uint256[2],uint256[N])`
    ///                 to `verifyProof(bytes,uint256[N])` where the bytes blob
    ///                 packs (proof || commitments || commitmentPok) = 384 bytes.
    ///                 `N` now equals 2 because the SHA-256 digest is exposed
    ///                 as two 128-bit public field elements.
    struct BucketVerifier {
        address verifier;
        bytes4 selector;
    }

    address public immutable OWNER;
    mapping(uint16 => BucketVerifier) public buckets;

    /// @notice Emitted when a per-bucket verifier is registered or replaced.
    event BucketSet(uint16 indexed bucket, address verifier, bytes4 selector);

    error UnknownBucket(uint16 bucket);
    error LengthMismatch();
    error NotOwner();
    error NoVerifierCode(address verifier);
    error ZeroOwner();

    constructor(address owner) {
        if (owner == address(0)) revert ZeroOwner();
        OWNER = owner;
    }

    /// @notice Registers the verifier contract for a bucket size.
    /// @dev Registered verifiers MUST revert on invalid proofs. `verifyBatchProof`
    ///      treats a non-reverting staticcall as success, matching gnark-generated
    ///      Solidity verifiers.
    function setBucket(uint16 bucket, address verifier, bytes4 selector) external {
        if (msg.sender != OWNER) revert NotOwner();
        if (verifier.code.length == 0) revert NoVerifierCode(verifier);
        buckets[bucket] = BucketVerifier({ verifier: verifier, selector: selector });
        emit BucketSet(bucket, verifier, selector);
    }

    /// @inheritdoc ISignatureVerifier
    function verifyBatchProof(
        uint16 bucket,
        uint256[8] calldata proof,
        uint256[2] calldata commitments,
        uint256[2] calldata commitmentPok,
        bytes32[] calldata pubkeys,
        bool[] calldata active,
        ISignatureVerifier.SharedBlock calldata shared
    )
        external
        view
        override
        returns (bool)
    {
        if (pubkeys.length != bucket || active.length != bucket) revert LengthMismatch();

        BucketVerifier memory bv = buckets[bucket];
        if (bv.verifier == address(0)) revert UnknownBucket(bucket);
        if (bv.verifier.code.length == 0) revert NoVerifierCode(bv.verifier);

        bytes32 h = _hashWitness(pubkeys, active, shared);

        uint256[2] memory publicInputs = _digestPublicInputs(h);

        bytes memory proofBytes = abi.encodePacked(proof, commitments, commitmentPok);
        bytes memory cd = abi.encodeWithSelector(bv.selector, proofBytes, publicInputs);
        (bool ok,) = bv.verifier.staticcall(cd);
        return ok;
    }

    /// @dev Canonical witness byte layout — must match Go's
    ///      witness.ComputeWitnessHash and BatchCircuit.Define exactly:
    ///        PrefixHead(11) || Round(8) || BlockHash(32)
    ///        || per slot: active(1) || A(32)
    ///      PrefixHead = Type(0x08 0x02) || Height(0x11 || sfixed64(height)) — the
    ///      first 11 bytes of every signer's canonical-vote body. Round is
    ///      sfixed64(round) (LE, all zero when round == 0, where proto omits the
    ///      field); the circuit asserts it equal in every active slot — so one
    ///      proof cannot mix precommits from different rounds — and derives
    ///      round-presence from it to select the block-hash offset (the round
    ///      field shifts BlockID by 9 bytes). BlockHash is the 32-byte BlockID hash. A is hashed so calldata
    ///      pubkey swaps can't misattribute voting power; R/S are bound by the
    ///      in-circuit Ed25519 verify, not here.
    function _hashWitness(
        bytes32[] calldata pubkeys,
        bool[] calldata active,
        ISignatureVerifier.SharedBlock calldata shared
    )
        internal
        pure
        returns (bytes32)
    {
        bytes memory buf = new bytes(WITNESS_HEAD_LEN + pubkeys.length * WITNESS_SLOT_LEN);

        // PrefixHead: Type(0x08 0x02) || Height(0x11 || sfixed64(height)).
        _storeByte(buf, 0, 0x08);
        _storeByte(buf, 1, 0x02);
        _storeByte(buf, 2, 0x11);
        _copyBytes(buf, 3, Encode.encodeSfixed64(int64(shared.height)));

        // Round (8) || BlockHash (32).
        // forge-lint: disable-next-line(unsafe-typecast)
        _copyBytes(buf, PREFIX_HEAD_LEN, Encode.encodeSfixed64(int64(shared.round)));
        _storeBytes32(buf, PREFIX_HEAD_LEN + ROUND_LEN, shared.blockIDHash);

        uint256 offset = WITNESS_HEAD_LEN;
        for (uint256 i = 0; i < pubkeys.length; i++) {
            _storeByte(buf, offset, active[i] ? 1 : 0);
            _storeBytes32(buf, offset + 1, pubkeys[i]);
            offset += WITNESS_SLOT_LEN;
        }
        return sha256(buf);
    }

    function _digestPublicInputs(bytes32 h) internal pure returns (uint256[2] memory publicInputs) {
        uint256 digest = uint256(h);
        publicInputs[0] = digest >> 128;
        publicInputs[1] = digest & type(uint128).max;
    }

    function _storeByte(bytes memory dst, uint256 offset, uint256 value) private pure {
        assembly ("memory-safe") {
            mstore8(add(add(dst, 0x20), offset), value)
        }
    }

    function _storeBytes32(bytes memory dst, uint256 offset, bytes32 value) private pure {
        assembly ("memory-safe") {
            mstore(add(add(dst, 0x20), offset), value)
        }
    }

    function _copyBytes(bytes memory dst, uint256 dstOffset, bytes memory src) private pure returns (uint256) {
        uint256 len;
        assembly ("memory-safe") {
            len := mload(src)
            mcopy(add(add(dst, 0x20), dstOffset), add(src, 0x20), len)
        }
        return dstOffset + len;
    }
}
