package grpc

// This suite owns its attestor processes. It starts a disposable daemon whose
// L1 RPC, L1/Nitro WebSocket, and replica endpoints all go through in-process
// TCP fault proxies, then injects outages by cutting a proxy, and restarts the
// daemon with SIGTERM and SIGKILL against its own state file. The devnet
// itself is never stopped or reconfigured: a "fault" is only this route
// losing its connection, and every proxy is restored before the test returns.
//
// scripts/local/test_attestor_fault_e2e.sh builds the daemon into a temporary
// directory, supplies the devnet handoff, and removes the directory afterwards.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"attestor/types/attestation"
	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const (
	faultFamily    = "ATTESTOR_FAULT_E2E_FAMILY"
	faultBinary    = "ATTESTOR_FAULT_E2E_BINARY"
	faultWorkDir   = "ATTESTOR_FAULT_E2E_WORK_DIR"
	faultGRPCPort  = "ATTESTOR_FAULT_E2E_GRPC_PORT"
	faultMetrics   = "ATTESTOR_FAULT_E2E_METRICS_PORT"
	faultTimeout   = "ATTESTOR_FAULT_E2E_TIMEOUT"
	faultOutageFor = "ATTESTOR_FAULT_E2E_OUTAGE"
)

// faultEnv is the devnet handoff, read verbatim from the environment the
// script sources. Endpoints listed here are the real ones; the daemon only
// ever sees proxied copies of the ones a fault can cut.
type faultEnv struct {
	family         string
	binary         string
	workDir        string
	grpcPort       int
	metricsPort    int
	timeout        time.Duration
	outage         time.Duration
	srcChain       string
	l1RPC          string
	l1WS           string
	l1ChainID      string
	l2RPC          string // the relayer's L2 RPC (sequencer), never proxied
	l2WS           string
	l2ChainID      uint64
	opNodeRPC      string
	factory        string
	gameType       string
	rollupCore     string
	deployBlock    string
	assertionsSlot string
	statusOffset   string
	publicKey      string
}

// faultRoute is one running disposable daemon and the proxies in front of it.
type faultRoute struct {
	env       faultEnv
	name      string
	head      string
	dir       string
	grpcPort  int
	metrics   int
	l1        *faultProxy
	l1WS      *faultProxy
	replica   *faultProxy
	replicaWS *faultProxy
	cmd       *exec.Cmd
	exited    chan error
	logFile   *os.File
	view      lifecycleConfig
	conn      *googlegrpc.ClientConn
	client    attestorpb.AttestorServiceClient
	health    healthpb.HealthClient
}

// TestLiveAttestorFaultRecoveryE2E covers issue #523. Every fault records the
// gRPC status while it is active, the metric delta and log line it produced,
// and the recovery afterwards.
func TestLiveAttestorFaultRecoveryE2E(t *testing.T) {
	env, enabled, err := loadFaultEnv()
	if !enabled {
		t.Skipf("set %s (via scripts/local/test_attestor_fault_e2e.sh) to run the fault-recovery suite", faultFamily)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), env.timeout)
	defer cancel()

	route := startFaultRoute(t, ctx, env, "main", "unsafe", env.grpcPort, env.metricsPort)

	if !t.Run("R0_baseline", func(t *testing.T) {
		frontier := route.waitFrontierAbove(t, ctx, 0)
		route.waitServing(t, ctx)
		evidence(t, "frontier %d (%s), health SERVING", frontier.GetL2BlockNumber(), frontier.GetSource())
	}) {
		return
	}

	t.Run("R1_replica_outage_and_recovery", func(t *testing.T) {
		route.assertReplicaOutage(t, ctx)
	})
	t.Run("R2_l1_rpc_outage", func(t *testing.T) {
		route.assertL1Outage(t, ctx)
	})
	t.Run("R3_websocket_loss_falls_back_to_polling", func(t *testing.T) {
		route.assertWebSocketLoss(t, ctx)
	})
	t.Run("R4_graceful_restart_keeps_frontier_and_cursor", func(t *testing.T) {
		route.assertRestart(t, ctx, syscall.SIGTERM)
	})
	t.Run("R5_sigkill_restart_keeps_frontier_and_cursor", func(t *testing.T) {
		route.assertRestart(t, ctx, syscall.SIGKILL)
	})
	t.Run("R6_replica_lag_behind_relayer_l2_rpc", func(t *testing.T) {
		assertReplicaLag(t, ctx, env)
	})
}

func loadFaultEnv() (faultEnv, bool, error) {
	env, enabled, err := loadRouteEnv(faultFamily, faultBinary, faultWorkDir, faultGRPCPort, faultMetrics)
	if !enabled || err != nil {
		return env, enabled, err
	}
	for name, target := range map[string]*time.Duration{faultTimeout: &env.timeout, faultOutageFor: &env.outage} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			parsed, parseErr := time.ParseDuration(value)
			if parseErr != nil || parsed <= 0 {
				return env, true, fmt.Errorf("%s must be a positive duration, got %q", name, value)
			}
			*target = parsed
		}
	}
	return env, true, nil
}

// loadRouteEnv reads the devnet handoff plus the suite-owned daemon settings
// named by its arguments. The fault and divergence suites both start
// disposable routes from it; each reads its own timeouts afterwards.
func loadRouteEnv(familyVar, binaryVar, workDirVar, grpcPortVar, metricsPortVar string) (faultEnv, bool, error) {
	family := strings.TrimSpace(os.Getenv(familyVar))
	if family == "" {
		return faultEnv{}, false, nil
	}
	get := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	env := faultEnv{
		family:         family,
		binary:         get(binaryVar),
		workDir:        get(workDirVar),
		srcChain:       get("SRC_CHAIN"),
		l1RPC:          get("L1_RPC_URL"),
		l1WS:           get("L1_WS_URL"),
		l1ChainID:      get("L1_CHAIN_ID"),
		l2RPC:          get("L2_RPC_URL"),
		l2WS:           get("L2_WS_URL"),
		opNodeRPC:      get("OP_NODE_RPC_URL"),
		factory:        get("DISPUTE_GAME_FACTORY"),
		gameType:       get("RESPECTED_GAME_TYPE"),
		rollupCore:     get("ROLLUP_CORE_ADDRESS"),
		deployBlock:    get("ROLLUP_DEPLOYMENT_BLOCK"),
		assertionsSlot: get("ASSERTIONS_MAPPING_SLOT"),
		statusOffset:   get("ASSERTION_STATUS_OFFSET"),
		publicKey:      get("ATTESTOR_PUBLIC_KEY"),
		timeout:        45 * time.Minute,
		outage:         20 * time.Second,
	}
	required := map[string]string{
		binaryVar: env.binary, workDirVar: env.workDir, "SRC_CHAIN": env.srcChain,
		"L1_RPC_URL": env.l1RPC, "L1_CHAIN_ID": env.l1ChainID, "L2_RPC_URL": env.l2RPC,
		"ATTESTOR_SIGNING_KEY": get("ATTESTOR_SIGNING_KEY"), "ATTESTOR_PUBLIC_KEY": env.publicKey,
	}
	switch family {
	case familyOP, familyBase:
		required["OP_NODE_RPC_URL"] = env.opNodeRPC
		required["L1_WS_URL"] = env.l1WS
		required["DISPUTE_GAME_FACTORY"] = env.factory
		required["RESPECTED_GAME_TYPE"] = env.gameType
	case familyArbitrum:
		required["L2_WS_URL"] = env.l2WS
		required["ROLLUP_CORE_ADDRESS"] = env.rollupCore
		required["ROLLUP_DEPLOYMENT_BLOCK"] = env.deployBlock
		required["ASSERTIONS_MAPPING_SLOT"] = env.assertionsSlot
		required["ASSERTION_STATUS_OFFSET"] = env.statusOffset
	default:
		return env, true, fmt.Errorf("%s = %q, want op, base, or arbitrum", familyVar, family)
	}
	for name, value := range required {
		if value == "" {
			return env, true, fmt.Errorf("%s is required for the %s route", name, family)
		}
	}
	var err error
	if env.l2ChainID, err = strconv.ParseUint(get("L2_CHAIN_ID"), 10, 64); err != nil || env.l2ChainID == 0 {
		return env, true, fmt.Errorf("L2_CHAIN_ID must be a positive integer, got %q", get("L2_CHAIN_ID"))
	}
	if env.grpcPort, err = strconv.Atoi(get(grpcPortVar)); err != nil {
		return env, true, fmt.Errorf("%s: %w", grpcPortVar, err)
	}
	if env.metricsPort, err = strconv.Atoi(get(metricsPortVar)); err != nil {
		return env, true, fmt.Errorf("%s: %w", metricsPortVar, err)
	}
	return env, true, nil
}

// ------------------------------------------------------------------ route

func startFaultRoute(t *testing.T, ctx context.Context, env faultEnv, name, head string, grpcPort, metricsPort int) *faultRoute {
	t.Helper()
	dir := filepath.Join(env.workDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	route := &faultRoute{env: env, name: name, head: head, dir: dir, grpcPort: grpcPort, metrics: metricsPort}
	route.l1 = newFaultProxy(t, env.l1RPC)
	if env.family == familyArbitrum {
		route.replica = newFaultProxy(t, env.l2RPC)
		route.replicaWS = newFaultProxy(t, env.l2WS)
	} else {
		route.replica = newFaultProxy(t, env.opNodeRPC)
		route.l1WS = newFaultProxy(t, env.l1WS)
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "attestor.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	route.logFile = logFile
	route.view = lifecycleConfig{
		family:    env.family,
		head:      head,
		statePath: filepath.Join(dir, "attested-roots.json"),
		logPath:   logFile.Name(),
		live: liveE2EConfig{
			sourceChain: env.srcChain,
			metricsURL:  fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort),
		},
	}
	route.writeConfig(t)
	t.Cleanup(route.close)
	route.start(t, ctx)

	connection, err := googlegrpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", grpcPort),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	route.conn = connection
	route.client = attestorpb.NewAttestorServiceClient(connection)
	route.health = healthpb.NewHealthClient(connection)
	return route
}

func (r *faultRoute) writeConfig(t *testing.T) {
	t.Helper()
	var config any
	if r.env.family == familyArbitrum {
		config = map[string]any{
			"grpc_listen_address":            fmt.Sprintf("127.0.0.1:%d", r.grpcPort),
			"metrics_listen_address":         fmt.Sprintf("127.0.0.1:%d", r.metrics),
			"runtime_poll_interval":          "2s",
			"attestation_head":               r.head,
			"derived_attestation_gap_blocks": 5,
			"src_chain":                      r.env.srcChain,
			"l1_rpc_url":                     r.l1.url,
			"l1_chain_id":                    json.Number(r.env.l1ChainID),
			"l2_chain_id":                    r.env.l2ChainID,
			"attestation_signing_key":        "env:ATTESTOR_SIGNING_KEY",
			"rollup_core_address":            r.env.rollupCore,
			"assertions_mapping_slot":        r.env.assertionsSlot,
			"assertion_status_offset":        json.Number(r.env.statusOffset),
			"assertion_start_block":          json.Number(nonZero(r.env.deployBlock)),
			"assertion_poll_interval":        "2s",
			"attestor_state_path":            r.view.statePath,
			"nitro_rpc_url":                  r.replica.url,
			"nitro_ws_url":                   r.replicaWS.url,
		}
	} else {
		config = map[string]any{
			"modules": []any{map[string]any{
				"name":      "op_source",
				"src_chain": r.env.srcChain,
				"config": map[string]any{
					"l1_rpc_url":                     r.l1.url,
					"l1_ws_url":                      r.l1WS.url,
					"l1_chain_id":                    json.Number(r.env.l1ChainID),
					"op_node_rpc_url":                r.replica.url,
					"dispute_game_factory":           r.env.factory,
					"respected_game_type":            json.Number(r.env.gameType),
					"attestation_head":               r.head,
					"poll_interval_seconds":          2,
					"derived_attestation_gap_blocks": 5,
					"state_path":                     r.view.statePath,
					"l1_bootstrap_lookback_blocks":   1000,
					"l2_chain_id":                    r.env.l2ChainID,
					"attestation_signing_key":        "env:ATTESTOR_SIGNING_KEY",
				},
			}},
			"server": map[string]any{"address": "127.0.0.1", "port": r.metrics, "grpc_port": r.grpcPort},
		}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nonZero(block string) string {
	if block == "0" {
		return "1"
	}
	return block
}

func (r *faultRoute) start(t *testing.T, ctx context.Context) {
	t.Helper()
	config := filepath.Join(r.dir, "config.json")
	args := []string{"--config", config}
	if r.env.family == familyArbitrum {
		args = []string{"start", "--config", config}
	}
	fmt.Fprintf(r.logFile, "\n=== fault suite: starting %s route at %s ===\n", r.name, time.Now().Format(time.RFC3339))
	r.cmd = exec.Command(r.env.binary, args...)
	r.cmd.Stdout = r.logFile
	r.cmd.Stderr = r.logFile
	r.cmd.Env = os.Environ()
	if err := r.cmd.Start(); err != nil {
		t.Fatalf("start %s attestor: %v", r.name, err)
	}
	exited := make(chan error, 1)
	r.exited = exited
	go func(cmd *exec.Cmd) { exited <- cmd.Wait() }(r.cmd)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := fetchMetric(ctx, r.view.live.metricsURL, "go_goroutines", ""); err == nil {
			if connection, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", r.grpcPort), time.Second); dialErr == nil {
				_ = connection.Close()
				return
			}
		}
		select {
		case err := <-exited:
			r.cmd = nil
			t.Fatalf("%s attestor exited during startup: %v\n%s", r.name, err, tailFile(r.view.logPath, 40))
		default:
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, time.Second) {
			t.Fatalf("%s attestor did not open metrics and gRPC\n%s", r.name, tailFile(r.view.logPath, 40))
		}
	}
}

// stop delivers sig and waits for the process. It reports the exit error so a
// graceful stop can be checked for a clean exit.
func (r *faultRoute) stop(sig syscall.Signal) error {
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	_ = r.cmd.Process.Signal(sig)
	var err error
	select {
	case err = <-r.exited:
	case <-time.After(45 * time.Second):
		_ = r.cmd.Process.Kill()
		err = fmt.Errorf("did not exit within 45s of %s: %w", sig, <-r.exited)
	}
	r.cmd = nil
	return err
}

func (r *faultRoute) close() {
	_ = r.stop(syscall.SIGTERM)
	if r.conn != nil {
		_ = r.conn.Close()
	}
	for _, proxy := range []*faultProxy{r.l1, r.l1WS, r.replica, r.replicaWS} {
		if proxy != nil {
			proxy.close()
		}
	}
	if r.logFile != nil {
		_ = r.logFile.Close()
	}
}

func (r *faultRoute) frontier(t *testing.T, ctx context.Context) *attestorpb.AttestedRoot {
	t.Helper()
	response, err := r.client.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{SrcChain: r.env.srcChain, IncludeProvisional: true})
	if err != nil {
		t.Fatalf("AttestedUpTo during %s: %v", t.Name(), err)
	}
	if !response.GetFound() {
		return nil
	}
	return response.GetRoot()
}

func (r *faultRoute) waitFrontierAbove(t *testing.T, ctx context.Context, height uint64) *attestorpb.AttestedRoot {
	t.Helper()
	for {
		if root := r.frontier(t, ctx); root != nil && root.GetL2BlockNumber() > height {
			return root
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("frontier did not advance past %d before %v\n%s", height, ctx.Err(), tailFile(r.view.logPath, 30))
		}
	}
}

// waitServing waits for health to report SERVING. The host refreshes health on
// its own poll (attestor/host/host.go defaultHealthPoll), so the frontier can
// appear before the first refresh that sees a ready route. The bound is a few
// polls: a daemon that stays NOT_SERVING still fails. Every Check runs under
// the same bounded context, so a listener that accepts but never answers
// cannot hold the call open until the suite deadline.
func (r *faultRoute) waitServing(t *testing.T, ctx context.Context) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		response, err := r.health.Check(waitCtx, &healthpb.HealthCheckRequest{})
		if err == nil && response.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return
		}
		if !sleepCtx(waitCtx, 500*time.Millisecond) {
			t.Fatalf("health = %v (%v), want SERVING once the replica has been seen", response.GetStatus(), err)
		}
	}
}

// assertFrozen samples the frontier across window and requires it unchanged:
// a halted feed must keep serving its last persisted root, never drop it.
func (r *faultRoute) assertFrozen(t *testing.T, ctx context.Context, before *attestorpb.AttestedRoot, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		current := r.frontier(t, ctx)
		if current == nil || current.GetL2BlockNumber() < before.GetL2BlockNumber() {
			t.Fatalf("frontier regressed from %d to %v during the fault", before.GetL2BlockNumber(), current)
		}
		if current.GetL2BlockNumber() != before.GetL2BlockNumber() {
			t.Fatalf("frontier advanced %d -> %d although the fault should halt it", before.GetL2BlockNumber(), current.GetL2BlockNumber())
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatal(ctx.Err())
		}
	}
}

func (r *faultRoute) metric(t *testing.T, ctx context.Context, name string) float64 {
	t.Helper()
	value, err := fetchMetric(ctx, r.view.live.metricsURL, name, r.env.srcChain)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// waitMetricAbove waits for a counter to move past before, the observable
// proof that the daemon noticed the fault rather than idling through it.
func (r *faultRoute) waitMetricAbove(t *testing.T, ctx context.Context, name string, before float64) float64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if value := r.metric(t, ctx, name); value > before {
			return value
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("%s stayed at %v during the fault\n%s", name, before, tailFile(r.view.logPath, 30))
		}
	}
}

// logOffset is the current size of the route's log. The log is append-only
// across subtests and restarts, so a fault records the offset before it cuts
// and then matches only what was written after it.
func (r *faultRoute) logOffset(t *testing.T) int {
	t.Helper()
	return len(readLogOrFail(t, r.view))
}

// waitForLogLineAfter is waitForLogLine restricted to bytes written after
// offset, so a line an earlier fault produced cannot satisfy a later one.
func (r *faultRoute) waitForLogLineAfter(t *testing.T, ctx context.Context, offset int, line string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if log := readLogOrFail(t, r.view); len(log) >= offset && strings.Contains(log[offset:], line) {
			return
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, time.Second) {
			t.Fatalf("attestor log %s has no line containing %q after byte %d", r.view.logPath, line, offset)
		}
	}
}

func (r *faultRoute) verifyAt(ctx context.Context, header l2Header) (*attestorpb.VerifyStateRootResponse, error) {
	mode, _ := runModeForHead(r.head)
	key, err := decodeLivePublicKey(r.env.publicKey)
	if err != nil {
		return nil, err
	}
	setHash, err := liveSetHash(key)
	if err != nil {
		return nil, err
	}
	config := liveE2EConfig{sourceChain: r.env.srcChain, router: make([]byte, 20)}
	config.router[19] = 1
	return r.client.VerifyStateRoot(ctx, liveVerifyRequest(config, header, mode, setHash))
}

// ---------------------------------------------------------------- faults

func (r *faultRoute) assertReplicaOutage(t *testing.T, ctx context.Context) {
	t.Helper()
	before := r.waitFrontierAbove(t, ctx, 0)
	header, err := getL2Header(ctx, r.env.l2RPC, before.GetL2BlockNumber())
	if err != nil {
		t.Fatal(err)
	}
	if response, err := r.verifyAt(ctx, header); err != nil || !response.GetValid() {
		t.Fatalf("VerifyStateRoot before the outage = %v, %v; want a valid verdict", response, err)
	}

	failureMetric := "spectre_op_attestor_replica_errors_total"
	if r.env.family == familyArbitrum {
		failureMetric = "spectre_arbitrum_attestor_runtime_refresh_failures_total"
	}
	failuresBefore := r.metric(t, ctx, failureMetric)
	logMark := r.logOffset(t)
	r.replica.down()
	if r.replicaWS != nil {
		r.replicaWS.down()
	}
	t.Cleanup(func() { r.replica.up(); r.replicaWS.upIfSet() })
	cutAt := time.Now()

	failuresDuring := r.waitMetricAbove(t, ctx, failureMetric, failuresBefore)
	_, verifyErr := r.verifyAt(ctx, header)
	if status.Code(verifyErr) != codes.Unavailable {
		t.Fatalf("VerifyStateRoot during replica outage error = %v (%s), want Unavailable", verifyErr, status.Code(verifyErr))
	}
	frozen := r.frontier(t, ctx)
	if frozen == nil || frozen.GetL2BlockNumber() < before.GetL2BlockNumber() {
		t.Fatalf("frontier during outage = %v, want the persisted %d or later", frozen, before.GetL2BlockNumber())
	}
	r.assertFrozen(t, ctx, frozen, r.env.outage)
	// Known health-versus-liveness limitation: health and Info are computed
	// from the last successful replica snapshot, so a dead replica still reads
	// as SERVING with stale heads. Only VerifyStateRoot reflects the outage.
	healthDuring, healthErr := r.health.Check(ctx, &healthpb.HealthCheckRequest{})
	if healthErr != nil {
		t.Fatalf("health check during outage: %v", healthErr)
	}
	if healthDuring.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Logf("health changed to %s during the replica outage; update the documented limitation", healthDuring.GetStatus())
	}
	failureLine := "attestation pass failed (nothing affirmed, retrying in"
	if r.env.family == familyArbitrum {
		failureLine = "attestor runtime refresh failed"
	}
	r.waitForLogLineAfter(t, ctx, logMark, failureLine)

	r.replica.up()
	r.replicaWS.upIfSet()
	restoredAt := time.Now()
	recovered := r.waitFrontierAbove(t, ctx, frozen.GetL2BlockNumber())
	response, err := r.verifyAt(ctx, header)
	if err != nil || !response.GetValid() {
		t.Fatalf("VerifyStateRoot after recovery = %v, %v; want valid", response, err)
	}
	evidence(t, "replica cut %s: %s %v -> %v, VerifyStateRoot=Unavailable, frontier held at %d, health=%s (stale snapshot: liveness limitation); restored: frontier %d after %s, verdict valid",
		time.Since(cutAt).Round(time.Second), failureMetric, failuresBefore, failuresDuring, frozen.GetL2BlockNumber(),
		healthDuring.GetStatus(), recovered.GetL2BlockNumber(), time.Since(restoredAt).Round(time.Second))
}

// assertL1Outage cuts only L1 RPC. OP couples its derived feed to the ingest
// pass (runOnce ingests first and affirms nothing on failure), so the whole OP
// frontier halts. Arbitrum runs derived roots on the Nitro runtime loop,
// independently of RollupCore ingestion, so its frontier keeps moving while the
// assertion cursor holds.
func (r *faultRoute) assertL1Outage(t *testing.T, ctx context.Context) {
	t.Helper()
	before := r.waitFrontierAbove(t, ctx, 0)
	cursorBefore := r.cursor(t)
	logMark := r.logOffset(t)

	r.l1.down()
	t.Cleanup(r.l1.up)
	if r.env.family == familyArbitrum {
		const failureMetric = "spectre_arbitrum_attestor_assertion_sync_failures_total"
		failuresBefore := r.metric(t, ctx, failureMetric)
		failuresDuring := r.waitMetricAbove(t, ctx, failureMetric, failuresBefore)
		// Re-read the baseline only now. A scan that finished its L1 reads
		// before the cut may still have persisted the cursor after it. Scans
		// run one at a time and the counter moves only after a failed scan
		// returns, so every scan from here on started with L1 already cut.
		cursorBefore = r.cursor(t)
		advanced := r.waitFrontierAbove(t, ctx, before.GetL2BlockNumber())
		if cursor := r.cursor(t); cursor != cursorBefore {
			// The cursor only moves on a successful scan of finalized L1.
			t.Fatalf("assertion cursor moved %d -> %d with L1 unreachable", cursorBefore, cursor)
		}
		r.l1.up()
		cursorAfter := r.waitCursorAbove(t, ctx, cursorBefore)
		evidence(t, "L1 cut: %s %v -> %v, derived frontier kept advancing %d -> %d, assertion cursor held at %d; restored: cursor -> %d",
			failureMetric, failuresBefore, failuresDuring, before.GetL2BlockNumber(), advanced.GetL2BlockNumber(), cursorBefore, cursorAfter)
		return
	}

	// OP exports no L1 failure counter; the failed pass is logged with its cause.
	r.waitForLogLineAfter(t, ctx, logMark, "read finalized DisputeGameFactory view")
	frozen := r.frontier(t, ctx)
	r.assertFrozen(t, ctx, frozen, r.env.outage)
	if cursor := r.cursor(t); cursor < cursorBefore {
		t.Fatalf("game cursor regressed %d -> %d during the L1 outage", cursorBefore, cursor)
	}
	r.l1.up()
	restoredAt := time.Now()
	recovered := r.waitFrontierAbove(t, ctx, frozen.GetL2BlockNumber())
	evidence(t, "L1 cut: OP pass failed reading the finalized factory view, whole feed held at %d (derived roots depend on ingest); game cursor %d not regressed; restored: frontier %d after %s (backoff >= 1m)",
		frozen.GetL2BlockNumber(), cursorBefore, recovered.GetL2BlockNumber(), time.Since(restoredAt).Round(time.Second))
}

// assertWebSocketLoss cuts the only WebSocket each daemon uses. Neither is on
// the correctness path: OP's is a DisputeGameCreated wake hint, Arbitrum's a
// new-head trigger with a reconcile-interval poll behind it.
func (r *faultRoute) assertWebSocketLoss(t *testing.T, ctx context.Context) {
	t.Helper()
	proxy, errorLine := r.l1WS, "DisputeGameCreated hint subscription error (poll loop unaffected)"
	if r.env.family == familyArbitrum {
		proxy, errorLine = r.replicaWS, "Nitro new" // "subscription ended" and "subscribe ... failed"
	}
	before := r.waitFrontierAbove(t, ctx, 0)
	// On Arbitrum, R1 already cut this socket and logged errorLine, so only a
	// line written after this cut counts.
	logMark := r.logOffset(t)
	proxy.down()
	t.Cleanup(proxy.up)
	r.waitForLogLineAfter(t, ctx, logMark, errorLine)
	advanced := r.waitFrontierAbove(t, ctx, before.GetL2BlockNumber())

	proxy.up()
	// Recovery: the subscription is re-established, so no new error lines
	// appear across several reconnect periods (OP retries every 10s).
	settle := time.Now().Add(15 * time.Second)
	for time.Now().Before(settle) {
		sleepCtx(ctx, time.Second)
	}
	errorsAtRestore := strings.Count(readLogOrFail(t, r.view), errorLine)
	quiet := time.Now().Add(35 * time.Second)
	for time.Now().Before(quiet) {
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatal(ctx.Err())
		}
	}
	if errorsNow := strings.Count(readLogOrFail(t, r.view), errorLine); errorsNow != errorsAtRestore {
		t.Fatalf("WebSocket errors kept growing after restore (%d -> %d): subscription did not recover", errorsAtRestore, errorsNow)
	}
	after := r.waitFrontierAbove(t, ctx, advanced.GetL2BlockNumber())
	evidence(t, "WS cut: %q logged, frontier still advanced %d -> %d by polling; restored: %d error lines then none for 35s, frontier %d",
		errorLine, before.GetL2BlockNumber(), advanced.GetL2BlockNumber(), errorsAtRestore, after.GetL2BlockNumber())
}

// assertRestart stops the daemon with sig and restarts it on the same state
// file. Neither the served frontier (both policies) nor the durable ingest
// cursor may regress, and the restart must reuse the state rather than
// bootstrap afresh.
func (r *faultRoute) assertRestart(t *testing.T, ctx context.Context, sig syscall.Signal) {
	t.Helper()
	r.waitFrontierAbove(t, ctx, 0)
	provisional := r.frontier(t, ctx)
	confirmed, _ := r.confirmedFrontier(t, ctx)
	stopErr := r.stop(sig)
	if sig == syscall.SIGTERM && stopErr != nil {
		t.Fatalf("graceful stop exited with %v\n%s", stopErr, tailFile(r.view.logPath, 20))
	}
	state := readStateOrFail(t, r.view)
	cursorBefore := r.cursor(t)
	bootstrapsBefore := strings.Count(readLogOrFail(t, r.view), "bootstrapped ingest cursor")

	r.start(t, ctx)
	afterProvisional := r.waitFrontierAtLeast(t, ctx, provisional.GetL2BlockNumber())
	afterConfirmed, found := r.confirmedFrontier(t, ctx)
	if confirmed != nil && (!found || afterConfirmed.GetL2BlockNumber() < confirmed.GetL2BlockNumber()) {
		t.Fatalf("confirmed frontier regressed across restart: %d -> %v", confirmed.GetL2BlockNumber(), afterConfirmed)
	}
	if cursor := r.cursor(t); cursor < cursorBefore {
		t.Fatalf("durable cursor regressed across restart: %d -> %d", cursorBefore, cursor)
	}
	if r.env.family != familyArbitrum && strings.Count(readLogOrFail(t, r.view), "bootstrapped ingest cursor") != bootstrapsBefore {
		t.Fatal("restart re-bootstrapped the game cursor instead of loading the persisted state")
	}
	advanced := r.waitFrontierAbove(t, ctx, afterProvisional.GetL2BlockNumber())
	confirmedHeight := uint64(0)
	if confirmed != nil {
		confirmedHeight = confirmed.GetL2BlockNumber()
	}
	evidence(t, "%s: exit=%v; %d persisted entries, cursor %d; after restart: frontier %d >= %d, confirmed >= %d, cursor %d, feed advanced to %d",
		sig, stopErr, len(state.Attested), cursorBefore, afterProvisional.GetL2BlockNumber(), provisional.GetL2BlockNumber(),
		confirmedHeight, r.cursor(t), advanced.GetL2BlockNumber())
}

func (r *faultRoute) waitFrontierAtLeast(t *testing.T, ctx context.Context, height uint64) *attestorpb.AttestedRoot {
	t.Helper()
	if height == 0 {
		return r.waitFrontierAbove(t, ctx, 0)
	}
	return r.waitFrontierAbove(t, ctx, height-1)
}

func (r *faultRoute) confirmedFrontier(t *testing.T, ctx context.Context) (*attestorpb.AttestedRoot, bool) {
	t.Helper()
	response, err := r.client.AttestedUpTo(ctx, &attestorpb.AttestedUpToRequest{SrcChain: r.env.srcChain})
	if err != nil {
		t.Fatalf("AttestedUpTo(confirmed): %v", err)
	}
	return response.GetRoot(), response.GetFound()
}

// cursor is the durable settlement-ingest cursor: OP's next factory index or
// Arbitrum's next finalized L1 block.
func (r *faultRoute) cursor(t *testing.T) uint64 {
	t.Helper()
	state := readStateOrFail(t, r.view)
	if r.env.family == familyArbitrum {
		return state.NextL1Block
	}
	return state.NextGameIndex
}

func (r *faultRoute) waitCursorAbove(t *testing.T, ctx context.Context, before uint64) uint64 {
	t.Helper()
	for {
		if cursor := r.cursor(t); cursor > before {
			return cursor
		}
		if !sleepCtx(ctx, 2*time.Second) {
			t.Fatalf("cursor did not advance past %d after L1 recovered", before)
		}
	}
}

// assertReplicaLag runs a second, finalized-head route: its gating head trails
// the relayer's L2 RPC (the sequencer) by L1 finality, a lag the devnet
// produces on its own. A request for the sequencer's latest block must be
// refused as FailedPrecondition until the replica catches up, then signed.
func assertReplicaLag(t *testing.T, ctx context.Context, env faultEnv) {
	t.Helper()
	route := startFaultRoute(t, ctx, env, "lag", "finalized", env.grpcPort+10, env.metricsPort+10)
	var latest struct {
		Number string `json:"number"`
	}
	if err := liveRPC(ctx, env.l2RPC, "eth_getBlockByNumber", []any{"latest", false}, &latest); err != nil {
		t.Fatal(err)
	}
	target, err := strconv.ParseUint(strings.TrimPrefix(latest.Number, "0x"), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	header, err := getL2Header(ctx, env.l2RPC, target)
	if err != nil {
		t.Fatal(err)
	}
	chain := waitForReplica(t, ctx, route.client, env.srcChain)
	if chain.GetReplicaFinalizedL2() >= target {
		t.Fatalf("replica finalized head %d already covers sequencer head %d; no lag to observe", chain.GetReplicaFinalizedL2(), target)
	}
	_, verifyErr := route.verifyAt(ctx, header)
	if status.Code(verifyErr) != codes.FailedPrecondition {
		t.Fatalf("VerifyStateRoot(%d) with replica finalized %d: %v (%s), want FailedPrecondition",
			target, chain.GetReplicaFinalizedL2(), verifyErr, status.Code(verifyErr))
	}
	lagBlocks := target - chain.GetReplicaFinalizedL2()
	started := time.Now()
	for {
		response, err := route.verifyAt(ctx, header)
		if err == nil {
			if !response.GetValid() || len(response.GetAttestationSignature()) == 0 {
				t.Fatalf("caught-up verdict = %+v, want valid and signed", response)
			}
			break
		}
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("VerifyStateRoot while lagging: %v (%s), want FailedPrecondition until caught up", err, status.Code(err))
		}
		if !sleepCtx(ctx, 3*time.Second) {
			t.Fatalf("replica finalized head never reached %d", target)
		}
	}
	finalizedGauge := route.metric(t, ctx, "spectre_op_attestor_replica_finalized_head")
	if env.family == familyArbitrum {
		finalizedGauge = route.metric(t, ctx, "spectre_arbitrum_attestor_nitro_finalized_head")
	}
	evidence(t, "sequencer block %d was %d blocks ahead of the replica finalized head: FailedPrecondition, then valid signed verdict after %s (finalized gauge %v)",
		target, lagBlocks, time.Since(started).Round(time.Second), finalizedGauge)
}

// ----------------------------------------------------------- fault proxy

// faultProxy is a TCP relay to one upstream endpoint. down() closes every
// live connection and refuses new ones until up(); that is what a crashed or
// partitioned endpoint looks like to the daemon, for HTTP and WebSocket alike.
type faultProxy struct {
	url      string
	target   string
	listener net.Listener
	mu       sync.Mutex
	isDown   bool
	conns    map[net.Conn]struct{}
	dial     func(network, address string, timeout time.Duration) (net.Conn, error)
}

func newFaultProxy(t *testing.T, upstream string) *faultProxy {
	return newFaultProxyWithDial(t, upstream, net.DialTimeout)
}

func newFaultProxyWithDial(t *testing.T, upstream string, dial func(network, address string, timeout time.Duration) (net.Conn, error)) *faultProxy {
	t.Helper()
	parsed, err := url.Parse(upstream)
	if err != nil || parsed.Host == "" {
		t.Fatalf("proxy upstream %q is not a URL: %v", upstream, err)
	}
	target := parsed.Host
	if parsed.Port() == "" {
		port := "80"
		if parsed.Scheme == "https" || parsed.Scheme == "wss" {
			port = "443"
		}
		target = net.JoinHostPort(parsed.Hostname(), port)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxied := *parsed
	proxied.Host = listener.Addr().String()
	proxy := &faultProxy{
		url:      proxied.String(),
		target:   target,
		listener: listener,
		conns:    map[net.Conn]struct{}{},
		dial:     dial,
	}
	go proxy.serve()
	return proxy
}

func (p *faultProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.isDown
		p.mu.Unlock()
		if down {
			_ = client.Close()
			continue
		}
		upstream, err := p.dial("tcp", p.target, 5*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !p.track(client, upstream) {
			_ = client.Close()
			_ = upstream.Close()
			continue
		}
		go p.pipe(client, upstream)
		go p.pipe(upstream, client)
	}
}

// track atomically checks the outage state and registers both sides. A down()
// that races an upstream dial either closes the registered pair or makes this
// return false so serve closes the unregistered pair itself.
func (p *faultProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isDown {
		return false
	}
	for _, conn := range conns {
		p.conns[conn] = struct{}{}
	}
	return true
}

func (p *faultProxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

func (p *faultProxy) down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isDown = true
	for conn := range p.conns {
		_ = conn.Close()
	}
}

func (p *faultProxy) up() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isDown = false
}

func (p *faultProxy) upIfSet() {
	if p != nil {
		p.up()
	}
}

func (p *faultProxy) close() {
	p.down()
	_ = p.listener.Close()
}

// ------------------------------------------------------------- helpers

// fetchMetric returns one sample; chain may be empty for unlabelled metrics.
// An absent labelled counter reads as 0, matching scrapeMetricOrFail.
func fetchMetric(ctx context.Context, metricsURL, name, chain string) (float64, error) {
	request, err := httpGet(ctx, metricsURL)
	if err != nil {
		return 0, err
	}
	if chain == "" {
		for _, line := range strings.Split(request, "\n") {
			if strings.HasPrefix(line, name+" ") {
				return strconv.ParseFloat(strings.Fields(line)[1], 64)
			}
		}
		return 0, fmt.Errorf("metric %s not exported", name)
	}
	value, _, err := metricSample(request, name, chain)
	return value, err
}

func liveSetHash(publicKey []byte) ([]byte, error) {
	hash, err := (attestation.AttestorConfig{PublicKeys: [][]byte{publicKey}, Threshold: 1}).SetHash()
	if err != nil {
		return nil, err
	}
	return hash[:], nil
}

func tailFile(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return "--- log tail ---\n" + strings.Join(all, "\n")
}

var errMetricsStatus = errors.New("metrics endpoint returned a non-200 status")

func httpGet(ctx context.Context, target string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %s %s", errMetricsStatus, response.Status, bytes.TrimSpace(body))
	}
	return string(body), nil
}

// TestFaultProxyCutsAndRestores pins the proxy semantics the live suite relies
// on: a cut drops live connections and refuses new ones, and up() restores.
func TestFaultProxyCutsAndRestores(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			conn, acceptErr := upstream.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _, _ = io.Copy(conn, conn); _ = conn.Close() }()
		}
	}()
	proxy := newFaultProxy(t, "http://"+upstream.Addr().String())
	defer proxy.close()
	address := strings.TrimPrefix(proxy.url, "http://")

	roundTrip := func(conn net.Conn) error {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte("ping")); err != nil {
			return err
		}
		reply := make([]byte, 4)
		_, err := io.ReadFull(conn, reply)
		return err
	}

	live, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := roundTrip(live); err != nil {
		t.Fatalf("round trip through an up proxy: %v", err)
	}

	proxy.down()
	if err := roundTrip(live); err == nil {
		t.Fatal("an established connection survived down()")
	}
	refused, err := net.Dial("tcp", address)
	if err == nil {
		if roundTrip(refused) == nil {
			t.Fatal("a new connection worked while the proxy was down")
		}
		_ = refused.Close()
	}

	proxy.up()
	restored, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := roundTrip(restored); err != nil {
		t.Fatalf("round trip after up(): %v", err)
	}
}

// TestFaultProxyRefusesConnectionCutDuringUpstreamDial closes the gap between
// the first outage check and connection registration. A dial that completes
// after down() must not leave a usable relay connection behind.
func TestFaultProxyRefusesConnectionCutDuringUpstreamDial(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			conn, acceptErr := upstream.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _, _ = io.Copy(conn, conn); _ = conn.Close() }()
		}
	}()

	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	proxy := newFaultProxyWithDial(t, "http://"+upstream.Addr().String(), func(network, address string, timeout time.Duration) (net.Conn, error) {
		close(dialStarted)
		<-releaseDial
		return net.DialTimeout(network, address, timeout)
	})
	defer proxy.close()

	client, err := net.Dial("tcp", strings.TrimPrefix(proxy.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never started the delayed upstream dial")
	}
	proxy.down()
	close(releaseDial)

	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("ping")); err == nil {
		reply := make([]byte, 4)
		if _, err := io.ReadFull(client, reply); err == nil {
			t.Fatal("connection completed during the outage still forwarded traffic")
		}
	}
}
