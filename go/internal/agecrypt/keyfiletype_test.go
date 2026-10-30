package agecrypt

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKeyFileParsersTakeOnlyARegularFile: both parsers opened whatever path
// they were given, so a FIFO at AGE_RECIPIENTS_FILE or AGE_IDENTITY hung
// doctor, init-key and backup preflight in open(2), past the first Ctrl-C,
// and a directory failed only on the read, in words that never said what was
// wrong. Now anything that is not a regular file is refused by name before it
// is opened. A symlink to a key file still parses, as `[[ -f ]]` follows one,
// and a missing file still fails with fs.ErrNotExist, which doctor tests for.
func TestKeyFileParsersTakeOnlyARegularFile(t *testing.T) {
	dir := t.TempDir()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	idPath := filepath.Join(dir, "identity.txt")
	if err := WriteIdentityFile(idPath, id); err != nil {
		t.Fatal(err)
	}
	recPath := filepath.Join(dir, "recipients.txt")
	if err := AppendRecipient(recPath, id.Recipient().String()); err != nil {
		t.Fatal(err)
	}

	parsers := []struct {
		kind  string
		good  string
		parse func(string) error
	}{
		{"recipients", recPath, func(p string) error { _, err := ParseRecipientsFile(p); return err }},
		{"identity", idPath, func(p string) error { _, err := ParseIdentityFile(p); return err }},
	}

	odd := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	irregular := []string{odd}
	fifo := filepath.Join(dir, "a-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err == nil {
		irregular = append(irregular, fifo)
		t.Cleanup(func() {
			// Releases a regression's goroutine blocked opening the FIFO: a
			// writer's open completes a waiting reader's, and with no reader
			// waiting O_NONBLOCK makes it fail at once instead of hanging.
			if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				f.Close()
			}
		})
	} else {
		t.Logf("no FIFO case: %v", err)
	}
	if fi, err := os.Stat(os.DevNull); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		irregular = append(irregular, os.DevNull)
	}

	for _, p := range parsers {
		for _, path := range irregular {
			done := make(chan error, 1)
			go func() { done <- p.parse(path) }()
			select {
			case err := <-done:
				want := "agecrypt: " + p.kind + " file " + path + " is not a regular file ("
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("parsing %s as a %s file = %v, want it refused as not a regular file", path, p.kind, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("parsing %s as a %s file has not returned after 5s", path, p.kind)
			}
		}

		link := filepath.Join(dir, p.kind+"-link")
		if err := os.Symlink(p.good, link); err != nil {
			t.Fatal(err)
		}
		if err := p.parse(link); err != nil {
			t.Errorf("a symlink to a good %s file was refused: %v", p.kind, err)
		}
		if err := p.parse(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a missing %s file = %v, want fs.ErrNotExist", p.kind, err)
		}
	}
}
