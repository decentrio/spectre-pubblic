// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { SpectreClient } from "contracts/light-clients/spectre/SpectreClient.sol";
import { Misbehaviour } from "contracts/light-clients/spectre/modules/Misbehaviour.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { ISignatureVerifier } from "contracts/light-clients/spectre/interfaces/ISignatureVerifier.sol";
import { IMembership } from "contracts/light-clients/spectre/interfaces/IMembership.sol";
import { IUpdateClient } from "contracts/light-clients/spectre/interfaces/IUpdateClient.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";
import { Header as HeaderLib } from "contracts/light-clients/spectre/libraries/Header.sol";
import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";
import { IAccessControl } from "@openzeppelin-contracts/access/IAccessControl.sol";

contract MockVerifierForMisbehaviour is ISignatureVerifier {
    bool internal result = true;
    uint256 public calls;

    function setResult(bool result_) external {
        result = result_;
    }

    function verifyBatchProof(
        uint16,
        uint256[8] calldata,
        uint256[2] calldata,
        uint256[2] calldata,
        bytes32[] calldata,
        bool[] calldata,
        ISignatureVerifier.SharedBlock calldata
    )
        external
        returns (bool)
    {
        calls++;
        return result;
    }
}

contract DummyMembershipForMisbehaviour is IMembership {
    function verifyMembership(
        bytes32,
        IMembershipMsgs.KVPair[] calldata,
        IMembershipMsgs.MerkleProof[] calldata
    )
        external
        pure { }
}

contract DummyUpdateClientForMisbehaviour is IUpdateClient {
    function verifyHeader(ISpectreClientMsgs.MsgUpdateApplicationState calldata msg_)
        external
        pure
        returns (ISpectreClientMsgs.VerifyHeaderOutput memory)
    {
        ISpectreTendermintMsgs.Header calldata header = msg_.proposedHeader;
        return ISpectreClientMsgs.VerifyHeaderOutput({
            trustedConsensusState: msg_.trustedConsensusState,
            newConsensusState: ISpectreTendermintMsgs.ConsensusState({
                timestamp: header.signedHeader.header.time,
                root: header.signedHeader.header.appHash,
                nextValidatorsHash: header.signedHeader.header.nextValidatorsHash
            }),
            trustedHeight: header.trustedHeight,
            newHeight: IICS02ClientMsgs.Height({
                revisionNumber: header.trustedHeight.revisionNumber, revisionHeight: header.signedHeader.header.height
            })
        });
    }
}

contract MisbehaviourTest is Test, ISpectreTendermintMsgs {
    SpectreClient internal lightClient;
    Misbehaviour internal misbehaviourVerifier;
    MockVerifierForMisbehaviour internal mockVerifier;
    DummyMembershipForMisbehaviour internal dummyMembership;
    DummyUpdateClientForMisbehaviour internal dummyUpdateClient;

    ClientState internal clientState_;
    ConsensusState internal consensusState_;
    bytes32 internal consensusStateHash_;
    ValidatorSet internal valset_;
    bytes32 internal valSetHash_;

    uint128 internal constant TRUSTED_TIME_NANOS = 1_700_000_000_000_000_000;
    uint128 internal constant HEADER_TIME_NANOS = 1_700_000_100_000_000_000;
    string internal constant CHAIN_ID = "test-chain-0";

    struct LegacyMsgSubmitMisbehaviour {
        ISpectreTendermintMsgs.ClientState clientState;
        ISpectreClientMsgs.Misbehaviour misbehaviour;
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState1;
        ISpectreTendermintMsgs.ConsensusState trustedConsensusState2;
        uint128 time;
    }

    function setUp() public {
        mockVerifier = new MockVerifierForMisbehaviour();
        misbehaviourVerifier = new Misbehaviour(address(mockVerifier));
        dummyMembership = new DummyMembershipForMisbehaviour();
        dummyUpdateClient = new DummyUpdateClientForMisbehaviour();

        ValidatorInfo[] memory vals = new ValidatorInfo[](4);
        vals[0] = _validator("val0", bytes32(uint256(1)), 25);
        vals[1] = _validator("val1", bytes32(uint256(2)), 25);
        vals[2] = _validator("val2", bytes32(uint256(3)), 25);
        vals[3] = _validator("val3", bytes32(uint256(4)), 25);

        valset_ = ValidatorSet({
            validators: vals, hasProposer: false, proposer: _validator("", bytes32(0), 0), totalVotingPower: 100
        });
        valSetHash_ = HeaderLib.hashValSet(valset_);

        consensusState_ = ConsensusState({
            timestamp: TRUSTED_TIME_NANOS, root: bytes32(uint256(0xABCD)), nextValidatorsHash: valSetHash_
        });
        consensusStateHash_ = keccak256(abi.encode(consensusState_));

        IICS02ClientMsgs.Height memory latestHeight = IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: 10 });

        clientState_ = ClientState({
            chainId: CHAIN_ID,
            trustLevel: TrustThreshold({ numerator: 1, denominator: 3 }),
            latestHeight: latestHeight,
            trustingPeriod: 3600,
            unbondingPeriod: 7200,
            isFrozen: false,
            clockDrift: 15
        });

        lightClient = _deploy(address(0));

        vm.warp(1_700_000_500);
    }

    function _deploy(address roleManager) internal returns (SpectreClient) {
        return _deployWithModules(
            address(dummyUpdateClient), address(dummyMembership), address(misbehaviourVerifier), roleManager
        );
    }

    function _deployWithModules(
        address updateClientModule,
        address membershipModule,
        address misbehaviourModule,
        address roleManager
    )
        internal
        returns (SpectreClient)
    {
        return new SpectreClient(
            updateClientModule,
            membershipModule,
            misbehaviourModule,
            abi.encode(clientState_),
            consensusState_,
            valset_,
            roleManager
        );
    }

    function test_misbehaviour_freezesWithProofBackedQuorums() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        bytes memory encoded = abi.encode(msg_);

        lightClient.misbehaviour(encoded);

        assertTrue(_isFrozen(lightClient), "client must freeze after both header proofs verify");
        assertEq(mockVerifier.calls(), 2, "both header proofs must be verified");
    }

    function test_misbehaviour_revertsWithGarbageBytes() public {
        bytes memory garbage = bytes("this is not a valid encoded message");
        vm.expectRevert();
        lightClient.misbehaviour(garbage);
        assertFalse(_isFrozen(lightClient), "garbage bytes must not freeze");
    }

    function test_misbehaviour_revertsWithEmptyBytes() public {
        bytes memory empty = bytes("");
        vm.expectRevert();
        lightClient.misbehaviour(empty);
        assertFalse(_isFrozen(lightClient), "empty bytes must not freeze");
    }

    function test_misbehaviour_cannotFreezeLegacyMessageWithoutProofs() public {
        bytes memory encoded = _buildLegacyMisbehaviourMsg();
        vm.expectRevert();
        lightClient.misbehaviour(encoded);

        assertFalse(_isFrozen(lightClient), "legacy no-proof message must not freeze");
        assertEq(mockVerifier.calls(), 0, "legacy message must not reach verifier");
    }

    function test_misbehaviour_cannotFreezeWhenVerifierRejectsProof() public {
        mockVerifier.setResult(false);
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        bytes memory encoded = abi.encode(msg_);

        vm.expectRevert(ISpectreClientErrors.ProofVerificationFailed.selector);
        lightClient.misbehaviour(encoded);

        assertFalse(_isFrozen(lightClient), "rejected proof must not freeze");
    }

    function test_misbehaviour_cannotFreezeWithSubQuorumProofMetadata() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.proof1 = _proof(2);

        bytes memory encoded = abi.encode(msg_);
        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.InsufficientVotingPower.selector, 50, 100));
        lightClient.misbehaviour(encoded);

        assertFalse(_isFrozen(lightClient), "sub-quorum proof metadata must not freeze");
        assertEq(mockVerifier.calls(), 0, "sub-quorum metadata must not reach verifier");
    }

    function test_misbehaviour_cannotFreezeWithStaleTime() public {
        // Long-range freeze attempt: `time` is consistent with the (old) trusted state's trusting
        // period but lags the wall clock beyond clockDrift. Must revert before any other check.
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.time = 1_700_000_200 * 1e9; // wall clock is 1_700_000_500, clockDrift is 15s

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.ProofIsTooOld.selector, 1_700_000_500, 1_700_000_200)
        );
        lightClient.misbehaviour(abi.encode(msg_));

        assertFalse(_isFrozen(lightClient), "stale time must not freeze");
        assertEq(mockVerifier.calls(), 0, "stale time must not reach verifier");
    }

    function test_misbehaviour_cannotFreezeWithFutureTime() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.time = 1_700_000_516 * 1e9; // 16s ahead of wall clock, beyond the 15s clockDrift

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.ProofIsInTheFuture.selector, 1_700_000_500, 1_700_000_516)
        );
        lightClient.misbehaviour(abi.encode(msg_));

        assertFalse(_isFrozen(lightClient), "future time must not freeze");
        assertEq(mockVerifier.calls(), 0, "future time must not reach verifier");
    }

    function test_misbehaviour_allowsFutureTimeWithinClockDrift() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.time = 1_700_000_501 * 1e9; // 1s ahead of wall clock, within the 15s clockDrift

        lightClient.misbehaviour(abi.encode(msg_));

        assertTrue(_isFrozen(lightClient), "future time within clockDrift must pass freshness");
        assertEq(mockVerifier.calls(), 2, "freshness must allow proof verification");
    }

    function test_misbehaviour_revertsWhenHistoricalSnapshotHasExpired() public {
        // Advance the trusted consensus state without re-anchoring the pinned validator set. The
        // initial snapshot at height 10 then remains the quorum snapshot for evidence at height 20.
        ConsensusState memory latestConsensus = _advanceApplicationState(20, uint128(block.timestamp) * 1e9);

        // The height-20 consensus state is still trusted, while its validator snapshot at height 10
        // is just outside the trusting period. `time` is deliberately fresh to isolate the snapshot.
        vm.warp(uint256(TRUSTED_TIME_NANOS / 1e9) + clientState_.trustingPeriod + 1);
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        uint128 headerTime = uint128(block.timestamp) * 1e9;
        msg_.misbehaviour.header1 =
            _headerAtTrustedHeightAndTime(_buildHeader(25, bytes32(uint256(0xAAA1))), 20, headerTime);
        msg_.misbehaviour.header2 =
            _headerAtTrustedHeightAndTime(_buildHeader(25, bytes32(uint256(0xAAA2))), 20, headerTime);
        msg_.trustedConsensusState1 = latestConsensus;
        msg_.trustedConsensusState2 = latestConsensus;

        vm.expectRevert(
            abi.encodeWithSelector(
                ISpectreClientErrors.InsufficientTrustingPeriod.selector, uint128(3601), uint128(3600)
            )
        );
        lightClient.misbehaviour(abi.encode(msg_));

        assertFalse(_isFrozen(lightClient), "expired historical snapshot must not freeze");
        assertEq(mockVerifier.calls(), 0, "expired snapshot must not reach verifier");
    }

    function test_misbehaviour_acceptsSnapshotJustInsideTrustingPeriod() public {
        vm.warp(uint256(TRUSTED_TIME_NANOS / 1e9) + clientState_.trustingPeriod - 1);

        lightClient.misbehaviour(abi.encode(_buildValidMisbehaviourMsg()));

        assertTrue(_isFrozen(lightClient), "snapshot just inside trusting period must be accepted");
        assertEq(mockVerifier.calls(), 2, "valid near-boundary evidence must reach verifier");
    }

    function test_misbehaviour_rejectsSpoofedTrustedOverlapForNonAdjacentHeaders() public {
        ValidatorSet memory attackerValSet = _attackerValidatorSet();
        ValidatorSet memory spoofedTrustedValSet = _trustedValidatorSetWithSpoofedAddresses();

        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.misbehaviour.header1 =
            _buildHeaderWithSets(15, bytes32(uint256(0xAAA1)), attackerValSet, spoofedTrustedValSet);
        msg_.misbehaviour.header2 =
            _buildHeaderWithSets(15, bytes32(uint256(0xAAA2)), attackerValSet, spoofedTrustedValSet);
        msg_.proof1 = _proofForValidatorSet(attackerValSet, 3);
        msg_.proof2 = _proofForValidatorSet(attackerValSet, 3);

        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.PubkeyMismatch.selector, uint32(0)));
        lightClient.misbehaviour(abi.encode(msg_));

        assertFalse(_isFrozen(lightClient), "spoofed trusted overlap must not freeze");
    }

    function test_misbehaviour_rejectsProofSignerAbsentFromCommitSigs() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.misbehaviour.header1.signedHeader.commit.commitSigs[0] = _commitSig(CommitSigFlag.BLOCK_ID_FLAG_ABSENT);

        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.ProofSignerCommitSigMismatch.selector, uint32(0)));
        lightClient.misbehaviour(abi.encode(msg_));

        assertFalse(_isFrozen(lightClient), "proof signer absent from commit must not freeze");
        assertEq(mockVerifier.calls(), 0, "mismatched commit metadata must not reach verifier");
    }

    function test_misbehaviour_revertsForMismatchedHeaderHeights() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.misbehaviour.header2 = _buildHeader(16, bytes32(uint256(0xAAA2)));

        bytes memory encoded = abi.encode(msg_);
        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.MismatchedMisbehaviourHeaderHeights.selector, 15, 16)
        );
        lightClient.misbehaviour(encoded);

        assertFalse(_isFrozen(lightClient), "different-height headers must not freeze");
        assertEq(mockVerifier.calls(), 0, "different-height headers must not reach verifier");
    }

    function test_misbehaviour_revertsForUnauthorizedCallerWhenManaged() public {
        address governance = makeAddr("governance");
        address unauthorized = makeAddr("unauthorized");
        SpectreClient managedClient = _deploy(governance);

        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        bytes memory encoded = abi.encode(msg_);

        bytes32 misbehaviourRole = managedClient.MISBEHAVIOUR_SUBMITTER_ROLE();

        vm.prank(unauthorized);
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector, unauthorized, misbehaviourRole
            )
        );
        managedClient.misbehaviour(encoded);

        assertFalse(_isFrozen(managedClient), "client must not be frozen by unauthorized caller");
    }

    function test_misbehaviour_distinctRoleFromProofSubmitter() public view {
        assertNotEq(
            keccak256("PROOF_SUBMITTER_ROLE"),
            lightClient.MISBEHAVIOUR_SUBMITTER_ROLE(),
            "MISBEHAVIOUR_SUBMITTER_ROLE must be distinct from PROOF_SUBMITTER_ROLE"
        );
    }

    function test_unfreeze_revertsWhenNotFrozen() public {
        address governance = makeAddr("governance");
        SpectreClient managedClient = _deploy(governance);

        vm.prank(governance);
        vm.expectRevert(ISpectreClientErrors.ClientNotFrozen.selector);
        managedClient.unfreeze();
    }

    function test_unfreeze_revertsForNonAdmin() public {
        address governance = makeAddr("governance");
        address unauthorized = makeAddr("unauthorized");
        SpectreClient managedClient = _deploy(governance);

        vm.prank(unauthorized);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, unauthorized, bytes32(0))
        );
        managedClient.unfreeze();
    }

    // ============ Modules without code ============

    /// @dev A delegatecall to an address without code succeeds and returns nothing, which for a module whose interface
    /// returns nothing reads as "verified". The addresses are immutable, so the constructor is the only chance.
    function test_constructor_revertsWhenAnyModuleHasNoCode() public {
        address notAContract = makeAddr("not-a-contract");
        address[2] memory withoutCode = [address(0), notAContract];

        for (uint256 i = 0; i < withoutCode.length; ++i) {
            address bad = withoutCode[i];
            bytes memory expected = abi.encodeWithSelector(ISpectreClientErrors.ModuleHasNoCode.selector, bad);

            vm.expectRevert(expected);
            _deployWithModules(bad, address(dummyMembership), address(misbehaviourVerifier), address(0));

            vm.expectRevert(expected);
            _deployWithModules(address(dummyUpdateClient), bad, address(misbehaviourVerifier), address(0));

            vm.expectRevert(expected);
            _deployWithModules(address(dummyUpdateClient), address(dummyMembership), bad, address(0));
        }
    }

    /// @dev Control for the reproduction of the missing-module freeze: two identical headers are not misbehaviour, so
    /// the real module rejects them even though their proof metadata names a >2/3 quorum.
    function test_misbehaviour_identicalHeadersNeverFreeze() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.misbehaviour.header2 = msg_.misbehaviour.header1;
        bytes memory encoded = abi.encode(msg_);

        vm.expectRevert(Misbehaviour.MisbehaviourNotDetected.selector);
        lightClient.misbehaviour(encoded);

        assertFalse(_isFrozen(lightClient), "identical headers must not freeze");
        assertEq(mockVerifier.calls(), 0, "identical headers must not reach the verifier");
    }

    /// @dev The same evidence with the module's code gone. This used to freeze the client without a single proof
    /// check: the delegatecall to the empty address succeeded and `verifyMisbehaviour` returns nothing.
    function test_misbehaviour_revertsWhenModuleHasNoCode() public {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        msg_.misbehaviour.header2 = msg_.misbehaviour.header1;
        bytes memory encoded = abi.encode(msg_);
        vm.etch(address(misbehaviourVerifier), hex"");

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.ModuleHasNoCode.selector, address(misbehaviourVerifier))
        );
        lightClient.misbehaviour(encoded);

        assertFalse(_isFrozen(lightClient), "a module without code must not freeze the client");
        assertEq(mockVerifier.calls(), 0, "no proof was checked");
    }

    function test_updateApplicationState_revertsWhenModuleHasNoCode() public {
        bytes memory encoded = abi.encode(_applicationUpdateMsg(20, uint128(block.timestamp) * 1e9));
        vm.etch(address(dummyUpdateClient), hex"");

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.ModuleHasNoCode.selector, address(dummyUpdateClient))
        );
        lightClient.updateApplicationState(encoded);
    }

    function test_updateConsensusState_revertsWhenModuleHasNoCode() public {
        bytes memory encoded = abi.encode(
            ISpectreClientMsgs.MsgUpdateConsensusState({
                update: _applicationUpdateMsg(20, uint128(block.timestamp) * 1e9), newValidatorSet: valset_
            })
        );
        vm.etch(address(dummyUpdateClient), hex"");

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.ModuleHasNoCode.selector, address(dummyUpdateClient))
        );
        lightClient.updateConsensusState(encoded);
    }

    function _buildValidMisbehaviourMsg() internal view returns (ISpectreClientMsgs.MsgSubmitMisbehaviour memory) {
        ISpectreTendermintMsgs.Header memory header1 = _buildHeader(15, bytes32(uint256(0xAAA1)));
        ISpectreTendermintMsgs.Header memory header2 = _buildHeader(15, bytes32(uint256(0xAAA2)));

        ISpectreClientMsgs.Misbehaviour memory misbehaviour_ =
            ISpectreClientMsgs.Misbehaviour({ header1: header1, header2: header2 });

        return ISpectreClientMsgs.MsgSubmitMisbehaviour({
            misbehaviour: misbehaviour_,
            trustedConsensusState1: consensusState_,
            trustedConsensusState2: consensusState_,
            time: uint128(block.timestamp) * 1_000_000_000,
            proof1: _proof(3),
            proof2: _proof(3)
        });
    }

    function _buildLegacyMisbehaviourMsg() internal view returns (bytes memory) {
        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_ = _buildValidMisbehaviourMsg();
        return abi.encode(
            LegacyMsgSubmitMisbehaviour({
                clientState: clientState_,
                misbehaviour: msg_.misbehaviour,
                trustedConsensusState1: msg_.trustedConsensusState1,
                trustedConsensusState2: msg_.trustedConsensusState2,
                time: msg_.time
            })
        );
    }

    function _applicationUpdateMsg(
        uint64 newHeight,
        uint128 consensusTimestamp
    )
        internal
        view
        returns (ISpectreClientMsgs.MsgUpdateApplicationState memory msg_)
    {
        msg_.proposedHeader = _headerAtTrustedHeightAndTime(
            _buildHeader(newHeight, bytes32(uint256(0xBBBB))),
            clientState_.latestHeight.revisionHeight,
            consensusTimestamp
        );
        msg_.trustedConsensusState = consensusState_;
        msg_.time = uint128(block.timestamp) * 1e9;
        msg_.proof = _proof(3);
    }

    function _advanceApplicationState(
        uint64 newHeight,
        uint128 consensusTimestamp
    )
        internal
        returns (ConsensusState memory latestConsensus)
    {
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _applicationUpdateMsg(newHeight, consensusTimestamp);

        lightClient.updateApplicationState(abi.encode(msg_));

        latestConsensus = ConsensusState({
            timestamp: consensusTimestamp,
            root: msg_.proposedHeader.signedHeader.header.appHash,
            nextValidatorsHash: msg_.proposedHeader.signedHeader.header.nextValidatorsHash
        });
    }

    function _proof(uint256 activeCount) internal view returns (ISpectreClientMsgs.BatchProof memory proof_) {
        return _proofForValidatorSet(valset_, activeCount);
    }

    function _proofForValidatorSet(
        ValidatorSet memory validatorSet,
        uint256 activeCount
    )
        internal
        pure
        returns (ISpectreClientMsgs.BatchProof memory proof_)
    {
        proof_.bucket = 4;
        proof_.signerIndices = new uint32[](4);
        proof_.pinnedValidatorIndices = new uint32[](4);
        proof_.signerPubkeys = new bytes32[](4);
        proof_.active = new bool[](4);

        for (uint32 i = 0; i < 4; i++) {
            proof_.signerIndices[i] = i;
            proof_.pinnedValidatorIndices[i] = i;
            proof_.signerPubkeys[i] = validatorSet.validators[i].pubKey;
            proof_.active[i] = i < activeCount;
        }
    }

    function _isFrozen(SpectreClient client) internal view returns (bool) {
        ClientState memory state = abi.decode(client.getClientState(), (ClientState));
        return state.isFrozen;
    }

    function _buildHeader(
        uint64 height,
        bytes32 lastBlockIdHash
    )
        internal
        view
        returns (ISpectreTendermintMsgs.Header memory)
    {
        return _buildHeaderWithSets(height, lastBlockIdHash, valset_, valset_);
    }

    function _buildHeaderWithSets(
        uint64 height,
        bytes32 lastBlockIdHash,
        ValidatorSet memory currentValSet,
        ValidatorSet memory trustedNextValSet
    )
        internal
        pure
        returns (ISpectreTendermintMsgs.Header memory)
    {
        bytes32 currentValSetHash = HeaderLib.hashValSet(currentValSet);
        BlockHeader memory blockHeader = BlockHeader({
            version: Version({ blockVersion: 11, appVersion: 0 }),
            chainId: CHAIN_ID,
            height: height,
            time: HEADER_TIME_NANOS,
            hasLastBlockId: true,
            lastBlockId: BlockId({
                hashData: lastBlockIdHash,
                partSetHeader: PartSetHeader({ total: 1, hashData: bytes32(uint256(0x1234)) })
            }),
            hasLastCommitHash: false,
            lastCommitHash: bytes32(0),
            hasDataHash: false,
            dataHash: bytes32(0),
            validatorsHash: currentValSetHash,
            nextValidatorsHash: currentValSetHash,
            consensusHash: bytes32(uint256(0xCAFE)),
            appHash: bytes32(uint256(0xBEEF)),
            hasLastResultsHash: false,
            lastResultsHash: bytes32(0),
            hasEvidenceHash: false,
            evidenceHash: bytes32(0),
            proposerAddress: bytes("prop")
        });

        bytes32 headerHash = HeaderLib.hashHeader(blockHeader);

        BlockCommit memory commit = BlockCommit({
            height: height,
            round: 0,
            blockId: BlockId({
                hashData: headerHash, partSetHeader: PartSetHeader({ total: 1, hashData: bytes32(uint256(0x5678)) })
            }),
            commitSigs: _commitSigsForValidatorSet(currentValSet)
        });

        SignedHeader memory signedHeader = SignedHeader({ header: blockHeader, commit: commit });

        IICS02ClientMsgs.Height memory trustedHeight =
            IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: 10 });

        return ISpectreTendermintMsgs.Header({ signedHeader: signedHeader, trustedHeight: trustedHeight });
    }

    function _headerAtTrustedHeightAndTime(
        ISpectreTendermintMsgs.Header memory header,
        uint64 trustedHeight,
        uint128 timestamp
    )
        internal
        pure
        returns (ISpectreTendermintMsgs.Header memory)
    {
        header.trustedHeight.revisionHeight = trustedHeight;
        header.signedHeader.header.time = timestamp;
        header.signedHeader.commit.blockId.hashData = HeaderLib.hashHeader(header.signedHeader.header);
        return header;
    }

    function _attackerValidatorSet() internal pure returns (ValidatorSet memory validatorSet) {
        ValidatorInfo[] memory vals = new ValidatorInfo[](4);
        vals[0] = _validator("spoof0", bytes32(uint256(0xA1)), 25);
        vals[1] = _validator("spoof1", bytes32(uint256(0xA2)), 25);
        vals[2] = _validator("spoof2", bytes32(uint256(0xA3)), 25);
        vals[3] = _validator("spoof3", bytes32(uint256(0xA4)), 25);

        validatorSet = ValidatorSet({
            validators: vals, hasProposer: false, proposer: _validator("", bytes32(0), 0), totalVotingPower: 100
        });
    }

    function _trustedValidatorSetWithSpoofedAddresses() internal view returns (ValidatorSet memory validatorSet) {
        ValidatorInfo[] memory vals = new ValidatorInfo[](4);
        vals[0] = _validator("spoof0", valset_.validators[0].pubKey, 25);
        vals[1] = _validator("spoof1", valset_.validators[1].pubKey, 25);
        vals[2] = _validator("spoof2", valset_.validators[2].pubKey, 25);
        vals[3] = _validator("spoof3", valset_.validators[3].pubKey, 25);

        validatorSet = ValidatorSet({
            validators: vals, hasProposer: false, proposer: _validator("", bytes32(0), 0), totalVotingPower: 100
        });
    }

    function _commitSigsForValidatorSet(ValidatorSet memory validatorSet)
        internal
        pure
        returns (CommitSig[] memory sigs)
    {
        sigs = new CommitSig[](validatorSet.validators.length);
        for (uint256 i = 0; i < validatorSet.validators.length; i++) {
            sigs[i] = CommitSig({
                flag: CommitSigFlag.BLOCK_ID_FLAG_COMMIT,
                validatorAddress: bytes20(sha256(abi.encodePacked(validatorSet.validators[i].pubKey)))
            });
        }
    }

    function _validator(string memory addr, bytes32 pubkey, uint64 power) internal pure returns (ValidatorInfo memory) {
        return ValidatorInfo({ valAddress: bytes(addr), pubKey: pubkey, votingPower: power, proposerPriority: 0 });
    }

    function _commitSig(CommitSigFlag flag) internal pure returns (CommitSig memory) {
        // validatorAddress is irrelevant for callers that only encode the commit; the quorum
        // path builds its sigs via _commitSigsForValidatorSet, which fills it in.
        return CommitSig({ flag: flag, validatorAddress: bytes20(0) });
    }
}
