package restore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
)

// squashTools returns a tool set able to build and extract squashfs images,
// skipping the test when the machine cannot. Everything in this file needs real
// images: the bugs it covers are about what unsquashfs does with a path it does
// not have, which no fake can reproduce.
func (e *env) squashTools(t *testing.T) *tools.Set {
	t.Helper()
	s := realTools(t, tools.Mksquashfs, tools.Unsquashfs)
	if !s.MksquashfsHasCpioStyle0(context.Background()) {
		t.Skip("skipping: this mksquashfs has no -cpiostyle0")
	}
	e.tools = s
	return s
}

// makeDisc writes disc n's image into the staging enc directory, holding
// exactly the given files, encrypted with both recorded hashes beside it —
// the same artifacts a backup leaves behind.
func (e *env) makeDisc(n int, files map[string]string) {
	e.t.Helper()
	ctx := context.Background()

	src := filepath.Join(e.dir, fmt.Sprintf("src%02d", n))
	for rel, body := range files {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}

	// mksquashfs is fed an explicit list, so the directories have to be in it
	// as well as the files.
	var list []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel != "." {
			list = append(list, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(list)

	base := strings.TrimSuffix(encName(n), ageExt)
	img := filepath.Join(e.dir, base)
	if err := e.tools.BuildImage(ctx, tools.MkOptions{
		SourceDir: src, Out: img, Files: list, Compression: "none", BlockSize: "128K",
	}); err != nil {
		e.t.Fatalf("BuildImage disc %d: %v", n, err)
	}

	enc := filepath.Join(e.cfg.Dirs().Enc, encName(n))
	sums, err := agecrypt.Encrypt(ctx, img, enc, e.recipients, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := agecrypt.WriteSumFile(enc+sumExt, sums.Cipher, encName(n)); err != nil {
		e.t.Fatal(err)
	}
	if err := agecrypt.WriteSumFile(filepath.Join(e.cfg.Dirs().Enc, base+sumExt), sums.Plain, base); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Remove(img); err != nil {
		e.t.Fatal(err)
	}
}

// threeDiscSetMissingTheLast stages discs 1 and 2 of a set whose manifest says
// three, which is the shape of HL-1: every disc carries the whole directory
// skeleton, so the restored tree looks complete and is not.
func threeDiscSetMissingTheLast(t *testing.T) *env {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"project/core/src/keep.txt": "one\n"})
	e.makeDisc(2, map[string]string{"lib/other.txt": "two\n"})
	e.writeManifest(manifestSaying(3))
	return e
}

// TestRestoreOfAPartialSetAnnouncesTheMissingDiscs is HL-1: two discs of three,
// restored under --yes. It may proceed, but it must name disc 3, say plainly
// that the files on it are absent, and it must not read like an ordinary clean
// success.
func TestRestoreOfAPartialSetAnnouncesTheMissingDiscs(t *testing.T) {
	e := threeDiscSetMissingTheLast(t)
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()

	for _, want := range []string{
		"MANIFEST says 3 discs; 2 present. MISSING: 3",
		"files on those discs will NOT be restored",
		"this restore was PARTIAL",
		"partial restore complete",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the run never said %q:\n%s", want, log)
		}
	}
	// The closing line must not be the one a clean run prints; an operator
	// scanning for "ok restore complete: <dest>" must not find it.
	if strings.Contains(log, "ok restore complete:") {
		t.Errorf("a partial restore ended on the clean success line:\n%s", log)
	}
	// The xcompat suite's assertion, run verbatim against the real output.
	if !strings.Contains(strings.ToLower(log), "missing") {
		t.Errorf("the suite's grep for /missing|incomplete|partial/ would not match:\n%s", log)
	}
	// ...and the restore really did happen, so the warning is about content
	// and not about a refusal.
	if _, err := os.Stat(filepath.Join(dest, "project", "core", "src", "keep.txt")); err != nil {
		t.Errorf("the discs that were present were not restored: %v", err)
	}
}

// TestRestoreOfAPartialSetNeedsConfirmationWhenInteractive covers the other
// half of HL-1: without --yes the operator is asked, and "no" stops the run.
func TestRestoreOfAPartialSetNeedsConfirmationWhenInteractive(t *testing.T) {
	e := threeDiscSetMissingTheLast(t)
	e.ui.SetInput(strings.NewReader("n\n"))

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest})
	if err == nil {
		t.Fatalf("Restore of a declined partial set succeeded\n%s", e.log())
	}
	if !strings.Contains(err.Error(), "aborted") || !strings.Contains(err.Error(), "3") {
		t.Errorf("error = %v\nwant it to say the run was aborted and name disc 3", err)
	}
	if !strings.Contains(e.log(), "Restore the partial set anyway?") {
		t.Errorf("the operator was never asked:\n%s", e.log())
	}
	if _, statErr := os.Stat(filepath.Join(dest, "project")); statErr == nil {
		t.Error("a declined restore still extracted something")
	}
}

// TestRestoreOfACompleteSetSaysSo is the other side of the check: it must not
// cry wolf. A set with every disc present restores clean, with no warning and
// no prompt.
func TestRestoreOfACompleteSetSaysSo(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"a.txt": "one\n"})
	e.makeDisc(2, map[string]string{"b.txt": "two\n"})
	e.writeManifest(manifestSaying(2))
	// Deliberately neither --yes nor any input: a spurious prompt would fail
	// the restore rather than pass unnoticed.
	e.ui.SetInput(strings.NewReader(""))

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()
	if !strings.Contains(log, "all 2 disc image(s) present") {
		t.Errorf("the completeness check did not report success:\n%s", log)
	}
	for _, unwanted := range []string{"MISSING", "PARTIAL", "partial restore", "Restore the partial set"} {
		if strings.Contains(log, unwanted) {
			t.Errorf("a complete set produced %q:\n%s", unwanted, log)
		}
	}
	if !strings.Contains(log, "ok restore complete: "+dest) {
		t.Errorf("a complete restore did not end on the clean success line:\n%s", log)
	}
	for _, rel := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("%s was not restored: %v", rel, err)
		}
	}
}

// TestRestoreOfOneNamedDiscSkipsTheCheck: --disc N is a deliberate single-disc
// restore, so it must not be told the other discs are missing and must not be
// asked to confirm anything.
func TestRestoreOfOneNamedDiscSkipsTheCheck(t *testing.T) {
	e := threeDiscSetMissingTheLast(t)
	// An empty input source: any prompt at all answers "no" and fails the run.
	e.ui.SetInput(strings.NewReader(""))

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Disc: 1}); err != nil {
		t.Fatalf("Restore --disc 1: %v\n%s", err, e.log())
	}
	log := e.log()
	for _, unwanted := range []string{"MISSING", "PARTIAL", "Restore the partial set", "cannot tell how many discs"} {
		if strings.Contains(log, unwanted) {
			t.Errorf("--disc 1 tripped the completeness check with %q:\n%s", unwanted, log)
		}
	}
	if !strings.Contains(log, "ok restore complete: "+dest) {
		t.Errorf("--disc 1 did not report a clean restore:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(dest, "project", "core", "src", "keep.txt")); err != nil {
		t.Errorf("disc 1's file was not restored: %v", err)
	}
}

// TestRestoreWithoutAManifestWarnsAndProceeds: the manifest is bookkeeping, and
// losing it must cost a warning, not the restore.
func TestRestoreWithoutAManifestWarnsAndProceeds(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"a.txt": "one\n"})
	e.ui.SetInput(strings.NewReader(""))

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	log := e.log()
	if !strings.Contains(log, "cannot tell how many discs this set has") {
		t.Errorf("the missing manifest was not reported:\n%s", log)
	}
	if strings.Contains(log, "Restore the partial set") {
		t.Errorf("a missing manifest triggered the partial-set prompt:\n%s", log)
	}
	if !strings.Contains(log, "ok restore complete: "+dest) {
		t.Errorf("the restore did not complete:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err != nil {
		t.Errorf("a.txt was not restored: %v", err)
	}
}

// TestRestoreOnlyFailsWhenNothingMatched is IDX-6: unsquashfs exits 0 having
// created nothing when the path is not in the image, so a --only run that
// matched nothing used to print "ok restore complete" and exit 0 over an empty
// directory.
func TestRestoreOnlyFailsWhenNothingMatched(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"a.txt": "one\n", "sub/b.txt": "two\n"})
	e.makeDisc(2, map[string]string{"lib/c.txt": "three\n"})
	e.writeManifest(manifestSaying(2))
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"definitely-not-in-the-archive.txt"},
	})
	if err == nil {
		t.Fatalf("--only matched nothing yet Restore succeeded\n%s", e.log())
	}
	for _, want := range []string{"definitely-not-in-the-archive.txt", "was not found on any", "brb index"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to contain %q", err, want)
		}
	}
	if strings.Contains(e.log(), "restore complete") {
		t.Errorf("a failed --only still claimed the restore was complete:\n%s", e.log())
	}
	// Nothing was extracted, and the run says which discs it looked at.
	var files int
	_ = filepath.WalkDir(dest, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Errorf("a failed --only left %d file(s) in %s", files, dest)
	}
}

// TestRestoreOnlyExtractsAPathThatIsThere is the other half of IDX-6: the
// stricter failure must not break the case it exists to protect.
func TestRestoreOnlyExtractsAPathThatIsThere(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"a.txt": "one\n", "sub/b.txt": "two\n"})
	e.makeDisc(2, map[string]string{"lib/c.txt": "three\n"})
	e.writeManifest(manifestSaying(2))
	e.ui.SetAssumeYes(true)

	// The path lives on disc 2 only, so disc 1 has to be skipped rather than
	// counted as a successful extraction of nothing.
	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"lib/c.txt"},
	}); err != nil {
		t.Fatalf("Restore --only: %v\n%s", err, e.log())
	}
	body, err := os.ReadFile(filepath.Join(dest, "lib", "c.txt"))
	if err != nil {
		t.Fatalf("the requested path was not restored: %v\n%s", err, e.log())
	}
	if string(body) != "three\n" {
		t.Errorf("lib/c.txt = %q, want %q", body, "three\n")
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err == nil {
		t.Error("--only extracted a path that was not asked for")
	}
	log := e.log()
	// The status lines quote archive paths (%q) so control bytes in a name are
	// shown rather than executed by the terminal.
	if !strings.Contains(log, `"lib/c.txt" is not on disc01.squashfs`) {
		t.Errorf("the disc that does not hold the path was not reported as skipped:\n%s", log)
	}
	if !strings.Contains(log, `"lib/c.txt" extracted from disc02.squashfs`) {
		t.Errorf("the run does not report what it actually extracted:\n%s", log)
	}
	if !strings.Contains(log, "extracted 1 requested path(s) from 1 of 2 image(s)") {
		t.Errorf("the summary does not report what was extracted:\n%s", log)
	}
}

// TestRestoreOnlyFailsWhenOneOfSeveralPathsIsMissing: recovering two of the
// three files an operator asked for and calling it a success is the same
// failure as IDX-6, smaller. brb.sh takes a single --only path and so has no
// behaviour here to match; failing is the safe reading of its rule.
func TestRestoreOnlyFailsWhenOneOfSeveralPathsIsMissing(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"a.txt": "one\n"})
	e.writeManifest(manifestSaying(1))
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"a.txt", "gone.txt"},
	})
	if err == nil {
		t.Fatalf("Restore succeeded although gone.txt was on no disc\n%s", e.log())
	}
	if !strings.Contains(err.Error(), "gone.txt") {
		t.Errorf("error = %v\nwant it to name gone.txt", err)
	}
	if strings.Contains(err.Error(), "'a.txt'") {
		t.Errorf("error = %v\nblames a.txt, which was found", err)
	}
}

// TestRestoreOnlyAcceptsADirectory: --only names a path inside the archive, and
// a directory is a legitimate one. The presence check must not turn that into
// "not found on any disc".
func TestRestoreOnlyAcceptsADirectory(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"sub/b.txt": "two\n", "sub/deep/c.txt": "three\n", "a.txt": "one\n"})
	e.writeManifest(manifestSaying(1))
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{
		Dest: dest, Only: []string{"sub"},
	}); err != nil {
		t.Fatalf("Restore --only sub: %v\n%s", err, e.log())
	}
	for _, rel := range []string{"sub/b.txt", "sub/deep/c.txt"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not restored: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err == nil {
		t.Error("--only sub extracted a.txt as well")
	}
}

// TestPathsPresentAgainstARealImage exercises the listing reader directly,
// including the names that are awkward everywhere else in this tool.
func TestPathsPresentAgainstARealImage(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{
		"a.txt":              "one\n",
		"sub/b.txt":          "two\n",
		"with space.txt":     "three\n",
		"odd/a\tb.txt":       "four\n",
		"odd/back\\slash.tx": "five\n",
	})
	ctx := context.Background()
	plain, err := PrepareImage(ctx, e.opts(), filepath.Join(e.cfg.Dirs().Enc, encName(1)))
	if err != nil {
		t.Fatalf("PrepareImage: %v\n%s", err, e.log())
	}

	tests := []struct {
		name string
		want []string
		got  []string
	}{
		{"an exact file", []string{"a.txt"}, []string{"a.txt"}},
		{"a directory", []string{"sub"}, []string{"sub"}},
		{"a path with a space", []string{"with space.txt"}, []string{"with space.txt"}},
		{"a path with a tab", []string{"odd/a\tb.txt"}, []string{"odd/a\tb.txt"}},
		{"a path with a backslash", []string{"odd/back\\slash.tx"}, []string{"odd/back\\slash.tx"}},
		{"nothing there", []string{"nope.txt"}, nil},
		{"a prefix that is not a path", []string{"a.tx"}, nil},
		{"some there, some not", []string{"nope.txt", "a.txt", "sub/b.txt"}, []string{"a.txt", "sub/b.txt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.opts().pathsPresent(ctx, plain, tc.want)
			if err != nil {
				t.Fatalf("pathsPresent: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.got, "\x00") {
				t.Fatalf("pathsPresent(%q) = %q, want %q", tc.want, got, tc.got)
			}
		})
	}

	// A cancelled context must not be reported as "the image does not hold it".
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.opts().pathsPresent(cancelled, plain, []string{"a.txt"}); err == nil {
		t.Error("pathsPresent ignored a cancelled context")
	}
}

// TestIngestReportsTheSetItHasSoFar: brb.sh runs the same check at the end of
// ingest, while the operator is still at the drive with the box of discs.
func TestIngestReportsTheSetItHasSoFar(t *testing.T) {
	e := newEnv(t)
	e.writeManifest(manifestSaying(3))
	for _, n := range []int{1, 2} {
		e.writeImage(n, []byte("ciphertext\n"))
	}
	ig := &ingester{o: e.opts()}
	if err := ig.finish(); err != nil {
		t.Fatalf("finish: %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "MISSING: 3") {
		t.Fatalf("ingest did not name the disc still to come:\n%s", e.log())
	}
}

// TestRestoreOnlyExtractsPathsUnsquashfsWouldTreatAsPatterns is the sharpest
// form of IDX-6 that survived the first fix. unsquashfs reads an extraction
// path as a wildcard pattern unless it is told not to, and in that syntax `\`
// escapes the next character: asking for the real file `e\f.txt` matched
// nothing, unsquashfs exited 0 having created nothing, and the presence check
// in front of it had already said the path was there — so the run reported
// "restore complete", exit 0, over an empty directory. That is the exact
// failure IDX-6 names, for every path the index escaping exists to support.
func TestRestoreOnlyExtractsPathsUnsquashfsWouldTreatAsPatterns(t *testing.T) {
	awkward := []string{
		`e\f.txt`,                // a backslash mid-name
		`endsbackslash\`,         // a trailing backslash
		`\\\`,                    // nothing but backslashes
		`lit\tbackslash-t.txt`,   // the two characters \ and t, not a tab
		"tab\tname.txt",          // a real tab
		"star*.txt",              // a wildcard metacharacter in the name
		"brackets[1].txt",        // another
		"question?.txt",          // another
		"sub/deep\\er/plain.txt", // a backslash in a directory component
	}
	files := make(map[string]string, len(awkward))
	for i, p := range awkward {
		files[p] = fmt.Sprintf("body %d\n", i)
	}

	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, files)
	e.writeManifest(manifestSaying(1))
	e.ui.SetAssumeYes(true)

	for _, p := range awkward {
		t.Run(p, func(t *testing.T) {
			dest := t.TempDir()
			if err := Restore(context.Background(), e.opts(), RestoreOptions{
				Dest: dest, Only: []string{p},
			}); err != nil {
				t.Fatalf("Restore --only %q: %v\n%s", p, err, e.log())
			}
			body, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
			if err != nil {
				t.Fatalf("--only %q reported success but the file is not there: %v\n%s",
					p, err, e.log())
			}
			if string(body) != files[p] {
				t.Errorf("%q = %q, want %q", p, body, files[p])
			}
			// A wildcard would have pulled in the other files as well.
			n := 0
			_ = filepath.WalkDir(dest, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					n++
				}
				return nil
			})
			if n != 1 {
				t.Errorf("--only %q extracted %d files, want 1", p, n)
			}
		})
	}
}
