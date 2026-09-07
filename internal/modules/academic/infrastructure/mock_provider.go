// Package infrastructure provides downstream adapters for academic queries.
package infrastructure

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
)

var chinaStandardTime = time.FixedZone("Asia/Shanghai", 8*60*60)

const (
	mockCurrentTemplatePeriodID  = "2025-2026-3"
	mockPreviousTemplatePeriodID = "2025-2026-2"
	mockOlderTemplatePeriodID    = "2024-2025-3"
	mockCurrentScheduleNote      = "这是 Mock 当前学期的课表全局备注，用于验证前端备注展示。"
)

var (
	mockUndergraduatePeriodPattern = regexp.MustCompile(`^(\d{4})-(\d{4})-([123])$`)
	mockGraduatePeriodPattern      = regexp.MustCompile(`^(\d{4}):(11|12)$`)
)

// MockProvider is a deterministic in-process Provider used before the RPC integration is available.
type MockProvider struct {
	now func() time.Time
}

// MockOption configures a MockProvider.
type MockOption func(*MockProvider)

// WithMockClock injects the clock used to construct relative examination times.
func WithMockClock(now func() time.Time) MockOption {
	return func(provider *MockProvider) {
		if now != nil {
			provider.now = now
		}
	}
}

// NewMockProvider creates the temporary academic data provider.
func NewMockProvider(options ...MockOption) *MockProvider {
	provider := &MockProvider{now: time.Now}
	for _, option := range options {
		option(provider)
	}
	return provider
}

// ListPeriods returns the mock student's available periods.
func (p *MockProvider) ListPeriods(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
) ([]domain.Period, error) {
	periods := mockPeriodsFor(p.now(), student.EducationLevel)
	if err := validateMockQuery(ctx, student, credential, ""); err != nil {
		return nil, err
	}
	return append([]domain.Period(nil), periods...), nil
}

// ListCourses returns mock official courses for one period.
func (p *MockProvider) ListCourses(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) (domain.CourseSchedule, error) {
	periods := mockPeriodsFor(p.now(), student.EducationLevel)
	if err := validateMockQuery(ctx, student, credential, periodID); err != nil {
		return domain.CourseSchedule{}, err
	}
	result := make([]domain.Course, 0)
	for _, course := range mockCourses {
		mappedPeriodID, ok := remapMockPeriodID(course.PeriodID, periods)
		if !ok || mappedPeriodID != periodID {
			continue
		}
		copied := course
		copied.PeriodID = mappedPeriodID
		copied.Weeks = mockWeeksWithinPeriod(course.Weeks, periodID, periods)
		result = append(result, copied)
	}
	scheduleNote := ""
	if periodID == periods[0].ID {
		scheduleNote = mockCurrentScheduleNote
	}
	return domain.CourseSchedule{Courses: result, ScheduleNote: scheduleNote}, nil
}

func (p *MockProvider) GetCourseSelectionSchedule(ctx context.Context, student application.StudentReference, credential application.Credential, periodID string) (domain.CourseSchedule, error) {
	return p.ListCourses(ctx, student, credential, periodID)
}

// ListGrades returns mock released grades for one period, or all periods when periodID is empty.
func (p *MockProvider) ListGrades(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Grade, error) {
	periods := mockPeriodsFor(p.now(), student.EducationLevel)
	if err := validateMockQuery(ctx, student, credential, periodID); err != nil {
		return nil, err
	}
	result := make([]domain.Grade, 0)
	for _, grade := range mockGrades {
		mappedPeriodID, ok := remapMockPeriodID(grade.PeriodID, periods)
		if !ok || (periodID != "" && mappedPeriodID != periodID) {
			continue
		}
		copied := grade
		copied.PeriodID = mappedPeriodID
		copied.Score = cloneFloat64(grade.Score)
		copied.GradeLevel = cloneGradeLevel(grade.GradeLevel)
		result = append(result, copied)
	}
	return result, nil
}

// ListExams returns mock examination arrangements for one period.
func (p *MockProvider) ListExams(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.Exam, error) {
	periods := mockPeriodsFor(p.now(), student.EducationLevel)
	if err := validateMockQuery(ctx, student, credential, periodID); err != nil {
		return nil, err
	}
	result := make([]domain.Exam, 0)
	for _, exam := range mockExams(p.now()) {
		mappedPeriodID, ok := remapMockPeriodID(exam.PeriodID, periods)
		if ok && mappedPeriodID == periodID {
			exam.PeriodID = mappedPeriodID
			result = append(result, exam)
		}
	}
	return result, nil
}

// ListCourseSelections returns mock course-selection results for one period.
func (p *MockProvider) ListCourseSelections(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) ([]domain.CourseSelection, error) {
	periods := mockPeriodsFor(p.now(), student.EducationLevel)
	if err := validateMockQuery(ctx, student, credential, periodID); err != nil {
		return nil, err
	}
	result := make([]domain.CourseSelection, 0)
	for _, selection := range mockCourseSelections {
		mappedPeriodID, ok := remapMockPeriodID(selection.PeriodID, periods)
		if !ok || mappedPeriodID != periodID {
			continue
		}
		copied := selection
		copied.PeriodID = mappedPeriodID
		copied.Note = cloneString(selection.Note)
		copied.SelectedAt = remapMockSelectionTime(selection.SelectedAt, periodID, periods)
		result = append(result, copied)
	}
	return result, nil
}

func validateMockQuery(
	ctx context.Context,
	student application.StudentReference,
	credential application.Credential,
	periodID string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if student.UserID == 0 ||
		strings.TrimSpace(student.StudentNo) == "" ||
		strings.TrimSpace(credential.StudentNo) != strings.TrimSpace(student.StudentNo) ||
		credential.Password == "" {
		return application.ErrProviderUnavailable
	}
	if periodID != "" && !validMockPeriodID(periodID, student.EducationLevel) {
		return application.ErrPeriodNotFound
	}
	return nil
}

func validMockPeriodID(periodID, educationLevel string) bool {
	switch normalizedMockEducationLevel(educationLevel) {
	case domain.EducationLevelGraduate:
		match := mockGraduatePeriodPattern.FindStringSubmatch(strings.TrimSpace(periodID))
		if len(match) != 3 {
			return false
		}
		year, err := strconv.Atoi(match[1])
		return err == nil && year >= 2000 && year <= 2100
	default:
		match := mockUndergraduatePeriodPattern.FindStringSubmatch(strings.TrimSpace(periodID))
		if len(match) != 4 {
			return false
		}
		startYear, startErr := strconv.Atoi(match[1])
		endYear, endErr := strconv.Atoi(match[2])
		return startErr == nil && endErr == nil &&
			startYear >= 2000 && startYear <= 2100 && endYear == startYear+1
	}
}

func mockPeriodsFor(now time.Time, educationLevel string) []domain.Period {
	now = now.In(chinaStandardTime)
	graduate := normalizedMockEducationLevel(educationLevel) == domain.EducationLevelGraduate
	currentOrdinal := mockCurrentPeriodOrdinal(now, graduate)
	periods := make([]domain.Period, 0, 3)
	for offset := 0; offset < 3; offset++ {
		period := mockPeriodFromOrdinal(currentOrdinal-offset, graduate)
		period.IsCurrent = offset == 0
		periods = append(periods, period)
	}
	return periods
}

func normalizedMockEducationLevel(educationLevel string) string {
	if strings.TrimSpace(educationLevel) == domain.EducationLevelGraduate {
		return domain.EducationLevelGraduate
	}
	return domain.EducationLevelUndergraduate
}

func mockCurrentPeriodOrdinal(now time.Time, graduate bool) int {
	type candidate struct {
		ordinal int
		period  domain.Period
	}
	termsPerYear := 3
	if graduate {
		termsPerYear = 2
	}
	candidates := make([]candidate, 0, termsPerYear*4)
	for academicYear := now.Year() - 2; academicYear <= now.Year()+1; academicYear++ {
		for term := 0; term < termsPerYear; term++ {
			ordinal := academicYear*termsPerYear + term
			candidates = append(candidates, candidate{
				ordinal: ordinal,
				period:  mockPeriodFromOrdinal(ordinal, graduate),
			})
		}
	}

	selected := candidates[0]
	selectedKind := 0 // 2 = in progress, 1 = upcoming, 0 = finished.
	for _, current := range candidates {
		start := current.period.StartDate.In(chinaStandardTime)
		endExclusive := start.AddDate(0, 0, current.period.WeekCount*7)
		kind := 0
		if !now.Before(start) && now.Before(endExclusive) {
			kind = 2
		} else if now.Before(start) {
			kind = 1
		}
		selectedStart := selected.period.StartDate.In(chinaStandardTime)
		switch {
		case kind > selectedKind:
			selected, selectedKind = current, kind
		case kind == 2 && selectedKind == 2 && start.After(selectedStart):
			selected = current
		case kind == 1 && selectedKind == 1 && start.Before(selectedStart):
			selected = current
		case kind == 0 && selectedKind == 0 && start.After(selectedStart):
			selected = current
		}
	}
	return selected.ordinal
}

func mockPeriodFromOrdinal(ordinal int, graduate bool) domain.Period {
	if graduate {
		year, term := ordinal/2, ordinal%2
		if term == 0 {
			return domain.Period{
				ID: fmt.Sprintf("%d:11", year), Label: fmt.Sprintf("%d-%d 学年夏秋季学期", year, year+1),
				ShortLabel: fmt.Sprintf("%02d-%02d 夏秋", year%100, (year+1)%100),
				StartDate:  mockMondayOnOrAfter(year, time.September, 1), WeekCount: 23,
			}
		}
		return domain.Period{
			ID: fmt.Sprintf("%d:12", year), Label: fmt.Sprintf("%d-%d 学年春季学期", year, year+1),
			ShortLabel: fmt.Sprintf("%02d-%02d 春", year%100, (year+1)%100),
			StartDate:  mockMondayOnOrAfter(year+1, time.March, 1), WeekCount: 23,
		}
	}
	year, term := ordinal/3, ordinal%3
	switch term {
	case 0:
		return domain.Period{
			ID: fmt.Sprintf("%d-%d-1", year, year+1), Label: fmt.Sprintf("%d-%d 学年夏季学期", year, year+1),
			ShortLabel: fmt.Sprintf("%02d-%02d 夏", year%100, (year+1)%100),
			StartDate:  mockNthMonday(year, time.August, 4), WeekCount: 4,
		}
	case 1:
		return domain.Period{
			ID: fmt.Sprintf("%d-%d-2", year, year+1), Label: fmt.Sprintf("%d-%d 学年秋季学期", year, year+1),
			ShortLabel: fmt.Sprintf("%02d-%02d 秋", year%100, (year+1)%100),
			StartDate:  mockNthMonday(year, time.September, 3), WeekCount: 19,
		}
	default:
		return domain.Period{
			ID: fmt.Sprintf("%d-%d-3", year, year+1), Label: fmt.Sprintf("%d-%d 学年春季学期", year, year+1),
			ShortLabel: fmt.Sprintf("%02d-%02d 春", year%100, (year+1)%100),
			StartDate:  mockNthMonday(year+1, time.March, 2), WeekCount: 19,
		}
	}
}

func mockNthMonday(year int, month time.Month, occurrence int) time.Time {
	first := mockMondayOnOrAfter(year, month, 1)
	return first.AddDate(0, 0, (occurrence-1)*7)
}

func mockMondayOnOrAfter(year int, month time.Month, day int) time.Time {
	date := time.Date(year, month, day, 0, 0, 0, 0, chinaStandardTime)
	daysUntilMonday := (int(time.Monday) - int(date.Weekday()) + 7) % 7
	return date.AddDate(0, 0, daysUntilMonday)
}

func remapMockPeriodID(templatePeriodID string, periods []domain.Period) (string, bool) {
	if len(periods) < 3 {
		return "", false
	}
	switch templatePeriodID {
	case mockCurrentTemplatePeriodID:
		return periods[0].ID, true
	case mockPreviousTemplatePeriodID:
		return periods[1].ID, true
	case mockOlderTemplatePeriodID:
		return periods[2].ID, true
	default:
		return "", false
	}
}

func mockWeeksWithinPeriod(weeks []int, periodID string, periods []domain.Period) []int {
	weekCount := 0
	for _, period := range periods {
		if period.ID == periodID {
			weekCount = period.WeekCount
			break
		}
	}
	result := make([]int, 0, len(weeks))
	for _, week := range weeks {
		if weekCount == 0 || week <= weekCount {
			result = append(result, week)
		}
	}
	return result
}

func remapMockSelectionTime(value *time.Time, periodID string, periods []domain.Period) *time.Time {
	if value == nil {
		return nil
	}
	for _, period := range periods {
		if period.ID != periodID {
			continue
		}
		date := period.StartDate.In(chinaStandardTime).AddDate(0, 0, -14)
		remapped := time.Date(
			date.Year(), date.Month(), date.Day(),
			value.In(chinaStandardTime).Hour(), value.In(chinaStandardTime).Minute(),
			0, 0, chinaStandardTime,
		)
		return &remapped
	}
	return cloneTime(value)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneGradeLevel(value *domain.GradeLevel) *domain.GradeLevel {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func score(value float64) *float64 { return &value }

func gradeLevel(value domain.GradeLevel) *domain.GradeLevel { return &value }

func note(value string) *string { return &value }

func weeks(start, end int) []int {
	result := make([]int, 0, end-start+1)
	for week := start; week <= end; week++ {
		result = append(result, week)
	}
	return result
}

func selectedAt(year int, month time.Month, day, hour, minute int) *time.Time {
	value := time.Date(year, month, day, hour, minute, 0, 0, chinaStandardTime)
	return &value
}

var mockCourses = []domain.Course{
	{
		ID: "course-ux", PeriodID: "2025-2026-3", CourseCode: "UXD2103",
		Name: "用户体验设计基础", Teacher: "王老师", Campus: "崂山校区", Location: "行远楼 A305",
		Weekday: 1, StartSection: 5, EndSection: 6, Weeks: weeks(3, 16),
	},
	{
		ID: "course-ocean", PeriodID: "2025-2026-3", CourseCode: "OCE1001",
		Name: "海洋科学导论", Teacher: "李老师", Campus: "崂山校区", Location: "教学楼 6204",
		Weekday: 2, StartSection: 1, EndSection: 2, Weeks: weeks(1, 18),
	},
	{
		ID: "course-data", PeriodID: "2025-2026-3", CourseCode: "DAT2206",
		Name: "数据可视化", Teacher: "刘老师", Campus: "崂山校区", Location: "信息楼 B201",
		Weekday: 3, StartSection: 3, EndSection: 4, Weeks: weeks(2, 17),
	},
	{
		ID: "course-data-practice", PeriodID: "2025-2026-3", CourseCode: "DAT2210",
		Name: "海洋数据实践", Teacher: "杨老师", Campus: "崂山校区", Location: "信息楼 B203",
		Weekday: 3, StartSection: 3, EndSection: 4, Weeks: weeks(4, 14),
	},
	{
		ID: "course-english", PeriodID: "2025-2026-3", CourseCode: "ENG1404",
		Name: "大学英语（四）", Teacher: "陈老师", Campus: "崂山校区", Location: "行知楼 412",
		Weekday: 4, StartSection: 1, EndSection: 2, Weeks: weeks(1, 16),
	},
	{
		ID: "course-prototype", PeriodID: "2025-2026-3", CourseCode: "UXD2208",
		Name: "交互原型与实践", Teacher: "赵老师", Campus: "崂山校区", Location: "工程训练中心 305",
		Weekday: 5, StartSection: 3, EndSection: 4, Weeks: weeks(1, 15),
	},
	{
		ID: "course-pe", PeriodID: "2025-2026-3", CourseCode: "PED1404",
		Name: "体育（游泳）", Teacher: "孙老师", Campus: "崂山校区", Location: "游泳馆",
		Weekday: 5, StartSection: 7, EndSection: 8, Weeks: []int{2, 4, 6, 8, 10, 12, 14, 16},
	},
	{
		ID: "course-history-ocean", PeriodID: "2025-2026-2", CourseCode: "HIS2012",
		Name: "海洋文明史", Teacher: "周老师", Campus: "鱼山校区", Location: "教学楼 5302",
		Weekday: 2, StartSection: 3, EndSection: 4, Weeks: weeks(1, 16),
	},
	{
		ID: "course-history-math", PeriodID: "2024-2025-3", CourseCode: "MTH1202",
		Name: "高等数学（二）", Teacher: "高老师", Campus: "崂山校区", Location: "行远楼 B101",
		Weekday: 4, StartSection: 1, EndSection: 2, Weeks: weeks(1, 18),
	},
}

var mockGrades = []domain.Grade{
	{ID: "grade-ux", PeriodID: "2025-2026-3", CourseCode: "UXD2103", CourseName: "用户体验设计基础", CourseType: "专业必修", Credit: 3, GradeType: domain.GradeTypeNumber, Score: score(92)},
	{ID: "grade-ocean", PeriodID: "2025-2026-3", CourseCode: "OCE1001", CourseName: "海洋科学导论", CourseType: "通识必修", Credit: 2, GradeType: domain.GradeTypeNumber, Score: score(86)},
	{ID: "grade-data", PeriodID: "2025-2026-3", CourseCode: "DAT2206", CourseName: "数据可视化", CourseType: "专业选修", Credit: 2.5, GradeType: domain.GradeTypeNumber, Score: score(89)},
	{ID: "grade-english", PeriodID: "2025-2026-3", CourseCode: "ENG1404", CourseName: "大学英语（四）", CourseType: "公共必修", Credit: 2, GradeType: domain.GradeTypeNumber, Score: score(81)},
	{ID: "grade-prototype", PeriodID: "2025-2026-3", CourseCode: "UXD2208", CourseName: "交互原型与实践", CourseType: "实践课程", Credit: 2, GradeType: domain.GradeTypeNumber, Score: score(95)},
	{ID: "grade-pe", PeriodID: "2025-2026-3", CourseCode: "PED1404", CourseName: "体育（游泳）", CourseType: "公共必修", Credit: 1, GradeType: domain.GradeTypeNumber, Score: score(88)},
	{ID: "grade-military", PeriodID: "2025-2026-3", CourseCode: "MTH1001", CourseName: "军事理论", CourseType: "公共必修", Credit: 2, GradeType: domain.GradeTypeLevel, GradeLevel: gradeLevel(domain.GradeLevelExcellent)},
	{ID: "grade-history-ocean", PeriodID: "2025-2026-2", CourseCode: "HIS2012", CourseName: "海洋文明史", CourseType: "通识选修", Credit: 2, GradeType: domain.GradeTypeNumber, Score: score(90)},
	{ID: "grade-history-design", PeriodID: "2025-2026-2", CourseCode: "UXD2101", CourseName: "设计心理学", CourseType: "专业必修", Credit: 3, GradeType: domain.GradeTypeNumber, Score: score(87)},
	{ID: "grade-history-practice", PeriodID: "2025-2026-2", CourseCode: "ENT1002", CourseName: "创新创业实践", CourseType: "实践课程", Credit: 1, GradeType: domain.GradeTypeLevel, GradeLevel: gradeLevel(domain.GradeLevelGood)},
	{ID: "grade-history-math", PeriodID: "2024-2025-3", CourseCode: "MTH1202", CourseName: "高等数学（二）", CourseType: "公共必修", Credit: 4, GradeType: domain.GradeTypeNumber, Score: score(83)},
}

func mockExams(now time.Time) []domain.Exam {
	base := now.In(chinaStandardTime)
	at := func(days, hour, minute int) time.Time {
		value := time.Date(base.Year(), base.Month(), base.Day(), hour, minute, 0, 0, chinaStandardTime)
		return value.AddDate(0, 0, days)
	}
	return []domain.Exam{
		{
			ID: "exam-data", PeriodID: "2025-2026-3", CourseCode: "DAT2206", CourseName: "数据可视化",
			StartAt: at(3, 9, 0), EndAt: at(3, 11, 0), Campus: "崂山校区", Location: "行远楼 A201",
			Seat: "18 号", Phase: domain.ExamPhaseMidterm, Method: "闭卷考试",
			Materials: "学生证、黑色签字笔", Notice: "请提前 20 分钟到场，开考 30 分钟后不得入场。",
		},
		{
			ID: "exam-ocean", PeriodID: "2025-2026-3", CourseCode: "OCE1001", CourseName: "海洋科学导论",
			StartAt: at(7, 14, 0), EndAt: at(7, 16, 0), Campus: "崂山校区", Location: "教学楼 6204",
			Seat: "32 号", Phase: domain.ExamPhaseFinal, Method: "闭卷考试",
			Materials: "学生证、2B 铅笔、橡皮", Notice: "答题卡请规范填涂，不允许携带计算器。",
		},
		{
			ID: "exam-ux", PeriodID: "2025-2026-3", CourseCode: "UXD2103", CourseName: "用户体验设计基础",
			StartAt: at(12, 10, 0), EndAt: at(12, 11, 30), Campus: "崂山校区", Location: "信息楼 B301",
			Seat: "第 4 组", Phase: domain.ExamPhaseFinal, Method: "课程展示",
			Materials: "展示文件、过程手册", Notice: "请在考试前一天将最终文件上传至课程平台。",
		},
		{
			ID: "exam-finished", PeriodID: "2025-2026-3", CourseCode: "ENG1404", CourseName: "大学英语（四）",
			StartAt: at(-5, 9, 0), EndAt: at(-5, 11, 0), Campus: "崂山校区", Location: "行知楼 412",
			Seat: "26 号", Phase: domain.ExamPhaseMakeup, Method: "闭卷考试",
			Materials: "学生证、2B 铅笔", Notice: "考试已结束，请关注成绩发布时间。",
		},
		{
			ID: "exam-history", PeriodID: "2025-2026-2", CourseCode: "HIS2012", CourseName: "海洋文明史",
			StartAt: at(-20, 14, 0), EndAt: at(-20, 16, 0), Campus: "鱼山校区", Location: "胜利楼 201",
			Seat: "12 号", Phase: domain.ExamPhaseEntrance, Method: "开卷考试",
			Materials: "学生证、纸质教材", Notice: "仅允许携带纸质资料。",
		},
	}
}

var mockCourseSelections = []domain.CourseSelection{
	{ID: "selection-ux", PeriodID: "2025-2026-3", CourseCode: "UXD2103", CourseName: "用户体验设计基础", CourseType: "专业必修", Credit: 3, Teacher: "王老师", Campus: "崂山校区", Location: "行远楼 A305", Schedule: "周一 第 5-6 节", Capacity: 48, Enrolled: 46, Status: domain.CourseSelectionSelected, SelectedAt: selectedAt(2026, time.January, 12, 10, 24)},
	{ID: "selection-ocean", PeriodID: "2025-2026-3", CourseCode: "OCE1001", CourseName: "海洋科学导论", CourseType: "通识必修", Credit: 2, Teacher: "李老师", Campus: "崂山校区", Location: "教学楼 6204", Schedule: "周二 第 1-2 节", Capacity: 80, Enrolled: 78, Status: domain.CourseSelectionSelected, SelectedAt: selectedAt(2026, time.January, 12, 10, 25)},
	{ID: "selection-data", PeriodID: "2025-2026-3", CourseCode: "DAT2206", CourseName: "数据可视化", CourseType: "专业选修", Credit: 2.5, Teacher: "刘老师", Campus: "崂山校区", Location: "信息楼 B201", Schedule: "周三 第 3-4 节", Capacity: 36, Enrolled: 36, Status: domain.CourseSelectionSelected, SelectedAt: selectedAt(2026, time.January, 13, 9, 11)},
	{ID: "selection-ai", PeriodID: "2025-2026-3", CourseCode: "CSM3018", CourseName: "生成式人工智能应用", CourseType: "跨学科选修", Credit: 2, Teacher: "张老师", Campus: "崂山校区", Location: "信息楼 A408", Schedule: "周四 第 7-8 节", Capacity: 40, Enrolled: 39, Status: domain.CourseSelectionPending, SelectedAt: selectedAt(2026, time.January, 13, 9, 14), Note: note("该课程正在等待系统最终确认。")},
	{ID: "selection-failed", PeriodID: "2025-2026-3", CourseCode: "OCE3127", CourseName: "海岸带生态修复", CourseType: "专业选修", Credit: 2, Teacher: "黄老师", Campus: "崂山校区", Location: "海洋楼 C102", Schedule: "周五 第 5-6 节", Capacity: 30, Enrolled: 30, Status: domain.CourseSelectionFailed, SelectedAt: selectedAt(2026, time.January, 13, 9, 16), Note: note("课程容量已满，本轮选课未成功。")},
	{ID: "selection-english", PeriodID: "2025-2026-3", CourseCode: "ENG1404", CourseName: "大学英语（四）", CourseType: "公共必修", Credit: 2, Teacher: "陈老师", Campus: "崂山校区", Location: "行知楼 412", Schedule: "周四 第 1-2 节", Capacity: 60, Enrolled: 58, Status: domain.CourseSelectionSelected, SelectedAt: selectedAt(2026, time.January, 12, 10, 26)},
	{ID: "selection-history", PeriodID: "2025-2026-2", CourseCode: "HIS2012", CourseName: "海洋文明史", CourseType: "通识选修", Credit: 2, Teacher: "周老师", Campus: "鱼山校区", Location: "教学楼 5302", Schedule: "周二 第 3-4 节", Capacity: 50, Enrolled: 47, Status: domain.CourseSelectionSelected, SelectedAt: selectedAt(2025, time.August, 28, 14, 20)},
}

var _ application.Provider = (*MockProvider)(nil)
