// Package academicprovider assembles the isolated school-system integration runtime.
package academicprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/weouc-plus/campus-academic/internal/configfile"
	"github.com/weouc-plus/campus-academic/internal/core/bootstrap"
	"github.com/weouc-plus/campus-academic/internal/core/configcenter"
	"github.com/weouc-plus/campus-academic/internal/core/ratelimitconfig"
	"github.com/weouc-plus/campus-academic/internal/infrastructure/logger"
	observability "github.com/weouc-plus/campus-academic/internal/infrastructure/metrics"
	"github.com/weouc-plus/campus-academic/internal/infrastructure/redisclient"
	academicinfra "github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/ouc"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/providerrouter"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/querycoord"
	academicrpc "github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/rpc"
	verificationapp "github.com/weouc-plus/campus-academic/internal/modules/academic_verification/application"
	verificationinfra "github.com/weouc-plus/campus-academic/internal/modules/academic_verification/infrastructure"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// Runtime owns the dependencies used only by the private academic provider.
type Runtime struct {
	Redis   *redis.Client
	Logger  *zap.Logger
	Config  *academicconfig.Resolver
	Limits  *ratelimitconfig.Resolver
	Server  *grpc.Server
	Health  *health.Server
	Metrics *observability.Registry
	OUC     *ouc.Provider
	closed  bool
}

// Build initializes the provider router, encrypted session cache and gRPC server.
func Build(ctx context.Context, cfg bootstrap.Config) (*Runtime, error) {
	log, err := logger.New()
	if err != nil {
		return nil, fmt.Errorf("create academic provider logger: %w", err)
	}
	runtime := &Runtime{Logger: log}
	initialized := false
	defer func() {
		if !initialized {
			_ = runtime.Close()
		}
	}()
	rdb, err := redisclient.Open(ctx, cfg.Redis)
	if err != nil {
		return nil, err
	}
	runtime.Redis = rdb
	cipher, err := configcenter.NewCipher(cfg.Secret.AcademicProviderKey)
	if err != nil {
		return nil, err
	}
	configSource, err := configfile.New(cfg.ProviderConfigFile)
	if err != nil {
		return nil, err
	}
	rateLimits, err := ratelimitconfig.NewResolver(ctx, configSource, 30*time.Second, log)
	if err != nil {
		return nil, fmt.Errorf("init rate-limit config: %w", err)
	}
	runtime.Limits = rateLimits
	rateLimits.Start(ctx)
	resolver, err := academicconfig.NewResolver(
		ctx,
		configSource,
		time.Minute,
		academicconfig.ProviderPolicy{
			AllowMock: cfg.AllowsAcademicMock(),
			AllowOUC:  cfg.AllowsAcademicOUC(),
		},
		log,
	)
	if err != nil {
		return nil, fmt.Errorf("init academic provider config: %w", err)
	}
	runtime.Config = resolver
	resolver.Start(ctx)
	metricsRegistry := observability.New("academic-provider", cfg.Release)
	runtime.Metrics = metricsRegistry
	sessions, err := ouc.NewEncryptedRedisSessionStore(
		rdb,
		cipher,
		cfg.Secret.AcademicProviderKey,
	)
	if err != nil {
		return nil, fmt.Errorf("init encrypted OUC sessions: %w", err)
	}
	oucProvider := ouc.NewProvider(
		resolver,
		ouc.WithHTTPProxy(cfg.Provider.HTTPProxyURL),
		ouc.WithSessionStore(sessions),
		ouc.WithLogger(log),
		ouc.WithObserver(metricsRegistry),
	)
	runtime.OUC = oucProvider
	coordinatedOUC, err := querycoord.New(
		oucProvider,
		rdb,
		cfg.Secret.AcademicQueryKey,
		querycoord.Config{
			CacheMode:                   cfg.AcademicQuery.CacheMode,
			CoursesTimeout:              cfg.AcademicQuery.CoursesTimeout,
			GradesTimeout:               cfg.AcademicQuery.GradesTimeout,
			ExamsTimeout:                cfg.AcademicQuery.ExamsTimeout,
			SelectionsTimeout:           cfg.AcademicQuery.SelectionsTimeout,
			StaleRefreshTimeout:         cfg.AcademicQuery.StaleRefreshTimeout,
			CircuitThreshold:            cfg.AcademicQuery.CircuitFailureThreshold,
			CircuitWindow:               cfg.AcademicQuery.CircuitWindow,
			CircuitOpenDuration:         cfg.AcademicQuery.CircuitOpenDuration,
			CircuitMinimumSamples:       cfg.AcademicQuery.CircuitMinimumSamples,
			CircuitDeadlineThreshold:    cfg.AcademicQuery.CircuitDeadlineThreshold,
			CircuitDeadlineRatio:        cfg.AcademicQuery.CircuitDeadlineRatio,
			CircuitHardProtectionCount:  cfg.AcademicQuery.CircuitHardProtectionCount,
			CircuitHardProtectionWindow: cfg.AcademicQuery.CircuitHardProtectionWindow,
			LeaseTTL:                    cfg.AcademicQuery.LeaseTTL,
			PollInterval:                cfg.AcademicQuery.PollInterval,
			MaxConcurrent:               cfg.Provider.MaxConcurrent,
			GlobalRate:                  cfg.AcademicQuery.GlobalRate,
			GlobalBurst:                 cfg.AcademicQuery.GlobalBurst,
			RateLimits:                  rateLimits,
			RetryAfter:                  cfg.Provider.RetryAfter,
			CoursesFreshTTL:             cfg.AcademicQuery.CoursesFreshTTL,
			CoursesStaleTTL:             cfg.AcademicQuery.CoursesStaleTTL,
			GradesFreshTTL:              cfg.AcademicQuery.GradesFreshTTL,
			GradesStaleTTL:              cfg.AcademicQuery.GradesStaleTTL,
			ExamsFreshTTL:               cfg.AcademicQuery.ExamsFreshTTL,
			ExamsStaleTTL:               cfg.AcademicQuery.ExamsStaleTTL,
			SelectionsFreshTTL:          cfg.AcademicQuery.SelectionsFreshTTL,
			SelectionsStaleTTL:          cfg.AcademicQuery.SelectionsStaleTTL,
		},
		log,
		querycoord.WithObserver(metricsRegistry),
	)
	if err != nil {
		return nil, fmt.Errorf("init academic query coordinator: %w", err)
	}
	metricsRegistry.SetAcademicQueryCacheMode(cfg.AcademicQuery.CacheMode)
	if cfg.AcademicQuery.CacheMode != "normal" {
		log.Warn("academic query cache mode enabled", zap.String("cache_mode", cfg.AcademicQuery.CacheMode))
	}
	var mockVerification verificationapp.Provider
	if cfg.AllowsAcademicMock() {
		mockVerification = verificationinfra.NewDynamicMockProvider(resolver)
	}
	router := providerrouter.New(
		resolver,
		academicinfra.NewMockProvider(),
		mockVerification,
		coordinatedOUC,
		oucProvider,
	)
	serverOptions, err := academicrpc.ServerOptions(cfg.Provider)
	if err != nil {
		return nil, err
	}
	serverOptions = append(serverOptions, grpc.ChainUnaryInterceptor(metricsRegistry.UnaryServerInterceptor()))
	grpcServer := grpc.NewServer(serverOptions...)
	providerServer := academicrpc.NewServer(
		router,
		router,
		querycoord.CombineRevokers(sessions, coordinatedOUC),
		cfg.Provider.MaxConcurrent,
		cfg.Provider.QueueWait,
		cfg.Provider.RetryAfter,
	).WithCourseCatalog(oucProvider)
	providerServer.Register(grpcServer)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus(
		"academic.provider.v1.AcademicProviderService",
		grpc_health_v1.HealthCheckResponse_SERVING,
	)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	runtime.Server = grpcServer
	runtime.Health = healthServer
	initialized = true
	return runtime, nil
}

// Close releases provider-owned resources.
func (r *Runtime) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	var first error
	if r.Health != nil {
		r.Health.Shutdown()
	}
	if r.Config != nil {
		r.Config.Stop()
	}
	if r.Limits != nil {
		r.Limits.Stop()
	}
	if r.OUC != nil {
		if err := r.OUC.Close(); err != nil && first == nil {
			first = err
		}
	}
	if r.Redis != nil {
		if err := r.Redis.Close(); err != nil && first == nil {
			first = err
		}
	}
	if r.Logger != nil {
		if err := r.Logger.Sync(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
