// Package ratelimitconfig owns the validated, hot-reloadable anti-abuse policy.
package ratelimitconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// Group identifies the admin-only configuration-center group.
	Group = "security"
	// Key identifies the rate-limit document inside Group.
	Key = "rate_limits"

	defaultDocument = `{
  "version": 1,
  "auth": {
    "password_login_ip": {"limit": 300, "window_seconds": 900},
    "login_failure_username": {"limit": 5, "window_seconds": 900},
    "wechat_login_ip": {"limit": 600, "window_seconds": 900},
    "refresh_family": {"limit": 20, "window_seconds": 300},
    "refresh_ip": {"limit": 300, "window_seconds": 300}
  },
  "academic_credentials": {
    "user_failure": {"limit": 5, "window_seconds": 900},
    "student_failure": {"limit": 5, "window_seconds": 900},
    "ip_failure": {"limit": 50, "window_seconds": 900}
  },
  "academic_query": {
    "global_rate": 10,
    "global_burst": 20
  },
  "error_report": {
    "source_ip": {"limit": 300, "window_seconds": 900}
  },
  "private_message": {
    "actor": {"limit": 60, "window_seconds": 60},
    "conversation": {"limit": 30, "window_seconds": 60}
  }
}`
)

// Rule is one fixed-window limiter rule.
type Rule struct {
	Limit         int64 `json:"limit"`
	WindowSeconds int64 `json:"window_seconds"`
}

// Window returns the configured fixed-window duration.
func (r Rule) Window() time.Duration { return time.Duration(r.WindowSeconds) * time.Second }

// AuthPolicy contains authentication endpoint limits.
type AuthPolicy struct {
	PasswordLoginIP      Rule `json:"password_login_ip"`
	LoginFailureUsername Rule `json:"login_failure_username"`
	WeChatLoginIP        Rule `json:"wechat_login_ip"`
	RefreshFamily        Rule `json:"refresh_family"`
	RefreshIP            Rule `json:"refresh_ip"`
}

// AcademicCredentialsPolicy contains failed school-credential limits.
type AcademicCredentialsPolicy struct {
	UserFailure    Rule `json:"user_failure"`
	StudentFailure Rule `json:"student_failure"`
	IPFailure      Rule `json:"ip_failure"`
}

// ErrorReportPolicy contains anonymous client-error ingestion limits.
type ErrorReportPolicy struct {
	SourceIP Rule `json:"source_ip"`
}

// PrivateMessagePolicy limits direct-message spam by sender and conversation.
type PrivateMessagePolicy struct {
	Actor        Rule `json:"actor"`
	Conversation Rule `json:"conversation"`
}

var defaultPrivateMessagePolicy = PrivateMessagePolicy{
	Actor:        Rule{Limit: 60, WindowSeconds: 60},
	Conversation: Rule{Limit: 30, WindowSeconds: 60},
}

// AcademicQueryPolicy protects the school system across all provider instances.
type AcademicQueryPolicy struct {
	GlobalRate  int `json:"global_rate"`
	GlobalBurst int `json:"global_burst"`
}

// Policy is the immutable runtime rate-limit snapshot.
type Policy struct {
	Version             int                       `json:"version"`
	Auth                AuthPolicy                `json:"auth"`
	AcademicCredentials AcademicCredentialsPolicy `json:"academic_credentials"`
	AcademicQuery       AcademicQueryPolicy       `json:"academic_query"`
	ErrorReport         ErrorReportPolicy         `json:"error_report"`
	PrivateMessage      PrivateMessagePolicy      `json:"private_message"`
	revision            string
	configured          bool
}

// Revision namespaces Redis counters. It changes whenever the canonical policy changes.
func (p Policy) Revision() string { return p.revision }

// Configured reports whether the snapshot came from a stored document.
func (p Policy) Configured() bool { return p.configured }

var defaultPolicy = func() Policy {
	policy := mustParse(defaultDocument)
	policy.configured = false
	return policy
}()

// Default returns the repository-owned safe policy.
func Default() Policy { return defaultPolicy }

// DefaultDocument returns the JSON seeded into the configuration center.
func DefaultDocument() string { return defaultDocument }

// ParseDocument decodes, validates and fingerprints one policy document.
func ParseDocument(value string) (Policy, error) {
	var policy Policy
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("限流配置必须是合法 JSON：%w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Policy{}, fmt.Errorf("限流配置只能包含一个 JSON 文档")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return Policy{}, fmt.Errorf("限流配置必须是合法 JSON：%w", err)
	}
	// Existing version-1 documents predate the private-message limiter. Treat a
	// missing section as the repository-safe default while still rejecting an
	// explicitly malformed or zero-valued section.
	if _, exists := raw["private_message"]; !exists {
		policy.PrivateMessage = defaultPrivateMessagePolicy
	}
	if err := validate(policy); err != nil {
		return Policy{}, err
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return Policy{}, fmt.Errorf("序列化限流配置：%w", err)
	}
	sum := sha256.Sum256(canonical)
	policy.revision = hex.EncodeToString(sum[:])
	policy.configured = true
	return policy, nil
}

// ValidateDocument validates a configuration-center update.
func ValidateDocument(value string) error {
	_, err := ParseDocument(value)
	return err
}

func validate(policy Policy) error {
	if policy.Version != 1 {
		return fmt.Errorf("限流配置 version 必须为 1")
	}
	rules := []struct {
		name      string
		rule      Rule
		min, max  int64
		minWindow int64
		maxWindow int64
	}{
		{"auth.password_login_ip", policy.Auth.PasswordLoginIP, 30, 10000, 60, 3600},
		{"auth.login_failure_username", policy.Auth.LoginFailureUsername, 3, 20, 60, 3600},
		{"auth.wechat_login_ip", policy.Auth.WeChatLoginIP, 100, 20000, 60, 3600},
		{"auth.refresh_family", policy.Auth.RefreshFamily, 5, 100, 60, 3600},
		{"auth.refresh_ip", policy.Auth.RefreshIP, 100, 20000, 60, 3600},
		{"academic_credentials.user_failure", policy.AcademicCredentials.UserFailure, 3, 20, 60, 3600},
		{"academic_credentials.student_failure", policy.AcademicCredentials.StudentFailure, 3, 20, 60, 3600},
		{"academic_credentials.ip_failure", policy.AcademicCredentials.IPFailure, 20, 10000, 60, 3600},
		{"error_report.source_ip", policy.ErrorReport.SourceIP, 100, 20000, 60, 3600},
		{"private_message.actor", policy.PrivateMessage.Actor, 10, 1000, 60, 3600},
		{"private_message.conversation", policy.PrivateMessage.Conversation, 5, 1000, 60, 3600},
	}
	for _, item := range rules {
		if item.rule.Limit < item.min || item.rule.Limit > item.max {
			return fmt.Errorf("%s.limit 必须在 %d 至 %d 之间", item.name, item.min, item.max)
		}
		if item.rule.WindowSeconds < item.minWindow || item.rule.WindowSeconds > item.maxWindow {
			return fmt.Errorf(
				"%s.window_seconds 必须在 %d 至 %d 之间",
				item.name,
				item.minWindow,
				item.maxWindow,
			)
		}
	}
	if policy.Auth.PasswordLoginIP.Limit < policy.Auth.LoginFailureUsername.Limit {
		return fmt.Errorf("auth.password_login_ip.limit 不能小于用户名失败限额")
	}
	if policy.Auth.RefreshIP.Limit < policy.Auth.RefreshFamily.Limit {
		return fmt.Errorf("auth.refresh_ip.limit 不能小于 refresh family 限额")
	}
	if policy.PrivateMessage.Actor.Limit < policy.PrivateMessage.Conversation.Limit {
		return fmt.Errorf("private_message.actor.limit 不能小于 conversation 限额")
	}
	identityLimit := max(
		policy.AcademicCredentials.UserFailure.Limit,
		policy.AcademicCredentials.StudentFailure.Limit,
	)
	if policy.AcademicCredentials.IPFailure.Limit < identityLimit {
		return fmt.Errorf("academic_credentials.ip_failure.limit 不能小于身份维度限额")
	}
	if policy.AcademicQuery.GlobalRate < 1 || policy.AcademicQuery.GlobalRate > 1000 {
		return fmt.Errorf("academic_query.global_rate 必须在 1 至 1000 之间")
	}
	if policy.AcademicQuery.GlobalBurst < 1 || policy.AcademicQuery.GlobalBurst > 2000 {
		return fmt.Errorf("academic_query.global_burst 必须在 1 至 2000 之间")
	}
	return nil
}

func mustParse(value string) Policy {
	policy, err := ParseDocument(value)
	if err != nil {
		panic(err)
	}
	return policy
}

// Source is implemented by configcenter.Service through ListDecrypted.
type Source interface {
	ListDecrypted(context.Context, string) (map[string]string, error)
}

// Provider supplies the current immutable policy to request-time limiters.
type Provider interface {
	Resolve() Policy
}

// Resolver caches the last valid policy and refreshes it outside request paths.
type Resolver struct {
	source   Source
	interval time.Duration
	logger   *zap.Logger

	mu       sync.RWMutex
	policy   Policy
	stopCh   chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// NewResolver loads the initial policy. A missing row uses repository defaults.
func NewResolver(
	ctx context.Context,
	source Source,
	interval time.Duration,
	logger *zap.Logger,
) (*Resolver, error) {
	if source == nil {
		return nil, fmt.Errorf("限流配置源不能为空")
	}
	resolver := &Resolver{
		source:   source,
		interval: interval,
		logger:   logger,
		policy:   Default(),
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	if err := resolver.refresh(ctx); err != nil {
		return nil, err
	}
	return resolver, nil
}

// Resolve returns the latest valid immutable policy.
func (r *Resolver) Resolve() Policy {
	if r == nil {
		return Default()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policy
}

// Start enables background refresh when interval is positive.
func (r *Resolver) Start(ctx context.Context) {
	if r == nil || r.interval <= 0 {
		return
	}
	go r.loop(ctx)
}

// Stop terminates the background refresh loop.
func (r *Resolver) Stop() {
	if r == nil || r.interval <= 0 {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stopCh)
		<-r.stopped
	})
}

func (r *Resolver) loop(ctx context.Context) {
	defer close(r.stopped)
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
				r.logger.Warn("refresh rate-limit config failed", zap.Error(err))
			}
		}
	}
}

func (r *Resolver) refresh(ctx context.Context) error {
	values, err := r.source.ListDecrypted(ctx, Group)
	if err != nil {
		return fmt.Errorf("读取限流配置：%w", err)
	}
	policy := Default()
	if value := strings.TrimSpace(values[Key]); value != "" {
		policy, err = ParseDocument(value)
		if err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.policy = policy
	r.mu.Unlock()
	return nil
}

var _ Provider = (*Resolver)(nil)
