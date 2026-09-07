package domain

import "time"

// AcademicCoursePassRateStatistic is the published all-term course projection.
// It keeps list queries off the much larger per-term source table.
type AcademicCoursePassRateStatistic struct {
	ID                  uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	BatchId             uint64    `gorm:"column:batch_id;not null" json:"batch_id"`
	EducationLevel      string    `gorm:"column:education_level;not null" json:"education_level"`
	CourseCode          string    `gorm:"column:course_code;not null" json:"course_code"`
	CourseName          string    `gorm:"column:course_name;not null" json:"course_name"`
	TermCount           int64     `gorm:"column:term_count;not null" json:"term_count"`
	ValidCount          int64     `gorm:"column:valid_count;not null" json:"valid_count"`
	PassCount           int64     `gorm:"column:pass_count;not null" json:"pass_count"`
	FailCount           int64     `gorm:"column:fail_count;not null" json:"fail_count"`
	NumericScoreCount   int64     `gorm:"column:numeric_score_count;not null" json:"numeric_score_count"`
	NumericScoreSumX100 int64     `gorm:"column:numeric_score_sum_x100;not null" json:"numeric_score_sum_x100"`
	NumericFailCount    int64     `gorm:"column:numeric_fail_count;not null" json:"numeric_fail_count"`
	Score6069Count      int64     `gorm:"column:score_60_69_count;not null" json:"score_60_69_count"`
	Score7079Count      int64     `gorm:"column:score_70_79_count;not null" json:"score_70_79_count"`
	Score8089Count      int64     `gorm:"column:score_80_89_count;not null" json:"score_80_89_count"`
	Score90100Count     int64     `gorm:"column:score_90_100_count;not null" json:"score_90_100_count"`
	LevelExcellentCount int64     `gorm:"column:level_excellent_count;not null" json:"level_excellent_count"`
	LevelGoodCount      int64     `gorm:"column:level_good_count;not null" json:"level_good_count"`
	LevelMediumCount    int64     `gorm:"column:level_medium_count;not null" json:"level_medium_count"`
	LevelPassCount      int64     `gorm:"column:level_pass_count;not null" json:"level_pass_count"`
	LevelFailCount      int64     `gorm:"column:level_fail_count;not null" json:"level_fail_count"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func (AcademicCoursePassRateStatistic) TableName() string {
	return "academic_course_pass_rate_statistics"
}
