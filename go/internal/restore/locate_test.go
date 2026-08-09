package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/indexfmt"
)

// HL-3. "brb restore --only <path>" used to par2-verify and decrypt every image
// in the set and filter the extraction afterwards: on a fifty-disc BD25 set,
// roughly 1.2 TB of hashing and decryption to retrieve one file, with every
// disc's plaintext written into the staging restore directory on the way. The
// encrypted index already records which disc holds what. These tests are about
// which images are opened, not about what comes out of them.

// threeDiscSetWithIndex stages a three-disc set with the encrypted index a
// backup leaves behind: keep.txt on disc 1, the lib/ directory split across
// discs 2 and 3, and main.c on disc 3 alone.
func threeDiscSetWithIndex(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"keep.txt": "one\n"})
	e.makeDisc(2, map[string]string{"lib/two.txt": "two\n"})
	e.makeDisc(3, map[string]string{
		"lib/three.txt":             "three\n",
		"project/core/src/main.c":   "int main(void){return 0;}\n",
		"project/core/src/notes.md": "notes\n",
	})
	e.writeManifest(manifestSaying(3))
	e.writeIndex(strings.Join([]string{
		indexfmt.FormatLine(1, "keep.txt"),
		indexfmt.FormatLine(2, "lib/two.txt"),
		indexfmt.FormatLine(3, "lib/three.txt"),
		indexfmt.FormatLine(3, "project/core/src/main.c"),
		indexfmt.FormatLine(3, "project/core/src/notes.md"),
	}, "\n") + "\n")
	return e
}

// prepared reports which discs a run actually decrypted, read back out of the
// log. It is the measurement the whole finding is about.
func prepared(log string) []int {
	var out []int
	for _, line := range strings.Split(log, "\n") {
		for n := 1; n <= 9; n++ {
			if strings.Contains(line, "preparing "+encName(n)) {
				out = append(out, n)
			}
		}
	}
	return out
}

func discsEqual(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestRestoreOnlyPreparesOnlyTheDiscTheIndexNames is HL-3 itself: a path the
// index puts on disc 3 must decrypt disc 3 and nothing else.
func TestRestoreOnlyPreparesOnlyTheDiscTheIndexNames(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"project/core/src/main.c"},
	})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()
	if got := prepared(log); !discsEqual(got, []int{3}) {
		t.Errorf("prepared disc(s) %v, want [3] — every other image was decrypted for nothing:\n%s", got, log)
	}
	if !strings.Contains(log, "the index puts 'project/core/src/main.c' on disc(s) 3") {
		t.Errorf("the run never said where the index put the path:\n%s", log)
	}
	body, err := os.ReadFile(filepath.Join(dest, "project/core/src/main.c"))
	if err != nil {
		t.Fatalf("the requested file was not restored: %v\n%s", err, log)
	}
	if string(body) != "int main(void){return 0;}\n" {
		t.Errorf("restored %q", body)
	}
	// Nothing else may have come back: the discs holding it were never opened.
	if _, err := os.Stat(filepath.Join(dest, "keep.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file from another disc was restored by --only: %v", err)
	}
	// And no other disc's plaintext was written into the restore directory.
	ents, err := os.ReadDir(e.cfg.Dirs().Restore)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		t.Errorf("the staging restore directory still holds %s", ent.Name())
	}
}

// TestRestoreOnlyPullsEveryDiscThePathSpans: a directory the packer split must
// bring back both halves, so every matching disc is collected, not the first.
func TestRestoreOnlyPullsEveryDiscThePathSpans(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Only: []string{"lib"}})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()
	if got := prepared(log); !discsEqual(got, []int{2, 3}) {
		t.Errorf("prepared disc(s) %v, want [2 3]:\n%s", got, log)
	}
	for rel, want := range map[string]string{"lib/two.txt": "two\n", "lib/three.txt": "three\n"} {
		body, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v\n%s", rel, err, log)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", rel, body, want)
		}
	}
}

// TestRestoreOnlySeveralPathsUnionsTheirDiscs: two requested paths on two
// different discs open those two discs and leave the third alone.
func TestRestoreOnlySeveralPathsUnionsTheirDiscs(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"keep.txt", "lib/three.txt"},
	})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	if got := prepared(e.log()); !discsEqual(got, []int{1, 3}) {
		t.Errorf("prepared disc(s) %v, want [1 3]:\n%s", got, e.log())
	}
	for _, rel := range []string{"keep.txt", "lib/three.txt"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s: %v\n%s", rel, err, e.log())
		}
	}
}

// TestRestoreOnlyRejectsAPathTheIndexDoesNotHave: brb.sh dies rather than
// decrypting the whole set to prove a negative, and says how to override it.
func TestRestoreOnlyRejectsAPathTheIndexDoesNotHave(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)

	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: filepath.Join(e.dir, "dest"), Only: []string{"nowhere/at/all.txt"},
	})
	if err == nil {
		t.Fatalf("Restore accepted a path that is not in the index\n%s", e.log())
	}
	for _, want := range []string{"is not in the index", "brb index", "--disc N"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if got := prepared(e.log()); len(got) != 0 {
		t.Errorf("images %v were decrypted before the index was consulted:\n%s", got, e.log())
	}
}

// TestRestoreOnlyWarnsAboutADiscThatWasNeverIngested: the index names a disc
// that is not in staging. The part that is here must still come back, and the
// disc that is missing must be named — "ingest disc 3" is the whole remedy.
func TestRestoreOnlyWarnsAboutADiscThatWasNeverIngested(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)
	if err := os.Remove(filepath.Join(e.cfg.Dirs().Enc, encName(3))); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Only: []string{"lib"}})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()
	if !strings.Contains(log, "'lib' is partly on disc 3, whose image is not in") {
		t.Errorf("the missing disc was not named:\n%s", log)
	}
	if got := prepared(log); !discsEqual(got, []int{2}) {
		t.Errorf("prepared disc(s) %v, want [2]:\n%s", got, log)
	}
	if _, err := os.Stat(filepath.Join(dest, "lib", "two.txt")); err != nil {
		t.Errorf("the half that was ingested did not come back: %v", err)
	}
}

// TestRestoreOnlyFailsWhenNoNamedDiscIsIngested: nothing to do and nothing to
// decrypt, so say which discs to fetch instead of opening the ones that are
// here.
func TestRestoreOnlyFailsWhenNoNamedDiscIsIngested(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)
	if err := os.Remove(filepath.Join(e.cfg.Dirs().Enc, encName(3))); err != nil {
		t.Fatal(err)
	}

	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: filepath.Join(e.dir, "dest"), Only: []string{"project/core/src/main.c"},
	})
	if err == nil {
		t.Fatalf("Restore succeeded with the only disc holding the path absent\n%s", e.log())
	}
	if !strings.Contains(err.Error(), "is on disc(s) 3, none of which have been ingested") {
		t.Errorf("error %q does not say which disc to ingest", err)
	}
	if got := prepared(e.log()); len(got) != 0 {
		t.Errorf("images %v were decrypted anyway:\n%s", got, e.log())
	}
}

// TestRestoreOnlyWithDiscIntersects: --only and --disc are both honoured. The
// index may not widen the operator's choice of disc, and when it is certain the
// path is on another one it says so before an image is decrypted.
func TestRestoreOnlyWithDiscIntersects(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)

	// The path is on disc 3; --disc 1 must not decrypt disc 1 to find out.
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: filepath.Join(e.dir, "a"), Only: []string{"project/core/src/main.c"}, Disc: 1,
	})
	if err == nil {
		t.Fatalf("Restore searched disc 1 for a path the index puts on disc 3\n%s", e.log())
	}
	for _, want := range []string{"on disc(s) 3", "not on disc 1", "--disc 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if got := prepared(e.log()); len(got) != 0 {
		t.Errorf("disc(s) %v were decrypted anyway:\n%s", got, e.log())
	}

	// The same request with the disc the index agrees with works, and stays
	// confined to that one disc.
	e.out.Reset()
	dest := filepath.Join(e.dir, "b")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"project/core/src/main.c"}, Disc: 3,
	}); err != nil {
		t.Fatalf("Restore --only --disc 3: %v\n%s", err, e.log())
	}
	if got := prepared(e.log()); !discsEqual(got, []int{3}) {
		t.Errorf("prepared disc(s) %v, want [3]:\n%s", got, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest, "project/core/src/main.c")); err != nil {
		t.Errorf("the file was not restored: %v\n%s", err, e.log())
	}
}

// TestRestoreOnlyWithDiscKeepsADiscTheIndexDoesNotKnow: an explicit --disc must
// still be searched when the index has never heard of the path — the index can
// be older than the disc, and the image is the authority on its own contents.
func TestRestoreOnlyWithDiscKeepsADiscTheIndexDoesNotKnow(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)

	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: filepath.Join(e.dir, "dest"), Only: []string{"project/core/src/notes.md"}, Disc: 3,
	})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}

	// Same again for a path the index does not list at all.
	e.out.Reset()
	err = Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: filepath.Join(e.dir, "d2"), Only: []string{"unlisted.txt"}, Disc: 3,
	})
	if err == nil {
		t.Fatalf("expected the disc itself to report the path missing\n%s", e.log())
	}
	if !strings.Contains(e.log(), "the index does not list 'unlisted.txt'; searching disc 3 anyway") {
		t.Errorf("the run did not say it was overriding the index:\n%s", e.log())
	}
	if got := prepared(e.log()); !discsEqual(got, []int{3}) {
		t.Errorf("prepared disc(s) %v, want [3] — --disc was not honoured:\n%s", got, e.log())
	}
	if !strings.Contains(err.Error(), "disc 3, the only disc searched") {
		t.Errorf("error %q does not say how little was searched", err)
	}
}

// TestRestoreOnlyWithoutAnIndexFallsBack: an old set, or one disc ingested on
// its own, has no index. That must still restore the way it always did — and
// must say that it is opening every image, because that is the cost --only
// exists to avoid.
func TestRestoreOnlyWithoutAnIndexFallsBack(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)
	if err := os.Remove(filepath.Join(e.cfg.Dirs().Enc, indexName)); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"project/core/src/main.c"},
	})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()
	if got := prepared(log); !discsEqual(got, []int{1, 2, 3}) {
		t.Errorf("prepared disc(s) %v, want all three: the fallback must not narrow anything:\n%s", got, log)
	}
	for _, want := range []string{
		"no " + indexName,
		"every one of the 3 ingested image(s) will be decrypted",
		"pass --disc N",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the fallback was not announced (%q missing):\n%s", want, log)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "project/core/src/main.c")); err != nil {
		t.Errorf("the file was not restored: %v", err)
	}
}

// TestRestoreOnlyWithAnUnreadableIndexFallsBack: a corrupt index must not be
// the end of a restore that can still be done the slow way.
func TestRestoreOnlyWithAnUnreadableIndexFallsBack(t *testing.T) {
	e := threeDiscSetWithIndex(t)
	e.ui.SetAssumeYes(true)
	idx := filepath.Join(e.cfg.Dirs().Enc, indexName)
	if err := os.WriteFile(idx, []byte("this is not an age file at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"project/core/src/main.c"},
	})
	if err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "the encrypted index could not be read") {
		t.Errorf("the run did not say why it was searching everything:\n%s", e.log())
	}
	if got := prepared(e.log()); !discsEqual(got, []int{1, 2, 3}) {
		t.Errorf("prepared disc(s) %v, want all three:\n%s", got, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest, "project/core/src/main.c")); err != nil {
		t.Errorf("the file was not restored: %v", err)
	}
}

// TestIndexDiscsResolvesThroughTheParsedForm: the index escapes backslashes,
// tabs and newlines, so a path is matched after unescaping and never against
// the raw record — which would both miss the files whose names made escaping
// necessary and match against the disc-number field.
func TestIndexDiscsResolvesThroughTheParsedForm(t *testing.T) {
	e := newEnv(t)
	odd := "notes\tand\\more/2 lines\nhere.txt"
	e.writeIndex(strings.Join([]string{
		indexfmt.FormatLine(1, "a/plain.txt"),
		indexfmt.FormatLine(2, odd),
		indexfmt.FormatLine(4, "dir/sub/deep.txt"),
		indexfmt.FormatLine(7, "dir/other.txt"),
	}, "\n") + "\n")

	got, err := e.opts().indexDiscs(context.Background(), []string{odd, "dir", "a/plain.txt", "2"})
	if err != nil {
		t.Fatalf("indexDiscs: %v\n%s", err, e.log())
	}
	if !discsEqual(got[odd], []int{2}) {
		t.Errorf("the escaped path resolved to %v, want [2]", got[odd])
	}
	if !discsEqual(got["dir"], []int{4, 7}) {
		t.Errorf("the directory resolved to %v, want [4 7]", got["dir"])
	}
	if !discsEqual(got["a/plain.txt"], []int{1}) {
		t.Errorf("the plain path resolved to %v, want [1]", got["a/plain.txt"])
	}
	// "2" is a disc number in the second record and a substring of nothing
	// else. brb.sh's grep would match it; resolving on the parsed path must
	// not.
	if len(got["2"]) != 0 {
		t.Errorf("a disc number matched as a path: %v", got["2"])
	}
}

// TestIndexDiscsWithoutAnIndexIsRecognisable: the caller distinguishes "no
// index" from "the index would not read", because only the first is ordinary.
func TestIndexDiscsWithoutAnIndexIsRecognisable(t *testing.T) {
	e := newEnv(t)
	_, err := e.opts().indexDiscs(context.Background(), []string{"a.txt"})
	if !errors.Is(err, errNoIndex) {
		t.Fatalf("indexDiscs = %v, want errNoIndex", err)
	}
}

// TestIndexDiscsIgnoresUnparseableRecords: escaping means no filename can
// produce a record without a tab, so one is corruption the hash check missed.
// The rest of the index is still an answer, and the damage is reported.
func TestIndexDiscsIgnoresUnparseableRecords(t *testing.T) {
	e := newEnv(t)
	e.writeIndex(fmt.Sprintf("%s\n%s\n%s\n",
		indexfmt.FormatLine(1, "good.txt"), "this record has no tab", indexfmt.FormatLine(2, "also-good.txt")))

	got, err := e.opts().indexDiscs(context.Background(), []string{"good.txt", "also-good.txt"})
	if err != nil {
		t.Fatalf("indexDiscs: %v\n%s", err, e.log())
	}
	if !discsEqual(got["good.txt"], []int{1}) || !discsEqual(got["also-good.txt"], []int{2}) {
		t.Errorf("the sound records did not resolve: %v", got)
	}
	if !strings.Contains(e.log(), "1 record(s) in "+indexName+" do not parse") {
		t.Errorf("the damaged record was not reported:\n%s", e.log())
	}
}
