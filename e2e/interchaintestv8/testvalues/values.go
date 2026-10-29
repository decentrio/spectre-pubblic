package testvalues

import (
	"math/big"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/crypto"

	"cosmossdk.io/math"

	ibctm "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"

	"github.com/cosmos/interchaintest/v10/chain/ethereum"

	"github.com/decentrio/spectre/packages/go-abigen/ics26router"
)

const (
	// InitialBalance is the amount of tokens to give to each user at the start of the test.
	InitialBalance int64 = 1_000_000_000_000

	// TransferAmount is the default transfer amount
	TransferAmount int64 = 1_000_000_000

	// EnvKeyTendermintRPC Tendermint RPC URL.
	EnvKeyTendermintRPC = "TENDERMINT_RPC_URL"
	// EnvKeyEthRPC Ethereum RPC URL.
	EnvKeyEthRPC = "RPC_URL"
	// EnvKeyOperatorPrivateKey Private key used to submit transactions by the operator.
	EnvKeyOperatorPrivateKey = "PRIVATE_KEY"
	// EnvKeyVerifier Optional address of the verifier contract to use.
	// if not set, the contract will be deployed
	// Can be set to "mock" to use the mock verifier
	EnvKeyVerifier = "VERIFIER"
	// EnvKeyGenerateSolidityFixtures Generate fixtures for the solidity tests if set to true.
	EnvKeyGenerateSolidityFixtures = "GENERATE_SOLIDITY_FIXTURES"
	// EnvKeyGenerateSolidityFixtures Generate fixtures for the solidity tests if set to true.
	EnvKeyGenerateWasmFixtures = "GENERATE_WASM_FIXTURES"
	// EnvValueVerifier_Mock is the verifier type for the mock verifier.
	EnvValueVerifier_Mock = "mock"
	// EnvValueGenerateFixtures_True is the value to set to generate fixtures for the solidity tests.
	EnvValueGenerateFixtures_True = "true"
	// EnvValueEthereumPosPreset_Minimal is the default preset for Ethereum PoS testnet.
	EnvValueEthereumPosPreset_Minimal = "minimal"
	// EnvValueProofType_Groth16 is the proof type for Groth16.
	EnvValueProofType_Groth16 = "groth16"
	// EnvValueProofType_Plonk is the proof type for Plonk.
	EnvValueProofType_Plonk = "plonk"
	// EnvValueWasmLightClientTag_Local is the value to set to use the local Wasm light client binary.
	EnvValueWasmLightClientTag_Local = "local"

	// EthTestnetTypePoW is the Ethereum testnet type for using a proof of work chain (anvil).
	EthTestnetTypePoW = "pow"
	// EthTestnetTypePoS is the Ethereum testnet type for using a proof of stake chain
	EthTestnetTypePoS = "pos"
	// EthTestnetTypeNone is the Ethereum testnet type for using no chain.
	EthTestnetTypeNone = "none"
	// EnvKeyEthTestnetType The Ethereum testnet type (pow|pos).
	EnvKeyEthTestnetType = "ETH_TESTNET_TYPE"
	// EnvKeyE2EFaucetAddress The address of the faucet
	EnvKeyE2EFaucetAddress = "E2E_FAUCET_ADDRESS"
	// EnvKeyEthereumPosNetworkPreset The environment variable name to configure the Kurtosis network preset
	EnvKeyEthereumPosNetworkPreset = "ETHEREUM_POS_NETWORK_PRESET"
	// EnvKeyE2EProofType is the environment variable name to configure the proof type. (groth16|plonk)
	// A randomly selected proof type is used if not set.
	EnvKeyE2EProofType = "E2E_PROOF_TYPE"
	// EnvKeyE2EWasmLightClientTag is the environment variable name to configure the eth light client version.
	// Either an empty string, or 'local', means it will use the local binary in the repo, unless running in mock mode
	// otherwise, it will download the version from the github release with the given tag
	EnvKeyE2EWasmLightClientTag = "E2E_WASM_LIGHT_CLIENT_TAG"

	// Groth16GenesisFilePath is the path to the genesis file for the Groth16 chain.
	// This file is generated and then deleted by the test.
	Groth16GenesisFilePath = "scripts/genesis.json"
	// SolidityFixturesDir is the directory where the Solidity fixtures are stored.
	SolidityFixturesDir = "test/fixtures/solidity/"
	// SpectreFixturesDir is the directory where the Spectre fixtures are stored.
	SpectreFixturesDir = "test/light-clients/spectre/fixtures"
	// WasmFixturesDir is the directory where the Rust fixtures are stored.
	WasmFixturesDir = "packages/ethereum/light-client/src/test_utils/fixtures"
	// RelayerConfigFilePath is the path to generate the relayer config file.
	RelayerConfigFilePath = "relayer/config.json"
	// E2EDeployScriptPath is the path to the E2E deploy script.
	E2EDeployScriptPath = "scripts/E2ETestDeploy.s.sol:E2ETestDeploy"
	// IbcCommitmentSlotHex is the storage slot in the IBC solidity contract for the IBC commitments.
	IbcCommitmentSlotHex = ics26router.IbcStoreStorageSlot

	// FirstWasmClientID is the first wasm client ID. Used for testing.
	FirstWasmClientID = "08-wasm-0"
	// FirstUniversalClientID is the first universal client ID. Used for testing.
	FirstUniversalClientID = "client-0"
	// SecondUniversalClientID is the second universal client ID. Used for testing.
	SecondUniversalClientID = "client-1"
	// CustomClientID is the custom client ID used for testing.
	// BUG: https://github.com/cosmos/ibc-go/issues/8145
	// We must use a client ID of the form `type-n` due to the issue above.
	CustomClientID = "cosmoshub-1"

	// Groth16 verifier address parameter key for the relayer's groth16 light client creation.
	ParameterKey_Groth16Verifier = "groth16_verifier"
	// Zk algorithm parameter key for the relayer's groth16 light client creation.
	ParameterKey_ZkAlgorithm = "zk_algorithm"
	// The role manager address parameter key for the relayer's groth16 light client creation.
	ParameterKey_RoleManager = "role_manager"
	// Checksum hex parameter key for the relayer's ethereum light client creation.
	ParameterKey_ChecksumHex = "checksum_hex"

	// EnvKeyRustLog is the environment variable name for the RUST_LOG level.
	EnvKeyRustLog = "RUST_LOG"
	// EnvKeyNetworkPrivateCluster is the environment variable name for using a private prover cluster.
	EnvKeyNetworkPrivateCluster = "NETWORK_PRIVATE_CLUSTER"
	// EnvValueGroth16Prover_PrivateCluster is the value to enable private cluster for Groth16 prover.
	EnvValueGroth16Prover_PrivateCluster = "true"
	// EnvValueGroth16Prover_Mock is the value to use the mock Groth16 prover.
	EnvValueGroth16Prover_Mock = "mock"
	// EnvValueGroth16Prover_Network is the value to use the network Groth16 prover.
	EnvValueGroth16Prover_Network = "network"
)

var (
	// MaxDepositPeriod Maximum period to deposit on a proposal.
	// This value overrides the default value in the gov module using the `modifyGovV1AppState` function.
	MaxDepositPeriod = time.Second * 10
	// VotingPeriod Duration of the voting period.
	// This value overrides the default value in the gov module using the `modifyGovV1AppState` function.
	VotingPeriod = time.Second * 30

	// StartingEthBalance is the amount of ETH to give to each user at the start of the test.
	StartingEthBalance = math.NewInt(2 * ethereum.ETHER.Int64())

	// DefaultTrustLevel is the trust level used by the SpectreClient contract.
	DefaultTrustLevel = ibctm.Fraction{Numerator: 1, Denominator: 3}.ToTendermint()

	// DefaultTrustPeriod is the trust period used by the SpectreClient contract.
	// 129600 seconds = 14 days
	DefaultTrustPeriod = 1209600

	// MaxUint256 is the maximum value for a uint256.
	MaxUint256 = uint256.MustFromHex("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")

	// StartingERC20Balance is the per-user starting balance funded by the test
	// ERC20 faucet. Must be ≤ the mint amount in scripts/E2ETestDeploy.s.sol
	// (which mints 1_000_000 * 10^18 = 10^24). 100_000 * 10^18 leaves plenty of
	// headroom for 10+ funded users per run while staying well below the mint cap.
	StartingERC20Balance = new(big.Int).Mul(big.NewInt(100_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))

	// DefaultAdminRole is the default admin role for AccessControl contract.
	DefaultAdminRole = [32]byte{0x00}

	// PortCustomizerRole is the role required to customize the port.
	PortCustomizerRole = func() (role [32]byte) {
		copy(role[:], crypto.Keccak256([]byte("PORT_CUSTOMIZER_ROLE")))
		return role
	}()
)
