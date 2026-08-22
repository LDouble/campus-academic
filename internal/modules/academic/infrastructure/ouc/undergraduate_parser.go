package ouc

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"golang.org/x/net/html"
)

var (
	undergraduatePeriodIDPattern = regexp.MustCompile(`^\d{4}-\d{4}-[123]$`)
	scheduleWeekGroupPattern     = regexp.MustCompile(`(?:(?:单|双)周?\s*)?\d+(?:\s*[-—~～至]\s*\d+)?(?:\s*[,，、]\s*\d+(?:\s*[-—~～至]\s*\d+)?)*\s*周(?:\s*[（(]?\s*(?:单|双)周?\s*[）)]?)?`)
	scheduleRangePattern         = regexp.MustCompile(`[-—~～至]`)
	scheduleSectionPattern       = regexp.MustCompile(`(?:第\s*)?(\d+)(?:\s*[-—~～至]\s*(\d+))?\s*节`)
	scheduleNumberPattern        = regexp.MustCompile(`\d+`)
)

func parseUndergraduatePeriods(body []byte, encoding string) ([]domain.Period, error) {
	if encoding != "html" {
		return parsePeriods(body, encoding)
	}
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, application.ErrProviderUnavailable
	}
	selector := findElement(root, func(node *html.Node) bool {
		return node.Data == "select" &&
			(attribute(node, "name") == "xnxq01id" || attribute(node, "id") == "xnxq01id")
	})
	if selector == nil {
		return nil, application.ErrProviderUnavailable
	}
	options := childElements(selector, "option")
	periods := make([]domain.Period, 0, len(options))
	for _, option := range options {
		id := strings.TrimSpace(attribute(option, "value"))
		label := compactText(option)
		if !undergraduatePeriodIDPattern.MatchString(id) || label == "" {
			continue
		}
		periods = append(periods, domain.Period{
			ID:         id,
			Label:      label,
			ShortLabel: label,
			StartDate:  inferredUndergraduatePeriodStart(id),
			WeekCount:  20,
			IsCurrent:  hasAttribute(option, "selected"),
		})
	}
	if len(periods) == 0 {
		return nil, application.ErrProviderUnavailable
	}
	return periods, nil
}

func parseUndergraduateCourses(
	body []byte,
	encoding string,
	periodID string,
) (domain.CourseSchedule, error) {
	if encoding != "html" {
		courses, err := parseCourses(body, encoding, periodID)
		return domain.CourseSchedule{Courses: courses}, err
	}
	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}
	table := findElement(root, func(node *html.Node) bool {
		return node.Data == "table" && hasClass(node, "qz-weeklyTable")
	})
	items := findElements(root, func(node *html.Node) bool {
		return node.Data == "li" && hasClass(node, "qz-toolitiplists")
	})
	if table == nil && len(items) == 0 {
		return domain.CourseSchedule{}, application.ErrProviderUnavailable
	}

	courses := make([]domain.Course, 0, len(items))
	for _, item := range items {
		course, ok := parseUndergraduateCourseItem(item, periodID)
		if !ok {
			continue
		}
		if course.Weekday == 0 {
			course.Weekday = weekdayFromTable(item)
		}
		if course.Weekday == 0 {
			continue
		}
		course.ID = derivedUndergraduateCourseID(course)
		courses = append(courses, course)
	}
	return domain.CourseSchedule{
		Courses:      mergeUndergraduateCourses(courses),
		ScheduleNote: parseUndergraduateScheduleNote(root),
	}, nil
}

func parseUndergraduateCourseItem(item *html.Node, periodID string) (domain.Course, bool) {
	nameNode := findElement(item, func(node *html.Node) bool {
		return hasClass(node, "qz-tooltipContent-title")
	})
	if nameNode == nil {
		return domain.Course{}, false
	}
	name := compactText(nameNode)
	if name == "" {
		return domain.Course{}, false
	}

	details := make(map[string]string)
	for _, child := range findElements(item, func(node *html.Node) bool {
		return hasClass(node, "qz-tooltipContent-detailitem")
	}) {
		key, value := splitLabeledText(compactText(child))
		key = undergraduateScheduleDetailKey(key)
		if key != "" {
			details[key] = value
		}
	}
	weeks, startSection, endSection := parseUndergraduateScheduleFields(
		details["weeks"], details["sections"], details["legacy_time"],
	)
	if len(weeks) == 0 || startSection <= 0 || endSection < startSection {
		return domain.Course{}, false
	}
	weekday := weekdayFromScheduleText(details["weeks"])
	if weekday == 0 {
		weekday = weekdayFromScheduleText(details["legacy_time"])
	}
	location := strings.TrimSpace(details["location"])
	if location == "" || location == "()" {
		location = strings.TrimSpace(details["note"])
	}
	selectionID := strings.TrimSpace(details["selection_id"])
	courseCode := selectionID
	if courseCode == "" {
		courseCode = strings.TrimSpace(details["course_code"])
	}
	course := domain.Course{
		PeriodID:     periodID,
		CourseCode:   courseCode,
		Name:         name,
		Teacher:      strings.TrimSpace(details["teacher"]),
		Campus:       strings.TrimSpace(details["campus"]),
		Location:     location,
		Note:         strings.TrimSpace(details["note"]),
		Weekday:      weekday,
		StartSection: startSection,
		EndSection:   endSection,
		Weeks:        weeks,
	}
	return course, true
}

func parseScheduleTime(value string) ([]int, int, int) {
	startSection, endSection := parseScheduleSections(value)
	return parseScheduleWeeks(value), startSection, endSection
}

func parseUndergraduateScheduleFields(weeksValue, sectionsValue, legacyValue string) ([]int, int, int) {
	weeks := parseScheduleWeeks(weeksValue)
	startSection, endSection := parseScheduleSections(sectionsValue)
	if legacyValue != "" && (len(weeks) == 0 || startSection <= 0) {
		legacyWeeks, legacyStart, legacyEnd := parseScheduleTime(legacyValue)
		if len(weeks) == 0 {
			weeks = legacyWeeks
		}
		if startSection <= 0 {
			startSection, endSection = legacyStart, legacyEnd
		}
	}
	return weeks, startSection, endSection
}

func parseScheduleWeeks(value string) []int {
	head := strings.TrimSpace(value)
	if weekdayIndex := strings.Index(head, "星期"); weekdayIndex >= 0 {
		head = head[:weekdayIndex]
	}
	matches := scheduleWeekGroupPattern.FindAllStringIndex(head, -1)
	if len(matches) == 0 {
		return nil
	}
	weekSet := make(map[int]struct{})
	for _, match := range matches {
		raw := head[match[0]:match[1]]
		parity := 0
		if strings.Contains(raw, "单") && !strings.Contains(raw, "双") {
			parity = 1
		} else if strings.Contains(raw, "双") && !strings.Contains(raw, "单") {
			parity = 2
		}
		for _, part := range strings.FieldsFunc(raw, func(character rune) bool {
			return character == ',' || character == '，' || character == '、'
		}) {
			numbers := scheduleNumberPattern.FindAllString(part, -1)
			if len(numbers) == 0 {
				continue
			}
			if len(numbers) >= 2 && scheduleRangePattern.MatchString(part) {
				start, startErr := strconv.Atoi(numbers[0])
				end, endErr := strconv.Atoi(numbers[1])
				if startErr != nil || endErr != nil {
					continue
				}
				if start > end {
					start, end = end, start
				}
				for week := start; week <= end; week++ {
					addScheduleWeek(weekSet, week, parity)
				}
				continue
			}
			for _, number := range numbers {
				week, parseErr := strconv.Atoi(number)
				if parseErr == nil {
					addScheduleWeek(weekSet, week, parity)
				}
			}
		}
	}
	weeks := make([]int, 0, len(weekSet))
	for week := range weekSet {
		weeks = append(weeks, week)
	}
	sort.Ints(weeks)
	return weeks
}

func addScheduleWeek(weekSet map[int]struct{}, week, parity int) {
	if week <= 0 || week > 30 {
		return
	}
	if parity == 1 && week%2 == 0 {
		return
	}
	if parity == 2 && week%2 != 0 {
		return
	}
	weekSet[week] = struct{}{}
}

func parseScheduleSections(value string) (int, int) {
	match := scheduleSectionPattern.FindStringSubmatch(value)
	if len(match) < 2 {
		return 0, 0
	}
	start, startErr := strconv.Atoi(match[1])
	if startErr != nil || start <= 0 {
		return 0, 0
	}
	end := start
	if len(match) > 2 && match[2] != "" {
		var endErr error
		end, endErr = strconv.Atoi(match[2])
		if endErr != nil || end < start {
			return 0, 0
		}
	}
	return start, end
}

func weekdayFromScheduleText(value string) int {
	for index, weekday := range []string{"一", "二", "三", "四", "五", "六"} {
		if strings.Contains(value, "星期"+weekday) || strings.Contains(value, "周"+weekday) {
			return index + 1
		}
	}
	if strings.Contains(value, "星期日") || strings.Contains(value, "星期天") ||
		strings.Contains(value, "星期七") || strings.Contains(value, "周日") ||
		strings.Contains(value, "周天") || strings.Contains(value, "周七") {
		return 7
	}
	return 0
}

func undergraduateScheduleDetailKey(key string) string {
	switch strings.TrimSpace(key) {
	case "教师", "老师", "任课教师", "任课老师":
		return "teacher"
	case "上课地点", "地点", "教室":
		return "location"
	case "周次":
		return "weeks"
	case "节次":
		return "sections"
	case "课表备注":
		return "note"
	case "时间":
		return "legacy_time"
	case "课程号", "课程编号":
		return "course_code"
	case "选课号":
		return "selection_id"
	case "校区":
		return "campus"
	default:
		return strings.TrimSpace(key)
	}
}

func weekdayFromTable(item *html.Node) int {
	cell := ancestorElement(item, "td")
	if cell == nil {
		cell = ancestorElement(item, "th")
	}
	if cell == nil {
		return 0
	}
	row := ancestorElement(cell, "tr")
	if row == nil {
		return 0
	}
	cells := directTableCells(row)
	labelIndex := -1
	for index, current := range cells {
		if hasClass(current, "qz-weeklyTable-label") {
			labelIndex = index
			break
		}
	}
	if labelIndex < 0 {
		return 0
	}
	weekday := 1
	for index := labelIndex + 1; index < len(cells); index++ {
		if cells[index] == cell {
			return weekday
		}
		weekday += positiveAttributeInt(
			cells[index],
			"colsize",
			positiveAttributeInt(cells[index], "colspan", 1),
		)
	}
	return 0
}

func ancestorElement(node *html.Node, tag string) *html.Node {
	for current := node; current != nil; current = current.Parent {
		if current.Type == html.ElementNode && current.Data == tag {
			return current
		}
	}
	return nil
}

func mergeUndergraduateCourses(courses []domain.Course) []domain.Course {
	result := make([]domain.Course, 0, len(courses))
	indexes := make(map[string]int, len(courses))
	for _, course := range courses {
		key := strings.Join([]string{
			course.ID,
			course.CourseCode,
			course.Name,
			course.Teacher,
			course.Campus,
			course.Location,
			course.Note,
			strconv.Itoa(course.Weekday),
			strconv.Itoa(course.StartSection),
			strconv.Itoa(course.EndSection),
		}, "\x00")
		if index, exists := indexes[key]; exists {
			result[index].Weeks = mergeScheduleWeeks(result[index].Weeks, course.Weeks)
			continue
		}
		course.Weeks = append([]int(nil), course.Weeks...)
		indexes[key] = len(result)
		result = append(result, course)
	}
	return result
}

func mergeScheduleWeeks(left, right []int) []int {
	set := make(map[int]struct{}, len(left)+len(right))
	for _, week := range left {
		set[week] = struct{}{}
	}
	for _, week := range right {
		set[week] = struct{}{}
	}
	merged := make([]int, 0, len(set))
	for week := range set {
		merged = append(merged, week)
	}
	sort.Ints(merged)
	return merged
}

func derivedUndergraduateCourseID(course domain.Course) string {
	value := strings.Join([]string{
		course.PeriodID,
		course.CourseCode,
		course.Name,
		course.Teacher,
		course.Campus,
		course.Location,
		course.Note,
		strconv.Itoa(course.Weekday),
		strconv.Itoa(course.StartSection),
		strconv.Itoa(course.EndSection),
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return "course-" + hex.EncodeToString(digest[:12])
}

func parseUndergraduateScheduleNote(root *html.Node) string {
	tfoot := findElement(root, func(node *html.Node) bool {
		return node.Data == "tfoot" && hasClass(node, "qz-weeklyTable-tfoot")
	})
	if tfoot == nil {
		return ""
	}
	for _, row := range findElements(tfoot, func(node *html.Node) bool { return node.Data == "tr" }) {
		label := findElement(row, func(node *html.Node) bool {
			return hasClass(node, "qz-weeklyTable-label")
		})
		if label == nil || strings.TrimSpace(compactText(label)) != "备注" {
			continue
		}
		detail := findElement(row, func(node *html.Node) bool {
			return hasClass(node, "qz-weeklyTable-detailtext")
		})
		if detail != nil {
			return truncateScheduleNote(compactText(detail))
		}
	}
	return ""
}

func truncateScheduleNote(value string) string {
	const maxLength = 2000
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxLength {
		runes = runes[:maxLength]
	}
	return string(runes)
}

func inferredUndergraduatePeriodStart(periodID string) time.Time {
	parts := strings.Split(periodID, "-")
	if len(parts) != 3 {
		return time.Time{}
	}
	firstYear, firstErr := strconv.Atoi(parts[0])
	secondYear, secondErr := strconv.Atoi(parts[1])
	term, termErr := strconv.Atoi(parts[2])
	if firstErr != nil || secondErr != nil || termErr != nil || secondYear != firstYear+1 {
		return time.Time{}
	}
	year, month := firstYear, time.July
	switch term {
	case 2:
		month = time.September
	case 3:
		year, month = secondYear, time.March
	default:
		if term != 1 {
			return time.Time{}
		}
	}
	start := time.Date(year, month, 1, 0, 0, 0, 0, shanghaiLocation)
	for start.Weekday() != time.Monday {
		start = start.AddDate(0, 0, 1)
	}
	return start
}

func splitLabeledText(value string) (string, string) {
	index := strings.IndexAny(value, "：:")
	if index < 0 {
		return "", ""
	}
	separatorSize := 1
	if strings.HasPrefix(value[index:], "：") {
		separatorSize = len("：")
	}
	return strings.TrimSpace(value[:index]), strings.TrimSpace(value[index+separatorSize:])
}

func directTableCells(row *html.Node) []*html.Node {
	result := make([]*html.Node, 0, 8)
	for child := row.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && (child.Data == "td" || child.Data == "th") {
			result = append(result, child)
		}
	}
	return result
}

func childElements(node *html.Node, tag string) []*html.Node {
	result := make([]*html.Node, 0)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == tag {
			result = append(result, child)
		}
	}
	return result
}

func findElement(node *html.Node, match func(*html.Node) bool) *html.Node {
	if node.Type == html.ElementNode && match(node) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, match); found != nil {
			return found
		}
	}
	return nil
}

func findElements(node *html.Node, match func(*html.Node) bool) []*html.Node {
	var result []*html.Node
	walk(node, func(candidate *html.Node) {
		if candidate.Type == html.ElementNode && match(candidate) {
			result = append(result, candidate)
		}
	})
	return result
}

func hasClass(node *html.Node, className string) bool {
	for _, current := range strings.Fields(attribute(node, "class")) {
		if current == className {
			return true
		}
	}
	return false
}

func attribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func hasAttribute(node *html.Node, name string) bool {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return true
		}
	}
	return false
}

func positiveAttributeInt(node *html.Node, name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(attribute(node, name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func compactText(node *html.Node) string {
	return strings.Join(strings.Fields(nodeText(node)), " ")
}
