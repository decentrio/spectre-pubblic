// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/// @title Production Config Library
/// @notice Shared preconditions for the production deployment and verification scripts.
/// @dev Lives in one place so `ProductionDeploy` and `ProductionVerify` cannot drift apart: a
///      config the deploy script rejects must also be one the verify script rejects, or a
///      deployment made before the check existed would still pass verification.
library ProductionConfigLib {
    /// @notice Reverts unless every address in the list is distinct.
    /// @dev O(n^2), but the lists are small and this runs once per deployment.
    /// @param addrs The addresses that must not collide.
    /// @param message The revert reason, naming which set collided.
    function requireDistinct(address[] memory addrs, string memory message) internal pure {
        for (uint256 i = 0; i < addrs.length; ++i) {
            for (uint256 j = 0; j < i; ++j) {
                require(addrs[i] != addrs[j], message);
            }
        }
    }

    /// @notice Collects the six production bucket verifier addresses in bucket order.
    /// @dev They must be six different contracts: each is generated from its own bucket's
    ///      verifying key, so pointing two buckets at one address is a wrong configuration,
    ///      not a harmless duplicate: every proof for the misconfigured bucket becomes
    ///      unverifiable. Nothing else catches this plausible copy-paste error because each
    ///      address can still be well-formed, contain code, and expose the expected selector.
    function verifierList(
        address n4,
        address n8,
        address n16,
        address n32,
        address n64,
        address n128
    )
        internal
        pure
        returns (address[] memory verifiers)
    {
        verifiers = new address[](6);
        verifiers[0] = n4;
        verifiers[1] = n8;
        verifiers[2] = n16;
        verifiers[3] = n32;
        verifiers[4] = n64;
        verifiers[5] = n128;
    }

    /// @notice Collects every privileged production principal in one canonical order.
    function principalList(
        address bootstrap,
        address governance,
        address upgrader,
        address relayer,
        address pauser1,
        address pauser2,
        address unpauser,
        address watcher,
        address rateLimiter
    )
        internal
        pure
        returns (address[] memory principals)
    {
        principals = new address[](9);
        principals[0] = bootstrap;
        principals[1] = governance;
        principals[2] = upgrader;
        principals[3] = relayer;
        principals[4] = pauser1;
        principals[5] = pauser2;
        principals[6] = unpauser;
        principals[7] = watcher;
        principals[8] = rateLimiter;
    }
}
