package querycoord

import (
	"context"
	"errors"
	"testing"
)

type recordingRevoker struct {
	calls int
	err   error
}

func (r *recordingRevoker) DeleteStudent(context.Context, string) error {
	r.calls++
	return r.err
}

func TestCombinedRevokerAttemptsEveryStore(t *testing.T) {
	firstError := errors.New("session cleanup failed")
	first := &recordingRevoker{err: firstError}
	second := &recordingRevoker{}
	err := CombineRevokers(first, nil, second).DeleteStudent(context.Background(), "20260001")
	if !errors.Is(err, firstError) {
		t.Fatalf("combined error=%v, want first cleanup error", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("cleanup calls first=%d second=%d, want both 1", first.calls, second.calls)
	}
}
