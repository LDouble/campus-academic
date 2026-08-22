// Command generator creates Analytics-owned type-safe GORM queries.
package main

import (
	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/domain"
	"gorm.io/gen"
)

func main() {
	generator := gen.NewGenerator(gen.Config{
		OutPath: "./query",
		Mode:    gen.WithDefaultQuery | gen.WithQueryInterface,
	})
	generator.ApplyBasic(
		domain.AcademicStatisticsBatch{},
		domain.AcademicCourseTermStatistic{},
		domain.AcademicInstructorCourseTermStatistic{},
	)
	generator.Execute()
}
