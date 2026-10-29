// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IClientMigrationModule } from "contracts/core/client-registry/migration/interfaces/IClientMigrationModule.sol";

/// @title Client Migration Proposer Interface
/// @notice Delegatecall entrypoint for committing a governed client migration.
interface IClientMigrationProposer is IClientMigrationModule {
    function proposeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external;
}
