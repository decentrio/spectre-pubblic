// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/*
    Real deploy of the L2-side receive infrastructure for the Cosmos->L2 (ZK
    "groth16") flow, on an L2 rollup's exec RPC.

    A near-twin of E2ETestDeploy (the L1 counterpart): it deploys ICS26Router +
    ICS20Transfer + the SpectreClient light-client modules and per-bucket Groth16
    verifiers, and — like E2ETestDeploy — DEFERS the SpectreClient itself to the
    relayer's create-clients-eth, which queries a fresh Cosmos genesis, deploys the
    SpectreClient wired to the modules below, and addClient()s it into the router.
    Keeping the genesis in the relayer (one source, fresh at create time) is why
    this script takes no SPECTRE_* bootstrap. Differences from E2ETestDeploy: no
    ERC20 faucet, a configurable PERMIT2, and an optional deployment JSON dump.
*/

// solhint-disable custom-errors,gas-custom-errors

import { stdJson } from "forge-std/StdJson.sol";
import { Script } from "forge-std/Script.sol";

import { ICS26Router } from "contracts/core/ICS26Router.sol";
import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { DeployAccessManagerWithRoles } from "scripts/deployments/DeployAccessManagerWithRoles.sol";
import { IBCERC20 } from "contracts/apps/ics20/IBCERC20.sol";
import { Escrow } from "contracts/apps/ics20/Escrow.sol";
import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";

import { Groth16Verifier_N4 } from "contracts/verifiers/Groth16Verifier_N4.sol";

import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";
import { UpdateClient } from "contracts/light-clients/spectre/modules/UpdateClient.sol";
import { Misbehaviour } from "contracts/light-clients/spectre/modules/Misbehaviour.sol";
import { ClientMigrationProposer } from "contracts/core/client-registry/migration/modules/ClientMigrationProposer.sol";
import { ClientMigrationExecutor } from "contracts/core/client-registry/migration/modules/ClientMigrationExecutor.sol";
import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";

/// @dev Cosmos->L2 receive-infrastructure deploy. See E2ETestDeploy for the L1 counterpart.
contract E2ETestDeployL2 is Script, DeployAccessManagerWithRoles {
    using stdJson for string;

    function run() public returns (string memory) {
        // ============ Step 1: Load parameters ==============
        address permit2 = vm.envOr("PERMIT2", address(0));
        string memory outputPath = vm.envOr("E2E_L2_DEPLOYMENT_OUT", string(""));

        // ============ Step 2: Deploy the contracts ==============

        vm.startBroadcast();

        // Deploy the multi-validator Groth16 batch verifier wrapper, then
        // deploy one per-bucket gnark verifier and register it. Per-bucket
        // Groth16Verifier_N{N}.sol files are generated offline by
        // `go run ./relayer/prover/cmd`.
        SignatureVerifier signatureVerifier = new SignatureVerifier(msg.sender);

        address verifierN4 = address(new Groth16Verifier_N4());

        // Hash-aggregate exposes the fixed 32-byte SHA-256 digest as two
        // 128-bit field elements, so every bucket uses the same uint256[2]
        // verifier ABI.

        signatureVerifier.setBucket(4, verifierN4, Groth16Verifier_N4.verifyProof.selector);

        address membership = address(new Membership());
        address updateClient = address(new UpdateClient(address(signatureVerifier)));
        address misbehaviour = address(new Misbehaviour(address(signatureVerifier)));

        // Deploy IBC v2 with proxy
        address ics26RouterLogic =
            address(new ICS26Router(address(new ClientMigrationProposer()), address(new ClientMigrationExecutor())));
        address ics20TransferLogic = address(new ICS20Transfer());

        AccessManager accessManager = new AccessManager(msg.sender);

        ERC1967Proxy routerProxy =
            new ERC1967Proxy(ics26RouterLogic, abi.encodeCall(ICS26Router.initialize, (address(accessManager))));

        ERC1967Proxy transferProxy = new ERC1967Proxy(
            ics20TransferLogic,
            abi.encodeCall(
                ICS20Transfer.initialize,
                (address(routerProxy), address(new Escrow()), address(new IBCERC20()), permit2, address(accessManager))
            )
        );

        // Wire up the IBCAdmin and access control using the relayer roles.
        accessManagerSetTargetRoles(accessManager, address(routerProxy), address(transferProxy), false);

        address[] memory relayers = new address[](1);
        relayers[0] = msg.sender;
        accessManagerSetRoles(
            accessManager, relayers, new address[](0), new address[](0), msg.sender, msg.sender, msg.sender
        );

        // Wire Transfer app. The Cosmos client (SpectreClient) is added later by the
        // relayer's create-clients-eth once it is deployed with a fresh Cosmos genesis.
        ICS26Router(address(routerProxy)).addIBCApp(ICS20Lib.DEFAULT_PORT_ID, address(transferProxy));

        vm.stopBroadcast();

        // Address handoff — the relayer's create-clients-eth reads these to deploy the
        // SpectreClient wired to the modules, then writes spectre_client into config.
        string memory json = "json";
        json.serialize("signatureVerifier", Strings.toHexString(address(signatureVerifier)));
        json.serialize("membership", Strings.toHexString(membership));
        json.serialize("updateClient", Strings.toHexString(updateClient));
        json.serialize("misbehaviour", Strings.toHexString(misbehaviour));
        json.serialize("ics26Router", Strings.toHexString(address(routerProxy)));
        string memory finalJson = json.serialize("ics20Transfer", Strings.toHexString(address(transferProxy)));

        if (bytes(outputPath).length != 0) {
            vm.writeFile(outputPath, finalJson);
        }

        return finalJson;
    }
}
