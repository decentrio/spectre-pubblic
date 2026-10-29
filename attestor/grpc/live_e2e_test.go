package grpc

// This file deliberately tests a running attestor and its running L2 replica.
// The contract/error-mapping matrix belongs in server_contract_test.go; keeping
// it there means this suite spends its time only on boundaries a fake port
// cannot exercise: daemon composition, the replica RPC, signed verdicts and
// the metrics endpoint.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"attestor/types/attestation"
	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	liveE2EGRPCAddress = "ATTESTOR_E2E_GRPC_ADDR"
	liveE2ESourceChain = "ATTESTOR_E2E_SRC_CHAIN"
	liveE2EL2RPCURL    = "ATTESTOR_E2E_L2_RPC_URL"
	liveE2EL2ChainID   = "ATTESTOR_E2E_L2_CHAIN_ID"
	liveE2EPublicKey   = "ATTESTOR_E2E_PUBLIC_KEY"
	liveE2ERouter      = "ATTESTOR_E2E_L2_ROUTER"
	liveE2EMetricsURL  = "ATTESTOR_E2E_METRICS_URL"
	liveE2EMetricPref  = "ATTESTOR_E2E_METRIC_PREFIX"
	liveE2EWatch       = "ATTESTOR_E2E_EXPECT_WATCH"
	liveE2ETimeout     = "ATTESTOR_E2E_TIMEOUT"
)

type liveE2EConfig struct {
	grpcAddress string
	sourceChain string
	l2RPCURL    string
	l2ChainID   uint64
	publicKey   ed25519.PublicKey
	router      []byte
	metricsURL  string
	metricPref  string
	watch       string
	timeout     time.Duration
}

type l2Header struct {
	number    uint64
	hash      []byte
	stateRoot []byte
}

// TestLiveAttestorE2E covers C1, C5-C7, C10-C14, C16-C18 in the manual test
// plan against a real sidecar. It is opt-in so the regular package suite never
// tries to contact a developer's local network. scripts/local/test_attestor_e2e.sh
// supplies these variables from an OP, Base, or Arbitrum handoff.
func TestLiveAttestorE2E(t *testing.T) {
	config, enabled, err := loadLiveE2EConfig()
	if !enabled {
		t.Skipf("set %s to run against a live attestor", liveE2EGRPCAddress)
	}
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.timeout)
	defer cancel()

	connection, err := googlegrpc.NewClient(
		config.grpcAddress,
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial attestor %s: %v", config.grpcAddress, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := attestorpb.NewAttestorServiceClient(connection)

	var chain *attestorpb.ChainInfo
	if !t.Run("C1_info_and_frontier", func(t *testing.T) {
		info, infoErr := client.Info(ctx, &attestorpb.InfoRequest{})
		if infoErr != nil {
			t.Fatalf("Info: %v", infoErr)
		}
		for _, candidate := range info.GetChains() {
			if candidate.GetSrcChain() == config.sourceChain {
				chain = candidate
				break
			}
		}
		if chain == nil {
			t.Fatalf("Info did not serve src_chain %q: %+v", config.sourceChain, info.GetChains())
		}
		if !chain.GetReplicaSeen() {
			t.Fatal("Info reports replica_seen=false")
		}
		if chain.GetReplicaFinalizedL2() > chain.GetReplicaSafeL2() || chain.GetReplicaSafeL2() > chain.GetReplicaUnsafeL2() {
			t.Fatalf(
				"replica heads are not finalized <= safe <= unsafe: finalized=%d safe=%d unsafe=%d",
				chain.GetReplicaFinalizedL2(), chain.GetReplicaSafeL2(), chain.GetReplicaUnsafeL2(),
			)
		}
		if _, ok := runModeForHead(chain.GetAttestationHead()); !ok {
			t.Fatalf("Info attestation_head = %q, want unsafe, safe, or finalized", chain.GetAttestationHead())
		}
	}) {
		return
	}

	var confirmed, provisional *attestorpb.AttestedRoot
	if !t.Run("C5_C6_C7_feed", func(t *testing.T) {
		confirmed = waitForLiveFrontier(t, ctx, client, config.sourceChain, false)
		provisional = waitForLiveFrontier(t, ctx, client, config.sourceChain, true)
		assertLiveRoot(t, "confirmed frontier", confirmed)
		assertLiveRoot(t, "provisional frontier", provisional)
		if provisional.GetL2BlockNumber() < confirmed.GetL2BlockNumber() {
			t.Fatalf(
				"provisional frontier %d is below confirmed frontier %d",
				provisional.GetL2BlockNumber(), confirmed.GetL2BlockNumber(),
			)
		}
		if chain.GetAttestationHead() == "finalized" && provisional.GetProvisional() {
			t.Fatal("finalized attestation head returned a provisional frontier")
		}

		atFrontier, queryErr := client.AttestedRootAtOrBelow(ctx, &attestorpb.AttestedRootAtOrBelowRequest{
			SrcChain:           config.sourceChain,
			L2BlockNumber:      provisional.GetL2BlockNumber(),
			IncludeProvisional: true,
		})
		if queryErr != nil {
			t.Fatalf("AttestedRootAtOrBelow(%d): %v", provisional.GetL2BlockNumber(), queryErr)
		}
		if !atFrontier.GetFound() || atFrontier.GetRoot().GetL2BlockNumber() != provisional.GetL2BlockNumber() {
			t.Fatalf("AttestedRootAtOrBelow(%d) = %+v, want frontier", provisional.GetL2BlockNumber(), atFrontier)
		}
		assertLiveRoot(t, "root at frontier", atFrontier.GetRoot())
	}) {
		return
	}

	var header l2Header
	if !t.Run("C10_C12_C16_signed_canonical_verdict", func(t *testing.T) {
		var headerErr error
		header, headerErr = getL2Header(ctx, config.l2RPCURL, provisional.GetL2BlockNumber())
		if headerErr != nil {
			t.Fatal(headerErr)
		}
		runMode, _ := runModeForHead(chain.GetAttestationHead())
		setHash := testSetHash(t, config.publicKey)

		request := liveVerifyRequest(config, header, runMode, setHash[:])
		response, verifyErr := client.VerifyStateRoot(ctx, request)
		if verifyErr != nil {
			t.Fatalf("VerifyStateRoot canonical block %d: %v", header.number, verifyErr)
		}
		assertCanonicalSignedVerdict(t, config, header, setHash[:], response)
		canonicalSignature := append([]byte(nil), response.GetAttestationSignature()...)

		withoutHash := liveVerifyRequest(config, header, runMode, setHash[:])
		withoutHash.ExpectedBlockHash = nil
		response, verifyErr = client.VerifyStateRoot(ctx, withoutHash)
		if verifyErr != nil {
			t.Fatalf("VerifyStateRoot without expected block hash: %v", verifyErr)
		}
		assertCanonicalSignedVerdict(t, config, header, setHash[:], response)

		otherRouter := append([]byte(nil), config.router...)
		otherRouter[len(otherRouter)-1] ^= 1
		routerBound := liveVerifyRequest(config, header, runMode, setHash[:])
		routerBound.L2Router = otherRouter
		response, verifyErr = client.VerifyStateRoot(ctx, routerBound)
		if verifyErr != nil {
			t.Fatalf("VerifyStateRoot with another router: %v", verifyErr)
		}
		boundConfig := config
		boundConfig.router = otherRouter
		assertCanonicalSignedVerdict(t, boundConfig, header, setHash[:], response)
		if bytes.Equal(response.GetAttestationSignature(), canonicalSignature) {
			t.Fatal("signature did not change when its router changed")
		}

		otherSetHash := append([]byte(nil), setHash[:]...)
		otherSetHash[len(otherSetHash)-1] ^= 1
		setBound := liveVerifyRequest(config, header, runMode, otherSetHash)
		response, verifyErr = client.VerifyStateRoot(ctx, setBound)
		if verifyErr != nil {
			t.Fatalf("VerifyStateRoot with another attestor set hash: %v", verifyErr)
		}
		assertCanonicalSignedVerdict(t, config, header, otherSetHash, response)
		if bytes.Equal(response.GetAttestationSignature(), canonicalSignature) {
			t.Fatal("signature did not change when its attestor set hash changed")
		}
	}) {
		return
	}

	t.Run("C11_mismatched_identity_has_no_signature", func(t *testing.T) {
		runMode, _ := runModeForHead(chain.GetAttestationHead())
		setHash := testSetHash(t, config.publicKey)
		wrongRoot := liveVerifyRequest(config, header, runMode, setHash[:])
		wrongRoot.ExpectedStateRoot = append([]byte(nil), header.stateRoot...)
		wrongRoot.ExpectedStateRoot[0] ^= 1
		response, verifyErr := client.VerifyStateRoot(ctx, wrongRoot)
		if verifyErr != nil {
			t.Fatalf("VerifyStateRoot wrong state root: %v", verifyErr)
		}
		assertMismatchedVerdict(t, header, response)

		wrongHash := liveVerifyRequest(config, header, runMode, setHash[:])
		wrongHash.ExpectedBlockHash = append([]byte(nil), header.hash...)
		wrongHash.ExpectedBlockHash[0] ^= 1
		response, verifyErr = client.VerifyStateRoot(ctx, wrongHash)
		if verifyErr != nil {
			t.Fatalf("VerifyStateRoot wrong block hash: %v", verifyErr)
		}
		assertMismatchedVerdict(t, header, response)
	})

	t.Run("C13_C14_head_guards", func(t *testing.T) {
		runMode, _ := runModeForHead(chain.GetAttestationHead())
		setHash := testSetHash(t, config.publicKey)
		wrongMode := aDifferentRunMode(runMode)
		request := liveVerifyRequest(config, header, wrongMode, setHash[:])
		if _, verifyErr := client.VerifyStateRoot(ctx, request); status.Code(verifyErr) != codes.FailedPrecondition {
			t.Fatalf("VerifyStateRoot with run mode %s error = %v (%s), want FailedPrecondition", wrongMode, verifyErr, status.Code(verifyErr))
		}

		// Read the snapshot immediately before the request. A head captured at
		// the start of a two-minute live suite can already be reachable by now.
		currentInfo, infoErr := client.Info(ctx, &attestorpb.InfoRequest{})
		if infoErr != nil {
			t.Fatalf("refresh Info before above-head check: %v", infoErr)
		}
		currentChain := chainInfoForSource(currentInfo, config.sourceChain)
		if currentChain == nil {
			t.Fatalf("refreshed Info did not serve src_chain %q", config.sourceChain)
		}
		aboveHead := replicaHeadForMode(currentChain, runMode) + 1000
		request = liveVerifyRequest(config, header, runMode, setHash[:])
		request.BlockNumber = aboveHead
		if _, verifyErr := client.VerifyStateRoot(ctx, request); status.Code(verifyErr) != codes.FailedPrecondition {
			t.Fatalf("VerifyStateRoot above replica head %d error = %v (%s), want FailedPrecondition", aboveHead, verifyErr, status.Code(verifyErr))
		}
	})

	t.Run("C8_C9_watch_capability", func(t *testing.T) {
		switch config.watch {
		case "skip":
			t.Skipf("set %s=supported or unsupported to check WatchAttested", liveE2EWatch)
		case "supported", "unsupported":
			watchCtx, watchCancel := context.WithTimeout(ctx, minDuration(config.timeout, 10*time.Second))
			defer watchCancel()
			stream, watchErr := client.WatchAttested(watchCtx, &attestorpb.WatchAttestedRequest{
				SrcChain:           config.sourceChain,
				IncludeProvisional: true,
			})
			if watchErr != nil {
				t.Fatalf("WatchAttested: %v", watchErr)
			}
			update, watchErr := stream.Recv()
			if config.watch == "unsupported" {
				if status.Code(watchErr) != codes.Unimplemented {
					t.Fatalf("WatchAttested error = %v (%s), want Unimplemented", watchErr, status.Code(watchErr))
				}
				return
			}
			if watchErr != nil {
				t.Fatalf("WatchAttested initial update: %v", watchErr)
			}
			assertLiveRoot(t, "watch update", update.GetRoot())
		default:
			t.Fatalf("%s = %q, want supported, unsupported, or skip", liveE2EWatch, config.watch)
		}
	})

	t.Run("C18_metrics", func(t *testing.T) {
		if config.metricsURL == "" || config.metricPref == "" {
			t.Skipf("set %s and %s to check metrics", liveE2EMetricsURL, liveE2EMetricPref)
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, config.metricsURL, nil)
		if requestErr != nil {
			t.Fatalf("build metrics request: %v", requestErr)
		}
		response, responseErr := http.DefaultClient.Do(request)
		if responseErr != nil {
			t.Fatalf("GET metrics: %v", responseErr)
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			t.Fatalf("read metrics: %v", readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("metrics status = %s: %s", response.Status, body)
		}
		if !bytes.Contains(body, []byte(config.metricPref)) {
			t.Fatalf("metrics did not contain %q", config.metricPref)
		}
	})
}

func chainInfoForSource(info *attestorpb.InfoResponse, sourceChain string) *attestorpb.ChainInfo {
	for _, chain := range info.GetChains() {
		if chain.GetSrcChain() == sourceChain {
			return chain
		}
	}
	return nil
}

func loadLiveE2EConfig() (liveE2EConfig, bool, error) {
	grpcAddress := strings.TrimSpace(os.Getenv(liveE2EGRPCAddress))
	if grpcAddress == "" {
		return liveE2EConfig{}, false, nil
	}

	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required when %s is set", name, liveE2EGRPCAddress)
		}
		return value, nil
	}
	sourceChain, err := required(liveE2ESourceChain)
	if err != nil {
		return liveE2EConfig{}, true, err
	}
	l2RPCURL, err := required(liveE2EL2RPCURL)
	if err != nil {
		return liveE2EConfig{}, true, err
	}
	chainIDText, err := required(liveE2EL2ChainID)
	if err != nil {
		return liveE2EConfig{}, true, err
	}
	chainID, err := strconv.ParseUint(chainIDText, 10, 64)
	if err != nil || chainID == 0 {
		return liveE2EConfig{}, true, fmt.Errorf("%s must be a non-zero decimal uint64, got %q", liveE2EL2ChainID, chainIDText)
	}
	publicKeyText, err := required(liveE2EPublicKey)
	if err != nil {
		return liveE2EConfig{}, true, err
	}
	publicKey, err := decodeLivePublicKey(publicKeyText)
	if err != nil {
		return liveE2EConfig{}, true, fmt.Errorf("%s: %w", liveE2EPublicKey, err)
	}
	routerText := strings.TrimSpace(os.Getenv(liveE2ERouter))
	if routerText == "" {
		routerText = "0x0000000000000000000000000000000000000001"
	}
	router, err := decodeExactHex(routerText, attestation.RouterLength)
	if err != nil {
		return liveE2EConfig{}, true, fmt.Errorf("%s: %w", liveE2ERouter, err)
	}
	// A fresh safe/unsafe daemon may need an L1 finality interval before it
	// has a confirmed root, so leave enough room for the two feed policies.
	timeout := 2 * time.Minute
	if value := strings.TrimSpace(os.Getenv(liveE2ETimeout)); value != "" {
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return liveE2EConfig{}, true, fmt.Errorf("%s must be a positive duration, got %q", liveE2ETimeout, value)
		}
	}
	return liveE2EConfig{
		grpcAddress: grpcAddress,
		sourceChain: sourceChain,
		l2RPCURL:    l2RPCURL,
		l2ChainID:   chainID,
		publicKey:   publicKey,
		router:      router,
		metricsURL:  strings.TrimSpace(os.Getenv(liveE2EMetricsURL)),
		metricPref:  strings.TrimSpace(os.Getenv(liveE2EMetricPref)),
		watch:       valueOrDefault(strings.TrimSpace(os.Getenv(liveE2EWatch)), "skip"),
		timeout:     timeout,
	}, true, nil
}

func decodeLivePublicKey(value string) (ed25519.PublicKey, error) {
	if decoded, err := decodeExactHex(value, ed25519.PublicKeySize); err == nil {
		return ed25519.PublicKey(decoded), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("must be a %d-byte hex or base64 Ed25519 public key", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}

func decodeExactHex(value string, want int) ([]byte, error) {
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if err != nil {
		return nil, fmt.Errorf("decode hex: %w", err)
	}
	if len(decoded) != want {
		return nil, fmt.Errorf("got %d bytes, want %d", len(decoded), want)
	}
	return decoded, nil
}

func waitForLiveFrontier(t *testing.T, ctx context.Context, client attestorpb.AttestorServiceClient, srcChain string, provisional bool) *attestorpb.AttestedRoot {
	t.Helper()
	for {
		response, err := client.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{
			SrcChain:           srcChain,
			IncludeProvisional: provisional,
		})
		if err != nil {
			t.Fatalf("AttestedUpTo(include_provisional=%t): %v", provisional, err)
		}
		if response.GetFound() && response.GetRoot() != nil {
			return response.GetRoot()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("attestor did not publish a frontier before %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func assertLiveRoot(t *testing.T, name string, root *attestorpb.AttestedRoot) {
	t.Helper()
	if root == nil {
		t.Fatalf("%s is nil", name)
	}
	if len(root.GetRoot()) != attestation.HashLength {
		t.Fatalf("%s root has %d bytes, want %d", name, len(root.GetRoot()), attestation.HashLength)
	}
	if root.GetL2BlockNumber() == 0 {
		t.Fatalf("%s has L2 block 0", name)
	}
	switch root.GetSource() {
	case "derived":
		if root.GetProvenance() != nil {
			t.Fatalf("%s derived root unexpectedly carries provenance %T", name, root.GetProvenance())
		}
	case "game":
		if _, ok := root.GetProvenance().(*attestorpb.AttestedRoot_GameIndex); !ok {
			t.Fatalf("%s game provenance = %T, want game_index", name, root.GetProvenance())
		}
	case "assertion":
		assertion, ok := root.GetProvenance().(*attestorpb.AttestedRoot_AssertionHash)
		if !ok {
			t.Fatalf("%s assertion provenance = %T, want assertion_hash", name, root.GetProvenance())
		}
		if len(assertion.AssertionHash) != attestation.HashLength {
			t.Fatalf("%s assertion hash has %d bytes, want %d", name, len(root.GetAssertionHash()), attestation.HashLength)
		}
	default:
		t.Fatalf("%s source = %q, want derived, game, or assertion", name, root.GetSource())
	}
	if root.GetAttestedAt() <= 0 || time.Since(time.Unix(root.GetAttestedAt(), 0)) > 24*time.Hour || root.GetAttestedAt() > time.Now().Add(5*time.Minute).Unix() {
		t.Fatalf("%s attested_at = %d, want a recent Unix time", name, root.GetAttestedAt())
	}
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func getL2Header(ctx context.Context, rpcURL string, number uint64) (l2Header, error) {
	payload, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_getBlockByNumber",
		Params:  []any{fmt.Sprintf("0x%x", number), false},
	})
	if err != nil {
		return l2Header{}, fmt.Errorf("encode eth_getBlockByNumber: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(payload))
	if err != nil {
		return l2Header{}, fmt.Errorf("build L2 RPC request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return l2Header{}, fmt.Errorf("call L2 RPC %s: %w", rpcURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return l2Header{}, fmt.Errorf("read L2 RPC response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return l2Header{}, fmt.Errorf("L2 RPC status %s: %s", response.Status, body)
	}
	var decoded struct {
		Result *struct {
			Number    string `json:"number"`
			Hash      string `json:"hash"`
			StateRoot string `json:"stateRoot"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return l2Header{}, fmt.Errorf("decode L2 RPC response: %w", err)
	}
	if decoded.Error != nil {
		return l2Header{}, fmt.Errorf("L2 RPC error %d: %s", decoded.Error.Code, decoded.Error.Message)
	}
	if decoded.Result == nil {
		return l2Header{}, fmt.Errorf("L2 RPC returned no block at %d", number)
	}
	returnedNumber, err := strconv.ParseUint(strings.TrimPrefix(decoded.Result.Number, "0x"), 16, 64)
	if err != nil {
		return l2Header{}, fmt.Errorf("decode returned block number %q: %w", decoded.Result.Number, err)
	}
	if returnedNumber != number {
		return l2Header{}, fmt.Errorf("L2 RPC returned block %d, want %d", returnedNumber, number)
	}
	hash, err := decodeExactHex(decoded.Result.Hash, attestation.HashLength)
	if err != nil {
		return l2Header{}, fmt.Errorf("decode block hash: %w", err)
	}
	stateRoot, err := decodeExactHex(decoded.Result.StateRoot, attestation.HashLength)
	if err != nil {
		return l2Header{}, fmt.Errorf("decode state root: %w", err)
	}
	return l2Header{number: number, hash: hash, stateRoot: stateRoot}, nil
}

func runModeForHead(head string) (attestorpb.RunMode, bool) {
	switch head {
	case "unsafe":
		return attestorpb.RunMode_RUN_MODE_UNSAFE, true
	case "safe":
		return attestorpb.RunMode_RUN_MODE_SAFE, true
	case "finalized":
		return attestorpb.RunMode_RUN_MODE_FINALIZED, true
	default:
		return attestorpb.RunMode_RUN_MODE_UNSPECIFIED, false
	}
}

func aDifferentRunMode(mode attestorpb.RunMode) attestorpb.RunMode {
	if mode != attestorpb.RunMode_RUN_MODE_UNSAFE {
		return attestorpb.RunMode_RUN_MODE_UNSAFE
	}
	return attestorpb.RunMode_RUN_MODE_SAFE
}

func replicaHeadForMode(chain *attestorpb.ChainInfo, mode attestorpb.RunMode) uint64 {
	switch mode {
	case attestorpb.RunMode_RUN_MODE_UNSAFE:
		return chain.GetReplicaUnsafeL2()
	case attestorpb.RunMode_RUN_MODE_SAFE:
		return chain.GetReplicaSafeL2()
	case attestorpb.RunMode_RUN_MODE_FINALIZED:
		return chain.GetReplicaFinalizedL2()
	default:
		return 0
	}
}

func testSetHash(t *testing.T, publicKey ed25519.PublicKey) [attestation.HashLength]byte {
	t.Helper()
	setHash, err := (attestation.AttestorConfig{PublicKeys: [][]byte{publicKey}, Threshold: 1}).SetHash()
	if err != nil {
		t.Fatalf("build attestor set hash: %v", err)
	}
	return setHash
}

func liveVerifyRequest(config liveE2EConfig, header l2Header, mode attestorpb.RunMode, setHash []byte) *attestorpb.VerifyStateRootRequest {
	return &attestorpb.VerifyStateRootRequest{
		SrcChain:          config.sourceChain,
		BlockNumber:       header.number,
		ExpectedStateRoot: append([]byte(nil), header.stateRoot...),
		ExpectedBlockHash: append([]byte(nil), header.hash...),
		RunMode:           mode,
		L2Router:          append([]byte(nil), config.router...),
		AttestorSetHash:   append([]byte(nil), setHash...),
	}
}

func assertCanonicalSignedVerdict(t *testing.T, config liveE2EConfig, header l2Header, setHash []byte, response *attestorpb.VerifyStateRootResponse) {
	t.Helper()
	if response == nil || !response.GetValid() {
		t.Fatalf("verdict = %+v, want valid", response)
	}
	if response.GetBlockNumber() != header.number || !bytes.Equal(response.GetBlockHash(), header.hash) || !bytes.Equal(response.GetStateRoot(), header.stateRoot) {
		t.Fatalf("verdict canonical identity = %+v, want block=%d hash=%x root=%x", response, header.number, header.hash, header.stateRoot)
	}
	if len(response.GetAttestationSignature()) != attestation.SignatureLength {
		t.Fatalf("signature has %d bytes, want %d", len(response.GetAttestationSignature()), attestation.SignatureLength)
	}
	statement, err := attestation.SigningBytes(config.l2ChainID, config.router, setHash, header.number, header.hash, header.stateRoot)
	if err != nil {
		t.Fatalf("build signed statement: %v", err)
	}
	if !ed25519.Verify(config.publicKey, statement, response.GetAttestationSignature()) {
		t.Fatal("attestor signature does not verify against the configured public key and statement")
	}
}

func assertMismatchedVerdict(t *testing.T, header l2Header, response *attestorpb.VerifyStateRootResponse) {
	t.Helper()
	if response == nil || response.GetValid() {
		t.Fatalf("mismatched verdict = %+v, want valid=false", response)
	}
	if response.GetBlockNumber() != header.number || !bytes.Equal(response.GetBlockHash(), header.hash) || !bytes.Equal(response.GetStateRoot(), header.stateRoot) {
		t.Fatalf("mismatched verdict identity = %+v, want canonical block=%d hash=%x root=%x", response, header.number, header.hash, header.stateRoot)
	}
	if len(response.GetAttestationSignature()) != 0 {
		t.Fatalf("mismatched verdict carries %d-byte signature", len(response.GetAttestationSignature()))
	}
}

func minDuration(first, second time.Duration) time.Duration {
	if first < second {
		return first
	}
	return second
}
