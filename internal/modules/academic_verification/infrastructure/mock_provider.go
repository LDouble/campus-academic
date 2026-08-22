package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
	"golang.org/x/crypto/bcrypt"
)

type mockCredential struct {
	StudentNo      string `json:"student_no"`
	RealName       string `json:"real_name"`
	PasswordHash   string `json:"password_hash"`
	EducationLevel string `json:"education_level,omitempty"`
}

// MockProvider verifies a Secret-mounted bcrypt whitelist.
type MockProvider struct {
	entries   map[string]mockCredential
	dummyHash []byte
}

// MockCredentialSource exposes the current decrypted development fixture.
type MockCredentialSource interface {
	MockCredentials() string
}

// DynamicMockProvider hot-reloads a local-only mock whitelist and retains the
// last valid parsed snapshot when a new value is malformed.
type DynamicMockProvider struct {
	source MockCredentialSource

	mu       sync.RWMutex
	raw      string
	rejected string
	provider *MockProvider
}

// NewDynamicMockProvider creates a configuration-center-backed mock provider.
func NewDynamicMockProvider(source MockCredentialSource) *DynamicMockProvider {
	return &DynamicMockProvider{source: source}
}

// NewMockProvider loads a development fixture without logging its contents.
func NewMockProvider(path string) (*MockProvider, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("academic provider whitelist path is required")
	}
	// #nosec G304 -- this is an explicit operator-controlled Secret path, never request input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read academic provider whitelist: %w", err)
	}
	return newMockProvider(string(data))
}

func newMockProvider(raw string) (*MockProvider, error) {
	entries := []mockCredential{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode academic provider whitelist: %w", err)
	}
	byStudent := make(map[string]mockCredential, len(entries))
	for _, entry := range entries {
		entry.StudentNo = strings.TrimSpace(entry.StudentNo)
		entry.RealName = strings.TrimSpace(entry.RealName)
		entry.EducationLevel = strings.TrimSpace(entry.EducationLevel)
		if entry.StudentNo == "" || entry.RealName == "" || entry.PasswordHash == "" {
			return nil, fmt.Errorf("academic provider whitelist contains an incomplete entry")
		}
		if entry.EducationLevel != application.EducationUndergraduate &&
			entry.EducationLevel != application.EducationGraduate {
			return nil, fmt.Errorf("academic provider whitelist contains an invalid education level")
		}
		if _, exists := byStudent[entry.StudentNo]; exists {
			return nil, fmt.Errorf("academic provider whitelist contains a duplicate student number")
		}
		if _, err := bcrypt.Cost([]byte(entry.PasswordHash)); err != nil {
			return nil, fmt.Errorf("academic provider whitelist contains an invalid password hash")
		}
		byStudent[entry.StudentNo] = entry
	}
	dummyHash, err := bcrypt.GenerateFromPassword([]byte("academic-provider-timing-placeholder"), 12)
	if err != nil {
		return nil, fmt.Errorf("create academic provider placeholder: %w", err)
	}
	return &MockProvider{entries: byStudent, dummyHash: dummyHash}, nil
}

// Verify resolves the latest valid dynamic whitelist before checking the
// supplied credentials.
func (p *DynamicMockProvider) Verify(
	ctx context.Context,
	command application.VerificationCommand,
) (application.VerificationResult, error) {
	if p == nil || p.source == nil {
		return application.VerificationResult{}, application.ErrProviderUnavailable
	}
	raw := strings.TrimSpace(p.source.MockCredentials())
	if raw == "" {
		return application.VerificationResult{}, application.ErrProviderUnavailable
	}
	p.mu.RLock()
	currentRaw, rejectedRaw, provider := p.raw, p.rejected, p.provider
	p.mu.RUnlock()
	if currentRaw != raw && rejectedRaw != raw {
		parsed, err := newMockProvider(raw)
		p.mu.Lock()
		if err == nil {
			if p.raw != raw {
				p.raw = raw
				p.rejected = ""
				p.provider = parsed
			}
			provider = p.provider
		} else {
			p.rejected = raw
			provider = p.provider
		}
		p.mu.Unlock()
		if err != nil && provider == nil {
			return application.VerificationResult{}, application.ErrProviderUnavailable
		}
	}
	if provider == nil {
		return application.VerificationResult{}, application.ErrProviderUnavailable
	}
	return provider.Verify(ctx, command)
}

// Verify performs bcrypt for both known and unknown student numbers.
func (p *MockProvider) Verify(
	ctx context.Context,
	command application.VerificationCommand,
) (application.VerificationResult, error) {
	if err := ctx.Err(); err != nil {
		return application.VerificationResult{}, application.ErrProviderUnavailable
	}
	entry, exists := p.entries[strings.TrimSpace(command.StudentNo)]
	hash := p.dummyHash
	if exists {
		hash = []byte(entry.PasswordHash)
	}
	err := bcrypt.CompareHashAndPassword(hash, []byte(command.Password))
	if err != nil || !exists {
		return application.VerificationResult{}, application.ErrInvalidCredentials
	}
	if entry.EducationLevel != strings.TrimSpace(command.EducationLevel) {
		return application.VerificationResult{}, application.ErrIdentityTypeMismatch
	}
	if err = ctx.Err(); err != nil {
		return application.VerificationResult{}, application.ErrProviderUnavailable
	}
	return application.VerificationResult{
		RealName:       entry.RealName,
		Provider:       application.ProviderMock,
		EducationLevel: entry.EducationLevel,
	}, nil
}

var _ application.Provider = (*DynamicMockProvider)(nil)
