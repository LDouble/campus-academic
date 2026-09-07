// Package academicconfig loads validated academic-provider settings from the
// platform configuration center.
package academicconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// MaxCourseCatalogPageSize bounds a single catalog response while allowing
	// the undergraduate OUC endpoint to return its supported 500-row pages.
	MaxCourseCatalogPageSize = 500

	// Group is the configuration-center group used by the academic provider.
	Group = "academic_provider"

	// ProviderMock selects the local deterministic academic provider.
	ProviderMock = "mock"
	// ProviderOUC selects the China University of Ocean academic provider.
	ProviderOUC = "ouc"
)

var allowedHosts = map[string]struct{}{
	"id.ouc.edu.cn":       {},
	"my.ouc.edu.cn":       {},
	"jwgl2024.ouc.edu.cn": {},
	"pgs.ouc.edu.cn":      {},
}

// Source is implemented by configcenter.Service through ListDecrypted.
type Source interface {
	ListDecrypted(context.Context, string) (map[string]string, error)
}

// OperationEndpoint describes one read-only school-system operation.
type OperationEndpoint struct {
	Path             string   `json:"path"`
	RequestMethod    string   `json:"request_method"`
	RequestEncoding  string   `json:"request_encoding"`
	PeriodParameter  string   `json:"period_parameter"`
	PeriodParameters []string `json:"period_parameters,omitempty"`
	PeriodSeparator  string   `json:"period_separator,omitempty"`
	// Parameters are configuration-owned, fixed operation inputs. They are
	// never populated from an API or task payload.
	Parameters        map[string]string `json:"parameters,omitempty"`
	PageParameter     string            `json:"page_parameter,omitempty"`
	PageSizeParameter string            `json:"page_size_parameter,omitempty"`
	PageSize          int               `json:"page_size,omitempty"`
	ResponseEncoding  string            `json:"response_encoding"`
}

// EndpointSet describes one education-level system without coupling the
// application service to that system's route layout.
type EndpointSet struct {
	ServiceURL string            `json:"service_url"`
	Periods    OperationEndpoint `json:"periods"`
	Courses    OperationEndpoint `json:"courses"`
	// CoursesFallback is an optional secondary schedule source used when the
	// primary course operation returns a valid but empty schedule.
	CoursesFallback OperationEndpoint `json:"courses_fallback,omitempty"`
	Grades          OperationEndpoint `json:"grades"`
	Exams           OperationEndpoint `json:"exams"`
	Selections      OperationEndpoint `json:"selections"`
	CourseCatalog   OperationEndpoint `json:"course_catalog"`
}

// OUCConfig contains the hot-reloadable OUC integration settings.
type OUCConfig struct {
	Version          int    `json:"version"`
	SSOLoginURL      string `json:"sso_login_url"`
	PortalServiceURL string `json:"portal_service_url"`
	PortalNoRedirect bool   `json:"portal_no_auto_redirect"`
	TraceEnabled     bool   `json:"trace_enabled"`
	RequestTimeoutMS int    `json:"request_timeout_ms"`
	// SessionTTLSeconds is a v1 compatibility value. New configurations use
	// independently bounded identity and target-system lifetimes.
	SessionTTLSeconds int `json:"session_ttl_seconds,omitempty"`
	// A zero scoped TTL or validation window means no override: use its
	// documented default. Non-zero values must pass validateOUC bounds.
	SSOSessionTTLSeconds                  int    `json:"sso_session_ttl_seconds,omitempty"`
	UndergraduateSessionTTLSeconds        int    `json:"undergraduate_session_ttl_seconds,omitempty"`
	GraduateSessionTTLSeconds             int    `json:"graduate_session_ttl_seconds,omitempty"`
	UndergraduateSessionProbePath         string `json:"undergraduate_session_probe_path,omitempty"`
	UndergraduateSessionValidationSeconds int    `json:"undergraduate_session_validation_seconds,omitempty"`
	MaxResponseBytes                      int64  `json:"max_response_bytes"`
	UserAgent                             string `json:"user_agent"`
	// IndexSelectionSessionID is the configuration-owned selection batch ID
	// required by the undergraduate selection schedule entry page.
	IndexSelectionSessionID string      `json:"index_selection_session_id,omitempty"`
	Undergraduate           EndpointSet `json:"undergraduate"`
	Graduate                EndpointSet `json:"graduate"`
}

// RequestTimeout returns the validated request timeout.
func (c OUCConfig) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutMS) * time.Millisecond
}

// SessionTTL returns the validated encrypted session-cache lifetime.
func (c OUCConfig) SessionTTL() time.Duration {
	return c.SSOSessionTTL()
}

// SSOSessionTTL returns the identity-session lifetime. A zero scoped value
// means no override. Old configuration snapshots retain their existing
// session_ttl_seconds behaviour.
func (c OUCConfig) SSOSessionTTL() time.Duration {
	seconds := c.SSOSessionTTLSeconds
	if seconds == 0 {
		seconds = c.SessionTTLSeconds
	}
	if seconds == 0 {
		seconds = 15 * 60
	}
	return time.Duration(seconds) * time.Second
}

// TargetSessionTTL returns the education-level target system cookie lifetime.
// A zero scoped value means no override and uses the documented default.
func (c OUCConfig) TargetSessionTTL(educationLevel string) time.Duration {
	seconds := c.UndergraduateSessionTTLSeconds
	if educationLevel == "graduate" {
		seconds = c.GraduateSessionTTLSeconds
	}
	if seconds == 0 {
		seconds = 60 * 60
	}
	return time.Duration(seconds) * time.Second
}

// UndergraduateProbePath returns the bounded undergraduate session probe path.
func (c OUCConfig) UndergraduateProbePath() string {
	if c.UndergraduateSessionProbePath == "" {
		return "/jsxsd/framework/xsMainV.htmlx"
	}
	return c.UndergraduateSessionProbePath
}

// UndergraduateValidationWindow returns the window in which a successful
// undergraduate probe or business request is trusted without a new probe. A
// zero configured value means no override and uses the documented default.
func (c OUCConfig) UndergraduateValidationWindow() time.Duration {
	seconds := c.UndergraduateSessionValidationSeconds
	if seconds == 0 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

// Snapshot is an immutable provider-routing view.
type Snapshot struct {
	ActiveProvider  string
	OUC             OUCConfig
	mockCredentials string
}

// ProviderPolicy constrains which downstream providers one deployment may use.
// Review deployments deny real OUC access even if dynamic configuration changes.
type ProviderPolicy struct {
	AllowMock bool
	AllowOUC  bool
}

// MockCredentials returns the decrypted development/test/review mock fixture.
// Callers must not log or persist this value outside the configuration center.
func (s Snapshot) MockCredentials() string {
	return s.mockCredentials
}

// Resolver keeps the last valid dynamic configuration and refreshes it in the
// background. Requests never block on configuration-center I/O.
type Resolver struct {
	source   Source
	interval time.Duration
	policy   ProviderPolicy
	logger   *zap.Logger

	mu       sync.RWMutex
	snapshot Snapshot

	stopCh    chan struct{}
	stoppedCh chan struct{}
	stopOnce  sync.Once
}

// NewResolver validates and synchronously primes the resolver.
func NewResolver(
	ctx context.Context,
	source Source,
	interval time.Duration,
	policy ProviderPolicy,
	logger *zap.Logger,
) (*Resolver, error) {
	if source == nil {
		return nil, fmt.Errorf("academic config source is required")
	}
	resolver := &Resolver{
		source:    source,
		interval:  interval,
		policy:    policy,
		logger:    logger,
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
	if err := resolver.refresh(ctx); err != nil {
		return nil, err
	}
	return resolver, nil
}

// Resolve returns a copy of the latest valid snapshot.
func (r *Resolver) Resolve() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot
}

// MockCredentials returns the current decrypted development/test/review fixture.
func (r *Resolver) MockCredentials() string {
	return r.Resolve().MockCredentials()
}

// Start enables hot reload when interval is positive.
func (r *Resolver) Start(ctx context.Context) {
	if r == nil || r.interval <= 0 {
		return
	}
	go r.loop(ctx)
}

// Stop terminates the refresh goroutine.
func (r *Resolver) Stop() {
	if r == nil || r.interval <= 0 {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
		<-r.stoppedCh
	})
}

func (r *Resolver) loop(ctx context.Context) {
	defer close(r.stoppedCh)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			if err := r.refresh(ctx); err != nil && r.logger != nil {
				r.logger.Warn("refresh academic provider config failed", zap.Error(err))
			}
		}
	}
}

func (r *Resolver) refresh(ctx context.Context) error {
	values, err := r.source.ListDecrypted(ctx, Group)
	if err != nil {
		return fmt.Errorf("load academic provider config: %w", err)
	}
	snapshot, err := parse(values, r.policy)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.snapshot = snapshot
	r.mu.Unlock()
	return nil
}

func parse(values map[string]string, policy ProviderPolicy) (Snapshot, error) {
	active := strings.ToLower(strings.TrimSpace(values["active_provider"]))
	mockCredentials := strings.TrimSpace(values["mock_credentials"])
	if !policy.AllowMock {
		mockCredentials = ""
	}
	if active == "" && policy.AllowMock {
		active = ProviderMock
	}
	if active == "" {
		return Snapshot{}, nil
	}
	if active != ProviderMock && active != ProviderOUC {
		return Snapshot{}, fmt.Errorf("academic_provider.active_provider must be mock or ouc")
	}
	if active == ProviderMock && !policy.AllowMock {
		return Snapshot{}, fmt.Errorf("mock academic provider is disabled in this environment")
	}
	if active == ProviderOUC && !policy.AllowOUC {
		return Snapshot{}, fmt.Errorf("OUC academic provider is disabled in this environment")
	}

	rawOUC := strings.TrimSpace(values["ouc"])
	if rawOUC == "" {
		if active == ProviderMock {
			return Snapshot{
				ActiveProvider:  active,
				mockCredentials: mockCredentials,
			}, nil
		}
		return Snapshot{}, fmt.Errorf("academic_provider.ouc is required")
	}
	var config OUCConfig
	decoder := json.NewDecoder(strings.NewReader(rawOUC))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Snapshot{}, fmt.Errorf("decode academic_provider.ouc: %w", err)
	}
	if err := validateOUC(config); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		ActiveProvider:  active,
		OUC:             config,
		mockCredentials: mockCredentials,
	}, nil
}

func validateOUC(config OUCConfig) error {
	if config.Version != 1 {
		return fmt.Errorf("academic_provider.ouc version must be 1")
	}
	if config.RequestTimeoutMS < 1000 || config.RequestTimeoutMS > 12000 {
		return fmt.Errorf("academic_provider.ouc request_timeout_ms must be between 1000 and 12000")
	}
	if config.SessionTTLSeconds != 0 &&
		(config.SSOSessionTTLSeconds != 0 || config.UndergraduateSessionTTLSeconds != 0 || config.GraduateSessionTTLSeconds != 0) {
		return fmt.Errorf("academic_provider.ouc session_ttl_seconds cannot be combined with scoped session TTLs")
	}
	ssoFallback := config.SessionTTLSeconds
	if ssoFallback == 0 {
		ssoFallback = 15 * 60
	}
	for name, limits := range map[string]struct {
		value    int
		fallback int
	}{
		"sso_session_ttl_seconds":           {config.SSOSessionTTLSeconds, ssoFallback},
		"undergraduate_session_ttl_seconds": {config.UndergraduateSessionTTLSeconds, 3600},
		"graduate_session_ttl_seconds":      {config.GraduateSessionTTLSeconds, 3600},
	} {
		// Zero is an explicit request to retain the default rather than an
		// invalid TTL override.
		if limits.value == 0 {
			limits.value = limits.fallback
		}
		if limits.value < 60 || limits.value > 3600 {
			return fmt.Errorf("academic_provider.ouc %s must be between 60 and 3600", name)
		}
	}
	if err := validateUndergraduateProbePath(config.UndergraduateProbePath()); err != nil {
		return fmt.Errorf("invalid undergraduate_session_probe_path: %w", err)
	}
	// Zero keeps the default five-minute validation window; non-zero values
	// must stay within the safe range.
	if config.UndergraduateSessionValidationSeconds != 0 &&
		(config.UndergraduateSessionValidationSeconds < 30 || config.UndergraduateSessionValidationSeconds > 3600) {
		return fmt.Errorf("academic_provider.ouc undergraduate_session_validation_seconds must be between 30 and 3600")
	}
	if config.MaxResponseBytes < 64*1024 || config.MaxResponseBytes > 8*1024*1024 {
		return fmt.Errorf("academic_provider.ouc max_response_bytes is outside the safe range")
	}
	if value := strings.TrimSpace(config.IndexSelectionSessionID); value != "" && !regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`).MatchString(value) {
		return fmt.Errorf("academic_provider.ouc index_selection_session_id is invalid")
	}
	if err := validateURL(config.SSOLoginURL, "id.ouc.edu.cn"); err != nil {
		return fmt.Errorf("invalid sso_login_url: %w", err)
	}
	if err := validateURL(config.PortalServiceURL, "my.ouc.edu.cn"); err != nil {
		return fmt.Errorf("invalid portal_service_url: %w", err)
	}
	if err := validateEndpoint(config.Undergraduate, "jwgl2024.ouc.edu.cn"); err != nil {
		return fmt.Errorf("invalid undergraduate endpoint: %w", err)
	}
	if err := validateEndpoint(config.Graduate, "pgs.ouc.edu.cn"); err != nil {
		return fmt.Errorf("invalid graduate endpoint: %w", err)
	}
	return nil
}

func validateEndpoint(endpoint EndpointSet, expectedHost string) error {
	if err := validateURL(endpoint.ServiceURL, expectedHost); err != nil {
		return err
	}
	for name, operation := range map[string]struct {
		endpoint       OperationEndpoint
		requiresPeriod bool
	}{
		"periods":          {endpoint: endpoint.Periods},
		"courses":          {endpoint: endpoint.Courses, requiresPeriod: true},
		"courses_fallback": {endpoint: endpoint.CoursesFallback, requiresPeriod: true},
		"grades":           {endpoint: endpoint.Grades},
		"exams":            {endpoint: endpoint.Exams, requiresPeriod: true},
		"selections":       {endpoint: endpoint.Selections},
		"course_catalog":   {endpoint: endpoint.CourseCatalog, requiresPeriod: true},
	} {
		if strings.TrimSpace(operation.endpoint.Path) == "" {
			continue
		}
		if err := validateOperation(operation.endpoint, operation.requiresPeriod); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name == "course_catalog" {
			if err := validateCatalogPagination(operation.endpoint); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

func validateOperation(endpoint OperationEndpoint, requiresPeriod bool) error {
	if err := validateRelativePath(endpoint.Path); err != nil {
		return fmt.Errorf("path: %w", err)
	}
	if err := validatePeriodParameters(endpoint); err != nil {
		return err
	}
	if endpoint.ResponseEncoding != "json" && endpoint.ResponseEncoding != "html" {
		return fmt.Errorf("response_encoding must be json or html")
	}
	if endpoint.RequestMethod != "GET" && endpoint.RequestMethod != "POST" {
		return fmt.Errorf("request_method must be GET or POST")
	}
	switch endpoint.RequestEncoding {
	case "query":
		if endpoint.RequestMethod != "GET" {
			return fmt.Errorf("query request_encoding requires GET")
		}
	case "form", "json":
		if endpoint.RequestMethod != "POST" {
			return fmt.Errorf("%s request_encoding requires POST", endpoint.RequestEncoding)
		}
	default:
		return fmt.Errorf("request_encoding must be query, form or json")
	}
	if requiresPeriod &&
		!strings.Contains(endpoint.Path, "{period_id}") &&
		strings.TrimSpace(endpoint.PeriodParameter) == "" &&
		len(endpoint.PeriodParameters) == 0 {
		return fmt.Errorf("period parameter mapping or {period_id} path placeholder is required")
	}
	if err := validateFixedParameters(endpoint); err != nil {
		return err
	}
	return nil
}

func validateCatalogPagination(endpoint OperationEndpoint) error {
	if !validParameterName(strings.TrimSpace(endpoint.PageParameter)) {
		return fmt.Errorf("page_parameter is required")
	}
	if endpoint.PageSize < 1 || endpoint.PageSize > MaxCourseCatalogPageSize {
		return fmt.Errorf("page_size must be between 1 and %d", MaxCourseCatalogPageSize)
	}
	if parameter := strings.TrimSpace(endpoint.PageSizeParameter); parameter != "" && !validParameterName(parameter) {
		return fmt.Errorf("page_size_parameter is invalid")
	}
	return nil
}

func validateFixedParameters(endpoint OperationEndpoint) error {
	if len(endpoint.Parameters) > 16 {
		return fmt.Errorf("parameters exceeds the safe limit")
	}
	for name, value := range endpoint.Parameters {
		if !validParameterName(name) || len(value) > 256 {
			return fmt.Errorf("parameters contains an invalid entry")
		}
		if name == endpoint.PeriodParameter || name == endpoint.PageParameter || name == endpoint.PageSizeParameter {
			return fmt.Errorf("parameters cannot override mapped parameters")
		}
		for _, periodName := range endpoint.PeriodParameters {
			if name == periodName {
				return fmt.Errorf("parameters cannot override mapped parameters")
			}
		}
	}
	return nil
}

func validatePeriodParameters(endpoint OperationEndpoint) error {
	single := strings.TrimSpace(endpoint.PeriodParameter)
	multiple := endpoint.PeriodParameters
	if single != "" && len(multiple) != 0 {
		return fmt.Errorf("period_parameter and period_parameters are mutually exclusive")
	}
	if len(multiple) == 0 {
		if strings.TrimSpace(endpoint.PeriodSeparator) != "" {
			return fmt.Errorf("period_separator requires period_parameters")
		}
		return nil
	}
	if len(multiple) < 2 || len(multiple) > 4 {
		return fmt.Errorf("period_parameters must contain between 2 and 4 names")
	}
	separator := endpoint.PeriodSeparator
	if len(separator) != 1 ||
		strings.ContainsAny(separator, "%&=?#") {
		return fmt.Errorf("period_separator must be one safe character")
	}
	seen := make(map[string]struct{}, len(multiple))
	for _, rawName := range multiple {
		name := strings.TrimSpace(rawName)
		if !validParameterName(name) {
			return fmt.Errorf("period_parameters contains an invalid name")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("period_parameters contains a duplicate name")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validParameterName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(index == 0 || character < '0' || character > '9') &&
			character != '_' &&
			character != '-' &&
			character != '.' {
			return false
		}
	}
	return true
}

func validateURL(raw, expectedHost string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || host != expectedHost || parsed.User != nil {
		return fmt.Errorf("URL must use HTTPS and exact host %s", expectedHost)
	}
	if _, ok := allowedHosts[host]; !ok {
		return fmt.Errorf("host is not allowlisted")
	}
	return nil
}

func validateRelativePath(path string) error {
	parsed, err := url.Parse(strings.TrimSpace(path))
	if err != nil {
		return err
	}
	if parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return fmt.Errorf("must be an absolute-path reference")
	}
	return nil
}

// validateUndergraduateProbePath applies the stricter constraint used when the
// probe path is resolved into an authenticated academic profile URL. Operation
// paths deliberately remain able to carry fixed query parameters.
func validateUndergraduateProbePath(path string) error {
	if err := validateRelativePath(path); err != nil {
		return err
	}
	parsed, _ := url.Parse(strings.TrimSpace(path))
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("query and fragment are not allowed")
	}
	return nil
}
