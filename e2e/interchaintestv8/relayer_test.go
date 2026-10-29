package main

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	sdkmath "cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	transfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ibchostv2 "github.com/cosmos/ibc-go/v10/modules/core/24-host/v2"

	"github.com/decentrio/spectre/packages/go-abigen/ibcerc20"
	"github.com/decentrio/spectre/packages/go-abigen/ics20transfer"

	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/e2esuite"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/testvalues"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types"
)

// RelayerTestSuite is a suite of tests that wraps EthCosmosTestSuite
// and can provide additional functionality
type RelayerTestSuite struct {
	EthCosmosTestSuite
}

// TestWithRelayerTestSuite is the boilerplate code that allows the test suite to be run
func TestWithRelayerTestSuite(t *testing.T) {
	suite.Run(t, new(RelayerTestSuite))
}

func (s *RelayerTestSuite) Test_10_RecvPacketToEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.RecvPacketToEthTest(ctx, proofType, 10)
}

func (s *RelayerTestSuite) Test_5_RecvPacketToEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.RecvPacketToEthTest(ctx, proofType, 5)
}

func (s *RelayerTestSuite) RecvPacketToEthTest(
	ctx context.Context, proofType types.SupportedProofType, numOfTransfers int,
) {
	s.Require().Greater(numOfTransfers, 0)

	s.SetupSuite(ctx, proofType)

	_, simd := s.EthChain, s.CosmosChains[0]

	transferAmount := big.NewInt(testvalues.TransferAmount)
	totalTransferAmount := big.NewInt(testvalues.TransferAmount * int64(numOfTransfers))
	if totalTransferAmount.Int64() > testvalues.InitialBalance {
		s.FailNow("Total transfer amount exceeds the initial balance")
	}
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()

	var transferCoin sdk.Coin
	s.Require().True(s.Run("Send transfers on Cosmos chain", func() {
		for range numOfTransfers {
			timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
			transferCoin = sdk.NewCoin(simd.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))

			transferPayload := transfertypes.FungibleTokenPacketData{
				Denom:    transferCoin.Denom,
				Amount:   transferCoin.Amount.String(),
				Sender:   cosmosUserAddress,
				Receiver: strings.ToLower(ethereumUserAddress.Hex()),
				Memo:     "",
			}
			encodedPayload, err := transfertypes.EncodeABIFungibleTokenPacketData(&transferPayload)
			s.Require().NoError(err)

			payload := channeltypesv2.Payload{
				SourcePort:      transfertypes.PortID,
				DestinationPort: transfertypes.PortID,
				Version:         transfertypes.V1,
				Encoding:        transfertypes.EncodingABI,
				Value:           encodedPayload,
			}
			msgSendPacket := channeltypesv2.MsgSendPacket{
				SourceClient:     testvalues.FirstWasmClientID,
				TimeoutTimestamp: timeout,
				Payloads: []channeltypesv2.Payload{
					payload,
				},
				Signer: cosmosUserWallet.FormattedAddress(),
			}

			resp, err := s.BroadcastMessages(ctx, simd, cosmosUserWallet, 200_000, &msgSendPacket)
			s.Require().NoError(err)
			s.Require().NotEmpty(resp.TxHash)
		}

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			// Check the balance of UserB
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   transferCoin.Denom,
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.InitialBalance-totalTransferAmount.Int64(), resp.Balance.Amount.Int64())
		}))
	}))

	s.Require().True(s.Run("Receive packets on Ethereum", func() {
		denomOnEthereum := transfertypes.NewDenom(transferCoin.Denom, transfertypes.NewHop(transfertypes.PortID, testvalues.CustomClientID))
		relayTimeout := autoRelayTimeout(numOfTransfers)

		s.Require().True(s.Run("Wait for auto-relay to deliver", func() {
			require.Eventuallyf(s.T(), func() bool {
				ibcERC20Addr, err := s.ics20Contract.IbcERC20Contract(nil, denomOnEthereum.Path())
				if err != nil || ibcERC20Addr == (ethcommon.Address{}) {
					return false
				}
				ibcERC20, err := ibcerc20.NewContract(ibcERC20Addr, s.EthChain.RPCClient)
				if err != nil {
					return false
				}
				userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
				if err != nil {
					return false
				}
				return userBalance.Cmp(totalTransferAmount) == 0
			}, relayTimeout, 5*time.Second,
				"auto-relay did not deliver %d packets to Ethereum within timeout", numOfTransfers)
		}))

		s.Require().True(s.Run("Verify balances on Ethereum", func() {
			ibcERC20Addr, err := s.ics20Contract.IbcERC20Contract(nil, denomOnEthereum.Path())
			s.Require().NoError(err)
			ibcERC20, err := ibcerc20.NewContract(ibcERC20Addr, s.EthChain.RPCClient)
			s.Require().NoError(err)
			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(totalTransferAmount, userBalance)
		}))
	}))
}

func (s *RelayerTestSuite) Test_10_BatchedAckPacketToEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenBatchedAckToEthTest(ctx, proofType, 10, big.NewInt(testvalues.TransferAmount))
}

func (s *RelayerTestSuite) Test_5_BatchedAckPacketToEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenBatchedAckToEthTest(ctx, proofType, 5, big.NewInt(testvalues.TransferAmount))
}

// Note that the relayer still only relays one tx, the batching is done
// on the cosmos transaction itself. So that it emits multiple IBC events.
func (s *RelayerTestSuite) ICS20TransferERC20TokenBatchedAckToEthTest(
	ctx context.Context, proofType types.SupportedProofType, numOfTransfers int, transferAmount *big.Int,
) {
	s.Require().Greater(numOfTransfers, 0)

	s.SetupSuite(ctx, proofType)

	eth, simd := s.EthChain, s.CosmosChains[0]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	totalTransferAmount := new(big.Int).Mul(transferAmount, big.NewInt(int64(numOfTransfers)))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()

	ics20transferAbi, err := abi.JSON(strings.NewReader(ics20transfer.ContractABI))
	s.Require().NoError(err)

	s.Require().True(s.Run("Approve the ICS20Transfer.sol contract to spend the erc20 tokens", func() {
		tx, err := s.erc20Contract.Approve(s.GetTransactOpts(s.key, eth), ics20Address, totalTransferAmount)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		allowance, err := s.erc20Contract.Allowance(nil, ethereumUserAddress, ics20Address)
		s.Require().NoError(err)
		s.Require().Equal(totalTransferAmount, allowance)
	}))

	var escrowAddress ethcommon.Address
	s.Require().True(s.Run(fmt.Sprintf("Send %d transfers on Ethereum", numOfTransfers), func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferMulticall := make([][]byte, numOfTransfers)

		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			SourceClient:     testvalues.CustomClientID,
			Denom:            erc20Address,
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: timeout,
			Memo:             "",
		}

		encodedMsg, err := ics20transferAbi.Pack("sendTransfer", msgSendPacket)
		s.Require().NoError(err)
		for i := range numOfTransfers {
			transferMulticall[i] = encodedMsg
		}

		tx, err := s.ics20Contract.Multicall(s.GetTransactOpts(s.key, eth), transferMulticall)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)
		s.T().Logf("Multicall send %d transfers gas used: %d", numOfTransfers, receipt.GasUsed)

		s.True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Sub(testvalues.StartingERC20Balance, totalTransferAmount), userBalance)

			// Get the escrow address
			escrowAddress, err = s.ics20Contract.GetEscrow(nil, testvalues.CustomClientID)
			s.Require().NoError(err)

			// ICS20 contract balance on Ethereum
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Equal(totalTransferAmount, escrowBalance)
		}))
	}))

	s.Require().True(s.Run("Receive packets on Cosmos chain", func() {
		denomOnCosmos := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))
		relayTimeout := autoRelayTimeout(numOfTransfers)

		s.Require().True(s.Run("Wait for auto-relay to deliver", func() {
			require.Eventuallyf(s.T(), func() bool {
				resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
					Address: cosmosUserAddress,
					Denom:   denomOnCosmos.IBCDenom(),
				})
				if err != nil || resp.Balance == nil {
					return false
				}
				return resp.Balance.Amount.BigInt().Cmp(totalTransferAmount) == 0
			}, relayTimeout, 5*time.Second,
				"auto-relay did not deliver %d packets to Cosmos within timeout", numOfTransfers)
		}))

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   denomOnCosmos.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(totalTransferAmount, resp.Balance.Amount.BigInt())
			s.Require().Equal(denomOnCosmos.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	s.Require().True(s.Run("Acknowledge packets on Ethereum", func() {
		relayTimeout := autoRelayTimeout(numOfTransfers)

		s.Require().True(s.Run("Wait for auto-relay to relay acks", func() {
			require.Eventuallyf(s.T(), func() bool {
				for i := range numOfTransfers {
					seq := uint64(i) + 1
					packetCommitmentPath := ibchostv2.PacketCommitmentKey(testvalues.CustomClientID, seq)
					var ethPath [32]byte
					copy(ethPath[:], crypto.Keccak256(packetCommitmentPath))
					resp, err := s.ics26Contract.GetCommitment(nil, ethPath)
					if err != nil || resp != ([32]byte{}) {
						return false
					}
				}
				return true
			}, relayTimeout, 5*time.Second,
				"auto-relay did not ack %d packets on Ethereum within timeout", numOfTransfers)
		}))

		s.Require().True(s.Run("Verify commitment removed", func() {
			for i := range numOfTransfers {
				seq := uint64(i) + 1
				packetCommitmentPath := ibchostv2.PacketCommitmentKey(testvalues.CustomClientID, seq)
				var ethPath [32]byte
				copy(ethPath[:], crypto.Keccak256(packetCommitmentPath))
				resp, err := s.ics26Contract.GetCommitment(nil, ethPath)
				s.Require().NoError(err)
				s.Require().Zero(resp)
			}
		}))
	}))
}

func (s *RelayerTestSuite) Test_10_RecvPacketToCosmos() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.SetupSuite(ctx, proofType)
	s.RecvPacketToCosmosTest(ctx, 10, big.NewInt(testvalues.TransferAmount))
}

func (s *RelayerTestSuite) RecvPacketToCosmosTest(ctx context.Context, numOfTransfers int, transferAmount *big.Int) {
	s.Require().Greater(numOfTransfers, 0)

	eth, simd := s.EthChain, s.CosmosChains[0]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	totalTransferAmount := new(big.Int).Mul(transferAmount, big.NewInt(int64(numOfTransfers)))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	escrowAddress, err := s.ics20Contract.GetEscrow(nil, testvalues.CustomClientID)
	s.Require().NoError(err)

	denomOnCosmos := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))

	escrowStartingBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
	s.Require().NoError(err)
	startBalanceEthereum, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
	s.Require().NoError(err)
	startBalanceCosmosResp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
		Address: cosmosUserAddress,
		Denom:   denomOnCosmos.IBCDenom(),
	})
	s.Require().NoError(err)
	startBalanceCosmos := startBalanceCosmosResp.Balance.Amount.BigInt()

	s.Require().True(s.Run("Approve the ICS20Transfer.sol contract to spend the erc20 tokens", func() {
		tx, err := s.erc20Contract.Approve(s.GetTransactOpts(s.key, eth), ics20Address, totalTransferAmount)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		allowance, err := s.erc20Contract.Allowance(nil, ethereumUserAddress, ics20Address)
		s.Require().NoError(err)
		s.Require().Equal(totalTransferAmount, allowance)
	}))

	s.Require().True(s.Run(fmt.Sprintf("Send %d transfers on Ethereum", numOfTransfers), func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		msgSendTransfer := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            erc20Address,
			SourceClient:     testvalues.CustomClientID,
			DestPort:         transfertypes.PortID,
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: timeout,
			Memo:             "",
		}

		for range numOfTransfers {
			tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendTransfer)
			s.Require().NoError(err)

			receipt, err := eth.GetTxReciept(ctx, tx.Hash())
			s.Require().NoError(err)
			s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)
		}

		s.True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Sub(startBalanceEthereum, totalTransferAmount), userBalance)

			// Get the escrow address
			escrowAddress, err = s.ics20Contract.GetEscrow(nil, testvalues.CustomClientID)
			s.Require().NoError(err)

			// ICS20 contract balance on Ethereum
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Add(escrowStartingBalance, totalTransferAmount), escrowBalance)
		}))
	}))

	s.Require().True(s.Run("Receive packets on Cosmos chain", func() {
		relayTimeout := autoRelayTimeout(numOfTransfers)

		s.Require().True(s.Run("Wait for auto-relay to deliver", func() {
			require.Eventuallyf(s.T(), func() bool {
				resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
					Address: cosmosUserAddress,
					Denom:   denomOnCosmos.IBCDenom(),
				})
				if err != nil || resp.Balance == nil {
					return false
				}
				return resp.Balance.Amount.BigInt().Cmp(new(big.Int).Add(startBalanceCosmos, totalTransferAmount)) == 0
			}, relayTimeout, 5*time.Second,
				"auto-relay did not deliver %d packets to Cosmos within timeout", numOfTransfers)
		}))

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   denomOnCosmos.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(new(big.Int).Add(startBalanceCosmos, totalTransferAmount), resp.Balance.Amount.BigInt())
			s.Require().Equal(denomOnCosmos.IBCDenom(), resp.Balance.Denom)
		}))
	}))
}

func (s *RelayerTestSuite) Test_10_BatchedAckPacketToCosmos() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenBatchedAckToCosmosTest(ctx, proofType, 10)
}

// Note that the relayer still only relays one tx, the batching is done
// on the cosmos transaction itself. So that it emits multiple IBC events.
func (s *RelayerTestSuite) ICS20TransferERC20TokenBatchedAckToCosmosTest(
	ctx context.Context, proofType types.SupportedProofType, numOfTransfers int,
) {
	s.Require().Greater(numOfTransfers, 0)

	s.SetupSuite(ctx, proofType)

	_, simd := s.EthChain, s.CosmosChains[0]

	transferAmount := big.NewInt(testvalues.TransferAmount)
	totalTransferAmount := big.NewInt(testvalues.TransferAmount * int64(numOfTransfers))
	if totalTransferAmount.Int64() > testvalues.InitialBalance {
		s.FailNow("Total transfer amount exceeds the initial balance")
	}
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	sendMemo := "batched ack to cosmos test memo"

	var transferCoin sdk.Coin
	s.Require().True(s.Run("Send transfers on Cosmos chain", func() {
		for range numOfTransfers {
			timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
			transferCoin = sdk.NewCoin(simd.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))

			transferPayload := transfertypes.FungibleTokenPacketData{
				Denom:    transferCoin.Denom,
				Amount:   transferCoin.Amount.String(),
				Sender:   cosmosUserAddress,
				Receiver: strings.ToLower(ethereumUserAddress.Hex()),
				Memo:     sendMemo,
			}
			encodedPayload, err := transfertypes.EncodeABIFungibleTokenPacketData(&transferPayload)
			s.Require().NoError(err)

			payload := channeltypesv2.Payload{
				SourcePort:      transfertypes.PortID,
				DestinationPort: transfertypes.PortID,
				Version:         transfertypes.V1,
				Encoding:        transfertypes.EncodingABI,
				Value:           encodedPayload,
			}
			msgSendPacket := channeltypesv2.MsgSendPacket{
				SourceClient:     testvalues.FirstWasmClientID,
				TimeoutTimestamp: timeout,
				Payloads: []channeltypesv2.Payload{
					payload,
				},
				Signer: cosmosUserWallet.FormattedAddress(),
			}

			resp, err := s.BroadcastMessages(ctx, simd, cosmosUserWallet, 200_000, &msgSendPacket)
			s.Require().NoError(err)
			s.Require().NotEmpty(resp.TxHash)
		}

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			// Check the balance of UserB
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   transferCoin.Denom,
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.InitialBalance-totalTransferAmount.Int64(), resp.Balance.Amount.Int64())
		}))
	}))

	s.Require().True(s.Run("Acknowledge packets on Cosmos", func() {
		relayTimeout := autoRelayTimeout(numOfTransfers) + 10*time.Minute // recv hop + ack hop

		s.Require().True(s.Run("Wait for auto-relay to deliver recv and ack", func() {
			require.Eventuallyf(s.T(), func() bool {
				for i := range numOfTransfers {
					seq := uint64(i) + 1
					_, err := e2esuite.GRPCQuery[channeltypesv2.QueryPacketCommitmentResponse](ctx, simd, &channeltypesv2.QueryPacketCommitmentRequest{
						ClientId: testvalues.FirstWasmClientID,
						Sequence: seq,
					})
					if err == nil || !strings.Contains(err.Error(), "packet commitment hash not found") {
						return false
					}
				}
				return true
			}, relayTimeout, 5*time.Second,
				"auto-relay did not ack %d packets on Cosmos within timeout", numOfTransfers)
		}))

		s.Require().True(s.Run("Verify commitments removed", func() {
			for i := range numOfTransfers {
				seq := uint64(i) + 1
				_, err := e2esuite.GRPCQuery[channeltypesv2.QueryPacketCommitmentResponse](ctx, simd, &channeltypesv2.QueryPacketCommitmentRequest{
					ClientId: testvalues.FirstWasmClientID,
					Sequence: seq,
				})
				s.Require().ErrorContains(err, "packet commitment hash not found")
			}
		}))
	}))
}
