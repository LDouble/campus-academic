package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadProviderDoesNotRequireAnalyticsCredentials(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("CAMPUS_ACADEMIC_QUERY_KEY", "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")

	path := writeBootstrapForTest(t, `
environment: production
provider:
  insecure: false
redis:
  tls: true
  tls_files_root: /etc/campus-academic/provider-redis-tls
  ca_file: ca.pem
  server_name: provider-redis.internal
analytics:
  insecure: true
`)

	cfg, err := LoadProvider(path)
	if err != nil {
		t.Fatalf("LoadProvider() error = %v", err)
	}
	if cfg.Provider.ListenAddress != ":9090" {
		t.Fatalf("provider listen address = %q, want :9090", cfg.Provider.ListenAddress)
	}
	if cfg.Provider.Target != "127.0.0.1:9090" {
		t.Fatalf("provider target = %q, want healthcheck loopback target", cfg.Provider.Target)
	}
	if cfg.Provider.DiagnosticHTMLDir != "/var/lib/campus-academic/diagnostics" {
		t.Fatalf("provider diagnostic directory = %q", cfg.Provider.DiagnosticHTMLDir)
	}
	if cfg.Analytics.MySQL.DSN != "" || cfg.Analytics.SourceDSN != "" {
		t.Fatalf("provider loader unexpectedly populated analytics credentials: %+v", cfg.Analytics)
	}
}

func TestLoadAnalyticsDoesNotRequireProviderSecrets(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_WRITABLE", "true")

	path := writeBootstrapForTest(t, `
environment: production
provider:
  insecure: true
analytics:
  insecure: false
  redis:
    tls: true
    tls_files_root: /etc/campus-academic/analytics-redis-tls
    ca_file: ca.pem
    server_name: analytics-redis.internal
`)

	cfg, err := LoadAnalytics(path)
	if err != nil {
		t.Fatalf("LoadAnalytics() error = %v", err)
	}
	if !cfg.Analytics.SourceWritable {
		t.Fatal("analytics managed source writable policy was not loaded")
	}
	if cfg.Analytics.ListenAddress != ":9091" {
		t.Fatalf("analytics listen address = %q, want :9091", cfg.Analytics.ListenAddress)
	}
	if cfg.Analytics.Target != "127.0.0.1:9091" {
		t.Fatalf("analytics target = %q, want healthcheck loopback target", cfg.Analytics.Target)
	}
	if len(cfg.Secret.AcademicProviderKey) != 0 || len(cfg.Secret.AcademicQueryKey) != 0 {
		t.Fatalf("analytics loader unexpectedly populated provider secrets")
	}
	if cfg.Analytics.RetryDelay != 15*time.Minute {
		t.Fatalf("analytics retry delay = %s, want 15m", cfg.Analytics.RetryDelay)
	}
	if cfg.Analytics.QueryTimeout != 20*time.Minute {
		t.Fatalf("analytics query timeout = %s, want 20m", cfg.Analytics.QueryTimeout)
	}
	if cfg.Analytics.MySQL.MaxOpenConns != 24 {
		t.Fatalf("analytics MySQL max open conns = %d, want 24", cfg.Analytics.MySQL.MaxOpenConns)
	}
	if cfg.Analytics.MySQL.MaxIdleConns != 8 {
		t.Fatalf("analytics MySQL max idle conns = %d, want 8", cfg.Analytics.MySQL.MaxIdleConns)
	}
	if cfg.Analytics.MySQL.ConnMaxLifetime != 30*time.Minute {
		t.Fatalf("analytics MySQL connection max lifetime = %s, want 30m", cfg.Analytics.MySQL.ConnMaxLifetime)
	}
	if cfg.Analytics.MySQL.ConnMaxIdleTime != 5*time.Minute {
		t.Fatalf("analytics MySQL connection max idle time = %s, want 5m", cfg.Analytics.MySQL.ConnMaxIdleTime)
	}
}

func TestAnalyticsMySQLPoolEnvironmentOverrides(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_MAX_OPEN_CONNS", "12")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_MAX_IDLE_CONNS", "4")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_CONN_MAX_LIFETIME", "12m")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_CONN_MAX_IDLE_TIME", "3m")

	cfg, err := LoadAnalytics(writeBootstrapForTest(t, "environment: development\n"))
	if err != nil {
		t.Fatalf("LoadAnalytics() error = %v", err)
	}
	if cfg.Analytics.MySQL.MaxOpenConns != 12 || cfg.Analytics.MySQL.MaxIdleConns != 4 {
		t.Fatalf("analytics MySQL connection pool = %+v", cfg.Analytics.MySQL)
	}
	if cfg.Analytics.MySQL.ConnMaxLifetime != 12*time.Minute || cfg.Analytics.MySQL.ConnMaxIdleTime != 3*time.Minute {
		t.Fatalf("analytics MySQL connection lifetime settings = %+v", cfg.Analytics.MySQL)
	}
}

func TestAnalyticsMySQLPoolRejectsNonPositiveEnvironmentValue(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_MAX_OPEN_CONNS", "0")

	if _, err := LoadAnalytics(writeBootstrapForTest(t, "environment: development\n")); err == nil {
		t.Fatal("LoadAnalytics() accepted non-positive Analytics MySQL max open connections")
	}
}

func TestAnalyticsMySQLPoolRejectsIdleConnectionsAboveOpenLimit(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_MAX_OPEN_CONNS", "4")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_MYSQL_MAX_IDLE_CONNS", "5")

	if _, err := LoadAnalytics(writeBootstrapForTest(t, "environment: development\n")); err == nil {
		t.Fatal("LoadAnalytics() accepted Analytics MySQL idle connections above open limit")
	}
}

func TestAnalyticsRetryDelayEnvironmentOverride(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_RETRY_DELAY", "1h30m")
	path := writeBootstrapForTest(t, "environment: development\n")

	cfg, err := LoadAnalytics(path)
	if err != nil {
		t.Fatalf("LoadAnalytics() error = %v", err)
	}
	if cfg.Analytics.RetryDelay != 90*time.Minute {
		t.Fatalf("analytics retry delay = %s, want 1h30m", cfg.Analytics.RetryDelay)
	}
}

func TestAnalyticsRetryDelayRejectsInvalidEnvironment(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_RETRY_DELAY", "never")
	path := writeBootstrapForTest(t, "environment: development\n")

	if _, err := LoadAnalytics(path); err == nil {
		t.Fatal("LoadAnalytics() accepted invalid analytics retry delay")
	}
}

func TestAnalyticsQueryTimeoutEnvironmentOverride(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_QUERY_TIMEOUT", "12m")
	path := writeBootstrapForTest(t, "environment: development\n")

	cfg, err := LoadAnalytics(path)
	if err != nil {
		t.Fatalf("LoadAnalytics() error = %v", err)
	}
	if cfg.Analytics.QueryTimeout != 12*time.Minute {
		t.Fatalf("analytics query timeout = %s, want 12m", cfg.Analytics.QueryTimeout)
	}
}

func TestAnalyticsQueryTimeoutRejectsInvalidEnvironment(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_QUERY_TIMEOUT", "unbounded")
	path := writeBootstrapForTest(t, "environment: development\n")

	if _, err := LoadAnalytics(path); err == nil {
		t.Fatal("LoadAnalytics() accepted invalid analytics query timeout")
	}
}

func TestAnalyticsQueryTimeoutMustBeShorterThanTaskTimeout(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")
	path := writeBootstrapForTest(t, `
environment: development
analytics:
  query_timeout: 30m
  task_timeout: 30m
`)

	if _, err := LoadAnalytics(path); err == nil {
		t.Fatal("LoadAnalytics() accepted query timeout equal to task timeout")
	}
}

func TestDevelopmentRedisDefaultsStayIsolated(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	path := writeBootstrapForTest(t, "environment: development\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Redis.Address != "127.0.0.1:6379" {
		t.Fatalf("provider Redis address = %q", cfg.Redis.Address)
	}
	if cfg.Analytics.Redis.Address != "127.0.0.1:6380" {
		t.Fatalf("analytics Redis address = %q", cfg.Analytics.Redis.Address)
	}
}

func TestRedisEnvironmentOverrides(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS", "provider-redis.internal:6380")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_USERNAME", "provider")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD", "provider-secret")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_DB", "2")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS", "true")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_FILES_ROOT", "/run/secrets/provider-redis")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_CA_FILE", "ca.pem")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_SERVER_NAME", "provider-redis.internal")
	path := writeBootstrapForTest(t, "environment: development\n")

	cfg, err := LoadProvider(path)
	if err != nil {
		t.Fatalf("LoadProvider() error = %v", err)
	}
	if cfg.Redis.Address != "provider-redis.internal:6380" || cfg.Redis.Username != "provider" ||
		cfg.Redis.Password != "provider-secret" || cfg.Redis.DB != 2 || !cfg.Redis.TLS ||
		cfg.Redis.ServerName != "provider-redis.internal" {
		t.Fatalf("provider Redis overrides = %+v", cfg.Redis)
	}
}

func TestProviderTargetEnvironmentOverride(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_TARGET", "academic-provider.internal:9090")
	path := writeBootstrapForTest(t, "environment: development\n")

	cfg, err := LoadProvider(path)
	if err != nil {
		t.Fatalf("LoadProvider() error = %v", err)
	}
	if cfg.Provider.Target != "academic-provider.internal:9090" {
		t.Fatalf("provider target = %q", cfg.Provider.Target)
	}
}

func TestProviderDiagnosticDirectoryEnvironmentOverride(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_DIAGNOSTIC_HTML_DIR", "/srv/provider-diagnostics")
	path := writeBootstrapForTest(t, "environment: development\n")

	cfg, err := LoadProvider(path)
	if err != nil {
		t.Fatalf("LoadProvider() error = %v", err)
	}
	if cfg.Provider.DiagnosticHTMLDir != "/srv/provider-diagnostics" {
		t.Fatalf("provider diagnostic directory = %q", cfg.Provider.DiagnosticHTMLDir)
	}
}

func TestProductionRedisRequiresTLS(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("CAMPUS_ACADEMIC_QUERY_KEY", "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
	path := writeBootstrapForTest(t, `
environment: production
provider:
  insecure: false
`)

	if _, err := LoadProvider(path); err == nil {
		t.Fatal("LoadProvider() accepted production Redis without TLS")
	}
}

func TestRedisEnvironmentRejectsInvalidScalar(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	path := writeBootstrapForTest(t, "environment: development\n")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_DB", "invalid")
	if _, err := LoadProvider(path); err == nil {
		t.Fatal("LoadProvider() accepted invalid Redis DB")
	}
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_DB", "0")
	t.Setenv("CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS", "sometimes")
	if _, err := LoadProvider(path); err == nil {
		t.Fatal("LoadProvider() accepted invalid Redis TLS boolean")
	}
}

func setEmptyAcademicEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CAMPUS_ACADEMIC_PROVIDER_KEY",
		"CAMPUS_ACADEMIC_QUERY_KEY",
		"CAMPUS_ACADEMIC_ANALYTICS_DSN",
		"CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN",
		"CAMPUS_ACADEMIC_PROVIDER_LISTEN",
		"CAMPUS_ACADEMIC_PROVIDER_TARGET",
		"CAMPUS_ACADEMIC_PROVIDER_DIAGNOSTIC_HTML_DIR",
		"CAMPUS_ACADEMIC_ANALYTICS_LISTEN",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_USERNAME",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_DB",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_FILES_ROOT",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_CA_FILE",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_CERT_FILE",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_KEY_FILE",
		"CAMPUS_ACADEMIC_PROVIDER_REDIS_SERVER_NAME",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_ADDRESS",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_USERNAME",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_DB",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_FILES_ROOT",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_CERT_FILE",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_KEY_FILE",
		"CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME",
		"CAMPUS_ACADEMIC_ANALYTICS_RETRY_DELAY",
		"CAMPUS_ACADEMIC_ANALYTICS_QUERY_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}

func writeBootstrapForTest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}
