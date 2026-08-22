// Package rpc exposes aggregate-only Analytics queries to the platform.
package rpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/weouc-plus/campus-academic/internal/core/apperror"
	"github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/application"
	"github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/domain"
	academicanalytics "github.com/weouc-plus/campus-academic/pkg/academic/analytics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements the versioned Analytics service. It receives actor
// metadata from the already-authorized platform but owns task idempotency and
// publication persistence itself.
type Server struct {
	academicanalytics.UnimplementedAcademicAnalyticsServiceServer
	manager           *application.Manager
	minimumSampleSize int64
}

func NewServer(manager *application.Manager, minimumSampleSize int64) *Server {
	if minimumSampleSize < 1 {
		minimumSampleSize = 1
	}
	return &Server{manager: manager, minimumSampleSize: minimumSampleSize}
}

func (server *Server) Register(registrar grpc.ServiceRegistrar) {
	academicanalytics.RegisterAcademicAnalyticsServiceServer(registrar, server)
}

func (server *Server) ListCoursePassRates(
	ctx context.Context,
	request *academicanalytics.ListCoursePassRatesRequest,
) (*academicanalytics.ListCoursePassRatesResponse, error) {
	page, pageSize, err := pageInput(request.GetPage(), request.GetPageSize())
	if err != nil {
		return nil, err
	}
	metadata, rows, total, err := server.manager.ListCourses(ctx, domain.Search{
		Keyword: request.GetKeyword(), CourseCode: request.GetCourseCode(),
		TermCode: request.GetTermCode(), EducationLevel: request.GetEducationLevel(),
	}, server.minimumSampleSize, page, pageSize)
	if err != nil {
		return nil, rpcError(err)
	}
	items := make([]*academicanalytics.CoursePassRate, 0, len(rows))
	for _, row := range rows {
		items = append(items, coursePassRate(row))
	}
	return &academicanalytics.ListCoursePassRatesResponse{
		Metadata: publicationMetadata(metadata), Items: items, Total: total,
		Page: int32(page), PageSize: int32(pageSize),
	}, nil
}

func (server *Server) ListInstructorPassRates(
	ctx context.Context,
	request *academicanalytics.ListInstructorPassRatesRequest,
) (*academicanalytics.ListInstructorPassRatesResponse, error) {
	page, pageSize, err := pageInput(request.GetPage(), request.GetPageSize())
	if err != nil {
		return nil, err
	}
	metadata, rows, total, err := server.manager.ListInstructors(ctx, domain.Search{
		CourseCode: request.GetCourseCode(), TeacherName: request.GetTeacherName(),
		TermCode: request.GetTermCode(), EducationLevel: request.GetEducationLevel(),
	}, server.minimumSampleSize, page, pageSize)
	if err != nil {
		return nil, rpcError(err)
	}
	items := make([]*academicanalytics.InstructorPassRate, 0, len(rows))
	for _, row := range rows {
		items = append(items, instructorPassRate(row))
	}
	return &academicanalytics.ListInstructorPassRatesResponse{
		Metadata: publicationMetadata(metadata), Items: items, Total: total,
		Page: int32(page), PageSize: int32(pageSize),
	}, nil
}

func (server *Server) GetCoursePassRateTrend(
	ctx context.Context,
	request *academicanalytics.GetCoursePassRateTrendRequest,
) (*academicanalytics.GetCoursePassRateTrendResponse, error) {
	if strings.TrimSpace(request.GetCourseCode()) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	metadata, trend, err := server.manager.GetCourseTrend(ctx, request.GetCourseCode(), server.minimumSampleSize)
	if err != nil {
		return nil, rpcError(err)
	}
	return &academicanalytics.GetCoursePassRateTrendResponse{
		Metadata: publicationMetadata(metadata), Trend: passRateTrend(trend),
	}, nil
}

func (server *Server) GetInstructorPassRateTrend(
	ctx context.Context,
	request *academicanalytics.GetInstructorPassRateTrendRequest,
) (*academicanalytics.GetInstructorPassRateTrendResponse, error) {
	if strings.TrimSpace(request.GetCourseCode()) == "" || strings.TrimSpace(request.GetTeacherKey()) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_request")
	}
	metadata, trend, err := server.manager.GetInstructorTrend(ctx, request.GetCourseCode(), request.GetTeacherKey(), server.minimumSampleSize)
	if err != nil {
		return nil, rpcError(err)
	}
	return &academicanalytics.GetInstructorPassRateTrendResponse{
		Metadata: publicationMetadata(metadata), Trend: passRateTrend(trend),
	}, nil
}

func (server *Server) ListBatches(
	ctx context.Context,
	request *academicanalytics.ListBatchesRequest,
) (*academicanalytics.ListBatchesResponse, error) {
	page, pageSize, err := pageInput(request.GetPage(), request.GetPageSize())
	if err != nil {
		return nil, err
	}
	rows, total, err := server.manager.ListBatches(ctx, domain.Search{Status: request.GetStatus()}, page, pageSize)
	if err != nil {
		return nil, rpcError(err)
	}
	items := make([]*academicanalytics.AcademicStatisticsBatch, 0, len(rows))
	for index := range rows {
		items = append(items, batch(rows[index]))
	}
	return &academicanalytics.ListBatchesResponse{Items: items, Total: total, Page: int32(page), PageSize: int32(pageSize)}, nil
}

func (server *Server) TriggerManualRun(
	ctx context.Context,
	request *academicanalytics.TriggerManualRunRequest,
) (*academicanalytics.TriggerManualRunResponse, error) {
	receipt, err := server.manager.TriggerManual(ctx, application.ManualRunInput{
		ActorID: request.GetActorId(), Confirmation: request.GetConfirmation(),
		Note: request.GetNote(), IdempotencyKey: request.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &academicanalytics.TriggerManualRunResponse{TaskId: receipt.TaskID, QueuedAt: timestamppb.New(receipt.QueuedAt)}, nil
}

func pageInput(page, pageSize int32) (int, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 || page > 1000000 {
		return 0, 0, status.Error(codes.InvalidArgument, "invalid_pagination")
	}
	return int(page), int(pageSize), nil
}

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request_canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request_deadline_exceeded")
	}
	if errors.Is(err, application.ErrRunAlreadyLocked) {
		return status.Error(codes.ResourceExhausted, "academic_statistics_busy")
	}
	if errors.Is(err, domain.ErrEmptySnapshot) {
		return status.Error(codes.FailedPrecondition, "academic_statistics_snapshot_invalid")
	}
	if value, ok := apperror.As(err); ok {
		switch {
		case value.Status == http.StatusBadRequest:
			return status.Error(codes.InvalidArgument, value.Code)
		case value.Status == http.StatusNotFound:
			return status.Error(codes.NotFound, value.Code)
		case value.Status == http.StatusConflict:
			return status.Error(codes.FailedPrecondition, value.Code)
		case value.Status == http.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, value.Code)
		case value.Status >= 500:
			return status.Error(codes.Unavailable, value.Code)
		}
	}
	return status.Error(codes.Internal, "academic_statistics_internal_error")
}

func publicationMetadata(value domain.PublishedMetadata) *academicanalytics.PublicationMetadata {
	return &academicanalytics.PublicationMetadata{
		BatchId: value.BatchID, SourceCutoffAt: timestamppb.New(value.SourceCutoffAt),
		PublishedAt: timestamppb.New(value.PublishedAt), RuleVersion: value.RuleVersion,
		MinimumSampleSize: value.MinimumSampleSize,
	}
}

func distribution(value domain.Distribution) *academicanalytics.GradeDistribution {
	return &academicanalytics.GradeDistribution{
		NumericFailCount: value.NumericFail, Score_60_69Count: value.Score6069,
		Score_70_79Count: value.Score7079, Score_80_89Count: value.Score8089,
		Score_90_100Count: value.Score90100, LevelExcellentCount: value.LevelExcellent,
		LevelGoodCount: value.LevelGood, LevelMediumCount: value.LevelMedium,
		LevelPassCount: value.LevelPass, LevelFailCount: value.LevelFail,
	}
}

func coursePassRate(value domain.CoursePassRate) *academicanalytics.CoursePassRate {
	return &academicanalytics.CoursePassRate{
		EducationLevel: value.EducationLevel, CourseCode: value.CourseCode,
		CourseName: value.CourseName, TermCount: value.TermCount,
		ValidCount: value.ValidCount, PassCount: value.PassCount, FailCount: value.FailCount,
		NumericScoreCount: value.NumericScoreCount, NumericScoreSumX100: value.NumericScoreSumX100,
		Distribution: distribution(value.Distribution),
	}
}

func instructorPassRate(value domain.InstructorPassRate) *academicanalytics.InstructorPassRate {
	return &academicanalytics.InstructorPassRate{
		EducationLevel: value.EducationLevel, CourseCode: value.CourseCode,
		CourseName: value.CourseName, TeacherKey: value.TeacherKey, TeacherName: value.TeacherName,
		TermCount: value.TermCount, ClassCount: value.ClassCount, ValidCount: value.ValidCount,
		PassCount: value.PassCount, FailCount: value.FailCount,
		NumericScoreCount: value.NumericScoreCount, NumericScoreSumX100: value.NumericScoreSumX100,
		Distribution: distribution(value.Distribution),
	}
}

func passRateTrend(value domain.PassRateTrend) *academicanalytics.PassRateTrend {
	points := make([]*academicanalytics.PassRateTrendPoint, 0, len(value.Points))
	for _, point := range value.Points {
		points = append(points, &academicanalytics.PassRateTrendPoint{
			EducationLevel: point.EducationLevel, PeriodId: point.PeriodID,
			TermLabel: point.TermLabel, TermCode: point.TermCode, ValidCount: point.ValidCount,
			PassCount: point.PassCount, FailCount: point.FailCount,
			NumericScoreCount: point.NumericScoreCount, NumericScoreSumX100: point.NumericScoreSumX100,
			Distribution: distribution(point.Distribution),
		})
	}
	return &academicanalytics.PassRateTrend{CourseCode: value.CourseCode, CourseName: value.CourseName, TeacherKey: value.TeacherKey, TeacherName: value.TeacherName, Points: points}
}

func batch(value domain.AcademicStatisticsBatch) *academicanalytics.AcademicStatisticsBatch {
	return &academicanalytics.AcademicStatisticsBatch{
		Id: value.ID, Status: value.Status, TriggerType: value.TriggerType,
		TriggeredBy: value.TriggeredBy, TriggerNote: value.TriggerNote, TriggerTaskId: value.TriggerTaskId,
		SourceCutoffAt: timestamppb.New(value.SourceCutoffAt), RuleVersion: value.RuleVersion,
		SourceRowCount: value.SourceRowCount, CourseStatCount: value.CourseStatCount,
		InstructorStatCount: value.InstructorStatCount, ErrorSummary: value.ErrorSummary,
		StartedAt: timestamppb.New(value.StartedAt), FinishedAt: optionalTimestamp(value.FinishedAt),
		PublishedAt: optionalTimestamp(value.PublishedAt), CreatedAt: timestamppb.New(value.CreatedAt),
		UpdatedAt: timestamppb.New(value.UpdatedAt),
	}
}

func optionalTimestamp(value *time.Time) *timestamppb.Timestamp {
	if value == nil {
		return nil
	}
	return timestamppb.New(*value)
}
