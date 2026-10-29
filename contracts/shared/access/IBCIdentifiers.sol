// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Bytes } from "@openzeppelin-contracts/utils/Bytes.sol";

/// @title IBC Identifiers
/// @notice Utilities for validating IBC identifiers
library IBCIdentifiers {
    /// @notice Prefix for universal client identifiers
    string internal constant CLIENT_ID_PREFIX = "client-";

    /// @notice Prefix for channel identifiers
    /// @dev Only used to prevent channel ids from being used
    string private constant CHANNEL_ID_PREFIX = "channel-";

    /// @dev Bit positions for `.`, `_`, `+`, `-`, `#`, `[`, `]`, `<`, and `>`.
    uint256 private constant VALID_SPECIAL_CHARACTER_MASK = 0xa80000005000680800000000;

    /// @notice validateCustomIBCIdentifier checks if a custom identifier is valid
    /**
     * @dev validateCustomIdentifier validates a custom identifier string
     *     check that the string does not start with "channel-" or "client-"
     *     check if the string consist of characters in one of the following categories only:
     *     - Alphanumeric
     *     - `.`, `_`, `+`, `-`, `#`
     *     - `[`, `]`, `<`, `>`
     */
    /// @custom:url
    /// https://github.com/hyperledger-labs/yui-ibc-solidity/blob/49d88ae8151a92e086e6ca7d27a2d3651889edff/contracts/core/26-router/IBCModuleManager.sol#L123
    /// @param customId The custom identifier
    /// @return True if the custom identifier is valid
    function validateCustomIBCIdentifier(bytes memory customId) internal pure returns (bool) {
        if (!validateIBCIdentifier(customId)) {
            return false;
        }
        if (hasPrefix(customId, bytes(CHANNEL_ID_PREFIX)) || hasPrefix(customId, bytes(CLIENT_ID_PREFIX))) {
            return false;
        }
        return true;
    }

    /// @notice Validates an identifier used in a counterparty field.
    /// @dev Unlike custom local identifiers, counterparty identifiers may use the canonical `client-` prefix.
    ///      The validation still rejects path separators and all characters outside the IBC identifier alphabet.
    /// @param identifier The identifier to validate
    /// @return True if the identifier is valid
    function validateIBCIdentifier(bytes memory identifier) internal pure returns (bool) {
        if (identifier.length < 4 || identifier.length > 128) {
            return false;
        }
        /* solhint-disable gas-strict-inequalities */
        unchecked {
            for (uint256 i = 0; i < identifier.length; ++i) {
                uint256 c = uint256(uint8(identifier[i]));
                // ASCII case folding lets one range cover both A-Z and a-z. The bitmask handles only
                // the nine punctuation bytes named by `VALID_SPECIAL_CHARACTER_MASK`.
                uint256 lowercase = c | 0x20;
                if (
                    (lowercase >= 0x61 && lowercase <= 0x7A) || (c >= 0x30 && c <= 0x39)
                        || (VALID_SPECIAL_CHARACTER_MASK & (uint256(1) << c)) != 0
                ) {
                    continue;
                }
                return false;
            }
        }
        /* solhint-enable gas-strict-inequalities */
        return true;
    }

    /// @notice Checks whether `value` begins with `prefix`.
    function hasPrefix(bytes memory value, bytes memory prefix) internal pure returns (bool) {
        if (value.length < prefix.length) {
            return false;
        }
        return keccak256(Bytes.slice(value, 0, prefix.length)) == keccak256(prefix);
    }
}
