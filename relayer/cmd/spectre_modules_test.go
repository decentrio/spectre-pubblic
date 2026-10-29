package main

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"relayer/rpcmock"
	"relayer/services"
)

// The real client must keep satisfying the seam, or the check stops compiling
// where it is used.
var _ codeReader = (*ethclient.Client)(nil)

var (
	updateClientAddr = common.HexToAddress("0x6666666666666666666666666666666666666666")
	membershipAddr   = common.HexToAddress("0x4444444444444444444444444444444444444444")
	misbehaviourAddr = common.HexToAddress("0x5555555555555555555555555555555555555555")
)

// fakeCodeReader answers eth_getCode from a table and records what it was asked.
type fakeCodeReader struct {
	code   map[common.Address][]byte
	errs   map[common.Address]error
	asked  []common.Address
	blocks []*big.Int
}

func (f *fakeCodeReader) CodeAt(_ context.Context, account common.Address, block *big.Int) ([]byte, error) {
	f.asked = append(f.asked, account)
	f.blocks = append(f.blocks, block)
	if err := f.errs[account]; err != nil {
		return nil, err
	}
	return f.code[account], nil
}

func deployedModules() *fakeCodeReader {
	return &fakeCodeReader{code: map[common.Address][]byte{
		updateClientAddr: {0x60}, membershipAddr: {0x60}, misbehaviourAddr: {0x60},
	}}
}

func threeModules() []spectreModule {
	return spectreModulesOf("cosmos_to_eth", services.EVMContracts{
		UpdateClient: updateClientAddr, Membership: membershipAddr, Misbehaviour: misbehaviourAddr,
	})
}

func TestRequireSpectreModulesDeployed_AcceptsModulesThatHoldCode(t *testing.T) {
	reader := deployedModules()

	if err := requireSpectreModulesDeployed(context.Background(), reader, "http://rpc", threeModules()); err != nil {
		t.Fatalf("rejected three deployed modules: %v", err)
	}

	// Asked about each module once, at the latest block: a historical block could
	// predate the deployment and report a module that exists as missing.
	want := []common.Address{updateClientAddr, membershipAddr, misbehaviourAddr}
	if len(reader.asked) != len(want) {
		t.Fatalf("asked about %v, want %v", reader.asked, want)
	}
	for i := range want {
		if reader.asked[i] != want[i] {
			t.Fatalf("asked about %v, want %v", reader.asked, want)
		}
		if reader.blocks[i] != nil {
			t.Fatalf("read the code at block %v, want the latest block (nil)", reader.blocks[i])
		}
	}
}

// An empty config field reaches the SpectreClient constructor as address(0)
// (common.HexToAddress("") is the zero address), which is how #568 happens.
func TestRequireSpectreModulesDeployed_RejectsAnUnsetModule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unset func(*services.EVMContracts)
		key   string
	}{
		{"update_client", func(c *services.EVMContracts) { c.UpdateClient = common.Address{} }, "cosmos_to_eth.update_client"},
		{"membership", func(c *services.EVMContracts) { c.Membership = common.Address{} }, "cosmos_to_eth.membership"},
		{"misbehaviour", func(c *services.EVMContracts) { c.Misbehaviour = common.Address{} }, "cosmos_to_eth.misbehaviour"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contracts := services.EVMContracts{UpdateClient: updateClientAddr, Membership: membershipAddr, Misbehaviour: misbehaviourAddr}
			tc.unset(&contracts)
			reader := deployedModules()

			err := requireSpectreModulesDeployed(context.Background(), reader, "http://rpc",
				spectreModulesOf("cosmos_to_eth", contracts))

			if err == nil {
				t.Fatalf("%s is the zero address and the check passed", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key+" is not set") {
				t.Fatalf("error does not name %s as unset: %v", tc.key, err)
			}
			for _, asked := range reader.asked {
				if asked == (common.Address{}) {
					t.Fatalf("asked the node about the zero address; the answer would hide the real cause")
				}
			}
		})
	}
}

func TestRequireSpectreModulesDeployed_RejectsAModuleWithoutCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr common.Address
		key  string
	}{
		{"update_client", updateClientAddr, "cosmos_to_eth.update_client"},
		{"membership", membershipAddr, "cosmos_to_eth.membership"},
		{"misbehaviour", misbehaviourAddr, "cosmos_to_eth.misbehaviour"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := deployedModules()
			delete(reader.code, tc.addr) // an address nobody deployed to answers with no code

			err := requireSpectreModulesDeployed(context.Background(), reader, "http://rpc", threeModules())

			if err == nil {
				t.Fatalf("%s holds no code and the check passed", tc.key)
			}
			// What an operator needs to act: the key, the address, and the chain it was read from.
			for _, want := range []string{tc.key, tc.addr.Hex(), "holds no code", "http://rpc"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error does not mention %q: %v", want, err)
				}
			}
		})
	}
}

func TestRequireSpectreModulesDeployed_ReportsEveryProblemAtOnce(t *testing.T) {
	boom := errors.New("connection refused")
	reader := deployedModules()
	delete(reader.code, membershipAddr)
	reader.errs = map[common.Address]error{misbehaviourAddr: boom}
	modules := spectreModulesOf("cosmos_to_eth", services.EVMContracts{
		UpdateClient: common.Address{}, Membership: membershipAddr, Misbehaviour: misbehaviourAddr,
	})

	err := requireSpectreModulesDeployed(context.Background(), reader, "http://rpc", modules)

	if err == nil {
		t.Fatal("three problems and the check passed")
	}
	for _, key := range []string{"cosmos_to_eth.update_client", "cosmos_to_eth.membership", "cosmos_to_eth.misbehaviour"} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error does not mention %s; it stopped at the first problem: %v", key, err)
		}
	}
}

// A node that does not answer is not a node that says the module is there.
func TestRequireSpectreModulesDeployed_FailsClosedWhenTheNodeDoesNotAnswer(t *testing.T) {
	boom := errors.New("connection refused")
	reader := deployedModules()
	reader.errs = map[common.Address]error{misbehaviourAddr: boom}

	err := requireSpectreModulesDeployed(context.Background(), reader, "http://rpc", threeModules())

	if err == nil {
		t.Fatal("the check passed although the code of a module could not be read")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the underlying cause is lost, so a caller cannot classify it: %v", err)
	}
	if !strings.Contains(err.Error(), "cosmos_to_eth.misbehaviour") {
		t.Fatalf("error does not name the module whose code could not be read: %v", err)
	}
}

func TestSpectreModulesOf_FollowsTheConstructorOrder(t *testing.T) {
	// SpectreClient(updateClientModule, membershipModule, misbehaviourModule, ...)
	got := spectreModulesOf("cosmos_to_l2", services.EVMContracts{
		UpdateClient: updateClientAddr, Membership: membershipAddr, Misbehaviour: misbehaviourAddr,
		// Never part of the check: it is not a SpectreClient module.
		SignatureVerifier: common.HexToAddress("0x3333333333333333333333333333333333333333"),
	})

	want := []spectreModule{
		{"cosmos_to_l2.update_client", updateClientAddr},
		{"cosmos_to_l2.membership", membershipAddr},
		{"cosmos_to_l2.misbehaviour", misbehaviourAddr},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d modules, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("module %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// selectCosmosSource promotes a cosmos_to_l2 module into CosmosToEthConfig when
// the file has no cosmos_to_eth one, so the label has to follow the flag and not
// the field it was promoted into.
func TestSpectreSourceLabel(t *testing.T) {
	if got := spectreSourceLabel(&appConfig{HasCosmosToEth: true}); got != "cosmos_to_eth" {
		t.Fatalf("label = %q for a cosmos_to_eth source", got)
	}
	if got := spectreSourceLabel(selectCosmosSource(&appConfig{HasCosmosToL2: true})); got != "cosmos_to_l2" {
		t.Fatalf("label = %q for a promoted cosmos_to_l2 source", got)
	}
}

func TestCheckSpectreModulesBeforeDeploy_AgainstANode(t *testing.T) {
	node := rpcmock.NewEVMNode(t)
	cfg := &appConfig{HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: node.URL()}}
	evm := services.EVMEndpoint{
		Client: node.Client(t),
		Contracts: services.EVMContracts{
			UpdateClient: updateClientAddr, Membership: membershipAddr, Misbehaviour: misbehaviourAddr,
		},
	}

	t.Run("refuses while a module has no code", func(t *testing.T) {
		// A node answers "0x" for an address nobody deployed to; only two are deployed here.
		node.SetCode(updateClientAddr, deployedCode)
		node.SetCode(membershipAddr, deployedCode)

		err := checkSpectreModulesBeforeDeploy(context.Background(), cfg, evm)

		if err == nil {
			t.Fatal("the check passed with no misbehaviour module deployed")
		}
		for _, want := range []string{"cosmos_to_eth.misbehaviour", misbehaviourAddr.Hex(), node.URL()} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error does not mention %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "cosmos_to_eth.membership") || strings.Contains(err.Error(), "cosmos_to_eth.update_client") {
			t.Fatalf("blamed a module that is deployed: %v", err)
		}
	})

	t.Run("accepts once all three are deployed", func(t *testing.T) {
		node.SetCode(misbehaviourAddr, deployedCode)

		if err := checkSpectreModulesBeforeDeploy(context.Background(), cfg, evm); err != nil {
			t.Fatalf("rejected three deployed modules: %v", err)
		}
	})
}
