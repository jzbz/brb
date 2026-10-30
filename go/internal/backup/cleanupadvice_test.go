package backup

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinishNamesStagingAsOneShellWord pins the closing advice of every
// backup. It used to paste STAGING into "rm -rf" bare, so a valid staging path
// with a space in it came out as two operands: pasted, it deleted the
// directory before the space and a relative path after it, and left the
// staging tree — plaintext images included — where it was, with -f making sure
// nothing said so.
func TestFinishNamesStagingAsOneShellWord(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"brb", "rm -rf %s"},
		{"brb staging", "rm -rf '%s'"},
		{"brb*", "rm -rf '%s'"},
	} {
		staging := filepath.Join(t.TempDir(), tc.name)
		r := stagingRunner(t, staging)
		p, out := recordingPrinter()
		r.p = p
		r.statePath = filepath.Join(staging, "state.json")
		r.finish(3)
		want := "Then wipe staging:  " + fmt.Sprintf(tc.want, staging)
		var line string
		for _, l := range strings.Split(out(), "\n") {
			if strings.Contains(l, "Then wipe staging:") {
				line = strings.TrimSpace(l)
			}
		}
		if line != want {
			t.Errorf("STAGING=%q: closing advice is %q, want %q", staging, line, want)
		}
	}
}
