// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title IBC Roles
/// @notice Dependency-free role identifiers shared by IBC contracts and deployment tooling.
library IBCRolesLib {
    /// @notice The admin role as per defined by AccessManager.
    uint64 internal constant ADMIN_ROLE = type(uint64).min;

    /// @notice The public role as per defined by AccessManager.
    uint64 internal constant PUBLIC_ROLE = type(uint64).max;

    /// @notice Only addresses with this role may relay packets.
    uint64 internal constant RELAYER_ROLE = 1;

    /// @notice The pauser role can pause IBC contracts.
    uint64 internal constant PAUSER_ROLE = 2;

    /// @notice The unpauser role can unpause IBC contracts.
    uint64 internal constant UNPAUSER_ROLE = 3;

    /// @notice Has permission to call `ICS20Transfer.sendTransferWithSender`.
    uint64 internal constant DELEGATE_SENDER_ROLE = 4;

    /// @notice Can set withdrawal rate limits per ERC20 token.
    uint64 internal constant RATE_LIMITER_ROLE = 5;

    /// @notice Can set custom port ids and client ids in ICS26Router.
    uint64 internal constant ID_CUSTOMIZER_ROLE = 6;

    /// @notice Can set custom ERC20 contracts for IBC denoms in ICS20Transfer.
    uint64 internal constant ERC20_CUSTOMIZER_ROLE = 7;

    /// @notice Only addresses with this role may submit misbehaviour reports.
    uint64 internal constant MISBEHAVIOUR_SUBMITTER_ROLE = 8;

    /// @notice Can execute delayed implementation and verifier configuration changes.
    uint64 internal constant UPGRADER_ROLE = 9;

    /// @notice Returns the AccessManager role id for the per-clientId light client migrator.
    /// @dev The id is derived from the clientId so that a single role grant only authorizes
    /// @dev migration of that specific client. The admin (ADMIN_ROLE) of every per-clientId
    /// @dev role is the global ADMIN_ROLE.
    /// @param clientId The client identifier
    /// @return The AccessManager role id that authorizes migration of `clientId`
    function getLightClientMigratorRole(string calldata clientId) internal pure returns (uint64) {
        return uint64(uint256(keccak256(abi.encodePacked("LIGHT_CLIENT_MIGRATOR_ROLE_", clientId))));
    }
}
