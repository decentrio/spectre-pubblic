// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IICS02Client } from "contracts/core/interfaces/IICS02Client.sol";
import { IICS02ClientErrors } from "contracts/core/errors/IICS02ClientErrors.sol";
import { IAccessManaged } from "@openzeppelin-contracts/access/manager/IAccessManaged.sol";
import { IAccessManager } from "@openzeppelin-contracts/access/manager/IAccessManager.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { ICS02ClientStore } from "contracts/core/client-registry/ICS02ClientStore.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";
import {
    IClientMigrationExecutor
} from "contracts/core/client-registry/migration/interfaces/IClientMigrationExecutor.sol";
import {
    IClientMigrationModule,
    ClientMigrationModuleIds
} from "contracts/core/client-registry/migration/interfaces/IClientMigrationModule.sol";

/// @title Client Migration Executor
/// @notice Executes or cancels a client migration in the router's storage context via delegatecall.
contract ClientMigrationExecutor is IClientMigrationExecutor, IICS02ClientErrors {
    address private immutable SELF;

    constructor() {
        SELF = address(this);
    }

    modifier onlyDelegated() {
        require(address(this) != SELF, IBCClientMigrationDirectCallNotAllowed());
        _;
    }

    /// @inheritdoc IClientMigrationModule
    function moduleId() external pure returns (bytes32) {
        return ClientMigrationModuleIds.EXECUTOR;
    }

    /// @inheritdoc IClientMigrationExecutor
    function executeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
        onlyDelegated
    {
        ICS02ClientStore.Layout storage $ = ICS02ClientStore.load();
        ICS02ClientStore.ClientMigration storage migration = $.migrations[clientId];
        require(migration.digest != bytes32(0), IBCClientMigrationNotProposed(clientId));
        require(block.timestamp >= migration.executeAfter, IBCClientMigrationNotReady(clientId, migration.executeAfter));
        require(block.timestamp <= migration.expireAfter, IBCClientMigrationExpired(clientId, migration.expireAfter));
        // The proposer is the only writer of this digest and validates that the counterparty client ID and
        // Merkle prefix match the existing binding before committing these exact execution parameters.
        require(
            migration.digest == _migrationDigest(clientId, counterpartyInfo, client),
            IBCClientMigrationMismatch(clientId)
        );
        // The proposer already rejects such clients. This also covers a proposal committed before it did, and a
        // client that has lost its code since.
        require(client.code.length != 0, IBCInvalidClientAddress());

        IAccessManager manager = IAccessManager(IAccessManaged(address(this)).authority());
        require(!manager.isTargetClosed(address(this)), IBCUnauthorizedMigrator(clientId, msg.sender));
        (bool isMember, uint32 executionDelay) =
            manager.hasRole(IBCRolesLib.getLightClientMigratorRole(clientId), migration.proposer);
        require(isMember, IBCUnauthorizedMigrator(clientId, migration.proposer));
        // The proposer needed a non-zero delay to commit the proposal, so a role re-granted without one no longer
        // authorizes it.
        require(executionDelay != 0, IBCClientMigrationDelayRequired(clientId));

        $.counterpartyInfos[clientId] = counterpartyInfo;
        $.clients[clientId] = ILightClient(client);
        delete $.migrations[clientId];

        emit IICS02Client.ICS02ClientMigrated(clientId, counterpartyInfo, client);
    }

    /// @inheritdoc IClientMigrationExecutor
    function cancelClientMigration(string calldata clientId) external onlyDelegated {
        IAccessManager manager = IAccessManager(IAccessManaged(address(this)).authority());
        // `hasRole` only reports the configured execution delay; it does not
        // consume it. Cancellation must remain available when the router is
        // closed, so it cannot use `restricted`/AccessManager.execute, but a
        // delayed PAUSER_ROLE must not gain an unintended immediate path.
        (bool isPauser, uint32 executionDelay) = manager.hasRole(IBCRolesLib.PAUSER_ROLE, msg.sender);
        require(isPauser && executionDelay == 0, IAccessManaged.AccessManagedUnauthorized(msg.sender));

        ICS02ClientStore.ClientMigration storage migration = ICS02ClientStore.load().migrations[clientId];
        require(migration.digest != bytes32(0), IBCClientMigrationNotProposed(clientId));
        bytes32 digest = migration.digest;
        delete ICS02ClientStore.load().migrations[clientId];
        emit IICS02Client.ICS02ClientMigrationCancelled(clientId, digest);
    }

    function _migrationDigest(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        private
        pure
        returns (bytes32)
    {
        return keccak256(abi.encode(clientId, counterpartyInfo.clientId, counterpartyInfo.merklePrefix, client));
    }
}
