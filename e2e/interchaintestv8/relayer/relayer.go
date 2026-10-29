package relayer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/cosmos/gogoproto/proto"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"

	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"

	ethereumtypes "github.com/srdtrk/solidity-ibc-eureka/e2e/v8/types/ethereum"
)

// DefaultRelayerGRPCAddress returns the default gRPC address for the relayer.
func DefaultRelayerGRPCAddress() string {
	return "127.0.0.1:3000"
}

// binaryPath returns the path to the relayer binary.
//
// Resolution order:
//  1. $RELAYER_BINARY (absolute path, used by the justfile test-e2e target)
//  2. `relayer` on $PATH (works after `just install-go-relayer`)
//
// Panics with a clear hint if neither is available — tests cannot proceed without the binary.
func binaryPath() string {
	if p := os.Getenv("RELAYER_BINARY"); p != "" {
		return p
	}
	if p, err := exec.LookPath("relayer"); err == nil {
		return p
	}
	panic("relayer binary not found: set $RELAYER_BINARY or run `just install-go-relayer`")
}

// proverEnv exposes the prover's address to the spawned relayer process.
//
// The relayer no longer proves in-process: it calls the spectre-prover service (proverd) over gRPC
// (relayer/cmd/prover.go, dialProver). proverd loads the per-bucket r1cs/pk/vk artifacts itself;
// `just build-prover-artifacts` populates them and `scripts/local/run_prover.sh --` starts proverd
// around the test run and sets PROVER_ADDR, which is why running the suite outside `just test-e2e`
// needs a proverd you started yourself.
func proverEnv() []string {
	addr := os.Getenv("PROVER_ADDR")
	if addr == "" {
		panic("PROVER_ADDR not set: start proverd (scripts/local/run_prover.sh) and point PROVER_ADDR at it, or run through `just test-e2e`")
	}
	return []string{"PROVER_ADDR=" + addr}
}

// StartRelayer starts the relayer with the given config file.
func StartRelayer(configPath string) (*os.Process, error) {
	config, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Starting relayer with config:\n%s\n", config)

	cmd := exec.Command(binaryPath(), "start", "--config", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), proverEnv()...)

	// run this command in the background
	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()

	// Wait long enough for the relayer to reach the prover and set up both Cosmos and
	// Ethereum event subscriptions. If the test sends a SendPacket tx before WatchSendPacket
	// is active, the event is missed (Watch defaults to fromBlock=latest).
	select {
	case err := <-exited:
		if err != nil {
			return nil, fmt.Errorf("relayer exited during startup: %w", err)
		}
		return nil, errors.New("relayer exited during startup")
	case <-time.After(60 * time.Second):
	}

	return cmd.Process, nil
}

// RunCreateClients brings up both light clients, one relayer command per chain (the umbrella
// create-clients command was removed): create-clients-cosmos first (creates the wasm Ethereum client
// on Cosmos and writes its client id into the config), then create-clients-eth (deploys SpectreClient
// wired to that id). An empty wasmChecksum skips the Cosmos half (PoW mode: no beacon API, so no
// Ethereum light client on Cosmos).
func RunCreateClients(configPath, wasmChecksum, trustLevel string) error {
	if wasmChecksum != "" {
		if err := RunCreateClientsCosmos(configPath, "--wasm-checksum", wasmChecksum); err != nil {
			return fmt.Errorf("create-clients-cosmos: %w", err)
		}
	}
	if err := RunCreateClientsEth(configPath, "--trust-level", trustLevel); err != nil {
		return fmt.Errorf("create-clients-eth: %w", err)
	}
	return nil
}

// RunCreateClientsCosmos runs the relayer's create-clients-cosmos command synchronously.
func RunCreateClientsCosmos(configPath string, extraArgs ...string) error {
	config, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("Running create-clients-cosmos with config:\n%s\n", config)

	args := append([]string{"create-clients-cosmos", "--config", configPath}, extraArgs...)
	cmd := exec.Command(binaryPath(), args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), proverEnv()...)

	return cmd.Run()
}

// RunCreateClientsEth runs the relayer's create-clients-eth command synchronously.
func RunCreateClientsEth(configPath string, extraArgs ...string) error {
	config, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("Running create-clients-eth with config:\n%s\n", config)

	args := append([]string{"create-clients-eth", "--config", configPath}, extraArgs...)
	cmd := exec.Command(binaryPath(), args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), proverEnv()...)

	return cmd.Run()
}

// RunUpdateClient runs the relayer's update-client command synchronously.
func RunUpdateClient(configPath string, extraArgs ...string) error {
	config, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("Running update-client with config:\n%s\n", config)

	args := append([]string{"update-client", "--config", configPath}, extraArgs...)
	cmd := exec.Command(binaryPath(), args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), proverEnv()...)

	return cmd.Run()
}

// GetRelayUpdateSlot extracts the latest update slot from the relay body's update messages.
func GetRelayUpdateSlotForWasmClient(relayBody []byte) (uint64, error) {
	var txBody txtypes.TxBody
	err := proto.Unmarshal(relayBody, &txBody)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal relay body: %w", err)
	}

	var updateClientMsgsAny []*codectypes.Any
	for _, msg := range txBody.Messages {
		if msg.TypeUrl == "/ibc.core.client.v1.MsgUpdateClient" {
			updateClientMsgsAny = append(updateClientMsgsAny, msg)
		}
	}
	if len(updateClientMsgsAny) == 0 {
		return 0, errors.New("no update client messages found in relay body")
	}

	var headers []ethereumtypes.Header
	for _, updateClientMsgAny := range updateClientMsgsAny {
		var updateClientMsg clienttypes.MsgUpdateClient
		err = proto.Unmarshal(updateClientMsgAny.Value, &updateClientMsg)
		if err != nil {
			return 0, fmt.Errorf("failed to unmarshal MsgUpdateClient: %w", err)
		}
		var clientMessage ibcwasmtypes.ClientMessage
		err = proto.Unmarshal(updateClientMsg.ClientMessage.Value, &clientMessage)
		if err != nil {
			return 0, fmt.Errorf("failed to unmarshal ClientMessage: %w", err)
		}

		var header ethereumtypes.Header
		err = json.Unmarshal(clientMessage.Data, &header)
		if err != nil {
			return 0, fmt.Errorf("failed to unmarshal header: %w", err)
		}

		headers = append(headers, header)
	}

	latestUpdateSlot := uint64(0)
	for _, header := range headers {
		updateSlot, err := strconv.ParseUint(header.ConsensusUpdate.FinalizedHeader.Beacon.Slot, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse slot from header: %w", err)
		}
		latestUpdateSlot = max(latestUpdateSlot, updateSlot)
	}

	return latestUpdateSlot, nil
}
