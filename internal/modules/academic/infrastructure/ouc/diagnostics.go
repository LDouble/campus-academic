package ouc

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

const contractDiagnosticRetention = 48 * time.Hour

// ContractDiagnosticSample is a raw upstream response that could not be
// interpreted by the Provider. It deliberately excludes all credentials and
// student identifiers; the response body is written only to the protected
// diagnostic directory.
type ContractDiagnosticSample struct {
	Body           []byte
	Encoding       string
	Operation      string
	EducationLevel string
	Host           string
	Path           string
	Stage          string
	Failure        string
}

// ContractDiagnosticCapture persists samples without exposing their bodies to
// logs, RPC responses, metrics, or other application storage.
type ContractDiagnosticCapture interface {
	Capture(ContractDiagnosticSample)
}

type fileContractDiagnosticCapture struct {
	dir string
	log *zap.Logger
	now func() time.Time
}

// NewFileContractDiagnosticCapture stores contract-failure responses in a
// protected local directory. Samples are retained for exactly two days.
func NewFileContractDiagnosticCapture(dir string, log *zap.Logger) (*fileContractDiagnosticCapture, error) {
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("contract diagnostic directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create contract diagnostic directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("set contract diagnostic directory permissions: %w", err)
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &fileContractDiagnosticCapture{dir: dir, log: log, now: time.Now}, nil
}

func (c *fileContractDiagnosticCapture) Capture(sample ContractDiagnosticSample) {
	if c == nil || len(sample.Body) == 0 {
		return
	}
	if err := c.removeExpired(); err != nil {
		c.log.Warn("clean OUC contract diagnostics failed", zap.Error(err))
	}
	extension := ".html"
	if strings.EqualFold(strings.TrimSpace(sample.Encoding), "json") {
		extension = ".json"
	}
	file, err := os.CreateTemp(c.dir, "ouc-contract-*"+extension)
	if err != nil {
		c.log.Warn("capture OUC contract diagnostic failed", zap.Error(err), zap.String("operation", sample.Operation))
		return
	}
	name := filepath.Base(file.Name())
	if _, err := file.Write(sample.Body); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(file.Name())
		c.log.Warn("write OUC contract diagnostic failed", zap.Error(err), zap.String("capture_id", name))
		return
	}
	sum := sha256.Sum256(sample.Body)
	c.log.Warn(
		"OUC contract diagnostic captured",
		zap.String("capture_id", name),
		zap.String("sha256", fmt.Sprintf("%x", sum)),
		zap.Int("response_bytes", len(sample.Body)),
		zap.String("operation", sample.Operation),
		zap.String("education_level", sample.EducationLevel),
		zap.String("host", strings.ToLower(strings.TrimSpace(sample.Host))),
		zap.String("path", safeLogPath(sample.Path)),
		zap.String("stage", sample.Stage),
		zap.String("failure", sample.Failure),
	)
}

func (c *fileContractDiagnosticCapture) removeExpired() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	cutoff := c.now().Add(-contractDiagnosticRetention)
	var firstErr error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "ouc-contract-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(c.dir, entry.Name())); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
