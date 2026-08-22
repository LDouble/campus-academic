package ouc

import (
	"testing"

	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

func TestParseUndergraduateCatalogPage(t *testing.T) {
	t.Parallel()
	page, err := parseCourseCatalogPage(
		verificationapp.EducationUndergraduate,
		"2026-2027-1",
		2,
		academicconfig.OperationEndpoint{ResponseEncoding: "json", PageSize: 20},
		[]byte(`{"code":0,"count":21,"pageSize":20,"pages":2,"data":[{"xnxq01id":"2026-2027-1","xkh":"UG-001","xqmc":"崂山","kch":"CS001","kcmc":"数据结构","kccm":"专业课","skjs":"教师甲、教师乙","skyx":"计算机学院","ktmc":"测试班","sksj":"周一 1-2 节","skdd":"教学楼101","bz":""}]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalCount != 21 || page.TotalPages != 2 || page.HasMore || len(page.Entries) != 1 {
		t.Fatalf("page=%+v", page)
	}
	entry := page.Entries[0]
	if entry.SourceKey != "UG-001" || entry.CourseCode != "CS001" || entry.Teachers != "教师甲、教师乙" || entry.Classes != "测试班" {
		t.Fatalf("entry=%+v", entry)
	}
}

func TestParseUndergraduateCatalogRejectsMissingStableKey(t *testing.T) {
	t.Parallel()
	_, err := parseUndergraduateCatalogPage("2026-2027-1", 1, 20, []byte(`{"code":0,"count":1,"data":[{"kcmc":"课程"}]}`))
	if err == nil {
		t.Fatal("missing stable key was accepted")
	}
}

func TestParseGraduateCatalogPage(t *testing.T) {
	t.Parallel()
	page, err := parseCourseCatalogPage(
		verificationapp.EducationGraduate,
		"2026:11",
		2,
		academicconfig.OperationEndpoint{ResponseEncoding: "html", PageSize: 20},
		[]byte(`<!doctype html><html><body><form id="lssjCxdcForm"></form><p>共 21 条，共 2 页</p><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-001</td><td>海洋科学</td><td>海洋学院</td><td>30/12</td><td>中文</td><td>教师甲；教师乙</td><td>1-16周||周一1-2节||崂山校区</td><td></td></tr></table></body></html>`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalCount != 21 || page.TotalPages != 2 || page.HasMore || len(page.Entries) != 1 {
		t.Fatalf("page=%+v", page)
	}
	entry := page.Entries[0]
	if entry.SourceKey == "" || entry.OpeningCode != "GR-001" || entry.CourseCode != "" || entry.Capacity != 0 || entry.Enrolled != 0 {
		t.Fatalf("entry=%+v", entry)
	}
}

func TestParseGraduateCatalogIgnoresInvalidCapacity(t *testing.T) {
	t.Parallel()
	body := `<!doctype html><html><body><p>共 1 条，共 1 页</p><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-001</td><td>海洋科学</td><td>海洋学院</td><td>不限制/超额</td><td>中文</td><td>教师甲</td><td>周一</td><td></td></tr></table></body></html>`
	page, err := parseGraduateCatalogPage("2026:11", 1, 20, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Capacity != 0 || page.Entries[0].Enrolled != 0 {
		t.Fatalf("entries=%+v", page.Entries)
	}
}

func TestParseGraduateCatalogDistinguishesDuplicateOpeningCodes(t *testing.T) {
	t.Parallel()
	body := `<!doctype html><html><body><p>共 2 条，共 1 页</p><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-SAME</td><td>海洋科学</td><td>海洋学院</td><td>30/12</td><td>中文</td><td>教师甲</td><td>周一</td><td></td></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-SAME</td><td>海洋科学</td><td>海洋学院</td><td>30/10</td><td>中文</td><td>教师乙</td><td>周二</td><td></td></tr></table></body></html>`
	page, err := parseGraduateCatalogPage("2026:11", 1, 20, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Entries[0].OpeningCode != "GR-SAME" ||
		page.Entries[1].OpeningCode != "GR-SAME" || page.Entries[0].SourceKey == page.Entries[1].SourceKey {
		t.Fatalf("entries=%+v", page.Entries)
	}
}

func TestParseGraduateCatalogUsesPagingLinksAndFinalPageCount(t *testing.T) {
	t.Parallel()
	body := `<!doctype html><html><body><form><input name="pageId" value="63"></form><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-LAST</td><td>海洋科学</td><td>海洋学院</td><td>30/12</td><td>中文</td><td>教师甲</td><td>周一</td><td></td></tr></table><span onclick="$.pagingSubmit('62')">上一页</span><span onclick="$.pagingSubmit('63')">63</span></body></html>`
	page, err := parseGraduateCatalogPage("2026:11", 63, 20, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalPages != 63 || page.TotalCount != 1241 || page.HasMore || len(page.Entries) != 1 {
		t.Fatalf("page=%+v", page)
	}
}

func TestParseGraduateCatalogIntermediatePageLeavesTotalUnknown(t *testing.T) {
	t.Parallel()
	body := `<!doctype html><html><body><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td><td>夏秋</td><td>GR-ONE</td><td>海洋科学</td><td>海洋学院</td><td>30/12</td><td>中文</td><td>教师甲</td><td>周一</td><td></td></tr></table><span onclick="$.pagingSubmit('1')">1</span><span onclick="$.pagingSubmit('63')">63</span></body></html>`
	page, err := parseGraduateCatalogPage("2026:11", 1, 20, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalCount != 0 || page.TotalPages != 63 || !page.HasMore {
		t.Fatalf("page=%+v", page)
	}
}

func TestParseGraduateCatalogRejectsLoginAndMalformedRows(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"login":     `<html><body><form id="loginForm"><input type="password"></form></body></html>`,
		"short row": `<p>共 1 条，共 1 页</p><table><tr><th>学年</th><th>学期</th><th>开课号</th><th>课程名称</th><th>开课学院</th><th>容量/已选</th><th>上课语言</th><th>任课教师</th><th>上课时间地点</th><th>备注</th></tr><tr><td>2026-2027</td></tr></table>`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseGraduateCatalogPage("2026:11", 1, 20, []byte(body))
			if err == nil {
				t.Fatal("invalid catalog page was accepted")
			}
		})
	}
}
