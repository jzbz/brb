package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/iso"
)

// testConfigWithPrunes is a configuration carrying just the prune list, which
// is all manifestPrunes reads.
func testConfigWithPrunes(prunes ...string) *config.Config {
	c := config.Default()
	c.PruneDirs = prunes
	return c
}

// A path the scan could not open is not on any disc. That is reported once,
// while the run is happening, to a terminal that is long gone by the time
// anyone reads the discs — and a restore missing a file looks exactly like a
// backup that never had it. So the manifest carries it too, the same way it
// already carries the mount points the scan refused to cross.
func TestManifestPrunesNamesUnreadablePaths(t *testing.T) {
	t.Run("named, and marked as unreadable rather than pruned", func(t *testing.T) {
		r := &runner{}
		r.cfg = testConfigWithPrunes(".cache")
		r.unreadable = []string{"secret/keys", "vault/db"}
		r.unreadableCount = 2

		got := strings.Join(r.manifestPrunes(), "\n")
		for _, want := range []string{
			".cache",
			"secret/keys  (could not be read: NOT on any disc)",
			"vault/db  (could not be read: NOT on any disc)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("manifestPrunes missing %q, got:\n%s", want, got)
			}
		}
	})

	// An unreadable directory of a million files must not become a million
	// lines. The count is the part that cannot be truncated away: it is what
	// tells a restorer the set is short, and by how much.
	t.Run("over the cap it names some and counts the rest", func(t *testing.T) {
		r := &runner{}
		r.cfg = testConfigWithPrunes()
		for i := 0; i < maxNamedUnreadable; i++ {
			r.unreadable = append(r.unreadable, "p")
		}
		r.unreadableCount = maxNamedUnreadable + 500

		got := strings.Join(r.manifestPrunes(), "\n")
		if !strings.Contains(got, "... and 500 more path(s) that could not be read") {
			t.Errorf("the overflow count is missing, got:\n%s", got)
		}
	})

	// Nothing unreadable and nothing skipped must leave the list exactly as
	// configured, or every ordinary set grows a section it does not need.
	t.Run("a clean scan changes nothing", func(t *testing.T) {
		r := &runner{}
		r.cfg = testConfigWithPrunes(".cache", "snap")
		got := r.manifestPrunes()
		if len(got) != 2 || got[0] != ".cache" || got[1] != "snap" {
			t.Errorf("manifestPrunes = %q, want the configured prunes untouched", got)
		}
	})
}

// TestManifestNamesCannotForgeLines. Everything under "excluded from this
// backup" comes from somewhere a stranger can choose the bytes: the mount
// points and unreadable paths are names in the source tree, and the prunes and
// masks are configuration. text/template escapes nothing, so a newline in any
// of them used to start a record of its own on every disc, and an ESC or a C1
// control acted on the terminal of whoever catted the file.
func TestManifestNamesCannotForgeLines(t *testing.T) {
	forged := []string{
		"x\n  mask : *.jpg",      // a newline, forging a record
		"esc\x1b[2J",             // ESC, clearing the reader's screen
		"c1\u009b2J",             // CSI as a C1 control, UTF-8 encoded
		"raw\x9b2J",              // CSI as a raw byte in an invalid-UTF-8 name
		"osc\u009d0;pwned\u009c", // OSC ... ST, retitling the window
		"tab\tand\rcarriage",     // the rest of C0
	}
	r := &runner{}
	r.cfg = testConfigWithPrunes(append([]string{".cache"}, forged...)...)
	r.skippedMounts = forged
	r.unreadable = forged
	r.unreadableCount = len(forged)

	got := r.manifestPrunes()
	if len(got) != 1+3*len(forged) {
		t.Fatalf("manifestPrunes returned %d entries, want %d", len(got), 1+3*len(forged))
	}
	for _, e := range got {
		if strings.ContainsAny(e, "\n\r\t\x1b") || strings.ContainsRune(e, '\u009b') ||
			strings.ContainsRune(e, '\u009d') || strings.Contains(e, "\x9b") {
			t.Errorf("manifest entry %q still carries a control character", e)
		}
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		`"x\n  mask : *.jpg"  (mount point:`,
		`"x\n  mask : *.jpg"  (could not be read:`,
		`"raw\x9b2J"`,
		`"c1\u009b2J"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("manifestPrunes does not render %s, got:\n%s", want, joined)
		}
	}
	if got[0] != ".cache" {
		t.Errorf("an ordinary prune came out as %q", got[0])
	}
}

// TestDocPathLeavesOrdinaryNamesAlone: quoting is for names that could forge
// a line or drive a terminal, and for nothing else. A quote, a backslash or a
// name in any script is written exactly as it is, so every ordinary set's
// documents read as they always have.
func TestDocPathLeavesOrdinaryNamesAlone(t *testing.T) {
	for _, s := range []string{
		"/srv/backup/home",
		`it's "quoted"`,
		`back\slash`,
		"café/日本語/\u00a0nbsp",
		"\u2028line separator is not a terminal control",
		"\xff\xfe invalid but not C1",
		"",
	} {
		if got := docPath(s); got != s {
			t.Errorf("docPath(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestManifestSourceCannotForgeTheDiscCount. SOURCE_DIR is printed in
// MANIFEST.txt above "discs", the one line both readers parse, and both — and
// iso.Total, which labels the burned volumes — believe the first "discs" line
// they meet. A source path holding "\ndiscs : 1\n" in a non-final component
// passes configuration (ARCHIVE_NAME comes from the last one), so it made a
// two-disc set read as complete after one disc and label its second volume
// "02 OF 01".
func TestManifestSourceCannotForgeTheDiscCount(t *testing.T) {
	r := layoutRunner(t, filepath.Join(t.TempDir(), "staging"))
	r.cfg.SourceDir = "/srv/a\ndiscs : 1\nb/data"
	if err := r.makeDirs(); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if err := os.MkdirAll(filepath.Join(r.dirs.Discs, discDirName(n), "data"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.writeManifest(context.Background(), 2); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	body := readFileString(t, filepath.Join(r.cfg.Staging, "MANIFEST.txt"))
	var discLines []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "discs") {
			discLines = append(discLines, line)
		}
	}
	if len(discLines) != 1 || !strings.HasSuffix(discLines[0], ": 2") {
		t.Errorf("MANIFEST.txt has %d line(s) starting \"discs\" (%q), want the one real one, saying 2",
			len(discLines), discLines)
	}
	if got := iso.Total(r.cfg.Staging, 0); got != 2 {
		t.Errorf("iso.Total reads %d discs off the manifest, want 2", got)
	}
	if !strings.Contains(body, `source          : "/srv/a\ndiscs : 1\nb/data"`) {
		t.Errorf("the source line does not carry the quoted path:\n%s", body)
	}

	// The README carries the same path, under a heading a restorer reads first.
	if err := r.writeReadmes(context.Background(), 1); err != nil {
		t.Fatalf("writeReadmes: %v", err)
	}
	readme := readFileString(t, filepath.Join(r.dirs.Discs, discDirName(1), "README.md"))
	if strings.Contains(readme, "\ndiscs : 1\n") || !strings.Contains(readme, `"/srv/a\ndiscs : 1\nb/data"`) {
		t.Error("README.md carries the source path raw")
	}
}

// TestScanRecordsUnreadablePathsInsideTheArchive. The manifest lists what the
// scan could not read so that a restorer knows the set is short, and where:
// relative to SOURCE_DIR, like the prunes and the mount points beside it, and
// like the names --only takes. The scan reports the path it passed to the OS,
// so they reached the manifest absolute — the backing-up machine's own path —
// while the comment and the test above, which planted relative names, said
// otherwise.
func TestScanRecordsUnreadablePathsInsideTheArchive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root can read a mode-0000 file")
	}
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"readable.txt": 0o600, "secret/keys": 0o000} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
	}
	p, out := recordingPrinter()
	r := &runner{cfg: testConfigWithPrunes(), p: p}
	r.cfg.SourceDir = src
	if _, err := r.scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(r.unreadable) != 1 || r.unreadable[0] != "secret/keys" {
		t.Fatalf("unreadable = %q, want [\"secret/keys\"]", r.unreadable)
	}
	if got := strings.Join(r.manifestPrunes(), "\n"); !strings.Contains(got, "secret/keys  (could not be read") ||
		strings.Contains(got, src) {
		t.Errorf("manifestPrunes = %q, want secret/keys relative to SOURCE_DIR", got)
	}
	// The warning names the full path once, with what failed — not twice.
	abs := filepath.Join(src, "secret", "keys")
	if n := strings.Count(out(), abs); n != 1 {
		t.Errorf("the warning names %s %d time(s), want once:\n%s", abs, n, out())
	}
	if !strings.Contains(out(), "open: permission denied") {
		t.Errorf("the warning lost what failed:\n%s", out())
	}
}
