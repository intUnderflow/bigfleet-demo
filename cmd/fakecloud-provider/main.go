// Command fakecloud-provider is the demo's three-cloud CapacityProvider. It
// composes the on-prem / AWS / GCP fake backends behind one providerkit.Server
// and serves the genuine bigfleet.v1alpha1.CapacityProvider gRPC contract, so a
// single BigFleet shard drives one heterogeneous inventory (committed BareMetal
// + Reserved, plus elastic OnDemand + Spot) and its real EffectiveCost engine
// routes across it. The substrate is simulated (kwok); the provider contract,
// the capacity declarations, and the cost math are real.
//
// Flags:
//
//	--addr            gRPC listen address (default :9100)
//	--state           durable state file (empty = in-memory)
//	--create-dwell    simulated provisioning latency per Create (default 0; node-creator owns the visible dwell)
//	--onprem-baremetal / --aws-reserved / --aws-ondemand / --aws-spot / --gcp-ondemand / --gcp-spot
//	                  per-tier sizes (0 = demo defaults 28/20/24/12/24/12).
//	                  hack/conformance.sh overrides the Speculative tiers large.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/intUnderflow/bigfleet-demo/providers/fakecloud"
	"github.com/intUnderflow/bigfleet-providers/providerkit"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fakecloud-provider exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", ":9100", "gRPC listen address")
		statePath = flag.String("state", "", "durable state file (empty = in-memory)")
		dwell     = flag.Duration("create-dwell", 0, "simulated provisioning latency per Create (0 = instant)")
		onprem    = flag.Int("onprem-baremetal", 0, "on-prem BareMetal Idle count (0 = default 28)")
		awsRes    = flag.Int("aws-reserved", 0, "AWS reserved Idle count (0 = default 20)")
		awsOD     = flag.Int("aws-ondemand", 0, "AWS on-demand Speculative count (0 = default 24)")
		awsSpot   = flag.Int("aws-spot", 0, "AWS spot Speculative count (0 = default 12)")
		gcpOD     = flag.Int("gcp-ondemand", 0, "GCP on-demand Speculative count (0 = default 24)")
		gcpSpot   = flag.Int("gcp-spot", 0, "GCP spot Speculative count (0 = default 12)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store, err := buildStore(*statePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	backend := fakecloud.New(fakecloud.Options{
		OnpremBareMetal: *onprem,
		AWSReserved:     *awsRes,
		AWSOnDemand:     *awsOD,
		AWSSpot:         *awsSpot,
		GCPOnDemand:     *gcpOD,
		GCPSpot:         *gcpSpot,
		CreateDwell:     *dwell,
	})

	srv, err := providerkit.New(backend, store, providerkit.Options{
		RequireZone: true, // AWS/GCP are multi-zone; every fake machine carries a zone
		Timeouts:    providerkit.Timeouts{Create: 5 * time.Minute},
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("build provider: %w", err)
	}

	gs := grpc.NewServer()
	srv.Register(gs)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		gs.GracefulStop()
	}()

	logger.Info("serving fakecloud CapacityProvider (onprem+aws+gcp, kwok-simulated)", "addr", lis.Addr().String())
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func buildStore(path string) (providerkit.Store, error) {
	if path == "" {
		return providerkit.NewMemStore(), nil
	}
	store, err := providerkit.NewFileStore(path)
	if err != nil {
		return nil, fmt.Errorf("open state file %s: %w", path, err)
	}
	return store, nil
}
