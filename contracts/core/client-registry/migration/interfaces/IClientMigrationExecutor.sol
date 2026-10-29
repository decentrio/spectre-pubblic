// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IClientMigrationModule } from "contracts/core/client-registry/migration/interfaces/IClientMigrationModule.sol";

/// @title Client Migration Executor Interface
/// @notice Delegatecall entrypoints for executing or cancelling a governed client migration.
interface IClientMigrationExecutor is IClientMigrationModule {
    function executeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external;

    function cancelClientMigration(string calldata clientId) external;
}
