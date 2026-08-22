// Package requestmeta carries validated request metadata across service boundaries.
package requestmeta

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

const maxRequestIDLength = 36

type requestIDKey struct{}

// WithRequestID stores a validated request identifier in ctx.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	requestID = strings.TrimSpace(requestID)
	if !ValidRequestID(requestID) {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID returns the validated request identifier carried by ctx.
func RequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	if !ValidRequestID(requestID) {
		return ""
	}
	return requestID
}

// ValidRequestID accepts UUIDs and canonical 26-character ULIDs.
func ValidRequestID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxRequestIDLength {
		return false
	}
	if _, err := uuid.Parse(value); err == nil {
		return true
	}
	if len(value) != 26 {
		return false
	}
	const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, char := range strings.ToUpper(value) {
		if !strings.ContainsRune(ulidAlphabet, char) {
			return false
		}
	}
	return true
}
