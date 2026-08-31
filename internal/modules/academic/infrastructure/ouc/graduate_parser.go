package ouc

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"golang.org/x/net/html"
)

const (
	graduateAutumnTermCode = "11"
	graduateSpringTermCode = "12"
	graduateWeekCount      = 23
)

var (
	graduateAcademicYearPattern = regexp.MustCompile(`^(\d{4})-(\d{4})$`)
	graduateLabeledValuePattern = regexp.MustCompile(
		`^(?:课程名称|课程编号|课程号|任课教师|教师|上课地点|地点|教室|校区|周次)[：:]\s*(.+)$`,
	)
	graduateWeekExpressionPattern = regexp.MustCompile(
		`(?:[\(（\[【][ \t]*|第[ \t]*)?([0-9０-９][0-9０-９ \t,，、;；|｜+＋/／\-—~～至单双单周]*)(?:[\)）\]】][ \t]*)?周`,
	)
	graduateSectionPattern = regexp.MustCompile(
		`第?\s*(\d{1,2})(?:\s*[-—~～至]\s*(\d{1,2}))?\s*节`,
	)
	graduateExamClockRangePattern = regexp.MustCompile(
		`^\s*(\d{1,2}:\d{2})\s*(?:->|→|[-—~～至])\s*(\d{1,2}:\d{2})\s*$`,
	)
	graduateUnreleasedGradeValues = map[string]struct{}{
		"未选":     {},
		"选课":     {},
		"退换课":    {},
		"正在申请免修": {},
		"正在修读":   {},
		"在修":     {},
		"正在重修":   {},
	}
)

type graduateHTMLTable struct {
	headers []string
	rows    [][]*html.Node
}

func parseGraduatePeriods(body []byte, encoding string) ([]domain.Period, error) {
	if encoding != "html" {
		return parsePeriods(body, encoding)
	}
	table, found, err := findGraduateHTMLTable(
		body,
		"课程编号",
		"课程名称",
		"选课学年",
		"学期",
	)
	if err != nil || !found {
		return nil, application.ErrProviderUnavailable
	}
	periods := make(map[string]domain.Period)
	for _, row := range graduateTableRows(table) {
		period, ok := graduatePeriod(
			fieldString(row, "选课学年"),
			fieldString(row, "学期"),
			time.Now(),
		)
		if !ok {
			continue
		}
		periods[period.ID] = period
	}
	result := make([]domain.Period, 0, len(periods))
	for _, period := range periods {
		result = append(result, period)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID > result[right].ID
	})
	return result, nil
}

func parseGraduateGrades(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Grade, error) {
	if encoding != "html" {
		return parseGrades(body, encoding, periodID)
	}
	table, found, err := findGraduateHTMLTable(
		body,
		"课程编号",
		"课程名称",
		"课程性质",
		"学分",
		"选课学年",
		"学期",
		"修读情况/成绩",
	)
	if err != nil || !found {
		return nil, application.ErrProviderUnavailable
	}
	rows := graduateTableRows(table)
	result := make([]domain.Grade, 0, len(rows))
	for index, row := range rows {
		resolvedPeriodID, ok := graduateGradePeriodID(
			fieldString(row, "选课学年"),
			fieldString(row, "学期"),
			time.Now(),
		)
		if !ok || (periodID != "" && resolvedPeriodID != periodID) {
			continue
		}
		name := fieldString(row, "课程名称")
		if name == "" {
			continue
		}
		scoreText := fieldString(row, "修读情况/成绩")
		score, gradeType, level, ok := graduateGradeValue(scoreText)
		if !ok {
			continue
		}
		code := fieldString(row, "课程编号")
		credit, _ := fieldFloat(row, "学分")
		result = append(result, domain.Grade{
			ID:         fmt.Sprintf("%s:%s:%d", resolvedPeriodID, code, index+1),
			PeriodID:   resolvedPeriodID,
			CourseCode: code,
			CourseName: name,
			CourseType: fieldString(row, "课程性质"),
			Credit:     credit,
			GradeType:  gradeType,
			Score:      score,
			GradeLevel: level,
		})
	}
	return result, nil
}

// graduateGradePeriodID keeps a released grade visible when the upstream
// record provides a valid academic year but omits its term. In that case, the
// original academic year remains the stable grouping value instead of guessing
// a term that the upstream system did not provide.
func graduateGradePeriodID(academicYear string, term string, now time.Time) (string, bool) {
	period, ok := graduatePeriod(academicYear, term, now)
	if ok {
		return period.ID, true
	}

	academicYear = strings.TrimSpace(academicYear)
	if strings.TrimSpace(term) != "" || !validGraduateAcademicYear(academicYear) {
		return "", false
	}
	return academicYear, true
}

func validGraduateAcademicYear(academicYear string) bool {
	match := graduateAcademicYearPattern.FindStringSubmatch(academicYear)
	if len(match) != 3 {
		return false
	}
	startYear, startErr := strconv.Atoi(match[1])
	endYear, endErr := strconv.Atoi(match[2])
	return startErr == nil && endErr == nil && endYear == startYear+1
}

func parseGraduateSelections(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.CourseSelection, error) {
	if encoding != "html" {
		return parseSelections(body, encoding, periodID)
	}
	table, found, err := findGraduateHTMLTable(
		body,
		"课程编号",
		"课程名称",
		"课程性质",
		"学分",
		"选课学年",
		"学期",
		"修读情况/成绩",
	)
	if err != nil || !found {
		return nil, application.ErrProviderUnavailable
	}
	rows := graduateTableRows(table)
	result := make([]domain.CourseSelection, 0, len(rows))
	for index, row := range rows {
		period, ok := graduatePeriod(
			fieldString(row, "选课学年"),
			fieldString(row, "学期"),
			time.Now(),
		)
		if !ok || (periodID != "" && period.ID != periodID) {
			continue
		}
		name := fieldString(row, "课程名称")
		if name == "" {
			continue
		}
		resultText := fieldString(row, "修读情况/成绩")
		if len([]rune(resultText)) > 500 {
			return nil, application.ErrProviderUnavailable
		}
		var resultTextPointer *string
		if resultText != "" {
			resultTextPointer = &resultText
		}
		code := fieldString(row, "课程编号")
		credit, _ := fieldFloat(row, "学分")
		result = append(result, domain.CourseSelection{
			ID:         fmt.Sprintf("%s:%s:%d", period.ID, code, index+1),
			PeriodID:   period.ID,
			CourseCode: code,
			CourseName: name,
			CourseType: fieldString(row, "课程性质"),
			Credit:     credit,
			Teacher:    fieldString(row, "任课老师", "任课教师"),
			Status:     graduateSelectionStatus(resultText),
			ResultText: resultTextPointer,
		})
	}
	return result, nil
}

func graduateSelectionStatus(resultText string) domain.CourseSelectionStatus {
	switch {
	case strings.Contains(resultText, "未选"):
		return domain.CourseSelectionFailed
	case strings.Contains(resultText, "正在申请免修"),
		strings.Contains(resultText, "退换课"):
		return domain.CourseSelectionPending
	default:
		return domain.CourseSelectionSelected
	}
}

func parseGraduateCourses(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Course, error) {
	if encoding != "html" {
		return parseCourses(body, encoding, periodID)
	}
	// 研究生课程响应同时兼容 grkcb.htm 的网格结构和 xkgrcx.htm 的
	// 选课历史结构。两种页面都按表头识别，便于主查询和空结果回退共用
	// 同一套解析规则。
	selectionTable, found, err := findGraduateHTMLTable(
		body,
		"开课学年",
		"开课学期",
		"班级编号",
		"课程名称",
		"学分",
		"任课教师",
		"时间与地点",
		"备注",
	)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	if found {
		return parseGraduateCourseHistory(selectionTable, periodID), nil
	}
	table, found, err := findGraduateHTMLTable(
		body,
		"时间",
		"星期一",
		"星期二",
		"星期三",
		"星期四",
		"星期五",
		"星期六",
		"星期日",
	)
	if err != nil || !found {
		return nil, application.ErrProviderUnavailable
	}
	result := make([]domain.Course, 0)
	rowSpans := make(map[int]int)
	for rowIndex, row := range table.rows {
		placements := graduateRowPlacements(row, rowSpans)
		section := graduateSection(placements, rowIndex)
		for _, placement := range placements {
			if placement.column < 2 || placement.column > 8 {
				continue
			}
			weekday := placement.column - 1
			if strings.TrimSpace(compactNodeText(placement.node)) == "" {
				continue
			}
			courses := graduateCoursesFromCell(
				placement.node,
				periodID,
				weekday,
				section,
				placement.rowspan,
				len(result),
			)
			for _, course := range courses {
				result = mergeGraduateCourse(result, course)
			}
		}
	}
	return result, nil
}

var graduateHistoryTimePattern = regexp.MustCompile(
	`\(\s*([^()]*)\s*\)\s*\|\|\s*星期\s*([一二三四五六日七天])\s*\|\|\s*(?:第\s*)?(\d{1,2})(?:\s*[-—~～至]\s*(\d{1,2}))?\s*节\s*\|\|\s*\(\s*([^()]*)\s*\)`,
)

func parseGraduateCourseHistory(
	table graduateHTMLTable,
	periodID string,
) []domain.Course {
	rows := graduateTableRows(table)
	result := make([]domain.Course, 0)
	for rowIndex, row := range rows {
		if !graduateHistoryPeriodMatches(row, periodID) {
			continue
		}
		name := fieldString(row, "课程名称")
		if name == "" {
			continue
		}
		selectionID := fieldString(row, "班级编号", "课程编号", "课程号")
		courseCode := graduateHistoryCourseCode(selectionID)
		teacher := fieldString(row, "任课教师", "任课老师", "教师")
		note := fieldString(row, "备注")
		// The Python check_time implementation removes the graduate-room
		// suffix before matching (for example, "3101研)" becomes "3101)").
		// Keep the normalized location value compatible with that behavior.
		timeAndLocation := strings.ReplaceAll(fieldString(row, "时间与地点"), "研)", ")")
		entries := graduateHistoryTimePattern.FindAllStringSubmatch(timeAndLocation, -1)
		for entryIndex, entry := range entries {
			if len(entry) != 6 {
				continue
			}
			weeks := parseGraduateWeeks(entry[1])
			if len(weeks) == 0 {
				weeks = integerRange(1, graduateWeekCount)
			}
			weekday := graduateWeekday(entry[2])
			if weekday == 0 {
				continue
			}
			startSection, startErr := strconv.Atoi(entry[3])
			if startErr != nil || startSection < 1 {
				continue
			}
			endSection := startSection
			if entry[4] != "" {
				parsedEnd, endErr := strconv.Atoi(entry[4])
				if endErr != nil || parsedEnd < startSection {
					continue
				}
				endSection = parsedEnd
			}
			campus, location := graduateHistoryLocation(entry[5])
			result = mergeGraduateCourse(result, domain.Course{
				ID:           graduateHistoryCourseID(periodID, selectionID, weekday, startSection, rowIndex, entryIndex),
				PeriodID:     periodID,
				CourseCode:   courseCode,
				Name:         name,
				Teacher:      teacher,
				Campus:       campus,
				Location:     location,
				Note:         note,
				Weekday:      weekday,
				StartSection: startSection,
				EndSection:   endSection,
				Weeks:        weeks,
			})
		}
	}
	return result
}

func graduateHistoryPeriodMatches(row map[string]any, periodID string) bool {
	if strings.TrimSpace(periodID) == "" {
		return true
	}
	period, ok := graduatePeriod(
		fieldString(row, "开课学年", "选课学年"),
		fieldString(row, "开课学期", "学期"),
		time.Now(),
	)
	return ok && period.ID == strings.TrimSpace(periodID)
}

func graduateHistoryCourseCode(selectionID string) string {
	selectionID = strings.TrimSpace(selectionID)
	if len([]rune(selectionID)) > 3 {
		// xkgrcx.htm 的班级编号末三位是班级序号；Python 版本将其
		// 去掉后作为 kch，这里沿用同一规则作为 CourseCode。
		return string([]rune(selectionID)[:len([]rune(selectionID))-3])
	}
	return selectionID
}

func graduateHistoryCourseID(
	periodID string,
	selectionID string,
	weekday int,
	startSection int,
	rowIndex int,
	entryIndex int,
) string {
	identity := strings.TrimSpace(selectionID)
	if identity == "" {
		identity = fmt.Sprintf("row-%d-entry-%d", rowIndex, entryIndex)
	}
	return fmt.Sprintf("%s:%d:%d:%s", periodID, weekday, startSection, identity)
}

func graduateWeekday(value string) int {
	return map[string]int{
		"一": 1,
		"二": 2,
		"三": 3,
		"四": 4,
		"五": 5,
		"六": 6,
		"日": 7,
		"七": 7,
		"天": 7,
	}[strings.TrimSpace(value)]
}

func graduateHistoryLocation(value string) (string, string) {
	parts := strings.Split(value, "||")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) == 0 {
		return "", ""
	}
	if len(cleaned) == 1 {
		return "", cleaned[0]
	}
	return cleaned[0], cleaned[len(cleaned)-1]
}

func parseGraduateExams(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Exam, error) {
	if encoding != "html" {
		return parseExams(body, encoding, periodID)
	}
	tables, err := graduateHTMLTables(body)
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	for _, table := range tables {
		if !graduateHeadersContain(table.headers, "课程名称") ||
			(!graduateHeadersContain(table.headers, "考试时间") &&
				!graduateHeadersContain(table.headers, "考试日期")) {
			continue
		}
		rows := graduateTableRows(table)
		result := make([]domain.Exam, 0, len(rows))
		for index, row := range rows {
			name := fieldString(row, "课程名称", "考试课程")
			if name == "" {
				continue
			}
			start, end := graduateExamTimes(row)
			code := fieldString(row, "开课号", "课程编号", "课程号")
			result = append(result, domain.Exam{
				ID:         fmt.Sprintf("%s:%s:%d", periodID, code, index+1),
				PeriodID:   periodID,
				CourseCode: code,
				CourseName: name,
				StartAt:    start,
				EndAt:      end,
				Campus:     fieldString(row, "校区", "考试校区"),
				Location:   fieldString(row, "考试地点", "考场", "地点"),
				Seat:       fieldString(row, "座位号", "座号"),
				Phase:      graduateExamPhase(fieldString(row, "考试性质", "考试类型")),
				Method:     fieldString(row, "考试方式"),
				Materials:  fieldString(row, "可带资料"),
				Notice:     fieldString(row, "备注"),
			})
		}
		return result, nil
	}
	if graduateExamPage(body) {
		return []domain.Exam{}, nil
	}
	return nil, application.ErrProviderUnavailable
}

func graduatePeriod(
	academicYear string,
	term string,
	now time.Time,
) (domain.Period, bool) {
	match := graduateAcademicYearPattern.FindStringSubmatch(strings.TrimSpace(academicYear))
	if len(match) != 3 {
		return domain.Period{}, false
	}
	startYear, startErr := strconv.Atoi(match[1])
	endYear, endErr := strconv.Atoi(match[2])
	if startErr != nil || endErr != nil || endYear != startYear+1 {
		return domain.Period{}, false
	}
	term = strings.TrimSpace(term)
	termCode := ""
	var startDate time.Time
	switch term {
	case "夏秋", "秋":
		termCode = graduateAutumnTermCode
		startDate = time.Date(startYear, time.September, 1, 0, 0, 0, 0, shanghaiLocation)
	case "春", "春季":
		termCode = graduateSpringTermCode
		startDate = time.Date(endYear, time.March, 1, 0, 0, 0, 0, shanghaiLocation)
	default:
		return domain.Period{}, false
	}
	id := fmt.Sprintf("%d:%s", startYear, termCode)
	return domain.Period{
		ID:         id,
		Label:      strings.TrimSpace(academicYear) + "学年 " + term,
		ShortLabel: match[1][2:] + "-" + match[2][2:] + " " + term,
		StartDate:  startDate,
		WeekCount:  graduateWeekCount,
		IsCurrent:  id == currentGraduatePeriodID(now),
	}, true
}

func currentGraduatePeriodID(now time.Time) string {
	year := now.In(shanghaiLocation).Year()
	month := now.In(shanghaiLocation).Month()
	switch {
	case month >= time.July:
		return fmt.Sprintf("%d:%s", year, graduateAutumnTermCode)
	case month <= time.February:
		return fmt.Sprintf("%d:%s", year-1, graduateAutumnTermCode)
	default:
		return fmt.Sprintf("%d:%s", year-1, graduateSpringTermCode)
	}
}

func graduateGradeValue(
	value string,
) (*float64, domain.GradeType, *domain.GradeLevel, bool) {
	rawValue := strings.TrimSpace(value)
	if rawValue == "" {
		return nil, "", nil, false
	}
	if _, unreleased := graduateUnreleasedGradeValues[rawValue]; unreleased {
		return nil, "", nil, false
	}
	if strings.Contains(rawValue, "正在申请免修") {
		return nil, "", nil, false
	}
	if strings.Contains(rawValue, string(domain.GradeLevelExempt)) {
		level := domain.GradeLevelExempt
		return nil, domain.GradeTypeLevel, &level, true
	}
	value = normalizeGraduateGradeText(rawValue)
	if value == "" {
		return nil, "", nil, false
	}
	if _, unreleased := graduateUnreleasedGradeValues[value]; unreleased {
		return nil, "", nil, false
	}
	if score, err := strconv.ParseFloat(value, 64); err == nil {
		if score < 0 || score > 100 {
			return nil, "", nil, false
		}
		return &score, domain.GradeTypeNumber, nil, true
	}
	if level, ok := normalizeGradeLevel(value); ok {
		return nil, domain.GradeTypeLevel, &level, true
	}
	level := domain.GradeLevel(value)
	return nil, domain.GradeTypeLevel, &level, true
}

func normalizeGraduateGradeText(value string) string {
	value = strings.TrimSpace(value)
	if separator := strings.LastIndex(value, "|"); separator >= 0 {
		value = value[separator+1:]
	}
	return strings.TrimSpace(value)
}

type graduateCellPlacement struct {
	node    *html.Node
	column  int
	rowspan int
}

func graduateRowPlacements(
	row []*html.Node,
	activeRowSpans map[int]int,
) []graduateCellPlacement {
	placements := make([]graduateCellPlacement, 0, len(row))
	newRowSpans := make(map[int]int)
	column := 0
	for _, cell := range row {
		for activeRowSpans[column] > 0 {
			column++
		}
		rowspan := positiveHTMLSpan(cell, "rowspan")
		colspan := positiveHTMLSpan(cell, "colspan")
		placements = append(placements, graduateCellPlacement{
			node:    cell,
			column:  column,
			rowspan: rowspan,
		})
		if rowspan > 1 {
			for offset := 0; offset < colspan; offset++ {
				newRowSpans[column+offset] = rowspan - 1
			}
		}
		column += colspan
	}
	for occupiedColumn, remaining := range activeRowSpans {
		if remaining <= 1 {
			delete(activeRowSpans, occupiedColumn)
		} else {
			activeRowSpans[occupiedColumn] = remaining - 1
		}
	}
	for occupiedColumn, remaining := range newRowSpans {
		activeRowSpans[occupiedColumn] = remaining
	}
	return placements
}

func graduateSection(placements []graduateCellPlacement, rowIndex int) int {
	for _, placement := range placements {
		if placement.column > 1 {
			continue
		}
		if value := fieldInt(
			map[string]any{"section": compactNodeText(placement.node)},
			"section",
		); value > 0 && value <= 30 {
			return value
		}
	}
	return rowIndex + 1
}

func graduateCoursesFromCell(
	cell *html.Node,
	periodID string,
	weekday int,
	startSection int,
	rowspan int,
	index int,
) []domain.Course {
	nodes := graduateCourseNodes(cell)
	result := make([]domain.Course, 0, len(nodes))
	for nodeIndex, node := range nodes {
		course, ok := graduateCourseFromNode(
			node,
			periodID,
			weekday,
			startSection,
			rowspan,
			index+nodeIndex,
		)
		if ok {
			result = append(result, course)
		}
	}
	return result
}

func graduateCourseNodes(cell *html.Node) []*html.Node {
	nodes := make([]*html.Node, 0)
	walk(cell, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "a" {
			return
		}
		if hasClass(node, "c666") || graduateCourseStrongName(node) != "" {
			nodes = append(nodes, node)
		}
	})
	if len(nodes) == 0 {
		return []*html.Node{cell}
	}
	return nodes
}

func graduateCourseFromNode(
	node *html.Node,
	periodID string,
	weekday int,
	startSection int,
	rowspan int,
	index int,
) (domain.Course, bool) {
	lines := graduateNodeLines(node)
	if len(lines) == 0 {
		return domain.Course{}, false
	}
	attributes := graduateMetadataAttributes(node)
	allText := strings.Join(append(lines, attributes...), "\n")
	name := graduateLabeledValue(lines, "课程名称")
	if name == "" {
		name = graduateCourseStrongName(node)
	}
	code := graduateLabeledValue(lines, "课程编号", "课程号")
	if code == "" {
		code = graduateCourseLinkID(node)
	}
	teacher := graduateLabeledValue(lines, "任课教师", "教师")
	location := graduateLabeledValue(lines, "上课地点", "地点", "教室")
	campus := graduateLabeledValue(lines, "校区")
	if name == "" {
		for _, line := range lines {
			if graduateCourseMetadataLine(line) {
				continue
			}
			name = line
			break
		}
	}
	if name == "" {
		return domain.Course{}, false
	}
	if teacher == "" || location == "" {
		metadata := graduateUnlabeledCourseMetadata(lines, name)
		if teacher == "" && len(metadata) > 0 {
			teacher = metadata[0]
			metadata = metadata[1:]
		}
		if location == "" && len(metadata) > 0 {
			location = strings.Join(metadata, " ")
		}
	}
	weeks := parseGraduateWeeks(allText)
	if len(weeks) == 0 {
		weeks = integerRange(1, graduateWeekCount)
	}
	if rowspan < 1 {
		rowspan = 1
	}
	return domain.Course{
		ID:           graduateCourseID(periodID, weekday, startSection, code, index),
		PeriodID:     periodID,
		CourseCode:   code,
		Name:         name,
		Teacher:      teacher,
		Campus:       campus,
		Location:     location,
		Weekday:      weekday,
		StartSection: startSection,
		EndSection:   startSection + rowspan - 1,
		Weeks:        weeks,
	}, true
}

func graduateCourseStrongName(node *html.Node) string {
	name := ""
	walk(node, func(item *html.Node) {
		if name != "" ||
			item.Type != html.ElementNode ||
			item.Data != "strong" ||
			!hasClass(item, "f14") {
			return
		}
		name = compactNodeText(item)
	})
	return strings.TrimSpace(name)
}

func graduateCourseLinkID(node *html.Node) string {
	link := node
	if link.Type != html.ElementNode || link.Data != "a" {
		link = nil
		walk(node, func(item *html.Node) {
			if link == nil && item.Type == html.ElementNode && item.Data == "a" {
				link = item
			}
		})
	}
	if link == nil {
		return ""
	}
	reference, err := url.Parse(strings.TrimSpace(attribute(link, "href")))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(reference.Query().Get("kcId"))
}

func graduateUnlabeledCourseMetadata(lines []string, name string) []string {
	result := make([]string, 0, 2)
	for _, line := range lines {
		value := strings.TrimSpace(line)
		if value == "" ||
			value == name ||
			strings.Trim(value, " | ") == "" ||
			graduateCourseMetadataLine(value) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func graduateCourseID(
	periodID string,
	weekday int,
	startSection int,
	code string,
	index int,
) string {
	identity := strings.TrimSpace(code)
	if identity == "" {
		identity = strconv.Itoa(index + 1)
	}
	return fmt.Sprintf("%s:%d:%d:%s", periodID, weekday, startSection, identity)
}

func mergeGraduateCourse(
	courses []domain.Course,
	course domain.Course,
) []domain.Course {
	course.Weeks = uniqueGraduateWeeks(course.Weeks)

	// The graduate grid emits one anchor for every week fragment. First merge
	// fragments that occupy the same day and sections, regardless of the
	// fragment's week set. This mirrors get_week_course_by_select_history,
	// which unions all week fragments under one placement key.
	exactIndex := -1
	for index := 0; index < len(courses); index++ {
		current := &courses[index]
		if !sameGraduateCourseIdentity(*current, course) ||
			current.StartSection != course.StartSection ||
			current.EndSection != course.EndSection {
			continue
		}
		if exactIndex == -1 {
			exactIndex = index
			current.Weeks = mergeGraduateWeeks(current.Weeks, course.Weeks)
			continue
		}
		courses[exactIndex].Weeks = mergeGraduateWeeks(courses[exactIndex].Weeks, current.Weeks)
		courses = append(courses[:index], courses[index+1:]...)
		index--
	}
	if exactIndex == -1 {
		courses = append(courses, course)
	}

	// Once a placement has collected all of its week fragments, adjacent
	// sections with the same identity and week set can be collapsed into one
	// course. Repeat until no pair remains so a late week fragment can also
	// unlock an adjacent merge.
	for {
		merged := false
		for leftIndex := 0; leftIndex < len(courses) && !merged; leftIndex++ {
			for rightIndex := leftIndex + 1; rightIndex < len(courses); rightIndex++ {
				left := &courses[leftIndex]
				right := courses[rightIndex]
				if !sameGraduateCoursePlacement(*left, right) ||
					!graduateCourseIntervalsTouch(*left, right) {
					continue
				}
				if right.StartSection < left.StartSection {
					left.StartSection = right.StartSection
				}
				if right.EndSection > left.EndSection {
					left.EndSection = right.EndSection
				}
				courses = append(courses[:rightIndex], courses[rightIndex+1:]...)
				merged = true
				break
			}
		}
		if !merged {
			break
		}
	}
	return courses
}

func sameGraduateCourseIdentity(left, right domain.Course) bool {
	if left.PeriodID != right.PeriodID ||
		left.Weekday != right.Weekday ||
		left.Name != right.Name ||
		left.Teacher != right.Teacher ||
		left.Campus != right.Campus ||
		left.Location != right.Location ||
		left.Note != right.Note {
		return false
	}
	if left.CourseCode != "" || right.CourseCode != "" {
		return left.CourseCode == right.CourseCode
	}
	return true
}

func sameGraduateCoursePlacement(left, right domain.Course) bool {
	return sameGraduateCourseIdentity(left, right) &&
		sameIntegerValues(uniqueGraduateWeeks(left.Weeks), uniqueGraduateWeeks(right.Weeks))
}

func graduateCourseIntervalsTouch(left, right domain.Course) bool {
	return right.StartSection <= left.EndSection+1 &&
		right.EndSection >= left.StartSection-1
}

func mergeGraduateWeeks(left, right []int) []int {
	return uniqueGraduateWeeks(append(append([]int{}, left...), right...))
}

func uniqueGraduateWeeks(weeks []int) []int {
	seen := make(map[int]struct{}, len(weeks))
	for _, week := range weeks {
		if week > 0 {
			seen[week] = struct{}{}
		}
	}
	result := make([]int, 0, len(seen))
	for week := range seen {
		result = append(result, week)
	}
	sort.Ints(result)
	return result
}

func sameIntegerValues(left, right []int) bool {
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

func graduateLabeledValue(lines []string, labels ...string) string {
	for _, line := range lines {
		match := graduateLabeledValuePattern.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		for _, label := range labels {
			if strings.HasPrefix(line, label+"：") || strings.HasPrefix(line, label+":") {
				return strings.TrimSpace(match[1])
			}
		}
	}
	return ""
}

func graduateCourseMetadataLine(value string) bool {
	value = normalizeGraduateWeekText(value)
	for _, prefix := range []string{
		"课程编号",
		"课程号",
		"任课教师",
		"教师",
		"上课地点",
		"地点",
		"教室",
		"校区",
		"周次",
	} {
		if strings.HasPrefix(value, prefix+"：") || strings.HasPrefix(value, prefix+":") {
			return true
		}
	}
	return graduateWeekExpressionPattern.MatchString(value) ||
		strings.Contains(value, "单周") ||
		strings.Contains(value, "双周") ||
		graduateSectionPattern.MatchString(value)
}

func parseGraduateWeeks(value string) []int {
	value = normalizeGraduateWeekText(value)
	seen := make(map[int]struct{})
	for _, match := range graduateWeekExpressionPattern.FindAllStringSubmatch(value, -1) {
		if len(match) == 2 {
			addGraduateWeekExpression(seen, match[1])
		}
	}
	// Some pages render only “单周”/“双周” without a numeric expression.
	// Keep the same 23-week boundary as the graduate academic calendar.
	if len(seen) == 0 {
		switch {
		case strings.Contains(value, "全周") || strings.Contains(value, "全"):
			for week := 1; week <= graduateWeekCount; week++ {
				seen[week] = struct{}{}
			}
		case strings.Contains(value, "单周"):
			for week := 1; week <= graduateWeekCount; week += 2 {
				seen[week] = struct{}{}
			}
		case strings.Contains(value, "双周"):
			for week := 2; week <= graduateWeekCount; week += 2 {
				seen[week] = struct{}{}
			}
		}
	}
	weeks := make([]int, 0, len(seen))
	for week := range seen {
		weeks = append(weeks, week)
	}
	sort.Ints(weeks)
	return weeks
}

func normalizeGraduateWeekText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return r
	}, value)
}

func addGraduateWeekExpression(seen map[int]struct{}, expression string) {
	expression = strings.Map(func(r rune) rune {
		switch r {
		case '０':
			return '0'
		case '１':
			return '1'
		case '２':
			return '2'
		case '３':
			return '3'
		case '４':
			return '4'
		case '５':
			return '5'
		case '６':
			return '6'
		case '７':
			return '7'
		case '８':
			return '8'
		case '９':
			return '9'
		default:
			return r
		}
	}, expression)
	expression = strings.TrimSpace(strings.Trim(expression, "()（）[]【】"))
	expression = strings.ReplaceAll(expression, "第", "")
	expression = strings.ReplaceAll(expression, "周", "")
	if strings.Contains(expression, "*") || strings.Contains(expression, "全") {
		for week := 1; week <= graduateWeekCount; week++ {
			seen[week] = struct{}{}
		}
		return
	}
	for _, token := range strings.FieldsFunc(expression, func(r rune) bool {
		switch r {
		case ',', '，', '、', ';', '；', '|', '｜', '+', '＋', '/', '／':
			return true
		default:
			return false
		}
	}) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		parity := 0
		if strings.Contains(token, "单") {
			parity = 1
		} else if strings.Contains(token, "双") {
			parity = 2
		}
		token = strings.ReplaceAll(token, "单周", "")
		token = strings.ReplaceAll(token, "双周", "")
		token = strings.ReplaceAll(token, "单", "")
		token = strings.ReplaceAll(token, "双", "")
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if match := regexp.MustCompile(`^(\d+)\s*[-—~～至]\s*(\d+)$`).FindStringSubmatch(token); len(match) == 3 {
			start, startErr := strconv.Atoi(match[1])
			end, endErr := strconv.Atoi(match[2])
			if startErr != nil || endErr != nil {
				continue
			}
			if start > end {
				start, end = end, start
			}
			for week := start; week <= end; week++ {
				addGraduateWeekValue(seen, week, parity)
			}
			continue
		}
		for _, number := range regexp.MustCompile(`\d+`).FindAllString(token, -1) {
			week, err := strconv.Atoi(number)
			if err == nil {
				addGraduateWeekValue(seen, week, parity)
			}
		}
	}
}

func addGraduateWeekValue(seen map[int]struct{}, week int, parity int) {
	if week < 1 || week > graduateWeekCount {
		return
	}
	if parity == 1 && week%2 == 0 {
		return
	}
	if parity == 2 && week%2 != 0 {
		return
	}
	seen[week] = struct{}{}
}

func integerRange(start int, end int) []int {
	result := make([]int, 0, end-start+1)
	for value := start; value <= end; value++ {
		result = append(result, value)
	}
	return result
}

func graduateExamTimes(row map[string]any) (time.Time, time.Time) {
	timeRange := fieldString(row, "考试时间")
	if start, end, ok := parseExamTimeRange(timeRange); ok {
		return start, end
	}
	date := fieldString(row, "考试日期")
	if match := graduateExamClockRangePattern.FindStringSubmatch(timeRange); len(match) == 3 {
		start := graduateDateTime(date, match[1])
		end := graduateDateTime(date, match[2])
		if !start.IsZero() && end.After(start) {
			return start, end
		}
	}
	startText := fieldString(row, "开始时间", "考试开始时间")
	endText := fieldString(row, "结束时间", "考试结束时间")
	start := fieldTime(row, "开始时间", "考试开始时间", "考试时间")
	end := fieldTime(row, "结束时间", "考试结束时间")
	if date != "" && startText != "" {
		start = graduateDateTime(date, startText)
	}
	if date != "" && endText != "" {
		end = graduateDateTime(date, endText)
	}
	if end.IsZero() && !start.IsZero() {
		end = start.Add(2 * time.Hour)
	}
	return start, end
}

func graduateDateTime(date string, clock string) time.Time {
	value := strings.TrimSpace(date) + " " + strings.TrimSpace(clock)
	for _, layout := range []string{
		"2006-01-02 15:04",
		"2006/01/02 15:04",
		"2006年01月02日 15:04",
	} {
		if parsed, err := time.ParseInLocation(layout, value, shanghaiLocation); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func graduateExamPhase(value string) domain.ExamPhase {
	for _, phase := range []domain.ExamPhase{
		domain.ExamPhaseMidterm,
		domain.ExamPhaseMakeup,
		domain.ExamPhaseEntrance,
		domain.ExamPhaseFinal,
	} {
		if strings.Contains(value, string(phase)) {
			return phase
		}
	}
	return domain.ExamPhaseFinal
}

func graduateExamPage(body []byte) bool {
	text := string(body)
	return strings.Contains(text, "考试安排") &&
		strings.Contains(text, `name="xn"`) &&
		strings.Contains(text, `name="xj"`)
}

func findGraduateHTMLTable(
	body []byte,
	requiredHeaders ...string,
) (graduateHTMLTable, bool, error) {
	tables, err := graduateHTMLTables(body)
	if err != nil {
		return graduateHTMLTable{}, false, err
	}
	for _, table := range tables {
		if graduateHeadersContain(table.headers, requiredHeaders...) {
			return table, true, nil
		}
	}
	return graduateHTMLTable{}, false, nil
}

func graduateHTMLTables(body []byte) ([]graduateHTMLTable, error) {
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	nodes := make([]*html.Node, 0)
	walk(root, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			nodes = append(nodes, node)
		}
	})
	result := make([]graduateHTMLTable, 0, len(nodes))
	for _, tableNode := range nodes {
		rows := directTableRows(tableNode)
		for headerIndex, row := range rows {
			headers := make([]string, 0, len(row))
			hasHeader := false
			for _, cell := range row {
				if cell.Data == "th" {
					hasHeader = true
				}
				headers = append(headers, graduateCellText(cell))
			}
			if !hasHeader {
				continue
			}
			result = append(result, graduateHTMLTable{
				headers: headers,
				rows:    rows[headerIndex+1:],
			})
			break
		}
	}
	return result, nil
}

func directTableRows(table *html.Node) [][]*html.Node {
	rows := make([][]*html.Node, 0)
	walk(table, func(node *html.Node) {
		if node.Type != html.ElementNode ||
			node.Data != "tr" ||
			nearestTable(node) != table {
			return
		}
		cells := make([]*html.Node, 0)
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode &&
				(child.Data == "th" || child.Data == "td") {
				cells = append(cells, child)
			}
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	})
	return rows
}

func nearestTable(node *html.Node) *html.Node {
	for current := node.Parent; current != nil; current = current.Parent {
		if current.Type == html.ElementNode && current.Data == "table" {
			return current
		}
	}
	return nil
}

func graduateHeadersContain(headers []string, required ...string) bool {
	values := make(map[string]struct{}, len(headers))
	for _, header := range headers {
		values[strings.TrimSpace(header)] = struct{}{}
	}
	for _, header := range required {
		if _, exists := values[header]; !exists {
			return false
		}
	}
	return true
}

func graduateTableRows(table graduateHTMLTable) []map[string]any {
	result := make([]map[string]any, 0, len(table.rows))
	for _, cells := range table.rows {
		if len(cells) < len(table.headers) {
			continue
		}
		row := make(map[string]any, len(table.headers))
		for index, header := range table.headers {
			row[header] = graduateCellText(cells[index])
		}
		result = append(result, row)
	}
	return result
}

func graduateCellText(node *html.Node) string {
	return strings.Join(strings.Fields(nodeText(node)), " ")
}

func positiveHTMLSpan(node *html.Node, name string) int {
	for _, attribute := range node.Attr {
		if !strings.EqualFold(attribute.Key, name) {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(attribute.Val))
		if err == nil && value > 0 && value <= 30 {
			return value
		}
	}
	return 1
}

func graduateNodeLines(node *html.Node) []string {
	lines := make([]string, 0)
	var current strings.Builder
	flush := func() {
		value := strings.TrimSpace(current.String())
		current.Reset()
		if value != "" {
			lines = append(lines, value)
		}
	}
	var visit func(*html.Node)
	visit = func(item *html.Node) {
		if item.Type == html.ElementNode &&
			(item.Data == "script" || item.Data == "style") {
			return
		}
		if item.Type == html.ElementNode && item.Data == "br" {
			flush()
			return
		}
		block := item.Type == html.ElementNode &&
			(item.Data == "div" || item.Data == "p" || item.Data == "li")
		if block {
			flush()
		}
		if item.Type == html.TextNode {
			if current.Len() > 0 {
				current.WriteString(" ")
			}
			current.WriteString(item.Data)
		}
		for child := item.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
		if block {
			flush()
		}
	}
	visit(node)
	flush()
	return lines
}

func graduateMetadataAttributes(node *html.Node) []string {
	result := make([]string, 0, 4)
	walk(node, func(item *html.Node) {
		if item.Type != html.ElementNode {
			return
		}
		for _, attribute := range item.Attr {
			switch strings.ToLower(attribute.Key) {
			case "title", "data-content", "data-original-title":
				if value := strings.TrimSpace(attribute.Val); value != "" {
					result = append(result, value)
				}
			}
		}
	})
	return result
}
