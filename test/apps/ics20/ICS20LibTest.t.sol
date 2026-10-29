// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length

import { Test } from "forge-std/Test.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";

contract ICS20LibTest is Test {
    // ---------------------------------------------------------------
    // Tests: hasDenomPrefix segment-anchored prefix check
    //
    // getDenomPrefix takes `string calldata` so we build the prefix
    // bytes inline (abi.encodePacked equivalent) to avoid the calldata
    // restriction inside pure test functions.
    // ---------------------------------------------------------------

    function test_hasDenomPrefix_matches() public pure {
        bytes memory prefix = abi.encodePacked("transfer", "/", "client-1", "/");
        assert(ICS20Lib.hasDenomPrefix(bytes("transfer/client-1/uatom"), prefix));
    }

    function test_hasDenomPrefix_multiHop() public pure {
        bytes memory prefix = abi.encodePacked("transfer", "/", "client-1", "/");
        assert(ICS20Lib.hasDenomPrefix(bytes("transfer/client-1/transfer/client-2/uatom"), prefix));
    }

    function test_hasDenomPrefix_noMatch_wrongClient() public pure {
        // "client-10" shares the "client-1" byte prefix but differs at the segment boundary
        bytes memory prefix = abi.encodePacked("transfer", "/", "client-1", "/");
        assert(!ICS20Lib.hasDenomPrefix(bytes("transfer/client-10/uatom"), prefix));
    }

    function test_hasDenomPrefix_noMatch_noSeparator() public pure {
        // "transfer/client-1" without trailing slash — shorter than prefix
        bytes memory prefix = abi.encodePacked("transfer", "/", "client-1", "/");
        assert(!ICS20Lib.hasDenomPrefix(bytes("transfer/client-1"), prefix));
    }

    function test_hasDenomPrefix_noMatch_exactPrefix() public pure {
        // denom equals the prefix exactly (no base denom after it) — must NOT match
        bytes memory prefix = abi.encodePacked("transfer", "/", "client-1", "/");
        assert(!ICS20Lib.hasDenomPrefix(bytes("transfer/client-1/"), prefix));
    }

    function test_hasDenomPrefix_noMatch_wrongPort() public pure {
        bytes memory prefix = abi.encodePacked("transfer", "/", "client-1", "/");
        assert(!ICS20Lib.hasDenomPrefix(bytes("other/client-1/uatom"), prefix));
    }
}
