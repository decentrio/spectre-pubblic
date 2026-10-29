// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { SpectreClient } from "contracts/light-clients/spectre/SpectreClient.sol";
import { UpdateClient } from "contracts/light-clients/spectre/modules/UpdateClient.sol";
import { Misbehaviour } from "contracts/light-clients/spectre/modules/Misbehaviour.sol";
import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { ISpectreClientMsgs } from "contracts/light-clients/spectre/messages/ISpectreClientMsgs.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";
import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";

contract CacheAlwaysTrueVerifier {
    function verifyProof(bytes calldata, uint256[2] calldata) external pure returns (bool) {
        return true;
    }
}

contract CacheAlwaysFailingVerifier {
    function verifyProof(bytes calldata, uint256[2] calldata) external pure returns (bool) {
        revert();
    }
}

contract UpdateClientCacheTest is Test {
    SignatureVerifier internal wrapper;
    UpdateClient internal updateClientImpl;
    CacheAlwaysTrueVerifier internal stubBucket;
    CacheAlwaysFailingVerifier internal failingStub;

    address constant STUB_MEMBERSHIP = address(0xBABE);
    address constant STUB_MISBEHAVIOUR = address(0xBEEF);

    string constant CHAIN_ID = "cosmoshub-0";
    uint64 constant TRUSTED_HEIGHT = 1000;
    uint64 constant HEIGHT_1001 = 1001;
    uint64 constant HEIGHT_1002 = 1002;
    uint128 constant TRUSTED_TS_NS = 1_700_000_000 * 1e9;
    uint128 constant TS_1001_NS = 1_700_000_010 * 1e9;
    uint128 constant TS_1002_NS = 1_700_000_020 * 1e9;
    uint32 constant TRUSTING_PERIOD = 14 days;
    uint32 constant UNBONDING_PERIOD = 21 days;
    uint16 constant BUCKET = 4;

    function setUp() public {
        vm.warp(1_700_000_030);

        wrapper = new SignatureVerifier(address(this));
        stubBucket = new CacheAlwaysTrueVerifier();
        failingStub = new CacheAlwaysFailingVerifier();
        updateClientImpl = new UpdateClient(address(wrapper));
        wrapper.setBucket(BUCKET, address(stubBucket), CacheAlwaysTrueVerifier.verifyProof.selector);

        // SpectreClient refuses a module without code. These two are never reached by these tests.
        vm.etch(STUB_MEMBERSHIP, hex"00");
        vm.etch(STUB_MISBEHAVIOUR, hex"00");
    }

    function test_constructor_pinsInitialValidatorSet() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        SpectreClient ics07 = _deployLightClient(_trustedConsensus(pinned), pinned);

        (uint32[] memory indices, bytes32[] memory pubkeys, uint64[] memory votingPowers) =
            ics07.getPinnedValidatorSet();
        assertEq(indices.length, pinned.validators.length, "pinned length");
        for (uint256 i = 0; i < pinned.validators.length; i++) {
            assertEq(indices[i], i, "pinned index");
            assertEq(pubkeys[i], pinned.validators[i].pubKey, "pinned pubkey");
            assertEq(votingPowers[i], pinned.validators[i].votingPower, "pinned power");
        }
    }

    function test_getConsensusStateHash_returnsTrustedStateHash() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        assertEq(ics07.getConsensusStateHash(TRUSTED_HEIGHT), keccak256(abi.encode(trustedCS)));
    }

    function test_getConsensusStateHash_revertsAtUnstoredHeight() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        SpectreClient ics07 = _deployLightClient(_trustedConsensus(pinned), pinned);

        vm.expectRevert(ISpectreClientErrors.ConsensusStateNotFound.selector);
        ics07.getConsensusStateHash(TRUSTED_HEIGHT - 1);
    }

    function test_getConsensusStateHash_returnsUpdatedStateHash() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 3);

        ics07.updateApplicationState(abi.encode(msg_));
        assertEq(ics07.getConsensusStateHash(HEIGHT_1001), keccak256(abi.encode(_consensusFromMsg(msg_))));
    }

    function test_constructor_reverts_whenGenesisPinnedSetMismatchesConsensusState() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory other = _buildValSet(4, 100);
        // Genesis consensus state commits to `pinned`'s hash, but the constructor is asked to pin
        // `other` instead — LC-03 must reject this mismatch on-chain rather than silently
        // deploying with an unverifiable genesis pin.
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);

        vm.expectRevert(
            abi.encodeWithSelector(
                ISpectreClientErrors.GenesisPinnedValidatorSetMismatch.selector,
                Header.hashValSet(pinned),
                Header.hashValSet(other)
            )
        );
        _deployLightClient(trustedCS, other);
    }

    function test_constructor_reverts_whenGenesisPinnedSetHasDuplicatePubkey() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        pinned.validators[2].pubKey = pinned.validators[0].pubKey;
        // Built after mutating pinned so the genesis-pin hash still matches (isolates the
        // dup-pubkey check in ValidatorSetLib.buildCache from LC-03's genesis-pin check).
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.DuplicateValidatorPubkey.selector, uint256(0), uint256(2))
        );
        _deployLightClient(trustedCS, pinned);
    }

    function test_constructor_reverts_whenGenesisPinnedSetHasZeroVotingPower() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        pinned.validators[1].votingPower = 0;
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);

        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.ZeroVotingPower.selector, uint256(1)));
        _deployLightClient(trustedCS, pinned);
    }

    function test_updateApplicationState_reverts_whenTrustedConsensusStateHashMismatchesStored() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory other = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        // Tampering with any field of the supplied trusted consensus state still fails because
        // UpdateClient binds it to the stored consensus-state hash. The pinned set itself is
        // allowed to lag trustedConsensusState.nextValidatorsHash on the app-state path, so this is
        // no longer rejected by _verifyPinnedQuorum.
        ISpectreTendermintMsgs.ConsensusState memory staleCS = ISpectreTendermintMsgs.ConsensusState({
            timestamp: trustedCS.timestamp, root: trustedCS.root, nextValidatorsHash: Header.hashValSet(other)
        });
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(staleCS, pinned, 3);

        vm.expectRevert(
            abi.encodeWithSelector(
                ISpectreClientErrors.ConsensusStateHashMismatch.selector,
                keccak256(abi.encode(trustedCS)),
                keccak256(abi.encode(staleCS))
            )
        );
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateApplicationState_acceptsPinnedQuorum() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 3);
        ILightClientMsgs.UpdateResult result = ics07.updateApplicationState(abi.encode(msg_));

        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.Update));
        ISpectreTendermintMsgs.ClientState memory updated =
            abi.decode(ics07.getClientState(), (ISpectreTendermintMsgs.ClientState));
        assertEq(updated.latestHeight.revisionHeight, HEIGHT_1001, "latest height");
    }

    function test_updateApplicationState_acceptsLaggingPinnedSetAfterNextValidatorsHashDrift() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory drifted = _buildValSet(4, 0);
        drifted.validators[0].votingPower = 101;
        drifted.totalVotingPower = 401;

        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1001 =
            _buildMsgWithNextHash(trustedCS, pinned, Header.hashValSet(drifted), 3);
        ics07.updateApplicationState(abi.encode(update1001));
        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = _consensusFromMsg(update1001);

        // The trusted consensus state at 1001 commits to `drifted` as nextValidatorsHash, but the
        // contract is still pinned to `pinned` because the cheap app-state path does not rotate.
        // A second app-state update must remain valid as long as the current pinned set still has
        // >2/3 overlap with the target block's signers.
        ISpectreClientMsgs.MsgUpdateApplicationState memory update1002 =
            _buildMsgAt(HEIGHT_1001, HEIGHT_1002, trustedCS1001, drifted, Header.hashValSet(drifted), TS_1002_NS, 3);
        ILightClientMsgs.UpdateResult result = ics07.updateApplicationState(abi.encode(update1002));

        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.Update));
        ISpectreTendermintMsgs.ClientState memory updated =
            abi.decode(ics07.getClientState(), (ISpectreTendermintMsgs.ClientState));
        assertEq(updated.latestHeight.revisionHeight, HEIGHT_1002, "latest height");

        (,, uint64[] memory votingPowers) = ics07.getPinnedValidatorSet();
        assertEq(votingPowers[0], pinned.validators[0].votingPower, "app-state path must not rotate pin");
    }

    function test_updateConsensusState_acceptsLaggingPinnedSetAfterLatestNextValidatorsHashDrift() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);

        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1001 =
            _buildMsgWithNextHash(trustedCS, pinnedA, Header.hashValSet(pinnedB), 3);
        ics07.updateApplicationState(abi.encode(update1001));
        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = _consensusFromMsg(update1001);

        // The latest trusted consensus state now commits to pinnedB as nextValidatorsHash, while
        // the on-chain pinned set is still pinnedA. RLY-01 recovery must be able to rotate through
        // updateConsensusState in that lagging state instead of requiring identity freshness.
        ISpectreClientMsgs.MsgUpdateApplicationState memory rotate1002 =
            _buildMsgAt(HEIGHT_1001, HEIGHT_1002, trustedCS1001, pinnedA, Header.hashValSet(pinnedB), TS_1002_NS, 3);
        ILightClientMsgs.UpdateResult result = ics07.updateConsensusState(
            abi.encode(ISpectreClientMsgs.MsgUpdateConsensusState({ update: rotate1002, newValidatorSet: pinnedB }))
        );

        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.Update));
        ISpectreTendermintMsgs.ClientState memory updated =
            abi.decode(ics07.getClientState(), (ISpectreTendermintMsgs.ClientState));
        assertEq(updated.latestHeight.revisionHeight, HEIGHT_1002, "latest height");

        (, bytes32[] memory pubkeys,) = ics07.getPinnedValidatorSet();
        assertEq(pubkeys[0], pinnedB.validators[0].pubKey, "consensus path rotates pin after drift");
    }

    function test_updateApplicationState_reverts_whenPinnedSnapshotOutsideTrustingPeriod() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        uint128 nearSnapshotExpiry = TRUSTED_TS_NS + (uint128(TRUSTING_PERIOD) - 1) * 1e9;
        vm.warp(uint256(nearSnapshotExpiry / 1e9));

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1001 = _buildMsgAt(
            TRUSTED_HEIGHT, HEIGHT_1001, trustedCS, pinned, Header.hashValSet(pinned), nearSnapshotExpiry, 3
        );
        update1001.time = nearSnapshotExpiry;
        ics07.updateApplicationState(abi.encode(update1001));

        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = _consensusFromMsg(update1001);
        uint128 staleSnapshotTime = TRUSTED_TS_NS + (uint128(TRUSTING_PERIOD) + 1) * 1e9;
        vm.warp(uint256(staleSnapshotTime / 1e9));

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1002 = _buildMsgAt(
            HEIGHT_1001, HEIGHT_1002, trustedCS1001, pinned, Header.hashValSet(pinned), nearSnapshotExpiry + 1e9, 3
        );
        update1002.time = staleSnapshotTime;

        vm.expectRevert(
            abi.encodeWithSelector(
                ISpectreClientErrors.InsufficientTrustingPeriod.selector,
                uint128(TRUSTING_PERIOD) + 1,
                uint128(TRUSTING_PERIOD)
            )
        );
        ics07.updateApplicationState(abi.encode(update1002));
    }

    /// @dev The relayer's forced rotation re-pins an unchanged validator set (#541). That must be
    ///      accepted and must refresh the snapshot timestamp, or a chain with a static validator set
    ///      expires one trusting period after client creation.
    function test_updateConsensusState_rePinningUnchangedSetRefreshesSnapshot() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);
        bytes32 pinnedHash = Header.hashValSet(pinned);

        uint128 nearSnapshotExpiry = TRUSTED_TS_NS + (uint128(TRUSTING_PERIOD) - 1) * 1e9;
        vm.warp(uint256(nearSnapshotExpiry / 1e9));

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1001 =
            _buildMsgAt(TRUSTED_HEIGHT, HEIGHT_1001, trustedCS, pinned, pinnedHash, nearSnapshotExpiry, 3);
        update1001.time = nearSnapshotExpiry;
        _updateConsensusState(ics07, update1001, pinned);
        assertEq(ics07.getPinnedValidatorsHash(), pinnedHash, "pinned hash unchanged");

        // Past the creation snapshot's trusting period: without the re-pin this reverts with
        // InsufficientTrustingPeriod (see test_updateApplicationState_reverts_whenPinnedSnapshotOutsideTrustingPeriod).
        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = _consensusFromMsg(update1001);
        uint128 afterOriginalExpiry = TRUSTED_TS_NS + (uint128(TRUSTING_PERIOD) + 1) * 1e9;
        vm.warp(uint256(afterOriginalExpiry / 1e9));

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1002 =
            _buildMsgAt(HEIGHT_1001, HEIGHT_1002, trustedCS1001, pinned, pinnedHash, nearSnapshotExpiry + 1e9, 3);
        update1002.time = afterOriginalExpiry;
        ILightClientMsgs.UpdateResult result = ics07.updateApplicationState(abi.encode(update1002));

        assertEq(uint8(result), uint8(ILightClientMsgs.UpdateResult.Update));
    }

    function test_updateApplicationState_revertsAtTrustingPeriodBoundary() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        uint128 boundaryTime = TRUSTED_TS_NS + uint128(TRUSTING_PERIOD) * 1e9;
        vm.warp(uint256(boundaryTime / 1e9));

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 3);
        msg_.time = boundaryTime;

        vm.expectRevert(
            abi.encodeWithSelector(
                ISpectreClientErrors.InsufficientTrustingPeriod.selector,
                uint128(TRUSTING_PERIOD),
                uint128(TRUSTING_PERIOD)
            )
        );
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateApplicationState_reverts_whenPinnedQuorumIsInsufficient() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 2);

        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.InsufficientVotingPower.selector, 200, 400));
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateApplicationState_reverts_whenPinnedIndexIsDuplicated() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 3);
        msg_.proof.pinnedValidatorIndices[1] = 0;
        msg_.proof.signerPubkeys[1] = pinned.validators[0].pubKey;

        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.DuplicateSigner.selector, 0));
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateApplicationState_reverts_whenPinnedIndexIsOutOfRange() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 3);
        msg_.proof.pinnedValidatorIndices[0] = 4;

        vm.expectRevert(abi.encodeWithSelector(ISpectreClientErrors.SignerIndexOutOfRange.selector, 4));
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateConsensusState_updatesPinnedSet() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ =
            _buildMsgWithNextHash(trustedCS, pinnedA, Header.hashValSet(pinnedB), 3);
        _updateConsensusState(ics07, msg_, pinnedB);

        (, bytes32[] memory pubkeys,) = ics07.getPinnedValidatorSet();
        assertEq(pubkeys[0], pinnedB.validators[0].pubKey, "re-anchored pubkey");
    }

    /// @dev The relayer anchors client expiry on getPinnedSnapshot (#541). It must report the
    ///      snapshot the trusting-period check uses: unmoved by updateApplicationState even though
    ///      latestHeight advances, moved by updateConsensusState.
    function test_getPinnedSnapshot_tracksRotationNotApplicationUpdates() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);

        SpectreClient appOnly = _deployLightClient(trustedCS, pinned);
        (uint64 height, uint128 timestamp) = appOnly.getPinnedSnapshot();
        assertEq(height, TRUSTED_HEIGHT, "constructor snapshot height");
        assertEq(timestamp, TRUSTED_TS_NS, "constructor snapshot timestamp");

        appOnly.updateApplicationState(abi.encode(_buildMsg(trustedCS, pinned, 3)));
        (height, timestamp) = appOnly.getPinnedSnapshot();
        assertEq(height, TRUSTED_HEIGHT, "app-state update must not move the snapshot height");
        assertEq(timestamp, TRUSTED_TS_NS, "app-state update must not move the snapshot timestamp");

        SpectreClient rotated = _deployLightClient(trustedCS, pinned);
        _updateConsensusState(rotated, _buildMsg(trustedCS, pinned, 3), pinned);
        (height, timestamp) = rotated.getPinnedSnapshot();
        assertEq(height, HEIGHT_1001, "rotation moves the snapshot height");
        assertEq(timestamp, TS_1001_NS, "rotation moves the snapshot timestamp");
    }

    function test_updateConsensusState_reverts_whenNewSetHashDoesNotMatchHeader() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        bytes32 expected = Header.hashValSet(pinnedA);
        bytes32 actual = Header.hashValSet(pinnedB);
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ =
            _buildMsgWithNextHash(trustedCS, pinnedA, expected, 3);

        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.MismatchedValidatorHashes.selector, expected, actual)
        );
        _updateConsensusState(ics07, msg_, pinnedB);
    }

    function test_updateConsensusState_doesNotUpdatePinnedSetWhenHeaderIsMisbehaviour() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        ISpectreClientMsgs.MsgUpdateApplicationState memory good = _buildMsg(trustedCS, pinnedA, 3);
        ics07.updateApplicationState(abi.encode(good));

        ISpectreClientMsgs.MsgUpdateApplicationState memory conflicting =
            _buildMsgWithNextHash(trustedCS, pinnedA, Header.hashValSet(pinnedB), 3);
        _updateConsensusState(ics07, conflicting, pinnedB);

        (, bytes32[] memory pubkeys,) = ics07.getPinnedValidatorSet();
        assertEq(pubkeys[0], pinnedA.validators[0].pubKey, "misbehaviour re-anchor must not mutate pin");
        ISpectreTendermintMsgs.ClientState memory updated =
            abi.decode(ics07.getClientState(), (ISpectreTendermintMsgs.ClientState));
        assertTrue(updated.isFrozen, "misbehaviour still freezes");
    }

    function test_updateConsensusState_reverts_whenNoOpHeaderIsNotLatestHeight() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedC = _buildValSet(4, 0);
        pinnedC.validators[3].pubKey = bytes32(uint256(0xC0FFEE));
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        ISpectreClientMsgs.MsgUpdateApplicationState memory reanchorB =
            _buildMsgWithNextHash(trustedCS, pinnedA, Header.hashValSet(pinnedB), 3);
        _updateConsensusState(ics07, reanchorB, pinnedB);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = _consensusFromMsg(reanchorB);

        ISpectreClientMsgs.MsgUpdateApplicationState memory reanchorC =
            _buildMsgAt(HEIGHT_1001, HEIGHT_1002, trustedCS1001, pinnedB, Header.hashValSet(pinnedC), TS_1002_NS, 3);
        _updateConsensusState(ics07, reanchorC, pinnedC);

        // Replaying reanchorB is now rejected by the NoOp branch's latest-height guard: the stored
        // consensus state for height 1001 matches reanchorB, but 1001 is no longer the latest
        // height after reanchorC advanced the client to 1002.
        vm.expectRevert(
            abi.encodeWithSelector(ISpectreClientErrors.NonMonotonicHeightUpdate.selector, HEIGHT_1002, HEIGHT_1001)
        );
        _updateConsensusState(ics07, reanchorB, pinnedB);
    }

    function test_updateApplicationState_reverts_whenProofVerificationFails() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        wrapper.setBucket(BUCKET, address(failingStub), CacheAlwaysFailingVerifier.verifyProof.selector);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ = _buildMsg(trustedCS, pinned, 3);
        vm.expectRevert(ISpectreClientErrors.ProofVerificationFailed.selector);
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateConsensusState_reverts_whenProofVerificationFails() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        wrapper.setBucket(BUCKET, address(failingStub), CacheAlwaysFailingVerifier.verifyProof.selector);

        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ =
            _buildMsgWithNextHash(trustedCS, pinnedA, Header.hashValSet(pinnedB), 3);
        vm.expectRevert(ISpectreClientErrors.ProofVerificationFailed.selector);
        _updateConsensusState(ics07, msg_, pinnedB);
    }

    function test_updateApplicationState_reverts_whenTrustedHeightNotStored() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinned = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinned);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinned);

        uint64 unstoredHeight = TRUSTED_HEIGHT - 1;
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ =
            _buildMsgAt(unstoredHeight, HEIGHT_1001, trustedCS, pinned, Header.hashValSet(pinned), TS_1001_NS, 3);
        vm.expectRevert(ISpectreClientErrors.ConsensusStateNotFound.selector);
        ics07.updateApplicationState(abi.encode(msg_));
    }

    function test_updateConsensusState_reverts_whenTrustedHeightNotStored() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClient(trustedCS, pinnedA);

        uint64 unstoredHeight = TRUSTED_HEIGHT - 1;
        ISpectreClientMsgs.MsgUpdateApplicationState memory msg_ =
            _buildMsgAt(unstoredHeight, HEIGHT_1001, trustedCS, pinnedA, Header.hashValSet(pinnedB), TS_1001_NS, 3);
        vm.expectRevert(ISpectreClientErrors.ConsensusStateNotFound.selector);
        _updateConsensusState(ics07, msg_, pinnedB);
    }

    function test_misbehaviourUsesHistoricalPinnedSetAfterReAnchor() public {
        ISpectreTendermintMsgs.ValidatorSet memory pinnedA = _buildValSet(4, 0);
        ISpectreTendermintMsgs.ValidatorSet memory pinnedB = _buildValSet(4, 100);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS = _trustedConsensus(pinnedA);
        SpectreClient ics07 = _deployLightClientWithMisbehaviour(trustedCS, pinnedA);

        ISpectreClientMsgs.MsgUpdateApplicationState memory reanchorB =
            _buildMsgWithNextHash(trustedCS, pinnedA, Header.hashValSet(pinnedB), 3);
        _updateConsensusState(ics07, reanchorB, pinnedB);
        ISpectreTendermintMsgs.ConsensusState memory trustedCS1001 = _consensusFromMsg(reanchorB);

        ISpectreClientMsgs.MsgUpdateApplicationState memory update1002 =
            _buildMsgAt(HEIGHT_1001, HEIGHT_1002, trustedCS1001, pinnedB, Header.hashValSet(pinnedB), TS_1002_NS, 3);
        ics07.updateApplicationState(abi.encode(update1002));
        ISpectreTendermintMsgs.ConsensusState memory trustedCS1002 = _consensusFromMsg(update1002);

        uint64 height1003 = HEIGHT_1002 + 1;
        uint128 ts1003Ns = TS_1002_NS + 10 * 1e9;

        ISpectreTendermintMsgs.Header memory h1 =
            _buildHeader(HEIGHT_1002, height1003, pinnedB, Header.hashValSet(pinnedB), ts1003Ns, 3);
        ISpectreTendermintMsgs.Header memory h2 =
            _buildHeader(HEIGHT_1002, height1003, pinnedB, Header.hashValSet(pinnedA), ts1003Ns, 3);

        ISpectreClientMsgs.MsgSubmitMisbehaviour memory msg_;
        msg_.misbehaviour = ISpectreClientMsgs.Misbehaviour({ header1: h1, header2: h2 });
        msg_.trustedConsensusState1 = trustedCS1002;
        msg_.trustedConsensusState2 = trustedCS1002;
        msg_.time = ts1003Ns;
        msg_.proof1 = _buildMisbehaviourProof(pinnedB, 3);
        msg_.proof2 = _buildMisbehaviourProof(pinnedB, 3);

        ics07.misbehaviour(abi.encode(msg_));
        ISpectreTendermintMsgs.ClientState memory updated =
            abi.decode(ics07.getClientState(), (ISpectreTendermintMsgs.ClientState));
        assertTrue(updated.isFrozen, "historical pinned evidence freezes");
    }

    function _updateConsensusState(
        SpectreClient ics07,
        ISpectreClientMsgs.MsgUpdateApplicationState memory update,
        ISpectreTendermintMsgs.ValidatorSet memory newValidatorSet
    )
        internal
    {
        ics07.updateConsensusState(
            abi.encode(ISpectreClientMsgs.MsgUpdateConsensusState({ update: update, newValidatorSet: newValidatorSet }))
        );
    }

    function _deployLightClient(
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.ValidatorSet memory pinned
    )
        internal
        returns (SpectreClient)
    {
        return new SpectreClient(
            address(updateClientImpl),
            STUB_MEMBERSHIP,
            STUB_MISBEHAVIOUR,
            abi.encode(_clientState()),
            trustedCS,
            pinned,
            address(0)
        );
    }

    function _deployLightClientWithMisbehaviour(
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.ValidatorSet memory pinned
    )
        internal
        returns (SpectreClient)
    {
        Misbehaviour misbehaviourImpl = new Misbehaviour(address(wrapper));
        return new SpectreClient(
            address(updateClientImpl),
            STUB_MEMBERSHIP,
            address(misbehaviourImpl),
            abi.encode(_clientState()),
            trustedCS,
            pinned,
            address(0)
        );
    }

    function _buildMsg(
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.ValidatorSet memory pinned,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreClientMsgs.MsgUpdateApplicationState memory)
    {
        return _buildMsgWithNextHash(trustedCS, pinned, Header.hashValSet(pinned), activeCount);
    }

    function _buildMsgWithNextHash(
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.ValidatorSet memory pinned,
        bytes32 nextValidatorsHash,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreClientMsgs.MsgUpdateApplicationState memory msg_)
    {
        return _buildMsgAt(TRUSTED_HEIGHT, HEIGHT_1001, trustedCS, pinned, nextValidatorsHash, TS_1001_NS, activeCount);
    }

    function _buildMsgAt(
        uint64 trustedHeight,
        uint64 newHeight,
        ISpectreTendermintMsgs.ConsensusState memory trustedCS,
        ISpectreTendermintMsgs.ValidatorSet memory pinned,
        bytes32 nextValidatorsHash,
        uint128 headerTime,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreClientMsgs.MsgUpdateApplicationState memory msg_)
    {
        ISpectreTendermintMsgs.Header memory header =
            _buildHeader(trustedHeight, newHeight, pinned, nextValidatorsHash, headerTime, activeCount);

        msg_.trustedConsensusState = trustedCS;
        msg_.proposedHeader = header;
        msg_.time = TS_1002_NS;
        msg_.proof = _buildMisbehaviourProof(pinned, activeCount);
    }

    function _buildMisbehaviourProof(
        ISpectreTendermintMsgs.ValidatorSet memory pinned,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreClientMsgs.BatchProof memory proof_)
    {
        proof_.proof = [uint256(0), 0, 0, 0, 0, 0, 0, 0];
        proof_.commitments = [uint256(0), 0];
        proof_.commitmentPok = [uint256(0), 0];
        proof_.bucket = BUCKET;
        proof_.signerIndices = new uint32[](BUCKET);
        proof_.pinnedValidatorIndices = new uint32[](BUCKET);
        proof_.signerPubkeys = new bytes32[](BUCKET);
        proof_.active = new bool[](BUCKET);
        for (uint256 i = 0; i < BUCKET; i++) {
            proof_.signerIndices[i] = uint32(i);
            proof_.pinnedValidatorIndices[i] = uint32(i);
            if (i < activeCount) {
                proof_.signerPubkeys[i] = pinned.validators[i].pubKey;
                proof_.active[i] = true;
            }
        }
    }

    function _consensusFromMsg(ISpectreClientMsgs.MsgUpdateApplicationState memory msg_)
        internal
        pure
        returns (ISpectreTendermintMsgs.ConsensusState memory)
    {
        return ISpectreTendermintMsgs.ConsensusState({
            timestamp: msg_.proposedHeader.signedHeader.header.time,
            root: msg_.proposedHeader.signedHeader.header.appHash,
            nextValidatorsHash: msg_.proposedHeader.signedHeader.header.nextValidatorsHash
        });
    }

    function _buildHeader(
        uint64 trustedHeight,
        uint64 newHeight,
        ISpectreTendermintMsgs.ValidatorSet memory currentValSet,
        bytes32 nextValidatorsHash,
        uint128 headerTime,
        uint16 activeCount
    )
        internal
        pure
        returns (ISpectreTendermintMsgs.Header memory header)
    {
        ISpectreTendermintMsgs.BlockHeader memory bh;
        bh.chainId = CHAIN_ID;
        bh.height = newHeight;
        bh.time = headerTime;
        bh.appHash = bytes32(uint256(0xCC0000 + newHeight));
        bh.validatorsHash = Header.hashValSet(currentValSet);
        bh.nextValidatorsHash = nextValidatorsHash;
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
            // The commit slot has to name the validator it belongs to, or the quorum check rejects
            // the proof that cites it (ZK-09). Here the commit and the pinned set are the same
            // validators in the same order, so slot i is validator i.
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

    function _buildValSet(
        uint16 valCount,
        uint256 seed
    )
        internal
        pure
        returns (ISpectreTendermintMsgs.ValidatorSet memory vs)
    {
        ISpectreTendermintMsgs.ValidatorInfo[] memory vals = new ISpectreTendermintMsgs.ValidatorInfo[](valCount);
        uint64 total = 0;
        for (uint256 i = 0; i < valCount; i++) {
            vals[i] = ISpectreTendermintMsgs.ValidatorInfo({
                valAddress: abi.encodePacked(uint160(seed + i + 1)),
                pubKey: bytes32(uint256(0xA0000000 + seed + i + 1)),
                votingPower: 100,
                proposerPriority: 0
            });
            total += 100;
        }
        vs = ISpectreTendermintMsgs.ValidatorSet({
            validators: vals, hasProposer: false, proposer: vals[0], totalVotingPower: total
        });
    }

    function _trustedConsensus(ISpectreTendermintMsgs.ValidatorSet memory pinned)
        internal
        pure
        returns (ISpectreTendermintMsgs.ConsensusState memory)
    {
        return ISpectreTendermintMsgs.ConsensusState({
            timestamp: TRUSTED_TS_NS, root: bytes32(uint256(0xAAA1)), nextValidatorsHash: Header.hashValSet(pinned)
        });
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
}
