// The update-eth-client command.
package main

import (
	"context"
	"fmt"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"relayer/chain"
	"relayer/chain/cosmos"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/transaction"
)

// UpdateEthClient advances the Ethereum light client that lives on Cosmos once.
// It deliberately does not start the relay loop: this is the recovery path for a
// client whose stale state is preventing that loop from making progress.
func UpdateEthClient(logger *zap.Logger) *cobra.Command {
	return newUpdateEthClientCommand(logger, &transaction.Handler{})
}

// Keep the transaction boundary injectable while exercising the real builder
// and Cosmos destination in command-execution tests.
func newUpdateEthClientCommand(logger *zap.Logger, txHandler services.TransactionHandler) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update-eth-client",
		Short: "update the Ethereum light client that lives on Cosmos (Ethereum → Cosmos) once",
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
			logConfigTarget(logger, "update-eth-client", configPath, cfg)
			if !cfg.HasCosmosToEth {
				return fmt.Errorf("update-eth-client requires a cosmos_to_eth module for the Cosmos RPC and Ethereum client ID")
			}
			if !cfg.HasEthToCosmos {
				return fmt.Errorf("update-eth-client requires an eth_to_cosmos module for the Ethereum beacon API")
			}

			ethClientID := cosmosWasmClientIDOrDefault(cfg)
			if ethClientID == "" {
				return fmt.Errorf("cosmos_wasm_client_id is required in cosmos_to_eth config")
			}

			cosmosClient, err := relayerclient.DialCosmosRPC(
				cfg.CosmosToEthConfig.TmRpcUrl,
				"/websocket",
				relayerclient.DefaultRPCTimeout,
			)
			if err != nil {
				return fmt.Errorf("failed to create Cosmos RPC client: %w", err)
			}

			// This command needs only the Cosmos HTTP RPC and the beacon REST API.
			// In particular, do not start a WebSocket subscription or dial Ethereum
			// execution RPC: neither participates in building or submitting an
			// 08-wasm beacon update, and requiring either weakens the recovery tool.
			cosmosEndpoint := services.CosmosEndpoint{Client: cosmosClient}
			worker := services.NewOneShotWorker(txHandler, nil)
			result, err := worker.BuildEthClientUpdateHeaders(
				cmd.Context(),
				cosmosEndpoint,
				services.EVMEndpoint{BeaconAPIURL: cfg.EthToCosmosConfig.BeaconUrl},
				ethClientID,
			)
			if err != nil {
				return fmt.Errorf("failed to build Ethereum client update on Cosmos: %w", err)
			}
			if result == nil || result.EthClientState == nil {
				return fmt.Errorf("update-eth-client: builder returned nil Ethereum client state")
			}
			if len(result.Headers) == 0 {
				cmd.Println(ethClientUpdateSummary(result.EthClientState, false))
				return nil
			}

			update := chain.ClientUpdate{
				Height:   result.EthClientState.LatestExecutionBlockNumber,
				Payloads: result.Headers,
			}
			destination := cosmos.NewDestination(worker, cosmosEndpoint, ethClientID)
			if err := destination.UpdateClient(contextOrBackground(cmd.Context()), ethClientID, update); err != nil {
				return fmt.Errorf("failed to update Ethereum client on Cosmos: %w", err)
			}
			cmd.Println(ethClientUpdateSummary(result.EthClientState, true))
			return nil
		},
	}
	cmd.Flags().String(flagConfigPath, "config.json", "path to JSON config file")
	return cmd
}

// contextOrBackground keeps the one-shot command usable in tests and embeds
// the command's cancellation semantics at the submission boundary. Cobra gives
// commands a context in normal operation, but a directly constructed command
// may not have one.
func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func ethClientUpdateSummary(state *relayerclient.EthereumClientState, submitted bool) string {
	if submitted {
		return fmt.Sprintf(
			"update-eth-client complete: latest_slot=%d latest_execution_block_number=%d",
			state.LatestSlot,
			state.LatestExecutionBlockNumber,
		)
	}
	return fmt.Sprintf(
		"update-eth-client: Ethereum light client on Cosmos is already current; no transaction submitted (latest_slot=%d latest_execution_block_number=%d)",
		state.LatestSlot,
		state.LatestExecutionBlockNumber,
	)
}
