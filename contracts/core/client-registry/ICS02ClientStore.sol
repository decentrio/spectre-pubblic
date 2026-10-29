// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";

/// @title ICS02 Client Store
/// @notice Shared ERC-7201 storage access for the client router and its migration modules.
library ICS02ClientStore {
    struct ClientMigration {
        bytes32 digest;
        uint48 executeAfter;
        uint48 expireAfter;
        address proposer;
    }

    /// @notice Storage of the ICS02Client contract.
    /// @dev The first three fields retain their original order and types. Migration state is appended as
    ///      a new top-level mapping, preserving storage written by earlier proxy implementations.
    /// @custom:storage-location erc7201:ibc.storage.ICS02Client
    struct Layout {
        mapping(string clientId => ILightClient) clients;
        mapping(string clientId => IICS02ClientMsgs.CounterpartyInfo info) counterpartyInfos;
        uint256 nextClientSeq;
        mapping(string clientId => ClientMigration migration) migrations;
    }

    /// @notice ERC-7201 slot for the ICS02Client storage.
    /// @dev keccak256(abi.encode(uint256(keccak256("ibc.storage.ICS02Client")) - 1)) & ~bytes32(uint256(0xff))
    bytes32 internal constant STORAGE_SLOT = 0x515a8336edcaab4ae6524d41223c1782132890f89189ba6632107a7b5a449600;

    function load() internal pure returns (Layout storage $) {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            $.slot := STORAGE_SLOT
        }
    }
}
