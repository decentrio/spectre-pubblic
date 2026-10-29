// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

contract WasmStorageLayoutCompatibilityTest is Test {
    function test_shared_wasm_storage_layout_fixture() public view {
        string memory fixture = vm.readFile("test/fixtures/wasm-contracts/solidity-storage-layout.json");
        bytes32 pathHash = vm.parseJsonBytes32(fixture, ".path_hash");
        bytes32 commitmentSlot = vm.parseJsonBytes32(fixture, ".commitment_slot");
        bytes32 storageKey = vm.parseJsonBytes32(fixture, ".storage_key");

        assertEq(keccak256(abi.encodePacked(pathHash, commitmentSlot)), storageKey);
    }
}
