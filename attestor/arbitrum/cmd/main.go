package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"attestor/arbitrum"
	"attestor/arbitrum/adapter"
	boldattestor "attestor/arbitrum/bold"
	"attestor/core"
	"attestor/host"
	attestortypes "attestor/types"
	"attestor/types/attestation"
	"attestor/types/redact"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := execute(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		// A startup failure on an RPC endpoint quotes the endpoint URL, whose
		// path or query can hold the provider's API key.
		log.Fatal(redact.Error(err))
	}
}

func runAttestor(ctx context.Context, configPath string) error {
	config, err := arbitrum.LoadDaemonConfig(configPath)
	if err != nil {
		return err
	}

	nitroClient, err := attestortypes.DialEthRPC(ctx, config.NitroRPCURL, attestortypes.DefaultRPCTimeout)
	if err != nil {
		return fmt.Errorf("connect to Nitro RPC: %w", err)
	}
	defer nitroClient.Close()
	if err := validateNitroChainID(ctx, nitroClient, config.L2ChainID, "RPC"); err != nil {
		return err
	}

	nitroWSClient, err := ethclient.DialContext(ctx, config.NitroWSURL)
	if err != nil {
		return fmt.Errorf("connect to Nitro WebSocket: %w", redact.Error(err))
	}
	defer nitroWSClient.Close()
	if err := validateNitroChainID(ctx, nitroWSClient, config.L2ChainID, "WebSocket"); err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	metrics := arbitrum.NewMetrics(registry)
	signer, err := attestation.NewSigner(config.L2ChainID, config.AttestationSigningKey)
	if err != nil {
		return fmt.Errorf("load attestation signing key: %w", err)
	}
	publicKey := signer.PublicKey()
	log.Printf("Arbitrum attestation public key: 0x%x", publicKey)
	log.Printf(
		"Arbitrum attestation public key (base64 for relayer attestors.public_keys): %s",
		base64.StdEncoding.EncodeToString(publicKey),
	)

	runtimeState, err := arbitrum.NewRuntimeStateWithConfig(nitroClient, arbitrum.RuntimeStateConfig{
		BackfillMaxBlocks:   config.BackfillMaxBlocks(),
		BackfillConcurrency: int(config.BackfillConcurrency()),
	})
	if err != nil {
		return fmt.Errorf("initialize Nitro runtime state: %w", err)
	}
	runtimePollInterval, err := config.RuntimePollDuration()
	if err != nil {
		return fmt.Errorf("load runtime poll interval: %w", err)
	}
	l1Client, err := attestortypes.DialEthRPC(ctx, config.L1RPCURL, attestortypes.DefaultRPCTimeout)
	if err != nil {
		return fmt.Errorf("connect to Ethereum L1 RPC: %w", err)
	}
	defer l1Client.Close()
	assertionSource, err := newAssertionSource(config, l1Client)
	if err != nil {
		return fmt.Errorf("initialize RollupCore source: %w", err)
	}
	attestedRootStore, err := arbitrum.LoadAttestedRootStore(
		config.AttestorStatePath,
		config.SrcChain,
		config.AssertionStartBlock,
	)
	if err != nil {
		return fmt.Errorf("load attested-root state: %w", err)
	}
	attestedRootStore.SetMetrics(metrics)
	metrics.ObserveStore(config.SrcChain, attestedRootStore.Stats())
	assertionPollInterval, err := config.AssertionPollDuration()
	if err != nil {
		return fmt.Errorf("load assertion poll interval: %w", err)
	}
	assertionCallTimeout, err := config.AssertionCallTimeoutDuration()
	if err != nil {
		return fmt.Errorf("load assertion call timeout: %w", err)
	}
	assertionLoop, err := boldattestor.NewAssertionAttestor(
		assertionSource,
		runtimeState,
		attestedRootStore,
		boldattestor.AssertionAttestorConfig{
			L1IngestHead:    config.L1IngestHead,
			PollInterval:    assertionPollInterval,
			CallTimeout:     assertionCallTimeout,
			MaxL1BlockRange: config.AssertionBlockRange(),
		},
	)
	if err != nil {
		return fmt.Errorf("initialize Arbitrum assertion attestor: %w", err)
	}
	if err := assertionLoop.ValidateChainIDs(ctx, config.L1ChainID, config.L2ChainID); err != nil {
		return fmt.Errorf("validate Arbitrum deployment identity: %w", err)
	}
	sourceIdentity := assertionSourceIdentity(config)
	if err := attestedRootStore.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
		return true, store.BindSourceIdentity(sourceIdentity)
	}); err != nil {
		return fmt.Errorf("bind and persist attested-root source identity: %w", err)
	}
	if err := assertionLoop.ReconcileStartup(ctx); err != nil {
		return err
	}
	attestationHead, err := config.NormalizedAttestationHead()
	if err != nil {
		return fmt.Errorf("load attestation head: %w", err)
	}
	derivedAttestor, err := arbitrum.NewDerivedRootAttestor(
		runtimeState,
		attestedRootStore,
		arbitrum.DerivedAttestorConfig{
			AttestationHead: attestationHead,
			Disabled:        config.DisableDerivedRoots,
			GapBlocks:       config.DerivedAttestationGap(),
			MaxRoots:        config.DerivedRootLimit(),
		},
	)
	if err != nil {
		return fmt.Errorf("initialize Arbitrum derived-root attestor: %w", err)
	}
	bridge, err := adapter.New(adapter.Options{
		SrcChain:        config.SrcChain,
		Runtime:         runtimeState,
		Feed:            attestedRootStore,
		AttestationHead: attestationHead,
		Signer:          signer,
		RunnerAlive:     assertionLoop.RunnerAlive,
	})
	if err != nil {
		return fmt.Errorf("compose Arbitrum attestor bridge: %w", err)
	}
	listener, err := net.Listen("tcp", config.GRPCListenAddress)
	if err != nil {
		return fmt.Errorf("listen for attestor gRPC at %s: %w", config.GRPCListenAddress, err)
	}
	defer listener.Close()
	metricsListener, err := net.Listen("tcp", config.MetricsListenAddress)
	if err != nil {
		return fmt.Errorf("listen for attestor metrics at %s: %w", config.MetricsListenAddress, err)
	}
	defer metricsListener.Close()
	metricsServer := &http.Server{
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if serveErr := metricsServer.Serve(metricsListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("attestor metrics server failed: %v", serveErr)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsServer.Shutdown(shutdownCtx)
	}()

	attestorHost, err := host.New(host.Config{
		Routes: map[string]core.Ports{
			config.SrcChain: {Feed: bridge, Verifier: bridge, Status: bridge},
		},
		Runners: []host.RunnerSpec{
			{
				Name: "nitro-runtime-monitor",
				Runner: host.RunnerFunc(func(runCtx context.Context) error {
					return arbitrum.MonitorRuntimeState(
						runCtx,
						runtimeAndDerivedRefresher{
							runtime: runtimeState,
							derived: derivedAttestor,
							store:   attestedRootStore,
							metrics: metrics,
						},
						nitroWSClient,
						arbitrum.RuntimeMonitorConfig{ReconcileInterval: runtimePollInterval},
					)
				}),
			},
			{
				Name: "arbitrum-assertion-attestor",
				Runner: host.RunnerFunc(func(runCtx context.Context) error {
					// Assertion backfill can be large; its loop retries transient L1
					// failures without taking the sidecar down.
					assertionLoop.RunWithObserver(runCtx, func(syncErr error) {
						if syncErr != nil {
							metrics.ObserveAssertionFailure(config.SrcChain)
						}
						metrics.ObserveStore(config.SrcChain, attestedRootStore.Stats())
					})
					return nil
				}),
			},
		},
		Listener: listener,
		ErrorLog: log.Printf,
	})
	if err != nil {
		return fmt.Errorf("initialize attestor host: %w", err)
	}
	log.Printf("attestor gRPC listening at %s", listener.Addr())
	log.Printf("attestor metrics listening at %s/metrics", metricsListener.Addr())
	if err := attestorHost.Run(ctx); err != nil {
		return fmt.Errorf("run attestor host: %w", err)
	}
	return nil
}

type chainIDReader interface {
	ChainID(context.Context) (*big.Int, error)
}

func validateNitroChainID(
	ctx context.Context,
	client chainIDReader,
	expected uint64,
	transport string,
) error {
	observed, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("query Nitro %s chain ID: %w", transport, err)
	}
	if observed == nil || !observed.IsUint64() || observed.Uint64() != expected {
		return fmt.Errorf(
			"Nitro %s chain ID is %v, expected %d",
			transport,
			observed,
			expected,
		)
	}
	return nil
}

func newAssertionSource(
	config arbitrum.DaemonConfig,
	client boldattestor.L1Client,
) (boldattestor.AssertionSource, error) {
	address := common.HexToAddress(config.RollupCoreAddress)
	return boldattestor.NewRollupCoreSource(
		client,
		address,
		common.HexToHash(config.AssertionsMappingSlot),
		config.AssertionStatusOffset,
	)
}

func assertionSourceIdentity(config arbitrum.DaemonConfig) string {
	address := common.HexToAddress(config.RollupCoreAddress).Hex()
	return fmt.Sprintf(
		"l1:%d/l2:%d/rollup:%s/assertions:%s/status-offset:%d",
		config.L1ChainID,
		config.L2ChainID,
		address,
		common.HexToHash(config.AssertionsMappingSlot).Hex(),
		config.AssertionStatusOffset,
	)
}

type runtimeStateRefresher interface {
	Refresh(context.Context) ([]arbitrum.FinalizedConsistency, error)
}

type derivedRootSyncer interface {
	SyncOnce(context.Context) error
}

type runtimeAndDerivedRefresher struct {
	runtime *arbitrum.RuntimeState
	derived derivedRootSyncer
	store   *arbitrum.AttestedRootStore
	metrics *arbitrum.Metrics
}

func (r runtimeAndDerivedRefresher) Refresh(
	ctx context.Context,
) ([]arbitrum.FinalizedConsistency, error) {
	checks, err := r.runtime.Refresh(ctx)
	if err != nil {
		r.metrics.ObserveRuntimeFailure(r.store.SrcChain())
		return nil, err
	}
	r.metrics.ObserveRuntime(r.store.SrcChain(), r.runtime.Snapshot(), checks)
	if err := r.derived.SyncOnce(ctx); err != nil {
		r.metrics.ObserveDerivedFailure(r.store.SrcChain())
		r.metrics.ObserveStore(r.store.SrcChain(), r.store.Stats())
		return nil, err
	}
	r.metrics.ObserveStore(r.store.SrcChain(), r.store.Stats())
	return checks, nil
}
