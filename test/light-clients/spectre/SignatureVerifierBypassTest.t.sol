// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";
import { ISignatureVerifier } from "contracts/light-clients/spectre/interfaces/ISignatureVerifier.sol";

/// @title SignatureVerifierBypassTest
/// @notice Locks down the SignatureVerifier code-less-verifier bypass
///         (issue.md: `staticcall` to an EOA / self-destructed address
///         returns `success = true`, silently passing every proof).
///
///         The fix closes the bypass at two layers:
///         (1) `setBucket` rejects code-less addresses (primary defense —
///             prevents the misconfiguration from ever being recorded).
///         (2) `verifyBatchProof` re-checks `verifier.code.length`
///             immediately before the `staticcall` (defense in depth —
///             protects against storage manipulation / future code paths
///             that could plant a code-less address without going through
///             `setBucket`).
contract SignatureVerifierBypassTest is Test {
    /// Harness that exposes the OWNER-gated `setBucket` setter without
    /// requiring an extra deployer actor.
    SignatureVerifierHarness internal wrapper;

    function setUp() public {
        wrapper = new SignatureVerifierHarness(address(this));
    }

    function test_constructor_revertsWhenOwnerIsZero() public {
        vm.expectRevert(SignatureVerifier.ZeroOwner.selector);
        new SignatureVerifierHarness(address(0));
    }

    // ------------------------------------------------------------------
    // Layer 1: `setBucket` rejects code-less addresses.
    // ------------------------------------------------------------------

    /// @notice `setBucket` must reject an EOA as the verifier address —
    ///         a `staticcall` to a code-less address would otherwise
    ///         return `success = true` with empty returndata, silently
    ///         passing every proof.
    function test_setBucket_revertsWhenVerifierHasNoCode_eoa() public {
        uint16 bucket = 4;
        address codeless = address(0xBEEF);
        assertEq(codeless.code.length, 0, "precondition: target must be code-less");

        vm.expectRevert(abi.encodeWithSelector(SignatureVerifier.NoVerifierCode.selector, codeless));
        wrapper.setBucket(bucket, codeless, bytes4(0xDEADBEEF));
    }

    /// @notice Same as above for a self-destructed contract: the
    ///         post-mortem address has no code and would otherwise
    ///         trigger the bypass.
    function test_setBucket_revertsWhenVerifierHasNoCode_selfDestructed() public {
        uint16 bucket = 4;
        Destructor d = new Destructor();
        address codeless = address(d);
        assertEq(codeless.code.length, 0, "precondition: SELFDESTRUCT must leave address code-less");

        vm.expectRevert(abi.encodeWithSelector(SignatureVerifier.NoVerifierCode.selector, codeless));
        wrapper.setBucket(bucket, codeless, bytes4(0xDEADBEEF));
    }

    // ------------------------------------------------------------------
    // Layer 2: `verifyBatchProof` re-checks before the staticcall.
    // ------------------------------------------------------------------

    /// @notice Defense in depth: even if a code-less address somehow ends
    ///         up in the `buckets` mapping (e.g. storage corruption, an
    ///         upgrade that bypasses `setBucket`, a future privileged
    ///         path), `verifyBatchProof` must still refuse to forward
    ///         the call. We simulate storage manipulation by writing the
    ///         struct directly via `vm.store`.
    function test_verifyBatchProof_revertsWhenBucketStorageIsCodeless() public {
        uint16 bucket = 4;

        Destructor d = new Destructor();
        address codeless = address(d);
        bytes4 selector = bytes4(0xDEADBEEF);
        assertEq(codeless.code.length, 0, "precondition: SELFDESTRUCT must leave address code-less");

        // Plant the code-less verifier directly into the mapping's storage
        // slot, bypassing `setBucket`.
        vm.store(address(wrapper), _bucketSlot(bucket), bytes32(_packBucketVerifier(codeless, selector)));

        ISignatureVerifier.SharedBlock memory shared =
            ISignatureVerifier.SharedBlock({ height: 1, round: 0, blockIDHash: bytes32(uint256(0x3333)) });

        vm.expectRevert(abi.encodeWithSelector(SignatureVerifier.NoVerifierCode.selector, codeless));
        wrapper.verifyBatchProof(
            bucket,
            _zeroProof(),
            _zeroCommitments(),
            _zeroCommitmentPok(),
            _zeroPubkeys(bucket),
            _zeroActive(bucket),
            shared
        );
    }

    // ------------------------------------------------------------------
    // Control: behavior of the wrapper for other failure modes is intact.
    // ------------------------------------------------------------------

    /// @notice Control: with **no bucket registered at all** the wrapper
    ///         reverts with `UnknownBucket` — guards against a regression
    ///         where the new `NoVerifierCode` check swallows every call.
    function test_verifyBatchProof_revertsWhenBucketUnregistered() public {
        uint16 bucket = 4;
        ISignatureVerifier.SharedBlock memory shared =
            ISignatureVerifier.SharedBlock({ height: 1, round: 0, blockIDHash: bytes32(uint256(0x5555)) });

        vm.expectRevert(abi.encodeWithSelector(SignatureVerifier.UnknownBucket.selector, bucket));
        wrapper.verifyBatchProof(
            bucket,
            _zeroProof(),
            _zeroCommitments(),
            _zeroCommitmentPok(),
            _zeroPubkeys(bucket),
            _zeroActive(bucket),
            shared
        );
    }

    // ------------------------------------------------------------------
    // Helpers
    // ------------------------------------------------------------------

    /// @dev Storage slot of `buckets[bucket]`. The mapping is the first
    ///      state variable of `SignatureVerifier` (slot 0); `OWNER` is
    ///      `immutable` and lives in code, not storage.
    function _bucketSlot(uint16 bucket) private pure returns (bytes32) {
        return keccak256(abi.encode(uint16(bucket), uint256(0)));
    }

    /// @dev Packed storage layout of `BucketVerifier { address verifier; bytes4 selector; }`
    ///      — verifier occupies the lowest 20 bytes, selector the next 4.
    function _packBucketVerifier(address verifier, bytes4 selector) private pure returns (uint256) {
        return uint256(uint160(verifier)) | (uint256(uint32(selector)) << 160);
    }

    function _zeroProof() private pure returns (uint256[8] memory p) {
        for (uint256 i = 0; i < 8; i++) {
            p[i] = 0;
        }
    }

    function _zeroCommitments() private pure returns (uint256[2] memory c) {
        c[0] = 0;
        c[1] = 0;
    }

    function _zeroCommitmentPok() private pure returns (uint256[2] memory p) {
        p[0] = 0;
        p[1] = 0;
    }

    function _zeroPubkeys(uint16 n) private pure returns (bytes32[] memory p) {
        p = new bytes32[](n);
    }

    function _zeroActive(uint16 n) private pure returns (bool[] memory a) {
        a = new bool[](n);
    }
}

contract SignatureVerifierHarness is SignatureVerifier {
    constructor(address owner) SignatureVerifier(owner) { }
}

contract Destructor {
    constructor() {
        // EIP-6780 (Cancun): selfdestruct only deletes the code/account
        // when called in the same transaction the contract was created.
        // Doing it in the constructor guarantees `address(d).code.length == 0`
        // by the time the outer test code resumes.
        selfdestruct(payable(msg.sender));
    }
}
