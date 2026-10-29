// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";

/// @dev Exposes the internal leaf/inner hashing helpers whose assembly was rewritten
/// to bounded `mcopy` in issue #114.
contract ApplyOpsHarness is Membership {
    function exposedApplyLeaf(
        IMembershipMsgs.LeafOp memory op,
        bytes memory key,
        bytes memory value
    )
        external
        pure
        returns (bytes32)
    {
        return applyLeaf(op, key, value);
    }

    function exposedApplyInner(IMembershipMsgs.InnerOp memory inner, bytes32 child) external view returns (bytes32) {
        return applyInner(inner, child);
    }

    function exposedPrepareLeafData(
        IMembershipMsgs.HashOp prehashOp,
        bytes memory data
    )
        external
        pure
        returns (bytes memory)
    {
        return prepareLeafData(prehashOp, data);
    }

    function exposedDecodeVarint(bytes memory data, uint256 offset) external pure returns (uint64, uint256) {
        return decodeVarint(data, offset);
    }
}

/// @notice Correctness coverage for the bounded-copy rewrite in applyLeaf / applyInner /
/// prepareLeafData (issue #114). The original assembly copied in 32-byte words and
/// overshot the destination by up to 31 bytes whenever a segment length was not
/// 32-aligned. These tests drive deliberately non-32-aligned segment lengths and assert
/// the output equals a pure-Solidity `abi.encodePacked` reference — so any byte the copy
/// drops, duplicates, or leaks would change the hash and fail.
contract MembershipApplyOpsTest is Test {
    ApplyOpsHarness internal h;

    function setUp() public {
        h = new ApplyOpsHarness();
    }

    /// @dev Single-byte varint for lengths < 128 (matches Membership.encodeVarint).
    function _varint1(uint256 n) internal pure returns (bytes memory) {
        require(n < 128, "test helper only covers short varints");
        return abi.encodePacked(bytes1(uint8(n)));
    }

    function _leafOp(
        IMembershipMsgs.HashOp hashOp,
        bytes memory prefix
    )
        internal
        pure
        returns (IMembershipMsgs.LeafOp memory)
    {
        return IMembershipMsgs.LeafOp({
            hashOp: hashOp,
            prehashKey: IMembershipMsgs.HashOp.NO_HASH,
            prehashValue: IMembershipMsgs.HashOp.NO_HASH,
            prefix: prefix
        });
    }

    // --- prepareLeafData ---------------------------------------------------

    function test_prepareLeafData_noHash_nonAligned() public view {
        bytes memory data = hex"0102030405"; // length 5 (non-aligned)
        bytes memory got = h.exposedPrepareLeafData(IMembershipMsgs.HashOp.NO_HASH, data);
        bytes memory want = abi.encodePacked(_varint1(data.length), data);
        assertEq(got, want, "noHash 5-byte");
    }

    function test_prepareLeafData_noHash_crossesWordBoundary() public view {
        // 33 bytes: forces a second copy word that overshoots under the old loop.
        bytes memory data = new bytes(33);
        for (uint256 i = 0; i < data.length; i++) {
            data[i] = bytes1(uint8(i + 1));
        }
        bytes memory got = h.exposedPrepareLeafData(IMembershipMsgs.HashOp.NO_HASH, data);
        bytes memory want = abi.encodePacked(_varint1(data.length), data);
        assertEq(got, want, "noHash 33-byte");
    }

    function test_prepareLeafData_sha256Prehash() public view {
        bytes memory data = hex"deadbeef"; // length 4
        bytes memory got = h.exposedPrepareLeafData(IMembershipMsgs.HashOp.SHA256, data);
        // prehash -> 32-byte digest, prefixed with varint(32) == 0x20
        bytes memory want = abi.encodePacked(bytes1(0x20), sha256(data));
        assertEq(got, want, "sha256 prehash");
    }

    function test_decodeVarint_slowPathReturnsAbsoluteOffset() public view {
        // The trailing continuation byte forces decodeVarint's fallback path; the
        // decoded varint itself starts at offset 1 and ends at absolute offset 3.
        (uint64 value, uint256 offset) = h.exposedDecodeVarint(hex"aa810180", 1);
        assertEq(value, 129, "decoded value");
        assertEq(offset, 3, "absolute offset");
    }

    // --- applyLeaf ---------------------------------------------------------

    function test_applyLeaf_sha256_nonAligned() public view {
        bytes memory prefix = hex"000203"; // len 3
        bytes memory key = hex"6162636465"; // len 5
        bytes memory value = hex"a1a2a3a4a5a6a7"; // len 7
        bytes32 got = h.exposedApplyLeaf(_leafOp(IMembershipMsgs.HashOp.SHA256, prefix), key, value);

        bytes memory image = abi.encodePacked(prefix, _varint1(key.length), key, _varint1(value.length), value);
        assertEq(got, sha256(image), "applyLeaf sha256");
    }

    function test_applyLeaf_keccak_nonAligned() public view {
        bytes memory prefix = hex"01"; // len 1
        bytes memory key = hex"112233445566778899"; // len 9
        bytes memory value = hex"ff"; // len 1
        bytes32 got = h.exposedApplyLeaf(_leafOp(IMembershipMsgs.HashOp.KECCAK256, prefix), key, value);

        bytes memory image = abi.encodePacked(prefix, _varint1(key.length), key, _varint1(value.length), value);
        assertEq(got, keccak256(image), "applyLeaf keccak");
    }

    // --- applyInner --------------------------------------------------------

    function _innerOp(
        IMembershipMsgs.HashOp hashOp,
        bytes memory prefix,
        bytes memory suffix
    )
        internal
        pure
        returns (IMembershipMsgs.InnerOp memory)
    {
        return IMembershipMsgs.InnerOp({ hashOp: hashOp, prefix: prefix, suffix: suffix });
    }

    function test_applyInner_sha256_nonAligned() public view {
        bytes memory prefix = hex"0102030405"; // len 5
        bytes memory suffix = hex"aabbcc"; // len 3
        bytes32 child = keccak256("child");
        bytes32 got = h.exposedApplyInner(_innerOp(IMembershipMsgs.HashOp.SHA256, prefix, suffix), child);
        assertEq(got, sha256(abi.encodePacked(prefix, child, suffix)), "applyInner sha256");
    }

    function test_applyInner_keccak_prefixCrossesWordBoundary() public view {
        // 33-byte prefix forces a prefix copy that overshoots into the child slot
        // under the old word-copy loop; the child write must still land correctly.
        bytes memory prefix = new bytes(33);
        for (uint256 i = 0; i < prefix.length; i++) {
            prefix[i] = bytes1(uint8(0x40 + i));
        }
        bytes memory suffix = hex"01"; // len 1
        bytes32 child = keccak256("another-child");
        bytes32 got = h.exposedApplyInner(_innerOp(IMembershipMsgs.HashOp.KECCAK256, prefix, suffix), child);
        assertEq(got, keccak256(abi.encodePacked(prefix, child, suffix)), "applyInner keccak");
    }

    function test_applyInner_emptySuffix() public view {
        bytes memory prefix = hex"deadbeefca"; // len 5
        bytes memory suffix = hex""; // len 0
        bytes32 child = keccak256("c");
        bytes32 got = h.exposedApplyInner(_innerOp(IMembershipMsgs.HashOp.SHA256, prefix, suffix), child);
        assertEq(got, sha256(abi.encodePacked(prefix, child, suffix)), "applyInner empty suffix");
    }
}
