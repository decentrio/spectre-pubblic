// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";

/// @dev Exposes the internal compareBytes used by non-membership key ordering.
contract CompareBytesHarness is Membership {
    function exposed(bytes memory a, bytes memory b) external pure returns (int8) {
        return compareBytes(a, b);
    }
}

/// @notice Boundary coverage for compareBytes (issue #113). Non-membership proofs
/// rely on this ordering matching Cosmos/IAVL key ordering, which is Go's
/// `bytes.Compare`: byte-by-byte UNSIGNED comparison up to the shorter length,
/// then the shorter slice sorts first (prefix < extension). Every expected value
/// below equals `bytes.Compare(a, b)` for the same inputs.
contract MembershipCompareBytesTest is Test {
    CompareBytesHarness internal h;

    function setUp() public {
        h = new CompareBytesHarness();
    }

    function _assert(bytes memory a, bytes memory b, int8 want, string memory label) internal view {
        assertEq(int256(h.exposed(a, b)), int256(want), label);
    }

    function test_equal() public view {
        _assert(bytes("abc"), bytes("abc"), 0, "equal");
        _assert(bytes(""), bytes(""), 0, "empty==empty");
        _assert(hex"00", hex"00", 0, "0x00==0x00");
    }

    function test_sameLength_byteDecides() public view {
        _assert(bytes("abc"), bytes("abd"), -1, "abc<abd");
        _assert(bytes("abd"), bytes("abc"), 1, "abd>abc");
        _assert(hex"0102", hex"0103", -1, "0102<0103");
        _assert(hex"0103", hex"0102", 1, "0103>0102");
    }

    function test_prefixSortsFirst() public view {
        // shorter slice that is a prefix of the longer one sorts first
        _assert(bytes("abc"), bytes("abcd"), -1, "abc<abcd");
        _assert(bytes("abcd"), bytes("abc"), 1, "abcd>abc");
        _assert(hex"00", hex"0000", -1, "0x00<0x0000");
        _assert(hex"0000", hex"00", 1, "0x0000>0x00");
    }

    function test_emptyOrdering() public view {
        _assert(bytes(""), bytes("a"), -1, "empty<a");
        _assert(bytes("a"), bytes(""), 1, "a>empty");
    }

    function test_firstDifferingByteWinsRegardlessOfLength() public view {
        // 'b' (0x62) > 'a' (0x61) at index 0, so longer 'azzzz' still loses
        _assert(bytes("b"), bytes("azzzz"), 1, "b>azzzz");
        _assert(bytes("azzzz"), bytes("b"), -1, "azzzz<b");
    }

    function test_unsignedByteComparison() public view {
        // Critical: bytes must compare UNSIGNED (0x80 is greater than 0x7f),
        // matching Go's byte (uint8) comparison. A signed interpretation would
        // flip these and silently break non-membership ordering.
        _assert(hex"80", hex"7f", 1, "0x80>0x7f");
        _assert(hex"7f", hex"80", -1, "0x7f<0x80");
        _assert(hex"ff", hex"00", 1, "0xff>0x00");
        _assert(hex"00", hex"ff", -1, "0x00<0xff");
        _assert(hex"80", hex"81", -1, "0x80<0x81");
    }

    // Property: antisymmetry — compareBytes(a,b) == -compareBytes(b,a).
    function testFuzz_antisymmetric(bytes memory a, bytes memory b) public view {
        int8 ab = h.exposed(a, b);
        int8 ba = h.exposed(b, a);
        assertEq(int256(ab), -int256(ba), "antisymmetry");
    }

    // Property: reflexivity — a value compares equal to itself.
    function testFuzz_reflexive(bytes memory a) public view {
        assertEq(int256(h.exposed(a, a)), int256(0), "reflexive");
    }

    // Property: result is always one of {-1, 0, 1}.
    function testFuzz_trichotomy(bytes memory a, bytes memory b) public view {
        int8 r = h.exposed(a, b);
        assertTrue(r == -1 || r == 0 || r == 1, "trichotomy");
    }
}
