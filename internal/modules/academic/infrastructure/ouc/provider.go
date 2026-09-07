// Package ouc implements the OUC SSO and academic-system adapters.
package ouc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// ConfigResolver supplies the latest validated dynamic configuration.
type ConfigResolver interface {
	Resolve() academicconfig.Snapshot
}

// Provider implements both credential verification and academic queries.
type Provider struct {
	config      ConfigResolver
	sessions    SessionStore
	adapters    map[string]systemAdapter
	newClient   sessionClientFactory
	transport   *http.Transport
	logger      *zap.Logger
	observer    Observer
	diagnostics ContractDiagnosticCapture
	recovery    singleflight.Group
}

// ProviderOption customizes the OUC provider without changing its application
// contract.
type ProviderOption func(*Provider)

// WithSessionStore enables encrypted, short-lived school cookie reuse.
func WithSessionStore(store SessionStore) ProviderOption {
	return func(provider *Provider) {
		provider.sessions = store
	}
}

// WithHTTPProxy routes only OUC HTTP clients through the deployment-local
// proxy. It deliberately does not modify process-wide proxy environment or
// affect MySQL, Redis, RPC, or configuration-center traffic.
func WithHTTPProxy(proxyURL string) ProviderOption {
	return func(provider *Provider) {
		proxyURL = strings.TrimSpace(proxyURL)
		transport, err := newOUCTransport(proxyURL)
		if err != nil {
			provider.newClient = func(academicconfig.OUCConfig) (*http.Client, error) {
				return nil, err
			}
			return
		}
		if provider.transport != nil {
			provider.transport.CloseIdleConnections()
		}
		provider.transport = transport
		provider.newClient = func(config academicconfig.OUCConfig) (*http.Client, error) {
			return newSessionClientWithTransport(config, transport)
		}
	}
}

// WithLogger enables redacted OUC process tracing when the current dynamic
// academic_provider.ouc configuration has trace_enabled=true.
func WithLogger(logger *zap.Logger) ProviderOption {
	return func(provider *Provider) {
		provider.logger = logger
	}
}

// WithContractDiagnosticCapture stores raw upstream responses that fail page
// parsing. The capture implementation must never expose bodies through logs.
func WithContractDiagnosticCapture(capture ContractDiagnosticCapture) ProviderOption {
	return func(provider *Provider) {
		provider.diagnostics = capture
	}
}

func withSessionClientFactory(factory sessionClientFactory) ProviderOption {
	return func(provider *Provider) {
		if factory != nil {
			provider.newClient = factory
		}
	}
}

// NewProvider creates an OUC provider.
func NewProvider(config ConfigResolver, options ...ProviderOption) *Provider {
	transport, err := newOUCTransport("")
	provider := &Provider{
		config:    config,
		adapters:  defaultAdapters(),
		transport: transport,
		observer:  noopObserver{},
	}
	if err != nil {
		provider.newClient = func(academicconfig.OUCConfig) (*http.Client, error) {
			return nil, err
		}
	} else {
		provider.newClient = func(config academicconfig.OUCConfig) (*http.Client, error) {
			return newSessionClientWithTransport(config, transport)
		}
	}
	for _, option := range options {
		option(provider)
	}
	return provider
}

// Close releases idle connections held by the Provider-owned OUC transport.
// Active requests are left to finish according to their request contexts.
func (p *Provider) Close() error {
	if p != nil && p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	return nil
}

// Verify authenticates once and validates the authoritative portal identity JSON.
func (p *Provider) Verify(
	ctx context.Context,
	command verificationapp.VerificationCommand,
) (verificationapp.VerificationResult, error) {
	config, err := p.oucConfig()
	if err != nil {
		return verificationapp.VerificationResult{}, err
	}
	command.StudentNo = strings.TrimSpace(command.StudentNo)
	command.EducationLevel = strings.TrimSpace(command.EducationLevel)
	trace := newProcessTrace(ctx, config, p.logger, "verify").
		withStudent(command.StudentNo).
		withEducationLevel(command.EducationLevel).
		withObserver(p.observer)
	trace.step("verify.start")
	_, err = adapterFor(p.adapters, command.EducationLevel)
	if err != nil {
		trace.step(
			"verify.finish",
			zap.String("outcome", "invalid_education_level"),
		)
		return verificationapp.VerificationResult{}, verificationapp.ErrIdentityTypeMismatch
	}
	current, err := authenticate(
		ctx,
		config,
		command.StudentNo,
		command.Password,
		p.newClient,
		trace,
	)
	if err != nil {
		traceProviderResult(trace, "verify.finish", err)
		return verificationapp.VerificationResult{}, err
	}
	identity, err := parsePortalIdentity(current.page, command.StudentNo)
	if err != nil {
		outcome := "portal_identity_invalid"
		if errors.Is(err, errPortalStudentMismatch) {
			outcome = "portal_student_mismatch"
		}
		trace.failure("verify.finish", outcome, "contract_error")
		// Authentication has already succeeded at this point. A malformed or
		// mismatched portal identity is not evidence that the submitted password
		// was wrong, so never make clients remember it as rejected credentials.
		return verificationapp.VerificationResult{}, verificationapp.ErrProviderUnavailable
	}
	result := verificationapp.VerificationResult{
		RealName:       identity.RealName,
		Provider:       verificationapp.ProviderOUC,
		EducationLevel: command.EducationLevel,
	}
	p.saveIdentitySession(
		ctx,
		config,
		command.StudentNo,
		command.Password,
		current,
		trace,
	)
	trace.step(
		"verify.finish",
		zap.String("outcome", "success"),
	)
	return result, nil
}

func academicProfileURL(serviceURL string, path string) (string, error) {
	base, err := url.Parse(serviceURL)
	if err != nil ||
		base.Scheme != "https" ||
		!allowedOUCHost(base.Hostname()) {
		return "", fmt.Errorf("invalid academic service URL")
	}
	reference, err := url.Parse(strings.TrimSpace(path))
	if err != nil ||
		reference.IsAbs() ||
		reference.Host != "" ||
		reference.RawQuery != "" ||
		reference.Fragment != "" ||
		!strings.HasPrefix(reference.Path, "/") {
		return "", fmt.Errorf("invalid academic profile path")
	}
	target := base.ResolveReference(reference)
	if target.Scheme != "https" ||
		!strings.EqualFold(target.Hostname(), base.Hostname()) {
		return "", fmt.Errorf("academic profile path escaped configured host")
	}
	return target.String(), nil
}

// ListPeriods returns normalized academic periods.
func (p *Provider) ListPeriods(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
) ([]domain.Period, error) {
	response, err := p.query(ctx, student, credential, queryPeriods, "")
	if err != nil {
		return nil, err
	}
	items, parseErr := response.adapter.ParsePeriods(response.body, response.encoding)
	traceQueryParse(response, parseErr, len(items))
	return items, parseErr
}

// ListCourses returns one normalized timetable snapshot.
func (p *Provider) ListCourses(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	response, err := p.query(ctx, student, credential, queryCourses, periodID)
	if err != nil {
		return domain.CourseSchedule{}, err
	}
	result, parseErr := response.adapter.ParseCourses(response.body, response.encoding, periodID)
	traceQueryParse(response, parseErr, len(result.Courses))
	if parseErr == nil &&
		len(result.Courses) == 0 &&
		student.EducationLevel == verificationapp.EducationGraduate &&
		strings.TrimSpace(response.fallbackOperation.Path) != "" {
		fallbackResponse, fallbackErr := p.queryWithOperation(
			ctx,
			student,
			credential,
			queryCourses,
			periodID,
			&response.fallbackOperation,
		)
		if fallbackErr != nil {
			return domain.CourseSchedule{}, fallbackErr
		}
		fallbackResult, fallbackParseErr := fallbackResponse.adapter.ParseCourses(
			fallbackResponse.body,
			fallbackResponse.encoding,
			periodID,
		)
		traceQueryParse(fallbackResponse, fallbackParseErr, len(fallbackResult.Courses))
		return fallbackResult, fallbackParseErr
	}
	return result, parseErr
}

// GetCourseSelectionSchedule reads the currently selected undergraduate courses.
// The school requires entering the selection context before its notice page can
// expose table#tbData, so both GET requests must stay in this order.
func (p *Provider) GetCourseSelectionSchedule(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	config, err := p.oucConfig()
	if err != nil || strings.TrimSpace(config.IndexSelectionSessionID) == "" || student.EducationLevel != verificationapp.EducationUndergraduate {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	entry := academicconfig.OperationEndpoint{
		Path: "/jsxsd/xsxk/newXsxkzx", RequestMethod: "GET", RequestEncoding: "query", ResponseEncoding: "html",
		Parameters: map[string]string{"jx0502zbid": config.IndexSelectionSessionID, "isallsc": ""},
	}
	entryResponse, err := p.queryWithOperation(ctx, student, credential, queryCourseSelectionSchedule, periodID, &entry)
	if err != nil {
		return domain.CourseSchedule{}, err
	}
	traceQueryParse(entryResponse, nil, 0)
	notice := academicconfig.OperationEndpoint{Path: "/jsxsd/xsxk/xsxk_tzsm", RequestMethod: "GET", RequestEncoding: "query", ResponseEncoding: "html"}
	response, err := p.queryWithOperation(ctx, student, credential, queryCourseSelectionSchedule, periodID, &notice)
	if err != nil {
		return domain.CourseSchedule{}, err
	}
	result, parseErr := response.adapter.ParseCourseSelectionSchedule(response.body, response.encoding, periodID)
	traceQueryParse(response, parseErr, len(result.Courses))
	return result, parseErr
}

// ListGrades returns normalized released grades.
func (p *Provider) ListGrades(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Grade, error) {
	response, err := p.query(ctx, student, credential, queryGrades, periodID)
	if err != nil {
		return nil, err
	}
	items, parseErr := response.adapter.ParseGrades(response.body, response.encoding, periodID)
	traceQueryParse(response, parseErr, len(items))
	return items, parseErr
}

// ListExams returns normalized exam arrangements.
func (p *Provider) ListExams(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Exam, error) {
	response, err := p.query(ctx, student, credential, queryExams, periodID)
	if err != nil {
		return nil, err
	}
	items, parseErr := response.adapter.ParseExams(response.body, response.encoding, periodID)
	traceQueryParse(response, parseErr, len(items))
	return items, parseErr
}

// ListCourseSelections returns normalized selection results.
func (p *Provider) ListCourseSelections(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.CourseSelection, error) {
	response, err := p.query(ctx, student, credential, querySelections, periodID)
	if err != nil {
		return nil, err
	}
	items, parseErr := response.adapter.ParseSelections(response.body, response.encoding, periodID)
	traceQueryParse(response, parseErr, len(items))
	return items, parseErr
}

const maxCourseCatalogPage = 500

// ListCourseCatalogPage reads and parses one configured school-wide course
// catalog page. It is an internal provider boundary for future RPC assembly;
// credentials remain only in memory and are never included in its result.
func (p *Provider) ListCourseCatalogPage(
	ctx context.Context,
	credential application.Credential,
	educationLevel string,
	periodID string,
	page int,
) (domain.CourseCatalogPage, error) {
	if page < 1 || page > maxCourseCatalogPage ||
		strings.TrimSpace(periodID) == "" ||
		strings.TrimSpace(credential.StudentNo) == "" ||
		credential.Password == "" {
		return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
	}
	config, err := p.oucConfig()
	if err != nil {
		return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
	}
	trace := newProcessTrace(ctx, config, p.logger, "query.course_catalog").
		withStudent(credential.StudentNo).
		withEducationLevel(educationLevel).
		withObserver(p.observer)
	trace.step("query.start", zap.Bool("period_provided", true))
	adapter, err := adapterFor(p.adapters, educationLevel)
	if err != nil {
		trace.step("query.finish", zap.String("outcome", "unsupported_identity"))
		return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
	}
	endpoint := adapter.Endpoint(config)
	operation := endpoint.CourseCatalog
	if strings.TrimSpace(operation.Path) == "" {
		trace.failure("query.finish", "operation_unconfigured", "configuration_error")
		return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
	}
	target, requestBody, contentType, err := academicCatalogRequest(endpoint.ServiceURL, operation, periodID, page)
	if err != nil {
		trace.failure("query.finish", "invalid_operation_config", "configuration_error")
		return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
	}
	expectedURL, err := url.Parse(target)
	if err != nil {
		trace.failure("query.finish", "invalid_operation_config", "invalid_request")
		return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
	}
	// Scoped session keys keep undergraduate and graduate target cookies apart
	// without mutating the credential that is used for SSO authentication.
	sessionCredential := credential
	if _, scoped := p.sessions.(ScopedSessionStore); !scoped {
		// Preserve the legacy store's historical per-level isolation. The
		// prefixed value is a cache subject only; SSO always receives credential.
		sessionCredential.StudentNo = educationLevel + "\x00" + credential.StudentNo
	}
	const maxAttempts = 3
	var current *session
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if current == nil {
			current, err = p.sessionForCatalogQuery(
				ctx,
				config,
				credential,
				sessionCredential,
				educationLevel,
				endpoint.ServiceURL,
				trace,
			)
			if err != nil {
				p.observer.ObserveAcademicUpstreamAttempt(
					"course_catalog",
					upstreamAttemptOutcome(err, "session_error"),
					educationLevel,
				)
				if attempt < maxAttempts && ctx.Err() == nil && retryableSessionError(err) &&
					!errors.Is(err, verificationapp.ErrProviderRetryable) {
					// A transport or SSO availability failure is not proof that the
					// cached target cookie is invalid.
					continue
				}
				trace.failure("query.finish", "session_failure", academicErrorKind(err), zap.Int("attempt", attempt))
				return domain.CourseCatalogPage{}, queryProviderError(err)
			}
		}
		if _, seekErr := requestBody.Seek(0, 0); seekErr != nil {
			p.observer.ObserveAcademicUpstreamAttempt(
				"course_catalog",
				"invalid_request",
				educationLevel,
			)
			trace.failure("business_query", "invalid_operation_payload", "invalid_request", zap.Int("attempt", attempt))
			trace.failure("query.finish", "invalid_operation_payload", "invalid_request", zap.Int("attempt", attempt))
			return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
		}
		trace.step(
			"business_query",
			zap.String("outcome", "start"),
			zap.Int("attempt", attempt),
			zap.String("catalog_request_url", safeAcademicRequestURL(target)),
		)
		finalURL, body, requestErr := requestPageWithOptions(
			ctx, current.client, operation.RequestMethod, target, requestBody, contentType,
			config, trace, catalogRequestOptions(educationLevel, endpoint.ServiceURL, target),
		)
		if requestErr != nil {
			p.observer.ObserveAcademicUpstreamAttempt(
				"course_catalog",
				upstreamAttemptOutcome(requestErr, "request_error"),
				educationLevel,
			)
			trace.failure("business_query", "failure", academicErrorKind(requestErr), zap.Int("attempt", attempt))
			if ctx.Err() == nil && attempt < maxAttempts {
				if err = waitCatalogRetry(ctx, attempt); err != nil {
					trace.failure("query.finish", "retry_wait_failure", academicErrorKind(err), zap.Int("attempt", attempt))
					return domain.CourseCatalogPage{}, queryProviderError(err)
				}
				continue
			}
			trace.failure("query.finish", "business_query_failure", academicErrorKind(requestErr), zap.Int("attempt", attempt))
			return domain.CourseCatalogPage{}, queryProviderError(requestErr)
		}
		trace.step("business_query", zap.String("outcome", "success"), zap.Int("attempt", attempt))
		if detection := detectInteractiveChallenge(body); detection.Detected {
			p.observer.ObserveAcademicUpstreamAttempt(
				"course_catalog",
				"challenge_required",
				educationLevel,
			)
			p.deleteTargetSession(ctx, sessionCredential, sessionScopeForEducationLevel(educationLevel), trace)
			trace.step(
				"response_classification",
				zap.String("outcome", "challenge_required"),
				zap.String("rule", detection.Rule),
				zap.Int("attempt", attempt),
			)
			trace.step("query.finish", zap.String("outcome", "challenge_required"), zap.Int("attempt", attempt))
			return domain.CourseCatalogPage{}, application.ErrChallengeRequired
		}
		if finalURL.Scheme != "https" || finalURL.Hostname() != expectedURL.Hostname() ||
			(finalURL.Hostname() == "id.ouc.edu.cn" || finalURL.Hostname() == "my.ouc.edu.cn") || isAcademicLoginPage(body) {
			p.observer.ObserveAcademicUpstreamAttempt(
				"course_catalog",
				"session_rejected",
				educationLevel,
			)
			p.deleteTargetSession(ctx, sessionCredential, sessionScopeForEducationLevel(educationLevel), trace)
			trace.step(
				"response_classification",
				zap.String("outcome", "session_rejected"),
				zap.Int("attempt", attempt),
			)
			if attempt < maxAttempts {
				current = nil
				continue
			}
			trace.step("query.finish", zap.String("outcome", "session_rejected"), zap.Int("attempt", attempt))
			return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
		}
		trace.step("response_classification", zap.String("outcome", "accepted"), zap.Int("attempt", attempt))
		catalogPage, parseErr := parseCourseCatalogPage(educationLevel, periodID, page, operation, body)
		if parseErr != nil {
			p.captureContractDiagnostic(ContractDiagnosticSample{
				Body:           body,
				Encoding:       operation.ResponseEncoding,
				Operation:      "course_catalog",
				EducationLevel: educationLevel,
				Host:           finalURL.Hostname(),
				Path:           finalURL.Path,
				Stage:          "response_parse",
				Failure:        "contract_error",
			})
			p.observer.ObserveAcademicUpstreamAttempt(
				"course_catalog",
				"invalid_response",
				educationLevel,
			)
			trace.failure(
				"response_parse",
				"failure",
				"contract_error",
				zap.Int("attempt", attempt),
				zap.String("parse_failure", catalogParseFailureKind(parseErr)),
			)
			if attempt < maxAttempts && ctx.Err() == nil {
				if err = waitCatalogRetry(ctx, attempt); err != nil {
					trace.failure("query.finish", "retry_wait_failure", academicErrorKind(err), zap.Int("attempt", attempt))
					return domain.CourseCatalogPage{}, queryProviderError(err)
				}
				continue
			}
			trace.failure(
				"query.finish",
				"contract_changed",
				"contract_error",
				zap.String("parse_failure", catalogParseFailureKind(parseErr)),
			)
			return domain.CourseCatalogPage{}, application.ErrContractChanged
		}
		trace.step("response_parse", zap.String("outcome", "success"), zap.Int("attempt", attempt))
		p.saveTargetSession(ctx, config, sessionCredential.StudentNo, sessionCredential.Password, current, sessionScopeForEducationLevel(educationLevel), trace)
		p.observer.ObserveAcademicUpstreamAttempt(
			"course_catalog",
			"success",
			educationLevel,
		)
		trace.step("query.finish", zap.String("outcome", "success"))
		return catalogPage, nil
	}
	trace.failure("query.finish", "attempts_exhausted", "provider_error")
	return domain.CourseCatalogPage{}, application.ErrProviderUnavailable
}

func waitCatalogRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(attempt*2) * time.Second
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type queryKind int

const (
	queryPeriods queryKind = iota
	queryCourses
	queryGrades
	queryExams
	querySelections
	queryCourseSelectionSchedule
)

type queryResponse struct {
	body              []byte
	encoding          string
	adapter           systemAdapter
	trace             *processTrace
	onParseResult     func(error)
	diagnostic        ContractDiagnosticSample
	capture           ContractDiagnosticCapture
	fallbackOperation academicconfig.OperationEndpoint
}

func traceQueryParse(response queryResponse, err error, itemCount int) {
	if response.onParseResult != nil {
		response.onParseResult(err)
	}
	if err != nil {
		response.captureContractDiagnostic()
		response.trace.failure("response_parse", "failure", "contract_error")
		response.trace.failure("query.finish", "contract_changed", "contract_error")
		return
	}
	response.trace.step(
		"response_parse",
		zap.String("outcome", "success"),
		zap.Int("item_count", itemCount),
	)
	response.trace.step("query.finish", zap.String("outcome", "success"))
}

func (r queryResponse) captureContractDiagnostic() {
	if r.capture != nil {
		r.capture.Capture(r.diagnostic)
	}
}

func (p *Provider) captureContractDiagnostic(sample ContractDiagnosticSample) {
	if p != nil && p.diagnostics != nil {
		p.diagnostics.Capture(sample)
	}
}

func (p *Provider) query(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	kind queryKind,
	periodID string,
) (queryResponse, error) {
	return p.queryWithOperation(ctx, student, credential, kind, periodID, nil)
}

func (p *Provider) queryWithOperation(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	kind queryKind,
	periodID string,
	operationOverride *academicconfig.OperationEndpoint,
) (queryResponse, error) {
	config, err := p.oucConfig()
	if err != nil {
		return queryResponse{}, application.ErrProviderUnavailable
	}
	trace := newProcessTrace(ctx, config, p.logger, "query."+queryKindName(kind)).
		withStudent(student.StudentNo).
		withEducationLevel(student.EducationLevel).
		withObserver(p.observer)
	trace.step(
		"query.start",
		zap.Bool("period_provided", strings.TrimSpace(periodID) != ""),
	)
	adapter, err := adapterFor(p.adapters, student.EducationLevel)
	if err != nil || student.Provider != verificationapp.ProviderOUC {
		trace.step(
			"query.finish",
			zap.String("outcome", "unsupported_identity"),
		)
		return queryResponse{}, application.ErrProviderUnavailable
	}
	endpoint := adapter.Endpoint(config)
	operation := operationFor(endpoint, kind)
	if operationOverride != nil {
		operation = *operationOverride
	}
	if strings.TrimSpace(operation.Path) == "" {
		trace.failure("query.finish", "operation_unconfigured", "configuration_error")
		return queryResponse{}, application.ErrProviderUnavailable
	}
	target, requestBody, contentType, err := academicRequest(
		endpoint.ServiceURL,
		operation,
		periodID,
	)
	if err != nil {
		trace.failure("query.finish", "invalid_operation_config", "configuration_error")
		return queryResponse{}, application.ErrProviderUnavailable
	}
	expectedURL, err := url.Parse(target)
	if err != nil {
		trace.failure("query.finish", "invalid_operation_config", "configuration_error")
		return queryResponse{}, application.ErrProviderUnavailable
	}
	const maxAttempts = 2
	operationName := queryKindName(kind)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		current, sessionErr := p.sessionForQuery(
			ctx,
			config,
			student.EducationLevel,
			credential,
			endpoint.ServiceURL,
			trace,
		)
		if sessionErr != nil {
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				upstreamAttemptOutcome(sessionErr, "session_error"),
				student.EducationLevel,
			)
			trace.failure(
				"query.finish",
				"session_failure",
				academicErrorKind(sessionErr),
			)
			return queryResponse{}, queryProviderError(sessionErr)
		}
		if _, seekErr := requestBody.Seek(0, 0); seekErr != nil {
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				"invalid_request",
				student.EducationLevel,
			)
			trace.failure("business_query", "invalid_operation_payload", "invalid_request", zap.Int("attempt", attempt))
			trace.failure("query.finish", "invalid_operation_payload", "invalid_request", zap.Int("attempt", attempt))
			return queryResponse{}, application.ErrProviderUnavailable
		}
		trace.step("business_query", zap.String("outcome", "start"), zap.Int("attempt", attempt))
		finalURL, body, requestErr := requestPage(
			ctx,
			current.client,
			operation.RequestMethod,
			target,
			requestBody,
			contentType,
			config,
			trace,
		)
		if requestErr != nil {
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				upstreamAttemptOutcome(requestErr, "request_error"),
				student.EducationLevel,
			)
			trace.failure("business_query", "failure", academicErrorKind(requestErr), zap.Int("attempt", attempt))
			trace.failure("query.finish", "business_query_failure", academicErrorKind(requestErr), zap.Int("attempt", attempt))
			return queryResponse{}, queryProviderError(requestErr)
		}
		trace.step("business_query", zap.String("outcome", "success"), zap.Int("attempt", attempt))
		detection := detectInteractiveChallenge(body)
		trace.step(
			"query.challenge.decision",
			zap.Bool("detected", detection.Detected),
			zap.String("rule", detection.Rule),
		)
		if detection.Detected {
			trace.step(
				"response_classification",
				zap.String("outcome", "challenge_required"),
				zap.String("rule", detection.Rule),
				zap.Int("attempt", attempt),
			)
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				"challenge_required",
				student.EducationLevel,
			)
			p.deleteTargetSession(ctx, credential, sessionScopeForEducationLevel(student.EducationLevel), trace)
			trace.step(
				"query.finish",
				zap.String("outcome", "challenge_required"),
			)
			return queryResponse{}, application.ErrChallengeRequired
		}
		rejectionRule := ""
		switch {
		case finalURL.Scheme == "https" &&
			(finalURL.Hostname() == "id.ouc.edu.cn" || finalURL.Hostname() == "my.ouc.edu.cn"):
			rejectionRule = "sso_redirect"
		case finalURL.Scheme == "https" &&
			finalURL.Hostname() == expectedURL.Hostname() &&
			isAcademicLoginPage(body):
			rejectionRule = "service_login_page"
		}
		if rejectionRule != "" {
			trace.observeSessionRejected(requestHost(finalURL.String()))
			trace.step(
				"response_classification",
				zap.String("outcome", "session_rejected"),
				zap.String("rule", rejectionRule),
				zap.Int("attempt", attempt),
			)
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				"session_rejected",
				student.EducationLevel,
			)
			p.deleteTargetSession(ctx, credential, sessionScopeForEducationLevel(student.EducationLevel), trace)
			trace.step(
				"query.session.rejected",
				zap.Int("attempt", attempt),
				zap.String("rule", rejectionRule),
			)
			if attempt < maxAttempts {
				continue
			}
			trace.step(
				"query.finish",
				zap.String("outcome", "session_rejected"),
			)
			return queryResponse{}, application.ErrProviderUnavailable
		}
		if finalURL.Scheme != "https" ||
			finalURL.Hostname() != expectedURL.Hostname() {
			p.captureContractDiagnostic(ContractDiagnosticSample{
				Body:           body,
				Encoding:       operation.ResponseEncoding,
				Operation:      operationName,
				EducationLevel: student.EducationLevel,
				Host:           finalURL.Hostname(),
				Path:           finalURL.Path,
				Stage:          "response_classification",
				Failure:        "contract_error",
			})
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				"invalid_response",
				student.EducationLevel,
			)
			// A malformed non-login response is a provider contract failure, not
			// evidence that the target cookie expired.
			trace.failure("response_classification", "unexpected_location", "contract_error", zap.Int("attempt", attempt))
			trace.failure("query.finish", "unexpected_response_location", "contract_error", zap.Int("attempt", attempt))
			return queryResponse{}, application.ErrProviderUnavailable
		}
		if operation.ResponseEncoding == "json" && !json.Valid(body) {
			p.captureContractDiagnostic(ContractDiagnosticSample{
				Body:           body,
				Encoding:       operation.ResponseEncoding,
				Operation:      operationName,
				EducationLevel: student.EducationLevel,
				Host:           finalURL.Hostname(),
				Path:           finalURL.Path,
				Stage:          "response_parse",
				Failure:        "invalid_json",
			})
			p.observer.ObserveAcademicUpstreamAttempt(
				operationName,
				"invalid_response",
				student.EducationLevel,
			)
			trace.failure("response_parse", "invalid_json", "contract_error", zap.Int("attempt", attempt))
			trace.failure("query.finish", "invalid_json_response", "contract_error", zap.Int("attempt", attempt))
			return queryResponse{}, application.ErrProviderUnavailable
		}
		trace.step("response_classification", zap.String("outcome", "accepted"), zap.Int("attempt", attempt))
		fallbackOperation := academicconfig.OperationEndpoint{}
		if operationOverride == nil && kind == queryCourses {
			fallbackOperation = endpoint.CoursesFallback
		}
		return queryResponse{
			body:              body,
			encoding:          operation.ResponseEncoding,
			adapter:           adapter,
			trace:             trace,
			fallbackOperation: fallbackOperation,
			onParseResult: func(parseErr error) {
				if parseErr != nil {
					p.observer.ObserveAcademicUpstreamAttempt(
						operationName,
						"invalid_response",
						student.EducationLevel,
					)
					return
				}
				p.saveTargetSession(
					ctx,
					config,
					credential.StudentNo,
					credential.Password,
					current,
					sessionScopeForEducationLevel(student.EducationLevel),
					trace,
				)
				p.observer.ObserveAcademicUpstreamAttempt(
					operationName,
					"success",
					student.EducationLevel,
				)
			},
			diagnostic: ContractDiagnosticSample{
				Body:           body,
				Encoding:       operation.ResponseEncoding,
				Operation:      operationName,
				EducationLevel: student.EducationLevel,
				Host:           finalURL.Hostname(),
				Path:           finalURL.Path,
				Stage:          "response_parse",
				Failure:        "contract_error",
			},
			capture: p.diagnostics,
		}, nil
	}
	return queryResponse{}, application.ErrProviderUnavailable
}

func retryableSessionError(err error) bool {
	return err != nil &&
		!errors.Is(err, verificationapp.ErrInvalidCredentials) &&
		!errors.Is(err, verificationapp.ErrPasswordExpired) &&
		!errors.Is(err, verificationapp.ErrChallengeRequired) &&
		!errors.Is(err, verificationapp.ErrAccountRestricted) &&
		!errors.Is(err, verificationapp.ErrIdentityTypeMismatch) &&
		!errors.Is(err, verificationapp.ErrIdentityTypeAmbiguous) &&
		!errors.Is(err, verificationapp.ErrIdentityTypeUnresolved)
}

func traceProviderResult(trace *processTrace, stage string, err error, fields ...zap.Field) {
	kind := academicErrorKind(err)
	switch kind {
	case "invalid_credentials", "password_expired", "challenge_required", "account_restricted",
		"identity_ambiguous", "identity_unresolved":
		fields = append([]zap.Field{zap.String("outcome", kind)}, fields...)
		trace.step(stage, fields...)
	default:
		trace.failure(stage, kind, kind, fields...)
	}
}

func (p *Provider) oucConfig() (academicconfig.OUCConfig, error) {
	if p == nil || p.config == nil {
		return academicconfig.OUCConfig{}, verificationapp.ErrProviderUnavailable
	}
	snapshot := p.config.Resolve()
	if snapshot.ActiveProvider != academicconfig.ProviderOUC {
		return academicconfig.OUCConfig{}, verificationapp.ErrProviderUnavailable
	}
	return snapshot.OUC, nil
}

func (p *Provider) sessionForQuery(
	ctx context.Context,
	config academicconfig.OUCConfig,
	educationLevel string,
	credential application.Credential,
	serviceURL string,
	trace *processTrace,
) (*session, error) {
	scoped, ok := p.sessions.(ScopedSessionStore)
	if !ok {
		return p.sessionForLegacyQuery(ctx, config, credential, serviceURL, trace)
	}
	scope := sessionScopeForEducationLevel(educationLevel)
	if !validSessionScope(scope) {
		return nil, verificationapp.ErrIdentityTypeMismatch
	}

	if current, found, err := p.loadTargetSession(ctx, scoped, config, credential, scope, educationLevel, trace); err != nil {
		p.observer.ObserveAcademicSessionRecovery(educationLevel, "target_cookie", "failure")
		return nil, err
	} else if found {
		p.observer.ObserveAcademicSessionRecovery(educationLevel, "target_cookie", "success")
		return current, nil
	}
	p.observer.ObserveAcademicSessionRecovery(educationLevel, "target_cookie", "failure")

	const maxRecoveryElections = 2
	for election := 0; election < maxRecoveryElections; election++ {
		recoveryResult := p.recovery.DoChan(recoveryKey(scope, credential), func() (any, error) {
			// A canceled leader must not poison concurrent followers. The recovery is
			// detached from cancellation but remains bounded by both the incoming
			// deadline and the validated OUC request budget.
			recoveryCtx, cancel, callerDeadlineBound := detachedRecoveryContextWithSource(
				ctx,
				config.RequestTimeout(),
			)
			defer cancel()
			value := targetSessionRecoveryResult{
				traceID: trace.id,
			}
			finishError := func(err error) (any, error) {
				value.callerDeadlineExpired = callerDeadlineBound &&
					errors.Is(recoveryCtx.Err(), context.DeadlineExceeded)
				return value, err
			}
			// A concurrent leader may have completed the CAS hand-off while this
			// request was waiting.
			current, found, err := p.loadTargetSession(
				recoveryCtx,
				scoped,
				config,
				credential,
				scope,
				educationLevel,
				trace,
			)
			if err != nil {
				return finishError(err)
			}
			if !found {
				current, err = p.recoverTargetSession(
					recoveryCtx,
					scoped,
					config,
					credential,
					scope,
					educationLevel,
					serviceURL,
					trace,
				)
				if err != nil {
					return finishError(err)
				}
			}
			// DoChan returns the same value to every waiter. Share only an immutable,
			// ephemeral snapshot so each caller can restore its own CookieJar; Redis
			// persistence remains best-effort and is not required for this request.
			value.state, err = snapshotSessionForRecovery(current, scope)
			if err != nil {
				return finishError(err)
			}
			return value, nil
		})
		select {
		case <-ctx.Done():
			trace.step(
				"target_session_recovery",
				zap.String("outcome", "caller_canceled"),
				zap.String("error_kind", academicErrorKind(ctx.Err())),
			)
			return nil, ctx.Err()
		case result := <-recoveryResult:
			value, valueOK := result.Val.(targetSessionRecoveryResult)
			fields := []zap.Field{zap.Bool("shared", result.Shared)}
			if value.traceID != "" && value.traceID != trace.id {
				fields = append(fields, zap.String("recovery_trace_id", value.traceID))
			}
			if result.Err != nil {
				isFollower := valueOK && value.traceID != "" && value.traceID != trace.id
				if election == 0 &&
					result.Shared &&
					isFollower &&
					value.callerDeadlineExpired &&
					errors.Is(result.Err, context.DeadlineExceeded) &&
					ctx.Err() == nil {
					trace.step(
						"target_session_recovery",
						append([]zap.Field{zap.String("outcome", "shared_leader_deadline_retry")}, fields...)...,
					)
					continue
				}
				trace.failure("target_session_recovery", "failure", academicErrorKind(result.Err), fields...)
				return nil, result.Err
			}
			if !valueOK {
				trace.failure("target_session_recovery", "invalid_result", "provider_error", fields...)
				return nil, verificationapp.ErrProviderUnavailable
			}
			current, restoreErr := restoreSession(config, value.state, p.newClient)
			if restoreErr != nil {
				trace.failure("target_session_recovery", "restore_failure", "session_restore_error", fields...)
				return nil, verificationapp.ErrProviderUnavailable
			}
			fields = append([]zap.Field{zap.String("outcome", "success")}, fields...)
			trace.step("target_session_recovery", fields...)
			return current, nil
		}
	}
	return nil, verificationapp.ErrProviderUnavailable
}

func (p *Provider) sessionForLegacyQuery(
	ctx context.Context,
	config academicconfig.OUCConfig,
	credential application.Credential,
	serviceURL string,
	trace *processTrace,
) (*session, error) {
	if p.sessions != nil {
		state, found, loadErr := p.sessions.Load(
			ctx,
			credential.StudentNo,
			credential.Password,
		)
		trace.step(
			"session.cache.load",
			zap.Bool("found", found),
			zap.Bool("error", loadErr != nil),
		)
		if loadErr == nil && found {
			cached, restoreErr := restoreSession(config, state, p.newClient)
			if restoreErr == nil {
				_, accessOutcome, accessErr := accessService(
					ctx,
					cached,
					config,
					serviceURL,
					"query_target_cached",
					trace,
				)
				if accessErr != nil {
					trace.step(
						"session.cache.restore",
						zap.String("outcome", academicErrorKind(accessErr)),
					)
					return nil, accessErr
				}
				if accessOutcome == serviceAccessGranted {
					trace.step(
						"session.cache.restore",
						zap.String("outcome", "success"),
					)
					p.saveSession(
						ctx,
						config,
						credential.StudentNo,
						credential.Password,
						cached,
						trace,
					)
					return cached, nil
				}
				if accessOutcome == serviceAccessTargetRejected {
					trace.step("session.cache.restore", zap.String("outcome", "target_rejected"))
					return nil, verificationapp.ErrProviderUnavailable
				}
			}
			trace.step(
				"session.cache.restore",
				zap.String("outcome", "rejected"),
			)
			p.deleteSession(ctx, credential, trace)
		}
	} else {
		trace.step(
			"session.cache.load",
			zap.String("outcome", "disabled"),
		)
	}
	current, err := authenticate(
		ctx,
		config,
		credential.StudentNo,
		credential.Password,
		p.newClient,
		trace,
	)
	if err != nil {
		return nil, err
	}
	if _, accessOutcome, accessErr := accessService(
		ctx,
		current,
		config,
		serviceURL,
		"query_target",
		trace,
	); accessErr != nil {
		return nil, accessErr
	} else if accessOutcome != serviceAccessGranted {
		return nil, verificationapp.ErrProviderUnavailable
	}
	p.saveSession(
		ctx,
		config,
		credential.StudentNo,
		credential.Password,
		current,
		trace,
	)
	return current, nil
}

func sessionScopeForEducationLevel(educationLevel string) SessionScope {
	switch educationLevel {
	case verificationapp.EducationUndergraduate:
		return SessionScopeUndergraduate
	case verificationapp.EducationGraduate:
		return SessionScopeGraduate
	default:
		return ""
	}
}

func recoveryKey(scope SessionScope, credential application.Credential) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(scope))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(credential.StudentNo))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(credential.Password))
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

type targetSessionRecoveryResult struct {
	traceID               string
	state                 SessionState
	callerDeadlineExpired bool
}

// detachedRecoveryContext lets one recovery serve concurrent waiters after
// the initiating caller is canceled, but never extends the caller's original
// deadline or the configured OUC request budget.
func detachedRecoveryContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	recovery, cancel, _ := detachedRecoveryContextWithSource(ctx, timeout)
	return recovery, cancel
}

func detachedRecoveryContextWithSource(
	ctx context.Context,
	timeout time.Duration,
) (context.Context, context.CancelFunc, bool) {
	detached := context.WithoutCancel(ctx)
	now := time.Now()
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(now.Add(timeout)) {
		recovery, cancel := context.WithDeadline(detached, deadline)
		return recovery, cancel, true
	}
	recovery, cancel := context.WithTimeout(detached, timeout)
	return recovery, cancel, false
}

func (p *Provider) loadTargetSession(
	ctx context.Context,
	store ScopedSessionStore,
	config academicconfig.OUCConfig,
	credential application.Credential,
	scope SessionScope,
	educationLevel string,
	trace *processTrace,
) (*session, bool, error) {
	state, found, err := store.LoadScope(ctx, credential.StudentNo, credential.Password, scope)
	if err != nil {
		if errors.Is(err, errInvalidCachedSession) {
			trace.failure(
				"target_session_load",
				"invalid_cache",
				"session_cache_invalid",
				zap.String("scope", string(scope)),
			)
			p.deleteTargetSession(ctx, credential, scope, trace)
			p.observer.ObserveAcademicSessionCache(educationLevel, "rejected")
			return nil, false, nil
		}
		trace.failure(
			"target_session_load",
			"failure",
			academicErrorKind(err),
			zap.String("scope", string(scope)),
		)
		p.observer.ObserveAcademicSessionCache(educationLevel, "indeterminate")
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, false, contextErr
		}
		// Provider concurrency remains bounded, and regular queries also arrive
		// after the distributed limiter and per-student lease. Keep the session
		// cache best-effort so one Redis read failure does not prevent a bounded
		// credential login from serving the current request.
		return nil, false, nil
	}
	if !found {
		trace.step(
			"target_session_load",
			zap.String("outcome", "miss"),
			zap.String("scope", string(scope)),
		)
		p.observer.ObserveAcademicSessionCache(educationLevel, "miss")
		return nil, false, nil
	}
	trace.step(
		"target_session_load",
		zap.String("outcome", "hit"),
		zap.String("scope", string(scope)),
	)
	p.observer.ObserveAcademicSessionCache(educationLevel, "hit")
	current, err := restoreSession(config, state, p.newClient)
	if err != nil {
		trace.failure(
			"target_cookie_check",
			"restore_failure",
			"session_restore_error",
			zap.String("scope", string(scope)),
		)
		p.deleteTargetSession(ctx, credential, scope, trace)
		p.observer.ObserveAcademicSessionCache(educationLevel, "rejected")
		return nil, false, nil
	}
	if scope != SessionScopeUndergraduate {
		trace.step(
			"target_cookie_check",
			zap.String("outcome", "deferred_to_business_query"),
			zap.String("scope", string(scope)),
		)
		return current, true, nil
	}
	if state.ValidatedAt > 0 && time.Since(time.UnixMilli(state.ValidatedAt)) <= config.UndergraduateValidationWindow() {
		trace.step(
			"target_cookie_check",
			zap.String("outcome", "valid_cached"),
			zap.String("scope", string(scope)),
		)
		return current, true, nil
	}
	valid, probeErr := probeUndergraduateSession(ctx, current, config, trace)
	if probeErr != nil {
		// Transport, deadline, and non-2xx upstream errors are indeterminate;
		// preserve the target cookie for the next user-initiated request.
		trace.failure(
			"target_cookie_check",
			"indeterminate",
			academicErrorKind(probeErr),
			zap.String("scope", string(scope)),
		)
		p.observer.ObserveAcademicSessionCache(educationLevel, "indeterminate")
		return nil, false, probeErr
	}
	if !valid {
		trace.step(
			"target_cookie_check",
			zap.String("outcome", "rejected"),
			zap.String("scope", string(scope)),
		)
		p.deleteTargetSession(ctx, credential, scope, trace)
		p.observer.ObserveAcademicSessionCache(educationLevel, "rejected")
		return nil, false, nil
	}
	trace.step(
		"target_cookie_check",
		zap.String("outcome", "valid"),
		zap.String("scope", string(scope)),
	)
	p.saveTargetSession(ctx, config, credential.StudentNo, credential.Password, current, scope, trace)
	p.observer.ObserveAcademicSessionCache(educationLevel, "valid")
	return current, true, nil
}

func (p *Provider) recoverTargetSession(
	ctx context.Context,
	store ScopedSessionStore,
	config academicconfig.OUCConfig,
	credential application.Credential,
	scope SessionScope,
	educationLevel string,
	serviceURL string,
	trace *processTrace,
) (*session, error) {
	identity, found, err := store.LoadScope(ctx, credential.StudentNo, credential.Password, SessionScopeIdentity)
	if err != nil {
		if errors.Is(err, errInvalidCachedSession) {
			trace.failure("identity_session_load", "invalid_cache", "session_cache_invalid")
			p.deleteIdentitySession(ctx, credential, trace)
			p.observer.ObserveAcademicSessionRecovery(educationLevel, "sso_cookie", "failure")
			found = false
			err = nil
		} else {
			trace.failure("identity_session_load", "failure", academicErrorKind(err))
			p.observer.ObserveAcademicSessionRecovery(educationLevel, "sso_cookie", "failure")
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			// A session-cache infrastructure failure is not evidence that the
			// bound credential or the OUC service is unavailable. Continue with
			// the already rate-limited credential-login path.
			found = false
		}
	} else if found {
		trace.step("identity_session_load", zap.String("outcome", "hit"))
	} else {
		trace.step("identity_session_load", zap.String("outcome", "miss"))
	}
	if err == nil && found {
		cached, restoreErr := restoreSession(config, identity, p.newClient)
		if restoreErr == nil {
			trace.step("sso_handoff", zap.String("outcome", "start"))
			_, accessOutcome, accessErr := accessService(ctx, cached, config, serviceURL, "target_identity_handoff", trace)
			if accessErr != nil {
				trace.failure("sso_handoff", "failure", academicErrorKind(accessErr))
				p.observer.ObserveAcademicSessionRecovery(educationLevel, "sso_cookie", "failure")
				return nil, accessErr
			}
			if accessOutcome == serviceAccessGranted {
				trace.step("sso_handoff", zap.String("outcome", "success"))
				p.saveTargetSession(ctx, config, credential.StudentNo, credential.Password, cached, scope, trace)
				p.observer.ObserveAcademicSessionRecovery(educationLevel, "sso_cookie", "success")
				return cached, nil
			}
			if accessOutcome == serviceAccessTargetRejected {
				trace.step("sso_handoff", zap.String("outcome", "target_rejected"))
				p.observer.ObserveAcademicSessionRecovery(educationLevel, "sso_cookie", "failure")
				return nil, verificationapp.ErrProviderUnavailable
			}
			trace.step("sso_handoff", zap.String("outcome", "rejected"))
		} else {
			trace.failure("sso_handoff", "identity_restore_failure", "session_restore_error")
		}
		p.deleteIdentitySession(ctx, credential, trace)
		p.observer.ObserveAcademicSessionRecovery(educationLevel, "sso_cookie", "failure")
	} else {
		outcome := "skipped_identity_miss"
		if err != nil {
			outcome = "skipped_identity_cache_failure"
		}
		trace.step("sso_handoff", zap.String("outcome", outcome))
	}

	trace.step("credential_login", zap.String("outcome", "start"))
	current, err := authenticate(ctx, config, credential.StudentNo, credential.Password, p.newClient, trace)
	if err != nil {
		trace.failure("credential_login", "authentication_failure", academicErrorKind(err))
		p.observer.ObserveAcademicSessionRecovery(educationLevel, "credential_login", "failure")
		return nil, err
	}
	p.saveIdentitySession(ctx, config, credential.StudentNo, credential.Password, current, trace)
	_, accessOutcome, err := accessService(ctx, current, config, serviceURL, "target_credential_handoff", trace)
	if err != nil {
		trace.failure("credential_login", "target_handoff_failure", academicErrorKind(err))
		p.observer.ObserveAcademicSessionRecovery(educationLevel, "credential_login", "failure")
		return nil, err
	}
	if accessOutcome != serviceAccessGranted {
		trace.failure("credential_login", "target_rejected", "provider_error")
		p.observer.ObserveAcademicSessionRecovery(educationLevel, "credential_login", "failure")
		return nil, verificationapp.ErrProviderUnavailable
	}
	p.saveTargetSession(ctx, config, credential.StudentNo, credential.Password, current, scope, trace)
	trace.step("credential_login", zap.String("outcome", "success"))
	p.observer.ObserveAcademicSessionRecovery(educationLevel, "credential_login", "success")
	return current, nil
}

// sessionForCatalogQuery authenticates with the original credentials but uses
// a level-scoped cache identity. The latter is deliberately never sent to OUC.
func (p *Provider) sessionForCatalogQuery(
	ctx context.Context,
	config academicconfig.OUCConfig,
	authCredential application.Credential,
	cacheCredential application.Credential,
	educationLevel string,
	serviceURL string,
	trace *processTrace,
) (*session, error) {
	if _, ok := p.sessions.(ScopedSessionStore); ok {
		return p.sessionForQuery(ctx, config, educationLevel, authCredential, serviceURL, trace)
	}
	if p.sessions != nil {
		state, found, loadErr := p.sessions.Load(ctx, cacheCredential.StudentNo, cacheCredential.Password)
		trace.step("session.cache.load", zap.Bool("found", found), zap.Bool("error", loadErr != nil))
		if loadErr == nil && found {
			cached, restoreErr := restoreSession(config, state, p.newClient)
			if restoreErr == nil {
				// A cached catalog session has already completed the SSO service
				// hand-off. Probing the portal again before every page both adds an
				// unnecessary upstream request and may rotate affinity cookies.
				return cached, nil
			}
			p.deleteSession(ctx, cacheCredential, trace)
		}
	}
	current, err := authenticate(ctx, config, authCredential.StudentNo, authCredential.Password, p.newClient, trace)
	if err != nil {
		return nil, err
	}
	if _, accessOutcome, accessErr := accessService(ctx, current, config, serviceURL, "catalog_target", trace); accessErr != nil {
		return nil, accessErr
	} else if accessOutcome != serviceAccessGranted {
		return nil, verificationapp.ErrProviderUnavailable
	}
	p.saveSession(ctx, config, cacheCredential.StudentNo, cacheCredential.Password, current, trace)
	return current, nil
}

func (p *Provider) saveSession(
	ctx context.Context,
	config academicconfig.OUCConfig,
	studentNo string,
	password string,
	current *session,
	trace *processTrace,
) {
	if p.sessions == nil {
		trace.step(
			"session.cache.save",
			zap.String("outcome", "disabled"),
		)
		return
	}
	state, err := snapshotSession(current, config)
	if err != nil {
		trace.failure("session.cache.save", "snapshot_error", academicErrorKind(err))
		return
	}
	err = p.sessions.Save(
		ctx,
		studentNo,
		password,
		state,
		config.SessionTTL(),
	)
	if err != nil {
		trace.failure("session.cache.save", "store_error", academicErrorKind(err))
		return
	}
	trace.step(
		"session.cache.save",
		zap.String("outcome", "success"),
		zap.Int("cookie_sets", len(state.Sets)),
	)
}

func (p *Provider) saveIdentitySession(
	ctx context.Context,
	config academicconfig.OUCConfig,
	studentNo string,
	password string,
	current *session,
	trace *processTrace,
) {
	store, ok := p.sessions.(ScopedSessionStore)
	if !ok {
		p.saveSession(ctx, config, studentNo, password, current, trace)
		return
	}
	state, err := snapshotSessionScope(current, config, SessionScopeIdentity, time.Time{})
	if err == nil {
		err = store.SaveScope(ctx, studentNo, password, SessionScopeIdentity, state, config.SSOSessionTTL())
	}
	p.traceSessionSave(trace, SessionScopeIdentity, state, err)
}

func (p *Provider) saveTargetSession(
	ctx context.Context,
	config academicconfig.OUCConfig,
	studentNo string,
	password string,
	current *session,
	scope SessionScope,
	trace *processTrace,
) {
	store, ok := p.sessions.(ScopedSessionStore)
	if !ok {
		p.saveSession(ctx, config, studentNo, password, current, trace)
		return
	}
	state, err := snapshotSessionScope(current, config, scope, time.Now())
	if err == nil {
		educationLevel := verificationapp.EducationUndergraduate
		if scope == SessionScopeGraduate {
			educationLevel = verificationapp.EducationGraduate
		}
		err = store.SaveScope(ctx, studentNo, password, scope, state, config.TargetSessionTTL(educationLevel))
	}
	p.traceSessionSave(trace, scope, state, err)
}

func (p *Provider) traceSessionSave(trace *processTrace, scope SessionScope, state SessionState, err error) {
	if err != nil {
		trace.failure(
			"session_save",
			"failure",
			academicErrorKind(err),
			zap.String("scope", string(scope)),
		)
		return
	}
	trace.step("session_save", zap.String("scope", string(scope)), zap.String("outcome", "success"), zap.Int("cookie_sets", len(state.Sets)))
}

func (p *Provider) deleteSession(
	ctx context.Context,
	credential application.Credential,
	trace *processTrace,
) {
	if p.sessions == nil {
		trace.step(
			"session.cache.delete",
			zap.String("outcome", "disabled"),
		)
		return
	}
	if err := p.sessions.Delete(
		ctx,
		credential.StudentNo,
		credential.Password,
	); err != nil {
		trace.failure("session.cache.delete", "store_error", academicErrorKind(err))
		return
	}
	trace.step(
		"session.cache.delete",
		zap.String("outcome", "success"),
	)
}

func (p *Provider) deleteTargetSession(
	ctx context.Context,
	credential application.Credential,
	scope SessionScope,
	trace *processTrace,
) {
	if store, ok := p.sessions.(ScopedSessionStore); ok {
		if err := store.DeleteScope(ctx, credential.StudentNo, credential.Password, scope); err != nil {
			trace.failure(
				"target_session_delete",
				"failure",
				academicErrorKind(err),
				zap.String("scope", string(scope)),
			)
			return
		}
		trace.step("target_session_delete", zap.String("scope", string(scope)), zap.String("outcome", "success"))
		return
	}
	p.deleteSession(ctx, credential, trace)
}

func (p *Provider) deleteIdentitySession(
	ctx context.Context,
	credential application.Credential,
	trace *processTrace,
) {
	if store, ok := p.sessions.(ScopedSessionStore); ok {
		if err := store.DeleteScope(ctx, credential.StudentNo, credential.Password, SessionScopeIdentity); err != nil {
			trace.failure("identity_session_delete", "failure", academicErrorKind(err))
			return
		}
		trace.step("identity_session_delete", zap.String("outcome", "success"))
		return
	}
	p.deleteSession(ctx, credential, trace)
}

func operationFor(
	endpoint academicconfig.EndpointSet,
	kind queryKind,
) academicconfig.OperationEndpoint {
	switch kind {
	case queryCourses:
		return endpoint.Courses
	case queryGrades:
		return endpoint.Grades
	case queryExams:
		return endpoint.Exams
	case querySelections:
		return endpoint.Selections
	default:
		return endpoint.Periods
	}
}

func queryKindName(kind queryKind) string {
	switch kind {
	case queryCourses:
		return "courses"
	case queryGrades:
		return "grades"
	case queryExams:
		return "exams"
	case querySelections:
		return "selections"
	case queryCourseSelectionSchedule:
		return "course_selection_schedule"
	default:
		return "periods"
	}
}

func academicRequest(
	serviceURL string,
	endpoint academicconfig.OperationEndpoint,
	periodID string,
) (string, *bytes.Reader, string, error) {
	return academicRequestWithValues(serviceURL, endpoint, periodID, nil)
}

func academicCatalogRequest(
	serviceURL string,
	endpoint academicconfig.OperationEndpoint,
	periodID string,
	page int,
) (string, *bytes.Reader, string, error) {
	if page < 1 || endpoint.PageParameter == "" {
		return "", nil, "", fmt.Errorf("invalid academic catalog page")
	}
	values := url.Values{}
	values.Set(endpoint.PageParameter, strconv.Itoa(page))
	if endpoint.PageSize > 0 && endpoint.PageSizeParameter != "" {
		values.Set(endpoint.PageSizeParameter, strconv.Itoa(endpoint.PageSize))
	}
	return academicRequestWithValues(serviceURL, endpoint, periodID, values)
}

func catalogRequestOptions(educationLevel, serviceURL, target string) requestPageOptions {
	base, baseErr := url.Parse(serviceURL)
	requestURL, requestErr := url.Parse(target)
	if baseErr != nil || requestErr != nil || base.Scheme != "https" || base.Hostname() != requestURL.Hostname() {
		return requestPageOptions{}
	}
	base.Path, base.RawQuery, base.Fragment = "", "", ""
	switch educationLevel {
	case verificationapp.EducationUndergraduate:
		referer := *base
		referer.Path = "/jsxsd/xkgl/xkkb_find"
		return requestPageOptions{
			accept:        "application/json, text/javascript, */*; q=0.01",
			referer:       referer.String(),
			requestedWith: "XMLHttpRequest",
		}
	case verificationapp.EducationGraduate:
		referer := *base
		referer.Path = "/py/page/student/lnsjCxdc.htm"
		return requestPageOptions{
			accept:  "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			referer: referer.String(),
			origin:  base.String(),
		}
	default:
		return requestPageOptions{}
	}
}

func academicRequestWithValues(
	serviceURL string,
	endpoint academicconfig.OperationEndpoint,
	periodID string,
	extraValues url.Values,
) (string, *bytes.Reader, string, error) {
	path := endpoint.Path
	base, err := url.Parse(serviceURL)
	if err != nil {
		return "", nil, "", err
	}
	if strings.Contains(path, "{period_id}") {
		path = strings.ReplaceAll(path, "{period_id}", url.PathEscape(periodID))
	}
	reference, err := url.Parse(path)
	if err != nil {
		return "", nil, "", err
	}
	target := base.ResolveReference(reference)
	if target.Scheme != "https" || target.Hostname() != base.Hostname() {
		return "", nil, "", fmt.Errorf("academic endpoint escaped configured host")
	}
	periodValues, err := academicPeriodValues(endpoint, periodID)
	if err != nil {
		return "", nil, "", err
	}
	for name, value := range endpoint.Parameters {
		periodValues.Set(name, value)
	}
	for name, values := range extraValues {
		if len(values) != 1 {
			return "", nil, "", fmt.Errorf("invalid academic request values")
		}
		periodValues.Set(name, values[0])
	}
	if (periodID != "" || len(periodValues) > 0) &&
		!strings.Contains(path, url.PathEscape(periodID)) &&
		endpoint.RequestEncoding == "query" {
		query := target.Query()
		for name, values := range periodValues {
			for _, value := range values {
				query.Set(name, value)
			}
		}
		target.RawQuery = query.Encode()
	}
	var payload []byte
	contentType := ""
	if len(periodValues) > 0 && !strings.Contains(path, url.PathEscape(periodID)) {
		switch endpoint.RequestEncoding {
		case "form":
			payload = []byte(periodValues.Encode())
			contentType = "application/x-www-form-urlencoded"
		case "json":
			values := make(map[string]string, len(periodValues))
			for name, entries := range periodValues {
				if len(entries) != 1 {
					return "", nil, "", fmt.Errorf("invalid academic period mapping")
				}
				values[name] = entries[0]
			}
			payload, err = json.Marshal(values)
			if err != nil {
				return "", nil, "", err
			}
			contentType = "application/json"
		}
	}
	return target.String(), bytes.NewReader(payload), contentType, nil
}

func academicPeriodValues(
	endpoint academicconfig.OperationEndpoint,
	periodID string,
) (url.Values, error) {
	values := make(url.Values)
	if periodID == "" {
		return values, nil
	}
	if len(endpoint.PeriodParameters) == 0 {
		parameter := strings.TrimSpace(endpoint.PeriodParameter)
		if parameter == "" {
			return values, nil
		}
		values.Set(parameter, periodID)
		return values, nil
	}
	parts := strings.Split(periodID, endpoint.PeriodSeparator)
	if len(parts) != len(endpoint.PeriodParameters) {
		return nil, fmt.Errorf("academic period ID does not match configured mapping")
	}
	for index, rawName := range endpoint.PeriodParameters {
		name := strings.TrimSpace(rawName)
		value := strings.TrimSpace(parts[index])
		if name == "" || value == "" {
			return nil, fmt.Errorf("academic period mapping contains an empty value")
		}
		values.Set(name, value)
	}
	return values, nil
}

var _ application.Provider = (*Provider)(nil)
var _ verificationapp.Provider = (*Provider)(nil)
