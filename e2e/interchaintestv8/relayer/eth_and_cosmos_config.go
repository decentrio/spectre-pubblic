package relayer

import "strings"

// ethWsURLFromRPC derives a WebSocket URL from an HTTP RPC URL by swapping the scheme.
// Geth-style nodes (Kurtosis ethereum-package, anvil, etc.) typically expose RPC and WS on
// the same host:port, so http://host:port → ws://host:port is the right default.
// Returns the input unchanged if it already has a ws:// or wss:// prefix, or empty for empty input.
func ethWsURLFromRPC(rpc string) string {
	switch {
	case rpc == "":
		return ""
	case strings.HasPrefix(rpc, "ws://") || strings.HasPrefix(rpc, "wss://"):
		return rpc
	case strings.HasPrefix(rpc, "https://"):
		return "wss://" + strings.TrimPrefix(rpc, "https://")
	case strings.HasPrefix(rpc, "http://"):
		return "ws://" + strings.TrimPrefix(rpc, "http://")
	default:
		return rpc
	}
}

// EthCosmosConfigInfo is a struct that holds the configuration information for the Eth to Cosmos config template
type EthCosmosConfigInfo struct {
	// Ethereum chain identifier
	EthChainID string
	// Cosmos chain identifier
	CosmosChainID string
	// Tendermint RPC URL
	TmRPC string
	// ICS26 Router address
	ICS26Address string
	// Ethereum RPC URL
	EthRPC string
	// Ethereum WebSocket URL (used by the eth subscriber for live event tailing)
	EthWs string
	// Ethereum Beacon API URL
	BeaconAPI string
	// Signer address cosmos
	SignerAddress string
	// Whether we use the mock client in Cosmos
	MockWasmClient bool
	// SpectreClient light client address (set after create-clients)
	SpectreClient string
	// SignatureVerifier contract address
	SignatureVerifier string
	// Membership module contract address
	Membership string
	// Misbehaviour module contract address
	Misbehaviour string
	// UpdateClient module contract address
	UpdateClient string
	// Cosmos wasm client ID (e.g. "08-wasm-0") — required by relayer cosmos_to_eth config.
	CosmosWasmClientID string
	// ICS26 client ID = the Cosmos light client ID registered on ETH's ICS26Router
	// (e.g. "cosmoshub-1"). The relayer uses this to set the CounterpartyClientId when
	// registering the wasm-eth-client on Cosmos, and the ETH-side packet's SourceClient
	// field must match it for Cosmos's verification to succeed.
	ICS26ClientID string
	// Trust level fraction for the Tendermint light client (e.g. "1/3", "2/3"). Empty = use relayer default.
	TrustLevel string
	// Proof type used by create-clients / update-client (e.g. "groth16"). Empty = use relayer default.
	ProofType string
}

func CreateEthCosmosModules(
	configInfo EthCosmosConfigInfo,
) []ModuleConfig {
	if configInfo.EthWs == "" {
		configInfo.EthWs = ethWsURLFromRPC(configInfo.EthRPC)
	}
	return []ModuleConfig{
		{
			Name:     ModuleEthToCosmos,
			SrcChain: configInfo.EthChainID,
			DstChain: configInfo.CosmosChainID,
			Config: ethToCosmosConfig{
				TmRpcUrl:        configInfo.TmRPC,
				Ics26Address:    configInfo.ICS26Address,
				EthRpcUrl:       configInfo.EthRPC,
				EthBeaconApiUrl: configInfo.BeaconAPI,
				SignerAddress:   configInfo.SignerAddress,
				Mock:            configInfo.MockWasmClient,
			},
		},
		{
			Name:     ModuleCosmosToEth,
			SrcChain: configInfo.CosmosChainID,
			DstChain: configInfo.EthChainID,
			Config: CosmosToEthModuleConfig{
				TmRpcUrl:           configInfo.TmRPC,
				Ics26Address:       configInfo.ICS26Address,
				ICS26ClientID:      configInfo.ICS26ClientID,
				CosmosWasmClientID: configInfo.CosmosWasmClientID,
				EthRpcUrl:          configInfo.EthRPC,
				EthWsUrl:           configInfo.EthWs,
				SpectreClient:      configInfo.SpectreClient,
				SignatureVerifier:  configInfo.SignatureVerifier,
				Membership:         configInfo.Membership,
				Misbehaviour:       configInfo.Misbehaviour,
				UpdateClient:       configInfo.UpdateClient,
				TrustLevel:         configInfo.TrustLevel,
				ProofType:          configInfo.ProofType,
			},
		},
	}
}
