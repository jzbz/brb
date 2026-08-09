package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/doc"
)

// writeManifest puts a MANIFEST.txt in the staging root, exactly where ingest
// copies one off a disc.
func (e *env) writeManifest(body string) {
	e.t.Helper()
	if err := os.MkdirAll(e.cfg.Staging, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.Staging, manifestName), []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// manifestSaying renders a real MANIFEST.txt for a set of n discs, through the
// same template a backup uses, so the parser is tested against the bytes the
// tool actually writes rather than against a hand-typed approximation.
func manifestSaying(n int) string {
	return doc.RenderManifest(doc.ManifestData{
		Archive: "test", Created: "now", Host: "h", Source: "/src",
		Total: n, DiscType: "BD-R", Version: "test",
	})
}

func TestExpectedDiscsReadsTheRenderedManifest(t *testing.T) {
	e := newEnv(t)
	e.writeManifest(manifestSaying(7))
	got, ok := expectedDiscs(e.cfg.Staging)
	if !ok || got != 7 {
		t.Fatalf("expectedDiscs = (%d, %v), want (7, true)\nmanifest:\n%s", got, ok, manifestSaying(7))
	}
}

func TestExpectedDiscs(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  int
		wantK bool
	}{
		{"the manifest's own spacing", "archive name    : x\ndiscs           : 3\n", 3, true},
		{"no padding at all", "discs:12\n", 12, true},
		{"tabs around the colon", "discs\t:\t9\n", 9, true},
		{"more than 99 discs", "discs : 128\n", 128, true},
		{"no discs line", "archive name    : x\ndisc type       : BD-R\n", 0, false},
		{"empty file", "", 0, false},
		{"not a number", "discs           : lots\n", 0, false},
		{"a number with trailing junk", "discs           : 3 discs\n", 0, false},
		{"a signed number", "discs           : +3\n", 0, false},
		{"zero discs", "discs           : 0\n", 0, false},
		{"the word appears mid-line only", "  disc 01 of 03\n", 0, false},
		{"a longer field that starts the same way", "disc type       : BD-R\n", 0, false},
		{"the first discs line decides", "discs           : bad\ndiscs           : 4\n", 0, false},
		{"no newline at the end", "discs           : 5", 5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeManifest(tc.body)
			got, ok := expectedDiscs(e.cfg.Staging)
			if got != tc.want || ok != tc.wantK {
				t.Fatalf("expectedDiscs(%q) = (%d, %v), want (%d, %v)", tc.body, got, ok, tc.want, tc.wantK)
			}
		})
	}
}

func TestExpectedDiscsWithoutAManifest(t *testing.T) {
	e := newEnv(t)
	if got, ok := expectedDiscs(e.cfg.Staging); ok || got != 0 {
		t.Fatalf("expectedDiscs = (%d, %v), want (0, false) with no manifest", got, ok)
	}
	// A directory where the manifest should be is just as unusable, and must
	// not panic or error either.
	if err := os.MkdirAll(filepath.Join(e.cfg.Staging, manifestName), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, ok := expectedDiscs(e.cfg.Staging); ok || got != 0 {
		t.Fatalf("expectedDiscs = (%d, %v), want (0, false) for a directory", got, ok)
	}
}

func TestCheckComplete(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		have     []int
		want     int
		known    bool
		missing  string
		says     []string
		notSays  []string
	}{
		{
			name:     "every disc present",
			manifest: manifestSaying(3),
			have:     []int{1, 2, 3},
			want:     3, known: true, missing: "",
			says:    []string{"all 3 disc image(s) present"},
			notSays: []string{"MISSING", "will NOT be restored"},
		},
		{
			name:     "the last disc never ingested",
			manifest: manifestSaying(3),
			have:     []int{1, 2},
			want:     3, known: true, missing: "3",
			says: []string{"MANIFEST says 3 discs; 2 present. MISSING: 3", "files on those discs will NOT be restored"},
		},
		{
			name:     "a gap in the middle",
			manifest: manifestSaying(4),
			have:     []int{1, 4},
			want:     4, known: true, missing: "2 3",
			says: []string{"MISSING: 2 3"},
		},
		{
			name:     "a manifest that says nothing usable",
			manifest: "discs           : some\n",
			have:     []int{1},
			want:     0, known: false, missing: "",
			says:    []string{"cannot tell how many discs this set has"},
			notSays: []string{"MISSING"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeManifest(tc.manifest)
			var imgs []discFile
			for _, n := range tc.have {
				imgs = append(imgs, discFile{N: n, Path: filepath.Join(e.cfg.Dirs().Enc, encName(n))})
			}
			st := e.opts().checkComplete(imgs)
			if st.Want != tc.want || st.Known != tc.known {
				t.Errorf("status = {Want:%d Known:%v}, want {Want:%d Known:%v}", st.Want, st.Known, tc.want, tc.known)
			}
			if st.Have != len(tc.have) {
				t.Errorf("status.Have = %d, want %d", st.Have, len(tc.have))
			}
			if got := st.missingList(); got != tc.missing {
				t.Errorf("missingList = %q, want %q", got, tc.missing)
			}
			if st.Complete() != (tc.missing == "") {
				t.Errorf("Complete() = %v with missing %q", st.Complete(), tc.missing)
			}
			for _, s := range tc.says {
				if !strings.Contains(e.log(), s) {
					t.Errorf("log does not contain %q:\n%s", s, e.log())
				}
			}
			for _, s := range tc.notSays {
				if strings.Contains(e.log(), s) {
					t.Errorf("log should not contain %q:\n%s", s, e.log())
				}
			}
		})
	}
}

// TestCheckCompleteWithNoManifestNeverBlocks pins the rule that a bookkeeping
// file must not stand between an operator and their data: with no manifest at
// all the set counts as complete, so nothing downstream prompts or refuses.
func TestCheckCompleteWithNoManifestNeverBlocks(t *testing.T) {
	e := newEnv(t)
	st := e.opts().checkComplete([]discFile{{N: 1}})
	if !st.Complete() {
		t.Fatal("a set with no manifest was treated as incomplete")
	}
	if st.Known {
		t.Fatal("status claims to know the disc count without a manifest")
	}
	if !strings.Contains(e.log(), "cannot tell how many discs this set has") {
		t.Fatalf("the missing manifest was not reported:\n%s", e.log())
	}
}

func TestArchivePath(t *testing.T) {
	tests := []struct {
		line string
		want string
		ok   bool
	}{
		{"squashfs-root/a.txt", "a.txt", true},
		{"squashfs-root/sub/b.txt", "sub/b.txt", true},
		{"squashfs-root/with space.txt", "with space.txt", true},
		// A trailing CR is a byte of the name: unsquashfs terminates listing
		// lines with '\n' alone, so there is no line ending to strip here, and
		// stripping one made a file named "a.txt\r" unrestorable by --only.
		{"squashfs-root/a.txt\r", "a.txt\r", true},
		{"squashfs-root", "", false},
		{"squashfs-root/", "", false},
		{"", "", false},
		{"something-else/a.txt", "", false},
	}
	for _, tc := range tests {
		got, ok := archivePath(tc.line)
		if got != tc.want || ok != tc.ok {
			t.Errorf("archivePath(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCovers(t *testing.T) {
	tests := []struct {
		want, entry string
		ok          bool
	}{
		{"a.txt", "a.txt", true},
		{"a.txt", "a.txt.bak", false},
		{"a.txt", "sub/a.txt", false},
		{"sub", "sub/b.txt", true},
		{"sub", "sub", true},
		{"sub", "subway/b.txt", false},
		{"sub/", "sub/b.txt", true},
		{"/sub", "sub/b.txt", true},
		{"", "anything", true},
	}
	for _, tc := range tests {
		if got := covers(tc.want, tc.entry); got != tc.ok {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.want, tc.entry, got, tc.ok)
		}
	}
}

func TestQuoteAll(t *testing.T) {
	if got := quoteAll([]string{"a b.txt"}); got != "'a b.txt'" {
		t.Errorf("quoteAll = %q", got)
	}
	if got := quoteAll([]string{"a", "b"}); got != "'a', 'b'" {
		t.Errorf("quoteAll = %q", got)
	}
}

// TestCheckCompleteSurvivesAnAbsurdDiscCount: MANIFEST.txt is read off a disc,
// so a rotted or hand-edited digit is a normal input, and "discs : 3" is one
// byte away from "discs : 30000000". Naming every absent disc of a set that
// size built a 20-million-element slice and a 168 MB warning line before this
// was bounded. The check has to answer the same question in bounded work.
func TestCheckCompleteSurvivesAnAbsurdDiscCount(t *testing.T) {
	e := newEnv(t)
	e.writeManifest("discs           : 2000000000\n")

	done := make(chan setStatus, 1)
	go func() { done <- e.opts().checkComplete([]discFile{{N: 1}, {N: 2}}) }()
	var st setStatus
	select {
	case st = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("checkComplete did not return: it is walking every disc the manifest claims")
	}

	if st.Complete() {
		t.Error("a set two discs into two billion was called complete")
	}
	if st.MissingCount != 2000000000-2 {
		t.Errorf("MissingCount = %d, want %d", st.MissingCount, 2000000000-2)
	}
	if len(st.Missing) > maxNamedMissing {
		t.Errorf("named %d missing discs, want at most %d", len(st.Missing), maxNamedMissing)
	}
	if got := st.missingList(); !strings.HasPrefix(got, "3 4 5 ") || !strings.HasSuffix(got, " more") {
		t.Errorf("missingList = %q, want the first few numbers then a count", got)
	}
	if n := len(e.log()); n > 4<<10 {
		t.Errorf("the warning is %d bytes long; a manifest must not be able to flood the terminal", n)
	}
}
