// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable gas-strict-inequalities

import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";

import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";
import { ISpectreClient } from "contracts/light-clients/spectre/interfaces/ISpectreClient.sol";
import { IMembership } from "contracts/light-clients/spectre/interfaces/IMembership.sol";
import { IUpdateClient } from "contracts/light-clients/spectre/interfaces/IUpdateClient.sol";
import { IMisbehaviour } from "contracts/light-clients/spectre/interfaces/IMisbehaviour.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";

import { SpectreStore } from "contracts/light-clients/spectre/store/SpectreStore.sol";
import { ValidatorSetLib } from "contracts/light-clients/spectre/libraries/ValidatorSetLib.sol";
import { Paths } from "contracts/light-clients/spectre/libraries/Paths.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { ChainId } from "contracts/light-clients/spectre/libraries/ChainId.sol";
import { SSTORE2 } from "contracts/light-clients/spectre/libraries/SSTORE2.sol";
import { AccessControl } from "@openzeppelin-contracts/access/AccessControl.sol";

/// @title SpectreClient
/// @notice Spectre light client using gnark Groth16. SpectreClient is the only stateful
///         piece: it owns the Store, performs the >2/3 quorum accounting, and applies every state
///         write. Header validation and signature verification are delegatecalled into the
///         UpdateClient / Misbehaviour modules; ICS-23 membership is staticcalled into the
///         Membership module.
contract SpectreClient is ISpectreClientErrors, ISpectreClient, ILightClient, AccessControl {
    using SpectreStore for SpectreStore.Store;

    /// @notice UpdateClient module — delegatecall target that validates headers + signature proofs.
    address private immutable UPDATE_CLIENT_MODULE;
    /// @notice Membership module — staticcall target for ICS-23 (non)membership proofs.
    address private immutable MEMBERSHIP_MODULE;
    /// @notice Misbehaviour module — delegatecall target that validates conflicting headers.
    address private immutable MISBEHAVIOUR_MODULE;

    bytes32 private constant PROOF_SUBMITTER_ROLE = keccak256("PROOF_SUBMITTER_ROLE");

    /// @inheritdoc ISpectreClient
    bytes32 public immutable MISBEHAVIOUR_SUBMITTER_ROLE = keccak256("MISBEHAVIOUR_SUBMITTER_ROLE");

    /// @notice Sets the modules and the initial client, consensus, and pinned validator states.
    /// @dev `consensusState_` is taken as the full struct (not a pre-hashed `bytes32`) so the
    ///      constructor can assert `initialPinnedValidatorSet` actually matches the genesis
    ///      consensus state's `nextValidatorsHash` — otherwise a mis-pinned genesis validator set
    ///      (deployer mistake or malice) would be undetectable on-chain.
    /// @dev Each module must already be deployed. They are reached with a delegatecall (or a call) whose success
    ///      is read as "verified", and a call to an address without code succeeds with no return data, so a module
    ///      that was never deployed (for instance `address(0)`) would accept anything: with no misbehaviour module,
    ///      two identical headers and a quorum in the proof metadata would freeze the client without a proof check.
    ///      The addresses are immutable, so the constructor is the only place to catch that.
    /// @dev roleManager==address(0) is the intentional permissionless escape hatch (devnet/test):
    ///      no account is granted `DEFAULT_ADMIN_ROLE` in that branch, which means `unfreeze()`
    ///      becomes permanently uncallable if this client ever freezes while in that mode — the
    ///      only recovery path is migrating the router to a replacement client. Production
    ///      deployments must pass a real `roleManager` (the relayer defaults it to the router,
    ///      see `effectiveRoleManager` in `relayer/cmd/build_source.go`). This is documented, not a bug.
    constructor(
        address updateClientModule,
        address membershipModule,
        address misbehaviourModule,
        bytes memory clientState_,
        ISpectreTendermintMsgs.ConsensusState memory consensusState_,
        ISpectreTendermintMsgs.ValidatorSet memory initialPinnedValidatorSet,
        address roleManager
    ) {
        _requireDeployed(updateClientModule);
        _requireDeployed(membershipModule);
        _requireDeployed(misbehaviourModule);

        SpectreStore.Store storage $ = SpectreStore.load();
        ISpectreTendermintMsgs.ClientState memory cs = abi.decode(clientState_, (ISpectreTendermintMsgs.ClientState));
        $.clientState = cs;

        require(cs.trustingPeriod > 0, LengthIsOutOfRange(cs.trustingPeriod, 1, type(uint32).max));
        require(cs.clockDrift > 0, LengthIsOutOfRange(cs.clockDrift, 1, type(uint32).max));

        uint64 parsedRevision = ChainId.get(cs.chainId).revisionNumber;
        require(
            parsedRevision == cs.latestHeight.revisionNumber,
            MismatchedRevisionHeights(parsedRevision, cs.latestHeight.revisionNumber)
        );

        // Verifiable genesis pin (LC-03): the pinned validator set MUST be the one the genesis
        // consensus state actually committed to, not merely whatever the deployer supplied
        // alongside it.
        bytes32 initialPinnedValidatorsHash = Header.hashValSet(initialPinnedValidatorSet);
        require(
            initialPinnedValidatorsHash == consensusState_.nextValidatorsHash,
            GenesisPinnedValidatorSetMismatch(consensusState_.nextValidatorsHash, initialPinnedValidatorsHash)
        );

        // Same hashing convention used everywhere else a consensus state is bound into the Store
        // (see `updateApplicationState`/`updateConsensusState` below, and
        // `UpdateClient._verifyHeader` / `Misbehaviour._requireTrustedConsensus`).
        $.consensusStateHashes[cs.latestHeight.revisionHeight] = keccak256(abi.encode(consensusState_));

        UPDATE_CLIENT_MODULE = updateClientModule;
        MEMBERSHIP_MODULE = membershipModule;
        MISBEHAVIOUR_MODULE = misbehaviourModule;

        require(
            cs.trustingPeriod + cs.clockDrift <= cs.unbondingPeriod,
            TrustingPeriodTooLong(cs.trustingPeriod, cs.unbondingPeriod)
        );

        _setPinnedValidatorSet(initialPinnedValidatorSet);
        _storePinnedValidatorSetSnapshot(cs.latestHeight.revisionHeight, consensusState_.timestamp);

        if (roleManager == address(0)) {
            _grantRole(PROOF_SUBMITTER_ROLE, address(0));
            _grantRole(MISBEHAVIOUR_SUBMITTER_ROLE, address(0));
        } else {
            _grantRole(DEFAULT_ADMIN_ROLE, roleManager);
            _grantRole(PROOF_SUBMITTER_ROLE, roleManager);
            _grantRole(MISBEHAVIOUR_SUBMITTER_ROLE, roleManager);
        }
    }

    // ============ Views ============

    /// @inheritdoc ILightClient
    function getClientState() external view returns (bytes memory) {
        return abi.encode(SpectreStore.load().clientState);
    }

    /// @inheritdoc ISpectreClient
    function getPinnedValidatorsHash() external view returns (bytes32) {
        return SpectreStore.load().pinnedValidatorsHash;
    }

    /// @inheritdoc ISpectreClient
    function getConsensusStateHash(uint64 revisionHeight) external view returns (bytes32) {
        return SpectreStore.load().getConsensusStateHash(revisionHeight);
    }

    /// @inheritdoc ISpectreClient
    function getPinnedSnapshot() external view returns (uint64 height, uint128 timestamp) {
        SpectreStore.Store storage $ = SpectreStore.load();
        return ($.snapshotHeights[$.snapshotHeights.length - 1], $.pinnedTimestamp);
    }

    /// @inheritdoc ISpectreClient
    function getPinnedValidatorSet()
        external
        view
        returns (uint32[] memory indices, bytes32[] memory pubkeys, uint64[] memory votingPowers)
    {
        SpectreStore.Store storage $ = SpectreStore.load();
        SpectreStore.PinnedValidatorSetSnapshot memory snapshot = $.currentSnapshot();
        (ValidatorSetLib.ValidatorCacheHeader memory cacheHeader, bytes memory cacheData) =
            ValidatorSetLib.readCache(snapshot);

        indices = new uint32[](cacheHeader.entryCount);
        pubkeys = new bytes32[](cacheHeader.entryCount);
        votingPowers = new uint64[](cacheHeader.entryCount);
        for (uint16 i = 0; i < cacheHeader.entryCount; i++) {
            indices[i] = uint32(i);
            (votingPowers[i], pubkeys[i]) =
                ValidatorSetLib.resolveValidator(snapshot.validatorsHash, cacheData, cacheHeader.entryCount, i);
        }
    }

    // ============ Update ============

    /// @inheritdoc ILightClient
    function updateApplicationState(bytes calldata updateMsg)
        external
        notFrozen
        onlyProofSubmitter
        returns (ILightClientMsgs.UpdateResult)
    {
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ =
            abi.decode(updateMsg, (ISpectreClientMsgs.MsgUpdateApplicationState));

        SpectreStore.Store storage $ = SpectreStore.load();
        _requireFreshness($, msg_.time);

        // Quorum accounting first: it gates the (expensive) signature proof and must revert before
        // the SignatureVerifier is reached when the proof metadata is sub-quorum or malformed.
        _verifyPinnedQuorum($, msg_.proof, msg_.proposedHeader);

        // Header validation + the batched signature proof run in the delegatecalled module.
        ISpectreClientMsgs.VerifyHeaderOutput memory output = abi.decode(
            _delegate(UPDATE_CLIENT_MODULE, abi.encodeCall(IUpdateClient.verifyHeader, (msg_))),
            (ISpectreClientMsgs.VerifyHeaderOutput)
        );

        ILightClientMsgs.UpdateResult updateResult = _checkUpdateResult($, output);

        if (updateResult == ILightClientMsgs.UpdateResult.Update) {
            require(
                output.newHeight.revisionHeight > $.clientState.latestHeight.revisionHeight,
                NonMonotonicHeightUpdate($.clientState.latestHeight.revisionHeight, output.newHeight.revisionHeight)
            );
            $.clientState.latestHeight = output.newHeight;
            $.consensusStateHashes[output.newHeight.revisionHeight] = keccak256(abi.encode(output.newConsensusState));
            emit ClientUpdated(output.newHeight.revisionHeight);
        } else if (updateResult == ILightClientMsgs.UpdateResult.Misbehaviour) {
            $.clientState.isFrozen = true;
            emit ClientFrozen();
        }
        return updateResult;
    }

    /// @inheritdoc ILightClient
    function updateConsensusState(bytes calldata updateMsg)
        external
        notFrozen
        onlyProofSubmitter
        returns (ILightClientMsgs.UpdateResult)
    {
        ISpectreClientMsgs.MsgUpdateConsensusState memory msg_ =
            abi.decode(updateMsg, (ISpectreClientMsgs.MsgUpdateConsensusState));
        ISpectreClientMsgs.MsgUpdateApplicationState memory app = msg_.update;

        SpectreStore.Store storage $ = SpectreStore.load();
        _requireFreshness($, app.time);

        // Quorum accounting first: it gates the (expensive) signature proof and must revert before
        // the SignatureVerifier is reached when the proof metadata is sub-quorum or malformed.
        _verifyPinnedQuorum($, app.proof, app.proposedHeader);

        // Header validation + the batched signature proof run in the delegatecalled module.
        ISpectreClientMsgs.VerifyHeaderOutput memory output = abi.decode(
            _delegate(UPDATE_CLIENT_MODULE, abi.encodeCall(IUpdateClient.verifyHeader, (app))),
            (ISpectreClientMsgs.VerifyHeaderOutput)
        );

        ILightClientMsgs.UpdateResult updateResult = _checkUpdateResult($, output);

        if (updateResult == ILightClientMsgs.UpdateResult.Misbehaviour) {
            $.clientState.isFrozen = true;
            emit ClientFrozen();
            return updateResult;
        }

        bytes32 newValidatorsHash = Header.hashValSet(msg_.newValidatorSet);
        require(
            newValidatorsHash == app.proposedHeader.signedHeader.header.nextValidatorsHash,
            MismatchedValidatorHashes(app.proposedHeader.signedHeader.header.nextValidatorsHash, newValidatorsHash)
        );

        if (updateResult == ILightClientMsgs.UpdateResult.Update) {
            require(
                output.newHeight.revisionHeight > $.clientState.latestHeight.revisionHeight,
                NonMonotonicHeightUpdate($.clientState.latestHeight.revisionHeight, output.newHeight.revisionHeight)
            );
            _setPinnedValidatorSet(msg_.newValidatorSet);
            $.clientState.latestHeight = output.newHeight;
            $.consensusStateHashes[output.newHeight.revisionHeight] = keccak256(abi.encode(output.newConsensusState));
            _storePinnedValidatorSetSnapshot(output.newHeight.revisionHeight, output.newConsensusState.timestamp);
            emit ClientUpdated(output.newHeight.revisionHeight);
            emit ConsensusStateUpdated(output.newHeight.revisionHeight, newValidatorsHash);
        } else {
            // NoOp: re-pin the validator set at the already-trusted height.
            require(
                output.newHeight.revisionHeight == $.clientState.latestHeight.revisionHeight,
                NonMonotonicHeightUpdate($.clientState.latestHeight.revisionHeight, output.newHeight.revisionHeight)
            );
            _setPinnedValidatorSet(msg_.newValidatorSet);
            _storePinnedValidatorSetSnapshot(output.newHeight.revisionHeight, output.newConsensusState.timestamp);
            emit ConsensusStateUpdated(output.newHeight.revisionHeight, newValidatorsHash);
        }
        return updateResult;
    }

    /// @notice Checks whether a verified header advances, no-ops, or reveals misbehaviour.
    /// @dev Misbehaviour if the consensus state at the new height differs from the mapping,
    ///      or the timestamp is not increasing.
    function _checkUpdateResult(
        SpectreStore.Store storage $,
        ISpectreClientMsgs.VerifyHeaderOutput memory output
    )
        private
        view
        returns (ILightClientMsgs.UpdateResult)
    {
        bytes32 consensusStateHash = $.consensusStateHashes[output.newHeight.revisionHeight];
        if (consensusStateHash == bytes32(0)) {
            return ILightClientMsgs.UpdateResult.Update;
        } else if (
            consensusStateHash != keccak256(abi.encode(output.newConsensusState))
                || output.trustedConsensusState.timestamp >= output.newConsensusState.timestamp
        ) {
            return ILightClientMsgs.UpdateResult.Misbehaviour;
        } else {
            return ILightClientMsgs.UpdateResult.NoOp;
        }
    }

    /// @notice Sums the verified signers' voting power against the current pinned set, requiring >2/3.
    /// @dev The app-state path intentionally accounts quorum against `$.currentSnapshot()` even
    ///      when that pinned set lags the trusted consensus state's `nextValidatorsHash`; the
    ///      relayer rotates with `updateConsensusState` only when the pinned-set overlap decays or
    ///      the refresh cadence forces a rotation. Freshness is time-based: the current pinned
    ///      snapshot's consensus timestamp must remain inside the trusting period, but it need not
    ///      equal the trusted height's next validator set. Contrast `_verifyMisbehaviourQuorum`
    ///      below, which legitimately uses `snapshotAt(trustedHeight)` because misbehaviour evidence
    ///      can reference any past trusted height, not just the latest. The selected historical
    ///      snapshot must nevertheless remain inside the trusting period before its voting power
    ///      can be counted.
    function _verifyPinnedQuorum(
        SpectreStore.Store storage $,
        ISpectreClientMsgs.BatchProof memory proof_,
        ISpectreTendermintMsgs.Header memory header
    )
        private
        view
    {
        SpectreStore.PinnedValidatorSetSnapshot memory snapshot = $.currentSnapshot();
        _validateConsensusStateTrustingPeriod(snapshot.timestamp);
        _verifyQuorum(proof_, header.signedHeader.commit.commitSigs, snapshot);
    }

    /// @notice Validates and sums the verified signers' voting power against the snapshot trusted
    ///         at the header's trusted height, requiring >2/3.
    function _verifyMisbehaviourQuorum(
        SpectreStore.Store storage $,
        ISpectreClientMsgs.BatchProof memory proof_,
        ISpectreTendermintMsgs.Header memory header
    )
        private
        view
    {
        SpectreStore.PinnedValidatorSetSnapshot memory snapshot = $.snapshotAt(header.trustedHeight.revisionHeight);
        _validateConsensusStateTrustingPeriod(snapshot.timestamp);
        _verifyQuorum(proof_, header.signedHeader.commit.commitSigs, snapshot);
    }

    function _verifyQuorum(
        ISpectreClientMsgs.BatchProof memory proof_,
        ISpectreTendermintMsgs.CommitSig[] memory commitSigs,
        SpectreStore.PinnedValidatorSetSnapshot memory snapshot
    )
        private
        view
    {
        require(
            proof_.signerIndices.length == proof_.bucket && proof_.pinnedValidatorIndices.length == proof_.bucket
                && proof_.signerPubkeys.length == proof_.bucket && proof_.active.length == proof_.bucket,
            BatchLengthMismatch()
        );

        (ValidatorSetLib.ValidatorCacheHeader memory cacheHeader, bytes memory cacheData) =
            ValidatorSetLib.readCache(snapshot);
        uint64 totalVotingPower = snapshot.totalVotingPower;
        uint64 accumulatedVotingPower = 0;
        uint256 seenPinned = 0;
        bool hasPrevCommitSigner = false;
        uint32 prevCommitSigner = 0;

        for (uint256 i = 0; i < proof_.bucket; i++) {
            if (!proof_.active[i]) {
                continue;
            }

            uint32 commitIdx = proof_.signerIndices[i];
            if (hasPrevCommitSigner) {
                require(commitIdx > prevCommitSigner, DuplicateSigner(commitIdx));
            }
            hasPrevCommitSigner = true;
            prevCommitSigner = commitIdx;

            uint32 pinnedIdx = proof_.pinnedValidatorIndices[i];
            require(pinnedIdx < cacheHeader.entryCount, SignerIndexOutOfRange(pinnedIdx));
            // Defense-in-depth (LC-07): `seenPinned` packs one bit per pinned index into a
            // uint256. On the EVM, `x << n` for `n >= 256` evaluates to 0 instead of reverting, so
            // an unchecked shift here would silently turn off duplicate-signer detection for any
            // pinnedIdx >= 256. `entryCount <= ValidatorSetLib.MAX_VALIDATOR_COUNT` (180) already
            // makes this unreachable today (see the INVARIANT note on that constant), but this
            // check makes a future increase of MAX_VALIDATOR_COUNT past 256 fail loudly here
            // instead of silently breaking dup-signer accounting.
            require(pinnedIdx < 256, PinnedIndexOverflowsBitmask(pinnedIdx));
            uint256 mask = uint256(1) << pinnedIdx;
            require((seenPinned & mask) == 0, DuplicateSigner(pinnedIdx));
            seenPinned |= mask;

            (uint64 votingPower, bytes32 pubKey) =
                ValidatorSetLib.resolveValidator(snapshot.validatorsHash, cacheData, cacheHeader.entryCount, pinnedIdx);
            require(pubKey == proof_.signerPubkeys[i], PubkeyMismatch(pinnedIdx));
            accumulatedVotingPower += votingPower;
        }

        require(
            uint256(accumulatedVotingPower) * 3 > uint256(totalVotingPower) * 2,
            InsufficientVotingPower(accumulatedVotingPower, totalVotingPower)
        );

        ValidatorSetLib.requireProofSignersCommitSigs(
            commitSigs, proof_.signerIndices, proof_.signerPubkeys, proof_.active
        );
    }

    // ============ Membership ============

    /// @inheritdoc ILightClient
    function verifyMembership(ILightClientMsgs.MsgVerifyMembership calldata msg_)
        external
        notFrozen
        onlyProofSubmitter
        returns (uint256)
    {
        require(msg_.value.length > 0, EmptyValue());
        return _membership(
            msg_.height,
            msg_.kvPairs,
            msg_.merkleProofs,
            msg_.appHash,
            msg_.trustedConsensusState,
            msg_.membershipType,
            msg_.path,
            msg_.value
        );
    }

    /// @inheritdoc ILightClient
    function verifyNonMembership(ILightClientMsgs.MsgVerifyNonMembership calldata msg_)
        external
        notFrozen
        onlyProofSubmitter
        returns (uint256)
    {
        return _membership(
            msg_.height,
            msg_.kvPairs,
            msg_.merkleProofs,
            msg_.appHash,
            msg_.trustedConsensusState,
            msg_.membershipType,
            msg_.path,
            bytes("")
        );
    }

    function _membership(
        IICS02ClientMsgs.Height calldata height,
        IMembershipMsgs.KVPair[] calldata kvPairs,
        IMembershipMsgs.MerkleProof[] calldata merkleProofs,
        bytes32 appHash,
        ISpectreTendermintMsgs.ConsensusState calldata trustedConsensusState,
        IMembershipMsgs.MembershipType membershipType,
        bytes[] calldata kvPath,
        bytes memory kvValue
    )
        private
        returns (uint256)
    {
        if (membershipType == IMembershipMsgs.MembershipType.Membership) {
            return _handleMembership(height, kvPairs, merkleProofs, appHash, trustedConsensusState, kvPath, kvValue);
        }

        revert UnknownMembershipType(uint8(membershipType));
    }

    function _handleMembership(
        IICS02ClientMsgs.Height calldata height,
        IMembershipMsgs.KVPair[] calldata kvPairs,
        IMembershipMsgs.MerkleProof[] calldata merkleProofs,
        bytes32 appHash,
        ISpectreTendermintMsgs.ConsensusState calldata trustedConsensusState,
        bytes[] calldata kvPath,
        bytes memory kvValue
    )
        private
        returns (uint256)
    {
        require(
            kvPairs.length > 0 && kvPairs.length <= type(uint16).max,
            LengthIsOutOfRange(kvPairs.length, 1, type(uint16).max)
        );

        _validateMembershipInput(appHash, height.revisionHeight, trustedConsensusState);

        {
            bool found = false;
            for (uint256 i = 0; i < kvPairs.length; ++i) {
                if (!Paths.equal(kvPairs[i].path, kvPath)) {
                    continue;
                }

                bytes memory value = kvPairs[i].value;
                require(
                    value.length == kvValue.length && keccak256(value) == keccak256(kvValue),
                    MembershipProofValueMismatch(kvValue, value)
                );

                found = true;
                break;
            }
            require(found, MembershipProofKeyNotFound(kvPath));
        }

        IMembership(MEMBERSHIP_MODULE).verifyMembership(appHash, kvPairs, merkleProofs);

        return _nanosToSeconds(trustedConsensusState.timestamp);
    }

    function _validateMembershipInput(
        bytes32 commitmentRoot,
        uint64 proofHeight,
        ISpectreTendermintMsgs.ConsensusState memory trustedConsensusState
    )
        private
        view
    {
        SpectreStore.Store storage $ = SpectreStore.load();
        bytes32 trustedConsensusStateHash = keccak256(abi.encode(trustedConsensusState));
        bytes32 storedConsensusStateHash = $.getConsensusStateHash(proofHeight);
        require(
            trustedConsensusStateHash == storedConsensusStateHash,
            ConsensusStateHashMismatch(storedConsensusStateHash, trustedConsensusStateHash)
        );

        require(
            commitmentRoot == trustedConsensusState.root,
            ConsensusStateRootMismatch(trustedConsensusState.root, commitmentRoot)
        );

        _validateConsensusStateTrustingPeriod(trustedConsensusState.timestamp);
    }

    function _validateConsensusStateTrustingPeriod(uint128 consensusStateTimestamp) private view {
        SpectreStore.Store storage $ = SpectreStore.load();
        uint256 consensusStateTimestampSeconds = _nanosToSeconds(consensusStateTimestamp);
        uint256 drift = $.clientState.clockDrift;
        require(
            // Membership proof freshness is defined against the destination chain clock.
            // forge-lint: disable-next-line(block-timestamp)
            consensusStateTimestampSeconds <= block.timestamp + drift,
            ProofIsInTheFuture(block.timestamp, consensusStateTimestampSeconds)
        );

        uint128 durationSinceConsensusState = 0;
        if (consensusStateTimestampSeconds < block.timestamp) {
            // forge-lint: disable-next-line(unsafe-typecast)
            durationSinceConsensusState = uint128(block.timestamp - consensusStateTimestampSeconds);
        }
        require(
            durationSinceConsensusState < $.clientState.trustingPeriod,
            InsufficientTrustingPeriod(durationSinceConsensusState, uint128($.clientState.trustingPeriod))
        );
    }

    // ============ Misbehaviour ============

    /// @dev Standalone misbehaviour is accepted only when both conflicting headers carry
    ///      proof-backed >2/3 Ed25519 quorums over the pinned validator set.
    /// @inheritdoc ILightClient
    function misbehaviour(bytes calldata misbehaviourMsg) external notFrozen onlyMisbehaviourSubmitter {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ =
            abi.decode(misbehaviourMsg, (ISpectreClientMsgs.MsgSubmitMisbehaviour));

        SpectreStore.Store storage $ = SpectreStore.load();
        // Bind the submitter-chosen time to the wall clock so the delegatecalled header validation
        // cannot accept stale trusted consensus states. `_verifyMisbehaviourQuorum` independently
        // requires the snapshot selected at each trusted height to remain within the trusting period.
        _requireFreshness($, msg_.time);

        // Quorum accounting first: it gates the (expensive) signature proof and must revert before
        // the SignatureVerifier is reached when the proof metadata is sub-quorum or malformed.
        _verifyMisbehaviourQuorum($, msg_.proof1, msg_.misbehaviour.header1);
        _verifyMisbehaviourQuorum($, msg_.proof2, msg_.misbehaviour.header2);

        // Header validation + the two batched signature proofs run in the delegatecalled module.
        _delegate(MISBEHAVIOUR_MODULE, abi.encodeCall(IMisbehaviour.verifyMisbehaviour, (msg_)));

        $.clientState.isFrozen = true;
        emit ClientFrozen();
    }

    /// @inheritdoc ISpectreClient
    /// @dev LC-04: gated on `DEFAULT_ADMIN_ROLE`, which is granted to no one when this client was
    ///      deployed with `roleManager == address(0)` (the intentional permissionless dev/test
    ///      escape hatch — see the constructor NatSpec). In that mode this function is
    ///      PERMANENTLY UNCALLABLE once the client freezes; the only recovery path is migrating
    ///      the router to a replacement client (`ICS26Router.migrateClient`). This is documented,
    ///      intentional behavior, not a bug — do not "fix" it with a fallback admin, and do not
    ///      assume `roleManager != address(0)` here.
    function unfreeze() external override(ISpectreClient, ILightClient) onlyRole(DEFAULT_ADMIN_ROLE) {
        SpectreStore.Store storage $ = SpectreStore.load();
        require($.clientState.isFrozen, ClientNotFrozen());
        $.clientState.isFrozen = false;
        emit ClientUnfrozen();
    }

    /// @inheritdoc ILightClient
    function upgradeClient(bytes calldata) external pure {
        // NOTE: This feature will not be supported.
        revert FeatureNotSupported();
    }

    // ============ Internal state writes ============

    function _setPinnedValidatorSet(ISpectreTendermintMsgs.ValidatorSet memory validatorSet) private {
        SpectreStore.Store storage $ = SpectreStore.load();
        bytes32 validatorsHash = Header.hashValSet(validatorSet);
        bytes memory cacheData = ValidatorSetLib.buildCache(validatorsHash, validatorSet);
        ValidatorSetLib.ValidatorCacheHeader memory cacheHeader =
            ValidatorSetLib.readValidatorCacheHeader(validatorsHash, cacheData);
        $.pinnedValidatorSetPointer = SSTORE2.write(cacheData);
        $.pinnedValidatorsHash = validatorsHash;
        $.pinnedTotalVotingPower = cacheHeader.totalVotingPower;
        $.pinnedEntryCount = cacheHeader.entryCount;
    }

    function _storePinnedValidatorSetSnapshot(uint64 height, uint128 timestamp) private {
        SpectreStore.Store storage $ = SpectreStore.load();
        uint256 snapshotCount = $.snapshotHeights.length;
        if (snapshotCount != 0) {
            uint64 lastHeight = $.snapshotHeights[snapshotCount - 1];
            require(height >= lastHeight, NonMonotonicHeightUpdate(lastHeight, height));
        }
        bool exists = $.snapshots[height].pointer != address(0);
        $.snapshots[height] = SpectreStore.PinnedValidatorSetSnapshot({
            validatorsHash: $.pinnedValidatorsHash,
            pointer: $.pinnedValidatorSetPointer,
            totalVotingPower: $.pinnedTotalVotingPower,
            entryCount: $.pinnedEntryCount,
            timestamp: timestamp
        });
        $.pinnedTimestamp = timestamp;
        if (!exists) {
            $.snapshotHeights.push(height);
        }
    }

    // ============ Helpers ============

    /// @notice Reverts unless `module` holds contract code.
    function _requireDeployed(address module) private view {
        require(module.code.length != 0, ModuleHasNoCode(module));
    }

    /// @notice Delegatecalls a module, bubbling up any revert data (so custom errors survive).
    /// @dev A delegatecall to an address without code succeeds and returns nothing. The constructor already
    ///      rejects such modules; the check here keeps that success from ever being read as a verified proof.
    function _delegate(address target, bytes memory data) private returns (bytes memory) {
        _requireDeployed(target);
        (bool ok, bytes memory ret) = target.delegatecall(data);
        if (!ok) {
            assembly ("memory-safe") {
                revert(add(ret, 0x20), mload(ret))
            }
        }
        return ret;
    }

    /// @notice Requires the proof time is not in the future and within clock drift of now.
    function _requireFreshness(SpectreStore.Store storage $, uint128 time) private view {
        uint256 timeSeconds = _nanosToSeconds(time);
        uint256 drift = $.clientState.clockDrift;
        // forge-lint: disable-next-line(block-timestamp)
        require(timeSeconds <= block.timestamp + drift, ProofIsInTheFuture(block.timestamp, timeSeconds));
        if (timeSeconds < block.timestamp) {
            require(block.timestamp - timeSeconds <= drift, ProofIsTooOld(block.timestamp, timeSeconds));
        }
    }

    function _nanosToSeconds(uint256 nanos) private pure returns (uint256) {
        return nanos / 1e9;
    }

    modifier notFrozen() {
        require(!SpectreStore.load().clientState.isFrozen, FrozenClientState());
        _;
    }

    modifier onlyProofSubmitter() {
        if (!hasRole(PROOF_SUBMITTER_ROLE, address(0))) {
            _checkRole(PROOF_SUBMITTER_ROLE);
        }
        _;
    }

    modifier onlyMisbehaviourSubmitter() {
        if (!hasRole(MISBEHAVIOUR_SUBMITTER_ROLE, address(0))) {
            _checkRole(MISBEHAVIOUR_SUBMITTER_ROLE);
        }
        _;
    }
}
