package rpc

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	academicapp "github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
	academicpb "github.com/LDouble/campus-academic/pkg/academic/provider/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const retryAfterMetadataKey = "retry-after-ms"

const (
	academicRetryableReason = "ACADEMIC_RETRYABLE"
	academicErrorDomain     = "campus.academic"
)

// AcademicSessionRevoker removes every cached school session for one student.
type AcademicSessionRevoker interface {
	DeleteStudent(context.Context, string) error
}

// CourseCatalogProvider reads bounded pages from the school-wide catalog.
type CourseCatalogProvider interface {
	ListCourseCatalogPage(
		context.Context,
		academicapp.Credential,
		string,
		string,
		int,
	) (domain.CourseCatalogPage, error)
}

// Server exposes the private academic provider contract without business authorization.
// The public API remains responsible for user authentication and student ownership checks.
type Server struct {
	academicpb.UnimplementedAcademicProviderServiceServer
	queries  academicapp.Provider
	verify   verificationapp.Provider
	sessions AcademicSessionRevoker
	catalog  CourseCatalogProvider
	bulkhead *bulkhead
}

// NewServer creates a bounded academic provider RPC server.
func NewServer(
	queries academicapp.Provider,
	verify verificationapp.Provider,
	sessions AcademicSessionRevoker,
	maxConcurrent int,
	wait time.Duration,
	retryAfter time.Duration,
) *Server {
	return &Server{
		queries: queries, verify: verify, sessions: sessions,
		bulkhead: newBulkhead(maxConcurrent, wait, retryAfter),
	}
}

// WithCourseCatalog attaches the internal school-wide catalog boundary.
func (s *Server) WithCourseCatalog(provider CourseCatalogProvider) *Server {
	s.catalog = provider
	return s
}

// Register attaches the provider service to one gRPC registrar.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	academicpb.RegisterAcademicProviderServiceServer(registrar, s)
}

// VerifyCredential verifies one request-scoped school credential.
func (s *Server) VerifyCredential(
	ctx context.Context,
	request *academicpb.VerifyCredentialRequest,
) (*academicpb.VerifyCredentialResponse, error) {
	credential, err := credentialFromProto(request.GetCredential())
	if err != nil || strings.TrimSpace(request.GetEducationLevel()) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	return withPermit(ctx, s, func() (*academicpb.VerifyCredentialResponse, error) {
		result, verifyErr := s.verify.Verify(ctx, verificationapp.VerificationCommand{
			StudentNo: credential.StudentNo, Password: credential.Password,
			EducationLevel: request.GetEducationLevel(),
		})
		if verifyErr != nil {
			return nil, verifyErr
		}
		return &academicpb.VerifyCredentialResponse{
			RealName: result.RealName, Provider: result.Provider,
			EducationLevel: result.EducationLevel,
		}, nil
	})
}

// ListCourses returns normalized timetable records.
func (s *Server) ListCourses(
	ctx context.Context,
	request *academicpb.ListCoursesRequest,
) (*academicpb.ListCoursesResponse, error) {
	student, credential, err := queryInput(request.GetStudent(), request.GetCredential())
	if err != nil || strings.TrimSpace(request.GetPeriodId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	return withPermit(ctx, s, func() (*academicpb.ListCoursesResponse, error) {
		result, queryErr := listCoursesWithCache(ctx, s.queries, student, credential, request.GetPeriodId())
		if queryErr != nil {
			return nil, queryErr
		}
		rows := make([]*academicpb.Course, len(result.Records.Courses))
		for index := range result.Records.Courses {
			rows[index] = courseToProto(result.Records.Courses[index])
		}
		return &academicpb.ListCoursesResponse{
			Courses: rows, ScheduleNote: result.Records.ScheduleNote,
			Cache: cacheMetadataToProto(result.Cache),
		}, nil
	})
}

// ListGrades returns normalized grade records.
func (s *Server) ListGrades(
	ctx context.Context,
	request *academicpb.ListGradesRequest,
) (*academicpb.ListGradesResponse, error) {
	student, credential, err := queryInput(request.GetStudent(), request.GetCredential())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	return withPermit(ctx, s, func() (*academicpb.ListGradesResponse, error) {
		result, queryErr := listGradesWithCache(ctx, s.queries, student, credential, request.GetPeriodId())
		if queryErr != nil {
			return nil, queryErr
		}
		rows := make([]*academicpb.Grade, len(result.Records))
		for index := range result.Records {
			rows[index] = gradeToProto(result.Records[index])
		}
		return &academicpb.ListGradesResponse{Grades: rows, Cache: cacheMetadataToProto(result.Cache)}, nil
	})
}

// ListExams returns normalized examination arrangements.
func (s *Server) ListExams(
	ctx context.Context,
	request *academicpb.ListExamsRequest,
) (*academicpb.ListExamsResponse, error) {
	student, credential, err := queryInput(request.GetStudent(), request.GetCredential())
	if err != nil || strings.TrimSpace(request.GetPeriodId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	return withPermit(ctx, s, func() (*academicpb.ListExamsResponse, error) {
		result, queryErr := listExamsWithCache(ctx, s.queries, student, credential, request.GetPeriodId())
		if queryErr != nil {
			return nil, queryErr
		}
		rows := make([]*academicpb.Exam, len(result.Records))
		for index := range result.Records {
			rows[index] = examToProto(result.Records[index])
		}
		return &academicpb.ListExamsResponse{Exams: rows, Cache: cacheMetadataToProto(result.Cache)}, nil
	})
}

// ListCourseSelections returns normalized course-selection records.
func (s *Server) ListCourseSelections(
	ctx context.Context,
	request *academicpb.ListCourseSelectionsRequest,
) (*academicpb.ListCourseSelectionsResponse, error) {
	student, credential, err := queryInput(request.GetStudent(), request.GetCredential())
	if err != nil || strings.TrimSpace(request.GetPeriodId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	return withPermit(ctx, s, func() (*academicpb.ListCourseSelectionsResponse, error) {
		result, queryErr := listCourseSelectionsWithCache(
			ctx, s.queries, student, credential, request.GetPeriodId(),
		)
		if queryErr != nil {
			return nil, queryErr
		}
		rows := make([]*academicpb.CourseSelection, len(result.Records))
		for index := range result.Records {
			rows[index] = selectionToProto(result.Records[index])
		}
		return &academicpb.ListCourseSelectionsResponse{Selections: rows, Cache: cacheMetadataToProto(result.Cache)}, nil
	})
}

// ListCourseCatalogPage returns one bounded school-wide catalog page.
func (s *Server) ListCourseCatalogPage(
	ctx context.Context,
	request *academicpb.ListCourseCatalogPageRequest,
) (*academicpb.ListCourseCatalogPageResponse, error) {
	credential, err := credentialFromProto(request.GetCredential())
	educationLevel := strings.TrimSpace(request.GetEducationLevel())
	periodID := strings.TrimSpace(request.GetPeriodId())
	page := int(request.GetPage())
	if err != nil || educationLevel == "" || periodID == "" || page < 1 || page > 1000 {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	if s.catalog == nil {
		return nil, status.Error(codes.Unavailable, "academic_provider_unavailable")
	}
	return withPermit(ctx, s, func() (*academicpb.ListCourseCatalogPageResponse, error) {
		result, queryErr := s.catalog.ListCourseCatalogPage(
			ctx, credential, educationLevel, periodID, page,
		)
		if queryErr != nil {
			return nil, queryErr
		}
		rows := make([]*academicpb.CourseCatalogEntry, len(result.Entries))
		for index, row := range result.Entries {
			rows[index] = courseCatalogEntryToProto(row)
		}
		return &academicpb.ListCourseCatalogPageResponse{
			Entries: rows, Page: int32(result.Page), PageSize: int32(result.PageSize),
			TotalCount: int32(result.TotalCount), TotalPages: int32(result.TotalPages),
			HasMore: result.HasMore,
		}, nil
	})
}

// DeleteStudentSessions removes every short-lived school session for a student.
func (s *Server) DeleteStudentSessions(
	ctx context.Context,
	request *academicpb.DeleteStudentSessionsRequest,
) (*academicpb.DeleteStudentSessionsResponse, error) {
	studentNo := strings.TrimSpace(request.GetStudentNo())
	if studentNo == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	if s.sessions == nil {
		return nil, status.Error(codes.Unavailable, "academic_provider_unavailable")
	}
	if err := s.sessions.DeleteStudent(ctx, studentNo); err != nil {
		return nil, status.Error(codes.Unavailable, "academic_provider_unavailable")
	}
	return &academicpb.DeleteStudentSessionsResponse{}, nil
}

func withPermit[T any](ctx context.Context, s *Server, operation func() (T, error)) (T, error) {
	var zero T
	if s == nil || s.bulkhead == nil {
		return zero, status.Error(codes.Unavailable, "academic_provider_unavailable")
	}
	release, err := s.bulkhead.acquire(ctx)
	if err != nil {
		setRetryAfterTrailer(ctx, err)
		return zero, providerStatus(err)
	}
	defer release()
	value, err := operation()
	setRetryAfterTrailer(ctx, err)
	if err != nil {
		return zero, providerStatus(err)
	}
	return value, nil
}

func setRetryAfterTrailer(ctx context.Context, err error) {
	retryAfter, ok := academicapp.ProviderRetryAfter(err)
	if !ok {
		return
	}
	_ = grpc.SetTrailer(ctx, metadata.Pairs(
		retryAfterMetadataKey,
		strconv.FormatInt(retryAfter.Milliseconds(), 10),
	))
}

func credentialFromProto(value *academicpb.Credential) (academicapp.Credential, error) {
	if value == nil || strings.TrimSpace(value.GetStudentNo()) == "" || value.GetPassword() == "" {
		return academicapp.Credential{}, errors.New("invalid credential")
	}
	return academicapp.Credential{
		StudentNo: strings.TrimSpace(value.GetStudentNo()),
		Password:  value.GetPassword(),
	}, nil
}

func queryInput(
	studentValue *academicpb.StudentReference,
	credentialValue *academicpb.Credential,
) (academicapp.StudentReference, academicapp.Credential, error) {
	credential, err := credentialFromProto(credentialValue)
	if err != nil || studentValue == nil || studentValue.GetUserId() == 0 ||
		strings.TrimSpace(studentValue.GetStudentNo()) == "" ||
		strings.TrimSpace(studentValue.GetProvider()) == "" ||
		strings.TrimSpace(studentValue.GetEducationLevel()) == "" {
		return academicapp.StudentReference{}, academicapp.Credential{}, errors.New("invalid query input")
	}
	return academicapp.StudentReference{
		UserID: studentValue.GetUserId(), StudentNo: strings.TrimSpace(studentValue.GetStudentNo()),
		Provider:       strings.TrimSpace(studentValue.GetProvider()),
		EducationLevel: strings.TrimSpace(studentValue.GetEducationLevel()),
	}, credential, nil
}

func listCoursesWithCache(
	ctx context.Context,
	provider academicapp.Provider,
	student academicapp.StudentReference,
	credential academicapp.Credential,
	periodID string,
) (academicapp.QueryResult[domain.CourseSchedule], error) {
	if cacheAware, ok := provider.(academicapp.CacheAwareProvider); ok {
		return cacheAware.ListCoursesWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListCourses(ctx, student, credential, periodID)
	return academicapp.QueryResult[domain.CourseSchedule]{Records: rows}, err
}

func listGradesWithCache(
	ctx context.Context,
	provider academicapp.Provider,
	student academicapp.StudentReference,
	credential academicapp.Credential,
	periodID string,
) (academicapp.QueryResult[[]domain.Grade], error) {
	if cacheAware, ok := provider.(academicapp.CacheAwareProvider); ok {
		return cacheAware.ListGradesWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListGrades(ctx, student, credential, periodID)
	return academicapp.QueryResult[[]domain.Grade]{Records: rows}, err
}

func listExamsWithCache(
	ctx context.Context,
	provider academicapp.Provider,
	student academicapp.StudentReference,
	credential academicapp.Credential,
	periodID string,
) (academicapp.QueryResult[[]domain.Exam], error) {
	if cacheAware, ok := provider.(academicapp.CacheAwareProvider); ok {
		return cacheAware.ListExamsWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListExams(ctx, student, credential, periodID)
	return academicapp.QueryResult[[]domain.Exam]{Records: rows}, err
}

func listCourseSelectionsWithCache(
	ctx context.Context,
	provider academicapp.Provider,
	student academicapp.StudentReference,
	credential academicapp.Credential,
	periodID string,
) (academicapp.QueryResult[[]domain.CourseSelection], error) {
	if cacheAware, ok := provider.(academicapp.CacheAwareProvider); ok {
		return cacheAware.ListCourseSelectionsWithCache(ctx, student, credential, periodID)
	}
	rows, err := provider.ListCourseSelections(ctx, student, credential, periodID)
	return academicapp.QueryResult[[]domain.CourseSelection]{Records: rows}, err
}

func cacheMetadataToProto(value *academicapp.CacheMetadata) *academicpb.CacheMetadata {
	if value == nil || (value.State != academicapp.CacheStateFresh && value.State != academicapp.CacheStateStale) ||
		value.CachedAt.IsZero() || value.FreshUntil.IsZero() || value.CachedAt.After(value.FreshUntil) {
		return nil
	}
	return &academicpb.CacheMetadata{
		State: string(value.State), CachedAt: timestamppb.New(value.CachedAt),
		FreshUntil: timestamppb.New(value.FreshUntil),
	}
}

func providerStatus(err error) error {
	switch {
	case errors.Is(err, academicapp.ErrProviderRetryable), errors.Is(err, verificationapp.ErrProviderRetryable):
		return academicRetryableStatus()
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request_canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "academic_provider_timeout")
	case errors.Is(err, academicapp.ErrProviderBusy):
		return status.Error(codes.ResourceExhausted, "academic_provider_busy")
	case errors.Is(err, academicapp.ErrPeriodNotFound):
		return status.Error(codes.NotFound, "academic_period_not_found")
	case errors.Is(err, academicapp.ErrInvalidCredentials),
		errors.Is(err, verificationapp.ErrInvalidCredentials):
		return status.Error(codes.Unauthenticated, "invalid_academic_credentials")
	case errors.Is(err, academicapp.ErrPasswordExpired),
		errors.Is(err, verificationapp.ErrPasswordExpired):
		return status.Error(codes.FailedPrecondition, "academic_password_expired")
	case errors.Is(err, academicapp.ErrChallengeRequired),
		errors.Is(err, verificationapp.ErrChallengeRequired):
		return status.Error(codes.FailedPrecondition, "academic_challenge_required")
	case errors.Is(err, academicapp.ErrAccountRestricted),
		errors.Is(err, verificationapp.ErrAccountRestricted):
		return status.Error(codes.FailedPrecondition, "academic_account_restricted")
	case errors.Is(err, academicapp.ErrContractChanged):
		return status.Error(codes.FailedPrecondition, "academic_provider_contract_changed")
	case errors.Is(err, verificationapp.ErrIdentityTypeMismatch):
		return status.Error(codes.FailedPrecondition, "academic_identity_type_mismatch")
	case errors.Is(err, verificationapp.ErrIdentityTypeAmbiguous):
		return status.Error(codes.FailedPrecondition, "academic_identity_type_ambiguous")
	case errors.Is(err, verificationapp.ErrIdentityTypeUnresolved):
		return status.Error(codes.FailedPrecondition, "academic_identity_type_unresolved")
	default:
		return status.Error(codes.Unavailable, "academic_provider_unavailable")
	}
}

func academicRetryableStatus() error {
	statusValue := status.New(codes.Unavailable, "academic_retryable")
	statusWithDetails, err := statusValue.WithDetails(&errdetails.ErrorInfo{
		Reason: academicRetryableReason,
		Domain: academicErrorDomain,
	})
	if err != nil {
		return statusValue.Err()
	}
	return statusWithDetails.Err()
}

func courseToProto(row domain.Course) *academicpb.Course {
	weeks := make([]int32, len(row.Weeks))
	for index, week := range row.Weeks {
		weeks[index] = int32(week)
	}
	return &academicpb.Course{
		Id: row.ID, PeriodId: row.PeriodID, CourseCode: row.CourseCode,
		Name: row.Name, Teacher: row.Teacher, Campus: row.Campus, Location: row.Location,
		Note:    row.Note,
		Weekday: int32(row.Weekday), StartSection: int32(row.StartSection),
		EndSection: int32(row.EndSection), Weeks: weeks,
	}
}

func gradeToProto(row domain.Grade) *academicpb.Grade {
	result := &academicpb.Grade{
		Id: row.ID, PeriodId: row.PeriodID, CourseCode: row.CourseCode,
		CourseName: row.CourseName, CourseType: row.CourseType,
		Credit: row.Credit, GradeType: string(row.GradeType), Score: row.Score,
	}
	if row.GradeLevel != nil {
		value := string(*row.GradeLevel)
		result.GradeLevel = &value
	}
	return result
}

func examToProto(row domain.Exam) *academicpb.Exam {
	return &academicpb.Exam{
		Id: row.ID, PeriodId: row.PeriodID, CourseCode: row.CourseCode,
		CourseName: row.CourseName, StartAt: timestamppb.New(row.StartAt),
		EndAt: timestamppb.New(row.EndAt), Campus: row.Campus, Location: row.Location,
		Seat: row.Seat, Phase: string(row.Phase), Method: row.Method,
		Materials: row.Materials, Notice: row.Notice,
	}
}

func selectionToProto(row domain.CourseSelection) *academicpb.CourseSelection {
	result := &academicpb.CourseSelection{
		Id: row.ID, PeriodId: row.PeriodID, CourseCode: row.CourseCode,
		CourseName: row.CourseName, CourseType: row.CourseType, Credit: row.Credit,
		Teacher: row.Teacher, Campus: row.Campus, Location: row.Location,
		Schedule: row.Schedule, Capacity: int32(row.Capacity), Enrolled: int32(row.Enrolled),
		Status: string(row.Status), ResultText: row.ResultText, Note: row.Note,
	}
	if row.SelectedAt != nil {
		result.SelectedAt = timestamppb.New(*row.SelectedAt)
	}
	return result
}

func courseCatalogEntryToProto(row domain.CourseCatalogEntry) *academicpb.CourseCatalogEntry {
	return &academicpb.CourseCatalogEntry{
		SourceKey: row.SourceKey, PeriodId: row.PeriodID, OpeningCode: row.OpeningCode,
		CourseCode: row.CourseCode, CourseName: row.CourseName, CourseType: row.CourseType,
		Department: row.Department, Teachers: row.Teachers, Campus: row.Campus,
		Classes: row.Classes, Schedule: row.Schedule, Location: row.Location,
		Language: row.Language, Capacity: int32(row.Capacity), Enrolled: int32(row.Enrolled),
		Note: row.Note,
	}
}
