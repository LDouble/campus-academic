package domain

import (
	"errors"
	"testing"
	"time"
)

func TestSnapshotValidate(t *testing.T) {
	valid := Snapshot{
		SourceCutoffAt: time.Now(),
		Courses: []CourseTermAggregate{{
			EducationLevel: EducationLevelUndergraduate,
			PeriodID:       "2025-2026-1",
			TermLabel:      "2025 夏季学期",
			TermCode:       "2025-2026-1",
			CourseCode:     "MATH1001",
			ValidCount:     2,
			PassCount:      1,
			FailCount:      1,
		}},
		Instructors: []InstructorCourseTermAggregate{{
			EducationLevel: EducationLevelUndergraduate,
			PeriodID:       "2025-2026-1",
			TermLabel:      "2025 夏季学期",
			TermCode:       "2025-2026-1",
			CourseCode:     "MATH1001",
			TeacherKey:     "teacher",
			ValidCount:     2,
			PassCount:      1,
			FailCount:      1,
		}},
	}
	tests := []struct {
		name     string
		snapshot Snapshot
		wantErr  bool
	}{
		{name: "valid", snapshot: valid},
		{name: "empty", snapshot: Snapshot{}, wantErr: true},
		{
			name: "invalid course counts",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses: []CourseTermAggregate{{
					EducationLevel: EducationLevelUndergraduate,
					PeriodID:       "2025-2026-1",
					TermLabel:      "2025 夏季学期",
					TermCode:       "2025-2026-1",
					CourseCode:     "MATH1001",
					ValidCount:     2,
					PassCount:      2,
					FailCount:      1,
				}},
			},
			wantErr: true,
		},
		{
			name: "missing teacher key",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses:        valid.Courses,
				Instructors: []InstructorCourseTermAggregate{{
					EducationLevel: EducationLevelUndergraduate,
					PeriodID:       "2025-2026-1",
					TermLabel:      "2025 夏季学期",
					TermCode:       "2025-2026-1",
					CourseCode:     "MATH1001",
					ValidCount:     1,
					PassCount:      1,
				}},
			},
			wantErr: true,
		},
		{
			name: "invalid numeric score aggregate",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses: []CourseTermAggregate{{
					EducationLevel:      EducationLevelUndergraduate,
					PeriodID:            "2025-2026-1",
					TermLabel:           "2025 夏季学期",
					TermCode:            "2025-2026-1",
					CourseCode:          "MATH1001",
					ValidCount:          2,
					PassCount:           1,
					FailCount:           1,
					NumericScoreCount:   2,
					NumericScoreSumX100: 20001,
				}},
			},
			wantErr: true,
		},
		{
			name: "missing term identity",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses: []CourseTermAggregate{{
					TermCode:   "2025-2026-1",
					CourseCode: "MATH1001",
					ValidCount: 1,
					PassCount:  1,
				}},
			},
			wantErr: true,
		},
		{
			name: "duplicate course partition",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses:        append(append([]CourseTermAggregate{}, valid.Courses...), valid.Courses[0]),
			},
			wantErr: true,
		},
		{
			name: "duplicate instructor partition",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses:        valid.Courses,
				Instructors: append(
					append([]InstructorCourseTermAggregate{}, valid.Instructors...),
					valid.Instructors[0],
				),
			},
			wantErr: true,
		},
		{
			name: "same course across education levels is distinct",
			snapshot: Snapshot{
				SourceCutoffAt: valid.SourceCutoffAt,
				Courses: append(append([]CourseTermAggregate{}, valid.Courses...), CourseTermAggregate{
					EducationLevel: EducationLevelGraduate,
					PeriodID:       "2025:11",
					TermLabel:      "2025-2026 学年夏秋学期",
					TermCode:       "2025-2026-2",
					CourseCode:     "MATH1001",
					ValidCount:     1,
					PassCount:      1,
				}),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.snapshot.Validate()
			if test.wantErr && !errors.Is(err, ErrEmptySnapshot) {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestDistributionAdd(t *testing.T) {
	value := Distribution{NumericFail: 1, Score6069: 2}
	value.Add(Distribution{NumericFail: 3, Score6069: 4, LevelGood: 5})
	if value.NumericFail != 4 || value.Score6069 != 6 || value.LevelGood != 5 {
		t.Fatalf("distribution = %+v", value)
	}
}
