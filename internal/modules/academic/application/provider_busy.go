package application

import (
	"errors"
	"time"
)

// ErrProviderBusy indicates that the academic provider concurrency bulkhead is full.
var ErrProviderBusy = errors.New("academic provider busy")

// ProviderBusyError carries the server-selected delay before another read attempt.
type ProviderBusyError struct {
	RetryAfter time.Duration
}

// Error implements error without exposing internal capacity details.
func (e *ProviderBusyError) Error() string { return ErrProviderBusy.Error() }

// Unwrap supports errors.Is with ErrProviderBusy.
func (e *ProviderBusyError) Unwrap() error { return ErrProviderBusy }

// ProviderRetryAfter returns the bounded retry delay carried by a busy error.
func ProviderRetryAfter(err error) (time.Duration, bool) {
	var busy *ProviderBusyError
	if !errors.As(err, &busy) || busy.RetryAfter <= 0 {
		return 0, false
	}
	return busy.RetryAfter, true
}
