package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/suite"

	sdkmath "cosmossdk.io/math"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	interchaintest "github.com/cosmos/interchaintest/v10"
	"github.com/cosmos/interchaintest/v10/ibc"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/decentrio/spectre/packages/go-abigen/ics26router"
	"github.com/decentrio/spectre/packages/go-abigen/spectreclient"

	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/e2esuite"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/ethereum"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/relayer"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/testvalues"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types"
)

// SpectreClientTestSuite is a suite of tests that wraps TestSuite
// and can provide additional functionality
type SpectreClientTestSuite struct {
	e2esuite.TestSuite

	// Whether to generate fixtures for the solidity tests
	generateFixtures bool

	// Addresses of the deployed contracts
	contractAddresses    ethereum.DeployedContracts
	spectreClientAddress ethcommon.Address
	ics26Address         ethcommon.Address

	// The private key of a test account
	key *ecdsa.PrivateKey
	// The SpectreClient contract
	contract *spectreclient.Contract
	// The ICS26 router contract, needed for the relayer to pass proofs
	ics26Contract *ics26router.Contract
}

func readCosmosToEthConfig(configPath string) (relayer.CosmosToEthModuleConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return relayer.CosmosToEthModuleConfig{}, err
	}

	var config struct {
		Modules []struct {
			Name   string                          `json:"name"`
			Config relayer.CosmosToEthModuleConfig `json:"config"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return relayer.CosmosToEthModuleConfig{}, err
	}

	for _, module := range config.Modules {
		if module.Name == relayer.ModuleCosmosToEth {
			return module.Config, nil
		}
	}

	return relayer.CosmosToEthModuleConfig{}, fmt.Errorf("%s module not found in %s", relayer.ModuleCosmosToEth, configPath)
}

func chdirRepoRoot() error {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("failed to resolve test filename")
	}

	return os.Chdir(filepath.Clean(filepath.Join(filepath.Dir(filename), "../..")))
}

// SetupSuite calls the underlying SpectreClientTestSuite's SetupSuite method
// and deploys the SpectreClient contract
func (s *SpectreClientTestSuite) SetupSuite(ctx context.Context, proofType types.SupportedProofType) {
	s.TestSuite.SetupSuite(ctx)

	eth, simd := s.EthChain, s.CosmosChains[0]

	s.T().Logf("Setting up the test suite with proof type: %s", proofType.String())
	s.T().Cleanup(func() {
		os.Remove(testvalues.RelayerConfigFilePath)
	})

	s.Require().True(s.Run("Set up environment", func() {
		err := chdirRepoRoot()
		s.Require().NoError(err)

		s.key, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		relayerMnemonic, err := generateRelayerMnemonic()
		s.Require().NoError(err)
		simdRelayerWallet, err := simd.BuildWallet(ctx, "spectre-groth16-relayer", relayerMnemonic)
		s.Require().NoError(err)
		err = simd.SendFunds(ctx, interchaintest.FaucetAccountKeyName, ibc.WalletAmount{
			Address: simdRelayerWallet.FormattedAddress(),
			Denom:   simd.Config().Denom,
			Amount:  sdkmath.NewInt(testvalues.InitialBalance),
		})
		s.Require().NoError(err)
		cosmosPrivKey, err := cosmosPrivateKeyHexFromMnemonic(simdRelayerWallet.Mnemonic())
		s.Require().NoError(err)

		ethPrivKey := hex.EncodeToString(crypto.FromECDSA(s.key))

		// Use mock verifier in E2E tests (gnark Groth16 prover is the real prover)
		os.Setenv(testvalues.EnvKeyVerifier, testvalues.EnvValueVerifier_Mock)
		os.Setenv(testvalues.EnvKeyEthRPC, eth.RPC)
		os.Setenv(testvalues.EnvKeyTendermintRPC, simd.GetHostRPCAddress())
		os.Setenv("ETH_PRIVATE_KEY", ethPrivKey)
		os.Setenv(testvalues.EnvKeyOperatorPrivateKey, ethPrivKey)
		os.Setenv("COSMOS_PRIVATE_KEY", cosmosPrivKey)
		os.Setenv("COSMOS_CHAIN_ID", simd.Config().ChainID)
		os.Setenv("COSMOS_FEE_DENOM", simd.Config().Denom)
		s.generateFixtures = os.Getenv(testvalues.EnvKeyGenerateSolidityFixtures) == testvalues.EnvValueGenerateFixtures_True
	}))

	s.Require().True(s.Run("Deploy IBC contracts", func() {
		stdout, err := eth.ForgeScript(s.key, testvalues.E2EDeployScriptPath)
		s.Require().NoError(err)

		s.contractAddresses, err = ethereum.GetEthContractsFromDeployOutput(string(stdout))
		s.Require().NoError(err)
		s.ics26Address = ethcommon.HexToAddress(s.contractAddresses.Ics26Router)
		s.ics26Contract, err = ics26router.NewContract(s.ics26Address, eth.RPCClient)
		s.Require().NoError(err)
	}))

	var relayerProcess *os.Process
	s.Require().True(s.Run("Create clients", func() {
		beaconAPI := ""
		// The BeaconAPIClient is nil when the testnet is `pow`
		if eth.BeaconAPIClient != nil {
			beaconAPI = eth.BeaconAPIClient.GetBeaconAPIURL()
		}

		config := relayer.NewConfig(relayer.CreateEthCosmosModules(
			relayer.EthCosmosConfigInfo{
				EthChainID:         eth.ChainID.String(),
				CosmosChainID:      simd.Config().ChainID,
				TmRPC:              simd.GetHostRPCAddress(),
				ICS26Address:       s.ics26Address.Hex(),
				EthRPC:             eth.RPC,
				EthWs:              eth.WS,
				BeaconAPI:          beaconAPI,
				SignerAddress:      "",   // unused
				MockWasmClient:     true, // unused
				SignatureVerifier:  s.contractAddresses.SignatureVerifier,
				Membership:         s.contractAddresses.Membership,
				Misbehaviour:       s.contractAddresses.Misbehaviour,
				UpdateClient:       s.contractAddresses.UpdateClient,
				CosmosWasmClientID: testvalues.FirstWasmClientID,
				ICS26ClientID:      testvalues.CustomClientID,
				TrustLevel:         "1/3",
				ProofType:          proofType.String(),
			}),
		)

		err := config.GenerateConfigFile(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)

		// This suite only exercises the Spectre Cosmos client on Ethereum; the full
		// create-clients flow also provisions the Ethereum wasm client on Cosmos and
		// requires a stored wasm checksum in PoS mode.
		err = relayer.RunCreateClientsEth(testvalues.RelayerConfigFilePath, "--trust-level", "1/3")
		s.Require().NoError(err)

		cosmosToEthConfig, err := readCosmosToEthConfig(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)
		s.Require().NotEmpty(cosmosToEthConfig.SpectreClient)
		s.spectreClientAddress = ethcommon.HexToAddress(cosmosToEthConfig.SpectreClient)

		s.contract, err = spectreclient.NewContract(s.spectreClientAddress, eth.RPCClient)
		s.Require().NoError(err)
	}))

	s.Require().True(s.Run("Start Relayer", func() {
		var err error
		relayerProcess, err = relayer.StartRelayer(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)
	}))

	s.T().Cleanup(func() {
		if relayerProcess != nil {
			err := relayerProcess.Kill()
			if err != nil {
				s.T().Logf("Failed to kill the relayer process: %v", err)
			}
		}
	})
}

// TestWithSpectreClientTestSuite is the boilerplate code that allows the test suite to be run
func TestWithSpectreClientTestSuite(t *testing.T) {
	suite.Run(t, new(SpectreClientTestSuite))
}

func (s *SpectreClientTestSuite) Test_Deploy() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.DeployTest(ctx, proofType)
}

// DeployTest tests the deployment of the SpectreClient contract with the given arguments
func (s *SpectreClientTestSuite) DeployTest(ctx context.Context, proofType types.SupportedProofType) {
	s.SetupSuite(ctx, proofType)

	_, simd := s.EthChain, s.CosmosChains[0]

	s.Require().True(s.Run("Verify deployment", func() {
		clientState, err := getGroth16ClientState(s.contract)
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
}

func (s *SpectreClientTestSuite) Test_UpdateClient() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()
	s.UpdateClientTest(ctx, proofType)
}

// UpdateClientTest tests the update client functionality
func (s *SpectreClientTestSuite) UpdateClientTest(ctx context.Context, proofType types.SupportedProofType) {
	s.SetupSuite(ctx, proofType)

	_, simd := s.EthChain, s.CosmosChains[0]

	if s.generateFixtures {
		s.T().Log("Generate fixtures is set to true, but TestUpdateClient does not support it (yet)")
	}

	s.Require().True(s.Run("Update client", func() {
		clientState, err := getGroth16ClientState(s.contract)
		s.Require().NoError(err)

		initialHeight := clientState.LatestHeight.RevisionHeight

		s.UpdateClient(ctx)

		clientState, err = getGroth16ClientState(s.contract)
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
		s.Require().Greater(clientState.LatestHeight.RevisionHeight, initialHeight)
	}))
}

func (s *SpectreClientTestSuite) UpdateClient(ctx context.Context) clienttypes.Height {
	var initialHeight uint64
	s.Require().True(s.Run("Get the initial height", func() {
		clientState, err := getGroth16ClientState(s.contract)
		s.Require().NoError(err)
		s.Require().NotZero(clientState.LatestHeight.RevisionHeight)

		initialHeight = clientState.LatestHeight.RevisionHeight
	}))

	var finalHeight spectreclient.IICS02ClientMsgsHeight
	s.Require().True(s.Run("Update the client on Ethereum", func() {
		err := relayer.RunUpdateClient(testvalues.RelayerConfigFilePath)
		s.Require().NoError(err)

		s.Require().True(s.Run("Verify the client state is updated", func() {
			clientState, err := getGroth16ClientState(s.contract)
			s.Require().NoError(err)
			s.Require().NotZero(clientState.LatestHeight.RevisionHeight)

			finalHeight = clientState.LatestHeight
			s.Require().Greater(finalHeight.RevisionHeight, initialHeight)
		}))
	}))

	return clienttypes.NewHeight(finalHeight.RevisionNumber, finalHeight.RevisionHeight)
}

// TODO: Port Test_Membership, Test_UpdateClientAndMembership, and Test_DoubleSignMisbehaviour
// to work with new SpectreClient contract ABI (struct fields changed from SP1 upstream).
