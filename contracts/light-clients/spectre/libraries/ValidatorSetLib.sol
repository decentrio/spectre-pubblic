// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { ISpectreClientErrors } from "contracts/light-clients/spectre/errors/ISpectreClientErrors.sol";
import { SpectreStore } from "contracts/light-clients/spectre/store/SpectreStore.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { SSTORE2 } from "contracts/light-clients/spectre/libraries/SSTORE2.sol";

/// @title ValidatorSetLib
/// @notice Codec + lookup helpers for the SSTORE2-packed pinned validator cache, plus the
///         per-signer commit-signature checks used by SpectreClient's quorum accounting.
/// @dev The byte layout MUST stay identical to the relayer prover — do not "improve" it.
library ValidatorSetLib {
    uint32 internal constant VALIDATOR_CACHE_MAGIC = 0x56414c34; // "VAL4"

    /// @notice Hard cap on the number of validators this client can pin, and therefore the
    ///         largest counterparty validator-set size this client can ever attest quorum for.
    /// @dev 180 == Cosmos Hub's current validator count (as of this writing) — NOT a coincidence.
    ///      Quorum requires >2/3 of pinned voting power (`SpectreClient._verifyQuorum`); under the
    ///      simplifying assumption of equal voting power per validator that's
    ///      `floor(2*180/3)+1 = 121` unique signers in one Groth16 proof. Production preserves a
    ///      largest bucket of N=128, leaving only ~7 validators of headroom (128 - 121 = 7) before
    ///      quorum becomes unprovable by any bucket. The prover setup tool (`relayer/prover/cmd`)
    ///      builds N=4 only by default; the supported bucket family is in
    ///      `relayer/prover/witness/buckets.go`. Production buckets require separately
    ///      published, deployed, and registered artifact sets.
    ///      That headroom is a BEST case, not a worst case: 121 assumes power is spread evenly, so
    ///      each honest signer contributes the same marginal power. Under a skewed distribution
    ///      where the largest stake-holders happen to be offline (the adversarial/liveness-hostile
    ///      case — not something this client controls), reaching >2/3 power can require signatures
    ///      from far more than 121 of the long-tail small validators, pushing the needed
    ///      unique-signer count toward all 180 and eating the headroom faster than the equal-power
    ///      estimate suggests. So 121/~7-headroom is the design target, not a bound this contract
    ///      enforces or can rely on.
    ///      This is a HARD LIVENESS CEILING, not a soft one: there is no on-chain oracle of the
    ///      counterparty chain's live validator count, so a Cosmos Hub governance proposal that
    ///      raises validator count past what fits under this client's cap can only be caught by
    ///      off-chain monitoring — nothing here can assert it or revert on it. Bumping this constant to
    ///      accommodate a larger validator set requires provisioning a larger Groth16 bucket first
    ///      (circuit regen + redeploy of every `Groth16Verifier_N{N}` + `setBucket`), not just
    ///      editing this number; raising the cap alone does not increase proof capacity.
    /// @dev INVARIANT: this constant must stay < 256. `SpectreClient._verifyQuorum`'s `seenPinned`
    ///      dup-signer bitmask packs one bit per pinned index into a `uint256`; a `pinnedIdx >= 256`
    ///      would make `uint256(1) << pinnedIdx` evaluate to 0 on the EVM (shifts >= 256 don't
    ///      revert, they just zero out) instead of reverting, silently disabling duplicate-signer
    ///      detection for indices at/above 256. `SpectreClient` also carries a defensive
    ///      `require(pinnedIdx < 256, ...)` immediately before that shift for this exact reason —
    ///      keep both in sync if this constant is ever raised.
    uint16 internal constant MAX_VALIDATOR_COUNT = 180;
    uint256 internal constant VALIDATOR_CACHE_HEADER_LEN = 48;
    uint256 internal constant VALIDATOR_CACHE_ENTRY_LEN = 44;

    struct ValidatorCacheHeader {
        uint64 totalVotingPower;
        uint16 entryCount;
        bytes32 validatorsHashLeaf;
        uint16 nodeCount;
    }

    /// @notice Packs a validator set into the SSTORE2 cache byte layout.
    function buildCache(
        bytes32 validatorsHash,
        ISpectreTendermintMsgs.ValidatorSet memory validatorSet
    )
        internal
        pure
        returns (bytes memory data)
    {
        ISpectreTendermintMsgs.ValidatorInfo[] memory vals = validatorSet.validators;
        uint256 validatorCount = vals.length;
        if (validatorCount == 0) {
            revert ISpectreClientErrors.BatchLengthMismatch();
        }
        if (validatorCount > MAX_VALIDATOR_COUNT) {
            revert ISpectreClientErrors.ValidatorCountExceedsLimit(validatorCount, MAX_VALIDATOR_COUNT);
        }

        uint256 totalVotingPower = 0;
        data = new bytes(VALIDATOR_CACHE_HEADER_LEN + validatorCount * VALIDATOR_CACHE_ENTRY_LEN);
        _writeUint32(data, 0, VALIDATOR_CACHE_MAGIC);
        _writeUint16(data, 12, validatorCount);
        _writeBytes32(data, 14, Header.bytes32LeafHash(validatorsHash));
        _writeUint16(data, 46, 0);

        uint256 offset = VALIDATOR_CACHE_HEADER_LEN;
        for (uint256 i = 0; i < vals.length; i++) {
            ISpectreTendermintMsgs.ValidatorInfo memory val = vals[i];
            require(val.votingPower > 0, ISpectreClientErrors.ZeroVotingPower(i));
            // O(n^2) duplicate-pubkey scan: only runs at genesis/rotation time (bounded by
            // MAX_VALIDATOR_COUNT = 180), never per-packet, so the quadratic cost is cheap here.
            // A duplicate (or repeated) pubkey pinned at multiple indices would let one signature
            // count multiple times toward quorum in `SpectreClient._verifyQuorum` (each index is
            // its own distinct slot there).
            for (uint256 j = 0; j < i; j++) {
                if (vals[j].pubKey == val.pubKey) {
                    revert ISpectreClientErrors.DuplicateValidatorPubkey(j, i);
                }
            }
            totalVotingPower += val.votingPower;
            _writeUint32(data, offset, uint32(i));
            _writeUint64(data, offset + 4, val.votingPower);
            _writeBytes32(data, offset + 12, val.pubKey);
            offset += VALIDATOR_CACHE_ENTRY_LEN;
        }
        require(totalVotingPower <= type(uint64).max, ISpectreClientErrors.BatchLengthMismatch());
        _writeUint64(data, 4, uint64(totalVotingPower));
    }

    /// @notice Reads and validates the cache pointed to by a snapshot.
    function readCache(SpectreStore.PinnedValidatorSetSnapshot memory snapshot)
        internal
        view
        returns (ValidatorCacheHeader memory cacheHeader, bytes memory cacheData)
    {
        if (snapshot.pointer == address(0)) {
            revert ISpectreClientErrors.ValidatorSetCacheMiss(snapshot.validatorsHash);
        }
        cacheData = SSTORE2.read(snapshot.pointer, 0, validatorCacheEntryDataLen(snapshot.entryCount));
        cacheHeader = readValidatorCacheHeader(snapshot.validatorsHash, cacheData);
    }

    function readValidatorCacheHeader(
        bytes32 validatorsHash,
        bytes memory cacheData
    )
        internal
        pure
        returns (ValidatorCacheHeader memory cacheHeader)
    {
        if (cacheData.length < VALIDATOR_CACHE_HEADER_LEN) {
            revert ISpectreClientErrors.CachedValidatorSetCorrupted(validatorsHash);
        }
        if (_readUint32(cacheData, 0) != VALIDATOR_CACHE_MAGIC) {
            revert ISpectreClientErrors.CachedValidatorSetCorrupted(validatorsHash);
        }

        cacheHeader = ValidatorCacheHeader({
            totalVotingPower: _readUint64(cacheData, 4),
            entryCount: _readUint16(cacheData, 12),
            validatorsHashLeaf: _readBytes32(cacheData, 14),
            nodeCount: _readUint16(cacheData, 46)
        });

        if (
            cacheHeader.entryCount == 0 || cacheHeader.entryCount > MAX_VALIDATOR_COUNT || cacheHeader.nodeCount != 0
                || cacheData.length != validatorCacheEntryDataLen(cacheHeader.entryCount)
                || cacheHeader.validatorsHashLeaf != Header.bytes32LeafHash(validatorsHash)
        ) {
            revert ISpectreClientErrors.CachedValidatorSetCorrupted(validatorsHash);
        }
    }

    function validatorCacheEntryDataLen(uint16 entryCount) internal pure returns (uint256) {
        return VALIDATOR_CACHE_HEADER_LEN + uint256(entryCount) * VALIDATOR_CACHE_ENTRY_LEN;
    }

    function resolveValidator(
        bytes32 validatorsHash,
        bytes memory cacheData,
        uint16 entryCount,
        uint32 index
    )
        internal
        pure
        returns (uint64 votingPower, bytes32 pubKey)
    {
        if (index >= entryCount) {
            revert ISpectreClientErrors.CachedSignerNotFound(validatorsHash, index);
        }

        uint256 offset = VALIDATOR_CACHE_HEADER_LEN + uint256(index) * VALIDATOR_CACHE_ENTRY_LEN;
        if (_readUint32(cacheData, offset) != index) {
            revert ISpectreClientErrors.CachedValidatorSetCorrupted(validatorsHash);
        }
        votingPower = _readUint64(cacheData, offset + 4);
        pubKey = _readBytes32(cacheData, offset + 12);
    }

    /// @notice Requires that every active proof signer corresponds to a COMMIT slot in the header
    ///         commit, and that the slot belongs to the same validator the proof claims.
    /// @dev `signerIndices` indexes the header commit; `signerPubkeys` is the per-slot pubkey the
    ///      caller has already matched against the pinned validator set. The two are otherwise
    ///      independent — the commit for the proposed height and the pinned set are different
    ///      validator sets, so the indices legitimately differ — which left a proof free to cite
    ///      one validator's commit slot while drawing another's voting power. Deriving the
    ///      Tendermint address from the pinned pubkey and matching it against the commit slot ties
    ///      them (ZK-09).
    ///
    ///      This is NOT a security boundary. `commitSigs` is relayer calldata, never itself proven
    ///      by the Groth16 circuit, and the Tendermint block hash covers the Header, not the
    ///      Commit — so this check compares relayer data against relayer data: it catches an
    ///      honest relayer's bug, not a malicious relayer. Security comes from the ZK proof over
    ///      the signer pubkeys bound into the SHA-256 witness commitment
    ///      (`SignatureVerifier._hashWitness`) plus the pinned-set pubkey match in `_verifyQuorum`.
    ///      Do not build on this check as if it authenticated the commit.
    /// @param commitSigs the proposed header's commit signatures.
    /// @param signerIndices per-slot index into commitSigs.
    /// @param signerPubkeys per-slot compressed Ed25519 pubkey, already checked against the pinned set.
    /// @param active per-slot real-signer flag; padding slots are skipped.
    function requireProofSignersCommitSigs(
        ISpectreTendermintMsgs.CommitSig[] memory commitSigs,
        uint32[] memory signerIndices,
        bytes32[] memory signerPubkeys,
        bool[] memory active
    )
        internal
        pure
    {
        require(
            signerIndices.length == active.length && signerPubkeys.length == active.length,
            ISpectreClientErrors.BatchLengthMismatch()
        );
        for (uint256 i = 0; i < signerIndices.length; i++) {
            if (!active[i]) {
                continue;
            }
            _requireProofSignerCommitSig(commitSigs, signerIndices[i], signerPubkeys[i]);
        }
    }

    /// @notice Returns the Tendermint address of an Ed25519 validator: the first 20 bytes of
    ///         sha256 over the 32-byte compressed pubkey.
    function tendermintAddress(bytes32 pubKey) internal pure returns (bytes20) {
        return bytes20(sha256(abi.encodePacked(pubKey)));
    }

    function _requireProofSignerCommitSig(
        ISpectreTendermintMsgs.CommitSig[] memory commitSigs,
        uint32 signerIndex,
        bytes32 signerPubkey
    )
        private
        pure
    {
        require(signerIndex < commitSigs.length, ISpectreClientErrors.SignerIndexOutOfRange(signerIndex));
        require(
            commitSigs[signerIndex].flag == ISpectreTendermintMsgs.CommitSigFlag.BLOCK_ID_FLAG_COMMIT,
            ISpectreClientErrors.ProofSignerCommitSigMismatch(signerIndex)
        );
        require(
            commitSigs[signerIndex].validatorAddress == tendermintAddress(signerPubkey),
            ISpectreClientErrors.ProofSignerValidatorMismatch(signerIndex)
        );
    }

    function _writeUint16(bytes memory data, uint256 offset, uint256 value) private pure {
        assembly ("memory-safe") {
            let ptr := add(add(data, 0x20), offset)
            mstore8(ptr, shr(8, value))
            mstore8(add(ptr, 1), value)
        }
    }

    function _writeUint32(bytes memory data, uint256 offset, uint32 value) private pure {
        assembly ("memory-safe") {
            let ptr := add(add(data, 0x20), offset)
            mstore8(ptr, shr(24, value))
            mstore8(add(ptr, 1), shr(16, value))
            mstore8(add(ptr, 2), shr(8, value))
            mstore8(add(ptr, 3), value)
        }
    }

    function _writeUint64(bytes memory data, uint256 offset, uint64 value) private pure {
        assembly ("memory-safe") {
            let ptr := add(add(data, 0x20), offset)
            mstore8(ptr, shr(56, value))
            mstore8(add(ptr, 1), shr(48, value))
            mstore8(add(ptr, 2), shr(40, value))
            mstore8(add(ptr, 3), shr(32, value))
            mstore8(add(ptr, 4), shr(24, value))
            mstore8(add(ptr, 5), shr(16, value))
            mstore8(add(ptr, 6), shr(8, value))
            mstore8(add(ptr, 7), value)
        }
    }

    function _writeBytes32(bytes memory data, uint256 offset, bytes32 value) private pure {
        assembly ("memory-safe") {
            mstore(add(add(data, 0x20), offset), value)
        }
    }

    function _readUint16(bytes memory data, uint256 offset) private pure returns (uint16 value) {
        assembly ("memory-safe") {
            value := shr(240, mload(add(add(data, 0x20), offset)))
        }
    }

    function _readUint32(bytes memory data, uint256 offset) private pure returns (uint32 value) {
        assembly ("memory-safe") {
            value := shr(224, mload(add(add(data, 0x20), offset)))
        }
    }

    function _readUint64(bytes memory data, uint256 offset) private pure returns (uint64 value) {
        assembly ("memory-safe") {
            value := shr(192, mload(add(add(data, 0x20), offset)))
        }
    }

    function _readBytes32(bytes memory data, uint256 offset) private pure returns (bytes32 value) {
        assembly ("memory-safe") {
            value := mload(add(add(data, 0x20), offset))
        }
    }
}
