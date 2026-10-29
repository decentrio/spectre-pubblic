// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";

/// @dev Exposes proof validation/root helpers for focused inner-spec regression tests.
contract MembershipInnerSpecHarness is Membership {
    function exposedCheckExistenceProof(
        IMembershipMsgs.ExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec
    )
        external
        view
    {
        checkExistenceProof(proof, spec);
    }

    function exposedCalculateExistenceRoot(IMembershipMsgs.ExistenceProof memory proof)
        external
        view
        returns (bytes32)
    {
        return calculateExistenceRoot(proof);
    }
}

contract MembershipInnerSpecTest is Test {
    MembershipInnerSpecHarness internal m;

    function setUp() public {
        m = new MembershipInnerSpecHarness();
    }

    function _singleInnerPath(
        IMembershipMsgs.HashOp hashOp,
        bytes memory prefix,
        bytes memory suffix
    )
        internal
        pure
        returns (IMembershipMsgs.InnerOp[] memory)
    {
        IMembershipMsgs.InnerOp[] memory path = new IMembershipMsgs.InnerOp[](1);
        path[0] = IMembershipMsgs.InnerOp({ hashOp: hashOp, prefix: prefix, suffix: suffix });
        return path;
    }

    function _iavlProof(
        bytes memory key,
        bytes memory value
    )
        internal
        pure
        returns (IMembershipMsgs.ExistenceProof memory)
    {
        return IMembershipMsgs.ExistenceProof({
            key: key,
            value: value,
            leaf: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.SHA256,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.SHA256,
                prefix: hex"000000"
            }),
            path: new IMembershipMsgs.InnerOp[](0)
        });
    }

    function _tendermintProof(
        bytes memory key,
        bytes memory value,
        IMembershipMsgs.InnerOp[] memory path
    )
        internal
        pure
        returns (IMembershipMsgs.ExistenceProof memory)
    {
        return IMembershipMsgs.ExistenceProof({
            key: key,
            value: value,
            leaf: IMembershipMsgs.LeafOp({
                hashOp: IMembershipMsgs.HashOp.SHA256,
                prehashKey: IMembershipMsgs.HashOp.NO_HASH,
                prehashValue: IMembershipMsgs.HashOp.SHA256,
                prefix: hex"00"
            }),
            path: path
        });
    }

    function _kvPairs(bytes memory ibcKey, bytes memory value) internal pure returns (IMembershipMsgs.KVPair[] memory) {
        bytes[] memory path = new bytes[](2);
        path[0] = bytes("ibc");
        path[1] = ibcKey;

        IMembershipMsgs.KVPair[] memory kvs = new IMembershipMsgs.KVPair[](1);
        kvs[0] = IMembershipMsgs.KVPair({ path: path, value: value });
        return kvs;
    }

    function _merkleProofs(
        IMembershipMsgs.ExistenceProof memory iavlProof,
        IMembershipMsgs.ExistenceProof memory tendermintProof
    )
        internal
        pure
        returns (IMembershipMsgs.MerkleProof[] memory)
    {
        IMembershipMsgs.CommitmentProof[] memory proofs = new IMembershipMsgs.CommitmentProof[](2);
        proofs[0].proofType = IMembershipMsgs.ProofType.EXIST;
        proofs[0].existenceProof = iavlProof;
        proofs[1].proofType = IMembershipMsgs.ProofType.EXIST;
        proofs[1].existenceProof = tendermintProof;

        IMembershipMsgs.MerkleProof[] memory merkleProofs = new IMembershipMsgs.MerkleProof[](1);
        merkleProofs[0] = IMembershipMsgs.MerkleProof({ proofs: proofs });
        return merkleProofs;
    }

    function test_checkExistenceProof_rejectsTendermintNoHashInnerOp() public {
        bytes32 appHash = bytes32(uint256(0xABCD));
        IMembershipMsgs.ProofSpec memory spec = m.tendermintSpec();
        IMembershipMsgs.ExistenceProof memory proof = _tendermintProof(
            bytes("ibc"),
            abi.encodePacked(bytes32(uint256(0xCAFE))),
            _singleInnerPath(IMembershipMsgs.HashOp.NO_HASH, abi.encodePacked(appHash), "")
        );

        vm.expectRevert(Membership.UnexpectedInnerHashOp.selector);
        m.exposedCheckExistenceProof(proof, spec);
    }

    function test_membership_rejectsForgedTendermintNoHashRoot() public {
        bytes memory ibcKey = bytes("commitments/forged");
        bytes memory commitmentValue = abi.encodePacked(bytes32(uint256(0xCAFE)));
        bytes32 appHash = bytes32(uint256(0xABCD));

        IMembershipMsgs.ExistenceProof memory iavlProof = _iavlProof(ibcKey, commitmentValue);
        bytes32 fakeIbcRoot = m.exposedCalculateExistenceRoot(iavlProof);
        IMembershipMsgs.ExistenceProof memory tendermintProof = _tendermintProof(
            bytes("ibc"),
            abi.encodePacked(fakeIbcRoot),
            _singleInnerPath(IMembershipMsgs.HashOp.NO_HASH, abi.encodePacked(appHash), "")
        );

        vm.expectRevert(Membership.UnexpectedInnerHashOp.selector);
        m.verifyMembership(appHash, _kvPairs(ibcKey, commitmentValue), _merkleProofs(iavlProof, tendermintProof));
    }

    function test_membership_acceptsValidTendermintSha256InnerOp() public {
        bytes memory ibcKey = bytes("commitments/real");
        bytes memory commitmentValue = abi.encodePacked(bytes32(uint256(0xBEEF)));

        IMembershipMsgs.ExistenceProof memory iavlProof = _iavlProof(ibcKey, commitmentValue);
        bytes32 fakeIbcRoot = m.exposedCalculateExistenceRoot(iavlProof);
        IMembershipMsgs.ExistenceProof memory tendermintProof = _tendermintProof(
            bytes("ibc"),
            abi.encodePacked(fakeIbcRoot),
            _singleInnerPath(IMembershipMsgs.HashOp.SHA256, hex"01", abi.encodePacked(bytes32(uint256(0x1234))))
        );
        bytes32 appHash = m.exposedCalculateExistenceRoot(tendermintProof);

        m.verifyMembership(appHash, _kvPairs(ibcKey, commitmentValue), _merkleProofs(iavlProof, tendermintProof));
    }
}
