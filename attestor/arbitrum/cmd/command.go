package main

import (
	"context"

	"github.com/spf13/cobra"
)

const (
	defaultConfigPath = "config.json"
	configFlagName    = "config"
)

type startRunner func(context.Context, string) error

func execute(ctx context.Context, args []string) error {
	command := newRootCommand(runAttestor)
	command.SetArgs(args)
	return command.ExecuteContext(ctx)
}

func newRootCommand(run startRunner) *cobra.Command {
	command := &cobra.Command{
		Use:           "attestor",
		Short:         "Run the Spectre Arbitrum attestor",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	command.AddCommand(newStartCommand(run))
	return command
}

func newStartCommand(run startRunner) *cobra.Command {
	configPath := defaultConfigPath
	command := &cobra.Command{
		Use:   "start",
		Short: "Start the Arbitrum attestor gRPC server",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return run(command.Context(), configPath)
		},
	}
	command.Flags().StringVarP(
		&configPath,
		configFlagName,
		"c",
		defaultConfigPath,
		"path to the attestor JSON configuration",
	)
	return command
}
