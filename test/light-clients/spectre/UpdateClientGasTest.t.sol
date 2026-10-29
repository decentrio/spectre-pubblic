// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test, console } from "forge-std/Test.sol";

import { SpectreClient } from "contracts/light-clients/spectre/SpectreClient.sol";
import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";
import { UpdateClient } from "contracts/light-clients/spectre/modules/UpdateClient.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";

/// @dev Stub bucket verifier — always returns true so the gas measurement excludes only
///      the cost of the real Groth16 verification. Everything else (the real
///      `SpectreClient.updateApplicationState`, `SpectreClient._verifyPinnedQuorum`,
///      `UpdateClient.verifyHeader` and `SignatureVerifier._hashWitness`) runs unmodified.
contract AlwaysTrueVerifier {
    function verifyProof(bytes calldata, uint256[2] calldata) external pure returns (bool) {
        return true;
    }
}

/// @notice Production-realistic gas benchmark for `SpectreClient.updateApplicationState`.
///         Builds a self-consistent MsgUpdateApplicationState (validator-set hash matches
///         header field, header merkle root matches commit.blockId.hashData,
///         commitSigs cover the validator set) so the real `UpdateClient.verifyHeader`
///         path executes end-to-end. Only the per-bucket Groth16 verifier is
///         stubbed, so each measured number excludes the real Groth16 verification
///         cost; add that cost, measured separately, to estimate production gas.
///
///         Run:    forge test --match-contract UpdateClientGasTest -vv
///
///         === Topology (bench-matched) ===
///         For each bucket, validator set + active count just clear the 2/3
///         quorum threshold, matching how the relayer picks signers in prod:
///         n=16 → 20 vals total, 14 active, 2 padding (3·14·100 > 2·20·100 ⇒ pass).
contract UpdateClientGasTest is Test {
    SignatureVerifier wrapper;
    UpdateClient updateClientImpl;
    AlwaysTrueVerifier stubBucket;

    address constant STUB_MEMBERSHIP = address(0xBABE);
    address constant STUB_MISBEHAVIOUR = address(0xBEEF);

    // chainId revisionNumber must match latestHeight.revisionNumber.
    // "cosmoshub-0" → revisionNumber=0.
    string constant CHAIN_ID = "cosmoshub-0";
    uint64 constant TRUSTED_HEIGHT = 1000;
    uint64 constant NEW_HEIGHT = 1001;
    uint64 constant NEXT_HEIGHT = 1002;
    uint128 constant TRUSTED_TS_NS = 1_700_000_000 * 1e9;
    uint128 constant NEW_TS_NS = 1_700_000_010 * 1e9;
    uint128 constant NEXT_TS_NS = 1_700_000_020 * 1e9;
    uint32 constant TRUSTING_PERIOD = 14 days;
    uint32 constant UNBONDING_PERIOD = 21 days;

    struct BucketConfig {
        uint16 bucket; // padded slot count (== per-slot array lengths)
        uint16 valCount; // validators in proposedHeader.validatorSet
        uint16 activeCount; // real signers; padding = bucket - activeCount
    }

    struct LegacyCommitSigData {
        bytes validatorAddress;
        uint128 timestamp;
        bool hasSignature;
        bytes signature;
    }

    struct LegacyCommitSig {
        ISpectreTendermintMsgs.CommitSigFlag flag;
        LegacyCommitSigData data;
    }

    struct LegacyBlockCommit {
        uint64 height;
        uint32 round;
        ISpectreTendermintMsgs.BlockId blockId;
        LegacyCommitSig[] commitSigs;
    }

    struct LegacySignedHeader {
        ISpectreTendermintMsgs.BlockHeader header;
        LegacyBlockCommit commit;
    }

    struct LegacyHeader {
        LegacySignedHeader signedHeader;
        IICS02ClientMsgs.Height trustedHeight;
    }

    struct LegacyMsgUpdateApplicationState {
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState;
        LegacyHeader proposedHeader;
        uint128 time;
        ISpectreClientMsgs.BatchProof proof;
    }

    /// Bench-matched: active just clears 2/3 quorum of valCount.
    function _cfg(uint16 bucket) internal pure returns (BucketConfig memory) {
        if (bucket == 4) return BucketConfig(4, 4, 3);
        if (bucket == 8) return BucketConfig(8, 10, 7);
        if (bucket == 16) return BucketConfig(16, 20, 14);
        if (bucket == 32) return BucketConfig(32, 30, 21);
        if (bucket == 64) return BucketConfig(64, 60, 42);
        if (bucket == 128) return BucketConfig(128, 120, 84);
        revert("unknown bucket");
    }

    function setUp() public {
        // pin block.timestamp so the clock-drift check in
        // _validateClientStateAndTime accepts NEW_TS_NS.
        vm.warp(1_700_000_020);

        wrapper = new SignatureVerifier(address(this));
        stubBucket = new AlwaysTrueVerifier();
        updateClientImpl = new UpdateClient(address(wrapper));

        uint16[6] memory bs = [uint16(4), 8, 16, 32, 64, 128];
        for (uint256 i = 0; i < bs.length; i++) {
            wrapper.setBucket(bs[i], address(stubBucket), AlwaysTrueVerifier.verifyProof.selector);
        }

        // SpectreClient refuses a module without code. These two are never reached by this test.
        vm.etch(STUB_MEMBERSHIP, hex"00");
        vm.etch(STUB_MISBEHAVIOUR, hex"00");
    }

    function _clientState() internal pure returns (ISpectreTendermintMsgs.ClientState memory) {
        return ISpectreTendermintMsgs.ClientState({
            chainId: CHAIN_ID,
            trustLevel: ISpectreTendermintMsgs.TrustThreshold({ numerator: 1, denominator: 3 }),
            latestHeight: IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: TRUSTED_HEIGHT }),
            trustingPeriod: TRUSTING_PERIOD,
            unbondingPeriod: UNBONDING_PERIOD,
            isFrozen: false,
            clockDrift: 1800
        });
    }

    function _buildValSet(uint16 valCount) internal pure returns (ISpectreTendermintMsgs.ValidatorSet memory vs) {
        ISpectreTendermintMsgs.ValidatorInfo[] memory vals = new ISpectreTendermintMsgs.ValidatorInfo[](valCount);
        uint64 total = 0;
        for (uint256 i = 0; i < valCount; i++) {
            vals[i] = ISpectreTendermintMsgs.ValidatorInfo({
                valAddress: abi.encodePacked(uint160(i + 1)),
                pubKey: bytes32(uint256(0xA0000000 + i + 1)),
                votingPower: 100,
                proposerPriority: 0
            });
            total += 100;
        }
        vs = ISpectreTendermintMsgs.ValidatorSet({
            validators: vals, hasProposer: false, proposer: vals[0], totalVotingPower: total
        });
    }

    function _emptyValSet() internal pure returns (ISpectreTendermintMsgs.ValidatorSet memory vs) {
        vs = ISpectreTendermintMsgs.ValidatorSet({
            validators: new ISpectreTendermintMsgs.ValidatorInfo[](0),
            hasProposer: false,
            proposer: ISpectreTendermintMsgs.ValidatorInfo({
                valAddress: "", pubKey: bytes32(0), votingPower: 0, proposerPriority: 0
            }),
            totalVotingPower: 0
        });
    }

    /// Build commit signatures: first `activeCount` are COMMIT, rest are ABSENT. Length == val_count, matching
    /// CometBFT's invariant that a commit carries one CommitSig per validator.
    function _buildCommitSigs(
        ISpectreTendermintMsgs.ValidatorSet memory vs,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreTendermintMsgs.CommitSig[] memory sigs)
    {
        sigs = new ISpectreTendermintMsgs.CommitSig[](vs.validators.length);
        for (uint256 i = 0; i < vs.validators.length; i++) {
            // The commit slot names the validator it belongs to, or the quorum check rejects the
            // proof citing it (ZK-09). Commit and pinned set are the same validators in the same
            // order here, so slot i is validator i.
            bytes20 addr = bytes20(sha256(abi.encodePacked(vs.validators[i].pubKey)));
            if (i < activeCount) {
                sigs[i] = ISpectreTendermintMsgs.CommitSig({
                    flag: ISpectreTendermintMsgs.CommitSigFlag.BLOCK_ID_FLAG_COMMIT, validatorAddress: addr
                });
            } else {
                sigs[i] = ISpectreTendermintMsgs.CommitSig({
                    flag: ISpectreTendermintMsgs.CommitSigFlag.BLOCK_ID_FLAG_ABSENT, validatorAddress: addr
                });
            }
        }
    }

    function _buildLegacyCommitSigs(
        ISpectreTendermintMsgs.ValidatorSet memory vs,
        uint16 activeCount
    )
        internal
        pure
        returns (LegacyCommitSig[] memory sigs)
    {
        sigs = new LegacyCommitSig[](vs.validators.length);
        for (uint256 i = 0; i < vs.validators.length; i++) {
            if (i < activeCount) {
                sigs[i] = LegacyCommitSig({
                    flag: ISpectreTendermintMsgs.CommitSigFlag.BLOCK_ID_FLAG_COMMIT,
                    data: LegacyCommitSigData({
                        validatorAddress: vs.validators[i].valAddress,
                        timestamp: NEW_TS_NS,
                        hasSignature: false,
                        signature: ""
                    })
                });
            } else {
                sigs[i] = LegacyCommitSig({
                    flag: ISpectreTendermintMsgs.CommitSigFlag.BLOCK_ID_FLAG_ABSENT,
                    data: LegacyCommitSigData({
                        validatorAddress: "", timestamp: 0, hasSignature: false, signature: ""
                    })
                });
            }
        }
    }

    function _buildHeader(
        uint64 trustedHeight,
        uint64 newHeight,
        uint128 newTimestamp,
        bytes32 appHash,
        ISpectreTendermintMsgs.ValidatorSet memory currentValSet,
        ISpectreTendermintMsgs.ValidatorSet memory trustedNextValSet,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreTendermintMsgs.Header memory header)
    {
        bytes32 currentValSetHash = Header.hashValSet(currentValSet);

        ISpectreTendermintMsgs.BlockHeader memory bh;
        bh.chainId = CHAIN_ID;
        bh.height = newHeight;
        bh.time = newTimestamp;
        bh.appHash = appHash;
        bh.validatorsHash = currentValSetHash;
        bh.nextValidatorsHash = currentValSetHash;
        bytes32 headerHash = Header.hashHeader(bh);

        ISpectreTendermintMsgs.BlockCommit memory bc = ISpectreTendermintMsgs.BlockCommit({
            height: newHeight,
            round: 0,
            blockId: ISpectreTendermintMsgs.BlockId({
                hashData: headerHash,
                partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: bytes32(uint256(0x9A57)) })
            }),
            commitSigs: _buildCommitSigs(currentValSet, activeCount)
        });

        header = ISpectreTendermintMsgs.Header({
            signedHeader: ISpectreTendermintMsgs.SignedHeader({ header: bh, commit: bc }),
            trustedHeight: IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: trustedHeight })
        });
    }

    /// Build a fully consistent Header: validatorsHash matches hashValSet,
    /// blockId.hashData matches hashHeader, trustedNextValSet hashes back to
    /// the trustedConsensusState.nextValidatorsHash.
    function _buildSelfConsistent(BucketConfig memory cfg)
        internal
        pure
        returns (
            ISpectreTendermintMsgs.Header memory header,
            ISpectreTendermintMsgs.ConsensusState memory trustedCS,
            ISpectreTendermintMsgs.ValidatorSet memory vs
        )
    {
        vs = _buildValSet(cfg.valCount);
        bytes32 valSetHash = Header.hashValSet(vs);

        ISpectreTendermintMsgs.BlockHeader memory bh;
        bh.chainId = CHAIN_ID;
        bh.height = NEW_HEIGHT;
        bh.time = NEW_TS_NS;
        bh.appHash = bytes32(uint256(0xCCC));
        bh.validatorsHash = valSetHash;
        bh.nextValidatorsHash = valSetHash;
        bytes32 headerHash = Header.hashHeader(bh);

        ISpectreTendermintMsgs.BlockCommit memory bc = ISpectreTendermintMsgs.BlockCommit({
            height: NEW_HEIGHT,
            round: 0,
            blockId: ISpectreTendermintMsgs.BlockId({
                hashData: headerHash,
                partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: bytes32(uint256(0x9A57)) })
            }),
            commitSigs: _buildCommitSigs(vs, cfg.activeCount)
        });

        header = ISpectreTendermintMsgs.Header({
            signedHeader: ISpectreTendermintMsgs.SignedHeader({ header: bh, commit: bc }),
            trustedHeight: IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: TRUSTED_HEIGHT })
        });

        // trustedConsensusState.nextValidatorsHash must equal hashValSet(trustedNextValSet)
        // (we reuse the same set, so the hash is identical).
        trustedCS = ISpectreTendermintMsgs.ConsensusState({
            timestamp: TRUSTED_TS_NS, root: bytes32(uint256(0xAAA)), nextValidatorsHash: valSetHash
        });
    }

    function _deployLightClient(
        ISpectreTendermintMsgs.ClientState memory cs,
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.ValidatorSet memory pinnedValidatorSet
    )
        internal
        returns (SpectreClient)
    {
        // Deploy a fresh light client per bucket, pre-seeded with this run's
        // trustedConsensusState hash at TRUSTED_HEIGHT.
        return new SpectreClient(
            address(updateClientImpl),
            STUB_MEMBERSHIP,
            STUB_MISBEHAVIOUR,
            abi.encode(cs),
            trustedCS,
            pinnedValidatorSet,
            address(0) // permissionless
        );
    }

    function _buildMsg(
        ISpectreTendermintMsgs.ClientState memory cs,
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.Header memory header,
        ISpectreTendermintMsgs.ValidatorSet memory vs,
        uint16 bucket,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreClientMsgs.MsgUpdateApplicationState memory msg_)
    {
        // Build per-slot bucket arrays (active = first cfg.activeCount, rest = padding).
        uint32[] memory idx = new uint32[](bucket);
        bytes32[] memory pks = new bytes32[](bucket);
        bool[] memory act = new bool[](bucket);
        uint32[] memory pinnedValidatorIndices = new uint32[](bucket);
        for (uint256 i = 0; i < bucket; i++) {
            pinnedValidatorIndices[i] = uint32(i);
            if (i < activeCount) {
                idx[i] = uint32(i);
                pks[i] = vs.validators[i].pubKey;
                act[i] = true;
            }
        }

        msg_.trustedConsensusState = trustedCS;
        msg_.proposedHeader = header;
        msg_.time = header.signedHeader.header.time;
        msg_.proof.proof = [uint256(0), 0, 0, 0, 0, 0, 0, 0];
        msg_.proof.commitments = [uint256(0), 0];
        msg_.proof.commitmentPok = [uint256(0), 0];
        msg_.proof.bucket = bucket;
        msg_.proof.signerIndices = idx;
        msg_.proof.signerPubkeys = pks;
        msg_.proof.active = act;
        msg_.proof.pinnedValidatorIndices = pinnedValidatorIndices;
    }

    function _buildLegacyMsg(
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_,
        ISpectreTendermintMsgs.ValidatorSet memory vs,
        uint16 activeCount
    )
        internal
        pure
        returns (LegacyMsgUpdateApplicationState memory legacy)
    {
        legacy.trustedConsensusState = msg_.trustedConsensusState;
        legacy.proposedHeader = LegacyHeader({
            signedHeader: LegacySignedHeader({
                header: msg_.proposedHeader.signedHeader.header,
                commit: LegacyBlockCommit({
                    height: msg_.proposedHeader.signedHeader.commit.height,
                    round: msg_.proposedHeader.signedHeader.commit.round,
                    blockId: msg_.proposedHeader.signedHeader.commit.blockId,
                    commitSigs: _buildLegacyCommitSigs(vs, activeCount)
                })
            }),
            trustedHeight: msg_.proposedHeader.trustedHeight
        });
        legacy.time = msg_.time;
        legacy.proof = msg_.proof;
    }

    function _measureCalldataDiet(uint16 bucket) internal pure {
        BucketConfig memory cfg = _cfg(bucket);
        (
            ISpectreTendermintMsgs.Header memory header,
            ISpectreTendermintMsgs.ConsensusState memory trustedCS,
            ISpectreTendermintMsgs.ValidatorSet memory vs
        ) = _buildSelfConsistent(cfg);

        ISpectreTendermintMsgs.ClientState memory cs = _clientState();
        ISpectreClientMsgs.MsgUpdateApplicationState memory current =
            _buildMsg(cs, trustedCS, header, vs, bucket, cfg.activeCount);
        LegacyMsgUpdateApplicationState memory legacy = _buildLegacyMsg(current, vs, cfg.activeCount);

        uint256 beforeLen = abi.encode(legacy).length;
        uint256 afterLen = abi.encode(current).length;
        assertLt(afterLen, beforeLen, "calldata diet should shrink updateApplicationState");

        console.log("updateApplicationState calldata bucket", bucket);
        console.log("before bytes", beforeLen);
        console.log("after bytes", afterLen);
        console.log("saved bytes", beforeLen - afterLen);
    }

    function _measure(uint16 bucket) internal {
        BucketConfig memory cfg = _cfg(bucket);

        (
            ISpectreTendermintMsgs.Header memory header,
            ISpectreTendermintMsgs.ConsensusState memory trustedCS,
            ISpectreTendermintMsgs.ValidatorSet memory vs
        ) = _buildSelfConsistent(cfg);

        ISpectreTendermintMsgs.ClientState memory cs = _clientState();
        SpectreClient ics07 = _deployLightClient(cs, trustedCS, vs);
        bytes memory encoded = abi.encode(_buildMsg(cs, trustedCS, header, vs, bucket, cfg.activeCount));

        uint256 g0 = gasleft();
        ILightClientMsgs.UpdateResult result = ics07.updateApplicationState(encoded);
        uint256 used = g0 - gasleft();
        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.Update), "expected Update");
        console.log("bucket=", bucket, "  gas=", used);
    }

    function _measureCacheHitReplay(uint16 bucket) internal {
        BucketConfig memory cfg = _cfg(bucket);

        (
            ISpectreTendermintMsgs.Header memory header,
            ISpectreTendermintMsgs.ConsensusState memory trustedCS,
            ISpectreTendermintMsgs.ValidatorSet memory vs
        ) = _buildSelfConsistent(cfg);

        ISpectreTendermintMsgs.ClientState memory cs = _clientState();
        SpectreClient ics07 = _deployLightClient(cs, trustedCS, vs);

        ISpectreClientMsgs.MsgUpdateApplicationState memory fullMsg =
            _buildMsg(cs, trustedCS, header, vs, bucket, cfg.activeCount);
        assertEq(
            uint8(ics07.updateApplicationState(abi.encode(fullMsg))),
            uint8(ILightClientMsgs.UpdateResult.Update),
            "warmup Update"
        );

        ISpectreClientMsgs.MsgUpdateApplicationState memory cacheMsg =
            _buildMsg(cs, trustedCS, header, vs, bucket, cfg.activeCount);

        uint256 g0 = gasleft();
        ILightClientMsgs.UpdateResult result = ics07.updateApplicationState(abi.encode(cacheMsg));
        uint256 used = g0 - gasleft();
        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.NoOp), "expected cache-hit replay NoOp");
        console.log("bucket=", bucket, "  cache-hit replay gas=", used);
    }

    function _measureCacheHitAdjacentUpdate(uint16 bucket) internal {
        BucketConfig memory cfg = _cfg(bucket);
        ISpectreTendermintMsgs.ClientState memory cs = _clientState();
        ISpectreTendermintMsgs.ValidatorSet memory valSet = _buildValSet(cfg.valCount);
        bytes32 valSetHash = Header.hashValSet(valSet);

        ISpectreTendermintMsgs.ConsensusState memory trustedCS0 = ISpectreTendermintMsgs.ConsensusState({
            timestamp: TRUSTED_TS_NS, root: bytes32(uint256(0xAAA1)), nextValidatorsHash: valSetHash
        });

        SpectreClient ics07 = _deployLightClient(cs, trustedCS0, valSet);

        ISpectreTendermintMsgs.Header memory header1001 = _buildHeader(
            TRUSTED_HEIGHT, NEW_HEIGHT, NEW_TS_NS, bytes32(uint256(0xCCC1)), valSet, valSet, cfg.activeCount
        );
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg1001 =
            _buildMsg(cs, trustedCS0, header1001, valSet, bucket, cfg.activeCount);
        assertEq(
            uint8(ics07.updateApplicationState(abi.encode(msg1001))),
            uint8(ILightClientMsgs.UpdateResult.Update),
            "warmup Update"
        );

        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = ISpectreTendermintMsgs.ConsensusState({
            timestamp: NEW_TS_NS, root: header1001.signedHeader.header.appHash, nextValidatorsHash: valSetHash
        });

        ISpectreTendermintMsgs.Header memory header1002 = _buildHeader(
            NEW_HEIGHT, NEXT_HEIGHT, NEXT_TS_NS, bytes32(uint256(0xCCC2)), valSet, valSet, cfg.activeCount
        );

        ISpectreClientMsgs.MsgUpdateApplicationState memory cacheMsg =
            _buildMsg(cs, trustedCS1001, header1002, valSet, bucket, cfg.activeCount);

        uint256 g0 = gasleft();
        ILightClientMsgs.UpdateResult result = ics07.updateApplicationState(abi.encode(cacheMsg));
        uint256 used = g0 - gasleft();
        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.Update), "expected cache-hit adjacent Update");
        console.log("bucket=", bucket, "  cache-hit adjacent update gas=", used);
    }

    function test_gas_n4() public {
        _measure(4);
    }

    function test_calldataDiet_n4() public pure {
        _measureCalldataDiet(4);
    }

    function test_gas_n8() public {
        _measure(8);
    }

    function test_gas_n16() public {
        _measure(16);
    }

    function test_gas_n16_cacheHitReplay() public {
        _measureCacheHitReplay(16);
    }

    function test_gas_n16_cacheHitAdjacentUpdate() public {
        _measureCacheHitAdjacentUpdate(16);
    }

    function test_gas_n32() public {
        _measure(32);
    }

    function test_gas_n64() public {
        _measure(64);
    }

    function test_gas_n128() public {
        _measure(128);
    }

    function test_gas_n128_cacheHitReplay() public {
        _measureCacheHitReplay(128);
    }

    function test_gas_n128_cacheHitAdjacentUpdate() public {
        _measureCacheHitAdjacentUpdate(128);
    }
}
