package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
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
	if cfg.Analytics.MySQL.DSN != "" || cfg.Analytics.SourceDSN != "" {
		t.Fatalf("provider loader unexpectedly populated analytics credentials: %+v", cfg.Analytics)
	}
}

func TestLoadAnalyticsDoesNotRequireProviderSecrets(t *testing.T) {
	setEmptyAcademicEnvironment(t)
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_DSN", "write@tcp(127.0.0.1:3306)/campus_academic")
	t.Setenv("CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN", "readonly@tcp(127.0.0.1:3306)/campus")

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
	if cfg.Analytics.ListenAddress != ":9091" {
		t.Fatalf("analytics listen address = %q, want :9091", cfg.Analytics.ListenAddress)
	}
	if len(cfg.Secret.AcademicProviderKey) != 0 || len(cfg.Secret.AcademicQueryKey) != 0 {
		t.Fatalf("analytics loader unexpectedly populated provider secrets")
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
