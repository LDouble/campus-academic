// Package metrics exposes low-cardinality metrics for the two academic
// services without importing the platform's application metrics registry.
package metrics

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// Registry owns one process-local Prometheus registry.
type Registry struct {
	service              string
	gather               *prometheus.Registry
	grpcReq              *prometheus.CounterVec
	grpcDur              *prometheus.HistogramVec
	academicCache        *prometheus.CounterVec
	academicQuery        *prometheus.CounterVec
	academicUpstream     *prometheus.CounterVec
	academicCircuit      *prometheus.CounterVec
	academicSingleflight *prometheus.CounterVec
	academicSession      *prometheus.CounterVec
	academicRecovery     *prometheus.CounterVec
	academicHTTP         *prometheus.CounterVec
	academicReject       *prometheus.CounterVec
	academicBypass       *prometheus.CounterVec
}

// New creates a registry with process and academic-provider metrics.
func New(service, release string) *Registry {
	service = normalized(service)
	release = normalized(release)
	gather := prometheus.NewRegistry()
	registry := &Registry{
		service:              service,
		gather:               gather,
		grpcReq:              prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_grpc_requests_total", Help: "Academic gRPC requests by method and code."}, []string{"service", "method", "code"}),
		grpcDur:              prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "campus", Name: "academic_grpc_request_duration_seconds", Help: "Academic gRPC request duration by method and code."}, []string{"service", "method", "code"}),
		academicCache:        prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_cache_events_total", Help: "Academic cache events."}, []string{"service", "operation", "state", "education_level"}),
		academicQuery:        prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_query_events_total", Help: "Academic query outcomes."}, []string{"service", "operation", "outcome", "education_level"}),
		academicUpstream:     prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_upstream_attempts_total", Help: "Academic upstream attempts."}, []string{"service", "operation", "outcome", "education_level"}),
		academicCircuit:      prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_circuit_events_total", Help: "Academic circuit events."}, []string{"service", "operation", "event", "education_level"}),
		academicSingleflight: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_singleflight_total", Help: "Academic singleflight roles."}, []string{"service", "operation", "role", "education_level"}),
		academicSession:      prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_session_cache_events_total", Help: "Academic session cache events."}, []string{"service", "education_level", "outcome"}),
		academicRecovery:     prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_session_recovery_total", Help: "Academic session recovery events."}, []string{"service", "education_level", "stage", "outcome"}),
		academicHTTP:         prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_ouc_http_requests_total", Help: "OUC HTTP exchanges."}, []string{"service", "host", "operation", "phase", "outcome"}),
		academicReject:       prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_ouc_session_rejections_total", Help: "OUC session rejections."}, []string{"service", "host", "operation"}),
		academicBypass:       prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "campus", Name: "academic_cache_bypasses_total", Help: "Academic cache bypass events."}, []string{"service", "operation", "state", "education_level", "mode"}),
	}
	gather.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "campus", Name: "academic_build_info", Help: "Academic service build information.", ConstLabels: prometheus.Labels{"service": service, "release": release}}, func() float64 { return 1 }),
		registry.grpcReq, registry.grpcDur, registry.academicCache, registry.academicQuery,
		registry.academicUpstream, registry.academicCircuit, registry.academicSingleflight,
		registry.academicSession, registry.academicRecovery, registry.academicHTTP,
		registry.academicReject, registry.academicBypass,
	)
	return registry
}

func normalized(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

// Handler returns the private metrics endpoint handler.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.gather, promhttp.HandlerOpts{})
}

// UnaryServerInterceptor records bounded gRPC method and status labels.
func (r *Registry) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		started := time.Now()
		response, err := handler(ctx, req)
		code := status.Code(err).String()
		method := normalized(info.FullMethod)
		r.grpcReq.WithLabelValues(r.service, method, code).Inc()
		r.grpcDur.WithLabelValues(r.service, method, code).Observe(time.Since(started).Seconds())
		return response, err
	}
}

func (r *Registry) ObserveAcademicCache(operation, state, educationLevel string) {
	r.academicCache.WithLabelValues(r.service, normalized(operation), normalized(state), normalized(educationLevel)).Inc()
}
func (r *Registry) ObserveAcademicCacheBypass(operation, state, educationLevel, mode string) {
	r.academicBypass.WithLabelValues(r.service, normalized(operation), normalized(state), normalized(educationLevel), normalized(mode)).Inc()
}
func (r *Registry) ObserveAcademicQuery(operation, outcome, educationLevel string) {
	r.academicQuery.WithLabelValues(r.service, normalized(operation), normalized(outcome), normalized(educationLevel)).Inc()
}
func (r *Registry) ObserveAcademicUpstreamAttempt(operation, outcome, educationLevel string) {
	r.academicUpstream.WithLabelValues(r.service, normalized(operation), normalized(outcome), normalized(educationLevel)).Inc()
}
func (r *Registry) ObserveAcademicCircuit(operation, event, educationLevel string) {
	r.academicCircuit.WithLabelValues(r.service, normalized(operation), normalized(event), normalized(educationLevel)).Inc()
}
func (r *Registry) ObserveAcademicStaleFallback(operation, reason, educationLevel string) {
	r.academicCache.WithLabelValues(r.service, normalized(operation), "stale_fallback:"+normalized(reason), normalized(educationLevel)).Inc()
}
func (r *Registry) ObserveAcademicSessionCache(educationLevel, outcome string) {
	r.academicSession.WithLabelValues(r.service, normalized(educationLevel), normalized(outcome)).Inc()
}
func (r *Registry) ObserveAcademicSessionRecovery(educationLevel, stage, outcome string) {
	r.academicRecovery.WithLabelValues(r.service, normalized(educationLevel), normalized(stage), normalized(outcome)).Inc()
}
func (r *Registry) ObserveAcademicSingleflight(operation, role, educationLevel string) {
	r.academicSingleflight.WithLabelValues(r.service, normalized(operation), normalized(role), normalized(educationLevel)).Inc()
}
func (r *Registry) ObserveAcademicOUCHTTP(operation, host, phase, outcome string, _ time.Duration) {
	r.academicHTTP.WithLabelValues(r.service, normalized(host), normalized(operation), normalized(phase), normalized(outcome)).Inc()
}
func (r *Registry) ObserveAcademicOUCSessionRejected(operation, host string) {
	r.academicReject.WithLabelValues(r.service, normalized(host), normalized(operation)).Inc()
}
func (r *Registry) SetAcademicQueryCacheMode(string) {}

// Serve exposes the metrics handler until the context is cancelled.
func Serve(ctx context.Context, address string, handler http.Handler) error {
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
