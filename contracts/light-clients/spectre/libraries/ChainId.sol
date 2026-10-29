// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";

/// @title ChainId
/// @notice Parses a Cosmos-style chain identifier `<prefix>-<revision>` into its
///         components. Shared by `SpectreClient.sol`, `UpdateClient.sol` and
///         `Misbehaviour.sol` so the logic has a single source of truth and the
///         per-call cost is paid only when the parsed value cannot be cached
///         on chain.
library ChainId {
    error InvalidChainIdLength();
    error InvalidChainPrefixLength();

    /// @dev Parse a chain ID of the form `<prefix>-<revision>`.
    /// Returns `revisionNumber = 0` when:
    ///   - the input has no `-`, or
    ///   - the suffix has a leading zero followed by more digits ("01"), or
    ///   - the suffix contains any non-digit, or
    ///   - the suffix overflows `uint64`.
    /// Reverts only when a hard length constraint is violated.
    function get(string memory id) internal pure returns (ISpectreTendermintMsgs.ChainId memory) {
        bytes memory b = bytes(id);
        if (b.length == 0 || b.length >= 64) revert InvalidChainIdLength();

        // Find last '-' without unsigned underflow.
        uint256 dashPos = type(uint256).max;
        for (uint256 i = b.length; i > 0; i--) {
            if (b[i - 1] == 0x2D) {
                dashPos = i - 1;
                break;
            }
        }
        if (dashPos == type(uint256).max) {
            return ISpectreTendermintMsgs.ChainId({ id: id, revisionNumber: 0 });
        }

        // Reject leading zero in revision (except the single character "0").
        if (b[dashPos + 1] == 0x30 && b.length - dashPos > 2) {
            return ISpectreTendermintMsgs.ChainId({ id: id, revisionNumber: 0 });
        }

        // Parse revision in place. Non-digit or uint64 overflow ⇒ revision 0.
        uint64 rev = 0;
        for (uint256 i = dashPos + 1; i < b.length; i++) {
            uint8 c = uint8(b[i]);
            if (c < 0x30 || c > 0x39) {
                return ISpectreTendermintMsgs.ChainId({ id: id, revisionNumber: 0 });
            }
            // Detect uint64 overflow manually since Solidity 0.8 checked
            // arithmetic would otherwise panic on the legitimate "revision
            // string exceeds uint64.max" input.
            unchecked {
                uint64 next = rev * 10 + uint64(c - 0x30);
                if (next < rev) {
                    return ISpectreTendermintMsgs.ChainId({ id: id, revisionNumber: 0 });
                }
                rev = next;
            }
        }

        // Prefix length constraint: longest valid identifier is
        // `{prefix}-{u64::MAX}` (suffix up to 20 chars + '-'), so prefix < 43.
        if (dashPos <= 1 || dashPos >= 43) revert InvalidChainPrefixLength();
        return ISpectreTendermintMsgs.ChainId({ id: id, revisionNumber: rev });
    }
}
