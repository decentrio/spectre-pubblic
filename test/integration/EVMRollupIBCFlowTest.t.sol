// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { Test, Vm } from "forge-std/Test.sol";

import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { IERC20 } from "@openzeppelin-contracts/token/ERC20/IERC20.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";

import { ICS20Transfer } from "contracts/apps/ics20/ICS20Transfer.sol";
import { ICS26Router } from "contracts/core/ICS26Router.sol";
import { SignatureVerifier } from "contracts/light-clients/spectre/SignatureVerifier.sol";
import { SpectreClient } from "contracts/light-clients/spectre/SpectreClient.sol";
import { Membership } from "contracts/light-clients/spectre/modules/Membership.sol";
import { Misbehaviour } from "contracts/light-clients/spectre/modules/Misbehaviour.sol";
import { UpdateClient } from "contracts/light-clients/spectre/modules/UpdateClient.sol";
import { ClientMigrationProposer } from "contracts/core/client-registry/migration/modules/ClientMigrationProposer.sol";
import { ClientMigrationExecutor } from "contracts/core/client-registry/migration/modules/ClientMigrationExecutor.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IICS20TransferMsgs } from "contracts/apps/ics20/messages/IICS20TransferMsgs.sol";
import { IICS26Router } from "contracts/core/interfaces/IICS26Router.sol";
import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";
import { Groth16Verifier_N4 } from "contracts/verifiers/Groth16Verifier_N4.sol";
import { IBCERC20 } from "contracts/apps/ics20/IBCERC20.sol";
import { Escrow } from "contracts/apps/ics20/Escrow.sol";
import { Header } from "contracts/light-clients/spectre/libraries/Header.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { ICS24Host } from "contracts/core/libraries/ICS24Host.sol";
import { RelayerHelper } from "contracts/periphery/relayer/RelayerHelper.sol";
import { DeployAccessManagerWithRoles } from "scripts/deployments/DeployAccessManagerWithRoles.sol";
import { DummyLightClient } from "test/mocks/DummyLightClient.sol";
import { TestERC20 } from "test/mocks/TestERC20.sol";

contract EVMRollupIBCFlowTest is Test, DeployAccessManagerWithRoles {
    uint256 internal constant REPRESENTATIVE_EVM_ROLLUP_CHAIN_ID = 42_161;
    uint256 internal constant SEND_AMOUNT = 1 ether;
    string internal constant SOURCE_CLIENT_ID = "evm-rollup-cosmos-0";
    string internal constant COUNTERPARTY_CLIENT_ID = "cosmos-evm-rollup-0";
    bytes32 internal constant PROOF_SUBMITTER_ROLE = keccak256("PROOF_SUBMITTER_ROLE");
    bytes32 internal constant DEFAULT_ADMIN_ROLE = 0x00;

    struct CoreDeployment {
        AccessManager accessManager;
        ICS26Router router;
        ICS20Transfer transfer;
        RelayerHelper helper;
    }

    function test_deploySendTransferAndStorePacketCommitmentOnEVMRollup() public {
        vm.chainId(REPRESENTATIVE_EVM_ROLLUP_CHAIN_ID);

        AccessManager accessManager = new AccessManager(address(this));
        ICS26Router routerLogic =
            new ICS26Router(address(new ClientMigrationProposer()), address(new ClientMigrationExecutor()));
        ERC1967Proxy routerProxy =
            new ERC1967Proxy(address(routerLogic), abi.encodeCall(ICS26Router.initialize, (address(accessManager))));

        ICS20Transfer transferLogic = new ICS20Transfer();
        ERC1967Proxy transferProxy = new ERC1967Proxy(
            address(transferLogic),
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

        ICS26Router router = ICS26Router(address(routerProxy));
        ICS20Transfer transfer = ICS20Transfer(address(transferProxy));

        accessManagerSetTargetRoles(accessManager, address(router), address(transfer), false);

        address[] memory relayers = new address[](1);
        relayers[0] = address(this);
        accessManagerSetRoles(
            accessManager, relayers, new address[](0), new address[](0), address(this), address(this), address(this)
        );

        bytes[] memory cosmosMerklePrefix = new bytes[](2);
        cosmosMerklePrefix[0] = bytes("ibc");
        cosmosMerklePrefix[1] = bytes("");

        DummyLightClient lightClient = new DummyLightClient(ILightClientMsgs.UpdateResult.Update, 0, false);
        router.addClient(
            SOURCE_CLIENT_ID,
            IICS02ClientMsgs.CounterpartyInfo({ clientId: COUNTERPARTY_CLIENT_ID, merklePrefix: cosmosMerklePrefix }),
            address(lightClient)
        );
        router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, address(transfer));
        assertEq(address(router.getIBCApp(ICS20Lib.DEFAULT_PORT_ID)), address(transfer));

        TestERC20 token = new TestERC20();
        address user = makeAddr("evmRollupUser");
        string memory cosmosReceiver = "cosmos1receiver000000000000000000000000000000000";
        token.mint(user, SEND_AMOUNT);

        vm.startPrank(user);
        token.approve(address(transfer), SEND_AMOUNT);
        vm.recordLogs();
        uint64 sequence = transfer.sendTransfer(
            IICS20TransferMsgs.SendTransferMsg({
                denom: address(token),
                amount: SEND_AMOUNT,
                receiver: cosmosReceiver,
                sourceClient: SOURCE_CLIENT_ID,
                destPort: ICS20Lib.DEFAULT_PORT_ID,
                timeoutTimestamp: uint64(block.timestamp + 1 hours),
                memo: "evm-rollup-spectre"
            })
        );
        vm.stopPrank();

        assertEq(sequence, 1);

        IICS26RouterMsgs.Packet memory packet = _getPacketFromSendEvent();
        assertEq(packet.sequence, sequence);
        assertEq(packet.sourceClient, SOURCE_CLIENT_ID);
        assertEq(packet.destClient, COUNTERPARTY_CLIENT_ID);
        assertEq(packet.timeoutTimestamp, uint64(block.timestamp + 1 hours));
        assertEq(packet.payloads.length, 1);
        assertEq(packet.payloads[0].sourcePort, ICS20Lib.DEFAULT_PORT_ID);
        assertEq(packet.payloads[0].destPort, ICS20Lib.DEFAULT_PORT_ID);
        assertEq(packet.payloads[0].version, ICS20Lib.ICS20_VERSION);
        assertEq(packet.payloads[0].encoding, ICS20Lib.ICS20_ENCODING);

        IICS20TransferMsgs.FungibleTokenPacketData memory packetData =
            abi.decode(packet.payloads[0].value, (IICS20TransferMsgs.FungibleTokenPacketData));
        assertEq(packetData.denom, Strings.toHexString(address(token)));
        assertEq(packetData.sender, Strings.toHexString(user));
        assertEq(packetData.receiver, cosmosReceiver);
        assertEq(packetData.amount, SEND_AMOUNT);

        RelayerHelper helper = new RelayerHelper(address(router));
        bytes32 storedCommitment = helper.queryPacketCommitment(SOURCE_CLIENT_ID, sequence);
        assertEq(storedCommitment, ICS24Host.packetCommitmentBytes32(packet));
        assertEq(token.balanceOf(transfer.getEscrow(SOURCE_CLIENT_ID)), SEND_AMOUNT);
    }

    function test_ackFromCosmosDeletesPacketCommitmentOnEVMRollup() public {
        vm.chainId(REPRESENTATIVE_EVM_ROLLUP_CHAIN_ID);

        CoreDeployment memory core = _deployCore(false);
        DummyLightClient lightClient = new DummyLightClient(ILightClientMsgs.UpdateResult.Update, 0, false);
        _configureSourceClient(core, address(lightClient));

        TestERC20 token = new TestERC20();
        address user = makeAddr("evmRollupAckUser");
        string memory cosmosReceiver = "cosmos1receiver000000000000000000000000000000000";
        token.mint(user, SEND_AMOUNT);

        vm.startPrank(user);
        token.approve(address(core.transfer), SEND_AMOUNT);
        vm.recordLogs();
        uint64 sequence = core.transfer
            .sendTransfer(
                IICS20TransferMsgs.SendTransferMsg({
                    denom: address(token),
                    amount: SEND_AMOUNT,
                    receiver: cosmosReceiver,
                    sourceClient: SOURCE_CLIENT_ID,
                    destPort: ICS20Lib.DEFAULT_PORT_ID,
                    timeoutTimestamp: uint64(block.timestamp + 1 hours),
                    memo: "evm-rollup-spectre"
                })
            );
        vm.stopPrank();

        IICS26RouterMsgs.Packet memory packet = _getPacketFromSendEvent();
        assertEq(
            core.helper.queryPacketCommitment(SOURCE_CLIENT_ID, sequence), ICS24Host.packetCommitmentBytes32(packet)
        );

        core.router
            .ackPacket(
                IICS26RouterMsgs.MsgAckPacket({
                    packet: packet,
                    acknowledgement: ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON,
                    membershipMsg: _dummyMembershipMsg()
                })
            );

        assertEq(core.helper.queryPacketCommitment(SOURCE_CLIENT_ID, sequence), bytes32(0));
        assertEq(token.balanceOf(user), 0);
        assertEq(token.balanceOf(core.transfer.getEscrow(SOURCE_CLIENT_ID)), SEND_AMOUNT);
    }

    function test_deployReceiveCapableSpectreStackOnEVMRollup() public {
        vm.chainId(REPRESENTATIVE_EVM_ROLLUP_CHAIN_ID);

        CoreDeployment memory core = _deployCore(false);

        SignatureVerifier signatureVerifier = new SignatureVerifier(address(this));
        Groth16Verifier_N4 verifierN4 = new Groth16Verifier_N4();
        signatureVerifier.setBucket(4, address(verifierN4), Groth16Verifier_N4.verifyProof.selector);

        Membership membership = new Membership();
        UpdateClient updateClient = new UpdateClient(address(signatureVerifier));
        Misbehaviour misbehaviour = new Misbehaviour(address(signatureVerifier));
        ISpectreTendermintMsgs.ValidatorSet memory validatorSet = _validatorSet();
        ISpectreTendermintMsgs.ConsensusState memory consensusState = _consensusState(validatorSet);
        ISpectreTendermintMsgs.ClientState memory clientState = _clientState();

        SpectreClient spectreClient = new SpectreClient(
            address(updateClient),
            address(membership),
            address(misbehaviour),
            abi.encode(clientState),
            consensusState,
            validatorSet,
            address(this)
        );
        spectreClient.grantRole(PROOF_SUBMITTER_ROLE, address(core.router));

        (address bucketVerifier, bytes4 bucketSelector) = signatureVerifier.buckets(4);
        bytes32 misbehaviourSubmitterRole = spectreClient.MISBEHAVIOUR_SUBMITTER_ROLE();
        assertEq(bucketVerifier, address(verifierN4));
        assertEq(bucketSelector, Groth16Verifier_N4.verifyProof.selector);
        assertTrue(spectreClient.hasRole(PROOF_SUBMITTER_ROLE, address(core.router)));
        assertTrue(spectreClient.hasRole(DEFAULT_ADMIN_ROLE, address(this)));
        assertTrue(spectreClient.hasRole(misbehaviourSubmitterRole, address(this)));
        assertFalse(spectreClient.hasRole(DEFAULT_ADMIN_ROLE, address(core.router)));
        assertFalse(spectreClient.hasRole(misbehaviourSubmitterRole, address(core.router)));

        _configureSourceClient(core, address(spectreClient));

        assertEq(address(core.router.getClient(SOURCE_CLIENT_ID)), address(spectreClient));
        assertEq(address(core.router.getIBCApp(ICS20Lib.DEFAULT_PORT_ID)), address(core.transfer));

        IICS02ClientMsgs.CounterpartyInfo memory counterparty = core.router.getCounterparty(SOURCE_CLIENT_ID);
        assertEq(counterparty.clientId, COUNTERPARTY_CLIENT_ID);
        assertEq(counterparty.merklePrefix.length, 2);
        assertEq(counterparty.merklePrefix[0], bytes("ibc"));
        assertEq(counterparty.merklePrefix[1], bytes(""));
    }

    function test_receiveCosmosPacketRoutesToICS20WithDummyLightClientOnEVMRollup() public {
        vm.chainId(REPRESENTATIVE_EVM_ROLLUP_CHAIN_ID);

        CoreDeployment memory core = _deployCore(false);
        DummyLightClient lightClient = new DummyLightClient(ILightClientMsgs.UpdateResult.Update, 0, false);
        _configureSourceClient(core, address(lightClient));

        address receiver = makeAddr("evmRollupReceiver");
        uint256 amount = 2 ether;
        IICS26RouterMsgs.Packet memory packet = _cosmosToEVMRollupPacket(receiver, amount);

        core.router.recvPacket(IICS26RouterMsgs.MsgRecvPacket({ packet: packet, membershipMsg: _dummyMembershipMsg() }));

        string memory fullDenomPath = string.concat(ICS20Lib.DEFAULT_PORT_ID, "/", SOURCE_CLIENT_ID, "/uatom");
        address ibcToken = core.transfer.ibcERC20Contract(fullDenomPath);

        assertTrue(ibcToken != address(0));
        assertEq(IERC20(ibcToken).balanceOf(receiver), amount);
        assertEq(
            core.helper.queryPacketReceipt(SOURCE_CLIENT_ID, packet.sequence),
            ICS24Host.packetReceiptCommitmentBytes32(packet)
        );

        bytes[] memory acks = new bytes[](1);
        acks[0] = ICS20Lib.SUCCESSFUL_ACKNOWLEDGEMENT_JSON;
        assertEq(
            core.helper.queryAckCommitment(SOURCE_CLIENT_ID, packet.sequence),
            ICS24Host.packetAcknowledgementCommitmentBytes32(acks)
        );
        assertTrue(core.helper.isPacketReceiveSuccessful(packet));
    }

    function _getPacketFromSendEvent() private returns (IICS26RouterMsgs.Packet memory) {
        Vm.Log[] memory logs = vm.getRecordedLogs();
        for (uint256 i = 0; i < logs.length; ++i) {
            for (uint256 j = 0; j < logs[i].topics.length; ++j) {
                if (logs[i].topics[j] == IICS26Router.SendPacket.selector) {
                    return abi.decode(logs[i].data, (IICS26RouterMsgs.Packet));
                }
            }
        }
        revert("SendPacket event not found");
    }

    function _deployCore(bool pubRelay) private returns (CoreDeployment memory core) {
        AccessManager accessManager = new AccessManager(address(this));
        ICS26Router routerLogic =
            new ICS26Router(address(new ClientMigrationProposer()), address(new ClientMigrationExecutor()));
        ERC1967Proxy routerProxy =
            new ERC1967Proxy(address(routerLogic), abi.encodeCall(ICS26Router.initialize, (address(accessManager))));

        ICS20Transfer transferLogic = new ICS20Transfer();
        ERC1967Proxy transferProxy = new ERC1967Proxy(
            address(transferLogic),
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

        ICS26Router router = ICS26Router(address(routerProxy));
        ICS20Transfer transfer = ICS20Transfer(address(transferProxy));

        accessManagerSetTargetRoles(accessManager, address(router), address(transfer), pubRelay);

        address[] memory relayers = new address[](1);
        relayers[0] = address(this);
        accessManagerSetRoles(
            accessManager, relayers, new address[](0), new address[](0), address(this), address(this), address(this)
        );

        core = CoreDeployment({
            accessManager: accessManager, router: router, transfer: transfer, helper: new RelayerHelper(address(router))
        });
    }

    function _configureSourceClient(CoreDeployment memory core, address lightClient) private {
        bytes[] memory cosmosMerklePrefix = new bytes[](2);
        cosmosMerklePrefix[0] = bytes("ibc");
        cosmosMerklePrefix[1] = bytes("");

        core.router
            .addClient(
                SOURCE_CLIENT_ID,
                IICS02ClientMsgs.CounterpartyInfo({
                    clientId: COUNTERPARTY_CLIENT_ID, merklePrefix: cosmosMerklePrefix
                }),
                lightClient
            );
        core.router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, address(core.transfer));
    }

    function _clientState() private pure returns (ISpectreTendermintMsgs.ClientState memory) {
        return ISpectreTendermintMsgs.ClientState({
            chainId: "cosmoshub-0",
            trustLevel: ISpectreTendermintMsgs.TrustThreshold({ numerator: 1, denominator: 3 }),
            latestHeight: IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: 10 }),
            trustingPeriod: 14 days,
            unbondingPeriod: 21 days,
            isFrozen: false,
            clockDrift: 1800
        });
    }

    function _consensusState(ISpectreTendermintMsgs.ValidatorSet memory validatorSet)
        private
        view
        returns (ISpectreTendermintMsgs.ConsensusState memory)
    {
        return ISpectreTendermintMsgs.ConsensusState({
            timestamp: uint128(block.timestamp) * 1_000_000_000,
            root: bytes32(uint256(0xAAA)),
            nextValidatorsHash: Header.hashValSet(validatorSet)
        });
    }

    function _validatorSet() private pure returns (ISpectreTendermintMsgs.ValidatorSet memory validatorSet) {
        validatorSet.validators = new ISpectreTendermintMsgs.ValidatorInfo[](4);
        validatorSet.validators[0] = _validator(
            hex"0000000000000000000000000000000000000001",
            0x0000000000000000000000000000000000000000000000000000000000000001
        );
        validatorSet.validators[1] = _validator(
            hex"0000000000000000000000000000000000000002",
            0x0000000000000000000000000000000000000000000000000000000000000002
        );
        validatorSet.validators[2] = _validator(
            hex"0000000000000000000000000000000000000003",
            0x0000000000000000000000000000000000000000000000000000000000000003
        );
        validatorSet.validators[3] = _validator(
            hex"0000000000000000000000000000000000000004",
            0x0000000000000000000000000000000000000000000000000000000000000004
        );
        validatorSet.totalVotingPower = 400;
    }

    function _validator(
        bytes memory valAddress,
        bytes32 pubKey
    )
        private
        pure
        returns (ISpectreTendermintMsgs.ValidatorInfo memory)
    {
        return ISpectreTendermintMsgs.ValidatorInfo({
            valAddress: valAddress, pubKey: pubKey, votingPower: 100, proposerPriority: 0
        });
    }

    function _cosmosToEVMRollupPacket(
        address receiver,
        uint256 amount
    )
        private
        view
        returns (IICS26RouterMsgs.Packet memory packet)
    {
        IICS26RouterMsgs.Payload[] memory payloads = new IICS26RouterMsgs.Payload[](1);
        payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: abi.encode(
                IICS20TransferMsgs.FungibleTokenPacketData({
                    denom: "uatom",
                    sender: "cosmos1sender0000000000000000000000000000000000",
                    receiver: Strings.toHexString(receiver),
                    amount: amount,
                    memo: "cosmos-to-evm-rollup"
                })
            )
        });

        packet = IICS26RouterMsgs.Packet({
            sequence: 1,
            sourceClient: COUNTERPARTY_CLIENT_ID,
            destClient: SOURCE_CLIENT_ID,
            timeoutTimestamp: uint64(block.timestamp + 1 hours),
            payloads: payloads
        });
    }

    function _dummyMembershipMsg() private pure returns (bytes memory) {
        ILightClientMsgs.MsgVerifyMembership memory membershipMsg = ILightClientMsgs.MsgVerifyMembership({
            height: IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: 1 }),
            kvPairs: new IMembershipMsgs.KVPair[](0),
            merkleProofs: new IMembershipMsgs.MerkleProof[](0),
            appHash: bytes32(0),
            trustedConsensusState: ISpectreTendermintMsgs.ConsensusState({
                timestamp: 0, root: bytes32(0), nextValidatorsHash: bytes32(0)
            }),
            membershipType: IMembershipMsgs.MembershipType.Membership,
            path: new bytes[](0),
            value: bytes("")
        });

        return abi.encode(membershipMsg);
    }
}
