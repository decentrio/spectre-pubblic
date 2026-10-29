// SPDX-License-Identifier: UNLICENSED
pragma solidity ^0.8.28;

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";

/// @title Spectre Messages
/// @notice Defines shared types for Spectre light client implementations.
interface ISpectreTendermintMsgs {
    /// @notice Fraction of validator overlap needed to update header
    /// @param numerator Numerator of the fraction
    /// @param denominator Denominator of the fraction
    struct TrustThreshold {
        uint8 numerator;
        uint8 denominator;
    }

    /// @notice Defines the Spectre ClientState
    /// @param chainId Chain ID
    /// @param trustLevel Fraction of validator overlap needed to update header.
    /// @dev LC-05: `trustLevel` is decoded into `Options.trustThreshold` in both
    ///      `UpdateClient.verifyHeader` and `Misbehaviour.verifyMisbehaviour`, but that decoded
    ///      value is never read afterward anywhere in this codebase — the real quorum threshold
    ///      is hardcoded `>2/3` in `SpectreClient._verifyQuorum`. This field is currently
    ///      decoded-but-unused / reserved. It is NOT removed here: removal is a wire-encoding /
    ///      ABI change requiring `EncodeTest` + the Go proto counterpart + bindings regen, out of
    ///      proportion for this low-severity finding.
    /// @param latestHeight Latest height the client was updated to
    /// @param trustingPeriod duration of the period since the LatestTimestamp during which the
    /// submitted headers are valid for upgrade in seconds.
    /// @param unbondingPeriod duration of the staking unbonding period in seconds
    /// @param isFrozen whether or not client is frozen (due to misbehavior)
    struct ClientState {
        string chainId;
        TrustThreshold trustLevel;
        IICS02ClientMsgs.Height latestHeight;
        uint32 trustingPeriod;
        uint32 unbondingPeriod;
        bool isFrozen;
        uint32 clockDrift;
    }

    /// @notice Defines the Spectre light client's consensus state at some height.
    /// @param timestamp timestamp that corresponds to the counterparty block height
    /// in which the ConsensusState was generated. (in unix nanoseconds)
    /// @param root commitment root (i.e app hash)
    /// @param nextValidatorsHash next validators hash
    struct ConsensusState {
        uint128 timestamp;
        bytes32 root;
        bytes32 nextValidatorsHash;
    }

    struct Header {
        SignedHeader signedHeader;
        IICS02ClientMsgs.Height trustedHeight;
    }

    struct SignedHeader {
        BlockHeader header;
        BlockCommit commit;
    }

    struct ValidatorSet {
        ValidatorInfo[] validators;
        bool hasProposer;
        ValidatorInfo proposer;
        uint64 totalVotingPower;
    }

    struct ValidatorInfo {
        bytes valAddress;
        bytes32 pubKey;
        uint64 votingPower;
        int64 proposerPriority;
    }

    struct SimpleValidator {
        bytes32 pubKey;
        uint64 votingPower;
    }

    struct BlockHeader {
        Version version;
        string chainId;
        uint64 height;
        uint128 time;
        bool hasLastBlockId;
        BlockId lastBlockId;
        bool hasLastCommitHash;
        bytes32 lastCommitHash;
        bool hasDataHash;
        bytes32 dataHash;
        bytes32 validatorsHash;
        bytes32 nextValidatorsHash;
        bytes32 consensusHash;
        bytes32 appHash;
        bool hasLastResultsHash;
        bytes32 lastResultsHash;
        bool hasEvidenceHash;
        bytes32 evidenceHash;
        bytes proposerAddress;
    }

    struct BlockCommit {
        uint64 height;
        uint32 round;
        BlockId blockId;
        CommitSig[] commitSigs;
    }

    struct BlockId {
        bytes32 hashData;
        PartSetHeader partSetHeader;
    }

    struct PartSetHeader {
        uint32 total;
        bytes32 hashData;
    }

    enum CommitSigFlag {
        /// unknown flag (maps to CometBFT BlockIDFlagUnknown = 0).
        BLOCK_ID_FLAG_UNKNOWN,
        /// no vote was received from a validator.
        BLOCK_ID_FLAG_ABSENT,
        /// voted for the Commit.BlockID.
        BLOCK_ID_FLAG_COMMIT,
        /// voted for nil.
        BLOCK_ID_FLAG_NIL
    }

    /// @notice One validator's entry in a block commit.
    /// @param flag whether the validator voted for the commit's BlockID, voted nil, or was absent.
    /// @param validatorAddress the Tendermint address of the validator this slot belongs to —
    ///        the first 20 bytes of sha256(ed25519 pubkey). It ties a commit slot to a specific
    ///        validator, which is what lets the quorum check verify that the commit slot a proof
    ///        cites is the same validator whose pinned-set voting power it is claiming (ZK-09).
    struct CommitSig {
        CommitSigFlag flag;
        bytes20 validatorAddress;
    }

    struct ChainId {
        string id;
        uint64 revisionNumber;
    }

    struct Options {
        TrustThreshold trustThreshold;
        uint32 trustingPeriod;
        uint32 clockDrift;
    }

    struct Version {
        uint64 blockVersion;
        uint64 appVersion;
    }
}
