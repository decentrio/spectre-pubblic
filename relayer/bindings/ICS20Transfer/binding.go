// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package contractICS20Transfer

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
	_ = abi.ConvertType
)

// IIBCAppCallbacksOnAcknowledgementPacketCallback is an auto generated low-level Go binding around an user-defined struct.
type IIBCAppCallbacksOnAcknowledgementPacketCallback struct {
	SourceClient      string
	DestinationClient string
	Sequence          uint64
	Payload           IICS26RouterMsgsPayload
	Acknowledgement   []byte
	Relayer           common.Address
}

// IIBCAppCallbacksOnRecvPacketCallback is an auto generated low-level Go binding around an user-defined struct.
type IIBCAppCallbacksOnRecvPacketCallback struct {
	SourceClient      string
	DestinationClient string
	Sequence          uint64
	Payload           IICS26RouterMsgsPayload
	Relayer           common.Address
}

// IIBCAppCallbacksOnTimeoutPacketCallback is an auto generated low-level Go binding around an user-defined struct.
type IIBCAppCallbacksOnTimeoutPacketCallback struct {
	SourceClient      string
	DestinationClient string
	Sequence          uint64
	Payload           IICS26RouterMsgsPayload
	Relayer           common.Address
}

// IICS20TransferMsgsSendTransferMsg is an auto generated low-level Go binding around an user-defined struct.
type IICS20TransferMsgsSendTransferMsg struct {
	Denom            common.Address
	Amount           *big.Int
	Receiver         string
	SourceClient     string
	DestPort         string
	TimeoutTimestamp uint64
	Memo             string
}

// IICS26RouterMsgsPayload is an auto generated low-level Go binding around an user-defined struct.
type IICS26RouterMsgsPayload struct {
	SourcePort string
	DestPort   string
	Version    string
	Encoding   string
	Value      []byte
}

// ISignatureTransferPermitTransferFrom is an auto generated low-level Go binding around an user-defined struct.
type ISignatureTransferPermitTransferFrom struct {
	Permitted ISignatureTransferTokenPermissions
	Nonce     *big.Int
	Deadline  *big.Int
}

// ISignatureTransferTokenPermissions is an auto generated low-level Go binding around an user-defined struct.
type ISignatureTransferTokenPermissions struct {
	Token  common.Address
	Amount *big.Int
}

// ContractICS20TransferMetaData contains all meta data concerning the ContractICS20Transfer contract.
var ContractICS20TransferMetaData = &bind.MetaData{
	ABI: "[{\"type\":\"constructor\",\"inputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"UPGRADE_INTERFACE_VERSION\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"string\",\"internalType\":\"string\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"activateEscrow\",\"inputs\":[{\"name\":\"clientId\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"tokens\",\"type\":\"address[]\",\"internalType\":\"address[]\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"authority\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"createEscrow\",\"inputs\":[{\"name\":\"clientId\",\"type\":\"string\",\"internalType\":\"string\"}],\"outputs\":[{\"name\":\"escrow\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"enableEscrowLaunchGate\",\"inputs\":[],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"getEscrow\",\"inputs\":[{\"name\":\"clientId\",\"type\":\"string\",\"internalType\":\"string\"}],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"getEscrowBeacon\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"getIBCERC20Beacon\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"getPermit2\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"ibcERC20Contract\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"}],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"ibcERC20Denom\",\"inputs\":[{\"name\":\"token\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[{\"name\":\"\",\"type\":\"string\",\"internalType\":\"string\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"ics26\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"initialize\",\"inputs\":[{\"name\":\"ics26Router\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"escrowLogic\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"ibcERC20Logic\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"permit2\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"authority\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"initializeV2\",\"inputs\":[{\"name\":\"authority\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"isConsumingScheduledOp\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bytes4\",\"internalType\":\"bytes4\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"isEscrowActive\",\"inputs\":[{\"name\":\"clientId\",\"type\":\"string\",\"internalType\":\"string\"}],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"multicall\",\"inputs\":[{\"name\":\"data\",\"type\":\"bytes[]\",\"internalType\":\"bytes[]\"}],\"outputs\":[{\"name\":\"results\",\"type\":\"bytes[]\",\"internalType\":\"bytes[]\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"onAcknowledgementPacket\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structIIBCAppCallbacks.OnAcknowledgementPacketCallback\",\"components\":[{\"name\":\"sourceClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destinationClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"sequence\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"payload\",\"type\":\"tuple\",\"internalType\":\"structIICS26RouterMsgs.Payload\",\"components\":[{\"name\":\"sourcePort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destPort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"version\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"encoding\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"value\",\"type\":\"bytes\",\"internalType\":\"bytes\"}]},{\"name\":\"acknowledgement\",\"type\":\"bytes\",\"internalType\":\"bytes\"},{\"name\":\"relayer\",\"type\":\"address\",\"internalType\":\"address\"}]}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"onRecvPacket\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structIIBCAppCallbacks.OnRecvPacketCallback\",\"components\":[{\"name\":\"sourceClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destinationClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"sequence\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"payload\",\"type\":\"tuple\",\"internalType\":\"structIICS26RouterMsgs.Payload\",\"components\":[{\"name\":\"sourcePort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destPort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"version\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"encoding\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"value\",\"type\":\"bytes\",\"internalType\":\"bytes\"}]},{\"name\":\"relayer\",\"type\":\"address\",\"internalType\":\"address\"}]}],\"outputs\":[{\"name\":\"\",\"type\":\"bytes\",\"internalType\":\"bytes\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"onTimeoutPacket\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structIIBCAppCallbacks.OnTimeoutPacketCallback\",\"components\":[{\"name\":\"sourceClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destinationClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"sequence\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"payload\",\"type\":\"tuple\",\"internalType\":\"structIICS26RouterMsgs.Payload\",\"components\":[{\"name\":\"sourcePort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destPort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"version\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"encoding\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"value\",\"type\":\"bytes\",\"internalType\":\"bytes\"}]},{\"name\":\"relayer\",\"type\":\"address\",\"internalType\":\"address\"}]}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"pause\",\"inputs\":[],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"paused\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"proxiableUUID\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"requiresPrecreatedEscrows\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"sendTransfer\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structIICS20TransferMsgs.SendTransferMsg\",\"components\":[{\"name\":\"denom\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"receiver\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"sourceClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destPort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"timeoutTimestamp\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"memo\",\"type\":\"string\",\"internalType\":\"string\"}]}],\"outputs\":[{\"name\":\"\",\"type\":\"uint64\",\"internalType\":\"uint64\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"sendTransferWithPermit2\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structIICS20TransferMsgs.SendTransferMsg\",\"components\":[{\"name\":\"denom\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"receiver\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"sourceClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destPort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"timeoutTimestamp\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"memo\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"name\":\"permit\",\"type\":\"tuple\",\"internalType\":\"structISignatureTransfer.PermitTransferFrom\",\"components\":[{\"name\":\"permitted\",\"type\":\"tuple\",\"internalType\":\"structISignatureTransfer.TokenPermissions\",\"components\":[{\"name\":\"token\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"}]},{\"name\":\"nonce\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"deadline\",\"type\":\"uint256\",\"internalType\":\"uint256\"}]},{\"name\":\"signature\",\"type\":\"bytes\",\"internalType\":\"bytes\"}],\"outputs\":[{\"name\":\"\",\"type\":\"uint64\",\"internalType\":\"uint64\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"sendTransferWithSender\",\"inputs\":[{\"name\":\"msg_\",\"type\":\"tuple\",\"internalType\":\"structIICS20TransferMsgs.SendTransferMsg\",\"components\":[{\"name\":\"denom\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"receiver\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"sourceClient\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"destPort\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"timeoutTimestamp\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"memo\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"name\":\"sender\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[{\"name\":\"\",\"type\":\"uint64\",\"internalType\":\"uint64\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"setAuthority\",\"inputs\":[{\"name\":\"newAuthority\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"setCustomERC20\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"token\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"setIBCERC20Metadata\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"name_\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"symbol_\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"decimals_\",\"type\":\"uint8\",\"internalType\":\"uint8\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"unpause\",\"inputs\":[],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"upgradeEscrowTo\",\"inputs\":[{\"name\":\"newEscrowLogic\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"upgradeIBCERC20To\",\"inputs\":[{\"name\":\"newIBCERC20Logic\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"upgradeToAndCall\",\"inputs\":[{\"name\":\"newImplementation\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"data\",\"type\":\"bytes\",\"internalType\":\"bytes\"}],\"outputs\":[],\"stateMutability\":\"payable\"},{\"type\":\"event\",\"name\":\"AuthorityUpdated\",\"inputs\":[{\"name\":\"authority\",\"type\":\"address\",\"indexed\":false,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"IBCERC20ContractCreated\",\"inputs\":[{\"name\":\"contractAddress\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"fullDenomPath\",\"type\":\"string\",\"indexed\":false,\"internalType\":\"string\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"IBCSenderAckPacketCallbackError\",\"inputs\":[{\"name\":\"callbackAddress\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"reason\",\"type\":\"bytes\",\"indexed\":false,\"internalType\":\"bytes\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"IBCSenderAckPacketCallbackError\",\"inputs\":[{\"name\":\"callbackAddress\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"reason\",\"type\":\"bytes\",\"indexed\":false,\"internalType\":\"bytes\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"IBCSenderTimeoutPacketCallbackError\",\"inputs\":[{\"name\":\"callbackAddress\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"reason\",\"type\":\"bytes\",\"indexed\":false,\"internalType\":\"bytes\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"IBCSenderTimeoutPacketCallbackError\",\"inputs\":[{\"name\":\"callbackAddress\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"reason\",\"type\":\"bytes\",\"indexed\":false,\"internalType\":\"bytes\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"ICS20EscrowActivated\",\"inputs\":[{\"name\":\"clientId\",\"type\":\"string\",\"indexed\":true,\"internalType\":\"string\"},{\"name\":\"escrow\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"ICS20EscrowCreated\",\"inputs\":[{\"name\":\"clientId\",\"type\":\"string\",\"indexed\":true,\"internalType\":\"string\"},{\"name\":\"escrow\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"ICS20EscrowLaunchGateEnabled\",\"inputs\":[],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Initialized\",\"inputs\":[{\"name\":\"version\",\"type\":\"uint64\",\"indexed\":false,\"internalType\":\"uint64\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Paused\",\"inputs\":[{\"name\":\"account\",\"type\":\"address\",\"indexed\":false,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Unpaused\",\"inputs\":[{\"name\":\"account\",\"type\":\"address\",\"indexed\":false,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"Upgraded\",\"inputs\":[{\"name\":\"implementation\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"error\",\"name\":\"AccessManagedInvalidAuthority\",\"inputs\":[{\"name\":\"authority\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"AccessManagedRequiredDelay\",\"inputs\":[{\"name\":\"caller\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"delay\",\"type\":\"uint32\",\"internalType\":\"uint32\"}]},{\"type\":\"error\",\"name\":\"AccessManagedUnauthorized\",\"inputs\":[{\"name\":\"caller\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"AddressEmptyCode\",\"inputs\":[{\"name\":\"target\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"ERC1967InvalidImplementation\",\"inputs\":[{\"name\":\"implementation\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"ERC1967NonPayable\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"EnforcedPause\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ExpectedPause\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"FailedCall\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ICS20CustomDenomIsAddress\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20DenomAlreadyExists\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20DenomNotFound\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20EscrowNotActive\",\"inputs\":[{\"name\":\"clientID\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20EscrowNotFound\",\"inputs\":[{\"name\":\"clientID\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20EscrowNotProvisioned\",\"inputs\":[{\"name\":\"clientID\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20EscrowRateLimitNotSet\",\"inputs\":[{\"name\":\"clientID\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"token\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"ICS20EscrowTokenListEmpty\",\"inputs\":[{\"name\":\"clientID\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20InsufficientCallbackGas\",\"inputs\":[{\"name\":\"required\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"available\",\"type\":\"uint256\",\"internalType\":\"uint256\"}]},{\"type\":\"error\",\"name\":\"ICS20InvalidAddress\",\"inputs\":[{\"name\":\"addr\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20InvalidAmount\",\"inputs\":[{\"name\":\"amount\",\"type\":\"uint256\",\"internalType\":\"uint256\"}]},{\"type\":\"error\",\"name\":\"ICS20InvalidPort\",\"inputs\":[{\"name\":\"expected\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"actual\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20Permit2TokenMismatch\",\"inputs\":[{\"name\":\"permitToken\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"sentToken\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"ICS20TokenAlreadyExists\",\"inputs\":[{\"name\":\"denom\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20Unauthorized\",\"inputs\":[{\"name\":\"caller\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"ICS20UnauthorizedPacketSender\",\"inputs\":[{\"name\":\"packetSender\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"ICS20UnexpectedERC20Balance\",\"inputs\":[{\"name\":\"expected\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"actual\",\"type\":\"uint256\",\"internalType\":\"uint256\"}]},{\"type\":\"error\",\"name\":\"ICS20UnexpectedEncoding\",\"inputs\":[{\"name\":\"expected\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"actual\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"ICS20UnexpectedVersion\",\"inputs\":[{\"name\":\"expected\",\"type\":\"string\",\"internalType\":\"string\"},{\"name\":\"version\",\"type\":\"string\",\"internalType\":\"string\"}]},{\"type\":\"error\",\"name\":\"InvalidInitialization\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"NotInitializing\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ReentrancyGuardReentrantCall\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"SafeERC20FailedOperation\",\"inputs\":[{\"name\":\"token\",\"type\":\"address\",\"internalType\":\"address\"}]},{\"type\":\"error\",\"name\":\"StringsInsufficientHexLength\",\"inputs\":[{\"name\":\"value\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"length\",\"type\":\"uint256\",\"internalType\":\"uint256\"}]},{\"type\":\"error\",\"name\":\"UUPSUnauthorizedCallContext\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"UUPSUnsupportedProxiableUUID\",\"inputs\":[{\"name\":\"slot\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}]}]",
	Bin: "0x60a080604052346100c257306080525f5160206158dd5f395f51905f525460ff8160401c166100b3576002600160401b03196001600160401b03821601610060575b60405161581690816100c7823960805181818161190d01526119be0152f35b6001600160401b0319166001600160401b039081175f5160206158dd5f395f51905f525581527fc7f505b2f371ae2175ee4913f4499e1f2633a7b5936321eed1cdaeb6115181d290602090a15f80610041565b63f92ee8a960e01b5f5260045ffd5b5f80fdfe60806040526004361015610011575f80fd5b5f5f3560e01c806306ab20bc14612f7d578063078c4a79146127ea5780631459457a146124585780631bbf2e23146124125780631cf0a67f146123cd5780631e5150e41461238757806329b6eca91461217c5780632ac3dc38146120de5780633f4ba83a1461201d578063428e4e1714611bf95780634f1ef2861461196c57806352d1902d146118f257806353816a7c146114d857806357a85bea1461148d5780635c975abb1461144b5780635e32b6b61461122457806362bfa172146110e75780637a9e5e4b14611056578063826cae7a146110105780638456cb5914610f755780638fb3603714610f0b57806390f6d62014610eb157806393dcfee414610c32578063969631d514610bb6578063a1d28f5714610849578063a50ee2b4146107eb578063aaa2c3431461073f578063ac9650d8146105d4578063ad3cb1cc14610573578063b29c715d146103be578063bf7e214f1461038b578063d413227d14610345578063dab8ed9c146102a25763e163b1af14610190575f80fd5b3461029f57602036600319011261029f576101e26101ac613022565b6001600160a01b03165f527f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80260205260405f2090565b9060405191818154916101f483613743565b80865292600181169081156102755750600114610234575b6102308561021c818703826130ea565b604051918291602083526020830190613096565b0390f35b815260208120939250905b80821061025b5750909150810160200161021c8261023061020c565b91926001816020925483858801015201910190929161023f565b8695506102309693506020925061021c94915060ff191682840152151560051b820101929361020c565b80fd5b503461029f578060031936011261029f576102bd3633613844565b7f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065460ff8160a01c16156102ef575080f35b60ff60a01b1916600160a01b177f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f806557f602a536c4b89ff28b2a974a997fe044117170c12aa39af142bdbf06a097bb7fd8180a180f35b503461029f578060031936011261029f5760206001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8035416604051908152f35b503461029f578060031936011261029f5760206001600160a01b035f5160206157bc5f395f51905f525416604051908152f35b503461029f57604036600319011261029f576004359067ffffffffffffffff821161029f578160040160e0600319843603011261056f576103fd613038565b90610406613a5f565b61040e6139eb565b6104183633613844565b602484013593841561055c579060206001600160a01b0361046861046361045c6064889761045561044b6104be9b614903565b878b161515613467565b018661320f565b3691613128565b613ab2565b169561047e8161047785613569565b8933613c79565b61048783613569565b60405163b4f22eb760e01b81526001600160a01b039091166004820152336024820152604481019190915293849081906064820190565b038187895af1928315610551578493610516575b50906020946104e19392613e13565b907f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005d67ffffffffffffffff60405191168152f35b9250906020833d602011610549575b81610532602093836130ea565b81010312610545579151919060206104d2565b5f80fd5b3d9150610525565b6040513d86823e3d90fd5b6024846304f6df8d60e41b815280600452fd5b5080fd5b503461029f578060031936011261029f57506102306040516105966040826130ea565b600581527f352e302e300000000000000000000000000000000000000000000000000000006020820152604051918291602083526020830190613096565b503461029f57602036600319011261029f5760043567ffffffffffffffff811161056f5761060690369060040161318c565b90602060405161061682826130ea565b84815281810191601f19810136843761062e85613818565b9361063c60405195866130ea565b858552601f1961064b87613818565b0182885b82811061072f57505050865b868110156106d0576001906106b4866106ae8b8761067e8660051b89018961320f565b94909285604051968795858701378401908382019081528c519283915e01018d815203601f1981018352826130ea565b306148bf565b6106be8289613830565b526106c98188613830565b500161065b565b82888760405191838301848452825180915260408401948060408360051b870101940192955b8287106107035785850386f35b90919293828061071f600193603f198a82030186528851613096565b96019201960195929190926106f6565b606082828a01015201839061064f565b503461029f57602036600319011261029f578061075a613022565b6107643633613844565b6001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f805541690813b156107e7576001600160a01b0360248492836040519586948593631b2ce7f360e11b85521660048401525af180156107dc576107cb5750f35b816107d5916130ea565b61029f5780f35b6040513d84823e3d90fd5b5050fd5b503461029f57602036600319011261029f576004359067ffffffffffffffff821161029f576020610841610822366004860161315e565b6001600160a01b0361083682849594613622565b541692831515613692565b604051908152f35b503461029f57604036600319011261029f5760043567ffffffffffffffff811161056f5761087b90369060040161315e565b610886929192613038565b6108903633613844565b6108a661089e368487613128565b805190614a44565b50610b7a576001600160a01b036108bd8386613622565b5416610b3a57610906610900826001600160a01b03165f527f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80260205260405f2090565b54613743565b15610941826001600160a01b03165f527f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80260205260405f2090565b9015610a7e57506109a7906109568386613622565b6001600160a01b03808316166001600160a01b03198254161790556001600160a01b03165f527f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80260205260405f2090565b9067ffffffffffffffff8111610a6a576109cb816109c58454613743565b8461377b565b82601f8211600114610a0a57819084956109fa9495926109ff575b50508160011b915f199060031b1c19161790565b905580f35b013590505f806109e6565b601f198216948385526020852091855b878110610a52575083600195969710610a39575b505050811b01905580f35b01355f19600384901b60f8161c191690555f8080610a2e565b90926020600181928686013581550194019101610a1a565b602483634e487b7160e01b81526041600452fd5b83906040519182917f778769c40000000000000000000000000000000000000000000000000000000083526020600484015281815491610abd83613743565b928360248701526001811690815f14610b1a5750600114610ae0575b5050500390fd5b9080935052602082205b818310610b005750508101604401838080610ad9565b805460448487010152849350602090920191600101610aea565b925050506044925060ff191682840152151560051b820101838080610ad9565b6040517f0c0ef5340000000000000000000000000000000000000000000000000000000081526020600482015280610b7660248201858861327d565b0390fd5b6040517fbf1d4d390000000000000000000000000000000000000000000000000000000081526020600482015280610b7660248201858861327d565b503461029f57602036600319011261029f5760043567ffffffffffffffff811161056f5760209182610bf46001600160a01b0393369060040161315e565b925082604051938492833781017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8008152030190205416604051908152f35b503461029f57604036600319011261029f5760043567ffffffffffffffff811161056f57610c6490369060040161315e565b60243567ffffffffffffffff8111610ead57610c8490369060040161318c565b9290610c903633613844565b6001600160a01b03604051848482376020818681017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8008152030190205416938415610e8a578015610e4e57855b818110610d3457505050610cf182826135ea565b600160ff1982541617905581604051928392833781018481520390207f4091a6ce2a3c52563f54d5437475ea54363fc120a36395a84533922c2d5ad9918380a380f35b6001600160a01b03610d4f610d4a8385876136c2565b613569565b16151580610e32575b610d72908686610d6c610d4a86888a6136c2565b926136e6565b610d80610d4a8284866136c2565b906001600160a01b03604051927f0b0aee690000000000000000000000000000000000000000000000000000000084521660048301526020826024818a5afa918215610e27578892610df2575b50610dec6001928787610de4610d4a86898b6136c2565b9215156136e6565b01610cdd565b91506020823d8211610e1f575b81610e0c602093836130ea565b8101031261054557905190610dec610dcd565b3d9150610dff565b6040513d8a823e3d90fd5b50610d72610e44610d4a8385876136c2565b3b15159050610d58565b6040517fd5c055800000000000000000000000000000000000000000000000000000000081526020600482015280610b7660248201878761327d565b6040516358d1c8a160e11b81526020600482015280610b7660248201878761327d565b8380fd5b503461029f57602036600319011261029f576004359067ffffffffffffffff821161029f5760206001600160a01b03610f02610efd610ef3366004880161315e565b61045c3633613844565b61471a565b16604051908152f35b503461029f578060031936011261029f575f5160206157bc5f395f51905f525460a01c60ff1615610f6d575060207f8fb36037000000000000000000000000000000000000000000000000000000005b6001600160e01b031960405191168152f35b602090610f5b565b503461029f578060031936011261029f57610f903633613844565b610f98613a5f565b600160ff197fcd5ed15c6e187e77e9aee88184c21f4f2182ab5827cb3b7e07fbedcd63f033005416177fcd5ed15c6e187e77e9aee88184c21f4f2182ab5827cb3b7e07fbedcd63f03300557f62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a2586020604051338152a180f35b503461029f578060031936011261029f5760206001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8045416604051908152f35b503461029f57602036600319011261029f57611070613022565b6001600160a01b035f5160206157bc5f395f51905f52541633036110d557803b156110a15761109e906146ba565b80f35b7fc2f31e5e0000000000000000000000000000000000000000000000000000000082526001600160a01b0316600452602490fd5b60248262d1953b60e31b815233600452fd5b503461029f57608036600319011261029f578060043567ffffffffffffffff81116112215761111a90369060040161315e565b60243567ffffffffffffffff811161121e5761113a90369060040161315e565b91909260443567ffffffffffffffff811161121a5761115d90369060040161315e565b94906064359360ff8516809503611216576111999061117c3633613844565b6001600160a01b0361118e8287613622565b541694851515613692565b823b156112125786946111e9946111fb8793604051998a98899788967f37d2c2f400000000000000000000000000000000000000000000000000000000885260606004890152606488019161327d565b8581036003190160248701529161327d565b90604483015203925af180156107dc576107cb5750f35b8680fd5b8780fd5b8580fd5b50505b50fd5b503461029f5761123336613062565b611269336001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f803541633146131bd565b6112716139eb565b6060810161129761128f61128583856131fa565b608081019061320f565b81019061339a565b906112cf6112ae6112a883866131fa565b8061320f565b93906112ba868061320f565b9060408801966112c98861357d565b93614412565b9390506112db84614d2c565b611307575b84807f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005d80f35b604051928392602084017f5e32b6b600000000000000000000000000000000000000000000000000000000905260248401602090526113468380614322565b6044860160a0905260e486019061135c9261327d565b6113696020850185614322565b86830360431901606488015261137f929161327d565b9161138990614354565b67ffffffffffffffff1660848501526113a29083614369565b8382036043190160a48501526113b8919061437d565b906080016113c59061304e565b6001600160a01b031660c483015203601f19810182526113e590826130ea565b6113ef9082614b23565b9190156113fe575b80806112e0565b6001600160a01b037f02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6916114416040519283926020845216946020830190613096565b0390a25f806113f7565b503461029f578060031936011261029f57602060ff7fcd5ed15c6e187e77e9aee88184c21f4f2182ab5827cb3b7e07fbedcd63f0330054166040519015158152f35b503461029f57602036600319011261029f576004359067ffffffffffffffff821161029f57602060ff6114cc6114c6366004870161315e565b906135ea565b54166040519015158152f35b503461029f5760c036600319011261029f576004359067ffffffffffffffff821161029f57816004019160e0600319823603011261056f57608036602319011261056f5760a43567ffffffffffffffff81116118ee5761153c90369060040161315e565b611547929192613a5f565b61154f6139eb565b60248201359182156118db57611563613553565b6001600160a01b038061157589613569565b16911614611581613553565b61158a88613569565b91156118a1575050906115a861046361045c6064602495018961320f565b9360206001600160a01b03806115bd8a613569565b16961695604051948580926370a0823160e01b82528960048301525afa928315611896578693611862575b506001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80654166040516040810181811067ffffffffffffffff82111761184e576040528681526020810192868452823b1561184a5788809493926116ce6001600160a01b0393604051988997889687957f30f28b7a0000000000000000000000000000000000000000000000000000000087528161168a613038565b166004880152604435602488015260643560448801526084356064880152511660848601525160a48501523360c485015261010060e485015261010484019161327d565b03925af1801561182a57908591611835575b5050906024929160206001600160a01b036116fa88613569565b16604051958680926370a0823160e01b82528760048301525afa93841561182a5785946117f2575b5090611745611785948361173884602096613592565b908211806117e9576135b3565b61174e86613569565b60405163b4f22eb760e01b81526001600160a01b039091166004820152336024820152604481019190915292839081906064820190565b038186855af19182156117de5783926117a9575b50906020936104e1923391613e13565b91506020823d6020116117d6575b816117c4602093836130ea565b81010312610545579051906020611799565b3d91506117b7565b6040513d85823e3d90fd5b508082146135b3565b9350906020843d602011611822575b8161180e602093836130ea565b810103126105455792519290611745611722565b3d9150611801565b6040513d87823e3d90fd5b8161183f916130ea565b610ead57835f6116e0565b8880fd5b602489634e487b7160e01b81526041600452fd5b9092506020813d60201161188e575b8161187e602093836130ea565b810103126105455751915f6115e8565b3d9150611871565b6040513d88823e3d90fd5b7fe36164780000000000000000000000000000000000000000000000000000000087526001600160a01b0390811660045216602452604485fd5b6024856304f6df8d60e41b815280600452fd5b8280fd5b503461029f578060031936011261029f576001600160a01b037f000000000000000000000000000000000000000000000000000000000000000016300361195d5760206040517f360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc8152f35b8063703e46dd60e11b60049252fd5b50604036600319011261029f57611981613022565b9060243567ffffffffffffffff811161056f573660238201121561056f576119b3903690602481600401359101613128565b916001600160a01b037f000000000000000000000000000000000000000000000000000000000000000016803014908115611bc4575b50611bb5576119f83633613844565b6001600160a01b03811690604051937f52d1902d000000000000000000000000000000000000000000000000000000008552602085600481865afa80958596611b7d575b50611a545760248484634c9c8ce360e01b8252600452fd5b9091847f360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc8103611b525750823b15611b4057806001600160a01b03197f360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc5416177f360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc557fbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b8480a2805115611b0e57611b0a916148bf565b5080f35b505034611b185780f35b807fb398979f0000000000000000000000000000000000000000000000000000000060049252fd5b634c9c8ce360e01b8452600452602483fd5b7faa1d49a4000000000000000000000000000000000000000000000000000000008552600452602484fd5b9095506020813d602011611bad575b81611b99602093836130ea565b81010312611ba95751945f611a3c565b8480fd5b3d9150611b8c565b60048263703e46dd60e11b8152fd5b90506001600160a01b037f360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc541614155f6119e9565b503461029f57602036600319011261029f5760043567ffffffffffffffff811161056f57806004019060c060031982360301126118ee57611c66336001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f803541633146131bd565b611c6e6139eb565b60648101611c8261128f61128583866131fa565b9160848101611c9461045c828761320f565b60208151910120611ca3613518565b805160209091012014611e6e57611cdb611cc06112a885886131fa565b9590611ccc888061320f565b9060448701986112c98a61357d565b959050611ce786614d2c565b611d19575b5050505050505b807f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005d80f35b60405194859460208601638dfcd9ad60e01b90528860248701526044860160409052611d458380614322565b6064880160c09052610124880190611d5c9261327d565b611d696024870185614322565b8883036063190160848a0152611d7f929161327d565b91611d8990614354565b67ffffffffffffffff1660a4870152611da29083614369565b8582036063190160c4870152611db8919061437d565b91611dc291614322565b8483036063190160e4860152611dd8929161327d565b9060a401611de59061304e565b6001600160a01b031661010483015203601f1981018252611e0690826130ea565b611e109082614b23565b919015611e21575b80808080611cec565b6001600160a01b037f898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e091611e646040519283926020845216946020830190613096565b0390a25f80611e18565b6020611e9494611e7e878061320f565b96906044860197611e8e8961357d565b916142e6565b50015194611ead611ea6875188614a44565b9790613467565b611eb686614d2c565b611ec6575b505050505050611cf3565b60405194859460208601638dfcd9ad60e01b905260248601600190526044860160409052611ef48380614322565b6064880160c09052610124880190611f0b9261327d565b611f186024870185614322565b8883036063190160848a0152611f2e929161327d565b91611f3890614354565b67ffffffffffffffff1660a4870152611f519083614369565b8582036063190160c4870152611f67919061437d565b91611f7191614322565b8483036063190160e4860152611f87929161327d565b9060a401611f949061304e565b6001600160a01b031661010483015203601f1981018252611fb590826130ea565b611fbf9082614b23565b919015611fd0575b80808080611ebb565b6001600160a01b037f898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0916120136040519283926020845216946020830190613096565b0390a25f80611fc7565b503461029f578060031936011261029f576120383633613844565b7fcd5ed15c6e187e77e9aee88184c21f4f2182ab5827cb3b7e07fbedcd63f033005460ff8116156120b65760ff19167fcd5ed15c6e187e77e9aee88184c21f4f2182ab5827cb3b7e07fbedcd63f03300557f5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa6020604051338152a180f35b6004827f8dfc202b000000000000000000000000000000000000000000000000000000008152fd5b503461029f57602036600319011261029f576004359067ffffffffffffffff821161029f57816004019160e0600319823603011261056f5761211e613a5f565b6121266139eb565b6024810135908115612169579060206001600160a01b0361215361046361045c606461178597018961320f565b16916117458161216288613569565b8533613c79565b6024836304f6df8d60e41b815280600452fd5b503461029f57602036600319011261029f57612196613022565b5f5160206157dc5f395f51905f525467ffffffffffffffff811690600182036123785760401c60ff1690811561236c575b5061235d576121fd600267ffffffffffffffff195f5160206157dc5f395f51905f525416175f5160206157dc5f395f51905f5255565b6801000000000000000068ff0000000000000000195f5160206157dc5f395f51905f525416175f5160206157dc5f395f51905f5255602460206001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8035416604051928380927f24d7806c0000000000000000000000000000000000000000000000000000000082523360048301525afa9081156117de57839161231e575b50906122b16122c69233906131bd565b6122b9614adf565b6122c1614adf565b6146ba565b68ff0000000000000000195f5160206157dc5f395f51905f5254165f5160206157dc5f395f51905f52557fc7f505b2f371ae2175ee4913f4499e1f2633a7b5936321eed1cdaeb6115181d2602060405160028152a180f35b90506020813d602011612355575b81612339602093836130ea565b810103126118ee57519081151582036118ee57906122b16122a1565b3d915061232c565b60048263f92ee8a960e01b8152fd5b6002915010155f6121c7565b60048463f92ee8a960e01b8152fd5b503461029f578060031936011261029f5760206001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8055416604051908152f35b503461029f578060031936011261029f57602060ff7f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065460a01c166040519015158152f35b503461029f578060031936011261029f5760206001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065416604051908152f35b503461029f5760a036600319011261029f57612472613022565b61247a613038565b6044356001600160a01b0381168103610ead57606435926001600160a01b038416809403611ba9576084356001600160a01b038116810361121a575f5160206157dc5f395f51905f525467ffffffffffffffff811690816127db5760401c60ff169081156127cf575b506127c057906125736001600160a01b0392612526600267ffffffffffffffff195f5160206157dc5f395f51905f525416175f5160206157dc5f395f51905f5255565b6801000000000000000068ff0000000000000000195f5160206157dc5f395f51905f525416175f5160206157dc5f395f51905f5255612563614adf565b61256b614adf565b6122b1614adf565b166001600160a01b03197f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8035416177f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f803556040519161040e9283810181811067ffffffffffffffff8211176127ac576126058291614ef895878785396001600160a01b0316815230602082015260400190565b039086f0801561182a576001600160a01b03166001600160a01b03197f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8045416177f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80455604051928084019284841067ffffffffffffffff8511176127ac57918493916126a39385396001600160a01b0316815230602082015260400190565b039083f080156107dc576001600160a01b03166001600160a01b03197f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8055416177f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f805556001600160a01b03197f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065416177f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065568ff0000000000000000195f5160206157dc5f395f51905f5254165f5160206157dc5f395f51905f52557fc7f505b2f371ae2175ee4913f4499e1f2633a7b5936321eed1cdaeb6115181d2602060405160028152a180f35b602487634e487b7160e01b81526041600452fd5b60048663f92ee8a960e01b8152fd5b6002915010155f6124e3565b60048863f92ee8a960e01b8152fd5b503461029f576127f936613062565b90612830336001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f803541633146131bd565b7f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005c61029f5761285e6139eb565b612866613a5f565b6060820161288461045c61287a83866131fa565b604081019061320f565b60208151910120612893613242565b60208151910120146128a3613242565b6128b061287a84876131fa565b909215612f47575050506129026128cd61045c6112a884876131fa565b602081519101206128dc6132c5565b60208151910120146128ec6132c5565b906128fa6112a885886131fa565b929091613300565b61291c61045c61291283866131fa565b606081019061320f565b6020815191012061292b613344565b602081519101201461293b613344565b61294861291284876131fa565b909215612f115750505061299c61296f61045c61296584876131fa565b602081019061320f565b6020815191012061297e6132c5565b602081519101201461298e6132c5565b906128fa61296585886131fa565b6129ac61128f61128583866131fa565b926060840180511561055c5760408501926129ea6001600160a01b0385516129df6129d8825183614a44565b9290613467565b169451851515613467565b60208301926129ff61046361045c868461320f565b965193612a2e612a126112a885856131fa565b612a29612a1f868061320f565b9390923691613128565b613c10565b92855184511080612f01575b15612b945750509051835195969495879590949092509060209085811890861102851880612a70612a6b82896137d9565b6137e6565b9603920101602085015e612a85835184614a44565b939015848115612b82575b50612b59575b506001600160a01b03905b16905191813b15610ead57836064926001600160a01b039460405197889687957f0779afe6000000000000000000000000000000000000000000000000000000008752166004860152602485015260448401525af180156107dc57612b44575b61023082612b0d613518565b907f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005d604051918291602083526020830190613096565b612b4f8280926130ea565b61029f575f612b01565b92506001600160a01b0390612b7c82612b71866134ad565b5416948515156134eb565b90612a96565b6001600160a01b03915016155f612a90565b612bfe9350612bc9836020979493612a29612a1f612bc1612bb88c989789986131fa565b8781019061320f565b93909461320f565b926040519684889551918291018487015e8401908282018a8152815193849201905e010186815203601f1981018452836130ea565b6001600160a01b038516946001600160a01b03612c1a846134ad565b5416928315612ca3575b5084956001600160a01b03849695969416908351823b15611212576040516340c10f1960e01b81526001600160a01b0392909216600483015260248201529085908290604490829084905af190811561182a578591612c8e575b50506001600160a01b0390612aa1565b81612c98916130ea565b610ead57835f612c7e565b92506001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80454166040517f4571e3a600000000000000000000000000000000000000000000000000000000602082015230602482015287604482015260606064820152612d2b81612d1d6084820188613096565b03601f1981018352826130ea565b604051916104b68084019084821067ffffffffffffffff831117612eed5791849391612d5b936153068639613c59565b039086f0801561182a576001600160a01b031695612d78846134ad565b6001600160a01b0388166001600160a01b0319825416179055612dcb876001600160a01b03165f527f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80260205260405f2090565b9684519767ffffffffffffffff8911612ed957612df289612dec8354613743565b8361377b565b602098601f8111600114612e765780612e23918a9b8b9a9b91612e6b575b508160011b915f199060031b1c19161790565b90555b7f6031fab685dd6d86e4dbac9a69eae347145f332c95b3a0d728d3730fc5233d62612e608296604051918291602083526020830190613096565b0390a2959493612c24565b90508801515f612e10565b81895289892099601f1982168a5b818110612ec157509a82916001938c9d9c9b9c10612ea9575b5050811b019055612e26565b8901515f1960f88460031b161c191690555f80612e9d565b898301518d556001909c019b60209283019201612e84565b602488634e487b7160e01b81526041600452fd5b60248a634e487b7160e01b81526041600452fd5b50612f0c84876149f2565b612a3a565b610b76906040519384937fd1ca953a0000000000000000000000000000000000000000000000000000000085526004850161329d565b610b76906040519384937f094af3b80000000000000000000000000000000000000000000000000000000085526004850161329d565b503461054557602036600319011261054557612f97613022565b612fa13633613844565b6001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f804541690813b15610545576001600160a01b0360245f92836040519586948593631b2ce7f360e11b85521660048401525af1801561301757613009575080f35b61301591505f906130ea565b005b6040513d5f823e3d90fd5b600435906001600160a01b038216820361054557565b602435906001600160a01b038216820361054557565b35906001600160a01b038216820361054557565b6020600319820112610545576004359067ffffffffffffffff82116105455760a09082900360031901126105455760040190565b805180835260209291819084018484015e5f828201840152601f01601f1916010190565b60a0810190811067ffffffffffffffff8211176130d657604052565b634e487b7160e01b5f52604160045260245ffd5b90601f8019910116810190811067ffffffffffffffff8211176130d657604052565b67ffffffffffffffff81116130d657601f01601f191660200190565b9291926131348261310c565b9161314260405193846130ea565b829481845281830111610545578281602093845f960137010152565b9181601f840112156105455782359167ffffffffffffffff8311610545576020838186019501011161054557565b9181601f840112156105455782359167ffffffffffffffff8311610545576020808501948460051b01011161054557565b156131c55750565b6001600160a01b03907f2ecb3242000000000000000000000000000000000000000000000000000000005f521660045260245ffd5b903590609e1981360301821215610545570190565b903590601e1981360301821215610545570180359067ffffffffffffffff82116105455760200191813603831361054557565b604051906132516040836130ea565b600782527f69637332302d31000000000000000000000000000000000000000000000000006020830152565b908060209392818452848401375f828201840152601f01601f1916010190565b916132b46132c29492604085526040850190613096565b92602081850391015261327d565b90565b604051906132d46040836130ea565b600882527f7472616e736665720000000000000000000000000000000000000000000000006020830152565b929091921561330e57505050565b610b76906040519384937f5d3a3cdd0000000000000000000000000000000000000000000000000000000085526004850161329d565b604051906133536040836130ea565b601a82527f6170706c69636174696f6e2f782d736f6c69646974792d6162690000000000006020830152565b9080601f83011215610545578160206132c293359101613128565b6020818303126105455780359067ffffffffffffffff8211610545570160a08183031261054557604051916133ce836130ba565b813567ffffffffffffffff811161054557816133eb91840161337f565b8352602082013567ffffffffffffffff8111610545578161340d91840161337f565b6020840152604082013567ffffffffffffffff8111610545578161343291840161337f565b604084015260608201356060840152608082013567ffffffffffffffff81116105455761345f920161337f565b608082015290565b1561346f5750565b610b76906040519182917f3fed5d87000000000000000000000000000000000000000000000000000000008352602060048401526024830190613096565b60208091604051928184925191829101835e81017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80181520301902090565b156134f35750565b610b769060405191829163e1275e2f60e01b8352602060048401526024830190613096565b604051906135276040836130ea565b601182527f7b22726573756c74223a2241513d3d227d0000000000000000000000000000006020830152565b6024356001600160a01b03811681036105455790565b356001600160a01b03811681036105455790565b3567ffffffffffffffff811681036105455790565b9190820180921161359f57565b634e487b7160e01b5f52601160045260245ffd5b156135bc575050565b7f2fb30cfc000000000000000000000000000000000000000000000000000000005f5260045260245260445ffd5b60209082604051938492833781017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80781520301902090565b60209082604051938492833781017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80181520301902090565b60209082604051938492833781017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80881520301902090565b9190911561369e575050565b610b7660405192839263e1275e2f60e01b845260206004850152602484019161327d565b91908110156136d25760051b0190565b634e487b7160e01b5f52603260045260245ffd5b929092156136f357505050565b6001600160a01b036137386040519485947f28203fe700000000000000000000000000000000000000000000000000000000865260406004870152604486019161327d565b911660248301520390fd5b90600182811c92168015613771575b602083101461375d57565b634e487b7160e01b5f52602260045260245ffd5b91607f1691613752565b919091601f831161378c575b505050565b81831161379857505050565b5f5260205f206020601f830160051c92106137d1575b81601f9101920160051c03905f5b82811015613787575f828201556001016137bc565b5f91506137ae565b9190820391821161359f57565b906137f08261310c565b6137fd60405191826130ea565b828152809261380e601f199161310c565b0190602036910137565b67ffffffffffffffff81116130d65760051b60200190565b80518210156136d25760209160051b010190565b5f5160206157bc5f395f51905f5254916001600160a01b0383169281600411610545575f5f9060405f8151966001600160a01b0360208901917fb700961300000000000000000000000000000000000000000000000000000000835216978860248201523060448201526001600160e01b03198335166064820152606481526138ce6084826130ea565b828052826020525190895afa6139d8575b156138ec575b5050505050565b63ffffffff16156139c65760ff60a01b1916600160a01b175f5160206157bc5f395f51905f5255823b15610545576020925f92836040518096819582947f94c7d7ee00000000000000000000000000000000000000000000000000000000845260048401526040602484015260448301908082528085848401378181018301859052601f01601f1916010103925af18015613017576139b6575b5060ff60a01b195f5160206157bc5f395f51905f5254165f5160206157bc5f395f51905f52555f808080806138e5565b5f6139c0916130ea565b5f613986565b8262d1953b60e31b5f5260045260245ffd5b50505f516020518060201c1502906138df565b7f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005c613a375760017f9b779b17422d0df92223018b32b4d1fa46e071723d6817e2486d003becc55f005d565b7f3ee5aeb5000000000000000000000000000000000000000000000000000000005f5260045ffd5b60ff7fcd5ed15c6e187e77e9aee88184c21f4f2182ab5827cb3b7e07fbedcd63f033005416613a8a57565b7fd93c0665000000000000000000000000000000000000000000000000000000005f5260045ffd5b604051906001600160a01b03815192602081818501958087835e81017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f80081520301902054169182155f14613b5d5750905060ff7f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065460a01c16613b38576132c29061471a565b610b76906040519182916358d1c8a160e11b8352602060048401526024830190613096565b60ff7f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8065460a01c16613b8e57505090565b60ff906020604051809285518091835e81017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f807815203019020541615613bd2575090565b610b76906040519182917f0d2e2ae3000000000000000000000000000000000000000000000000000000008352602060048401526024830190613096565b60018091602080956132c2958160405198858a9651918291018688015e850191602f60f81b8584015260218301370101602f60f81b838201520301601e198101845201826130ea565b6040906001600160a01b036132c294931681528160208201520190613096565b916001600160a01b031691604051906370a0823160e01b82526001600160a01b03831691826004820152602081602481885afa9384156130175786915f95613ddc575b506040517f23b872dd0000000000000000000000000000000000000000000000000000000060208281019182526001600160a01b03958616602484015294909216604482015260648101929092525f91613d198160848101612d1d565b519082875af115613017575f513d613dd35750823b155b613da7576020906024604051809581936370a0823160e01b835260048301525afa918215613017575f92613d6f575b50611738613d6d9382613592565b565b9291506020833d602011613d9f575b81613d8b602093836130ea565b810103126105455791519091611738613d5f565b3d9150613d7e565b827f5274afe7000000000000000000000000000000000000000000000000000000005f5260045260245ffd5b60011415613d30565b915093506020813d602011613e0b575b81613df9602093836130ea565b8101031261054557519285905f613cbc565b3d9150613dec565b92919092613e236101ac82613569565b935f9060405195865f825492613e3884613743565b80845293600181169081156142c45750600114614280575b50613e5d925003876130ea565b858051155f1461419b575050613fdf9450613e88613e82613e7d84613569565b614903565b93614903565b613e95604084018461320f565b94613ee4613ec9613ea960c088018861320f565b94909860405194613eb9866130ba565b8552602085019687523691613128565b96604083019788526060830193602088013585523691613128565b9160808201928352613f886001600160a01b037f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8035416946060880198613f2a8a8a61320f565b969094613fd5613f3c60a08d0161357d565b97613fc7613f486132c5565b93613f516132c5565b95613fae613f5d613242565b98613f9b613f69613344565b9b6040519d8e986020808b01525160a060408b015260e08a0190613096565b9051888203603f190160608a0152613096565b9051868203603f19016080880152613096565b915160a085015251838203603f190160c0850152613096565b03601f1981018752866130ea565b6040519d8e6130ba565b8d5260208d015260408c015260608b015260808a0152604051926060840184811067ffffffffffffffff8211176127ac57936141046020969461403561408b958a9567ffffffffffffffff996040523691613128565b83528688840191168152604083019c8d526040519c8d97889687957f4d6e7ce30000000000000000000000000000000000000000000000000000000087528b600488015251606060248801526084870190613096565b9251166044850152519060231984820301606485015260806140f36140e16140cf6140bf865160a0875260a0870190613096565b8d8701518682038f880152613096565b60408601518582036040870152613096565b60608501518482036060860152613096565b920151906080818403910152613096565b03925af194851561418e578195614143575b505061412b916141259161320f565b9061365a565b67ffffffffffffffff83165f5260205260405f205590565b909194506020813d602011614186575b81614160602093836130ea565b8101031261056f57519067ffffffffffffffff8216820361029f57509281614125614116565b3d9150614153565b50604051903d90823e3d90fd5b6141bc6141a99795976132c5565b6141b6606087018761320f565b91613c10565b8151815110918261426f575b50506141dc575b50613e88613fdf95614903565b6001600160a01b0316946001600160a01b036141f784613569565b16863b15610545575f9660448892604051998a9384927f9dc29fac0000000000000000000000000000000000000000000000000000000084526004840152602089013560248401525af195861561301757613fdf96614258575b50946141cf565b6142659192505f906130ea565b5f90613e88614251565b61427992506149f2565b5f806141c8565b90505f9291925260205f20905f915b8183106142a8575050906020613e5d928201015f613e50565b6020919350806001915483858d0101520191019091889261428f565b905060209250613e5d94915060ff191682840152151560051b8201015f613e50565b92919061431367ffffffffffffffff91614300818761365a565b8385165f5260205260405f20549561365a565b91165f526020525f6040812055565b9035601e198236030181121561054557016020813591019167ffffffffffffffff821161054557813603831361054557565b359067ffffffffffffffff8216820361054557565b9035609e1982360301811215610545570190565b6132c2916144046143f96143de6143c36143a861439a8780614322565b60a0885260a088019161327d565b6143b56020880188614322565b90878303602089015261327d565b6143d06040870187614322565b90868303604088015261327d565b6143eb6060860186614322565b90858303606087015261327d565b926080810190614322565b91608081850391015261327d565b95949392939190915f936001600160a01b03604051878582376020818981017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f800815203019020541693841561467e578684612a296144809b6144879461045c60208901519e8f805190614a44565b9f90613467565b8251908151815110918261466d575b5050156145ef576001600160a01b036144af83516134ad565b5416956144bf83518815156134eb565b6060830151873b15610545576040516340c10f1960e01b81526001600160a01b038716600482015260248101919091525f81604481838c5af18015613017576145ca575b509067ffffffffffffffff614533606094935b614520818861365a565b8385165f5260205260405f20549661365a565b91165f526020528460405f20550151823b15610ead5790608484928360405195869485937fc920a6350000000000000000000000000000000000000000000000000000000085526001600160a01b038b1660048601526001600160a01b038d166024860152604485015260648401525af180156107dc576145b5575b50509190565b6145c08280926130ea565b61029f57806145af565b606093929196505f6145db916130ea565b5f959192509067ffffffffffffffff614503565b6001600160a01b0361460183516134ad565b5416958615614621575b9067ffffffffffffffff61453360609493614516565b95509060609167ffffffffffffffff61453383519861464b6146448b518c614a44565b9b90613467565b6146628a6001600160a01b038751911615156134eb565b92939450505061460b565b61467792506149f2565b5f80614496565b6040517f5778f3780000000000000000000000000000000000000000000000000000000081526020600482015280610b76602482018a8861327d565b60206001600160a01b037f2f658b440c35314f52658ea8a740e05b284cdc84dc9ae01e891f21b8933e7cad9216806001600160a01b03195f5160206157bc5f395f51905f525416175f5160206157bc5f395f51905f5255604051908152a1565b6040516001600160a01b03825191602081818601948086835e81017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f800815203019020541691821561476a57505090565b7f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f805545f5160206157bc5f395f51905f52546040517f485cc9550000000000000000000000000000000000000000000000000000000060208201523060248201526001600160a01b039182166044808301919091528152939450919291166147f26064836130ea565b604051916104b6908184019284841067ffffffffffffffff8511176130d6578493614821936153068639613c59565b03905ff08015613017576001600160a01b031691829160405160208183518086835e81017f823f7a8ea9ae6df0eb03ec5e1682d7a2839417ad8a91774118e6acf2e8d2f8008152030190206001600160a01b0384166001600160a01b0319825416179055604051918291518091835e81015f81520390207fd71bcf9347e4c440706076986a93115883dc9a480f43ec5d00610402a437be655f80a390565b5f806132c293602081519101845af43d156148fb573d916148df8361310c565b926148ed60405194856130ea565b83523d5f602085013e614bb3565b606091614bb3565b6001600160a01b031680614917602a61310c565b9161492560405193846130ea565b602a8352614933602a61310c565b6020840190601f19013682378351156136d257603090538251600110156136d2576078602184015360295b6001811161499f575061496f575090565b7fe22e27eb000000000000000000000000000000000000000000000000000000005f52600452601460245260445ffd5b90600f811660108110156136d25784518310156136d2577f3031323334353637383961626364656600000000000000000000000000000000901a8483016020015360041c90801561359f575f190161495e565b8051908251809210614a3d578051918280821091180280831892141582028218906020614a22612a6b84866137d9565b92808285019503920101835e51902090602081519101201490565b5050505f90565b805182118015614ad8575b614a9c576001821180614aa4575b158015908160011b918204600214171561359f576028018060281161359f578203614a9c576001600160a01b0392915f614a9692614c3f565b90921690565b50505f905f90565b5061060f60f31b7fffff00000000000000000000000000000000000000000000000000000000000060208301511614614a5d565b505f614a4f565b60ff5f5160206157dc5f395f51905f525460401c1615614afb57565b7fd7e6bcf8000000000000000000000000000000000000000000000000000000005f5260045ffd5b906060915a62019c73808210614b85575050815f9291836020819451930191620186a0f1918215614b5057565b3d91506101008211614b7b575b602060405192808452805f8386013e601f01601f1916830101604052565b6101009150614b5d565b7ffadae36e000000000000000000000000000000000000000000000000000000005f5260045260245260445ffd5b90614bf05750805115614bc857602081519101fd5b7fd6bda275000000000000000000000000000000000000000000000000000000005f5260045ffd5b81511580614c36575b614c01575090565b6001600160a01b03907f9996b315000000000000000000000000000000000000000000000000000000005f521660045260245ffd5b50803b15614bf9565b9290926001840180851161359f57831180614cf6575b158015908160011b918204600214171561359f57614c78905f9492939495613592565b915b818310614c8a5750505060019190565b9092919360ff614cc17fff000000000000000000000000000000000000000000000000000000000000006020888601015116614e85565b16600f8111614ceb578160041b918083046010149015171561359f57600191019401919290614c7a565b505f94508493505050565b5061060f60f31b7fffff000000000000000000000000000000000000000000000000000000000000602086840101511614614c55565b60205f604051828101906301ffc9a760e01b82526301ffc9a760e01b602482015260248152614d5c6044826130ea565b519084617530fa903d5f519083614e79575b5082614e6f575b5081614e07575b81614d85575090565b602091505f90604051838101906301ffc9a760e01b82526001600160e01b03197fd3ce6f1b0000000000000000000000000000000000000000000000000000000016602482015260248152614ddb6044826130ea565b5191617530fa5f513d82614dfb575b5081614df4575090565b9050151590565b6020111591505f614dea565b905060205f604051828101906301ffc9a760e01b82526001600160e01b0319602482015260248152614e3a6044826130ea565b519084617530fa5f513d82614e63575b5081614e59575b501590614d7c565b905015155f614e51565b6020111591505f614e4a565b151591505f614d75565b6020111592505f614d6e565b60f81c602f811180614eed575b15614ea157602f190160ff1690565b6060811180614ee3575b15614eba576056190160ff1690565b6040811180614ed9575b15614ed3576036190160ff1690565b5060ff90565b5060478110614ec4565b5060678110614eab565b50603a8110614e9256fe60803461013457601f61040e38819003918201601f19168301916001600160401b03831184841017610138578084926040948552833981010312610134576100468161014c565b906001600160a01b039061005c9060200161014c565b16908115610121575f80546001600160a01b031981168417825560405193916001600160a01b03909116907f8be0079c531659141344cd1fd0a4f28419497f9722a3daafe3b4186f6b6457e09080a3803b1561010157600180546001600160a01b0319166001600160a01b039290921691821790557fbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b5f80a26102ad90816101618239f35b63211eb15960e21b5f9081526001600160a01b0391909116600452602490fd5b631e4fbdf760e01b5f525f60045260245ffd5b5f80fd5b634e487b7160e01b5f52604160045260245ffd5b51906001600160a01b03821682036101345756fe60806040526004361015610011575f80fd5b5f3560e01c80633659cfe6146101af5780635c60da1b14610189578063715018a6146101255780638da5cb5b146101005763f2fde38b14610050575f80fd5b346100fc5760203660031901126100fc576004356001600160a01b0381168091036100fc5761007d610253565b80156100d0576001600160a01b035f548273ffffffffffffffffffffffffffffffffffffffff198216175f55167f8be0079c531659141344cd1fd0a4f28419497f9722a3daafe3b4186f6b6457e05f80a3005b7f1e4fbdf7000000000000000000000000000000000000000000000000000000005f525f60045260245ffd5b5f80fd5b346100fc575f3660031901126100fc5760206001600160a01b035f5416604051908152f35b346100fc575f3660031901126100fc5761013d610253565b5f6001600160a01b03815473ffffffffffffffffffffffffffffffffffffffff1981168355167f8be0079c531659141344cd1fd0a4f28419497f9722a3daafe3b4186f6b6457e08280a3005b346100fc575f3660031901126100fc5760206001600160a01b0360015416604051908152f35b346100fc5760203660031901126100fc576004356001600160a01b038116908181036100fc576101dd610253565b3b15610228578073ffffffffffffffffffffffffffffffffffffffff1960015416176001557fbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b5f80a2005b7f847ac564000000000000000000000000000000000000000000000000000000005f5260045260245ffd5b6001600160a01b035f5416330361026657565b7f118cdaa7000000000000000000000000000000000000000000000000000000005f523360045260245ffdfea26c6578706572696d656e74616cf564736f6c6343000825001860a0806040526104b680380380916100178285610292565b833981016040828203126101eb5761002e826102c9565b602083015190926001600160401b0382116101eb57019080601f830112156101eb57815161005b816102dd565b926100696040519485610292565b8184526020840192602083830101116101eb57815f926020809301855e84010152823b15610274577fa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d5080546001600160a01b0319166001600160a01b038516908117909155604051635c60da1b60e01b8152909190602081600481865afa9081156101f7575f9161023a575b50803b1561021a5750817f1cf3b03a6cf19fa2baba4df148e9dcabedea7f8a5c07840e207e5c089be95d3e5f80a282511561020257602060049260405193848092635c60da1b60e01b82525afa9182156101f7575f926101ae575b505f809161018a945190845af43d156101a6573d9161016e836102dd565b9261017c6040519485610292565b83523d5f602085013e6102f8565b505b60805260405161015f908161035782396080518160460152f35b6060916102f8565b9291506020833d6020116101ef575b816101ca60209383610292565b810103126101eb575f80916101e161018a956102c9565b9394509150610150565b5f80fd5b3d91506101bd565b6040513d5f823e3d90fd5b505050341561018c5763b398979f60e01b5f5260045ffd5b634c9c8ce360e01b5f9081526001600160a01b0391909116600452602490fd5b90506020813d60201161026c575b8161025560209383610292565b810103126101eb57610266906102c9565b5f6100f5565b3d9150610248565b631933b43b60e21b5f9081526001600160a01b038416600452602490fd5b601f909101601f19168101906001600160401b038211908210176102b557604052565b634e487b7160e01b5f52604160045260245ffd5b51906001600160a01b03821682036101eb57565b6001600160401b0381116102b557601f01601f191660200190565b9061031c575080511561030d57602081519101fd5b63d6bda27560e01b5f5260045ffd5b8151158061034d575b61032d575090565b639996b31560e01b5f9081526001600160a01b0391909116600452602490fd5b50803b1561032556fe60806040527f5c60da1b000000000000000000000000000000000000000000000000000000006080526020608060048173ffffffffffffffffffffffffffffffffffffffff7f0000000000000000000000000000000000000000000000000000000000000000165afa80156100e9575f9015610127575060203d6020116100e2575b601f19601f820116608001906080821067ffffffffffffffff8311176100b5576100b0916040526080016100f4565b610127565b7f4e487b71000000000000000000000000000000000000000000000000000000005f52604160045260245ffd5b503d610081565b6040513d5f823e3d90fd5b602090607f1901126101235760805173ffffffffffffffffffffffffffffffffffffffff811681036101235790565b5f80fd5b5f8091368280378136915af43d5f803e15610140573d5ff35b3d5ffdfea26c6578706572696d656e74616cf564736f6c63430008250018f3177357ab46d8af007ab3fdb9af81da189e1068fefdc0073dca88a2cab40a00f0c57e16840df040f15088dc2f81fe391c3923bec73e23a9662efc9c229c6a00a26c6578706572696d656e74616cf564736f6c63430008250018f0c57e16840df040f15088dc2f81fe391c3923bec73e23a9662efc9c229c6a00",
}

// ContractICS20TransferABI is the input ABI used to generate the binding from.
// Deprecated: Use ContractICS20TransferMetaData.ABI instead.
var ContractICS20TransferABI = ContractICS20TransferMetaData.ABI

// ContractICS20TransferBin is the compiled bytecode used for deploying new contracts.
// Deprecated: Use ContractICS20TransferMetaData.Bin instead.
var ContractICS20TransferBin = ContractICS20TransferMetaData.Bin

// DeployContractICS20Transfer deploys a new Ethereum contract, binding an instance of ContractICS20Transfer to it.
func DeployContractICS20Transfer(auth *bind.TransactOpts, backend bind.ContractBackend) (common.Address, *types.Transaction, *ContractICS20Transfer, error) {
	parsed, err := ContractICS20TransferMetaData.GetAbi()
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	if parsed == nil {
		return common.Address{}, nil, nil, errors.New("GetABI returned nil")
	}

	address, tx, contract, err := bind.DeployContract(auth, *parsed, common.FromHex(ContractICS20TransferBin), backend)
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	return address, tx, &ContractICS20Transfer{ContractICS20TransferCaller: ContractICS20TransferCaller{contract: contract}, ContractICS20TransferTransactor: ContractICS20TransferTransactor{contract: contract}, ContractICS20TransferFilterer: ContractICS20TransferFilterer{contract: contract}}, nil
}

// ContractICS20Transfer is an auto generated Go binding around an Ethereum contract.
type ContractICS20Transfer struct {
	ContractICS20TransferCaller     // Read-only binding to the contract
	ContractICS20TransferTransactor // Write-only binding to the contract
	ContractICS20TransferFilterer   // Log filterer for contract events
}

// ContractICS20TransferCaller is an auto generated read-only Go binding around an Ethereum contract.
type ContractICS20TransferCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractICS20TransferTransactor is an auto generated write-only Go binding around an Ethereum contract.
type ContractICS20TransferTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractICS20TransferFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type ContractICS20TransferFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractICS20TransferSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type ContractICS20TransferSession struct {
	Contract     *ContractICS20Transfer // Generic contract binding to set the session for
	CallOpts     bind.CallOpts          // Call options to use throughout this session
	TransactOpts bind.TransactOpts      // Transaction auth options to use throughout this session
}

// ContractICS20TransferCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type ContractICS20TransferCallerSession struct {
	Contract *ContractICS20TransferCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts                // Call options to use throughout this session
}

// ContractICS20TransferTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type ContractICS20TransferTransactorSession struct {
	Contract     *ContractICS20TransferTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts                // Transaction auth options to use throughout this session
}

// ContractICS20TransferRaw is an auto generated low-level Go binding around an Ethereum contract.
type ContractICS20TransferRaw struct {
	Contract *ContractICS20Transfer // Generic contract binding to access the raw methods on
}

// ContractICS20TransferCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type ContractICS20TransferCallerRaw struct {
	Contract *ContractICS20TransferCaller // Generic read-only contract binding to access the raw methods on
}

// ContractICS20TransferTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type ContractICS20TransferTransactorRaw struct {
	Contract *ContractICS20TransferTransactor // Generic write-only contract binding to access the raw methods on
}

// NewContractICS20Transfer creates a new instance of ContractICS20Transfer, bound to a specific deployed contract.
func NewContractICS20Transfer(address common.Address, backend bind.ContractBackend) (*ContractICS20Transfer, error) {
	contract, err := bindContractICS20Transfer(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &ContractICS20Transfer{ContractICS20TransferCaller: ContractICS20TransferCaller{contract: contract}, ContractICS20TransferTransactor: ContractICS20TransferTransactor{contract: contract}, ContractICS20TransferFilterer: ContractICS20TransferFilterer{contract: contract}}, nil
}

// NewContractICS20TransferCaller creates a new read-only instance of ContractICS20Transfer, bound to a specific deployed contract.
func NewContractICS20TransferCaller(address common.Address, caller bind.ContractCaller) (*ContractICS20TransferCaller, error) {
	contract, err := bindContractICS20Transfer(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferCaller{contract: contract}, nil
}

// NewContractICS20TransferTransactor creates a new write-only instance of ContractICS20Transfer, bound to a specific deployed contract.
func NewContractICS20TransferTransactor(address common.Address, transactor bind.ContractTransactor) (*ContractICS20TransferTransactor, error) {
	contract, err := bindContractICS20Transfer(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferTransactor{contract: contract}, nil
}

// NewContractICS20TransferFilterer creates a new log filterer instance of ContractICS20Transfer, bound to a specific deployed contract.
func NewContractICS20TransferFilterer(address common.Address, filterer bind.ContractFilterer) (*ContractICS20TransferFilterer, error) {
	contract, err := bindContractICS20Transfer(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferFilterer{contract: contract}, nil
}

// bindContractICS20Transfer binds a generic wrapper to an already deployed contract.
func bindContractICS20Transfer(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := ContractICS20TransferMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ContractICS20Transfer *ContractICS20TransferRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ContractICS20Transfer.Contract.ContractICS20TransferCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ContractICS20Transfer *ContractICS20TransferRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.ContractICS20TransferTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ContractICS20Transfer *ContractICS20TransferRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.ContractICS20TransferTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ContractICS20Transfer *ContractICS20TransferCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ContractICS20Transfer.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ContractICS20Transfer *ContractICS20TransferTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ContractICS20Transfer *ContractICS20TransferTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.contract.Transact(opts, method, params...)
}

// UPGRADEINTERFACEVERSION is a free data retrieval call binding the contract method 0xad3cb1cc.
//
// Solidity: function UPGRADE_INTERFACE_VERSION() view returns(string)
func (_ContractICS20Transfer *ContractICS20TransferCaller) UPGRADEINTERFACEVERSION(opts *bind.CallOpts) (string, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "UPGRADE_INTERFACE_VERSION")

	if err != nil {
		return *new(string), err
	}

	out0 := *abi.ConvertType(out[0], new(string)).(*string)

	return out0, err

}

// UPGRADEINTERFACEVERSION is a free data retrieval call binding the contract method 0xad3cb1cc.
//
// Solidity: function UPGRADE_INTERFACE_VERSION() view returns(string)
func (_ContractICS20Transfer *ContractICS20TransferSession) UPGRADEINTERFACEVERSION() (string, error) {
	return _ContractICS20Transfer.Contract.UPGRADEINTERFACEVERSION(&_ContractICS20Transfer.CallOpts)
}

// UPGRADEINTERFACEVERSION is a free data retrieval call binding the contract method 0xad3cb1cc.
//
// Solidity: function UPGRADE_INTERFACE_VERSION() view returns(string)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) UPGRADEINTERFACEVERSION() (string, error) {
	return _ContractICS20Transfer.Contract.UPGRADEINTERFACEVERSION(&_ContractICS20Transfer.CallOpts)
}

// Authority is a free data retrieval call binding the contract method 0xbf7e214f.
//
// Solidity: function authority() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) Authority(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "authority")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// Authority is a free data retrieval call binding the contract method 0xbf7e214f.
//
// Solidity: function authority() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) Authority() (common.Address, error) {
	return _ContractICS20Transfer.Contract.Authority(&_ContractICS20Transfer.CallOpts)
}

// Authority is a free data retrieval call binding the contract method 0xbf7e214f.
//
// Solidity: function authority() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) Authority() (common.Address, error) {
	return _ContractICS20Transfer.Contract.Authority(&_ContractICS20Transfer.CallOpts)
}

// GetEscrow is a free data retrieval call binding the contract method 0x969631d5.
//
// Solidity: function getEscrow(string clientId) view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) GetEscrow(opts *bind.CallOpts, clientId string) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "getEscrow", clientId)

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// GetEscrow is a free data retrieval call binding the contract method 0x969631d5.
//
// Solidity: function getEscrow(string clientId) view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) GetEscrow(clientId string) (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetEscrow(&_ContractICS20Transfer.CallOpts, clientId)
}

// GetEscrow is a free data retrieval call binding the contract method 0x969631d5.
//
// Solidity: function getEscrow(string clientId) view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) GetEscrow(clientId string) (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetEscrow(&_ContractICS20Transfer.CallOpts, clientId)
}

// GetEscrowBeacon is a free data retrieval call binding the contract method 0x1e5150e4.
//
// Solidity: function getEscrowBeacon() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) GetEscrowBeacon(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "getEscrowBeacon")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// GetEscrowBeacon is a free data retrieval call binding the contract method 0x1e5150e4.
//
// Solidity: function getEscrowBeacon() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) GetEscrowBeacon() (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetEscrowBeacon(&_ContractICS20Transfer.CallOpts)
}

// GetEscrowBeacon is a free data retrieval call binding the contract method 0x1e5150e4.
//
// Solidity: function getEscrowBeacon() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) GetEscrowBeacon() (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetEscrowBeacon(&_ContractICS20Transfer.CallOpts)
}

// GetIBCERC20Beacon is a free data retrieval call binding the contract method 0x826cae7a.
//
// Solidity: function getIBCERC20Beacon() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) GetIBCERC20Beacon(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "getIBCERC20Beacon")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// GetIBCERC20Beacon is a free data retrieval call binding the contract method 0x826cae7a.
//
// Solidity: function getIBCERC20Beacon() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) GetIBCERC20Beacon() (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetIBCERC20Beacon(&_ContractICS20Transfer.CallOpts)
}

// GetIBCERC20Beacon is a free data retrieval call binding the contract method 0x826cae7a.
//
// Solidity: function getIBCERC20Beacon() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) GetIBCERC20Beacon() (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetIBCERC20Beacon(&_ContractICS20Transfer.CallOpts)
}

// GetPermit2 is a free data retrieval call binding the contract method 0x1bbf2e23.
//
// Solidity: function getPermit2() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) GetPermit2(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "getPermit2")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// GetPermit2 is a free data retrieval call binding the contract method 0x1bbf2e23.
//
// Solidity: function getPermit2() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) GetPermit2() (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetPermit2(&_ContractICS20Transfer.CallOpts)
}

// GetPermit2 is a free data retrieval call binding the contract method 0x1bbf2e23.
//
// Solidity: function getPermit2() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) GetPermit2() (common.Address, error) {
	return _ContractICS20Transfer.Contract.GetPermit2(&_ContractICS20Transfer.CallOpts)
}

// IbcERC20Contract is a free data retrieval call binding the contract method 0xa50ee2b4.
//
// Solidity: function ibcERC20Contract(string denom) view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) IbcERC20Contract(opts *bind.CallOpts, denom string) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "ibcERC20Contract", denom)

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// IbcERC20Contract is a free data retrieval call binding the contract method 0xa50ee2b4.
//
// Solidity: function ibcERC20Contract(string denom) view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) IbcERC20Contract(denom string) (common.Address, error) {
	return _ContractICS20Transfer.Contract.IbcERC20Contract(&_ContractICS20Transfer.CallOpts, denom)
}

// IbcERC20Contract is a free data retrieval call binding the contract method 0xa50ee2b4.
//
// Solidity: function ibcERC20Contract(string denom) view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) IbcERC20Contract(denom string) (common.Address, error) {
	return _ContractICS20Transfer.Contract.IbcERC20Contract(&_ContractICS20Transfer.CallOpts, denom)
}

// IbcERC20Denom is a free data retrieval call binding the contract method 0xe163b1af.
//
// Solidity: function ibcERC20Denom(address token) view returns(string)
func (_ContractICS20Transfer *ContractICS20TransferCaller) IbcERC20Denom(opts *bind.CallOpts, token common.Address) (string, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "ibcERC20Denom", token)

	if err != nil {
		return *new(string), err
	}

	out0 := *abi.ConvertType(out[0], new(string)).(*string)

	return out0, err

}

// IbcERC20Denom is a free data retrieval call binding the contract method 0xe163b1af.
//
// Solidity: function ibcERC20Denom(address token) view returns(string)
func (_ContractICS20Transfer *ContractICS20TransferSession) IbcERC20Denom(token common.Address) (string, error) {
	return _ContractICS20Transfer.Contract.IbcERC20Denom(&_ContractICS20Transfer.CallOpts, token)
}

// IbcERC20Denom is a free data retrieval call binding the contract method 0xe163b1af.
//
// Solidity: function ibcERC20Denom(address token) view returns(string)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) IbcERC20Denom(token common.Address) (string, error) {
	return _ContractICS20Transfer.Contract.IbcERC20Denom(&_ContractICS20Transfer.CallOpts, token)
}

// Ics26 is a free data retrieval call binding the contract method 0xd413227d.
//
// Solidity: function ics26() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCaller) Ics26(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "ics26")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// Ics26 is a free data retrieval call binding the contract method 0xd413227d.
//
// Solidity: function ics26() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferSession) Ics26() (common.Address, error) {
	return _ContractICS20Transfer.Contract.Ics26(&_ContractICS20Transfer.CallOpts)
}

// Ics26 is a free data retrieval call binding the contract method 0xd413227d.
//
// Solidity: function ics26() view returns(address)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) Ics26() (common.Address, error) {
	return _ContractICS20Transfer.Contract.Ics26(&_ContractICS20Transfer.CallOpts)
}

// IsConsumingScheduledOp is a free data retrieval call binding the contract method 0x8fb36037.
//
// Solidity: function isConsumingScheduledOp() view returns(bytes4)
func (_ContractICS20Transfer *ContractICS20TransferCaller) IsConsumingScheduledOp(opts *bind.CallOpts) ([4]byte, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "isConsumingScheduledOp")

	if err != nil {
		return *new([4]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([4]byte)).(*[4]byte)

	return out0, err

}

// IsConsumingScheduledOp is a free data retrieval call binding the contract method 0x8fb36037.
//
// Solidity: function isConsumingScheduledOp() view returns(bytes4)
func (_ContractICS20Transfer *ContractICS20TransferSession) IsConsumingScheduledOp() ([4]byte, error) {
	return _ContractICS20Transfer.Contract.IsConsumingScheduledOp(&_ContractICS20Transfer.CallOpts)
}

// IsConsumingScheduledOp is a free data retrieval call binding the contract method 0x8fb36037.
//
// Solidity: function isConsumingScheduledOp() view returns(bytes4)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) IsConsumingScheduledOp() ([4]byte, error) {
	return _ContractICS20Transfer.Contract.IsConsumingScheduledOp(&_ContractICS20Transfer.CallOpts)
}

// IsEscrowActive is a free data retrieval call binding the contract method 0x57a85bea.
//
// Solidity: function isEscrowActive(string clientId) view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferCaller) IsEscrowActive(opts *bind.CallOpts, clientId string) (bool, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "isEscrowActive", clientId)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsEscrowActive is a free data retrieval call binding the contract method 0x57a85bea.
//
// Solidity: function isEscrowActive(string clientId) view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferSession) IsEscrowActive(clientId string) (bool, error) {
	return _ContractICS20Transfer.Contract.IsEscrowActive(&_ContractICS20Transfer.CallOpts, clientId)
}

// IsEscrowActive is a free data retrieval call binding the contract method 0x57a85bea.
//
// Solidity: function isEscrowActive(string clientId) view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) IsEscrowActive(clientId string) (bool, error) {
	return _ContractICS20Transfer.Contract.IsEscrowActive(&_ContractICS20Transfer.CallOpts, clientId)
}

// Paused is a free data retrieval call binding the contract method 0x5c975abb.
//
// Solidity: function paused() view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferCaller) Paused(opts *bind.CallOpts) (bool, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "paused")

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// Paused is a free data retrieval call binding the contract method 0x5c975abb.
//
// Solidity: function paused() view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferSession) Paused() (bool, error) {
	return _ContractICS20Transfer.Contract.Paused(&_ContractICS20Transfer.CallOpts)
}

// Paused is a free data retrieval call binding the contract method 0x5c975abb.
//
// Solidity: function paused() view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) Paused() (bool, error) {
	return _ContractICS20Transfer.Contract.Paused(&_ContractICS20Transfer.CallOpts)
}

// ProxiableUUID is a free data retrieval call binding the contract method 0x52d1902d.
//
// Solidity: function proxiableUUID() view returns(bytes32)
func (_ContractICS20Transfer *ContractICS20TransferCaller) ProxiableUUID(opts *bind.CallOpts) ([32]byte, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "proxiableUUID")

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// ProxiableUUID is a free data retrieval call binding the contract method 0x52d1902d.
//
// Solidity: function proxiableUUID() view returns(bytes32)
func (_ContractICS20Transfer *ContractICS20TransferSession) ProxiableUUID() ([32]byte, error) {
	return _ContractICS20Transfer.Contract.ProxiableUUID(&_ContractICS20Transfer.CallOpts)
}

// ProxiableUUID is a free data retrieval call binding the contract method 0x52d1902d.
//
// Solidity: function proxiableUUID() view returns(bytes32)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) ProxiableUUID() ([32]byte, error) {
	return _ContractICS20Transfer.Contract.ProxiableUUID(&_ContractICS20Transfer.CallOpts)
}

// RequiresPrecreatedEscrows is a free data retrieval call binding the contract method 0x1cf0a67f.
//
// Solidity: function requiresPrecreatedEscrows() view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferCaller) RequiresPrecreatedEscrows(opts *bind.CallOpts) (bool, error) {
	var out []interface{}
	err := _ContractICS20Transfer.contract.Call(opts, &out, "requiresPrecreatedEscrows")

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// RequiresPrecreatedEscrows is a free data retrieval call binding the contract method 0x1cf0a67f.
//
// Solidity: function requiresPrecreatedEscrows() view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferSession) RequiresPrecreatedEscrows() (bool, error) {
	return _ContractICS20Transfer.Contract.RequiresPrecreatedEscrows(&_ContractICS20Transfer.CallOpts)
}

// RequiresPrecreatedEscrows is a free data retrieval call binding the contract method 0x1cf0a67f.
//
// Solidity: function requiresPrecreatedEscrows() view returns(bool)
func (_ContractICS20Transfer *ContractICS20TransferCallerSession) RequiresPrecreatedEscrows() (bool, error) {
	return _ContractICS20Transfer.Contract.RequiresPrecreatedEscrows(&_ContractICS20Transfer.CallOpts)
}

// ActivateEscrow is a paid mutator transaction binding the contract method 0x93dcfee4.
//
// Solidity: function activateEscrow(string clientId, address[] tokens) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) ActivateEscrow(opts *bind.TransactOpts, clientId string, tokens []common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "activateEscrow", clientId, tokens)
}

// ActivateEscrow is a paid mutator transaction binding the contract method 0x93dcfee4.
//
// Solidity: function activateEscrow(string clientId, address[] tokens) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) ActivateEscrow(clientId string, tokens []common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.ActivateEscrow(&_ContractICS20Transfer.TransactOpts, clientId, tokens)
}

// ActivateEscrow is a paid mutator transaction binding the contract method 0x93dcfee4.
//
// Solidity: function activateEscrow(string clientId, address[] tokens) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) ActivateEscrow(clientId string, tokens []common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.ActivateEscrow(&_ContractICS20Transfer.TransactOpts, clientId, tokens)
}

// CreateEscrow is a paid mutator transaction binding the contract method 0x90f6d620.
//
// Solidity: function createEscrow(string clientId) returns(address escrow)
func (_ContractICS20Transfer *ContractICS20TransferTransactor) CreateEscrow(opts *bind.TransactOpts, clientId string) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "createEscrow", clientId)
}

// CreateEscrow is a paid mutator transaction binding the contract method 0x90f6d620.
//
// Solidity: function createEscrow(string clientId) returns(address escrow)
func (_ContractICS20Transfer *ContractICS20TransferSession) CreateEscrow(clientId string) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.CreateEscrow(&_ContractICS20Transfer.TransactOpts, clientId)
}

// CreateEscrow is a paid mutator transaction binding the contract method 0x90f6d620.
//
// Solidity: function createEscrow(string clientId) returns(address escrow)
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) CreateEscrow(clientId string) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.CreateEscrow(&_ContractICS20Transfer.TransactOpts, clientId)
}

// EnableEscrowLaunchGate is a paid mutator transaction binding the contract method 0xdab8ed9c.
//
// Solidity: function enableEscrowLaunchGate() returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) EnableEscrowLaunchGate(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "enableEscrowLaunchGate")
}

// EnableEscrowLaunchGate is a paid mutator transaction binding the contract method 0xdab8ed9c.
//
// Solidity: function enableEscrowLaunchGate() returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) EnableEscrowLaunchGate() (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.EnableEscrowLaunchGate(&_ContractICS20Transfer.TransactOpts)
}

// EnableEscrowLaunchGate is a paid mutator transaction binding the contract method 0xdab8ed9c.
//
// Solidity: function enableEscrowLaunchGate() returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) EnableEscrowLaunchGate() (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.EnableEscrowLaunchGate(&_ContractICS20Transfer.TransactOpts)
}

// Initialize is a paid mutator transaction binding the contract method 0x1459457a.
//
// Solidity: function initialize(address ics26Router, address escrowLogic, address ibcERC20Logic, address permit2, address authority) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) Initialize(opts *bind.TransactOpts, ics26Router common.Address, escrowLogic common.Address, ibcERC20Logic common.Address, permit2 common.Address, authority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "initialize", ics26Router, escrowLogic, ibcERC20Logic, permit2, authority)
}

// Initialize is a paid mutator transaction binding the contract method 0x1459457a.
//
// Solidity: function initialize(address ics26Router, address escrowLogic, address ibcERC20Logic, address permit2, address authority) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) Initialize(ics26Router common.Address, escrowLogic common.Address, ibcERC20Logic common.Address, permit2 common.Address, authority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Initialize(&_ContractICS20Transfer.TransactOpts, ics26Router, escrowLogic, ibcERC20Logic, permit2, authority)
}

// Initialize is a paid mutator transaction binding the contract method 0x1459457a.
//
// Solidity: function initialize(address ics26Router, address escrowLogic, address ibcERC20Logic, address permit2, address authority) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) Initialize(ics26Router common.Address, escrowLogic common.Address, ibcERC20Logic common.Address, permit2 common.Address, authority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Initialize(&_ContractICS20Transfer.TransactOpts, ics26Router, escrowLogic, ibcERC20Logic, permit2, authority)
}

// InitializeV2 is a paid mutator transaction binding the contract method 0x29b6eca9.
//
// Solidity: function initializeV2(address authority) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) InitializeV2(opts *bind.TransactOpts, authority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "initializeV2", authority)
}

// InitializeV2 is a paid mutator transaction binding the contract method 0x29b6eca9.
//
// Solidity: function initializeV2(address authority) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) InitializeV2(authority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.InitializeV2(&_ContractICS20Transfer.TransactOpts, authority)
}

// InitializeV2 is a paid mutator transaction binding the contract method 0x29b6eca9.
//
// Solidity: function initializeV2(address authority) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) InitializeV2(authority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.InitializeV2(&_ContractICS20Transfer.TransactOpts, authority)
}

// Multicall is a paid mutator transaction binding the contract method 0xac9650d8.
//
// Solidity: function multicall(bytes[] data) returns(bytes[] results)
func (_ContractICS20Transfer *ContractICS20TransferTransactor) Multicall(opts *bind.TransactOpts, data [][]byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "multicall", data)
}

// Multicall is a paid mutator transaction binding the contract method 0xac9650d8.
//
// Solidity: function multicall(bytes[] data) returns(bytes[] results)
func (_ContractICS20Transfer *ContractICS20TransferSession) Multicall(data [][]byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Multicall(&_ContractICS20Transfer.TransactOpts, data)
}

// Multicall is a paid mutator transaction binding the contract method 0xac9650d8.
//
// Solidity: function multicall(bytes[] data) returns(bytes[] results)
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) Multicall(data [][]byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Multicall(&_ContractICS20Transfer.TransactOpts, data)
}

// OnAcknowledgementPacket is a paid mutator transaction binding the contract method 0x428e4e17.
//
// Solidity: function onAcknowledgementPacket((string,string,uint64,(string,string,string,string,bytes),bytes,address) msg_) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) OnAcknowledgementPacket(opts *bind.TransactOpts, msg_ IIBCAppCallbacksOnAcknowledgementPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "onAcknowledgementPacket", msg_)
}

// OnAcknowledgementPacket is a paid mutator transaction binding the contract method 0x428e4e17.
//
// Solidity: function onAcknowledgementPacket((string,string,uint64,(string,string,string,string,bytes),bytes,address) msg_) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) OnAcknowledgementPacket(msg_ IIBCAppCallbacksOnAcknowledgementPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.OnAcknowledgementPacket(&_ContractICS20Transfer.TransactOpts, msg_)
}

// OnAcknowledgementPacket is a paid mutator transaction binding the contract method 0x428e4e17.
//
// Solidity: function onAcknowledgementPacket((string,string,uint64,(string,string,string,string,bytes),bytes,address) msg_) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) OnAcknowledgementPacket(msg_ IIBCAppCallbacksOnAcknowledgementPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.OnAcknowledgementPacket(&_ContractICS20Transfer.TransactOpts, msg_)
}

// OnRecvPacket is a paid mutator transaction binding the contract method 0x078c4a79.
//
// Solidity: function onRecvPacket((string,string,uint64,(string,string,string,string,bytes),address) msg_) returns(bytes)
func (_ContractICS20Transfer *ContractICS20TransferTransactor) OnRecvPacket(opts *bind.TransactOpts, msg_ IIBCAppCallbacksOnRecvPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "onRecvPacket", msg_)
}

// OnRecvPacket is a paid mutator transaction binding the contract method 0x078c4a79.
//
// Solidity: function onRecvPacket((string,string,uint64,(string,string,string,string,bytes),address) msg_) returns(bytes)
func (_ContractICS20Transfer *ContractICS20TransferSession) OnRecvPacket(msg_ IIBCAppCallbacksOnRecvPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.OnRecvPacket(&_ContractICS20Transfer.TransactOpts, msg_)
}

// OnRecvPacket is a paid mutator transaction binding the contract method 0x078c4a79.
//
// Solidity: function onRecvPacket((string,string,uint64,(string,string,string,string,bytes),address) msg_) returns(bytes)
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) OnRecvPacket(msg_ IIBCAppCallbacksOnRecvPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.OnRecvPacket(&_ContractICS20Transfer.TransactOpts, msg_)
}

// OnTimeoutPacket is a paid mutator transaction binding the contract method 0x5e32b6b6.
//
// Solidity: function onTimeoutPacket((string,string,uint64,(string,string,string,string,bytes),address) msg_) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) OnTimeoutPacket(opts *bind.TransactOpts, msg_ IIBCAppCallbacksOnTimeoutPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "onTimeoutPacket", msg_)
}

// OnTimeoutPacket is a paid mutator transaction binding the contract method 0x5e32b6b6.
//
// Solidity: function onTimeoutPacket((string,string,uint64,(string,string,string,string,bytes),address) msg_) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) OnTimeoutPacket(msg_ IIBCAppCallbacksOnTimeoutPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.OnTimeoutPacket(&_ContractICS20Transfer.TransactOpts, msg_)
}

// OnTimeoutPacket is a paid mutator transaction binding the contract method 0x5e32b6b6.
//
// Solidity: function onTimeoutPacket((string,string,uint64,(string,string,string,string,bytes),address) msg_) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) OnTimeoutPacket(msg_ IIBCAppCallbacksOnTimeoutPacketCallback) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.OnTimeoutPacket(&_ContractICS20Transfer.TransactOpts, msg_)
}

// Pause is a paid mutator transaction binding the contract method 0x8456cb59.
//
// Solidity: function pause() returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) Pause(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "pause")
}

// Pause is a paid mutator transaction binding the contract method 0x8456cb59.
//
// Solidity: function pause() returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) Pause() (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Pause(&_ContractICS20Transfer.TransactOpts)
}

// Pause is a paid mutator transaction binding the contract method 0x8456cb59.
//
// Solidity: function pause() returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) Pause() (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Pause(&_ContractICS20Transfer.TransactOpts)
}

// SendTransfer is a paid mutator transaction binding the contract method 0x2ac3dc38.
//
// Solidity: function sendTransfer((address,uint256,string,string,string,uint64,string) msg_) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferTransactor) SendTransfer(opts *bind.TransactOpts, msg_ IICS20TransferMsgsSendTransferMsg) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "sendTransfer", msg_)
}

// SendTransfer is a paid mutator transaction binding the contract method 0x2ac3dc38.
//
// Solidity: function sendTransfer((address,uint256,string,string,string,uint64,string) msg_) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferSession) SendTransfer(msg_ IICS20TransferMsgsSendTransferMsg) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SendTransfer(&_ContractICS20Transfer.TransactOpts, msg_)
}

// SendTransfer is a paid mutator transaction binding the contract method 0x2ac3dc38.
//
// Solidity: function sendTransfer((address,uint256,string,string,string,uint64,string) msg_) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) SendTransfer(msg_ IICS20TransferMsgsSendTransferMsg) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SendTransfer(&_ContractICS20Transfer.TransactOpts, msg_)
}

// SendTransferWithPermit2 is a paid mutator transaction binding the contract method 0x53816a7c.
//
// Solidity: function sendTransferWithPermit2((address,uint256,string,string,string,uint64,string) msg_, ((address,uint256),uint256,uint256) permit, bytes signature) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferTransactor) SendTransferWithPermit2(opts *bind.TransactOpts, msg_ IICS20TransferMsgsSendTransferMsg, permit ISignatureTransferPermitTransferFrom, signature []byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "sendTransferWithPermit2", msg_, permit, signature)
}

// SendTransferWithPermit2 is a paid mutator transaction binding the contract method 0x53816a7c.
//
// Solidity: function sendTransferWithPermit2((address,uint256,string,string,string,uint64,string) msg_, ((address,uint256),uint256,uint256) permit, bytes signature) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferSession) SendTransferWithPermit2(msg_ IICS20TransferMsgsSendTransferMsg, permit ISignatureTransferPermitTransferFrom, signature []byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SendTransferWithPermit2(&_ContractICS20Transfer.TransactOpts, msg_, permit, signature)
}

// SendTransferWithPermit2 is a paid mutator transaction binding the contract method 0x53816a7c.
//
// Solidity: function sendTransferWithPermit2((address,uint256,string,string,string,uint64,string) msg_, ((address,uint256),uint256,uint256) permit, bytes signature) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) SendTransferWithPermit2(msg_ IICS20TransferMsgsSendTransferMsg, permit ISignatureTransferPermitTransferFrom, signature []byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SendTransferWithPermit2(&_ContractICS20Transfer.TransactOpts, msg_, permit, signature)
}

// SendTransferWithSender is a paid mutator transaction binding the contract method 0xb29c715d.
//
// Solidity: function sendTransferWithSender((address,uint256,string,string,string,uint64,string) msg_, address sender) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferTransactor) SendTransferWithSender(opts *bind.TransactOpts, msg_ IICS20TransferMsgsSendTransferMsg, sender common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "sendTransferWithSender", msg_, sender)
}

// SendTransferWithSender is a paid mutator transaction binding the contract method 0xb29c715d.
//
// Solidity: function sendTransferWithSender((address,uint256,string,string,string,uint64,string) msg_, address sender) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferSession) SendTransferWithSender(msg_ IICS20TransferMsgsSendTransferMsg, sender common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SendTransferWithSender(&_ContractICS20Transfer.TransactOpts, msg_, sender)
}

// SendTransferWithSender is a paid mutator transaction binding the contract method 0xb29c715d.
//
// Solidity: function sendTransferWithSender((address,uint256,string,string,string,uint64,string) msg_, address sender) returns(uint64)
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) SendTransferWithSender(msg_ IICS20TransferMsgsSendTransferMsg, sender common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SendTransferWithSender(&_ContractICS20Transfer.TransactOpts, msg_, sender)
}

// SetAuthority is a paid mutator transaction binding the contract method 0x7a9e5e4b.
//
// Solidity: function setAuthority(address newAuthority) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) SetAuthority(opts *bind.TransactOpts, newAuthority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "setAuthority", newAuthority)
}

// SetAuthority is a paid mutator transaction binding the contract method 0x7a9e5e4b.
//
// Solidity: function setAuthority(address newAuthority) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) SetAuthority(newAuthority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SetAuthority(&_ContractICS20Transfer.TransactOpts, newAuthority)
}

// SetAuthority is a paid mutator transaction binding the contract method 0x7a9e5e4b.
//
// Solidity: function setAuthority(address newAuthority) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) SetAuthority(newAuthority common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SetAuthority(&_ContractICS20Transfer.TransactOpts, newAuthority)
}

// SetCustomERC20 is a paid mutator transaction binding the contract method 0xa1d28f57.
//
// Solidity: function setCustomERC20(string denom, address token) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) SetCustomERC20(opts *bind.TransactOpts, denom string, token common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "setCustomERC20", denom, token)
}

// SetCustomERC20 is a paid mutator transaction binding the contract method 0xa1d28f57.
//
// Solidity: function setCustomERC20(string denom, address token) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) SetCustomERC20(denom string, token common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SetCustomERC20(&_ContractICS20Transfer.TransactOpts, denom, token)
}

// SetCustomERC20 is a paid mutator transaction binding the contract method 0xa1d28f57.
//
// Solidity: function setCustomERC20(string denom, address token) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) SetCustomERC20(denom string, token common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SetCustomERC20(&_ContractICS20Transfer.TransactOpts, denom, token)
}

// SetIBCERC20Metadata is a paid mutator transaction binding the contract method 0x62bfa172.
//
// Solidity: function setIBCERC20Metadata(string denom, string name_, string symbol_, uint8 decimals_) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) SetIBCERC20Metadata(opts *bind.TransactOpts, denom string, name_ string, symbol_ string, decimals_ uint8) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "setIBCERC20Metadata", denom, name_, symbol_, decimals_)
}

// SetIBCERC20Metadata is a paid mutator transaction binding the contract method 0x62bfa172.
//
// Solidity: function setIBCERC20Metadata(string denom, string name_, string symbol_, uint8 decimals_) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) SetIBCERC20Metadata(denom string, name_ string, symbol_ string, decimals_ uint8) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SetIBCERC20Metadata(&_ContractICS20Transfer.TransactOpts, denom, name_, symbol_, decimals_)
}

// SetIBCERC20Metadata is a paid mutator transaction binding the contract method 0x62bfa172.
//
// Solidity: function setIBCERC20Metadata(string denom, string name_, string symbol_, uint8 decimals_) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) SetIBCERC20Metadata(denom string, name_ string, symbol_ string, decimals_ uint8) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.SetIBCERC20Metadata(&_ContractICS20Transfer.TransactOpts, denom, name_, symbol_, decimals_)
}

// Unpause is a paid mutator transaction binding the contract method 0x3f4ba83a.
//
// Solidity: function unpause() returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) Unpause(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "unpause")
}

// Unpause is a paid mutator transaction binding the contract method 0x3f4ba83a.
//
// Solidity: function unpause() returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) Unpause() (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Unpause(&_ContractICS20Transfer.TransactOpts)
}

// Unpause is a paid mutator transaction binding the contract method 0x3f4ba83a.
//
// Solidity: function unpause() returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) Unpause() (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.Unpause(&_ContractICS20Transfer.TransactOpts)
}

// UpgradeEscrowTo is a paid mutator transaction binding the contract method 0xaaa2c343.
//
// Solidity: function upgradeEscrowTo(address newEscrowLogic) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) UpgradeEscrowTo(opts *bind.TransactOpts, newEscrowLogic common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "upgradeEscrowTo", newEscrowLogic)
}

// UpgradeEscrowTo is a paid mutator transaction binding the contract method 0xaaa2c343.
//
// Solidity: function upgradeEscrowTo(address newEscrowLogic) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) UpgradeEscrowTo(newEscrowLogic common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.UpgradeEscrowTo(&_ContractICS20Transfer.TransactOpts, newEscrowLogic)
}

// UpgradeEscrowTo is a paid mutator transaction binding the contract method 0xaaa2c343.
//
// Solidity: function upgradeEscrowTo(address newEscrowLogic) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) UpgradeEscrowTo(newEscrowLogic common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.UpgradeEscrowTo(&_ContractICS20Transfer.TransactOpts, newEscrowLogic)
}

// UpgradeIBCERC20To is a paid mutator transaction binding the contract method 0x06ab20bc.
//
// Solidity: function upgradeIBCERC20To(address newIBCERC20Logic) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) UpgradeIBCERC20To(opts *bind.TransactOpts, newIBCERC20Logic common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "upgradeIBCERC20To", newIBCERC20Logic)
}

// UpgradeIBCERC20To is a paid mutator transaction binding the contract method 0x06ab20bc.
//
// Solidity: function upgradeIBCERC20To(address newIBCERC20Logic) returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) UpgradeIBCERC20To(newIBCERC20Logic common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.UpgradeIBCERC20To(&_ContractICS20Transfer.TransactOpts, newIBCERC20Logic)
}

// UpgradeIBCERC20To is a paid mutator transaction binding the contract method 0x06ab20bc.
//
// Solidity: function upgradeIBCERC20To(address newIBCERC20Logic) returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) UpgradeIBCERC20To(newIBCERC20Logic common.Address) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.UpgradeIBCERC20To(&_ContractICS20Transfer.TransactOpts, newIBCERC20Logic)
}

// UpgradeToAndCall is a paid mutator transaction binding the contract method 0x4f1ef286.
//
// Solidity: function upgradeToAndCall(address newImplementation, bytes data) payable returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactor) UpgradeToAndCall(opts *bind.TransactOpts, newImplementation common.Address, data []byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.contract.Transact(opts, "upgradeToAndCall", newImplementation, data)
}

// UpgradeToAndCall is a paid mutator transaction binding the contract method 0x4f1ef286.
//
// Solidity: function upgradeToAndCall(address newImplementation, bytes data) payable returns()
func (_ContractICS20Transfer *ContractICS20TransferSession) UpgradeToAndCall(newImplementation common.Address, data []byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.UpgradeToAndCall(&_ContractICS20Transfer.TransactOpts, newImplementation, data)
}

// UpgradeToAndCall is a paid mutator transaction binding the contract method 0x4f1ef286.
//
// Solidity: function upgradeToAndCall(address newImplementation, bytes data) payable returns()
func (_ContractICS20Transfer *ContractICS20TransferTransactorSession) UpgradeToAndCall(newImplementation common.Address, data []byte) (*types.Transaction, error) {
	return _ContractICS20Transfer.Contract.UpgradeToAndCall(&_ContractICS20Transfer.TransactOpts, newImplementation, data)
}

// ContractICS20TransferAuthorityUpdatedIterator is returned from FilterAuthorityUpdated and is used to iterate over the raw logs and unpacked data for AuthorityUpdated events raised by the ContractICS20Transfer contract.
type ContractICS20TransferAuthorityUpdatedIterator struct {
	Event *ContractICS20TransferAuthorityUpdated // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferAuthorityUpdatedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferAuthorityUpdated)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferAuthorityUpdated)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferAuthorityUpdatedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferAuthorityUpdatedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferAuthorityUpdated represents a AuthorityUpdated event raised by the ContractICS20Transfer contract.
type ContractICS20TransferAuthorityUpdated struct {
	Authority common.Address
	Raw       types.Log // Blockchain specific contextual infos
}

// FilterAuthorityUpdated is a free log retrieval operation binding the contract event 0x2f658b440c35314f52658ea8a740e05b284cdc84dc9ae01e891f21b8933e7cad.
//
// Solidity: event AuthorityUpdated(address authority)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterAuthorityUpdated(opts *bind.FilterOpts) (*ContractICS20TransferAuthorityUpdatedIterator, error) {

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "AuthorityUpdated")
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferAuthorityUpdatedIterator{contract: _ContractICS20Transfer.contract, event: "AuthorityUpdated", logs: logs, sub: sub}, nil
}

// WatchAuthorityUpdated is a free log subscription operation binding the contract event 0x2f658b440c35314f52658ea8a740e05b284cdc84dc9ae01e891f21b8933e7cad.
//
// Solidity: event AuthorityUpdated(address authority)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchAuthorityUpdated(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferAuthorityUpdated) (event.Subscription, error) {

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "AuthorityUpdated")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferAuthorityUpdated)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "AuthorityUpdated", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseAuthorityUpdated is a log parse operation binding the contract event 0x2f658b440c35314f52658ea8a740e05b284cdc84dc9ae01e891f21b8933e7cad.
//
// Solidity: event AuthorityUpdated(address authority)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseAuthorityUpdated(log types.Log) (*ContractICS20TransferAuthorityUpdated, error) {
	event := new(ContractICS20TransferAuthorityUpdated)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "AuthorityUpdated", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferIBCERC20ContractCreatedIterator is returned from FilterIBCERC20ContractCreated and is used to iterate over the raw logs and unpacked data for IBCERC20ContractCreated events raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCERC20ContractCreatedIterator struct {
	Event *ContractICS20TransferIBCERC20ContractCreated // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferIBCERC20ContractCreatedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferIBCERC20ContractCreated)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferIBCERC20ContractCreated)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferIBCERC20ContractCreatedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferIBCERC20ContractCreatedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferIBCERC20ContractCreated represents a IBCERC20ContractCreated event raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCERC20ContractCreated struct {
	ContractAddress common.Address
	FullDenomPath   string
	Raw             types.Log // Blockchain specific contextual infos
}

// FilterIBCERC20ContractCreated is a free log retrieval operation binding the contract event 0x6031fab685dd6d86e4dbac9a69eae347145f332c95b3a0d728d3730fc5233d62.
//
// Solidity: event IBCERC20ContractCreated(address indexed contractAddress, string fullDenomPath)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterIBCERC20ContractCreated(opts *bind.FilterOpts, contractAddress []common.Address) (*ContractICS20TransferIBCERC20ContractCreatedIterator, error) {

	var contractAddressRule []interface{}
	for _, contractAddressItem := range contractAddress {
		contractAddressRule = append(contractAddressRule, contractAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "IBCERC20ContractCreated", contractAddressRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferIBCERC20ContractCreatedIterator{contract: _ContractICS20Transfer.contract, event: "IBCERC20ContractCreated", logs: logs, sub: sub}, nil
}

// WatchIBCERC20ContractCreated is a free log subscription operation binding the contract event 0x6031fab685dd6d86e4dbac9a69eae347145f332c95b3a0d728d3730fc5233d62.
//
// Solidity: event IBCERC20ContractCreated(address indexed contractAddress, string fullDenomPath)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchIBCERC20ContractCreated(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferIBCERC20ContractCreated, contractAddress []common.Address) (event.Subscription, error) {

	var contractAddressRule []interface{}
	for _, contractAddressItem := range contractAddress {
		contractAddressRule = append(contractAddressRule, contractAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "IBCERC20ContractCreated", contractAddressRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferIBCERC20ContractCreated)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCERC20ContractCreated", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseIBCERC20ContractCreated is a log parse operation binding the contract event 0x6031fab685dd6d86e4dbac9a69eae347145f332c95b3a0d728d3730fc5233d62.
//
// Solidity: event IBCERC20ContractCreated(address indexed contractAddress, string fullDenomPath)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseIBCERC20ContractCreated(log types.Log) (*ContractICS20TransferIBCERC20ContractCreated, error) {
	event := new(ContractICS20TransferIBCERC20ContractCreated)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCERC20ContractCreated", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator is returned from FilterIBCSenderAckPacketCallbackError and is used to iterate over the raw logs and unpacked data for IBCSenderAckPacketCallbackError events raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator struct {
	Event *ContractICS20TransferIBCSenderAckPacketCallbackError // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferIBCSenderAckPacketCallbackError)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferIBCSenderAckPacketCallbackError)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferIBCSenderAckPacketCallbackError represents a IBCSenderAckPacketCallbackError event raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderAckPacketCallbackError struct {
	CallbackAddress common.Address
	Reason          []byte
	Raw             types.Log // Blockchain specific contextual infos
}

// FilterIBCSenderAckPacketCallbackError is a free log retrieval operation binding the contract event 0x898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0.
//
// Solidity: event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterIBCSenderAckPacketCallbackError(opts *bind.FilterOpts, callbackAddress []common.Address) (*ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "IBCSenderAckPacketCallbackError", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferIBCSenderAckPacketCallbackErrorIterator{contract: _ContractICS20Transfer.contract, event: "IBCSenderAckPacketCallbackError", logs: logs, sub: sub}, nil
}

// WatchIBCSenderAckPacketCallbackError is a free log subscription operation binding the contract event 0x898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0.
//
// Solidity: event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchIBCSenderAckPacketCallbackError(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferIBCSenderAckPacketCallbackError, callbackAddress []common.Address) (event.Subscription, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "IBCSenderAckPacketCallbackError", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferIBCSenderAckPacketCallbackError)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderAckPacketCallbackError", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseIBCSenderAckPacketCallbackError is a log parse operation binding the contract event 0x898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0.
//
// Solidity: event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseIBCSenderAckPacketCallbackError(log types.Log) (*ContractICS20TransferIBCSenderAckPacketCallbackError, error) {
	event := new(ContractICS20TransferIBCSenderAckPacketCallbackError)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderAckPacketCallbackError", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator is returned from FilterIBCSenderAckPacketCallbackError0 and is used to iterate over the raw logs and unpacked data for IBCSenderAckPacketCallbackError0 events raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator struct {
	Event *ContractICS20TransferIBCSenderAckPacketCallbackError0 // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferIBCSenderAckPacketCallbackError0)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferIBCSenderAckPacketCallbackError0)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferIBCSenderAckPacketCallbackError0 represents a IBCSenderAckPacketCallbackError0 event raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderAckPacketCallbackError0 struct {
	CallbackAddress common.Address
	Reason          []byte
	Raw             types.Log // Blockchain specific contextual infos
}

// FilterIBCSenderAckPacketCallbackError0 is a free log retrieval operation binding the contract event 0x898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0.
//
// Solidity: event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterIBCSenderAckPacketCallbackError0(opts *bind.FilterOpts, callbackAddress []common.Address) (*ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "IBCSenderAckPacketCallbackError0", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferIBCSenderAckPacketCallbackError0Iterator{contract: _ContractICS20Transfer.contract, event: "IBCSenderAckPacketCallbackError0", logs: logs, sub: sub}, nil
}

// WatchIBCSenderAckPacketCallbackError0 is a free log subscription operation binding the contract event 0x898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0.
//
// Solidity: event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchIBCSenderAckPacketCallbackError0(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferIBCSenderAckPacketCallbackError0, callbackAddress []common.Address) (event.Subscription, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "IBCSenderAckPacketCallbackError0", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferIBCSenderAckPacketCallbackError0)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderAckPacketCallbackError0", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseIBCSenderAckPacketCallbackError0 is a log parse operation binding the contract event 0x898f81cc34db3cf328a50a3f019f67f2f82077674357844e51ac4445baa466e0.
//
// Solidity: event IBCSenderAckPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseIBCSenderAckPacketCallbackError0(log types.Log) (*ContractICS20TransferIBCSenderAckPacketCallbackError0, error) {
	event := new(ContractICS20TransferIBCSenderAckPacketCallbackError0)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderAckPacketCallbackError0", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator is returned from FilterIBCSenderTimeoutPacketCallbackError and is used to iterate over the raw logs and unpacked data for IBCSenderTimeoutPacketCallbackError events raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator struct {
	Event *ContractICS20TransferIBCSenderTimeoutPacketCallbackError // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferIBCSenderTimeoutPacketCallbackError represents a IBCSenderTimeoutPacketCallbackError event raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderTimeoutPacketCallbackError struct {
	CallbackAddress common.Address
	Reason          []byte
	Raw             types.Log // Blockchain specific contextual infos
}

// FilterIBCSenderTimeoutPacketCallbackError is a free log retrieval operation binding the contract event 0x02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6.
//
// Solidity: event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterIBCSenderTimeoutPacketCallbackError(opts *bind.FilterOpts, callbackAddress []common.Address) (*ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "IBCSenderTimeoutPacketCallbackError", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferIBCSenderTimeoutPacketCallbackErrorIterator{contract: _ContractICS20Transfer.contract, event: "IBCSenderTimeoutPacketCallbackError", logs: logs, sub: sub}, nil
}

// WatchIBCSenderTimeoutPacketCallbackError is a free log subscription operation binding the contract event 0x02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6.
//
// Solidity: event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchIBCSenderTimeoutPacketCallbackError(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferIBCSenderTimeoutPacketCallbackError, callbackAddress []common.Address) (event.Subscription, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "IBCSenderTimeoutPacketCallbackError", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderTimeoutPacketCallbackError", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseIBCSenderTimeoutPacketCallbackError is a log parse operation binding the contract event 0x02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6.
//
// Solidity: event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseIBCSenderTimeoutPacketCallbackError(log types.Log) (*ContractICS20TransferIBCSenderTimeoutPacketCallbackError, error) {
	event := new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderTimeoutPacketCallbackError", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator is returned from FilterIBCSenderTimeoutPacketCallbackError0 and is used to iterate over the raw logs and unpacked data for IBCSenderTimeoutPacketCallbackError0 events raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator struct {
	Event *ContractICS20TransferIBCSenderTimeoutPacketCallbackError0 // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError0)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError0)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferIBCSenderTimeoutPacketCallbackError0 represents a IBCSenderTimeoutPacketCallbackError0 event raised by the ContractICS20Transfer contract.
type ContractICS20TransferIBCSenderTimeoutPacketCallbackError0 struct {
	CallbackAddress common.Address
	Reason          []byte
	Raw             types.Log // Blockchain specific contextual infos
}

// FilterIBCSenderTimeoutPacketCallbackError0 is a free log retrieval operation binding the contract event 0x02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6.
//
// Solidity: event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterIBCSenderTimeoutPacketCallbackError0(opts *bind.FilterOpts, callbackAddress []common.Address) (*ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "IBCSenderTimeoutPacketCallbackError0", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferIBCSenderTimeoutPacketCallbackError0Iterator{contract: _ContractICS20Transfer.contract, event: "IBCSenderTimeoutPacketCallbackError0", logs: logs, sub: sub}, nil
}

// WatchIBCSenderTimeoutPacketCallbackError0 is a free log subscription operation binding the contract event 0x02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6.
//
// Solidity: event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchIBCSenderTimeoutPacketCallbackError0(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferIBCSenderTimeoutPacketCallbackError0, callbackAddress []common.Address) (event.Subscription, error) {

	var callbackAddressRule []interface{}
	for _, callbackAddressItem := range callbackAddress {
		callbackAddressRule = append(callbackAddressRule, callbackAddressItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "IBCSenderTimeoutPacketCallbackError0", callbackAddressRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError0)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderTimeoutPacketCallbackError0", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseIBCSenderTimeoutPacketCallbackError0 is a log parse operation binding the contract event 0x02c075b43cbb5fee7c14de7aea6a6ddde6e3646144712fd9c7159705152537e6.
//
// Solidity: event IBCSenderTimeoutPacketCallbackError(address indexed callbackAddress, bytes reason)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseIBCSenderTimeoutPacketCallbackError0(log types.Log) (*ContractICS20TransferIBCSenderTimeoutPacketCallbackError0, error) {
	event := new(ContractICS20TransferIBCSenderTimeoutPacketCallbackError0)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "IBCSenderTimeoutPacketCallbackError0", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferICS20EscrowActivatedIterator is returned from FilterICS20EscrowActivated and is used to iterate over the raw logs and unpacked data for ICS20EscrowActivated events raised by the ContractICS20Transfer contract.
type ContractICS20TransferICS20EscrowActivatedIterator struct {
	Event *ContractICS20TransferICS20EscrowActivated // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferICS20EscrowActivatedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferICS20EscrowActivated)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferICS20EscrowActivated)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferICS20EscrowActivatedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferICS20EscrowActivatedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferICS20EscrowActivated represents a ICS20EscrowActivated event raised by the ContractICS20Transfer contract.
type ContractICS20TransferICS20EscrowActivated struct {
	ClientId common.Hash
	Escrow   common.Address
	Raw      types.Log // Blockchain specific contextual infos
}

// FilterICS20EscrowActivated is a free log retrieval operation binding the contract event 0x4091a6ce2a3c52563f54d5437475ea54363fc120a36395a84533922c2d5ad991.
//
// Solidity: event ICS20EscrowActivated(string indexed clientId, address indexed escrow)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterICS20EscrowActivated(opts *bind.FilterOpts, clientId []string, escrow []common.Address) (*ContractICS20TransferICS20EscrowActivatedIterator, error) {

	var clientIdRule []interface{}
	for _, clientIdItem := range clientId {
		clientIdRule = append(clientIdRule, clientIdItem)
	}
	var escrowRule []interface{}
	for _, escrowItem := range escrow {
		escrowRule = append(escrowRule, escrowItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "ICS20EscrowActivated", clientIdRule, escrowRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferICS20EscrowActivatedIterator{contract: _ContractICS20Transfer.contract, event: "ICS20EscrowActivated", logs: logs, sub: sub}, nil
}

// WatchICS20EscrowActivated is a free log subscription operation binding the contract event 0x4091a6ce2a3c52563f54d5437475ea54363fc120a36395a84533922c2d5ad991.
//
// Solidity: event ICS20EscrowActivated(string indexed clientId, address indexed escrow)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchICS20EscrowActivated(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferICS20EscrowActivated, clientId []string, escrow []common.Address) (event.Subscription, error) {

	var clientIdRule []interface{}
	for _, clientIdItem := range clientId {
		clientIdRule = append(clientIdRule, clientIdItem)
	}
	var escrowRule []interface{}
	for _, escrowItem := range escrow {
		escrowRule = append(escrowRule, escrowItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "ICS20EscrowActivated", clientIdRule, escrowRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferICS20EscrowActivated)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "ICS20EscrowActivated", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseICS20EscrowActivated is a log parse operation binding the contract event 0x4091a6ce2a3c52563f54d5437475ea54363fc120a36395a84533922c2d5ad991.
//
// Solidity: event ICS20EscrowActivated(string indexed clientId, address indexed escrow)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseICS20EscrowActivated(log types.Log) (*ContractICS20TransferICS20EscrowActivated, error) {
	event := new(ContractICS20TransferICS20EscrowActivated)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "ICS20EscrowActivated", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferICS20EscrowCreatedIterator is returned from FilterICS20EscrowCreated and is used to iterate over the raw logs and unpacked data for ICS20EscrowCreated events raised by the ContractICS20Transfer contract.
type ContractICS20TransferICS20EscrowCreatedIterator struct {
	Event *ContractICS20TransferICS20EscrowCreated // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferICS20EscrowCreatedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferICS20EscrowCreated)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferICS20EscrowCreated)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferICS20EscrowCreatedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferICS20EscrowCreatedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferICS20EscrowCreated represents a ICS20EscrowCreated event raised by the ContractICS20Transfer contract.
type ContractICS20TransferICS20EscrowCreated struct {
	ClientId common.Hash
	Escrow   common.Address
	Raw      types.Log // Blockchain specific contextual infos
}

// FilterICS20EscrowCreated is a free log retrieval operation binding the contract event 0xd71bcf9347e4c440706076986a93115883dc9a480f43ec5d00610402a437be65.
//
// Solidity: event ICS20EscrowCreated(string indexed clientId, address indexed escrow)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterICS20EscrowCreated(opts *bind.FilterOpts, clientId []string, escrow []common.Address) (*ContractICS20TransferICS20EscrowCreatedIterator, error) {

	var clientIdRule []interface{}
	for _, clientIdItem := range clientId {
		clientIdRule = append(clientIdRule, clientIdItem)
	}
	var escrowRule []interface{}
	for _, escrowItem := range escrow {
		escrowRule = append(escrowRule, escrowItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "ICS20EscrowCreated", clientIdRule, escrowRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferICS20EscrowCreatedIterator{contract: _ContractICS20Transfer.contract, event: "ICS20EscrowCreated", logs: logs, sub: sub}, nil
}

// WatchICS20EscrowCreated is a free log subscription operation binding the contract event 0xd71bcf9347e4c440706076986a93115883dc9a480f43ec5d00610402a437be65.
//
// Solidity: event ICS20EscrowCreated(string indexed clientId, address indexed escrow)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchICS20EscrowCreated(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferICS20EscrowCreated, clientId []string, escrow []common.Address) (event.Subscription, error) {

	var clientIdRule []interface{}
	for _, clientIdItem := range clientId {
		clientIdRule = append(clientIdRule, clientIdItem)
	}
	var escrowRule []interface{}
	for _, escrowItem := range escrow {
		escrowRule = append(escrowRule, escrowItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "ICS20EscrowCreated", clientIdRule, escrowRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferICS20EscrowCreated)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "ICS20EscrowCreated", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseICS20EscrowCreated is a log parse operation binding the contract event 0xd71bcf9347e4c440706076986a93115883dc9a480f43ec5d00610402a437be65.
//
// Solidity: event ICS20EscrowCreated(string indexed clientId, address indexed escrow)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseICS20EscrowCreated(log types.Log) (*ContractICS20TransferICS20EscrowCreated, error) {
	event := new(ContractICS20TransferICS20EscrowCreated)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "ICS20EscrowCreated", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferICS20EscrowLaunchGateEnabledIterator is returned from FilterICS20EscrowLaunchGateEnabled and is used to iterate over the raw logs and unpacked data for ICS20EscrowLaunchGateEnabled events raised by the ContractICS20Transfer contract.
type ContractICS20TransferICS20EscrowLaunchGateEnabledIterator struct {
	Event *ContractICS20TransferICS20EscrowLaunchGateEnabled // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferICS20EscrowLaunchGateEnabledIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferICS20EscrowLaunchGateEnabled)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferICS20EscrowLaunchGateEnabled)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferICS20EscrowLaunchGateEnabledIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferICS20EscrowLaunchGateEnabledIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferICS20EscrowLaunchGateEnabled represents a ICS20EscrowLaunchGateEnabled event raised by the ContractICS20Transfer contract.
type ContractICS20TransferICS20EscrowLaunchGateEnabled struct {
	Raw types.Log // Blockchain specific contextual infos
}

// FilterICS20EscrowLaunchGateEnabled is a free log retrieval operation binding the contract event 0x602a536c4b89ff28b2a974a997fe044117170c12aa39af142bdbf06a097bb7fd.
//
// Solidity: event ICS20EscrowLaunchGateEnabled()
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterICS20EscrowLaunchGateEnabled(opts *bind.FilterOpts) (*ContractICS20TransferICS20EscrowLaunchGateEnabledIterator, error) {

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "ICS20EscrowLaunchGateEnabled")
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferICS20EscrowLaunchGateEnabledIterator{contract: _ContractICS20Transfer.contract, event: "ICS20EscrowLaunchGateEnabled", logs: logs, sub: sub}, nil
}

// WatchICS20EscrowLaunchGateEnabled is a free log subscription operation binding the contract event 0x602a536c4b89ff28b2a974a997fe044117170c12aa39af142bdbf06a097bb7fd.
//
// Solidity: event ICS20EscrowLaunchGateEnabled()
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchICS20EscrowLaunchGateEnabled(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferICS20EscrowLaunchGateEnabled) (event.Subscription, error) {

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "ICS20EscrowLaunchGateEnabled")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferICS20EscrowLaunchGateEnabled)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "ICS20EscrowLaunchGateEnabled", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseICS20EscrowLaunchGateEnabled is a log parse operation binding the contract event 0x602a536c4b89ff28b2a974a997fe044117170c12aa39af142bdbf06a097bb7fd.
//
// Solidity: event ICS20EscrowLaunchGateEnabled()
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseICS20EscrowLaunchGateEnabled(log types.Log) (*ContractICS20TransferICS20EscrowLaunchGateEnabled, error) {
	event := new(ContractICS20TransferICS20EscrowLaunchGateEnabled)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "ICS20EscrowLaunchGateEnabled", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferInitializedIterator is returned from FilterInitialized and is used to iterate over the raw logs and unpacked data for Initialized events raised by the ContractICS20Transfer contract.
type ContractICS20TransferInitializedIterator struct {
	Event *ContractICS20TransferInitialized // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferInitializedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferInitialized)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferInitialized)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferInitializedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferInitializedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferInitialized represents a Initialized event raised by the ContractICS20Transfer contract.
type ContractICS20TransferInitialized struct {
	Version uint64
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterInitialized is a free log retrieval operation binding the contract event 0xc7f505b2f371ae2175ee4913f4499e1f2633a7b5936321eed1cdaeb6115181d2.
//
// Solidity: event Initialized(uint64 version)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterInitialized(opts *bind.FilterOpts) (*ContractICS20TransferInitializedIterator, error) {

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "Initialized")
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferInitializedIterator{contract: _ContractICS20Transfer.contract, event: "Initialized", logs: logs, sub: sub}, nil
}

// WatchInitialized is a free log subscription operation binding the contract event 0xc7f505b2f371ae2175ee4913f4499e1f2633a7b5936321eed1cdaeb6115181d2.
//
// Solidity: event Initialized(uint64 version)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchInitialized(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferInitialized) (event.Subscription, error) {

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "Initialized")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferInitialized)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "Initialized", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseInitialized is a log parse operation binding the contract event 0xc7f505b2f371ae2175ee4913f4499e1f2633a7b5936321eed1cdaeb6115181d2.
//
// Solidity: event Initialized(uint64 version)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseInitialized(log types.Log) (*ContractICS20TransferInitialized, error) {
	event := new(ContractICS20TransferInitialized)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "Initialized", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferPausedIterator is returned from FilterPaused and is used to iterate over the raw logs and unpacked data for Paused events raised by the ContractICS20Transfer contract.
type ContractICS20TransferPausedIterator struct {
	Event *ContractICS20TransferPaused // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferPausedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferPaused)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferPaused)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferPausedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferPausedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferPaused represents a Paused event raised by the ContractICS20Transfer contract.
type ContractICS20TransferPaused struct {
	Account common.Address
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterPaused is a free log retrieval operation binding the contract event 0x62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a258.
//
// Solidity: event Paused(address account)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterPaused(opts *bind.FilterOpts) (*ContractICS20TransferPausedIterator, error) {

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "Paused")
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferPausedIterator{contract: _ContractICS20Transfer.contract, event: "Paused", logs: logs, sub: sub}, nil
}

// WatchPaused is a free log subscription operation binding the contract event 0x62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a258.
//
// Solidity: event Paused(address account)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchPaused(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferPaused) (event.Subscription, error) {

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "Paused")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferPaused)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "Paused", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParsePaused is a log parse operation binding the contract event 0x62e78cea01bee320cd4e420270b5ea74000d11b0c9f74754ebdbfc544b05a258.
//
// Solidity: event Paused(address account)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParsePaused(log types.Log) (*ContractICS20TransferPaused, error) {
	event := new(ContractICS20TransferPaused)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "Paused", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferUnpausedIterator is returned from FilterUnpaused and is used to iterate over the raw logs and unpacked data for Unpaused events raised by the ContractICS20Transfer contract.
type ContractICS20TransferUnpausedIterator struct {
	Event *ContractICS20TransferUnpaused // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferUnpausedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferUnpaused)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferUnpaused)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferUnpausedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferUnpausedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferUnpaused represents a Unpaused event raised by the ContractICS20Transfer contract.
type ContractICS20TransferUnpaused struct {
	Account common.Address
	Raw     types.Log // Blockchain specific contextual infos
}

// FilterUnpaused is a free log retrieval operation binding the contract event 0x5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa.
//
// Solidity: event Unpaused(address account)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterUnpaused(opts *bind.FilterOpts) (*ContractICS20TransferUnpausedIterator, error) {

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "Unpaused")
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferUnpausedIterator{contract: _ContractICS20Transfer.contract, event: "Unpaused", logs: logs, sub: sub}, nil
}

// WatchUnpaused is a free log subscription operation binding the contract event 0x5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa.
//
// Solidity: event Unpaused(address account)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchUnpaused(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferUnpaused) (event.Subscription, error) {

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "Unpaused")
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferUnpaused)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "Unpaused", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseUnpaused is a log parse operation binding the contract event 0x5db9ee0a495bf2e6ff9c91a7834c1ba4fdd244a5e8aa4e537bd38aeae4b073aa.
//
// Solidity: event Unpaused(address account)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseUnpaused(log types.Log) (*ContractICS20TransferUnpaused, error) {
	event := new(ContractICS20TransferUnpaused)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "Unpaused", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ContractICS20TransferUpgradedIterator is returned from FilterUpgraded and is used to iterate over the raw logs and unpacked data for Upgraded events raised by the ContractICS20Transfer contract.
type ContractICS20TransferUpgradedIterator struct {
	Event *ContractICS20TransferUpgraded // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ContractICS20TransferUpgradedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractICS20TransferUpgraded)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ContractICS20TransferUpgraded)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ContractICS20TransferUpgradedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractICS20TransferUpgradedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractICS20TransferUpgraded represents a Upgraded event raised by the ContractICS20Transfer contract.
type ContractICS20TransferUpgraded struct {
	Implementation common.Address
	Raw            types.Log // Blockchain specific contextual infos
}

// FilterUpgraded is a free log retrieval operation binding the contract event 0xbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b.
//
// Solidity: event Upgraded(address indexed implementation)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) FilterUpgraded(opts *bind.FilterOpts, implementation []common.Address) (*ContractICS20TransferUpgradedIterator, error) {

	var implementationRule []interface{}
	for _, implementationItem := range implementation {
		implementationRule = append(implementationRule, implementationItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.FilterLogs(opts, "Upgraded", implementationRule)
	if err != nil {
		return nil, err
	}
	return &ContractICS20TransferUpgradedIterator{contract: _ContractICS20Transfer.contract, event: "Upgraded", logs: logs, sub: sub}, nil
}

// WatchUpgraded is a free log subscription operation binding the contract event 0xbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b.
//
// Solidity: event Upgraded(address indexed implementation)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) WatchUpgraded(opts *bind.WatchOpts, sink chan<- *ContractICS20TransferUpgraded, implementation []common.Address) (event.Subscription, error) {

	var implementationRule []interface{}
	for _, implementationItem := range implementation {
		implementationRule = append(implementationRule, implementationItem)
	}

	logs, sub, err := _ContractICS20Transfer.contract.WatchLogs(opts, "Upgraded", implementationRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractICS20TransferUpgraded)
				if err := _ContractICS20Transfer.contract.UnpackLog(event, "Upgraded", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseUpgraded is a log parse operation binding the contract event 0xbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b.
//
// Solidity: event Upgraded(address indexed implementation)
func (_ContractICS20Transfer *ContractICS20TransferFilterer) ParseUpgraded(log types.Log) (*ContractICS20TransferUpgraded, error) {
	event := new(ContractICS20TransferUpgraded)
	if err := _ContractICS20Transfer.contract.UnpackLog(event, "Upgraded", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}
