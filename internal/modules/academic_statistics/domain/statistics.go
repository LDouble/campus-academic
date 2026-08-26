// Package domain contains academic pass-rate statistics and publication rules.
package domain

import (
	"errors"
	"strings"
	"time"
)

const (
	// BatchStatusRunning marks a batch whose source query is still executing.
	BatchStatusRunning = "running"
	// BatchStatusFailed marks a batch that was not published.
	BatchStatusFailed = "failed"
	// BatchStatusPublished marks the only batch state visible to readers.
	BatchStatusPublished = "published"

	// TriggerScheduled identifies the daily worker schedule.
	TriggerScheduled = "scheduled"
	// TriggerManual identifies an administrator-requested run.
	TriggerManual = "manual"

	// RuleVersion identifies the normalization, term-identity, and pass/fail
	// rules used by the current offline aggregation implementation. v6 gives
	// level-grade and exemption classification its own publication boundary.
	RuleVersion = "v6"

	// EducationLevelUndergraduate identifies undergraduate academic periods.
	EducationLevelUndergraduate = "undergraduate"
	// EducationLevelGraduate identifies graduate academic periods.
	EducationLevelGraduate = "graduate"
)

// ErrEmptySnapshot prevents an empty or invalid source result from replacing a
// previously published statistics version.
var ErrEmptySnapshot = errors.New("academic statistics snapshot is empty")

// BatchTrigger records why an aggregation batch started and, for manual runs,
// which administrator requested it.
type BatchTrigger struct {
	Type    string
	ActorID *uint64
	Note    *string
	TaskID  *string
}

// ScheduledBatchTrigger creates the audit metadata for the daily run.
func ScheduledBatchTrigger() BatchTrigger {
	return BatchTrigger{Type: TriggerScheduled}
}

// ManualBatchTrigger creates the audit metadata for an administrator run.
func ManualBatchTrigger(actorID uint64, note, taskID string) BatchTrigger {
	trigger := BatchTrigger{
		Type:    TriggerManual,
		ActorID: &actorID,
		TaskID:  &taskID,
	}
	if note != "" {
		trigger.Note = &note
	}
	return trigger
}

// OperationalBatchTrigger creates audit metadata for a synchronous operator
// run that is not associated with an administrator account.
func OperationalBatchTrigger(note, taskID string) BatchTrigger {
	trigger := BatchTrigger{
		Type:   TriggerManual,
		TaskID: &taskID,
	}
	if note != "" {
		trigger.Note = &note
	}
	return trigger
}

// Distribution stores aggregate score buckets rather than individual grades.
type Distribution struct {
	NumericFail    int64
	Score6069      int64
	Score7079      int64
	Score8089      int64
	Score90100     int64
	LevelExcellent int64
	LevelGood      int64
	LevelMedium    int64
	LevelPass      int64
	LevelFail      int64
}

// Add merges another aggregate distribution into this value.
func (distribution *Distribution) Add(other Distribution) {
	distribution.NumericFail += other.NumericFail
	distribution.Score6069 += other.Score6069
	distribution.Score7079 += other.Score7079
	distribution.Score8089 += other.Score8089
	distribution.Score90100 += other.Score90100
	distribution.LevelExcellent += other.LevelExcellent
	distribution.LevelGood += other.LevelGood
	distribution.LevelMedium += other.LevelMedium
	distribution.LevelPass += other.LevelPass
	distribution.LevelFail += other.LevelFail
}

// CourseTermAggregate is one course's aggregate for one academic term.
type CourseTermAggregate struct {
	EducationLevel      string
	PeriodID            string
	TermLabel           string
	TermCode            string
	CourseCode          string
	CourseName          string
	ValidCount          int64
	PassCount           int64
	FailCount           int64
	NumericScoreCount   int64
	NumericScoreSumX100 int64
	Distribution        Distribution
}

// InstructorCourseTermAggregate is one teacher-course aggregate for one term.
type InstructorCourseTermAggregate struct {
	EducationLevel      string
	PeriodID            string
	TermLabel           string
	TermCode            string
	CourseCode          string
	CourseName          string
	TeacherKey          string
	TeacherName         string
	ClassCount          int64
	ValidCount          int64
	PassCount           int64
	FailCount           int64
	NumericScoreCount   int64
	NumericScoreSumX100 int64
	Distribution        Distribution
}

// Snapshot is the complete offline aggregate produced from one consistent
// source-database view.
type Snapshot struct {
	SourceCutoffAt time.Time
	SourceRowCount int64
	Courses        []CourseTermAggregate
	Instructors    []InstructorCourseTermAggregate
}

// Validate rejects incomplete snapshots before they can become public.
func (snapshot Snapshot) Validate() error {
	if snapshot.SourceCutoffAt.IsZero() || len(snapshot.Courses) == 0 {
		return ErrEmptySnapshot
	}
	courses := make(map[coursePartition]struct{}, len(snapshot.Courses))
	for _, course := range snapshot.Courses {
		if !validTermIdentity(
			course.EducationLevel,
			course.PeriodID,
			course.TermLabel,
			course.TermCode,
		) ||
			strings.TrimSpace(course.CourseCode) == "" ||
			course.ValidCount <= 0 ||
			course.PassCount+course.FailCount != course.ValidCount ||
			!validNumericScoreAggregate(
				course.ValidCount,
				course.NumericScoreCount,
				course.NumericScoreSumX100,
			) {
			return ErrEmptySnapshot
		}
		partition := coursePartition{
			educationLevel: course.EducationLevel,
			periodID:       course.PeriodID,
			courseCode:     course.CourseCode,
		}
		if _, exists := courses[partition]; exists {
			return ErrEmptySnapshot
		}
		courses[partition] = struct{}{}
	}
	instructors := make(
		map[instructorCoursePartition]struct{},
		len(snapshot.Instructors),
	)
	for _, instructor := range snapshot.Instructors {
		if !validTermIdentity(
			instructor.EducationLevel,
			instructor.PeriodID,
			instructor.TermLabel,
			instructor.TermCode,
		) ||
			strings.TrimSpace(instructor.CourseCode) == "" ||
			strings.TrimSpace(instructor.TeacherKey) == "" ||
			instructor.ValidCount <= 0 ||
			instructor.PassCount+instructor.FailCount != instructor.ValidCount ||
			!validNumericScoreAggregate(
				instructor.ValidCount,
				instructor.NumericScoreCount,
				instructor.NumericScoreSumX100,
			) {
			return ErrEmptySnapshot
		}
		partition := instructorCoursePartition{
			coursePartition: coursePartition{
				educationLevel: instructor.EducationLevel,
				periodID:       instructor.PeriodID,
				courseCode:     instructor.CourseCode,
			},
			teacherKey: instructor.TeacherKey,
		}
		if _, exists := instructors[partition]; exists {
			return ErrEmptySnapshot
		}
		instructors[partition] = struct{}{}
	}
	return nil
}

type coursePartition struct {
	educationLevel string
	periodID       string
	courseCode     string
}

type instructorCoursePartition struct {
	coursePartition
	teacherKey string
}

func validTermIdentity(educationLevel, periodID, termLabel, termCode string) bool {
	return (educationLevel == EducationLevelUndergraduate ||
		educationLevel == EducationLevelGraduate) &&
		strings.TrimSpace(periodID) != "" &&
		strings.TrimSpace(termLabel) != "" &&
		strings.TrimSpace(termCode) != ""
}

func validNumericScoreAggregate(validCount, scoreCount, sumX100 int64) bool {
	return scoreCount >= 0 &&
		scoreCount <= validCount &&
		sumX100 >= 0 &&
		sumX100 <= scoreCount*10000
}

// Search filters published statistics. Empty values leave a dimension
// unrestricted.
type Search struct {
	Keyword        string
	CourseCode     string
	TeacherKey     string
	TeacherName    string
	TermCode       string
	EducationLevel string
	Status         string
}

// CoursePassRate is a query projection across one or more term aggregates.
type CoursePassRate struct {
	EducationLevel      string
	CourseCode          string
	CourseName          string
	TermCount           int64
	ValidCount          int64
	PassCount           int64
	FailCount           int64
	NumericScoreCount   int64
	NumericScoreSumX100 int64
	Distribution        Distribution
}

// InstructorPassRate is a query projection across one or more term
// aggregates.
type InstructorPassRate struct {
	EducationLevel      string
	CourseCode          string
	CourseName          string
	TeacherKey          string
	TeacherName         string
	TermCount           int64
	ClassCount          int64
	ValidCount          int64
	PassCount           int64
	FailCount           int64
	NumericScoreCount   int64
	NumericScoreSumX100 int64
	Distribution        Distribution
}

// PassRateTrendPoint is one aggregate academic-term observation.
type PassRateTrendPoint struct {
	EducationLevel      string
	PeriodID            string
	TermLabel           string
	TermCode            string
	ValidCount          int64
	PassCount           int64
	FailCount           int64
	NumericScoreCount   int64
	NumericScoreSumX100 int64
	Distribution        Distribution
}

// PassRateTrend is one course or one teacher-course series across terms.
type PassRateTrend struct {
	CourseCode  string
	CourseName  string
	TeacherKey  string
	TeacherName string
	Points      []PassRateTrendPoint
}

// PublishedMetadata identifies the immutable batch backing a response.
type PublishedMetadata struct {
	BatchID           uint64
	SourceCutoffAt    time.Time
	PublishedAt       time.Time
	RuleVersion       string
	MinimumSampleSize int64
}
