// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";

/// @dev Guards the explicit left/right neighbour root cross-check in
/// calculateNonExistenceRoot (issue #112): a two-sided non-existence proof whose
/// neighbours hash to different subtree roots must be rejected, not silently
/// accepted by reusing the self-derived root.
contract MembershipNonExistHarness is Membership {
    function exposedCalculateNonExistenceRoot(IMembershipMsgs.NonExistenceProof memory proof)
        external
        view
        returns (bytes32)
    {
        return calculateNonExistenceRoot(proof);
    }

    function exposedVerifyNonExistenceProof(
        IMembershipMsgs.NonExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec,
        bytes32 root,
        bytes memory key
    )
        external
        view
        returns (bool)
    {
        return verifyNonExistenceProof(proof, spec, root, key);
    }
}

contract MembershipNonExistenceRootTest is Test {
    MembershipNonExistHarness internal m;

    function setUp() public {
        m = new MembershipNonExistHarness();
    }

    // A minimal-but-valid existence proof; `value` distinguishes the leaf hash so
    // two proofs can be made to produce distinct roots.
    function _existence(bytes memory value) internal pure returns (IMembershipMsgs.ExistenceProof memory) {
        return IMembershipMsgs.ExistenceProof({
            key: bytes("k"),
            value: value,
            leaf: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.SHA256,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.NO_HASH,
                prefix: ""
            }),
            path: new IMembershipMsgs.InnerOp[](0)
        });
    }

    function test_revertsWhenLeftAndRightRootsDiffer() public {
        IMembershipMsgs.ExistenceProof memory left = _existence(bytes("L"));
        IMembershipMsgs.ExistenceProof memory right = _existence(bytes("R")); // distinct value -> distinct root

        IMembershipMsgs.NonExistenceProof memory ne = IMembershipMsgs.NonExistenceProof({
            key: bytes("absent"), hasLeft: true, left: left, hasRight: true, right: right
        });

        bytes32 leftRoot = m.exposedCalculateNonExistenceRoot(_oneSided(left)); // compute expected roots for the error
        // args
        bytes32 rightRoot = m.exposedCalculateNonExistenceRoot(_oneSided(right));
        assertTrue(leftRoot != rightRoot, "test setup: roots must differ");

        vm.expectRevert(abi.encodeWithSelector(Membership.NonExistenceRootMismatch.selector, leftRoot, rightRoot));
        m.exposedCalculateNonExistenceRoot(ne);
    }

    function test_acceptsWhenLeftAndRightRootsMatch() public view {
        IMembershipMsgs.ExistenceProof memory same = _existence(bytes("S"));
        IMembershipMsgs.NonExistenceProof memory ne = IMembershipMsgs.NonExistenceProof({
            key: bytes("absent"), hasLeft: true, left: same, hasRight: true, right: same
        });
        // matching roots -> returns the shared root, no revert
        bytes32 root = m.exposedCalculateNonExistenceRoot(ne);
        assertTrue(root != bytes32(0), "expected a non-zero root");
    }

    function test_singleSidedSkipsCrossCheck() public view {
        IMembershipMsgs.ExistenceProof memory left = _existence(bytes("L"));
        IMembershipMsgs.NonExistenceProof memory ne = IMembershipMsgs.NonExistenceProof({
            key: bytes("absent"),
            hasLeft: true,
            left: left,
            hasRight: false,
            right: _existence(bytes("")) // ignored when hasRight == false
        });
        m.exposedCalculateNonExistenceRoot(ne); // no cross-check, no revert
    }

    function test_twoSidedEmptyNeighborPathRevertsCleanly() public {
        bytes memory prefix = abi.encodePacked(bytes32(uint256(0xABCDEF)));
        IMembershipMsgs.ExistenceProof memory left = _existenceWithPrefix(bytes("a"), bytes("L"), prefix);
        IMembershipMsgs.ExistenceProof memory right = _existenceWithPrefix(bytes("z"), bytes("R"), prefix);
        IMembershipMsgs.NonExistenceProof memory ne = IMembershipMsgs.NonExistenceProof({
            key: bytes("m"), hasLeft: true, left: left, hasRight: true, right: right
        });

        vm.expectRevert(Membership.EmptyNeighborPath.selector);
        m.exposedVerifyNonExistenceProof(ne, _prefixRootSpec(prefix), bytes32(uint256(0xABCDEF)), bytes("m"));
    }

    function _oneSided(IMembershipMsgs.ExistenceProof memory e)
        internal
        pure
        returns (IMembershipMsgs.NonExistenceProof memory)
    {
        return IMembershipMsgs.NonExistenceProof({
            key: bytes("x"), hasLeft: true, left: e, hasRight: false, right: _existence(bytes(""))
        });
    }

    function _existenceWithPrefix(
        bytes memory key,
        bytes memory value,
        bytes memory prefix
    )
        internal
        pure
        returns (IMembershipMsgs.ExistenceProof memory)
    {
        return IMembershipMsgs.ExistenceProof({
            key: key,
            value: value,
            leaf: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.NO_HASH,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.NO_HASH,
                prefix: prefix
            }),
            path: new IMembershipMsgs.InnerOp[](0)
        });
    }

    function _prefixRootSpec(bytes memory prefix) internal pure returns (IMembershipMsgs.ProofSpec memory) {
        uint32[] memory childOrder = new uint32[](2);
        childOrder[0] = 0;
        childOrder[1] = 1;

        return IMembershipMsgs.ProofSpec({
            specType: IMembershipMsgs.SpecType.TENDERMINT,
            hasLeafSpec: true,
            leafOp: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.NO_HASH,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.NO_HASH,
                prefix: prefix
            }),
            hasInnerSpec: true,
            innerSpec: IMembershipMsgs.InnerSpec({
                childOrder: childOrder,
                childSize: 32,
                minPrefixLength: 0,
                maxPrefixLength: 32,
                emptyChild: "",
                hashOp: IMembershipMsgs.HashOp.NO_HASH
            }),
            minDepth: 0,
            maxDepth: 0,
            prehashKeyBeforeComparison: false
        });
    }
}
