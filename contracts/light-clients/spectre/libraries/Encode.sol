// SPDX-License-Identifier: MIT

pragma solidity ^0.8.28;

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";

/// @dev CometBFT valid-commit-domain encoders, not a general-purpose proto3 encoder.
/// Some helpers intentionally assume valid commit fields are populated and do not implement
/// every zero-value omission edge case for malformed or unreachable commit data.
library Encode {
    /// @notice Encodes a nanosecond timestamp as a protobuf google.protobuf.Timestamp.
    /// @dev Omits fields with zero value per proto3 rules.
    function encodeTimestamp(uint128 nanos) internal pure returns (bytes memory) {
        uint128 secs = nanos / 1_000_000_000;
        uint128 ns = nanos % 1_000_000_000;
        uint256 totalLen = 0;

        if (secs > 0) {
            totalLen += 1 + _varintLen(uint256(secs));
        }
        if (ns > 0) {
            totalLen += 1 + _varintLen(uint256(ns));
        }

        bytes memory out = new bytes(totalLen);
        uint256 offset = 0;
        if (secs > 0) {
            offset = _storeByte(out, offset, 0x08);
            offset = _writeVarint(out, offset, uint256(secs));
        }
        if (ns > 0) {
            offset = _storeByte(out, offset, 0x10);
            _writeVarint(out, offset, uint256(ns));
        }
        return out;
    }

    function encodeValidator(ISpectreTendermintMsgs.SimpleValidator memory validator)
        internal
        pure
        returns (bytes memory)
    {
        uint256 totalLen = 36;
        if (validator.votingPower > 0) {
            totalLen += 1 + _varintLen(uint256(validator.votingPower));
        }

        bytes memory out = new bytes(totalLen);
        uint256 offset = 0;

        // Field 1: pub_key (tag = 1, wire type = 2 for length-delimited)
        // Nested PublicKey{Ed25519: pubKey} is always 34 bytes: 0x0A 0x20 <32-byte key>.
        offset = _storeByte(out, offset, 0x0A);
        offset = _storeByte(out, offset, 0x22);
        offset = _storeByte(out, offset, 0x0A);
        offset = _storeByte(out, offset, 0x20);
        offset = _storeBytes32(out, offset, validator.pubKey);

        // Field 2: voting_power (tag = 2, wire type = 0 for varint)
        if (validator.votingPower > 0) {
            offset = _storeByte(out, offset, 0x10);
            _writeVarint(out, offset, uint256(validator.votingPower));
        }

        return out;
    }

    function encodeVersion(ISpectreTendermintMsgs.Version memory version) internal pure returns (bytes memory) {
        uint256 totalLen = 0;

        if (version.blockVersion > 0) {
            totalLen += 1 + _varintLen(uint256(version.blockVersion));
        }

        if (version.appVersion > 0) {
            totalLen += 1 + _varintLen(uint256(version.appVersion));
        }

        bytes memory out = new bytes(totalLen);
        uint256 offset = 0;
        if (version.blockVersion > 0) {
            offset = _storeByte(out, offset, 0x08);
            offset = _writeVarint(out, offset, uint256(version.blockVersion));
        }
        if (version.appVersion > 0) {
            offset = _storeByte(out, offset, 0x10);
            _writeVarint(out, offset, uint256(version.appVersion));
        }

        return out;
    }

    function encodeBlockId(ISpectreTendermintMsgs.BlockId memory blockId) internal pure returns (bytes memory) {
        // Valid commits always carry a populated block hash and part set header.
        bytes memory partSetHeaderEncoded = encodePartSetHeader(blockId.partSetHeader);
        uint256 partSetLen = partSetHeaderEncoded.length;
        bytes memory out = new bytes(34 + 1 + _varintLen(partSetLen) + partSetLen);

        uint256 offset = 0;
        // Field 1: hashData (tag = 1, wire type = 2 for bytes)
        offset = _storeByte(out, offset, 0x0A);
        offset = _storeByte(out, offset, 0x20);
        offset = _storeBytes32(out, offset, blockId.hashData);

        // Field 2: partSetHeader (tag = 2, wire type = 2 for message)
        offset = _storeByte(out, offset, 0x12);
        offset = _writeVarint(out, offset, partSetLen);
        _copyBytes(out, offset, partSetHeaderEncoded);

        return out;
    }

    /// @notice Wraps a string in gogoproto StringValue{Value: str} for header hashing.
    /// Matches CometBFT's cdcEncode(string) used in Header.Hash().
    function cdcEncodeString(string memory value) internal pure returns (bytes memory) {
        bytes memory valueBytes = bytes(value);
        if (valueBytes.length == 0) return new bytes(0);
        bytes memory out = new bytes(1 + _varintLen(valueBytes.length) + valueBytes.length);
        uint256 offset = _storeByte(out, 0, 0x0A);
        offset = _writeVarint(out, offset, valueBytes.length);
        _copyBytes(out, offset, valueBytes);
        return out;
    }

    /// @notice Wraps an int64 in gogoproto Int64Value{Value: n} for header hashing.
    /// Matches CometBFT's cdcEncode(int64) used in Header.Hash().
    function cdcEncodeInt64(uint256 value) internal pure returns (bytes memory) {
        if (value == 0) return new bytes(0);
        bytes memory out = new bytes(1 + _varintLen(value));
        uint256 offset = _storeByte(out, 0, 0x08);
        _writeVarint(out, offset, value);
        return out;
    }

    /// @notice Wraps variable-length bytes in gogoproto BytesValue{Value: bz} for header hashing.
    /// Matches CometBFT's cdcEncode([]byte) used in Header.Hash().
    function cdcEncodeBytes(bytes memory value) internal pure returns (bytes memory) {
        if (value.length == 0) return new bytes(0);
        bytes memory out = new bytes(1 + _varintLen(value.length) + value.length);
        uint256 offset = _storeByte(out, 0, 0x0A);
        offset = _writeVarint(out, offset, value.length);
        _copyBytes(out, offset, value);
        return out;
    }

    /// @notice Wraps a bytes32 hash in gogoproto BytesValue{Value: hash} for header hashing.
    /// Matches CometBFT's cdcEncode(HexBytes) used in Header.Hash().
    function cdcEncodeBytes32(bytes32 value) internal pure returns (bytes memory) {
        bytes memory out = new bytes(34);
        uint256 offset = _storeByte(out, 0, 0x0A);
        offset = _storeByte(out, offset, 0x20);
        _storeBytes32(out, offset, value);
        return out;
    }

    /// @notice Encodes a signed 64-bit integer as 8-byte little-endian (protobuf sfixed64).
    function encodeSfixed64(int64 value) internal pure returns (bytes memory) {
        bytes memory result = new bytes(8);
        // Preserve the 64-bit two's-complement representation for protobuf sfixed64.
        // forge-lint: disable-next-line(unsafe-typecast)
        uint64 v = uint64(value);
        assembly ("memory-safe") {
            // mstore is big-endian, so each byte of v must be placed in the most-significant
            // 8 bytes of the 256-bit word. Byte i of little-endian output = bits [(i*8)+7:(i*8)]
            // of v, which must land at word bits [255-(i*8):248-(i*8)].
            mstore(
                add(result, 0x20),
                or(
                    or(
                        or(shl(248, and(v, 0xff)), shl(232, and(v, 0xff00))),
                        or(shl(216, and(v, 0xff0000)), shl(200, and(v, 0xff000000)))
                    ),
                    or(
                        or(shl(184, and(v, 0xff00000000)), shl(168, and(v, 0xff0000000000))),
                        or(shl(152, and(v, 0xff000000000000)), shl(136, and(v, 0xff00000000000000)))
                    )
                )
            )
        }
        return result;
    }

    function encodePartSetHeader(ISpectreTendermintMsgs.PartSetHeader memory partSetHeader)
        internal
        pure
        returns (bytes memory)
    {
        // Valid commits always carry total >= 1 and a populated part set hash.
        bytes memory out = new bytes(1 + _varintLen(uint256(partSetHeader.total)) + 34);
        uint256 offset = 0;

        // Field 1: total (tag = 1, wire type = 0 for varint)
        offset = _storeByte(out, offset, 0x08);
        offset = _writeVarint(out, offset, uint256(partSetHeader.total));

        // Field 2: hashData (tag = 2, wire type = 2 for bytes)
        offset = _storeByte(out, offset, 0x12);
        offset = _storeByte(out, offset, 0x20);
        _storeBytes32(out, offset, partSetHeader.hashData);

        return out;
    }

    function _varintLen(uint256 value) private pure returns (uint256 len) {
        len = 1;
        while (value >= 128) {
            value >>= 7;
            unchecked {
                ++len;
            }
        }
    }

    /// @notice Encodes `value` as a protobuf varint into `out` starting at `offset`.
    /// @dev Each byte holds 7 bits of value; the MSB is 1 if more bytes follow, 0 on the last byte.
    /// @return newOffset The offset of the next unwritten byte after the varint.
    function _writeVarint(bytes memory out, uint256 offset, uint256 value) private pure returns (uint256 newOffset) {
        assembly ("memory-safe") {
            // Point ptr at out[offset]: skip the 32-byte length prefix (0x20), then advance by offset.
            let ptr := add(add(out, 0x20), offset)
            // Each iteration emits one continuation byte: low 7 bits of value | 0x80 (MSB = "more follows").
            for { } iszero(lt(value, 0x80)) { } {
                mstore8(ptr, or(and(value, 0x7f), 0x80))
                ptr := add(ptr, 1)
                offset := add(offset, 1)
                value := shr(7, value) // consume the 7 bits just written
            }
            // Final byte: value < 0x80, so MSB is 0 — signals end of varint.
            mstore8(ptr, value)
            newOffset := add(offset, 1)
        }
    }

    function _storeByte(bytes memory out, uint256 offset, uint8 value) private pure returns (uint256) {
        assembly ("memory-safe") {
            mstore8(add(add(out, 0x20), offset), value)
        }
        return offset + 1;
    }

    function _storeBytes32(bytes memory out, uint256 offset, bytes32 value) private pure returns (uint256) {
        assembly ("memory-safe") {
            mstore(add(add(out, 0x20), offset), value)
        }
        return offset + 32;
    }

    function _copyBytes(bytes memory out, uint256 dstOffset, bytes memory src) private pure returns (uint256) {
        uint256 len = src.length;
        if (len == 0) return dstOffset;
        // mcopy (EIP-5656, Cancun): 3 + 3*ceil(len/32) gas vs ~18-20 gas/word with a manual loop.
        assembly ("memory-safe") {
            mcopy(add(add(out, 0x20), dstOffset), add(src, 0x20), len)
        }
        return dstOffset + len;
    }
}
