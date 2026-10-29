package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"go.uber.org/zap"

	"relayer/rpcmock"
	"relayer/services"
)

// ethToCosmosPlusL2Config is the shape that makes #310 reachable: an eth_to_cosmos
// module supplies the beacon endpoint, an L2 module supplies the ics26_client_id
// selectCosmosSource promotes into CosmosToEthConfig, and no cosmos_to_eth module
// exists for the write-back to target. Every guard except the write itself passes.
const ethToCosmosPlusL2Config = `{
	"modules": [
		{"name": "eth-to-cosmos", "src_chain": "ethereum", "dst_chain": "cosmos",
		 "config": {"eth_beacon_api_url": "http://127.0.0.1:5052"}},
		{"name": "cosmos-to-arbitrum", "src_chain": "cosmos", "dst_chain": "arbitrum",
		 "config": {"tm_rpc_url": "http://127.0.0.1:26657", "eth_rpc_url": "http://127.0.0.1:8547",
		            "eth_ws_url": "ws://127.0.0.1:8548",
		            "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3",
		            "ics26_client_id": "arb-client-0"}}
	]
}`

func writeConfigFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), configFilePerm); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestCreateClientsCosmos_RefusesAConfigItCannotRecordInto is the #310 regression.
//
// The RPC endpoints point at closed ports on purpose: the precheck must run BEFORE
// buildCreateClientsDeps dials anything, so a failure here has to be the recording
// destination and not a connection. If the check is ever moved below the dial, this
// test reports a dial error instead and fails.
func TestCreateClientsCosmos_RefusesAConfigItCannotRecordInto(t *testing.T) {
	configPath := writeConfigFixture(t, ethToCosmosPlusL2Config)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sel := selectCosmosSource(cfg)
	// Precondition of the regression: this config reaches the spend in every other way.
	if sel.CosmosToEthConfig.ICS26ClientID != "arb-client-0" {
		t.Fatalf("selectCosmosSource picked %q, want the L2 module — fixture no longer reproduces the shape",
			sel.CosmosToEthConfig.ICS26ClientID)
	}
	if resolveBeaconURL(sel) == "" {
		t.Fatal("beacon URL empty — the earlier guard would abort and this fixture proves nothing")
	}

	_, err = runCreateClientsCosmos(zap.NewNop(), sel, configPath)
	if err == nil {
		t.Fatal("runCreateClientsCosmos succeeded; want a refusal before any client is created")
	}
	if !strings.Contains(err.Error(), "could not be recorded") {
		t.Fatalf("error = %v; want the pre-spend refusal (a dial error means the check runs too late)", err)
	}
	if !strings.Contains(err.Error(), "arb-client-0") {
		t.Fatalf("error = %v; want it to name the unresolvable source id", err)
	}
}

const validCosmosToEthConfig = `{
	"modules": [
		{"name": "cosmos-to-eth", "src_chain": "cosmos", "dst_chain": "ethereum",
		 "config": {"tm_rpc_url": "http://127.0.0.1:26657", "eth_rpc_url": "http://127.0.0.1:8545",
		            "eth_ws_url": "ws://127.0.0.1:8546",
		            "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3",
		            "ics26_client_id": "chain-a"}},
		{"name": "eth-to-cosmos", "src_chain": "ethereum", "dst_chain": "cosmos",
		 "config": {"eth_beacon_api_url": "http://127.0.0.1:5052"}}
	]
}`

// TestDeployCosmosClientOnEth_ChecksTheModulesBeforeSpendingAnything is the #568
// regression for create-clients-eth. A module left out of the config reaches the
// SpectreClient constructor as address(0), and a module without code makes the
// client accept whatever it is shown; the deploy has to stop first, before any
// Cosmos work and before a transaction is sent.
//
// spectre_client is left empty on purpose: a configured one that is reusable returns
// early and is never deployed, so it is not judged here.
func TestDeployCosmosClientOnEth_ChecksTheModulesBeforeSpendingAnything(t *testing.T) {
	contracts := services.EVMContracts{
		UpdateClient: updateClientAddr, Membership: membershipAddr, Misbehaviour: misbehaviourAddr,
	}
	run := func(t *testing.T, evmNode *rpcmock.EVMNode, cosmosNode *rpcmock.CosmosNode, c services.EVMContracts) error {
		t.Helper()
		cfg := &appConfig{HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: evmNode.URL()}}
		deps := services.RelayDeps{
			Cosmos: services.CosmosEndpoint{Client: cosmosNode.Client(t)},
			EVM:    services.EVMEndpoint{Client: evmNode.Client(t), Contracts: c},
			IDs:    services.ClientIDs{CosmosOnEVM: "client-0", EVMOnCosmos: "08-wasm-0"},
		}
		// A trusting period is passed so the unbonding-time query is not what fails.
		_, err := deployCosmosClientOnEth(zap.NewNop(), cfg, "", "08-wasm-0", "2/3", 100, deps, deps.Cosmos.Client)
		return err
	}

	t.Run("an unset misbehaviour module", func(t *testing.T) {
		evmNode, cosmosNode := rpcmock.NewEVMNode(t), rpcmock.NewCosmosNode(t)
		evmNode.SetCode(updateClientAddr, deployedCode)
		evmNode.SetCode(membershipAddr, deployedCode)
		unset := contracts
		unset.Misbehaviour = common.HexToAddress("") // what an empty config field becomes

		err := run(t, evmNode, cosmosNode, unset)

		if err == nil || !strings.Contains(err.Error(), "cosmos_to_eth.misbehaviour is not set") {
			t.Fatalf("error = %v; want a refusal naming the unset misbehaviour module", err)
		}
		assertNothingSpent(t, evmNode, cosmosNode)
	})

	t.Run("a misbehaviour module without code", func(t *testing.T) {
		evmNode, cosmosNode := rpcmock.NewEVMNode(t), rpcmock.NewCosmosNode(t)
		evmNode.SetCode(updateClientAddr, deployedCode)
		evmNode.SetCode(membershipAddr, deployedCode) // nothing at misbehaviourAddr

		err := run(t, evmNode, cosmosNode, contracts)

		if err == nil || !strings.Contains(err.Error(), misbehaviourAddr.Hex()+" holds no code") {
			t.Fatalf("error = %v; want a refusal naming the module without code", err)
		}
		assertNothingSpent(t, evmNode, cosmosNode)
	})

	// The control: with every module deployed the check lets the run through, which
	// here means it fails later, on the Cosmos side that the stand-in does not serve.
	t.Run("all three deployed goes on to the Cosmos side", func(t *testing.T) {
		evmNode, cosmosNode := rpcmock.NewEVMNode(t), rpcmock.NewCosmosNode(t)
		for _, a := range []common.Address{updateClientAddr, membershipAddr, misbehaviourAddr} {
			evmNode.SetCode(a, deployedCode)
		}

		err := run(t, evmNode, cosmosNode, contracts)

		if err == nil || !strings.Contains(err.Error(), "failed to get genesis") {
			t.Fatalf("error = %v; want the run to get past the module check and fail fetching the Cosmos genesis", err)
		}
		if strings.Contains(err.Error(), "refusing to deploy") {
			t.Fatalf("refused to deploy modules that are all deployed: %v", err)
		}
	})
}

// assertNothingSpent proves the refusal came first: no transaction reached the
// Ethereum node and the Cosmos node was not asked for anything the deploy needs.
func assertNothingSpent(t *testing.T, evmNode *rpcmock.EVMNode, cosmosNode *rpcmock.CosmosNode) {
	t.Helper()
	if sent := evmNode.SentRawTxs(); len(sent) != 0 {
		t.Fatalf("%d transaction(s) were sent before the modules were checked", len(sent))
	}
	for _, method := range []string{"status", "block", "commit", "validators", "abci_query"} {
		if n := cosmosNode.Calls(method); n != 0 {
			t.Fatalf("the Cosmos node was asked %q %d time(s) before the modules were checked", method, n)
		}
	}
}
