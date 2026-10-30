//go:build linux

package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMountPointsUnderAnswersInTheCallersTerms. mountinfo names mount points
// by their real path, and the staging tree a caller names may be reached
// through a symlink — a ~/.cache that links to /tmp is the common one — so
// the comparison has to resolve the caller's path and the answer has to come
// back under the path as given. /proc is the mount point every system that
// can answer at all has, since the answer is read from it.
func TestMountPointsUnderAnswersInTheCallersTerms(t *testing.T) {
	t.Parallel()
	link := filepath.Join(t.TempDir(), "proc-link")
	if err := os.Symlink("/proc", link); err != nil {
		t.Fatal(err)
	}
	mps, ok, err := MountPointsUnder(link)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("/proc/self/mountinfo cannot be read here")
	}
	if len(mps) == 0 || mps[0] != link {
		t.Fatalf("MountPointsUnder(%s -> /proc) = %q, want %s first", link, mps, link)
	}

	plain := t.TempDir()
	if mps, ok, err := MountPointsUnder(plain); err != nil || !ok || len(mps) != 0 {
		t.Errorf("MountPointsUnder(%s) = %q, %v, %v; want no mount points", plain, mps, ok, err)
	}
	missing := filepath.Join(plain, "not-yet")
	if mps, ok, err := MountPointsUnder(missing); err != nil || !ok || len(mps) != 0 {
		t.Errorf("MountPointsUnder(%s) = %q, %v, %v; want no mount points", missing, mps, ok, err)
	}
}

// TestUnescapeMountinfo covers the kernel's escaping of the characters that
// would break the line apart, and leaves anything that is not a whole escape
// as it is.
func TestUnescapeMountinfo(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`/plain/path`:             "/plain/path",
		`/with\040space`:          "/with space",
		`/tab\011and\012newline`:  "/tab\tand\nnewline",
		`/back\134slash`:          `/back\slash`,
		`/not\777a-byte`:          `/not\777a-byte`,
		`/short\04`:               `/short\04`,
		`/trailing\`:              `/trailing\`,
		`/not\08octal`:            `/not\08octal`,
		`\134040stays-one-escape`: `\040stays-one-escape`,
	} {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}
