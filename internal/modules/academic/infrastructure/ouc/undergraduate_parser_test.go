package ouc

import (
	"strings"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
)

func TestParseUndergraduatePeriodsFromScheduleSelector(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<form>
			<select id="xnxq01id" name="xnxq01id">
				<option value="">请选择</option>
				<option value="2025-2026-3" selected>2026春季学期</option>
				<option value="2025-2026-2">2025秋季学期</option>
			</select>
		</form>`)
	periods, err := parseUndergraduatePeriods(body, "html")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 2 {
		t.Fatalf("periods=%+v", periods)
	}
	current := periods[0]
	if current.ID != "2025-2026-3" || !current.IsCurrent || current.WeekCount != 20 {
		t.Fatalf("current period=%+v", current)
	}
	if got := current.StartDate.Format("2006-01-02"); got != "2026-03-02" {
		t.Fatalf("start date=%q", got)
	}
}

func TestParseUndergraduateScheduleTooltip(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="qz-weeklyTable">
			<tr><th class="qz-weeklyTable-label">周次</th><th>星期一</th><th>星期二</th></tr>
			<tr>
				<td class="qz-weeklyTable-label">第一大节 (01、02小节)</td>
				<td class="qz-weeklyTable-td"></td>
				<td class="qz-weeklyTable-td qz-hasCourse">
					<div class="qz-tooltip">
						<ul class="qz-toolitiplists">
							<li class="qz-toolitiplists">
								<div class="qz-tooltipContent-title">数据结构</div>
								<div class="qz-tooltipContent-detaillists">
									<div class="qz-tooltipContent-detailitem">选课号：FAKE-CLASS-1</div>
									<div class="qz-tooltipContent-detailitem" name="kchDiv">课程号：FAKE1001</div>
									<div class="qz-tooltipContent-detailitem">老师：测试教师</div>
									<div class="qz-tooltipContent-detailitem"><span>时间：1-4,6-8周[1-2节]</span></div>
									<div class="qz-tooltipContent-detailitem">地点：测试楼101</div>
								</div>
							</li>
						</ul>
					</div>
				</td>
			</tr>
			<tr><td class="qz-weeklyTable-label">备注</td><td colspan="2"></td></tr>
		</table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2025-2026-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 1 {
		t.Fatalf("schedule=%+v", schedule)
	}
	course := schedule.Courses[0]
	if !strings.HasPrefix(course.ID, "course-") ||
		course.PeriodID != "2025-2026-3" ||
		course.CourseCode != "FAKE1001" || course.ClassNum != "FAKE-CLASS-1" ||
		course.Weekday != 2 ||
		course.StartSection != 1 ||
		course.EndSection != 2 ||
		len(course.Weeks) != 7 ||
		course.Weeks[4] != 6 {
		t.Fatalf("course=%+v", course)
	}
}

func TestParseUndergraduateScheduleSupportsSeparatedFieldsAndNote(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<div class="qz-schedule-items">
			<li class="qz-toolitiplists">
				<div class="qz-tooltipContent-title qz-ellipse">高等数学</div>
				<div class="qz-tooltipContent-detailitem">选课号：MATH-1</div>
				<div class="qz-tooltipContent-detailitem">课程号：MATH1001</div>
				<div class="qz-tooltipContent-detailitem">教师：张老师</div>
				<div class="qz-tooltipContent-detailitem">上课地点：</div>
				<div class="qz-tooltipContent-detailitem">周次：1-8周（单）,10-14周（双） 星期四</div>
				<div class="qz-tooltipContent-detailitem">节次：7~9节</div>
				<div class="qz-tooltipContent-detailitem">学分：4</div>
				<div class="qz-tooltipContent-detailitem">课表备注：线上教学</div>
			</li>
		</div>
		<table class="qz-weeklyTable">
			<tfoot class="qz-weeklyTable-tfoot">
				<tr><td class="qz-weeklyTable-label">备注</td><td><span class="qz-weeklyTable-detailtext">请按教学安排参加课程。
  期末另行通知</span></td></tr>
			</tfoot>
		</table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2025-2026-3")
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ScheduleNote != "请按教学安排参加课程。 期末另行通知" {
		t.Fatalf("schedule note=%q", schedule.ScheduleNote)
	}
	if len(schedule.Courses) != 1 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	course := schedule.Courses[0]
	if !strings.HasPrefix(course.ID, "course-") || course.CourseCode != "MATH1001" || course.ClassNum != "MATH-1" || course.Note != "线上教学" ||
		course.Teacher != "张老师" || course.Location != "线上教学" ||
		course.Weekday != 4 || course.StartSection != 7 || course.EndSection != 9 {
		t.Fatalf("course=%+v", course)
	}
	if want := []int{1, 3, 5, 7, 10, 12, 14}; !equalInts(course.Weeks, want) {
		t.Fatalf("weeks=%v want=%v", course.Weeks, want)
	}
}

func TestParseUndergraduateScheduleTreatsEmptyLocationPlaceholderAsEmpty(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="qz-weeklyTable">
			<tr>
				<td class="qz-weeklyTable-label">第一大节</td>
				<td class="qz-hasCourse"><li class="qz-toolitiplists">
				<div class="qz-tooltipContent-title">人工智能综合实践</div>
				<div class="qz-tooltipContent-detailitem">选课号：26152029</div>
				<div class="qz-tooltipContent-detailitem">老师：王胜科</div>
				<div class="qz-tooltipContent-detailitem">时间：1-4周[1-2节]</div>
				<div class="qz-tooltipContent-detailitem">地点：()</div>
				<div class="qz-tooltipContent-detailitem">课表备注：上课实验室：计算机楼B123</div>
				</li></td>
			</tr>
		</table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2026-2027-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 1 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	course := schedule.Courses[0]
	if course.Location != "上课实验室：计算机楼B123" || course.Note != "上课实验室：计算机楼B123" {
		t.Fatalf("course location/note=%q/%q", course.Location, course.Note)
	}
}

func TestParseUndergraduateScheduleStoresSelectionIDAsClassNum(t *testing.T) {
	t.Parallel()
	body := []byte(`<table class="qz-weeklyTable"><tr>` +
		`<td class="qz-weeklyTable-label">第二大节</td>` +
		`<td class="qz-hasCourse"><li class="qz-toolitiplists">` +
		`<div class="qz-tooltipContent-title qz-ellipse">理解中国：视野与方法</div>` +
		`<div class="qz-tooltipContent-detaillists">` +
		`<div class="qz-tooltipContent-detailitem">选课号：25214119</div>` +
		`<div class="qz-tooltipContent-detailitem">教师：聂友军</div>` +
		`<div class="qz-tooltipContent-detailitem">上课地点：教学楼7区7507（研讨）</div>` +
		`<div class="qz-tooltipContent-detailitem">周次：[3周]星期日</div>` +
		`<div class="qz-tooltipContent-detailitem">节次：3~4节</div>` +
		`</div></li></td></tr></table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2025-2026-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 1 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	course := schedule.Courses[0]
	if !strings.HasPrefix(course.ID, "course-") || course.CourseCode != "" || course.ClassNum != "25214119" ||
		course.Weekday != 7 || course.StartSection != 3 || course.EndSection != 4 {
		t.Fatalf("course=%+v", course)
	}
}

func TestParseUndergraduateCourseSelectionScheduleKeepsEveryMeetingForClassNum(t *testing.T) {
	t.Parallel()
	body := []byte(`<table id="tbData"><thead><tr>
		<th><div>选课号</div></th><th><div>课程号</div></th><th><div>课程名称</div></th><th><div>上课教师</div></th><th><div>上课时间</div></th><th><div>上课地点</div></th>
	</tr></thead><tbody><tr>
		<td>001234</td><td>CS1001</td><td>数据结构</td><td>张老师</td><td>1-8周 星期一 1-2节; 1-8周 星期三 3-4节</td><td>A101;B202</td>
	</tr></tbody></table>`)
	schedule, err := parseUndergraduateCourseSelectionSchedule(body, "html", "2026-2027-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 2 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	first, second := schedule.Courses[0], schedule.Courses[1]
	if first.ClassNum != "001234" || second.ClassNum != "001234" || first.CourseCode != "CS1001" || second.CourseCode != "CS1001" {
		t.Fatalf("class numbers=%+v", schedule.Courses)
	}
	if first.Weekday != 1 || first.StartSection != 1 || first.EndSection != 2 || first.Location != "A101" || second.Weekday != 3 || second.StartSection != 3 || second.EndSection != 4 || second.Location != "B202" {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
}

func TestParseUndergraduateCourseSelectionScheduleMatchesOUCPageLayout(t *testing.T) {
	t.Parallel()
	body := []byte(`<table class="layui-table" id="tbData"><thead><tr>
		<th><div>选课号</div></th><th><div>课程编号</div></th><th><div>课程名称</div></th><th><div>学分</div></th><th><div>上课教师</div></th><th><div>上课时间</div></th><th><div>上课地点</div></th><th><div>课表备注</div></th><th><div>上课校区</div></th><th><div>选课状态</div></th>
	</tr></thead><tbody>
		<tr><td><div>26209008</div></td><td><div>1109250001</div></td><td><div>工程制图基础[分组03]</div></td><td><div>2</div></td><td><div>姚阳</div></td><td><div>1-17周 星期三 5-6 </div></td><td><div>2703</div></td><td><div>&nbsp;</div></td><td><div>崂山校区</div></td><td><div>选中</div></td></tr>
		<tr><td><div>26251067</div></td><td><div>071502101329</div></td><td><div>电子信息学科概论</div></td><td><div>1</div></td><td><div>顾肇瑞,郭宗辉</div></td><td><div>1-8周 星期二 5-6 </div></td><td><div>4202</div></td><td><div>&nbsp;</div></td><td><div>崂山校区</div></td><td><div>选中</div></td></tr>
		<tr><td><div>26251072</div></td><td><div>071502101213</div></td><td><div>高级语言程序设计[分组04]</div></td><td><div>3</div></td><td><div>李林</div></td><td><div>1-16周 星期一 7-8 <br>1-16周 星期二 1-2 </div></td><td><div>4504<br>4504</div></td><td><div>&nbsp;</div></td><td><div>崂山校区</div></td><td><div>选中</div></td></tr>
		<tr><td><div>26216105</div></td><td><div>008401101055</div></td><td><div>高等数学Ⅱ1[分组01]</div></td><td><div>6</div></td><td><div>高振</div></td><td><div>1-17周 星期一 3-4 <br>1-17周 星期二 3-4 <br>1-17周 星期四 3-4 </div></td><td><div>4202<br>4101<br>6318</div></td><td><div>&nbsp;</div></td><td><div>崂山校区</div></td><td><div>选中</div></td></tr>
	</tbody></table>`)
	schedule, err := parseUndergraduateCourseSelectionSchedule(body, "html", "2026-2027-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 7 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	first := schedule.Courses[0]
	if first.ClassNum != "26209008" || first.CourseCode != "1109250001" || first.Name != "工程制图基础[分组03]" || first.Teacher != "姚阳" || first.Campus != "崂山校区" || first.Location != "2703" || first.Weekday != 3 || first.StartSection != 5 || first.EndSection != 6 || len(first.Weeks) != 17 {
		t.Fatalf("first=%+v", first)
	}
	if schedule.Courses[2].Weekday != 1 || schedule.Courses[2].StartSection != 7 || schedule.Courses[2].EndSection != 8 || schedule.Courses[2].Location != "4504" || schedule.Courses[3].Weekday != 2 || schedule.Courses[3].StartSection != 1 || schedule.Courses[3].EndSection != 2 || schedule.Courses[3].Location != "4504" {
		t.Fatalf("multi-meeting course=%+v/%+v", schedule.Courses[2], schedule.Courses[3])
	}
}

func TestParseUndergraduateScheduleFallsBackToTableWeekdayAndMergesWeeks(t *testing.T) {
	t.Parallel()
	body := []byte(`
		<table class="qz-weeklyTable">
			<tr><th class="qz-weeklyTable-label">周次</th><th>星期一</th><th>星期二</th></tr>
			<tr>
				<td class="qz-weeklyTable-label">第一大节</td><td></td><td class="qz-hasCourse">
					<li class="qz-toolitiplists"><div class="qz-tooltipContent-title">英语</div><div class="qz-tooltipContent-detaillists">
						<div class="qz-tooltipContent-detailitem">选课号：EN-1</div><div class="qz-tooltipContent-detailitem">老师：李老师</div>
						<div class="qz-tooltipContent-detailitem">时间：1-4周[1-2节]</div><div class="qz-tooltipContent-detailitem">地点：教室101</div>
					</div></li>
					<li class="qz-toolitiplists"><div class="qz-tooltipContent-title">英语</div><div class="qz-tooltipContent-detaillists">
						<div class="qz-tooltipContent-detailitem">选课号：EN-1</div><div class="qz-tooltipContent-detailitem">老师：李老师</div>
						<div class="qz-tooltipContent-detailitem">时间：6-8周[1-2节]</div><div class="qz-tooltipContent-detailitem">地点：教室101</div>
					</div></li>
				</td>
			</tr>
		</table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2025-2026-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 1 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	if schedule.ScheduleNote != "" {
		t.Fatalf("schedule note=%q, want empty", schedule.ScheduleNote)
	}
	course := schedule.Courses[0]
	if course.Weekday != 2 || !equalInts(course.Weeks, []int{1, 2, 3, 4, 6, 7, 8}) {
		t.Fatalf("course=%+v", course)
	}
}

func TestParseUndergraduateScheduleDoesNotExposeUnlabeledFooterText(t *testing.T) {
	t.Parallel()
	body := []byte(`<table class="qz-weeklyTable"><tfoot class="qz-weeklyTable-tfoot"><tr>` +
		`<td class="qz-weeklyTable-label">内部信息</td>` +
		`<td class="qz-weeklyTable-detailtext">不应作为课表备注返回</td>` +
		`</tr></tfoot></table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2025-2026-3")
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ScheduleNote != "" {
		t.Fatalf("schedule note=%q, want empty", schedule.ScheduleNote)
	}
}

func TestParseUndergraduateScheduleDerivesDistinctIDsAfterWeekdayFallback(t *testing.T) {
	t.Parallel()
	body := []byte(`<table class="qz-weeklyTable"><tr>` +
		`<th class="qz-weeklyTable-label">周次</th><th>星期一</th><th>星期二</th>` +
		`</tr><tr><td class="qz-weeklyTable-label">第一大节</td>` +
		`<td class="qz-hasCourse"><li class="qz-toolitiplists">` +
		`<div class="qz-tooltipContent-title">大学英语</div>` +
		`<div class="qz-tooltipContent-detailitem">老师：李老师</div>` +
		`<div class="qz-tooltipContent-detailitem">时间：1-4周[1-2节]</div>` +
		`<div class="qz-tooltipContent-detailitem">地点：教学楼101</div></li></td>` +
		`<td class="qz-hasCourse"><li class="qz-toolitiplists">` +
		`<div class="qz-tooltipContent-title">大学英语</div>` +
		`<div class="qz-tooltipContent-detailitem">老师：李老师</div>` +
		`<div class="qz-tooltipContent-detailitem">时间：1-4周[1-2节]</div>` +
		`<div class="qz-tooltipContent-detailitem">地点：教学楼101</div></li></td>` +
		`</tr></table>`)
	schedule, err := parseUndergraduateCourses(body, "html", "2025-2026-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule.Courses) != 2 {
		t.Fatalf("courses=%+v", schedule.Courses)
	}
	if schedule.Courses[0].Weekday == schedule.Courses[1].Weekday ||
		schedule.Courses[0].ID == schedule.Courses[1].ID {
		t.Fatalf("courses must have distinct weekdays and IDs: %+v", schedule.Courses)
	}
}

func TestParseScheduleWeeksAppliesParityPerGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  []int
	}{
		{name: "odd and even groups", value: "1-8周(单),10-14周(双)星期四", want: []int{1, 3, 5, 7, 10, 12, 14}},
		{name: "prefixed odd weeks", value: "单周1-8周星期二", want: []int{1, 3, 5, 7}},
		{name: "prefixed even weeks", value: "双周2-8周星期三", want: []int{2, 4, 6, 8}},
		{name: "discrete Chinese separators", value: "1、3、5周 星期一", want: []int{1, 3, 5}},
		{name: "Chinese range", value: "8至10周 星期二", want: []int{8, 9, 10}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseScheduleWeeks(test.value); !equalInts(got, test.want) {
				t.Fatalf("weeks=%v want=%v", got, test.want)
			}
		})
	}
}

func TestMergeUndergraduateCoursesKeepsDistinctIdentities(t *testing.T) {
	t.Parallel()
	base := domain.Course{
		PeriodID: "2025-2026-3", CourseCode: "MATH1001", Name: "高等数学",
		Teacher: "张老师", Campus: "崂山校区", Location: "教学楼101",
		Weekday: 2, StartSection: 1, EndSection: 2, Weeks: []int{1, 3},
	}
	tests := []struct {
		name   string
		mutate func(*domain.Course)
	}{
		{name: "selection id", mutate: func(course *domain.Course) { course.ID = "selection-2" }},
		{name: "course code", mutate: func(course *domain.Course) { course.CourseCode = "MATH1002" }},
		{name: "campus", mutate: func(course *domain.Course) { course.Campus = "鱼山校区" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := base
			first.ID = "selection-1"
			second := first
			second.Weeks = []int{2, 4}
			test.mutate(&second)
			if got := mergeUndergraduateCourses([]domain.Course{first, second}); len(got) != 2 {
				t.Fatalf("merged courses=%+v, want two distinct rows", got)
			}
		})
	}
}

func TestMergeUndergraduateCoursesMergesAdjacentSectionsAfterWeekUnion(t *testing.T) {
	t.Parallel()
	base := domain.Course{
		PeriodID: "2025-2026-3", CourseCode: "PHYS1001", Name: "大学物理",
		Teacher: "王老师", Campus: "崂山校区", Location: "教学楼201",
		Weekday: 3,
	}
	first := base
	first.StartSection, first.EndSection = 5, 6
	first.Weeks = []int{3, 1, 3}
	first.ID = derivedUndergraduateCourseID(first)
	second := first
	second.Weeks = []int{2, 1}
	third := base
	third.StartSection, third.EndSection = 7, 7
	third.Weeks = []int{1, 2, 3}
	third.ID = derivedUndergraduateCourseID(third)

	got := mergeUndergraduateCourses([]domain.Course{first, second, third})
	if len(got) != 1 {
		t.Fatalf("merged courses=%+v, want one row", got)
	}
	course := got[0]
	if course.StartSection != 5 || course.EndSection != 7 ||
		!equalInts(course.Weeks, []int{1, 2, 3}) {
		t.Fatalf("course=%+v, want 5-7 with weeks [1 2 3]", course)
	}
	if wantID := derivedUndergraduateCourseID(course); course.ID != wantID {
		t.Fatalf("course ID=%q want final-placement ID %q", course.ID, wantID)
	}
}

func TestMergeUndergraduateCoursesKeepsAdjacentSectionsWithDifferentWeeksSeparate(t *testing.T) {
	t.Parallel()
	base := domain.Course{
		PeriodID: "2025-2026-3", CourseCode: "CHEM1001", Name: "大学化学",
		Teacher: "李老师", Campus: "崂山校区", Location: "实验楼101", Weekday: 4,
	}
	first := base
	first.StartSection, first.EndSection, first.Weeks = 5, 6, []int{1, 2}
	first.ID = derivedUndergraduateCourseID(first)
	second := base
	second.StartSection, second.EndSection, second.Weeks = 7, 7, []int{3, 4}
	second.ID = derivedUndergraduateCourseID(second)

	got := mergeUndergraduateCourses([]domain.Course{first, second})
	if len(got) != 2 {
		t.Fatalf("merged courses=%+v, want two rows", got)
	}
}

func TestMergeUndergraduateCoursesKeepsNonAdjacentSectionsSeparate(t *testing.T) {
	t.Parallel()
	base := domain.Course{
		PeriodID: "2025-2026-3", CourseCode: "ENGL1001", Name: "大学英语",
		Teacher: "赵老师", Campus: "崂山校区", Location: "教学楼301", Weekday: 5,
	}
	first := base
	first.StartSection, first.EndSection, first.Weeks = 1, 4, []int{1, 2, 3}
	first.ID = derivedUndergraduateCourseID(first)
	second := base
	second.StartSection, second.EndSection, second.Weeks = 7, 8, []int{1, 2, 3}
	second.ID = derivedUndergraduateCourseID(second)

	got := mergeUndergraduateCourses([]domain.Course{first, second})
	if len(got) != 2 {
		t.Fatalf("merged courses=%+v, want two rows", got)
	}
	if got[0].StartSection != 1 || got[0].EndSection != 4 ||
		got[1].StartSection != 7 || got[1].EndSection != 8 {
		t.Fatalf("merged courses=%+v, want 1-4 and 7-8", got)
	}
}

func TestMergeUndergraduateCoursesKeepsAdjacentSectionsWithDifferentIdentitiesSeparate(t *testing.T) {
	t.Parallel()
	base := domain.Course{
		PeriodID: "2025-2026-3", CourseCode: "MATH1001", Name: "高等数学",
		Teacher: "张老师", Campus: "崂山校区", Location: "教学楼101", Note: "线下",
		Weekday: 2, Weeks: []int{1, 2, 3},
	}
	tests := []struct {
		name   string
		mutate func(*domain.Course)
	}{
		{name: "course code", mutate: func(course *domain.Course) { course.CourseCode = "MATH1002" }},
		{name: "teacher", mutate: func(course *domain.Course) { course.Teacher = "李老师" }},
		{name: "location", mutate: func(course *domain.Course) { course.Location = "教学楼202" }},
		{name: "note", mutate: func(course *domain.Course) { course.Note = "线上" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := base
			first.StartSection, first.EndSection = 5, 6
			first.ID = derivedUndergraduateCourseID(first)
			second := base
			second.StartSection, second.EndSection = 7, 7
			test.mutate(&second)
			second.ID = derivedUndergraduateCourseID(second)

			if got := mergeUndergraduateCourses([]domain.Course{first, second}); len(got) != 2 {
				t.Fatalf("merged courses=%+v, want two rows", got)
			}
		})
	}
}

func TestMergeUndergraduateCoursesKeepsExplicitIDsAsIdentityBoundaries(t *testing.T) {
	t.Parallel()
	base := domain.Course{
		PeriodID: "2025-2026-3", CourseCode: "PHYS1001", Name: "大学物理",
		Teacher: "王老师", Campus: "崂山校区", Location: "教学楼201",
		Weekday: 3, Weeks: []int{1, 2, 3},
	}
	first := base
	first.StartSection, first.EndSection, first.ID = 5, 6, "selection-1"
	second := base
	second.StartSection, second.EndSection, second.ID = 7, 7, "selection-2"

	if got := mergeUndergraduateCourses([]domain.Course{first, second}); len(got) != 2 {
		t.Fatalf("merged courses=%+v, want two rows", got)
	}
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestParseUndergraduateLayuiGrades(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"code":0,
		"msg":"",
		"count":4,
		"data":[
			{
				"cj0708id":"FAKE-GRADE-1",
				"xqstr":"2025-2026-3",
				"xnxqid":"2026春季学期",
				"kch":"FAKE1001",
				"kc_mc":"数据结构",
				"kcxzmc":"专业必修",
				"xf":3.5,
				"zcj":88,
				"zcjstr":"88"
			},
			{
				"cj0708id":"FAKE-GRADE-2",
				"xnxqid":"2025-2026-3",
				"kch":"FAKE1002",
				"kc_mc":"劳动教育",
				"kccm":"通识课程",
				"xf":1,
				"zcj":0,
				"zcjstr":"合格"
			},
			{
				"cj0708id":"FAKE-GRADE-3",
				"xnxqid":"2025-2026-3",
				"kch":"FAKE1003",
				"kc_mc":"大学英语",
				"kccm":"通识课程",
				"xf":2,
				"zcj":0,
				"zcjstr":"A+"
			},
			{
				"cj0708id":"FAKE-GRADE-1",
				"xqstr":"2025-2026-3",
				"kch":"FAKE1001",
				"kc_mc":"数据结构（重复记录）",
				"kcxzmc":"专业必修",
				"xf":3.5,
				"zcj":90,
				"zcjstr":"90"
			}
		]
	}`)
	grades, err := parseGrades(body, "json", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if len(grades) != 3 {
		t.Fatalf("grades=%+v", grades)
	}
	if grades[0].ID != "FAKE-GRADE-1" ||
		grades[0].PeriodID != "2025-2026-3" ||
		grades[0].Score == nil ||
		*grades[0].Score != 88 ||
		grades[0].GradeType != domain.GradeTypeNumber {
		t.Fatalf("numeric grade=%+v", grades[0])
	}
	if grades[1].GradeLevel == nil ||
		*grades[1].GradeLevel != domain.GradeLevel("合格") ||
		grades[1].Score == nil ||
		*grades[1].Score != 60 ||
		grades[1].CourseType != "通识课程" {
		t.Fatalf("level grade=%+v", grades[1])
	}
	if grades[2].GradeLevel == nil ||
		*grades[2].GradeLevel != domain.GradeLevel("A+") ||
		grades[2].Score == nil ||
		*grades[2].Score != 0 {
		t.Fatalf("raw level grade=%+v", grades[2])
	}
}

func TestConvertUndergraduateScore(t *testing.T) {
	t.Parallel()
	tests := map[string]float64{
		"优秀":  90,
		"优":   90,
		"免修":  90,
		"通过":  85,
		"良":   80,
		"良好":  80,
		"中":   70,
		"中等":  70,
		"合格":  60,
		"及格":  60,
		"不合格": 0,
		"不及格": 0,
		"A+":  0,
	}
	for raw, expected := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if actual := convertUndergraduateScore(raw); actual != expected {
				t.Fatalf("convertUndergraduateScore(%q)=%v, want %v", raw, actual, expected)
			}
		})
	}
}

func TestParseUndergraduateLayuiExams(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"code":0,
		"msg":"",
		"count":1,
		"data":[{
			"kw0410id":"FAKE-EXAM-1",
			"kch":"FAKE1001",
			"kskcmc":"数据结构",
			"kssj":"2026-06-18 09:00~11:00",
			"ksxq":"测试校区",
			"js_mc":"测试楼101",
			"zwh":"08",
			"bzywmc":"请提前入场"
		}]
	}`)
	exams, err := parseExams(body, "json", "2025-2026-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(exams) != 1 {
		t.Fatalf("exams=%+v", exams)
	}
	exam := exams[0]
	if exam.ID != "FAKE-EXAM-1" ||
		exam.CourseName != "数据结构" ||
		exam.Location != "测试楼101" ||
		exam.Seat != "08" ||
		exam.StartAt.Format(time.RFC3339) != "2026-06-18T09:00:00+08:00" ||
		exam.EndAt.Format(time.RFC3339) != "2026-06-18T11:00:00+08:00" {
		t.Fatalf("exam=%+v", exam)
	}
}

func TestParseUndergraduateLayuiCourseSelections(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"code":0,
		"msg":"",
		"count":3,
		"data":[
			{
				"jx02id":"FAKE-SELECTION-1",
				"xnxqid":"2025-2026-3",
				"kch":"FAKE1001",
				"kc_mc":"数据结构",
				"xm":"测试教师",
				"xf":3.5,
				"kcxz_mc":"专业必修",
				"kclb_mc":"专业课程",
				"sksj":"星期二 0102节",
				"skdd":"测试楼101",
				"xkzt":"已选"
			},
			{
				"jx02id":"FAKE-SELECTION-2",
				"kch":"FAKE1002",
				"kc_mc":"课程设计",
				"kclb_mc":"实践课程",
				"xkzt":"待确认"
			},
			{
				"jx02id":"FAKE-SELECTION-3",
				"kch":"FAKE1003",
				"kc_mc":"通识课程",
				"kclb_mc":"通识选修",
				"xkzt":"落选"
			}
		]
	}`)
	selections, err := parseSelections(body, "json", "fallback-period")
	if err != nil {
		t.Fatal(err)
	}
	if len(selections) != 3 {
		t.Fatalf("selections=%+v", selections)
	}
	selected := selections[0]
	if selected.ID != "FAKE-SELECTION-1" ||
		selected.PeriodID != "2025-2026-3" ||
		selected.CourseName != "数据结构" ||
		selected.CourseType != "专业必修" ||
		selected.Teacher != "测试教师" ||
		selected.Location != "测试楼101" ||
		selected.Schedule != "星期二 0102节" ||
		selected.Status != domain.CourseSelectionSelected ||
		selected.SelectedAt != nil {
		t.Fatalf("selected=%+v", selected)
	}
	if selections[1].PeriodID != "fallback-period" ||
		selections[1].CourseType != "实践课程" ||
		selections[1].Status != domain.CourseSelectionPending {
		t.Fatalf("pending=%+v", selections[1])
	}
	if selections[2].Status != domain.CourseSelectionFailed {
		t.Fatalf("failed=%+v", selections[2])
	}
}

func TestParseScheduleTimeSupportsOddWeeks(t *testing.T) {
	t.Parallel()
	weeks, start, end := parseScheduleTime("1-8周(单)[7-9节]")
	if start != 7 || end != 9 {
		t.Fatalf("sections=%d-%d", start, end)
	}
	if len(weeks) != 4 || weeks[0] != 1 || weeks[3] != 7 {
		t.Fatalf("weeks=%v", weeks)
	}
}

func TestParseScheduleTimeSupportsListedAndRangedWeeksWithoutSectionSuffix(t *testing.T) {
	t.Parallel()
	weeks, start, end := parseScheduleTime("4,5,6,10-12,13-15周 星期三 5-6")
	if start != 5 || end != 6 {
		t.Fatalf("sections=%d-%d", start, end)
	}
	want := []int{4, 5, 6, 10, 11, 12, 13, 14, 15}
	if !equalInts(weeks, want) {
		t.Fatalf("weeks=%v want=%v", weeks, want)
	}
}
