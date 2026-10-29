// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";

/// @dev Exposes the internal checkExistenceProof so the depth bound (issue #110)
/// can be unit-tested directly.
contract MembershipDepthHarness is Membership {
    function exposedCheckExistenceProof(
        IMembershipMsgs.ExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec
    )
        external
        view
    {
        checkExistenceProof(proof, spec);
    }

    /// @dev calculateExistenceRoot is the function that actually loops over the
    /// path, and the non-membership path reaches it (via calculateNonExistenceRoot)
    /// BEFORE checkExistenceProof. The cap must live here.
    function exposedCalculateExistenceRoot(IMembershipMsgs.ExistenceProof memory proof)
        external
        view
        returns (bytes32)
    {
        return calculateExistenceRoot(proof);
    }
}

contract MembershipDepthTest is Test {
    MembershipDepthHarness internal m;

    uint256 internal constant MAX_DEPTH = 128; // mirrors Membership.MAX_PROOF_DEPTH

    function setUp() public {
        m = new MembershipDepthHarness();
    }

    // Build an existence proof whose leaf and inner ops match the Tendermint spec
    // (so validation reaches the depth gate) with a path of `pathLen` inner ops.
    function _proofWithPath(
        uint256 pathLen,
        IMembershipMsgs.ProofSpec memory spec
    )
        internal
        pure
        returns (IMembershipMsgs.ExistenceProof memory)
    {
        IMembershipMsgs.InnerOp[] memory path = new IMembershipMsgs.InnerOp[](pathLen);
        for (uint256 i = 0; i < pathLen; i++) {
            path[i] = IMembershipMsgs.InnerOp({ hashOp: IMembershipMsgs.HashOp.SHA256, prefix: hex"01", suffix: "" });
        }
        return IMembershipMsgs.ExistenceProof({
            key: bytes("key"),
            value: bytes("value"),
            leaf: IMembershipMsgs.LeafOp({
                hashOp: spec.leafOp.hashOp,
                prehashKey: spec.leafOp.prehashKey,
                prehashValue: spec.leafOp.prehashValue,
                prefix: spec.leafOp.prefix
            }),
            path: path
        });
    }

    function test_checkExistenceProof_revertsWhenPathExceedsMaxDepth() public {
        IMembershipMsgs.ProofSpec memory spec = m.tendermintSpec();
        IMembershipMsgs.ExistenceProof memory proof = _proofWithPath(MAX_DEPTH + 1, spec);

        vm.expectRevert(abi.encodeWithSelector(Membership.ProofPathTooLong.selector, MAX_DEPTH + 1, MAX_DEPTH));
        m.exposedCheckExistenceProof(proof, spec);
    }

    function test_checkExistenceProof_allowsPathAtMaxDepth() public view {
        IMembershipMsgs.ProofSpec memory spec = m.tendermintSpec();
        IMembershipMsgs.ExistenceProof memory proof = _proofWithPath(MAX_DEPTH, spec);

        // Reaching here without ProofPathTooLong means the boundary (== MAX_PROOF_DEPTH)
        // is accepted.
        m.exposedCheckExistenceProof(proof, spec);
    }

    function test_checkExistenceProof_allowsTypicalShallowPath() public view {
        IMembershipMsgs.ProofSpec memory spec = m.tendermintSpec();
        IMembershipMsgs.ExistenceProof memory proof = _proofWithPath(8, spec);
        m.exposedCheckExistenceProof(proof, spec);
    }

    // Direct proof that the cap lives in calculateExistenceRoot (the loop source),
    // not only in checkExistenceProof. Before the fix this would loop over the path
    // and return a root; now it reverts before the loop.
    function test_calculateExistenceRoot_revertsWhenPathExceedsMaxDepth() public {
        IMembershipMsgs.InnerOp[] memory longPath = new IMembershipMsgs.InnerOp[](MAX_DEPTH + 1);
        for (uint256 i = 0; i <= MAX_DEPTH; i++) {
            longPath[i] =
                IMembershipMsgs.InnerOp({ hashOp: IMembershipMsgs.HashOp.SHA256, prefix: hex"01", suffix: "" });
        }
        IMembershipMsgs.ExistenceProof memory proof = IMembershipMsgs.ExistenceProof({
            key: bytes("k"),
            value: bytes("v"),
            leaf: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.SHA256,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.SHA256,
                prefix: hex"00"
            }),
            path: longPath
        });
        vm.expectRevert(abi.encodeWithSelector(Membership.ProofPathTooLong.selector, MAX_DEPTH + 1, MAX_DEPTH));
        m.exposedCalculateExistenceRoot(proof);
    }

    // The non-membership path computes calculateNonExistenceRoot (-> calculateExistenceRoot,
    // which loops over the path) BEFORE checkExistenceProof runs. This guards against an
    // over-long nonExistenceProof.left/right path bypassing the cap via the public entrypoint.
    function test_membership_nonExistence_revertsWhenLeftPathExceedsMaxDepth() public {
        IMembershipMsgs.InnerOp[] memory longPath = new IMembershipMsgs.InnerOp[](MAX_DEPTH + 1);
        for (uint256 i = 0; i <= MAX_DEPTH; i++) {
            longPath[i] =
                IMembershipMsgs.InnerOp({ hashOp: IMembershipMsgs.HashOp.SHA256, prefix: hex"01", suffix: "" });
        }

        IMembershipMsgs.ExistenceProof memory left = IMembershipMsgs.ExistenceProof({
            key: bytes("k"),
            value: bytes("v"),
            leaf: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.SHA256,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.SHA256,
                prefix: hex"00"
            }),
            path: longPath
        });

        IMembershipMsgs.ExistenceProof memory emptyProof; // unused right neighbour
        IMembershipMsgs.NonExistenceProof memory ne = IMembershipMsgs.NonExistenceProof({
            key: bytes("absent"), hasLeft: true, left: left, hasRight: false, right: emptyProof
        });

        // proofs.length must equal proofSpecs.length (2) to reach calculateNonExistenceRoot.
        IMembershipMsgs.CommitmentProof[] memory proofs = new IMembershipMsgs.CommitmentProof[](2);
        proofs[0].proofType = IMembershipMsgs.ProofType.NON_EXIST;
        proofs[0].nonExistenceProof = ne;
        proofs[1].proofType = IMembershipMsgs.ProofType.EXIST; // unused; reverts before this is read

        IMembershipMsgs.MerkleProof[] memory mps = new IMembershipMsgs.MerkleProof[](1);
        mps[0] = IMembershipMsgs.MerkleProof({ proofs: proofs });

        bytes[] memory path = new bytes[](2);
        path[0] = bytes("ibc");
        path[1] = bytes("receipts/key");
        IMembershipMsgs.KVPair[] memory kvs = new IMembershipMsgs.KVPair[](1);
        kvs[0] = IMembershipMsgs.KVPair({ path: path, value: bytes("") }); // empty value -> non-membership

        vm.expectRevert(abi.encodeWithSelector(Membership.ProofPathTooLong.selector, MAX_DEPTH + 1, MAX_DEPTH));
        m.verifyMembership(bytes32(uint256(0xABCD)), kvs, mps);
    }
}
