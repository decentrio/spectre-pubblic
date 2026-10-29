// The update-client command.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	tendermintClient "relayer/client"
	"relayer/relay"
	"relayer/services"
	"relayer/transaction"
)

// UpdateClient advances the Cosmos light client that lives on Ethereum once.
func UpdateClient(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update-client",
		Short: "update the Cosmos light client that lives on Ethereum (Cosmos → Ethereum) once",
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
			logConfigTarget(logger, "update-client", configPath, cfg)

			ethClient, err := tendermintClient.DialEthRPC(context.Background(), cfg.CosmosToEthConfig.EthRpcUrl, tendermintClient.DefaultRPCTimeout)
			if err != nil {
				return fmt.Errorf("failed to connect to Ethereum: %w", err)
			}

			cosmosClient, err := tendermintClient.DialCosmosRPC(cfg.CosmosToEthConfig.TmRpcUrl, "/websocket", tendermintClient.DefaultRPCTimeout)
			if err != nil {
				return fmt.Errorf("failed to create Cosmos RPC client: %w", err)
			}

			p, err := dialProver(cmd.Context(), cmd, logger)
			if err != nil {
				return err
			}
			defer p.Close()

			cosmosWasmClientID := cosmosWasmClientIDOrDefault(cfg)
			if cosmosWasmClientID == "" {
				return fmt.Errorf("cosmos_wasm_client_id is required in cosmos_to_eth config")
			}

			cosmosRouterClientID := cosmosRouterClientIDOrDefault(cfg)
			if cosmosRouterClientID == "" {
				return fmt.Errorf("cosmos router client ID (ICS26_CLIENT_ID or ics26_client_id) is required and cannot be empty")
			}
			roleManager := roleManagerOrDefault(cfg)
			if cfg.CosmosToEthConfig.SpectreClient == "" {
				return fmt.Errorf("spectre_client address is required in cosmos_to_eth config")
			}
			deps := services.RelayDeps{
				Cosmos: services.CosmosEndpoint{Client: cosmosClient},
				EVM: services.EVMEndpoint{Client: ethClient, BeaconAPIURL: cfg.EthToCosmosConfig.BeaconUrl, Contracts: services.EVMContracts{
					Router: common.HexToAddress(cfg.CosmosToEthConfig.ICS26Address), SignatureVerifier: common.HexToAddress(cfg.CosmosToEthConfig.SignatureVerifier),
					Membership: common.HexToAddress(cfg.CosmosToEthConfig.Membership), Misbehaviour: common.HexToAddress(cfg.CosmosToEthConfig.Misbehaviour),
					UpdateClient: common.HexToAddress(cfg.CosmosToEthConfig.UpdateClient), RoleManager: common.HexToAddress(roleManager), SpectreClient: common.HexToAddress(cfg.CosmosToEthConfig.SpectreClient),
				}},
				IDs: services.ClientIDs{CosmosOnEVM: cosmosRouterClientID, EVMOnCosmos: cosmosWasmClientID},
			}

			cosmosConfig := services.DefaultConfig()
			if cfg.CosmosToEthConfig.TrustingPeriod != 0 {
				cosmosConfig.TrustingPeriod = cfg.CosmosToEthConfig.TrustingPeriod
			}
			if cfg.CosmosToEthConfig.TrustLevel != "" {
				cosmosConfig.TrustLevel = cfg.CosmosToEthConfig.TrustLevel
			}
			if cfg.CosmosToEthConfig.ProofType != "" {
				cosmosConfig.ProofType = cfg.CosmosToEthConfig.ProofType
			}
			if cfg.CosmosToEthConfig.ClockDrift != 0 {
				cosmosConfig.ClockDrift = cfg.CosmosToEthConfig.ClockDrift
			}
			if cfg.CosmosToEthConfig.BeaconFinalityRetries != 0 {
				cosmosConfig.BeaconFinalityRetries = cfg.CosmosToEthConfig.BeaconFinalityRetries
			}
			if cfg.CosmosToEthConfig.AppHashWaitRetries != 0 {
				cosmosConfig.AppHashWaitRetries = cfg.CosmosToEthConfig.AppHashWaitRetries
			}
			if cfg.CosmosToEthConfig.AppHashWaitInterval != 0 {
				cosmosConfig.AppHashWaitInterval = time.Duration(cfg.CosmosToEthConfig.AppHashWaitInterval) * time.Second
			}
			if cfg.CosmosToEthConfig.FetchTimeout != 0 {
				cosmosConfig.FetchTimeout = time.Duration(cfg.CosmosToEthConfig.FetchTimeout) * time.Second
			}
			if cfg.CosmosToEthConfig.RotationThreshold != "" {
				cosmosConfig.RotationThreshold = cfg.CosmosToEthConfig.RotationThreshold
			}
			if cfg.CosmosToEthConfig.RefreshInterval != 0 {
				cosmosConfig.RefreshInterval = time.Duration(cfg.CosmosToEthConfig.RefreshInterval) * time.Second
				cosmosConfig.RefreshIntervalConfigured = true
			}
			if envVal := os.Getenv("FETCH_TIMEOUT"); envVal != "" {
				if d, err := strconv.Atoi(envVal); err == nil && d > 0 {
					cosmosConfig.FetchTimeout = time.Duration(d) * time.Second
				}
			}
			cosmosConfig.BatchConfig = cfg.BatchConfig
			deps.Config = cosmosConfig

			if err := cosmosClient.Start(); err != nil {
				return fmt.Errorf("failed to start Cosmos WS client: %w", err)
			}
			defer cosmosClient.Stop()

			trustedBlock, err := cmd.Flags().GetInt64(flagTrustedBlock)
			if err != nil {
				return fmt.Errorf("failed to get trusted block: %w", err)
			}
			forceRotation, err := cmd.Flags().GetBool(flagForceRotation)
			if err != nil {
				return fmt.Errorf("failed to get force-rotation flag: %w", err)
			}
			targetHeight, err := cmd.Flags().GetInt64(flagTargetHeight)
			if err != nil {
				return fmt.Errorf("failed to get target-height flag: %w", err)
			}
			maxHops, err := cmd.Flags().GetInt64(flagMaxHops)
			if err != nil {
				return fmt.Errorf("failed to get max-hops flag: %w", err)
			}
			if maxHops <= 0 {
				return fmt.Errorf("update-client: --max-hops must be greater than 0")
			}

			worker := services.NewOneShotWorker(&transaction.Handler{}, p)

			// RLY-01: validator churn can force the builder onto a multi-hop
			// path that only advances the client to an intermediate height
			// per call. Loop only while the builder reports that it actually
			// selected a hop; an ordinary update to the live latest height is
			// complete even if the chain produces another block while the ETH
			// transaction is landing.
			var latestBlock *tendermintClient.LightBlock
			for hop := int64(0); ; hop++ {
				if hop >= maxHops {
					return fmt.Errorf("update-client: reached --max-hops=%d without catching up", maxHops)
				}
				result, err := worker.BuildCosmosClientUpdateMsg(
					cmd.Context(),
					deps.Cosmos,
					deps.EVM,
					cosmosConfig.FetchTimeout,
					cosmosConfig.RotationThreshold,
					cosmosConfig.ProofType,
					trustedBlock,
					cosmosConfig.TrustLevel,
					forceRotation,
					targetHeight,
				)
				if err != nil {
					return fmt.Errorf("failed to build Cosmos client update on Ethereum: %w", err)
				}
				if result == nil || result.LightBlock == nil {
					return fmt.Errorf("update-client: builder returned nil light block")
				}
				if result.HasMsg {
					if err := worker.TxHandler.SendEthTx(cmd.Context(), deps.EVM, deps.IDs.CosmosOnEVM, *result); err != nil {
						return fmt.Errorf("failed to update Cosmos client on Ethereum: %w", err)
					}
				}
				latestBlock = result.LightBlock
				logger.Sugar().Infof("update-client hop %d complete: height=%d kind=%d isHop=%t hopTarget=%d hasMsg=%t",
					hop, latestBlock.BlockHeight, result.Kind, result.IsHop, result.HopTarget, result.HasMsg)
				if !result.IsHop {
					break
				}
				trustedBlock = 0 // re-derive from on-chain state next iteration
			}
			logger.Sugar().Infof("update-client complete: latest_height=%d", latestBlock.BlockHeight)

			return nil
		},
	}
	cmd.Flags().String(flagConfigPath, "config.json", "path to JSON config file")
	cmd.Flags().Int64(flagTrustedBlock, 0, "trusted Cosmos block height hint; 0 reads from the on-chain client")
	registerProverFlag(cmd)
	cmd.Flags().Bool(flagForceRotation, false, "force pinned-set rotation regardless of overlap threshold (operator stopgap for RLY-01)")
	cmd.Flags().Int64(flagTargetHeight, 0, "override the update target height; 0 uses the chain's current latest height (operator stopgap for RLY-01)")
	cmd.Flags().Int64(flagMaxHops, relay.MaxClientUpdateHops, "maximum multi-hop iterations before giving up in one invocation")
	return cmd
}
