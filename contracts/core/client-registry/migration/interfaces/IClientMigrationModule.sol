// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @notice Canonical identities for migration modules accepted by the router.
/// @dev The preimages are compatibility identifiers, checked once in the router implementation's
///      constructor. Changing them changes the module IDs: a router implementation built with new
///      IDs must be deployed against migration modules built from the same source.
library ClientMigrationModuleIds {
    bytes32 internal constant PROPOSER = keccak256("spectre.client-migration.proposer.v1");
    bytes32 internal constant EXECUTOR = keccak256("spectre.client-migration.executor.v1");
}

/// @notice Runtime identity exposed by a migration delegatecall module.
interface IClientMigrationModule {
    function moduleId() external pure returns (bytes32);
}
