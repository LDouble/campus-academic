package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/weouc-plus/campus-academic/internal/core/apperror"
	platformquery "github.com/weouc-plus/campus-academic/internal/infrastructure/mysql/query"
	"github.com/weouc-plus/campus-academic/internal/modules/academic_statistics/domain"
	"gorm.io/gen/field"
	"gorm.io/gorm"
)

const (
	academicStatisticsLockName = "campus:academic-statistics:daily"
	abandonedBatchSummary      = "aggregation worker terminated before completion"
	failedBatchRetention       = 90 * 24 * time.Hour
)

// Store persists immutable aggregate publication batches.
type Store struct {
	db                *gorm.DB
	minimumSampleSize int64
}

// NewStore creates an academic-statistics store.
func NewStore(db *gorm.DB, minimumSampleSize int64) *Store {
	if minimumSampleSize < 1 {
		panic("academic statistics minimum sample size must be positive")
	}
	return &Store{
		db:                db,
		minimumSampleSize: minimumSampleSize,
	}
}

// StartBatch records an attempted publication before the source query starts.
func (store *Store) StartBatch(
	ctx context.Context,
	trigger domain.BatchTrigger,
	sourceCutoffAt,
	startedAt time.Time,
) (*domain.AcademicStatisticsBatch, error) {
	batch := &domain.AcademicStatisticsBatch{
		Status:              domain.BatchStatusRunning,
		TriggerType:         trigger.Type,
		TriggeredBy:         trigger.ActorID,
		TriggerNote:         trigger.Note,
		TriggerTaskId:       trigger.TaskID,
		SourceCutoffAt:      sourceCutoffAt,
		RuleVersion:         domain.RuleVersion,
		SourceRowCount:      0,
		CourseStatCount:     0,
		InstructorStatCount: 0,
		StartedAt:           startedAt,
	}
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := platformquery.Use(tx).AcademicStatisticsBatch
		if _, updateErr := q.WithContext(ctx).
			Where(q.Status.Eq(domain.BatchStatusRunning)).
			UpdateSimple(
				q.Status.Value(domain.BatchStatusFailed),
				q.ErrorSummary.Value(abandonedBatchSummary),
				q.FinishedAt.Value(startedAt),
			); updateErr != nil {
			return fmt.Errorf(
				"recover abandoned academic statistics batches: %w",
				updateErr,
			)
		}
		if pruneErr := pruneExpiredFailedBatches(
			ctx,
			tx,
			startedAt.Add(-failedBatchRetention),
		); pruneErr != nil {
			return pruneErr
		}
		if trigger.TaskID != nil {
			existing, findErr := q.WithContext(ctx).
				Where(q.TriggerTaskId.Eq(*trigger.TaskID)).
				First()
			if findErr == nil {
				if existing.Status == domain.BatchStatusPublished {
					batch = existing
					return nil
				}
				resetBatch(existing, batch)
				if saveErr := q.WithContext(ctx).Save(existing); saveErr != nil {
					return fmt.Errorf(
						"restart academic statistics batch: %w",
						saveErr,
					)
				}
				batch = existing
				return nil
			}
			if !errors.Is(findErr, gorm.ErrRecordNotFound) {
				return fmt.Errorf(
					"find academic statistics batch task: %w",
					findErr,
				)
			}
		}
		if createErr := q.WithContext(ctx).Create(batch); createErr != nil {
			return fmt.Errorf(
				"create academic statistics batch: %w",
				createErr,
			)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

func resetBatch(
	target,
	source *domain.AcademicStatisticsBatch,
) {
	target.Status = source.Status
	target.TriggerType = source.TriggerType
	target.TriggeredBy = source.TriggeredBy
	target.TriggerNote = source.TriggerNote
	target.SourceCutoffAt = source.SourceCutoffAt
	target.RuleVersion = source.RuleVersion
	target.SourceRowCount = 0
	target.CourseStatCount = 0
	target.InstructorStatCount = 0
	target.ErrorSummary = nil
	target.StartedAt = source.StartedAt
	target.FinishedAt = nil
	target.PublishedAt = nil
}

func pruneExpiredFailedBatches(
	ctx context.Context,
	db *gorm.DB,
	cutoff time.Time,
) error {
	batches := platformquery.Use(db).AcademicStatisticsBatch
	query := batches.WithContext(ctx).Where(
		batches.Status.Eq(domain.BatchStatusFailed),
		batches.CreatedAt.Lt(cutoff),
	)
	if _, err := query.Delete(); err != nil {
		return fmt.Errorf(
			"prune expired failed academic statistics batches: %w",
			err,
		)
	}
	return nil
}

// FailBatch marks an unpublished attempt as failed.
func (store *Store) FailBatch(
	ctx context.Context,
	batchID uint64,
	finishedAt time.Time,
	summary string,
) error {
	q := platformquery.Use(store.db).AcademicStatisticsBatch
	result, err := q.WithContext(ctx).
		Where(
			q.ID.Eq(batchID),
			q.Status.Eq(domain.BatchStatusRunning),
		).
		UpdateSimple(
			q.Status.Value(domain.BatchStatusFailed),
			q.ErrorSummary.Value(truncate(summary, 1000)),
			q.FinishedAt.Value(finishedAt),
		)
	if err != nil {
		return fmt.Errorf("fail academic statistics batch: %w", err)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("academic statistics batch %d is not running", batchID)
	}
	return nil
}

// PublishBatch atomically inserts all aggregate rows and makes the batch
// visible. Readers continue using the previous published batch until commit.
func (store *Store) PublishBatch(
	ctx context.Context,
	batch *domain.AcademicStatisticsBatch,
	snapshot domain.Snapshot,
	finishedAt time.Time,
) error {
	courses := courseEntities(batch.ID, snapshot.Courses)
	instructors := instructorEntities(batch.ID, snapshot.Instructors)
	effectiveCutoff := snapshot.SourceCutoffAt
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		carriedCutoff, err := store.suppressSmallPublicationDeltas(
			ctx,
			tx,
			courses,
			instructors,
		)
		if err != nil {
			return err
		}
		if carriedCutoff != nil && carriedCutoff.Before(effectiveCutoff) {
			effectiveCutoff = *carriedCutoff
		}
		q := platformquery.Use(tx)
		if err := q.AcademicCourseTermStatistic.WithContext(ctx).
			CreateInBatches(courses, 500); err != nil {
			return fmt.Errorf("insert academic course statistics: %w", err)
		}
		if len(instructors) > 0 {
			if err := q.AcademicInstructorCourseTermStatistic.WithContext(ctx).
				CreateInBatches(instructors, 500); err != nil {
				return fmt.Errorf("insert academic instructor statistics: %w", err)
			}
		}
		batches := q.AcademicStatisticsBatch
		result, err := batches.WithContext(ctx).
			Where(
				batches.ID.Eq(batch.ID),
				batches.Status.Eq(domain.BatchStatusRunning),
			).
			UpdateSimple(
				batches.Status.Value(domain.BatchStatusPublished),
				batches.SourceCutoffAt.Value(effectiveCutoff),
				batches.SourceRowCount.Value(snapshot.SourceRowCount),
				batches.CourseStatCount.Value(int64(len(courses))),
				batches.InstructorStatCount.Value(int64(len(instructors))),
				batches.ErrorSummary.Null(),
				batches.FinishedAt.Value(finishedAt),
				batches.PublishedAt.Value(finishedAt),
			)
		if err != nil {
			return fmt.Errorf("publish academic statistics batch: %w", err)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf(
				"academic statistics batch %d is not running",
				batch.ID,
			)
		}
		return nil
	})
	if err != nil {
		return err
	}
	batch.Status = domain.BatchStatusPublished
	batch.SourceCutoffAt = effectiveCutoff
	batch.SourceRowCount = snapshot.SourceRowCount
	batch.CourseStatCount = int64(len(courses))
	batch.InstructorStatCount = int64(len(instructors))
	batch.ErrorSummary = nil
	batch.FinishedAt = &finishedAt
	batch.PublishedAt = &finishedAt
	return nil
}

func (store *Store) suppressSmallPublicationDeltas(
	ctx context.Context,
	tx *gorm.DB,
	courses []*domain.AcademicCourseTermStatistic,
	instructors []*domain.AcademicInstructorCourseTermStatistic,
) (*time.Time, error) {
	previousCourses, err := latestPublishedCourseBaselines(ctx, tx)
	if err != nil {
		return nil, err
	}
	previousInstructors, err := latestPublishedInstructorBaselines(ctx, tx)
	if err != nil {
		return nil, err
	}
	suppressedBatchIDs := make(map[uint64]struct{})
	instructorSuppressed := suppressInstructorDeltas(
		instructors,
		previousInstructors,
		store.minimumSampleSize,
		suppressedBatchIDs,
	)
	suppressCourseDeltas(
		courses,
		previousCourses,
		instructorSuppressed,
		store.minimumSampleSize,
		suppressedBatchIDs,
	)
	if len(suppressedBatchIDs) == 0 {
		return nil, nil
	}
	return earliestPublishedCutoff(ctx, tx, suppressedBatchIDs)
}

const latestPublishedCourseBaselinesSQL = `
SELECT *
FROM (
    SELECT
        statistics.*,
        ROW_NUMBER() OVER (
            PARTITION BY
                statistics.education_level,
                statistics.period_id,
                statistics.course_code
            ORDER BY batches.published_at DESC, batches.id DESC
        ) AS privacy_rank
    FROM academic_course_term_statistics AS statistics
    INNER JOIN academic_statistics_batches AS batches
        ON batches.id = statistics.batch_id
    WHERE batches.status = ?
      AND batches.rule_version = ?
) AS ranked
WHERE privacy_rank = 1`

const latestPublishedInstructorBaselinesSQL = `
SELECT *
FROM (
    SELECT
        statistics.*,
        ROW_NUMBER() OVER (
            PARTITION BY
                statistics.education_level,
                statistics.period_id,
                statistics.course_code,
                statistics.teacher_key
            ORDER BY batches.published_at DESC, batches.id DESC
        ) AS privacy_rank
    FROM academic_instructor_course_term_statistics AS statistics
    INNER JOIN academic_statistics_batches AS batches
        ON batches.id = statistics.batch_id
    WHERE batches.status = ?
      AND batches.rule_version = ?
) AS ranked
WHERE privacy_rank = 1`

// These history lookups require a window over immutable publications to find
// the latest public value per privacy cohort. GORM Gen cannot express that
// partitioned ranking, so the exceptional SQL remains repository-local.
func latestPublishedCourseBaselines(
	ctx context.Context,
	db *gorm.DB,
) ([]*domain.AcademicCourseTermStatistic, error) {
	rows := []*domain.AcademicCourseTermStatistic{}
	if err := db.WithContext(ctx).
		Raw(
			latestPublishedCourseBaselinesSQL,
			domain.BatchStatusPublished,
			domain.RuleVersion,
		).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf(
			"get latest published academic course baselines: %w",
			err,
		)
	}
	return rows, nil
}

func latestPublishedInstructorBaselines(
	ctx context.Context,
	db *gorm.DB,
) ([]*domain.AcademicInstructorCourseTermStatistic, error) {
	rows := []*domain.AcademicInstructorCourseTermStatistic{}
	if err := db.WithContext(ctx).
		Raw(
			latestPublishedInstructorBaselinesSQL,
			domain.BatchStatusPublished,
			domain.RuleVersion,
		).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf(
			"get latest published academic instructor baselines: %w",
			err,
		)
	}
	return rows, nil
}

func earliestPublishedCutoff(
	ctx context.Context,
	db *gorm.DB,
	batchIDs map[uint64]struct{},
) (*time.Time, error) {
	ids := make([]uint64, 0, len(batchIDs))
	for batchID := range batchIDs {
		ids = append(ids, batchID)
	}
	q := platformquery.Use(db).AcademicStatisticsBatch
	batches, err := q.WithContext(ctx).
		Where(
			q.ID.In(ids...),
			q.Status.Eq(domain.BatchStatusPublished),
		).
		Find()
	if err != nil {
		return nil, fmt.Errorf(
			"get suppressed academic statistics baseline cutoffs: %w",
			err,
		)
	}
	if len(batches) != len(ids) {
		return nil, errors.New(
			"one or more academic statistics privacy baselines are unavailable",
		)
	}
	cutoff := batches[0].SourceCutoffAt
	for _, batch := range batches[1:] {
		if batch.SourceCutoffAt.Before(cutoff) {
			cutoff = batch.SourceCutoffAt
		}
	}
	return &cutoff, nil
}

type coursePartitionKey struct {
	educationLevel string
	periodID       string
	courseCode     string
}

func suppressCourseDeltas(
	current,
	previous []*domain.AcademicCourseTermStatistic,
	instructorSuppressed map[coursePartitionKey]struct{},
	minimumSampleSize int64,
	suppressedBatchIDs map[uint64]struct{},
) {
	previousByKey := make(
		map[coursePartitionKey]*domain.AcademicCourseTermStatistic,
		len(previous),
	)
	for _, row := range previous {
		previousByKey[coursePartitionKey{
			educationLevel: row.EducationLevel,
			periodID:       row.PeriodId,
			courseCode:     row.CourseCode,
		}] = row
	}
	for _, row := range current {
		baseline := previousByKey[coursePartitionKey{
			educationLevel: row.EducationLevel,
			periodID:       row.PeriodId,
			courseCode:     row.CourseCode,
		}]
		if baseline == nil || baseline.ValidCount < minimumSampleSize {
			continue
		}
		key := coursePartitionKey{
			educationLevel: row.EducationLevel,
			periodID:       row.PeriodId,
			courseCode:     row.CourseCode,
		}
		_, forceSuppression := instructorSuppressed[key]
		if !forceSuppression &&
			!unsafeCoursePublicationDelta(
				baseline,
				row,
				minimumSampleSize,
			) {
			continue
		}
		copyCourseStatistics(row, baseline)
		suppressedBatchIDs[baseline.BatchId] = struct{}{}
	}
}

type instructorPartitionKey struct {
	educationLevel string
	periodID       string
	courseCode     string
	teacherKey     string
}

func suppressInstructorDeltas(
	current,
	previous []*domain.AcademicInstructorCourseTermStatistic,
	minimumSampleSize int64,
	suppressedBatchIDs map[uint64]struct{},
) map[coursePartitionKey]struct{} {
	previousByKey := make(
		map[instructorPartitionKey]*domain.AcademicInstructorCourseTermStatistic,
		len(previous),
	)
	suppressed := make(map[coursePartitionKey]struct{})
	for _, row := range previous {
		previousByKey[instructorPartitionKey{
			educationLevel: row.EducationLevel,
			periodID:       row.PeriodId,
			courseCode:     row.CourseCode,
			teacherKey:     row.TeacherKey,
		}] = row
	}
	for _, row := range current {
		baseline := previousByKey[instructorPartitionKey{
			educationLevel: row.EducationLevel,
			periodID:       row.PeriodId,
			courseCode:     row.CourseCode,
			teacherKey:     row.TeacherKey,
		}]
		if baseline == nil ||
			baseline.ValidCount < minimumSampleSize ||
			!unsafeInstructorPublicationDelta(
				baseline,
				row,
				minimumSampleSize,
			) {
			continue
		}
		copyInstructorStatistics(row, baseline)
		suppressedBatchIDs[baseline.BatchId] = struct{}{}
		suppressed[coursePartitionKey{
			educationLevel: row.EducationLevel,
			periodID:       row.PeriodId,
			courseCode:     row.CourseCode,
		}] = struct{}{}
	}
	return suppressed
}

func unsafeCoursePublicationDelta(
	previous,
	current *domain.AcademicCourseTermStatistic,
	minimumSampleSize int64,
) bool {
	return unsafePublicationDelta(
		coursePublicationCounts(previous),
		coursePublicationCounts(current),
		previous.NumericScoreSumX100,
		current.NumericScoreSumX100,
		minimumSampleSize,
	)
}

func unsafeInstructorPublicationDelta(
	previous,
	current *domain.AcademicInstructorCourseTermStatistic,
	minimumSampleSize int64,
) bool {
	return unsafePublicationDelta(
		instructorPublicationCounts(previous),
		instructorPublicationCounts(current),
		previous.NumericScoreSumX100,
		current.NumericScoreSumX100,
		minimumSampleSize,
	)
}

const publicationCountFields = 15
const numericScoreCountIndex = 3

type publicationCounts [publicationCountFields]int64

func coursePublicationCounts(
	row *domain.AcademicCourseTermStatistic,
) publicationCounts {
	return publicationCounts{
		row.ValidCount,
		row.PassCount,
		row.FailCount,
		row.NumericScoreCount,
		row.ValidCount - row.NumericScoreCount,
		row.NumericFailCount,
		row.Score6069Count,
		row.Score7079Count,
		row.Score8089Count,
		row.Score90100Count,
		row.LevelExcellentCount,
		row.LevelGoodCount,
		row.LevelMediumCount,
		row.LevelPassCount,
		row.LevelFailCount,
	}
}

func instructorPublicationCounts(
	row *domain.AcademicInstructorCourseTermStatistic,
) publicationCounts {
	return publicationCounts{
		row.ValidCount,
		row.PassCount,
		row.FailCount,
		row.NumericScoreCount,
		row.ValidCount - row.NumericScoreCount,
		row.NumericFailCount,
		row.Score6069Count,
		row.Score7079Count,
		row.Score8089Count,
		row.Score90100Count,
		row.LevelExcellentCount,
		row.LevelGoodCount,
		row.LevelMediumCount,
		row.LevelPassCount,
		row.LevelFailCount,
	}
}

func unsafePublicationDelta(
	previous,
	current publicationCounts,
	previousScoreSum,
	currentScoreSum,
	minimumSampleSize int64,
) bool {
	for index := range previous {
		delta := absoluteDelta(previous[index], current[index])
		if delta > 0 && delta < minimumSampleSize {
			return true
		}
	}

	previousNumericCount := previous[numericScoreCountIndex]
	currentNumericCount := current[numericScoreCountIndex]
	if previousNumericCount < minimumSampleSize ||
		currentNumericCount < minimumSampleSize {
		return false
	}
	numericDelta := absoluteDelta(
		previousNumericCount,
		currentNumericCount,
	)
	return numericDelta < minimumSampleSize &&
		(numericDelta > 0 || previousScoreSum != currentScoreSum)
}

func absoluteDelta(previous, current int64) int64 {
	delta := previous - current
	if delta < 0 {
		return -delta
	}
	return delta
}

func copyCourseStatistics(
	target,
	source *domain.AcademicCourseTermStatistic,
) {
	target.ValidCount = source.ValidCount
	target.PassCount = source.PassCount
	target.FailCount = source.FailCount
	target.NumericScoreCount = source.NumericScoreCount
	target.NumericScoreSumX100 = source.NumericScoreSumX100
	target.NumericFailCount = source.NumericFailCount
	target.Score6069Count = source.Score6069Count
	target.Score7079Count = source.Score7079Count
	target.Score8089Count = source.Score8089Count
	target.Score90100Count = source.Score90100Count
	target.LevelExcellentCount = source.LevelExcellentCount
	target.LevelGoodCount = source.LevelGoodCount
	target.LevelMediumCount = source.LevelMediumCount
	target.LevelPassCount = source.LevelPassCount
	target.LevelFailCount = source.LevelFailCount
}

func copyInstructorStatistics(
	target,
	source *domain.AcademicInstructorCourseTermStatistic,
) {
	target.ClassCount = source.ClassCount
	target.ValidCount = source.ValidCount
	target.PassCount = source.PassCount
	target.FailCount = source.FailCount
	target.NumericScoreCount = source.NumericScoreCount
	target.NumericScoreSumX100 = source.NumericScoreSumX100
	target.NumericFailCount = source.NumericFailCount
	target.Score6069Count = source.Score6069Count
	target.Score7079Count = source.Score7079Count
	target.Score8089Count = source.Score8089Count
	target.Score90100Count = source.Score90100Count
	target.LevelExcellentCount = source.LevelExcellentCount
	target.LevelGoodCount = source.LevelGoodCount
	target.LevelMediumCount = source.LevelMediumCount
	target.LevelPassCount = source.LevelPassCount
	target.LevelFailCount = source.LevelFailCount
}

// LatestPublished returns the publication currently visible to readers.
func (store *Store) LatestPublished(
	ctx context.Context,
) (*domain.AcademicStatisticsBatch, error) {
	q := platformquery.Use(store.db).AcademicStatisticsBatch
	batch, err := q.WithContext(ctx).
		Where(
			q.Status.Eq(domain.BatchStatusPublished),
			q.RuleVersion.Eq(domain.RuleVersion),
		).
		Order(q.PublishedAt.Desc(), q.ID.Desc()).
		First()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest academic statistics batch: %w", err)
	}
	return batch, nil
}

// ListCourses returns course projections from the latest immutable batch.
func (store *Store) ListCourses(
	ctx context.Context,
	search domain.Search,
	minimumSampleSize int64,
	page,
	pageSize int,
) (
	domain.PublishedMetadata,
	[]domain.CoursePassRate,
	int64,
	error,
) {
	batch, metadata, err := store.publishedBatch(ctx)
	if err != nil {
		return domain.PublishedMetadata{}, nil, 0, err
	}
	filtered := store.filteredCourses(ctx, batch.ID, search)
	grouped := courseGroupedSQL(minimumSampleSize)
	groupedArgs := groupedQueryArgs(filtered)
	countSQL := "SELECT COUNT(*) FROM (" + grouped + ") AS published_courses"
	var total int64
	if err = store.db.WithContext(ctx).
		Raw(countSQL, groupedArgs...).
		Scan(&total).Error; err != nil {
		return domain.PublishedMetadata{}, nil, 0,
			fmt.Errorf("count academic course pass rates: %w", err)
	}
	rows := []courseProjection{}
	listSQL := grouped +
		" ORDER BY valid_count DESC, course_code ASC LIMIT ? OFFSET ?"
	listArgs := append(
		groupedQueryArgs(filtered),
		pageSize,
		(page-1)*pageSize,
	)
	if err = store.db.WithContext(ctx).
		Raw(listSQL, listArgs...).
		Scan(&rows).Error; err != nil {
		return domain.PublishedMetadata{}, nil, 0,
			fmt.Errorf("list academic course pass rates: %w", err)
	}
	values := make([]domain.CoursePassRate, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.domainValue())
	}
	return metadata, values, total, nil
}

// ListInstructors returns teacher-course projections from the latest batch.
func (store *Store) ListInstructors(
	ctx context.Context,
	search domain.Search,
	minimumSampleSize int64,
	page,
	pageSize int,
) (
	domain.PublishedMetadata,
	[]domain.InstructorPassRate,
	int64,
	error,
) {
	batch, metadata, err := store.publishedBatch(ctx)
	if err != nil {
		return domain.PublishedMetadata{}, nil, 0, err
	}
	filtered := store.filteredInstructors(ctx, batch.ID, search)
	grouped := instructorGroupedSQL(minimumSampleSize)
	groupedArgs := groupedQueryArgs(
		filtered,
	)
	countSQL := "SELECT COUNT(*) FROM (" + grouped + ") AS published_instructors"
	var total int64
	if err = store.db.WithContext(ctx).
		Raw(countSQL, groupedArgs...).
		Scan(&total).Error; err != nil {
		return domain.PublishedMetadata{}, nil, 0,
			fmt.Errorf("count academic instructor pass rates: %w", err)
	}
	rows := []instructorProjection{}
	listSQL := grouped +
		" ORDER BY valid_count DESC, teacher_name ASC LIMIT ? OFFSET ?"
	listArgs := append(
		groupedQueryArgs(
			filtered,
		),
		pageSize,
		(page-1)*pageSize,
	)
	if err = store.db.WithContext(ctx).
		Raw(listSQL, listArgs...).
		Scan(&rows).Error; err != nil {
		return domain.PublishedMetadata{}, nil, 0,
			fmt.Errorf("list academic instructor pass rates: %w", err)
	}
	values := make([]domain.InstructorPassRate, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.domainValue())
	}
	return metadata, values, total, nil
}

// GetCourseTrend returns one course's published per-term observations.
func (store *Store) GetCourseTrend(
	ctx context.Context,
	courseCode string,
	minimumSampleSize int64,
) (domain.PublishedMetadata, domain.PassRateTrend, error) {
	batch, metadata, err := store.publishedBatch(ctx)
	if err != nil {
		return domain.PublishedMetadata{}, domain.PassRateTrend{}, err
	}
	rows := []trendProjection{}
	filtered := store.filteredCourses(ctx, batch.ID, domain.Search{
		CourseCode: courseCode,
	})
	query := `SELECT * FROM (?) AS filtered
	WHERE valid_count >= ?
	ORDER BY term_code ASC, education_level ASC, period_id ASC`
	args := []any{filtered, minimumSampleSize}
	if err = store.db.WithContext(ctx).
		Raw(query, args...).
		Scan(&rows).Error; err != nil {
		return domain.PublishedMetadata{}, domain.PassRateTrend{},
			fmt.Errorf("get academic course pass-rate trend: %w", err)
	}
	if len(rows) == 0 {
		return domain.PublishedMetadata{}, domain.PassRateTrend{},
			academicStatisticsTrendNotFound()
	}
	return metadata, trendValue(courseCode, "", rows), nil
}

// GetInstructorTrend returns one teacher-course pair's published per-term
// observations.
func (store *Store) GetInstructorTrend(
	ctx context.Context,
	courseCode,
	teacherKey string,
	minimumSampleSize int64,
) (domain.PublishedMetadata, domain.PassRateTrend, error) {
	batch, metadata, err := store.publishedBatch(ctx)
	if err != nil {
		return domain.PublishedMetadata{}, domain.PassRateTrend{}, err
	}
	rows := []trendProjection{}
	q := platformquery.Use(store.db).AcademicInstructorCourseTermStatistic
	if err = q.WithContext(ctx).
		Where(
			q.BatchId.Eq(batch.ID),
			q.CourseCode.Eq(courseCode),
			q.TeacherKey.Eq(teacherKey),
			q.ValidCount.Gte(minimumSampleSize),
		).
		Order(q.TermCode.Asc(), q.EducationLevel.Asc(), q.PeriodId.Asc()).
		Scan(&rows); err != nil {
		return domain.PublishedMetadata{}, domain.PassRateTrend{},
			fmt.Errorf("get academic instructor pass-rate trend: %w", err)
	}
	if len(rows) == 0 {
		return domain.PublishedMetadata{}, domain.PassRateTrend{},
			academicStatisticsTrendNotFound()
	}
	return metadata, trendValue(courseCode, teacherKey, rows), nil
}

// ListBatches returns operational batch history.
func (store *Store) ListBatches(
	ctx context.Context,
	search domain.Search,
	page,
	pageSize int,
) ([]domain.AcademicStatisticsBatch, int64, error) {
	q := platformquery.Use(store.db).AcademicStatisticsBatch
	query := q.WithContext(ctx)
	if search.Status != "" {
		query = query.Where(q.Status.Eq(search.Status))
	}
	total, err := query.Count()
	if err != nil {
		return nil, 0, fmt.Errorf("count academic statistics batches: %w", err)
	}
	rows, err := query.Order(q.ID.Desc()).
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find()
	if err != nil {
		return nil, 0, fmt.Errorf("list academic statistics batches: %w", err)
	}
	values := make([]domain.AcademicStatisticsBatch, 0, len(rows))
	for _, row := range rows {
		values = append(values, *row)
	}
	return values, total, nil
}

func (store *Store) publishedBatch(
	ctx context.Context,
) (
	*domain.AcademicStatisticsBatch,
	domain.PublishedMetadata,
	error,
) {
	batch, err := store.LatestPublished(ctx)
	if err != nil {
		return nil, domain.PublishedMetadata{}, err
	}
	if batch == nil || batch.PublishedAt == nil {
		return nil, domain.PublishedMetadata{}, apperror.New(
			http.StatusNotFound,
			"academic_statistics_unavailable",
			"课程通过率数据尚未生成",
		)
	}
	return batch, domain.PublishedMetadata{
		BatchID:           batch.ID,
		SourceCutoffAt:    batch.SourceCutoffAt,
		PublishedAt:       *batch.PublishedAt,
		RuleVersion:       batch.RuleVersion,
		MinimumSampleSize: store.minimumSampleSize,
	}, nil
}

func academicStatisticsTrendNotFound() error {
	return apperror.New(
		http.StatusNotFound,
		"academic_statistics_not_found",
		"未找到可公开的课程通过率趋势数据",
	)
}

func courseEntities(
	batchID uint64,
	values []domain.CourseTermAggregate,
) []*domain.AcademicCourseTermStatistic {
	rows := make([]*domain.AcademicCourseTermStatistic, 0, len(values))
	for _, value := range values {
		rows = append(rows, &domain.AcademicCourseTermStatistic{
			BatchId:             batchID,
			EducationLevel:      value.EducationLevel,
			PeriodId:            value.PeriodID,
			TermLabel:           value.TermLabel,
			TermCode:            value.TermCode,
			CourseCode:          value.CourseCode,
			CourseName:          value.CourseName,
			ValidCount:          value.ValidCount,
			PassCount:           value.PassCount,
			FailCount:           value.FailCount,
			NumericScoreCount:   value.NumericScoreCount,
			NumericScoreSumX100: value.NumericScoreSumX100,
			NumericFailCount:    value.Distribution.NumericFail,
			Score6069Count:      value.Distribution.Score6069,
			Score7079Count:      value.Distribution.Score7079,
			Score8089Count:      value.Distribution.Score8089,
			Score90100Count:     value.Distribution.Score90100,
			LevelExcellentCount: value.Distribution.LevelExcellent,
			LevelGoodCount:      value.Distribution.LevelGood,
			LevelMediumCount:    value.Distribution.LevelMedium,
			LevelPassCount:      value.Distribution.LevelPass,
			LevelFailCount:      value.Distribution.LevelFail,
		})
	}
	return rows
}

func instructorEntities(
	batchID uint64,
	values []domain.InstructorCourseTermAggregate,
) []*domain.AcademicInstructorCourseTermStatistic {
	rows := make(
		[]*domain.AcademicInstructorCourseTermStatistic,
		0,
		len(values),
	)
	for _, value := range values {
		rows = append(rows, &domain.AcademicInstructorCourseTermStatistic{
			BatchId:             batchID,
			EducationLevel:      value.EducationLevel,
			PeriodId:            value.PeriodID,
			TermLabel:           value.TermLabel,
			TermCode:            value.TermCode,
			CourseCode:          value.CourseCode,
			CourseName:          value.CourseName,
			TeacherKey:          value.TeacherKey,
			TeacherName:         value.TeacherName,
			ClassCount:          value.ClassCount,
			ValidCount:          value.ValidCount,
			PassCount:           value.PassCount,
			FailCount:           value.FailCount,
			NumericScoreCount:   value.NumericScoreCount,
			NumericScoreSumX100: value.NumericScoreSumX100,
			NumericFailCount:    value.Distribution.NumericFail,
			Score6069Count:      value.Distribution.Score6069,
			Score7079Count:      value.Distribution.Score7079,
			Score8089Count:      value.Distribution.Score8089,
			Score90100Count:     value.Distribution.Score90100,
			LevelExcellentCount: value.Distribution.LevelExcellent,
			LevelGoodCount:      value.Distribution.LevelGood,
			LevelMediumCount:    value.Distribution.LevelMedium,
			LevelPassCount:      value.Distribution.LevelPass,
			LevelFailCount:      value.Distribution.LevelFail,
		})
	}
	return rows
}

func (store *Store) filteredCourses(
	ctx context.Context,
	batchID uint64,
	search domain.Search,
) *gorm.DB {
	q := platformquery.Use(store.db).AcademicCourseTermStatistic
	dao := q.WithContext(ctx).Where(q.BatchId.Eq(batchID))
	if search.CourseCode != "" {
		dao = dao.Where(q.CourseCode.Eq(search.CourseCode))
	}
	if search.TermCode != "" {
		dao = dao.Where(q.TermCode.Eq(search.TermCode))
	}
	if search.EducationLevel != "" {
		dao = dao.Where(q.EducationLevel.Eq(search.EducationLevel))
	}
	if search.Keyword != "" {
		pattern := "%" + search.Keyword + "%"
		dao = dao.Where(
			field.Or(
				q.CourseCode.Like(pattern),
				q.CourseName.Like(pattern),
			),
		)
	}
	return dao.UnderlyingDB()
}

func (store *Store) filteredInstructors(
	ctx context.Context,
	batchID uint64,
	search domain.Search,
) *gorm.DB {
	q := platformquery.Use(store.db).AcademicInstructorCourseTermStatistic
	dao := q.WithContext(ctx).Where(
		q.BatchId.Eq(batchID),
		q.CourseCode.Eq(search.CourseCode),
	)
	if search.TermCode != "" {
		dao = dao.Where(q.TermCode.Eq(search.TermCode))
	}
	if search.EducationLevel != "" {
		dao = dao.Where(q.EducationLevel.Eq(search.EducationLevel))
	}
	if search.TeacherName != "" {
		dao = dao.Where(q.TeacherName.Like("%" + search.TeacherName + "%"))
	}
	return dao.UnderlyingDB()
}

// GORM Gen applies every ordinary filter in filteredCourses. Raw SQL is
// restricted to the grouped projection because the generated API cannot map
// aggregate aliases into the projection or filter by those aliases.
func courseGroupedSQL(
	minimumSampleSize int64,
) string {
	return `
SELECT
    education_level,
    course_code,
    MAX(course_name) AS course_name,
    COUNT(*) AS term_count,
    SUM(valid_count) AS valid_count,
    SUM(pass_count) AS pass_count,
    SUM(fail_count) AS fail_count,
    SUM(numeric_score_count) AS numeric_score_count,
    SUM(numeric_score_sum_x100) AS numeric_score_sum_x100,
    SUM(numeric_fail_count) AS numeric_fail_count,
    SUM(score_60_69_count) AS score_60_69_count,
    SUM(score_70_79_count) AS score_70_79_count,
    SUM(score_80_89_count) AS score_80_89_count,
    SUM(score_90_100_count) AS score_90_100_count,
    SUM(level_excellent_count) AS level_excellent_count,
    SUM(level_good_count) AS level_good_count,
    SUM(level_medium_count) AS level_medium_count,
    SUM(level_pass_count) AS level_pass_count,
	SUM(level_fail_count) AS level_fail_count
FROM (?) AS filtered
GROUP BY education_level, course_code
HAVING SUM(valid_count) >= ` + fmt.Sprintf("%d", minimumSampleSize)
}

// GORM Gen applies every ordinary filter in filteredInstructors. Raw SQL is
// restricted to this grouped SUM/MAX projection for the same reason as the
// course query above.
func instructorGroupedSQL(
	minimumSampleSize int64,
) string {
	return `
SELECT
    education_level,
    course_code,
    MAX(course_name) AS course_name,
    teacher_key,
    teacher_name,
    COUNT(*) AS term_count,
    SUM(class_count) AS class_count,
    SUM(valid_count) AS valid_count,
    SUM(pass_count) AS pass_count,
    SUM(fail_count) AS fail_count,
    SUM(numeric_score_count) AS numeric_score_count,
    SUM(numeric_score_sum_x100) AS numeric_score_sum_x100,
    SUM(numeric_fail_count) AS numeric_fail_count,
    SUM(score_60_69_count) AS score_60_69_count,
    SUM(score_70_79_count) AS score_70_79_count,
    SUM(score_80_89_count) AS score_80_89_count,
    SUM(score_90_100_count) AS score_90_100_count,
    SUM(level_excellent_count) AS level_excellent_count,
    SUM(level_good_count) AS level_good_count,
    SUM(level_medium_count) AS level_medium_count,
    SUM(level_pass_count) AS level_pass_count,
    SUM(level_fail_count) AS level_fail_count
FROM (?) AS filtered
GROUP BY education_level, course_code, teacher_key, teacher_name
HAVING SUM(valid_count) >= ` + fmt.Sprintf("%d", minimumSampleSize)
}

func groupedQueryArgs(
	filtered *gorm.DB,
) []any {
	return []any{filtered}
}

type courseProjection struct {
	EducationLevel      string `gorm:"column:education_level"`
	CourseCode          string `gorm:"column:course_code"`
	CourseName          string `gorm:"column:course_name"`
	TermCount           int64  `gorm:"column:term_count"`
	ValidCount          int64  `gorm:"column:valid_count"`
	PassCount           int64  `gorm:"column:pass_count"`
	FailCount           int64  `gorm:"column:fail_count"`
	NumericScoreCount   int64  `gorm:"column:numeric_score_count"`
	NumericScoreSumX100 int64  `gorm:"column:numeric_score_sum_x100"`
	NumericFailCount    int64  `gorm:"column:numeric_fail_count"`
	Score6069Count      int64  `gorm:"column:score_60_69_count"`
	Score7079Count      int64  `gorm:"column:score_70_79_count"`
	Score8089Count      int64  `gorm:"column:score_80_89_count"`
	Score90100Count     int64  `gorm:"column:score_90_100_count"`
	LevelExcellentCount int64  `gorm:"column:level_excellent_count"`
	LevelGoodCount      int64  `gorm:"column:level_good_count"`
	LevelMediumCount    int64  `gorm:"column:level_medium_count"`
	LevelPassCount      int64  `gorm:"column:level_pass_count"`
	LevelFailCount      int64  `gorm:"column:level_fail_count"`
}

func (row courseProjection) domainValue() domain.CoursePassRate {
	return domain.CoursePassRate{
		EducationLevel:      row.EducationLevel,
		CourseCode:          row.CourseCode,
		CourseName:          row.CourseName,
		TermCount:           row.TermCount,
		ValidCount:          row.ValidCount,
		PassCount:           row.PassCount,
		FailCount:           row.FailCount,
		NumericScoreCount:   row.NumericScoreCount,
		NumericScoreSumX100: row.NumericScoreSumX100,
		Distribution:        row.distribution(),
	}
}

func (row courseProjection) distribution() domain.Distribution {
	return domain.Distribution{
		NumericFail:    row.NumericFailCount,
		Score6069:      row.Score6069Count,
		Score7079:      row.Score7079Count,
		Score8089:      row.Score8089Count,
		Score90100:     row.Score90100Count,
		LevelExcellent: row.LevelExcellentCount,
		LevelGood:      row.LevelGoodCount,
		LevelMedium:    row.LevelMediumCount,
		LevelPass:      row.LevelPassCount,
		LevelFail:      row.LevelFailCount,
	}
}

type instructorProjection struct {
	EducationLevel      string `gorm:"column:education_level"`
	CourseCode          string `gorm:"column:course_code"`
	CourseName          string `gorm:"column:course_name"`
	TeacherKey          string `gorm:"column:teacher_key"`
	TeacherName         string `gorm:"column:teacher_name"`
	TermCount           int64  `gorm:"column:term_count"`
	ClassCount          int64  `gorm:"column:class_count"`
	ValidCount          int64  `gorm:"column:valid_count"`
	PassCount           int64  `gorm:"column:pass_count"`
	FailCount           int64  `gorm:"column:fail_count"`
	NumericScoreCount   int64  `gorm:"column:numeric_score_count"`
	NumericScoreSumX100 int64  `gorm:"column:numeric_score_sum_x100"`
	NumericFailCount    int64  `gorm:"column:numeric_fail_count"`
	Score6069Count      int64  `gorm:"column:score_60_69_count"`
	Score7079Count      int64  `gorm:"column:score_70_79_count"`
	Score8089Count      int64  `gorm:"column:score_80_89_count"`
	Score90100Count     int64  `gorm:"column:score_90_100_count"`
	LevelExcellentCount int64  `gorm:"column:level_excellent_count"`
	LevelGoodCount      int64  `gorm:"column:level_good_count"`
	LevelMediumCount    int64  `gorm:"column:level_medium_count"`
	LevelPassCount      int64  `gorm:"column:level_pass_count"`
	LevelFailCount      int64  `gorm:"column:level_fail_count"`
}

func (row instructorProjection) domainValue() domain.InstructorPassRate {
	aggregate := row.aggregate()
	return domain.InstructorPassRate{
		EducationLevel:      aggregate.EducationLevel,
		CourseCode:          aggregate.CourseCode,
		CourseName:          aggregate.CourseName,
		TeacherKey:          row.TeacherKey,
		TeacherName:         row.TeacherName,
		TermCount:           aggregate.TermCount,
		ClassCount:          row.ClassCount,
		ValidCount:          aggregate.ValidCount,
		PassCount:           aggregate.PassCount,
		FailCount:           aggregate.FailCount,
		NumericScoreCount:   aggregate.NumericScoreCount,
		NumericScoreSumX100: aggregate.NumericScoreSumX100,
		Distribution:        aggregate.distribution(),
	}
}

func (row instructorProjection) aggregate() courseProjection {
	return courseProjection{
		EducationLevel:      row.EducationLevel,
		CourseCode:          row.CourseCode,
		CourseName:          row.CourseName,
		TermCount:           row.TermCount,
		ValidCount:          row.ValidCount,
		PassCount:           row.PassCount,
		FailCount:           row.FailCount,
		NumericScoreCount:   row.NumericScoreCount,
		NumericScoreSumX100: row.NumericScoreSumX100,
		NumericFailCount:    row.NumericFailCount,
		Score6069Count:      row.Score6069Count,
		Score7079Count:      row.Score7079Count,
		Score8089Count:      row.Score8089Count,
		Score90100Count:     row.Score90100Count,
		LevelExcellentCount: row.LevelExcellentCount,
		LevelGoodCount:      row.LevelGoodCount,
		LevelMediumCount:    row.LevelMediumCount,
		LevelPassCount:      row.LevelPassCount,
		LevelFailCount:      row.LevelFailCount,
	}
}

type trendProjection struct {
	EducationLevel      string `gorm:"column:education_level"`
	PeriodID            string `gorm:"column:period_id"`
	TermLabel           string `gorm:"column:term_label"`
	TermCode            string `gorm:"column:term_code"`
	CourseCode          string `gorm:"column:course_code"`
	CourseName          string `gorm:"column:course_name"`
	TeacherKey          string `gorm:"column:teacher_key"`
	TeacherName         string `gorm:"column:teacher_name"`
	ValidCount          int64  `gorm:"column:valid_count"`
	PassCount           int64  `gorm:"column:pass_count"`
	FailCount           int64  `gorm:"column:fail_count"`
	NumericScoreCount   int64  `gorm:"column:numeric_score_count"`
	NumericScoreSumX100 int64  `gorm:"column:numeric_score_sum_x100"`
	NumericFailCount    int64  `gorm:"column:numeric_fail_count"`
	Score6069Count      int64  `gorm:"column:score_60_69_count"`
	Score7079Count      int64  `gorm:"column:score_70_79_count"`
	Score8089Count      int64  `gorm:"column:score_80_89_count"`
	Score90100Count     int64  `gorm:"column:score_90_100_count"`
	LevelExcellentCount int64  `gorm:"column:level_excellent_count"`
	LevelGoodCount      int64  `gorm:"column:level_good_count"`
	LevelMediumCount    int64  `gorm:"column:level_medium_count"`
	LevelPassCount      int64  `gorm:"column:level_pass_count"`
	LevelFailCount      int64  `gorm:"column:level_fail_count"`
}

func (row trendProjection) aggregate() courseProjection {
	return courseProjection{
		CourseCode:          row.CourseCode,
		CourseName:          row.CourseName,
		ValidCount:          row.ValidCount,
		PassCount:           row.PassCount,
		FailCount:           row.FailCount,
		NumericScoreCount:   row.NumericScoreCount,
		NumericScoreSumX100: row.NumericScoreSumX100,
		NumericFailCount:    row.NumericFailCount,
		Score6069Count:      row.Score6069Count,
		Score7079Count:      row.Score7079Count,
		Score8089Count:      row.Score8089Count,
		Score90100Count:     row.Score90100Count,
		LevelExcellentCount: row.LevelExcellentCount,
		LevelGoodCount:      row.LevelGoodCount,
		LevelMediumCount:    row.LevelMediumCount,
		LevelPassCount:      row.LevelPassCount,
		LevelFailCount:      row.LevelFailCount,
	}
}

func trendValue(
	courseCode,
	teacherKey string,
	rows []trendProjection,
) domain.PassRateTrend {
	value := domain.PassRateTrend{
		CourseCode: courseCode,
		TeacherKey: teacherKey,
		Points:     make([]domain.PassRateTrendPoint, 0, len(rows)),
	}
	for _, row := range rows {
		aggregate := row.aggregate()
		if value.CourseName == "" {
			value.CourseName = aggregate.CourseName
			value.TeacherName = row.TeacherName
		}
		value.Points = append(value.Points, domain.PassRateTrendPoint{
			EducationLevel:      row.EducationLevel,
			PeriodID:            row.PeriodID,
			TermLabel:           row.TermLabel,
			TermCode:            row.TermCode,
			ValidCount:          aggregate.ValidCount,
			PassCount:           aggregate.PassCount,
			FailCount:           aggregate.FailCount,
			NumericScoreCount:   aggregate.NumericScoreCount,
			NumericScoreSumX100: aggregate.NumericScoreSumX100,
			Distribution:        aggregate.distribution(),
		})
	}
	return value
}

// MySQLRunLocker holds a named advisory lock on one dedicated destination
// connection for the duration of an offline aggregation.
type MySQLRunLocker struct {
	db *sql.DB
}

// NewMySQLRunLocker creates the cross-process aggregation lock.
func NewMySQLRunLocker(db *gorm.DB) (*MySQLRunLocker, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get academic statistics lock database: %w", err)
	}
	return &MySQLRunLocker{db: sqlDB}, nil
}

// TryLock attempts to acquire the daily aggregation lock without waiting.
func (locker *MySQLRunLocker) TryLock(
	ctx context.Context,
) (func() error, bool, error) {
	connection, err := locker.db.Conn(ctx)
	if err != nil {
		return func() error { return nil }, false,
			fmt.Errorf("open lock connection: %w", err)
	}
	var acquired sql.NullInt64
	if err = connection.QueryRowContext(
		ctx,
		"SELECT GET_LOCK(?, 0)",
		academicStatisticsLockName,
	).Scan(&acquired); err != nil {
		discardSQLConnection(connection)
		return func() error { return nil }, false,
			fmt.Errorf("acquire named lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		closeErr := connection.Close()
		return func() error { return nil }, false, closeErr
	}
	once := sync.Once{}
	var releaseErr error
	release := func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(
				context.Background(),
				5*time.Second,
			)
			defer cancel()
			var released sql.NullInt64
			queryErr := connection.QueryRowContext(
				releaseCtx,
				"SELECT RELEASE_LOCK(?)",
				academicStatisticsLockName,
			).Scan(&released)
			if queryErr != nil {
				discardSQLConnection(connection)
				releaseErr = fmt.Errorf("release named lock: %w", queryErr)
				return
			}
			if !released.Valid || released.Int64 != 1 {
				releaseErr = fmt.Errorf(
					"release named lock returned %v",
					released,
				)
				// A non-success response means the connection's advisory-lock
				// state cannot be trusted. Do not return it to the pool.
				discardSQLConnection(connection)
				return
			}
			if closeErr := connection.Close(); closeErr != nil {
				releaseErr = errors.Join(releaseErr, closeErr)
			}
		})
		return releaseErr
	}
	return release, true, nil
}

func discardSQLConnection(connection *sql.Conn) {
	if connection == nil {
		return
	}
	_ = connection.Raw(func(any) error {
		return driver.ErrBadConn
	})
	_ = connection.Close()
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
