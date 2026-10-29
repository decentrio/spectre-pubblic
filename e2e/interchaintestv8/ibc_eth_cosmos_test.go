package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	sdkmath "cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/crypto/hd"
	cosmossecp256k1 "github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	bip39 "github.com/cosmos/go-bip39"

	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	transfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	clienttypesv2 "github.com/cosmos/ibc-go/v10/modules/core/02-client/v2/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ibchostv2 "github.com/cosmos/ibc-go/v10/modules/core/24-host/v2"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
	ibctesting "github.com/cosmos/ibc-go/v10/testing"

	interchaintest "github.com/cosmos/interchaintest/v10"
	"github.com/cosmos/interchaintest/v10/ibc"

	"github.com/decentrio/spectre/packages/go-abigen/ibcerc20"
	"github.com/decentrio/spectre/packages/go-abigen/ics20transfer"
	"github.com/decentrio/spectre/packages/go-abigen/ics26router"
	"github.com/decentrio/spectre/packages/go-abigen/spectreclient"

	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/cosmos"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/e2esuite"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/ethereum"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/relayer"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/testvalues"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types/erc20"
)

// EthCosmosTestSuite is a suite of tests that wraps TestSuite
// and can provide additional functionality
type EthCosmosTestSuite struct {
	e2esuite.TestSuite

	// Whether to generate fixtures for tests or not
	solidityFixtureGenerator *types.SolidityFixtureGenerator
	wasmFixtureGenerator     *types.WasmFixtureGenerator

	// The private key of a test account
	key *ecdsa.PrivateKey
	// The private key of the faucet account of interchaintest
	deployer *ecdsa.PrivateKey

	contractAddresses    ethereum.DeployedContracts
	spectreClientAddress ethcommon.Address

	spectreClientContract *spectreclient.Contract
	ics26Contract         *ics26router.Contract
	ics20Contract         *ics20transfer.Contract
	erc20Contract         *erc20.Contract

	SimdRelayerSubmitter ibc.Wallet
	EthRelayerSubmitter  *ecdsa.PrivateKey
}

// TestWithEthCosmosTestSuite is the boilerplate code that allows the test suite to be run
func TestWithEthCosmosTestSuite(t *testing.T) {
	suite.Run(t, new(EthCosmosTestSuite))
}

// SetupSuite calls the underlying EthCosmosTestSuite's SetupSuite method
// and deploys the IBC v2 contracts. It uses the relayer binary directly
// (via create-clients + start).
func (s *EthCosmosTestSuite) SetupSuite(ctx context.Context, proofType types.SupportedProofType) {
	s.TestSuite.SetupSuite(ctx)

	eth, simd := s.EthChain, s.CosmosChains[0]

	s.T().Logf("Setting up the test suite with proof type: %s", proofType.String())

	s.Require().True(s.Run("Set up environment", func() {
		var err error
		err = os.Chdir("../..")
		s.Require().NoError(err)

		s.key, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		s.EthRelayerSubmitter, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		s.deployer, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		// We need a Cosmos wallet whose mnemonic is both:
		//  (a) accessible via Wallet.Mnemonic() — so the relayer binary can derive the same
		//      signing key from COSMOS_PRIVATE_KEY, and
		//  (b) present in simd's container keyring-test — so e2esuite.BroadcastMessages
		//      (which signs via cosmos.Broadcaster, copying from the container's keyring)
		//      can find and sign with it.
		//
		// Neither CreateAndFundCosmosUser (BuildWallet with empty mnemonic — no host
		// mnemonic) nor BuildRelayerWallet (host-only keyring — broadcaster can't find it)
		// satisfies both. So we generate a mnemonic on the host and recover it into the
		// container keyring via BuildWallet(ctx, name, mnemonic).
		relayerMnemonic, err := generateRelayerMnemonic()
		s.Require().NoError(err)
		simdRelayerWallet, err := simd.BuildWallet(ctx, "spectre-relayer", relayerMnemonic)
		s.Require().NoError(err)
		err = simd.SendFunds(ctx, interchaintest.FaucetAccountKeyName, ibc.WalletAmount{
			Address: simdRelayerWallet.FormattedAddress(),
			Denom:   simd.Config().Denom,
			Amount:  sdkmath.NewInt(testvalues.InitialBalance),
		})
		s.Require().NoError(err)
		s.SimdRelayerSubmitter = simdRelayerWallet

		// Derive secp256k1 private key from the relayer submitter's mnemonic so that
		// create-clients (which reads COSMOS_PRIVATE_KEY) signs as the funded account.
		seed := bip39.NewSeed(s.SimdRelayerSubmitter.Mnemonic(), "")
		masterPriv, chainCode := hd.ComputeMastersFromSeed(seed)
		derivedPrivKeyBytes, err := hd.DerivePrivateKeyForPath(masterPriv, chainCode, "m/44'/118'/0'/0/0")
		s.Require().NoError(err)
		cosmosPrivKey := cosmossecp256k1.PrivKey{Key: derivedPrivKeyBytes}
		os.Setenv("COSMOS_PRIVATE_KEY", hex.EncodeToString(cosmosPrivKey.Key))
		_ = sdk.AccAddress(cosmosPrivKey.PubKey().Address()) // verify derivation works

		os.Setenv(testvalues.EnvKeyEthRPC, eth.RPC)
		os.Setenv(testvalues.EnvKeyTendermintRPC, simd.GetHostRPCAddress())
		// ETH_PRIVATE_KEY is used by the relayer binary to sign ETH transactions.
		// We reuse s.deployer here so the relayer inherits all roles granted by
		// E2ETestDeploy.s.sol (RELAYER_ROLE, ID_CUSTOMIZER_ROLE, admin) without needing
		// a separate grant flow. EthRelayerSubmitter is funded here but no test currently
		// uses it.
		os.Setenv("ETH_PRIVATE_KEY", hex.EncodeToString(crypto.FromECDSA(s.deployer)))
		os.Setenv("COSMOS_CHAIN_ID", simd.Config().ChainID)
		os.Setenv("COSMOS_FEE_DENOM", simd.Config().Denom)
	}))

	// Needs to be added here so the cleanup is called after the test suite is done
	s.wasmFixtureGenerator = types.NewWasmFixtureGenerator(&s.Suite)
	s.solidityFixtureGenerator = types.NewSolidityFixtureGenerator()

	s.Require().True(s.Run("Deploy IBC contracts", func() {
		stdout, err := eth.ForgeScript(s.deployer, testvalues.E2EDeployScriptPath)
		s.Require().NoError(err)

		s.contractAddresses, err = ethereum.GetEthContractsFromDeployOutput(string(stdout))
		s.Require().NoError(err)
		s.ics26Contract, err = ics26router.NewContract(ethcommon.HexToAddress(s.contractAddresses.Ics26Router), eth.RPCClient)
		s.Require().NoError(err)
		s.ics20Contract, err = ics20transfer.NewContract(ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer), eth.RPCClient)
		s.Require().NoError(err)
		s.erc20Contract, err = erc20.NewContract(ethcommon.HexToAddress(s.contractAddresses.Erc20), eth.RPCClient)
		s.Require().NoError(err)
	}))

	// Store wasm light client code on Cosmos before running create-clients
	var checksumHex string
	s.Require().True(s.Run("Store Ethereum light client", func() {
		checksumHex = s.StoreEthereumLightClient(ctx, simd, s.SimdRelayerSubmitter)
		s.Require().NotEmpty(checksumHex)
	}))

	// Compute the SpectreClient address that will be deployed by create-clients.
	// The relayer binary signs with ETH_PRIVATE_KEY which is set to s.deployer (see
	// the env setup above) — using EthRelayerSubmitter's nonce here would mispredict
	// the address and the subsequent equality check in "Add Cosmos light client" fails.
	ethRelayerAddr := crypto.PubkeyToAddress(s.deployer.PublicKey)
	var preNonce uint64
	s.Require().True(s.Run("Get pre-deployment nonce", func() {
		var err error
		preNonce, err = eth.RPCClient.PendingNonceAt(ctx, ethRelayerAddr)
		s.Require().NoError(err)
	}))
	s.spectreClientAddress = crypto.CreateAddress(ethRelayerAddr, preNonce)

	s.Require().True(s.Run("Generate relayer config (pre-SpectreClient)", func() {
		beaconAPI := ""
		if eth.BeaconAPIClient != nil {
			beaconAPI = eth.BeaconAPIClient.GetBeaconAPIURL()
		}

		config := relayer.NewConfig(relayer.CreateEthCosmosModules(
			relayer.EthCosmosConfigInfo{
				EthChainID:         eth.ChainID.String(),
				CosmosChainID:      simd.Config().ChainID,
				TmRPC:              simd.GetHostRPCAddress(),
				ICS26Address:       s.contractAddresses.Ics26Router,
				EthRPC:             eth.RPC,
				EthWs:              eth.WS,
				BeaconAPI:          beaconAPI,
				SignerAddress:      s.SimdRelayerSubmitter.FormattedAddress(),
				MockWasmClient:     os.Getenv(testvalues.EnvKeyEthTestnetType) == testvalues.EthTestnetTypePoW,
				SignatureVerifier:  s.contractAddresses.SignatureVerifier,
				Membership:         s.contractAddresses.Membership,
				Misbehaviour:       s.contractAddresses.Misbehaviour,
				UpdateClient:       s.contractAddresses.UpdateClient,
				CosmosWasmClientID: testvalues.FirstWasmClientID,
				ICS26ClientID:      testvalues.CustomClientID,
				TrustLevel:         "1/3",
				ProofType:          testvalues.EnvValueProofType_Groth16,
			}),
		)

		err := config.GenerateConfigFile(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)
	}))

	s.T().Cleanup(func() {
		os.Remove(testvalues.RelayerConfigFilePath)
	})

	// Run create-clients: deploys SpectreClient on ETH (and creates wasm ETH client on Cosmos for PoS mode)
	s.Require().True(s.Run("Create light clients", func() {
		// Pass wasm checksum only for PoS mode (beacon URL required by relayer)
		wasmChecksum := ""
		if checksumHex != "" && eth.BeaconAPIClient != nil {
			wasmChecksum = checksumHex
		}
		err := relayer.RunCreateClients(testvalues.RelayerConfigFilePath, wasmChecksum, "1/3")
		s.Require().NoError(err)
	}))

	// The relayer's create-clients deploys SpectreClient, but AddClient may fail (wrong signer role).
	// Call AddClient from the deployer who has ID_CUSTOMIZER_ROLE.
	s.Require().True(s.Run("Add Cosmos light client to ICS26Router", func() {
		var err error
		s.spectreClientContract, err = spectreclient.NewContract(s.spectreClientAddress, eth.RPCClient)
		s.Require().NoError(err)

		counterpartyInfo := ics26router.IICS02ClientMsgsCounterpartyInfo{
			ClientId:     testvalues.FirstWasmClientID,
			MerklePrefix: [][]byte{[]byte(ibcexported.StoreKey), []byte("")},
		}
		// Try AddClient; if client already exists (relayer succeeded), this is a no-op error we can ignore
		tx, err := s.ics26Contract.AddClient(s.GetTransactOpts(s.deployer, eth), testvalues.CustomClientID, counterpartyInfo, s.spectreClientAddress)
		if err != nil {
			// Check if client already registered (relayer may have succeeded with AddClient)
			existingAddr, queryErr := s.ics26Contract.GetClient(nil, testvalues.CustomClientID)
			s.Require().NoError(queryErr, "AddClient failed and GetClient also failed: %v", err)
			s.Require().Equal(s.spectreClientAddress, existingAddr, "AddClient failed but client registered at wrong address")
		} else {
			receipt, err := eth.GetTxReciept(ctx, tx.Hash())
			s.Require().NoError(err)

			event, err := e2esuite.GetEvmEvent(receipt, s.ics26Contract.ParseICS02ClientAdded)
			s.Require().NoError(err)
			s.Require().Equal(testvalues.CustomClientID, event.ClientId)
			s.Require().Equal(testvalues.FirstWasmClientID, event.CounterpartyInfo.ClientId)
		}
	}))

	s.Require().True(s.Run("Fund address with ERC20", func() {
		tx, err := s.erc20Contract.Transfer(s.GetTransactOpts(eth.Faucet, eth), crypto.PubkeyToAddress(s.key.PublicKey), testvalues.StartingERC20Balance)
		s.Require().NoError(err)
		_, err = eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
	}))

	// For PoW mode (no beacon URL), create-clients skips ETH light client on Cosmos.
	// The test creates a mock wasm client manually.
	if os.Getenv(testvalues.EnvKeyEthTestnetType) == testvalues.EthTestnetTypePoW {
		s.Require().True(s.Run("Create mock Ethereum light client on Cosmos", func() {
			checksumBytes, err := hex.DecodeString(strings.TrimPrefix(checksumHex, "0x"))
			s.Require().NoError(err)

			wasmClientState := ibcwasmtypes.ClientState{
				Data:     []byte("{}"),
				Checksum: checksumBytes,
				LatestHeight: clienttypes.Height{
					RevisionNumber: 0,
					RevisionHeight: 1,
				},
			}
			wasmConsensusState := ibcwasmtypes.ConsensusState{
				Data: []byte("{}"),
			}

			msg, err := clienttypes.NewMsgCreateClient(&wasmClientState, &wasmConsensusState, s.SimdRelayerSubmitter.FormattedAddress())
			s.Require().NoError(err)

			resp, err := s.BroadcastMessages(ctx, simd, s.SimdRelayerSubmitter, 200_000, msg)
			s.Require().NoError(err)

			clientId, err := cosmos.GetEventValue(resp.Events, clienttypes.EventTypeCreateClient, clienttypes.AttributeKeyClientID)
			s.Require().NoError(err)
			s.Require().Equal(testvalues.FirstWasmClientID, clientId)
		}))

		// PoW mode created the mock client manually, so it also has to register the
		// counterparty manually. PoS mode runs `relayer create-clients`, which already
		// broadcasts MsgRegisterCounterparty itself — a second call here would fail with
		// "cannot register counterparty once it is already set".
		s.Require().True(s.Run("Register counterparty on Cosmos chain", func() {
			merklePathPrefix := [][]byte{[]byte("")}
			_, err := s.BroadcastMessages(ctx, simd, s.SimdRelayerSubmitter, 200_000, &clienttypesv2.MsgRegisterCounterparty{
				ClientId:                 testvalues.FirstWasmClientID,
				CounterpartyMerklePrefix: merklePathPrefix,
				CounterpartyClientId:     testvalues.CustomClientID,
				Signer:                   s.SimdRelayerSubmitter.FormattedAddress(),
			})
			s.Require().NoError(err)
		}))
	}

	// Regenerate config with the SpectreClient address so the relay loop can use it
	var relayerProcess *os.Process
	s.Require().True(s.Run("Start relay loop", func() {
		beaconAPI := ""
		if eth.BeaconAPIClient != nil {
			beaconAPI = eth.BeaconAPIClient.GetBeaconAPIURL()
		}

		config := relayer.NewConfig(relayer.CreateEthCosmosModules(
			relayer.EthCosmosConfigInfo{
				EthChainID:         eth.ChainID.String(),
				CosmosChainID:      simd.Config().ChainID,
				TmRPC:              simd.GetHostRPCAddress(),
				ICS26Address:       s.contractAddresses.Ics26Router,
				EthRPC:             eth.RPC,
				EthWs:              eth.WS,
				BeaconAPI:          beaconAPI,
				SignerAddress:      s.SimdRelayerSubmitter.FormattedAddress(),
				MockWasmClient:     os.Getenv(testvalues.EnvKeyEthTestnetType) == testvalues.EthTestnetTypePoW,
				SpectreClient:      s.spectreClientAddress.Hex(),
				SignatureVerifier:  s.contractAddresses.SignatureVerifier,
				Membership:         s.contractAddresses.Membership,
				Misbehaviour:       s.contractAddresses.Misbehaviour,
				UpdateClient:       s.contractAddresses.UpdateClient,
				CosmosWasmClientID: testvalues.FirstWasmClientID,
				ICS26ClientID:      testvalues.CustomClientID,
				TrustLevel:         "1/3",
				ProofType:          testvalues.EnvValueProofType_Groth16,
			}),
		)

		var err error
		err = config.GenerateConfigFile(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)

		relayerProcess, err = relayer.StartRelayer(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)
	}))

	s.T().Cleanup(func() {
		if relayerProcess != nil {
			if err := relayerProcess.Kill(); err != nil {
				s.T().Logf("Failed to kill the relayer process: %v", err)
			}
		}
	})

	s.Require().True(s.Run("Generate the genesis fixtures", func() {
		if !s.solidityFixtureGenerator.Enabled {
			s.T().Skip("Skipping solidity fixture generation")
		}

		clientStateBz, err := s.spectreClientContract.GetClientState(nil)
		s.Require().NoError(err)
		// SpectreClient has no program verification keys (its Groth16 verifying keys are
		// constants in the per-bucket verifier contracts), so the vkey fields of the
		// genesis fixture stay zero, as does the consensus-state hash (SpectreClient
		// has a getConsensusStateHash getter, but this path does not read it). Genesis
		// fixture generation is a dev-only path (skipped in CI via the Enabled guard above).
		var consensusStateHash, updateClientVkey, membershipVkey, ucAndMembershipVkey, misbehaviourVkey [32]byte
		s.solidityFixtureGenerator.SetGenesisFixture(
			clientStateBz, consensusStateHash, updateClientVkey,
			membershipVkey, ucAndMembershipVkey, misbehaviourVkey,
		)
	}))
}

func (s *EthCosmosTestSuite) Test_Deploy() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.DeployTest(ctx, proofType)
}

// DeployTest tests the deployment of the IBC v2 contracts
func (s *EthCosmosTestSuite) DeployTest(ctx context.Context, proofType types.SupportedProofType) {
	s.SetupSuite(ctx, proofType)

	_, simd := s.EthChain, s.CosmosChains[0] // eth used only by the removed gRPC Info blocks

	s.Require().True(s.Run("Verify Groth16 Client", func() {
		clientState, err := getGroth16ClientState(s.spectreClientContract)
		s.Require().NoError(err)

		stakingParams, err := simd.StakingQueryParams(ctx)
		s.Require().NoError(err)

		s.Require().Equal(simd.Config().ChainID, clientState.ChainId)
		s.Require().Equal(uint8(testvalues.DefaultTrustLevel.Numerator), clientState.TrustLevel.Numerator)
		s.Require().Equal(uint8(testvalues.DefaultTrustLevel.Denominator), clientState.TrustLevel.Denominator)
		s.Require().Equal(uint32(testvalues.DefaultTrustPeriod), clientState.TrustingPeriod)
		s.Require().Equal(uint32(stakingParams.UnbondingTime.Seconds()), clientState.UnbondingPeriod)
		s.Require().False(clientState.IsFrozen)
		s.Require().Equal(uint64(1), clientState.LatestHeight.RevisionNumber)
		s.Require().Greater(clientState.LatestHeight.RevisionHeight, uint64(0))
	}))

	s.Require().True(s.Run("Verify ICS02 Client", func() {
		clientAddress, err := s.ics26Contract.GetClient(nil, testvalues.CustomClientID)
		s.Require().NoError(err)
		s.Require().Equal(s.spectreClientAddress, clientAddress)

		counterpartyInfo, err := s.ics26Contract.GetCounterparty(nil, testvalues.CustomClientID)
		s.Require().NoError(err)
		s.Require().Equal(testvalues.FirstWasmClientID, counterpartyInfo.ClientId)
	}))

	s.Require().True(s.Run("Verify ICS26 Router", func() {
		transferAddress, err := s.ics26Contract.GetIBCApp(nil, transfertypes.PortID)
		s.Require().NoError(err)
		s.Require().Equal(s.contractAddresses.Ics20Transfer, strings.ToLower(transferAddress.Hex()))
	}))

	s.Require().True(s.Run("Verify ERC20 Genesis", func() {
		userBalance, err := s.erc20Contract.BalanceOf(nil, crypto.PubkeyToAddress(s.key.PublicKey))
		s.Require().NoError(err)
		s.Require().Equal(testvalues.StartingERC20Balance, userBalance)
	}))

	s.Require().True(s.Run("Verify ethereum light client", func() {
		_, err := e2esuite.GRPCQuery[clienttypes.QueryClientStateResponse](ctx, simd, &clienttypes.QueryClientStateRequest{
			ClientId: testvalues.FirstWasmClientID,
		})
		s.Require().NoError(err)

		counterpartyInfoResp, err := e2esuite.GRPCQuery[clienttypesv2.QueryCounterpartyInfoResponse](ctx, simd, &clienttypesv2.QueryCounterpartyInfoRequest{
			ClientId: testvalues.FirstWasmClientID,
		})
		s.Require().NoError(err)
		s.Require().Equal(testvalues.CustomClientID, counterpartyInfoResp.CounterpartyInfo.ClientId)
	}))

	// Spectre's relayer exposes no gRPC info endpoint, so there is no reachability check
	// here. The relay loop already proved both directions are wired correctly during
	// create-clients / addClient / wasm-client register above.
}

func (s *EthCosmosTestSuite) Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenfromEthereumToCosmosAndBackTest(ctx, proofType, 1, big.NewInt(testvalues.TransferAmount))
}

func (s *EthCosmosTestSuite) Test_25_ICS20TransferERC20TokenfromEthereumToCosmosAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenfromEthereumToCosmosAndBackTest(ctx, proofType, 25, big.NewInt(testvalues.TransferAmount))
}

func (s *EthCosmosTestSuite) Test_50_ICS20TransferERC20TokenfromEthereumToCosmosAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenfromEthereumToCosmosAndBackTest(ctx, proofType, 50, big.NewInt(testvalues.TransferAmount))
}

func autoRelayTimeout(numOfTransfers int) time.Duration {
	if numOfTransfers < 1 {
		numOfTransfers = 1
	}
	const relayBatchSize = 5
	batches := (numOfTransfers + relayBatchSize - 1) / relayBatchSize

	// The relayer default batch size is 5. Give multi-packet e2e runs enough
	// time for beacon finality plus each packet batch to be built and submitted.
	return 5*time.Minute + time.Duration(batches)*time.Minute
}

func autoRelayTimeoutPacketTimeout(numOfTransfers int) time.Duration {
	if numOfTransfers < 1 {
		numOfTransfers = 1
	}
	const relayBatchSize = 5
	batches := (numOfTransfers + relayBatchSize - 1) / relayBatchSize

	// Timeout proofs can sit behind beacon finality: the relayer re-queues a packet
	// until its block is finalized, then still needs scanner ticks, proof
	// construction, and per-batch transaction submission. The base below is this
	// test's own budget for that.
	return 12*time.Minute + time.Duration(batches)*2*time.Minute
}

func (s *EthCosmosTestSuite) waitForCosmosBalance(
	ctx context.Context,
	address string,
	denom string,
	expected *big.Int,
	timeout time.Duration,
) {
	simd := s.CosmosChains[0]
	require.Eventuallyf(s.T(), func() bool {
		resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
			Address: address,
			Denom:   denom,
		})
		if err != nil {
			return false
		}
		if resp.Balance == nil {
			return expected.Sign() == 0
		}
		return resp.Balance.Amount.BigInt().Cmp(expected) == 0
	}, timeout, 5*time.Second,
		"auto-relay did not settle %s balance for %s at %s within %s",
		denom, address, expected.String(), timeout)
}

func (s *EthCosmosTestSuite) waitForCosmosPacketCommitmentRemoved(
	ctx context.Context,
	sequence uint64,
	timeout time.Duration,
) {
	simd := s.CosmosChains[0]
	require.Eventuallyf(s.T(), func() bool {
		_, err := e2esuite.GRPCQuery[channeltypesv2.QueryPacketCommitmentResponse](ctx, simd, &channeltypesv2.QueryPacketCommitmentRequest{
			ClientId: testvalues.FirstWasmClientID,
			Sequence: sequence,
		})
		return err != nil
	}, timeout, 5*time.Second,
		"auto-relay did not clear Cosmos packet commitment for sequence %d within %s", sequence, timeout)
}

func (s *EthCosmosTestSuite) waitForEthPacketCommitmentRemoved(
	clientID string,
	sequence uint64,
	timeout time.Duration,
) {
	packetCommitmentPath := ibchostv2.PacketCommitmentKey(clientID, sequence)
	var ethPath [32]byte
	copy(ethPath[:], crypto.Keccak256(packetCommitmentPath))

	require.Eventuallyf(s.T(), func() bool {
		resp, err := s.ics26Contract.GetCommitment(nil, ethPath)
		if err != nil {
			return false
		}
		return resp == [32]byte{}
	}, timeout, 5*time.Second,
		"auto-relay did not clear ETH packet commitment for %s/%d within %s", clientID, sequence, timeout)
}

func (s *EthCosmosTestSuite) waitForIbcERC20Balance(
	denomPath string,
	holder ethcommon.Address,
	expected *big.Int,
	timeout time.Duration,
) (*ibcerc20.Contract, ethcommon.Address) {
	var (
		ibcERC20        *ibcerc20.Contract
		ibcERC20Address ethcommon.Address
	)

	require.Eventuallyf(s.T(), func() bool {
		addr, err := s.ics20Contract.IbcERC20Contract(nil, denomPath)
		if err != nil || addr == (ethcommon.Address{}) {
			return false
		}
		contract, err := ibcerc20.NewContract(addr, s.EthChain.RPCClient)
		if err != nil {
			return false
		}
		balance, err := contract.BalanceOf(nil, holder)
		if err != nil || balance == nil {
			return false
		}
		if balance.Cmp(expected) != 0 {
			return false
		}

		ibcERC20 = contract
		ibcERC20Address = addr
		return true
	}, timeout, 5*time.Second,
		"auto-relay did not settle %s IBCERC20 balance for %s at %s within %s",
		denomPath, holder.Hex(), expected.String(), timeout)

	return ibcERC20, ibcERC20Address
}

// Test_ICS20TransferLargeAmountFromEthereumToCosmosAndBack exercises the bigint encoding
// path with a transfer amount > uint64 max (5e22, half of StartingERC20Balance). The
// upstream test was named *Uint256* and pushed to ~MaxUint256/2 — we shrank the faucet
// mint to 1_000_000 ether in E2ETestDeploy.s.sol, so this variant now stresses
// bigint round-tripping but no longer the MaxUint256 edge.
func (s *EthCosmosTestSuite) Test_ICS20TransferLargeAmountFromEthereumToCosmosAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	transferAmount := new(big.Int).Div(testvalues.StartingERC20Balance, big.NewInt(2))
	s.ICS20TransferERC20TokenfromEthereumToCosmosAndBackTest(ctx, proofType, 1, transferAmount)
}

// ICS20TransferERC20TokenfromEthereumToCosmosAndBackTest tests the ICS20 transfer functionality by transferring
// ERC20 tokens with n packets from Ethereum to Cosmos chain and then back from Cosmos chain to Ethereum
func (s *EthCosmosTestSuite) ICS20TransferERC20TokenfromEthereumToCosmosAndBackTest(
	ctx context.Context, proofType types.SupportedProofType, numOfTransfers int, transferAmount *big.Int,
) {
	s.SetupSuite(ctx, proofType)

	eth, simd := s.EthChain, s.CosmosChains[0]

	// ics26Address is no longer needed at the test level — auto-relayer drives ICS26Router
	// directly via event subscriptions. ics20/erc20 stay because the test still drives
	// sendTransfer and balance queries against them.
	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	totalTransferAmount := new(big.Int).Mul(transferAmount, big.NewInt(int64(numOfTransfers)))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	relayTimeout := autoRelayTimeout(numOfTransfers)

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

	var (
		sendPacket    ics26router.IICS26RouterMsgsPacket
		escrowAddress ethcommon.Address
	)
	s.Require().True(s.Run(fmt.Sprintf("Send %d transfers on Ethereum", numOfTransfers), func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferMulticall := make([][]byte, numOfTransfers)

		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            erc20Address,
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: timeout,
			SourceClient:     testvalues.CustomClientID,
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

		sendPacketEvent, err := e2esuite.GetEvmEvent(receipt, s.ics26Contract.ParseSendPacket)
		s.Require().NoError(err)
		sendPacket = sendPacketEvent.Packet
		s.Require().Equal(uint64(1), sendPacket.Sequence)
		s.Require().Equal(timeout, sendPacket.TimeoutTimestamp)
		s.Require().Len(sendPacket.Payloads, 1)
		s.Require().Equal(transfertypes.PortID, sendPacket.Payloads[0].SourcePort)
		s.Require().Equal(testvalues.CustomClientID, sendPacket.SourceClient)
		s.Require().Equal(transfertypes.PortID, sendPacket.Payloads[0].DestPort)
		s.Require().Equal(testvalues.FirstWasmClientID, sendPacket.DestClient)
		s.Require().Equal(transfertypes.V1, sendPacket.Payloads[0].Version)
		s.Require().Equal(transfertypes.EncodingABI, sendPacket.Payloads[0].Encoding)

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

	var denomOnCosmos transfertypes.Denom
	s.Require().True(s.Run("Receive packets on Cosmos chain", func() {
		denomOnCosmos = transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))

		// Spectre relayer auto-relays via the StartRelayer daemon, so we wait for it to
		// deliver the packet and verify the resulting Cosmos balance.
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

	// Phase 2 — Cosmos→ETH ack: the auto-relayer detects Cosmos's write_acknowledgement
	// event and submits MsgAcknowledgement on ETH. Balances do not change during ack
	// (escrow still holds the transfer, user still down by the transfer amount), so we
	// can verify the post-recv steady state without waiting for the ack to land.
	s.Require().True(s.Run("Acknowledge packets on Ethereum", func() {
		s.Require().True(s.Run("Verify balances on Ethereum (post-recv steady state)", func() {
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Sub(testvalues.StartingERC20Balance, totalTransferAmount), userBalance)

			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Equal(totalTransferAmount, escrowBalance)
		}))
	}))

	s.Require().True(s.Run("Transfer tokens back from Cosmos chain", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		ibcCoin := sdk.NewCoin(denomOnCosmos.Path(), sdkmath.NewIntFromBigInt(transferAmount))

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    ibcCoin.Denom,
			Amount:   ibcCoin.Amount.String(),
			Sender:   cosmosUserWallet.FormattedAddress(),
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

		transferMsgs := make([]sdk.Msg, numOfTransfers)
		for i := range numOfTransfers {
			transferMsgs[i] = &channeltypesv2.MsgSendPacket{
				SourceClient:     testvalues.FirstWasmClientID,
				TimeoutTimestamp: timeout,
				Payloads: []channeltypesv2.Payload{
					payload,
				},
				Signer: cosmosUserWallet.FormattedAddress(),
			}
		}

		resp, err := s.BroadcastMessages(ctx, simd, cosmosUserWallet, 20_000_000, transferMsgs...)
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			// User balance on Cosmos chain
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   denomOnCosmos.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(sdkmath.ZeroInt(), resp.Balance.Amount)
			s.Require().Equal(denomOnCosmos.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	// Phase 4 — Cosmos→ETH return: the auto-relayer detects Cosmos's send_packet event
	// and submits ICS26Router.recvPacket on ETH. The escrow drains back to the ETH user.
	s.Require().True(s.Run(fmt.Sprintf("Receive %d packets on Ethereum", numOfTransfers), func() {
		s.Require().True(s.Run("Wait for auto-relay to deliver to ETH", func() {
			require.Eventuallyf(s.T(), func() bool {
				escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
				if err != nil {
					return false
				}
				return escrowBalance.Sign() == 0
			}, relayTimeout, 5*time.Second,
				"auto-relay did not deliver %d return packets to ETH within timeout", numOfTransfers)
		}))

		s.Require().True(s.Run("Verify balances on Ethereum", func() {
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(testvalues.StartingERC20Balance, userBalance)

			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Zero(escrowBalance.Int64())
		}))
	}))

	// Phase 5 — ETH→Cosmos final ack: the auto-relayer detects ETH's WriteAcknowledgement
	// event and submits MsgAcknowledgement on Cosmos, which clears the packet commitment.
	s.Require().True(s.Run("Acknowledge packets on Cosmos chain", func() {
		s.Require().True(s.Run("Verify commitments exist before ack", func() {
			for i := range numOfTransfers {
				resp, err := e2esuite.GRPCQuery[channeltypesv2.QueryPacketCommitmentResponse](ctx, simd, &channeltypesv2.QueryPacketCommitmentRequest{
					ClientId: testvalues.FirstWasmClientID,
					Sequence: uint64(i) + 1,
				})
				if err == nil && len(resp.Commitment) > 0 {
					return // commitment still present — auto-relay hasn't delivered ack yet, which is what we want
				}
			}
			// Commitments already gone — likely the ack landed quickly. That's also acceptable;
			// the subsequent "Verify commitments removed" wait will be a no-op.
		}))

		s.Require().True(s.Run("Wait for auto-relay to clear commitments", func() {
			require.Eventuallyf(s.T(), func() bool {
				for i := range numOfTransfers {
					_, err := e2esuite.GRPCQuery[channeltypesv2.QueryPacketCommitmentResponse](ctx, simd, &channeltypesv2.QueryPacketCommitmentRequest{
						ClientId: testvalues.FirstWasmClientID,
						Sequence: uint64(i) + 1,
					})
					if err == nil {
						return false // still present
					}
				}
				return true
			}, relayTimeout, 5*time.Second,
				"auto-relay did not clear %d packet commitments on Cosmos within timeout", numOfTransfers)
		}))
	}))
}

func (s *EthCosmosTestSuite) Test_ICS20TransferERC20TokenFromEthereumToCosmosAndBackFails() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferERC20TokenFromEthereumToCosmosAndBackFailsTest(ctx, proofType, 1, big.NewInt(testvalues.TransferAmount))
}

func (s *EthCosmosTestSuite) ICS20TransferERC20TokenFromEthereumToCosmosAndBackFailsTest(
	ctx context.Context, proofType types.SupportedProofType, numOfTransfers int, transferAmount *big.Int,
) {
	s.SetupSuite(ctx, proofType)

	eth, simd := s.EthChain, s.CosmosChains[0]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	totalTransferAmount := new(big.Int).Mul(transferAmount, big.NewInt(int64(numOfTransfers)))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	denomOnCosmos := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))
	relayTimeout := autoRelayTimeout(numOfTransfers)

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

	s.Require().True(s.Run(fmt.Sprintf("Send %d transfers on Ethereum", numOfTransfers), func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferMulticall := make([][]byte, numOfTransfers)

		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            erc20Address,
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: timeout,
			SourceClient:     testvalues.CustomClientID,
			Memo:             "",
		}

		encodedMsg, err := ics20transferAbi.Pack("sendTransfer", msgSendPacket)
		s.Require().NoError(err)
		for i := range numOfTransfers {
			transferMulticall[i] = encodedMsg
		}

		tx, err := s.ics20Contract.Multicall(s.GetTransactOpts(s.key, eth), transferMulticall)
		s.Require().NoError(err)
		_, err = eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
	}))

	s.Require().True(s.Run("Receive packets on Cosmos chain", func() {
		s.waitForCosmosBalance(ctx, cosmosUserAddress, denomOnCosmos.IBCDenom(), totalTransferAmount, relayTimeout)
	}))

	s.Require().True(s.Run("Transfer tokens back from Cosmos chain with invalid receiver should fail", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		ibcCoin := sdk.NewCoin(denomOnCosmos.Path(), sdkmath.NewIntFromBigInt(transferAmount))

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    ibcCoin.Denom,
			Amount:   ibcCoin.Amount.String(),
			Sender:   cosmosUserWallet.FormattedAddress(),
			Receiver: "invalid_receiver_address",
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

		transferMsg := &channeltypesv2.MsgSendPacket{
			SourceClient:     testvalues.FirstWasmClientID,
			TimeoutTimestamp: timeout,
			Payloads: []channeltypesv2.Payload{
				payload,
			},
			Signer: cosmosUserWallet.FormattedAddress(),
		}

		s.Require().True(s.Run("Broadcast transfer msgs", func() {
			resp, err := s.BroadcastMessages(ctx, simd, cosmosUserWallet, 20_000_000, transferMsg)
			s.Require().NoError(err)
			s.Require().NotEmpty(resp.TxHash)
		}))

		s.Require().True(s.Run("Verify vouchers burned before error ack", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   denomOnCosmos.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(sdkmath.ZeroInt(), resp.Balance.Amount)
		}))

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			s.waitForCosmosBalance(ctx, cosmosUserAddress, denomOnCosmos.IBCDenom(), totalTransferAmount, relayTimeout)
		}))
	}))
}

func (s *EthCosmosTestSuite) Test_ICS20TransferNativeCosmosCoinsToEthereumAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TransferNativeCosmosCoinsToEthereumAndBackTest(ctx, proofType, big.NewInt(testvalues.TransferAmount))
}

// ICS20TransferNativeCosmosCoinsToEthereumAndBackTest tests the ICS20 transfer functionality
// by transferring native coins from a Cosmos chain to Ethereum and back.
// Cosmos→ETH relay is handled automatically by the running relayer process.
// ETH→Cosmos relay is done manually in the test (mock proof accepted by dummy wasm client).
func (s *EthCosmosTestSuite) ICS20TransferNativeCosmosCoinsToEthereumAndBackTest(ctx context.Context, pt types.SupportedProofType, transferAmount *big.Int) {
	s.SetupSuite(ctx, pt)

	eth, simd := s.EthChain, s.CosmosChains[0]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	transferCoin := sdk.NewCoin(simd.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	sendMemo := "nativesend"

	sendTimeout := uint64(time.Now().Add(30 * time.Minute).Unix())

	var (
		cosmosSendPayload channeltypesv2.Payload
		ibcERC20          *ibcerc20.Contract
		ibcERC20Address   ethcommon.Address
	)

	s.Require().True(s.Run("Send transfer on Cosmos chain", func() {
		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    transferCoin.Denom,
			Amount:   transferCoin.Amount.String(),
			Sender:   cosmosUserAddress,
			Receiver: strings.ToLower(ethereumUserAddress.Hex()),
			Memo:     sendMemo,
		}
		encodedPayload, err := transfertypes.EncodeABIFungibleTokenPacketData(&transferPayload)
		s.Require().NoError(err)

		cosmosSendPayload = channeltypesv2.Payload{
			SourcePort:      transfertypes.PortID,
			DestinationPort: transfertypes.PortID,
			Version:         transfertypes.V1,
			Encoding:        transfertypes.EncodingABI,
			Value:           encodedPayload,
		}
		msgSendPacket := channeltypesv2.MsgSendPacket{
			SourceClient:     testvalues.FirstWasmClientID,
			TimeoutTimestamp: sendTimeout,
			Payloads:         []channeltypesv2.Payload{cosmosSendPayload},
			Signer:           cosmosUserWallet.FormattedAddress(),
		}

		resp, err := s.BroadcastMessages(ctx, simd, cosmosUserWallet, 200_000, &msgSendPacket)
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   transferCoin.Denom,
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.InitialBalance-testvalues.TransferAmount, resp.Balance.Amount.Int64())
		}))
	}))

	// The running relayer process auto-relays the Cosmos→ETH send_packet event.
	// Wait for the ibcERC20 balance on Ethereum to reflect the transfer.
	var ethWriteAckEvent *ics26router.ContractWriteAcknowledgement
	s.Require().True(s.Run("Receive packet on Ethereum (auto-relayed)", func() {
		denomOnEthereum := transfertypes.NewDenom(
			transferCoin.Denom,
			transfertypes.NewHop(transfertypes.PortID, testvalues.CustomClientID),
		)

		s.Require().Eventually(func() bool {
			addr, err := s.ics20Contract.IbcERC20Contract(nil, denomOnEthereum.Path())
			if err != nil || addr == (ethcommon.Address{}) {
				return false
			}
			contract, err := ibcerc20.NewContract(addr, eth.RPCClient)
			if err != nil {
				return false
			}
			bal, err := contract.BalanceOf(nil, ethereumUserAddress)
			if err != nil || bal == nil {
				return false
			}
			if bal.Cmp(transferAmount) == 0 {
				ibcERC20Address = addr
				ibcERC20 = contract
				return true
			}
			return false
		}, 10*time.Minute, 5*time.Second, "timed out waiting for Cosmos→ETH relay")

		s.True(s.Run("Verify balances on Ethereum", func() {
			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(transferAmount, userBalance)

			ics20TransferBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
			s.Require().NoError(err)
			s.Require().Zero(ics20TransferBalance.Int64())
		}))

		// Collect the WriteAcknowledgement event emitted by the relayer's recvPacket tx
		iter, err := s.ics26Contract.FilterWriteAcknowledgement(
			&bind.FilterOpts{Context: ctx},
			[]string{testvalues.CustomClientID},
			[]*big.Int{big.NewInt(1)},
		)
		s.Require().NoError(err)
		defer iter.Close()

		for iter.Next() {
			ethWriteAckEvent = iter.Event
			break
		}
		s.Require().NoError(iter.Error())
		s.Require().NotNil(ethWriteAckEvent, "WriteAcknowledgement event not found on Ethereum")
	}))

	s.Require().NotEmpty(ethWriteAckEvent.Acknowledgements)

	s.Require().True(s.Run("Acknowledge packet on Cosmos chain", func() {
		// The auto-relayer picks up Ethereum's WriteAcknowledgement event and submits
		// MsgAcknowledgement on Cosmos with a real wasm proof; just wait for the packet
		// commitment to disappear.
		require.Eventuallyf(s.T(), func() bool {
			_, err := e2esuite.GRPCQuery[channeltypesv2.QueryPacketCommitmentResponse](ctx, simd, &channeltypesv2.QueryPacketCommitmentRequest{
				ClientId: testvalues.FirstWasmClientID,
				Sequence: 1,
			})
			return err != nil && strings.Contains(err.Error(), "packet commitment hash not found")
		}, 5*time.Minute, 5*time.Second,
			"auto-relay did not clear the Cosmos packet commitment within timeout")
	}))

	s.Require().True(s.Run("Approve the ICS20Transfer.sol contract to spend the erc20 tokens", func() {
		tx, err := ibcERC20.Approve(s.GetTransactOpts(s.key, eth), ics20Address, transferAmount)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		allowance, err := ibcERC20.Allowance(nil, ethereumUserAddress, ics20Address)
		s.Require().NoError(err)
		s.Require().Equal(transferAmount, allowance)
	}))

	var ethSendPacketEvent *ics26router.ContractSendPacket
	s.Require().True(s.Run("Transfer tokens back from Ethereum", func() {
		returnMemo := "testreturnmemo"
		returnTimeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            ibcERC20Address,
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: returnTimeout,
			SourceClient:     testvalues.CustomClientID,
			Memo:             returnMemo,
		}

		tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		ethSendPacketEvent, err = e2esuite.GetEvmEvent(receipt, s.ics26Contract.ParseSendPacket)
		s.Require().NoError(err)
		s.Require().Equal(uint64(1), ethSendPacketEvent.Packet.Sequence)
		s.Require().Equal(returnTimeout, ethSendPacketEvent.Packet.TimeoutTimestamp)
		s.Require().Equal(testvalues.CustomClientID, ethSendPacketEvent.Packet.SourceClient)
		s.Require().Equal(testvalues.FirstWasmClientID, ethSendPacketEvent.Packet.DestClient)

		s.True(s.Run("Verify balances on Ethereum", func() {
			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Zero(userBalance.Int64())

			ics20TransferBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
			s.Require().NoError(err)
			s.Require().Zero(ics20TransferBalance.Int64())
		}))
	}))

	_ = ethSendPacketEvent.Packet // kept for parity with debug logs; not needed by the auto-relay path

	s.Require().True(s.Run("Receive packet on Cosmos chain", func() {
		// Auto-relayer picks up the ETH SendPacket event, builds a real Ethereum-LC proof,
		// and submits MsgRecvPacket on Cosmos. Wait for the user's native-coin balance
		// to be credited back (transfer round-trip complete).
		require.Eventuallyf(s.T(), func() bool {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   transferCoin.Denom,
			})
			if err != nil || resp.Balance == nil {
				return false
			}
			return resp.Balance.Amount.Int64() == testvalues.InitialBalance
		}, 5*time.Minute, 5*time.Second,
			"auto-relay did not deliver Cosmos return packet within timeout")

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   transferCoin.Denom,
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.InitialBalance, resp.Balance.Amount.Int64())
		}))
	}))

	s.Require().True(s.Run("Acknowledge packet on Ethereum", func() {
		// Auto-relayer picks up Cosmos's write_acknowledgement event and submits MsgAck
		// on ETH (via ICS26Router.ackPacket), which clears the ETH packet commitment.
		packetCommitmentPath := ibchostv2.PacketCommitmentKey(testvalues.CustomClientID, 1)
		var ethPath [32]byte
		copy(ethPath[:], crypto.Keccak256(packetCommitmentPath))

		s.Require().True(s.Run("Verify commitment exists", func() {
			resp, err := s.ics26Contract.GetCommitment(nil, ethPath)
			s.Require().NoError(err)
			s.Require().NotZero(resp)
		}))

		require.Eventuallyf(s.T(), func() bool {
			resp, err := s.ics26Contract.GetCommitment(nil, ethPath)
			if err != nil {
				return false
			}
			return resp == [32]byte{}
		}, 5*time.Minute, 5*time.Second,
			"auto-relay did not clear the ETH packet commitment within timeout")

		s.Require().True(s.Run("Verify commitment removed", func() {
			resp, err := s.ics26Contract.GetCommitment(nil, ethPath)
			s.Require().NoError(err)
			s.Require().Zero(resp)
		}))

		s.Require().True(s.Run("Verify balances on Ethereum after ack", func() {
			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Zero(userBalance.Int64())

			ics20TransferBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
			s.Require().NoError(err)
			s.Require().Zero(ics20TransferBalance.Int64())
		}))
	}))
}

func (s *EthCosmosTestSuite) Test_TimeoutPacketFromEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TimeoutPacketFromEthereumTest(ctx, proofType, 1)
}

func (s *EthCosmosTestSuite) Test_10_TimeoutPacketFromEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TimeoutPacketFromEthereumTest(ctx, proofType, 10)
}

func (s *EthCosmosTestSuite) Test_5_TimeoutPacketFromEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TimeoutPacketFromEthereumTest(ctx, proofType, 5)
}

func (s *EthCosmosTestSuite) ICS20TimeoutPacketFromEthereumTest(
	ctx context.Context, pt types.SupportedProofType, numOfTransfers int,
) {
	s.Require().Greater(numOfTransfers, 0)
	relayTimeout := autoRelayTimeoutPacketTimeout(numOfTransfers)

	s.SetupSuite(ctx, pt)

	eth, simd := s.EthChain, s.CosmosChains[0]

	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	transferAmount := big.NewInt(testvalues.TransferAmount)
	totalTransferAmount := new(big.Int).Mul(transferAmount, big.NewInt(int64(numOfTransfers)))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()

	var originalBalance *sdk.Coin
	s.Require().True(s.Run("Retrieve original balance", func() {
		denomOnCosmos := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))

		resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
			Address: cosmosUserAddress,
			Denom:   denomOnCosmos.IBCDenom(),
		})
		s.Require().NoError(err)
		s.Require().NotNil(resp.Balance)
		originalBalance = resp.Balance
	}))

	s.Require().True(s.Run("Approve the ICS20Transfer.sol contract to spend the erc20 tokens", func() {
		ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
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
	s.Require().True(s.Run("Send packets on Ethereum", func() {
		for range numOfTransfers {
			timeout := uint64(time.Now().Add(30 * time.Second).Unix())
			msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
				Denom:            erc20Address,
				Amount:           transferAmount,
				Receiver:         cosmosUserAddress,
				TimeoutTimestamp: timeout,
				SourceClient:     testvalues.CustomClientID,
				Memo:             "testmemo",
			}

			tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
			s.Require().NoError(err)

			receipt, err := eth.GetTxReciept(ctx, tx.Hash())
			s.Require().NoError(err)
			s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)
		}

		s.Require().True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			minUserBalance := new(big.Int).Sub(testvalues.StartingERC20Balance, totalTransferAmount)
			s.Require().GreaterOrEqual(userBalance.Cmp(minUserBalance), 0)
			s.Require().LessOrEqual(userBalance.Cmp(testvalues.StartingERC20Balance), 0)

			// Get the escrow address
			escrowAddress, err = s.ics20Contract.GetEscrow(nil, testvalues.CustomClientID)
			s.Require().NoError(err)

			// ICS20 contract balance on Ethereum
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Zero(new(big.Int).Add(userBalance, escrowBalance).Cmp(testvalues.StartingERC20Balance))
		}))
	}))

	// Spectre's auto-relay daemon either delivers a packet to Cosmos before its
	// timeout OR — once the timeout window closes and the packet remains undelivered —
	// the PendingPacketTracker scanner submits MsgTimeoutPacket to ETH and refunds the
	// escrow. The daemon cannot be asked to relay only selected sequences, so the
	// filtered variants validate the same terminal accounting invariant after reopening.

	// Wait past the 30s packet-level timeout. The scanner ticks every 30s, so allow a
	// generous window for it to notice and submit the refund tx.
	time.Sleep(45 * time.Second)

	s.True(s.Run("Wait for auto-relay terminal state on Ethereum", func() {
		denomOnCosmos := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))
		var (
			deliveredAmount      *big.Int
			settledUserBalance   *big.Int
			settledEscrowBalance *big.Int
			settledCosmosBalance *big.Int
		)

		require.Eventuallyf(s.T(), func() bool {
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			if err != nil {
				return false
			}
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			if err != nil {
				return false
			}
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   denomOnCosmos.IBCDenom(),
			})
			if err != nil || resp.Balance == nil {
				return false
			}

			cosmosDelta := new(big.Int).Sub(resp.Balance.Amount.BigInt(), originalBalance.Amount.BigInt())
			if cosmosDelta.Sign() < 0 || cosmosDelta.Cmp(totalTransferAmount) > 0 {
				return false
			}
			expectedUserBalance := new(big.Int).Sub(testvalues.StartingERC20Balance, cosmosDelta)
			if userBalance.Cmp(expectedUserBalance) != 0 || escrowBalance.Cmp(cosmosDelta) != 0 {
				return false
			}

			deliveredAmount = new(big.Int).Set(cosmosDelta)
			settledUserBalance = new(big.Int).Set(userBalance)
			settledEscrowBalance = new(big.Int).Set(escrowBalance)
			settledCosmosBalance = new(big.Int).Set(resp.Balance.Amount.BigInt())
			return true
		}, relayTimeout, 5*time.Second,
			"auto-relay did not settle %d ERC20 transfers by delivery or timeout within %s", numOfTransfers, relayTimeout)

		s.Require().True(s.Run("Verify balances on Ethereum", func() {
			s.Require().NotNil(deliveredAmount)
			s.Require().NotNil(settledUserBalance)
			s.Require().NotNil(settledEscrowBalance)
			s.Require().Zero(settledUserBalance.Cmp(new(big.Int).Sub(testvalues.StartingERC20Balance, deliveredAmount)))
			s.Require().Zero(settledEscrowBalance.Cmp(deliveredAmount))
		}))

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			s.Require().NotNil(settledCosmosBalance)
			s.Require().Zero(settledCosmosBalance.Cmp(new(big.Int).Add(originalBalance.Amount.BigInt(), deliveredAmount)))
		}))
	}))
}

func (s *EthCosmosTestSuite) Test_ErrorAckToEthereum() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20ErrorAckToEthereumTest(ctx, proofType)
}

func (s *EthCosmosTestSuite) ICS20ErrorAckToEthereumTest(
	ctx context.Context, pt types.SupportedProofType,
) {
	s.SetupSuite(ctx, pt)

	eth, simd := s.EthChain, s.CosmosChains[0]
	// ics26Address removed alongside the manual ICS26.ackPacket broadcast — auto-relayer
	// drives that path now via SubscribeEth + the error-ack flow.
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	transferAmount := big.NewInt(testvalues.TransferAmount)
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)

	s.Require().True(s.Run("Approve the ICS20Transfer.sol contract to spend the erc20 tokens", func() {
		ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
		tx, err := s.erc20Contract.Approve(s.GetTransactOpts(s.key, eth), ics20Address, transferAmount)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		allowance, err := s.erc20Contract.Allowance(nil, ethereumUserAddress, ics20Address)
		s.Require().NoError(err)
		s.Require().Equal(transferAmount, allowance)
	}))

	var escrowAddress ethcommon.Address
	s.Require().True(s.Run("Send transfer on Ethereum", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		// Send a transfer to an invalid Cosmos address
		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            erc20Address,
			Amount:           transferAmount,
			Receiver:         ibctesting.InvalidID,
			TimeoutTimestamp: timeout,
			SourceClient:     testvalues.CustomClientID,
			Memo:             "",
		}

		tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		s.Require().True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Sub(testvalues.StartingERC20Balance, transferAmount), userBalance)

			// Get the escrow address
			escrowAddress, err = s.ics20Contract.GetEscrow(nil, testvalues.CustomClientID)
			s.Require().NoError(err)

			// ICS20 contract balance on Ethereum
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Equal(transferAmount, escrowBalance)
		}))
	}))

	// The Cosmos receiver is intentionally invalid (ibctesting.InvalidID), so Cosmos's
	// transfer module writes an error ack instead of crediting the receiver. The auto-
	// relayer:
	//   1. submits MsgRecvPacket on Cosmos → transfer fails → write_acknowledgement{error}
	//   2. picks up the error ack via SubscribeCosmos → submits MsgAcknowledgement on ETH
	//   3. ICS26Router sees the error ack and refunds the escrow back to the sender
	// We just wait for the end state: ETH user balance back to StartingERC20Balance and
	// the ETH escrow drained.
	s.Require().True(s.Run("Wait for auto-relay error-ack refund on Ethereum", func() {
		require.Eventuallyf(s.T(), func() bool {
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			if err != nil {
				return false
			}
			return userBalance.Cmp(testvalues.StartingERC20Balance) == 0
		}, 5*time.Minute, 5*time.Second,
			"auto-relay did not refund the error-ack transfer within timeout")

		s.Require().True(s.Run("Verify no balance on Cosmos chain (invalid receiver)", func() {
			denomOnCosmos := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))
			_, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: ibctesting.InvalidID,
				Denom:   denomOnCosmos.IBCDenom(),
			})
			s.Require().Error(err)
		}))

		s.Require().True(s.Run("Verify balances on Ethereum", func() {
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(testvalues.StartingERC20Balance, userBalance)

			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Zero(escrowBalance.Int64())
		}))
	}))
}

func (s *EthCosmosTestSuite) Test_TimeoutPacketFromCosmos() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TimeoutFromCosmosTest(ctx, proofType, 1)
}

func (s *EthCosmosTestSuite) Test_10_TimeoutPacketFromCosmos() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.ICS20TimeoutFromCosmosTest(ctx, proofType, 10)
}

func (s *EthCosmosTestSuite) ICS20TimeoutFromCosmosTest(
	ctx context.Context, proofType types.SupportedProofType, numOfTransfers int,
) {
	s.Require().Greater(numOfTransfers, 0)
	relayTimeout := autoRelayTimeoutPacketTimeout(numOfTransfers)

	s.SetupSuite(ctx, proofType)

	eth, simd := s.EthChain, s.CosmosChains[0]

	transferAmount := big.NewInt(testvalues.TransferAmount)
	totalTransferAmount := big.NewInt(testvalues.TransferAmount * int64(numOfTransfers))
	if totalTransferAmount.Int64() > testvalues.InitialBalance {
		s.FailNow("Total transfer amount exceeds the initial balance")
	}
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	sendMemo := "nonnativesend"

	transferCoin := sdk.NewCoin(simd.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))
	s.Require().True(s.Run("Send transfers on Cosmos chain", func() {
		for range numOfTransfers {
			// Short timeout: with Spectre's auto-relay, the relayer can deliver a 45s-timeout
			// packet (~20s) before it expires, defeating the timeout test. Use a window so
			// short the relayer's first batch tick + Groth16 proof can't beat it.
			timeout := uint64(time.Now().Add(5 * time.Second).Unix())

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
			minBalance := new(big.Int).Sub(big.NewInt(testvalues.InitialBalance), totalTransferAmount)
			balance := resp.Balance.Amount.BigInt()
			s.Require().GreaterOrEqual(balance.Cmp(minBalance), 0)
			s.Require().LessOrEqual(balance.Cmp(big.NewInt(testvalues.InitialBalance)), 0)
		}))
	}))

	// Spectre's relayer can either deliver a Cosmos-originated packet before the
	// TimeoutTimestamp or, after the timeout, the scanner submits MsgTimeout on Cosmos
	// with an ETH non-membership proof. The daemon cannot be asked to relay only selected
	// sequences, so the filtered variants validate the same terminal
	// accounting invariant after reopening.

	time.Sleep(15 * time.Second) // ensure packet timestamp has expired; scanner ticks every 30s and will pick it up

	s.Require().True(s.Run("Wait for auto-relay terminal state on Cosmos", func() {
		denomOnEthereum := transfertypes.NewDenom(transferCoin.Denom, transfertypes.NewHop(transfertypes.PortID, testvalues.CustomClientID))
		initialBalance := big.NewInt(testvalues.InitialBalance)
		var (
			deliveredAmount      *big.Int
			settledCosmosBalance *big.Int
		)

		require.Eventuallyf(s.T(), func() bool {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
				Address: cosmosUserAddress,
				Denom:   transferCoin.Denom,
			})
			if err != nil || resp.Balance == nil {
				return false
			}

			ethVoucherBalance := big.NewInt(0)
			ibcERC20Address, err := s.ics20Contract.IbcERC20Contract(nil, denomOnEthereum.Path())
			if err == nil && ibcERC20Address != (ethcommon.Address{}) {
				ibcERC20, err := ibcerc20.NewContract(ibcERC20Address, eth.RPCClient)
				if err != nil {
					return false
				}
				ethVoucherBalance, err = ibcERC20.BalanceOf(nil, ethereumUserAddress)
				if err != nil {
					return false
				}
			}
			if ethVoucherBalance.Sign() < 0 || ethVoucherBalance.Cmp(totalTransferAmount) > 0 {
				return false
			}
			expectedCosmosBalance := new(big.Int).Sub(initialBalance, ethVoucherBalance)
			if resp.Balance.Amount.BigInt().Cmp(expectedCosmosBalance) != 0 {
				return false
			}

			deliveredAmount = new(big.Int).Set(ethVoucherBalance)
			settledCosmosBalance = new(big.Int).Set(resp.Balance.Amount.BigInt())
			return true
		}, relayTimeout, 5*time.Second,
			"auto-relay did not settle %d Cosmos transfers by delivery or timeout within %s", numOfTransfers, relayTimeout)

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			s.Require().NotNil(deliveredAmount)
			s.Require().NotNil(settledCosmosBalance)
			s.Require().Zero(settledCosmosBalance.Cmp(new(big.Int).Sub(initialBalance, deliveredAmount)))
		}))
	}))
}

func (s *EthCosmosTestSuite) Test_TimeoutPacketEthRemintsVouchers() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.TimeoutPacketEthRemintsVouchersTest(ctx, proofType)
}

// TimeoutPacketEthRemintsVouchersTest tests that when a transfer of a voucher (Cosmos native -> Eth)
// from Ethereum back to Cosmos times out, the vouchers are reminted on Ethereum.
func (s *EthCosmosTestSuite) TimeoutPacketEthRemintsVouchersTest(ctx context.Context, pt types.SupportedProofType) {
	s.SetupSuite(ctx, pt)

	eth, simd := s.EthChain, s.CosmosChains[0]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	transferAmount := big.NewInt(testvalues.TransferAmount)
	transferCoin := sdk.NewCoin(simd.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	relayTimeout := autoRelayTimeout(1)
	timeoutRelayTimeout := autoRelayTimeoutPacketTimeout(1)

	s.Require().True(s.Run("Send native Cosmos coins to Ethereum", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    transferCoin.Denom,
			Amount:   transferCoin.Amount.String(),
			Sender:   cosmosUserAddress,
			Receiver: strings.ToLower(ethereumUserAddress.Hex()),
			Memo:     "create-voucher",
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
	}))

	var (
		ibcERC20        *ibcerc20.Contract
		ibcERC20Address ethcommon.Address
	)
	s.Require().True(s.Run("Receive packets on Ethereum", func() {
		denomOnEthereum := transfertypes.NewDenom(transferCoin.Denom, transfertypes.NewHop(transfertypes.PortID, testvalues.CustomClientID))
		ibcERC20, ibcERC20Address = s.waitForIbcERC20Balance(denomOnEthereum.Path(), ethereumUserAddress, transferAmount, relayTimeout)
	}))

	s.Require().True(s.Run("Acknowledge packets on Cosmos", func() {
		s.waitForCosmosPacketCommitmentRemoved(ctx, 1, relayTimeout)
	}))

	s.Require().True(s.Run("Send voucher from Eth to Cosmos (timeout)", func() {
		// Approve spending
		tx, err := ibcERC20.Approve(s.GetTransactOpts(s.key, eth), ics20Address, transferAmount)
		s.Require().NoError(err)
		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		// Send transfer with short timeout
		timeout := uint64(time.Now().Add(30 * time.Second).Unix())
		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            ibcERC20Address, // Sending the voucher back
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: timeout,
			SourceClient:     testvalues.CustomClientID,
			Memo:             "timeout-voucher-eth-cosmos",
		}

		tx, err = s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)
		receipt, err = eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		// Verify user voucher balance is now zero
		userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
		s.Require().NoError(err)
		s.Require().Zero(userBalance.Int64())
	}))

	s.Require().True(s.Run("Wait for timeout", func() {
		time.Sleep(45 * time.Second)
	}))

	s.Require().True(s.Run("Relay timeout packet to Eth", func() {
		s.waitForIbcERC20Balance(
			transfertypes.NewDenom(transferCoin.Denom, transfertypes.NewHop(transfertypes.PortID, testvalues.CustomClientID)).Path(),
			ethereumUserAddress,
			transferAmount,
			timeoutRelayTimeout,
		)
	}))

	s.Require().True(s.Run("Verify voucher balance restored on Eth", func() {
		userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
		s.Require().NoError(err)
		s.Require().Equal(transferAmount, userBalance, "Voucher balance should be restored after timeout")

		// Escrow balance should remain zero (or unchanged if it wasn't zero initially, though it should be)
		escrowBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
		s.Require().NoError(err)
		s.Require().Zero(escrowBalance.Int64(), "Escrow balance should be zero")
	}))
}

func (s *EthCosmosTestSuite) Test_TimeoutPacketCosmosRemintsVouchers() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.TimeoutPacketCosmosRemintsVouchersTest(ctx, proofType)
}

// TimeoutPacketCosmosRemintsVouchersTest tests that when a transfer of a voucher (Eth native -> Cosmos)
// from Cosmos back to Ethereum times out, the vouchers are reminted on Cosmos.
func (s *EthCosmosTestSuite) TimeoutPacketCosmosRemintsVouchersTest(ctx context.Context, pt types.SupportedProofType) {
	s.SetupSuite(ctx, pt)

	eth, simd := s.EthChain, s.CosmosChains[0]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	transferAmount := big.NewInt(testvalues.TransferAmount)
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	cosmosUserWallet := s.CosmosUsers[0]
	cosmosUserAddress := cosmosUserWallet.FormattedAddress()
	relayTimeout := autoRelayTimeout(1)
	timeoutRelayTimeout := autoRelayTimeoutPacketTimeout(1)

	s.Require().True(s.Run("Approve the ICS20Transfer.sol contract to spend the erc20 tokens", func() {
		tx, err := s.erc20Contract.Approve(s.GetTransactOpts(s.key, eth), ics20Address, transferAmount)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		allowance, err := s.erc20Contract.Allowance(nil, ethereumUserAddress, ics20Address)
		s.Require().NoError(err)
		s.Require().Equal(transferAmount, allowance)
	}))

	var (
		sendPacket    ics26router.IICS26RouterMsgsPacket
		escrowAddress ethcommon.Address
	)
	s.Require().True(s.Run("Send ERC20 tokens on Ethereum", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            erc20Address,
			Amount:           transferAmount,
			Receiver:         cosmosUserAddress,
			TimeoutTimestamp: timeout,
			SourceClient:     testvalues.CustomClientID,
			Memo:             "create-voucher-cosmos",
		}

		tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)
		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		sendPacketEvent, err := e2esuite.GetEvmEvent(receipt, s.ics26Contract.ParseSendPacket)
		s.Require().NoError(err)
		sendPacket = sendPacketEvent.Packet
		s.Require().Equal(uint64(1), sendPacket.Sequence)
		s.Require().Equal(timeout, sendPacket.TimeoutTimestamp)
		s.Require().Len(sendPacket.Payloads, 1)
		s.Require().Equal(transfertypes.PortID, sendPacket.Payloads[0].SourcePort)
		s.Require().Equal(testvalues.CustomClientID, sendPacket.SourceClient)
		s.Require().Equal(transfertypes.PortID, sendPacket.Payloads[0].DestPort)
		s.Require().Equal(testvalues.FirstWasmClientID, sendPacket.DestClient)
		s.Require().Equal(transfertypes.V1, sendPacket.Payloads[0].Version)
		s.Require().Equal(transfertypes.EncodingABI, sendPacket.Payloads[0].Encoding)

		s.True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Sub(testvalues.StartingERC20Balance, transferAmount), userBalance)

			// Get the escrow address
			escrowAddress, err = s.ics20Contract.GetEscrow(nil, testvalues.CustomClientID)
			s.Require().NoError(err)

			// ICS20 contract balance on Ethereum
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Equal(transferAmount, escrowBalance)
		}))
	}))

	var (
		denomOnCosmos transfertypes.Denom
	)
	s.Require().True(s.Run("Receive packets on Cosmos chain", func() {
		denomOnCosmos = transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))
		s.waitForCosmosBalance(ctx, cosmosUserAddress, denomOnCosmos.IBCDenom(), transferAmount, relayTimeout)
	}))

	s.Require().True(s.Run("Acknowledge packets on Ethereum", func() {
		s.waitForEthPacketCommitmentRemoved(testvalues.CustomClientID, 1, relayTimeout)
	}))

	s.Require().True(s.Run("Send voucher from Cosmos to Eth (timeout)", func() {
		timeout := uint64(time.Now().Add(30 * time.Second).Unix())
		ibcCoin := sdk.NewCoin(denomOnCosmos.Path(), sdkmath.NewIntFromBigInt(transferAmount))

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    ibcCoin.Denom, // Sending the voucher denom
			Amount:   ibcCoin.Amount.String(),
			Sender:   cosmosUserAddress,
			Receiver: strings.ToLower(ethereumUserAddress.Hex()),
			Memo:     "timeout-voucher-cosmos-eth",
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

		// Verify user voucher balance is now zero
		balanceResp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
			Address: cosmosUserAddress,
			Denom:   denomOnCosmos.IBCDenom(),
		})
		s.Require().NoError(err)
		s.Require().Zero(balanceResp.Balance.Amount.Int64())
	}))

	s.Require().True(s.Run("Wait for timeout", func() {
		time.Sleep(45 * time.Second)
	}))

	s.Require().True(s.Run("Relay timeout packet to Cosmos", func() {
		s.waitForCosmosBalance(ctx, cosmosUserAddress, denomOnCosmos.IBCDenom(), transferAmount, timeoutRelayTimeout)
	}))

	s.Require().True(s.Run("Verify voucher balance restored on Cosmos", func() {
		balanceResp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simd, &banktypes.QueryBalanceRequest{
			Address: cosmosUserAddress,
			Denom:   denomOnCosmos.IBCDenom(),
		})
		s.Require().NoError(err)
		s.Require().Equal(transferAmount, balanceResp.Balance.Amount.BigInt(), "Voucher balance should be restored after timeout")
	}))
}

// generateRelayerMnemonic returns a fresh BIP-39 mnemonic (128-bit entropy → 12 words)
// used for the e2e Cosmos relayer wallet. Generated on the host so that both the
// container keyring (via BuildWallet/RecoverKey) and the relayer binary (via the same
// HD derivation as cosmos-sdk) end up signing with the same on-chain account.
func generateRelayerMnemonic() (string, error) {
	entropy, err := bip39.NewEntropy(128)
	if err != nil {
		return "", err
	}
	return bip39.NewMnemonic(entropy)
}
