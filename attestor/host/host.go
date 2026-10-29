// Package host owns shared attestor lifecycle, gRPC health, and graceful
// shutdown. Plugins only implement core ports/runners and never open the
// common AttestorService listener themselves.
package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"attestor/core"
	attestorgrpc "attestor/grpc"
	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const (
	defaultHealthPoll       = time.Second
	defaultGracefulStopWait = 10 * time.Second
)

// RunnerFunc adapts a function to a core Runner.
type RunnerFunc func(context.Context) error

func (f RunnerFunc) Run(ctx context.Context) error { return f(ctx) }

// RunnerSpec identifies a background plugin loop supervised by the host.
type RunnerSpec struct {
	Name   string
	Runner core.Runner
}

// Config is supplied only by a composition root. Listener may be nil for a
// deployment that intentionally disables gRPC while retaining lifecycle
// supervision of its plugins.
type Config struct {
	Routes              map[string]core.Ports
	Runners             []RunnerSpec
	Listener            net.Listener
	EnableReflection    bool
	HealthPoll          time.Duration
	GracefulStopTimeout time.Duration
	// ErrorLog receives the cause of every error the gRPC server answers with
	// only a classified message, URLs redacted. Nil discards them.
	ErrorLog func(format string, args ...any)
}

// Host owns a shared gRPC server and runner supervisor.
type Host struct {
	service     *attestorgrpc.Server
	grpcServer  *googlegrpc.Server
	health      *health.Server
	routes      map[string]core.Ports
	runners     []RunnerSpec
	listener    net.Listener
	healthPoll  time.Duration
	stopTimeout time.Duration
}

// New validates lifecycle wiring and constructs a shared server. The route
// map is copied by attestor/grpc; host keeps only the read-only ports needed
// to aggregate readiness.
func New(config Config) (*Host, error) {
	if len(config.Routes) == 0 {
		return nil, fmt.Errorf("attestor host requires at least one route")
	}
	service, err := attestorgrpc.New(config.Routes, attestorgrpc.WithErrorLog(config.ErrorLog))
	if err != nil {
		return nil, err
	}
	for index, runner := range config.Runners {
		if runner.Name == "" {
			return nil, fmt.Errorf("runner %d name must not be empty", index)
		}
		if runner.Runner == nil {
			return nil, fmt.Errorf("runner %q must not be nil", runner.Name)
		}
	}
	healthPoll := config.HealthPoll
	if healthPoll == 0 {
		healthPoll = defaultHealthPoll
	}
	if healthPoll <= 0 {
		return nil, fmt.Errorf("health poll interval must be greater than zero")
	}
	stopTimeout := config.GracefulStopTimeout
	if stopTimeout == 0 {
		stopTimeout = defaultGracefulStopWait
	}
	if stopTimeout <= 0 {
		return nil, fmt.Errorf("graceful stop timeout must be greater than zero")
	}

	grpcServer := googlegrpc.NewServer()
	service.Register(grpcServer)
	if config.EnableReflection {
		reflection.Register(grpcServer)
	}
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	healthServer.SetServingStatus(attestorpb.AttestorService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)

	routes := make(map[string]core.Ports, len(config.Routes))
	for srcChain, ports := range config.Routes {
		routes[srcChain] = ports
	}
	return &Host{
		service:     service,
		grpcServer:  grpcServer,
		health:      healthServer,
		routes:      routes,
		runners:     append([]RunnerSpec(nil), config.Runners...),
		listener:    config.Listener,
		healthPoll:  healthPoll,
		stopTimeout: stopTimeout,
	}, nil
}

// Service exposes the shared adapter for in-memory contract tests. Commands
// should use Run rather than registering it themselves.
func (h *Host) Service() *attestorgrpc.Server { return h.service }

// GRPCServer exposes the server for in-memory contract tests. Commands should
// not call Serve directly.
func (h *Host) GRPCServer() *googlegrpc.Server { return h.grpcServer }

// Run starts every runner, optionally serves gRPC, continuously aggregates
// readiness, and shuts all components down if a runner exits unexpectedly.
func (h *Host) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	serveDone := make(chan error, 1)
	if h.listener != nil {
		go func() { serveDone <- h.grpcServer.Serve(h.listener) }()
	}

	runnerDone := make(chan error, len(h.runners))
	var runners sync.WaitGroup
	for _, spec := range h.runners {
		runners.Add(1)
		go func(spec RunnerSpec) {
			defer runners.Done()
			err := spec.Runner.Run(runCtx)
			if runCtx.Err() == nil {
				if err == nil {
					err = fmt.Errorf("runner %q stopped unexpectedly", spec.Name)
				} else {
					err = fmt.Errorf("runner %q failed: %w", spec.Name, err)
				}
				select {
				case runnerDone <- err:
				case <-runCtx.Done():
				}
			}
		}(spec)
	}

	healthDone := make(chan struct{})
	go func() {
		defer close(healthDone)
		h.monitorReadiness(runCtx)
	}()

	var result error
	select {
	case err := <-runnerDone:
		result = err
	case err := <-serveDone:
		if !errors.Is(err, googlegrpc.ErrServerStopped) {
			result = err
		}
	case <-ctx.Done():
		result = ctx.Err()
	}

	cancel()
	// monitorReadiness may have been in Status while cancellation began. Wait
	// for it to exit before publishing the terminal state, then keep gRPC up
	// long enough for active health-watch clients to receive NOT_SERVING during
	// the graceful drain.
	<-healthDone
	h.setServing(false)
	if h.listener != nil {
		h.stopGRPC()
	}
	runners.Wait()
	if errors.Is(result, context.Canceled) {
		return nil
	}
	return result
}

func (h *Host) monitorReadiness(ctx context.Context) {
	check := func() {
		h.setServing(h.ready(ctx))
	}
	check()
	ticker := time.NewTicker(h.healthPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func (h *Host) ready(ctx context.Context) bool {
	for _, ports := range h.routes {
		status, err := ports.Status.Status(ctx)
		if err != nil || !status.Ready {
			return false
		}
	}
	return true
}

func (h *Host) setServing(serving bool) {
	state := healthpb.HealthCheckResponse_NOT_SERVING
	if serving {
		state = healthpb.HealthCheckResponse_SERVING
	}
	h.health.SetServingStatus("", state)
	h.health.SetServingStatus(attestorpb.AttestorService_ServiceDesc.ServiceName, state)
}

func (h *Host) stopGRPC() {
	done := make(chan struct{})
	go func() {
		h.grpcServer.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(h.stopTimeout):
		h.grpcServer.Stop()
		<-done
	}
}
