// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title ISpectreClientErrors
/// @notice Interface for errors of the SpectreClient contract and its modules.
interface ISpectreClientErrors {
    /// @notice The error that is returned when attempting to update the client with a non-monotonic height.
    /// @param latestHeight The latest height in client state.
    /// @param updateHeight The height being updated to.
    error NonMonotonicHeightUpdate(uint64 latestHeight, uint64 updateHeight);

    /// @notice The error that is returned when the client state is frozen.
    error FrozenClientState();

    /// @notice The error that is returned when attempting to unfreeze a client that is not frozen.
    error ClientNotFrozen();

    /// @notice The error that is returned when a proof is in the future.
    /// @param now The current timestamp in seconds.
    /// @param proofTimestamp The timestamp in the proof in seconds.
    error ProofIsInTheFuture(uint256 now, uint256 proofTimestamp);

    /// @notice The error that is returned when a proof is too old.
    /// @param now The current timestamp in seconds.
    /// @param proofTimestamp The timestamp in the proof in seconds.
    error ProofIsTooOld(uint256 now, uint256 proofTimestamp);

    /// @notice The error that is returned when the chain ID does not match the expected value.
    /// @param expected The expected chain ID.
    /// @param actual The actual chain ID.
    error ChainIdMismatch(string expected, string actual);

    /// @notice The error that is returned when the trusting period is longer than the unbonding period.
    /// @param trustingPeriod The trusting period in seconds.
    /// @param unbondingPeriod The unbonding period in seconds.
    error TrustingPeriodTooLong(uint256 trustingPeriod, uint256 unbondingPeriod);

    /// @notice The error that is returned when the consensus state hash does not match the expected value.
    /// @param expected The expected consensus state hash.
    /// @param actual The actual consensus state hash.
    error ConsensusStateHashMismatch(bytes32 expected, bytes32 actual);

    /// @notice The error that is returned when the consensus state is not found.
    error ConsensusStateNotFound();

    /// @notice The error that is returned when the length of a value is out of range.
    /// @param length The length of the value.
    /// @param min The minimum length of the value.
    /// @param max The maximum length of the value.
    error LengthIsOutOfRange(uint256 length, uint256 min, uint256 max);

    /// @notice The error that is returned when the key-value pair's value does not match the expected value.
    /// @param expected The expected value.
    /// @param actual The actual value.
    error MembershipProofValueMismatch(bytes expected, bytes actual);

    /// @notice The error that is returned when the key-value pair's path is not contained in the proof.
    /// @param path The path of the key-value pair.
    error MembershipProofKeyNotFound(bytes[] path);

    /// @notice The error that is returned when the consensus state root does not match the expected value.
    /// @param expected The expected consensus state root.
    /// @param actual The actual consensus state root.
    error ConsensusStateRootMismatch(bytes32 expected, bytes32 actual);

    /// @notice The error that is returned when the membership type is unknown.
    /// @param membershipType The unknown membership type.
    error UnknownMembershipType(uint8 membershipType);

    /// @notice Returned when the feature is not supported.
    error FeatureNotSupported();

    /// @notice Returned when the membership value is empty.
    error EmptyValue();

    /// @notice Returned when standalone misbehaviour headers are not at the same height.
    /// @param height1 first header height.
    /// @param height2 second header height.
    error MismatchedMisbehaviourHeaderHeights(uint64 height1, uint64 height2);

    /// @notice mismatched revision heights.
    /// @param expected height.
    /// @param actual height.
    error MismatchedRevisionHeights(uint64 expected, uint64 actual);

    /// @notice invalid header height.
    /// @param height invalid value.
    error InvalidHeaderHeight(uint64 height);

    /// @notice vaidator hashes mismatch.
    /// @param expected validator hashes.
    /// @param actual validator hashes.
    error MismatchedValidatorHashes(bytes32 expected, bytes32 actual);

    /// @notice failed to verify header.
    /// @param description.
    error FailedToVerifyHeader(string description);

    /// @notice duration since consensus state exceeds than trusting period.
    /// @param durationSinceConsensusState.
    /// @param trustingPeriod.
    error InsufficientTrustingPeriod(uint128 durationSinceConsensusState, uint128 trustingPeriod);

    error InvalidConsensusStateTimestamp(uint128 timestamp);

    /// @notice Returned when the Groth16 proof verification fails.
    error ProofVerificationFailed();

    /// @notice Returned when any of the per-slot arrays in a BatchProof has a length
    ///         different from the declared bucket size.
    error BatchLengthMismatch();

    /// @notice Returned when the active validator set exceeds this light client's hard cache limit.
    /// @param validatorCount active validators in the supplied set.
    /// @param maxValidatorCount maximum active validators supported by this client.
    error ValidatorCountExceedsLimit(uint256 validatorCount, uint256 maxValidatorCount);

    /// @notice Returned when a signer index in the update message exceeds the proposed validator set size.
    /// @param index the out-of-range signer index.
    error SignerIndexOutOfRange(uint32 index);

    /// @notice Returned when the pubkey bundled in the update message does not
    ///         match the validator it claims to represent.
    /// @param index the validator index with the mismatched pubkey.
    error PubkeyMismatch(uint32 index);

    /// @notice Returned when the accumulated voting power of the unique signers is
    ///         not strictly greater than 2/3 of the total voting power.
    /// @param accumulated summed voting power of unique signers.
    /// @param total total voting power of the proposed validator set.
    error InsufficientVotingPower(uint64 accumulated, uint64 total);

    /// @notice Returned when the same active validator index appears more than
    ///         once in signerIndices — defends against malicious calldata that
    ///         tries to inflate quorum past what active gating allows.
    /// @param index the validator index that appears multiple times.
    error DuplicateSigner(uint32 index);

    /// @notice Returned when an active proof signer does not correspond to a
    ///         COMMIT slot at the same validator index in the header commit.
    /// @param index the validator index proven as active but not present in commitSigs.
    error ProofSignerCommitSigMismatch(uint32 index);

    /// @notice Returned when the commit slot cited by signerIndices[i] belongs to a different
    ///         validator than the pinned-set entry cited by pinnedValidatorIndices[i]. Without
    ///         this the two index arrays are independent, and a proof can claim one validator's
    ///         voting power while pointing at another validator's commit slot (ZK-09).
    /// @param signerIndex the commitSigs index whose validator address does not match.
    error ProofSignerValidatorMismatch(uint32 signerIndex);

    /// @notice Returned when the contract is asked to reuse a cached validator
    ///         set but no cache entry exists for the requested hash.
    /// @param validatorsHash the missing validator-set hash.
    error ValidatorSetCacheMiss(bytes32 validatorsHash);

    /// @notice Returned when a cached validator set is internally inconsistent.
    /// @param validatorsHash the corrupted validator-set hash.
    error CachedValidatorSetCorrupted(bytes32 validatorsHash);

    /// @notice Returned when an active signer is not present in the cached quorum subset.
    /// @param validatorsHash the validator-set hash used for the cache lookup.
    /// @param index the missing validator index.
    error CachedSignerNotFound(bytes32 validatorsHash, uint32 index);

    /// @notice Returned when a delegatecall-only module is invoked directly.
    error DirectCallNotAllowed();

    /// @notice Deprecated: retained until the next ABI cleanup for consumers that already generated
    ///         bindings from the audit branch. The app-state path allows the pinned validator set to
    ///         lag `trustedConsensusState.nextValidatorsHash`; freshness is time-based, not an
    ///         identity check against the trusted height's next validator set.
    /// @param expectedNextValidatorsHash formerly trustedConsensusState.nextValidatorsHash.
    /// @param actualPinnedValidatorsHash formerly the hash of the validator set pinned in storage.
    error PinnedValidatorSetStale(bytes32 expectedNextValidatorsHash, bytes32 actualPinnedValidatorsHash);

    /// @notice Returned when the constructor's `initialPinnedValidatorSet` does not hash to the
    ///         `nextValidatorsHash` committed by the genesis consensus state — i.e. the deployer
    ///         tried to pin a validator set unrelated to the genesis state being trusted.
    /// @param expected consensusState.nextValidatorsHash from the genesis consensus state.
    /// @param actual Header.hashValSet(initialPinnedValidatorSet).
    error GenesisPinnedValidatorSetMismatch(bytes32 expected, bytes32 actual);

    /// @notice Returned when a validator entry in a pinned/proposed validator set has zero
    ///         voting power — such an entry can never contribute to quorum and only bloats the
    ///         cache, and a zero-power entry is a common signature of a malformed genesis set.
    /// @param index the index of the zero-power validator entry.
    error ZeroVotingPower(uint256 index);

    /// @notice Returned when the same pubkey appears at two different indices in a validator
    ///         set being pinned. Left unrejected, a repeated pubkey could let one signature be
    ///         counted multiple times toward the >2/3 quorum threshold (each index is a distinct
    ///         accounting slot in `SpectreClient._verifyQuorum`).
    /// @param firstIndex the first (lower) index at which the pubkey appears.
    /// @param secondIndex the second (higher) index at which the same pubkey appears again.
    error DuplicateValidatorPubkey(uint256 firstIndex, uint256 secondIndex);

    /// @notice Returned when a pinned validator index would overflow the 256-bit `seenPinned`
    ///         dup-signer bitmask used by `SpectreClient._verifyQuorum`. Defense-in-depth: this
    ///         should be unreachable while `ValidatorSetLib.MAX_VALIDATOR_COUNT < 256`, but reverts
    ///         loudly instead of silently disabling duplicate-signer detection if that constant is
    ///         ever raised past 256 without revisiting the bitmask.
    /// @param index the out-of-range pinned validator index.
    error PinnedIndexOverflowsBitmask(uint32 index);

    /// @notice Returned when a module address holds no contract code (for example `address(0)`).
    /// @dev A delegatecall to such an address succeeds with empty return data. A module whose
    ///      interface returns nothing, like `IMisbehaviour.verifyMisbehaviour`, is then
    ///      indistinguishable from a module that ran and accepted the proof.
    /// @param module the module address without code.
    error ModuleHasNoCode(address module);
}
