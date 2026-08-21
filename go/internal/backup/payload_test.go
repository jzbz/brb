package backup

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/ui"
)

// writeFileMode writes a file with an exact mode, defeating the umask.
func writeFileMode(t *testing.T, path, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// modeOf returns a file's permission bits.
func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// sameInode reports whether two paths are the same file on disk, which is how
// "was it hard-linked or copied?" is answered.
func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(fa, fb)
}

func TestPayloadMode(t *testing.T) {
	tests := []struct {
		name string
		want fs.FileMode
	}{
		{"brb.sh", 0o755},
		{"brb-linux-amd64", 0o755},
		{"brb-linux-aarch64", 0o755},
		{"brb-src.tar.gz", 0o644},
	}
	for _, tt := range tests {
		if got := PayloadMode(tt.name); got != tt.want {
			t.Errorf("PayloadMode(%s) = %o, want %o", tt.name, got, tt.want)
		}
	}
	if got, want := PayloadNames(), []string{"brb.sh", "brb-linux-amd64", "brb-linux-aarch64", "brb-src.tar.gz"}; !equalStrings(got, want) {
		t.Errorf("PayloadNames() = %v, want %v", got, want)
	}
	// The returned slice is a copy: a caller that sorts it must not reorder
	// what every later disc is built from.
	PayloadNames()[0] = "clobbered"
	if PayloadNames()[0] != "brb.sh" {
		t.Error("PayloadNames() hands out the package's own slice")
	}
}

// TestPlacePayloadHardLinksWhenTheModeIsRight proves the optimisation that
// keeps a twenty-disc staging directory from holding twenty extra copies of an
// 8 MB binary.
func TestPlacePayloadHardLinksWhenTheModeIsRight(t *testing.T) {
	dist, disc := t.TempDir(), t.TempDir()
	src := filepath.Join(dist, "brb-linux-amd64")
	dst := filepath.Join(disc, "brb-linux-amd64")
	writeFileMode(t, src, "binary", 0o755)

	if err := placePayload(context.Background(), src, dst, 0o755); err != nil {
		t.Fatalf("placePayload: %v", err)
	}
	if !sameInode(t, src, dst) {
		t.Error("a payload file whose mode is already right was copied instead of linked")
	}
	if got := modeOf(t, dst); got != 0o755 {
		t.Errorf("mode on the disc = %o, want 755", got)
	}
}

// TestPlacePayloadCopiesWhenTheModeIsWrong pins the reason placePayload does
// not always link. A hard link shares an inode with the file in the operator's dist
// directory, so chmodding through it would rewrite their copy. When the mode
// has to change, the file is copied and only the copy is touched.
func TestPlacePayloadCopiesWhenTheModeIsWrong(t *testing.T) {
	dist, disc := t.TempDir(), t.TempDir()

	tests := []struct {
		name    string
		srcMode fs.FileMode
		want    fs.FileMode
	}{
		{"brb-linux-amd64", 0o644, 0o755}, // a binary that lost its +x
		{"brb-src.tar.gz", 0o755, 0o644},  // a tarball marked executable
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(dist, tt.name)
			dst := filepath.Join(disc, tt.name)
			writeFileMode(t, src, "payload "+tt.name, tt.srcMode)

			if err := placePayload(context.Background(), src, dst, tt.want); err != nil {
				t.Fatalf("placePayload: %v", err)
			}
			if sameInode(t, src, dst) {
				t.Fatal("the file was hard-linked, so the chmod below rewrote the operator's dist directory")
			}
			if got := modeOf(t, dst); got != tt.want {
				t.Errorf("mode on the disc = %o, want %o", got, tt.want)
			}
			if got := modeOf(t, src); got != tt.srcMode {
				t.Errorf("mode in the dist directory = %o, want it untouched at %o", got, tt.srcMode)
			}
			body, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "payload "+tt.name {
				t.Errorf("content on the disc = %q", body)
			}
		})
	}
}

// TestPlacePayloadWithAReadOnlyDistDirectory covers the packaged case:
// /usr/share/brb is not writable by the operator running the backup. Both the
// link path and the copy path have to work, and neither may modify anything
// under it.
func TestPlacePayloadWithAReadOnlyDistDirectory(t *testing.T) {
	dist, disc := t.TempDir(), t.TempDir()
	linked := filepath.Join(dist, "brb-linux-amd64") // mode already right
	copied := filepath.Join(dist, "brb-src.tar.gz")  // mode has to change
	writeFileMode(t, linked, "binary", 0o755)
	writeFileMode(t, copied, "tarball", 0o755)

	if err := os.Chmod(dist, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dist, 0o755) })

	for _, name := range []string{"brb-linux-amd64", "brb-src.tar.gz"} {
		src, dst := filepath.Join(dist, name), filepath.Join(disc, name)
		if err := placePayload(context.Background(), src, dst, PayloadMode(name)); err != nil {
			t.Fatalf("placePayload(%s) from a read-only dist directory: %v", name, err)
		}
		if got := modeOf(t, dst); got != PayloadMode(name) {
			t.Errorf("%s: mode on the disc = %o, want %o", name, got, PayloadMode(name))
		}
	}
	if got := modeOf(t, copied); got != 0o755 {
		t.Errorf("the read-only dist directory was modified: %s is now %o", copied, got)
	}
	if got := modeOf(t, dist); got != 0o555 {
		t.Errorf("the dist directory's own mode changed to %o", got)
	}
}

// TestPlacePayloadReplacesWhatIsThere proves a re-run overwrites an older
// artifact rather than leaving it in place.
func TestPlacePayloadReplacesWhatIsThere(t *testing.T) {
	dist, disc := t.TempDir(), t.TempDir()
	src, dst := filepath.Join(dist, "brb.sh"), filepath.Join(disc, "brb.sh")
	writeFileMode(t, src, "new", 0o755)
	writeFileMode(t, dst, "stale", 0o600)

	if err := placePayload(context.Background(), src, dst, 0o755); err != nil {
		t.Fatalf("placePayload: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Errorf("disc still holds %q", body)
	}
}

// payloadRunner builds a runner with staging in a temporary directory, plus the
// disc directories a payload would be written into, and returns it with the
// buffer its diagnostics go to.
func payloadRunner(t *testing.T, distDir string, discs int) (*runner, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.SourceDir = t.TempDir()
	cfg.Staging = t.TempDir()
	cfg.ArchiveName = "payload-test"
	cfg.DistDir = distDir

	var out bytes.Buffer
	r, err := newRunner(Options{Cfg: cfg, UI: ui.New(&out, false)})
	if err != nil {
		t.Fatalf("newRunner: %v", err)
	}
	for n := 1; n <= discs; n++ {
		if err := os.MkdirAll(filepath.Join(r.dirs.Discs, discDirName(n)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r, &out
}

// noSystemDist points the payload search at nothing, so a test sees the "no
// dist directory" path whatever this machine happens to have installed under
// /usr/share.
func noSystemDist(t *testing.T) {
	t.Helper()
	old := config.SystemDistDirs
	config.SystemDistDirs = nil
	t.Cleanup(func() { config.SystemDistDirs = old })
}

// fakeDist writes a dist directory holding the named payload files.
func fakeDist(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		writeFileMode(t, filepath.Join(dir, n), "fake "+n, PayloadMode(n))
	}
	return dir
}

// TestWritePayloadWithoutADistDirectory is the promise that a missing payload
// never costs an operator a backup.
func TestWritePayloadWithoutADistDirectory(t *testing.T) {
	noSystemDist(t)
	r, out := payloadRunner(t, "", 2)

	if err := r.writePayload(context.Background(), 2); err != nil {
		t.Fatalf("writePayload with no dist directory: %v", err)
	}
	log := out.String()
	for _, want := range []string{"no dist directory", SelfCopyName(), "./build-dist.sh", "BRB_DIST_DIR"} {
		if !strings.Contains(log, want) {
			t.Errorf("output does not mention %q:\n%s", want, log)
		}
	}
	ents, err := os.ReadDir(filepath.Join(r.dirs.Discs, discDirName(1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("disc 1 holds %d file(s) after a payload-less run", len(ents))
	}
}

// TestWritePayloadWithAMissingDistDirectory: DIST_DIR set to a path that is not
// there is said out loud, and still does not fail the backup.
func TestWritePayloadWithAMissingDistDirectory(t *testing.T) {
	noSystemDist(t)
	missing := filepath.Join(t.TempDir(), "typo")
	r, out := payloadRunner(t, missing, 1)

	if err := r.writePayload(context.Background(), 1); err != nil {
		t.Fatalf("writePayload with a missing DIST_DIR: %v", err)
	}
	log := out.String()
	for _, want := range []string{"DIST_DIR", missing, "does not exist"} {
		if !strings.Contains(log, want) {
			t.Errorf("output does not mention %q:\n%s", want, log)
		}
	}
}

// TestWritePayloadPlacesEveryFileOnEveryDisc covers the ordinary case.
func TestWritePayloadPlacesEveryFileOnEveryDisc(t *testing.T) {
	dist := fakeDist(t, PayloadNames()...)
	r, out := payloadRunner(t, dist, 3)

	if err := r.writePayload(context.Background(), 3); err != nil {
		t.Fatalf("writePayload: %v", err)
	}
	for n := 1; n <= 3; n++ {
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		for _, name := range PayloadNames() {
			path := filepath.Join(dd, name)
			if got := modeOf(t, path); got != PayloadMode(name) {
				t.Errorf("disc %d: %s has mode %o, want %o", n, name, got, PayloadMode(name))
			}
			if !sameInode(t, filepath.Join(dist, name), path) {
				t.Errorf("disc %d: %s was copied although its mode was already right", n, name)
			}
		}
		if got, want := discToolArtifacts(dd), PayloadNames(); !equalStrings(got, want) {
			t.Errorf("disc %d artifacts = %v, want %v", n, got, want)
		}
	}
	if !strings.Contains(out.String(), "4 payload file(s) on every disc") {
		t.Errorf("output does not report the count:\n%s", out.String())
	}
}

// TestWritePayloadWithAnIncompleteDistDirectory warns per missing file and
// carries on with what is there.
func TestWritePayloadWithAnIncompleteDistDirectory(t *testing.T) {
	dist := fakeDist(t, "brb.sh", "brb-linux-amd64")
	r, out := payloadRunner(t, dist, 1)

	if err := r.writePayload(context.Background(), 1); err != nil {
		t.Fatalf("writePayload: %v", err)
	}
	log := out.String()
	for _, want := range []string{
		"payload missing: brb-linux-aarch64",
		"payload missing: brb-src.tar.gz",
		"run ./build-dist.sh to produce the full payload",
		"2 payload file(s) on every disc",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("output does not mention %q:\n%s", want, log)
		}
	}
	dd := filepath.Join(r.dirs.Discs, discDirName(1))
	if got, want := discToolArtifacts(dd), []string{"brb.sh", "brb-linux-amd64"}; !equalStrings(got, want) {
		t.Errorf("disc artifacts = %v, want %v", got, want)
	}
}

// TestWritePayloadWithAnEmptyDistDirectory: a directory with nothing in it is
// reported as such, not silently treated as success.
func TestWritePayloadWithAnEmptyDistDirectory(t *testing.T) {
	dist := t.TempDir()
	r, out := payloadRunner(t, dist, 1)

	if err := r.writePayload(context.Background(), 1); err != nil {
		t.Fatalf("writePayload: %v", err)
	}
	if !strings.Contains(out.String(), "no payload files were found in "+dist) {
		t.Errorf("output does not say the directory is empty:\n%s", out.String())
	}
}

// TestWritePayloadFromAReadOnlyDistDirectory: the packaged install case, end to
// end through the step rather than through placePayload alone.
func TestWritePayloadFromAReadOnlyDistDirectory(t *testing.T) {
	dist := fakeDist(t, PayloadNames()...)
	// Give one file the wrong mode so the copy path runs too.
	if err := os.Chmod(filepath.Join(dist, "brb-src.tar.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dist, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dist, 0o755) })

	r, _ := payloadRunner(t, dist, 2)
	if err := r.writePayload(context.Background(), 2); err != nil {
		t.Fatalf("writePayload from a read-only dist directory: %v", err)
	}
	for n := 1; n <= 2; n++ {
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		if got, want := discToolArtifacts(dd), PayloadNames(); !equalStrings(got, want) {
			t.Errorf("disc %d artifacts = %v, want %v", n, got, want)
		}
		if got := modeOf(t, filepath.Join(dd, "brb-src.tar.gz")); got != 0o644 {
			t.Errorf("disc %d: tarball mode = %o, want 644", n, got)
		}
	}
	if got := modeOf(t, filepath.Join(dist, "brb-src.tar.gz")); got != 0o755 {
		t.Errorf("the read-only dist directory was rewritten: tarball is now %o", got)
	}
}

// TestDiscToolArtifactsIgnoresEverythingElse: only copies of brb are listed, and
// a directory of that name is not a file a restorer can run.
func TestDiscToolArtifactsIgnoresEverythingElse(t *testing.T) {
	dd := t.TempDir()
	writeFileMode(t, filepath.Join(dd, "README.md"), "x", 0o644)
	writeFileMode(t, filepath.Join(dd, "SHA512SUMS"), "x", 0o644)
	writeFileMode(t, filepath.Join(dd, "brb.sh"), "x", 0o755)
	if err := os.MkdirAll(filepath.Join(dd, "brb-src.tar.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := discToolArtifacts(dd), []string{"brb.sh"}; !equalStrings(got, want) {
		t.Errorf("discToolArtifacts = %v, want %v", got, want)
	}
	if got := discToolArtifacts(t.TempDir()); len(got) != 0 {
		t.Errorf("discToolArtifacts on an empty disc = %v, want nothing", got)
	}
}

// equalStrings compares two string slices element by element.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
