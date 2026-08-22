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

	"github.com/weouc-plus/campus-academic/internal/academicanalytics"
	"github.com/weouc-plus/campus-academic/internal/core/bootstrap"
	"github.com/weouc-plus/campus-academic/internal/infrastructure/metrics"
	statisticsworker "github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/worker"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 {
		return fmt.Errorf("usage: academic-analytics")
	}
	cfg, err := bootstrap.Load(configPath())
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
		errCh <- statisticsworker.Run(ctx, runtime.Manager, runtime.Location, cfg.Analytics.ScheduleHour, runtime.Logger)
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

func configPath() string {
	if value := os.Getenv("CAMPUS_ACADEMIC_BOOTSTRAP_FILE"); value != "" {
		return value
	}
	if value := os.Getenv("CAMPUS_BOOTSTRAP_FILE"); value != "" {
		return value
	}
	return "bootstrap.yaml"
}
