package querycoord

import (
	"testing"

	"github.com/weouc-plus/campus-academic/internal/modules/academic/application"
)

func TestQueryOutcomeMapsAccountRestricted(t *testing.T) {
	t.Parallel()
	if got := queryOutcome(application.ErrAccountRestricted); got != "account_restricted" {
		t.Fatalf("queryOutcome(account restricted)=%q want=%q", got, "account_restricted")
	}
}
