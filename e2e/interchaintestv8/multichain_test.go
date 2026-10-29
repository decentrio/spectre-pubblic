package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	comettypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/suite"

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
	commitmenttypes "github.com/cosmos/ibc-go/v10/modules/core/23-commitment/types"
	ibchostv2 "github.com/cosmos/ibc-go/v10/modules/core/24-host/v2"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
	ibctm "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"
	ibctesting "github.com/cosmos/ibc-go/v10/testing"

	interchaintest "github.com/cosmos/interchaintest/v10"
	ictcosmos "github.com/cosmos/interchaintest/v10/chain/cosmos"
	"github.com/cosmos/interchaintest/v10/ibc"

	"github.com/decentrio/spectre/packages/go-abigen/ibcerc20"
	"github.com/decentrio/spectre/packages/go-abigen/ics20transfer"
	"github.com/decentrio/spectre/packages/go-abigen/ics26router"
	"github.com/decentrio/spectre/packages/go-abigen/spectreclient"

	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/chainconfig"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/cosmos"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/e2esuite"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/ethereum"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/relayer"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/testvalues"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types"
	"github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types/erc20"
)

type MultichainTestSuite struct {
	e2esuite.TestSuite

	// The private key of a test account
	key *ecdsa.PrivateKey
	// The private key of the faucet account of interchaintest
	deployer *ecdsa.PrivateKey

	contractAddresses          ethereum.DeployedContracts
	chainASpectreClientAddress ethcommon.Address
	chainBSpectreClientAddress ethcommon.Address

	chainASpectreClientContract *spectreclient.Contract
	chainBSpectreClientContract *spectreclient.Contract
	ics26Contract               *ics26router.Contract
	ics20Contract               *ics20transfer.Contract
	erc20Contract               *erc20.Contract

	SimdARelayerSubmitter ibc.Wallet
	SimdBRelayerSubmitter ibc.Wallet
	EthRelayerSubmitter   *ecdsa.PrivateKey

	simdACosmosPrivateKey string
	simdBCosmosPrivateKey string
}

const (
	multichainRelayerConfigAPath = "relayer/config-multichain-a.json"
	multichainRelayerConfigBPath = "relayer/config-multichain-b.json"

	multichainChainAUniversalClientID = "simd-1-client-0"
	multichainChainBUniversalClientID = "simd-2-client-0"
)

// TestWithMultichainTestSuite is the boilerplate code that allows the test suite to be run
func TestWithMultichainTestSuite(t *testing.T) {
	suite.Run(t, new(MultichainTestSuite))
}

func cosmosPrivateKeyHexFromMnemonic(mnemonic string) (string, error) {
	seed := bip39.NewSeed(mnemonic, "")
	masterPriv, chainCode := hd.ComputeMastersFromSeed(seed)
	derivedPrivKeyBytes, err := hd.DerivePrivateKeyForPath(masterPriv, chainCode, "m/44'/118'/0'/0/0")
	if err != nil {
		return "", err
	}

	cosmosPrivKey := cosmossecp256k1.PrivKey{Key: derivedPrivKeyBytes}
	return hex.EncodeToString(cosmosPrivKey.Key), nil
}

func (s *MultichainTestSuite) setCosmosRelayerEnv(chain *ictcosmos.CosmosChain, privateKeyHex string) {
	os.Setenv("COSMOS_PRIVATE_KEY", privateKeyHex)
	os.Setenv("COSMOS_CHAIN_ID", chain.Config().ChainID)
	os.Setenv("COSMOS_FEE_DENOM", chain.Config().Denom)
}

func (s *MultichainTestSuite) generateEthCosmosRelayerConfig(
	eth ethereum.Ethereum,
	chain *ictcosmos.CosmosChain,
	signer ibc.Wallet,
	ics26ClientID string,
	spectreClientHex string,
	configPath string,
	proofType types.SupportedProofType,
) {
	beaconAPI := ""
	if eth.BeaconAPIClient != nil {
		beaconAPI = eth.BeaconAPIClient.GetBeaconAPIURL()
	}

	config := relayer.NewConfig(relayer.CreateEthCosmosModules(
		relayer.EthCosmosConfigInfo{
			EthChainID:         eth.ChainID.String(),
			CosmosChainID:      chain.Config().ChainID,
			TmRPC:              chain.GetHostRPCAddress(),
			ICS26Address:       s.contractAddresses.Ics26Router,
			EthRPC:             eth.RPC,
			EthWs:              eth.WS,
			BeaconAPI:          beaconAPI,
			SignerAddress:      signer.FormattedAddress(),
			MockWasmClient:     os.Getenv(testvalues.EnvKeyEthTestnetType) == testvalues.EthTestnetTypePoW,
			SpectreClient:      spectreClientHex,
			SignatureVerifier:  s.contractAddresses.SignatureVerifier,
			Membership:         s.contractAddresses.Membership,
			Misbehaviour:       s.contractAddresses.Misbehaviour,
			UpdateClient:       s.contractAddresses.UpdateClient,
			CosmosWasmClientID: testvalues.FirstWasmClientID,
			ICS26ClientID:      ics26ClientID,
			TrustLevel:         "1/3",
			ProofType:          proofType.String(),
		}),
	)

	err := config.GenerateConfigFile(configPath)
	s.Require().NoError(err)
}

func (s *MultichainTestSuite) createEthCosmosClients(
	ctx context.Context,
	eth ethereum.Ethereum,
	chain *ictcosmos.CosmosChain,
	signer ibc.Wallet,
	cosmosPrivateKey string,
	ics26ClientID string,
	configPath string,
	proofType types.SupportedProofType,
) ethcommon.Address {
	s.setCosmosRelayerEnv(chain, cosmosPrivateKey)

	checksumHex := s.StoreEthereumLightClient(ctx, chain, signer)
	s.Require().NotEmpty(checksumHex)

	ethRelayerAddr := crypto.PubkeyToAddress(s.deployer.PublicKey)
	preNonce, err := eth.RPCClient.PendingNonceAt(ctx, ethRelayerAddr)
	s.Require().NoError(err)
	spectreClientAddress := crypto.CreateAddress(ethRelayerAddr, preNonce)

	s.generateEthCosmosRelayerConfig(eth, chain, signer, ics26ClientID, "", configPath, proofType)

	wasmChecksum := ""
	if eth.BeaconAPIClient != nil {
		wasmChecksum = checksumHex
	}
	err = relayer.RunCreateClients(configPath, wasmChecksum, "1/3")
	s.Require().NoError(err)

	clientAddress, err := s.ics26Contract.GetClient(nil, ics26ClientID)
	s.Require().NoError(err)
	s.Require().Equal(spectreClientAddress, clientAddress)

	if eth.BeaconAPIClient == nil {
		s.createMockEthereumLightClient(ctx, chain, signer, checksumHex, ics26ClientID)
	}

	s.generateEthCosmosRelayerConfig(eth, chain, signer, ics26ClientID, spectreClientAddress.Hex(), configPath, proofType)

	return spectreClientAddress
}

func (s *MultichainTestSuite) createMockEthereumLightClient(
	ctx context.Context,
	chain *ictcosmos.CosmosChain,
	signer ibc.Wallet,
	checksumHex string,
	counterpartyClientID string,
) {
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

	msg, err := clienttypes.NewMsgCreateClient(&wasmClientState, &wasmConsensusState, signer.FormattedAddress())
	s.Require().NoError(err)

	resp, err := s.BroadcastMessages(ctx, chain, signer, 200_000, msg)
	s.Require().NoError(err)

	clientID, err := cosmos.GetEventValue(resp.Events, clienttypes.EventTypeCreateClient, clienttypes.AttributeKeyClientID)
	s.Require().NoError(err)
	s.Require().Equal(testvalues.FirstWasmClientID, clientID)

	_, err = s.BroadcastMessages(ctx, chain, signer, 200_000, &clienttypesv2.MsgRegisterCounterparty{
		ClientId:                 testvalues.FirstWasmClientID,
		CounterpartyClientId:     counterpartyClientID,
		CounterpartyMerklePrefix: [][]byte{[]byte("")},
		Signer:                   signer.FormattedAddress(),
	})
	s.Require().NoError(err)
}

func (s *MultichainTestSuite) createTendermintClient(
	ctx context.Context,
	src *ictcosmos.CosmosChain,
	dst *ictcosmos.CosmosChain,
	signer ibc.Wallet,
	expectedClientID string,
) {
	header, err := s.FetchCosmosHeader(ctx, src)
	s.Require().NoError(err)
	s.Require().Greater(header.Height, int64(0))

	stakingParams, err := src.StakingQueryParams(ctx)
	s.Require().NoError(err)

	latestHeight := clienttypes.NewHeight(clienttypes.ParseChainID(src.Config().ChainID), uint64(header.Height))
	clientState := ibctm.NewClientState(
		src.Config().ChainID,
		ibctm.NewFractionFromTm(testvalues.DefaultTrustLevel),
		time.Duration(testvalues.DefaultTrustPeriod)*time.Second,
		stakingParams.UnbondingTime,
		ibctesting.MaxClockDrift,
		latestHeight,
		commitmenttypes.GetSDKSpecs(),
		ibctesting.UpgradePath,
	)
	consensusState := ibctm.NewConsensusState(
		header.Time,
		commitmenttypes.NewMerkleRoot(header.AppHash),
		header.NextValidatorsHash,
	)

	msg, err := clienttypes.NewMsgCreateClient(clientState, consensusState, signer.FormattedAddress())
	s.Require().NoError(err)

	resp, err := s.BroadcastMessages(ctx, dst, signer, 2_000_000, msg)
	s.Require().NoError(err)

	clientID, err := cosmos.GetEventValue(resp.Events, clienttypes.EventTypeCreateClient, clienttypes.AttributeKeyClientID)
	s.Require().NoError(err)
	s.Require().Equal(expectedClientID, clientID)
}

func (s *MultichainTestSuite) packetFromSendPacketResponse(
	resp *sdk.TxResponse,
	sourceClient string,
	destinationClient string,
	timeout uint64,
	payload channeltypesv2.Payload,
) channeltypesv2.Packet {
	sequenceStr, err := cosmos.GetEventValue(resp.Events, channeltypesv2.EventTypeSendPacket, channeltypesv2.AttributeKeySequence)
	s.Require().NoError(err)

	sequence, err := strconv.ParseUint(sequenceStr, 10, 64)
	s.Require().NoError(err)

	eventSrcClient, err := cosmos.GetEventValue(resp.Events, channeltypesv2.EventTypeSendPacket, channeltypesv2.AttributeKeySrcClient)
	s.Require().NoError(err)
	s.Require().Equal(sourceClient, eventSrcClient)

	eventDstClient, err := cosmos.GetEventValue(resp.Events, channeltypesv2.EventTypeSendPacket, channeltypesv2.AttributeKeyDstClient)
	s.Require().NoError(err)
	s.Require().Equal(destinationClient, eventDstClient)

	return channeltypesv2.NewPacket(sequence, sourceClient, destinationClient, timeout, payload)
}

func (s *MultichainTestSuite) queryIBCProof(
	ctx context.Context,
	chain *ictcosmos.CosmosChain,
	key []byte,
	proofHeight uint64,
) ([]byte, clienttypes.Height) {
	s.Require().Greater(proofHeight, uint64(1))

	resp, err := e2esuite.ABCIQuery(ctx, chain, &abci.RequestQuery{
		Path:   fmt.Sprintf("store/%s/key", ibcexported.StoreKey),
		Data:   key,
		Height: int64(proofHeight) - 1,
		Prove:  true,
	})
	s.Require().NoError(err)
	s.Require().Equal(uint32(0), resp.Code, resp.Log)
	s.Require().Equal(int64(proofHeight)-1, resp.Height)
	s.Require().Equal(key, resp.Key)
	s.Require().NotEmpty(resp.Value)
	s.Require().NotEmpty(resp.ProofOps.Ops)

	merkleProof, err := commitmenttypes.ConvertProofs(resp.ProofOps)
	s.Require().NoError(err)

	proof, err := proto.Marshal(&merkleProof)
	s.Require().NoError(err)

	revision := clienttypes.ParseChainID(chain.Config().ChainID)
	return proof, clienttypes.NewHeight(revision, uint64(resp.Height)+1)
}

type multichainValidatorsPager interface {
	Validators(ctx context.Context, height *int64, page, perPage *int) (*coretypes.ResultValidators, error)
}

func fetchMultichainValidators(client multichainValidatorsPager, height int64) ([]*comettypes.Validator, error) {
	const cometBFTMaxPerPage = 100

	var validators []*comettypes.Validator
	perPage := cometBFTMaxPerPage
	for page := 1; ; page++ {
		currentPage := page
		resp, err := client.Validators(context.Background(), &height, &currentPage, &perPage)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch validators at height %d page %d: %w", height, page, err)
		}

		validators = append(validators, resp.Validators...)
		if len(validators) >= resp.Total || len(resp.Validators) == 0 {
			break
		}
	}

	return validators, nil
}

func fetchMultichainValidatorSet(client multichainValidatorsPager, height int64, proposerAddress comettypes.Address) (*comettypes.ValidatorSet, error) {
	validators, err := fetchMultichainValidators(client, height)
	if err != nil {
		return nil, err
	}

	valSet := comettypes.NewValidatorSet(validators)
	for _, validator := range validators {
		if validator != nil && bytes.Equal(validator.Address.Bytes(), proposerAddress.Bytes()) {
			valSet.Proposer = validator
			break
		}
	}
	if valSet.Proposer == nil {
		return nil, fmt.Errorf("proposer %X not found in validator set at height %d", proposerAddress.Bytes(), height)
	}

	return valSet, nil
}

func (s *MultichainTestSuite) buildTendermintUpdateHeader(
	ctx context.Context,
	src *ictcosmos.CosmosChain,
	trustedHeight clienttypes.Height,
	targetHeight uint64,
) *ibctm.Header {
	rpcClient, err := rpchttp.New(src.GetHostRPCAddress(), "/websocket")
	s.Require().NoError(err)

	targetHeightInt := int64(targetHeight)
	targetCommit, err := rpcClient.Commit(ctx, &targetHeightInt)
	s.Require().NoError(err)

	targetValidatorSet, err := fetchMultichainValidatorSet(rpcClient, targetHeightInt, targetCommit.SignedHeader.Header.ProposerAddress)
	s.Require().NoError(err)
	s.Require().True(bytes.Equal(targetValidatorSet.Hash(), targetCommit.SignedHeader.Header.ValidatorsHash.Bytes()))

	trustedHeightInt := int64(trustedHeight.RevisionHeight)
	trustedCommit, err := rpcClient.Commit(ctx, &trustedHeightInt)
	s.Require().NoError(err)

	trustedValidatorSet, err := fetchMultichainValidatorSet(rpcClient, trustedHeightInt, trustedCommit.SignedHeader.Header.ProposerAddress)
	s.Require().NoError(err)
	s.Require().True(bytes.Equal(trustedValidatorSet.Hash(), trustedCommit.SignedHeader.Header.ValidatorsHash.Bytes()))

	signedHeaderProto := targetCommit.SignedHeader.ToProto()

	validatorSetProto, err := targetValidatorSet.ToProto()
	s.Require().NoError(err)

	trustedValidatorSetProto, err := trustedValidatorSet.ToProto()
	s.Require().NoError(err)

	return &ibctm.Header{
		SignedHeader:      signedHeaderProto,
		ValidatorSet:      validatorSetProto,
		TrustedHeight:     trustedHeight,
		TrustedValidators: trustedValidatorSetProto,
	}
}

func (s *MultichainTestSuite) updateTendermintClient(
	ctx context.Context,
	src *ictcosmos.CosmosChain,
	dst *ictcosmos.CosmosChain,
	signer ibc.Wallet,
	clientID string,
	targetHeight uint64,
) {
	clientStateResp, err := e2esuite.GRPCQuery[clienttypes.QueryClientStateResponse](ctx, dst, &clienttypes.QueryClientStateRequest{
		ClientId: clientID,
	})
	s.Require().NoError(err)

	var clientState ibctm.ClientState
	err = proto.Unmarshal(clientStateResp.ClientState.Value, &clientState)
	s.Require().NoError(err)

	if clientState.LatestHeight.RevisionHeight >= targetHeight {
		return
	}

	header := s.buildTendermintUpdateHeader(ctx, src, clientState.LatestHeight, targetHeight)
	msg, err := clienttypes.NewMsgUpdateClient(clientID, header, signer.FormattedAddress())
	s.Require().NoError(err)

	_, err = s.BroadcastMessages(ctx, dst, signer, 2_000_000, msg)
	s.Require().NoError(err)
}

func (s *MultichainTestSuite) relayCosmosPacket(
	ctx context.Context,
	src *ictcosmos.CosmosChain,
	dst *ictcosmos.CosmosChain,
	signer ibc.Wallet,
	dstClientID string,
	packet channeltypesv2.Packet,
) {
	latestHeight, err := src.Height(ctx)
	s.Require().NoError(err)
	s.Require().Greater(latestHeight, int64(2))

	proofHeight := uint64(latestHeight - 1)
	s.updateTendermintClient(ctx, src, dst, signer, dstClientID, proofHeight)

	proof, proofClientHeight := s.queryIBCProof(ctx, src, ibchostv2.PacketCommitmentKey(packet.SourceClient, packet.Sequence), proofHeight)
	msg := channeltypesv2.NewMsgRecvPacket(packet, proof, proofClientHeight, signer.FormattedAddress())

	_, err = s.BroadcastMessages(ctx, dst, signer, 3_000_000, msg)
	s.Require().NoError(err)
}

func (s *MultichainTestSuite) waitForCosmosBalance(
	ctx context.Context,
	chain *ictcosmos.CosmosChain,
	address string,
	denom string,
	expected *big.Int,
	timeout time.Duration,
) {
	s.Require().Eventually(func() bool {
		resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, chain, &banktypes.QueryBalanceRequest{
			Address: address,
			Denom:   denom,
		})
		if err != nil || resp.Balance == nil {
			return false
		}

		return resp.Balance.Amount.BigInt().Cmp(expected) == 0
	}, timeout, 5*time.Second, "timed out waiting for Cosmos balance %s/%s to become %s", address, denom, expected.String())
}

func (s *MultichainTestSuite) waitForIbcERC20Balance(
	eth ethereum.Ethereum,
	denomPath string,
	account ethcommon.Address,
	expected *big.Int,
	timeout time.Duration,
) (ethcommon.Address, *ibcerc20.Contract) {
	var ibcERC20Address ethcommon.Address
	var ibcERC20 *ibcerc20.Contract

	s.Require().Eventually(func() bool {
		addr, err := s.ics20Contract.IbcERC20Contract(nil, denomPath)
		if err != nil || addr == (ethcommon.Address{}) {
			return false
		}

		contract, err := ibcerc20.NewContract(addr, eth.RPCClient)
		if err != nil {
			return false
		}

		balance, err := contract.BalanceOf(nil, account)
		if err != nil || balance == nil {
			return false
		}
		if balance.Cmp(expected) != 0 {
			return false
		}

		ibcERC20Address = addr
		ibcERC20 = contract
		return true
	}, timeout, 5*time.Second, "timed out waiting for IBC ERC20 balance for %s", denomPath)

	return ibcERC20Address, ibcERC20
}

func (s *MultichainTestSuite) SetupSuite(ctx context.Context, proofType types.SupportedProofType) {
	chainconfig.DefaultChainSpecs = append(chainconfig.DefaultChainSpecs, chainconfig.IbcGoChainSpec("ibc-go-simd-2", "simd-2"))

	s.TestSuite.SetupSuite(ctx)

	eth, simdA, simdB := s.EthChain, s.CosmosChains[0], s.CosmosChains[1]

	s.T().Logf("Setting up test suite with proof type: %s", proofType.String())

	s.Require().True(s.Run("Set up environment", func() {
		err := os.Chdir("../..")
		s.Require().NoError(err)

		s.key, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		s.EthRelayerSubmitter, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		operatorKey, err := eth.CreateAndFundUser()
		s.Require().NoError(err)

		s.deployer, err = eth.CreateAndFundUser()
		s.Require().NoError(err)

		simdARelayerMnemonic, err := generateRelayerMnemonic()
		s.Require().NoError(err)
		s.SimdARelayerSubmitter, err = simdA.BuildWallet(ctx, "spectre-relayer-a", simdARelayerMnemonic)
		s.Require().NoError(err)
		err = simdA.SendFunds(ctx, interchaintest.FaucetAccountKeyName, ibc.WalletAmount{
			Address: s.SimdARelayerSubmitter.FormattedAddress(),
			Denom:   simdA.Config().Denom,
			Amount:  sdkmath.NewInt(testvalues.InitialBalance),
		})
		s.Require().NoError(err)

		simdBRelayerMnemonic, err := generateRelayerMnemonic()
		s.Require().NoError(err)
		s.SimdBRelayerSubmitter, err = simdB.BuildWallet(ctx, "spectre-relayer-b", simdBRelayerMnemonic)
		s.Require().NoError(err)
		err = simdB.SendFunds(ctx, interchaintest.FaucetAccountKeyName, ibc.WalletAmount{
			Address: s.SimdBRelayerSubmitter.FormattedAddress(),
			Denom:   simdB.Config().Denom,
			Amount:  sdkmath.NewInt(testvalues.InitialBalance),
		})
		s.Require().NoError(err)

		s.simdACosmosPrivateKey, err = cosmosPrivateKeyHexFromMnemonic(s.SimdARelayerSubmitter.Mnemonic())
		s.Require().NoError(err)
		s.simdBCosmosPrivateKey, err = cosmosPrivateKeyHexFromMnemonic(s.SimdBRelayerSubmitter.Mnemonic())
		s.Require().NoError(err)
		s.setCosmosRelayerEnv(simdA, s.simdACosmosPrivateKey)

		// Use mock verifier in E2E tests (gnark Groth16 prover is the real prover)
		os.Setenv(testvalues.EnvKeyVerifier, testvalues.EnvValueVerifier_Mock)
		os.Setenv(testvalues.EnvKeyEthRPC, eth.RPC)
		os.Setenv("ETH_PRIVATE_KEY", hex.EncodeToString(crypto.FromECDSA(s.deployer)))
		os.Setenv(testvalues.EnvKeyOperatorPrivateKey, hex.EncodeToString(crypto.FromECDSA(operatorKey)))
	}))

	s.Require().True(s.Run("Deploy ethereum contracts with SimdA client", func() {
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

	s.T().Cleanup(func() {
		os.Remove(multichainRelayerConfigAPath)
		os.Remove(multichainRelayerConfigBPath)
	})

	s.Require().True(s.Run("Create Ethereum/Cosmos clients for SimdA", func() {
		s.chainASpectreClientAddress = s.createEthCosmosClients(
			ctx,
			eth,
			simdA,
			s.SimdARelayerSubmitter,
			s.simdACosmosPrivateKey,
			multichainChainAUniversalClientID,
			multichainRelayerConfigAPath,
			proofType,
		)

		var err error
		s.chainASpectreClientContract, err = spectreclient.NewContract(s.chainASpectreClientAddress, eth.RPCClient)
		s.Require().NoError(err)
	}))

	s.Require().True(s.Run("Create Ethereum/Cosmos clients for SimdB", func() {
		s.chainBSpectreClientAddress = s.createEthCosmosClients(
			ctx,
			eth,
			simdB,
			s.SimdBRelayerSubmitter,
			s.simdBCosmosPrivateKey,
			multichainChainBUniversalClientID,
			multichainRelayerConfigBPath,
			proofType,
		)

		var err error
		s.chainBSpectreClientContract, err = spectreclient.NewContract(s.chainBSpectreClientAddress, eth.RPCClient)
		s.Require().NoError(err)
	}))

	s.Require().True(s.Run("Fund address with ERC20", func() {
		tx, err := s.erc20Contract.Transfer(s.GetTransactOpts(eth.Faucet, eth), crypto.PubkeyToAddress(s.key.PublicKey), testvalues.StartingERC20Balance)
		s.Require().NoError(err)

		_, err = eth.GetTxReciept(ctx, tx.Hash()) // wait for the tx to be mined
		s.Require().NoError(err)
	}))

	s.Require().True(s.Run("Create Light Client of Chain A on Chain B", func() {
		s.createTendermintClient(ctx, simdA, simdB, s.SimdBRelayerSubmitter, ibctesting.SecondClientID)
	}))

	s.Require().True(s.Run("Create Light Client of Chain B on Chain A", func() {
		s.createTendermintClient(ctx, simdB, simdA, s.SimdARelayerSubmitter, ibctesting.SecondClientID)
	}))

	s.Require().True(s.Run("Create Channel and register counterparty on Chain A", func() {
		merklePathPrefix := [][]byte{[]byte(ibcexported.StoreKey), []byte("")}

		// We can do this because we know what the counterparty channel ID will be
		_, err := s.BroadcastMessages(ctx, simdA, s.SimdARelayerSubmitter, 200_000, &clienttypesv2.MsgRegisterCounterparty{
			ClientId:                 ibctesting.SecondClientID,
			CounterpartyClientId:     ibctesting.SecondClientID,
			CounterpartyMerklePrefix: merklePathPrefix,
			Signer:                   s.SimdARelayerSubmitter.FormattedAddress(),
		})
		s.Require().NoError(err)
	}))

	s.Require().True(s.Run("Create Channel and register counterparty on Chain B", func() {
		merklePathPrefix := [][]byte{[]byte(ibcexported.StoreKey), []byte("")}

		_, err := s.BroadcastMessages(ctx, simdB, s.SimdBRelayerSubmitter, 200_000, &clienttypesv2.MsgRegisterCounterparty{
			ClientId:                 ibctesting.SecondClientID,
			CounterpartyClientId:     ibctesting.SecondClientID,
			CounterpartyMerklePrefix: merklePathPrefix,
			Signer:                   s.SimdBRelayerSubmitter.FormattedAddress(),
		})
		s.Require().NoError(err)
	}))

	var relayerProcesses []*os.Process
	s.T().Cleanup(func() {
		for _, relayerProcess := range relayerProcesses {
			if relayerProcess == nil {
				continue
			}
			if err := relayerProcess.Kill(); err != nil {
				s.T().Logf("Failed to kill the relayer process: %v", err)
			}
		}
	})

	s.Require().True(s.Run("Start SimdA relay loop", func() {
		s.setCosmosRelayerEnv(simdA, s.simdACosmosPrivateKey)

		relayerProcess, err := relayer.StartRelayer(multichainRelayerConfigAPath)
		s.Require().NoError(err)
		relayerProcesses = append(relayerProcesses, relayerProcess)
	}))

	s.Require().True(s.Run("Start SimdB relay loop", func() {
		s.setCosmosRelayerEnv(simdB, s.simdBCosmosPrivateKey)

		relayerProcess, err := relayer.StartRelayer(multichainRelayerConfigBPath)
		s.Require().NoError(err)
		relayerProcesses = append(relayerProcesses, relayerProcess)
	}))
}

func (s *MultichainTestSuite) Test_Deploy() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()

	s.SetupSuite(ctx, proofType)

	simdA, simdB := s.CosmosChains[0], s.CosmosChains[1]

	s.Require().True(s.Run("Verify SimdA Groth16 Client", func() {
		clientState, err := getGroth16ClientState(s.chainASpectreClientContract)
		s.Require().NoError(err)

		stakingParams, err := simdA.StakingQueryParams(ctx)
		s.Require().NoError(err)

		s.Require().Equal(simdA.Config().ChainID, clientState.ChainId)
		s.Require().Equal(uint8(testvalues.DefaultTrustLevel.Numerator), clientState.TrustLevel.Numerator)
		s.Require().Equal(uint8(testvalues.DefaultTrustLevel.Denominator), clientState.TrustLevel.Denominator)
		s.Require().Equal(uint32(testvalues.DefaultTrustPeriod), clientState.TrustingPeriod)
		s.Require().Equal(uint32(stakingParams.UnbondingTime.Seconds()), clientState.UnbondingPeriod)
		s.Require().False(clientState.IsFrozen)
		s.Require().Equal(uint64(1), clientState.LatestHeight.RevisionNumber)
		s.Require().Greater(clientState.LatestHeight.RevisionHeight, uint64(0))
	}))

	s.Require().True(s.Run("Verify SimdB Groth16 Client", func() {
		clientState, err := getGroth16ClientState(s.chainBSpectreClientContract)
		s.Require().NoError(err)

		stakingParams, err := simdB.StakingQueryParams(ctx)
		s.Require().NoError(err)

		s.Require().Equal(simdB.Config().ChainID, clientState.ChainId)
		s.Require().Equal(uint8(testvalues.DefaultTrustLevel.Numerator), clientState.TrustLevel.Numerator)
		s.Require().Equal(uint8(testvalues.DefaultTrustLevel.Denominator), clientState.TrustLevel.Denominator)
		s.Require().Equal(uint32(testvalues.DefaultTrustPeriod), clientState.TrustingPeriod)
		s.Require().Equal(uint32(stakingParams.UnbondingTime.Seconds()), clientState.UnbondingPeriod)
		s.Require().False(clientState.IsFrozen)
		s.Require().Equal(uint64(2), clientState.LatestHeight.RevisionNumber)
		s.Require().Greater(clientState.LatestHeight.RevisionHeight, uint64(0))
	}))

	s.Require().True(s.Run("Verify ICS02 Client", func() {
		clientAddress, err := s.ics26Contract.GetClient(nil, multichainChainAUniversalClientID)
		s.Require().NoError(err)
		s.Require().Equal(s.chainASpectreClientAddress, clientAddress)

		counterpartyInfo, err := s.ics26Contract.GetCounterparty(nil, multichainChainAUniversalClientID)
		s.Require().NoError(err)
		s.Require().Equal(testvalues.FirstWasmClientID, counterpartyInfo.ClientId)

		clientAddress, err = s.ics26Contract.GetClient(nil, multichainChainBUniversalClientID)
		s.Require().NoError(err)
		s.Require().Equal(s.chainBSpectreClientAddress, clientAddress)

		counterpartyInfo, err = s.ics26Contract.GetCounterparty(nil, multichainChainBUniversalClientID)
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

	s.Require().True(s.Run("Verify ethereum light client for SimdA", func() {
		_, err := e2esuite.GRPCQuery[clienttypes.QueryClientStateResponse](ctx, simdA, &clienttypes.QueryClientStateRequest{
			ClientId: testvalues.FirstWasmClientID,
		})
		s.Require().NoError(err)

		counterpartyInfoResp, err := e2esuite.GRPCQuery[clienttypesv2.QueryCounterpartyInfoResponse](ctx, simdA, &clienttypesv2.QueryCounterpartyInfoRequest{
			ClientId: testvalues.FirstWasmClientID,
		})
		s.Require().NoError(err)
		s.Require().Equal(multichainChainAUniversalClientID, counterpartyInfoResp.CounterpartyInfo.ClientId)
	}))

	s.Require().True(s.Run("Verify ethereum light client for SimdB", func() {
		_, err := e2esuite.GRPCQuery[clienttypes.QueryClientStateResponse](ctx, simdB, &clienttypes.QueryClientStateRequest{
			ClientId: testvalues.FirstWasmClientID,
		})
		s.Require().NoError(err)

		counterpartyInfoResp, err := e2esuite.GRPCQuery[clienttypesv2.QueryCounterpartyInfoResponse](ctx, simdB, &clienttypesv2.QueryCounterpartyInfoRequest{
			ClientId: testvalues.FirstWasmClientID,
		})
		s.Require().NoError(err)
		s.Require().Equal(multichainChainBUniversalClientID, counterpartyInfoResp.CounterpartyInfo.ClientId)
	}))

	s.Require().True(s.Run("Verify Light Client of Chain A on Chain B", func() {
		clientStateResp, err := e2esuite.GRPCQuery[clienttypes.QueryClientStateResponse](ctx, simdB, &clienttypes.QueryClientStateRequest{
			ClientId: ibctesting.SecondClientID,
		})
		s.Require().NoError(err)
		s.Require().NotZero(clientStateResp.ClientState.Value)

		var clientState ibctm.ClientState
		err = proto.Unmarshal(clientStateResp.ClientState.Value, &clientState)
		s.Require().NoError(err)
		s.Require().Equal(simdA.Config().ChainID, clientState.ChainId)
	}))

	s.Require().True(s.Run("Verify Light Client of Chain B on Chain A", func() {
		clientStateResp, err := e2esuite.GRPCQuery[clienttypes.QueryClientStateResponse](ctx, simdA, &clienttypes.QueryClientStateRequest{
			ClientId: ibctesting.SecondClientID,
		})
		s.Require().NoError(err)
		s.Require().NotZero(clientStateResp.ClientState.Value)

		var clientState ibctm.ClientState
		err = proto.Unmarshal(clientStateResp.ClientState.Value, &clientState)
		s.Require().NoError(err)
		s.Require().Equal(simdB.Config().ChainID, clientState.ChainId)
	}))

}

func (s *MultichainTestSuite) Test_TransferCosmosToEthToCosmosAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()

	s.SetupSuite(ctx, proofType)

	eth, simdA, simdB := s.EthChain, s.CosmosChains[0], s.CosmosChains[1]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	transferAmount := big.NewInt(testvalues.TransferAmount)
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	simdAUser, simdBUser := s.CosmosUsers[0], s.CosmosUsers[1]

	var denomOnEthereum transfertypes.Denom
	s.Require().True(s.Run("Send transfer on SimdA chain", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferCoin := sdk.NewCoin(simdA.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    transferCoin.Denom,
			Amount:   transferCoin.Amount.String(),
			Sender:   simdAUser.FormattedAddress(),
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
			Signer: simdAUser.FormattedAddress(),
		}

		resp, err := s.BroadcastMessages(ctx, simdA, simdAUser, 200_000, &msgSendPacket)
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		packet := s.packetFromSendPacketResponse(resp, testvalues.FirstWasmClientID, multichainChainAUniversalClientID, timeout, payload)
		denomOnEthereum = transfertypes.NewDenom(transferCoin.Denom, transfertypes.NewHop(packet.Payloads[0].DestinationPort, packet.DestinationClient))

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			// Check the balance of UserB
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdA, &banktypes.QueryBalanceRequest{
				Address: simdAUser.FormattedAddress(),
				Denom:   transferCoin.Denom,
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.InitialBalance-testvalues.TransferAmount, resp.Balance.Amount.Int64())
		}))
	}))

	var (
		ibcERC20        *ibcerc20.Contract
		ibcERC20Address ethcommon.Address
	)
	s.Require().True(s.Run("Receive packet on Ethereum (auto-relayed)", func() {
		ibcERC20Address, ibcERC20 = s.waitForIbcERC20Balance(
			eth,
			denomOnEthereum.Path(),
			ethereumUserAddress,
			transferAmount,
			autoRelayTimeout(1),
		)

		actualFullDenom, err := ibcERC20.FullDenomPath(nil)
		s.Require().NoError(err)
		s.Require().Equal(denomOnEthereum.Path(), actualFullDenom)

		s.True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(transferAmount, userBalance)

			// ICS20 contract balance on Ethereum
			ics20TransferBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
			s.Require().NoError(err)
			s.Require().Zero(ics20TransferBalance.Int64())
		}))
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

	s.Require().True(s.Run("Transfer tokens from Ethereum to SimdB", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            ibcERC20Address,
			Amount:           transferAmount,
			Receiver:         simdBUser.FormattedAddress(),
			SourceClient:     multichainChainBUniversalClientID,
			DestPort:         transfertypes.PortID,
			TimeoutTimestamp: timeout,
			Memo:             "",
		}

		tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		s.True(s.Run("Verify balances on Ethereum", func() {
			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Zero(userBalance.Int64())

			// the whole balance should have been burned
			ics20TransferBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
			s.Require().NoError(err)
			s.Require().Zero(ics20TransferBalance.Int64())
		}))
	}))

	var finalDenom transfertypes.Denom
	s.Require().True(s.Run("Receive packet on SimdB (auto-relayed)", func() {
		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			finalDenom = transfertypes.NewDenom(
				simdA.Config().Denom,
				transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID),
				transfertypes.NewHop(transfertypes.PortID, multichainChainAUniversalClientID),
			)

			// Check the balance of UserB
			s.waitForCosmosBalance(ctx, simdB, simdBUser.FormattedAddress(), finalDenom.IBCDenom(), transferAmount, autoRelayTimeout(1))
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdB, &banktypes.QueryBalanceRequest{
				Address: simdBUser.FormattedAddress(),
				Denom:   finalDenom.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.TransferAmount, resp.Balance.Amount.Int64())
			s.Require().Equal(finalDenom.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	// Transfer back (unwind)
	s.Require().True(s.Run("Transfer tokens from SimdB to Ethereum", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    finalDenom.Path(), // XXX: IBCDenom()?
			Amount:   transferAmount.String(),
			Sender:   simdBUser.FormattedAddress(),
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

		resp, err := s.BroadcastMessages(ctx, simdB, simdBUser, 200_000, &channeltypesv2.MsgSendPacket{
			SourceClient:     testvalues.FirstWasmClientID,
			TimeoutTimestamp: timeout,
			Payloads: []channeltypesv2.Payload{
				payload,
			},
			Signer: simdBUser.FormattedAddress(),
		})
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		s.Require().True(s.Run("Receive packet on Ethereum (auto-relayed)", func() {
			_, ibcERC20 = s.waitForIbcERC20Balance(eth, denomOnEthereum.Path(), ethereumUserAddress, transferAmount, autoRelayTimeout(1))
			s.True(s.Run("Verify balances on Ethereum", func() {
				userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
				s.Require().NoError(err)
				s.Require().Equal(transferAmount, userBalance)

				// ICS20 contract balance on Ethereum
				ics20TransferBalance, err := ibcERC20.BalanceOf(nil, ics20Address)
				s.Require().NoError(err)
				s.Require().Zero(ics20TransferBalance.Int64())
			}))
		}))
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

	s.Require().True(s.Run("Transfer tokens from Ethereum to SimdA", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            ibcERC20Address,
			Amount:           transferAmount,
			Receiver:         simdAUser.FormattedAddress(),
			SourceClient:     multichainChainAUniversalClientID,
			DestPort:         transfertypes.PortID,
			TimeoutTimestamp: timeout,
			Memo:             "",
		}

		tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)

		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)
	}))

	s.Require().True(s.Run("Receive packet on SimdA (auto-relayed)", func() {
		s.waitForCosmosBalance(ctx, simdA, simdAUser.FormattedAddress(), simdA.Config().Denom, big.NewInt(testvalues.InitialBalance), autoRelayTimeout(1))
		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdA, &banktypes.QueryBalanceRequest{
				Address: simdAUser.FormattedAddress(),
				Denom:   simdA.Config().Denom,
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(testvalues.InitialBalance, resp.Balance.Amount.Int64())
		}))
	}))
}

func (s *MultichainTestSuite) Test_TransferEthToCosmosToCosmosAndBack() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()

	s.SetupSuite(ctx, proofType)

	eth, simdA, simdB := s.EthChain, s.CosmosChains[0], s.CosmosChains[1]

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)
	erc20Address := ethcommon.HexToAddress(s.contractAddresses.Erc20)

	transferAmount := big.NewInt(testvalues.TransferAmount)
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)
	simdAUser, simdBUser := s.CosmosUsers[0], s.CosmosUsers[1]
	denomOnSimdA := transfertypes.NewDenom(s.contractAddresses.Erc20, transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID))

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

	var escrowAddress ethcommon.Address
	s.Require().True(s.Run("Send from Ethereum to SimdA", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
			Denom:            erc20Address,
			Amount:           transferAmount,
			Receiver:         simdAUser.FormattedAddress(),
			SourceClient:     multichainChainAUniversalClientID,
			DestPort:         transfertypes.PortID,
			TimeoutTimestamp: timeout,
			Memo:             "",
		}

		tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
		s.Require().NoError(err)
		receipt, err := eth.GetTxReciept(ctx, tx.Hash())
		s.Require().NoError(err)
		s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)

		s.True(s.Run("Verify balances on Ethereum", func() {
			// User balance on Ethereum
			userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(new(big.Int).Sub(testvalues.StartingERC20Balance, transferAmount), userBalance)

			// Get the escrow contract address
			escrowAddress, err = s.ics20Contract.GetEscrow(nil, multichainChainAUniversalClientID)
			s.Require().NoError(err)

			// ICS20 contract balance on Ethereum
			escrowBalance, err := s.erc20Contract.BalanceOf(nil, escrowAddress)
			s.Require().NoError(err)
			s.Require().Equal(transferAmount, escrowBalance)
		}))
	}))

	s.Require().True(s.Run("Receive packets on SimdA (auto-relayed)", func() {
		s.waitForCosmosBalance(ctx, simdA, simdAUser.FormattedAddress(), denomOnSimdA.IBCDenom(), transferAmount, autoRelayTimeout(1))
		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			// User balance on Cosmos chain
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdA, &banktypes.QueryBalanceRequest{
				Address: simdAUser.FormattedAddress(),
				Denom:   denomOnSimdA.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(transferAmount, resp.Balance.Amount.BigInt())
			s.Require().Equal(denomOnSimdA.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	var simdAPacket channeltypesv2.Packet
	s.Require().True(s.Run("Send from SimdA to SimdB", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    denomOnSimdA.Path(),
			Amount:   transferAmount.String(),
			Sender:   simdAUser.FormattedAddress(),
			Receiver: simdBUser.FormattedAddress(),
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

		resp, err := s.BroadcastMessages(ctx, simdA, simdAUser, 2_000_000, &channeltypesv2.MsgSendPacket{
			SourceClient:     ibctesting.SecondClientID,
			TimeoutTimestamp: timeout,
			Payloads: []channeltypesv2.Payload{
				payload,
			},
			Signer: simdAUser.FormattedAddress(),
		})
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		simdAPacket = s.packetFromSendPacketResponse(resp, ibctesting.SecondClientID, ibctesting.SecondClientID, timeout, payload)
	}))

	var finalDenom transfertypes.Denom
	s.Require().True(s.Run("Receive packet on SimdB", func() {
		s.relayCosmosPacket(ctx, simdA, simdB, s.SimdBRelayerSubmitter, ibctesting.SecondClientID, simdAPacket)

		s.Require().True(s.Run("Verify balances on Cosmos chain", func() {
			finalDenom = transfertypes.NewDenom(
				s.contractAddresses.Erc20,
				transfertypes.NewHop(transfertypes.PortID, ibctesting.SecondClientID),
				transfertypes.NewHop(transfertypes.PortID, testvalues.FirstWasmClientID),
			)

			// User balance on Cosmos chain
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdB, &banktypes.QueryBalanceRequest{
				Address: simdBUser.FormattedAddress(),
				Denom:   finalDenom.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(transferAmount, resp.Balance.Amount.BigInt())
			s.Require().Equal(finalDenom.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	// Transfer back (unwind)
	var simdBPacket channeltypesv2.Packet
	s.Require().True(s.Run("Transfer tokens from SimdB to SimdA", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    finalDenom.Path(), // XXX: IBCDenom()?
			Amount:   transferAmount.String(),
			Sender:   simdBUser.FormattedAddress(),
			Receiver: simdAUser.FormattedAddress(),
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

		resp, err := s.BroadcastMessages(ctx, simdB, simdBUser, 2_000_000, &channeltypesv2.MsgSendPacket{
			SourceClient:     ibctesting.SecondClientID,
			TimeoutTimestamp: timeout,
			Payloads:         []channeltypesv2.Payload{payload},
			Signer:           simdBUser.FormattedAddress(),
		})
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		simdBPacket = s.packetFromSendPacketResponse(resp, ibctesting.SecondClientID, ibctesting.SecondClientID, timeout, payload)
	}))

	s.Require().True(s.Run("Receive packet on SimdA", func() {
		s.relayCosmosPacket(ctx, simdB, simdA, s.SimdARelayerSubmitter, ibctesting.SecondClientID, simdBPacket)

		s.Require().True(s.Run("Verify balances on SimdA", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdA, &banktypes.QueryBalanceRequest{
				Address: simdAUser.FormattedAddress(),
				Denom:   denomOnSimdA.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(transferAmount, resp.Balance.Amount.BigInt())
			s.Require().Equal(denomOnSimdA.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	s.Require().True(s.Run("Transfer tokens from SimdA to Ethereum", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    denomOnSimdA.Path(),
			Amount:   transferAmount.String(),
			Sender:   simdAUser.FormattedAddress(),
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

		resp, err := s.BroadcastMessages(ctx, simdA, simdAUser, 2_000_000, &channeltypesv2.MsgSendPacket{
			SourceClient:     testvalues.FirstWasmClientID,
			TimeoutTimestamp: timeout,
			Payloads:         []channeltypesv2.Payload{payload},
			Signer:           simdAUser.FormattedAddress(),
		})
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		s.Require().True(s.Run("Receive packet on Ethereum (auto-relayed)", func() {
			s.Require().Eventually(func() bool {
				userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
				if err != nil || userBalance == nil {
					return false
				}
				return userBalance.Cmp(testvalues.StartingERC20Balance) == 0
			}, autoRelayTimeout(1), 5*time.Second, "timed out waiting for ERC20 balance on Ethereum")
			s.True(s.Run("Verify balances on Ethereum", func() {
				userBalance, err := s.erc20Contract.BalanceOf(nil, ethereumUserAddress)
				s.Require().NoError(err)
				s.Require().Equal(testvalues.StartingERC20Balance, userBalance)
			}))
		}))
	}))
}

func (s *MultichainTestSuite) Test_TransferCosmosToCosmosToEth() {
	ctx := context.Background()
	proofType := types.GetEnvProofType()

	s.SetupSuite(ctx, proofType)

	eth, simdA, simdB := s.EthChain, s.CosmosChains[0], s.CosmosChains[1]

	transferAmount := big.NewInt(testvalues.TransferAmount)
	transferCoin := sdk.NewCoin(simdA.Config().Denom, sdkmath.NewIntFromBigInt(transferAmount))
	simdAUser := s.CosmosUsers[0]
	simdBUser := s.CosmosUsers[1]
	ethereumUserAddress := crypto.PubkeyToAddress(s.key.PublicKey)

	ics20Address := ethcommon.HexToAddress(s.contractAddresses.Ics20Transfer)

	var simdAPacket channeltypesv2.Packet
	s.Require().True(s.Run("Send from SimdA to SimdB", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())

		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    transferCoin.Denom,
			Amount:   transferCoin.Amount.String(),
			Sender:   simdAUser.FormattedAddress(),
			Receiver: simdBUser.FormattedAddress(),
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

		resp, err := s.BroadcastMessages(ctx, simdA, simdAUser, 2_000_000, &channeltypesv2.MsgSendPacket{
			SourceClient:     ibctesting.SecondClientID,
			TimeoutTimestamp: timeout,
			Payloads:         []channeltypesv2.Payload{payload},
			Signer:           simdAUser.FormattedAddress(),
		})
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)

		simdAPacket = s.packetFromSendPacketResponse(resp, ibctesting.SecondClientID, ibctesting.SecondClientID, timeout, payload)
	}))

	denomOnSimdB := transfertypes.NewDenom(
		transferCoin.Denom,
		transfertypes.NewHop(transfertypes.PortID, ibctesting.SecondClientID),
	)
	s.Require().True(s.Run("Receive packet on SimdB", func() {
		s.relayCosmosPacket(ctx, simdA, simdB, s.SimdBRelayerSubmitter, ibctesting.SecondClientID, simdAPacket)

		s.Require().True(s.Run("Verify balances on SimdB", func() {
			resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdB, &banktypes.QueryBalanceRequest{
				Address: simdBUser.FormattedAddress(),
				Denom:   denomOnSimdB.IBCDenom(),
			})
			s.Require().NoError(err)
			s.Require().NotNil(resp.Balance)
			s.Require().Equal(transferAmount, resp.Balance.Amount.BigInt())
			s.Require().Equal(denomOnSimdB.IBCDenom(), resp.Balance.Denom)
		}))
	}))

	s.Require().True(s.Run("Transfer tokens from SimdB to Eth", func() {
		timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
		transferCoin := sdk.NewCoin(denomOnSimdB.IBCDenom(), sdkmath.NewIntFromBigInt(transferAmount))
		transferPayload := transfertypes.FungibleTokenPacketData{
			Denom:    denomOnSimdB.Path(),
			Amount:   transferCoin.Amount.String(),
			Sender:   simdBUser.FormattedAddress(),
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
		msgSendPacket := &channeltypesv2.MsgSendPacket{
			SourceClient:     testvalues.FirstWasmClientID,
			TimeoutTimestamp: timeout,
			Payloads:         []channeltypesv2.Payload{payload},
			Signer:           simdBUser.FormattedAddress(),
		}

		resp, err := s.BroadcastMessages(ctx, simdB, simdBUser, 2_000_000, msgSendPacket)
		s.Require().NoError(err)
		s.Require().NotEmpty(resp.TxHash)
	}))

	var denomOnEthereum transfertypes.Denom
	var ibcERC20 *ibcerc20.Contract
	s.Require().True(s.Run("Receive packet on Ethereum (auto-relayed)", func() {
		denomOnEthereum = transfertypes.NewDenom(
			simdA.Config().Denom,
			transfertypes.NewHop(transfertypes.PortID, multichainChainBUniversalClientID),
			transfertypes.NewHop(transfertypes.PortID, ibctesting.SecondClientID),
		)
		_, ibcERC20 = s.waitForIbcERC20Balance(eth, denomOnEthereum.Path(), ethereumUserAddress, transferAmount, autoRelayTimeout(1))

		s.True(s.Run("Verify balances on Ethereum", func() {
			ibcERC20Address, err := s.ics20Contract.IbcERC20Contract(nil, denomOnEthereum.Path())
			s.Require().NoError(err)

			ibcERC20, err = ibcerc20.NewContract(ibcERC20Address, eth.RPCClient)
			s.Require().NoError(err)

			userBalance, err := ibcERC20.BalanceOf(nil, ethereumUserAddress)
			s.Require().NoError(err)
			s.Require().Equal(transferAmount, userBalance)
		}))
	}))

	s.Require().True(s.Run("Transfer tokens from Ethereum to SimdB", func() {
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

		s.Require().True(s.Run("Send packet on Ethereum", func() {
			ibcERC20Address, err := s.ics20Contract.IbcERC20Contract(nil, denomOnEthereum.Path())
			s.Require().NoError(err)
			timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
			msgSendPacket := ics20transfer.IICS20TransferMsgsSendTransferMsg{
				Denom:            ibcERC20Address,
				Amount:           transferAmount,
				Receiver:         simdBUser.FormattedAddress(),
				TimeoutTimestamp: timeout,
				SourceClient:     multichainChainBUniversalClientID,
				DestPort:         transfertypes.PortID,
				Memo:             "testmemo",
			}

			tx, err := s.ics20Contract.SendTransfer(s.GetTransactOpts(s.key, eth), msgSendPacket)
			s.Require().NoError(err)

			receipt, err := eth.GetTxReciept(ctx, tx.Hash())
			s.Require().NoError(err)
			s.Require().Equal(ethtypes.ReceiptStatusSuccessful, receipt.Status)
		}))

		s.Require().True(s.Run("Receive packet on SimdB (auto-relayed)", func() {
			s.waitForCosmosBalance(ctx, simdB, simdBUser.FormattedAddress(), denomOnSimdB.IBCDenom(), transferAmount, autoRelayTimeout(1))
			s.Require().True(s.Run("Verify balances on SimdB", func() {
				resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdB, &banktypes.QueryBalanceRequest{
					Address: simdBUser.FormattedAddress(),
					Denom:   denomOnSimdB.IBCDenom(),
				})
				s.Require().NoError(err)
				s.Require().NotNil(resp.Balance)
				s.Require().Equal(transferAmount, resp.Balance.Amount.BigInt())
				s.Require().Equal(denomOnSimdB.IBCDenom(), resp.Balance.Denom)
			}))
		}))
	}))

	var simdBPacket channeltypesv2.Packet
	s.Require().True(s.Run("Transfer tokens from SimdB to SimdA", func() {
		s.Require().True(s.Run("Send packet on SimdB", func() {
			timeout := uint64(time.Now().Add(30 * time.Minute).Unix())
			transferPayload := transfertypes.FungibleTokenPacketData{
				Denom:    denomOnSimdB.Path(),
				Amount:   transferAmount.String(),
				Sender:   simdBUser.FormattedAddress(),
				Receiver: simdAUser.FormattedAddress(),
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

			msgSendPacket := &channeltypesv2.MsgSendPacket{
				SourceClient:     ibctesting.SecondClientID,
				TimeoutTimestamp: timeout,
				Payloads:         []channeltypesv2.Payload{payload},
				Signer:           simdBUser.FormattedAddress(),
			}

			resp, err := s.BroadcastMessages(ctx, simdB, simdBUser, 2_000_000, msgSendPacket)
			s.Require().NoError(err)
			s.Require().NotEmpty(resp.TxHash)

			simdBPacket = s.packetFromSendPacketResponse(resp, ibctesting.SecondClientID, ibctesting.SecondClientID, timeout, payload)
		}))

		s.Require().True(s.Run("Receive packet on SimdA", func() {
			s.relayCosmosPacket(ctx, simdB, simdA, s.SimdARelayerSubmitter, ibctesting.SecondClientID, simdBPacket)

			s.Require().True(s.Run("Verify balances on SimdA", func() {
				resp, err := e2esuite.GRPCQuery[banktypes.QueryBalanceResponse](ctx, simdA, &banktypes.QueryBalanceRequest{
					Address: simdAUser.FormattedAddress(),
					Denom:   simdA.Config().Denom,
				})
				s.Require().NoError(err)
				s.Require().NotNil(resp.Balance)
				s.Require().Equal(testvalues.InitialBalance, resp.Balance.Amount.Int64())
			}))
		}))
	}))
}
