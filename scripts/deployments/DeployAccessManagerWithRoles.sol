// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable gas-custom-errors,reason-string

import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IBCSelectorLib } from "scripts/deployments/IBCSelectorLib.sol";
import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";
import { IAccessManager } from "@openzeppelin-contracts/access/manager/IAccessManager.sol";

abstract contract DeployAccessManagerWithRoles {
    function accessManagerSetTargetRoles(
        IAccessManager accessManager,
        address ics26,
        address ics20,
        bool pubRelay
    )
        public
    {
        accessManager.setTargetFunctionRole(
            ics26, IBCSelectorLib.ics26IdCustomizerSelectors(), IBCRolesLib.ID_CUSTOMIZER_ROLE
        );
        accessManager.setTargetFunctionRole(ics26, IBCSelectorLib.ics26RelayerSelectors(), IBCRolesLib.RELAYER_ROLE);
        accessManager.setTargetFunctionRole(ics26, IBCSelectorLib.pauserSelectors(), IBCRolesLib.PAUSER_ROLE);
        accessManager.setTargetFunctionRole(ics26, IBCSelectorLib.unpauserSelectors(), IBCRolesLib.UNPAUSER_ROLE);
        accessManager.setTargetFunctionRole(ics20, IBCSelectorLib.pauserSelectors(), IBCRolesLib.PAUSER_ROLE);
        accessManager.setTargetFunctionRole(ics20, IBCSelectorLib.unpauserSelectors(), IBCRolesLib.UNPAUSER_ROLE);
        accessManager.setTargetFunctionRole(
            ics20, IBCSelectorLib.erc20CustomizerSelectors(), IBCRolesLib.ERC20_CUSTOMIZER_ROLE
        );
        accessManager.setTargetFunctionRole(
            ics20, IBCSelectorLib.delegateSenderSelectors(), IBCRolesLib.DELEGATE_SENDER_ROLE
        );
        accessManager.setTargetFunctionRole(
            ics26, IBCSelectorLib.ics26MisbehaviourSelectors(), IBCRolesLib.MISBEHAVIOUR_SUBMITTER_ROLE
        );

        // Add admin role for upgradeable contracts
        // This is actually a no-op since if no role is set, the admin role is assumed
        accessManager.setTargetFunctionRole(ics20, IBCSelectorLib.beaconUpgradeSelectors(), IBCRolesLib.ADMIN_ROLE);
        accessManager.setTargetFunctionRole(ics20, IBCSelectorLib.uupsUpgradeSelectors(), IBCRolesLib.ADMIN_ROLE);
        accessManager.setTargetFunctionRole(ics26, IBCSelectorLib.uupsUpgradeSelectors(), IBCRolesLib.ADMIN_ROLE);

        if (pubRelay) {
            accessManager.setTargetFunctionRole(ics26, IBCSelectorLib.ics26RelayerSelectors(), IBCRolesLib.PUBLIC_ROLE);
        }
    }

    /// @notice Applies the production-only delayed role to destructive target functions.
    /// @dev Test deployments intentionally retain ADMIN_ROLE for upgrade tests; production
    /// deployments must call this helper before handing the manager to governance.
    function accessManagerSetProductionUpgradeRoles(
        IAccessManager accessManager,
        address ics26,
        address ics20,
        address signatureVerifier
    )
        public
    {
        accessManager.setTargetFunctionRole(ics26, IBCSelectorLib.uupsUpgradeSelectors(), IBCRolesLib.UPGRADER_ROLE);
        accessManager.setTargetFunctionRole(ics20, IBCSelectorLib.upgraderSelectors(), IBCRolesLib.UPGRADER_ROLE);
        bytes4[] memory verifierSelectors = new bytes4[](1);
        verifierSelectors[0] = SignatureVerifier.setBucket.selector;
        accessManager.setTargetFunctionRole(signatureVerifier, verifierSelectors, IBCRolesLib.UPGRADER_ROLE);
    }

    /// @notice Grants a role without coupling production callers to the test helper's fixed role list.
    function accessManagerGrantRole(IAccessManager accessManager, uint64 role, address account, uint32 delay) public {
        require(account != address(0), "zero role account");
        accessManager.grantRole(role, account, delay);
    }

    function accessManagerSetRoles(
        IAccessManager accessManager,
        address[] memory relayers,
        address[] memory pausers,
        address[] memory unpausers,
        address idCustomizer,
        address erc20Customizer,
        address delegateSender
    )
        public
    {
        for (uint256 i = 0; i < relayers.length; ++i) {
            accessManager.grantRole(IBCRolesLib.RELAYER_ROLE, relayers[i], 0);
        }
        for (uint256 i = 0; i < pausers.length; ++i) {
            accessManager.grantRole(IBCRolesLib.PAUSER_ROLE, pausers[i], 0);
        }
        for (uint256 i = 0; i < unpausers.length; ++i) {
            accessManager.grantRole(IBCRolesLib.UNPAUSER_ROLE, unpausers[i], 0);
        }
        if (idCustomizer != address(0)) {
            accessManager.grantRole(IBCRolesLib.ID_CUSTOMIZER_ROLE, idCustomizer, 0);
        }
        if (erc20Customizer != address(0)) {
            accessManager.grantRole(IBCRolesLib.ERC20_CUSTOMIZER_ROLE, erc20Customizer, 0);
        }
        if (delegateSender != address(0)) {
            accessManager.grantRole(IBCRolesLib.DELEGATE_SENDER_ROLE, delegateSender, 0);
        }
    }
}
