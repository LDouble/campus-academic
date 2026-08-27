package infrastructure

import (
	"strings"
	"testing"
)

func TestSourceAggregateQueryMaterializesNormalizedRows(t *testing.T) {
	query := strings.ToUpper(sourceAggregateQueryTemplate)
	if count := strings.Count(query, "FROM GRADE_DETAILS"); count != 1 {
		t.Fatalf("source aggregate query references grade_details %d times, want 1", count)
	}
	if count := strings.Count(query, "NO_MERGE(VALID)"); count != 2 {
		t.Fatalf("source aggregate query contains %d valid CTE materialization hints, want 2", count)
	}
	if !strings.Contains(query, "UNION ALL") {
		t.Fatal("source aggregate query no longer preserves separate course and instructor groupings")
	}
	if !strings.Contains(query, "MAX_EXECUTION_TIME(%D)") {
		t.Fatal("source aggregate query does not enforce a MySQL server-side deadline")
	}
}

func TestSourceAggregateQueryPrefersRecognizedGradeText(t *testing.T) {
	query := strings.ToUpper(sourceAggregateQueryTemplate)

	if strings.Contains(query, "WHEN SCORE BETWEEN 0 AND 100 THEN SCORE") {
		t.Fatal("source aggregate query trusts the stored score before grade text")
	}
	if !strings.Contains(
		query,
		"WHEN SCORE_STR = '' AND SCORE BETWEEN 0 AND 100 THEN SCORE",
	) {
		t.Fatal("source aggregate query has no score fallback for rows without grade text")
	}
	for _, fragment := range []string{
		"'免修'",
		"'已批准免修'",
		"'优秀', '优'",
		"'不及格', '不合格', '未通过'",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("source aggregate query does not classify %s", fragment)
		}
	}
}

func TestSourceAggregateQueryNormalizesNullableCounts(t *testing.T) {
	query := strings.ToUpper(sourceAggregateQueryTemplate)

	for _, fragment := range []string{
		"COALESCE(SUM(NUMERIC_SCORE >= 60 AND NUMERIC_SCORE < 70), 0) AS SCORE_60_69_COUNT",
		"COALESCE(SUM(NUMERIC_SCORE >= 70 AND NUMERIC_SCORE < 80), 0) AS SCORE_70_79_COUNT",
		"COALESCE(SUM(NUMERIC_SCORE >= 80 AND NUMERIC_SCORE < 90), 0) AS SCORE_80_89_COUNT",
		"COALESCE(SUM(NUMERIC_SCORE >= 90 AND NUMERIC_SCORE <= 100), 0) AS SCORE_90_100_COUNT",
	} {
		if count := strings.Count(query, fragment); count != 2 {
			t.Fatalf("source aggregate query contains %q %d times, want 2", fragment, count)
		}
	}
}
