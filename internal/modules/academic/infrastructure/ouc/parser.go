package ouc

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"golang.org/x/net/html"
)

var (
	realNamePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)["'](?:realName|real_name|xm|studentName)["']\s*:\s*["']([\p{Han}·]{2,20})["']`),
		regexp.MustCompile(`(?:姓名|欢迎您)[：:，,\s]+([\p{Han}·]{2,20})`),
	}
	validRealNamePattern        = regexp.MustCompile(`^[\p{Han}·]{2,20}$`)
	undergraduateProfilePattern = regexp.MustCompile(
		`^([\p{Han}·]{2,20})\s*[-－—]\s*[0-9]{6,32}$`,
	)
	chineseStringPropertyPattern = regexp.MustCompile(
		`(?i)["']([a-z_][a-z0-9_.-]{0,63})["']\s*:\s*["'][\p{Han}·]{2,20}["']`,
	)
	numberPattern        = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
	examTimeRangePattern = regexp.MustCompile(
		`^\s*(\d{4}-\d{2}-\d{2})\s+(\d{1,2}:\d{2})\s*[~～\-至]\s*(\d{1,2}:\d{2})\s*$`,
	)
	shanghaiLocation = time.FixedZone("Asia/Shanghai", 8*60*60)
)

func extractRealName(body []byte) string {
	for _, pattern := range realNamePatterns {
		matches := pattern.FindSubmatch(body)
		if len(matches) == 2 {
			return strings.TrimSpace(string(matches[1]))
		}
	}
	return extractRealNameFromDOM(body)
}

func extractRealNameFromDOM(body []byte) string {
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}
	candidates := make(map[string]struct{})
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode && hasElementClasses(node, "user", "name") {
			value := compactNodeText(node)
			if validRealNamePattern.MatchString(value) {
				candidates[value] = struct{}{}
			}
		}
		if node.Type == html.ElementNode &&
			hasElementClasses(node, "infoContentTitle", "qz-ellipse") {
			value := compactNodeText(node)
			matches := undergraduateProfilePattern.FindStringSubmatch(value)
			if len(matches) == 2 {
				candidates[matches[1]] = struct{}{}
			}
		}
		if node.Type == html.ElementNode &&
			node.Data == "p" &&
			hasElementClasses(node.Parent, "right-person") {
			value := compactNodeText(node)
			if validRealNamePattern.MatchString(value) {
				candidates[value] = struct{}{}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	if len(candidates) != 1 {
		return ""
	}
	for candidate := range candidates {
		return candidate
	}
	return ""
}

func hasElementClasses(node *html.Node, required ...string) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	classes := make(map[string]struct{})
	for _, attribute := range node.Attr {
		if strings.ToLower(attribute.Key) != "class" {
			continue
		}
		for _, className := range strings.Fields(attribute.Val) {
			classes[strings.ToLower(className)] = struct{}{}
		}
	}
	for _, className := range required {
		if _, exists := classes[strings.ToLower(className)]; !exists {
			return false
		}
	}
	return true
}

func compactNodeText(node *html.Node) string {
	if node == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	var visit func(*html.Node)
	visit = func(current *html.Node) {
		if current.Type == html.ElementNode &&
			(current.Data == "script" || current.Data == "style") {
			return
		}
		if current.Type == html.TextNode {
			parts = append(parts, strings.Fields(current.Data)...)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(node)
	return strings.Join(parts, "")
}

type realNameContractHints struct {
	CandidateKeys          []string
	ScriptPaths            []string
	TextCandidateSelectors []string
	ProfilePaths           []string
}

// collectRealNameContractHints returns structure-only diagnostics for an
// authenticated page whose real-name field is not yet supported. Values,
// query strings and response text are deliberately excluded.
func collectRealNameContractHints(body []byte) realNameContractHints {
	hints := realNameContractHints{
		CandidateKeys:          make([]string, 0, 8),
		ScriptPaths:            make([]string, 0, 8),
		TextCandidateSelectors: make([]string, 0, 16),
		ProfilePaths:           make([]string, 0, 8),
	}
	seenKeys := make(map[string]struct{})
	for _, match := range chineseStringPropertyPattern.FindAllSubmatch(body, 32) {
		if len(match) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(string(match[1])))
		if _, exists := seenKeys[key]; exists {
			continue
		}
		seenKeys[key] = struct{}{}
		hints.CandidateKeys = append(hints.CandidateKeys, key)
		if len(hints.CandidateKeys) == 8 {
			break
		}
	}

	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return hints
	}
	seenPaths := make(map[string]struct{})
	seenTextSelectors := make(map[string]struct{})
	seenProfilePaths := make(map[string]struct{})
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "script" {
			for _, attribute := range node.Attr {
				if len(hints.ScriptPaths) >= 8 {
					break
				}
				if strings.ToLower(attribute.Key) != "src" {
					continue
				}
				parsed, parseErr := url.Parse(strings.TrimSpace(attribute.Val))
				if parseErr != nil ||
					parsed.User != nil ||
					(parsed.IsAbs() && !allowedOUCHost(parsed.Hostname())) {
					continue
				}
				path := parsed.EscapedPath()
				if path == "" {
					continue
				}
				if _, exists := seenPaths[path]; exists {
					continue
				}
				seenPaths[path] = struct{}{}
				hints.ScriptPaths = append(hints.ScriptPaths, path)
			}
		}
		if node.Type == html.TextNode &&
			len(hints.TextCandidateSelectors) < 16 &&
			shortHanText(strings.TrimSpace(node.Data)) {
			if selector := safeElementSelector(node.Parent); selector != "" {
				if _, exists := seenTextSelectors[selector]; !exists {
					seenTextSelectors[selector] = struct{}{}
					hints.TextCandidateSelectors = append(
						hints.TextCandidateSelectors,
						selector,
					)
				}
			}
		}
		if node.Type == html.ElementNode && len(hints.ProfilePaths) < 8 {
			for _, attribute := range node.Attr {
				key := strings.ToLower(attribute.Key)
				if key != "href" && key != "src" && key != "action" {
					continue
				}
				parsed, parseErr := url.Parse(strings.TrimSpace(attribute.Val))
				if parseErr != nil ||
					parsed.User != nil ||
					(parsed.IsAbs() && !allowedOUCHost(parsed.Hostname())) {
					continue
				}
				path := parsed.EscapedPath()
				if path == "" || !looksLikeProfilePath(path) {
					continue
				}
				if _, exists := seenProfilePaths[path]; exists {
					continue
				}
				seenProfilePaths[path] = struct{}{}
				hints.ProfilePaths = append(hints.ProfilePaths, path)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return hints
}

var shortHanTextPattern = regexp.MustCompile(`^[\p{Han}·]{2,4}$`)

func shortHanText(value string) bool {
	return shortHanTextPattern.MatchString(value)
}

func safeElementSelector(node *html.Node) string {
	if node == nil || node.Type != html.ElementNode {
		return ""
	}
	selector := strings.ToLower(node.Data)
	for _, attribute := range node.Attr {
		key := strings.ToLower(attribute.Key)
		if key != "id" && key != "class" {
			continue
		}
		for _, token := range strings.Fields(attribute.Val) {
			if !safeSelectorToken(token) {
				continue
			}
			if key == "id" {
				selector += "#" + token
			} else {
				selector += "." + token
			}
		}
	}
	if selector == strings.ToLower(node.Data) {
		return ""
	}
	return selector
}

func safeSelectorToken(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			character != '-' &&
			character != '_' {
			return false
		}
	}
	return true
}

func looksLikeProfilePath(path string) bool {
	lower := strings.ToLower(path)
	for _, fragment := range []string{
		"user",
		"person",
		"profile",
		"student",
		"account",
		"info",
		"home",
		"main",
	} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func parsePeriods(body []byte, encoding string) ([]domain.Period, error) {
	rows, err := tabularRows(body, encoding)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	result := make([]domain.Period, 0, len(rows))
	for index, row := range rows {
		id := fieldString(row, "id", "period_id", "semesterId", "semesterCode", "xnxqdm", "xnxq")
		label := fieldString(row, "label", "name", "semesterName", "xnxqmc", "学期")
		if id == "" {
			id = label
		}
		if id == "" || label == "" {
			continue
		}
		start := fieldTime(row, "start_date", "startDate", "kssj", "开始日期")
		if start.IsZero() {
			start = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		weekCount := fieldInt(row, "week_count", "weekCount", "zcs", "周数")
		if weekCount <= 0 {
			weekCount = 20
		}
		shortLabel := fieldString(row, "short_label", "shortLabel")
		if shortLabel == "" {
			shortLabel = label
		}
		result = append(result, domain.Period{
			ID: id, Label: label, ShortLabel: shortLabel, StartDate: start,
			WeekCount: weekCount, IsCurrent: fieldBool(row, "is_current", "isCurrent", "dq", "当前学期"),
		})
		_ = index
	}
	if len(result) == 0 {
		return nil, application.ErrProviderUnavailable
	}
	return result, nil
}

func parseCourses(body []byte, encoding, periodID string) ([]domain.Course, error) {
	rows, err := tabularRows(body, encoding)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	result := make([]domain.Course, 0, len(rows))
	for index, row := range rows {
		name := fieldString(
			row,
			"name",
			"course_name",
			"courseName",
			"kcmc",
			"kc_mc",
			"课程名称",
		)
		code := fieldString(row, "course_code", "courseCode", "kch", "kcdm", "课程号")
		if name == "" {
			continue
		}
		id := fieldString(row, "id", "course_id", "courseId", "jx0404id")
		if id == "" {
			id = fmt.Sprintf("%s-%d", code, index+1)
		}
		weeks := fieldInts(row, "weeks", "week_list", "zcd", "上课周次")
		if len(weeks) == 0 {
			weeks = []int{1}
		}
		result = append(result, domain.Course{
			ID: id, PeriodID: valueOr(
				fieldString(row, "period_id", "semesterId", "xnxqdm", "xnxqid"),
				periodID,
			),
			CourseCode: code, Name: name,
			Teacher:      fieldString(row, "teacher", "teacherName", "jsxm", "授课教师"),
			Campus:       fieldString(row, "campus", "campusName", "xqmc", "校区"),
			Location:     fieldString(row, "location", "classroom", "jsmc", "上课地点"),
			Weekday:      fieldInt(row, "weekday", "dayOfWeek", "xqj", "星期"),
			StartSection: fieldInt(row, "start_section", "startSection", "ksjc", "开始节次"),
			EndSection:   fieldInt(row, "end_section", "endSection", "jsjc", "结束节次"),
			Weeks:        weeks,
		})
	}
	return result, nil
}

func parseGrades(body []byte, encoding, periodID string) ([]domain.Grade, error) {
	rows, err := tabularRows(body, encoding)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	result := make([]domain.Grade, 0, len(rows))
	seenUndergraduateGradeIDs := make(map[string]struct{}, len(rows))
	for index, row := range rows {
		name := fieldString(row, "course_name", "courseName", "kcmc", "kc_mc", "课程名称")
		if name == "" {
			continue
		}
		undergraduateGradeID := fieldString(row, "cj0708id")
		if undergraduateGradeID != "" {
			if _, exists := seenUndergraduateGradeIDs[undergraduateGradeID]; exists {
				continue
			}
			seenUndergraduateGradeIDs[undergraduateGradeID] = struct{}{}
		}
		code := fieldString(row, "course_code", "courseCode", "kch", "课程号")
		rawGrade := fieldString(
			row,
			"grade_level",
			"level",
			"dj",
			"zcjstr",
			"score",
			"cj",
			"成绩",
			"等级",
			"zcj",
		)
		var score *float64
		gradeType := domain.GradeTypeLevel
		var level *domain.GradeLevel
		if scoreValue, err := strconv.ParseFloat(rawGrade, 64); err == nil {
			score = &scoreValue
			gradeType = domain.GradeTypeNumber
		} else if rawGrade != "" {
			value := domain.GradeLevel(rawGrade)
			level = &value
			convertedScore := convertUndergraduateScore(rawGrade)
			score = &convertedScore
		}
		id := fieldString(row, "id", "grade_id", "cj0708id")
		if id == "" {
			id = fmt.Sprintf("%s-%d", code, index+1)
		}
		credit, _ := fieldFloat(row, "credit", "xf", "学分")
		result = append(result, domain.Grade{
			ID: id, PeriodID: valueOr(
				fieldString(row, "period_id", "semesterId", "xnxqdm", "xqstr", "xnxqid"),
				periodID,
			),
			CourseCode: code, CourseName: name,
			CourseType: fieldString(
				row,
				"course_type",
				"courseType",
				"kcxzmc",
				"kccm",
				"课程性质",
			),
			Credit: credit, GradeType: gradeType, Score: score, GradeLevel: level,
		})
	}
	return result, nil
}

func convertUndergraduateScore(raw string) float64 {
	switch strings.TrimSpace(raw) {
	case "优秀", "优", "免修":
		return 90
	case "通过":
		return 85
	case "良", "良好":
		return 80
	case "中", "中等":
		return 70
	case "合格", "及格":
		return 60
	case "不合格", "不及格":
		return 0
	default:
		return 0
	}
}

func parseExams(body []byte, encoding, periodID string) ([]domain.Exam, error) {
	rows, err := tabularRows(body, encoding)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	result := make([]domain.Exam, 0, len(rows))
	for index, row := range rows {
		name := fieldString(
			row,
			"course_name",
			"courseName",
			"kcmc",
			"kskcmc",
			"课程名称",
		)
		if name == "" {
			continue
		}
		code := fieldString(row, "course_code", "courseCode", "kch", "课程号")
		start := fieldTime(row, "start_at", "startAt", "kssj", "考试时间")
		end := fieldTime(row, "end_at", "endAt", "jssj")
		if rangeStart, rangeEnd, ok := parseExamTimeRange(
			fieldString(row, "kssj", "考试时间"),
		); ok {
			start, end = rangeStart, rangeEnd
		}
		if end.IsZero() && !start.IsZero() {
			end = start.Add(2 * time.Hour)
		}
		id := fieldString(row, "id", "exam_id", "kw0410id", "kw0413id")
		if id == "" {
			id = fmt.Sprintf("%s-%d", code, index+1)
		}
		phase := domain.ExamPhase(fieldString(row, "phase", "examPhase", "ksxzmc", "考试性质"))
		if phase == "" {
			phase = domain.ExamPhaseFinal
		}
		result = append(result, domain.Exam{
			ID: id, PeriodID: valueOr(
				fieldString(row, "period_id", "semesterId", "xnxqdm", "xnxqid"),
				periodID,
			),
			CourseCode: code, CourseName: name, StartAt: start, EndAt: end,
			Campus:   fieldString(row, "campus", "campusName", "ksxq", "xqmc", "校区"),
			Location: fieldString(row, "location", "classroom", "jsmc", "js_mc", "考试地点"),
			Seat:     fieldString(row, "seat", "seatNo", "zwh", "座位号"), Phase: phase,
			Method:    fieldString(row, "method", "examMethod", "ksfs", "考试方式"),
			Materials: fieldString(row, "materials", "allowedMaterials", "可带资料"),
			Notice:    fieldString(row, "notice", "remark", "bz", "bzywmc", "备注"),
		})
	}
	return result, nil
}

func parseSelections(body []byte, encoding, periodID string) ([]domain.CourseSelection, error) {
	rows, err := tabularRows(body, encoding)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	result := make([]domain.CourseSelection, 0, len(rows))
	for index, row := range rows {
		name := fieldString(row, "course_name", "courseName", "kcmc", "kc_mc", "课程名称")
		if name == "" {
			continue
		}
		code := fieldString(row, "course_code", "courseCode", "kch", "课程号")
		id := fieldString(row, "id", "selection_id", "jx02id", "jx0404id")
		if id == "" {
			id = fmt.Sprintf("%s-%d", code, index+1)
		}
		credit, _ := fieldFloat(row, "credit", "xf", "学分")
		rawStatus := fieldString(
			row,
			"status",
			"selectionStatus",
			"tklx",
			"退课类型",
			"xkzt",
			"选课状态",
		)
		status := normalizeCourseSelectionStatus(rawStatus)
		resultText := fieldString(row, "result_text", "resultText", "tklx", "退课类型")
		if len([]rune(resultText)) > 500 {
			return nil, fmt.Errorf("course selection result text is too long")
		}
		var resultTextPointer *string
		if resultText != "" {
			resultTextPointer = &resultText
		}
		selectedAtValue := fieldTime(row, "selected_at", "selectedAt", "xksj", "选课时间")
		var selectedAt *time.Time
		if !selectedAtValue.IsZero() {
			selectedAt = &selectedAtValue
		}
		var note *string
		if value := fieldString(row, "note", "remark", "bz", "备注"); value != "" {
			note = &value
		}
		schedule := fieldString(row, "schedule", "courseTime", "sksj", "上课时间")
		if status == domain.CourseSelectionFailed {
			schedule = selectionScheduleWithResult(schedule, resultText)
		}
		result = append(result, domain.CourseSelection{
			ID: id, PeriodID: valueOr(
				fieldString(row, "period_id", "semesterId", "xnxqdm", "xnxqid"),
				periodID,
			),
			CourseCode: code, CourseName: name,
			CourseType: fieldString(
				row,
				"course_type",
				"courseType",
				"kcxzmc",
				"kcxz_mc",
				"kclb_mc",
				"kcsxmc",
				"课程性质",
			),
			Credit: credit, Teacher: fieldString(
				row,
				"teacher",
				"teacherName",
				"jsxm",
				"xm",
				"skls",
				"授课教师",
			),
			Campus:   fieldString(row, "campus", "campusName", "xqmc", "校区"),
			Location: fieldString(row, "location", "classroom", "jsmc", "skdd", "地点"),
			Schedule: schedule,
			Capacity: fieldInt(row, "capacity", "maxCount", "krl", "容量"),
			Enrolled: fieldInt(row, "enrolled", "selectedCount", "yxrs", "已选人数"),
			Status:   status, SelectedAt: selectedAt, ResultText: resultTextPointer, Note: note,
		})
	}
	return result, nil
}

func selectionScheduleWithResult(schedule, resultText string) string {
	schedule = strings.TrimSpace(schedule)
	resultText = strings.TrimSpace(resultText)
	if resultText == "" {
		return schedule
	}
	if schedule == "" {
		return resultText
	}
	if strings.Contains(schedule, resultText) {
		return schedule
	}
	return schedule + "（" + resultText + "）"
}

func normalizeCourseSelectionStatus(raw string) domain.CourseSelectionStatus {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch domain.CourseSelectionStatus(value) {
	case domain.CourseSelectionSelected:
		return domain.CourseSelectionSelected
	case domain.CourseSelectionPending:
		return domain.CourseSelectionPending
	case domain.CourseSelectionFailed:
		return domain.CourseSelectionFailed
	}
	for _, keyword := range []string{"待", "确认中", "处理中", "预选"} {
		if strings.Contains(value, keyword) {
			return domain.CourseSelectionPending
		}
	}
	for _, keyword := range []string{"落选", "未选", "失败", "退选", "取消"} {
		if strings.Contains(value, keyword) {
			return domain.CourseSelectionFailed
		}
	}
	return domain.CourseSelectionSelected
}

func tabularRows(body []byte, encoding string) ([]map[string]any, error) {
	if encoding == "html" {
		return htmlRows(body)
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return unwrapRows(payload)
}

func unwrapRows(payload any) ([]map[string]any, error) {
	switch value := payload.(type) {
	case []any:
		return objectRows(value), nil
	case map[string]any:
		for _, key := range []string{"data", "rows", "list", "result", "items"} {
			nested, ok := value[key]
			if !ok {
				continue
			}
			if nestedMap, isMap := nested.(map[string]any); isMap {
				rows, err := unwrapRows(nestedMap)
				if err == nil {
					return rows, nil
				}
			}
			if array, isArray := nested.([]any); isArray {
				return objectRows(array), nil
			}
		}
	}
	return nil, fmt.Errorf("tabular data not found")
}

func objectRows(values []any) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if row, ok := value.(map[string]any); ok {
			result = append(result, row)
		}
	}
	return result
}

func htmlRows(body []byte) ([]map[string]any, error) {
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	var tables []*html.Node
	walk(root, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			tables = append(tables, node)
		}
	})
	for _, table := range tables {
		var rows [][]string
		walk(table, func(node *html.Node) {
			if node.Type != html.ElementNode || node.Data != "tr" {
				return
			}
			var cells []string
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.ElementNode && (child.Data == "th" || child.Data == "td") {
					cells = append(cells, strings.TrimSpace(nodeText(child)))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
		})
		if len(rows) < 2 {
			continue
		}
		headers := rows[0]
		for _, cells := range rows[1:] {
			row := make(map[string]any, len(headers))
			for index, header := range headers {
				if index < len(cells) {
					row[header] = cells[index]
				}
			}
			result = append(result, row)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("HTML table data not found")
	}
	return result, nil
}

func walk(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func nodeText(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var builder strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		builder.WriteString(nodeText(child))
	}
	return builder.String()
}

func fieldString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := row[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			if result := strings.TrimSpace(typed); result != "" {
				return result
			}
		case json.Number:
			return typed.String()
		case float64:
			return strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			return strings.TrimSpace(fmt.Sprint(typed))
		}
	}
	return ""
}

func fieldInt(row map[string]any, keys ...string) int {
	value := fieldString(row, keys...)
	match := numberPattern.FindString(value)
	if match == "" {
		return 0
	}
	number, _ := strconv.ParseFloat(match, 64)
	return int(number)
}

func fieldFloat(row map[string]any, keys ...string) (float64, bool) {
	value := fieldString(row, keys...)
	match := numberPattern.FindString(value)
	if match == "" {
		return 0, false
	}
	number, err := strconv.ParseFloat(match, 64)
	return number, err == nil
}

func fieldBool(row map[string]any, keys ...string) bool {
	value := strings.ToLower(fieldString(row, keys...))
	return value == "true" || value == "1" || value == "是" || value == "当前"
}

func fieldTime(row map[string]any, keys ...string) time.Time {
	value := fieldString(row, keys...)
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02 15:04",
		"2006/01/02",
	} {
		if parsed, err := time.ParseInLocation(layout, value, shanghaiLocation); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func parseExamTimeRange(value string) (time.Time, time.Time, bool) {
	match := examTimeRangePattern.FindStringSubmatch(value)
	if len(match) != 4 {
		return time.Time{}, time.Time{}, false
	}
	start, startErr := time.ParseInLocation(
		"2006-01-02 15:04",
		match[1]+" "+match[2],
		shanghaiLocation,
	)
	end, endErr := time.ParseInLocation(
		"2006-01-02 15:04",
		match[1]+" "+match[3],
		shanghaiLocation,
	)
	if startErr != nil || endErr != nil || !end.After(start) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

func normalizeGradeLevel(raw string) (domain.GradeLevel, bool) {
	switch strings.TrimSpace(raw) {
	case string(domain.GradeLevelExcellent):
		return domain.GradeLevelExcellent, true
	case string(domain.GradeLevelGood):
		return domain.GradeLevelGood, true
	case string(domain.GradeLevelMedium):
		return domain.GradeLevelMedium, true
	case string(domain.GradeLevelPass), "合格", "通过":
		return domain.GradeLevelPass, true
	case string(domain.GradeLevelFail), "不合格", "未通过":
		return domain.GradeLevelFail, true
	case string(domain.GradeLevelExempt):
		return domain.GradeLevelExempt, true
	default:
		return "", false
	}
}

func fieldInts(row map[string]any, keys ...string) []int {
	for _, key := range keys {
		value, ok := row[key]
		if !ok {
			continue
		}
		if values, array := value.([]any); array {
			result := make([]int, 0, len(values))
			for _, item := range values {
				number, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(item)))
				if err == nil {
					result = append(result, number)
				}
			}
			return result
		}
		text := fieldString(row, key)
		matches := numberPattern.FindAllString(text, -1)
		result := make([]int, 0, len(matches))
		for _, match := range matches {
			number, err := strconv.Atoi(match)
			if err == nil {
				result = append(result, number)
			}
		}
		return result
	}
	return nil
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
