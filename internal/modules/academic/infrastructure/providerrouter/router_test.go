package providerrouter

import (
	"context"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
)

type staticResolver struct{ snapshot academicconfig.Snapshot }

func (r staticResolver) Resolve() academicconfig.Snapshot { return r.snapshot }

type plainQueryProvider struct{}

func (plainQueryProvider) ListCourses(context.Context, application.StudentReference, application.Credential, string) (domain.CourseSchedule, error) {
	return domain.CourseSchedule{Courses: []domain.Course{{ID: "course"}}}, nil
}

func (plainQueryProvider) ListGrades(context.Context, application.StudentReference, application.Credential, string) ([]domain.Grade, error) {
	return []domain.Grade{{ID: "grade"}}, nil
}

func (plainQueryProvider) ListExams(context.Context, application.StudentReference, application.Credential, string) ([]domain.Exam, error) {
	return []domain.Exam{{ID: "exam"}}, nil
}

func (plainQueryProvider) ListCourseSelections(context.Context, application.StudentReference, application.Credential, string) ([]domain.CourseSelection, error) {
	return []domain.CourseSelection{{ID: "selection"}}, nil
}

type cacheAwareQueryProvider struct {
	plainQueryProvider
	metadata application.CacheMetadata
}

func (p cacheAwareQueryProvider) ListCoursesWithCache(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (application.QueryResult[domain.CourseSchedule], error) {
	rows, err := p.ListCourses(ctx, student, credential, periodID)
	return application.QueryResult[domain.CourseSchedule]{Records: rows, Cache: &p.metadata}, err
}

func (p cacheAwareQueryProvider) ListGradesWithCache(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (application.QueryResult[[]domain.Grade], error) {
	rows, err := p.ListGrades(ctx, student, credential, periodID)
	return application.QueryResult[[]domain.Grade]{Records: rows, Cache: &p.metadata}, err
}

func (p cacheAwareQueryProvider) ListExamsWithCache(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (application.QueryResult[[]domain.Exam], error) {
	rows, err := p.ListExams(ctx, student, credential, periodID)
	return application.QueryResult[[]domain.Exam]{Records: rows, Cache: &p.metadata}, err
}

func (p cacheAwareQueryProvider) ListCourseSelectionsWithCache(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (application.QueryResult[[]domain.CourseSelection], error) {
	rows, err := p.ListCourseSelections(ctx, student, credential, periodID)
	return application.QueryResult[[]domain.CourseSelection]{Records: rows, Cache: &p.metadata}, err
}

func TestRouterPreservesCacheMetadataForEveryQuery(t *testing.T) {
	cachedAt := time.Date(2026, time.August, 16, 6, 20, 0, 0, time.UTC)
	provider := cacheAwareQueryProvider{metadata: application.CacheMetadata{
		State: application.CacheStateFresh, CachedAt: cachedAt, FreshUntil: cachedAt.Add(time.Minute),
	}}
	router := New(
		staticResolver{snapshot: academicconfig.Snapshot{ActiveProvider: academicconfig.ProviderOUC}},
		nil, nil, provider, nil,
	)
	student := application.StudentReference{UserID: 1, StudentNo: "20260001"}
	credential := application.Credential{StudentNo: student.StudentNo, Password: "secret"}

	assertMetadata := func(metadata *application.CacheMetadata) {
		t.Helper()
		if metadata == nil || metadata.State != application.CacheStateFresh || !metadata.CachedAt.Equal(cachedAt) ||
			!metadata.FreshUntil.Equal(cachedAt.Add(time.Minute)) {
			t.Fatalf("cache metadata=%+v", metadata)
		}
	}
	if result, err := router.ListCoursesWithCache(context.Background(), student, credential, "period"); err != nil || len(result.Records.Courses) != 1 {
		t.Fatalf("courses result=%+v err=%v", result, err)
	} else {
		assertMetadata(result.Cache)
	}
	if result, err := router.ListGradesWithCache(context.Background(), student, credential, "period"); err != nil || len(result.Records) != 1 {
		t.Fatalf("grades result=%+v err=%v", result, err)
	} else {
		assertMetadata(result.Cache)
	}
	if result, err := router.ListExamsWithCache(context.Background(), student, credential, "period"); err != nil || len(result.Records) != 1 {
		t.Fatalf("exams result=%+v err=%v", result, err)
	} else {
		assertMetadata(result.Cache)
	}
	if result, err := router.ListCourseSelectionsWithCache(context.Background(), student, credential, "period"); err != nil || len(result.Records) != 1 {
		t.Fatalf("selections result=%+v err=%v", result, err)
	} else {
		assertMetadata(result.Cache)
	}
}

func TestRouterWrapsPlainProviderWithoutCacheMetadata(t *testing.T) {
	router := New(
		staticResolver{snapshot: academicconfig.Snapshot{ActiveProvider: academicconfig.ProviderOUC}},
		nil, nil, plainQueryProvider{}, nil,
	)
	result, err := router.ListCoursesWithCache(
		context.Background(), application.StudentReference{}, application.Credential{}, "period",
	)
	if err != nil || len(result.Records.Courses) != 1 || result.Cache != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
