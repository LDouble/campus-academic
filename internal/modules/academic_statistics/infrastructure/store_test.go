package infrastructure

import (
	"testing"

	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/domain"
)

func TestCoursePassRateEntitiesAggregateAllListFields(t *testing.T) {
	rows := coursePassRateEntities(42, []*domain.AcademicCourseTermStatistic{
		{
			EducationLevel: domain.EducationLevelUndergraduate,
			CourseCode:     "MATH101", CourseName: "高等数学A", ValidCount: 3,
			PassCount: 2, FailCount: 1, NumericScoreCount: 3,
			NumericScoreSumX100: 21000, NumericFailCount: 1,
			Score6069Count: 1, LevelGoodCount: 2,
		},
		{
			EducationLevel: domain.EducationLevelUndergraduate,
			CourseCode:     "MATH101", CourseName: "高等数学B", ValidCount: 4,
			PassCount: 4, NumericScoreCount: 4, NumericScoreSumX100: 32000,
			Score8089Count: 4, LevelExcellentCount: 1,
		},
		{
			EducationLevel: domain.EducationLevelGraduate,
			CourseCode:     "MATH101", CourseName: "高等数学", ValidCount: 2,
			PassCount: 2,
		},
	})
	if len(rows) != 2 {
		t.Fatalf("projection row count = %d, want 2", len(rows))
	}
	var row *domain.AcademicCoursePassRateStatistic
	for _, candidate := range rows {
		if candidate.EducationLevel == domain.EducationLevelUndergraduate {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatal("undergraduate projection is missing")
	}
	if row.BatchId != 42 || row.CourseName != "高等数学B" || row.TermCount != 2 ||
		row.ValidCount != 7 || row.PassCount != 6 || row.FailCount != 1 ||
		row.NumericScoreSumX100 != 53000 || row.LevelGoodCount != 2 {
		t.Fatalf("unexpected projection: %#v", row)
	}
}

func TestCountCoursePassRatesExcludesSmallSamples(t *testing.T) {
	rows := coursePassRateEntities(1, []*domain.AcademicCourseTermStatistic{{
		EducationLevel: domain.EducationLevelUndergraduate,
		CourseCode:     "MATH101", ValidCount: 4,
	}})
	if len(rows) != 1 {
		t.Fatalf("projection row count = %d, want 1", len(rows))
	}
	if count := countCoursePassRates(rows, 5); count != 0 {
		t.Fatalf("public projection count = %d, want 0", count)
	}
}

func TestCourseProjectionUsesCachedCount(t *testing.T) {
	batch := &domain.AcademicStatisticsBatch{
		CoursePassRateCount: 20,
		MinimumSampleSize:   5,
	}
	if !courseProjectionUsesCachedCount(batch, domain.Search{}, 5) {
		t.Fatal("expected unfiltered list to use cached count")
	}
	if courseProjectionUsesCachedCount(batch, domain.Search{Keyword: "数学"}, 5) {
		t.Fatal("keyword-filtered list must not use cached count")
	}
	if courseProjectionUsesCachedCount(batch, domain.Search{}, 6) {
		t.Fatal("changed sample threshold must not use cached count")
	}
	if courseProjectionUsesCachedCount(
		&domain.AcademicStatisticsBatch{CoursePassRateCount: 20},
		domain.Search{},
		5,
	) {
		t.Fatal("legacy batch without a recorded threshold must not use cached count")
	}
}
