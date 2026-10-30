package backup

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestPreflightRefusesAKeyFileThatIsNotARegularFile: preflight opened
// AGE_RECIPIENTS_FILE, and under --verify-roundtrip AGE_IDENTITY, as given, so
// a FIFO at either hung the backup in open(2) before a single disc, past the
// first Ctrl-C. Both go through agecrypt's parsers, which now refuse anything
// but a regular file before opening it.
func TestPreflightRefusesAKeyFileThatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	t.Cleanup(func() {
		// Releases a regression's goroutine blocked opening the FIFO: a
		// writer's open completes a waiting reader's, and with no reader
		// waiting O_NONBLOCK makes it fail at once instead of hanging.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})

	r := stagingRunner(t, filepath.Join(dir, "staging"))
	r.cfg.AgeRecipientsFile = fifo
	r.cfg.AgeIdentity = fifo
	r.opts.VerifyRoundTrip = true
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"loadRecipients", r.loadRecipients},
		{"loadIdentity", r.loadIdentity},
	} {
		done := make(chan error, 1)
		go func() { done <- step.run() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), fifo+" is not a regular file") {
				t.Errorf("%s with a FIFO for its key file = %v, want it refused as not a regular file", step.name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s with a FIFO for its key file has not returned after 5s", step.name)
		}
	}
}
