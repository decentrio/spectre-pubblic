package main

import (
	"bytes"
	"os"
	"path/filepath"
	contractICS26Router "relayer/bindings/ICS26Router"
	spectreContract "relayer/bindings/SpectreClient"
	"relayer/services"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"go.uber.org/zap"
)

func TestLoadMisbehaviourEvidence_StandardJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	data := []byte(`{
  "client_id": "client-0",
  "header_1": {"trusted_height": {"revision_number": "1", "revision_height": "10"}},
  "header_2": {"trusted_height": {"revision_number": "1", "revision_height": "11"}}
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	evidence, err := loadMisbehaviourEvidence(path)
	if err != nil {
		t.Fatalf("load evidence: %v", err)
	}
	if evidence.ClientId != "client-0" {
		t.Fatalf("client ID = %q, want client-0", evidence.ClientId)
	}
	if evidence.Header1 == nil || evidence.Header1.TrustedHeight.RevisionHeight != 10 {
		t.Fatalf("header1 trusted height = %+v, want 10", evidence.Header1)
	}
	if evidence.Header2 == nil || evidence.Header2.TrustedHeight.RevisionHeight != 11 {
		t.Fatalf("header2 trusted height = %+v, want 11", evidence.Header2)
	}
}

func TestSubmitMisbehaviour(t *testing.T) {
	t.Run("dry run rejects invalid gas limit before loading config", func(t *testing.T) {
		t.Setenv("ETH_MISBEHAVIOUR_GAS_LIMIT", "invalid")

		cmd := SubmitMisbehaviour(zap.NewNop())
		cmd.SetArgs([]string{"--dry-run", "--evidence", "missing.json", "--config", "missing.json"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "invalid ETH_MISBEHAVIOUR_GAS_LIMIT") {
			t.Fatalf("Execute() error = %v, want invalid gas limit", err)
		}
	})

	t.Run("rejects invalid private key before loading config", func(t *testing.T) {
		t.Setenv("MISBEHAVIOUR_PRIVATE_KEY", "invalid")

		cmd := SubmitMisbehaviour(zap.NewNop())
		cmd.SetArgs([]string{"--evidence", "missing.json", "--config", "missing.json"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "invalid MISBEHAVIOUR_PRIVATE_KEY") {
			t.Fatalf("Execute() error = %v, want invalid private key", err)
		}
	})

	t.Run("dry run does not read private key", func(t *testing.T) {
		t.Setenv("MISBEHAVIOUR_PRIVATE_KEY", "invalid")

		cmd := SubmitMisbehaviour(zap.NewNop())
		cmd.SetArgs([]string{"--dry-run", "--evidence", "missing.json", "--config", "missing.json"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "load config") {
			t.Fatalf("Execute() error = %v, want config error before any key access", err)
		}
	})
}

func TestBuildMisbehaviourCalldata_RouterAndDirect(t *testing.T) {
	routerAddress := common.HexToAddress("0x1000000000000000000000000000000000000001")
	spectreAddress := common.HexToAddress("0x2000000000000000000000000000000000000002")
	prepared := services.PreparedCosmosMisbehaviour{EncodedMessage: []byte{0xaa, 0xbb}}

	routerEndpoint := services.EVMEndpoint{Contracts: services.EVMContracts{
		Router: routerAddress, RoleManager: routerAddress, SpectreClient: spectreAddress,
	}}
	to, method, data, err := buildMisbehaviourCalldata(routerEndpoint, "client-7", prepared)
	if err != nil {
		t.Fatalf("router calldata: %v", err)
	}
	if to != routerAddress || method != "ICS26Router.submitMisbehaviour" {
		t.Fatalf("router target/method = %s/%s", to, method)
	}
	routerABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("router ABI: %v", err)
	}
	if !bytes.Equal(data[:4], routerABI.Methods["submitMisbehaviour"].ID) {
		t.Fatalf("router selector = %x", data[:4])
	}

	directEndpoint := services.EVMEndpoint{Contracts: services.EVMContracts{
		Router: routerAddress, SpectreClient: spectreAddress,
	}}
	to, method, data, err = buildMisbehaviourCalldata(directEndpoint, "client-7", prepared)
	if err != nil {
		t.Fatalf("direct calldata: %v", err)
	}
	if to != spectreAddress || method != "SpectreClient.misbehaviour" {
		t.Fatalf("direct target/method = %s/%s", to, method)
	}
	spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
	if err != nil {
		t.Fatalf("spectre ABI: %v", err)
	}
	if !bytes.Equal(data[:4], spectreABI.Methods["misbehaviour"].ID) {
		t.Fatalf("direct selector = %x", data[:4])
	}
}
