//go:build l2devnet

// The Cosmos -> attestor-L2 timeout lifecycle (#517), driven against a live
// devnet: pause the relayer, let a Cosmos packet expire on the L2, restart the
// relayer on the same config and state directory, and prove the timeout settled.
//
// It is one test for every attestor-backed L2 (OP, Base): the rollup is chosen by
// the config file, not by the test. It is behind a build tag because it needs a
// devnet brought up with the scripts in docs/E2E.md (L2 + attestor + Cosmos +
// gov-stored wasm + L2 contracts + both clients). Run once per rollup:
//
//	L2_TIMEOUT_CONFIG=$PWD/config.json \
//	L2_TIMEOUT_SENDER_KEY=$("$GAIAD" keys export test2 --unarmored-hex --unsafe \
//	    --keyring-backend test --home "$HOME/.gaia" -y) \
//	go test -tags l2devnet -race -count=1 -timeout 30m -run TestL2TimeoutLifecycle -v ./cmd
//
// Set L2_TIMEOUT_LOG_DIR to keep the relayer logs; by default they live in a
// temporary directory and are only dumped (tail) when the test fails.
//
// The sender must be funded and must not be COSMOS_PRIVATE_KEY: the relayer pays
// MsgTimeout fees from that account, which would make the refund unmeasurable.
// No other relayer may be running on this config; the state-directory lock would
// refuse the test's relayer.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"relayer/services"
	"relayer/status"
	"relayer/transaction"

	contractICS26Router "relayer/bindings/ICS26Router"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"

	sdkmath "cosmossdk.io/math"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	transfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

const (
	l2TimeoutTransferAmount = 777
	l2TimeoutPollInterval   = 3 * time.Second
)

func TestL2TimeoutLifecycle(t *testing.T) {
	configPath := os.Getenv("L2_TIMEOUT_CONFIG")
	if configPath == "" {
		t.Skip("L2_TIMEOUT_CONFIG is not set; this test needs a running Cosmos <-> L2 devnet (see the file comment)")
	}
	configPath, err := filepath.Abs(configPath)
	requireNoErr(t, err, "resolve config path")
	configDir := filepath.Dir(configPath)
	// The relayer reads .env from its working directory; the test needs the same
	// COSMOS_CHAIN_ID, address prefix and ETH key. Existing env vars win.
	_ = godotenv.Load(filepath.Join(configDir, ".env"))

	senderKeyHex := strings.TrimPrefix(os.Getenv("L2_TIMEOUT_SENDER_KEY"), "0x")
	if senderKeyHex == "" {
		t.Fatal("L2_TIMEOUT_SENDER_KEY is required: hex secp256k1 key of a funded Cosmos account other than COSMOS_PRIVATE_KEY")
	}
	if strings.EqualFold(senderKeyHex, strings.TrimPrefix(os.Getenv("COSMOS_PRIVATE_KEY"), "0x")) {
		t.Fatal("L2_TIMEOUT_SENDER_KEY must differ from COSMOS_PRIVATE_KEY: the relayer's MsgTimeout fees would hide the refund")
	}
	denom := envOr("L2_TIMEOUT_DENOM", "stake")
	timeoutWindow := envDuration(t, "L2_TIMEOUT_WINDOW", 45*time.Second)
	settleTimeout := envDuration(t, "L2_TIMEOUT_SETTLE", 15*time.Minute)

	cfg, err := loadConfig(configPath)
	requireNoErr(t, err, "load relayer config")
	if !cfg.HasCosmosToL2 || !cfg.HasL2ToCosmos {
		t.Fatalf("%s is not a Cosmos <-> L2 config (cosmos_to_l2=%v l2_to_cosmos=%v)", configPath, cfg.HasCosmosToL2, cfg.HasL2ToCosmos)
	}
	if cfg.StatusAddress == "" {
		t.Fatalf("%s has no status_address: the test reads the relayer's live pending set, because the Cosmos "+
			"commitment disappears before the relayer retires its tracker entry", configPath)
	}
	dst := cfg.CosmosToL2Config
	sourceClient, destClient := dst.CosmosWasmClientID, dst.ICS26ClientID
	stateDir := relayStateDir(configPath, "cosmos-to-l2", relayClientIDs{router: destClient, wasm: sourceClient})
	t.Logf("path %s: cosmos %s -> L2 %s, state dir %s", cfg.CosmosToL2Name, sourceClient, destClient, stateDir)

	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout+15*time.Minute)
	defer cancel()

	cosmosRPC, err := rpchttp.New(dst.TmRpcUrl, "/websocket")
	requireNoErr(t, err, "dial Cosmos RPC")
	l2, err := ethclient.DialContext(ctx, dst.EthRpcUrl)
	requireNoErr(t, err, "dial L2 RPC")
	defer l2.Close()
	l2Endpoint := services.EVMEndpoint{Client: l2, Contracts: services.EVMContracts{Router: common.HexToAddress(dst.ICS26Address)}}

	sender := &transaction.Handler{}
	sender.SetSigner(cosmosOnlySigner{hexKey: senderKeyHex})
	senderAddr, err := sender.CosmosSignerAddress()
	requireNoErr(t, err, "derive sender address")

	binary := relayerBinary(t)

	// 1. Run the relayer until it has a durable Cosmos cursor at or past the
	// current height, so the restart resumes from it rather than from a lookback
	// that could already cover the send.
	status0, err := cosmosRPC.Status(ctx)
	requireNoErr(t, err, "query Cosmos status")
	startHeight := uint64(status0.SyncInfo.LatestBlockHeight)
	first := startRelayer(t, binary, configPath, "first")
	waitFor(t, ctx, 5*time.Minute, "relayer to persist a Cosmos cursor >= "+strconv.FormatUint(startHeight, 10), func() (bool, error) {
		if err := first.exited(); err != nil {
			return false, err
		}
		cursor, err := cosmosCursor(stateDir)
		return cursor >= startHeight, err
	})

	// 2. Pause: only this relayer stops; the chains and attestor keep running.
	first.stop(t)
	requireNoOwedTimeouts(t, ctx, cosmosRPC, stateDir, sourceClient)

	// 3. Send while nothing relays, with a short absolute timeout.
	sequence := queryNextSequenceSend(t, ctx, cosmosRPC, sourceClient)
	balanceBefore := queryBalance(t, ctx, cosmosRPC, senderAddr, denom)
	timeoutTimestamp := uint64(time.Now().Add(timeoutWindow).Unix())
	packet := sendTransfer(t, ctx, sender, cosmosRPC, senderAddr, sourceClient, destClient, denom, sequence, timeoutTimestamp)
	balanceSent := queryBalance(t, ctx, cosmosRPC, senderAddr, denom)
	if want := new(big.Int).Sub(balanceBefore, big.NewInt(l2TimeoutTransferAmount)); balanceSent.Cmp(want) > 0 {
		t.Fatalf("balance after send = %s, want <= %s (amount escrowed, fee paid)", balanceSent, want)
	}
	if !packetCommitmentExists(t, ctx, cosmosRPC, sourceClient, sequence) {
		t.Fatalf("no Cosmos packet commitment for %s/%d after send", sourceClient, sequence)
	}
	t.Logf("sent seq=%d timeout=%d; balance %s -> %s", sequence, timeoutTimestamp, balanceBefore, balanceSent)

	// 4. Expire: the L2 clock, not the host's, decides the timeout.
	waitFor(t, ctx, timeoutWindow+5*time.Minute, "L2 head timestamp to pass the packet timeout", func() (bool, error) {
		head, err := l2.HeaderByNumber(ctx, nil)
		if err != nil {
			return false, err
		}
		return head.Time >= timeoutTimestamp, nil
	})

	// 5. Restart on the same config, hence the same state directory.
	second := startRelayer(t, binary, configPath, "restart")
	defer second.stop(t)
	// Settled means both halves: the commitment is gone from Cosmos AND the relayer
	// has retired its tracker entry. The first happens at block inclusion, the
	// second only after the relayer confirms the tx, so either alone races.
	seenPending := false
	waitFor(t, ctx, settleTimeout, "MsgTimeout to settle the packet on Cosmos and leave the pending set", func() (bool, error) {
		if err := second.exited(); err != nil {
			return false, err
		}
		pending, err := statusHasPending(ctx, cfg.StatusAddress, sourceClient, sequence)
		if err != nil {
			return false, err
		}
		seenPending = seenPending || pending
		return !pending && !packetCommitmentExists(t, ctx, cosmosRPC, sourceClient, sequence), nil
	})

	t.Run("refund", func(t *testing.T) {
		got := queryBalance(t, ctx, cosmosRPC, senderAddr, denom)
		want := new(big.Int).Add(balanceSent, big.NewInt(l2TimeoutTransferAmount))
		if got.Cmp(want) != 0 {
			t.Fatalf("balance after timeout = %s, want %s (amount refunded, send fee still spent)", got, want)
		}
	})

	t.Run("no L2 receive", func(t *testing.T) {
		received, err := services.HasEthPacketReceipt(ctx, l2Endpoint, packet)
		requireNoErr(t, err, "query L2 packet receipt")
		if received {
			t.Fatalf("L2 router holds a receipt for %s/%d; the packet must not have been received", destClient, sequence)
		}
	})

	t.Run("late receive is rejected", func(t *testing.T) {
		requireLateReceiveRejected(t, ctx, l2, l2Endpoint.Contracts.Router, packet)
	})

	t.Run("pending state", func(t *testing.T) {
		t.Logf("status endpoint listed the packet as pending during recovery: %v", seenPending)
		// The durable record is what a further restart would replay; read it with
		// the relayer stopped so the state lock is free.
		second.stop(t)
		tracker, err := services.NewPersistentPendingPacketTracker(filepath.Join(stateDir, "cosmos.json"))
		requireNoErr(t, err, "open persisted Cosmos pending tracker")
		for _, info := range tracker.GetAll() {
			if info.Packet.SourceClient == sourceClient && info.Packet.Sequence == sequence {
				t.Fatalf("persisted pending tracker still holds %s/%d (attempts=%d deferrals=%d)",
					sourceClient, sequence, info.TimeoutAttempts, info.Deferrals)
			}
		}
	})
}

// cosmosOnlySigner gives the test's transaction.Handler a Cosmos key without an
// ETH one: the sender only ever submits MsgSendPacket.
type cosmosOnlySigner struct{ hexKey string }

func (s cosmosOnlySigner) EthKey() (*ecdsa.PrivateKey, error) {
	return nil, errors.New("test sender has no ETH key")
}

func (s cosmosOnlySigner) CosmosKeyBytes() ([]byte, error) {
	return common.FromHex("0x" + s.hexKey), nil
}

func sendTransfer(
	t *testing.T, ctx context.Context, sender *transaction.Handler, cosmosRPC *rpchttp.HTTP,
	senderAddr, sourceClient, destClient, denom string, sequence, timeoutTimestamp uint64,
) channeltypesv2.Packet {
	t.Helper()
	receiver := crypto.PubkeyToAddress(mustKey(t).PublicKey)
	value, err := transfertypes.EncodeABIFungibleTokenPacketData(&transfertypes.FungibleTokenPacketData{
		Denom:    denom,
		Amount:   sdkmath.NewInt(l2TimeoutTransferAmount).String(),
		Sender:   senderAddr,
		Receiver: strings.ToLower(receiver.Hex()),
		Memo:     "l2-timeout-lifecycle",
	})
	requireNoErr(t, err, "encode transfer payload")
	payload := channeltypesv2.Payload{
		SourcePort:      transfertypes.PortID,
		DestinationPort: transfertypes.PortID,
		Version:         transfertypes.V1,
		Encoding:        transfertypes.EncodingABI,
		Value:           value,
	}
	msg := &channeltypesv2.MsgSendPacket{
		SourceClient:     sourceClient,
		TimeoutTimestamp: timeoutTimestamp,
		Payloads:         []channeltypesv2.Payload{payload},
		Signer:           senderAddr,
	}
	requireNoErr(t, sender.SendCosmosTxBatch(ctx, services.CosmosEndpoint{Client: cosmosRPC}, []any{msg}), "send MsgSendPacket")
	return channeltypesv2.Packet{
		Sequence:          sequence,
		SourceClient:      sourceClient,
		DestinationClient: destClient,
		TimeoutTimestamp:  timeoutTimestamp,
		Payloads:          []channeltypesv2.Payload{payload},
	}
}

// requireLateReceiveRejected simulates the relayer delivering the packet now. The
// router checks the timeout before it decodes the proof, so any membership bytes
// reach the check; the call is made from the relayer's address to pass the role
// gate in front of it.
func requireLateReceiveRejected(t *testing.T, ctx context.Context, l2 *ethclient.Client, router common.Address, packet channeltypesv2.Packet) {
	t.Helper()
	routerABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	requireNoErr(t, err, "load ICS26Router ABI")
	data, err := routerABI.Pack("recvPacket", contractICS26Router.IICS26RouterMsgsMsgRecvPacket{
		Packet:        services.ToEthPacket(packet),
		MembershipMsg: []byte{0},
	})
	requireNoErr(t, err, "pack recvPacket")
	from := crypto.PubkeyToAddress(mustKey(t).PublicKey)
	_, callErr := l2.CallContract(ctx, ethereum.CallMsg{From: from, To: &router, Data: data}, nil)
	if callErr == nil {
		t.Fatal("late recvPacket succeeded in simulation; want IBCInvalidTimeoutTimestamp")
	}
	want := routerABI.Errors["IBCInvalidTimeoutTimestamp"].ID
	var de interface{ ErrorData() interface{} }
	if !errors.As(callErr, &de) {
		t.Fatalf("late recvPacket failed without revert data: %v", callErr)
	}
	raw, _ := de.ErrorData().(string)
	revert := common.FromHex(raw)
	if len(revert) < 4 || !strings.EqualFold(common.Bytes2Hex(revert[:4]), common.Bytes2Hex(want[:4])) {
		t.Fatalf("late recvPacket reverted with %x, want IBCInvalidTimeoutTimestamp (%x)", revert, want[:4])
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.TrimPrefix(os.Getenv("ETH_PRIVATE_KEY"), "0x"))
	requireNoErr(t, err, "parse ETH_PRIVATE_KEY")
	return key
}

// cosmosCursor reads the file directly: services.LoadRecoveryState memoizes per
// path, so polling through it would keep returning the first read.
func cosmosCursor(stateDir string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, services.RecoveryCursorsFileName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var file struct {
		Cursors map[string]uint64 `json:"cursors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return 0, err
	}
	return file.Cursors["cosmos"], nil
}

// statusHasPending reports whether the running relayer lists the packet in any
// pending set. A fetch error is returned, never read as "not pending": the
// endpoint is down until the relayer finishes starting.
func statusHasPending(ctx context.Context, addr, sourceClient string, sequence uint64) (bool, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := status.Fetch(fetchCtx, addr)
	if err != nil {
		return false, err
	}
	for _, q := range snap.Queues {
		for _, p := range q.Pending {
			if p.SourceClient == sourceClient && p.Sequence == sequence {
				return true, nil
			}
		}
	}
	return false, nil
}

// requireNoOwedTimeouts refuses to start from a state directory that still owes
// a refund on this client. The restart would settle it alongside the test's
// packet, and a second refund to the same sender makes the balance check
// meaningless. Only entries whose Cosmos commitment still exists count: a
// restart can re-track an already timed-out packet from the recovery lookback,
// and its redundant MsgTimeout refunds nothing. Called with the relayer stopped,
// so the state lock is free.
func requireNoOwedTimeouts(t *testing.T, ctx context.Context, c *rpchttp.HTTP, stateDir, sourceClient string) {
	t.Helper()
	tracker, err := services.NewPersistentPendingPacketTracker(filepath.Join(stateDir, "cosmos.json"))
	requireNoErr(t, err, "open persisted Cosmos pending tracker")
	var leftover []string
	for _, info := range tracker.GetAll() {
		if info.Packet.SourceClient == sourceClient && packetCommitmentExists(t, ctx, c, sourceClient, info.Packet.Sequence) {
			leftover = append(leftover, fmt.Sprintf("seq=%d (not before %s)", info.Packet.Sequence, info.NotBefore.Format(time.RFC3339)))
		}
	}
	if len(leftover) > 0 {
		t.Fatalf("%s still owes timeouts on %s from an earlier run: %s; run `relayer start` on this "+
			"config until `relayer status` shows none, then rerun", stateDir, sourceClient, strings.Join(leftover, ", "))
	}
}

func abciQuery(t *testing.T, ctx context.Context, c *rpchttp.HTTP, path string, req, resp proto.Message) (uint32, string) {
	t.Helper()
	bz, err := proto.Marshal(req)
	requireNoErr(t, err, "marshal "+path)
	res, err := c.ABCIQuery(ctx, path, bz)
	requireNoErr(t, err, "query "+path)
	if res.Response.Code != 0 {
		return res.Response.Code, res.Response.Log
	}
	requireNoErr(t, proto.Unmarshal(res.Response.Value, resp), "unmarshal "+path)
	return 0, ""
}

func queryBalance(t *testing.T, ctx context.Context, c *rpchttp.HTTP, addr, denom string) *big.Int {
	t.Helper()
	var resp banktypes.QueryBalanceResponse
	if code, log := abciQuery(t, ctx, c, "/cosmos.bank.v1beta1.Query/Balance",
		&banktypes.QueryBalanceRequest{Address: addr, Denom: denom}, &resp); code != 0 {
		t.Fatalf("balance query failed (code %d): %s", code, log)
	}
	if resp.Balance == nil {
		return new(big.Int)
	}
	return resp.Balance.Amount.BigInt()
}

func queryNextSequenceSend(t *testing.T, ctx context.Context, c *rpchttp.HTTP, clientID string) uint64 {
	t.Helper()
	var resp channeltypesv2.QueryNextSequenceSendResponse
	if code, log := abciQuery(t, ctx, c, "/ibc.core.channel.v2.Query/NextSequenceSend",
		&channeltypesv2.QueryNextSequenceSendRequest{ClientId: clientID}, &resp); code != 0 {
		t.Fatalf("next sequence query for %s failed (code %d): %s", clientID, code, log)
	}
	return resp.NextSequenceSend
}

func packetCommitmentExists(t *testing.T, ctx context.Context, c *rpchttp.HTTP, clientID string, sequence uint64) bool {
	t.Helper()
	var resp channeltypesv2.QueryPacketCommitmentResponse
	code, log := abciQuery(t, ctx, c, "/ibc.core.channel.v2.Query/PacketCommitment",
		&channeltypesv2.QueryPacketCommitmentRequest{ClientId: clientID, Sequence: sequence}, &resp)
	if code != 0 {
		if strings.Contains(strings.ToLower(log), "not found") {
			return false
		}
		t.Fatalf("packet commitment query for %s/%d failed (code %d): %s", clientID, sequence, code, log)
	}
	return len(resp.Commitment) > 0
}

// relayerProcess is one `relayer start` run, logging to a file that is dumped
// when the test fails.
type relayerProcess struct {
	cmd     *exec.Cmd
	logPath string
	done    chan error
	stopped bool
}

func relayerBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("RELAYER_BINARY"); p != "" {
		return p
	}
	bin := filepath.Join(t.TempDir(), "relayer")
	build := exec.Command("go", "build", "-o", bin, "./cmd")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build relayer: %v\n%s", err, out)
	}
	return bin
}

func startRelayer(t *testing.T, binary, configPath, label string) *relayerProcess {
	t.Helper()
	logDir := os.Getenv("L2_TIMEOUT_LOG_DIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	logPath := filepath.Join(logDir, "relayer-"+label+".log")
	logFile, err := os.Create(logPath)
	requireNoErr(t, err, "create relayer log")
	cmd := exec.Command(binary, "start", "--config", configPath)
	cmd.Dir = filepath.Dir(configPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	requireNoErr(t, cmd.Start(), "start relayer ("+label+")")
	p := &relayerProcess{cmd: cmd, logPath: logPath, done: make(chan error, 1)}
	go func() {
		p.done <- cmd.Wait()
		logFile.Close()
	}()
	t.Cleanup(func() {
		p.stop(t)
		if t.Failed() {
			dumpTail(t, label, logPath, 120)
		}
	})
	t.Logf("relayer %s started (pid %d), log %s", label, cmd.Process.Pid, logPath)
	return p
}

// exited reports an error once the process has stopped on its own.
func (p *relayerProcess) exited() error {
	select {
	case err := <-p.done:
		p.stopped = true
		return fmt.Errorf("relayer exited early: %v (log %s)", err, p.logPath)
	default:
		return nil
	}
}

func (p *relayerProcess) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		t.Logf("relayer pid %d ignored SIGINT for 60s; killing", p.cmd.Process.Pid)
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func dumpTail(t *testing.T, label, path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	t.Logf("--- last %d lines of relayer %s log (%s) ---\n%s", len(lines), label, path, strings.Join(lines, "\n"))
}

func waitFor(t *testing.T, ctx context.Context, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := cond()
		if ok {
			return
		}
		lastErr = err
		if err != nil && strings.HasPrefix(err.Error(), "relayer exited early") {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("timed out after %s waiting for %s (last error: %v)", timeout, what, lastErr)
		}
		time.Sleep(l2TimeoutPollInterval)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	requireNoErr(t, err, "parse "+key)
	return d
}

func requireNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
