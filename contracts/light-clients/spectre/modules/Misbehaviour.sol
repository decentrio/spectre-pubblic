// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IMisbehaviour } from "contracts/light-clients/spectre/interfaces/IMisbehaviour.sol";
import { ISignatureVerifier } from "contracts/light-clients/spectre/interfaces/ISignatureVerifier.sol";
import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";
import { SpectreStore } from "contracts/light-clients/spectre/store/SpectreStore.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { HeightCmp } from "contracts/light-clients/spectre/libraries/HeightCmp.sol";
import { ChainId } from "contracts/light-clients/spectre/libraries/ChainId.sol";

/// @title Misbehaviour module
/// @notice Validates two conflicting signed headers at the same height and their batched signature
///         proofs. Executes in SpectreClient's storage context via delegatecall: it reads the
///         client state and trusted consensus hashes from the shared Store. The two quorum checks
///         and the freeze are done by SpectreClient.
contract Misbehaviour is IMisbehaviour {
    using SpectreStore for SpectreStore.Store;

    error MisbehaviourNotDetected();

    ISignatureVerifier internal immutable SIGNATURE_VERIFIER;
    address private immutable SELF;

    constructor(address signatureVerifier) {
        SIGNATURE_VERIFIER = ISignatureVerifier(signatureVerifier);
        SELF = address(this);
    }

    modifier onlyDelegated() {
        require(address(this) != SELF, ISpectreClientErrors.DirectCallNotAllowed());
        _;
    }

    /// @inheritdoc IMisbehaviour
    function verifyMisbehaviour(ISpectreClientMsgs.MsgSubmitMisbehaviour calldata msg_) external onlyDelegated {
        SpectreStore.Store storage $ = SpectreStore.load();
        ISpectreTendermintMsgs.ClientState storage clientState = $.clientState;

        ISpectreClientMsgs.Misbehaviour calldata misbehaviour_ = msg_.misbehaviour;
        require(
            keccak256(bytes(clientState.chainId))
                == keccak256(bytes(misbehaviour_.header1.signedHeader.header.chainId)),
            ISpectreClientErrors.ChainIdMismatch(clientState.chainId, misbehaviour_.header1.signedHeader.header.chainId)
        );

        _validateBasic(misbehaviour_);

        // NOTE (LC-05): `trustThreshold` is threaded through `options` into
        // `_verifyMisbehaviourHeader` below but is never actually read there — the real quorum
        // threshold is hardcoded `>2/3` in `SpectreClient._verifyQuorum` (used for both
        // `_verifyMisbehaviourQuorum` calls in `SpectreClient.misbehaviour`). Currently
        // decoded-but-unused / reserved; see the field comment on `ClientState.trustLevel`.
        ISpectreTendermintMsgs.Options memory options = ISpectreTendermintMsgs.Options({
            trustThreshold: clientState.trustLevel,
            trustingPeriod: clientState.trustingPeriod,
            clockDrift: clientState.clockDrift
        });
        ISpectreTendermintMsgs.ChainId memory chainId = ISpectreTendermintMsgs.ChainId({
            id: clientState.chainId, revisionNumber: clientState.latestHeight.revisionNumber
        });

        _verifyMisbehaviourHeader(
            misbehaviour_.header1, chainId, options, msg_.trustedConsensusState1.timestamp, msg_.time
        );
        _verifyMisbehaviourHeader(
            misbehaviour_.header2, chainId, options, msg_.trustedConsensusState2.timestamp, msg_.time
        );

        // Bind both supplied trusted consensus states to the ones this client already trusts.
        _requireTrustedConsensus($, misbehaviour_.header1, msg_.trustedConsensusState1);
        _requireTrustedConsensus($, misbehaviour_.header2, msg_.trustedConsensusState2);

        // Signature checks: the batched Ed25519 Groth16 proof over each conflicting header.
        _verifyBatchProof(misbehaviour_.header1, msg_.proof1);
        _verifyBatchProof(misbehaviour_.header2, msg_.proof2);
    }

    function _requireTrustedConsensus(
        SpectreStore.Store storage $,
        ISpectreTendermintMsgs.Header calldata header,
        ISpectreTendermintMsgs.ConsensusState calldata trustedConsensusState
    )
        private
        view
    {
        bytes32 trustedHash = keccak256(abi.encode(trustedConsensusState));
        bytes32 storedHash = $.getConsensusStateHash(header.trustedHeight.revisionHeight);
        require(trustedHash == storedHash, ISpectreClientErrors.ConsensusStateHashMismatch(storedHash, trustedHash));
    }

    function _validateBasic(ISpectreClientMsgs.Misbehaviour calldata misbehaviour_) private pure {
        _validateHeaderBasic(misbehaviour_.header1);
        _validateHeaderBasic(misbehaviour_.header2);

        if (
            keccak256(bytes(misbehaviour_.header1.signedHeader.header.chainId))
                != keccak256(bytes(misbehaviour_.header2.signedHeader.header.chainId))
        ) {
            revert ISpectreClientErrors.ChainIdMismatch({
                expected: misbehaviour_.header1.signedHeader.header.chainId,
                actual: misbehaviour_.header2.signedHeader.header.chainId
            });
        }

        if (misbehaviour_.header1.signedHeader.header.height != misbehaviour_.header2.signedHeader.header.height) {
            revert ISpectreClientErrors.MismatchedMisbehaviourHeaderHeights({
                height1: misbehaviour_.header1.signedHeader.header.height,
                height2: misbehaviour_.header2.signedHeader.header.height
            });
        }

        if (
            misbehaviour_.header1.signedHeader.commit.blockId.hashData
                == misbehaviour_.header2.signedHeader.commit.blockId.hashData
        ) {
            revert MisbehaviourNotDetected();
        }
    }

    function _validateHeaderBasic(ISpectreTendermintMsgs.Header calldata header) private pure {
        ISpectreTendermintMsgs.ChainId memory chainId = ChainId.get(header.signedHeader.header.chainId);
        if (chainId.revisionNumber != header.trustedHeight.revisionNumber) {
            revert ISpectreClientErrors.MismatchedRevisionHeights(
                chainId.revisionNumber, header.trustedHeight.revisionNumber
            );
        }

        IICS02ClientMsgs.Height memory height = IICS02ClientMsgs.Height({
            revisionNumber: chainId.revisionNumber, revisionHeight: header.signedHeader.header.height
        });
        if (HeightCmp.ge(header.trustedHeight, height)) {
            revert ISpectreClientErrors.InvalidHeaderHeight(height.revisionHeight);
        }

        if (Header.hashHeader(header.signedHeader.header) != header.signedHeader.commit.blockId.hashData) {
            revert ISpectreClientErrors.FailedToVerifyHeader({ description: "invalid block: header hash mismatch" });
        }
    }

    function _verifyMisbehaviourHeader(
        ISpectreTendermintMsgs.Header calldata header,
        ISpectreTendermintMsgs.ChainId memory chainId,
        ISpectreTendermintMsgs.Options memory options,
        uint128 trustedTime,
        uint128 currentTimestamp
    )
        private
        pure
    {
        uint128 currentTimeInSeconds = _nanosToSeconds(currentTimestamp);
        uint128 trustedTimeInSeconds = _nanosToSeconds(trustedTime);

        if (currentTimeInSeconds < trustedTimeInSeconds) {
            revert ISpectreClientErrors.InvalidConsensusStateTimestamp({ timestamp: trustedTime });
        }
        uint128 durationSinceConsensusState = currentTimeInSeconds - trustedTimeInSeconds;
        if (durationSinceConsensusState >= options.trustingPeriod) {
            revert ISpectreClientErrors.InsufficientTrustingPeriod({
                durationSinceConsensusState: durationSinceConsensusState, trustingPeriod: options.trustingPeriod
            });
        }

        require(
            keccak256(bytes(header.signedHeader.header.chainId)) == keccak256(bytes(chainId.id)),
            ISpectreClientErrors.ChainIdMismatch(chainId.id, header.signedHeader.header.chainId)
        );
        require(header.signedHeader.header.time > trustedTime, "invalid block: non monotonic bft time");
        uint128 drifted = currentTimestamp + uint128(options.clockDrift) * 1_000_000_000;
        require(header.signedHeader.header.time < drifted, "invalid block: header is from the future");
    }

    function _verifyBatchProof(
        ISpectreTendermintMsgs.Header calldata header,
        ISpectreClientMsgs.BatchProof calldata proof_
    )
        private
    {
        ISpectreTendermintMsgs.BlockCommit calldata commit = header.signedHeader.commit;
        require(
            commit.height == header.signedHeader.header.height, ISpectreClientErrors.InvalidHeaderHeight(commit.height)
        );

        ISignatureVerifier.SharedBlock memory shared = ISignatureVerifier.SharedBlock({
            height: commit.height, round: uint64(commit.round), blockIDHash: commit.blockId.hashData
        });

        require(
            SIGNATURE_VERIFIER.verifyBatchProof(
                proof_.bucket,
                proof_.proof,
                proof_.commitments,
                proof_.commitmentPok,
                proof_.signerPubkeys,
                proof_.active,
                shared
            ),
            ISpectreClientErrors.ProofVerificationFailed()
        );
    }

    function _nanosToSeconds(uint128 timestamp) private pure returns (uint128) {
        return timestamp / 1e9;
    }
}
