// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";
import { ProductionConfigLib } from "scripts/deployments/ProductionConfigLib.sol";

contract ProductionConfigHarness {
    function verifierList(address[6] calldata configured) external pure returns (address[] memory) {
        return ProductionConfigLib.verifierList(
            configured[0], configured[1], configured[2], configured[3], configured[4], configured[5]
        );
    }
}

/// @notice Locks the deployment topology that must move atomically with prover support.
contract CompatibilityDeploymentTest is Test {
    function testProductionVerifierTopologyPreservesAllSixBuckets() public {
        ProductionConfigHarness harness = new ProductionConfigHarness();
        address[6] memory expected =
            [address(0x4), address(0x8), address(0x16), address(0x32), address(0x64), address(0x128)];
        address[] memory verifiers = harness.verifierList(expected);

        assertEq(verifiers.length, expected.length);
        for (uint256 i = 0; i < expected.length; ++i) {
            assertEq(verifiers[i], expected[i]);
        }
    }
}
