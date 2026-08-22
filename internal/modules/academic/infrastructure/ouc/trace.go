package ouc

import (
	"context"
	"errors"
	"mime"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/weouc-plus/campus-academic/internal/core/requestmeta"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/application"
	"github.com/weouc-plus/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/weouc-plus/campus-academic/internal/modules/academic_verification/application"
	"go.uber.org/zap"
)

// processTrace emits one correlated, redacted view of an OUC operation. It
// deliberately has no API for recording credentials, cookies, tickets, query
// strings, request bodies, or response bodies.
type processTrace struct {
	enabled        bool
	logger         *zap.Logger
	id             string
	requestID      string
	operation      string
	educationLevel string
	student        string
	observer       Observer
}

func (t *processTrace) withObserver(observer Observer) *processTrace {
	if t != nil {
		t.observer = observer
	}
	return t
}

func (t *processTrace) observeHTTP(host, phase, outcome string, elapsed time.Duration) {
	if t == nil || t.observer == nil {
		return
	}
	if observer, ok := t.observer.(HTTPObserver); ok {
		observer.ObserveAcademicOUCHTTP(t.operation, host, phase, outcome, elapsed)
	}
}

func (t *processTrace) observeSessionRejected(host string) {
	if t == nil || t.observer == nil {
		return
	}
	if observer, ok := t.observer.(SessionRejectionObserver); ok {
		observer.ObserveAcademicOUCSessionRejected(t.operation, host)
	}
}

// withStudent attaches only a non-reversible display mask. It intentionally
// accepts no password or session material.
func (t *processTrace) withStudent(studentNo string) *processTrace {
	if t != nil {
		t.student = maskStudentNo(studentNo)
	}
	return t
}

// withEducationLevel keeps the diagnostic label bounded. Unknown values are
// still useful when an invalid request reaches the provider, but must not turn
// the process log into an unbounded user-controlled field.
func (t *processTrace) withEducationLevel(educationLevel string) *processTrace {
	if t == nil {
		return t
	}
	switch strings.TrimSpace(educationLevel) {
	case verificationapp.EducationUndergraduate:
		t.educationLevel = verificationapp.EducationUndergraduate
	case verificationapp.EducationGraduate:
		t.educationLevel = verificationapp.EducationGraduate
	default:
		t.educationLevel = "unknown"
	}
	return t
}

func newProcessTrace(
	ctx context.Context,
	config academicconfig.OUCConfig,
	logger *zap.Logger,
	operation string,
) *processTrace {
	return &processTrace{
		enabled:   config.TraceEnabled && logger != nil,
		logger:    logger,
		id:        "ouc-" + uuid.NewString(),
		requestID: requestmeta.RequestID(ctx),
		operation: operation,
	}
}

func (t *processTrace) step(stage string, fields ...zap.Field) {
	t.log(false, stage, fields...)
}

// failure records only a bounded error kind, never err.Error(). Upstream and
// storage errors can contain URLs or driver details, so their raw text is kept
// out of the trace by construction.
func (t *processTrace) failure(stage, outcome, errorKind string, fields ...zap.Field) {
	fields = append([]zap.Field{
		zap.String("outcome", outcome),
		zap.String("error_kind", errorKind),
	}, fields...)
	t.log(true, stage, fields...)
}

// recordLoginResponseError always records the bounded SSO error metadata even
// when detailed process tracing is disabled. The upstream message is required
// to maintain the business-error mapping; no surrounding response body is
// accepted by this API.
func (t *processTrace) recordLoginResponseError(outcome string, code int, message string) {
	if t == nil || t.logger == nil {
		return
	}
	t.write(false, "sso.login_response.parsed",
		zap.String("outcome", outcome),
		zap.Int("upstream_code", code),
		zap.String("upstream_message", message),
	)
}

func (t *processTrace) log(warn bool, stage string, fields ...zap.Field) {
	if t == nil || !t.enabled {
		return
	}
	t.write(warn, stage, fields...)
}

func (t *processTrace) write(warn bool, stage string, fields ...zap.Field) {
	base := []zap.Field{
		zap.String("component", "academic_ouc"),
		zap.String("trace_id", t.id),
		zap.String("operation", t.operation),
		zap.String("stage", stage),
	}
	if t.requestID != "" {
		base = append(base, zap.String("request_id", t.requestID))
	}
	if t.educationLevel != "" {
		base = append(base, zap.String("education_level", t.educationLevel))
	}
	if t.student != "" {
		base = append(base, zap.String("student_masked", t.student))
	}
	if warn {
		t.logger.Warn("OUC academic process", append(base, fields...)...)
		return
	}
	t.logger.Info("OUC academic process", append(base, fields...)...)
}

func maskStudentNo(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) < 6 {
		return strings.Repeat("*", len(runes))
	}
	middle := len(runes) - 4
	if middle > 28 {
		middle = 28
	}
	return string(runes[:2]) + strings.Repeat("*", middle) + string(runes[len(runes)-2:])
}

func safeURLFields(prefix string, target string) []zap.Field {
	parsed, err := url.Parse(target)
	if err != nil {
		return []zap.Field{
			zap.String(prefix+"_host", "invalid"),
			zap.String(prefix+"_path", "invalid"),
		}
	}
	return safeParsedURLFields(prefix, parsed)
}

func safeParsedURLFields(prefix string, target *url.URL) []zap.Field {
	if target == nil {
		return []zap.Field{
			zap.String(prefix+"_host", ""),
			zap.String(prefix+"_path", ""),
			zap.Strings(prefix+"_query_keys", nil),
		}
	}
	queryKeys := make([]string, 0, len(target.Query()))
	for key := range target.Query() {
		queryKeys = append(queryKeys, key)
	}
	sort.Strings(queryKeys)
	return []zap.Field{
		zap.String(prefix+"_host", strings.ToLower(target.Hostname())),
		zap.String(prefix+"_path", safeLogPath(target.EscapedPath())),
		zap.Strings(prefix+"_query_keys", queryKeys),
	}
}

func safeLogPath(path string) string {
	segments := strings.Split(path, "/")
	previousSensitive := false
	for index, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			segments[index] = ":redacted"
			previousSensitive = false
			continue
		}
		lower := strings.ToLower(decoded)
		sensitiveName := lower == "ticket" || lower == "token" || lower == "code" ||
			lower == "credential" || lower == "session"
		if previousSensitive || strings.HasPrefix(lower, "st-") || studentLikePathSegment(decoded) ||
			len([]rune(decoded)) > 64 || opaquePathSegment(decoded) {
			segments[index] = ":redacted"
		}
		previousSensitive = sensitiveName
	}
	return strings.Join(segments, "/")
}

func studentLikePathSegment(value string) bool {
	if len(value) < 6 || len(value) > 20 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func opaquePathSegment(value string) bool {
	if len(value) < 24 {
		return false
	}
	alphaNumeric := 0
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			alphaNumeric++
		case char == '-' || char == '_' || char == '.':
		default:
			return false
		}
	}
	return alphaNumeric*10 >= len(value)*8
}

func safeContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		return mediaType
	}
	value = strings.TrimSpace(strings.SplitN(value, ";", 2)[0])
	if len(value) > 80 {
		return value[:80]
	}
	return value
}

func safeRequestErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "network_error"
	}
}

func academicErrorKind(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, verificationapp.ErrInvalidCredentials),
		errors.Is(err, application.ErrInvalidCredentials):
		return "invalid_credentials"
	case errors.Is(err, verificationapp.ErrPasswordExpired),
		errors.Is(err, application.ErrPasswordExpired):
		return "password_expired"
	case errors.Is(err, verificationapp.ErrChallengeRequired),
		errors.Is(err, application.ErrChallengeRequired):
		return "challenge_required"
	case errors.Is(err, verificationapp.ErrAccountRestricted),
		errors.Is(err, application.ErrAccountRestricted):
		return "account_restricted"
	case errors.Is(err, verificationapp.ErrIdentityTypeAmbiguous):
		return "identity_ambiguous"
	case errors.Is(err, verificationapp.ErrIdentityTypeUnresolved):
		return "identity_unresolved"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "provider_error"
	}
}
