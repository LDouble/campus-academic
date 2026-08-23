// Command academic-provider runs the isolated school-system integration service.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LDouble/campus-academic/internal/academicprovider"
	"github.com/LDouble/campus-academic/internal/core/bootstrap"
	observability "github.com/LDouble/campus-academic/internal/infrastructure/metrics"
	academicrpc "github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/rpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		return runHealthcheck()
	}
	if len(os.Args) != 1 {
		return fmt.Errorf("usage: academic-provider [healthcheck]")
	}
	cfg, err := bootstrap.LoadProvider(configPath())
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	runtime, err := academicprovider.Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()
	listener, err := net.Listen("tcp", cfg.Provider.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for academic provider RPC: %w", err)
	}
	errCh := make(chan error, 2)
	go func() {
		runtime.Logger.Info(
			"academic provider started",
			zap.String("address", cfg.Provider.ListenAddress),
		)
		errCh <- runtime.Server.Serve(listener)
	}()
	go func() {
		runtime.Logger.Info("provider metrics server started", zap.String("address", cfg.Observability.MetricsAddress))
		errCh <- observability.Serve(ctx, cfg.Observability.MetricsAddress, runtime.Metrics.Handler())
	}()
	select {
	case <-ctx.Done():
		return gracefulStop(runtime, listener)
	case err = <-errCh:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return fmt.Errorf("serve academic provider RPC: %w", err)
	}
}

func runHealthcheck() error {
	cfg, err := bootstrap.LoadProvider(configPath())
	if err != nil {
		return err
	}
	options, err := academicrpc.ClientDialOptions(cfg.Provider)
	if err != nil {
		return err
	}
	connection, err := grpc.NewClient(cfg.Provider.Target, options...)
	if err != nil {
		return fmt.Errorf("create provider health client: %w", err)
	}
	defer func() { _ = connection.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := grpc_health_v1.NewHealthClient(connection).Check(
		ctx,
		&grpc_health_v1.HealthCheckRequest{Service: "academic.provider.v1.AcademicProviderService"},
	)
	if err != nil {
		return fmt.Errorf("check academic provider health: %w", err)
	}
	if response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("academic provider is not serving")
	}
	return nil
}

func gracefulStop(runtime *academicprovider.Runtime, listener net.Listener) error {
	if runtime.Health != nil {
		runtime.Health.Shutdown()
	}
	stopped := make(chan struct{})
	go func() {
		runtime.Server.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		return listener.Close()
	case <-time.After(10 * time.Second):
		runtime.Server.Stop()
		return listener.Close()
	}
}

func configPath() string {
	if value := os.Getenv("CAMPUS_BOOTSTRAP_FILE"); value != "" {
		return value
	}
	return "bootstrap.yaml"
}
