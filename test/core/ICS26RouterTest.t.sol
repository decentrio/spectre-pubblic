// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

// solhint-disable custom-errors,max-line-length

import { Test } from "forge-std/Test.sol";

import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";
import { IIBCAppCallbacks } from "contracts/core/messages/IIBCAppCallbacks.sol";
import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";

import { IICS26RouterErrors } from "contracts/core/errors/IICS26RouterErrors.sol";
import { IICS24HostErrors } from "contracts/core/errors/IICS24HostErrors.sol";
import { IICS26Router } from "contracts/core/interfaces/IICS26Router.sol";
import { ILightClient } from "contracts/light-clients/interfaces/ILightClient.sol";
import { IAccessManaged } from "@openzeppelin-contracts/access/manager/IAccessManaged.sol";
import { IIBCApp } from "contracts/core/interfaces/IIBCApp.sol";

import { ICS26Router } from "contracts/core/ICS26Router.sol";
import { ISpectreTendermintMsgs } from "contracts/light-clients/spectre/messages/ISpectreTendermintMsgs.sol";
import { IMembershipMsgs } from "contracts/light-clients/spectre/messages/IMembershipMsgs.sol";
import { ICS20Lib } from "contracts/apps/ics20/libraries/ICS20Lib.sol";
import { ICS24Host } from "contracts/core/libraries/ICS24Host.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { ERC1967Proxy } from "@openzeppelin-contracts/proxy/ERC1967/ERC1967Proxy.sol";
import { PausableUpgradeable } from "@openzeppelin-upgradeable/utils/PausableUpgradeable.sol";
import { TestHelper } from "test/utils/TestHelper.sol";
import { AccessManager } from "@openzeppelin-contracts/access/manager/AccessManager.sol";
import { IBCRolesLib } from "contracts/shared/access/IBCRolesLib.sol";
import { IBCSelectorLib } from "scripts/deployments/IBCSelectorLib.sol";
import { ClientMigrationProposer } from "contracts/core/client-registry/migration/modules/ClientMigrationProposer.sol";
import { ClientMigrationExecutor } from "contracts/core/client-registry/migration/modules/ClientMigrationExecutor.sol";

contract ICS26RouterTest is Test {
    ICS26Router public ics26Router;

    TestHelper public testHelper = new TestHelper();

    address public relayer = makeAddr("relayer");
    address public idCustomizer = makeAddr("idCustomizer");
    address public pauser = makeAddr("pauser");
    address public unpauser = makeAddr("unpauser");
    address public mockClient = makeAddr("mockClient");

    function setUp() public {
        ICS26Router ics26RouterLogic =
            new ICS26Router(address(new ClientMigrationProposer()), address(new ClientMigrationExecutor()));

        AccessManager accessManager = new AccessManager(address(this));

        ERC1967Proxy routerProxy = new ERC1967Proxy(
            address(ics26RouterLogic), abi.encodeCall(ICS26Router.initialize, (address(accessManager)))
        );

        ics26Router = ICS26Router(address(routerProxy));

        accessManager.setTargetFunctionRole(
            address(ics26Router), IBCSelectorLib.ics26RelayerSelectors(), IBCRolesLib.RELAYER_ROLE
        );
        accessManager.setTargetFunctionRole(
            address(ics26Router), IBCSelectorLib.ics26IdCustomizerSelectors(), IBCRolesLib.ID_CUSTOMIZER_ROLE
        );
        accessManager.setTargetFunctionRole(
            address(ics26Router), IBCSelectorLib.pauserSelectors(), IBCRolesLib.PAUSER_ROLE
        );
        accessManager.setTargetFunctionRole(
            address(ics26Router), IBCSelectorLib.unpauserSelectors(), IBCRolesLib.UNPAUSER_ROLE
        );

        accessManager.grantRole(IBCRolesLib.RELAYER_ROLE, relayer, 0);
        accessManager.grantRole(IBCRolesLib.ID_CUSTOMIZER_ROLE, idCustomizer, 0);
        accessManager.grantRole(IBCRolesLib.PAUSER_ROLE, pauser, 0);
        accessManager.grantRole(IBCRolesLib.UNPAUSER_ROLE, unpauser, 0);

        // `addClient` rejects an address without code; the calls to the client are mocked below
        vm.etch(mockClient, hex"00");
        ics26Router.addClient(
            IICS02ClientMsgs.CounterpartyInfo("42-dummy-01", testHelper.COSMOS_MERKLE_PREFIX()), mockClient
        );
    }

    function dummyMembershipMsg() internal pure returns (bytes memory) {
        return abi.encode(
            ILightClientMsgs.MsgVerifyMembership({
                height: IICS02ClientMsgs.Height({ revisionNumber: 0, revisionHeight: 0 }),
                kvPairs: new IMembershipMsgs.KVPair[](0),
                merkleProofs: new IMembershipMsgs.MerkleProof[](0),
                appHash: bytes32(0),
                trustedConsensusState: ISpectreTendermintMsgs.ConsensusState({
                    timestamp: 0, root: bytes32(0), nextValidatorsHash: bytes32(0)
                }),
                membershipType: IMembershipMsgs.MembershipType.Membership,
                path: new bytes[](0),
                value: bytes("")
            })
        );
    }

    function test_success_addIBCAppUsingAddress() public {
        address mockApp = makeAddr("mockApp");
        string memory mockAppStr = Strings.toHexString(mockApp);

        vm.expectEmit();
        emit IICS26Router.IBCAppAdded(mockAppStr, mockApp);
        ics26Router.addIBCApp(mockApp);

        assertEq(mockApp, address(ics26Router.getIBCApp(mockAppStr)));
    }

    function test_success_pauseBlocksSendAndRecvPacket() public {
        vm.prank(pauser);
        ics26Router.pause();
        assert(ics26Router.paused());

        IICS26RouterMsgs.MsgSendPacket memory msgSendPacket;
        vm.expectRevert(abi.encodeWithSelector(PausableUpgradeable.EnforcedPause.selector));
        ics26Router.sendPacket(msgSendPacket);

        IICS26RouterMsgs.MsgRecvPacket memory msgRecvPacket;
        vm.expectRevert(abi.encodeWithSelector(PausableUpgradeable.EnforcedPause.selector));
        vm.prank(relayer);
        ics26Router.recvPacket(msgRecvPacket);

        vm.prank(unpauser);
        ics26Router.unpause();
        assert(!ics26Router.paused());
    }

    function test_success_addIBCAppUsingNamedPort() public {
        address mockApp = makeAddr("mockApp");

        vm.expectEmit();
        emit IICS26Router.IBCAppAdded(ICS20Lib.DEFAULT_PORT_ID, mockApp);
        vm.prank(idCustomizer);
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, mockApp);

        assertEq(mockApp, address(ics26Router.getIBCApp(ICS20Lib.DEFAULT_PORT_ID)));
    }

    function test_failure_addIBCAppUsingNamedPort() public {
        address mockApp = makeAddr("mockApp");
        // port is an address
        string memory mockAppStr = Strings.toHexString(mockApp);
        vm.prank(idCustomizer);
        vm.expectRevert(abi.encodeWithSelector(IICS26RouterErrors.IBCInvalidPortIdentifier.selector, mockAppStr));
        ics26Router.addIBCApp(mockAppStr, mockApp);

        // unauthorized
        address unauthorized = makeAddr("unauthorized");
        vm.prank(unauthorized);
        vm.expectRevert(abi.encodeWithSelector(IAccessManaged.AccessManagedUnauthorized.selector, unauthorized));
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, mockApp);

        // reuse of the same port
        vm.prank(idCustomizer);
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, mockApp);
        vm.prank(idCustomizer);
        vm.expectRevert(
            abi.encodeWithSelector(IICS26RouterErrors.IBCPortAlreadyExists.selector, ICS20Lib.DEFAULT_PORT_ID)
        );
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, mockApp);
    }

    function test_unauthorizedSender() public {
        address mockApp = makeAddr("mockApp");
        vm.prank(idCustomizer);
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, mockApp);

        address unauthorizedSender = makeAddr("unauthorizedSender");

        IICS26RouterMsgs.MsgSendPacket memory msgSendPacket;
        msgSendPacket.payload.sourcePort = ICS20Lib.DEFAULT_PORT_ID;

        vm.prank(unauthorizedSender);
        vm.expectRevert(abi.encodeWithSelector(IICS26RouterErrors.IBCUnauthorizedSender.selector, unauthorizedSender));
        ics26Router.sendPacket(msgSendPacket);
    }

    function test_timeoutCeiling() public {
        address mockApp = makeAddr("mockApp");
        string memory mockPort = "mockport";

        vm.prank(idCustomizer);
        ics26Router.addIBCApp(mockPort, mockApp);

        string memory clientId = testHelper.FIRST_CLIENT_ID();

        vm.prank(mockApp);
        uint64 sequence = ics26Router.sendPacket(
            IICS26RouterMsgs.MsgSendPacket({
                sourceClient: clientId,
                timeoutTimestamp: uint64(block.timestamp + 1 days),
                payload: IICS26RouterMsgs.Payload({
                    sourcePort: mockPort, destPort: mockPort, version: "", encoding: "", value: "0x"
                })
            })
        );
        assertEq(sequence, 1);

        uint64 timeoutTimestamp = uint64(block.timestamp + 1 days + 1);
        vm.expectRevert(
            abi.encodeWithSelector(IICS26RouterErrors.IBCInvalidTimeoutDuration.selector, 1 days, 1 days + 1)
        );
        vm.prank(mockApp);
        ics26Router.sendPacket(
            IICS26RouterMsgs.MsgSendPacket({
                sourceClient: clientId,
                timeoutTimestamp: timeoutTimestamp,
                payload: IICS26RouterMsgs.Payload({
                    sourcePort: mockPort, destPort: mockPort, version: "", encoding: "", value: "0x"
                })
            })
        );
    }

    function test_RecvPacketWithFailedMembershipVerification() public {
        string memory counterpartyID = "42-dummy-01";
        bytes memory errorMsg = "Membership verification failed";

        vm.mockCallRevert(mockClient, ILightClient.verifyMembership.selector, errorMsg);

        address mockIcs20 = makeAddr("mockIcs20");
        vm.prank(idCustomizer);
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, address(mockIcs20));

        IICS26RouterMsgs.Payload[] memory payloads = new IICS26RouterMsgs.Payload[](1);
        payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: "0x"
        });
        IICS26RouterMsgs.Packet memory packet = IICS26RouterMsgs.Packet({
            sequence: 1,
            sourceClient: counterpartyID,
            destClient: testHelper.FIRST_CLIENT_ID(),
            timeoutTimestamp: uint64(block.timestamp + 1000),
            payloads: payloads
        });

        IICS26RouterMsgs.MsgRecvPacket memory msgRecvPacket =
            IICS26RouterMsgs.MsgRecvPacket({ packet: packet, membershipMsg: dummyMembershipMsg() });

        vm.expectRevert(errorMsg);
        vm.prank(relayer);
        ics26Router.recvPacket(msgRecvPacket);
    }

    function test_RecvPacketWithErrorAck() public {
        string memory counterpartyID = "42-dummy-01";

        vm.mockCall(mockClient, ILightClient.verifyMembership.selector, abi.encode(true));

        // We add an unusable ICS20Transfer app to the router
        address mockIcs20 = makeAddr("mockIcs20");
        vm.prank(idCustomizer);
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, mockIcs20);

        vm.mockCallRevert(mockIcs20, IIBCApp.onRecvPacket.selector, bytes("mockErr"));

        IICS26RouterMsgs.Payload[] memory payloads = new IICS26RouterMsgs.Payload[](1);
        payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: "0x"
        });
        IICS26RouterMsgs.Packet memory packet = IICS26RouterMsgs.Packet({
            sequence: 1,
            sourceClient: counterpartyID,
            destClient: testHelper.FIRST_CLIENT_ID(),
            timeoutTimestamp: uint64(block.timestamp + 1000),
            payloads: payloads
        });

        IICS26RouterMsgs.MsgRecvPacket memory msgRecvPacket =
            IICS26RouterMsgs.MsgRecvPacket({ packet: packet, membershipMsg: dummyMembershipMsg() });

        bytes[] memory expAcks = new bytes[](1);
        expAcks[0] = ICS24Host.UNIVERSAL_ERROR_ACK;

        vm.expectEmit();
        emit IICS26Router.WriteAcknowledgement(packet.destClient, packet.sequence, packet, expAcks);
        vm.prank(relayer);
        ics26Router.recvPacket(msgRecvPacket);
    }

    function test_RecvPacketWithOOG() public {
        string memory counterpartyID = "42-dummy-01";

        vm.mockCall(
            mockClient,
            ILightClient.verifyMembership.selector,
            abi.encode(true) // simulate successful membership verification
        );

        // We add a mock application that will run out of gas
        MockApplication mockApp = new MockApplication();
        vm.prank(idCustomizer);
        ics26Router.addIBCApp(ICS20Lib.DEFAULT_PORT_ID, address(mockApp));

        IICS26RouterMsgs.Payload[] memory payloads = new IICS26RouterMsgs.Payload[](1);
        payloads[0] = IICS26RouterMsgs.Payload({
            sourcePort: ICS20Lib.DEFAULT_PORT_ID,
            destPort: ICS20Lib.DEFAULT_PORT_ID,
            version: ICS20Lib.ICS20_VERSION,
            encoding: ICS20Lib.ICS20_ENCODING,
            value: "0x"
        });
        IICS26RouterMsgs.Packet memory packet = IICS26RouterMsgs.Packet({
            sequence: 1,
            sourceClient: counterpartyID,
            destClient: testHelper.FIRST_CLIENT_ID(),
            timeoutTimestamp: uint64(block.timestamp + 1000),
            payloads: payloads
        });

        IICS26RouterMsgs.MsgRecvPacket memory msgRecvPacket =
            IICS26RouterMsgs.MsgRecvPacket({ packet: packet, membershipMsg: dummyMembershipMsg() });

        vm.expectRevert(abi.encodeWithSelector(IICS26RouterErrors.IBCFailedCallback.selector));
        vm.prank(relayer);
        ics26Router.recvPacket{ gas: 900_000 }(msgRecvPacket);
    }

    function test_failure_multiPayloadPackets() public {
        IICS26RouterMsgs.Payload[] memory payloads = new IICS26RouterMsgs.Payload[](2);
        IICS26RouterMsgs.Packet memory packet = IICS26RouterMsgs.Packet({
            sequence: 1,
            sourceClient: "source-client",
            destClient: "destination-client",
            timeoutTimestamp: uint64(block.timestamp + 1000),
            payloads: payloads
        });

        vm.expectRevert(IICS24HostErrors.IBCMultiPayloadPacketNotSupported.selector);
        vm.prank(relayer);
        ics26Router.recvPacket(IICS26RouterMsgs.MsgRecvPacket({ packet: packet, membershipMsg: bytes("") }));

        vm.expectRevert(IICS24HostErrors.IBCMultiPayloadPacketNotSupported.selector);
        vm.prank(relayer);
        ics26Router.ackPacket(
            IICS26RouterMsgs.MsgAckPacket({ packet: packet, acknowledgement: bytes(""), membershipMsg: bytes("") })
        );

        vm.expectRevert(IICS24HostErrors.IBCMultiPayloadPacketNotSupported.selector);
        vm.prank(relayer);
        ics26Router.timeoutPacket(IICS26RouterMsgs.MsgTimeoutPacket({ packet: packet, nonMembershipMsg: bytes("") }));
    }

    function test_failure_counterpartyMismatch() public {
        string memory otherClient = "not-the-registered-counterparty";
        IICS26RouterMsgs.Packet memory packet = IICS26RouterMsgs.Packet({
            sequence: 1,
            sourceClient: otherClient,
            destClient: testHelper.FIRST_CLIENT_ID(),
            timeoutTimestamp: uint64(block.timestamp + 1000),
            payloads: new IICS26RouterMsgs.Payload[](1)
        });

        // An inbound packet names its sender's client, which must be the counterparty of the receiving client
        vm.expectRevert(
            abi.encodeWithSelector(IICS26RouterErrors.IBCInvalidCounterparty.selector, "42-dummy-01", otherClient)
        );
        vm.prank(relayer);
        ics26Router.recvPacket(IICS26RouterMsgs.MsgRecvPacket({ packet: packet, membershipMsg: bytes("") }));

        // An ack or timeout names the destination client, which must be the counterparty of the sending client
        packet.sourceClient = testHelper.FIRST_CLIENT_ID();
        packet.destClient = otherClient;

        vm.expectRevert(
            abi.encodeWithSelector(IICS26RouterErrors.IBCInvalidCounterparty.selector, "42-dummy-01", otherClient)
        );
        vm.prank(relayer);
        ics26Router.ackPacket(
            IICS26RouterMsgs.MsgAckPacket({ packet: packet, acknowledgement: bytes(""), membershipMsg: bytes("") })
        );

        vm.expectRevert(
            abi.encodeWithSelector(IICS26RouterErrors.IBCInvalidCounterparty.selector, "42-dummy-01", otherClient)
        );
        vm.prank(relayer);
        ics26Router.timeoutPacket(IICS26RouterMsgs.MsgTimeoutPacket({ packet: packet, nonMembershipMsg: bytes("") }));
    }
}

contract MockApplication is Test {
    function onRecvPacket(IIBCAppCallbacks.OnRecvPacketCallback calldata) external pure returns (bytes memory) {
        for (uint256 i = 0; i < 14_000; ++i) {
            uint256 x;
            x = x * i;
        }

        return bytes("mock");
    }
}
