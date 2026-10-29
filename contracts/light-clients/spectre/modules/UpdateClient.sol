// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IUpdateClient } from "contracts/light-clients/spectre/interfaces/IUpdateClient.sol";
import { ISignatureVerifier } from "contracts/light-clients/spectre/interfaces/ISignatureVerifier.sol";
import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";
import { SpectreStore } from "contracts/light-clients/spectre/store/SpectreStore.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { HeightCmp } from "contracts/light-clients/spectre/libraries/HeightCmp.sol";
import { ChainId } from "contracts/light-clients/spectre/libraries/ChainId.sol";

/// @title UpdateClient module
/// @notice Validates a proposed header and its batched Ed25519 signature proof. Executes in
///         SpectreClient's storage context via delegatecall: it reads the client state and the
///         trusted consensus hash from the shared Store, and forwards the signature proof to the
///         SignatureVerifier. Quorum accounting and all writes are done by SpectreClient.
contract UpdateClient is IUpdateClient {
    using SpectreStore for SpectreStore.Store;

    /// @notice The signature verifier this module forwards the batched Groth16 proof to.
    ISignatureVerifier internal immutable SIGNATURE_VERIFIER;
    /// @notice This module's own address, used to reject direct (non-delegatecall) invocation.
    address private immutable SELF;

    constructor(address signatureVerifier) {
        SIGNATURE_VERIFIER = ISignatureVerifier(signatureVerifier);
        SELF = address(this);
    }

    /// @notice Rejects direct calls: this module is only meant to run via delegatecall from SpectreClient.
    modifier onlyDelegated() {
        require(address(this) != SELF, ISpectreClientErrors.DirectCallNotAllowed());
        _;
    }

    /// @inheritdoc IUpdateClient
    function verifyHeader(ISpectreClientMsgs.MsgUpdateApplicationState calldata msg_)
        external
        onlyDelegated
        returns (ISpectreClientMsgs.VerifyHeaderOutput memory)
    {
        SpectreStore.Store storage $ = SpectreStore.load();
        ISpectreTendermintMsgs.ClientState storage clientState = $.clientState;

        ISpectreTendermintMsgs.ChainId memory chainId = ISpectreTendermintMsgs.ChainId({
            id: clientState.chainId, revisionNumber: clientState.latestHeight.revisionNumber
        });
        // NOTE (LC-05): `trustThreshold` is threaded through `options` into `_verifyHeader` /
        // `_verifyAgainstTrusted` below but is never actually read there — the real quorum
        // threshold is hardcoded `>2/3` in `SpectreClient._verifyQuorum`. Currently
        // decoded-but-unused / reserved; see the field comment on `ClientState.trustLevel`.
        ISpectreTendermintMsgs.Options memory options = ISpectreTendermintMsgs.Options({
            trustThreshold: clientState.trustLevel,
            trustingPeriod: clientState.trustingPeriod,
            clockDrift: clientState.clockDrift
        });

        _verifyHeader(msg_.proposedHeader, chainId, options, msg_.time, msg_.trustedConsensusState);

        // Bind the supplied trusted consensus state to the one this client already trusts.
        bytes32 trustedHash = keccak256(abi.encode(msg_.trustedConsensusState));
        bytes32 storedHash = $.getConsensusStateHash(msg_.proposedHeader.trustedHeight.revisionHeight);
        require(trustedHash == storedHash, ISpectreClientErrors.ConsensusStateHashMismatch(storedHash, trustedHash));

        // Signature check: the batched Ed25519 Groth16 proof over the proposed header.
        _verifyBatchProof(msg_.proposedHeader, msg_.proof);

        return _buildOutput(msg_, chainId.revisionNumber);
    }

    function _verifyHeader(
        ISpectreTendermintMsgs.Header calldata proposedHeader,
        ISpectreTendermintMsgs.ChainId memory chainId,
        ISpectreTendermintMsgs.Options memory options,
        uint128 time,
        ISpectreTendermintMsgs.ConsensusState calldata trustedConsensusState
    )
        private
        pure
    {
        ISpectreTendermintMsgs.ChainId memory headerChainId = ChainId.get(proposedHeader.signedHeader.header.chainId);
        _validateBasic(proposedHeader, headerChainId);
        if (chainId.revisionNumber != headerChainId.revisionNumber) {
            revert ISpectreClientErrors.ChainIdMismatch(chainId.id, headerChainId.id);
        }

        if (
            Header.hashHeader(proposedHeader.signedHeader.header) != proposedHeader.signedHeader.commit.blockId.hashData
        ) {
            revert ISpectreClientErrors.FailedToVerifyHeader("invalid block: header hash mismatch");
        }

        _verifyAgainstTrusted(proposedHeader, chainId.id, options, time, trustedConsensusState);
    }

    function _verifyAgainstTrusted(
        ISpectreTendermintMsgs.Header calldata proposedHeader,
        string memory chainId,
        ISpectreTendermintMsgs.Options memory options,
        uint128 time,
        ISpectreTendermintMsgs.ConsensusState calldata trustedConsensusState
    )
        private
        pure
    {
        uint128 trustingPeriodNanos = uint128(options.trustingPeriod) * 1_000_000_000;
        if (time < trustedConsensusState.timestamp || time - trustedConsensusState.timestamp >= trustingPeriodNanos) {
            revert ISpectreClientErrors.FailedToVerifyHeader("invalid block: untrusted state is outside of trusting period");
        }
        require(
            proposedHeader.signedHeader.header.time > trustedConsensusState.timestamp,
            "invalid block: non monotonic bft time"
        );
        require(
            keccak256(bytes(proposedHeader.signedHeader.header.chainId)) == keccak256(bytes(chainId)),
            "invalid block: chain-id mismatch"
        );
        uint128 drifted = time + uint128(options.clockDrift) * 1_000_000_000;
        require(proposedHeader.signedHeader.header.time < drifted, "invalid block: header is from the future");

        uint64 trustedNextHeight = proposedHeader.trustedHeight.revisionHeight + 1;
        if (proposedHeader.signedHeader.header.height <= trustedNextHeight) {
            require(
                proposedHeader.signedHeader.header.height == trustedNextHeight, "invalid block: non increasing height"
            );
        }
    }

    function _validateBasic(
        ISpectreTendermintMsgs.Header calldata header,
        ISpectreTendermintMsgs.ChainId memory headerChainId
    )
        private
        pure
    {
        if (headerChainId.revisionNumber != header.trustedHeight.revisionNumber) {
            revert ISpectreClientErrors.MismatchedRevisionHeights(
                headerChainId.revisionNumber, header.trustedHeight.revisionNumber
            );
        }

        IICS02ClientMsgs.Height memory height = IICS02ClientMsgs.Height({
            revisionNumber: headerChainId.revisionNumber, revisionHeight: header.signedHeader.header.height
        });

        if (HeightCmp.ge(header.trustedHeight, height)) {
            revert ISpectreClientErrors.InvalidHeaderHeight(height.revisionHeight);
        }
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

    function _buildOutput(
        ISpectreClientMsgs.MsgUpdateApplicationState calldata msg_,
        uint64 revisionNumber
    )
        private
        pure
        returns (ISpectreClientMsgs.VerifyHeaderOutput memory)
    {
        IICS02ClientMsgs.Height memory trustedHeight = IICS02ClientMsgs.Height({
            revisionNumber: revisionNumber, revisionHeight: msg_.proposedHeader.trustedHeight.revisionHeight
        });
        IICS02ClientMsgs.Height memory newHeight = IICS02ClientMsgs.Height({
            revisionNumber: revisionNumber, revisionHeight: msg_.proposedHeader.signedHeader.header.height
        });
        ISpectreTendermintMsgs.ConsensusState memory newConsensusState = ISpectreTendermintMsgs.ConsensusState({
            timestamp: msg_.proposedHeader.signedHeader.header.time,
            root: msg_.proposedHeader.signedHeader.header.appHash,
            nextValidatorsHash: msg_.proposedHeader.signedHeader.header.nextValidatorsHash
        });
        return ISpectreClientMsgs.VerifyHeaderOutput({
            trustedConsensusState: msg_.trustedConsensusState,
            newConsensusState: newConsensusState,
            trustedHeight: trustedHeight,
            newHeight: newHeight
        });
    }
}
