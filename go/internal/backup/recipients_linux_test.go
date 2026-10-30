package backup

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/config"
)

// TestLoadRecipientsReadsTheFileOnce pins the one-snapshot rule: the keys every
// image is encrypted to and the keys MANIFEST.txt and state.json record must
// come from a single read of AGE_RECIPIENTS_FILE.
//
// They came from two opens. A file changed between them — init-key
// --rescue-key appending a key is the writer brb itself provides — left the
// images encrypted to the old set while the manifest named the rescue key as
// opening them, and every later resume checked against keys nothing was
// encrypted to. The window is microseconds wide, so this does not race for it:
// it counts the opens instead, through inotify. (It used to serve the file
// from a fifo that answered exactly one open, but a recipients file that is
// not a regular file is now refused before anything opens it.) Linux only,
// for inotify; the property is not platform-specific.
func TestLoadRecipientsReadsTheFileOnce(t *testing.T) {
	t.Parallel()
	first, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recipients.txt")
	if err := os.WriteFile(path, []byte(first.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		t.Skipf("inotify unavailable here: %v", err)
	}
	defer syscall.Close(fd)
	// Closes are watched as well as opens, though only opens are counted:
	// inotify folds an event into the one queued before it when the two are
	// identical, so two opens with nothing between them would count as one.
	// Each open's close lands between it and the next.
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_OPEN|syscall.IN_CLOSE); err != nil {
		t.Skipf("inotify cannot watch %s: %v", path, err)
	}

	cfg := config.Default()
	cfg.AgeRecipientsFile = path
	r := &runner{cfg: cfg, p: quiet()}
	if err := r.loadRecipients(); err != nil {
		t.Fatalf("loadRecipients: %v", err)
	}

	// The events are queued as each open happens, so every one is there to be
	// read by the time loadRecipients has returned. A watch on a file reports
	// events with no name: each is the fixed header alone, unless Len says
	// otherwise.
	opens := 0
	buf := make([]byte, 64*(syscall.SizeofInotifyEvent+syscall.NAME_MAX+1))
	for {
		n, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatalf("reading inotify events: %v", err)
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			if mask&syscall.IN_OPEN != 0 {
				opens++
			}
			off += syscall.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(buf[off+12:]))
		}
	}
	if opens != 1 {
		t.Errorf("loadRecipients opened the recipients file %d time(s), want 1: a file that changed "+
			"between two opens would be encrypted to as one set of keys and recorded as another", opens)
	}
	if len(r.recipients) != 1 || !slices.Equal(r.pubkeys, []string{first.Recipient().String()}) {
		t.Errorf("encrypting to %d key(s) and recording %q, want the one key the file held",
			len(r.recipients), r.pubkeys)
	}
}
