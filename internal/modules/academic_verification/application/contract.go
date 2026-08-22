// Package application contains the provider-side credential verification
// contract. Platform identity persistence is intentionally not part of this
// repository.
package application

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrInvalidCredentials     = errors.New("invalid academic credentials")
	ErrPasswordExpired        = errors.New("academic password expired")
	ErrProviderUnavailable    = errors.New("academic provider unavailable")
	ErrIdentityTypeAmbiguous  = errors.New("academic identity type ambiguous")
	ErrIdentityTypeUnresolved = errors.New("academic identity type unresolved")
	ErrIdentityTypeMismatch   = errors.New("academic identity type mismatch")
	ErrChallengeRequired      = errors.New("academic interactive challenge required")
	ErrAccountRestricted      = errors.New("academic account restricted")
	ErrProviderRetryable      = errors.New("academic provider retryable")
)

const (
	ProviderOUC    = "ouc"
	ProviderMock   = "mock"
	ProviderManual = "manual"

	EducationUndergraduate = "undergraduate"
	EducationGraduate      = "graduate"
	EducationUnknown       = "unknown"
)

// VerificationResult is the provider-owned identity result.
type VerificationResult struct {
	RealName       string
	Provider       string
	EducationLevel string
}

// VerificationCommand contains transient credentials only.
type VerificationCommand struct {
	StudentNo      string
	Password       string
	EducationLevel string
}

// Provider verifies a credential without persisting it.
type Provider interface {
	Verify(context.Context, VerificationCommand) (VerificationResult, error)
}

// NewProviderRetryableError preserves the upstream cause for logs while
// keeping the transport classification stable.
func NewProviderRetryableError(cause error) error {
	if cause == nil {
		return ErrProviderRetryable
	}
	return fmt.Errorf("%w: %w", ErrProviderRetryable, cause)
}
