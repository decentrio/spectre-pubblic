// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { stdJson } from "forge-std/StdJson.sol";
import { Script } from "forge-std/Script.sol";
import { console } from "forge-std/console.sol";
import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { TimelockController } from "@openzeppelin-contracts/governance/TimelockController.sol";
import { ICS26Router } from "contracts/core/ICS26Router.sol";
import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IBCIdentifiers } from "contracts/shared/access/IBCIdentifiers.sol";
import { IBCSelectorLib } from "scripts/deployments/IBCSelectorLib.sol";
import { ProductionConfigLib } from "scripts/deployments/ProductionConfigLib.sol";
import { IGroth16Verifier } from "contracts/light-clients/spectre/interfaces/IGroth16Verifier.sol";
import { IRateLimit } from "contracts/apps/ics20/interfaces/IRateLimit.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";

/// @notice Read-only post-deployment checks for ProductionDeploy.
/// @dev Governance must be a timelock whose minimum delay covers the
///      AccessManager security delay; ADMIN_ROLE is intentionally immediate.
contract ProductionVerify is Script {
    struct LaunchConfig {
        string[] clientIds;
        address[] tokens;
        uint256[] limits;
    }

    using stdJson for string;

    uint32 internal constant DEFAULT_DELAY = 2 days;

    function run() external view returns (bool) {
        AccessManager manager = AccessManager(vm.envAddress("ACCESS_MANAGER"));
        SignatureVerifier verifier = SignatureVerifier(vm.envAddress("SIGNATURE_VERIFIER"));
        address router = vm.envAddress("ICS26_ROUTER");
        address transfer = vm.envAddress("ICS20_TRANSFER");
        address governance = vm.envAddress("GOVERNANCE_ADMIN");
        address upgrader = vm.envAddress("UPGRADER_ACCOUNT");
        address bootstrap = vm.envAddress("BOOTSTRAP_ACCOUNT");
        address relayer = vm.envAddress("RELAYER_ACCOUNT");
        address pauser1 = vm.envAddress("PAUSER_1");
        address pauser2 = vm.envAddress("PAUSER_2");
        address unpauser = vm.envAddress("UNPAUSER_ACCOUNT");
        address watcher = vm.envAddress("MISBEHAVIOUR_WATCHER");
        address rateLimiter = vm.envAddress("RATE_LIMITER_ACCOUNT");
        uint256 configuredDelay = vm.envOr("SECURITY_DELAY", uint256(DEFAULT_DELAY));
        LaunchConfig memory launch = _loadLaunchConfig();

        require(configuredDelay >= DEFAULT_DELAY && configuredDelay <= type(uint32).max, "invalid security delay");
        uint32 delay = uint32(configuredDelay);

        require(address(manager).code.length != 0, "access manager has no code");
        require(router.code.length != 0 && transfer.code.length != 0, "IBC proxy has no code");
        require(governance.code.length != 0, "governance admin has no code");
        require(upgrader.code.length != 0, "upgrader must be a contract");
        require(
            TimelockController(payable(governance)).getMinDelay() >= configuredDelay,
            "governance timelock delay too short"
        );
        require(verifier.OWNER() == address(manager), "signature verifier owner mismatch");
        require(ICS26Router(router).authority() == address(manager), "router authority mismatch");
        require(ICS20Transfer(transfer).authority() == address(manager), "transfer authority mismatch");

        address[] memory principals = ProductionConfigLib.principalList(
            bootstrap, governance, upgrader, relayer, pauser1, pauser2, unpauser, watcher, rateLimiter
        );
        ProductionConfigLib.requireDistinct(principals, "role separation failure");

        bytes4 verifierSelector = IGroth16Verifier.verifyProof.selector;
        address[] memory verifiers = ProductionConfigLib.verifierList(
            vm.envAddress("VERIFIER_N4"),
            vm.envAddress("VERIFIER_N8"),
            vm.envAddress("VERIFIER_N16"),
            vm.envAddress("VERIFIER_N32"),
            vm.envAddress("VERIFIER_N64"),
            vm.envAddress("VERIFIER_N128")
        );
        // Re-checked here, not only at deploy time: a deployment made before this precondition
        // existed must not pass verification. See ProductionConfigLib.verifierList.
        ProductionConfigLib.requireDistinct(verifiers, "verifier buckets must be distinct contracts");

        uint16[6] memory buckets = [uint16(4), 8, 16, 32, 64, 128];
        for (uint256 i = 0; i < buckets.length; ++i) {
            _checkBucket(verifier, buckets[i], verifiers[i], verifierSelector);
        }

        _checkRoleAbsent(manager, IBCRolesLib.ADMIN_ROLE, bootstrap, "bootstrap admin role still active");
        _checkRole(manager, IBCRolesLib.ADMIN_ROLE, governance, "governance admin role missing");
        _checkRoleAbsent(manager, IBCRolesLib.RATE_LIMITER_ROLE, bootstrap, "bootstrap rate limiter role still active");
        _checkRoleDelay(manager, IBCRolesLib.UPGRADER_ROLE, upgrader, delay, "upgrader role delay is too short");
        _checkRoleExactDelay(manager, IBCRolesLib.RELAYER_ROLE, relayer, 0, "relayer role delay mismatch");
        _checkRoleExactDelay(manager, IBCRolesLib.PAUSER_ROLE, pauser1, 0, "pauser 1 role delay mismatch");
        _checkRoleExactDelay(manager, IBCRolesLib.PAUSER_ROLE, pauser2, 0, "pauser 2 role delay mismatch");
        _checkRoleDelay(manager, IBCRolesLib.UNPAUSER_ROLE, unpauser, delay, "unpauser role delay is too short");
        _checkRoleExactDelay(
            manager, IBCRolesLib.MISBEHAVIOUR_SUBMITTER_ROLE, watcher, 0, "watcher role delay mismatch"
        );
        _checkRoleDelay(
            manager, IBCRolesLib.ID_CUSTOMIZER_ROLE, governance, delay, "ID customizer role delay is too short"
        );
        _checkRoleExactDelay(manager, IBCRolesLib.RATE_LIMITER_ROLE, rateLimiter, 0, "rate limiter role delay mismatch");

        _checkSelectors(manager, router, IBCSelectorLib.ics26RelayerSelectors(), IBCRolesLib.RELAYER_ROLE);
        _checkSelectors(manager, router, IBCSelectorLib.ics26IdCustomizerSelectors(), IBCRolesLib.ID_CUSTOMIZER_ROLE);
        _checkSelectors(manager, router, IBCSelectorLib.pauserSelectors(), IBCRolesLib.PAUSER_ROLE);
        _checkSelectors(manager, router, IBCSelectorLib.unpauserSelectors(), IBCRolesLib.UNPAUSER_ROLE);
        _checkSelectors(
            manager, router, IBCSelectorLib.ics26MisbehaviourSelectors(), IBCRolesLib.MISBEHAVIOUR_SUBMITTER_ROLE
        );
        _checkSelectors(manager, router, IBCSelectorLib.uupsUpgradeSelectors(), IBCRolesLib.UPGRADER_ROLE);
        _checkSelectors(manager, transfer, IBCSelectorLib.pauserSelectors(), IBCRolesLib.PAUSER_ROLE);
        _checkSelectors(manager, transfer, IBCSelectorLib.unpauserSelectors(), IBCRolesLib.UNPAUSER_ROLE);
        _checkSelectors(manager, transfer, IBCSelectorLib.upgraderSelectors(), IBCRolesLib.UPGRADER_ROLE);

        bytes4[] memory setBucketSelector = new bytes4[](1);
        setBucketSelector[0] = SignatureVerifier.setBucket.selector;
        _checkSelectors(manager, address(verifier), setBucketSelector, IBCRolesLib.UPGRADER_ROLE);

        _validateLaunchConfig(launch);
        require(ICS20Transfer(transfer).requiresPrecreatedEscrows(), "production escrow gate disabled");
        for (uint256 i = 0; i < launch.clientIds.length; ++i) {
            address escrow = ICS20Transfer(transfer).getEscrow(launch.clientIds[i]);
            require(escrow.code.length != 0, "production escrow missing");
            require(ICS20Transfer(transfer).isEscrowActive(launch.clientIds[i]), "production escrow inactive");
            for (uint256 j = 0; j < launch.tokens.length; ++j) {
                require(IRateLimit(escrow).getRateLimit(launch.tokens[j]) == launch.limits[j], "rate limit mismatch");
            }
        }
        _checkLaunchLightClients(router, launch);
        return true;
    }

    /// @dev Optional, because the launch clients are registered through governance after the bootstrap and so do
    ///      not exist when this script first runs. Once they do, set PRODUCTION_LIGHT_CLIENTS to
    ///      `{"lightClients":["0x..", ..]}`, one address per launch client id, in the same order as
    ///      PRODUCTION_ESCROW_CONFIG `.clients`, to prove each launch id resolves to the light client you deployed.
    function _checkLaunchLightClients(address router, LaunchConfig memory launch) private view {
        string memory encoded = vm.envOr("PRODUCTION_LIGHT_CLIENTS", string(""));
        if (bytes(encoded).length == 0) {
            console.log("PRODUCTION_LIGHT_CLIENTS not set: launch client registration not verified");
            return;
        }

        address[] memory expected = encoded.readAddressArray(".lightClients");
        require(expected.length == launch.clientIds.length, "light client count mismatch");
        for (uint256 i = 0; i < launch.clientIds.length; ++i) {
            require(expected[i] != address(0) && expected[i].code.length != 0, "expected light client has no code");
            try ICS26Router(router).getClient(launch.clientIds[i]) returns (ILightClient registered) {
                require(address(registered) == expected[i], "launch client address mismatch");
            } catch {
                revert("launch client not registered");
            }
        }
    }

    function _loadLaunchConfig() internal view returns (LaunchConfig memory config) {
        string memory encoded = vm.envString("PRODUCTION_ESCROW_CONFIG");
        config.clientIds = encoded.readStringArray(".clients");
        config.tokens = encoded.readAddressArray(".tokens");
        config.limits = encoded.readUintArray(".limits");
    }

    function _validateLaunchConfig(LaunchConfig memory launch) private view {
        require(launch.clientIds.length != 0, "no production client escrows configured");
        require(launch.tokens.length != 0 && launch.tokens.length == launch.limits.length, "invalid rate-limit config");

        for (uint256 i = 0; i < launch.clientIds.length; ++i) {
            require(bytes(launch.clientIds[i]).length != 0, "empty production client id");
            // The default-ID `addClient` is permissionless and hands out `client-<seq>`, so a launch id in
            // that namespace can be taken by the first caller, who then inherits the pre-activated escrow.
            // Custom ids are disjoint from it and only the ID_CUSTOMIZER_ROLE can register one.
            require(
                IBCIdentifiers.validateCustomIBCIdentifier(bytes(launch.clientIds[i])),
                "production client id must be a custom id"
            );
            for (uint256 j = 0; j < i; ++j) {
                require(
                    keccak256(bytes(launch.clientIds[i])) != keccak256(bytes(launch.clientIds[j])),
                    "duplicate production client id"
                );
            }
        }

        for (uint256 i = 0; i < launch.tokens.length; ++i) {
            require(launch.tokens[i] != address(0) && launch.limits[i] != 0, "invalid rate-limit entry");
            require(launch.tokens[i].code.length != 0, "rate-limit token has no code");
            for (uint256 j = 0; j < i; ++j) {
                require(launch.tokens[i] != launch.tokens[j], "duplicate rate-limit token");
            }
        }
    }

    function _checkBucket(SignatureVerifier verifier, uint16 bucket, address expected, bytes4 selector) private view {
        (address implementation, bytes4 configuredSelector) = verifier.buckets(bucket);
        require(implementation == expected, "bucket verifier address mismatch");
        require(implementation.code.length != 0, "bucket verifier has no code");
        require(configuredSelector == selector, "bucket selector mismatch");
    }

    function _checkRole(AccessManager manager, uint64 role, address account, string memory message) private view {
        (bool member,) = manager.hasRole(role, account);
        require(member, message);
    }

    function _checkRoleDelay(
        AccessManager manager,
        uint64 role,
        address account,
        uint32 minimumDelay,
        string memory message
    )
        private
        view
    {
        (bool member, uint32 delay) = manager.hasRole(role, account);
        require(member && delay >= minimumDelay, message);
    }

    function _checkRoleExactDelay(
        AccessManager manager,
        uint64 role,
        address account,
        uint32 expectedDelay,
        string memory message
    )
        private
        view
    {
        (bool member, uint32 delay) = manager.hasRole(role, account);
        require(member && delay == expectedDelay, message);
    }

    function _checkRoleAbsent(AccessManager manager, uint64 role, address account, string memory message) private view {
        (bool member,) = manager.hasRole(role, account);
        require(!member, message);
    }

    function _checkSelectors(
        AccessManager manager,
        address target,
        bytes4[] memory selectors,
        uint64 role
    )
        private
        view
    {
        for (uint256 i = 0; i < selectors.length; ++i) {
            require(manager.getTargetFunctionRole(target, selectors[i]) == role, "target role mismatch");
        }
    }
}
