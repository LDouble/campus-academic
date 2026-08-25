package ouc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestFileContractDiagnosticCaptureWritesProtectedFullResponseAndRemovesExpiredSamples(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "ouc-contract-old.html")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	then := time.Date(2026, time.August, 25, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(old, then.Add(-contractDiagnosticRetention-time.Second), then.Add(-contractDiagnosticRetention-time.Second)); err != nil {
		t.Fatal(err)
	}
	capture, err := NewFileContractDiagnosticCapture(dir, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(capture.Close)
	capture.now = func() time.Time { return then }
	body := []byte("<html><body>changed table</body></html>")
	capture.Capture(ContractDiagnosticSample{Body: body, Encoding: "html", Operation: "query.selections"})
	capture.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired sample still exists, err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("sample count=%d, want 1", len(entries))
	}
	path := filepath.Join(dir, entries[0].Name())
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("stored body=%q, want %q", got, body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sample mode=%#o, want 0600", info.Mode().Perm())
	}
}

func TestFileContractDiagnosticCaptureDoesNotBlockWhenQueueIsFull(t *testing.T) {
	capture := &fileContractDiagnosticCapture{
		log:   zap.NewNop(),
		queue: make(chan ContractDiagnosticSample, 1),
	}
	capture.queue <- ContractDiagnosticSample{Body: []byte("queued")}
	done := make(chan struct{})
	go func() {
		capture.Capture(ContractDiagnosticSample{Body: []byte("dropped")})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Capture blocked on a full queue")
	}
}

func TestFileContractDiagnosticCaptureRequiresAbsoluteDirectory(t *testing.T) {
	if _, err := NewFileContractDiagnosticCapture("relative", zap.NewNop()); err == nil {
		t.Fatal("expected absolute directory validation error")
	}
}
