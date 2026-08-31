package ouc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
)

const graduateCoursePlanFixture = `
<!doctype html>
<html><body>
<table class="table table-bordered-xk">
  <tr>
    <th></th><th>课程编号</th><th>课程名称</th><th>课程性质</th>
    <th>学分</th><th>选课学年</th><th>学期</th><th>任课老师</th>
    <th>修读情况/成绩</th>
  </tr>
  <tr>
    <td><span>必修</span></td><td>GR001</td><td>研究方法</td><td>公共课</td>
    <td>2.0</td><td>2017-2018</td><td>夏秋</td><td>测试教师</td>
    <td><span class="cgreen">已获得学分 | 87.0</span></td>
  </tr>
  <tr>
    <td><span>必修</span></td><td>GR002</td><td>专业英语</td><td>专业课</td>
    <td>1.0</td><td>2018-2019</td><td>春</td><td>测试教师</td>
    <td><span class="cgreen">已获得学分 | 优秀</span></td>
  </tr>
  <tr>
    <td><span>选修</span></td><td>GR003</td><td>前沿讲座</td><td>选修课</td>
    <td>1.0</td><td>2018-2019</td><td>春</td><td>测试教师</td>
    <td><span>在修</span></td>
  </tr>
  <tr>
    <td><span>必修</span></td><td>GR004</td><td>海洋数据分析</td><td>专业课</td>
    <td>2.0</td><td>2019-2020</td><td></td><td>测试教师</td>
    <td><span class="cgreen">已获得学分 | 91.0</span></td>
  </tr>
</table>
</body></html>`

func TestParseGraduatePeriodsFromCoursePlan(t *testing.T) {
	t.Parallel()
	periods, err := parseGraduatePeriods([]byte(graduateCoursePlanFixture), "html")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 2 {
		t.Fatalf("period count=%d want=2", len(periods))
	}
	if periods[0].ID != "2018:12" ||
		periods[0].Label != "2018-2019学年 春" ||
		periods[0].WeekCount != graduateWeekCount ||
		periods[1].ID != "2017:11" {
		t.Fatalf("periods=%+v", periods)
	}
}

func TestParseGraduateGradesFromCoursePlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		periodID  string
		wantCount int
		check     func(*testing.T, []domain.Grade)
	}{
		{
			name:      "all periods",
			wantCount: 3,
			check: func(t *testing.T, grades []domain.Grade) {
				t.Helper()
				if grades[0].Score == nil || *grades[0].Score != 87 ||
					grades[0].PeriodID != "2017:11" {
					t.Fatalf("numeric grade=%+v", grades[0])
				}
				if grades[1].GradeLevel == nil ||
					*grades[1].GradeLevel != domain.GradeLevelExcellent ||
					grades[1].PeriodID != "2018:12" {
					t.Fatalf("level grade=%+v", grades[1])
				}
				if grades[2].Score == nil || *grades[2].Score != 91 ||
					grades[2].PeriodID != "2019-2020" {
					t.Fatalf("year-only grade=%+v", grades[2])
				}
			},
		},
		{
			name:      "one period",
			periodID:  "2018:12",
			wantCount: 1,
			check: func(t *testing.T, grades []domain.Grade) {
				t.Helper()
				if grades[0].CourseCode != "GR002" {
					t.Fatalf("grade=%+v", grades[0])
				}
			},
		},
		{
			name:      "academic year without term",
			periodID:  "2019-2020",
			wantCount: 1,
			check: func(t *testing.T, grades []domain.Grade) {
				t.Helper()
				if grades[0].CourseCode != "GR004" || grades[0].PeriodID != "2019-2020" {
					t.Fatalf("year-only grade=%+v", grades[0])
				}
			},
		},
		{
			name:      "period without grades",
			periodID:  "2026:11",
			wantCount: 0,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			grades, err := parseGraduateGrades(
				[]byte(graduateCoursePlanFixture),
				"html",
				test.periodID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(grades) != test.wantCount {
				t.Fatalf("grade count=%d want=%d", len(grades), test.wantCount)
			}
			if test.check != nil {
				test.check(t, grades)
			}
		})
	}
}

func TestParseGraduateSelectionsFromCoursePlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		periodID  string
		wantCount int
		check     func(*testing.T, []domain.CourseSelection)
	}{
		{
			name:      "all rows including released grades",
			wantCount: 3,
			check: func(t *testing.T, selections []domain.CourseSelection) {
				t.Helper()
				if selections[0].CourseCode != "GR001" ||
					selections[0].Teacher != "测试教师" ||
					selections[0].Status != domain.CourseSelectionSelected ||
					selections[0].ResultText == nil ||
					*selections[0].ResultText != "已获得学分 | 87.0" {
					t.Fatalf("released selection=%+v", selections[0])
				}
				if selections[2].CourseCode != "GR003" ||
					selections[2].Status != domain.CourseSelectionSelected ||
					selections[2].ResultText == nil ||
					*selections[2].ResultText != "在修" {
					t.Fatalf("studying selection=%+v", selections[2])
				}
			},
		},
		{
			name:      "one period",
			periodID:  "2018:12",
			wantCount: 2,
			check: func(t *testing.T, selections []domain.CourseSelection) {
				t.Helper()
				if selections[0].CourseCode != "GR002" ||
					selections[1].CourseCode != "GR003" {
					t.Fatalf("selections=%+v", selections)
				}
			},
		},
		{
			name:      "period without rows",
			periodID:  "2026:11",
			wantCount: 0,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selections, err := parseGraduateSelections(
				[]byte(graduateCoursePlanFixture),
				"html",
				test.periodID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(selections) != test.wantCount {
				t.Fatalf(
					"selection count=%d want=%d",
					len(selections),
					test.wantCount,
				)
			}
			if test.check != nil {
				test.check(t, selections)
			}
		})
	}
}

func TestParseGraduateSelectionsRejectsOversizedResultText(t *testing.T) {
	t.Parallel()
	fixture := strings.Replace(
		graduateCoursePlanFixture,
		"已获得学分 | 87.0",
		strings.Repeat("状", 501),
		1,
	)
	_, err := parseGraduateSelections([]byte(fixture), "html", "")
	if !errors.Is(err, application.ErrProviderUnavailable) {
		t.Fatalf("error=%v want=%v", err, application.ErrProviderUnavailable)
	}
}

func TestGraduateSelectionStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want domain.CourseSelectionStatus
	}{
		{name: "not selected", raw: "未选", want: domain.CourseSelectionFailed},
		{name: "changing selection", raw: "退换课", want: domain.CourseSelectionPending},
		{name: "exemption pending", raw: "正在申请免修 | 选课", want: domain.CourseSelectionPending},
		{name: "selecting", raw: "选课", want: domain.CourseSelectionSelected},
		{name: "studying", raw: "在修", want: domain.CourseSelectionSelected},
		{name: "released grade", raw: "已获得学分 | 87.0", want: domain.CourseSelectionSelected},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := graduateSelectionStatus(test.raw); got != test.want {
				t.Fatalf("status=%q want=%q", got, test.want)
			}
		})
	}
}

func TestGraduateGradeValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		raw       string
		wantOK    bool
		wantScore *float64
		wantLevel domain.GradeLevel
		wantType  domain.GradeType
	}{
		{
			name: "numeric after status delimiter", raw: "已获得学分 | 87.0",
			wantOK: true, wantScore: gradeScorePointer(87), wantType: domain.GradeTypeNumber,
		},
		{
			name: "known named grade", raw: "已获得学分 | 合格",
			wantOK: true, wantLevel: domain.GradeLevelPass, wantType: domain.GradeTypeLevel,
		},
		{
			name: "exempt grade", raw: "已批准免修",
			wantOK: true, wantLevel: domain.GradeLevelExempt, wantType: domain.GradeTypeLevel,
		},
		{
			name: "exempt before selection status", raw: "免修 | 选课",
			wantOK: true, wantLevel: domain.GradeLevelExempt, wantType: domain.GradeTypeLevel,
		},
		{
			name: "unlisted released grade", raw: "已获得学分 | A+",
			wantOK: true, wantLevel: domain.GradeLevel("A+"), wantType: domain.GradeTypeLevel,
		},
		{name: "not selected", raw: "未选"},
		{name: "selecting", raw: "选课"},
		{name: "changing selection", raw: "退换课"},
		{name: "exemption pending", raw: "正在申请免修"},
		{name: "exemption pending before selection status", raw: "正在申请免修 | 选课"},
		{name: "studying", raw: "正在修读"},
		{name: "legacy studying label", raw: "在修"},
		{name: "retaking", raw: "正在重修"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			score, gradeType, level, ok := graduateGradeValue(test.raw)
			if ok != test.wantOK {
				t.Fatalf("ok=%t want=%t", ok, test.wantOK)
			}
			if !test.wantOK {
				if score != nil || level != nil || gradeType != "" {
					t.Fatalf(
						"score=%v level=%v gradeType=%q",
						score,
						level,
						gradeType,
					)
				}
				return
			}
			if gradeType != test.wantType {
				t.Fatalf("gradeType=%q want=%q", gradeType, test.wantType)
			}
			if test.wantScore != nil {
				if score == nil || *score != *test.wantScore || level != nil {
					t.Fatalf("score=%v level=%v", score, level)
				}
				return
			}
			if level == nil || *level != test.wantLevel || score != nil {
				t.Fatalf("score=%v level=%v wantLevel=%q", score, level, test.wantLevel)
			}
		})
	}
}

func gradeScorePointer(value float64) *float64 {
	return &value
}

func TestParseGraduateScheduleGrid(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-bordered table-course">
		  <tr>
		    <th colspan="2">时间</th>
		    <th>星期一</th><th>星期二</th><th>星期三</th><th>星期四</th>
		    <th>星期五</th><th>星期六</th><th>星期日</th>
		  </tr>
		  <tr>
		    <td rowspan="2">上午</td><td>第1节</td><td></td>
		    <td rowspan="2">
		      <div>课程名称：研究方法</div>
		      <div>课程编号：GR001</div>
		      <div>任课教师：测试教师</div>
		      <div>上课地点：教学楼101</div>
		      <div>1-16周</div>
		    </td>
		    <td></td><td></td><td></td><td></td><td></td>
		  </tr>
		  <tr>
		    <td>第2节</td><td></td><td></td><td></td><td></td><td></td><td></td>
		  </tr>
		</table>`)
	courses, err := parseGraduateCourses(body, "html", "2026:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) != 1 {
		t.Fatalf("courses=%+v", courses)
	}
	course := courses[0]
	if course.Name != "研究方法" ||
		course.CourseCode != "GR001" ||
		course.Teacher != "测试教师" ||
		course.Location != "教学楼101" ||
		course.Weekday != 2 ||
		course.StartSection != 1 ||
		course.EndSection != 2 ||
		len(course.Weeks) != 16 {
		t.Fatalf("course=%+v", course)
	}
}

func TestParseGraduateScheduleGridFromLiveStructure(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-bordered table-course">
		  <tr>
		    <th colspan="2">时间</th>
		    <th>星期一</th><th>星期二</th><th>星期三</th><th>星期四</th>
		    <th>星期五</th><th>星期六</th><th>星期日</th>
		  </tr>
		  <tr>
		    <td rowspan="2">上午</td><td>第1节</td>
		    <td></td><td></td><td></td>
		    <td>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=LIVE-COURSE-1">
		        <strong class="f14">信息检索</strong><br>
		        &nbsp;||&nbsp;<span class="cpink">( 5-20 )周</span><br>
		        测试教师<br>测试楼101<br><br>
		      </a>
		    </td>
		    <td></td><td></td><td></td>
		  </tr>
		  <tr>
		    <td>第2节</td>
		    <td></td><td></td><td></td>
		    <td>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=LIVE-COURSE-1">
		        <strong class="f14">信息检索</strong><br>
		        &nbsp;||&nbsp;<span class="cpink">( 5-20 )周</span><br>
		        测试教师<br>测试楼101<br><br>
		      </a>
		    </td>
		    <td></td><td></td><td></td>
		  </tr>
		  <tr>
		    <td rowspan="2">下午</td><td>第3节</td>
		    <td></td><td></td><td></td><td></td>
		    <td>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=LIVE-COURSE-2">
		        <strong class="f14">学术道德与规范</strong><br>
		        &nbsp;||&nbsp;<span class="cpink">( 11-14 )周</span><br>
		        测试教师甲<br>测试楼201<br><br>
		      </a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=LIVE-COURSE-3">
		        <strong class="f14">学术论文写作</strong><br>
		        &nbsp;||&nbsp;<span class="cpink">( 3-10 )周</span><br>
		        测试教师乙<br>测试楼202<br><br>
		      </a>
		    </td>
		    <td></td><td></td>
		  </tr>
		  <tr>
		    <td>第4节</td>
		    <td></td><td></td><td></td><td></td>
		    <td>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=LIVE-COURSE-2">
		        <strong class="f14">学术道德与规范</strong><br>
		        &nbsp;||&nbsp;<span class="cpink">( 11-14 )周</span><br>
		        测试教师甲<br>测试楼201<br><br>
		      </a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=LIVE-COURSE-3">
		        <strong class="f14">学术论文写作</strong><br>
		        &nbsp;||&nbsp;<span class="cpink">( 3-10 )周</span><br>
		        测试教师乙<br>测试楼202<br><br>
		      </a>
		    </td>
		    <td></td><td></td>
		  </tr>
		</table>`)
	courses, err := parseGraduateCourses(body, "html", "2019:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) != 3 {
		t.Fatalf("courses=%+v", courses)
	}
	byName := make(map[string]domain.Course, len(courses))
	for _, course := range courses {
		byName[course.Name] = course
	}
	tests := []struct {
		name         string
		code         string
		teacher      string
		location     string
		weekday      int
		startSection int
		endSection   int
		firstWeek    int
		lastWeek     int
		weekCount    int
	}{
		{
			name: "信息检索", code: "LIVE-COURSE-1",
			teacher: "测试教师", location: "测试楼101",
			weekday: 4, startSection: 1, endSection: 2,
			firstWeek: 5, lastWeek: 20, weekCount: 16,
		},
		{
			name: "学术道德与规范", code: "LIVE-COURSE-2",
			teacher: "测试教师甲", location: "测试楼201",
			weekday: 5, startSection: 3, endSection: 4,
			firstWeek: 11, lastWeek: 14, weekCount: 4,
		},
		{
			name: "学术论文写作", code: "LIVE-COURSE-3",
			teacher: "测试教师乙", location: "测试楼202",
			weekday: 5, startSection: 3, endSection: 4,
			firstWeek: 3, lastWeek: 10, weekCount: 8,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			course, ok := byName[test.name]
			if !ok {
				t.Fatalf("course %q missing: %+v", test.name, courses)
			}
			if course.CourseCode != test.code ||
				course.Teacher != test.teacher ||
				course.Location != test.location ||
				course.Weekday != test.weekday ||
				course.StartSection != test.startSection ||
				course.EndSection != test.endSection ||
				len(course.Weeks) != test.weekCount ||
				course.Weeks[0] != test.firstWeek ||
				course.Weeks[len(course.Weeks)-1] != test.lastWeek {
				t.Fatalf("course=%+v", course)
			}
		})
	}
}

func TestParseGraduateScheduleRecognizesEmptyGrid(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-course">
		  <tr>
		    <th colspan="2">时间</th>
		    <th>星期一</th><th>星期二</th><th>星期三</th><th>星期四</th>
		    <th>星期五</th><th>星期六</th><th>星期日</th>
		  </tr>
		  <tr><td>上午</td><td>第1节</td><td></td><td></td><td></td><td></td><td></td><td></td><td></td></tr>
		</table>`)
	courses, err := parseGraduateCourses(body, "html", "2026:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) != 0 {
		t.Fatalf("courses=%+v", courses)
	}
}

func TestParseGraduateWeeksMatchesSelectionHistoryFragments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []int
	}{
		{name: "single week", text: "( 13 )周", want: []int{13}},
		{name: "range", text: "( 5-6 )周", want: []int{5, 6}},
		{name: "mixed range", text: "( 4,9-11,14 )周", want: []int{4, 9, 10, 11, 14}},
		{name: "unbracketed range", text: "1-17周 星期三", want: integerRange(1, 17)},
		{name: "odd weeks", text: "单周", want: []int{1, 3, 5, 7, 9, 11, 13, 15, 17, 19, 21, 23}},
		{name: "even weeks", text: "双周", want: []int{2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := parseGraduateWeeks(test.text); !sameIntegerValues(got, test.want) {
				t.Fatalf("parseGraduateWeeks(%q)=%v want=%v", test.text, got, test.want)
			}
		})
	}
}

func TestParseGraduateScheduleMergesSelectionHistoryWeekFragments(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-course">
		  <tr>
		    <th colspan="2">时间</th>
		    <th>星期一</th><th>星期二</th><th>星期三</th><th>星期四</th>
		    <th>星期五</th><th>星期六</th><th>星期日</th>
		  </tr>
		  <tr>
		    <td rowspan="2">上午</td><td>第1节</td><td></td>
		    <td>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 13 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 5-6 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 8 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 4,9-11,14 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 2-3,12 )周</span><br>测试教师<br>教学楼101<br><br></a>
		    </td>
		    <td></td><td></td><td></td><td></td><td></td>
		  </tr>
		  <tr>
		    <td>第2节</td><td></td>
		    <td>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 13 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 5-6 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 8 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 4,9-11,14 )周</span><br>测试教师<br>教学楼101<br><br></a>
		      <a class="c666" href="/py/page/student/xkkcxx.htm?kcId=GR-001"><strong class="f14">研究方法</strong><br>&nbsp;||&nbsp;<span>( 2-3,12 )周</span><br>测试教师<br>教学楼101<br><br></a>
		    </td>
		    <td></td><td></td><td></td><td></td><td></td>
		  </tr>
		</table>`)
	courses, err := parseGraduateCourses(body, "html", "2026:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) != 1 {
		t.Fatalf("courses=%+v", courses)
	}
	wantWeeks := []int{2, 3, 4, 5, 6, 8, 9, 10, 11, 12, 13, 14}
	course := courses[0]
	if course.CourseCode != "GR-001" ||
		course.Name != "研究方法" ||
		course.Teacher != "测试教师" ||
		course.Location != "教学楼101" ||
		course.Weekday != 2 ||
		course.StartSection != 1 ||
		course.EndSection != 2 ||
		!sameIntegerValues(course.Weeks, wantWeeks) {
		t.Fatalf("course=%+v", course)
	}
}

func TestParseGraduateCourseHistoryFiltersPeriodAndMergesEntries(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-bordered table-striped">
		  <thead><tr>
		    <th>开课学年</th><th>开课学期</th><th>班级编号</th><th>课程名称</th>
		    <th>是否重修/重考</th><th>容量</th><th>学分</th><th>任课教师</th>
		    <th>时间与地点</th><th>备注</th>
		  </tr></thead>
		  <tbody>
		    <tr>
		      <td>2026-2027</td><td>夏秋</td><td>040K0010001</td><td>研究方法</td>
		      <td>否</td><td>100</td><td>3.0</td><td>测试教师</td>
		      <td>(第13周)||星期一||第1-4节||(鱼山校区||鱼山楼群||教学楼101研)<br>(5-6周)||星期一||第1-4节||(鱼山校区||鱼山楼群||教学楼101研)</td><td>备注一</td>
		    </tr>
		    <tr>
		      <td>2025-2026</td><td>夏秋</td><td>040K0099001</td><td>历史课程</td>
		      <td>否</td><td>100</td><td>2.0</td><td>历史教师</td>
		      <td>(1-2周)||星期二||第3-4节||(崂山校区||崂山楼群||教学楼202)</td><td></td>
		    </tr>
		    <tr>
		      <td>2026-2027</td><td>春</td><td>040K0020001</td><td>春季课程</td>
		      <td>否</td><td>100</td><td>2.0</td><td>春季教师</td>
		      <td>(8-9周)||星期三||第5-6节||(鱼山校区||鱼山楼群||教学楼303)</td><td></td>
		    </tr>
		  </tbody>
		</table>`)
	courses, err := parseGraduateCourses(body, "html", "2026:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) != 1 {
		t.Fatalf("filtered courses=%+v", courses)
	}
	course := courses[0]
	if course.ID != "2026:11:1:1:040K0010001" ||
		course.CourseCode != "040K0010" ||
		course.Name != "研究方法" ||
		course.Teacher != "测试教师" ||
		course.Campus != "鱼山校区" ||
		course.Location != "教学楼101" ||
		course.Note != "备注一" ||
		course.Weekday != 1 ||
		course.StartSection != 1 ||
		course.EndSection != 4 ||
		!sameIntegerValues(course.Weeks, []int{5, 6, 13}) {
		t.Fatalf("course=%+v", course)
	}

	springCourses, err := parseGraduateCourses(body, "html", "2026:12")
	if err != nil {
		t.Fatal(err)
	}
	if len(springCourses) != 1 || springCourses[0].Name != "春季课程" {
		t.Fatalf("spring courses=%+v", springCourses)
	}
}

func TestParseGraduateExams(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-bordered">
		  <tr>
		    <th>课程编号</th><th>课程名称</th><th>考试时间</th>
		    <th>考试地点</th><th>座位号</th><th>考试方式</th><th>备注</th>
		  </tr>
		  <tr>
		    <td>GR001</td><td>研究方法</td><td>2026-06-20 09:00-11:00</td>
		    <td>教学楼101</td><td>12</td><td>闭卷</td><td>携带证件</td>
		  </tr>
		</table>`)
	exams, err := parseGraduateExams(body, "html", "2025:12")
	if err != nil {
		t.Fatal(err)
	}
	if len(exams) != 1 {
		t.Fatalf("exams=%+v", exams)
	}
	exam := exams[0]
	if exam.CourseCode != "GR001" ||
		exam.Location != "教学楼101" ||
		exam.Seat != "12" ||
		exam.StartAt.Hour() != 9 ||
		exam.EndAt.Hour() != 11 ||
		exam.Phase != domain.ExamPhaseFinal {
		t.Fatalf("exam=%+v", exam)
	}
}

func TestParseGraduateExamsFromLiveStructure(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="table table-bordered table-striped">
		  <thead>
		    <tr>
		      <th>学期</th><th>开课号</th><th>课程名称</th><th>校区</th>
		      <th>考试日期</th><th>考试时间</th><th>考试地点</th>
		      <th>座位号</th><th>备注</th>
		    </tr>
		  </thead>
		  <tbody>
		    <tr>
		      <td>夏秋</td><td>130K0001001</td><td>环境海洋学</td><td>崂山校区</td>
		      <td>2026-01-18</td><td>08:00->09:40</td>
		      <td>崂山4区 - 4101</td><td>18</td><td></td>
		    </tr>
		    <tr>
		      <td>夏秋</td><td>000K9002022</td><td>学术道德与规范</td><td>崂山校区</td>
		      <td>2026-01-17</td><td>08:00->09:00</td>
		      <td>崂山4区 - 4203</td><td>32</td><td></td>
		    </tr>
		    <tr>
		      <td>夏秋</td><td>000K9001015</td><td>学术论文写作</td><td>崂山校区</td>
		      <td>2026-01-17</td><td>09:30->11:10</td>
		      <td>崂山4区 - 4203</td><td>84</td><td></td>
		    </tr>
		  </tbody>
		</table>`)
	exams, err := parseGraduateExams(body, "html", "2025:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(exams) != 3 {
		t.Fatalf("exams=%+v", exams)
	}
	byCode := make(map[string]domain.Exam, len(exams))
	for _, exam := range exams {
		byCode[exam.CourseCode] = exam
	}
	tests := []struct {
		code     string
		name     string
		date     string
		start    string
		end      string
		location string
		seat     string
	}{
		{
			code: "130K0001001", name: "环境海洋学", date: "2026-01-18",
			start: "08:00", end: "09:40", location: "崂山4区 - 4101", seat: "18",
		},
		{
			code: "000K9002022", name: "学术道德与规范", date: "2026-01-17",
			start: "08:00", end: "09:00", location: "崂山4区 - 4203", seat: "32",
		},
		{
			code: "000K9001015", name: "学术论文写作", date: "2026-01-17",
			start: "09:30", end: "11:10", location: "崂山4区 - 4203", seat: "84",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.code, func(t *testing.T) {
			exam, ok := byCode[test.code]
			if !ok {
				t.Fatalf("exam %q missing: %+v", test.code, exams)
			}
			wantStart := mustGraduateExamTime(t, test.date, test.start)
			wantEnd := mustGraduateExamTime(t, test.date, test.end)
			if exam.CourseName != test.name ||
				exam.PeriodID != "2025:11" ||
				exam.Campus != "崂山校区" ||
				exam.Location != test.location ||
				exam.Seat != test.seat ||
				!exam.StartAt.Equal(wantStart) ||
				!exam.EndAt.Equal(wantEnd) ||
				!strings.Contains(exam.ID, test.code) {
				t.Fatalf("exam=%+v", exam)
			}
		})
	}
}

func mustGraduateExamTime(t *testing.T, date string, clock string) time.Time {
	t.Helper()
	result := graduateDateTime(date, clock)
	if result.IsZero() {
		t.Fatalf("invalid test time %s %s", date, clock)
	}
	return result
}

func TestParseGraduateExamsRecognizesEmptyPage(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<html><body>
		  <h3>2026-2027学年 夏秋 考试安排</h3>
		  <form><select name="xn"></select><select name="xj"></select></form>
		</body></html>`)
	exams, err := parseGraduateExams(body, "html", "2026:11")
	if err != nil {
		t.Fatal(err)
	}
	if len(exams) != 0 {
		t.Fatalf("exams=%+v", exams)
	}
}

func TestGraduatePeriodCurrentTermMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{
			name: "summer autumn",
			now:  time.Date(2026, time.July, 27, 0, 0, 0, 0, shanghaiLocation),
			want: "2026:11",
		},
		{
			name: "spring",
			now:  time.Date(2026, time.April, 1, 0, 0, 0, 0, shanghaiLocation),
			want: "2025:12",
		},
		{
			name: "winter remains autumn",
			now:  time.Date(2026, time.January, 10, 0, 0, 0, 0, shanghaiLocation),
			want: "2025:11",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := currentGraduatePeriodID(test.now); got != test.want {
				t.Fatalf("period ID=%q want=%q", got, test.want)
			}
		})
	}
}
