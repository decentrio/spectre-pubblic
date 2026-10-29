// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test } from "forge-std/Test.sol";

import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { UUPSUpgradeable } from "@openzeppelin-contracts/proxy/utils/UUPSUpgradeable.sol";

/// @dev Models the ICS20Transfer namespaced layout immediately before refund credits were added.
contract ICS20TransferLaunchGateV1 is UUPSUpgradeable {
    struct ICS20TransferStorage {
        mapping(string clientId => address escrow) _escrows;
        mapping(string denom => address token) _ibcERC20Contracts;
        mapping(address token => string denom) _ibcERC20Denoms;
        address _ics26;
        address _ibcERC20Beacon;
        address _escrowBeacon;
        address _permit2;
        bool _requirePrecreatedEscrows;
        mapping(string clientId => bool active) _activeEscrows;
    }

    bytes32 private constant ICS20TRANSFER_STORAGE_SLOT =
        0x823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f800;

    function initialize(string calldata clientId, address escrow) external {
        ICS20TransferStorage storage $ = _getICS20TransferStorage();
        $._escrows[clientId] = escrow;
        $._requirePrecreatedEscrows = true;
        $._activeEscrows[clientId] = true;
    }

    function _authorizeUpgrade(address) internal override { }

    function _getICS20TransferStorage() private pure returns (ICS20TransferStorage storage $) {
        assembly {
            $.slot := ICS20TRANSFER_STORAGE_SLOT
        }
    }
}

contract ICS20TransferStorageLayoutTest is Test {
    function test_success_upgradePreservesEscrowLaunchGateStorage() public {
        string memory activeClient = "active-client";
        string memory inactiveClient = "inactive-client";
        address escrow = makeAddr("escrow");

        ICS20TransferLaunchGateV1 oldImplementation = new ICS20TransferLaunchGateV1();
        ERC1967Proxy proxy = new ERC1967Proxy(
            address(oldImplementation), abi.encodeCall(ICS20TransferLaunchGateV1.initialize, (activeClient, escrow))
        );

        ICS20TransferLaunchGateV1(address(proxy)).upgradeToAndCall(address(new ICS20Transfer()), "");
        ICS20Transfer upgraded = ICS20Transfer(address(proxy));

        assertTrue(upgraded.requiresPrecreatedEscrows());
        assertEq(upgraded.getEscrow(activeClient), escrow);
        assertTrue(upgraded.isEscrowActive(activeClient));
        assertFalse(upgraded.isEscrowActive(inactiveClient));
    }
}
