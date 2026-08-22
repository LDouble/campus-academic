// Package domain defines transport-independent academic query models.
package domain

import "time"

const (
	// EducationLevelUndergraduate identifies the undergraduate calendar.
	EducationLevelUndergraduate = "undergraduate"
	// EducationLevelGraduate identifies the graduate calendar.
	EducationLevelGraduate = "graduate"
)

// GradeType identifies the grading scheme used by one course.
type GradeType string

const (
	// GradeTypeNumber identifies a percentage score.
	GradeTypeNumber GradeType = "number"
	// GradeTypeLevel identifies a named grade level.
	GradeTypeLevel GradeType = "level"
)

// GradeLevel is a named academic result.
type GradeLevel string

const (
	// GradeLevelExcellent represents 优秀.
	GradeLevelExcellent GradeLevel = "优秀"
	// GradeLevelGood represents 良好.
	GradeLevelGood GradeLevel = "良好"
	// GradeLevelMedium represents 中等.
	GradeLevelMedium GradeLevel = "中等"
	// GradeLevelPass represents 及格.
	GradeLevelPass GradeLevel = "及格"
	// GradeLevelFail represents 不及格.
	GradeLevelFail GradeLevel = "不及格"
	// GradeLevelExempt represents 免修.
	GradeLevelExempt GradeLevel = "免修"
)

// ExamPhase identifies an examination stage.
type ExamPhase string

const (
	// ExamPhaseMidterm represents a midterm exam.
	ExamPhaseMidterm ExamPhase = "期中"
	// ExamPhaseFinal represents a final exam.
	ExamPhaseFinal ExamPhase = "期末"
	// ExamPhaseMakeup represents a makeup exam.
	ExamPhaseMakeup ExamPhase = "补考"
	// ExamPhaseEntrance represents an entrance exam.
	ExamPhaseEntrance ExamPhase = "入学"
)

// CourseSelectionStatus identifies the result of one course selection.
type CourseSelectionStatus string

const (
	// CourseSelectionSelected indicates a confirmed selection.
	CourseSelectionSelected CourseSelectionStatus = "selected"
	// CourseSelectionPending indicates a selection awaiting confirmation.
	CourseSelectionPending CourseSelectionStatus = "pending"
	// CourseSelectionFailed indicates an unsuccessful selection.
	CourseSelectionFailed CourseSelectionStatus = "failed"
)

// Period is one academic term visible to the current student.
type Period struct {
	ID         string
	Label      string
	ShortLabel string
	StartDate  time.Time
	WeekCount  int
	IsCurrent  bool
}

// CalendarEventType identifies one public academic-calendar event category.
type CalendarEventType string

const (
	// CalendarEventTermStart marks registration, return-to-campus, or term opening.
	CalendarEventTermStart CalendarEventType = "term_start"
	// CalendarEventTeaching marks a teaching arrangement or milestone.
	CalendarEventTeaching CalendarEventType = "teaching"
	// CalendarEventExam marks an examination period or arrangement.
	CalendarEventExam CalendarEventType = "exam"
	// CalendarEventHoliday marks a public or university holiday.
	CalendarEventHoliday CalendarEventType = "holiday"
	// CalendarEventMakeup marks a makeup teaching-day adjustment.
	CalendarEventMakeup CalendarEventType = "makeup"
	// CalendarEventRegistration marks course registration or enrollment.
	CalendarEventRegistration CalendarEventType = "registration"
	// CalendarEventOther marks an event outside the standard categories.
	CalendarEventOther CalendarEventType = "other"
)

// CalendarEventPriority controls how prominently an event appears in “Today”.
type CalendarEventPriority string

const (
	// CalendarEventPriorityNormal is the default event priority.
	CalendarEventPriorityNormal CalendarEventPriority = "normal"
	// CalendarEventPriorityImportant marks an event as time-sensitive.
	CalendarEventPriorityImportant CalendarEventPriority = "important"
)

// CalendarEvent is one public event maintained in the platform calendar.
type CalendarEvent struct {
	ID                  string
	Title               string
	Type                CalendarEventType
	StartDate           time.Time
	EndDate             time.Time
	PeriodID            string
	Campuses            []string
	Description         string
	HomepageRecommended bool
	Priority            CalendarEventPriority
	Remindable          bool
}

// Calendar is one education level's public calendar snapshot.
type Calendar struct {
	EducationLevel string
	Timezone       string
	RefreshedAt    time.Time
	Periods        []Period
	Events         []CalendarEvent
}

// Course is one official timetable entry.
type Course struct {
	ID           string
	PeriodID     string
	CourseCode   string
	Name         string
	Teacher      string
	Campus       string
	Location     string
	Note         string
	Weekday      int
	StartSection int
	EndSection   int
	Weeks        []int
}

// CourseSchedule is one timetable snapshot returned by an academic provider.
// Courses and ScheduleNote come from the same upstream response and must stay
// together across caching and transport boundaries.
type CourseSchedule struct {
	Courses      []Course
	ScheduleNote string
}

// Grade is one released course result.
type Grade struct {
	ID         string
	PeriodID   string
	CourseCode string
	CourseName string
	CourseType string
	Credit     float64
	GradeType  GradeType
	Score      *float64
	GradeLevel *GradeLevel
}

// Exam is one examination arrangement.
type Exam struct {
	ID         string
	PeriodID   string
	CourseCode string
	CourseName string
	StartAt    time.Time
	EndAt      time.Time
	Campus     string
	Location   string
	Seat       string
	Phase      ExamPhase
	Method     string
	Materials  string
	Notice     string
}

// CourseSelection is one course-selection result.
type CourseSelection struct {
	ID         string
	PeriodID   string
	CourseCode string
	CourseName string
	CourseType string
	Credit     float64
	Teacher    string
	Campus     string
	Location   string
	Schedule   string
	Capacity   int
	Enrolled   int
	Status     CourseSelectionStatus
	SelectedAt *time.Time
	ResultText *string
	Note       *string
}

// CourseCatalogEntry is one school-wide course offering. It deliberately
// excludes student identities and enrollment rosters.
type CourseCatalogEntry struct {
	SourceKey   string
	PeriodID    string
	OpeningCode string
	CourseCode  string
	CourseName  string
	CourseType  string
	Department  string
	Teachers    string
	Campus      string
	Classes     string
	Schedule    string
	Location    string
	Language    string
	Capacity    int
	Enrolled    int
	Note        string
}

// CourseCatalogPage is one bounded page from a school-wide course catalog.
type CourseCatalogPage struct {
	Entries    []CourseCatalogEntry
	Page       int
	PageSize   int
	TotalCount int
	TotalPages int
	HasMore    bool
}
