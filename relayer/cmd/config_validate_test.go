package main

import (
	attestorpb "attestor/types/attestor"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"relayer/rpcmock"
	"relayer/services"
	"strings"
	"testing"

	cmtp2p "github.com/cometbft/cometbft/p2p"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"
)

// `start` with nothing to relay is a config mistake, not an idle process.
func TestRequireRelayPath_RejectsEmptyConfig(t *testing.T) {
	err := requireRelayPath(&appConfig{}, "/etc/relayer/config.json")
	if err == nil {
		t.Fatal("start accepted a config with no relay module")
	}
	if !strings.Contains(err.Error(), "/etc/relayer/config.json") {
		t.Errorf("error must name the config file it read; got: %v", err)
	}
}

func TestValidateStateDir_AcceptsAWritableDirAndLeavesNothingBehind(t *testing.T) {
	// The directory the process will really write into, passed in. It used to be
	// read from RELAYER_RECOVERY_STATE_FILE; that variable is now REFUSED at
	// startup, so validating it would judge a path nothing writes.
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	if findings := validateStateDir(stateDir); len(findings) != 0 {
		t.Fatalf("expected no findings for a writable dir, got %+v", findings)
	}

	// The probe must not survive. A leftover file in the state directory is not
	// merely untidy: the next run's O_EXCL probe would fail on it and report a
	// writable directory as unwritable.
	entries, err := os.ReadDir(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("probe left files behind: %v", names)
	}
}

func TestValidateStateDir_RejectsADirectoryItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this test relies on")
	}
	parent := t.TempDir()
	readOnly := filepath.Join(parent, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatalf("create read-only dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	findings := validateStateDir(filepath.Join(readOnly, "nested"))
	if len(findings) != 1 {
		t.Fatalf("expected exactly one finding, got %+v", findings)
	}
	if findings[0].key != "state directory" {
		t.Fatalf("finding names %q, want the thing an operator would fix", findings[0].key)
	}
}

// --- cosmos chain id ---------------------------------------------------------

func TestValidateCosmosChainID(t *testing.T) {
	t.Run("fails before dialing when the variable is unset", func(t *testing.T) {
		node := rpcmock.NewCosmosNode(t)
		node.Handle("status", func(json.RawMessage) (any, error) {
			return statusResult("some-chain"), nil
		})
		t.Setenv("COSMOS_CHAIN_ID", "")

		findings := validateCosmosChainID(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{TmRpcUrl: node.URL()},
		})

		if len(findings) != 1 || findings[0].key != "COSMOS_CHAIN_ID" {
			t.Fatalf("expected one COSMOS_CHAIN_ID finding, got %+v", findings)
		}
		// Nothing to compare against means nothing to ask, and asking anyway would
		// make an unset variable look like an endpoint problem in the log.
		if calls := node.Calls("status"); calls != 0 {
			t.Fatalf("queried the node %d times with no declared chain id; want 0", calls)
		}
	})

	t.Run("reports a definite disagreement", func(t *testing.T) {
		node := rpcmock.NewCosmosNode(t)
		node.Handle("status", func(json.RawMessage) (any, error) {
			return statusResult("served-chain"), nil
		})
		t.Setenv("COSMOS_CHAIN_ID", "declared-chain")

		findings := validateCosmosChainID(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{TmRpcUrl: node.URL()},
		})

		if len(findings) != 1 {
			t.Fatalf("expected one finding, got %+v", findings)
		}
		// Both halves must appear: an operator has to know which of the two to fix,
		// and a message naming only one of them cannot tell them.
		for _, want := range []string{"declared-chain", "served-chain", node.URL()} {
			if !strings.Contains(findings[0].detail, want) {
				t.Fatalf("finding %q does not mention %q", findings[0].detail, want)
			}
		}
	})

	t.Run("accepts a match", func(t *testing.T) {
		node := rpcmock.NewCosmosNode(t)
		node.Handle("status", func(json.RawMessage) (any, error) {
			return statusResult("test-ibc-eth"), nil
		})
		t.Setenv("COSMOS_CHAIN_ID", "test-ibc-eth")

		if findings := validateCosmosChainID(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{TmRpcUrl: node.URL()},
		}); len(findings) != 0 {
			t.Fatalf("expected no findings for a matching chain id, got %+v", findings)
		}
	})

	t.Run("tolerates an endpoint that does not answer", func(t *testing.T) {
		// A node with no "status" handler answers "method not found" — the shape of
		// an endpoint that is reachable but useless. Startup must survive it: an
		// unavailable node is an operations problem, and the relay loops retry.
		node := rpcmock.NewCosmosNode(t)
		t.Setenv("COSMOS_CHAIN_ID", "declared-chain")

		if findings := validateCosmosChainID(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{TmRpcUrl: node.URL()},
		}); len(findings) != 0 {
			t.Fatalf("a non-answering endpoint must not fail startup, got %+v", findings)
		}
	})
}

func TestCosmosEndpointsInConfig_AreGatheredFromEveryDirectionWithoutDuplicates(t *testing.T) {
	cfg := &appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{TmRpcUrl: "http://a"},
		HasCosmosToL2: true, CosmosToL2Config: cosmosToEthConfig{TmRpcUrl: "http://a"},
		HasL2ToCosmos: true, L2ToCosmosConfig: l2ToCosmosConfig{TmRpcUrl: "http://c"},
	}
	got := cosmosEndpointsInConfig(cfg)
	want := []string{"http://a", "http://c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("gathered %v, want %v", got, want)
	}
}

// --- EVM deployments ---------------------------------------------------------

func TestValidateEVMDeployments(t *testing.T) {
	t.Run("reports every address that holds no code", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		// Only the router is deployed, so the other five are the findings — and the
		// router being present is what lets the client-id check run at all.
		node.SetCode(common.HexToAddress(cfg.ICS26Address), deployedCode)
		node.SetCallResult("0x000000000000000000000000" + strings.Repeat("ab", 20))

		findings := validateEVMDeployments(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		})

		got := map[string]bool{}
		for _, f := range findings {
			got[f.key] = true
		}
		for _, key := range []string{
			"cosmos_to_eth.spectre_client", "cosmos_to_eth.signature_verifier",
			"cosmos_to_eth.membership", "cosmos_to_eth.misbehaviour",
			"cosmos_to_eth.update_client",
		} {
			if !got[key] {
				t.Fatalf("no finding for %s; got %+v", key, findings)
			}
		}
		if got["cosmos_to_eth.ics26_address"] {
			t.Fatalf("reported the deployed router as code-less: %+v", findings)
		}
		// This test used a presence map, which is why it could not see the router
		// check handing back the findings it was given -- five problems reported as
		// ten. Count as well as check.
		if len(findings) != len(got) {
			t.Fatalf("%d findings for %d distinct keys; some are reported twice: %+v",
				len(findings), len(got), findings)
		}
	})

	// validateRouterClientID receives the findings so far so it can stay quiet when
	// the router address is itself a finding. It must not hand them back: the caller
	// appends its return value, so returning the accumulator reports everything
	// twice. Found in review -- a module with five missing contracts came out as ten
	// problems.
	t.Run("findings are not reported twice", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		// Router deployed (so the client-id check runs) and nothing else, which is
		// the shape that accumulates findings BEFORE the router check.
		node.SetCode(common.HexToAddress(cfg.ICS26Address), deployedCode)
		node.SetCallResult("0x000000000000000000000000" + strings.Repeat("ab", 20))

		findings := validateEVMDeployments(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		})

		seen := map[string]int{}
		for _, f := range findings {
			seen[f.key]++
		}
		for key, n := range seen {
			if n > 1 {
				t.Fatalf("%q reported %d times (%d findings, %d distinct): an operator "+
					"reading this cannot tell how many problems there actually are",
					key, n, len(findings), len(seen))
			}
		}
	})

	// The mirror of TestValidateEVMDeployments/validates the ID startup will actually use,
	// and the bug is its exact inverse: buildCosmosToL2Dest reads ICS26ClientID
	// straight from the JSON and never consults the environment, so applying the
	// override to a cosmos_to_l2 entry validates an id that path will never use.
	// A stale value in .env then fails a good config, and a correct one hides a bad
	// JSON id. Validation must read whatever the BUILDER reads, per path.
	t.Run("cosmos to L2 validates the config ID not the env override", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		cfg.ICS26ClientID = "client-from-json"
		deployAll(node, cfg)
		// Every getClient reverts, so whichever id is asked about becomes a finding
		// naming that id -- which is how we see WHICH id was validated.
		node.FailCall(errors.New("execution reverted"))
		t.Setenv("ICS26_CLIENT_ID", "stale-from-dotenv")

		findings := validateEVMDeployments(context.Background(),
			&appConfig{HasCosmosToL2: true, CosmosToL2Config: cfg})

		if len(findings) == 0 {
			t.Fatal("no finding at all; the cosmos_to_l2 entry was not validated")
		}
		for _, f := range findings {
			if strings.Contains(f.detail, "stale-from-dotenv") {
				t.Fatalf("validated the env override on a cosmos_to_l2 entry: %s\n"+
					"buildCosmosToL2Dest never reads it, so this rejects a config the relay "+
					"would have run fine", f.detail)
			}
		}
		for _, f := range findings {
			if strings.Contains(f.detail, "client-from-json") {
				return
			}
		}
		t.Fatalf("no finding names the JSON client id, so nothing proves which id was checked: %+v", findings)
	})

	t.Run("accepts a fully deployed module", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		deployAll(node, cfg)

		if findings := validateEVMDeployments(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		}); len(findings) != 0 {
			t.Fatalf("expected no findings for a deployed module, got %+v", findings)
		}
	})

	t.Run("reports a client ID the router does not know", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		deployAll(node, cfg)
		// getClient reverts with IBCClientNotFound for an unregistered id
		// (contracts/core/client-registry/ICS02ClientUpgradeable.sol:78-80).
		node.FailCall(errors.New("execution reverted"))

		findings := validateEVMDeployments(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		})

		if len(findings) != 1 || findings[0].key != "cosmos_to_eth.ics26_client_id" {
			t.Fatalf("expected one ics26_client_id finding, got %+v", findings)
		}
		if !strings.Contains(findings[0].detail, "client-0") {
			t.Fatalf("finding %q does not name the client id", findings[0].detail)
		}
	})

	t.Run("tolerates a transport error from the router call", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		deployAll(node, cfg)
		// Not a revert. The node did not execute the call, so this says nothing
		// about the client id and must not stop startup.
		node.FailCall(errors.New("connection reset by peer"))

		if findings := validateEVMDeployments(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		}); len(findings) != 0 {
			t.Fatalf("a transport error must not fail startup, got %+v", findings)
		}
	})

	t.Run("does not report the client ID when the router itself is missing", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := cosmosToEthConfig{
			EthRpcUrl:     node.URL(),
			ICS26Address:  "0x1111111111111111111111111111111111111111",
			ICS26ClientID: "client-0",
		}
		// Nothing deployed, and getClient would revert. "The router has no code" and
		// "the router does not know this client" are then the same fact, and
		// reporting both sends an operator looking for a second problem.
		node.FailCall(errors.New("execution reverted"))

		findings := validateEVMDeployments(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		})

		if len(findings) != 1 || findings[0].key != "cosmos_to_eth.ics26_address" {
			t.Fatalf("expected only the missing-router finding, got %+v", findings)
		}
	})

	t.Run("tolerates an unreachable endpoint", func(t *testing.T) {
		cfg := cosmosToEthConfig{
			// Reserved TEST-NET-1 address (RFC 5737): routable syntax, no listener.
			EthRpcUrl:     "http://192.0.2.1:8545",
			ICS26Address:  "0x1111111111111111111111111111111111111111",
			ICS26ClientID: "client-0",
		}
		ctx, cancel := context.WithTimeout(context.Background(), configProbeTimeout)
		defer cancel()

		if findings := validateEVMDeployments(ctx, &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		}); len(findings) != 0 {
			t.Fatalf("an unreachable endpoint must not fail startup, got %+v", findings)
		}
	})

	t.Run("covers the L2 return leg too", func(t *testing.T) {
		// The mirror of TestValidateEVMDeployments/reports every address that holds no code. An
		// l2_to_cosmos module reaches a router and a client id just like the outbound
		// legs; it only names them differently, and checking one direction while
		// leaving the other exposed is the asymmetry this repo keeps rediscovering.
		node := rpcmock.NewEVMNode(t)
		router := "0x7777777777777777777777777777777777777777"
		cfg := &appConfig{
			HasL2ToCosmos: true,
			L2ToCosmosConfig: l2ToCosmosConfig{
				L2RpcUrl:        node.URL(),
				L2ICS26ClientID: "l2-client-0",
				RollupProfile: json.RawMessage(
					`{"common":{"l2_chain_id":1,"l2_router":"` + router + `"}}`),
			},
		}

		// Undeployed router AND a reverting getClient. Both facts have the same
		// cause, so only the router may be reported — and the dedupe that ensures
		// it has to match on the L2 leg's own key name, not the outbound one.
		node.FailCall(jsonRPCError{msg: "execution reverted", data: "0x"})
		findings := validateEVMDeployments(context.Background(), cfg)
		if len(findings) != 1 || findings[0].key != "l2_to_cosmos.rollup_profile.common.l2_router" {
			t.Fatalf("expected only the undeployed L2 router to be reported, got %+v", findings)
		}
		node.FailCall(nil)

		// Deployed: the router now resolves and there is nothing to report.
		node.SetCode(common.HexToAddress(router), deployedCode)
		node.SetCallResult("0x000000000000000000000000" + strings.Repeat("ab", 20))
		if findings := validateEVMDeployments(context.Background(), cfg); len(findings) != 0 {
			t.Fatalf("expected no findings once the L2 router is deployed, got %+v", findings)
		}

		// And an unregistered client id on that router is reported against the L2
		// key, not the outbound one.
		node.FailCall(jsonRPCError{msg: "execution reverted", data: "0x"})
		findings = validateEVMDeployments(context.Background(), cfg)
		if len(findings) != 1 || findings[0].key != "l2_to_cosmos.l2_ics26_client_id" {
			t.Fatalf("expected an l2_ics26_client_id finding, got %+v", findings)
		}
	})

	// Reported by @DongLieu on #467. getClient succeeding proves only that the
	// router knows SOME client under that id. It says nothing about whether that
	// client is the spectre_client this config deploys against, nor whether its
	// counterparty is the Cosmos client this config relays to. A wrong-but-
	// registered id passes startup, and then every update and proof targets one
	// client while the router verifies another.
	//
	// routerWiringIsReusable already decides this and is already tested; only the
	// RPC to feed it was missing here. Per-selector call results are what let the
	// stub answer getClient and getCounterparty differently, which SetCallResult
	// alone could not express.
	t.Run("rejects a router wired to another client", func(t *testing.T) {
		const (
			getClientSelector       = "7eb78932"
			getCounterpartySelector = "b0777bfa"
		)
		abiAddress := func(hexAddr string) string {
			return "0x000000000000000000000000" + strings.TrimPrefix(hexAddr, "0x")
		}

		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		deployAll(node, cfg)
		// The counterparty the router reports, ABI-encoded as (string,bytes[]): a
		// dynamic head, then the Cosmos client id.
		node.SetCallResultFor(getCounterpartySelector, abiCounterparty("08-wasm-0"))

		t.Run("the client id points at a different light client", func(t *testing.T) {
			node.SetCallResultFor(getClientSelector, abiAddress("0x9999999999999999999999999999999999999999"))
			local := cfg
			local.CosmosWasmClientID = "08-wasm-0"
			findings := validateEVMDeployments(context.Background(), &appConfig{HasCosmosToEth: true, CosmosToEthConfig: local})
			if len(findings) == 0 {
				t.Fatal("a router pointing that client id at another light client was accepted; " +
					"updates would target spectre_client while the router verifies something else")
			}
		})

		t.Run("the counterparty is a different Cosmos client", func(t *testing.T) {
			node.SetCallResultFor(getClientSelector, abiAddress(cfg.SpectreClient))
			local := cfg
			local.CosmosWasmClientID = "08-wasm-7" // the router says 08-wasm-0
			findings := validateEVMDeployments(context.Background(), &appConfig{HasCosmosToEth: true, CosmosToEthConfig: local})
			if len(findings) == 0 {
				t.Fatal("a router counterparty-wired to another Cosmos client was accepted; " +
					"recvPacket would reject every packet's counterparty")
			}
		})

		t.Run("correctly wired passes", func(t *testing.T) {
			node.SetCallResultFor(getClientSelector, abiAddress(cfg.SpectreClient))
			local := cfg
			local.CosmosWasmClientID = "08-wasm-0"
			if findings := validateEVMDeployments(context.Background(), &appConfig{HasCosmosToEth: true, CosmosToEthConfig: local}); len(findings) != 0 {
				t.Fatalf("a correctly wired router was rejected: %v", findings)
			}
		})
	})

	// Reported by @DongLieu on #467. build_source.go:102 reads ICS26_CLIENT_ID over
	// the config, so validating the raw config value proves something startup then
	// does not use: a stale id in .env passes here and fails at the first packet,
	// which is exactly the failure this validation exists to move forward.
	t.Run("validates the ID startup will actually use", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(node)
		deployAll(node, cfg)
		// The router knows only the id in the config file: an id it does not know
		// reverts, which is how getClient reports IBCClientNotFound.
		node.FailCall(errors.New("execution reverted"))

		t.Setenv("ICS26_CLIENT_ID", "stale-from-dotenv")
		findings := validateEVMDeployments(context.Background(), &appConfig{HasCosmosToEth: true, CosmosToEthConfig: cfg})
		if len(findings) == 0 {
			t.Fatal("the override was ignored and the config value validated instead; a stale " +
				"ICS26_CLIENT_ID in .env would pass startup and fail at the first packet")
		}
		for _, f := range findings {
			if strings.Contains(f.detail, "stale-from-dotenv") {
				return
			}
		}
		t.Fatalf("no finding names the effective client id: %v", findings)
	})
}

func TestEvmDeploymentsInConfig_SkipUnsetAndMalformedAddresses(t *testing.T) {
	deployments := evmDeploymentsInConfig(&appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{
			EthRpcUrl:     "http://eth",
			ICS26Address:  "0x1111111111111111111111111111111111111111",
			SpectreClient: "",            // unset
			Membership:    "not-an-addr", // malformed
		},
		HasCosmosToL2: true, CosmosToL2Config: cosmosToEthConfig{
			EthRpcUrl:    "http://l2",
			ICS26Address: "0x2222222222222222222222222222222222222222",
		},
	})

	if len(deployments) != 2 {
		t.Fatalf("expected one deployment per EVM endpoint, got %d", len(deployments))
	}
	if len(deployments[0].addresses) != 1 {
		t.Fatalf("unset/malformed addresses must not be probed, got %v", deployments[0].addresses)
	}
	if deployments[0].label != "cosmos_to_eth" || deployments[1].label != "cosmos_to_l2" {
		t.Fatalf("labels are %q and %q; an operator greps the config by these",
			deployments[0].label, deployments[1].label)
	}
}

func TestEvmDeploymentsInConfig_SkipAModuleWithNoRPC(t *testing.T) {
	if d := evmDeploymentsInConfig(&appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{ICS26Address: "0x1111111111111111111111111111111111111111"},
	}); len(d) != 0 {
		t.Fatalf("a module with no eth_rpc_url has nothing to probe, got %+v", d)
	}
}

// --- revert classification ---------------------------------------------------

func TestIsEVMRevert_SeparatesExecutionFromTransport(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"geth revert text", errors.New("execution reverted"), true},
		{"revert text with a reason", errors.New("execution reverted: IBCClientNotFound"), true},
		{"mixed case", errors.New("Execution Reverted"), true},
		{"wrapped revert", fmt.Errorf("call getClient: %w", errors.New("execution reverted")), true},
		{"dial failure", errors.New("dial tcp 127.0.0.1:8545: connect: connection refused"), false},
		{"timeout", context.DeadlineExceeded, false},
		{"cancelled", context.Canceled, false},

		// The rpc.DataError branch. go-ethereum wraps EVERY JSON-RPC error in a
		// type satisfying it, so the interface alone cannot separate a revert
		// from a node complaint — only the presence of data can.
		{"revert with data", jsonRPCError{msg: "execution reverted", data: "0x08c379a0"}, true},
		{"pruned node", jsonRPCError{msg: "header not found"}, false},
		{"rate limited", jsonRPCError{msg: "your app has exceeded its compute units"}, false},

		// Data alone is not a revert signal: JSON-RPC lets a server put any
		// detail there. Only revert-shaped data, or code 3, means the EVM ran.
		{"code 3 without revert text", jsonRPCError{code: 3, msg: "VM Exception", data: "0x"}, true},
		{"nethermind revert data", jsonRPCError{code: -32015, msg: "VM execution error.", data: "Reverted 0x08c379a0"}, true},
		{"rate limit with structured data", jsonRPCError{code: -32005, msg: "limit exceeded", data: map[string]any{"retryAfter": 1}}, false},
		{"node error with a string detail", jsonRPCError{code: -32000, msg: "header not found", data: "pruned at 1200"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := services.IsEVMRevert(tc.err); got != tc.want {
				t.Fatalf("IsEVMRevert(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestValidateStartupConfig(t *testing.T) {
	t.Run("reports every problem in a single message", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("RELAYER_RECOVERY_STATE_FILE", filepath.Join(dir, "recovery-cursors.json"))
		t.Setenv("COSMOS_CHAIN_ID", "declared-chain")

		cosmos := rpcmock.NewCosmosNode(t)
		cosmos.Handle("status", func(json.RawMessage) (any, error) {
			return statusResult("served-chain"), nil
		})
		eth := rpcmock.NewEVMNode(t)
		cfg := evmConfigFor(eth)
		cfg.TmRpcUrl = cosmos.URL()

		err := validateStartupConfig(context.Background(), &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cfg,
		}, "")
		if err == nil {
			t.Fatal("expected config validation to fail")
		}

		// One run must diagnose the whole config. Failing on the first problem turns
		// a three-key mistake into three restarts.
		msg := err.Error()
		for _, want := range []string{
			"COSMOS_CHAIN_ID", "served-chain",
			"cosmos_to_eth.ics26_address", "cosmos_to_eth.update_client",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message does not mention %q:\n%s", want, msg)
			}
		}
	})

	t.Run("passes on a config with nothing to check", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("RELAYER_RECOVERY_STATE_FILE", filepath.Join(dir, "recovery-cursors.json"))

		if err := validateStartupConfig(context.Background(), &appConfig{}, ""); err != nil {
			t.Fatalf("an empty config has nothing to disagree with: %v", err)
		}
	})

	// Over a real connection: the finding reaches validateStartupConfig's single
	// message, which is where an operator reads it.
	t.Run("reports an attestor head mismatch", func(t *testing.T) {
		addr := serveInfo(t, &attestorpb.ChainInfo{SrcChain: "opstack", AttestationHead: "finalized"})
		captureStartupLog(t)

		err := validateStartupConfig(context.Background(), l2ConfigWithAttestor("opstack", "unsafe", addr), "")
		if err == nil {
			t.Fatal("expected config validation to fail")
		}
		for _, want := range []string{"attestor_endpoints[0].address / head_kind", `"unsafe"`, `"finalized"`, addr} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("message does not mention %q:\n%s", want, err)
			}
		}
	})
}

func TestSortedKeys_IsStable(t *testing.T) {
	got := sortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	if fmt.Sprint(got) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("sortedKeys returned %v", got)
	}
}

// deployedCode is any non-empty code answer; the check only asks whether there
// is code at all, never what it is.
const deployedCode = "0x60006000"

func evmConfigFor(node *rpcmock.EVMNode) cosmosToEthConfig {
	return cosmosToEthConfig{
		EthRpcUrl:         node.URL(),
		ICS26Address:      "0x1111111111111111111111111111111111111111",
		ICS26ClientID:     "client-0",
		SpectreClient:     "0x2222222222222222222222222222222222222222",
		SignatureVerifier: "0x3333333333333333333333333333333333333333",
		Membership:        "0x4444444444444444444444444444444444444444",
		Misbehaviour:      "0x5555555555555555555555555555555555555555",
		UpdateClient:      "0x6666666666666666666666666666666666666666",
	}
}

func deployAll(node *rpcmock.EVMNode, c cosmosToEthConfig) {
	for _, a := range []string{
		c.ICS26Address, c.SpectreClient, c.SignatureVerifier,
		c.Membership, c.Misbehaviour, c.UpdateClient,
	} {
		node.SetCode(common.HexToAddress(a), deployedCode)
	}
	// getClient's return value: one ABI-encoded non-zero address.
	node.SetCallResult("0x000000000000000000000000" + strings.Repeat("ab", 20))
}

// jsonRPCError mirrors go-ethereum's jsonError: it satisfies rpc.Error and
// rpc.DataError for every node-returned error, whatever its data holds.
type jsonRPCError struct {
	code int // 0 means -32000, the generic server error
	msg  string
	data any
}

func (e jsonRPCError) Error() string { return e.msg }

func (e jsonRPCError) ErrorData() any { return e.data }

func (e jsonRPCError) ErrorCode() int {
	if e.code == 0 {
		return -32000
	}
	return e.code
}

// statusResult is the minimal /status reply: validation reads only the
// network name, so the stub models only that.
func statusResult(network string) *coretypes.ResultStatus {
	return &coretypes.ResultStatus{
		NodeInfo: cmtp2p.DefaultNodeInfo{Network: network},
	}
}

// abiCounterparty encodes what getCounterparty returns: a single dynamic STRUCT
// {string clientId; bytes[] merklePrefix}. A dynamic struct return is itself
// referenced by an offset, so the head is one word pointing at the tuple, and
// only then do the tuple's own two offsets begin. Getting that outer word wrong
// is why the first version of this test could not decode.
func abiCounterparty(clientID string) string {
	word := func(v int) string { return fmt.Sprintf("%064x", v) }
	padded := hex.EncodeToString([]byte(clientID))
	for len(padded)%64 != 0 {
		padded += "0"
	}
	tuple := word(0x40) + // clientId offset, relative to the tuple
		word(0x60+len(padded)/2) + // merklePrefix offset, past the string
		word(len(clientID)) + padded +
		word(0) // empty bytes[]
	return "0x" + word(0x20) + tuple
}

func TestValidateStartupKeys_RejectsInvalidCosmosAddressPrefix(t *testing.T) {
	const privateKey = "a81f9eb900c02f35d28ec80d1da79dde52cc4cc37d6fbaef5768ab59f32cbfdd"
	t.Setenv("ETH_PRIVATE_KEY", privateKey)
	t.Setenv("COSMOS_PRIVATE_KEY", privateKey)
	t.Setenv("COSMOS_ADDRESS_PREFIX", "invalid prefix")

	if err := validateStartupKeys(); err == nil {
		t.Fatal("startup accepted a Cosmos signer address with an invalid bech32 prefix")
	}
}

// An eth_to_cosmos module without a websocket used to start and then run half-dead:
// the forward direction relayed, SubscribeEth bailed out after one log line, and no
// acknowledgement was ever picked up. Refuse to START on that config.
func TestValidateRelayStartupConfig_RequiresEthWs(t *testing.T) {
	const cfg = `{
		"modules": [
			{"name": "cosmos_to_eth", "src_chain": "cosmos", "dst_chain": "ethereum",
			 "config": {"tm_rpc_url": "http://localhost:26657", "eth_rpc_url": "http://localhost:8545",
			            "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "chain-a"}},
			{"name": "eth_to_cosmos", "src_chain": "ethereum", "dst_chain": "cosmos",
			 "config": {"eth_beacon_api_url": "http://localhost:5052"}}
		]
	}`
	appCfg, err := loadConfig(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("loadConfig must accept this — create-clients-* never read eth_ws_url: %v", err)
	}
	err = validateRelayStartupConfig(appCfg)
	if err == nil {
		t.Fatal("start accepted eth_to_cosmos without eth_ws_url; the ETH→Cosmos direction cannot run")
	}
	if !strings.Contains(err.Error(), "eth_ws_url") {
		t.Fatalf("error should name the missing field, got: %v", err)
	}
}
