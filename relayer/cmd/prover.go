// Prover wiring: how a command reaches the prover service.
//
// The prover is its own process in its own repository (spectre-prover, cmd/proverd);
// the relayer only speaks to it over gRPC. A command that has to prove dials it once
// and waits for it to answer Info, so a mistyped address or a prover that is not
// running stops the command at startup, with the address in the message, instead of
// at the first client update, minutes later.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"relayer/prover/grpcclient"
	"relayer/services"
)

const (
	flagProverAddr = "prover-addr"
	envProverAddr  = "PROVER_ADDR"
)

// proverDialBudget is how long a command waits for the prover to answer Info before it
// gives up. Long enough for a prover started alongside the relayer to load its
// circuit artifacts; short enough that a wrong address is reported while the operator
// is still looking at the terminal. A variable so tests do not wait it out.
var proverDialBudget = 30 * time.Second

// proverClient is what a command holds on to: the prover the services prove through,
// and the connection to close when the command is done.
type proverClient interface {
	services.Prover
	Close() error
}

// registerProverFlag declares --prover-addr on cmd.
func registerProverFlag(cmd *cobra.Command) {
	cmd.Flags().String(flagProverAddr, "",
		"address (host:port) of the spectre-prover gRPC service (or set "+envProverAddr+")")
}

// resolveProverAddr returns --prover-addr, falling back to PROVER_ADDR: the flag wins
// over the variable, as it does everywhere else in this CLI. Empty means neither is set.
func resolveProverAddr(cmd *cobra.Command) string {
	if cmd.Flags().Changed(flagProverAddr) {
		if v, err := cmd.Flags().GetString(flagProverAddr); err == nil {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(os.Getenv(envProverAddr))
}

// checkProverAddr rejects an address written as a URL. The prover speaks gRPC, so the
// address is host:port. gRPC does not call "http://host:port" an error: it reports the
// target as unresolvable, which looks like "prover not up yet", so without this check a
// command retries something that can never work until the whole dial budget is spent.
//
// https:// is refused for a second reason. It reads as TLS, the client has none, and
// dropping the scheme quietly would send plaintext to a server the operator believes is
// protected. Schemes that are gRPC's own (dns:///, unix:///) are left alone.
func checkProverAddr(addr string) error {
	lower := strings.ToLower(addr)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return fmt.Errorf("prover address %q: TLS is not supported yet, the relayer speaks plaintext gRPC. "+
			"Use host:port (for example %s) on a network you control", addr, addr[len("https://"):])
	case strings.HasPrefix(lower, "http://"):
		return fmt.Errorf("prover address %q is a URL: write host:port without the scheme (for example %s)",
			addr, strings.TrimRight(addr[len("http://"):], "/"))
	}
	return nil
}

// dialProver connects to the prover named by --prover-addr / PROVER_ADDR and waits,
// within proverDialBudget, for it to answer. The caller closes the returned client.
func dialProver(ctx context.Context, cmd *cobra.Command, logger *zap.Logger) (proverClient, error) {
	addr := resolveProverAddr(cmd)
	if addr == "" {
		return nil, fmt.Errorf("prover address not set: pass --%s or set %s "+
			"(the prover is a separate process: run spectre-prover's proverd and point this at its listen address)",
			flagProverAddr, envProverAddr)
	}
	if err := checkProverAddr(addr); err != nil {
		return nil, err
	}
	logger.Sugar().Infof("connecting to prover at %s", addr)
	dialCtx, cancel := context.WithTimeout(ctx, proverDialBudget)
	defer cancel()
	c, err := grpcclient.Dial(dialCtx, addr)
	if err != nil {
		return nil, fmt.Errorf("failed to reach prover: %w", err)
	}
	return c, nil
}
