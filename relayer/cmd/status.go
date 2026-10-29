package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"relayer/status"
)

// statusFetchTimeout bounds the whole command. It is generous because the answer
// is worth waiting for during an incident, and short enough that a relayer that
// is wedged rather than slow does not hang a terminal.
const statusFetchTimeout = 30 * time.Second

// Status reads a running relayer's in-flight state over its local endpoint.
//
// The question it answers is the one a packet commitment cannot: a commitment
// surviving on the source says the round trip is incomplete, but not WHICH HALF
// is stuck. The waiting set is "not relayed yet, and here is the gate"; the
// pending set is "relayed, awaiting delivery or a timeout refund".
func Status(logger *zap.Logger) *cobra.Command {
	var configPath, addr string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Read a running relayer's in-flight state",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := statusTarget(logger, "status", addr, configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), statusFetchTimeout)
			defer cancel()
			snap, err := status.Fetch(ctx, target)
			if err != nil {
				return err
			}

			if asJSON {
				body, err := json.MarshalIndent(snap, "", "  ")
				if err != nil {
					return fmt.Errorf("encode status: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			return renderStatus(cmd.OutOrStdout(), snap)
		},
	}

	// Persistent so the reactivate subcommand addresses the same relayer without
	// repeating how to find it.
	cmd.PersistentFlags().StringVar(&configPath, flagConfigPath, "config.json", "config file to read status_address from")
	cmd.PersistentFlags().StringVar(&addr, "addr", "", "status endpoint to query, overriding the config")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw snapshot instead of the table")
	cmd.AddCommand(statusReactivate(logger, &configPath, &addr))
	return cmd
}

// statusReactivate is the one write path an operator has into the dead-letter
// set. It is a subcommand of status rather than a verb of its own because that
// is the order the work happens in: read what gave up, then act on it.
//
// A dead-lettered packet has funds escrowed and a log line asking for
// intervention; until this existed, the only mechanism for intervening was
// unreachable Go.
func statusReactivate(logger *zap.Logger, configPath, addr *string) *cobra.Command {
	var sel status.DeadLetterSelector

	cmd := &cobra.Command{
		Use:   "reactivate",
		Short: "Return a dead-lettered packet to the timeout scan set",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validated here so a typo costs nothing: the request is not built,
			// no relayer is contacted, and no state is touched.
			if err := sel.Validate(); err != nil {
				return err
			}
			target, err := statusTarget(logger, "status reactivate", *addr, *configPath)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), statusFetchTimeout)
			defer cancel()
			if err := status.Reactivate(ctx, target, sel); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"reactivated %s: it is back in the timeout scan set with a fresh retry budget\n", sel)
			return nil
		},
	}
	// --path and --direction are required, not conveniences. A packet identity is
	// (client, sequence) and client ids are chain-local, so those two alone can
	// match rows in more than one tracker -- and this command WRITES.
	cmd.Flags().StringVar(&sel.Path, "path", "", "relay path holding the packet, as printed by `relayer status`")
	cmd.Flags().StringVar(&sel.Direction, "direction", "", "queue direction: cosmos, eth or l2, as printed by `relayer status`")
	cmd.Flags().StringVar(&sel.SourceClient, "client", "", "source client id of the dead-lettered packet")
	cmd.Flags().Uint64Var(&sel.Sequence, "sequence", 0, "sequence of the dead-lettered packet")
	return cmd
}

// statusTarget resolves which relayer to talk to: the flag when given, otherwise
// the config. One resolution, so the two subcommands cannot address different
// processes from the same invocation.
//
// The config file is named only when it is actually read: with --addr no file
// is opened, and a line claiming one was would be the misdirection
// logConfigTarget exists to prevent.
func statusTarget(logger *zap.Logger, command, addr, configPath string) (string, error) {
	if addr != "" {
		return addr, nil
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return "", err
	}
	logConfigTarget(logger, command, configPath, cfg)
	if cfg.StatusAddress == "" {
		return "", fmt.Errorf(
			"no status address: the endpoint is off by default, so set status_address in %s or pass --addr",
			configPath)
	}
	return cfg.StatusAddress, nil
}

// renderStatus prints the snapshot for a person. Kept pure -- writer in, no
// globals -- so what an operator reads under pressure is covered by a test.
func renderStatus(w io.Writer, snap status.Snapshot) error {
	out := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)

	if len(snap.Paths) == 0 && len(snap.Queues) == 0 {
		fmt.Fprintln(w, "The relayer is running but drives no relay path.")
		return nil
	}

	for _, path := range snap.Paths {
		fmt.Fprintf(out, "\n%s\t%s -> %s\tclient %s\n", path.Path, path.Source, path.Dest, path.ClientID)
		if path.ClientHeld != "" {
			fmt.Fprintf(out, "  HELD\t%s\n", path.ClientHeld)
		}
		if path.UpdateInFlight {
			fmt.Fprintf(out, "  trusted height\t(a client update is in flight)\n")
		} else {
			fmt.Fprintf(out, "  trusted height\t%d\n", path.TrustedHeight)
		}
		if path.RelayableHeightErr != "" {
			fmt.Fprintf(out, "  relayable height\tunknown: %s\n", path.RelayableHeightErr)
		} else {
			fmt.Fprintf(out, "  relayable height\t%d\n", path.RelayableHeight)
		}

		// Stalest first, and printed before the packet lists: "which loop stopped"
		// outranks "which packet is waiting", because a stopped loop explains every
		// packet under it.
		for _, loop := range path.Loops {
			note := ""
			if loop.Note != "" {
				note = "\t" + loop.Note
			}
			fmt.Fprintf(out, "  loop %s\tlast pass %s ago%s\n", loop.Worker, loop.SinceText, note)
		}

		if len(path.Waiting) == 0 {
			fmt.Fprintf(out, "  waiting\tnothing\n")
			continue
		}
		fmt.Fprintf(out, "  waiting\t%d packet(s), oldest first\n", len(path.Waiting))
		for _, p := range path.Waiting {
			fmt.Fprintf(out, "    %s seq=%d\th=%d\tfor %s\n", p.Type, p.Sequence, p.Height, p.AgeText)
		}
	}

	for _, q := range snap.Queues {
		if q.Queued == 0 && len(q.Pending) == 0 && len(q.DeadLettered) == 0 && !q.RetryPaused {
			continue // an idle tracker is noise in an incident
		}
		fmt.Fprintf(out, "\n%s\t%s queue\tqueued %d\n", q.Path, q.Direction, q.Queued)
		if q.RetryPaused {
			// Loud on purpose: this one means the relayer has stopped doing timeout
			// work, so nothing below it is being retried.
			fmt.Fprintf(out, "  ATTENTION\ttimeout retries are paused: %s\n", q.RetryPausedReason)
		}
		for _, p := range q.Pending {
			attempts := ""
			if p.TimeoutAttempts > 0 {
				attempts = fmt.Sprintf("\t%d failed timeout attempt(s)", p.TimeoutAttempts)
			}
			fmt.Fprintf(out, "    pending %s seq=%d\tfor %s%s\n", p.SourceClient, p.Sequence, p.AgeText, attempts)
		}
		for _, d := range q.DeadLettered {
			fmt.Fprintf(out, "    DEAD-LETTERED %s seq=%d\tgave up %s ago after %d attempt(s)\n",
				d.SourceClient, d.Sequence, snap.Now.Sub(d.DeadLetteredAt).Round(time.Second), d.Attempts)
		}
	}

	if err := out.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	return nil
}
