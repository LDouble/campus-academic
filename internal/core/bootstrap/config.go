// Package bootstrap loads the configuration owned by campus-academic.
package bootstrap

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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

// ProviderConfig configures the gRPC listener and upstream query protection.
type ProviderConfig struct {
	ListenAddress  string        `yaml:"listen_address"`
	Target         string        `yaml:"target"`
	HTTPProxyURL   string        `yaml:"http_proxy_url"`
	MaxConcurrent  int           `yaml:"max_concurrent"`
	QueueWait      time.Duration `yaml:"queue_wait"`
	RetryAfter     time.Duration `yaml:"retry_after"`
	Insecure       bool          `yaml:"insecure"`
	TLSFilesRoot   string        `yaml:"tls_files_root"`
	CAFile         string        `yaml:"ca_file"`
	ClientCertFile string        `yaml:"client_cert_file"`
	ClientKeyFile  string        `yaml:"client_key_file"`
	ServerCertFile string        `yaml:"server_cert_file"`
	ServerKeyFile  string        `yaml:"server_key_file"`
	ServerName     string        `yaml:"server_name"`
}

// AnalyticsConfig owns the Analytics write database, source read database,
// queue Redis and gRPC server. SourceDSN is environment-only by design.
type AnalyticsConfig struct {
	ListenAddress     string        `yaml:"listen_address"`
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
	Timezone          string        `yaml:"timezone"`
	ScheduleHour      int           `yaml:"schedule_hour"`
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
	Address  string `yaml:"address"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
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
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read campus-academic bootstrap: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode campus-academic bootstrap: %w", err)
	}
	applyDefaults(&cfg)
	applyEnvironment(&cfg)
	if err := loadSecrets(&cfg); err != nil {
		return Config{}, err
	}
	if err := validate(cfg); err != nil {
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
		cfg.Provider.ListenAddress = ":9100"
	}
	if cfg.Analytics.ListenAddress == "" {
		cfg.Analytics.ListenAddress = ":9200"
	}
	if cfg.Redis.Address == "" {
		cfg.Redis.Address = "127.0.0.1:6379"
	}
	if cfg.Analytics.Redis.Address == "" {
		cfg.Analytics.Redis = cfg.Redis
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

func applyEnvironment(cfg *Config) {
	setString(&cfg.Environment, "CAMPUS_ACADEMIC_ENV")
	setString(&cfg.Release, "CAMPUS_RELEASE")
	setString(&cfg.Provider.ListenAddress, "CAMPUS_ACADEMIC_PROVIDER_LISTEN")
	setString(&cfg.ProviderConfigFile, "CAMPUS_ACADEMIC_PROVIDER_CONFIG_FILE")
	setString(&cfg.Redis.Address, "CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS")
	setString(&cfg.Redis.Password, "CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD")
	setString(&cfg.Analytics.ListenAddress, "CAMPUS_ACADEMIC_ANALYTICS_LISTEN")
	setString(&cfg.Analytics.MySQL.DSN, "CAMPUS_ACADEMIC_ANALYTICS_DSN")
	setString(&cfg.Analytics.SourceDSN, "CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN")
	setString(&cfg.Analytics.Redis.Address, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_ADDRESS")
	setString(&cfg.Analytics.Redis.Password, "CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD")
	setString(&cfg.Observability.MetricsAddress, "CAMPUS_ACADEMIC_METRICS_ADDRESS")
}

func setString(target *string, name string) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		*target = value
	}
}

func loadSecrets(cfg *Config) error {
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

func validate(cfg Config) error {
	if cfg.Environment != EnvironmentDevelopment && cfg.Environment != EnvironmentReview && cfg.Environment != EnvironmentProduction {
		return fmt.Errorf("environment must be development, review or production")
	}
	if strings.TrimSpace(cfg.Provider.ListenAddress) == "" || strings.TrimSpace(cfg.Analytics.ListenAddress) == "" {
		return errors.New("provider and analytics listen addresses are required")
	}
	if cfg.Provider.MaxConcurrent < 1 || cfg.Provider.QueueWait <= 0 || cfg.Provider.RetryAfter <= 0 {
		return errors.New("provider concurrency and queue settings must be positive")
	}
	if cfg.UsesProductionSafeguards() && (cfg.Provider.Insecure || cfg.Analytics.Insecure) {
		return errors.New("review/production deployments must use mutual TLS")
	}
	if cfg.Environment == EnvironmentProduction && (strings.TrimSpace(cfg.Analytics.MySQL.DSN) == "" || strings.TrimSpace(cfg.Analytics.SourceDSN) == "") {
		return errors.New("production analytics database and source DSN are required")
	}
	if cfg.Analytics.MinimumSampleSize < 1 {
		return errors.New("analytics minimum sample size must be positive")
	}
	return nil
}
