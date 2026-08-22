// Package academicanalytics assembles the independently deployed Analytics
// API, publication worker and service-owned storage.
package academicanalytics

import (
	"context"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/weouc-plus/campus-academic/internal/core/bootstrap"
	"github.com/weouc-plus/campus-academic/internal/infrastructure/logger"
	"github.com/weouc-plus/campus-academic/internal/infrastructure/metrics"
	"github.com/weouc-plus/campus-academic/internal/infrastructure/mysql"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/rpc"
	statisticsapp "github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/application"
	statisticsinfra "github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/infrastructure"
	statisticsrpc "github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/infrastructure/rpc"
	statisticsworker "github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/worker"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"gorm.io/gorm"
)

type Runtime struct {
	DB       *gorm.DB
	Source   *statisticsinfra.GradeAggregateSource
	Redis    *redis.Client
	Tasks    *asynq.Client
	Worker   *asynq.Server
	Manager  *statisticsapp.Manager
	Mux      *asynq.ServeMux
	Logger   *zap.Logger
	Metrics  *metrics.Registry
	Server   *grpc.Server
	Health   *health.Server
	Location *time.Location
	closed   bool
}

// Build creates the Analytics gRPC server and its direct task worker.
func Build(ctx context.Context, cfg bootstrap.Config) (*Runtime, error) {
	log, err := logger.New()
	if err != nil {
		return nil, fmt.Errorf("create academic analytics logger: %w", err)
	}
	runtime := &Runtime{Logger: log}
	initialized := false
	defer func() {
		if !initialized {
			_ = runtime.Close()
		}
	}()

	db, err := mysql.Open(ctx, cfg.Analytics.MySQL)
	if err != nil {
		return nil, err
	}
	runtime.DB = db
	source, err := statisticsinfra.NewGradeAggregateSource(cfg.Analytics.SourceDSN, cfg.Analytics.QueryTimeout)
	if err != nil {
		return nil, err
	}
	runtime.Source = source
	if err := source.Preflight(ctx); err != nil {
		return nil, fmt.Errorf("preflight academic grade source: %w", err)
	}
	locker, err := statisticsinfra.NewMySQLRunLocker(db)
	if err != nil {
		return nil, err
	}
	store := statisticsinfra.NewStore(db, cfg.Analytics.MinimumSampleSize)
	redisClient := redis.NewClient(&redis.Options{
		Addr: cfg.Analytics.Redis.Address, Password: cfg.Analytics.Redis.Password, DB: cfg.Analytics.Redis.DB,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		_ = redisClient.Close()
		return nil, fmt.Errorf("ping academic analytics Redis: %w", err)
	}
	runtime.Redis = redisClient
	taskClient := asynq.NewClient(asynq.RedisClientOpt{
		Addr: cfg.Analytics.Redis.Address, Password: cfg.Analytics.Redis.Password, DB: cfg.Analytics.Redis.DB,
	})
	runtime.Tasks = taskClient
	publisher := statisticsinfra.NewRunPublisher(taskClient, cfg.Analytics.TaskQueue, cfg.Analytics.QueryTimeout)
	manager := statisticsapp.NewManager(store).
		WithRunner(source, locker).
		WithRunPublisher(publisher)
	runtime.Manager = manager

	location, err := time.LoadLocation(cfg.Analytics.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load academic analytics timezone: %w", err)
	}
	runtime.Location = location
	metricsRegistry := metrics.New("academic-analytics", cfg.Release)
	runtime.Metrics = metricsRegistry
	serverOptions, err := rpc.AnalyticsServerOptions(cfg.Analytics)
	if err != nil {
		return nil, err
	}
	serverOptions = append(serverOptions, grpc.ChainUnaryInterceptor(metricsRegistry.UnaryServerInterceptor()))
	grpcServer := grpc.NewServer(serverOptions...)
	statisticsrpc.NewServer(manager, cfg.Analytics.MinimumSampleSize).Register(grpcServer)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("academic.analytics.v1.AcademicAnalyticsService", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	runtime.Server = grpcServer
	runtime.Health = healthServer

	mux := asynq.NewServeMux()
	statisticsworker.NewProcessor(manager).Register(mux)
	runtime.Mux = mux
	runtime.Worker = asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.Analytics.Redis.Address, Password: cfg.Analytics.Redis.Password, DB: cfg.Analytics.Redis.DB},
		asynq.Config{
			Concurrency:    cfg.Analytics.WorkerConcurrency,
			Queues:         map[string]int{cfg.Analytics.TaskQueue: 1},
			RetryDelayFunc: statisticsworker.TaskRetryDelay,
		},
	)
	initialized = true
	return runtime, nil
}

func (runtime *Runtime) Close() error {
	if runtime == nil || runtime.closed {
		return nil
	}
	runtime.closed = true
	if runtime.Health != nil {
		runtime.Health.Shutdown()
	}
	if runtime.Worker != nil {
		runtime.Worker.Shutdown()
	}
	var first error
	if runtime.Tasks != nil {
		if err := runtime.Tasks.Close(); err != nil {
			first = err
		}
	}
	if runtime.Source != nil {
		if err := runtime.Source.Close(); err != nil && first == nil {
			first = err
		}
	}
	if runtime.Redis != nil {
		if err := runtime.Redis.Close(); err != nil && first == nil {
			first = err
		}
	}
	if runtime.DB != nil {
		if db, err := runtime.DB.DB(); err == nil {
			if closeErr := db.Close(); closeErr != nil && first == nil {
				first = closeErr
			}
		}
	}
	if runtime.Logger != nil {
		if err := runtime.Logger.Sync(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
