// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length,gas-small-strings

import { Test } from "forge-std/Test.sol";

import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IICS02Client } from "contracts/core/interfaces/IICS02Client.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";
import { IAccessManaged } from "@openzeppelin-contracts/access/manager/IAccessManaged.sol";
import { IICS02ClientErrors } from "contracts/core/errors/IICS02ClientErrors.sol";

import { ICS02ClientUpgradeable } from "contracts/core/client-registry/ICS02ClientUpgradeable.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { ICS26Router } from "contracts/core/ICS26Router.sol";
import { ClientMigrationProposer } from "contracts/core/client-registry/migration/modules/ClientMigrationProposer.sol";
import { ClientMigrationExecutor } from "contracts/core/client-registry/migration/modules/ClientMigrationExecutor.sol";
import {
    ClientMigrationModuleIds
} from "contracts/core/client-registry/migration/interfaces/IClientMigrationModule.sol";
import { TestHelper } from "test/utils/TestHelper.sol";
import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IBCSelectorLib } from "scripts/deployments/IBCSelectorLib.sol";

contract ICS02ClientTest is Test {
    ICS02ClientUpgradeable public ics02Client;
    AccessManager public accessManager;
    ClientMigrationProposer public clientMigrationProposer;
    ClientMigrationExecutor public clientMigrationExecutor;

    address public lightClient = makeAddr("lightClient");

    bytes[] public merklePrefix = [bytes("ibc"), bytes("")];
    bytes[] public randomPrefix = [bytes("test"), bytes("prefix")];

    string public clientIdentifier;

    address public idCustomizer = makeAddr("idCustomizer");
    address public relayer = makeAddr("relayer");
    address public misbehaviourSubmitter = makeAddr("misbehaviourSubmitter");
    address public pauser = makeAddr("pauser");

    TestHelper public th = new TestHelper();

    function setUp() public {
        clientMigrationProposer = new ClientMigrationProposer();
        clientMigrationExecutor = new ClientMigrationExecutor();
        ICS26Router ics26RouterLogic =
            new ICS26Router(address(clientMigrationProposer), address(clientMigrationExecutor));

        accessManager = new AccessManager(address(this));

        ERC1967Proxy routerProxy = new ERC1967Proxy(
            address(ics26RouterLogic), abi.encodeCall(ICS26Router.initialize, (address(accessManager)))
        );

        ics02Client = ICS02ClientUpgradeable(address(routerProxy));

        accessManager.setTargetFunctionRole(
            address(ics02Client), IBCSelectorLib.ics26IdCustomizerSelectors(), IBCRolesLib.ID_CUSTOMIZER_ROLE
        );
        accessManager.setTargetFunctionRole(
            address(ics02Client), IBCSelectorLib.ics26RelayerSelectors(), IBCRolesLib.RELAYER_ROLE
        );
        accessManager.setTargetFunctionRole(
            address(ics02Client), IBCSelectorLib.ics26MisbehaviourSelectors(), IBCRolesLib.MISBEHAVIOUR_SUBMITTER_ROLE
        );
        accessManager.grantRole(IBCRolesLib.ID_CUSTOMIZER_ROLE, idCustomizer, 0);
        accessManager.grantRole(IBCRolesLib.RELAYER_ROLE, relayer, 0);
        accessManager.grantRole(IBCRolesLib.MISBEHAVIOUR_SUBMITTER_ROLE, misbehaviourSubmitter, 0);
        accessManager.grantRole(IBCRolesLib.PAUSER_ROLE, pauser, 0);

        // The registry rejects light clients without code, whether they are added or migrated to
        vm.etch(lightClient, hex"00");
        vm.etch(makeAddr("newLightClient"), hex"00");

        string memory counterpartyId = "dummy-counterparty-01";
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo(counterpartyId, merklePrefix);
        vm.expectEmit();
        emit IICS02Client.ICS02ClientAdded(th.FIRST_CLIENT_ID(), counterpartyInfo, lightClient);
        clientIdentifier = ics02Client.addClient(counterpartyInfo, lightClient);

        ILightClient fetchedLightClient = ics02Client.getClient(clientIdentifier);
        assertNotEq(address(fetchedLightClient), address(0), "client not found");

        IICS02ClientMsgs.CounterpartyInfo memory fetchedCounterparty = ics02Client.getCounterparty(clientIdentifier);
        assertEq(fetchedCounterparty.clientId, counterpartyId, "counterparty not set correctly");
    }

    function test_constructorRejectsMissingMigrationModule() public {
        vm.expectRevert(abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationModuleMissing.selector, address(0)));
        new ICS26Router(address(0), address(clientMigrationExecutor));
    }

    function test_constructorRejectsWrongMigrationModule() public {
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCClientMigrationModuleMismatch.selector,
                address(clientMigrationExecutor),
                ClientMigrationModuleIds.PROPOSER,
                ClientMigrationModuleIds.EXECUTOR
            )
        );
        new ICS26Router(address(clientMigrationExecutor), address(clientMigrationExecutor));
    }

    function test_constructorRejectsWrongExecutorModule() public {
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCClientMigrationModuleMismatch.selector,
                address(clientMigrationProposer),
                ClientMigrationModuleIds.EXECUTOR,
                ClientMigrationModuleIds.PROPOSER
            )
        );
        new ICS26Router(address(clientMigrationProposer), address(clientMigrationProposer));
    }

    function test_constructorRejectsModuleWithoutModuleId() public {
        NoModuleIdContract unrelated = new NoModuleIdContract();
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCClientMigrationModuleNotResponding.selector, address(unrelated)
            )
        );
        new ICS26Router(address(unrelated), address(clientMigrationExecutor));
    }

    function test_constructorRejectsModuleWithMalformedModuleId() public {
        MalformedModuleIdContract malformed = new MalformedModuleIdContract();
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCClientMigrationModuleNotResponding.selector, address(malformed)
            )
        );
        new ICS26Router(address(clientMigrationProposer), address(malformed));
    }

    function test_success_customClientId() public {
        string memory customClientId = "custom-client-id";
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo(customClientId, merklePrefix);
        vm.prank(idCustomizer);
        string memory newId = ics02Client.addClient(customClientId, counterpartyInfo, lightClient);
        assertEq(customClientId, newId, "custom client id not set correctly");
    }

    function test_success_counterpartyGeneratedClientId() public {
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("client-0", merklePrefix);
        vm.prank(idCustomizer);
        ics02Client.addClient("counterparty-client", counterpartyInfo, lightClient);
    }

    function test_failure_counterpartyClientIdWithPath() public {
        IICS02ClientMsgs.CounterpartyInfo memory invalidCounterparty =
            IICS02ClientMsgs.CounterpartyInfo("bad/client", merklePrefix);

        vm.expectRevert(IICS02ClientErrors.IBCInvalidCounterpartyClientId.selector);
        ics02Client.addClient(invalidCounterparty, lightClient);

        vm.prank(idCustomizer);
        vm.expectRevert(IICS02ClientErrors.IBCInvalidCounterpartyClientId.selector);
        ics02Client.addClient("custom-counterparty", invalidCounterparty, lightClient);
    }

    function test_failure_customClientId() public {
        vm.startPrank(idCustomizer);
        // client id is not custom (starts with "client-")
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo(clientIdentifier, merklePrefix);
        vm.expectRevert(IICS02ClientErrors.IBCInvalidLocalClientId.selector);
        ics02Client.addClient(clientIdentifier, counterpartyInfo, lightClient);

        // reuse of client id
        string memory customClientId = "custom-client-id";
        IICS02ClientMsgs.CounterpartyInfo memory validCounterparty =
            IICS02ClientMsgs.CounterpartyInfo(customClientId, merklePrefix);
        ics02Client.addClient(customClientId, validCounterparty, lightClient);
        vm.expectRevert(abi.encodeWithSelector(IICS02ClientErrors.IBCClientAlreadyExists.selector, customClientId));
        ics02Client.addClient(customClientId, validCounterparty, lightClient);

        // unauthorized id customizer
        vm.stopPrank();
        address unauthorized = makeAddr("unauthorized");
        vm.prank(unauthorized);
        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        ics02Client.addClient(customClientId, validCounterparty, lightClient);
    }

    function test_failure_addClient_invalidClient() public {
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        address codeless = makeAddr("codeless");

        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.addClient(counterpartyInfo, address(0));
        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.addClient(counterpartyInfo, codeless);

        vm.startPrank(idCustomizer);
        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.addClient("custom-client-id", counterpartyInfo, address(0));
        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.addClient("custom-client-id", counterpartyInfo, codeless);
        vm.stopPrank();

        // Neither attempt registered a client or consumed a generated id
        assertEq(ics02Client.getNextClientSeq(), 1, "client seq must not advance");
        vm.expectRevert(abi.encodeWithSelector(IICS02ClientErrors.IBCClientNotFound.selector, "custom-client-id"));
        ics02Client.getClient("custom-client-id");
    }

    function test_failure_addClient_emptyMerklePrefix() public {
        IICS02ClientMsgs.CounterpartyInfo memory noPrefix =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", new bytes[](0));

        vm.expectRevert(IICS02ClientErrors.IBCInvalidCounterpartyMerklePrefix.selector);
        ics02Client.addClient(noPrefix, lightClient);

        vm.prank(idCustomizer);
        vm.expectRevert(IICS02ClientErrors.IBCInvalidCounterpartyMerklePrefix.selector);
        ics02Client.addClient("custom-client-id", noPrefix, lightClient);

        // A prefix with one empty element is not empty: it is what counterparties without a store prefix use
        IICS02ClientMsgs.CounterpartyInfo memory emptyElementPrefix =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", th.EMPTY_MERKLE_PREFIX());
        string memory newId = ics02Client.addClient(emptyElementPrefix, lightClient);
        assertEq(ics02Client.getCounterparty(newId).merklePrefix.length, 1, "prefix not stored");
    }

    function test_failure_proposeClientMigration_invalidClient() public {
        address clientMigrator = makeAddr("clientMigrator");
        accessManager.grantRole(ics02Client.getLightClientMigratorRole(clientIdentifier), clientMigrator, 60);
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);

        // Executing a migration to address(0) would silently unregister the client
        vm.startPrank(clientMigrator);
        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, address(0));
        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, makeAddr("codeless"));
        vm.stopPrank();

        (bytes32 digest,,,) = ics02Client.getClientMigration(clientIdentifier);
        assertEq(digest, bytes32(0), "no proposal may be committed");
    }

    function test_failure_executeClientMigration_clientLostItsCode() public {
        address clientMigrator = makeAddr("clientMigrator");
        accessManager.grantRole(ics02Client.getLightClientMigratorRole(clientIdentifier), clientMigrator, 60);
        address newLightClient = makeAddr("newLightClient");
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);

        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (, uint48 executeAfter,,) = ics02Client.getClientMigration(clientIdentifier);
        vm.warp(executeAfter);

        // The executor does not rely on the proposer having checked: it is also the only gate for a proposal
        // that was committed before the proposer did
        vm.etch(newLightClient, "");
        vm.expectRevert(IICS02ClientErrors.IBCInvalidClientAddress.selector);
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        assertEq(address(ics02Client.getClient(clientIdentifier)), lightClient, "client must stay registered");
    }

    function test_failure_executeClientMigration_proposerRoleLostItsDelay() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);
        address newLightClient = makeAddr("newLightClient");
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);

        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (, uint48 executeAfter,,) = ics02Client.getClientMigration(clientIdentifier);

        // Re-granting the role without a delay still makes the proposer a member, but a zero-delay role could not
        // have created the proposal, so it cannot keep it alive either
        accessManager.grantRole(migratorRole, clientMigrator, 0);
        vm.warp(executeAfter);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationDelayRequired.selector, clientIdentifier)
        );
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        assertEq(address(ics02Client.getClient(clientIdentifier)), lightClient, "client must stay registered");
    }

    function test_MigrateClient() public {
        address unauthorized = makeAddr("unauthorized");
        address clientMigrator = makeAddr("clientMigrator");

        // Grant the per-clientId migrator role for clientIdentifier to clientMigrator.
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);

        string memory counterpartyId = "dummy-counterparty-01";
        address newLightClient = makeAddr("newLightClient");
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo(counterpartyId, merklePrefix);

        // An address without the per-clientId migrator role is rejected.
        vm.prank(unauthorized);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCUnauthorizedMigrator.selector, clientIdentifier, unauthorized)
        );
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);

        // The granted migrator must first create a delayed proposal.
        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (, uint48 executeAfter, uint48 expireAfter, address proposer) = ics02Client.getClientMigration(clientIdentifier);
        assertEq(executeAfter, block.timestamp + 48 hours, "migration floor not applied");
        assertEq(expireAfter, executeAfter + accessManager.expiration(), "migration expiry not snapshotted");
        assertEq(proposer, clientMigrator, "migration proposer not recorded");
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCClientMigrationNotReady.selector, clientIdentifier, executeAfter
            )
        );
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        vm.warp(executeAfter);
        vm.prank(makeAddr("randomer"));
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        ILightClient fetchedLightClient = ics02Client.getClient(clientIdentifier);
        assertEq(address(fetchedLightClient), newLightClient, "client not migrated");

        IICS02ClientMsgs.CounterpartyInfo memory fetchedCounterparty = ics02Client.getCounterparty(clientIdentifier);
        assertEq(fetchedCounterparty.clientId, counterpartyId, "counterparty not migrated");
        assertEq(fetchedCounterparty.merklePrefix, merklePrefix, "counterparty not migrated");
        assertEq(ics02Client.getNextClientSeq(), 1, "client seq not incremented");

        IICS02ClientMsgs.CounterpartyInfo memory changedCounterparty =
            IICS02ClientMsgs.CounterpartyInfo(counterpartyId, randomPrefix);
        vm.prank(clientMigrator);
        vm.expectRevert(IICS02ClientErrors.IBCCounterpartyMismatch.selector);
        ics02Client.proposeClientMigration(clientIdentifier, changedCounterparty, newLightClient);

        bytes[] memory shortenedPrefix = new bytes[](1);
        shortenedPrefix[0] = bytes("ibc");
        changedCounterparty = IICS02ClientMsgs.CounterpartyInfo(counterpartyId, shortenedPrefix);
        vm.prank(clientMigrator);
        vm.expectRevert(IICS02ClientErrors.IBCCounterpartyMismatch.selector);
        ics02Client.proposeClientMigration(clientIdentifier, changedCounterparty, newLightClient);

        changedCounterparty = IICS02ClientMsgs.CounterpartyInfo("other-counterparty", merklePrefix);
        vm.prank(clientMigrator);
        vm.expectRevert(IICS02ClientErrors.IBCCounterpartyMismatch.selector);
        ics02Client.proposeClientMigration(clientIdentifier, changedCounterparty, newLightClient);

        changedCounterparty = IICS02ClientMsgs.CounterpartyInfo("bad/client", merklePrefix);
        vm.prank(clientMigrator);
        vm.expectRevert(IICS02ClientErrors.IBCInvalidCounterpartyClientId.selector);
        ics02Client.proposeClientMigration(clientIdentifier, changedCounterparty, newLightClient);

        // A grant for one clientId must not authorize migration of any other clientId.
        // Use the unrestricted addClient (no custom id) to register a second default client.
        string memory otherClientId = ics02Client.addClient(counterpartyInfo, lightClient);

        IICS02ClientMsgs.CounterpartyInfo memory otherCounterparty =
            IICS02ClientMsgs.CounterpartyInfo(counterpartyId, merklePrefix);
        vm.prank(clientMigrator);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCUnauthorizedMigrator.selector, otherClientId, clientMigrator)
        );
        ics02Client.proposeClientMigration(otherClientId, otherCounterparty, newLightClient);
    }

    function test_failure_MigrateClient_withoutDelay() public {
        address delayedMigrator = makeAddr("delayedMigrator");

        // Grant the per-clientId migrator role for delayedMigrator without a delay.
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, delayedMigrator, 0);

        string memory counterpartyId = "dummy-counterparty-01";
        address newLightClient = makeAddr("newLightClient");
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo(counterpartyId, merklePrefix);

        // A zero-delay role cannot create a migration proposal.
        vm.prank(delayedMigrator);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationDelayRequired.selector, clientIdentifier)
        );
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
    }

    function test_cancelClientMigration() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);

        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        address newLightClient = makeAddr("newLightClient");

        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);

        address unauthorized = makeAddr("unauthorized");
        vm.prank(unauthorized);
        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        ics02Client.cancelClientMigration(clientIdentifier);

        vm.prank(pauser);
        ics02Client.cancelClientMigration(clientIdentifier);
        (bytes32 digest, uint48 executeAfter,,) = ics02Client.getClientMigration(clientIdentifier);
        assertEq(digest, bytes32(0), "migration digest not cleared");
        assertEq(executeAfter, 0, "migration timestamp not cleared");

        vm.warp(block.timestamp + 48 hours);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationNotProposed.selector, clientIdentifier)
        );
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
    }

    function test_delayedPauserCannotCancelClientMigration() public {
        address clientMigrator = makeAddr("clientMigrator");
        address delayedPauser = makeAddr("delayedPauser");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);
        accessManager.grantRole(IBCRolesLib.PAUSER_ROLE, delayedPauser, 1 days);

        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, makeAddr("newLightClient"));

        vm.prank(delayedPauser);
        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, delayedPauser));
        ics02Client.cancelClientMigration(clientIdentifier);

        vm.prank(pauser);
        ics02Client.cancelClientMigration(clientIdentifier);
    }

    function test_migrationMismatchDoesNotConsumeProposal() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);

        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        address newLightClient = makeAddr("newLightClient");

        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (bytes32 digest, uint48 executeAfter,,) = ics02Client.getClientMigration(clientIdentifier);
        vm.warp(executeAfter);

        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationMismatch.selector, clientIdentifier)
        );
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, makeAddr("wrongLightClient"));

        (bytes32 pendingDigest,,,) = ics02Client.getClientMigration(clientIdentifier);
        assertEq(pendingDigest, digest, "mismatched execution consumed proposal");
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        assertEq(address(ics02Client.getClient(clientIdentifier)), newLightClient, "client not migrated");
    }

    function test_failure_MigrateClient_whenClosed() public {
        address clientMigrator = makeAddr("clientMigrator");

        // Grant the per-clientId migrator role for clientIdentifier to clientMigrator.
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 0);

        string memory counterpartyId = "dummy-counterparty-01";
        address newLightClient = makeAddr("newLightClient");
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo(counterpartyId, merklePrefix);

        // Close the target contract via AccessManager.
        accessManager.setTargetClosed(address(ics02Client), true);

        // A granted migrator cannot propose while the target is closed.
        vm.prank(clientMigrator);
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCUnauthorizedMigrator.selector, clientIdentifier, clientMigrator
            )
        );
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
    }

    function test_revokedMigratorCannotExecuteAndPauserCanCancel() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);

        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        address newLightClient = makeAddr("newLightClient");
        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (, uint48 executeAfter,,) = ics02Client.getClientMigration(clientIdentifier);

        accessManager.revokeRole(migratorRole, clientMigrator);
        vm.warp(executeAfter);
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCUnauthorizedMigrator.selector, clientIdentifier, clientMigrator
            )
        );
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);

        vm.prank(pauser);
        ics02Client.cancelClientMigration(clientIdentifier);
    }

    function test_expiredMigrationCannotExecuteAndCanBeReproposed() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);

        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        address newLightClient = makeAddr("newLightClient");
        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (bytes32 oldDigest,, uint48 expireAfter,) = ics02Client.getClientMigration(clientIdentifier);

        vm.warp(uint256(expireAfter) + 1);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationExpired.selector, clientIdentifier, expireAfter)
        );
        ics02Client.executeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);

        address replacementClient = makeAddr("replacementClient");
        vm.etch(replacementClient, hex"00");
        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, replacementClient);
        (bytes32 replacementDigest,, uint48 replacementExpiry, address proposer) =
            ics02Client.getClientMigration(clientIdentifier);
        assertNotEq(replacementDigest, oldDigest, "expired migration was not replaced");
        assertGt(replacementExpiry, expireAfter, "replacement expiry not refreshed");
        assertEq(proposer, clientMigrator, "replacement proposer not recorded");
    }

    function test_pauserCanCancelWhileTargetClosed() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);

        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, makeAddr("newLightClient"));
        accessManager.setTargetClosed(address(ics02Client), true);

        address unauthorized = makeAddr("unauthorized");
        vm.prank(unauthorized);
        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        ics02Client.cancelClientMigration(clientIdentifier);

        vm.prank(pauser);
        ics02Client.cancelClientMigration(clientIdentifier);
        (bytes32 digest,,,) = ics02Client.getClientMigration(clientIdentifier);
        assertEq(digest, bytes32(0), "closed-target cancellation did not clear migration");
    }

    function test_migrateClientShimEnforcesProposalMaturityDigestAndTargetState() public {
        address clientMigrator = makeAddr("clientMigrator");
        uint64 migratorRole = ics02Client.getLightClientMigratorRole(clientIdentifier);
        accessManager.grantRole(migratorRole, clientMigrator, 60);
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        address newLightClient = makeAddr("newLightClient");

        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationNotProposed.selector, clientIdentifier)
        );
        ics02Client.migrateClient(clientIdentifier, counterpartyInfo, newLightClient);

        vm.prank(clientMigrator);
        ics02Client.proposeClientMigration(clientIdentifier, counterpartyInfo, newLightClient);
        (, uint48 executeAfter,,) = ics02Client.getClientMigration(clientIdentifier);
        vm.expectRevert(
            abi.encodeWithSelector(
                IICS02ClientErrors.IBCClientMigrationNotReady.selector, clientIdentifier, executeAfter
            )
        );
        ics02Client.migrateClient(clientIdentifier, counterpartyInfo, newLightClient);

        vm.warp(executeAfter);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCClientMigrationMismatch.selector, clientIdentifier)
        );
        ics02Client.migrateClient(clientIdentifier, counterpartyInfo, makeAddr("wrongLightClient"));

        accessManager.setTargetClosed(address(ics02Client), true);
        vm.expectRevert(
            abi.encodeWithSelector(IICS02ClientErrors.IBCUnauthorizedMigrator.selector, clientIdentifier, address(this))
        );
        ics02Client.migrateClient(clientIdentifier, counterpartyInfo, newLightClient);

        accessManager.setTargetClosed(address(ics02Client), false);
        ics02Client.migrateClient(clientIdentifier, counterpartyInfo, newLightClient);
        assertEq(address(ics02Client.getClient(clientIdentifier)), newLightClient, "shim did not execute migration");
    }

    function test_migrationModulesRejectDirectCalls() public {
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo =
            IICS02ClientMsgs.CounterpartyInfo("dummy-counterparty-01", merklePrefix);
        vm.expectRevert(IICS02ClientErrors.IBCClientMigrationDirectCallNotAllowed.selector);
        clientMigrationProposer.proposeClientMigration(clientIdentifier, counterpartyInfo, makeAddr("newLightClient"));

        vm.expectRevert(IICS02ClientErrors.IBCClientMigrationDirectCallNotAllowed.selector);
        clientMigrationExecutor.cancelClientMigration(clientIdentifier);
    }

    function test_Misbehaviour() public {
        bytes memory misbehaviourMsg = "testMisbehaviourMsg";
        bytes memory misbehaviourCall = abi.encodeCall(ILightClient.misbehaviour, (misbehaviourMsg));
        vm.mockCall(lightClient, misbehaviourCall, bytes(""));

        vm.expectCall(lightClient, misbehaviourCall);
        vm.prank(misbehaviourSubmitter);
        ics02Client.submitMisbehaviour(clientIdentifier, misbehaviourMsg);
    }

    function test_failure_submitMisbehaviour() public {
        address unauthorized = makeAddr("unauthorized");
        bytes memory misbehaviourMsg = "testMisbehaviourMsg";

        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        vm.prank(unauthorized);
        ics02Client.submitMisbehaviour(clientIdentifier, misbehaviourMsg);
    }

    function test_unfreezeClient() public {
        bytes memory unfreezeCall = abi.encodeCall(ILightClient.unfreeze, ());
        vm.mockCall(lightClient, unfreezeCall, bytes(""));

        vm.expectEmit();
        emit IICS02Client.ICS02ClientUnfrozen(clientIdentifier);
        ics02Client.unfreezeClient(clientIdentifier);
    }

    function test_failure_unfreezeClient() public {
        address unauthorized = makeAddr("unauthorized");

        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        vm.prank(unauthorized);
        ics02Client.unfreezeClient(clientIdentifier);
    }

    function test_success_updateApplicationState() public {
        bytes memory updateMsg = "testUpdateMsg";
        bytes memory updateCall = abi.encodeCall(ILightClient.updateApplicationState, (updateMsg));
        vm.mockCall(lightClient, updateCall, abi.encode(ILightClientMsgs.UpdateResult(0)));

        vm.expectCall(lightClient, updateCall);
        vm.prank(relayer);
        ics02Client.updateApplicationState(clientIdentifier, updateMsg);
    }

    function test_success_updateConsensusState() public {
        bytes memory updateMsg = "testUpdateConsensusStateMsg";
        bytes memory updateCall = abi.encodeCall(ILightClient.updateConsensusState, (updateMsg));
        vm.mockCall(lightClient, updateCall, abi.encode(ILightClientMsgs.UpdateResult(0)));

        vm.expectCall(lightClient, updateCall);
        vm.prank(relayer);
        ics02Client.updateConsensusState(clientIdentifier, updateMsg);
    }

    function test_failure_updateConsensusState() public {
        address unauthorized = makeAddr("unauthorized");
        bytes memory updateMsg = "testUpdateConsensusStateMsg";

        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        vm.prank(unauthorized);
        ics02Client.updateConsensusState(clientIdentifier, updateMsg);
    }

    function test_failure_updateApplicationState() public {
        address unauthorized = makeAddr("unauthorized");
        bytes memory updateMsg = "testUpdateMsg";

        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        vm.prank(unauthorized);
        ics02Client.updateApplicationState(clientIdentifier, updateMsg);
    }
}

/// @dev A contract with code but no `moduleId()`: the staticcall reverts.
contract NoModuleIdContract {
    uint256 public value;
}

/// @dev Answers `moduleId()` with 64 bytes instead of a single `bytes32`.
contract MalformedModuleIdContract {
    fallback() external {
        assembly ("memory-safe") {
            mstore(0, 0)
            mstore(32, 0)
            return(0, 64)
        }
    }
}
