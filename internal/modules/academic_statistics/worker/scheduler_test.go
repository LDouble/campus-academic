package worker

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/domain"
)

func TestDelayAfterRun(t *testing.T) {
	now := time.Date(2026, time.August, 24, 8, 0, 0, 0, time.UTC)
	retryDelay := 45 * time.Minute

	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{name: "success waits for daily schedule", want: 20 * time.Hour},
		{name: "empty source waits for daily schedule", err: fmt.Errorf("validate source: %w", domain.ErrEmptySnapshot), want: 20 * time.Hour},
		{name: "ordinary failure uses configured retry", err: errors.New("database unavailable"), want: retryDelay},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := delayAfterRun(now, time.UTC, 4, retryDelay, test.err); got != test.want {
				t.Fatalf("delayAfterRun() = %s, want %s", got, test.want)
			}
		})
	}
}
