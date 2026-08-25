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
