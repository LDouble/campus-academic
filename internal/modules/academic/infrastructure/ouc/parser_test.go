package ouc

import (
	"testing"

	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
)

func TestParseUndergraduateJSONAliases(t *testing.T) {
	t.Parallel()
	courses, err := parseCourses([]byte(`{"data":{"rows":[{
		"kch":"OUC1001","kcmc":"海洋科学导论","jsxm":"张老师","xqmc":"崂山校区",
		"jsmc":"教学楼 6101","xqj":2,"ksjc":1,"jsjc":2,"zcd":[1,2,3]
	}]}}`), "json", "2026-1")
	if err != nil || len(courses) != 1 {
		t.Fatalf("courses=%+v err=%v", courses, err)
	}
	if courses[0].CourseCode != "OUC1001" ||
		courses[0].Weekday != 2 ||
		len(courses[0].Weeks) != 3 {
		t.Fatalf("course=%+v", courses[0])
	}
}

func TestParseGraduateHTMLGradeTable(t *testing.T) {
	t.Parallel()
	grades, err := parseGrades([]byte(`
		<table>
			<tr><th>课程号</th><th>课程名称</th><th>学分</th><th>成绩</th></tr>
			<tr><td>G001</td><td>学术规范</td><td>1</td><td>92</td></tr>
		</table>`), "html", "2026-1")
	if err != nil || len(grades) != 1 {
		t.Fatalf("grades=%+v err=%v", grades, err)
	}
	if grades[0].GradeType != domain.GradeTypeNumber ||
		grades[0].Score == nil ||
		*grades[0].Score != 92 {
		t.Fatalf("grade=%+v", grades[0])
	}
}

func TestExtractRealName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "portal json",
			body: `{"realName":"海大同学"}`,
			want: "海大同学",
		},
		{
			name: "graduate home user node",
			body: `<a class="user name" href="/allogene/page/home.htm">
				测试同学
			</a>`,
			want: "测试同学",
		},
		{
			name: "class order is irrelevant",
			body: `<a class="name active user">海大同学</a>`,
			want: "海大同学",
		},
		{
			name: "undergraduate home profile node",
			body: `<div class="infoContentTitle qz-ellipse">
					测试同学-20260000001
				</div>`,
			want: "测试同学",
		},
		{
			name: "undergraduate class order and full width separator",
			body: `<div class="qz-ellipse active infoContentTitle">
					海大同学－13020031080
				</div>`,
			want: "海大同学",
		},
		{
			name: "undergraduate new home profile",
			body: `<div class="right-person">
					<span><iconpark-icon name="touxiang"></iconpark-icon></span>
					<p>测试同学</p>
					<iconpark-icon name="xiala" size="8"></iconpark-icon>
					<ul class="right-person-hover">
						<li><span>修改密码</span></li>
						<li><span>注销登录</span></li>
					</ul>
				</div>`,
			want: "测试同学",
		},
		{
			name: "nested right person paragraph is ignored",
			body: `<div class="right-person">
					<section><p>普通文本</p></section>
				</div>`,
		},
		{
			name: "conflicting profile nodes are rejected",
			body: `<a class="user name">测试同学</a>
				<a class="name user">另一同学</a>`,
		},
		{
			name: "undergraduate node without student number is ignored",
			body: `<div class="infoContentTitle qz-ellipse">普通栏目标题</div>`,
		},
		{
			name: "unrelated user class is ignored",
			body: `<a class="user">测试同学</a>`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := extractRealName([]byte(test.body)); got != test.want {
				t.Fatalf("real name=%q want=%q", got, test.want)
			}
		})
	}
}

func TestCollectRealNameContractHintsExcludesValuesAndQueries(t *testing.T) {
	t.Parallel()
	hints := collectRealNameContractHints([]byte(`
		<script>
			window.currentUser = {"user_name":"测试同学","department":"海洋学院"};
		</script>
		<script src="/allogene/assets/index.js?ticket=must-not-leak"></script>
		<script src="https://outside.example.com/ignored.js"></script>
		<a class="student-profile" href="/allogene/page/studentInfo.htm?id=private">
			个人资料
		</a>
	`))
	if len(hints.CandidateKeys) != 2 ||
		hints.CandidateKeys[0] != "user_name" ||
		hints.CandidateKeys[1] != "department" {
		t.Fatalf("candidate keys=%v", hints.CandidateKeys)
	}
	if len(hints.ScriptPaths) != 1 ||
		hints.ScriptPaths[0] != "/allogene/assets/index.js" {
		t.Fatalf("script paths=%v", hints.ScriptPaths)
	}
	if len(hints.TextCandidateSelectors) != 1 ||
		hints.TextCandidateSelectors[0] != "a.student-profile" {
		t.Fatalf(
			"text candidate selectors=%v",
			hints.TextCandidateSelectors,
		)
	}
	if len(hints.ProfilePaths) != 1 ||
		hints.ProfilePaths[0] != "/allogene/page/studentInfo.htm" {
		t.Fatalf("profile paths=%v", hints.ProfilePaths)
	}
}
