// Command academic-analytics runs the independent Analytics API and worker.
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

	"github.com/LDouble/campus-academic/internal/academicanalytics"
	"github.com/LDouble/campus-academic/internal/core/bootstrap"
	"github.com/LDouble/campus-academic/internal/infrastructure/metrics"
	academicrpc "github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/rpc"
	statisticsworker "github.com/LDouble/campus-academic/internal/modules/academic_statistics/worker"
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
		return fmt.Errorf("usage: academic-analytics [healthcheck]")
	}
	cfg, err := bootstrap.LoadAnalytics(configPath())
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	runtime, err := academicanalytics.Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = runtime.Close() }()
	listener, err := net.Listen("tcp", cfg.Analytics.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for academic analytics RPC: %w", err)
	}
	errCh := make(chan error, 4)
	go func() {
		runtime.Logger.Info("academic analytics started", zap.String("address", cfg.Analytics.ListenAddress))
		errCh <- runtime.Server.Serve(listener)
	}()
	go func() {
		errCh <- metrics.Serve(ctx, cfg.Observability.MetricsAddress, runtime.Metrics.Handler())
	}()
	go func() {
		errCh <- runtime.Worker.Run(runtime.Mux)
	}()
	go func() {
		errCh <- statisticsworker.Run(ctx, runtime.Manager, runtime.Location, cfg.Analytics.ScheduleHour, cfg.Analytics.RetryDelay, runtime.Logger)
	}()
	select {
	case <-ctx.Done():
		runtime.Server.GracefulStop()
		_ = listener.Close()
		return nil
	case err := <-errCh:
		if errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("academic analytics runtime stopped: %w", err)
	}
}

func runHealthcheck() error {
	cfg, err := bootstrap.LoadAnalytics(configPath())
	if err != nil {
		return err
	}
	options, err := academicrpc.AnalyticsClientDialOptions(cfg.Analytics)
	if err != nil {
		return err
	}
	connection, err := grpc.NewClient(cfg.Analytics.Target, options...)
	if err != nil {
		return fmt.Errorf("create analytics health client: %w", err)
	}
	defer func() { _ = connection.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := grpc_health_v1.NewHealthClient(connection).Check(
		ctx,
		&grpc_health_v1.HealthCheckRequest{Service: "academic.analytics.v1.AcademicAnalyticsService"},
	)
	if err != nil {
		return fmt.Errorf("check academic analytics health: %w", err)
	}
	if response.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("academic analytics is not serving")
	}
	return nil
}

func configPath() string {
	if value := os.Getenv("CAMPUS_ACADEMIC_BOOTSTRAP_FILE"); value != "" {
		return value
	}
	if value := os.Getenv("CAMPUS_BOOTSTRAP_FILE"); value != "" {
		return value
	}
	return "bootstrap.yaml"
}
