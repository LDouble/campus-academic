package ouc

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	contractDiagnosticRetention       = 48 * time.Hour
	contractDiagnosticQueueSize       = 16
	contractDiagnosticMaxSamples      = 128
	contractDiagnosticMaxBytes        = 128 << 20
	contractDiagnosticCleanupInterval = time.Minute
)

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
	dir             string
	log             *zap.Logger
	now             func() time.Time
	queue           chan ContractDiagnosticSample
	cleanupInterval time.Duration

	mu      sync.Mutex
	closed  bool
	done    sync.WaitGroup
	dropped atomic.Uint64
}

// NewFileContractDiagnosticCapture stores contract-failure responses in a
// protected local directory. Samples are retained for exactly two days.
func NewFileContractDiagnosticCapture(dir string, log *zap.Logger) (*fileContractDiagnosticCapture, error) {
	return newFileContractDiagnosticCapture(dir, log, time.Now, contractDiagnosticCleanupInterval)
}

func newFileContractDiagnosticCapture(
	dir string,
	log *zap.Logger,
	now func() time.Time,
	cleanupInterval time.Duration,
) (*fileContractDiagnosticCapture, error) {
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
	if now == nil {
		now = time.Now
	}
	if cleanupInterval <= 0 {
		cleanupInterval = contractDiagnosticCleanupInterval
	}
	capture := &fileContractDiagnosticCapture{
		dir:             dir,
		log:             log,
		now:             now,
		queue:           make(chan ContractDiagnosticSample, contractDiagnosticQueueSize),
		cleanupInterval: cleanupInterval,
	}
	capture.done.Add(1)
	go capture.run()
	return capture, nil
}

// Capture transfers the immutable response body to a bounded queue and returns
// immediately. A full queue intentionally drops new samples rather than adding
// latency to an already failed academic request.
func (c *fileContractDiagnosticCapture) Capture(sample ContractDiagnosticSample) {
	if c == nil || len(sample.Body) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.queue <- sample:
	default:
		c.dropped.Add(1)
	}
}

// Close finishes queued writes during graceful provider shutdown.
func (c *fileContractDiagnosticCapture) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.queue)
	c.mu.Unlock()
	c.done.Wait()
}

func (c *fileContractDiagnosticCapture) run() {
	defer c.done.Done()
	ticker := time.NewTicker(c.cleanupInterval)
	defer ticker.Stop()
	c.cleanExpired()
	for {
		select {
		case sample, ok := <-c.queue:
			if !ok {
				return
			}
			if dropped := c.dropped.Swap(0); dropped > 0 {
				c.log.Warn("dropped OUC contract diagnostics because capture queue was full", zap.Uint64("dropped", dropped))
			}
			c.capture(sample)
		case <-ticker.C:
			c.cleanExpired()
		}
	}
}

func (c *fileContractDiagnosticCapture) capture(sample ContractDiagnosticSample) {
	if err := c.prepareStorage(int64(len(sample.Body))); err != nil {
		c.log.Warn("skip OUC contract diagnostic", zap.Error(err), zap.String("operation", sample.Operation))
		return
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

type diagnosticFile struct {
	path     string
	modified time.Time
	size     int64
}

func (c *fileContractDiagnosticCapture) prepareStorage(incomingBytes int64) error {
	if incomingBytes > contractDiagnosticMaxBytes {
		return fmt.Errorf("contract diagnostic response exceeds %d byte storage limit", contractDiagnosticMaxBytes)
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	cutoff := c.now().Add(-contractDiagnosticRetention)
	files := make([]diagnosticFile, 0, len(entries))
	var retainedBytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "ouc-contract-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		path := filepath.Join(c.dir, entry.Name())
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		files = append(files, diagnosticFile{path: path, modified: info.ModTime(), size: info.Size()})
		retainedBytes += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	for len(files) >= contractDiagnosticMaxSamples || retainedBytes+incomingBytes > contractDiagnosticMaxBytes {
		oldest := files[0]
		if err := os.Remove(oldest.path); err != nil {
			return err
		}
		retainedBytes -= oldest.size
		files = files[1:]
	}
	return nil
}

func (c *fileContractDiagnosticCapture) cleanExpired() {
	if err := c.removeExpired(); err != nil {
		c.log.Warn("clean OUC contract diagnostics failed", zap.Error(err))
	}
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
