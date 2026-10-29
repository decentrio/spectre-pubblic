// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";
import { IMembership } from "contracts/light-clients/spectre/interfaces/IMembership.sol";

import "@openzeppelin-contracts/utils/math/Math.sol";
import "@openzeppelin-contracts/utils/Bytes.sol";

/**
 * @title MerkleTreeMembership
 * @dev Contract to verify membership of key-value pairs in a Merkle tree
 * Converted from Rust zkVM code for Cosmos SDK proof verification
 */
contract Membership is IMembership {
    using Math for uint256;
    using Bytes for *;

    // Custom errors
    error InvalidLength();
    error EmptyRequest();
    error VerificationMembershipFailed();
    error VerificationNonMembershipFailed();
    error MissingMerkleProof();
    error InvalidValueLength();
    error MissingMerkleRoot();
    error MismatchedNumberOfProofs(uint256 expected, uint256 actual);
    error InvalidMerkleProof();
    error InvalidExistenceProof();
    error FailedToVerifyMembership();
    error InputDataMissing();
    error ProvidedKeyValueMismatch();
    error RootMismatch();
    error MissingLeafSpec();
    error InvalidVarint();
    error InvalidOffset();
    error BranchNotFound(uint256 branch);
    /// @notice Thrown when an existence proof's inner-op path is longer than MAX_PROOF_DEPTH (issue #110).
    error ProofPathTooLong(uint256 length, uint256 maxDepth);
    /// @notice Thrown when a non-existence proof's left and right neighbours hash to
    /// different subtree roots, i.e. they are not in the same tree (issue #112).
    error NonExistenceRootMismatch(bytes32 leftRoot, bytes32 rightRoot);

    // checkExistenceProof errors
    error InvalidInnerChildSize();
    error BadLeafPrefix();
    error UnexpectedLeafHashOp();
    error UnexpectedLeafPrehashKeyOp();
    error UnexpectedLeafPrehashValueOp();
    error IncorrectLeafPrefix();
    error TooFewInnerOps();
    error TooManyInnerOps();
    error BadInnerOpPrefix();
    error BadInnerHashOp();
    error InnerSpecRequired();
    error UnexpectedInnerHashOp();
    error InnerNodeWithLeafPrefix();
    error InnerPrefixTooShort();
    error InnerPrefixTooLong();
    error InnerChildSizeZero();
    error InnerSuffixMalformed();

    // verifyNonExistenceProof errors
    error LeftKeyNotBeforeKey();
    error RightKeyNotAfterKey();
    error InnerSpecMissing();
    error NeitherNeighborDefined();
    error NotLeftNeighbor();
    error EmptyNeighborPath();
    error InvalidNonExistenceProofState();

    // misc errors
    error NoPaddingBranchFound();
    error RightPaddingMismatch();
    error LeftPaddingMismatch();
    error MissingChildHash();
    error UnsupportedHashOp();
    error InvalidIavlPrefixHeight();
    error InvalidIavlPrefixSize();
    error InvalidIavlPrefixVersion();
    error InvalidSliceRange();
    error SliceRangeExceedsLength();

    /// @notice Hard upper bound on the number of inner ops (tree depth) in any
    /// existence proof, enforced for every spec regardless of its min/max depth.
    /// Both shipped specs set min/max depth = 0, so the spec-defined bounds never
    /// run; a legitimate ICS-23 / IAVL path equals the tree depth and stays well
    /// under this, while an over-long path only burns gas before the root check
    /// rejects it. Bounding it up front prevents gas griefing (issue #110).
    uint256 internal constant MAX_PROOF_DEPTH = 128;

    /**
     * @dev Verify membership of multiple key-value pairs in the Merkle tree
     * @param appHash The root hash of the Merkle tree (32 bytes)
     * @param kvPairs Array of key-value pairs to verify
     * @param merkleProofs Array of corresponding Merkle proofs
     */
    function verifyMembership(
        bytes32 appHash,
        IMembershipMsgs.KVPair[] calldata kvPairs,
        IMembershipMsgs.MerkleProof[] calldata merkleProofs
    )
        public
        view
    {
        if (kvPairs.length == 0) {
            revert EmptyRequest();
        }

        if (kvPairs.length != merkleProofs.length) {
            revert InvalidLength();
        }

        bytes32 commitmentRoot = appHash;
        IMembershipMsgs.ProofSpec[] memory proofSpecs = new IMembershipMsgs.ProofSpec[](2);
        proofSpecs[0] = iavlSpec();
        proofSpecs[1] = tendermintSpec();

        for (uint256 i = 0; i < kvPairs.length; i++) {
            IMembershipMsgs.KVPair memory kvPair = kvPairs[i];
            IMembershipMsgs.MerkleProof memory merkleProof = merkleProofs[i];

            // Check if this is a non-membership proof (empty value)
            if (kvPair.value.length == 0) {
                // Verify non-membership
                if (!verifyNonMembership(proofSpecs, commitmentRoot, kvPair.path, merkleProof)) {
                    revert VerificationNonMembershipFailed();
                }
            } else {
                // Verify membership
                verifyMembership(proofSpecs, commitmentRoot, kvPair.path, kvPair.value, 0, merkleProof);
            }
        }
    }

    function verifyMembership(
        IMembershipMsgs.ProofSpec[] memory proofSpecs,
        bytes32 root,
        bytes[] memory path,
        bytes memory value,
        uint256 startIndex,
        IMembershipMsgs.MerkleProof memory proof
    )
        internal
        view
    {
        if (proof.proofs.length == 0) {
            revert MissingMerkleProof();
        }

        if (root == bytes32(0)) {
            revert MissingMerkleRoot();
        }

        // Intentional invariant (issue #111): every value proven on this path is a
        // 32-byte commitment. In IBC v2 the committed value at a packet/ack path is
        // a SHA-256 hash (32 bytes), and the intermediate subroots chained between
        // proof levels are tree roots (also 32 bytes), so the whole chain below is
        // built on `bytes32`. This is NOT a generic ICS-23 verifier: if a future
        // commitment format stores raw, variable-length values, generalize the leaf
        // level to hash the full value (keeping this 32-byte fast path) rather than
        // relaxing this check blindly.
        if (value.length != 32) {
            revert InvalidValueLength();
        }

        uint256 proofLength = proof.proofs.length;
        if (proofSpecs.length != proofLength) {
            revert MismatchedNumberOfProofs(proofSpecs.length, proofLength);
        }

        if (path.length != proofLength) {
            revert MismatchedNumberOfProofs(path.length, proofLength);
        }

        // Process proofs from startIndex onwards
        // Keys are represented from root-to-leaf, so we iterate in reverse
        bytes32 subroot = bytesToBytes32(value);
        bytes32 valueUpdate = subroot;

        uint256 pathLength = path.length;
        for (uint256 i = startIndex; i < proofLength; i++) {
            bytes memory keyPath = path[pathLength - i - 1];

            IMembershipMsgs.CommitmentProof memory commitmentProof = proof.proofs[i];
            if (commitmentProof.proofType != IMembershipMsgs.ProofType.EXIST) {
                revert InvalidMerkleProof();
            }

            subroot = _verifyExistenceProofBytes32(commitmentProof.existenceProof, proofSpecs[i], keyPath, valueUpdate);
            valueUpdate = subroot;
        }

        if (root != subroot) {
            revert FailedToVerifyMembership();
        }
    }

    /**
     * @dev Verify non-membership of a key in the Merkle tree
     * @param proofSpecs Array of proof specifications
     * @param root The Merkle root
     * @param path The key to verify non-existence
     * @param proof The Merkle proof for non-membership
     * @return True if the non-membership proof is valid
     */
    function verifyNonMembership(
        IMembershipMsgs.ProofSpec[] memory proofSpecs,
        bytes32 root,
        bytes[] memory path,
        IMembershipMsgs.MerkleProof memory proof
    )
        internal
        view
        returns (bool)
    {
        if (proof.proofs.length == 0) {
            revert MissingMerkleProof();
        }

        if (root == bytes32(0)) {
            revert MissingMerkleRoot();
        }

        uint256 proofLength = proof.proofs.length;
        if (proofSpecs.length != proofLength) {
            revert MismatchedNumberOfProofs(proofSpecs.length, proofLength);
        }

        if (path.length != proofLength) {
            revert MismatchedNumberOfProofs(path.length, proofLength);
        }

        // verify the absence of key in lowest subtree
        IMembershipMsgs.CommitmentProof memory firstProof = proof.proofs[0];
        IMembershipMsgs.ProofSpec memory firstSpec = proofSpecs[0];

        // keys are represented from root-to-leaf
        bytes memory key = path[proofLength - 1];

        if (firstProof.proofType != IMembershipMsgs.ProofType.NON_EXIST) {
            revert InvalidMerkleProof();
        }

        IMembershipMsgs.NonExistenceProof memory nonExistenceProof = firstProof.nonExistenceProof;
        bytes32 subroot = calculateNonExistenceRoot(nonExistenceProof);

        if (!verifyNonExistenceProof(nonExistenceProof, firstSpec, subroot, key)) {
            revert FailedToVerifyMembership();
        }
        // verify membership proofs starting from index 1 with value = subroot
        verifyMembership(proofSpecs, root, path, abi.encodePacked(subroot), 1, proof);

        return true;
    }

    function calculateExistenceRoot(IMembershipMsgs.ExistenceProof memory proof) internal view returns (bytes32) {
        if (proof.key.length == 0 || proof.value.length == 0) {
            revert InvalidExistenceProof();
        }

        // Bound the inner-op loop at its source (issue #110/#134). This is the
        // function that actually iterates proof.path, and it is reachable from the
        // non-membership path (calculateNonExistenceRoot → here) BEFORE
        // checkExistenceProof runs, so capping only in checkExistenceProof would
        // leave nonExistenceProof.left/right.path unbounded.
        if (proof.path.length > MAX_PROOF_DEPTH) {
            revert ProofPathTooLong(proof.path.length, MAX_PROOF_DEPTH);
        }

        IMembershipMsgs.LeafOp memory leafOp = proof.leaf;
        bytes32 current = applyLeaf(leafOp, proof.key, proof.value);
        for (uint256 i = 0; i < proof.path.length; i++) {
            current = applyInner(proof.path[i], current);
        }

        return current;
    }

    function calculateNonExistenceRoot(IMembershipMsgs.NonExistenceProof memory proof) internal view returns (bytes32) {
        if (proof.hasLeft && proof.hasRight) {
            // Both neighbours must live in the SAME subtree. Assert their existence
            // roots match explicitly here (issue #112): the returned root is reused
            // as the *expected* root for both neighbours in verifyNonExistenceProof,
            // so checking the side it was derived from is otherwise self-referential.
            // Make the cross-check explicit instead of relying on that implicit
            // structure + the outer membership binding.
            bytes32 leftRoot = calculateExistenceRoot(proof.left);
            bytes32 rightRoot = calculateExistenceRoot(proof.right);
            if (leftRoot != rightRoot) {
                revert NonExistenceRootMismatch(leftRoot, rightRoot);
            }
            return leftRoot;
        } else if (proof.hasLeft) {
            return calculateExistenceRoot(proof.left);
        } else if (proof.hasRight) {
            return calculateExistenceRoot(proof.right);
        } else {
            revert InvalidMerkleProof();
        }
    }

    function checkExistenceProof(
        IMembershipMsgs.ExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec
    )
        internal
        view
    {
        if (!spec.hasLeafSpec) {
            revert MissingLeafSpec();
        }

        if (spec.hasInnerSpec) {
            if (spec.innerSpec.childSize < 32) {
                revert InvalidInnerChildSize();
            }
        }

        bytes memory leafPrefix = proof.leaf.prefix;
        // ensure leaf prefix matches the spec
        if (spec.specType == IMembershipMsgs.SpecType.IAVL) {
            uint256 remainingLength = ensureIavlPrefix(leafPrefix, 0);
            if (remainingLength != 0) {
                revert BadLeafPrefix();
            }
        }

        //  ensure leaf hash matches the spec
        IMembershipMsgs.LeafOp memory leaf = proof.leaf;
        if (spec.leafOp.hashOp != leaf.hashOp) {
            revert UnexpectedLeafHashOp();
        }
        if (spec.leafOp.prehashKey != leaf.prehashKey) {
            revert UnexpectedLeafPrehashKeyOp();
        }
        if (spec.leafOp.prehashValue != leaf.prehashValue) {
            revert UnexpectedLeafPrehashValueOp();
        }
        bytes memory leafSpecPrefix = spec.leafOp.prefix;
        if (
            leafSpecPrefix.length > leafPrefix.length
                || !(keccak256(leafSpecPrefix) == keccak256(getSlice(leafPrefix, 0, leafSpecPrefix.length)))
        ) {
            revert IncorrectLeafPrefix();
        }

        // Hard cap on proof depth for every spec (issue #110). The spec-defined
        // bounds below only run when the spec sets them (both shipped specs leave
        // min/max depth = 0), so without this an attacker-supplied path could be
        // arbitrarily long and burn gas before the root check rejects it.
        if (proof.path.length > MAX_PROOF_DEPTH) {
            revert ProofPathTooLong(proof.path.length, MAX_PROOF_DEPTH);
        }

        // ensure min/max depths (when the spec sets them)
        if (spec.minDepth != 0) {
            if (proof.path.length < uint256(spec.minDepth)) {
                revert TooFewInnerOps();
            }
            if (proof.path.length > uint256(spec.maxDepth)) {
                revert TooManyInnerOps();
            }
        }

        uint256 stepLength = proof.path.length;
        for (uint256 i = 0; i < stepLength; i++) {
            IMembershipMsgs.InnerOp memory innerOp = proof.path[i];

            if (spec.specType == IMembershipMsgs.SpecType.IAVL) {
                uint256 remainingLength = ensureIavlPrefix(innerOp.prefix, 0);
                if (remainingLength != 0) {
                    // 1 byte due to containing length prefix for left hash.
                    // 33 bytes due to IAVL length prefix + left hash + next IAVL legnth prefix
                    if (remainingLength != 1 && remainingLength != 34) {
                        revert BadInnerOpPrefix();
                    }
                    if (innerOp.hashOp != IMembershipMsgs.HashOp.SHA256) {
                        revert BadInnerHashOp();
                    }
                }
            }

            if (!spec.hasInnerSpec) {
                revert InnerSpecRequired();
            }
            if (spec.innerSpec.hashOp != innerOp.hashOp) {
                revert UnexpectedInnerHashOp();
            }

            if (
                leafSpecPrefix.length <= innerOp.prefix.length
                    && keccak256(leafSpecPrefix) == keccak256(getSlice(innerOp.prefix, 0, leafSpecPrefix.length))
            ) {
                revert InnerNodeWithLeafPrefix();
            }

            if (innerOp.prefix.length < spec.innerSpec.minPrefixLength) {
                revert InnerPrefixTooShort();
            }

            uint32 maxLeftChild = uint32(spec.innerSpec.childOrder.length - 1) * (spec.innerSpec.childSize);
            if (innerOp.prefix.length > maxLeftChild + spec.innerSpec.maxPrefixLength) {
                revert InnerPrefixTooLong();
            }

            if (spec.innerSpec.childSize == 0) {
                revert InnerChildSizeZero();
            }

            if (innerOp.suffix.length % spec.innerSpec.childSize != 0) {
                revert InnerSuffixMalformed();
            }
        }
    }

    function verifyExistenceProof(
        IMembershipMsgs.ExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec,
        bytes32 subroot,
        bytes memory key,
        bytes memory value
    )
        internal
        view
        returns (bool)
    {
        checkExistenceProof(proof, spec);
        if (keccak256(proof.key) != keccak256(key) || keccak256(proof.value) != keccak256(value)) {
            revert ProvidedKeyValueMismatch();
        }

        bytes32 calculateRoot = calculateExistenceRoot(proof);
        if (calculateRoot != subroot) {
            revert RootMismatch();
        }
        return true;
    }

    function _verifyExistenceProofBytes32(
        IMembershipMsgs.ExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec,
        bytes memory key,
        bytes32 value
    )
        internal
        view
        returns (bytes32)
    {
        checkExistenceProof(proof, spec);
        if (proof.value.length != 32) {
            revert InvalidValueLength();
        }
        if (keccak256(proof.key) != keccak256(key) || bytesToBytes32(proof.value) != value) {
            revert ProvidedKeyValueMismatch();
        }
        return calculateExistenceRoot(proof);
    }

    function verifyNonExistenceProof(
        IMembershipMsgs.NonExistenceProof memory proof,
        IMembershipMsgs.ProofSpec memory spec,
        bytes32 root,
        bytes memory key
    )
        internal
        view
        returns (bool)
    {
        bool preHash = spec.prehashKeyBeforeComparison;
        IMembershipMsgs.HashOp prehashOp = spec.leafOp.prehashKey;
        if (proof.hasLeft) {
            verifyExistenceProof(proof.left, spec, root, proof.left.key, proof.left.value);
            if (
                compareBytes(
                        keyForComparison(key, preHash, prehashOp), keyForComparison(proof.left.key, preHash, prehashOp)
                    ) != 1
            ) {
                revert LeftKeyNotBeforeKey();
            }
        }

        if (proof.hasRight) {
            verifyExistenceProof(proof.right, spec, root, proof.right.key, proof.right.value);
            if (
                compareBytes(
                        keyForComparison(key, preHash, prehashOp), keyForComparison(proof.right.key, preHash, prehashOp)
                    ) != -1
            ) {
                revert RightKeyNotAfterKey();
            }
        }

        if (!spec.hasInnerSpec) {
            revert InnerSpecMissing();
        }
        IMembershipMsgs.InnerSpec memory innerSpec = spec.innerSpec;

        if (!proof.hasLeft && !proof.hasRight) {
            revert NeitherNeighborDefined();
        } else if (!proof.hasLeft && proof.hasRight) {
            ensureLeftMost(innerSpec, proof.right.path, proof.right.path.length);
        } else if (proof.hasLeft && !proof.hasRight) {
            ensureRightMost(innerSpec, proof.left.path, proof.left.path.length);
        } else if (proof.hasLeft && proof.hasRight) {
            if (proof.left.path.length == 0 || proof.right.path.length == 0) {
                revert EmptyNeighborPath();
            }
            uint256 leftIndex = proof.left.path.length - 1;
            uint256 rightIndex = proof.right.path.length - 1;

            IMembershipMsgs.InnerOp memory topLeft = proof.left.path[leftIndex];
            IMembershipMsgs.InnerOp memory topRight = proof.right.path[rightIndex];
            while (
                leftIndex > 0 && rightIndex > 0 && keccak256(topLeft.prefix) == keccak256(topRight.prefix)
                    && keccak256(topLeft.suffix) == keccak256(topRight.suffix)
            ) {
                leftIndex--;
                rightIndex--;
                topLeft = proof.left.path[leftIndex];
                topRight = proof.right.path[rightIndex];
            }

            uint256 leftPaddingIdx = orderFromPadding(innerSpec, topLeft);
            uint256 rightPaddingIdx = orderFromPadding(innerSpec, topRight);

            if (!(leftPaddingIdx + 1 == rightPaddingIdx)) {
                revert NotLeftNeighbor();
            }

            // left neighbor (max of left subtree) must be rightmost below divergence
            ensureRightMost(innerSpec, proof.left.path, leftIndex);
            // right neighbor (min of right subtree) must be leftmost below divergence
            ensureLeftMost(innerSpec, proof.right.path, rightIndex);
        } else {
            revert InvalidNonExistenceProofState();
        }
        return true;
    }

    function applyLeaf(
        IMembershipMsgs.LeafOp memory leafOp,
        bytes memory key,
        bytes memory value
    )
        internal
        pure
        returns (bytes32)
    {
        bytes memory hashedData = leafOp.prefix;

        bytes memory prekey = prepareLeafData(leafOp.prehashKey, key);
        bytes memory preval = prepareLeafData(leafOp.prehashValue, value);

        uint256 prefixLen = hashedData.length;
        uint256 prekeyLen = prekey.length;
        uint256 prevalLen = preval.length;
        uint256 totalLen = prefixLen + prekeyLen + prevalLen;

        bytes memory result = new bytes(totalLen);
        // Bounded mcopy (exact length) keeps every write inside `result`'s allocation,
        // so the memory-safe annotation holds. A word-copy loop would overshoot by up
        // to 31 bytes on non-32-aligned segments (issue #114).
        assembly ("memory-safe") {
            let dest := add(result, 0x20)
            mcopy(dest, add(hashedData, 0x20), prefixLen)
            dest := add(dest, prefixLen)
            mcopy(dest, add(prekey, 0x20), prekeyLen)
            dest := add(dest, prekeyLen)
            mcopy(dest, add(preval, 0x20), prevalLen)
        }

        return hashData(result, leafOp.hashOp);
    }

    function applyInner(IMembershipMsgs.InnerOp memory inner, bytes32 child) internal view returns (bytes32) {
        if (child == bytes32(0)) {
            revert MissingChildHash();
        }

        bytes32 result;
        bytes memory prefix = inner.prefix;
        bytes memory suffix = inner.suffix;
        uint256 prefixLen = prefix.length;
        uint256 suffixLen = suffix.length;
        uint256 totalLen = prefixLen + 32 + suffixLen;

        if (inner.hashOp == IMembershipMsgs.HashOp.SHA256) {
            assembly ("memory-safe") {
                // Scratch starts at the free-memory pointer (not advanced — consumed in place).
                // Bounded mcopy avoids the word-copy overshoot of issue #114.
                let freeMem := mload(0x40)
                mcopy(freeMem, add(prefix, 0x20), prefixLen)
                mstore(add(freeMem, prefixLen), child)
                mcopy(add(add(freeMem, prefixLen), 32), add(suffix, 0x20), suffixLen)

                // Call sha256 precompile (0x02)
                let success := staticcall(gas(), 0x02, freeMem, totalLen, freeMem, 32)
                if iszero(success) {
                    revert(0, 0)
                }
                result := mload(freeMem)
            }
            return result;
        } else if (inner.hashOp == IMembershipMsgs.HashOp.KECCAK256) {
            assembly ("memory-safe") {
                // Scratch starts at the free-memory pointer (not advanced — consumed in place).
                // Bounded mcopy avoids the word-copy overshoot of issue #114.
                let freeMem := mload(0x40)
                mcopy(freeMem, add(prefix, 0x20), prefixLen)
                mstore(add(freeMem, prefixLen), child)
                mcopy(add(add(freeMem, prefixLen), 32), add(suffix, 0x20), suffixLen)

                result := keccak256(freeMem, totalLen)
            }
            return result;
        } else {
            bytes memory image = abi.encodePacked(inner.prefix, child, inner.suffix);
            return hashData(image, inner.hashOp);
        }
    }

    function prepareLeafData(IMembershipMsgs.HashOp prehashOp, bytes memory data) internal pure returns (bytes memory) {
        if (data.length == 0) {
            revert InputDataMissing();
        }

        if (prehashOp == IMembershipMsgs.HashOp.NO_HASH) {
            bytes memory encodedLen = encodeVarint(data.length);
            uint256 len1 = encodedLen.length;
            uint256 len2 = data.length;
            bytes memory res1 = new bytes(len1 + len2);
            // Bounded mcopy keeps writes inside res1's allocation (issue #114).
            assembly ("memory-safe") {
                let dest := add(res1, 0x20)
                mcopy(dest, add(encodedLen, 0x20), len1)
                mcopy(add(dest, len1), add(data, 0x20), len2)
            }
            return res1;
        }

        bytes32 hashedData = hashData(data, prehashOp);
        bytes memory encodedLength = encodeVarint(uint256(32));
        uint256 lenLength = encodedLength.length;
        bytes memory res2 = new bytes(lenLength + 32);
        // Bounded mcopy keeps writes inside res2's allocation (issue #114).
        assembly ("memory-safe") {
            let dest := add(res2, 0x20)
            mcopy(dest, add(encodedLength, 0x20), lenLength)
            mstore(add(dest, lenLength), hashedData)
        }
        return res2;
    }

    // true if this is the right-most path in the tree, excluding placeholder (empty child) nodes
    function ensureRightMost(
        IMembershipMsgs.InnerSpec memory innerSpec,
        IMembershipMsgs.InnerOp[] memory path,
        uint256 length
    )
        internal
        view
    {
        IMembershipMsgs.Padding memory padding = getPadding(innerSpec, innerSpec.childOrder.length - 1);

        for (uint256 i = 0; i < length; i++) {
            IMembershipMsgs.InnerOp memory innerOp = path[i];
            bool rightHasPadding = hasPadding(innerOp, padding);
            uint256 rightBranches = innerSpec.childOrder.length - 1 - orderFromPadding(innerSpec, innerOp);

            bool isEmpty = true;
            uint256 childSize = uint256(innerSpec.childSize);
            if (rightBranches == 0 || innerOp.suffix.length != childSize) {
                isEmpty = false;
            } else {
                // compare prefix with the expected number of empty branches
                for (uint256 j = 0; j < rightBranches; j++) {
                    bool found = false;
                    uint256 idx;
                    for (uint256 k = 0; k < innerSpec.childOrder.length; k++) {
                        if (innerSpec.childOrder[k] == j) {
                            found = true;
                            idx = k;
                            break;
                        }
                    }

                    if (!found) {
                        isEmpty = false;
                        break;
                    }

                    uint256 from = idx * childSize;
                    if (keccak256(innerSpec.emptyChild) != keccak256(getSlice(innerOp.suffix, from, from + childSize)))
                    {
                        isEmpty = false;
                        break;
                    }
                }
            }
            if (!rightHasPadding && !isEmpty) {
                revert RightPaddingMismatch();
            }
        }
    }

    function ensureLeftMost(
        IMembershipMsgs.InnerSpec memory innerSpec,
        IMembershipMsgs.InnerOp[] memory path,
        uint256 length
    )
        internal
        view
    {
        // fails unless this is the left-most path in the tree, excluding placeholder (empty child) nodes
        IMembershipMsgs.Padding memory padding = getPadding(innerSpec, 0);
        for (uint256 i = 0; i < length; i++) {
            IMembershipMsgs.InnerOp memory innerOp = path[i];
            bool leftHasPadding = hasPadding(innerOp, padding);
            uint256 leftBranches = orderFromPadding(innerSpec, innerOp);

            bool isEmpty = true;
            if (leftBranches == 0) {
                isEmpty = false;
            } else {
                // compare prefix with the expected number of empty branches
                uint256 childSize = uint256(innerSpec.childSize);
                (bool subSuccess, uint256 actualPrefix) = Math.trySub(innerOp.prefix.length, childSize * leftBranches);
                if (!subSuccess) {
                    isEmpty = false;
                } else {
                    for (uint256 j = 0; j < leftBranches; j++) {
                        bool found = false;
                        uint256 idx;
                        for (uint256 k = 0; k < innerSpec.childOrder.length; k++) {
                            if (innerSpec.childOrder[k] == j) {
                                found = true;
                                idx = k;
                                break;
                            }
                        }

                        if (!found) {
                            isEmpty = false;
                            break;
                        }

                        uint256 from = actualPrefix + idx * childSize;
                        if (
                            keccak256(innerSpec.emptyChild)
                                != keccak256(getSlice(innerOp.prefix, from, from + childSize))
                        ) {
                            isEmpty = false;
                            break;
                        }
                    }
                }
            }
            if (!leftHasPadding && !isEmpty) {
                revert LeftPaddingMismatch();
            }
        }
    }

    function orderFromPadding(
        IMembershipMsgs.InnerSpec memory innerSpec,
        IMembershipMsgs.InnerOp memory innerOp
    )
        internal
        pure
        returns (uint256)
    {
        uint256 childOrderLength = innerSpec.childOrder.length;
        for (uint256 branch = 0; branch < childOrderLength; branch++) {
            IMembershipMsgs.Padding memory padding = getPadding(innerSpec, uint32(branch));
            if (hasPadding(innerOp, padding)) {
                return branch;
            }
        }
        revert NoPaddingBranchFound();
    }

    function getPadding(
        IMembershipMsgs.InnerSpec memory innerSpec,
        uint256 branch
    )
        internal
        pure
        returns (IMembershipMsgs.Padding memory)
    {
        uint256 foundIdx = 0;
        bool found = false;
        for (uint256 i = 0; i < innerSpec.childOrder.length; i++) {
            if (uint32(innerSpec.childOrder[i]) == branch) {
                foundIdx = i;
                found = true;
                break;
            }
        }

        // If branch not found, revert with error
        if (!found) {
            revert BranchNotFound(branch);
        }

        uint32 idx = uint32(foundIdx);
        uint32 prefix = idx * innerSpec.childSize;
        uint256 suffix = uint256(innerSpec.childSize) * (innerSpec.childOrder.length - 1 - idx);
        return IMembershipMsgs.Padding({
            minPrefix: prefix + innerSpec.minPrefixLength, maxPrefix: prefix + innerSpec.maxPrefixLength, suffix: suffix
        });
    }

    function hasPadding(
        IMembershipMsgs.InnerOp memory inner,
        IMembershipMsgs.Padding memory padding
    )
        internal
        pure
        returns (bool)
    {
        return (inner.prefix.length >= padding.minPrefix && inner.prefix.length <= padding.maxPrefix
                && inner.suffix.length == padding.suffix);
    }

    /// @notice Lexicographic byte comparison matching Go's `bytes.Compare` (the ordering
    /// Cosmos/IAVL uses), which non-membership proofs depend on: bytes are compared
    /// UNSIGNED up to the shorter length, then the shorter slice sorts first (a prefix
    /// sorts before its extension). Returns -1 if a < b, 1 if a > b, 0 if equal.
    /// See MembershipCompareBytesTest for boundary + fuzz coverage (issue #113).
    function compareBytes(bytes memory a, bytes memory b) internal pure returns (int8) {
        if (a.length != b.length) {
            // For different lengths, we still need to compare byte by byte
            // up to the shorter length, then compare lengths
            uint256 minLength = a.length < b.length ? a.length : b.length;

            for (uint256 i = 0; i < minLength; i++) {
                if (a[i] < b[i]) return -1;
                if (a[i] > b[i]) return 1;
            }

            return a.length < b.length ? int8(-1) : int8(1);
        }

        // compare byte by byte
        for (uint256 i = 0; i < a.length; i++) {
            if (a[i] < b[i]) return -1;
            if (a[i] > b[i]) return 1;
        }

        return 0;
    }

    /**
     * @dev Convert bytes to bytes32
     * @param data The bytes to convert
     * @return result The bytes32 result
     */
    function bytesToBytes32(bytes memory data) internal pure returns (bytes32 result) {
        if (data.length >= 32) {
            assembly ("memory-safe") {
                result := mload(add(data, 32))
            }
        } else {
            // Pad with zeros if data is shorter than 32 bytes
            bytes32 temp;
            assembly ("memory-safe") {
                temp := mload(add(data, 32))
            }
            result = temp >> (8 * (32 - data.length));
        }
    }

    function bytes32ToBytes(bytes32 data) internal pure returns (bytes memory) {
        bytes memory result = new bytes(32);
        assembly ("memory-safe") {
            mstore(add(result, 32), data)
        }
        return result;
    }

    function getSlice(bytes memory array, uint256 from, uint256 to) internal view returns (bytes memory) {
        if (from > to) revert InvalidSliceRange();
        if (to > array.length) revert SliceRangeExceedsLength();

        uint256 length = to - from;
        bytes memory result = new bytes(length);

        assembly ("memory-safe") {
            let src := add(add(array, 0x20), from)
            let dest := add(result, 0x20)

            // Use identity precompile for efficient copying and check success
            let success := staticcall(gas(), 0x04, src, length, dest, length)
            if iszero(success) {
                revert(0, 0)
            }
        }

        return result;
    }

    function hashData(bytes memory data, IMembershipMsgs.HashOp hashOp) internal pure returns (bytes32) {
        if (hashOp == IMembershipMsgs.HashOp.NO_HASH) {
            return bytesToBytes32(data);
        } else if (hashOp == IMembershipMsgs.HashOp.SHA256) {
            return sha256(data);
        } else if (hashOp == IMembershipMsgs.HashOp.KECCAK256) {
            return keccak256(data);
        } else {
            revert UnsupportedHashOp();
        }
    }

    function keyForComparison(
        bytes memory key,
        bool prehash,
        IMembershipMsgs.HashOp prehashOp
    )
        internal
        pure
        returns (bytes memory)
    {
        if (prehash) {
            return bytes32ToBytes(hashData(key, prehashOp));
        } else {
            return key;
        }
    }

    function readVarint(bytes memory data, uint256 offset) internal pure returns (int64, uint256) {
        (uint64 ux, uint256 newOffset) = decodeVarint(data, offset);
        int64 x = int64(ux >> 1);
        if (ux & 1 != 0) {
            x = ~x;
        }
        return (x, newOffset);
    }

    function encodeVarint(uint256 value) internal pure returns (bytes memory) {
        if (value < 128) {
            bytes memory b = new bytes(1);
            b[0] = bytes1(uint8(value));
            return b;
        }
        bytes memory buf = new bytes(10); // max varint bytes
        uint256 i = 0;
        while (value >= 128) {
            buf[i] = bytes1(uint8((value & 0x7F) | 0x80));
            i++;
            value >>= 7;
        }
        buf[i] = bytes1(uint8(value));
        i++;
        assembly ("memory-safe") { mstore(buf, i) } // truncate to actual length
        return buf;
    }

    function decodeVarint(bytes memory data, uint256 offset) internal pure returns (uint64, uint256) {
        if (offset >= data.length) {
            revert InvalidOffset();
        }

        if (data.length - offset == 0) {
            revert InvalidVarint();
        }

        uint8 firstByte = uint8(data[offset]);
        if (firstByte < 0x80) {
            return (uint64(firstByte), offset + 1);
        } else if (data.length - offset > 10 || data[data.length - 1] < 0x80) {
            uint32 part0 = uint32(firstByte) - 0x80;
            uint8 b = uint8(data[offset + 1]);
            part0 += (uint32(b) << 7);
            if (b < 0x80) {
                return (part0, offset + 2);
            }
            part0 -= 0x80 << 7;

            b = uint8(data[offset + 2]);
            part0 += (uint32(b) << 14);
            if (b < 0x80) {
                return (part0, offset + 3);
            }
            part0 -= 0x80 << 14;

            b = uint8(data[offset + 3]);
            part0 += (uint32(b) << 21);
            if (b < 0x80) {
                return (part0, offset + 4);
            }
            part0 -= 0x80 << 21;

            uint64 value = uint64(part0);

            b = uint8(data[offset + 4]);
            uint32 part1 = uint32(b);
            if (b < 0x80) {
                return (value + (uint64(part1) << 28), offset + 5);
            }
            part1 -= 0x80;

            b = uint8(data[offset + 5]);
            part1 += (uint32(b) << 7);
            if (b < 0x80) {
                return (value + (uint64(part1) << 28), offset + 6);
            }
            part1 -= 0x80 << 7;

            b = uint8(data[offset + 6]);
            part1 += (uint32(b) << 14);
            if (b < 0x80) {
                return (value + (uint64(part1) << 28), offset + 7);
            }
            part1 -= 0x80 << 14;

            b = uint8(data[offset + 7]);
            part1 += (uint32(b) << 21);
            if (b < 0x80) {
                return (value + (uint64(part1) << 28), offset + 8);
            }
            part1 -= 0x80 << 21;

            value = value + (uint64(part1) << 28);

            b = uint8(data[offset + 8]);
            uint32 part2 = uint32(b);
            if (b < 0x80) {
                return (value + (uint64(part2) << 56), offset + 9);
            }
            part2 -= 0x80;

            b = uint8(data[offset + 9]);
            part2 += (uint32(b) << 7);

            if (b < 0x02) {
                return (value + (uint64(part2) << 56), offset + 10);
            }
            revert InvalidVarint();
        } else {
            uint64 value = 0;
            uint256 remaining = data.length - offset;
            uint256 maxCount = remaining < 10 ? remaining : 10;
            for (uint256 count = 0; count < maxCount; count++) {
                uint8 b = uint8(data[offset + count]);
                value |= uint64(b & 0x7F) << uint64(count * 7);

                if (b <= 0x7F) {
                    // Check for overflow on the final byte
                    if (count == 9 && b >= 0x02) {
                        revert InvalidVarint();
                    }
                    return (value, offset + count + 1);
                }
            }
            revert InvalidVarint();
        }
    }

    function tendermintSpec() public pure returns (IMembershipMsgs.ProofSpec memory) {
        IMembershipMsgs.LeafOp memory leaf = IMembershipMsgs.LeafOp({
            hashOp: IMembershipMsgs.HashOp.SHA256,
            prehashKey: IMembershipMsgs.HashOp.NO_HASH,
            prehashValue: IMembershipMsgs.HashOp.SHA256,
            prefix: hex"00"
        });

        uint32[] memory childOrder = new uint32[](2);
        childOrder[0] = 0;
        childOrder[1] = 1;

        IMembershipMsgs.InnerSpec memory inner = IMembershipMsgs.InnerSpec({
            childOrder: childOrder,
            minPrefixLength: 1,
            maxPrefixLength: 1,
            childSize: 32,
            emptyChild: "",
            hashOp: IMembershipMsgs.HashOp.SHA256
        });

        return IMembershipMsgs.ProofSpec({
            specType: IMembershipMsgs.SpecType.TENDERMINT,
            hasLeafSpec: true,
            leafOp: leaf,
            hasInnerSpec: true,
            innerSpec: inner,
            minDepth: 0,
            maxDepth: 0,
            prehashKeyBeforeComparison: false
        });
    }

    function iavlSpec() public pure returns (IMembershipMsgs.ProofSpec memory) {
        IMembershipMsgs.LeafOp memory leaf = IMembershipMsgs.LeafOp({
            hashOp: IMembershipMsgs.HashOp.SHA256,
            prehashKey: IMembershipMsgs.HashOp.NO_HASH,
            prehashValue: IMembershipMsgs.HashOp.SHA256,
            prefix: hex"00"
        });

        uint32[] memory childOrder = new uint32[](2);
        childOrder[0] = 0;
        childOrder[1] = 1;

        IMembershipMsgs.InnerSpec memory inner = IMembershipMsgs.InnerSpec({
            childOrder: childOrder,
            minPrefixLength: 4,
            maxPrefixLength: 12,
            childSize: 33,
            emptyChild: "",
            hashOp: IMembershipMsgs.HashOp.SHA256
        });

        // Return proof spec
        return IMembershipMsgs.ProofSpec({
            specType: IMembershipMsgs.SpecType.IAVL,
            hasLeafSpec: true,
            leafOp: leaf,
            hasInnerSpec: true,
            innerSpec: inner,
            minDepth: 0,
            maxDepth: 0,
            prehashKeyBeforeComparison: false
        });
    }

    function ensureIavlPrefix(bytes memory leafPrefix, int64 minHeight) internal pure returns (uint256) {
        uint256 offset = 0;
        (int64 height, uint256 newOffset) = readVarint(leafPrefix, offset);
        if (height < minHeight) {
            revert InvalidIavlPrefixHeight();
        }
        (int64 size, uint256 newOffset2) = readVarint(leafPrefix, newOffset);
        if (size < 0) {
            revert InvalidIavlPrefixSize();
        }
        (int64 version, uint256 newOffset3) = readVarint(leafPrefix, newOffset2);
        if (version < 0) {
            revert InvalidIavlPrefixVersion();
        }

        return leafPrefix.length - newOffset3;
    }
}
