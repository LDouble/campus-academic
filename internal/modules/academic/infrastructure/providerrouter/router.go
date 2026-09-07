// Package providerrouter dispatches academic operations using the latest
// validated configuration snapshot.
package providerrouter

import (
	"context"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

// Resolver exposes the current provider mode.
type Resolver interface {
	Resolve() academicconfig.Snapshot
}

// Router implements both provider boundaries while keeping the application
// layer independent of provider selection.
type Router struct {
	config           Resolver
	mockQueries      application.Provider
	mockVerification verificationapp.Provider
	oucQueries       application.Provider
	oucVerification  verificationapp.Provider
}

// New creates a dynamic provider router.
func New(
	config Resolver,
	mockQueries application.Provider,
	mockVerification verificationapp.Provider,
	oucQueries application.Provider,
	oucVerification verificationapp.Provider,
) *Router {
	return &Router{
		config:           config,
		mockQueries:      mockQueries,
		mockVerification: mockVerification,
		oucQueries:       oucQueries,
		oucVerification:  oucVerification,
	}
}

// Verify routes credential verification without retaining the password.
func (r *Router) Verify(
	ctx context.Context,
	command verificationapp.VerificationCommand,
) (verificationapp.VerificationResult, error) {
	provider := r.verificationProvider()
	if provider == nil {
		return verificationapp.VerificationResult{}, verificationapp.ErrProviderUnavailable
	}
	return provider.Verify(ctx, command)
}

// ListCourses routes a timetable query.
func (r *Router) ListCourses(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	provider := r.queryProvider()
	if provider == nil {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	return provider.ListCourses(ctx, student, credential, periodID)
}

func (r *Router) GetCourseSelectionSchedule(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (domain.CourseSchedule, error) {
	provider := r.queryProvider()
	if provider == nil {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	scheduleProvider, ok := provider.(interface {
		GetCourseSelectionSchedule(context.Context, application.StudentReference, application.Credential, string) (domain.CourseSchedule, error)
	})
	if !ok {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	return scheduleProvider.GetCourseSelectionSchedule(ctx, student, credential, periodID)
}

// ListCoursesWithCache routes a timetable query and preserves result-cache
// provenance when the selected provider supports it.
func (r *Router) ListCoursesWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[domain.CourseSchedule], error) {
	provider := r.queryProvider()
	if provider == nil {
		return application.QueryResult[domain.CourseSchedule]{}, application.ErrProviderUnavailable
	}
	if cacheAware, ok := provider.(application.CacheAwareProvider); ok {
		return cacheAware.ListCoursesWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListCourses(ctx, student, credential, periodID)
	return application.QueryResult[domain.CourseSchedule]{Records: rows}, err
}

// ListGrades routes a grade query.
func (r *Router) ListGrades(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Grade, error) {
	provider := r.queryProvider()
	if provider == nil {
		return nil, application.ErrProviderUnavailable
	}
	return provider.ListGrades(ctx, student, credential, periodID)
}

// ListGradesWithCache routes a grade query and preserves result-cache provenance.
func (r *Router) ListGradesWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[[]domain.Grade], error) {
	provider := r.queryProvider()
	if provider == nil {
		return application.QueryResult[[]domain.Grade]{}, application.ErrProviderUnavailable
	}
	if cacheAware, ok := provider.(application.CacheAwareProvider); ok {
		return cacheAware.ListGradesWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListGrades(ctx, student, credential, periodID)
	return application.QueryResult[[]domain.Grade]{Records: rows}, err
}

// ListExams routes an examination query.
func (r *Router) ListExams(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Exam, error) {
	provider := r.queryProvider()
	if provider == nil {
		return nil, application.ErrProviderUnavailable
	}
	return provider.ListExams(ctx, student, credential, periodID)
}

// ListExamsWithCache routes an exam query and preserves result-cache provenance.
func (r *Router) ListExamsWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[[]domain.Exam], error) {
	provider := r.queryProvider()
	if provider == nil {
		return application.QueryResult[[]domain.Exam]{}, application.ErrProviderUnavailable
	}
	if cacheAware, ok := provider.(application.CacheAwareProvider); ok {
		return cacheAware.ListExamsWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListExams(ctx, student, credential, periodID)
	return application.QueryResult[[]domain.Exam]{Records: rows}, err
}

// ListCourseSelections routes a course-selection query.
func (r *Router) ListCourseSelections(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.CourseSelection, error) {
	provider := r.queryProvider()
	if provider == nil {
		return nil, application.ErrProviderUnavailable
	}
	return provider.ListCourseSelections(ctx, student, credential, periodID)
}

// ListCourseSelectionsWithCache routes a selection query and preserves
// result-cache provenance.
func (r *Router) ListCourseSelectionsWithCache(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (application.QueryResult[[]domain.CourseSelection], error) {
	provider := r.queryProvider()
	if provider == nil {
		return application.QueryResult[[]domain.CourseSelection]{}, application.ErrProviderUnavailable
	}
	if cacheAware, ok := provider.(application.CacheAwareProvider); ok {
		return cacheAware.ListCourseSelectionsWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListCourseSelections(ctx, student, credential, periodID)
	return application.QueryResult[[]domain.CourseSelection]{Records: rows}, err
}

func (r *Router) queryProvider() application.Provider {
	if r == nil || r.config == nil {
		return nil
	}
	switch r.config.Resolve().ActiveProvider {
	case academicconfig.ProviderOUC:
		return r.oucQueries
	case academicconfig.ProviderMock:
		return r.mockQueries
	default:
		return nil
	}
}

func (r *Router) verificationProvider() verificationapp.Provider {
	if r == nil || r.config == nil {
		return nil
	}
	switch r.config.Resolve().ActiveProvider {
	case academicconfig.ProviderOUC:
		return r.oucVerification
	case academicconfig.ProviderMock:
		return r.mockVerification
	default:
		return nil
	}
}

var _ application.Provider = (*Router)(nil)
var _ application.CacheAwareProvider = (*Router)(nil)
var _ verificationapp.Provider = (*Router)(nil)
