// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title ISpectreClient
/// @notice Interface for the Spectre light client.
interface ISpectreClient {
    /// @notice Emitted when the application state (appHash / latest height) advances.
    /// @param height The new latest revision height.
    event ClientUpdated(uint64 height);

    /// @notice Emitted when the pinned validator set is rotated by updateConsensusState.
    /// @param height The height whose `nextValidatorsHash` the new set was verified against.
    /// @param validatorsHash The hash of the newly pinned validator set.
    event ConsensusStateUpdated(uint64 height, bytes32 validatorsHash);

    /// @notice Emitted when the client freezes, either by detecting conflicting
    /// state during an update or via a submitted misbehaviour proof.
    event ClientFrozen();

    /// @notice Emitted when the client is unfrozen by the admin.
    event ClientUnfrozen();

    /// @notice The role identifier for the misbehaviour submitter role
    /// @dev If `address(0)` has this role, then anyone can submit misbehaviour reports.
    /// @dev This role is distinct from PROOF_SUBMITTER_ROLE to allow separate access control.
    /// @return The role identifier
    function MISBEHAVIOUR_SUBMITTER_ROLE() external view returns (bytes32);

    /// @notice Unfreezes a previously frozen client.
    /// @dev Can only be called by an address with the `DEFAULT_ADMIN_ROLE`.
    function unfreeze() external;

    /// @notice Returns pinned validator metadata.
    /// @return indices      Validator indices in the pinned set, sorted ascending.
    /// @return pubkeys      Ed25519 public keys for each pinned validator.
    /// @return votingPowers Voting power for each pinned validator.
    function getPinnedValidatorSet()
        external
        view
        returns (uint32[] memory indices, bytes32[] memory pubkeys, uint64[] memory votingPowers);

    /// @notice Returns the CometBFT validators hash of the currently pinned validator set.
    /// @dev The relayer compares this against a header's nextValidatorsHash to decide whether an
    ///      update rotates the validator set (updateConsensusState) or only advances the block
    ///      state (updateApplicationState).
    /// @return The pinned validators hash.
    function getPinnedValidatorsHash() external view returns (bytes32);

    /// @notice Returns the hash of the consensus state trusted at `revisionHeight`.
    /// @dev Reverts when the client has no consensus state at that height.
    ///      This lets an operator verify that a live chain still matches the state this client
    ///      stored before reusing the client after a restart.
    /// @param revisionHeight The Cosmos revision height of the trusted consensus state.
    /// @return The `keccak256(abi.encode(consensusState))` stored by the client.
    function getConsensusStateHash(uint64 revisionHeight) external view returns (bytes32);

    /// @notice Returns the height and consensus timestamp of the currently pinned validator-set snapshot.
    /// @dev Both update paths reject the client once `block.timestamp - timestamp / 1e9` reaches the
    ///      trusting period, and only `updateConsensusState` refreshes it — `updateApplicationState`
    ///      advances `latestHeight` without touching the snapshot. Off-chain expiry tracking must
    ///      therefore anchor here, not on the latest height's header time.
    /// @return height    The revision height the snapshot was pinned at.
    /// @return timestamp The pinned consensus state's timestamp, in nanoseconds.
    function getPinnedSnapshot() external view returns (uint64 height, uint128 timestamp);
}
