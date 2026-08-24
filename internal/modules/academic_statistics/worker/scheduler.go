// Package worker schedules the daily academic-statistics publication.
package worker

import (
	"context"
	"errors"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/application"
	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/domain"
	"go.uber.org/zap"
)

// DueRunner is the narrow scheduling contract implemented by the application
// manager.
type DueRunner interface {
	RunIfDue(
		context.Context,
		time.Time,
		*time.Location,
		int,
	) (bool, error)
}

// Run checks immediately for a missed daily execution, then waits until the
// next configured wall-clock schedule. Failures retry without stopping other
// worker responsibilities.
func Run(
	ctx context.Context,
	runner DueRunner,
	location *time.Location,
	scheduleHour int,
	retryDelay time.Duration,
	log *zap.Logger,
) error {
	for {
		now := time.Now()
		ran, err := runner.RunIfDue(
			ctx,
			now,
			location,
			scheduleHour,
		)
		delay := delayAfterRun(time.Now(), location, scheduleHour, retryDelay, err)
		switch {
		case errors.Is(err, context.Canceled):
			return ctx.Err()
		case errors.Is(err, application.ErrRunAlreadyLocked):
			log.Info("academic statistics aggregation already running")
		case errors.Is(err, domain.ErrEmptySnapshot):
			log.Info("academic statistics aggregation skipped because source snapshot is empty")
		case err != nil:
			log.Error(
				"academic statistics aggregation failed",
				zap.Error(err),
			)
			delay = retryDelay
		case ran:
			log.Info("academic statistics aggregation published")
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func delayAfterRun(now time.Time, location *time.Location, scheduleHour int, retryDelay time.Duration, err error) time.Duration {
	if err != nil && !errors.Is(err, domain.ErrEmptySnapshot) {
		return retryDelay
	}
	return untilNextSchedule(now, location, scheduleHour)
}

func untilNextSchedule(
	now time.Time,
	location *time.Location,
	scheduleHour int,
) time.Duration {
	localNow := now.In(location)
	next := time.Date(
		localNow.Year(),
		localNow.Month(),
		localNow.Day(),
		scheduleHour,
		0,
		0,
		0,
		location,
	)
	if !next.After(localNow) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(localNow)
}
