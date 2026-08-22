package application

import (
	"errors"
	"fmt"
)

// ErrProviderRetryable identifies a provider failure that the user may retry
// explicitly. It is intentionally distinct from provider busy (429) so the
// HTTP layer can keep Retry-After semantics scoped to capacity protection.
var ErrProviderRetryable = errors.New("academic provider retryable")

// NewProviderRetryableError preserves the upstream cause for logs and tests
// while exposing a stable application-level classification to transports.
func NewProviderRetryableError(cause error) error {
	if cause == nil {
		return ErrProviderRetryable
	}
	return fmt.Errorf("%w: %w", ErrProviderRetryable, cause)
}
