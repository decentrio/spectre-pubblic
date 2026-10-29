// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { IICS26RouterAccessControlled } from "contracts/core/interfaces/IICS26Router.sol";
import { IICS02ClientAccessControlled } from "contracts/core/interfaces/IICS02Client.sol";
import { IICS20TransferAccessControlled } from "contracts/apps/ics20/interfaces/IICS20Transfer.sol";
import { IPausable } from "contracts/core/interfaces/IPausable.sol";
import { UUPSUpgradeable } from "@openzeppelin-contracts/proxy/utils/UUPSUpgradeable.sol";

/// @title IBC Selector Manifest
/// @notice Deployment selector groups used to configure and verify AccessManager policy.
library IBCSelectorLib {
    /// @notice The functions that can be called by the RELAYER_ROLE in ICS26Router.
    /// @return An array of function selectors that can be called by the RELAYER_ROLE.
    function ics26RelayerSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory relayerFunctions = new bytes4[](5);
        relayerFunctions[0] = IICS26RouterAccessControlled.recvPacket.selector;
        relayerFunctions[1] = IICS26RouterAccessControlled.timeoutPacket.selector;
        relayerFunctions[2] = IICS26RouterAccessControlled.ackPacket.selector;
        relayerFunctions[3] = IICS02ClientAccessControlled.updateApplicationState.selector;
        relayerFunctions[4] = IICS02ClientAccessControlled.updateConsensusState.selector;
        return relayerFunctions;
    }

    /// @notice The functions that can be called by the ID_CUSTOMIZER_ROLE in ICS26Router.
    /// @return An array of function selectors that can be called by the ID_CUSTOMIZER_ROLE.
    function ics26IdCustomizerSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory idCustomizerFunctions = new bytes4[](2);
        idCustomizerFunctions[0] = IICS26RouterAccessControlled.addIBCApp.selector;
        idCustomizerFunctions[1] = IICS02ClientAccessControlled.addClient.selector;
        return idCustomizerFunctions;
    }

    /// @notice The functions that can be called by the MISBEHAVIOUR_SUBMITTER_ROLE.
    /// @return An array of function selectors that can be called by the MISBEHAVIOUR_SUBMITTER_ROLE.
    function ics26MisbehaviourSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory fns = new bytes4[](1);
        fns[0] = IICS02ClientAccessControlled.submitMisbehaviour.selector;
        return fns;
    }

    /// @notice The functions that can be called by the ERC20_CUSTOMIZER_ROLE in ICS20Transfer.
    /// @return An array of function selectors that can be called by the ERC20_CUSTOMIZER_ROLE.
    function erc20CustomizerSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory erc20CustomizerFunctions = new bytes4[](2);
        erc20CustomizerFunctions[0] = IICS20TransferAccessControlled.setCustomERC20.selector;
        erc20CustomizerFunctions[1] = IICS20TransferAccessControlled.setIBCERC20Metadata.selector;
        return erc20CustomizerFunctions;
    }

    /// @notice The functions that can be called by the DELEGATE_SENDER_ROLE in ICS20Transfer.
    /// @return An array of function selectors that can be called by the DELEGATE_SENDER_ROLE.
    function delegateSenderSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory delegateSenderFunctions = new bytes4[](1);
        delegateSenderFunctions[0] = IICS20TransferAccessControlled.sendTransferWithSender.selector;
        return delegateSenderFunctions;
    }

    /// @notice The functions that can be called by the PAUSER_ROLE.
    /// @return An array of function selectors that can be called by the PAUSER_ROLE.
    function pauserSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory pauserFunctions = new bytes4[](1);
        pauserFunctions[0] = IPausable.pause.selector;
        return pauserFunctions;
    }

    /// @notice The functions that can be called by the UNPAUSER_ROLE.
    /// @return An array of function selectors that can be called by the UNPAUSER_ROLE.
    function unpauserSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory unpauserFunctions = new bytes4[](1);
        unpauserFunctions[0] = IPausable.unpause.selector;
        return unpauserFunctions;
    }

    // NOTE: there is deliberately no rateLimiterSelectors(). RATE_LIMITER_ROLE is not wired
    // through setTargetFunctionRole: escrows are created per client at runtime, so a per-target
    // mapping cannot exist before the escrow does, and one that only covered pre-created escrows
    // would leave the rest uncapped. Escrow.setRateLimit checks the global role itself — see
    // RateLimitUpgradeable.setRateLimit.

    /// @notice Functions that must be executed through the delayed upgrade role.
    function upgraderSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory functions_ = new bytes4[](3);
        functions_[0] = UUPSUpgradeable.upgradeToAndCall.selector;
        functions_[1] = IICS20TransferAccessControlled.upgradeEscrowTo.selector;
        functions_[2] = IICS20TransferAccessControlled.upgradeIBCERC20To.selector;
        return functions_;
    }

    /// @notice The functions that can be used to upgrade the beacon contracts in ICS20Transfer.
    /// @dev These functions are not associated with a specific role, but are restricted to the ADMIN_ROLE.
    /// @return An array of function selectors that can be used to upgrade the beacon contracts.
    function beaconUpgradeSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory beaconUpgradeFunctions = new bytes4[](2);
        beaconUpgradeFunctions[0] = IICS20TransferAccessControlled.upgradeEscrowTo.selector;
        beaconUpgradeFunctions[1] = IICS20TransferAccessControlled.upgradeIBCERC20To.selector;
        return beaconUpgradeFunctions;
    }

    /// @notice The functions that can be used to upgrade UUPS contracts such as ICS26Router and ICS20Transfer.
    /// @dev These functions are not associated with a specific role, but are restricted to the ADMIN_ROLE.
    /// @return An array of function selectors that can be used to upgrade UUPS contracts.
    function uupsUpgradeSelectors() internal pure returns (bytes4[] memory) {
        bytes4[] memory uupsUpgradeFunctions = new bytes4[](1);
        uupsUpgradeFunctions[0] = UUPSUpgradeable.upgradeToAndCall.selector;
        return uupsUpgradeFunctions;
    }
}
