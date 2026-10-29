// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";

/// @title SpectreStore
/// @notice ERC-7201 namespaced storage for the SpectreClient light client.
/// @dev SpectreClient is the only writer. The delegatecalled modules read this store
///      (they execute in SpectreClient's storage context) but never write to it.
library SpectreStore {
    /// @notice A per-height snapshot of the pinned validator set, so a proof anchored at a past
    ///         trusted height resolves against the set that was trusted then.
    /// @param validatorsHash The CometBFT validators hash of the pinned set at that height.
    /// @param pointer The SSTORE2 pointer holding the packed validator cache.
    /// @param totalVotingPower The total voting power of the pinned set.
    /// @param entryCount The number of validators in the pinned set.
    /// @param timestamp The consensus-state timestamp at the height where this set was pinned.
    struct PinnedValidatorSetSnapshot {
        bytes32 validatorsHash;
        address pointer;
        uint64 totalVotingPower;
        uint16 entryCount;
        uint128 timestamp;
    }

    /// @notice The SpectreClient on-chain state.
    /// @custom:storage-location erc7201:spectre.storage.SpectreClient
    struct Store {
        ISpectreTendermintMsgs.ClientState clientState;
        mapping(uint64 height => bytes32 hash) consensusStateHashes;
        bytes32 pinnedValidatorsHash;
        address pinnedValidatorSetPointer;
        uint64 pinnedTotalVotingPower;
        uint16 pinnedEntryCount;
        mapping(uint64 height => PinnedValidatorSetSnapshot snapshot) snapshots;
        uint64[] snapshotHeights;
        uint128 pinnedTimestamp;
    }

    /// @notice ERC-7201 slot for the SpectreClient store.
    /// @dev keccak256(abi.encode(uint256(keccak256("spectre.storage.SpectreClient")) - 1)) & ~bytes32(uint256(0xff))
    bytes32 internal constant STORE_SLOT = 0x5e9f695da54d5993169597b18a19a3159f4a62cbfd7f6669a996ca09c6df1700;

    /// @notice Binds and returns the SpectreClient store.
    function load() internal pure returns (Store storage $) {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            $.slot := STORE_SLOT
        }
    }

    /// @notice Returns the stored consensus-state hash at the given height, reverting if absent.
    function getConsensusStateHash(Store storage $, uint64 revisionHeight) internal view returns (bytes32) {
        bytes32 hash = $.consensusStateHashes[revisionHeight];
        require(hash != 0, ISpectreClientErrors.ConsensusStateNotFound());
        return hash;
    }

    /// @notice Returns the snapshot of the pinned validator set as of the current pinned state.
    function currentSnapshot(Store storage $) internal view returns (PinnedValidatorSetSnapshot memory) {
        return PinnedValidatorSetSnapshot({
            validatorsHash: $.pinnedValidatorsHash,
            pointer: $.pinnedValidatorSetPointer,
            totalVotingPower: $.pinnedTotalVotingPower,
            entryCount: $.pinnedEntryCount,
            timestamp: $.pinnedTimestamp
        });
    }

    /// @notice Returns the pinned validator-set snapshot trusted at (or before) the given height.
    /// @dev Binary-searches the sorted `snapshotHeights` for the highest checkpoint <= height.
    function snapshotAt(
        Store storage $,
        uint64 height
    )
        internal
        view
        returns (PinnedValidatorSetSnapshot memory snapshot)
    {
        uint256 len = $.snapshotHeights.length;
        if (len == 0) {
            revert ISpectreClientErrors.ValidatorSetCacheMiss(bytes32(0));
        }
        uint256 left = 0;
        uint256 right = len - 1;
        while (left < right) {
            uint256 mid = (left + right + 1) / 2;
            if ($.snapshotHeights[mid] <= height) {
                left = mid;
            } else {
                right = mid - 1;
            }
        }
        uint64 checkpoint = $.snapshotHeights[left];
        require(checkpoint <= height, ISpectreClientErrors.ValidatorSetCacheMiss(bytes32(0)));
        snapshot = $.snapshots[checkpoint];
        if (snapshot.pointer == address(0)) {
            revert ISpectreClientErrors.ValidatorSetCacheMiss(snapshot.validatorsHash);
        }
    }
}
