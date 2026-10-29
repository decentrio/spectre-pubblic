package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"relayer/prover/proverpb"
)

// proverFlagCommand is a command carrying only the prover flag, the way each command
// that proves declares it.
func proverFlagCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "x"}
	registerProverFlag(cmd)
	return cmd
}

func TestResolveProverAddr(t *testing.T) {
	t.Run("the flag wins over the environment", func(t *testing.T) {
		t.Setenv(envProverAddr, "env:1")
		cmd := proverFlagCommand()
		if err := cmd.ParseFlags([]string{"--" + flagProverAddr, "flag:2"}); err != nil {
			t.Fatal(err)
		}
		if got := resolveProverAddr(cmd); got != "flag:2" {
			t.Fatalf("got %q, want the flag value", got)
		}
	})
	t.Run("the environment is the fallback", func(t *testing.T) {
		t.Setenv(envProverAddr, "  env:1 \n")
		if got := resolveProverAddr(proverFlagCommand()); got != "env:1" {
			t.Fatalf("got %q, want the trimmed environment value", got)
		}
	})
	t.Run("neither set is empty", func(t *testing.T) {
		t.Setenv(envProverAddr, "")
		if got := resolveProverAddr(proverFlagCommand()); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestDialProver_NoAddressSaysHowToSetOne(t *testing.T) {
	t.Setenv(envProverAddr, "")
	_, err := dialProver(context.Background(), proverFlagCommand(), zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "--"+flagProverAddr) || !strings.Contains(err.Error(), envProverAddr) {
		t.Fatalf("err = %v, want it to name both --%s and %s", err, flagProverAddr, envProverAddr)
	}
}

// The address was once written as a URL ("http://65.21.11.18:50060"). gRPC answered with an
// unresolvable-target error that looks like "not up yet", so the command retried for the whole
// dial budget. It must stop at once and say what to write instead.
func TestDialProver_AURLIsRefusedAtOnceWithTheFixSpelledOut(t *testing.T) {
	// A budget long enough that waiting it out would be caught by the elapsed-time check.
	previous := proverDialBudget
	proverDialBudget = 20 * time.Second
	t.Cleanup(func() { proverDialBudget = previous })

	cases := []struct {
		addr     string
		wantSubs []string
	}{
		{"http://65.21.11.18:50060", []string{"host:port", "65.21.11.18:50060"}},
		{"HTTP://65.21.11.18:50060/", []string{"host:port", "65.21.11.18:50060"}},
		// https:// must not be quietly turned into plaintext.
		{"https://65.21.11.18:50060", []string{"TLS is not supported", "65.21.11.18:50060"}},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			t.Setenv(envProverAddr, tc.addr)
			start := time.Now()
			_, err := dialProver(context.Background(), proverFlagCommand(), zap.NewNop())
			if err == nil {
				t.Fatal("a URL was accepted as a prover address")
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %s: a URL is a mistake to report, not a prover to wait for", time.Since(start))
			}
		})
	}
}

func TestCheckProverAddr_AcceptsWhatGRPCAccepts(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:50060", "65.21.11.18:50060", "prover.internal:50060", "[::1]:50060", "dns:///prover.internal:50060", "unix:///run/proverd.sock"} {
		if err := checkProverAddr(ok); err != nil {
			t.Errorf("checkProverAddr(%q) = %v, want nil", ok, err)
		}
	}
}

func TestDialProver_UnreachableNamesTheAddressAndGivesUp(t *testing.T) {
	previous := proverDialBudget
	proverDialBudget = 300 * time.Millisecond
	t.Cleanup(func() { proverDialBudget = previous })
	t.Setenv(envProverAddr, "127.0.0.1:1")

	start := time.Now()
	_, err := dialProver(context.Background(), proverFlagCommand(), zap.NewNop())
	if err == nil {
		t.Fatal("dialProver succeeded against an address nothing listens on")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("err = %v, want the address in the message", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s: the budget was not honoured", time.Since(start))
	}
}

// infoOnlyProver answers Info and nothing else.
type infoOnlyProver struct {
	proverpb.UnimplementedProverServiceServer
}

func (infoOnlyProver) Info(context.Context, *proverpb.InfoRequest) (*proverpb.InfoResponse, error) {
	return &proverpb.InfoResponse{Buckets: []*proverpb.BucketInfo{{Bucket: 4, VkSha256: make([]byte, 32)}}}, nil
}

func TestDialProver_ConnectsToARunningProver(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	proverpb.RegisterProverServiceServer(gs, infoOnlyProver{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	t.Setenv(envProverAddr, "")
	cmd := proverFlagCommand()
	if err := cmd.ParseFlags([]string{"--" + flagProverAddr, lis.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	p, err := dialProver(context.Background(), cmd, zap.NewNop())
	if err != nil {
		t.Fatalf("dialProver: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if got := p.MaxBucket(); got != 4 {
		t.Fatalf("MaxBucket = %d, want 4", got)
	}
}

// A config with a Cosmos-sourced path proves, so start must refuse to run without a prover
// address, and say so at startup rather than when the first client update needs a proof.
func TestStart_AProvingPathRequiresAProverAddress(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := `{"modules":[{"name":"cosmos_to_eth","src_chain":"cosmoshub-1","config":{` +
		`"tm_rpc_url":"http://127.0.0.1:26657","eth_rpc_url":"http://127.0.0.1:8545",` +
		`"ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","ics26_client_id":"cosmoshub-1"}}]}`
	if err := os.WriteFile(configPath, []byte(config), configFilePerm); err != nil {
		t.Fatal(err)
	}
	const testKey = "a81f9eb900c02f35d28ec80d1da79dde52cc4cc37d6fbaef5768ab59f32cbfdd"
	t.Setenv("ETH_PRIVATE_KEY", testKey)
	t.Setenv("COSMOS_PRIVATE_KEY", testKey)
	t.Setenv("COSMOS_CHAIN_ID", "cosmoshub-1")
	t.Setenv(envProverAddr, "")

	command := Start(zap.NewNop())
	command.SetArgs([]string{"--config", configPath})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SilenceUsage = true

	err = command.Execute()
	if err == nil || !strings.Contains(err.Error(), "prover address not set") {
		t.Fatalf("start returned %v, want the prover-address error", err)
	}
}
