// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";

import { IICS02ClientErrors } from "contracts/core/errors/IICS02ClientErrors.sol";
import { IICS02Client, IICS02ClientAccessControlled } from "contracts/core/interfaces/IICS02Client.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";
import {
    IClientMigrationProposer
} from "contracts/core/client-registry/migration/interfaces/IClientMigrationProposer.sol";
import {
    IClientMigrationExecutor
} from "contracts/core/client-registry/migration/interfaces/IClientMigrationExecutor.sol";
import {
    IClientMigrationModule,
    ClientMigrationModuleIds
} from "contracts/core/client-registry/migration/interfaces/IClientMigrationModule.sol";

import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { AccessManagedUpgradeable } from "@openzeppelin-upgradeable/access/manager/AccessManagedUpgradeable.sol";
import { IBCIdentifiers } from "contracts/shared/access/IBCIdentifiers.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { ICS02ClientStore } from "contracts/core/client-registry/ICS02ClientStore.sol";

/// @title ICS02 Client Router
/// @notice This is the ICS02 Light Client Router contract, storing the light clients and their identifiers.
/// @dev Light client migrations are gated by a per-clientId role on the connected AccessManager
/// @dev (see `getLightClientMigratorRole`). The role id is derived from the clientId, so a grant
/// @dev for one client does not authorize migration of any other client. The role must be granted
/// @dev explicitly by the AccessManager admin; it is not auto-assigned in `addClient`.
abstract contract ICS02ClientUpgradeable is IICS02Client, IICS02ClientErrors, AccessManagedUpgradeable {
    IClientMigrationProposer internal immutable CLIENT_MIGRATION_PROPOSER;
    IClientMigrationExecutor internal immutable CLIENT_MIGRATION_EXECUTOR;

    constructor(address clientMigrationProposer, address clientMigrationExecutor) {
        _requireClientMigrationModule(clientMigrationProposer, ClientMigrationModuleIds.PROPOSER);
        _requireClientMigrationModule(clientMigrationExecutor, ClientMigrationModuleIds.EXECUTOR);
        CLIENT_MIGRATION_PROPOSER = IClientMigrationProposer(clientMigrationProposer);
        CLIENT_MIGRATION_EXECUTOR = IClientMigrationExecutor(clientMigrationExecutor);
    }

    function _requireClientMigrationModule(address module, bytes32 expectedModuleId) private view {
        if (module.code.length == 0) revert IBCClientMigrationModuleMissing(module);
        (bool ok, bytes memory result) = module.staticcall(abi.encodeCall(IClientMigrationModule.moduleId, ()));
        if (!ok || result.length != 32) revert IBCClientMigrationModuleNotResponding(module);
        bytes32 actualModuleId = abi.decode(result, (bytes32));
        if (actualModuleId != expectedModuleId) {
            revert IBCClientMigrationModuleMismatch(module, expectedModuleId, actualModuleId);
        }
    }

    /// @notice This function initializes the ICS02Client contract
    /// @dev This function is meant to be called by the initializer of the contract that inherits this.
    /// @param authority The address of the AccessManager contract
    function __ICS02Client_init(address authority) internal onlyInitializing {
        __AccessManaged_init(authority);
    }

    /// @inheritdoc IICS02Client
    function getNextClientSeq() external view returns (uint256) {
        return ICS02ClientStore.load().nextClientSeq;
    }

    /// @notice Generates the next client identifier
    /// @return The next client identifier
    function nextClientId() private returns (string memory) {
        ICS02ClientStore.Layout storage $ = ICS02ClientStore.load();
        // initial client sequence should be 0, hence we use x++ instead of ++x
        // solhint-disable-next-line gas-increment-by-one
        return string.concat(IBCIdentifiers.CLIENT_ID_PREFIX, Strings.toString($.nextClientSeq++));
    }

    /// @inheritdoc IICS02Client
    function getCounterparty(string calldata clientId) public view returns (IICS02ClientMsgs.CounterpartyInfo memory) {
        IICS02ClientMsgs.CounterpartyInfo memory counterpartyInfo = ICS02ClientStore.load().counterpartyInfos[clientId];
        require(bytes(counterpartyInfo.clientId).length != 0, IBCCounterpartyClientNotFound(clientId));

        return counterpartyInfo;
    }

    /// @inheritdoc IICS02Client
    function getClient(string calldata clientId) public view returns (ILightClient) {
        ILightClient client = ICS02ClientStore.load().clients[clientId];
        require(address(client) != address(0), IBCClientNotFound(clientId));

        return client;
    }

    /// @inheritdoc IICS02Client
    function addClient(
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
        returns (string memory)
    {
        string memory clientId = nextClientId();
        _addClient(clientId, counterpartyInfo, client);
        return clientId;
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function addClient(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
        restricted
        returns (string memory)
    {
        require(IBCIdentifiers.validateCustomIBCIdentifier(bytes(clientId)), IBCInvalidLocalClientId());
        _addClient(clientId, counterpartyInfo, client);
        return clientId;
    }

    /// @notice This function adds a client to the client router
    /// @dev This function assumes that the clientId has already been generated and validated.
    /// @param clientId The client identifier
    /// @param counterpartyInfo The counterparty client information
    /// @param client The address of the client contract
    function _addClient(
        string memory clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        private
    {
        ICS02ClientStore.Layout storage $ = ICS02ClientStore.load();
        require(address($.clients[clientId]) == address(0), IBCClientAlreadyExists(clientId));
        require(
            IBCIdentifiers.validateIBCIdentifier(bytes(counterpartyInfo.clientId)), IBCInvalidCounterpartyClientId()
        );
        // `sendPacket` needs only the counterparty, so a client that cannot answer, or a prefix that no packet path
        // can be built from, would still escrow tokens that `ackPacket` and `timeoutPacket` could never refund.
        require(client.code.length != 0, IBCInvalidClientAddress());
        require(counterpartyInfo.merklePrefix.length != 0, IBCInvalidCounterpartyMerklePrefix());

        $.clients[clientId] = ILightClient(client);
        $.counterpartyInfos[clientId] = counterpartyInfo;

        emit ICS02ClientAdded(clientId, counterpartyInfo, client);
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function updateApplicationState(
        string calldata clientId,
        bytes calldata updateMsg
    )
        external
        restricted
        returns (ILightClientMsgs.UpdateResult)
    {
        ILightClientMsgs.UpdateResult result = getClient(clientId).updateApplicationState(updateMsg);
        emit ICS02ClientUpdated(clientId, result);
        return result;
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function updateConsensusState(
        string calldata clientId,
        bytes calldata updateMsg
    )
        external
        restricted
        returns (ILightClientMsgs.UpdateResult)
    {
        ILightClientMsgs.UpdateResult result = getClient(clientId).updateConsensusState(updateMsg);
        emit ICS02ClientUpdated(clientId, result);
        return result;
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function migrateClient(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
    {
        // ABI-compatible alias for executeClientMigration. It succeeds only for
        // a mature, digest-matching proposal created through the delayed API.
        _delegateClientMigration(
            address(CLIENT_MIGRATION_EXECUTOR), IClientMigrationExecutor.executeClientMigration.selector
        );
    }

    function proposeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
    {
        _delegateClientMigration(
            address(CLIENT_MIGRATION_PROPOSER), IClientMigrationProposer.proposeClientMigration.selector
        );
    }

    function executeClientMigration(
        string calldata clientId,
        IICS02ClientMsgs.CounterpartyInfo calldata counterpartyInfo,
        address client
    )
        external
    {
        _delegateClientMigration(
            address(CLIENT_MIGRATION_EXECUTOR), IClientMigrationExecutor.executeClientMigration.selector
        );
    }

    function cancelClientMigration(string calldata clientId) external {
        _delegateClientMigration(
            address(CLIENT_MIGRATION_EXECUTOR), IClientMigrationExecutor.cancelClientMigration.selector
        );
    }

    function getClientMigration(string calldata clientId)
        external
        view
        returns (bytes32 digest, uint48 executeAfter, uint48 expireAfter, address proposer)
    {
        ICS02ClientStore.ClientMigration memory migration = ICS02ClientStore.load().migrations[clientId];
        return (migration.digest, migration.executeAfter, migration.expireAfter, migration.proposer);
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function getLightClientMigratorRole(string calldata clientId) external pure returns (uint64) {
        return IBCRolesLib.getLightClientMigratorRole(clientId);
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function submitMisbehaviour(string calldata clientId, bytes calldata misbehaviourMsg) external restricted {
        getClient(clientId).misbehaviour(misbehaviourMsg);
        emit ICS02MisbehaviourSubmitted(clientId);
    }

    /// @inheritdoc IICS02ClientAccessControlled
    function unfreezeClient(string calldata clientId) external restricted {
        getClient(clientId).unfreeze();
        emit ICS02ClientUnfrozen(clientId);
    }

    function _delegateClientMigration(address target, bytes4 selector) private {
        // This terminal assembly block may overwrite Solidity-managed memory because it
        // returns or reverts directly and never resumes Solidity execution.
        // solhint-disable-next-line no-inline-assembly
        assembly {
            mstore(0, selector)
            // Copying calldatasize() bytes from offset 4 zero-pads four unused bytes past
            // calldata, outside the delegatecall's input range, and saves router bytecode.
            calldatacopy(4, 4, calldatasize())
            let ok := delegatecall(gas(), target, 0, calldatasize(), 0, 0)
            returndatacopy(0, 0, returndatasize())
            switch ok
            case 0 { revert(0, returndatasize()) }
            default { return(0, returndatasize()) }
        }
    }
}
