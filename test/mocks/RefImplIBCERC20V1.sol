// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ERC20Upgradeable } from "@openzeppelin-upgradeable/token/ERC20/ERC20Upgradeable.sol";
import { UUPSUpgradeable } from "@openzeppelin-contracts/proxy/utils/UUPSUpgradeable.sol";
import { OwnableUpgradeable } from "@openzeppelin-upgradeable/access/OwnableUpgradeable.sol";

/// @notice Storage-compatible pre-escrow reference implementation used to
/// verify migrations of already-initialized custom-token proxies.
contract RefImplIBCERC20V1 is UUPSUpgradeable, ERC20Upgradeable, OwnableUpgradeable {
    error CallerIsNotICS20(address caller);

    // This is the exact prefix of RefImplIBCERC20.RefIBCERC20Storage before
    // the escrow-only burn model appended `_escrow`.
    struct RefIBCERC20StorageV1 {
        address _ics20;
    }

    bytes32 private constant REFIBCERC20_STORAGE_SLOT =
        0x7f1f4ef08fb1ecf5e6ce5f1511ee420a1716a929ca6536d77be0398bd880e400;

    constructor() {
        _disableInitializers();
    }

    function initialize(
        address owner_,
        address ics20_,
        string calldata name_,
        string calldata symbol_
    )
        external
        initializer
    {
        __ERC20_init(name_, symbol_);
        __Ownable_init(owner_);
        _getRefIBCERC20Storage()._ics20 = ics20_;
    }

    function ics20() external view returns (address) {
        return _getRefIBCERC20Storage()._ics20;
    }

    function decimals() public pure override(ERC20Upgradeable) returns (uint8) {
        return 6;
    }

    function mint(address mintAddress, uint256 amount) external onlyICS20 {
        _mint(mintAddress, amount);
    }

    function burn(address mintAddress, uint256 amount) external onlyICS20 {
        _burn(mintAddress, amount);
    }

    function _authorizeUpgrade(address) internal view override(UUPSUpgradeable) onlyOwner { }
    // solhint-disable-previous-line no-empty-blocks

    function _getRefIBCERC20Storage() private pure returns (RefIBCERC20StorageV1 storage $) {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            $.slot := REFIBCERC20_STORAGE_SLOT
        }
    }

    modifier onlyICS20() {
        require(_msgSender() == _getRefIBCERC20Storage()._ics20, CallerIsNotICS20(_msgSender()));
        _;
    }
}
