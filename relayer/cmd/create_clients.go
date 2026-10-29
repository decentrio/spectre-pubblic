// The create-clients commands: one per chain, Cosmos first so the ETH side can
// be wired to the wasm client id it produces.
package main

import (
	"context"
	"fmt"
	"strings"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	proto "github.com/cosmos/gogoproto/proto"
	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	tendermintClient "relayer/client"
	"relayer/services"
	"relayer/transaction"
)

func cosmosHasWasmChecksum(cosmosClient *rpchttp.HTTP, checksum string) (bool, error) {
	checksum = strings.ToLower(strings.TrimPrefix(checksum, "0x"))

	reqBytes, err := proto.Marshal(&ibcwasmtypes.QueryChecksumsRequest{})
	if err != nil {
		return false, fmt.Errorf("failed to marshal checksum query: %w", err)
	}

	result, err := cosmosClient.ABCIQuery(context.Background(), "/ibc.lightclients.wasm.v1.Query/Checksums", reqBytes)
	if err != nil {
		return false, fmt.Errorf("failed to query wasm checksums: %w", err)
	}
	if result.Response.Code != 0 {
		return false, fmt.Errorf("checksum query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}

	var resp ibcwasmtypes.QueryChecksumsResponse
	if err := proto.Unmarshal(result.Response.Value, &resp); err != nil {
		return false, fmt.Errorf("failed to unmarshal checksum query response: %w", err)
	}

	for _, existing := range resp.Checksums {
		if strings.ToLower(existing) == checksum {
			return true, nil
		}
	}

	return false, nil
}

// buildCreateClientsDeps dials both chains and returns scoped relay dependencies
// wired for the create-clients flow. wasmClientID is the eth-light-client-on-
// Cosmos id used as the on-chain counterparty; pass "" for the Cosmos step,
// which discovers it. The returned cosmosClient has its WebSocket started — the
// caller must Stop it.
func buildCreateClientsDeps(logger *zap.Logger, cfg *appConfig, wasmClientID string) (services.RelayDeps, *rpchttp.HTTP, error) {
	logger.Sugar().Infof("create-clients: dialing ethereum rpc %s", cfg.CosmosToEthConfig.EthRpcUrl)
	ethClient, err := tendermintClient.DialEthRPC(context.Background(), cfg.CosmosToEthConfig.EthRpcUrl, tendermintClient.DefaultRPCTimeout)
	if err != nil {
		return services.RelayDeps{}, nil, fmt.Errorf("failed to connect to Ethereum: %w", err)
	}

	logger.Sugar().Infof("create-clients: creating cosmos rpc client %s", cfg.CosmosToEthConfig.TmRpcUrl)
	cosmosClient, err := tendermintClient.DialCosmosRPC(cfg.CosmosToEthConfig.TmRpcUrl, "/websocket", tendermintClient.DefaultRPCTimeout)
	if err != nil {
		return services.RelayDeps{}, nil, fmt.Errorf("failed to create Cosmos RPC client: %w", err)
	}

	cosmosRouterClientID := cosmosRouterClientIDOrDefault(cfg)
	if cosmosRouterClientID == "" {
		return services.RelayDeps{}, nil, fmt.Errorf("cosmos router client ID (ICS26_CLIENT_ID or ics26_client_id) is required and cannot be empty")
	}

	deps := services.RelayDeps{
		Cosmos: services.CosmosEndpoint{Client: cosmosClient},
		EVM: services.EVMEndpoint{Client: ethClient, BeaconAPIURL: resolveBeaconURL(cfg), Contracts: services.EVMContracts{
			Router: common.HexToAddress(cfg.CosmosToEthConfig.ICS26Address), SignatureVerifier: common.HexToAddress(cfg.CosmosToEthConfig.SignatureVerifier),
			Membership: common.HexToAddress(cfg.CosmosToEthConfig.Membership), Misbehaviour: common.HexToAddress(cfg.CosmosToEthConfig.Misbehaviour),
			UpdateClient: common.HexToAddress(cfg.CosmosToEthConfig.UpdateClient), RoleManager: common.HexToAddress(roleManagerOrDefault(cfg)),
		}},
		IDs: services.ClientIDs{CosmosOnEVM: cosmosRouterClientID, EVMOnCosmos: wasmClientID},
	}

	if err := cosmosClient.Start(); err != nil {
		return services.RelayDeps{}, nil, fmt.Errorf("failed to start Cosmos WS client: %w", err)
	}
	return deps, cosmosClient, nil
}

// runCreateClientsCosmos creates the Ethereum light client on Cosmos — the side
// whose client id is auto-assigned by ibc-go's global client sequence —
// registers the counterparty, and persists the resulting cosmos_wasm_client_id
// back into config. Returns the created id. Run this BEFORE the ETH step so the
// real id (not a guessed one) can be wired into the ETH router counterparty.
//
// The client is the chain's native spectre light client module, so there is no
// contract to store and no checksum to name: the bootstrap states go on chain as
// protobuf. cosmos_wasm_client_id keeps its name (it still holds the id of the
// client that tracks Ethereum) so existing configs stay readable.
func runCreateClientsCosmos(logger *zap.Logger, cfg *appConfig, configPath string) (string, error) {
	if resolveBeaconURL(cfg) == "" {
		return "", fmt.Errorf("an L1 beacon endpoint is required to create the Ethereum light client on Cosmos: " +
			"set eth_beacon_api_url on the eth_to_cosmos module")
	}

	// Resolve where the id will be recorded BEFORE spending an on-chain
	// MsgCreateClient. ibc-go assigns the id and the client cannot be un-created, so
	// discovering the destination is missing afterwards leaves a paid-for client that
	// nothing points at and nothing advances until it expires (#310).
	//
	// selectCosmosSource promotes the cosmos_to_l2 module into CosmosToEthConfig when
	// the config has no cosmos_to_eth source, and the write-back is pinned to
	// cosmos_to_eth — so an eth_to_cosmos + L2 config passes every other guard and
	// fails only here.
	// Cheap to know, irreversible to learn late.
	if err := assertConfigMemberWritable(configPath, cfg.CosmosToEthConfig.ICS26ClientID,
		"cosmos_wasm_client_id", dirCosmosToEth); err != nil {
		return "", fmt.Errorf("refusing to create an Ethereum light client that could not be recorded: %w", err)
	}

	deps, cosmosClient, err := buildCreateClientsDeps(logger, cfg, "")
	if err != nil {
		return "", err
	}
	defer cosmosClient.Stop()

	if existing := cosmosWasmClientIDOrDefault(cfg); existing != "" {
		logger.Sugar().Warnf("create-clients-cosmos: config already has cosmos_wasm_client_id=%s; a new client will be created and the value overwritten", existing)
	}

	worker := services.NewWorker(&transaction.Handler{}, nil)
	logger.Sugar().Infof("Creating Ethereum light client on Cosmos...")
	wasmClientID, err := worker.CreateEthClient(context.Background(), deps.Cosmos, deps.EVM, deps.IDs.CosmosOnEVM)
	if err != nil {
		return "", fmt.Errorf("failed to create Ethereum client on Cosmos: %w", err)
	}
	logger.Sugar().Infof("Ethereum light client created on Cosmos: clientID=%s", wasmClientID)

	if err := writeEthWasmClientID(configPath, cfg.CosmosToEthConfig.ICS26ClientID, wasmClientID); err != nil {
		return "", unrecordedClientError(configPath, wasmClientID, err)
	}
	logger.Sugar().Infof("create-clients-cosmos: wrote cosmos_wasm_client_id=%s into %s", wasmClientID, configPath)
	return wasmClientID, nil
}

// runCreateClientsEth deploys the Cosmos light client (SpectreClient) on
// Ethereum, registers wasmClientID as its counterparty, and persists the
// spectre_client address back into config. wasmClientID must already be known
// (created by the Cosmos step) so the on-chain counterparty is wired to the
// real id. Idempotent: if spectre_client already has deployed code, it is reused.
func runCreateClientsEth(logger *zap.Logger, cfg *appConfig, configPath, wasmClientID, trustLevel string, trustingPeriod uint32) (common.Address, error) {
	if wasmClientID == "" {
		return common.Address{}, fmt.Errorf("cosmos_wasm_client_id is empty; run create-clients-cosmos first")
	}

	deps, cosmosClient, err := buildCreateClientsDeps(logger, cfg, wasmClientID)
	if err != nil {
		return common.Address{}, err
	}
	defer cosmosClient.Stop()

	return deployCosmosClientOnEth(logger, cfg, configPath, wasmClientID, trustLevel, trustingPeriod, deps, cosmosClient)
}

// deployCosmosClientOnEth is runCreateClientsEth once both chains are dialed. It is
// split out so the checks that guard the deploy can be exercised against stand-in
// nodes: dialing is the one step that needs a real Cosmos websocket.
func deployCosmosClientOnEth(
	logger *zap.Logger,
	cfg *appConfig,
	configPath, wasmClientID, trustLevel string,
	trustingPeriod uint32,
	deps services.RelayDeps,
	cosmosClient *rpchttp.HTTP,
) (common.Address, error) {
	// Idempotency: skip the deploy when the configured spectre_client is a light
	// client this run can actually keep, so re-running after a partial failure
	// does not redeploy SpectreClient. Reusability is a property of the client's state,
	// not of the address having code — see reusableSpectreClientAt.
	if cfg.CosmosToEthConfig.SpectreClient != "" {
		addr := common.HexToAddress(cfg.CosmosToEthConfig.SpectreClient)
		ok, reason := reusableSpectreClientAt(deps.EVM, cosmosClient, addr, cfg.CosmosToEthConfig.ICS26ClientID, wasmClientID)
		if ok {
			logger.Sugar().Infof(
				"create-clients-eth: reusing the SpectreClient already deployed at %s; skipping deploy", addr.Hex())
			return addr, nil
		}
		logger.Sugar().Warnf(
			"create-clients-eth: not reusing spectre_client %s — %s; deploying a new one",
			addr.Hex(), reason)
	}

	// Past this point a SpectreClient is going to be deployed, and its modules are
	// immutable. Refuse before any Cosmos work or gas is spent if one is unset or
	// holds no code (#568). A reused client has already been deployed and is not
	// judged here.
	if err := checkSpectreModulesBeforeDeploy(context.Background(), cfg, deps.EVM); err != nil {
		return common.Address{}, err
	}

	if trustingPeriod == 0 {
		unbondingPeriod, err := tendermintClient.GetUnbondingTime(cosmosClient)
		if err != nil {
			return common.Address{}, fmt.Errorf("failed to fetch unbonding time: %w", err)
		}
		trustingPeriod = 2 * uint32(unbondingPeriod) / 3
	}

	proofType := cfg.CosmosToEthConfig.ProofType
	if proofType == "" {
		proofType = "groth16"
	}

	clockDrift := cfg.CosmosToEthConfig.ClockDrift
	if clockDrift == 0 {
		clockDrift = tendermintClient.DefaultClockDrift
	}

	worker := services.NewWorker(&transaction.Handler{}, nil)
	logger.Sugar().Infof(
		"Creating Cosmos light client on Ethereum (trustingPeriod=%d, trustLevel=%s, proofType=%s, clockDrift=%d, counterparty=%s)...",
		trustingPeriod, trustLevel, proofType, clockDrift, wasmClientID,
	)
	spectreAddr, err := worker.CreateCosmosClient(context.Background(), deps.Cosmos, deps.EVM, deps.IDs, proofType, trustingPeriod, 0, trustLevel, clockDrift)
	if err != nil {
		return common.Address{}, fmt.Errorf("failed to create Cosmos client on Ethereum: %w", err)
	}
	if (spectreAddr == common.Address{}) {
		return common.Address{}, fmt.Errorf("spectre_client address missing after deploy")
	}

	if err := writeSpectreClientAddress(configPath, cfg.CosmosToEthConfig.ICS26ClientID, spectreAddr.Hex()); err != nil {
		return common.Address{}, fmt.Errorf("persist spectre_client address to %s: %w", configPath, err)
	}
	logger.Sugar().Infof("create-clients-eth: wrote spectre_client=%s into %s", spectreAddr.Hex(), configPath)
	return spectreAddr, nil
}

// CreateClients runs the full two-chain setup as a one-shot (devnet bring-up):
// create-clients-cosmos first (so the auto-assigned wasm client id is known),
// then create-clients-eth wired to that id. No id guessing, no assertion.
// CreateClientsCosmos creates only the Ethereum light client on Cosmos and
// persists cosmos_wasm_client_id. Run this before create-clients-eth.
func CreateClientsCosmos(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create-clients-cosmos",
		Short: "create the Ethereum light client on Cosmos (writes cosmos_wasm_client_id)",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := cmd.Flags().GetString(flagConfigPath)
			if err != nil {
				return fmt.Errorf("failed to get config path: %w", err)
			}
			_ = godotenv.Load()
			// This command creates the L2 wasm clients, so their ids are allowed
			// to still be empty here — it is what fills them in (#309).
			cfg, err := loadConfigForClientCreation(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			cfg = selectCosmosSource(cfg)
			logConfigTarget(logger, "create-clients-cosmos", configPath, cfg)
			if err := preflightCreateClients(cfg); err != nil {
				return err
			}
			l2Configs, err := cmd.Flags().GetStringArray(flagL2Config)
			if err != nil {
				return err
			}

			// One invocation creates one kind of client. --l2-config means "create these
			// L2 clients"; without it the command creates the Ethereum client for the
			// Cosmos<->Ethereum path.
			//
			// The two used to be combined, because every L2 client was anchored to an
			// Ethereum client whose id had to be injected into its rollup profile. The
			// attestor-trusted profile has no ethereum_client member, so that reason is
			// gone — and combining them is actively harmful. runCreateClientsCosmos keys
			// its cosmos_wasm_client_id write-back on cfg.CosmosToEthConfig, which
			// selectCosmosSource sets to the cosmos_to_l2 module when the file declares
			// no cosmos_to_eth one: on such a file the Ethereum MsgCreateClient lands and
			// the write-back then fails because no cosmos_to_eth module carries that id,
			// leaving an orphaned client on chain. And a file that carried both, sharing
			// one id (config.example.json ships cosmos-to-eth and cosmos-to-op both on
			// cosmoshub-1), would have had the write-back succeed and repoint the
			// Ethereum module at a brand-new client the Ethereum-side SpectreClient was
			// never registered against — every recvPacket then reverts on a counterparty
			// mismatch and the original client is orphaned to expire. One path per file
			// closes that second case at parse time; the first still needs this guard.
			//
			// Splitting the invocations removes the ambiguity instead of trying to detect
			// it, and matches the one-command-per-chain direction of #255.
			if len(l2Configs) == 0 {
				if _, err := runCreateClientsCosmos(logger, cfg, configPath); err != nil {
					return err
				}
				return nil
			}
			logger.Sugar().Infof(
				"create-clients-cosmos: creating %d L2 client(s); the Ethereum client is not touched "+
					"(run this command without --l2-config to create it)", len(l2Configs))
			// Then create one L2 wasm client on Cosmos per --l2-config. All Cosmos-side,
			// same signer — one command per chain (see #255).
			for _, l2Path := range l2Configs {
				l2cfg, err := loadL2ClientConfig(l2Path)
				if err != nil {
					return err
				}
				l2ClientID, err := runCreateClientsL2(logger, cfg, l2cfg)
				if err != nil {
					return fmt.Errorf("create L2 client from %s: %w", l2Path, err)
				}
				// A cosmos_to_l2 module's cosmos_wasm_client_id is the Cosmos client
				// tracking the L2. create-clients-eth reads it to register the
				// SpectreClient's counterparty, so a wrong id there makes every
				// recvPacket revert on a counterparty mismatch. The module is keyed by
				// the L2-side client id (the rollup router's client that tracks Cosmos).
				target := l2cfg.CounterpartyClientID
				if target == "" {
					logger.Sugar().Warnf(
						"create-clients-cosmos[l2]: %s has no counterparty_client_id, cannot locate its module; "+
							"set cosmos_wasm_client_id=%s manually before create-clients-eth",
						l2Path, l2ClientID)
					continue
				}
				if err := writeL2WasmClientID(configPath, target, l2ClientID); err != nil {
					return fmt.Errorf("persist cosmos_wasm_client_id for %s: %w", l2Path, err)
				}
				logger.Sugar().Infof(
					"create-clients-cosmos[l2]: wrote cosmos_wasm_client_id=%s into the %q module",
					l2ClientID, target)

				// The return path (l2_to_cosmos) queries the same client, and its id is
				// not predictable before MsgCreateClient lands — so write it back here
				// rather than leaving the operator to copy it across two modules.
				if err := writeL2SourceClientIDs(configPath, target, l2ClientID); err != nil {
					return fmt.Errorf("persist l2_to_cosmos client ids for %s: %w", l2Path, err)
				}
				logger.Sugar().Infof(
					"create-clients-cosmos[l2]: wrote l2_wasm_client_id=%s into the %q l2_to_cosmos module",
					l2ClientID, target)
			}
			return nil
		},
	}
	cmd.Flags().String(flagConfigPath, "config.json", "path to JSON config file")
	cmd.Flags().StringArray(flagL2Config, nil, "path to an L2 client config JSON; repeatable, one per L2 rollup source — creates its L2 wasm client on Cosmos")
	return cmd
}

// CreateClientsEth deploys only the Cosmos light client on Ethereum (SpectreClient) and
// persists spectre_client. Requires cosmos_wasm_client_id (run create-clients-cosmos first).
func CreateClientsEth(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create-clients-eth",
		Short: "deploy the Cosmos light client on Ethereum (requires cosmos_wasm_client_id)",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := cmd.Flags().GetString(flagConfigPath)
			if err != nil {
				return fmt.Errorf("failed to get config path: %w", err)
			}
			_ = godotenv.Load()
			cfg, err := loadConfig(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			cfg = selectCosmosSource(cfg)
			logConfigTarget(logger, "create-clients-eth", configPath, cfg)
			if err := preflightCreateClients(cfg); err != nil {
				return err
			}
			trustLevel, err := cmd.Flags().GetString(flagTrustLevel)
			if err != nil {
				return fmt.Errorf("failed to get trust level: %w", err)
			}
			trustingPeriod, err := cmd.Flags().GetUint32(flagTrustingPeriod)
			if err != nil {
				return fmt.Errorf("failed to get trusting period: %w", err)
			}
			wasmClientID := cosmosWasmClientIDOrDefault(cfg)
			if wasmClientID == "" {
				return fmt.Errorf("cosmos_wasm_client_id is empty in config; run create-clients-cosmos first")
			}
			_, err = runCreateClientsEth(logger, cfg, configPath, wasmClientID, trustLevel, trustingPeriod)
			return err
		},
	}
	cmd.Flags().String(flagConfigPath, "config.json", "path to JSON config file")
	cmd.Flags().String(flagTrustLevel, "2/3", "trust level for Cosmos light client (e.g., 1/3, 2/3)")
	cmd.Flags().Uint32(flagTrustingPeriod, 0, "trusting period in seconds for Cosmos light client (default: 2/3 of chain unbonding period)")
	return cmd
}
