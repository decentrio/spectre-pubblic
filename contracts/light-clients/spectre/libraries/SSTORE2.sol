// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @notice Minimal SSTORE2 helper for immutable byte blobs.
/// @dev Stores bytes as contract runtime code prefixed by STOP, then reads via EXTCODECOPY.
library SSTORE2 {
    error SSTORE2WriteFailed();
    error SSTORE2InvalidPointer(address pointer);
    error SSTORE2DataTooLarge(uint256 length);

    uint256 private constant MAX_RUNTIME_SIZE = 24_576;

    function write(bytes memory data) internal returns (address pointer) {
        bytes memory runtime = abi.encodePacked(hex"00", data);
        if (runtime.length > MAX_RUNTIME_SIZE) {
            revert SSTORE2DataTooLarge(data.length);
        }

        bytes memory creation = abi.encodePacked(hex"61", bytes2(uint16(runtime.length)), hex"80600a3d393df3", runtime);

        assembly ("memory-safe") {
            pointer := create(0, add(creation, 0x20), mload(creation))
        }
        if (pointer == address(0)) {
            revert SSTORE2WriteFailed();
        }
    }

    function read(address pointer) internal view returns (bytes memory data) {
        uint256 size;
        assembly ("memory-safe") {
            size := extcodesize(pointer)
        }
        if (size <= 1) {
            revert SSTORE2InvalidPointer(pointer);
        }

        uint256 len = size - 1;
        data = new bytes(len);
        assembly ("memory-safe") {
            extcodecopy(pointer, add(data, 0x20), 1, len)
        }
    }

    function read(address pointer, uint256 start, uint256 len) internal view returns (bytes memory data) {
        uint256 size;
        assembly ("memory-safe") {
            size := extcodesize(pointer)
        }
        if (size <= 1) {
            revert SSTORE2InvalidPointer(pointer);
        }

        uint256 dataSize = size - 1;
        if (start > dataSize || len > dataSize - start) {
            revert SSTORE2InvalidPointer(pointer);
        }

        data = new bytes(len);
        assembly ("memory-safe") {
            extcodecopy(pointer, add(data, 0x20), add(start, 1), len)
        }
    }
}
