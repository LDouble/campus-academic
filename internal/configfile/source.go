// Package configfile provides the service-owned, file-backed configuration
// source used by Provider. It intentionally exposes only a group/key map so
// the Provider cannot reach the platform configuration database.
package configfile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// Source reads a YAML document on every refresh. Resolver callers decide the
// refresh interval, so malformed updates keep the last valid snapshot.
type Source struct {
	path string
	mu   sync.RWMutex
}

// New creates a source rooted at one explicitly configured file.
func New(path string) (*Source, error) {
	if path == "" {
		return nil, fmt.Errorf("academic provider config file is required")
	}
	return &Source{path: path}, nil
}

// ListDecrypted implements the provider and rate-limit Source interfaces.
func (s *Source) ListDecrypted(ctx context.Context, group string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read academic config file: %w", err)
	}
	var document map[string]map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode academic config file: %w", err)
	}
	values := document[group]
	result := make(map[string]string, len(values))
	for key, value := range values {
		encoded, err := encodeValue(value)
		if err != nil {
			return nil, fmt.Errorf("encode academic config %s.%s: %w", group, key, err)
		}
		result[key] = encoded
	}
	return result, nil
}

func encodeValue(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case bool:
		return fmt.Sprint(typed), nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(typed), nil
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
