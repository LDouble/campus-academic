package ouc

import (
	"context"
	"errors"
	"time"
)

// Observer receives bounded OUC attempt events. Implementations must not add
// student identifiers, credentials, request IDs, URLs or raw errors as labels.
type Observer interface {
	ObserveAcademicUpstreamAttempt(operation, outcome, educationLevel string)
	ObserveAcademicSessionCache(educationLevel, outcome string)
	ObserveAcademicSessionRecovery(educationLevel, stage, outcome string)
}

// HTTPObserver receives one low-cardinality observation for every OUC HTTP
// exchange. It is optional so existing provider test doubles remain source
// compatible.
type HTTPObserver interface {
	ObserveAcademicOUCHTTP(operation, host, phase, outcome string, elapsed time.Duration)
}

// SessionRejectionObserver records business-level rejection of an otherwise
// successful HTTP response without double-counting that exchange as another
// HTTP request outcome.
type SessionRejectionObserver interface {
	ObserveAcademicOUCSessionRejected(operation, host string)
}

// CoalescingObserver records the local singleflight role of a query.
type CoalescingObserver interface {
	ObserveAcademicSingleflight(operation, role, educationLevel string)
}

type noopObserver struct{}

func (noopObserver) ObserveAcademicUpstreamAttempt(string, string, string) {}
func (noopObserver) ObserveAcademicSessionCache(string, string)            {}
func (noopObserver) ObserveAcademicSessionRecovery(string, string, string) {}

// WithObserver records the terminal outcome of each OUC query attempt.
func WithObserver(observer Observer) ProviderOption {
	return func(provider *Provider) {
		if observer != nil {
			provider.observer = observer
		}
	}
}

func upstreamAttemptOutcome(err error, fallback string) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return fallback
	}
}
