// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

/*
    This script is used for end-to-end testing
*/

// solhint-disable custom-errors,gas-custom-errors

import { stdJson } from "forge-std/StdJson.sol";
import { Script } from "forge-std/Script.sol";

import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { ICS26Router } from "contracts/core/ICS26Router.sol";
import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { TestERC20 } from "test/mocks/TestERC20.sol";
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

/// @dev See the Solidity Scripting tutorial: https://book.getfoundry.sh/tutorials/solidity-scripting
contract E2ETestDeploy is Script, ISpectreTendermintMsgs, DeployAccessManagerWithRoles {
    using stdJson for string;

    string internal constant GENESIS_DIR = "/scripts/";

    function run() public returns (string memory) {
        // ============ Step 1: Load parameters ==============
        address e2eFaucet = vm.envAddress("E2E_FAUCET_ADDRESS");

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
        address clientMigrationProposer = address(new ClientMigrationProposer());
        address clientMigrationExecutor = address(new ClientMigrationExecutor());
        // address verifierMock = address(new MockGroth16Verifier());

        // Deploy IBC v2 with proxy
        address ics26RouterLogic = address(new ICS26Router(clientMigrationProposer, clientMigrationExecutor));
        address ics20TransferLogic = address(new ICS20Transfer());

        AccessManager accessManager = new AccessManager(msg.sender);

        ERC1967Proxy routerProxy =
            new ERC1967Proxy(ics26RouterLogic, abi.encodeCall(ICS26Router.initialize, (address(accessManager))));

        ERC1967Proxy transferProxy = new ERC1967Proxy(
            ics20TransferLogic,
            abi.encodeCall(
                ICS20Transfer.initialize,
                (
                    address(routerProxy),
                    address(new Escrow()),
                    address(new IBCERC20()),
                    address(0),
                    address(accessManager)
                )
            )
        );

        // Wire up the IBCAdmin and access control using the relayer roles.
        accessManagerSetTargetRoles(accessManager, address(routerProxy), address(transferProxy), false);

        address[] memory relayers = new address[](1);
        relayers[0] = msg.sender;
        accessManagerSetRoles(
            accessManager, relayers, new address[](0), new address[](0), msg.sender, msg.sender, msg.sender
        );

        // Wire Transfer app
        ICS26Router(address(routerProxy)).addIBCApp(ICS20Lib.DEFAULT_PORT_ID, address(transferProxy));

        // Mint some tokens
        TestERC20 erc20 = new TestERC20();
        erc20.mint(e2eFaucet, 1_000_000 * 10 ** 18);

        vm.stopBroadcast();

        string memory json = "json";
        json.serialize("signatureVerifier", Strings.toHexString(address(signatureVerifier)));
        json.serialize("membership", Strings.toHexString(address(membership)));
        json.serialize("updateClient", Strings.toHexString(address(updateClient)));
        json.serialize("misbehaviour", Strings.toHexString(address(misbehaviour)));
        json.serialize("clientMigrationProposer", Strings.toHexString(clientMigrationProposer));
        json.serialize("clientMigrationExecutor", Strings.toHexString(clientMigrationExecutor));
        json.serialize("ics26Router", Strings.toHexString(address(routerProxy)));
        json.serialize("ics20Transfer", Strings.toHexString(address(transferProxy)));
        string memory finalJson = json.serialize("erc20", Strings.toHexString(address(erc20)));

        return finalJson;
    }
}
