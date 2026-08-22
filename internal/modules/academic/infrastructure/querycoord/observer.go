package querycoord

import (
	"context"
	"errors"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
)

const (
	cacheStateMissLabel  = "miss"
	cacheStateFreshLabel = "fresh"
	cacheStateStaleLabel = "stale"
	cacheStateErrorLabel = "error"
)

// Observer receives bounded academic coordinator events. Implementations must
// not derive labels from student identifiers, credentials or request IDs.
type Observer interface {
	ObserveAcademicCache(operation, state, educationLevel string)
	ObserveAcademicCacheBypass(operation, state, educationLevel, mode string)
	ObserveAcademicQuery(operation, outcome, educationLevel string)
	ObserveAcademicCircuit(operation, event, educationLevel string)
	ObserveAcademicStaleFallback(operation, reason, educationLevel string)
}

// SingleflightObserver is optional to preserve compatibility with existing
// observers while exposing leader/follower coalescing behavior.
type SingleflightObserver interface {
	ObserveAcademicSingleflight(operation, role, educationLevel string)
}

type noopObserver struct{}

func (noopObserver) ObserveAcademicCache(string, string, string) {}
func (noopObserver) ObserveAcademicCacheBypass(string, string, string, string) {
}
func (noopObserver) ObserveAcademicQuery(string, string, string)         {}
func (noopObserver) ObserveAcademicCircuit(string, string, string)       {}
func (noopObserver) ObserveAcademicStaleFallback(string, string, string) {}

// Option customizes a Coordinator without changing its provider contract.
type Option func(*Coordinator)

// WithObserver records bounded cache and query outcomes.
func WithObserver(observer Observer) Option {
	return func(coordinator *Coordinator) {
		if observer != nil {
			coordinator.observer = observer
		}
	}
}

func cacheStateLabel(state cacheState, err error) string {
	if err != nil {
		return cacheStateErrorLabel
	}
	switch state {
	case cacheFresh:
		return cacheStateFreshLabel
	case cacheStale:
		return cacheStateStaleLabel
	default:
		return cacheStateMissLabel
	}
}

func queryOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, application.ErrInvalidCredentials):
		return "invalid_credentials"
	case errors.Is(err, application.ErrPasswordExpired):
		return "password_expired"
	case errors.Is(err, application.ErrChallengeRequired):
		return "challenge_required"
	case errors.Is(err, application.ErrAccountRestricted):
		return "account_restricted"
	case errors.Is(err, application.ErrProviderBusy):
		return "busy"
	case errors.Is(err, application.ErrProviderRetryable):
		return "retryable"
	case errors.Is(err, application.ErrProviderUnavailable):
		return "unavailable"
	default:
		return "error"
	}
}
