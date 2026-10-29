// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { Encode } from "contracts/light-clients/spectre/libraries/Encode.sol";

library Header {
    bytes32 internal constant EMPTY_LEAF_HASH = 0x6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d;

    function hashValSet(ISpectreTendermintMsgs.ValidatorSet memory valset) internal pure returns (bytes32) {
        uint256 validatorCount = valset.validators.length;
        if (validatorCount == 0) {
            return bytes32(0);
        }

        bytes32[] memory leafHashes = new bytes32[](validatorCount);
        for (uint256 i = 0; i < valset.validators.length; i++) {
            bytes memory validatorBytes = Encode.encodeValidator(
                ISpectreTendermintMsgs.SimpleValidator({
                    pubKey: valset.validators[i].pubKey, votingPower: valset.validators[i].votingPower
                })
            );
            leafHashes[i] = _leafHash(validatorBytes);
        }

        return _merkleHashRange(leafHashes, 0, validatorCount);
    }

    function hashHeader(ISpectreTendermintMsgs.BlockHeader memory header) internal pure returns (bytes32) {
        return hashHeaderWithCachedChainId(header, chainIdLeafHash(header.chainId));
    }

    function hashHeaderWithCachedChainId(
        ISpectreTendermintMsgs.BlockHeader memory header,
        bytes32 cachedChainIdLeafHash
    )
        internal
        pure
        returns (bytes32)
    {
        // CometBFT Header.Hash() ALWAYS hashes 14 fields via merkle.
        // Missing fields are encoded as empty bytes (nil in Go).
        bytes32[] memory leafHashes = new bytes32[](14);

        leafHashes[0] = _leafHash(Encode.encodeVersion(header.version));
        leafHashes[1] = cachedChainIdLeafHash;
        leafHashes[2] = _leafHash(Encode.cdcEncodeInt64(uint256(header.height)));
        leafHashes[3] = _leafHash(Encode.encodeTimestamp(header.time));
        leafHashes[4] = header.hasLastBlockId ? _leafHash(Encode.encodeBlockId(header.lastBlockId)) : _emptyLeafHash();
        leafHashes[5] =
            header.hasLastCommitHash ? _leafHash(Encode.cdcEncodeBytes32(header.lastCommitHash)) : _emptyLeafHash();
        leafHashes[6] = header.hasDataHash ? _leafHash(Encode.cdcEncodeBytes32(header.dataHash)) : _emptyLeafHash();
        leafHashes[7] = _leafHash(Encode.cdcEncodeBytes32(header.validatorsHash));
        leafHashes[8] = _leafHash(Encode.cdcEncodeBytes32(header.nextValidatorsHash));
        leafHashes[9] = _leafHash(Encode.cdcEncodeBytes32(header.consensusHash));
        leafHashes[10] = _leafHash(Encode.cdcEncodeBytes32(header.appHash));
        leafHashes[11] =
            header.hasLastResultsHash ? _leafHash(Encode.cdcEncodeBytes32(header.lastResultsHash)) : _emptyLeafHash();
        leafHashes[12] =
            header.hasEvidenceHash ? _leafHash(Encode.cdcEncodeBytes32(header.evidenceHash)) : _emptyLeafHash();
        leafHashes[13] = _leafHash(Encode.cdcEncodeBytes(header.proposerAddress));

        return _merkleHashRange(leafHashes, 0, leafHashes.length);
    }

    function hashHeaderWithCachedLeaves(
        ISpectreTendermintMsgs.BlockHeader memory header,
        bytes32 cachedChainIdLeafHash,
        bytes32 cachedValidatorsHashLeaf
    )
        internal
        pure
        returns (bytes32)
    {
        // CometBFT Header.Hash() ALWAYS hashes 14 fields via merkle.
        // Missing fields are encoded as empty bytes (nil in Go).
        bytes32[] memory leafHashes = new bytes32[](14);

        leafHashes[0] = _leafHash(Encode.encodeVersion(header.version));
        leafHashes[1] = cachedChainIdLeafHash;
        leafHashes[2] = _leafHash(Encode.cdcEncodeInt64(uint256(header.height)));
        leafHashes[3] = _leafHash(Encode.encodeTimestamp(header.time));
        leafHashes[4] = header.hasLastBlockId ? _leafHash(Encode.encodeBlockId(header.lastBlockId)) : _emptyLeafHash();
        leafHashes[5] =
            header.hasLastCommitHash ? _leafHash(Encode.cdcEncodeBytes32(header.lastCommitHash)) : _emptyLeafHash();
        leafHashes[6] = header.hasDataHash ? _leafHash(Encode.cdcEncodeBytes32(header.dataHash)) : _emptyLeafHash();
        leafHashes[7] = cachedValidatorsHashLeaf;
        leafHashes[8] = header.nextValidatorsHash == header.validatorsHash
            ? cachedValidatorsHashLeaf
            : _leafHash(Encode.cdcEncodeBytes32(header.nextValidatorsHash));
        leafHashes[9] = _leafHash(Encode.cdcEncodeBytes32(header.consensusHash));
        leafHashes[10] = _leafHash(Encode.cdcEncodeBytes32(header.appHash));
        leafHashes[11] =
            header.hasLastResultsHash ? _leafHash(Encode.cdcEncodeBytes32(header.lastResultsHash)) : _emptyLeafHash();
        leafHashes[12] =
            header.hasEvidenceHash ? _leafHash(Encode.cdcEncodeBytes32(header.evidenceHash)) : _emptyLeafHash();
        leafHashes[13] = _leafHash(Encode.cdcEncodeBytes(header.proposerAddress));

        return _merkleHashRange(leafHashes, 0, leafHashes.length);
    }

    function chainIdLeafHash(string memory chainId) internal pure returns (bytes32) {
        return _leafHash(Encode.cdcEncodeString(chainId));
    }

    function bytes32LeafHash(bytes32 value) internal pure returns (bytes32) {
        return _leafHash(Encode.cdcEncodeBytes32(value));
    }

    function simpleValidatorLeafHash(bytes32 pubKey, uint64 votingPower) internal pure returns (bytes32) {
        return _leafHash(
            Encode.encodeValidator(ISpectreTendermintMsgs.SimpleValidator({ pubKey: pubKey, votingPower: votingPower }))
        );
    }

    function innerHash(bytes32 left, bytes32 right) internal pure returns (bytes32) {
        return _innerHash(left, right);
    }

    function _merkleHashRange(bytes32[] memory leafHashes, uint256 from, uint256 to) private pure returns (bytes32) {
        uint256 length = to - from;
        if (length == 1) {
            return leafHashes[from];
        }

        uint256 split = _splitPoint(length);
        bytes32 left = _merkleHashRange(leafHashes, from, from + split);
        bytes32 right = _merkleHashRange(leafHashes, from + split, to);
        return _innerHash(left, right);
    }

    function _splitPoint(uint256 n) private pure returns (uint256 split) {
        split = 1;
        while ((split << 1) < n) {
            split <<= 1;
        }
    }

    function _leafHash(bytes memory leaf) private pure returns (bytes32) {
        bytes memory prefixed = new bytes(leaf.length + 1);
        prefixed[0] = bytes1(0x00);
        _copyBytes(prefixed, 1, leaf);
        return sha256(prefixed);
    }

    function _emptyLeafHash() private pure returns (bytes32) {
        return EMPTY_LEAF_HASH;
    }

    function _innerHash(bytes32 left, bytes32 right) private pure returns (bytes32) {
        bytes memory prefixed = new bytes(65);
        prefixed[0] = bytes1(0x01);
        assembly ("memory-safe") {
            mstore(add(prefixed, 0x21), left)
            mstore(add(prefixed, 0x41), right)
        }
        return sha256(prefixed);
    }

    function _copyBytes(bytes memory out, uint256 dstOffset, bytes memory src) private pure returns (uint256) {
        uint256 len = src.length;
        if (len == 0) {
            return dstOffset;
        }

        assembly ("memory-safe") {
            mcopy(add(add(out, 0x20), dstOffset), add(src, 0x20), len)
        }

        return dstOffset + len;
    }
}
