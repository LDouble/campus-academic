package application

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/core/apperror"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
)

type stubIdentityResolver struct {
	student StudentReference
	err     error
}

func (resolver stubIdentityResolver) ResolveStudent(
	_ context.Context,
	_ uint64,
) (StudentReference, error) {
	return resolver.student, resolver.err
}

type stubProvider struct {
	calls    []string
	students []StudentReference
	periods  []string
	grades   []domain.Grade
	err      error
	block    bool
	timeouts []time.Duration
}

type credentialLimitCall struct {
	userID    uint64
	studentNo string
	clientIP  string
}

type stubCredentialFailureLimiter struct {
	limit      int
	failures   int
	allowErr   error
	recordErr  error
	clearErr   error
	allowCalls []credentialLimitCall
	records    []credentialLimitCall
	clears     []credentialLimitCall
}

func (limiter *stubCredentialFailureLimiter) Allow(
	_ context.Context,
	userID uint64,
	studentNo string,
	clientIP string,
) (bool, error) {
	limiter.allowCalls = append(limiter.allowCalls, credentialLimitCall{userID, studentNo, clientIP})
	if limiter.allowErr != nil {
		return false, limiter.allowErr
	}
	return limiter.limit <= 0 || limiter.failures < limiter.limit, nil
}

func (limiter *stubCredentialFailureLimiter) RecordFailure(
	_ context.Context,
	userID uint64,
	studentNo string,
	clientIP string,
) error {
	limiter.records = append(limiter.records, credentialLimitCall{userID, studentNo, clientIP})
	if limiter.recordErr != nil {
		return limiter.recordErr
	}
	limiter.failures++
	return nil
}

func (limiter *stubCredentialFailureLimiter) Clear(
	_ context.Context,
	userID uint64,
	studentNo string,
	clientIP string,
) error {
	limiter.clears = append(limiter.clears, credentialLimitCall{userID, studentNo, clientIP})
	if limiter.clearErr != nil {
		return limiter.clearErr
	}
	limiter.failures = 0
	return nil
}

type stubPeriodCatalog struct {
	calls    int
	students []StudentReference
	periods  []domain.Period
	err      error
}

func (catalog *stubPeriodCatalog) ListPeriods(
	_ context.Context,
	student StudentReference,
) ([]domain.Period, error) {
	catalog.calls++
	catalog.students = append(catalog.students, student)
	if catalog.periods == nil {
		return []domain.Period{{ID: "period"}}, catalog.err
	}
	return catalog.periods, catalog.err
}

func (provider *stubProvider) record(
	ctx context.Context,
	call string,
	student StudentReference,
	periodID string,
) error {
	if deadline, ok := ctx.Deadline(); ok {
		provider.timeouts = append(provider.timeouts, time.Until(deadline))
	}
	provider.calls = append(provider.calls, call)
	provider.students = append(provider.students, student)
	provider.periods = append(provider.periods, periodID)
	if provider.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return provider.err
}

func (provider *stubProvider) ListPeriods(
	ctx context.Context,
	student StudentReference,
	_ Credential,
) ([]domain.Period, error) {
	return []domain.Period{{ID: "period"}}, provider.record(ctx, "periods", student, "")
}

func (provider *stubProvider) ListCourses(
	ctx context.Context,
	student StudentReference,
	_ Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	return domain.CourseSchedule{Courses: []domain.Course{{ID: "course"}}}, provider.record(ctx, "courses", student, periodID)
}

func (provider *stubProvider) ListGrades(
	ctx context.Context,
	student StudentReference,
	_ Credential,
	periodID string,
) ([]domain.Grade, error) {
	err := provider.record(ctx, "grades", student, periodID)
	if provider.grades == nil {
		return []domain.Grade{{ID: "grade"}}, err
	}
	return provider.grades, err
}

func (provider *stubProvider) ListExams(
	ctx context.Context,
	student StudentReference,
	_ Credential,
	periodID string,
) ([]domain.Exam, error) {
	return []domain.Exam{{ID: "exam"}}, provider.record(ctx, "exams", student, periodID)
}

func (provider *stubProvider) ListCourseSelections(
	ctx context.Context,
	student StudentReference,
	_ Credential,
	periodID string,
) ([]domain.CourseSelection, error) {
	return []domain.CourseSelection{{ID: "selection"}}, provider.record(ctx, "selections", student, periodID)
}

func TestServiceDelegatesEveryAcademicQuery(t *testing.T) {
	student := StudentReference{UserID: 7, StudentNo: "20260001"}
	provider := &stubProvider{}
	catalog := &stubPeriodCatalog{}
	service := NewService(
		stubIdentityResolver{student: student},
		provider,
		WithPeriodCatalog(catalog),
	)
	ctx := context.Background()
	credential := Credential{StudentNo: student.StudentNo, Password: "request-password"}

	periods, err := service.ListPeriods(ctx, student.UserID)
	if err != nil || len(periods) != 1 {
		t.Fatalf("ListPeriods() = %#v, %v", periods, err)
	}
	courses, err := service.ListCourses(ctx, student.UserID, credential, " 2025-2026-2 ", "")
	if err != nil || len(courses.Records.Courses) != 1 {
		t.Fatalf("ListCourses() = %#v, %v", courses, err)
	}
	grades, err := service.ListGrades(ctx, student.UserID, credential, "2025-2026-2", "")
	if err != nil || len(grades.Records) != 1 {
		t.Fatalf("ListGrades() = %#v, %v", grades, err)
	}
	allGrades, err := service.ListGrades(ctx, student.UserID, credential, "  ", "")
	if err != nil || len(allGrades.Records) != 1 {
		t.Fatalf("ListGrades(all) = %#v, %v", allGrades, err)
	}
	exams, err := service.ListExams(ctx, student.UserID, credential, "2025-2026-2", "")
	if err != nil || len(exams.Records) != 1 {
		t.Fatalf("ListExams() = %#v, %v", exams, err)
	}
	selections, err := service.ListCourseSelections(ctx, student.UserID, credential, "2025-2026-2", "")
	if err != nil || len(selections.Records) != 1 {
		t.Fatalf("ListCourseSelections() = %#v, %v", selections, err)
	}

	if want := []string{"courses", "grades", "grades", "exams", "selections"}; !reflect.DeepEqual(provider.calls, want) {
		t.Fatalf("provider calls = %v, want %v", provider.calls, want)
	}
	if catalog.calls != 1 || len(catalog.students) != 1 || catalog.students[0] != student {
		t.Fatalf("period catalog calls=%d students=%#v", catalog.calls, catalog.students)
	}
	for _, actual := range provider.students {
		if actual != student {
			t.Fatalf("provider student = %#v, want %#v", actual, student)
		}
	}
	if provider.periods[0] != "2025-2026-2" {
		t.Fatalf("trimmed period = %q", provider.periods[0])
	}
	if provider.periods[2] != "" {
		t.Fatalf("all-period query = %q", provider.periods[2])
	}
}

func TestServiceSortsGradesByPeriodDescending(t *testing.T) {
	t.Parallel()
	student := StudentReference{UserID: 7, StudentNo: "20260001"}
	provider := &stubProvider{grades: []domain.Grade{
		{ID: "old", PeriodID: "2024-2025-3"},
		{ID: "latest-first", PeriodID: "2025-2026-3"},
		{ID: "middle", PeriodID: "2025-2026-2"},
		{ID: "latest-second", PeriodID: "2025-2026-3"},
	}}
	service := NewService(stubIdentityResolver{student: student}, provider)

	grades, err := service.ListGrades(
		context.Background(),
		student.UserID,
		Credential{StudentNo: student.StudentNo, Password: "request-password"},
		"",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]string, 0, len(grades.Records))
	for _, grade := range grades.Records {
		actual = append(actual, grade.ID)
	}
	expected := []string{"latest-first", "latest-second", "middle", "old"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("grade order=%v, want %v", actual, expected)
	}
	if provider.grades[0].ID != "old" {
		t.Fatalf("provider-owned grades were mutated: %+v", provider.grades)
	}
}

func TestServiceCredentialFailuresAreSharedAcrossOperationsAndPasswords(t *testing.T) {
	student := StudentReference{UserID: 7, StudentNo: "20260001"}
	provider := &stubProvider{err: ErrInvalidCredentials}
	limiter := &stubCredentialFailureLimiter{limit: 1}
	service := NewService(
		stubIdentityResolver{student: student},
		provider,
		WithCredentialFailureLimiter(limiter),
	)
	const clientIP = "198.51.100.24"

	_, err := service.ListCourses(
		context.Background(),
		student.UserID,
		Credential{StudentNo: student.StudentNo, Password: "wrong-password-1"},
		"2025-2026-2",
		clientIP,
	)
	assertApplicationCode(t, err, "invalid_academic_credentials")

	tests := []struct {
		name     string
		password string
		query    func(Credential) error
	}{
		{
			name:     "grades",
			password: "wrong-password-2",
			query: func(credential Credential) error {
				_, queryErr := service.ListGrades(context.Background(), student.UserID, credential, "", clientIP)
				return queryErr
			},
		},
		{
			name:     "exams",
			password: "wrong-password-3",
			query: func(credential Credential) error {
				_, queryErr := service.ListExams(context.Background(), student.UserID, credential, "2025-2026-2", clientIP)
				return queryErr
			},
		},
		{
			name:     "course selections",
			password: "wrong-password-4",
			query: func(credential Credential) error {
				_, queryErr := service.ListCourseSelections(context.Background(), student.UserID, credential, "2025-2026-2", clientIP)
				return queryErr
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.query(Credential{StudentNo: student.StudentNo, Password: test.password})
			assertApplicationCode(t, err, "academic_credentials_limited")
		})
	}

	if want := []string{"courses"}; !reflect.DeepEqual(provider.calls, want) {
		t.Fatalf("provider calls = %v, want %v", provider.calls, want)
	}
	if len(limiter.allowCalls) != 4 || len(limiter.records) != 1 || len(limiter.clears) != 0 {
		t.Fatalf("limiter allows=%d records=%d clears=%d", len(limiter.allowCalls), len(limiter.records), len(limiter.clears))
	}
	wantScope := credentialLimitCall{userID: student.UserID, studentNo: student.StudentNo, clientIP: clientIP}
	for _, call := range append(append([]credentialLimitCall{}, limiter.allowCalls...), limiter.records...) {
		if call != wantScope {
			t.Fatalf("limiter scope = %#v, want %#v", call, wantScope)
		}
	}
}

func TestServiceSuccessfulQueryClearsCredentialFailures(t *testing.T) {
	student := StudentReference{UserID: 7, StudentNo: "20260001"}
	limiter := &stubCredentialFailureLimiter{limit: 2, failures: 1}
	service := NewService(
		stubIdentityResolver{student: student},
		&stubProvider{},
		WithCredentialFailureLimiter(limiter),
	)

	rows, err := service.ListExams(
		context.Background(),
		student.UserID,
		Credential{StudentNo: student.StudentNo, Password: "correct-password"},
		"2025-2026-2",
		"198.51.100.24",
	)
	if err != nil || len(rows.Records) != 1 {
		t.Fatalf("ListExams() = %#v, %v", rows, err)
	}
	if limiter.failures != 0 || len(limiter.clears) != 1 {
		t.Fatalf("failures=%d clear calls=%d", limiter.failures, len(limiter.clears))
	}
}

func TestServiceDoesNotCountNonCredentialProviderFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "period not found", err: ErrPeriodNotFound, code: "academic_period_not_found"},
		{name: "password expired", err: ErrPasswordExpired, code: "academic_password_expired"},
		{name: "challenge required", err: ErrChallengeRequired, code: "academic_challenge_required"},
		{name: "account restricted", err: ErrAccountRestricted, code: "academic_account_restricted"},
		{name: "provider unavailable", err: ErrProviderUnavailable, code: "academic_provider_unavailable"},
		{name: "unknown", err: errors.New("provider detail"), code: "academic_provider_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			student := StudentReference{UserID: 7, StudentNo: "20260001"}
			limiter := &stubCredentialFailureLimiter{limit: 1}
			provider := &stubProvider{err: test.err}
			service := NewService(
				stubIdentityResolver{student: student},
				provider,
				WithCredentialFailureLimiter(limiter),
			)

			_, err := service.ListCourses(
				context.Background(),
				student.UserID,
				Credential{StudentNo: student.StudentNo, Password: "candidate-password"},
				"2025-2026-2",
				"198.51.100.24",
			)
			assertApplicationCode(t, err, test.code)
			if limiter.failures != 0 || len(limiter.records) != 0 || len(limiter.clears) != 0 {
				t.Fatalf("failures=%d records=%d clears=%d", limiter.failures, len(limiter.records), len(limiter.clears))
			}
		})
	}
}

func TestServiceFailsClosedWhenCredentialLimiterUnavailable(t *testing.T) {
	tests := []struct {
		name              string
		providerErr       error
		limiter           *stubCredentialFailureLimiter
		wantProviderCalls int
	}{
		{
			name:              "allow",
			limiter:           &stubCredentialFailureLimiter{allowErr: errors.New("redis unavailable")},
			wantProviderCalls: 0,
		},
		{
			name:              "record invalid credential failure",
			providerErr:       ErrInvalidCredentials,
			limiter:           &stubCredentialFailureLimiter{recordErr: errors.New("redis unavailable")},
			wantProviderCalls: 1,
		},
		{
			name:              "clear after success",
			limiter:           &stubCredentialFailureLimiter{clearErr: errors.New("redis unavailable")},
			wantProviderCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			student := StudentReference{UserID: 7, StudentNo: "20260001"}
			provider := &stubProvider{err: test.providerErr}
			service := NewService(
				stubIdentityResolver{student: student},
				provider,
				WithCredentialFailureLimiter(test.limiter),
			)

			_, err := service.ListCourses(
				context.Background(),
				student.UserID,
				Credential{StudentNo: student.StudentNo, Password: "candidate-password"},
				"2025-2026-2",
				"198.51.100.24",
			)
			assertApplicationCode(t, err, "academic_provider_unavailable")
			if len(provider.calls) != test.wantProviderCalls {
				t.Fatalf("provider calls = %v, want count %d", provider.calls, test.wantProviderCalls)
			}
		})
	}
}

func TestServiceRejectsInvalidQueryInputs(t *testing.T) {
	service := NewService(
		stubIdentityResolver{student: StudentReference{UserID: 7, StudentNo: "20260001"}},
		&stubProvider{},
	)
	tests := []struct {
		name     string
		userID   uint64
		periodID string
		code     string
	}{
		{name: "missing user", periodID: "2025-2026-2", code: "unauthorized"},
		{name: "blank period", userID: 7, periodID: "  ", code: "invalid_academic_period"},
		{name: "unsafe period", userID: 7, periodID: "../2026", code: "invalid_academic_period"},
		{name: "query injection period", userID: 7, periodID: "2018:12&xn=2026", code: "invalid_academic_period"},
		{name: "long period", userID: 7, periodID: "a1234567890123456789012345678901234567890123456789012345678901234", code: "invalid_academic_period"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.ListCourses(
				context.Background(),
				test.userID,
				Credential{StudentNo: "20260001", Password: "request-password"},
				test.periodID,
				"",
			)
			assertApplicationCode(t, err, test.code)
		})
	}
}

func TestServiceAcceptsSafeOpaquePeriodIDs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		periodID string
	}{
		{name: "undergraduate", periodID: "2025-2026-2"},
		{name: "graduate", periodID: "2018:12"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := &stubProvider{}
			service := NewService(
				stubIdentityResolver{student: StudentReference{
					UserID:    7,
					StudentNo: "20260001",
				}},
				provider,
			)
			grades, err := service.ListGrades(
				context.Background(),
				7,
				Credential{
					StudentNo: "20260001",
					Password:  "request-password",
				},
				test.periodID,
				"",
			)
			if err != nil || len(grades.Records) != 1 {
				t.Fatalf("ListGrades() = %#v, %v", grades, err)
			}
			if len(provider.periods) != 1 || provider.periods[0] != test.periodID {
				t.Fatalf("provider periods=%v want=%q", provider.periods, test.periodID)
			}
		})
	}
}

func TestServiceRejectsInvalidResolvedIdentity(t *testing.T) {
	tests := []StudentReference{
		{UserID: 8, StudentNo: "20260001"},
		{UserID: 7, StudentNo: "  "},
	}
	for _, student := range tests {
		service := NewService(
			stubIdentityResolver{student: student},
			&stubProvider{},
			WithPeriodCatalog(&stubPeriodCatalog{}),
		)
		_, err := service.ListCourses(
			context.Background(),
			7,
			Credential{StudentNo: "20260001", Password: "request-password"},
			"2025-2026-2",
			"",
		)
		assertApplicationCode(t, err, "academic_verification_required")
	}
}

func TestServiceMapsProviderFailures(t *testing.T) {
	identity := stubIdentityResolver{student: StudentReference{UserID: 7, StudentNo: "20260001"}}
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "period", err: ErrPeriodNotFound, code: "academic_period_not_found"},
		{name: "password expired", err: ErrPasswordExpired, code: "academic_password_expired"},
		{name: "account restricted", err: ErrAccountRestricted, code: "academic_account_restricted"},
		{name: "retryable", err: NewProviderRetryableError(context.DeadlineExceeded), code: "academic_retryable"},
		{name: "unavailable", err: ErrProviderUnavailable, code: "academic_provider_unavailable"},
		{name: "unknown", err: errors.New("rpc implementation detail"), code: "academic_provider_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(identity, &stubProvider{err: test.err})
			_, err := service.ListCourses(
				context.Background(),
				7,
				Credential{StudentNo: "20260001", Password: "request-password"},
				"2025-2026-2",
				"",
			)
			assertApplicationCode(t, err, test.code)
			if test.name == "unknown" && err.Error() == test.err.Error() {
				t.Fatal("provider implementation detail leaked to the public error")
			}
		})
	}
}

func TestProviderErrorMapsRestrictedAccount(t *testing.T) {
	t.Parallel()

	err := providerError(context.Background(), ErrAccountRestricted)
	var appErr *apperror.Error
	if !errors.As(err, &appErr) ||
		appErr.Code != "academic_account_restricted" ||
		appErr.Status != http.StatusLocked {
		t.Fatalf("error=%v", err)
	}
}

func TestProviderErrorPreservesParentCancellationBeforeRetryableMapping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := providerError(ctx, NewProviderRetryableError(context.DeadlineExceeded))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want caller cancellation", err)
	}
}

func TestServiceBoundsProviderCallAndPreservesParentCancellation(t *testing.T) {
	identity := stubIdentityResolver{student: StudentReference{UserID: 7, StudentNo: "20260001"}}
	service := NewService(identity, &stubProvider{block: true}, WithProviderTimeout(10*time.Millisecond))
	started := time.Now()
	credential := Credential{StudentNo: "20260001", Password: "request-password"}
	_, err := service.ListCourses(context.Background(), 7, credential, "2025-2026-2", "")
	assertApplicationCode(t, err, "academic_provider_unavailable")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("provider timeout took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.ListCourses(ctx, 7, credential, "2025-2026-2", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query error = %v", err)
	}
}

func TestServiceUsesOperationSpecificProviderTimeouts(t *testing.T) {
	student := StudentReference{UserID: 7, StudentNo: "20260001"}
	provider := &stubProvider{}
	service := NewService(
		stubIdentityResolver{student: student},
		provider,
		WithOperationTimeouts(
			100*time.Millisecond,
			200*time.Millisecond,
			300*time.Millisecond,
			400*time.Millisecond,
		),
	)
	credential := Credential{StudentNo: student.StudentNo, Password: "request-password"}

	if _, err := service.ListCourses(context.Background(), student.UserID, credential, "2025-2026-2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListGrades(context.Background(), student.UserID, credential, "2025-2026-2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListExams(context.Background(), student.UserID, credential, "2025-2026-2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListCourseSelections(context.Background(), student.UserID, credential, "2025-2026-2", ""); err != nil {
		t.Fatal(err)
	}

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond}
	if len(provider.timeouts) != len(want) {
		t.Fatalf("provider timeouts=%v", provider.timeouts)
	}
	for index, timeout := range provider.timeouts {
		if timeout <= 0 || timeout > want[index] || timeout < want[index]-50*time.Millisecond {
			t.Fatalf("operation %d timeout=%s, want about %s", index, timeout, want[index])
		}
	}
}

func TestServiceRequiresLocalPeriodCatalogWithoutCallingProvider(t *testing.T) {
	student := StudentReference{UserID: 7, StudentNo: "20260001"}
	provider := &stubProvider{}
	service := NewService(stubIdentityResolver{student: student}, provider)
	_, err := service.ListPeriods(
		context.Background(),
		student.UserID,
	)
	assertApplicationCode(t, err, "academic_provider_unavailable")
	if len(provider.calls) != 0 {
		t.Fatalf("provider calls = %v, want none", provider.calls)
	}
}

func TestServiceUnavailableWithoutDependencies(t *testing.T) {
	service := NewService(nil, nil)
	_, err := service.ListPeriods(
		context.Background(),
		7,
	)
	assertApplicationCode(t, err, "academic_provider_unavailable")

	_, err = service.ListCourses(
		context.Background(),
		0,
		Credential{},
		"2025-2026-2",
		"",
	)
	assertApplicationCode(t, err, "unauthorized")
}

func TestServiceListsPeriodsUsingBoundEducationLevel(t *testing.T) {
	student := StudentReference{
		UserID:         7,
		StudentNo:      "20260001",
		EducationLevel: domain.EducationLevelGraduate,
	}
	catalog := &stubPeriodCatalog{}
	service := NewService(
		stubIdentityResolver{student: student},
		nil,
		WithPeriodCatalog(catalog),
	)

	periods, err := service.ListPeriods(context.Background(), student.UserID)
	if err != nil || len(periods) != 1 {
		t.Fatalf("ListPeriods() = %#v, %v", periods, err)
	}
	if catalog.calls != 1 || len(catalog.students) != 1 || catalog.students[0] != student {
		t.Fatalf("period catalog calls=%d students=%#v", catalog.calls, catalog.students)
	}
}

func TestServiceListsUndergraduatePeriodsForUnknownEducationLevel(t *testing.T) {
	student := StudentReference{
		UserID:         7,
		StudentNo:      "20260001",
		Provider:       "manual",
		EducationLevel: "unknown",
	}
	catalog := &stubPeriodCatalog{}
	service := NewService(
		stubIdentityResolver{student: student},
		nil,
		WithPeriodCatalog(catalog),
	)

	periods, err := service.ListPeriods(context.Background(), student.UserID)
	if err != nil || len(periods) != 1 {
		t.Fatalf("ListPeriods() = %#v, %v", periods, err)
	}
	if catalog.calls != 1 || len(catalog.students) != 1 {
		t.Fatalf("period catalog calls=%d students=%#v", catalog.calls, catalog.students)
	}
	want := student
	want.EducationLevel = domain.EducationLevelUndergraduate
	if catalog.students[0] != want {
		t.Fatalf("period catalog student=%#v, want %#v", catalog.students[0], want)
	}
}

func TestServiceListsUndergraduatePeriodsWithoutBoundIdentity(t *testing.T) {
	catalog := &stubPeriodCatalog{}
	service := NewService(
		stubIdentityResolver{err: apperror.New(
			http.StatusForbidden,
			"academic_verification_required",
			"完成教务认证后才能查询教务信息",
		)},
		nil,
		WithPeriodCatalog(catalog),
	)

	periods, err := service.ListPeriods(context.Background(), 7)
	if err != nil || len(periods) != 1 {
		t.Fatalf("ListPeriods() = %#v, %v", periods, err)
	}
	if len(catalog.students) != 1 {
		t.Fatalf("period catalog students = %#v", catalog.students)
	}
	student := catalog.students[0]
	if student.UserID != 7 || student.StudentNo != "" ||
		student.EducationLevel != domain.EducationLevelUndergraduate {
		t.Fatalf("period student = %#v", student)
	}
}

func TestServicePreservesPeriodIdentityInfrastructureError(t *testing.T) {
	want := errors.New("identity database unavailable")
	service := NewService(
		stubIdentityResolver{err: want},
		nil,
		WithPeriodCatalog(&stubPeriodCatalog{}),
	)

	_, err := service.ListPeriods(context.Background(), 7)
	if !errors.Is(err, want) {
		t.Fatalf("ListPeriods() error = %v, want %v", err, want)
	}
}

func assertApplicationCode(t *testing.T, err error, want string) {
	t.Helper()
	appError, ok := apperror.As(err)
	if !ok || appError.Code != want {
		t.Fatalf("error = %v, application error = %#v, want code %q", err, appError, want)
	}
}
