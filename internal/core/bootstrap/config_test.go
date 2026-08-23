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

func setEmptyAcademicEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CAMPUS_ACADEMIC_PROVIDER_KEY",
		"CAMPUS_ACADEMIC_QUERY_KEY",
		"CAMPUS_ACADEMIC_ANALYTICS_DSN",
		"CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN",
		"CAMPUS_ACADEMIC_PROVIDER_LISTEN",
		"CAMPUS_ACADEMIC_ANALYTICS_LISTEN",
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
