// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";

/// @title ICS02 Client Access Controlled Interface
/// @notice Interface for the access controlled functions of the IBC v2 light client router
interface IICS02ClientAccessControlled {
    /// @notice Adds a client to the client router.
    /// @dev Only a caller with `ID_CUSTOMIZER_ROLE` can call this function. The client must hold code and the
    /// counterparty Merkle prefix must have at least one element. Clients are not deduplicated: see the default
    /// overload.
    /// @param clientId The custom client identifier
    /// @param counterpartyInfo The counterparty client information
    /// @param client The address of the client contract
    /// @return The client identifier
    function addClient(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
        returns (string memory);

    /// @notice Advances the block state of the client with the given client identifier.
    /// @dev Can only be called with the `RELAYER_ROLE`. The frequent, cheap update path.
    /// @param clientId The client identifier
    /// @param updateMsg The encoded update message e.g., a Groth16 proof.
    /// @return The result of the update operation
    function updateApplicationState(
        string calldata clientId,
        bytes calldata updateMsg
    )
        external
        returns (ILightClientMsgs.UpdateResult);

    /// @notice Rotates the pinned validator set of the client with the given client identifier.
    /// @dev Can only be called with the `RELAYER_ROLE`. The rare, heavy update path.
    /// @param clientId The client identifier
    /// @param updateMsg The encoded update message e.g., a Groth16 proof.
    /// @return The result of the update operation
    function updateConsensusState(
        string calldata clientId,
        bytes calldata updateMsg
    )
        external
        returns (ILightClientMsgs.UpdateResult);

    /// @notice Executes a previously proposed and matured client migration.
    /// @dev Retained for ABI compatibility; equivalent to `executeClientMigration`.
    /// @param clientId The client identifier of the client to migrate
    /// @param counterpartyInfo The counterparty information, which must match the existing client binding
    /// @param client The address of the new client contract
    function migrateClient(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external;

    /// @notice Commits a delayed migration for a specific client.
    /// @dev The caller must hold the delayed per-client role returned by
    /// `getLightClientMigratorRole(clientId)`.
    /// @param counterpartyInfo The counterparty information, which must match the existing client binding
    function proposeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external;

    /// @notice Executes a matured migration proposal. Anyone may submit the execution transaction.
    /// @param counterpartyInfo The counterparty information committed by the proposal
    function executeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external;

    /// @notice Cancels a pending migration through the configured pause authority.
    function cancelClientMigration(string calldata clientId) external;

    /// @notice Returns the committed migration details.
    function getClientMigration(string calldata clientId)
        external
        view
        returns (bytes32 digest, uint48 executeAfter, uint48 expireAfter, address proposer);

    /// @notice Submits misbehaviour to the client with the given client identifier.
    /// @dev Can only be called with the `MISBEHAVIOUR_SUBMITTER_ROLE`.
    /// @param clientId The client identifier
    /// @param misbehaviourMsg The misbehaviour message
    function submitMisbehaviour(string calldata clientId, bytes calldata misbehaviourMsg) external;

    /// @notice Unfreezes a previously frozen client.
    /// @dev Can only be called with the `ADMIN_ROLE` (governance recovery path).
    /// @param clientId The client identifier of the frozen client
    function unfreezeClient(string calldata clientId) external;

    /// @notice Returns the AccessManager role id that authorizes migration of a specific client.
    /// @dev The id is unique per clientId, so granting this role only authorizes migration
    /// @dev of the matching client (a single grant cannot repoint arbitrary clients).
    /// @param clientId The client identifier
    /// @return The AccessManager role id to grant for per-clientId migration rights
    function getLightClientMigratorRole(string calldata clientId) external pure returns (uint64);
}

/// @title ICS02 Light Client Router Interface
/// @notice Interface for the IBC v2 light client router
interface IICS02Client is IICS02ClientAccessControlled {
    /// @notice Returns the counterparty client information given the client identifier.
    /// @param clientId The client identifier
    /// @return The counterparty client information
    function getCounterparty(string calldata clientId) external view returns (IICS02ClientMsgs.CounterpartyInfo memory);

    /// @notice Returns the address of the client contract given the client identifier.
    /// @param clientId The client identifier
    /// @return The address of the client contract
    function getClient(string calldata clientId) external view returns (ILightClient);

    /// @notice Returns the next client sequence number.
    /// @dev This function can be used to determine when to stop iterating over clients.
    /// @return The next client sequence number
    function getNextClientSeq() external view returns (uint256);

    /// @notice Adds a client to the client router.
    /// @dev Anyone can call this function. The client must hold code and the counterparty Merkle prefix must have
    /// at least one element. Clients are not deduplicated: a second local client that names the same counterparty
    /// client id shares that counterparty's receipt and acknowledgement paths, so a packet sent through it can be
    /// acknowledged with an unrelated packet's acknowledgement and cannot time out. Use only client identifiers
    /// that were registered by the operator.
    /// @param counterpartyInfo The counterparty client information
    /// @param client The address of the client contract
    /// @return The client identifier
    function addClient(
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
        returns (string memory);

    // ============ Events ============

    /// @notice Emitted when a new client is added to the client router.
    /// @param clientId The newly created client identifier
    /// @param counterpartyInfo The counterparty client information
    /// @param client The address of the client contract
    event ICS02ClientAdded(string clientId, IICS02ClientMsgs.CounterpartyInfo counterpartyInfo, address client);

    /// @notice Emitted when a client is migrated to a new client.
    /// @param clientId The client identifier of the migrated client
    /// @param counterpartyInfo The new counterparty client information
    /// @param client The address of the new client contract
    event ICS02ClientMigrated(string clientId, IICS02ClientMsgs.CounterpartyInfo counterpartyInfo, address client);

    event ICS02ClientMigrationProposed(
        string indexed clientId, bytes32 indexed digest, uint48 executeAfter, uint48 expireAfter, address proposer
    );
    event ICS02ClientMigrationCancelled(string indexed clientId, bytes32 indexed digest);

    /// @notice Emitted when a client is updated.
    /// @param clientId The client identifier of the updated ILightClientMsgs
    /// @param result The result of the update operation
    event ICS02ClientUpdated(string clientId, ILightClientMsgs.UpdateResult result);

    /// @notice Emitted when a misbehaviour is submitted to a client and the client is frozen.
    /// @param clientId The client identifier of the frozen client
    event ICS02MisbehaviourSubmitted(string clientId);

    /// @notice Emitted when a frozen client is unfrozen via governance recovery.
    /// @param clientId The client identifier of the unfrozen client
    event ICS02ClientUnfrozen(string clientId);
}
