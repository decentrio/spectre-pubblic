// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length

import { Test, console } from "forge-std/Test.sol";

import { Encode } from "contracts/light-clients/spectre/libraries/Encode.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";

/// @title EncodeTest
/// @notice Checks Encode.sol and Header.sol against CometBFT.
///         - `test_goVectors_*` read test/fixtures/encode_vectors.json, which `TestEncodeVectors`
///           (relayer/client) computes with CometBFT's own types. Regenerate it with
///           `cd relayer && go test ./client -run TestEncodeVectors -update`.
///         - The remaining tests use hard-coded vectors whose generator is not in this repository
///           (SUP-06 in #288). The fixture-driven tests are the authoritative cross-check; these
///           are kept as readable examples.
///         One known gap: a header with an empty last_block_id is hashed differently on-chain and by
///         CometBFT, so no such header is in the fixture (see TestEmptyBlockIDMarshalsToTwoBytes).
contract EncodeTest is Test {
    // ─── Test data (hard-coded; see the contract NatSpec for provenance) ───

    function _pubKey() internal pure returns (bytes32) {
        bytes32 pk;
        assembly {
            pk := 0xaaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9
        }
        return pk;
    }

    function _hash1() internal pure returns (bytes32) {
        return bytes32(0x101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f);
    }

    function _hash2() internal pure returns (bytes32) {
        return bytes32(0x202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f);
    }

    function _hash3() internal pure returns (bytes32) {
        return bytes32(0x303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f);
    }

    function _hash4() internal pure returns (bytes32) {
        return bytes32(0x404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f);
    }

    function _hash5() internal pure returns (bytes32) {
        return bytes32(0x505152535455565758595a5b5c5d5e5f606162636465666768696a6b6c6d6e6f);
    }

    function _hash6() internal pure returns (bytes32) {
        return bytes32(0x606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f);
    }

    function _hash7() internal pure returns (bytes32) {
        return bytes32(0x707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f);
    }

    // ─── encodeValidator tests ───
    // Go reference: proto.Marshal(SimpleValidator{PubKey: &PublicKey{Ed25519: pubKey}, VotingPower: vp})

    function test_encodeValidator_power100() public pure {
        ISpectreTendermintMsgs.SimpleValidator memory v =
            ISpectreTendermintMsgs.SimpleValidator({ pubKey: _pubKey(), votingPower: 100 });
        assertEq(
            Encode.encodeValidator(v), hex"0a220a20aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c91064"
        );
    }

    function test_encodeValidator_power1() public pure {
        ISpectreTendermintMsgs.SimpleValidator memory v =
            ISpectreTendermintMsgs.SimpleValidator({ pubKey: _pubKey(), votingPower: 1 });
        assertEq(
            Encode.encodeValidator(v), hex"0a220a20aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c91001"
        );
    }

    function test_encodeValidator_power0() public pure {
        ISpectreTendermintMsgs.SimpleValidator memory v =
            ISpectreTendermintMsgs.SimpleValidator({ pubKey: _pubKey(), votingPower: 0 });
        // Go proto.Marshal skips votingPower=0
        assertEq(
            Encode.encodeValidator(v), hex"0a220a20aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9"
        );
    }

    function test_encodeValidator_power1000000() public pure {
        ISpectreTendermintMsgs.SimpleValidator memory v =
            ISpectreTendermintMsgs.SimpleValidator({ pubKey: _pubKey(), votingPower: 1_000_000 });
        assertEq(
            Encode.encodeValidator(v),
            hex"0a220a20aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c910c0843d"
        );
    }

    // ─── encodeVersion tests ───

    function test_encodeVersion_11_0() public pure {
        ISpectreTendermintMsgs.Version memory v = ISpectreTendermintMsgs.Version({ blockVersion: 11, appVersion: 0 });
        // Go proto.Marshal: appVersion=0 is skipped
        assertEq(Encode.encodeVersion(v), hex"080b");
    }

    function test_encodeVersion_11_2() public pure {
        ISpectreTendermintMsgs.Version memory v = ISpectreTendermintMsgs.Version({ blockVersion: 11, appVersion: 2 });
        assertEq(Encode.encodeVersion(v), hex"080b1002");
    }

    function test_encodeVersion_1_1() public pure {
        ISpectreTendermintMsgs.Version memory v = ISpectreTendermintMsgs.Version({ blockVersion: 1, appVersion: 1 });
        assertEq(Encode.encodeVersion(v), hex"08011001");
    }

    // ─── encodePartSetHeader tests ───

    function test_encodePartSetHeader() public pure {
        ISpectreTendermintMsgs.PartSetHeader memory psh =
            ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: _hash1() });
        assertEq(
            Encode.encodePartSetHeader(psh),
            hex"08011220101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f"
        );
    }

    // ─── encodeBlockId tests ───

    function test_encodeBlockId() public pure {
        ISpectreTendermintMsgs.BlockId memory bid = ISpectreTendermintMsgs.BlockId({
            hashData: _hash2(), partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: _hash1() })
        });
        assertEq(
            Encode.encodeBlockId(bid),
            hex"0a20202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f122408011220101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f"
        );
    }

    // ─── cdcEncode tests ───

    function test_cdcEncodeString() public pure {
        // Go: cdcEncode("cosmoshub-4") = StringValue{Value: "cosmoshub-4"}.Marshal()
        assertEq(Encode.cdcEncodeString("cosmoshub-4"), hex"0a0b636f736d6f736875622d34");
    }

    function test_cdcEncodeString_empty() public pure {
        assertEq(Encode.cdcEncodeString(""), hex"");
    }

    function test_cdcEncodeInt64() public pure {
        // Go: cdcEncode(int64(12345)) = Int64Value{Value: 12345}.Marshal()
        assertEq(Encode.cdcEncodeInt64(12_345), hex"08b960");
    }

    function test_cdcEncodeInt64_zero() public pure {
        assertEq(Encode.cdcEncodeInt64(0), hex"");
    }

    // cdcEncodeInt64(v) is tag 0x08 followed by the protobuf varint of v, so these cases also pin
    // _varintLen/_writeVarint at every varint width boundary. Every encoder in this library shares them.

    function test_cdcEncodeInt64_one() public pure {
        assertEq(Encode.cdcEncodeInt64(1), hex"0801");
    }

    function test_cdcEncodeInt64_127() public pure {
        assertEq(Encode.cdcEncodeInt64(127), hex"087f");
    }

    function test_cdcEncodeInt64_128() public pure {
        assertEq(Encode.cdcEncodeInt64(128), hex"088001");
    }

    function test_cdcEncodeInt64_255() public pure {
        assertEq(Encode.cdcEncodeInt64(255), hex"08ff01");
    }

    function test_cdcEncodeInt64_256() public pure {
        assertEq(Encode.cdcEncodeInt64(256), hex"088002");
    }

    function test_cdcEncodeInt64_300() public pure {
        assertEq(Encode.cdcEncodeInt64(300), hex"08ac02");
    }

    function test_cdcEncodeInt64_16384() public pure {
        assertEq(Encode.cdcEncodeInt64(16_384), hex"08808001");
    }

    function test_cdcEncodeInt64_1000000() public pure {
        assertEq(Encode.cdcEncodeInt64(1_000_000), hex"08c0843d");
    }

    function test_cdcEncodeBytes32() public pure {
        // Go: cdcEncode(hash5) = BytesValue{Value: hash5}.Marshal()
        assertEq(
            Encode.cdcEncodeBytes32(_hash5()), hex"0a20505152535455565758595a5b5c5d5e5f606162636465666768696a6b6c6d6e6f"
        );
    }

    /// @notice Reproduces issue.md: cdcEncodeBytes32(bytes32(0)) wrongly returns empty bytes.
    /// CometBFT's cdcEncode(HexBytes) on a 32-byte all-zero hash emits BytesValue{Value: zeros}
    /// = 0x0a 0x20 followed by 32 zero bytes (34 bytes total). The current implementation
    /// short-circuits on `value == bytes32(0)` and returns 0 bytes, causing a header-hash
    /// mismatch whenever any unconditionally-hashed field (e.g. appHash) is all zero.
    function test_cdcEncodeBytes32_allZero_matchesCometBFT() public pure {
        bytes memory expected = hex"0a200000000000000000000000000000000000000000000000000000000000000000";
        assertEq(Encode.cdcEncodeBytes32(bytes32(0)), expected);
    }

    /// @notice Sanity cross-check: cdcEncodeBytes on a 32-byte zero buffer (which CometBFT
    /// treats identically to cdcEncodeBytes32(bytes32(0))) emits the full BytesValue,
    /// proving the bug is isolated to the bytes32 overload.
    function test_cdcEncodeBytes32_allZero_matchesBytesOverload() public pure {
        bytes memory zeros32 = new bytes(32);
        assertEq(Encode.cdcEncodeBytes32(bytes32(0)), Encode.cdcEncodeBytes(zeros32));
    }

    function test_encodeTimestamp() public pure {
        // Go: gogotypes.StdTimeMarshal(time.Unix(1700000000, 0))
        // Encode.encodeTimestamp now takes nanoseconds and emits both
        // seconds (field 1) and nanos (field 2) when non-zero.
        assertEq(Encode.encodeTimestamp(uint128(1_700_000_000) * 1e9), hex"0880e2cfaa06");
    }

    // ─── hashValSet tests ───

    function test_hashValSet() public pure {
        ISpectreTendermintMsgs.ValidatorInfo[] memory vals = new ISpectreTendermintMsgs.ValidatorInfo[](2);
        vals[0] = ISpectreTendermintMsgs.ValidatorInfo({
            valAddress: hex"", pubKey: _pubKey(), votingPower: 100, proposerPriority: 0
        });
        vals[1] = ISpectreTendermintMsgs.ValidatorInfo({
            valAddress: hex"",
            pubKey: bytes32(0xbbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9da),
            votingPower: 200,
            proposerPriority: 0
        });

        ISpectreTendermintMsgs.ValidatorSet memory valSet = ISpectreTendermintMsgs.ValidatorSet({
            validators: vals,
            hasProposer: false,
            proposer: ISpectreTendermintMsgs.ValidatorInfo({
                valAddress: hex"", pubKey: bytes32(0), votingPower: 0, proposerPriority: 0
            }),
            totalVotingPower: 300
        });

        // Go reference: merkle.HashFromByteSlices(proto-encoded validators)
        assertEq(Header.hashValSet(valSet), bytes32(0x1bf78c35d508c8720ea6cca7225cf9fe1a628438d1aa6c7e2d2dbddff036023f));
    }

    // ─── hashHeader test ───

    function test_hashHeader() public pure {
        ISpectreTendermintMsgs.BlockHeader memory header = ISpectreTendermintMsgs.BlockHeader({
            version: ISpectreTendermintMsgs.Version({ blockVersion: 11, appVersion: 0 }),
            chainId: "cosmoshub-4",
            height: 12_345,
            time: uint128(1_700_000_000) * 1e9,
            hasLastBlockId: true,
            lastBlockId: ISpectreTendermintMsgs.BlockId({
                hashData: _hash2(),
                partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: _hash1() })
            }),
            hasLastCommitHash: true,
            lastCommitHash: _hash3(),
            hasDataHash: true,
            dataHash: _hash4(),
            validatorsHash: _hash5(),
            nextValidatorsHash: _hash6(),
            consensusHash: _hash7(),
            appHash: bytes32(0xa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf),
            hasLastResultsHash: true,
            lastResultsHash: _hash1(),
            hasEvidenceHash: true,
            evidenceHash: _hash2(),
            proposerAddress: hex"f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00010203"
        });

        // Go reference: types.Header.Hash()
        assertEq(Header.hashHeader(header), bytes32(0x94db05d2ad72b0ad975ce0b1a832c5cd4b7bfdc40008a93a815509b12cfebc20));
    }

    function test_hashHeaderWithCachedChainId_matchesHashHeader() public pure {
        ISpectreTendermintMsgs.BlockHeader memory header = ISpectreTendermintMsgs.BlockHeader({
            version: ISpectreTendermintMsgs.Version({ blockVersion: 11, appVersion: 0 }),
            chainId: "cosmoshub-4",
            height: 12_345,
            time: uint128(1_700_000_000) * 1e9,
            hasLastBlockId: true,
            lastBlockId: ISpectreTendermintMsgs.BlockId({
                hashData: _hash2(),
                partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: _hash1() })
            }),
            hasLastCommitHash: true,
            lastCommitHash: _hash3(),
            hasDataHash: true,
            dataHash: _hash4(),
            validatorsHash: _hash5(),
            nextValidatorsHash: _hash6(),
            consensusHash: _hash7(),
            appHash: bytes32(0xa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf),
            hasLastResultsHash: true,
            lastResultsHash: _hash1(),
            hasEvidenceHash: true,
            evidenceHash: _hash2(),
            proposerAddress: hex"f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00010203"
        });

        assertEq(
            Header.hashHeaderWithCachedChainId(header, Header.chainIdLeafHash(header.chainId)),
            Header.hashHeader(header)
        );
    }

    function test_hashHeaderWithCachedLeaves_matchesHashHeader() public pure {
        ISpectreTendermintMsgs.BlockHeader memory header = ISpectreTendermintMsgs.BlockHeader({
            version: ISpectreTendermintMsgs.Version({ blockVersion: 11, appVersion: 0 }),
            chainId: "cosmoshub-4",
            height: 12_345,
            time: uint128(1_700_000_000) * 1e9,
            hasLastBlockId: true,
            lastBlockId: ISpectreTendermintMsgs.BlockId({
                hashData: _hash2(),
                partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: _hash1() })
            }),
            hasLastCommitHash: true,
            lastCommitHash: _hash3(),
            hasDataHash: true,
            dataHash: _hash4(),
            validatorsHash: _hash5(),
            nextValidatorsHash: _hash6(),
            consensusHash: _hash7(),
            appHash: bytes32(0xa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf),
            hasLastResultsHash: true,
            lastResultsHash: _hash1(),
            hasEvidenceHash: true,
            evidenceHash: _hash2(),
            proposerAddress: hex"f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00010203"
        });

        assertEq(
            Header.hashHeaderWithCachedLeaves(
                header, Header.chainIdLeafHash(header.chainId), Header.bytes32LeafHash(header.validatorsHash)
            ),
            Header.hashHeader(header)
        );

        header.nextValidatorsHash = header.validatorsHash;
        assertEq(
            Header.hashHeaderWithCachedLeaves(
                header, Header.chainIdLeafHash(header.chainId), Header.bytes32LeafHash(header.validatorsHash)
            ),
            Header.hashHeader(header)
        );
    }

    // ─── CometBFT cross-check vectors ───
    // test/fixtures/encode_vectors.json is written by relayer/client TestEncodeVectors, which computes every
    // expected value with CometBFT's own types. A failure here with a passing Go test means Encode.sol or
    // Header.sol drifted from CometBFT; a failing Go test means the fixture is stale (rerun it with -update).

    string private constant VECTORS = "test/fixtures/encode_vectors.json";

    function _path(string memory array, uint256 i, string memory field) private pure returns (string memory) {
        return string.concat(".", array, "[", vm.toString(i), "].", field);
    }

    function _count(string memory raw, string memory name) private view returns (uint256 n) {
        n = vm.parseJsonUint(raw, string.concat(".counts.", name));
        assertGt(n, 0, name);
    }

    function test_goVectors_versions() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "versions"); i++) {
            ISpectreTendermintMsgs.Version memory v = ISpectreTendermintMsgs.Version({
                blockVersion: uint64(vm.parseJsonUint(raw, _path("versions", i, "block"))),
                appVersion: uint64(vm.parseJsonUint(raw, _path("versions", i, "app")))
            });
            assertEq(Encode.encodeVersion(v), vm.parseJsonBytes(raw, _path("versions", i, "encoded")), "version");
        }
    }

    function test_goVectors_validators() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "validators"); i++) {
            ISpectreTendermintMsgs.SimpleValidator memory v = ISpectreTendermintMsgs.SimpleValidator({
                pubKey: vm.parseJsonBytes32(raw, _path("validators", i, "pubKey")),
                votingPower: uint64(vm.parseJsonUint(raw, _path("validators", i, "votingPower")))
            });
            assertEq(Encode.encodeValidator(v), vm.parseJsonBytes(raw, _path("validators", i, "encoded")), "validator");
        }
    }

    function test_goVectors_blockIds() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "blockIds"); i++) {
            ISpectreTendermintMsgs.BlockId memory id = ISpectreTendermintMsgs.BlockId({
                hashData: vm.parseJsonBytes32(raw, _path("blockIds", i, "hash")),
                partSetHeader: ISpectreTendermintMsgs.PartSetHeader({
                    total: uint32(vm.parseJsonUint(raw, _path("blockIds", i, "total"))),
                    hashData: vm.parseJsonBytes32(raw, _path("blockIds", i, "partSetHash"))
                })
            });
            assertEq(Encode.encodeBlockId(id), vm.parseJsonBytes(raw, _path("blockIds", i, "encoded")), "blockId");
        }
    }

    function test_goVectors_partSetHeaders() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "partSetHeaders"); i++) {
            ISpectreTendermintMsgs.PartSetHeader memory psh = ISpectreTendermintMsgs.PartSetHeader({
                total: uint32(vm.parseJsonUint(raw, _path("partSetHeaders", i, "total"))),
                hashData: vm.parseJsonBytes32(raw, _path("partSetHeaders", i, "hash"))
            });
            assertEq(
                Encode.encodePartSetHeader(psh), vm.parseJsonBytes(raw, _path("partSetHeaders", i, "encoded")), "psh"
            );
        }
    }

    function test_goVectors_timestamps() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "timestamps"); i++) {
            uint128 nanos = uint128(vm.parseJsonUint(raw, _path("timestamps", i, "nanos")));
            assertEq(Encode.encodeTimestamp(nanos), vm.parseJsonBytes(raw, _path("timestamps", i, "encoded")), "ts");
        }
    }

    function test_goVectors_cdcEncode() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "cdcStrings"); i++) {
            assertEq(
                Encode.cdcEncodeString(vm.parseJsonString(raw, _path("cdcStrings", i, "value"))),
                vm.parseJsonBytes(raw, _path("cdcStrings", i, "encoded")),
                "cdcString"
            );
        }
        for (uint256 i = 0; i < _count(raw, "cdcInt64s"); i++) {
            assertEq(
                Encode.cdcEncodeInt64(vm.parseJsonUint(raw, _path("cdcInt64s", i, "value"))),
                vm.parseJsonBytes(raw, _path("cdcInt64s", i, "encoded")),
                "cdcInt64"
            );
        }
        for (uint256 i = 0; i < _count(raw, "cdcBytes"); i++) {
            assertEq(
                Encode.cdcEncodeBytes(vm.parseJsonBytes(raw, _path("cdcBytes", i, "value"))),
                vm.parseJsonBytes(raw, _path("cdcBytes", i, "encoded")),
                "cdcBytes"
            );
        }
        for (uint256 i = 0; i < _count(raw, "cdcBytes32s"); i++) {
            assertEq(
                Encode.cdcEncodeBytes32(vm.parseJsonBytes32(raw, _path("cdcBytes32s", i, "value"))),
                vm.parseJsonBytes(raw, _path("cdcBytes32s", i, "encoded")),
                "cdcBytes32"
            );
        }
    }

    function test_goVectors_validatorSetHashes() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "valSets"); i++) {
            bytes32[] memory pubKeys = vm.parseJsonBytes32Array(raw, _path("valSets", i, "pubKeys"));
            uint256[] memory powers = vm.parseJsonUintArray(raw, _path("valSets", i, "votingPowers"));
            assertEq(pubKeys.length, powers.length, "length");

            ISpectreTendermintMsgs.ValidatorSet memory valSet;
            valSet.validators = new ISpectreTendermintMsgs.ValidatorInfo[](pubKeys.length);
            for (uint256 j = 0; j < pubKeys.length; j++) {
                valSet.validators[j].pubKey = pubKeys[j];
                valSet.validators[j].votingPower = uint64(powers[j]);
            }

            assertEq(
                Header.hashValSet(valSet),
                vm.parseJsonBytes32(raw, _path("valSets", i, "hash")),
                vm.parseJsonString(raw, _path("valSets", i, "name"))
            );
        }
    }

    function test_goVectors_headerHashes() public view {
        string memory raw = vm.readFile(VECTORS);
        for (uint256 i = 0; i < _count(raw, "headers"); i++) {
            ISpectreTendermintMsgs.BlockHeader memory header = _headerVector(raw, i);
            bytes32 want = vm.parseJsonBytes32(raw, _path("headers", i, "hash"));
            string memory name = vm.parseJsonString(raw, _path("headers", i, "name"));

            assertEq(Header.hashHeader(header), want, name);
            assertEq(Header.hashHeaderWithCachedChainId(header, Header.chainIdLeafHash(header.chainId)), want, name);
        }
    }

    function _headerVector(
        string memory raw,
        uint256 i
    )
        private
        view
        returns (ISpectreTendermintMsgs.BlockHeader memory h)
    {
        h.version = ISpectreTendermintMsgs.Version({
            blockVersion: uint64(vm.parseJsonUint(raw, _path("headers", i, "versionBlock"))),
            appVersion: uint64(vm.parseJsonUint(raw, _path("headers", i, "versionApp")))
        });
        h.chainId = vm.parseJsonString(raw, _path("headers", i, "chainId"));
        h.height = uint64(vm.parseJsonUint(raw, _path("headers", i, "height")));
        h.time = uint128(vm.parseJsonUint(raw, _path("headers", i, "timeNanos")));

        h.hasLastBlockId = vm.parseJsonBool(raw, _path("headers", i, "hasLastBlockId"));
        h.lastBlockId = ISpectreTendermintMsgs.BlockId({
            hashData: vm.parseJsonBytes32(raw, _path("headers", i, "lastBlockIdHash")),
            partSetHeader: ISpectreTendermintMsgs.PartSetHeader({
                total: uint32(vm.parseJsonUint(raw, _path("headers", i, "lastPartSetTotal"))),
                hashData: vm.parseJsonBytes32(raw, _path("headers", i, "lastPartSetHash"))
            })
        });
        h.hasLastCommitHash = vm.parseJsonBool(raw, _path("headers", i, "hasLastCommitHash"));
        h.lastCommitHash = vm.parseJsonBytes32(raw, _path("headers", i, "lastCommitHash"));
        h.hasDataHash = vm.parseJsonBool(raw, _path("headers", i, "hasDataHash"));
        h.dataHash = vm.parseJsonBytes32(raw, _path("headers", i, "dataHash"));
        h.validatorsHash = vm.parseJsonBytes32(raw, _path("headers", i, "validatorsHash"));
        h.nextValidatorsHash = vm.parseJsonBytes32(raw, _path("headers", i, "nextValidatorsHash"));
        h.consensusHash = vm.parseJsonBytes32(raw, _path("headers", i, "consensusHash"));
        h.appHash = vm.parseJsonBytes32(raw, _path("headers", i, "appHash"));
        h.hasLastResultsHash = vm.parseJsonBool(raw, _path("headers", i, "hasLastResultsHash"));
        h.lastResultsHash = vm.parseJsonBytes32(raw, _path("headers", i, "lastResultsHash"));
        h.hasEvidenceHash = vm.parseJsonBool(raw, _path("headers", i, "hasEvidenceHash"));
        h.evidenceHash = vm.parseJsonBytes32(raw, _path("headers", i, "evidenceHash"));
        h.proposerAddress = vm.parseJsonBytes(raw, _path("headers", i, "proposerAddress"));
    }

    // ─── Debug: emit encoded bytes for manual inspection ───

    function test_debug_printEncodings() public pure {
        console.log("=== encodeVersion ===");
        console.logBytes(Encode.encodeVersion(ISpectreTendermintMsgs.Version({ blockVersion: 11, appVersion: 0 })));

        console.log("=== encodeValidator ===");
        console.logBytes(
            Encode.encodeValidator(ISpectreTendermintMsgs.SimpleValidator({ pubKey: _pubKey(), votingPower: 100 }))
        );

        console.log("=== encodeBlockId ===");
        console.logBytes(
            Encode.encodeBlockId(
                ISpectreTendermintMsgs.BlockId({
                    hashData: _hash2(),
                    partSetHeader: ISpectreTendermintMsgs.PartSetHeader({ total: 1, hashData: _hash1() })
                })
            )
        );
    }
}
