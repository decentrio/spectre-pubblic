// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package contractDisputeGameFactory

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

// ContractMetaData contains all meta data concerning the Contract contract.
var ContractMetaData = &bind.MetaData{
	ABI: "[{\"type\":\"function\",\"name\":\"gameCount\",\"inputs\":[],\"outputs\":[{\"name\":\"gameCount_\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"gameAtIndex\",\"inputs\":[{\"name\":\"_index\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[{\"name\":\"gameType_\",\"type\":\"uint32\",\"internalType\":\"GameType\"},{\"name\":\"timestamp_\",\"type\":\"uint64\",\"internalType\":\"Timestamp\"},{\"name\":\"proxy_\",\"type\":\"address\",\"internalType\":\"contractIDisputeGame\"}],\"stateMutability\":\"view\"},{\"type\":\"event\",\"name\":\"DisputeGameCreated\",\"inputs\":[{\"name\":\"disputeProxy\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"gameType\",\"type\":\"uint32\",\"indexed\":true,\"internalType\":\"GameType\"},{\"name\":\"rootClaim\",\"type\":\"bytes32\",\"indexed\":true,\"internalType\":\"Claim\"}],\"anonymous\":false}]",
}

// ContractABI is the input ABI used to generate the binding from.
// Deprecated: Use ContractMetaData.ABI instead.
var ContractABI = ContractMetaData.ABI

// Contract is an auto generated Go binding around an Ethereum contract.
type Contract struct {
	ContractCaller     // Read-only binding to the contract
	ContractTransactor // Write-only binding to the contract
	ContractFilterer   // Log filterer for contract events
}

// ContractCaller is an auto generated read-only Go binding around an Ethereum contract.
type ContractCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractTransactor is an auto generated write-only Go binding around an Ethereum contract.
type ContractTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type ContractFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ContractSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type ContractSession struct {
	Contract     *Contract         // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// ContractCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type ContractCallerSession struct {
	Contract *ContractCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts   // Call options to use throughout this session
}

// ContractTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type ContractTransactorSession struct {
	Contract     *ContractTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts   // Transaction auth options to use throughout this session
}

// ContractRaw is an auto generated low-level Go binding around an Ethereum contract.
type ContractRaw struct {
	Contract *Contract // Generic contract binding to access the raw methods on
}

// ContractCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type ContractCallerRaw struct {
	Contract *ContractCaller // Generic read-only contract binding to access the raw methods on
}

// ContractTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type ContractTransactorRaw struct {
	Contract *ContractTransactor // Generic write-only contract binding to access the raw methods on
}

// NewContract creates a new instance of Contract, bound to a specific deployed contract.
func NewContract(address common.Address, backend bind.ContractBackend) (*Contract, error) {
	contract, err := bindContract(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &Contract{ContractCaller: ContractCaller{contract: contract}, ContractTransactor: ContractTransactor{contract: contract}, ContractFilterer: ContractFilterer{contract: contract}}, nil
}

// NewContractCaller creates a new read-only instance of Contract, bound to a specific deployed contract.
func NewContractCaller(address common.Address, caller bind.ContractCaller) (*ContractCaller, error) {
	contract, err := bindContract(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &ContractCaller{contract: contract}, nil
}

// NewContractTransactor creates a new write-only instance of Contract, bound to a specific deployed contract.
func NewContractTransactor(address common.Address, transactor bind.ContractTransactor) (*ContractTransactor, error) {
	contract, err := bindContract(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &ContractTransactor{contract: contract}, nil
}

// NewContractFilterer creates a new log filterer instance of Contract, bound to a specific deployed contract.
func NewContractFilterer(address common.Address, filterer bind.ContractFilterer) (*ContractFilterer, error) {
	contract, err := bindContract(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &ContractFilterer{contract: contract}, nil
}

// bindContract binds a generic wrapper to an already deployed contract.
func bindContract(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := ContractMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_Contract *ContractRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _Contract.Contract.ContractCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_Contract *ContractRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _Contract.Contract.ContractTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_Contract *ContractRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _Contract.Contract.ContractTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_Contract *ContractCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _Contract.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_Contract *ContractTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _Contract.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_Contract *ContractTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _Contract.Contract.contract.Transact(opts, method, params...)
}

// GameAtIndex is a free data retrieval call binding the contract method 0xbb8aa1fc.
//
// Solidity: function gameAtIndex(uint256 _index) view returns(uint32 gameType_, uint64 timestamp_, address proxy_)
func (_Contract *ContractCaller) GameAtIndex(opts *bind.CallOpts, _index *big.Int) (struct {
	GameType  uint32
	Timestamp uint64
	Proxy     common.Address
}, error) {
	var out []interface{}
	err := _Contract.contract.Call(opts, &out, "gameAtIndex", _index)

	outstruct := new(struct {
		GameType  uint32
		Timestamp uint64
		Proxy     common.Address
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.GameType = *abi.ConvertType(out[0], new(uint32)).(*uint32)
	outstruct.Timestamp = *abi.ConvertType(out[1], new(uint64)).(*uint64)
	outstruct.Proxy = *abi.ConvertType(out[2], new(common.Address)).(*common.Address)

	return *outstruct, err

}

// GameAtIndex is a free data retrieval call binding the contract method 0xbb8aa1fc.
//
// Solidity: function gameAtIndex(uint256 _index) view returns(uint32 gameType_, uint64 timestamp_, address proxy_)
func (_Contract *ContractSession) GameAtIndex(_index *big.Int) (struct {
	GameType  uint32
	Timestamp uint64
	Proxy     common.Address
}, error) {
	return _Contract.Contract.GameAtIndex(&_Contract.CallOpts, _index)
}

// GameAtIndex is a free data retrieval call binding the contract method 0xbb8aa1fc.
//
// Solidity: function gameAtIndex(uint256 _index) view returns(uint32 gameType_, uint64 timestamp_, address proxy_)
func (_Contract *ContractCallerSession) GameAtIndex(_index *big.Int) (struct {
	GameType  uint32
	Timestamp uint64
	Proxy     common.Address
}, error) {
	return _Contract.Contract.GameAtIndex(&_Contract.CallOpts, _index)
}

// GameCount is a free data retrieval call binding the contract method 0x4d1975b4.
//
// Solidity: function gameCount() view returns(uint256 gameCount_)
func (_Contract *ContractCaller) GameCount(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _Contract.contract.Call(opts, &out, "gameCount")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// GameCount is a free data retrieval call binding the contract method 0x4d1975b4.
//
// Solidity: function gameCount() view returns(uint256 gameCount_)
func (_Contract *ContractSession) GameCount() (*big.Int, error) {
	return _Contract.Contract.GameCount(&_Contract.CallOpts)
}

// GameCount is a free data retrieval call binding the contract method 0x4d1975b4.
//
// Solidity: function gameCount() view returns(uint256 gameCount_)
func (_Contract *ContractCallerSession) GameCount() (*big.Int, error) {
	return _Contract.Contract.GameCount(&_Contract.CallOpts)
}

// ContractDisputeGameCreatedIterator is returned from FilterDisputeGameCreated and is used to iterate over the raw logs and unpacked data for DisputeGameCreated events raised by the Contract contract.
type ContractDisputeGameCreatedIterator struct {
	Event *ContractDisputeGameCreated // Event containing the contract specifics and raw log

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
func (it *ContractDisputeGameCreatedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ContractDisputeGameCreated)
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
		it.Event = new(ContractDisputeGameCreated)
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
func (it *ContractDisputeGameCreatedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ContractDisputeGameCreatedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ContractDisputeGameCreated represents a DisputeGameCreated event raised by the Contract contract.
type ContractDisputeGameCreated struct {
	DisputeProxy common.Address
	GameType     uint32
	RootClaim    [32]byte
	Raw          types.Log // Blockchain specific contextual infos
}

// FilterDisputeGameCreated is a free log retrieval operation binding the contract event 0x5b565efe82411da98814f356d0e7bcb8f0219b8d970307c5afb4a6903a8b2e35.
//
// Solidity: event DisputeGameCreated(address indexed disputeProxy, uint32 indexed gameType, bytes32 indexed rootClaim)
func (_Contract *ContractFilterer) FilterDisputeGameCreated(opts *bind.FilterOpts, disputeProxy []common.Address, gameType []uint32, rootClaim [][32]byte) (*ContractDisputeGameCreatedIterator, error) {

	var disputeProxyRule []interface{}
	for _, disputeProxyItem := range disputeProxy {
		disputeProxyRule = append(disputeProxyRule, disputeProxyItem)
	}
	var gameTypeRule []interface{}
	for _, gameTypeItem := range gameType {
		gameTypeRule = append(gameTypeRule, gameTypeItem)
	}
	var rootClaimRule []interface{}
	for _, rootClaimItem := range rootClaim {
		rootClaimRule = append(rootClaimRule, rootClaimItem)
	}

	logs, sub, err := _Contract.contract.FilterLogs(opts, "DisputeGameCreated", disputeProxyRule, gameTypeRule, rootClaimRule)
	if err != nil {
		return nil, err
	}
	return &ContractDisputeGameCreatedIterator{contract: _Contract.contract, event: "DisputeGameCreated", logs: logs, sub: sub}, nil
}

// WatchDisputeGameCreated is a free log subscription operation binding the contract event 0x5b565efe82411da98814f356d0e7bcb8f0219b8d970307c5afb4a6903a8b2e35.
//
// Solidity: event DisputeGameCreated(address indexed disputeProxy, uint32 indexed gameType, bytes32 indexed rootClaim)
func (_Contract *ContractFilterer) WatchDisputeGameCreated(opts *bind.WatchOpts, sink chan<- *ContractDisputeGameCreated, disputeProxy []common.Address, gameType []uint32, rootClaim [][32]byte) (event.Subscription, error) {

	var disputeProxyRule []interface{}
	for _, disputeProxyItem := range disputeProxy {
		disputeProxyRule = append(disputeProxyRule, disputeProxyItem)
	}
	var gameTypeRule []interface{}
	for _, gameTypeItem := range gameType {
		gameTypeRule = append(gameTypeRule, gameTypeItem)
	}
	var rootClaimRule []interface{}
	for _, rootClaimItem := range rootClaim {
		rootClaimRule = append(rootClaimRule, rootClaimItem)
	}

	logs, sub, err := _Contract.contract.WatchLogs(opts, "DisputeGameCreated", disputeProxyRule, gameTypeRule, rootClaimRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ContractDisputeGameCreated)
				if err := _Contract.contract.UnpackLog(event, "DisputeGameCreated", log); err != nil {
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

// ParseDisputeGameCreated is a log parse operation binding the contract event 0x5b565efe82411da98814f356d0e7bcb8f0219b8d970307c5afb4a6903a8b2e35.
//
// Solidity: event DisputeGameCreated(address indexed disputeProxy, uint32 indexed gameType, bytes32 indexed rootClaim)
func (_Contract *ContractFilterer) ParseDisputeGameCreated(log types.Log) (*ContractDisputeGameCreated, error) {
	event := new(ContractDisputeGameCreated)
	if err := _Contract.contract.UnpackLog(event, "DisputeGameCreated", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}
