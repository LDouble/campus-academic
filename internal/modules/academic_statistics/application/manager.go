// Package application coordinates offline aggregation and published queries.
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/domain"
)

// ErrRunAlreadyLocked reports that another worker is already aggregating.
var ErrRunAlreadyLocked = errors.New("academic statistics aggregation is already running")

// Source computes an aggregate from the separately operated grade database. It
// must never return student-level records.
type Source interface {
	Aggregate(context.Context) (domain.Snapshot, error)
}

// RunLocker serializes the expensive daily aggregation across worker replicas.
type RunLocker interface {
	TryLock(context.Context) (release func() error, acquired bool, err error)
}

// Store persists immutable publication batches and serves their projections.
type Store interface {
	StartBatch(
		context.Context,
		domain.BatchTrigger,
		time.Time,
		time.Time,
	) (*domain.AcademicStatisticsBatch, error)
	FailBatch(context.Context, uint64, time.Time, string) error
	PublishBatch(
		context.Context,
		// The implementation refreshes this batch with the exact published
		// summary before returning success.
		*domain.AcademicStatisticsBatch,
		domain.Snapshot,
		time.Time,
	) error
	LatestPublished(context.Context) (*domain.AcademicStatisticsBatch, error)
	ListCourses(
		context.Context,
		domain.Search,
		int64,
		int,
		int,
	) (domain.PublishedMetadata, []domain.CoursePassRate, int64, error)
	ListInstructors(
		context.Context,
		domain.Search,
		int64,
		int,
		int,
	) (domain.PublishedMetadata, []domain.InstructorPassRate, int64, error)
	GetCourseTrend(
		context.Context,
		string,
		int64,
	) (domain.PublishedMetadata, domain.PassRateTrend, error)
	GetInstructorTrend(
		context.Context,
		string,
		string,
		int64,
	) (domain.PublishedMetadata, domain.PassRateTrend, error)
	ListBatches(
		context.Context,
		domain.Search,
		int,
		int,
	) ([]domain.AcademicStatisticsBatch, int64, error)
}

// Manager exposes query operations and, when configured with a source and
// locker, the daily publication workflow.
type Manager struct {
	store  Store
	source Source
	locker RunLocker
	queue  RunPublisher
	now    func() time.Time
}

// RunResult summarizes the exact batch published while the run lock was held.
type RunResult struct {
	BatchID             uint64
	SourceCutoffAt      time.Time
	SourceRowCount      int64
	CourseStatCount     int64
	InstructorStatCount int64
}

// NewManager creates an academic-statistics manager.
func NewManager(store Store) *Manager {
	return &Manager{
		store: store,
		now:   time.Now,
	}
}

// WithRunner attaches the external aggregate source and distributed lock.
func (manager *Manager) WithRunner(source Source, locker RunLocker) *Manager {
	manager.source = source
	manager.locker = locker
	return manager
}

// WithRunPublisher attaches the asynchronous manual-run queue.
func (manager *Manager) WithRunPublisher(publisher RunPublisher) *Manager {
	manager.queue = publisher
	return manager
}

// Run publishes one scheduled aggregate snapshot.
func (manager *Manager) Run(ctx context.Context) error {
	_, err := manager.run(ctx, domain.ScheduledBatchTrigger())
	return err
}

// RunManual publishes one administrator-requested aggregate snapshot.
func (manager *Manager) RunManual(
	ctx context.Context,
	actorID uint64,
	note,
	taskID string,
) error {
	_, err := manager.run(
		ctx,
		domain.ManualBatchTrigger(actorID, note, taskID),
	)
	return err
}

// RunOperational synchronously publishes one operator-requested batch and
// returns the summary captured under the same cross-process run lock.
func (manager *Manager) RunOperational(
	ctx context.Context,
	note,
	taskID string,
) (RunResult, error) {
	note = strings.TrimSpace(note)
	taskID = strings.TrimSpace(taskID)
	if note == "" || len([]rune(note)) > 200 {
		return RunResult{}, errors.New(
			"academic statistics operation note must contain 1 to 200 characters",
		)
	}
	if taskID == "" || len(taskID) > 96 {
		return RunResult{}, errors.New(
			"academic statistics operation task ID must contain 1 to 96 bytes",
		)
	}
	return manager.run(ctx, domain.OperationalBatchTrigger(note, taskID))
}

func (manager *Manager) run(
	ctx context.Context,
	trigger domain.BatchTrigger,
) (RunResult, error) {
	return manager.withRunLockResult(ctx, func() (RunResult, error) {
		return manager.runLocked(ctx, trigger)
	})
}

func (manager *Manager) withRunLockResult(
	ctx context.Context,
	run func() (RunResult, error),
) (RunResult, error) {
	if manager.source == nil || manager.locker == nil {
		return RunResult{}, errors.New(
			"academic statistics runner is not configured",
		)
	}
	release, acquired, err := manager.locker.TryLock(ctx)
	if err != nil {
		return RunResult{}, fmt.Errorf(
			"lock academic statistics aggregation: %w",
			err,
		)
	}
	if !acquired {
		return RunResult{}, ErrRunAlreadyLocked
	}
	result, runErr := run()
	releaseErr := release()
	if releaseErr != nil {
		releaseErr = fmt.Errorf(
			"release academic statistics aggregation lock: %w",
			releaseErr,
		)
	}
	return result, errors.Join(runErr, releaseErr)
}

func (manager *Manager) withRunLock(
	ctx context.Context,
	run func() error,
) error {
	_, err := manager.withRunLockResult(ctx, func() (RunResult, error) {
		return RunResult{}, run()
	})
	return err
}

// RunIfDue runs after today's configured wall-clock schedule unless a
// published batch already completed today. It supports worker restarts after
// the scheduled hour without publishing duplicate batches.
func (manager *Manager) RunIfDue(
	ctx context.Context,
	now time.Time,
	location *time.Location,
	scheduleHour int,
) (bool, error) {
	localNow := now.In(location)
	scheduledAt := time.Date(
		localNow.Year(),
		localNow.Month(),
		localNow.Day(),
		scheduleHour,
		0,
		0,
		0,
		location,
	)
	if localNow.Before(scheduledAt) {
		return false, nil
	}
	ran := false
	err := manager.withRunLock(ctx, func() error {
		latest, err := manager.store.LatestPublished(ctx)
		if err != nil {
			return fmt.Errorf(
				"load latest academic statistics batch: %w",
				err,
			)
		}
		if publishedAfterSchedule(latest, scheduledAt) {
			return nil
		}
		if _, err := manager.runLocked(
			ctx,
			domain.ScheduledBatchTrigger(),
		); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return ran, nil
}

func publishedAfterSchedule(
	batch *domain.AcademicStatisticsBatch,
	scheduledAt time.Time,
) bool {
	if batch == nil || batch.PublishedAt == nil {
		return false
	}
	return !batch.PublishedAt.Before(scheduledAt)
}

func (manager *Manager) runLocked(
	ctx context.Context,
	trigger domain.BatchTrigger,
) (RunResult, error) {
	startedAt := manager.now().UTC()
	batch, err := manager.store.StartBatch(
		ctx,
		trigger,
		startedAt,
		startedAt,
	)
	if err != nil {
		return RunResult{}, fmt.Errorf(
			"start academic statistics batch: %w",
			err,
		)
	}
	if batch.Status == domain.BatchStatusPublished {
		return resultFromBatch(batch), nil
	}
	snapshot, aggregateErr := manager.source.Aggregate(ctx)
	if aggregateErr != nil {
		failureErr := manager.failBatch(
			ctx,
			batch.ID,
			"source aggregation failed",
		)
		return RunResult{BatchID: batch.ID}, errors.Join(
			fmt.Errorf("aggregate source grades: %w", aggregateErr),
			failureErr,
		)
	}
	if err = snapshot.Validate(); err != nil {
		failureErr := manager.failBatch(
			ctx,
			batch.ID,
			"source aggregate validation failed",
		)
		return RunResult{BatchID: batch.ID}, errors.Join(
			fmt.Errorf("validate source aggregate: %w", err),
			failureErr,
		)
	}
	finishedAt := manager.now().UTC()
	if err = manager.store.PublishBatch(
		ctx,
		batch,
		snapshot,
		finishedAt,
	); err != nil {
		failureErr := manager.failBatch(
			ctx,
			batch.ID,
			"statistics publication failed",
		)
		return RunResult{BatchID: batch.ID}, errors.Join(
			fmt.Errorf("publish academic statistics batch: %w", err),
			failureErr,
		)
	}
	return resultFromBatch(batch), nil
}

func resultFromBatch(batch *domain.AcademicStatisticsBatch) RunResult {
	if batch == nil {
		return RunResult{}
	}
	return RunResult{
		BatchID:             batch.ID,
		SourceCutoffAt:      batch.SourceCutoffAt,
		SourceRowCount:      batch.SourceRowCount,
		CourseStatCount:     batch.CourseStatCount,
		InstructorStatCount: batch.InstructorStatCount,
	}
}

func (manager *Manager) failBatch(
	ctx context.Context,
	batchID uint64,
	summary string,
) error {
	failureCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		10*time.Second,
	)
	defer cancel()
	err := manager.store.FailBatch(
		failureCtx,
		batchID,
		manager.now().UTC(),
		summary,
	)
	if err != nil {
		return fmt.Errorf("record failed academic statistics batch: %w", err)
	}
	return nil
}

// ListCourses returns published course pass rates.
func (manager *Manager) ListCourses(
	ctx context.Context,
	search domain.Search,
	minimumSampleSize int64,
	page,
	pageSize int,
) (domain.PublishedMetadata, []domain.CoursePassRate, int64, error) {
	search.Keyword = strings.TrimSpace(search.Keyword)
	search.CourseCode = strings.TrimSpace(search.CourseCode)
	search.TermCode = strings.TrimSpace(search.TermCode)
	search.EducationLevel = strings.TrimSpace(search.EducationLevel)
	return manager.store.ListCourses(
		ctx,
		search,
		minimumSampleSize,
		page,
		pageSize,
	)
}

// ListInstructors returns published teacher-course pass rates.
func (manager *Manager) ListInstructors(
	ctx context.Context,
	search domain.Search,
	minimumSampleSize int64,
	page,
	pageSize int,
) (domain.PublishedMetadata, []domain.InstructorPassRate, int64, error) {
	search.CourseCode = strings.TrimSpace(search.CourseCode)
	search.TeacherName = strings.TrimSpace(search.TeacherName)
	search.TermCode = strings.TrimSpace(search.TermCode)
	search.EducationLevel = strings.TrimSpace(search.EducationLevel)
	return manager.store.ListInstructors(
		ctx,
		search,
		minimumSampleSize,
		page,
		pageSize,
	)
}

// GetCourseTrend returns sample-size-eligible per-term observations for one course.
func (manager *Manager) GetCourseTrend(
	ctx context.Context,
	courseCode string,
	minimumSampleSize int64,
) (domain.PublishedMetadata, domain.PassRateTrend, error) {
	return manager.store.GetCourseTrend(
		ctx,
		strings.TrimSpace(courseCode),
		minimumSampleSize,
	)
}

// GetInstructorTrend returns sample-size-eligible per-term observations for one
// teacher-course pair.
func (manager *Manager) GetInstructorTrend(
	ctx context.Context,
	courseCode,
	teacherKey string,
	minimumSampleSize int64,
) (domain.PublishedMetadata, domain.PassRateTrend, error) {
	return manager.store.GetInstructorTrend(
		ctx,
		strings.TrimSpace(courseCode),
		strings.TrimSpace(teacherKey),
		minimumSampleSize,
	)
}

// ListBatches returns operational publication history.
func (manager *Manager) ListBatches(
	ctx context.Context,
	search domain.Search,
	page,
	pageSize int,
) ([]domain.AcademicStatisticsBatch, int64, error) {
	search.Status = strings.TrimSpace(search.Status)
	return manager.store.ListBatches(ctx, search, page, pageSize)
}
