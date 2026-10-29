// SPDX-License-Identifier: MIT
pragma solidity ^0.8.28;

import { ILightClientMsgs } from "contracts/light-clients/messages/ILightClientMsgs.sol";
import { IICS26RouterMsgs } from "contracts/core/messages/IICS26RouterMsgs.sol";
import { IICS02ClientMsgs } from "contracts/core/messages/IICS02ClientMsgs.sol";
import { IIBCAppCallbacks } from "contracts/core/messages/IIBCAppCallbacks.sol";

import { IICS26RouterErrors } from "contracts/core/errors/IICS26RouterErrors.sol";
import { IIBCApp } from "contracts/core/interfaces/IIBCApp.sol";
import { IICS26Router, IICS26RouterAccessControlled } from "contracts/core/interfaces/IICS26Router.sol";
import { IPausable } from "contracts/core/interfaces/IPausable.sol";

import {
    ReentrancyGuardTransientUpgradeable
} from "@openzeppelin-upgradeable/utils/ReentrancyGuardTransientUpgradeable.sol";
import { PausableUpgradeable } from "@openzeppelin-upgradeable/utils/PausableUpgradeable.sol";
import { IBCStoreUpgradeable } from "contracts/core/store/IBCStoreUpgradeable.sol";
import { Strings } from "@openzeppelin-contracts/utils/Strings.sol";
import { IBCIdentifiers } from "contracts/shared/access/IBCIdentifiers.sol";
import { ICS24Host } from "contracts/core/libraries/ICS24Host.sol";
import { ICS02ClientUpgradeable } from "contracts/core/client-registry/ICS02ClientUpgradeable.sol";
import { MulticallUpgradeable } from "@openzeppelin-upgradeable/utils/MulticallUpgradeable.sol";
import { UUPSUpgradeable } from "@openzeppelin-contracts/proxy/utils/UUPSUpgradeable.sol";
import { ICS26AdminsDeprecated } from "contracts/core/compatibility/ICS26AdminsDeprecated.sol";

/// @title IBC v2 Router
/// @notice The core router for the IBC v2 protocol
contract ICS26Router is
    IICS26RouterErrors,
    IICS26Router,
    ICS02ClientUpgradeable,
    IBCStoreUpgradeable,
    ReentrancyGuardTransientUpgradeable,
    MulticallUpgradeable,
    UUPSUpgradeable,
    PausableUpgradeable
{
    /// @notice Storage of the ICS26Router contract
    /// @dev It's implemented on a custom ERC-7201 namespace to reduce the risk of storage collisions when using with
    /// upgradeable contracts.
    /// @param _apps The mapping of port identifiers to IBC application contracts
    /// @custom:storage-location erc7201:ibc.storage.ICS26Router
    struct ICS26RouterStorage {
        mapping(string => IIBCApp) _apps;
    }

    /// @notice ERC-7201 slot for the ICS26Router storage
    /// @dev keccak256(abi.encode(uint256(keccak256("ibc.storage.ICS26Router")) - 1)) & ~bytes32(uint256(0xff))
    bytes32 private constant ICS26ROUTER_STORAGE_SLOT =
        0xc5779f3c2c21083eefa6d04f6a698bc0d8c10db124ad5e0df6ef394b6d7bf600;

    /// @notice The maximum timeout duration for a packet
    uint256 private constant MAX_TIMEOUT_DURATION = 1 days;

    /// @dev This contract is meant to be deployed by a proxy, so the constructor is not used
    // natlint-disable-next-line MissingNotice
    constructor(
        address clientMigrationProposer,
        address clientMigrationExecutor
    )
        ICS02ClientUpgradeable(clientMigrationProposer, clientMigrationExecutor)
    {
        _disableInitializers();
    }

    /// @inheritdoc IICS26Router
    function initialize(address authority) external onlyVersion(0) reinitializer(2) {
        __ReentrancyGuardTransient_init();
        __Multicall_init();
        __Pausable_init();
        __IBCStore_init();
        __ICS02Client_init(authority);
    }

    /// @inheritdoc IICS26Router
    function initializeV2(address authority) external onlyVersion(1) reinitializer(2) {
        require(ICS26AdminsDeprecated.isAdmin(_msgSender()), IBCUnauthorizedSender(_msgSender()));

        ICS26AdminsDeprecated.__IBCUUPSUpgradeable_deinit();
        __ICS02Client_init(authority);
    }

    /// @inheritdoc IPausable
    function pause() external restricted {
        _pause();
    }

    /// @inheritdoc IPausable
    function unpause() external restricted {
        _unpause();
    }

    /// @inheritdoc IICS26Router
    function getIBCApp(string calldata portId) public view returns (IIBCApp) {
        IIBCApp app = _getICS26RouterStorage()._apps[portId];
        require(address(app) != address(0), IBCAppNotFound(portId));
        return app;
    }

    /// @inheritdoc IICS26Router
    function addIBCApp(address app) external nonReentrant {
        string memory portId = Strings.toHexString(app);
        _addIBCApp(portId, app);
    }

    /// @inheritdoc IICS26RouterAccessControlled
    function addIBCApp(string calldata portId, address app) external nonReentrant restricted {
        require(bytes(portId).length != 0, IBCInvalidPortIdentifier(portId));
        // NOTE: We do not allow port-ids to be addresses, as this would conflict with the permissionless port-ids
        // slither-disable-next-line unused-return
        (bool isAddress,) = Strings.tryParseAddress(portId);
        require(!isAddress, IBCInvalidPortIdentifier(portId));
        require(IBCIdentifiers.validateCustomIBCIdentifier(bytes(portId)), IBCInvalidPortIdentifier(portId));
        _addIBCApp(portId, app);
    }

    /// @notice This function adds an app to the app router
    /// @dev This function assumes that the portId has already been generated and validated.
    /// @param portId The port identifier
    /// @param app The address of the app contract
    function _addIBCApp(string memory portId, address app) private {
        ICS26RouterStorage storage $ = _getICS26RouterStorage();
        require(address($._apps[portId]) == address(0), IBCPortAlreadyExists(portId));
        $._apps[portId] = IIBCApp(app);
        emit IBCAppAdded(portId, app);
    }

    /// @inheritdoc IICS26Router
    function sendPacket(IICS26RouterMsgs.MsgSendPacket calldata msg_)
        external
        whenNotPaused
        nonReentrant
        returns (uint64)
    {
        address ibcApp = address(getIBCApp(msg_.payload.sourcePort));
        require(ibcApp == _msgSender(), IBCUnauthorizedSender(_msgSender()));

        string memory counterpartyId = getCounterparty(msg_.sourceClient).clientId;

        require(
            msg_.timeoutTimestamp > block.timestamp, IBCInvalidTimeoutTimestamp(msg_.timeoutTimestamp, block.timestamp)
        );
        require(
            // solhint-disable-next-line gas-strict-inequalities
            msg_.timeoutTimestamp - block.timestamp <= MAX_TIMEOUT_DURATION,
            IBCInvalidTimeoutDuration(MAX_TIMEOUT_DURATION, msg_.timeoutTimestamp - block.timestamp)
        );

        uint64 sequence = nextSequenceSend(msg_.sourceClient);

        // TODO: Support multi-payload packets #93
        IICS26RouterMsgs.Packet memory packet = IICS26RouterMsgs.Packet({
            sequence: sequence,
            sourceClient: msg_.sourceClient,
            destClient: counterpartyId,
            timeoutTimestamp: msg_.timeoutTimestamp,
            payloads: new IICS26RouterMsgs.Payload[](1)
        });
        packet.payloads[0] = msg_.payload;

        commitPacket(packet);

        emit SendPacket(msg_.sourceClient, sequence, packet);
        return sequence;
    }

    /// @inheritdoc IICS26RouterAccessControlled
    // NOTE: Reentrancy disabled for this function via the `nonReentrant` modifier.
    // slither-disable-next-line reentrancy-no-eth
    function recvPacket(IICS26RouterMsgs.MsgRecvPacket calldata msg_) external whenNotPaused nonReentrant restricted {
        IICS26RouterMsgs.Payload calldata payload = _singlePayload(msg_.packet);

        IICS02ClientMsgs.CounterpartyInfo memory cInfo = getCounterparty(msg_.packet.destClient);
        _requireCounterparty(cInfo, msg_.packet.sourceClient);

        require(
            msg_.packet.timeoutTimestamp > block.timestamp,
            IBCInvalidTimeoutTimestamp(msg_.packet.timeoutTimestamp, block.timestamp)
        );

        bytes memory commitmentPath =
            ICS24Host.packetCommitmentPathCalldata(msg_.packet.sourceClient, msg_.packet.sequence);
        bytes32 commitmentBz = ICS24Host.packetCommitmentBytes32(msg_.packet);

        // Security note: membershipMsg is fully relayer-supplied. The router overrides
        // `path` and `value` below so the light client proves the correct packet
        // commitment regardless of what the relayer provided for those fields. All
        // remaining fields (height, proofs, appHash, trustedConsensusState, …) are
        // relayer-controlled; correctness and integrity for those fields is delegated
        // entirely to the light client's verifyMembership implementation.
        ILightClientMsgs.MsgVerifyMembership memory membershipMsg =
            abi.decode(msg_.membershipMsg, (ILightClientMsgs.MsgVerifyMembership));
        // Override path and value so the light client proves the actual packet commitment
        membershipMsg.path = ICS24Host.prefixedPath(cInfo.merklePrefix, commitmentPath);
        membershipMsg.value = abi.encodePacked(commitmentBz);
        getClient(msg_.packet.destClient).verifyMembership(membershipMsg);

        // recvPacket will no-op if the packet receipt already exists
        // The no-op check runs after membership verification so a replayed packet still requires a valid proof
        bool receiptAlreadySet = !setPacketReceipt(msg_.packet);
        if (receiptAlreadySet) {
            emit Noop();
            return;
        }

        bytes[] memory acks = new bytes[](1);
        try getIBCApp(payload.destPort)
            .onRecvPacket(
                IIBCAppCallbacks.OnRecvPacketCallback({
                    sourceClient: msg_.packet.sourceClient,
                    destinationClient: msg_.packet.destClient,
                    sequence: msg_.packet.sequence,
                    payload: payload,
                    relayer: _msgSender()
                })
            ) returns (
            bytes memory ack
        ) {
            require(ack.length != 0, IBCAsyncAcknowledgementNotSupported());
            require(keccak256(ack) != ICS24Host.KECCAK256_UNIVERSAL_ERROR_ACK, IBCErrorUniversalAcknowledgement());
            acks[0] = ack;
        } catch (bytes memory reason) {
            require(reason.length != 0, IBCFailedCallback()); // covers OOG
            emit IBCAppRecvPacketCallbackError(reason);
            acks[0] = ICS24Host.UNIVERSAL_ERROR_ACK;
        }

        commitPacketAcknowledgement(msg_.packet, acks);
        emit WriteAcknowledgement(msg_.packet.destClient, msg_.packet.sequence, msg_.packet, acks);
    }

    /// @inheritdoc IICS26RouterAccessControlled
    // NOTE: Reentrancy disabled for this function via the `nonReentrant` modifier.
    // slither-disable-next-line reentrancy-no-eth
    function ackPacket(IICS26RouterMsgs.MsgAckPacket calldata msg_) external nonReentrant restricted {
        IICS26RouterMsgs.Payload calldata payload = _singlePayload(msg_.packet);

        IICS02ClientMsgs.CounterpartyInfo memory cInfo = getCounterparty(msg_.packet.sourceClient);
        _requireCounterparty(cInfo, msg_.packet.destClient);

        bytes memory commitmentPath =
            ICS24Host.packetAcknowledgementCommitmentPathCalldata(msg_.packet.destClient, msg_.packet.sequence);
        bytes[] memory acks = new bytes[](1);
        acks[0] = msg_.acknowledgement;
        bytes32 commitmentBz = ICS24Host.packetAcknowledgementCommitmentBytes32(acks);

        // verify the packet acknowledgement
        ILightClientMsgs.MsgVerifyMembership memory membershipMsg =
            abi.decode(msg_.membershipMsg, (ILightClientMsgs.MsgVerifyMembership));
        // Override path and value so the light client proves the actual packet commitment
        membershipMsg.path = ICS24Host.prefixedPath(cInfo.merklePrefix, commitmentPath);
        membershipMsg.value = abi.encodePacked(commitmentBz);
        getClient(msg_.packet.sourceClient).verifyMembership(membershipMsg);

        // ackPacket will no-op if the packet commitment does not exist
        // The no-op check runs after membership verification so a replayed ack still requires a valid proof
        bool commitmentFound = checkAndDeletePacketCommitment(msg_.packet);
        if (!commitmentFound) {
            emit Noop();
            return;
        }

        getIBCApp(payload.sourcePort)
            .onAcknowledgementPacket(
                IIBCAppCallbacks.OnAcknowledgementPacketCallback({
                    sourceClient: msg_.packet.sourceClient,
                    destinationClient: msg_.packet.destClient,
                    sequence: msg_.packet.sequence,
                    payload: payload,
                    acknowledgement: msg_.acknowledgement,
                    relayer: _msgSender()
                })
            );

        emit AckPacket(msg_.packet.sourceClient, msg_.packet.sequence, msg_.packet, msg_.acknowledgement);
    }

    /// @inheritdoc IICS26RouterAccessControlled
    function timeoutPacket(IICS26RouterMsgs.MsgTimeoutPacket calldata msg_) external nonReentrant restricted {
        IICS26RouterMsgs.Payload calldata payload = _singlePayload(msg_.packet);

        IICS02ClientMsgs.CounterpartyInfo memory cInfo = getCounterparty(msg_.packet.sourceClient);
        _requireCounterparty(cInfo, msg_.packet.destClient);

        bytes memory receiptPath =
            ICS24Host.packetReceiptCommitmentPathCalldata(msg_.packet.destClient, msg_.packet.sequence);
        ILightClientMsgs.MsgVerifyNonMembership memory nonMembershipMsg =
            abi.decode(msg_.nonMembershipMsg, (ILightClientMsgs.MsgVerifyNonMembership));
        // Override path and value so the light client proves the actual packet commitment
        nonMembershipMsg.path = ICS24Host.prefixedPath(cInfo.merklePrefix, receiptPath);
        uint256 counterpartyTimestamp = getClient(msg_.packet.sourceClient).verifyNonMembership(nonMembershipMsg);
        require(
            counterpartyTimestamp >= msg_.packet.timeoutTimestamp,
            IBCInvalidTimeoutTimestamp(msg_.packet.timeoutTimestamp, counterpartyTimestamp)
        );

        // timeoutPacket will no-op if the packet commitment does not exist
        // The no-op check runs after non-membership verification so a replayed timeout still requires a valid proof
        bool commitmentFound = checkAndDeletePacketCommitment(msg_.packet);
        if (!commitmentFound) {
            emit Noop();
            return;
        }

        getIBCApp(payload.sourcePort)
            .onTimeoutPacket(
                IIBCAppCallbacks.OnTimeoutPacketCallback({
                    sourceClient: msg_.packet.sourceClient,
                    destinationClient: msg_.packet.destClient,
                    sequence: msg_.packet.sequence,
                    payload: payload,
                    relayer: _msgSender()
                })
            );

        emit TimeoutPacket(msg_.packet.sourceClient, msg_.packet.sequence, msg_.packet);
    }

    /// @notice Returns the only payload of a packet
    /// @dev TODO: Support multi-payload packets (#93)
    /// @param packet The packet
    /// @return The payload of the packet
    function _singlePayload(IICS26RouterMsgs.Packet calldata packet)
        private
        pure
        returns (IICS26RouterMsgs.Payload calldata)
    {
        require(packet.payloads.length == 1, IBCMultiPayloadPacketNotSupported());
        return packet.payloads[0];
    }

    /// @notice Reverts unless the packet's counterparty client is the one registered for the local client
    /// @param cInfo The counterparty registered for the local client
    /// @param packetClient The counterparty client id carried by the packet
    function _requireCounterparty(
        IICS02ClientMsgs.CounterpartyInfo memory cInfo,
        string calldata packetClient
    )
        private
        pure
    {
        require(
            keccak256(bytes(cInfo.clientId)) == keccak256(bytes(packetClient)),
            IBCInvalidCounterparty(cInfo.clientId, packetClient)
        );
    }

    /// @notice Returns the storage of the ICS26Router contract
    /// @return $ The storage of the ICS26Router contract
    function _getICS26RouterStorage() private pure returns (ICS26RouterStorage storage $) {
        // solhint-disable-next-line no-inline-assembly
        assembly {
            $.slot := ICS26ROUTER_STORAGE_SLOT
        }
    }

    /// @inheritdoc UUPSUpgradeable
    function _authorizeUpgrade(address) internal override restricted { }
    // solhint-disable-previous-line no-empty-blocks

    /// @notice Modifier to check if the initialization version matches the expected version
    /// @param version The expected current version of the contract
    modifier onlyVersion(uint256 version) {
        require(_getInitializedVersion() == version, InvalidInitialization());
        _;
    }
}
