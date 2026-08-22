// Package application coordinates authenticated academic queries.
package application

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/core/apperror"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
)

const (
	defaultCoursesTimeout    = 10 * time.Second
	defaultGradesTimeout     = 10 * time.Second
	defaultExamsTimeout      = 12 * time.Second
	defaultSelectionsTimeout = 12 * time.Second
	unknownEducationLevel    = "unknown"
)

var periodIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

var (
	// ErrProviderUnavailable indicates that the downstream academic provider cannot serve a query.
	ErrProviderUnavailable = errors.New("academic provider unavailable")
	// ErrPeriodNotFound indicates that a period does not exist or is not visible to the student.
	ErrPeriodNotFound = errors.New("academic period not found")
	// ErrInvalidCredentials indicates that the supplied downstream credentials were rejected.
	ErrInvalidCredentials = errors.New("invalid academic credentials")
	// ErrPasswordExpired indicates that the school accepted the credential but requires a password change.
	ErrPasswordExpired = errors.New("academic password expired")
	// ErrChallengeRequired indicates that the school requires an interactive CAPTCHA or device check.
	ErrChallengeRequired = errors.New("academic interactive challenge required")
	// ErrAccountRestricted indicates that the school account is locked or frozen.
	ErrAccountRestricted = errors.New("academic account restricted")
	// ErrContractChanged indicates that a successful upstream response no longer matches its configured contract.
	ErrContractChanged = errors.New("academic provider contract changed")
)

// StudentReference is the private identity supplied to an academic provider.
type StudentReference struct {
	UserID         uint64
	StudentNo      string
	Provider       string
	EducationLevel string
}

// Credential contains the request-scoped secret used only for the downstream call.
// It must never be persisted or logged.
type Credential struct {
	StudentNo string
	Password  string
}

// IdentityResolver maps the authenticated platform user to a verified student reference.
type IdentityResolver interface {
	ResolveStudent(context.Context, uint64) (StudentReference, error)
}

// CredentialFailureLimiter bounds credential guessing by authenticated user,
// authoritative student number and trusted client IP.
type CredentialFailureLimiter interface {
	Allow(context.Context, uint64, string, string) (bool, error)
	RecordFailure(context.Context, uint64, string, string) error
	Clear(context.Context, uint64, string, string) error
}

// Provider is the replaceable downstream boundary for academic data.
type Provider interface {
	ListCourses(context.Context, StudentReference, Credential, string) (domain.CourseSchedule, error)
	ListGrades(context.Context, StudentReference, Credential, string) ([]domain.Grade, error)
	ListExams(context.Context, StudentReference, Credential, string) ([]domain.Exam, error)
	ListCourseSelections(context.Context, StudentReference, Credential, string) ([]domain.CourseSelection, error)
}

// CacheState identifies the serving state of a Redis-backed query result.
type CacheState string

const (
	CacheStateFresh CacheState = "fresh"
	CacheStateStale CacheState = "stale"
)

// CacheMetadata describes a Redis-backed academic query result. It is absent
// when the current request obtained data directly from the downstream system.
type CacheMetadata struct {
	State      CacheState
	CachedAt   time.Time
	FreshUntil time.Time
}

// QueryResult carries academic records and optional cache provenance through
// the application boundary. Records is populated even when it is empty.
type QueryResult[T any] struct {
	Records T
	Cache   *CacheMetadata
}

// CacheAwareProvider is implemented by provider adapters that can report
// whether the current response came from the query-result cache. Plain
// Provider implementations remain valid and are treated as direct downstream
// responses for rolling compatibility.
type CacheAwareProvider interface {
	ListCoursesWithCache(context.Context, StudentReference, Credential, string) (QueryResult[domain.CourseSchedule], error)
	ListGradesWithCache(context.Context, StudentReference, Credential, string) (QueryResult[[]domain.Grade], error)
	ListExamsWithCache(context.Context, StudentReference, Credential, string) (QueryResult[[]domain.Exam], error)
	ListCourseSelectionsWithCache(context.Context, StudentReference, Credential, string) (QueryResult[[]domain.CourseSelection], error)
}

// PeriodCatalog generates academic periods from platform-owned calendar data.
type PeriodCatalog interface {
	ListPeriods(context.Context, StudentReference) ([]domain.Period, error)
}

// CalendarCatalog returns the public calendar without requiring a student identity.
type CalendarCatalog interface {
	GetCalendar(context.Context, string) (domain.Calendar, error)
}

// Option configures an academic Service.
type Option func(*Service)

// WithPeriodCatalog attaches the platform-owned academic calendar.
func WithPeriodCatalog(catalog PeriodCatalog) Option {
	return func(service *Service) {
		service.periods = catalog
	}
}

// WithCalendarCatalog attaches the public academic calendar.
func WithCalendarCatalog(catalog CalendarCatalog) Option {
	return func(service *Service) {
		service.calendar = catalog
	}
}

// WithProviderTimeout configures the same maximum duration for every
// downstream query. It is retained for callers that do not need per-operation
// budgets.
func WithProviderTimeout(timeout time.Duration) Option {
	return func(service *Service) {
		if timeout > 0 {
			service.coursesTimeout = timeout
			service.gradesTimeout = timeout
			service.examsTimeout = timeout
			service.selectionsTimeout = timeout
		}
	}
}

// WithOperationTimeouts configures independent cold-query budgets for each
// downstream academic operation. Non-positive values retain their defaults.
func WithOperationTimeouts(courses, grades, exams, selections time.Duration) Option {
	return func(service *Service) {
		if courses > 0 {
			service.coursesTimeout = courses
		}
		if grades > 0 {
			service.gradesTimeout = grades
		}
		if exams > 0 {
			service.examsTimeout = exams
		}
		if selections > 0 {
			service.selectionsTimeout = selections
		}
	}
}

// WithCredentialFailureLimiter attaches the distributed failed-credential limiter.
func WithCredentialFailureLimiter(limiter CredentialFailureLimiter) Option {
	return func(service *Service) {
		service.limiter = limiter
	}
}

// Service resolves the current student and delegates read operations to a Provider.
type Service struct {
	identities        IdentityResolver
	provider          Provider
	periods           PeriodCatalog
	calendar          CalendarCatalog
	limiter           CredentialFailureLimiter
	coursesTimeout    time.Duration
	gradesTimeout     time.Duration
	examsTimeout      time.Duration
	selectionsTimeout time.Duration
}

// NewService creates an academic query service.
func NewService(identities IdentityResolver, provider Provider, options ...Option) *Service {
	service := &Service{
		identities:        identities,
		provider:          provider,
		coursesTimeout:    defaultCoursesTimeout,
		gradesTimeout:     defaultGradesTimeout,
		examsTimeout:      defaultExamsTimeout,
		selectionsTimeout: defaultSelectionsTimeout,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// GetCalendar returns one public education-level calendar snapshot.
func (s *Service) GetCalendar(ctx context.Context, educationLevel string) (domain.Calendar, error) {
	if s.calendar == nil {
		return domain.Calendar{}, apperror.New(
			http.StatusServiceUnavailable,
			"academic_calendar_unavailable",
			"校历暂时不可用",
		)
	}
	value, err := s.calendar.GetCalendar(ctx, educationLevel)
	if err != nil {
		if ctx.Err() != nil {
			return domain.Calendar{}, ctx.Err()
		}
		return domain.Calendar{}, apperror.Wrap(
			http.StatusServiceUnavailable,
			"academic_calendar_unavailable",
			"校历暂时不可用",
			err,
		)
	}
	return value, nil
}

// ListPeriods returns every period visible to the authenticated user.
// Periods come from platform-owned calendar data and default to the undergraduate
// calendar when the user has no eligible bound academic identity.
func (s *Service) ListPeriods(ctx context.Context, userID uint64) ([]domain.Period, error) {
	student, err := s.resolvePeriodStudent(ctx, userID)
	if err != nil {
		return nil, err
	}
	if s.periods == nil {
		return nil, apperror.New(
			http.StatusServiceUnavailable,
			"academic_provider_unavailable",
			"教务服务暂时不可用",
		)
	}
	rows, err := s.periods.ListPeriods(ctx, student)
	if err != nil {
		return nil, providerError(ctx, err)
	}
	return rows, nil
}

func (s *Service) resolvePeriodStudent(
	ctx context.Context,
	userID uint64,
) (StudentReference, error) {
	student, err := s.resolveVerifiedStudent(ctx, userID)
	if err == nil {
		// Periods are platform-owned calendar data, so a legacy identity whose
		// education level was not detected can safely use the undergraduate
		// calendar. Provider-backed queries keep the original strict identity.
		if student.EducationLevel == unknownEducationLevel {
			student.EducationLevel = domain.EducationLevelUndergraduate
		}
		return student, nil
	}
	appError, ok := apperror.As(err)
	if !ok || appError.Code != "academic_verification_required" {
		return StudentReference{}, err
	}
	return StudentReference{
		UserID:         userID,
		EducationLevel: domain.EducationLevelUndergraduate,
	}, nil
}

// ListCourses returns official timetable entries and the timetable-wide note
// from the same provider snapshot for one period.
func (s *Service) ListCourses(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
	clientIP string,
) (QueryResult[domain.CourseSchedule], error) {
	student, credential, periodID, err := s.resolveQuery(ctx, userID, credential, periodID)
	if err != nil {
		return QueryResult[domain.CourseSchedule]{}, err
	}
	return queryWithCredentialLimit(ctx, s, userID, student, clientIP, s.coursesTimeout, func(providerContext context.Context) (QueryResult[domain.CourseSchedule], error) {
		if provider, ok := s.provider.(CacheAwareProvider); ok {
			return provider.ListCoursesWithCache(providerContext, student, credential, periodID)
		}
		rows, queryErr := s.provider.ListCourses(providerContext, student, credential, periodID)
		return QueryResult[domain.CourseSchedule]{Records: rows}, queryErr
	})
}

// ListGrades returns released results for one period, or every period when periodID is empty.
func (s *Service) ListGrades(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
	clientIP string,
) (QueryResult[[]domain.Grade], error) {
	student, credential, periodID, err := s.resolveOptionalPeriodQuery(
		ctx,
		userID,
		credential,
		periodID,
	)
	if err != nil {
		return QueryResult[[]domain.Grade]{}, err
	}
	grades, err := queryWithCredentialLimit(ctx, s, userID, student, clientIP, s.gradesTimeout, func(providerContext context.Context) (QueryResult[[]domain.Grade], error) {
		if provider, ok := s.provider.(CacheAwareProvider); ok {
			return provider.ListGradesWithCache(providerContext, student, credential, periodID)
		}
		rows, queryErr := s.provider.ListGrades(providerContext, student, credential, periodID)
		return QueryResult[[]domain.Grade]{Records: rows}, queryErr
	})
	if err != nil {
		return QueryResult[[]domain.Grade]{}, err
	}
	result := append([]domain.Grade(nil), grades.Records...)
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].PeriodID > result[right].PeriodID
	})
	grades.Records = result
	return grades, nil
}

// ListExams returns examination arrangements for one period.
func (s *Service) ListExams(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
	clientIP string,
) (QueryResult[[]domain.Exam], error) {
	student, credential, periodID, err := s.resolveQuery(ctx, userID, credential, periodID)
	if err != nil {
		return QueryResult[[]domain.Exam]{}, err
	}
	return queryWithCredentialLimit(ctx, s, userID, student, clientIP, s.examsTimeout, func(providerContext context.Context) (QueryResult[[]domain.Exam], error) {
		if provider, ok := s.provider.(CacheAwareProvider); ok {
			return provider.ListExamsWithCache(providerContext, student, credential, periodID)
		}
		rows, queryErr := s.provider.ListExams(providerContext, student, credential, periodID)
		return QueryResult[[]domain.Exam]{Records: rows}, queryErr
	})
}

// ListCourseSelections returns course-selection results for one period.
func (s *Service) ListCourseSelections(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
	clientIP string,
) (QueryResult[[]domain.CourseSelection], error) {
	student, credential, periodID, err := s.resolveQuery(ctx, userID, credential, periodID)
	if err != nil {
		return QueryResult[[]domain.CourseSelection]{}, err
	}
	return queryWithCredentialLimit(ctx, s, userID, student, clientIP, s.selectionsTimeout, func(providerContext context.Context) (QueryResult[[]domain.CourseSelection], error) {
		if provider, ok := s.provider.(CacheAwareProvider); ok {
			return provider.ListCourseSelectionsWithCache(providerContext, student, credential, periodID)
		}
		rows, queryErr := s.provider.ListCourseSelections(providerContext, student, credential, periodID)
		return QueryResult[[]domain.CourseSelection]{Records: rows}, queryErr
	})
}

func queryWithCredentialLimit[T any](
	ctx context.Context,
	service *Service,
	userID uint64,
	student StudentReference,
	clientIP string,
	timeout time.Duration,
	query func(context.Context) (QueryResult[T], error),
) (QueryResult[T], error) {
	var zero QueryResult[T]
	clientIP = strings.TrimSpace(clientIP)
	if service.limiter != nil {
		allowed, err := service.limiter.Allow(ctx, userID, student.StudentNo, clientIP)
		if err != nil {
			return zero, credentialLimiterError(ctx, err)
		}
		if !allowed {
			return zero, apperror.New(
				http.StatusTooManyRequests,
				"academic_credentials_limited",
				"尝试次数过多，请稍后再试",
			)
		}
	}

	providerContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := query(providerContext)
	if err != nil {
		if service.limiter != nil && errors.Is(err, ErrInvalidCredentials) {
			if recordErr := service.limiter.RecordFailure(ctx, userID, student.StudentNo, clientIP); recordErr != nil {
				return zero, credentialLimiterError(ctx, recordErr)
			}
		}
		return zero, providerError(ctx, err)
	}
	if service.limiter != nil {
		if err := service.limiter.Clear(ctx, userID, student.StudentNo, clientIP); err != nil {
			return zero, credentialLimiterError(ctx, err)
		}
	}
	return rows, nil
}

func credentialLimiterError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return apperror.Wrap(
		http.StatusServiceUnavailable,
		"academic_provider_unavailable",
		"教务服务暂时不可用",
		err,
	)
}

func (s *Service) resolveQuery(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
) (StudentReference, Credential, string, error) {
	return s.resolvePeriodQuery(ctx, userID, credential, periodID, false)
}

func (s *Service) resolveOptionalPeriodQuery(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
) (StudentReference, Credential, string, error) {
	return s.resolvePeriodQuery(ctx, userID, credential, periodID, true)
}

func (s *Service) resolvePeriodQuery(
	ctx context.Context,
	userID uint64,
	credential Credential,
	periodID string,
	allowEmpty bool,
) (StudentReference, Credential, string, error) {
	periodID = strings.TrimSpace(periodID)
	if (!allowEmpty || periodID != "") && !periodIDPattern.MatchString(periodID) {
		return StudentReference{}, Credential{}, "", apperror.New(
			http.StatusBadRequest,
			"invalid_academic_period",
			"学期标识无效",
		)
	}
	student, credential, err := s.resolveStudent(ctx, userID, credential)
	return student, credential, periodID, err
}

func (s *Service) resolveStudent(
	ctx context.Context,
	userID uint64,
	credential Credential,
) (StudentReference, Credential, error) {
	if userID == 0 {
		return StudentReference{}, Credential{}, apperror.New(http.StatusUnauthorized, "unauthorized", "请先登录")
	}
	if s.provider == nil {
		return StudentReference{}, Credential{}, apperror.New(
			http.StatusServiceUnavailable,
			"academic_provider_unavailable",
			"教务服务暂时不可用",
		)
	}
	student, err := s.resolveVerifiedStudent(ctx, userID)
	if err != nil {
		return StudentReference{}, Credential{}, err
	}
	credential.StudentNo = strings.TrimSpace(credential.StudentNo)
	if credential.StudentNo == "" || credential.Password == "" || len(credential.StudentNo) > 64 || len(credential.Password) > 256 {
		return StudentReference{}, Credential{}, apperror.New(
			http.StatusUnauthorized,
			"invalid_academic_credentials",
			"教务凭据无效",
		)
	}
	if credential.StudentNo != student.StudentNo {
		return StudentReference{}, Credential{}, apperror.New(
			http.StatusForbidden,
			"academic_identity_mismatch",
			"教务账号与当前已绑定身份不一致",
		)
	}
	return student, credential, nil
}

func (s *Service) resolveVerifiedStudent(
	ctx context.Context,
	userID uint64,
) (StudentReference, error) {
	if userID == 0 {
		return StudentReference{}, apperror.New(http.StatusUnauthorized, "unauthorized", "请先登录")
	}
	if s.identities == nil {
		return StudentReference{}, apperror.New(
			http.StatusServiceUnavailable,
			"academic_provider_unavailable",
			"教务服务暂时不可用",
		)
	}
	student, err := s.identities.ResolveStudent(ctx, userID)
	if err != nil {
		return StudentReference{}, err
	}
	student.StudentNo = strings.TrimSpace(student.StudentNo)
	if student.UserID != userID || student.StudentNo == "" {
		return StudentReference{}, apperror.New(
			http.StatusForbidden,
			"academic_verification_required",
			"完成教务认证后才能查询教务信息",
		)
	}
	return student, nil
}

func providerError(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, ErrProviderRetryable) {
		return apperror.Wrap(
			http.StatusServiceUnavailable,
			"academic_retryable",
			"教务服务暂时繁忙，请点击重试",
			err,
		)
	}
	switch {
	case errors.Is(err, ErrProviderBusy):
		return apperror.Wrap(
			http.StatusTooManyRequests,
			"academic_provider_busy",
			"当前查询人数较多，请稍后重试",
			err,
		)
	case errors.Is(err, ErrPeriodNotFound):
		return apperror.Wrap(
			http.StatusNotFound,
			"academic_period_not_found",
			"未找到该学期的教务信息",
			err,
		)
	case errors.Is(err, ErrInvalidCredentials):
		return apperror.Wrap(
			http.StatusUnauthorized,
			"invalid_academic_credentials",
			"教务凭据无效",
			err,
		)
	case errors.Is(err, ErrPasswordExpired):
		return apperror.Wrap(
			http.StatusConflict,
			"academic_password_expired",
			"统一身份认证密码已过期，请先修改密码后重试",
			err,
		)
	case errors.Is(err, ErrChallengeRequired):
		return apperror.Wrap(
			http.StatusConflict,
			"academic_challenge_required",
			"校方要求完成验证码或设备确认，请重新绑定后再试",
			err,
		)
	case errors.Is(err, ErrAccountRestricted):
		return apperror.Wrap(
			http.StatusLocked,
			"academic_account_restricted",
			"教务账号已被锁定或冻结，请联系校方处理后重试",
			err,
		)
	default:
		return apperror.Wrap(
			http.StatusServiceUnavailable,
			"academic_provider_unavailable",
			"教务服务暂时不可用",
			err,
		)
	}
}
