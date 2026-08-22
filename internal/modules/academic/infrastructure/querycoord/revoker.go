package querycoord

import (
	"context"
	"errors"
)

// StudentRevoker removes provider-owned state for one student number.
type StudentRevoker interface {
	DeleteStudent(context.Context, string) error
}

type combinedRevoker struct {
	revokers []StudentRevoker
}

// CombineRevokers returns one revoker that attempts every cleanup even when an
// earlier store fails, so partial outages do not skip independent deletion.
func CombineRevokers(revokers ...StudentRevoker) StudentRevoker {
	filtered := make([]StudentRevoker, 0, len(revokers))
	for _, revoker := range revokers {
		if revoker != nil {
			filtered = append(filtered, revoker)
		}
	}
	return &combinedRevoker{revokers: filtered}
}

func (r *combinedRevoker) DeleteStudent(ctx context.Context, studentNo string) error {
	var result error
	for _, revoker := range r.revokers {
		result = errors.Join(result, revoker.DeleteStudent(ctx, studentNo))
	}
	return result
}
