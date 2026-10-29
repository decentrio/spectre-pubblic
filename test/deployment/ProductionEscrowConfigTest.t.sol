// SPDX-License-Identifier: UNLICENSED
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { ProductionDeploy } from "scripts/ProductionDeploy.s.sol";
import { TestERC20 } from "test/mocks/TestERC20.sol";

contract ProductionDeployHarness is ProductionDeploy {
    function validateLaunchConfig(
        string[] calldata clientIds,
        address[] calldata tokens,
        uint256[] calldata limits
    )
        external
        view
    {
        _validateLaunchConfig(LaunchConfig({ clientIds: clientIds, tokens: tokens, limits: limits }));
    }
}

contract ProductionEscrowConfigTest is Test {
    ProductionDeployHarness internal deploy;

    function setUp() public {
        deploy = new ProductionDeployHarness();
    }

    function test_success_validateUniqueClientIds() public {
        string[] memory clientIds = new string[](2);
        clientIds[0] = "eth-main-a";
        clientIds[1] = "eth-main-b";
        address[] memory tokens = new address[](1);
        tokens[0] = address(new TestERC20());
        uint256[] memory limits = new uint256[](1);
        limits[0] = 1;

        deploy.validateLaunchConfig(clientIds, tokens, limits);
    }

    function test_failure_validateDuplicateClientIds() public {
        string[] memory clientIds = new string[](2);
        clientIds[0] = "eth-main-a";
        clientIds[1] = "eth-main-a";
        address[] memory tokens = new address[](1);
        tokens[0] = address(new TestERC20());
        uint256[] memory limits = new uint256[](1);
        limits[0] = 1;

        vm.expectRevert("duplicate production client id");
        deploy.validateLaunchConfig(clientIds, tokens, limits);
    }

    function test_failure_validateSquattableClientId() public {
        address[] memory tokens = new address[](1);
        tokens[0] = address(new TestERC20());
        uint256[] memory limits = new uint256[](1);
        limits[0] = 1;

        string[4] memory squattable = ["client-0", "client-a", "channel-0", "ab"];
        for (uint256 i = 0; i < squattable.length; ++i) {
            string[] memory clientIds = new string[](1);
            clientIds[0] = squattable[i];

            vm.expectRevert("production client id must be a custom id");
            deploy.validateLaunchConfig(clientIds, tokens, limits);
        }
    }

    function test_failure_validateSquattableAmongValidClientIds() public {
        string[] memory clientIds = new string[](2);
        clientIds[0] = "eth-main-a";
        clientIds[1] = "client-1";
        address[] memory tokens = new address[](1);
        tokens[0] = address(new TestERC20());
        uint256[] memory limits = new uint256[](1);
        limits[0] = 1;

        vm.expectRevert("production client id must be a custom id");
        deploy.validateLaunchConfig(clientIds, tokens, limits);
    }

    function test_failure_validateEmptyClientId() public {
        string[] memory clientIds = new string[](1);
        clientIds[0] = "";
        address[] memory tokens = new address[](1);
        tokens[0] = address(new TestERC20());
        uint256[] memory limits = new uint256[](1);
        limits[0] = 1;

        vm.expectRevert("empty production client id");
        deploy.validateLaunchConfig(clientIds, tokens, limits);
    }

    function test_failure_validateDuplicateRateLimitTokens() public {
        string[] memory clientIds = new string[](1);
        clientIds[0] = "eth-main-a";
        address duplicateToken = address(new TestERC20());
        address[] memory tokens = new address[](2);
        tokens[0] = duplicateToken;
        tokens[1] = duplicateToken;
        uint256[] memory limits = new uint256[](2);
        limits[0] = 1;
        limits[1] = 2;

        vm.expectRevert("duplicate rate-limit token");
        deploy.validateLaunchConfig(clientIds, tokens, limits);
    }

    function test_failure_validateRateLimitTokenWithoutCode() public {
        string[] memory clientIds = new string[](1);
        clientIds[0] = "eth-main-a";
        address[] memory tokens = new address[](1);
        tokens[0] = makeAddr("not-a-contract");
        uint256[] memory limits = new uint256[](1);
        limits[0] = 1;

        vm.expectRevert("rate-limit token has no code");
        deploy.validateLaunchConfig(clientIds, tokens, limits);
    }
}
