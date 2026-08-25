// Package bootstrap loads the configuration owned by campus-academic.
package bootstrap

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	EnvironmentDevelopment = "development"
	EnvironmentReview      = "review"
	EnvironmentProduction  = "production"
)

// Config contains only the independently operated Provider and Analytics
// settings. Platform bootstrap configuration is deliberately not reused.
type Config struct {
	Environment        string              `yaml:"environment"`
	Release            string              `yaml:"release"`
	Provider           ProviderConfig      `yaml:"provider"`
	Analytics          AnalyticsConfig     `yaml:"analytics"`
	Redis              RedisConfig         `yaml:"redis"`
	AcademicQuery      AcademicQueryConfig `yaml:"academic_query"`
	ProviderConfigFile string              `yaml:"provider_config_file"`
	Observability      ObservabilityConfig `yaml:"observability"`
	Secret             SecretConfig        `yaml:"-"`
}

type component string

const (
	componentAll       component = "all"
	componentProvider  component = "provider"
	componentAnalytics component = "analytics"
)

// ProviderConfig configures the gRPC listener and upstream query protection.
type ProviderConfig struct {
	ListenAddress     string        `yaml:"listen_address"`
	Target            string        `yaml:"target"`
	HTTPProxyURL      string        `yaml:"http_proxy_url"`
	DiagnosticHTMLDir string        `yaml:"diagnostic_html_dir"`
	MaxConcurrent     int           `yaml:"max_concurrent"`
	QueueWait         time.Duration `yaml:"queue_wait"`
	RetryAfter        time.Duration `yaml:"retry_after"`
	Insecure          bool          `yaml:"insecure"`
	TLSFilesRoot      string        `yaml:"tls_files_root"`
	CAFile            string        `yaml:"ca_file"`
	ClientCertFile    string        `yaml:"client_cert_file"`
	ClientKeyFile     string        `yaml:"client_key_file"`
	ServerCertFile    string        `yaml:"server_cert_file"`
	ServerKeyFile     string        `yaml:"server_key_file"`
	ServerName        string        `yaml:"server_name"`
}

// AnalyticsConfig owns the Analytics write database, grade source database,
// queue Redis and gRPC server. Source credentials are environment-only by design.
type AnalyticsConfig struct {
	ListenAddress     string        `yaml:"listen_address"`
	Target            string        `yaml:"target"`
	Insecure          bool          `yaml:"insecure"`
	TLSFilesRoot      string        `yaml:"tls_files_root"`
	CAFile            string        `yaml:"ca_file"`
	ClientCertFile    string        `yaml:"client_cert_file"`
	ClientKeyFile     string        `yaml:"client_key_file"`
	ServerCertFile    string        `yaml:"server_cert_file"`
	ServerKeyFile     string        `yaml:"server_key_file"`
	ServerName        string        `yaml:"server_name"`
	MySQL             MySQLConfig   `yaml:"mysql"`
	Redis             RedisConfig   `yaml:"redis"`
	SourceDSN         string        `yaml:"-"`
	SourceWritable    bool          `yaml:"-"`
	Timezone          string        `yaml:"timezone"`
	ScheduleHour      int           `yaml:"schedule_hour"`
	RetryDelay        time.Duration `yaml:"retry_delay"`
	MinimumSampleSize int64         `yaml:"minimum_sample_size"`
	QueryTimeout      time.Duration `yaml:"query_timeout"`
	WorkerConcurrency int           `yaml:"worker_concurrency"`
	TaskQueue         string        `yaml:"task_queue"`
	TaskTimeout       time.Duration `yaml:"task_timeout"`
}

// MySQLConfig contains one service-owned database connection.
type MySQLConfig struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
}

// RedisConfig contains one service-owned Redis connection.
type RedisConfig struct {
	Address        string `yaml:"address"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	DB             int    `yaml:"db"`
	TLS            bool   `yaml:"tls"`
	TLSFilesRoot   string `yaml:"tls_files_root"`
	CAFile         string `yaml:"ca_file"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
	ServerName     string `yaml:"server_name"`
}

// AcademicQueryConfig controls encrypted provider query caching and limits.
type AcademicQueryConfig struct {
	CacheMode                   string        `yaml:"cache_mode"`
	CoursesTimeout              time.Duration `yaml:"courses_timeout"`
	GradesTimeout               time.Duration `yaml:"grades_timeout"`
	ExamsTimeout                time.Duration `yaml:"exams_timeout"`
	SelectionsTimeout           time.Duration `yaml:"selections_timeout"`
	StaleRefreshTimeout         time.Duration `yaml:"stale_refresh_timeout"`
	CircuitFailureThreshold     int           `yaml:"circuit_failure_threshold"`
	CircuitWindow               time.Duration `yaml:"circuit_window"`
	CircuitOpenDuration         time.Duration `yaml:"circuit_open_duration"`
	CircuitMinimumSamples       int           `yaml:"circuit_minimum_samples"`
	CircuitDeadlineThreshold    int           `yaml:"circuit_deadline_threshold"`
	CircuitDeadlineRatio        float64       `yaml:"circuit_deadline_ratio"`
	CircuitHardProtectionCount  int           `yaml:"circuit_hard_protection_count"`
	CircuitHardProtectionWindow time.Duration `yaml:"circuit_hard_protection_window"`
	LeaseTTL                    time.Duration `yaml:"lease_ttl"`
	PollInterval                time.Duration `yaml:"poll_interval"`
	GlobalRate                  int           `yaml:"global_rate"`
	GlobalBurst                 int           `yaml:"global_burst"`
	CoursesFreshTTL             time.Duration `yaml:"courses_fresh_ttl"`
	CoursesStaleTTL             time.Duration `yaml:"courses_stale_ttl"`
	GradesFreshTTL              time.Duration `yaml:"grades_fresh_ttl"`
	GradesStaleTTL              time.Duration `yaml:"grades_stale_ttl"`
	ExamsFreshTTL               time.Duration `yaml:"exams_fresh_ttl"`
	ExamsStaleTTL               time.Duration `yaml:"exams_stale_ttl"`
	SelectionsFreshTTL          time.Duration `yaml:"selections_fresh_ttl"`
	SelectionsStaleTTL          time.Duration `yaml:"selections_stale_ttl"`
}

type ObservabilityConfig struct {
	MetricsAddress string `yaml:"metrics_address"`
}

type SecretConfig struct {
	AcademicProviderKey []byte
	AcademicQueryKey    []byte
}

// IsProduction reports whether external production safeguards apply.
func (c Config) IsProduction() bool { return c.Environment == EnvironmentProduction }

// IsReview reports whether synthetic review safeguards apply.
func (c Config) IsReview() bool { return c.Environment == EnvironmentReview }

// UsesProductionSafeguards applies production transport requirements to review.
func (c Config) UsesProductionSafeguards() bool { return c.IsProduction() || c.IsReview() }

// AllowsAcademicMock is intentionally limited to development and review.
func (c Config) AllowsAcademicMock() bool { return !c.IsProduction() }

// AllowsAcademicOUC allows real upstream access outside review by default.
func (c Config) AllowsAcademicOUC() bool { return true }

// Load reads YAML and environment-only secrets for both services.
func Load(path string) (Config, error) {
	return load(path, componentAll)
}

// LoadProvider loads only the configuration required by academic-provider.
// Provider startup must not require Analytics database credentials.
func LoadProvider(path string) (Config, error) {
	return load(path, componentProvider)
}

// LoadAnalytics loads only the configuration required by academic-analytics.
// Analytics startup must not require Provider session keys.
func LoadAnalytics(path string) (Config, error) {
	return load(path, componentAnalytics)
}

func load(path string, service component) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read campus-academic bootstrap: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode campus-academic bootstrap: %w", err)
	}
	applyDefaults(&cfg)
	if err := applyEnvironment(&cfg); err != nil {
		return Config{}, err
	}
	if err := loadSecrets(&cfg, service); err != nil {
		return Config{}, err
	}
	if err := validate(cfg, service); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Environment == "" {
		cfg.Environment = EnvironmentDevelopment
	}
	if cfg.Release == "" {
		cfg.Release = "dev"
	}
	if cfg.Provider.ListenAddress == "" {
		cfg.Provider.ListenAddress = ":9090"
	}
	if cfg.Provider.Target == "" {
		cfg.Provider.Target = "127.0.0.1:9090"
	}
	if cfg.Analytics.ListenAddress == "" {
		cfg.Analytics.ListenAddress = ":9091"
	}
	if cfg.Analytics.Target == "" {
		cfg.Analytics.Target = "127.0.0.1:9091"
	}
	if cfg.Redis.Address == "" {
		cfg.Redis.Address = "127.0.0.1:6379"
	}
	if cfg.Analytics.Redis.Address == "" {
		cfg.Analytics.Redis.Address = "127.0.0.1:6380"
	}
	if cfg.Observability.MetricsAddress == "" {
		cfg.Observability.MetricsAddress = ":9300"
	}
	if cfg.ProviderConfigFile == "" {
		cfg.ProviderConfigFile = "provider-config.yaml"
	}
	if cfg.Provider.MaxConcurrent < 1 {
		cfg.Provider.MaxConcurrent = 32
	}
	if cfg.Provider.QueueWait <= 0 {
		cfg.Provider.QueueWait = 250 * time.Millisecond
	}
	if cfg.Provider.RetryAfter <= 0 {
		cfg.Provider.RetryAfter = 2 * time.Second
	}
	if cfg.Provider.DiagnosticHTMLDir == "" {
		cfg.Provider.DiagnosticHTMLDir = "/var/lib/campus-academic/diagnostics"
	}
	if cfg.AcademicQuery.CacheMode == "" {
		cfg.AcademicQuery.CacheMode = "normal"
	}
	if cfg.AcademicQuery.CoursesTimeout <= 0 {
		cfg.AcademicQuery.CoursesTimeout = 10 * time.Second
	}
	if cfg.AcademicQuery.GradesTimeout <= 0 {
		cfg.AcademicQuery.GradesTimeout = 10 * time.Second
	}
	if cfg.AcademicQuery.ExamsTimeout <= 0 {
		cfg.AcademicQuery.ExamsTimeout = 12 * time.Second
	}
	if cfg.AcademicQuery.SelectionsTimeout <= 0 {
		cfg.AcademicQuery.SelectionsTimeout = 12 * time.Second
	}
	if cfg.AcademicQuery.StaleRefreshTimeout <= 0 {
		cfg.AcademicQuery.StaleRefreshTimeout = 2 * time.Second
	}
	if cfg.AcademicQuery.LeaseTTL <= 0 {
		cfg.AcademicQuery.LeaseTTL = 20 * time.Second
	}
	if cfg.AcademicQuery.PollInterval <= 0 {
		cfg.AcademicQuery.PollInterval = 100 * time.Millisecond
	}
	if cfg.AcademicQuery.GlobalRate <= 0 {
		cfg.AcademicQuery.GlobalRate = 10
	}
	if cfg.AcademicQuery.GlobalBurst <= 0 {
		cfg.AcademicQuery.GlobalBurst = 20
	}
	if cfg.Analytics.Timezone == "" {
		cfg.Analytics.Timezone = "Asia/Shanghai"
	}
	if cfg.Analytics.ScheduleHour == 0 {
		cfg.Analytics.ScheduleHour = 4
	}
	if cfg.Analytics.RetryDelay <= 0 {
		cfg.Analytics.RetryDelay = 15 * time.Minute
	}
	if cfg.Analytics.MinimumSampleSize <= 0 {
		cfg.Analytics.MinimumSampleSize = 5
	}
	if cfg.Analytics.QueryTimeout <= 0 {
		cfg.Analytics.QueryTimeout = 2 * time.Minute
	}
	if cfg.Analytics.WorkerConcurrency <= 0 {
		cfg.Analytics.WorkerConcurrency = 2
	}
	if cfg.Analytics.TaskQueue == "" {
		cfg.Analytics.TaskQueue = "academic_analytics"
	}
	if cfg.Analytics.TaskTimeout <= 0 {
		cfg.Analytics.TaskTimeout = 30 * time.Minute
	}
}

func applyEnvironment(cfg *Config) error {
	setString(&cfg.Environment, "CAMPUS_ACADEMIC_ENV")
	setString(&cfg.Release, "CAMPUS_RELEASE")
	setString(&cfg.Provider.ListenAddress, "CAMPUS_ACADEMIC_PROVIDER_LISTEN")
	setString(&cfg.Provider.Target, "CAMPUS_ACADEMIC_PROVIDER_TARGET")
	setString(&cfg.Provider.DiagnosticHTMLDir, "CAMPUS_ACADEMIC_PROVIDER_DIAGNOSTIC_HTML_DIR")
	setString(&cfg.ProviderConfigFile, "CAMPUS_ACADEMIC_PROVIDER_CONFIG_FILE")
	setString(&cfg.Redis.Address, "CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS")
	setString(&cfg.Redis.Username, "CAMPUS_ACADEMIC_PROVIDER_REDIS_USERNAME")
	setString(&cfg.Redis.Password, "CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD")
	if err := setInt(&cfg.Redis.DB, "CAMPUS_ACADEMIC_PROVIDER_REDIS_DB"); err != nil {
		return err
	}
	if err := setBool(&cfg.Redis.TLS, "CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS"); err != nil {
		return err
	}
	setString(&cfg.Redis.TLSFilesRoot, "CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_FILES_ROOT")
	setString(&cfg.Redis.CAFile, "CAMPUS_ACADEMIC_PROVIDER_REDIS_CA_FILE")
	setString(&cfg.Redis.ClientCertFile, "CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_CERT_FILE")
	setString(&cfg.Redis.ClientKeyFile, "CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_KEY_FILE")
	setString(&cfg.Redis.ServerName, "CAMPUS_ACADEMIC_PROVIDER_REDIS_SERVER_NAME")
	setString(&cfg.Analytics.ListenAddress, "CAMPUS_ACADEMIC_ANALYTICS_LISTEN")
	setString(&cfg.Analytics.Target, "CAMPUS_ACADEMIC_ANALYTICS_TARGET")
	setString(&cfg.Analytics.MySQL.DSN, "CAMPUS_ACADEMIC_ANALYTICS_DSN")
	setString(&cfg.Analytics.SourceDSN, "CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN")
	if err := setBool(&cfg.Analytics.SourceWritable, "CAMPUS_ACADEMIC_ANALYTICS_SOURCE_WRITABLE"); err != nil {
		return err
	}
	setString(&cfg.Analytics.Redis.Address, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_ADDRESS")
	setString(&cfg.Analytics.Redis.Username, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_USERNAME")
	setString(&cfg.Analytics.Redis.Password, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD")
	if err := setInt(&cfg.Analytics.Redis.DB, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_DB"); err != nil {
		return err
	}
	if err := setBool(&cfg.Analytics.Redis.TLS, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS"); err != nil {
		return err
	}
	setString(&cfg.Analytics.Redis.TLSFilesRoot, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_FILES_ROOT")
	setString(&cfg.Analytics.Redis.CAFile, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE")
	setString(&cfg.Analytics.Redis.ClientCertFile, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_CERT_FILE")
	setString(&cfg.Analytics.Redis.ClientKeyFile, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_KEY_FILE")
	setString(&cfg.Analytics.Redis.ServerName, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME")
	if err := setDuration(&cfg.Analytics.RetryDelay, "CAMPUS_ACADEMIC_ANALYTICS_RETRY_DELAY"); err != nil {
		return err
	}
	setString(&cfg.Observability.MetricsAddress, "CAMPUS_ACADEMIC_METRICS_ADDRESS")
	return nil
}

func setString(target *string, name string) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		*target = value
	}
}

func setInt(target *int, name string) error {
	value, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	*target = parsed
	return nil
}

func setBool(target *bool, name string) error {
	value, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s must be a boolean", name)
	}
	*target = parsed
	return nil
}

func setDuration(target *time.Duration, name string) error {
	value, configured := os.LookupEnv(name)
	if !configured || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fmt.Errorf("%s must be a positive duration", name)
	}
	*target = parsed
	return nil
}

func loadSecrets(cfg *Config, service component) error {
	if service == componentAnalytics {
		return nil
	}
	provider, err := secretFromEnv("CAMPUS_ACADEMIC_PROVIDER_KEY", "provider key")
	if err != nil {
		return err
	}
	query, err := secretFromEnv("CAMPUS_ACADEMIC_QUERY_KEY", "query key")
	if err != nil {
		return err
	}
	if provider == nil && cfg.Environment == EnvironmentDevelopment {
		provider = developmentKey("provider")
	}
	if query == nil && cfg.Environment == EnvironmentDevelopment {
		query = developmentKey("query")
	}
	if len(provider) != 32 || len(query) != 32 {
		return errors.New("CAMPUS_ACADEMIC_PROVIDER_KEY and CAMPUS_ACADEMIC_QUERY_KEY must be 32-byte base64 or hex secrets")
	}
	cfg.Secret.AcademicProviderKey = provider
	cfg.Secret.AcademicQueryKey = query
	return nil
}

func secretFromEnv(name, label string) ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	if len(raw) == 64 {
		if decoded, err := hex.DecodeString(raw); err == nil {
			return decoded, nil
		}
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: expected base64 or hex: %w", label, err)
	}
	return decoded, nil
}

func developmentKey(label string) []byte {
	sum := sha256.Sum256([]byte("campus-academic-development:" + label))
	return sum[:]
}

func validate(cfg Config, service component) error {
	if cfg.Environment != EnvironmentDevelopment && cfg.Environment != EnvironmentReview && cfg.Environment != EnvironmentProduction {
		return fmt.Errorf("environment must be development, review or production")
	}
	if service != componentAnalytics && strings.TrimSpace(cfg.Provider.ListenAddress) == "" {
		return errors.New("provider listen address is required")
	}
	if service != componentProvider && strings.TrimSpace(cfg.Analytics.ListenAddress) == "" {
		return errors.New("analytics listen address is required")
	}
	if service != componentAnalytics && (cfg.Provider.MaxConcurrent < 1 || cfg.Provider.QueueWait <= 0 || cfg.Provider.RetryAfter <= 0) {
		return errors.New("provider concurrency and queue settings must be positive")
	}
	if service != componentAnalytics && !filepath.IsAbs(cfg.Provider.DiagnosticHTMLDir) {
		return errors.New("provider diagnostic HTML directory must be absolute")
	}
	if cfg.UsesProductionSafeguards() && ((service != componentAnalytics && cfg.Provider.Insecure) ||
		(service != componentProvider && cfg.Analytics.Insecure)) {
		return errors.New("review/production deployments must use mutual TLS")
	}
	if service != componentProvider && cfg.Environment == EnvironmentProduction && (strings.TrimSpace(cfg.Analytics.MySQL.DSN) == "" || strings.TrimSpace(cfg.Analytics.SourceDSN) == "") {
		return errors.New("production analytics database and source DSN are required")
	}
	if service != componentAnalytics {
		if err := validateRedisConfig("provider", cfg.Redis, cfg.UsesProductionSafeguards()); err != nil {
			return err
		}
	}
	if service != componentProvider {
		if err := validateRedisConfig("analytics", cfg.Analytics.Redis, cfg.UsesProductionSafeguards()); err != nil {
			return err
		}
	}
	if cfg.Analytics.MinimumSampleSize < 1 {
		return errors.New("analytics minimum sample size must be positive")
	}
	if cfg.Analytics.RetryDelay <= 0 {
		return errors.New("analytics retry delay must be positive")
	}
	return nil
}

func validateRedisConfig(name string, config RedisConfig, requireTLS bool) error {
	if strings.TrimSpace(config.Address) == "" {
		return fmt.Errorf("%s Redis address is required", name)
	}
	if config.DB < 0 {
		return fmt.Errorf("%s Redis DB must not be negative", name)
	}
	if requireTLS && !config.TLS {
		return fmt.Errorf("review/production %s Redis must use TLS", name)
	}
	if !config.TLS {
		return nil
	}
	if strings.TrimSpace(config.TLSFilesRoot) == "" || strings.TrimSpace(config.CAFile) == "" || strings.TrimSpace(config.ServerName) == "" {
		return fmt.Errorf("%s Redis TLS files root, CA file and server name are required", name)
	}
	if !filepath.IsAbs(config.TLSFilesRoot) {
		return fmt.Errorf("%s Redis TLS files root must be absolute", name)
	}
	if (strings.TrimSpace(config.ClientCertFile) == "") != (strings.TrimSpace(config.ClientKeyFile) == "") {
		return fmt.Errorf("%s Redis client certificate and key must be configured together", name)
	}
	return nil
}
