// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IICS02ClientErrors
/// @notice Interface for ICS02Client errors
interface IICS02ClientErrors {
    /// @notice Invalid client id
    /// @param clientId the invalid client identifier
    error IBCInvalidClientId(string clientId);

    /// @notice The requested local client identifier is malformed or reserved.
    error IBCInvalidLocalClientId();

    /// @notice The counterparty client identifier is malformed.
    error IBCInvalidCounterpartyClientId();

    /// @notice The counterparty Merkle prefix has no elements, so no packet path could be proven against it.
    error IBCInvalidCounterpartyMerklePrefix();

    /// @notice The light client address is zero or holds no contract code.
    error IBCInvalidClientAddress();

    /// @notice Proposed counterparty information differs from the client's existing binding.
    error IBCCounterpartyMismatch();

    /// @notice Client not found
    /// @param clientId client identifier
    error IBCClientNotFound(string clientId);

    /// @notice Counterparty client not found
    /// @param counterpartyClientId counterparty client identifier
    error IBCCounterpartyClientNotFound(string counterpartyClientId);

    /// @notice IBC client identifier already exists
    /// @param clientId client identifier
    error IBCClientAlreadyExists(string clientId);

    /// @notice Unreachable code
    error Unreachable();

    /// @notice Caller is not granted the per-clientId light client migrator role for this client
    /// @param clientId client identifier
    /// @param caller the address that attempted to migrate the client
    error IBCUnauthorizedMigrator(string clientId, address caller);

    /// @notice A migration proposal is not ready to execute.
    error IBCClientMigrationNotReady(string clientId, uint256 executeAfter);

    /// @notice A migration proposal is already active for this client.
    error IBCClientMigrationAlreadyProposed(string clientId);

    /// @notice No migration proposal exists for this client.
    error IBCClientMigrationNotProposed(string clientId);

    /// @notice A migration proposal has passed its execution window.
    error IBCClientMigrationExpired(string clientId, uint256 expireAfter);

    /// @notice The supplied migration does not match the committed proposal.
    error IBCClientMigrationMismatch(string clientId);

    /// @notice The migration role must carry a non-zero execution delay.
    error IBCClientMigrationDelayRequired(string clientId);

    /// @notice A configured migration module address has no deployed code.
    error IBCClientMigrationModuleMissing(address module);

    /// @notice A configured contract does not answer `moduleId()` with a single `bytes32`.
    /// @dev The call reverted or returned data of the wrong length: the address holds an unrelated contract.
    error IBCClientMigrationModuleNotResponding(address module);

    /// @notice A configured contract identifies as a different migration module than required.
    error IBCClientMigrationModuleMismatch(address module, bytes32 expectedModuleId, bytes32 actualModuleId);

    /// @notice A migration module was called directly instead of through the router.
    error IBCClientMigrationDirectCallNotAllowed();
}
