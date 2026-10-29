// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.28;

import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { IICS20Errors } from "contracts/apps/ics20/errors/IICS20Errors.sol";
import { IBCIdentifiers } from "contracts/shared/access/IBCIdentifiers.sol";

/// @title ICS20 Library
/// @notice This library provides utility functions for the ICS20Tranfer, including versioning, encoding, and denom
/// handling.
// This library was originally copied, with minor adjustments, from https://github.com/hyperledger-labs/yui-ibc-solidity
// It has since been modified heavily (e.g. replacing JSON with ABI encoding, adding new functions, etc.)
library ICS20Lib {
    /// @notice ICS20_VERSION is the version string for ICS20 packet data.
    string internal constant ICS20_VERSION = "ics20-1";

    /// @notice ICS20_ENCODING is the encoding string for ICS20 packet data.
    string internal constant ICS20_ENCODING = "application/x-solidity-abi";

    /// @notice DEFAULT_PORT_ID is the default port id for ICS20.
    string internal constant DEFAULT_PORT_ID = "transfer";

    /// @notice SUCCESSFUL_ACKNOWLEDGEMENT_JSON is the JSON bytes for a successful acknowledgement.
    bytes internal constant SUCCESSFUL_ACKNOWLEDGEMENT_JSON = bytes("{\"result\":\"AQ==\"}");

    /// @notice KECCAK256_ICS20_VERSION is the keccak256 hash of the ICS20_VERSION.
    bytes32 internal constant KECCAK256_ICS20_VERSION = keccak256(bytes(ICS20_VERSION));

    /// @notice KECCAK256_ICS20_ENCODING is the keccak256 hash of the ICS20_ENCODING.
    bytes32 internal constant KECCAK256_ICS20_ENCODING = keccak256(bytes(ICS20_ENCODING));

    /// @notice KECCAK256_DEFAULT_PORT_ID is the keccak256 hash of the DEFAULT_PORT_ID.
    bytes32 internal constant KECCAK256_DEFAULT_PORT_ID = keccak256(bytes(DEFAULT_PORT_ID));

    /// @notice mustHexStringToAddress converts a hex string to an address and reverts on failure.
    /// @param addrHexString hex address string
    /// @return address the converted address
    function mustHexStringToAddress(string memory addrHexString) internal pure returns (address) {
        (bool success, address addr) = Strings.tryParseAddress(addrHexString);
        require(success, IICS20Errors.ICS20InvalidAddress(addrHexString));
        return addr;
    }

    /// @notice hasPrefix checks a denom for a prefix
    /// @param denomBz the denom to check
    /// @param prefix the prefix to check with
    /// @return true if `denomBz` has the prefix `prefix`
    function hasPrefix(bytes memory denomBz, bytes memory prefix) internal pure returns (bool) {
        return IBCIdentifiers.hasPrefix(denomBz, prefix);
    }

    /// @notice getDenomPrefix returns an ibc path prefix
    /// @param portId Port
    /// @param clientId client
    /// @return Denom prefix
    function getDenomPrefix(string memory portId, string calldata clientId) internal pure returns (bytes memory) {
        return abi.encodePacked(portId, "/", clientId, "/");
    }

    /// @notice hasDenomPrefix checks whether `denomBz` is prefixed by exactly
    /// portId + "/" + clientId + "/" and has at least one additional byte of base
    /// denom after the prefix. Using the full portId/clientId/ segment as the
    /// unit of comparison (rather than a raw byte prefix) ensures the boundary
    /// always falls on a path separator — latent protection against any future
    /// clientId format that could otherwise be a byte-prefix of another clientId.
    /// @param denomBz the denom bytes to check
    /// @param prefix the exact portId/clientId/ prefix bytes (from getDenomPrefix)
    /// @return true iff denomBz starts with the prefix AND has content after it
    function hasDenomPrefix(bytes memory denomBz, bytes memory prefix) internal pure returns (bool) {
        return denomBz.length > prefix.length && IBCIdentifiers.hasPrefix(denomBz, prefix);
    }
}
