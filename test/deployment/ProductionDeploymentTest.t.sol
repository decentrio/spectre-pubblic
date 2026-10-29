// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";
import { stdJson } from "forge-std/StdJson.sol";
import { TimelockController } from "@openzeppelin-contracts/governance/TimelockController.sol";
import { ProductionDeploy } from "scripts/ProductionDeploy.s.sol";
import { ProductionVerify } from "scripts/ProductionVerify.s.sol";
import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { IICS02Client } from "contracts/core/interfaces/IICS02Client.sol";
import { TestERC20 } from "test/mocks/TestERC20.sol";

contract ProductionVerifierMock {
    function verifyProof(bytes calldata, uint256[2] calldata) external pure { }
}

contract ProductionUpgraderMock { }

/// @notice Exercises the production deployment and verification scripts together.
contract ProductionDeploymentTest is Test {
    using stdJson for string;

    uint256 internal constant SECURITY_DELAY = 2 days;

    function _setValidEnv() internal {
        address[] memory proposers = new address[](1);
        address[] memory executors = new address[](1);
        proposers[0] = address(this);
        executors[0] = address(this);
        address governance = address(new TimelockController(SECURITY_DELAY, proposers, executors, address(this)));

        _setAddressEnv("BOOTSTRAP_ACCOUNT", address(0xB001));
        _setAddressEnv("GOVERNANCE_ADMIN", governance);
        _setAddressEnv("UPGRADER_ACCOUNT", address(new ProductionUpgraderMock()));
        _setAddressEnv("RELAYER_ACCOUNT", address(0xB002));
        _setAddressEnv("PAUSER_1", address(0xB003));
        _setAddressEnv("PAUSER_2", address(0xB004));
        _setAddressEnv("UNPAUSER_ACCOUNT", address(0xB005));
        _setAddressEnv("MISBEHAVIOUR_WATCHER", address(0xB006));
        _setAddressEnv("RATE_LIMITER_ACCOUNT", address(0xB007));

        _setAddressEnv("VERIFIER_N4", address(new ProductionVerifierMock()));
        _setAddressEnv("VERIFIER_N8", address(new ProductionVerifierMock()));
        _setAddressEnv("VERIFIER_N16", address(new ProductionVerifierMock()));
        _setAddressEnv("VERIFIER_N32", address(new ProductionVerifierMock()));
        _setAddressEnv("VERIFIER_N64", address(new ProductionVerifierMock()));
        _setAddressEnv("VERIFIER_N128", address(new ProductionVerifierMock()));

        vm.setEnv("SECURITY_DELAY", vm.toString(SECURITY_DELAY));
        // Optional input: an operator's exported value must not leak into the first verification.
        vm.setEnv("PRODUCTION_LIGHT_CLIENTS", "");
        address launchToken = address(new TestERC20());
        vm.setEnv(
            "PRODUCTION_ESCROW_CONFIG",
            string.concat('{"clients":["eth-main"],"tokens":["', vm.toString(launchToken), '"],"limits":["1000000"]}')
        );
    }

    /// @notice Deploy, verify, and the two preconditions that cannot be checked after the fact.
    ///
    /// All of it lives in ONE test function on purpose. `vm.setEnv` writes the PROCESS
    /// environment, which is shared across test functions while each function gets its own EVM
    /// state — so a second function reads the first function's addresses and finds no code at
    /// them. Forge also runs functions in parallel, which makes the clobbering nondeterministic.
    /// Splitting these apart is what made this file fail intermittently.
    function testProductionDeployVerifyAndRejectBrokenConfigs() public {
        _setValidEnv();

        string memory deployment = new ProductionDeploy().run();
        _setAddressEnv("ACCESS_MANAGER", deployment.readAddress(".accessManager"));
        _setAddressEnv("SIGNATURE_VERIFIER", deployment.readAddress(".signatureVerifier"));
        _setAddressEnv("ICS26_ROUTER", deployment.readAddress(".ics26Router"));
        _setAddressEnv("ICS20_TRANSFER", deployment.readAddress(".ics20Transfer"));

        assertTrue(ICS20Transfer(deployment.readAddress(".ics20Transfer")).requiresPrecreatedEscrows());
        assertTrue(ICS20Transfer(deployment.readAddress(".ics20Transfer")).isEscrowActive("eth-main"));
        assertTrue(new ProductionVerify().run());

        ProductionVerify verifier = new ProductionVerify();
        address configuredUpgrader = vm.envAddress("UPGRADER_ACCOUNT");
        address configuredUnpauser = vm.envAddress("UNPAUSER_ACCOUNT");
        address configuredRateLimiter = vm.envAddress("RATE_LIMITER_ACCOUNT");

        // Verification must reject old deployments that predate deploy-time role separation.
        _setAddressEnv("UNPAUSER_ACCOUNT", vm.envAddress("PAUSER_1"));
        vm.expectRevert("role separation failure");
        verifier.run();
        _setAddressEnv("UNPAUSER_ACCOUNT", configuredUnpauser);

        // The upgrade role must remain contract-owned after deployment, not only at bootstrap.
        _setAddressEnv("UPGRADER_ACCOUNT", address(0xB007));
        vm.expectRevert("upgrader must be a contract");
        verifier.run();
        _setAddressEnv("UPGRADER_ACCOUNT", configuredUpgrader);

        // The launch-gate role is part of the same separation invariant as the
        // original eight privileged principals.
        _setAddressEnv("RATE_LIMITER_ACCOUNT", vm.envAddress("RELAYER_ACCOUNT"));
        vm.expectRevert("role separation failure");
        verifier.run();
        _setAddressEnv("RATE_LIMITER_ACCOUNT", configuredRateLimiter);

        // Verification must reject a launch id in the permissionless `client-` namespace, including
        // deployments that predate the deploy-time check.
        string memory launchConfig = vm.envString("PRODUCTION_ESCROW_CONFIG");
        vm.setEnv(
            "PRODUCTION_ESCROW_CONFIG",
            string.concat(
                '{"clients":["client-0"],"tokens":["',
                vm.toString(launchConfig.readAddressArray(".tokens")[0]),
                '"],"limits":["1000000"]}'
            )
        );
        vm.expectRevert("production client id must be a custom id");
        verifier.run();
        vm.setEnv("PRODUCTION_ESCROW_CONFIG", launchConfig);

        // Optional launch-client registration check: the launch clients are registered through governance after
        // the bootstrap, so it only runs once PRODUCTION_LIGHT_CLIENTS is set. The governed addClient round trip
        // (two timelocks) is not replayed here; getClient is mocked to stand in for its result.
        address router = vm.envAddress("ICS26_ROUTER");
        address lightClient = address(new ProductionVerifierMock());
        _setLightClients(lightClient);

        vm.expectRevert("launch client not registered");
        verifier.run();

        vm.mockCall(router, abi.encodeCall(IICS02Client.getClient, ("eth-main")), abi.encode(lightClient));
        assertTrue(verifier.run());

        vm.mockCall(router, abi.encodeCall(IICS02Client.getClient, ("eth-main")), abi.encode(address(0xBAD)));
        vm.expectRevert("launch client address mismatch");
        verifier.run();
        vm.clearMockedCalls();

        vm.setEnv("PRODUCTION_LIGHT_CLIENTS", string.concat('{"lightClients":["', vm.toString(address(0xBAD)), '"]}'));
        vm.expectRevert("expected light client has no code");
        verifier.run();

        vm.setEnv(
            "PRODUCTION_LIGHT_CLIENTS",
            string.concat('{"lightClients":["', vm.toString(lightClient), '","', vm.toString(lightClient), '"]}')
        );
        vm.expectRevert("light client count mismatch");
        verifier.run();

        vm.setEnv("PRODUCTION_LIGHT_CLIENTS", "");
        assertTrue(verifier.run());

        ProductionDeploy deployer = new ProductionDeploy();

        _setValidEnv();
        _setAddressEnv("VERIFIER_N4", address(0xBEEF));
        vm.expectRevert("all verifier buckets require deployed code");
        deployer.run();

        _setValidEnv();
        _setAddressEnv("VERIFIER_N8", vm.envAddress("VERIFIER_N4"));
        vm.expectRevert("verifier buckets must be distinct contracts");
        deployer.run();

        _setValidEnv();
        _setAddressEnv("UNPAUSER_ACCOUNT", vm.envAddress("PAUSER_1"));
        vm.expectRevert("role separation failure");
        deployer.run();
    }

    function _setLightClients(address lightClient) internal {
        vm.setEnv("PRODUCTION_LIGHT_CLIENTS", string.concat('{"lightClients":["', vm.toString(lightClient), '"]}'));
    }

    function _setAddressEnv(string memory name, address value) internal {
        vm.setEnv(name, vm.toString(value));
    }
}
