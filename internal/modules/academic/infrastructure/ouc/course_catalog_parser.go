package ouc

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

var (
	catalogTotalPattern    = regexp.MustCompile(`(?:共|总(?:计|数)?[：:]?)\s*(\d+)\s*条`)
	catalogPagesPattern    = regexp.MustCompile(`(?:共|总(?:计|数)?[：:]?)\s*(\d+)\s*页`)
	catalogPageLinkPattern = regexp.MustCompile(`(?i)pagingSubmit\(\s*['\"](\d+)['\"]\s*\)`)
)

func parseCourseCatalogPage(
	educationLevel string,
	periodID string,
	page int,
	operation academicconfig.OperationEndpoint,
	body []byte,
) (domain.CourseCatalogPage, error) {
	switch educationLevel {
	case verificationapp.EducationUndergraduate:
		if operation.ResponseEncoding != "json" {
			return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog must be JSON")
		}
		return parseUndergraduateCatalogPage(periodID, page, operation.PageSize, body)
	case verificationapp.EducationGraduate:
		if operation.ResponseEncoding != "html" {
			return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog must be HTML")
		}
		return parseGraduateCatalogPage(periodID, page, operation.PageSize, body)
	default:
		return domain.CourseCatalogPage{}, fmt.Errorf("unsupported catalog education level")
	}
}

func parseUndergraduateCatalogPage(
	periodID string,
	page int,
	configuredPageSize int,
	body []byte,
) (domain.CourseCatalogPage, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return domain.CourseCatalogPage{}, err
	}
	if code, exists := payload["code"]; exists && fieldString(map[string]any{"code": code}, "code") != "0" {
		return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog response rejected")
	}
	rawRows, ok := payload["data"].([]any)
	if !ok {
		return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog rows missing")
	}
	total, ok := catalogJSONInt(payload, "count", "total", "records")
	if !ok || total < 0 || total < len(rawRows) {
		return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog total missing or invalid")
	}
	pageSize := configuredPageSize
	if value, found := catalogJSONInt(payload, "limit", "pageSize", "size"); found && value > 0 {
		pageSize = value
	}
	if pageSize <= 0 || pageSize > 200 {
		return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog page size invalid")
	}
	pageCount := catalogPageCount(total, pageSize)
	if value, found := catalogJSONInt(payload, "pages", "pageCount", "totalPage"); found {
		if value != pageCount {
			return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog page count inconsistent")
		}
		pageCount = value
	}
	if pageCount > 0 && page > pageCount {
		return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog page exceeds total")
	}
	entries := make([]domain.CourseCatalogEntry, 0, len(rawRows))
	for _, raw := range rawRows {
		row, rowOK := raw.(map[string]any)
		if !rowOK {
			return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog row invalid")
		}
		entry := domain.CourseCatalogEntry{
			PeriodID: catalogValue(row, "xnxq01id", "xnxqid", "xnxq"), SourceKey: catalogValue(row, "xkh"), OpeningCode: catalogValue(row, "xkh"),
			CourseCode: catalogValue(row, "kch"), CourseName: catalogValue(row, "kcmc", "kc_mc"),
			Department: catalogValue(row, "skyx"), Teachers: catalogValue(row, "skjs"), Campus: catalogValue(row, "xqmc"),
			CourseType: catalogValue(row, "kccm"), Classes: catalogValue(row, "ktmc"),
			Schedule: catalogValue(row, "sksj"), Location: catalogValue(row, "skdd"), Note: catalogValue(row, "bz"),
		}
		if entry.PeriodID == "" {
			entry.PeriodID = periodID
		}
		if entry.PeriodID != periodID || entry.SourceKey == "" || entry.CourseName == "" {
			return domain.CourseCatalogPage{}, fmt.Errorf("undergraduate catalog row violates contract")
		}
		entries = append(entries, entry)
	}
	return domain.CourseCatalogPage{Page: page, PageSize: pageSize, TotalCount: total, TotalPages: pageCount, HasMore: page < pageCount, Entries: entries}, nil
}

func parseGraduateCatalogPage(
	periodID string,
	page int,
	configuredPageSize int,
	body []byte,
) (domain.CourseCatalogPage, error) {
	table, found, err := findGraduateHTMLTable(body, "学年", "学期", "开课号", "课程名称", "开课学院", "上课语言", "任课教师", "上课时间地点", "备注")
	if err != nil || !found || len(table.headers) < 9 {
		return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog table missing")
	}
	pageCount, ok := graduateCatalogPageCount(body)
	if !ok {
		return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog pagination missing")
	}
	if pageCount < 1 || page > pageCount {
		return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog pagination invalid")
	}
	if configuredPageSize <= 0 || configuredPageSize > 200 {
		return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog page size invalid")
	}
	rows := graduateTableRows(table)
	if len(rows) == 0 {
		return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog rows missing")
	}
	entries := make([]domain.CourseCatalogEntry, 0, len(rows))
	for _, row := range rows {
		termID, ok := graduateCatalogPeriodID(catalogValue(row, "学年"), catalogValue(row, "学期"))
		if !ok || termID != periodID {
			return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog period invalid")
		}
		openingCode := catalogValue(row, "开课号")
		courseName := catalogValue(row, "课程名称")
		department := catalogValue(row, "开课学院")
		language := catalogValue(row, "上课语言")
		teachers := catalogValue(row, "任课教师")
		schedule := catalogValue(row, "上课时间地点")
		entry := domain.CourseCatalogEntry{
			PeriodID: periodID, SourceKey: graduateCatalogSourceKey(
				openingCode, courseName, department, language, teachers, schedule,
			), OpeningCode: openingCode, CourseName: courseName,
			Department: department, Language: language,
			Teachers: teachers, Schedule: schedule,
			Note: catalogValue(row, "备注"),
		}
		if entry.SourceKey == "" || entry.CourseName == "" {
			return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog row violates contract")
		}
		entries = append(entries, entry)
	}
	total := 0
	if match := catalogTotalPattern.FindSubmatch(body); len(match) == 2 {
		parsed, parseErr := strconv.Atoi(string(match[1]))
		if parseErr != nil || parsed < len(entries) {
			return domain.CourseCatalogPage{}, fmt.Errorf("graduate catalog total invalid")
		}
		total = parsed
	} else if page == pageCount {
		total = (pageCount-1)*configuredPageSize + len(entries)
	}
	return domain.CourseCatalogPage{Page: page, PageSize: configuredPageSize, TotalCount: total, TotalPages: pageCount, HasMore: page < pageCount, Entries: entries}, nil
}

func graduateCatalogSourceKey(fields ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return "graduate:" + fmt.Sprintf("%x", digest[:])
}

func graduateCatalogPageCount(body []byte) (int, bool) {
	if match := catalogPagesPattern.FindSubmatch(body); len(match) == 2 {
		value, err := strconv.Atoi(string(match[1]))
		return value, err == nil && value > 0
	}
	maximum := 0
	for _, match := range catalogPageLinkPattern.FindAllSubmatch(body, -1) {
		value, err := strconv.Atoi(string(match[1]))
		if err == nil && value > maximum {
			maximum = value
		}
	}
	return maximum, maximum > 0
}

func catalogParseFailureKind(err error) string {
	message := err.Error()
	for _, kind := range []string{"encoding", "response", "rows", "total", "page", "pagination", "table", "period", "capacity", "contract"} {
		if strings.Contains(message, kind) {
			return kind
		}
	}
	return "invalid_payload"
}

func catalogJSONInt(payload map[string]any, names ...string) (int, bool) {
	for _, name := range names {
		value, found := payload[name]
		if !found {
			continue
		}
		parsed, err := strconv.Atoi(fieldString(map[string]any{"value": value}, "value"))
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func catalogPageCount(total int, pageSize int) int {
	if total == 0 {
		return 0
	}
	return (total + pageSize - 1) / pageSize
}

func catalogValue(row map[string]any, names ...string) string { return fieldString(row, names...) }

func graduateCatalogPeriodID(year string, term string) (string, bool) {
	period, ok := graduatePeriod(year, term, time.Now())
	return period.ID, ok
}
