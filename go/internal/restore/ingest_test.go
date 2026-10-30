package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/tools"
)

// TestIngestTerminationRules pins down the behaviour brb.sh gets wrong: its
// loop calls prompt_enter and confirm, both of which succeed immediately under
// --yes, so "brb --yes ingest" re-copies the same disc forever.
func TestIngestTerminationRules(t *testing.T) {
	tests := []struct {
		name      string
		assumeYes bool
		input     string
		wantDiscs int
	}{
		{
			name:      "assume-yes ingests the mounted disc exactly once",
			assumeYes: true,
			input:     "",
			wantDiscs: 1,
		},
		{
			name:      "end of input at the first prompt stops before any disc",
			input:     "",
			wantDiscs: 0,
		},
		{
			name:      "one disc, then no",
			input:     "\nn\n",
			wantDiscs: 1,
		},
		{
			name:      "one disc, then end of input at the confirmation",
			input:     "\n",
			wantDiscs: 1,
		},
		{
			name:      "three discs, then no",
			input:     "\ny\n\ny\n\nn\n",
			wantDiscs: 3,
		},
		{
			name:      "an answer that is not yes stops the loop",
			input:     "\nmaybe\n",
			wantDiscs: 1,
		},
		{
			name:      "yes, then end of input at the next prompt",
			input:     "\ny\n",
			wantDiscs: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.ui.SetAssumeYes(tc.assumeYes)
			e.ui.SetInput(strings.NewReader(tc.input))

			calls := 0
			ig := &ingester{o: e.opts()}
			ig.one = func(context.Context) (bool, error) {
				calls++
				if calls > 20 {
					t.Fatal("the ingest loop is not terminating")
				}
				// Every fake pass stages something, so it counts as a disc.
				return true, nil
			}
			if err := ig.run(context.Background()); err != nil {
				t.Fatalf("run: %v\n%s", err, e.log())
			}
			if calls != tc.wantDiscs {
				t.Fatalf("ingested %d disc(s), want %d\n%s", calls, tc.wantDiscs, e.log())
			}
			if ig.discs != tc.wantDiscs {
				t.Fatalf("recorded %d disc(s), want %d", ig.discs, tc.wantDiscs)
			}
		})
	}
}

func TestIngestUnderAssumeYesReportsAFailedDisc(t *testing.T) {
	e := newEnv(t)
	e.ui.SetAssumeYes(true)

	want := errors.New("no disc in the drive")
	ig := &ingester{o: e.opts(), one: func(context.Context) (bool, error) { return false, want }}
	if err := ig.run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("run = %v, want %v", err, want)
	}
}

func TestIngestKeepsGoingAfterABadDiscButStillFails(t *testing.T) {
	e := newEnv(t)
	e.ui.SetInput(strings.NewReader("\ny\n\nn\n"))

	calls := 0
	ig := &ingester{o: e.opts()}
	ig.one = func(context.Context) (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("this disc has no data/ directory")
		}
		return true, nil
	}
	err := ig.run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded despite an unreadable disc\n%s", e.log())
	}
	if calls != 2 {
		t.Fatalf("ingested %d disc(s), want 2", calls)
	}
	if ig.discs != 1 {
		t.Fatalf("counted %d good disc(s), want 1", ig.discs)
	}
}

func TestIngestStopsOnCancellation(t *testing.T) {
	e := newEnv(t)
	e.ui.SetInput(strings.NewReader(strings.Repeat("\ny\n", 50)))

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	ig := &ingester{o: e.opts()}
	ig.one = func(context.Context) (bool, error) {
		calls++
		cancel()
		return true, nil
	}
	if err := ig.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("ingested %d disc(s) after cancellation, want 1", calls)
	}
}

// TestIngestFileRecognisesWhatIsAlreadyStaged covers the rule that a staged
// copy is only replaced when it is known to be wrong.
func TestIngestFileRecognisesWhatIsAlreadyStaged(t *testing.T) {
	body := []byte("the encrypted image, pretend\n")
	sum := sha512Hex(t, body)

	tests := []struct {
		name string
		// staged is what is already in the staging directory; nil means nothing.
		staged []byte
		// want is the recorded hash to pass in, "" for none.
		want       string
		wantResult []byte
		wantLog    string
	}{
		{
			name:       "nothing staged yet: copied",
			want:       sum,
			wantResult: body,
			wantLog:    "copied and verified",
		},
		{
			name:       "identical copy already staged and verified",
			staged:     body,
			want:       sum,
			wantResult: body,
			wantLog:    "matches the hash on this disc",
		},
		{
			// The replacement is copied to a sibling name and hashed before the
			// staged copy is touched: the staged bytes are never destroyed
			// ahead of the proof that the disc's copy is better.
			name:       "damaged copy staged, replaced by this disc's verified copy",
			staged:     []byte("the encrypted image, damaged\n"),
			want:       sum,
			wantResult: body,
			wantLog:    "replaced the staged",
		},
		{
			name:       "identical copy staged, no recorded hash",
			staged:     body,
			wantResult: body,
			wantLog:    "byte for byte",
		},
		{
			name:       "different copy of the same size, no recorded hash: kept",
			staged:     []byte("the encrypted image, DAMAGED\n"),
			wantResult: []byte("the encrypted image, DAMAGED\n"),
			wantLog:    "keeping the staged copy",
		},
		{
			name:       "different size, no recorded hash: kept",
			staged:     []byte("short\n"),
			wantResult: []byte("short\n"),
			wantLog:    "differs in size",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			src := filepath.Join(e.dir, "disc01.squashfs.age")
			if err := os.WriteFile(src, body, 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(e.cfg.Dirs().Enc, "disc01.squashfs.age")
			if tc.staged != nil {
				if err := os.WriteFile(dst, tc.staged, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.opts().ingestFile(context.Background(), src, dst, tc.want); err != nil {
				t.Fatalf("ingestFile: %v\n%s", err, e.log())
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("read staged file: %v", err)
			}
			if string(got) != string(tc.wantResult) {
				t.Fatalf("staged file = %q, want %q", got, tc.wantResult)
			}
			if !strings.Contains(e.log(), tc.wantLog) {
				t.Fatalf("log does not mention %q:\n%s", tc.wantLog, e.log())
			}
		})
	}
}

func TestIngestFileReportsACopyThatDoesNotMatchTheDiscsOwnHash(t *testing.T) {
	e := newEnv(t)
	src := filepath.Join(e.dir, "disc01.squashfs.age")
	if err := os.WriteFile(src, []byte("what the drive actually returned"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.cfg.Dirs().Enc, "disc01.squashfs.age")

	_, err := e.opts().ingestFile(context.Background(), src, dst, strings.Repeat("cd", 64))
	if !errors.Is(err, ErrIncompleteCopy) {
		t.Fatalf("ingestFile = %v, want an ErrIncompleteCopy", err)
	}
	var cp *CopyProblem
	if !errors.As(err, &cp) {
		t.Fatalf("error is not a *CopyProblem: %v", err)
	}
	if _, statErr := os.Stat(dst); statErr != nil {
		t.Fatal("the copy should be kept so par2 can repair it")
	}
}

// TestIngestFileKeepsBothDamagedCopies is par2's combine path: two pressings
// of the same disc, each damaged differently, may still hold every block
// between them — but only if ingest keeps both copies on disk. Deleting the
// staged one and re-copying used to leave exactly one damaged copy, whichever
// disc came last.
func TestIngestFileKeepsBothDamagedCopies(t *testing.T) {
	e := newEnv(t)
	staged := []byte("the encrypted image, damaged one way\n")
	fromDisc := []byte("the encrypted image, damaged another\n")
	recorded := strings.Repeat("ab", 64) // matches neither pressing

	src := filepath.Join(e.dir, "disc01.squashfs.age")
	if err := os.WriteFile(src, fromDisc, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.cfg.Dirs().Enc, "disc01.squashfs.age")
	if err := os.WriteFile(dst, staged, 0o600); err != nil {
		t.Fatal(err)
	}

	stagedNew, err := e.opts().ingestFile(context.Background(), src, dst, recorded)
	if err != nil {
		t.Fatalf("ingestFile: %v\n%s", err, e.log())
	}
	if !stagedNew {
		t.Error("keeping a second pressing's copy should count as staging something")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(staged) {
		t.Fatalf("the staged copy was replaced by an unproven one: %q", got)
	}
	alts := altCopies(e.cfg.Dirs().Enc, "disc01.squashfs.age")
	if len(alts) != 1 {
		t.Fatalf("expected one .copy* alternate, found %v\n%s", alts, e.log())
	}
	body, err := os.ReadFile(filepath.Join(e.cfg.Dirs().Enc, alts[0]))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(fromDisc) {
		t.Fatalf("the alternate copy is not the disc's pressing: %q", body)
	}
	if !strings.Contains(e.log(), "keeping both") {
		t.Fatalf("the operator was not told both copies are kept:\n%s", e.log())
	}
}

// TestIngestFileTrustsAVerifiedCopyOverAStaleMapfile: a Ctrl-C mid-salvage
// leaves a map file behind; a later clean re-ingest proves the staged copy
// whole. The hash must speak first — the leftover map used to brand the good
// copy an incomplete salvage forever.
func TestIngestFileTrustsAVerifiedCopyOverAStaleMapfile(t *testing.T) {
	e := newEnv(t)
	body := []byte("the encrypted image, pretend\n")
	sum := sha512Hex(t, body)

	src := filepath.Join(e.dir, "disc01.squashfs.age")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.cfg.Dirs().Enc, "disc01.squashfs.age")
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := "# Mapfile. Created by GNU ddrescue version 1.27\n" +
		"0x00000000  ?\n" +
		"0x00000000  0x10000  ?\n"
	if err := os.WriteFile(dst+mapfileExt, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	staged, err := e.opts().ingestFile(context.Background(), src, dst, sum)
	if err != nil {
		t.Fatalf("a verified staged copy was reported as a problem: %v\n%s", err, e.log())
	}
	if staged {
		t.Error("nothing new was staged, yet the pass claims otherwise")
	}
	if !strings.Contains(e.log(), "matches the hash on this disc") {
		t.Fatalf("the staged copy was not recognised:\n%s", e.log())
	}
	if _, err := os.Stat(dst + mapfileExt); err == nil {
		t.Fatal("the stale map file survived a hash-verified copy")
	}
}

// discRoot opens mp the way ingestDisc does, so these tests read a disc through
// the same confinement the reader uses rather than by plain path.
func discRoot(t *testing.T, mp string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(mp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestDiscSumsKeysByBaseName(t *testing.T) {
	e := newEnv(t)
	mp := t.TempDir()
	lines := "" +
		strings.Repeat("a", 128) + "  ./data/disc01.squashfs.age\n" +
		strings.Repeat("b", 128) + "  ./README.md\n"
	if err := os.WriteFile(filepath.Join(mp, agecrypt.SumsName), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	sums := e.opts().discSums(discRoot(t, mp), mp)
	if got := sums["disc01.squashfs.age"]; got != strings.Repeat("a", 128) {
		t.Fatalf("disc01.squashfs.age = %q", got)
	}
	if got := sums["README.md"]; got != strings.Repeat("b", 128) {
		t.Fatalf("README.md = %q", got)
	}
}

// Two entries whose base names collide used to be resolved by whichever the
// map iterator reached last, so the same disc could be checked against digest
// A on one run and digest B on the next — a copy that verifies clean once and
// is reported damaged the next time, which reads like failing hardware. The
// ambiguous name is dropped instead, deterministically, and every other name
// on the disc is still checked. Run enough times to catch the old behaviour:
// Go randomises map iteration, so one run of the unfixed code would have
// picked the "right" answer most of the time.
func TestDiscSumsDropsANameRecordedTwiceWithDifferentHashes(t *testing.T) {
	for i := 0; i < 50; i++ {
		e := newEnv(t)
		mp := t.TempDir()
		lines := "" +
			strings.Repeat("a", 128) + "  ./data/disc01.squashfs.age\n" +
			strings.Repeat("b", 128) + "  disc01.squashfs.age\n" +
			strings.Repeat("c", 128) + "  ./README.md\n"
		if err := os.WriteFile(filepath.Join(mp, agecrypt.SumsName), []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
		sums := e.opts().discSums(discRoot(t, mp), mp)
		if got, ok := sums["disc01.squashfs.age"]; ok {
			t.Fatalf("run %d: the contradicted name answered %q; it must answer nothing", i, got)
		}
		if got := sums["README.md"]; got != strings.Repeat("c", 128) {
			t.Fatalf("run %d: an unaffected name lost its hash: %q", i, got)
		}
		if !strings.Contains(e.log(), "two different hashes for disc01.squashfs.age") {
			t.Fatalf("run %d: the operator was not told which name is ambiguous:\n%s", i, e.log())
		}
	}
}

// The same name twice with the SAME digest is not a contradiction — one entry
// written "./x" and one written "x" say the same thing — and must not cost the
// disc its check.
func TestDiscSumsKeepsANameRecordedTwiceWithOneHash(t *testing.T) {
	e := newEnv(t)
	mp := t.TempDir()
	lines := "" +
		strings.Repeat("a", 128) + "  ./data/disc01.squashfs.age\n" +
		strings.Repeat("A", 128) + "  disc01.squashfs.age\n"
	if err := os.WriteFile(filepath.Join(mp, agecrypt.SumsName), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.opts().discSums(discRoot(t, mp), mp)["disc01.squashfs.age"]; !strings.EqualFold(got, strings.Repeat("a", 128)) {
		t.Fatalf("disc01.squashfs.age = %q, want the hash both entries agree on", got)
	}
}

func TestDiscSumsWithoutASumsFile(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	if sums := e.opts().discSums(discRoot(t, dir), dir); sums != nil {
		t.Fatalf("expected no sums, got %v", sums)
	}
	if !strings.Contains(e.log(), "cannot be checked") {
		t.Fatalf("the operator was not warned:\n%s", e.log())
	}
}

func TestCopyManifest(t *testing.T) {
	e := newEnv(t)
	mp := t.TempDir()
	if err := os.MkdirAll(e.cfg.Staging, 0o700); err != nil {
		t.Fatal(err)
	}
	// Absent manifest: not an error.
	if err := e.opts().copyManifest(context.Background(), discRoot(t, mp), mp); err != nil {
		t.Fatalf("copyManifest with no manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mp, manifestName), []byte("manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.opts().copyManifest(context.Background(), discRoot(t, mp), mp); err != nil {
		t.Fatalf("copyManifest: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(e.cfg.Staging, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "manifest\n" {
		t.Fatalf("manifest = %q", got)
	}
}

// TestDiscEntryRefusesLinksAtADiscsOwnNames pins the rule that ingest reads a
// disc's own names only when the disc really carries them.
//
// A disc brb wrote holds data/ as a real directory and SHA512SUMS, MANIFEST.txt
// and identity.txt as real files, so a link at any of those names was put there
// by hand or by somebody else. Following one reads a file that is not on the
// disc: an os.Root alone would still follow a link whose target stays inside the
// mount, and would open a device node, which is why discEntry lstats.
func TestDiscEntryRefusesLinksAtADiscsOwnNames(t *testing.T) {
	t.Parallel()
	mp := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("not on the disc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "real.txt"), []byte("on the disc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Absolute, inside-the-mount, and relative-escaping: an os.Root refuses the
	// first and third on its own and follows the second, so the second is the
	// one that proves the Lstat is doing work.
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(mp, "abs.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(mp, "inside.txt")); err != nil {
		t.Fatal(err)
	}
	root := discRoot(t, mp)

	for _, name := range []string{"abs.txt", "inside.txt"} {
		if _, err := discEntry(root, name); err == nil {
			t.Errorf("discEntry(%q) accepted a symbolic link", name)
		} else if !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("discEntry(%q) refused for the wrong reason: %v", name, err)
		}
	}
	// The companion: a real file must still be accepted, or the check above
	// would pass by refusing everything.
	fi, err := discEntry(root, "real.txt")
	if err != nil {
		t.Fatalf("discEntry refused a real file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("real.txt reported mode %v, want a regular file", fi.Mode())
	}
	if _, err := discEntry(root, "absent.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing name gave %v, want fs.ErrNotExist so callers can skip it", err)
	}
}

// TestIngestPublicIdentityRefusesADeviceNode pins the gap the size gate had.
// maxPublicIdentityBytes is judged from fi.Size(), which is 0 for a character
// device, so identity.txt -> /dev/zero passed the gate and io.ReadAll then took
// the process out through the allocator — the exact death the gate exists to
// prevent.
func TestIngestPublicIdentityRefusesADeviceNode(t *testing.T) {
	e := newEnv(t)
	mp := t.TempDir()
	if err := os.Symlink("/dev/zero", filepath.Join(mp, doc.PublicIdentityName)); err != nil {
		t.Skipf("cannot symlink /dev/zero here: %v", err)
	}
	staged, err := e.opts().ingestPublicIdentity(discRoot(t, mp), mp, "")
	if err == nil {
		t.Fatal("ingestPublicIdentity accepted a device node as a public archive's key")
	}
	if staged {
		t.Error("a refused identity was reported as staged")
	}
	var cp *CopyProblem
	if !errors.As(err, &cp) && !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("refusal was neither a CopyProblem nor a link refusal: %v", err)
	}
}

// writeDisc lays out a disc directory from a map of names relative to the disc
// root, creating data/ even when nothing is put in it.
func writeDisc(t *testing.T, files map[string]string) string {
	t.Helper()
	mp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mp, dataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(mp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return mp
}

// TestIngestDoesNotStageNamesStagingKeepsForItself pins INC-1: ingest stages
// every data file under its own name in one flat enc/, and some names there
// already mean something. A disc carrying data/disc01.squashfs.age.mapfile
// used to have it staged as the salvage map of the staged image, and the next
// ingest resumed that "salvage": ddrescue wrote the disc's bytes over the
// staged image in place, and with no recorded hash nothing checked the result.
//
// The ddrescue here does what the real one does with a map that marks the
// whole file unread: it copies the input over the output. Reverting the
// filter stages the map on the first pass and overwrites the genuine image on
// the second, which is what this asserts against — along with the other names
// staging keeps for itself, each of which has to stay out of enc/ too.
func TestIngestDoesNotStageNamesStagingKeepsForItself(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build a ddrescue stand-in with")
	}
	e := newEnv(t)
	enc := e.cfg.Dirs().Enc
	genuine := "GENUINE-CIPHERTEXT-BYTES"
	if err := os.WriteFile(filepath.Join(enc, encName(1)), []byte(genuine), 0o600); err != nil {
		t.Fatal(err)
	}
	_, key := publicKey(t)
	img := encName(1)
	bookkeeping := []string{
		img + mapfileExt,
		img + ".copy1700000000",
		img + partExt,
		doc.PublicIdentityName,
		"sidecars-disc02.par2",
	}
	files := map[string]string{
		"data/" + img: "ATTACKER-CHOSEN-BYTES!!!",
	}
	for _, nm := range bookkeeping {
		files["data/"+nm] = "planted\n"
	}
	files["data/"+img+mapfileExt] = "0x00000000  0x00000018  -\n"
	files["data/"+doc.PublicIdentityName] = string(key)
	mp := writeDisc(t, files)

	bin := filepath.Join(t.TempDir(), "ddrescue")
	stub := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\ncat \"$1\" > \"$2\"\nprintf '0x0 0x18 +\\n' > \"$3\"\n"
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Ddrescue, Path: bin, Found: true}})

	for pass := 1; pass <= 2; pass++ {
		if _, err := (&ingester{o: e.opts(), mountPoint: mp}).ingestDisc(context.Background()); err != nil {
			t.Fatalf("pass %d: %v\n%s", pass, err, e.log())
		}
	}
	got, err := os.ReadFile(filepath.Join(enc, img))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != genuine {
		t.Fatalf("the staged image is now %q: a map file off the disc steered a salvage into it\n%s", got, e.log())
	}
	for _, nm := range bookkeeping {
		if _, err := os.Lstat(filepath.Join(enc, nm)); err == nil {
			t.Errorf("%s was staged off the disc under a name staging keeps for itself", nm)
		}
		if !strings.Contains(e.log(), nm+" on this disc has a name staging keeps") {
			t.Errorf("the operator was not told %s was skipped:\n%s", nm, e.log())
		}
	}
	// The disc's own image is a differing copy with no recorded hash, so it is
	// still kept beside the staged one for par2 — once, not once per pass.
	if alts := altCopies(enc, img); len(alts) != 1 {
		t.Errorf("alternates = %v, want the disc's differing copy kept exactly once", alts)
	}
}

// TestIngestIgnoresFilesNestedInData pins INC-2 from the Go side, where it was
// already so: the format's data/ is flat, and a file in a subdirectory of it
// is not one of the disc's files. brb.sh used to stage data/sub/x as x.
func TestIngestIgnoresFilesNestedInData(t *testing.T) {
	e := newEnv(t)
	mp := writeDisc(t, map[string]string{
		"data/sub/" + encName(1): "nested image\n",
		"data/" + indexName:      "the index\n",
	})
	ingestOne(t, e, mp)
	if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Enc, encName(1))); err == nil {
		t.Fatal("a file nested under data/ was staged as the disc's image")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Enc, indexName)); err != nil {
		t.Fatalf("the top-level index was not staged: %v", err)
	}
}

// TestIngestFallsBackToThePerFileSidecar pins INC-3. brb.sh checks a copy
// against the file's own data/<name>.sha512 when the disc's SHA512SUMS says
// nothing about it; this reader did not, so the two disagreed about the same
// disc. Each case below fails with the fallback removed.
func TestIngestFallsBackToThePerFileSidecar(t *testing.T) {
	img := encName(1)
	body := strings.Repeat("image bytes\n", 40)
	other := strings.Repeat("ab", 64) // the digest of nothing on this disc

	sidecar := func(t *testing.T, mp, name, digest string) {
		t.Helper()
		if err := agecrypt.WriteSumFile(filepath.Join(mp, dataDir, name+sumExt), digest, name); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an image that fails its sidecar is a bad copy", func(t *testing.T) {
		e := newEnv(t)
		mp := writeDisc(t, map[string]string{"data/" + img: body})
		sidecar(t, mp, img, other)
		ig := &ingester{o: e.opts(), mountPoint: mp}
		if _, err := ig.ingestDisc(context.Background()); err != nil {
			t.Fatalf("ingestDisc: %v\n%s", err, e.log())
		}
		if ig.incomplete != 1 {
			t.Fatalf("incomplete = %d, want the image counted bad against its sidecar\n%s", ig.incomplete, e.log())
		}
		if !strings.Contains(e.log(), "does not match the hash this disc records") {
			t.Errorf("the mismatch was not explained:\n%s", e.log())
		}
	})

	t.Run("an image that matches its sidecar is verified", func(t *testing.T) {
		e := newEnv(t)
		mp := writeDisc(t, map[string]string{"data/" + img: body})
		sidecar(t, mp, img, sha512Hex(t, []byte(body)))
		ingestOne(t, e, mp)
		if !strings.Contains(e.log(), img+" copied and verified") {
			t.Errorf("the copy was not checked against its sidecar:\n%s", e.log())
		}
	})

	t.Run("a rotted index the sidecar vouches against is damage, not another set", func(t *testing.T) {
		e := newEnv(t)
		real := "the set's real index\n"
		if err := os.WriteFile(filepath.Join(e.cfg.Dirs().Enc, indexName), []byte(real), 0o600); err != nil {
			t.Fatal(err)
		}
		mp := writeDisc(t, map[string]string{
			"data/" + img:          body,
			"data/" + indexName:    "the set's real indeX\n",
			"data/" + sidecarsPar2: "parity\n",
		})
		sidecar(t, mp, indexName, sha512Hex(t, []byte(real)))
		if _, err := (&ingester{o: e.opts(), mountPoint: mp}).ingestDisc(context.Background()); err != nil {
			t.Fatalf("ingestDisc = %v, want the rotted index read as damage on this disc\n%s", err, e.log())
		}
		if !strings.Contains(e.log(), "this disc's copy is damaged") {
			t.Errorf("the rotted index was not reported as damage:\n%s", e.log())
		}
		// Had it been refused, nothing would have been staged after the
		// refusal — the sidecar parity, which sorts after the index, included.
		if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Enc, stagedSidecarName(sidecarsPar2, 1))); err != nil {
			t.Errorf("the disc's sidecar parity was not staged: %v", err)
		}
	})

	t.Run("a damaged sidecar is no witness, and says so", func(t *testing.T) {
		e := newEnv(t)
		mp := writeDisc(t, map[string]string{
			"data/" + img:          body,
			"data/" + img + sumExt: "not a checksum line\n",
		})
		ig := &ingester{o: e.opts(), mountPoint: mp}
		if _, err := ig.ingestDisc(context.Background()); err != nil {
			t.Fatalf("ingestDisc: %v\n%s", err, e.log())
		}
		if ig.incomplete != 0 {
			t.Errorf("incomplete = %d; a sidecar that does not parse says nothing about the image", ig.incomplete)
		}
		if !strings.Contains(e.log(), img+sumExt+" on this disc is damaged") {
			t.Errorf("the damaged sidecar was not reported:\n%s", e.log())
		}
		if !strings.Contains(e.log(), img+" copied\n") {
			t.Errorf("the image was not copied unchecked:\n%s", e.log())
		}
	})

	t.Run("a linked sidecar is no witness, and says so", func(t *testing.T) {
		e := newEnv(t)
		mp := writeDisc(t, map[string]string{"data/" + img: body})
		target := filepath.Join(t.TempDir(), "elsewhere.sha512")
		if err := agecrypt.WriteSumFile(target, other, img); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(mp, dataDir, img+sumExt)); err != nil {
			t.Fatal(err)
		}
		ig := &ingester{o: e.opts(), mountPoint: mp}
		if _, err := ig.ingestDisc(context.Background()); err != nil {
			t.Fatalf("ingestDisc: %v\n%s", err, e.log())
		}
		if ig.incomplete != 0 {
			t.Errorf("incomplete = %d; a digest read through a link off the disc judged the image", ig.incomplete)
		}
		if !strings.Contains(e.log(), "symbolic link") {
			t.Errorf("the linked sidecar was not reported:\n%s", e.log())
		}
	})
}

// TestIngestRefusesAForeignIndexBeforeStagingAnything is the Go half of
// km-RV-008. The index used to be judged only when the copy loop reached it,
// and the loop is sorted, so a disc from another set had already staged its
// image, its sidecars and — for a public set — its key by the time it was
// refused. brb.sh refuses at the top; so does this reader now.
func TestIngestRefusesAForeignIndexBeforeStagingAnything(t *testing.T) {
	e := newEnv(t)
	enc := e.cfg.Dirs().Enc
	ingestOne(t, e, discWithIndex(t, 1, []byte("the set's real index\n")))

	mp := discWithIndex(t, 2, []byte("another set's index entirely\n"))
	_, key := publicKey(t)
	if err := os.WriteFile(filepath.Join(mp, doc.PublicIdentityName), key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
		t.Fatal(err)
	}
	before := stagedSet(t, enc)

	_, err := (&ingester{o: e.opts(), mountPoint: mp}).ingestDisc(context.Background())
	if err == nil || !strings.Contains(err.Error(), "differs from the one already staged") {
		t.Fatalf("ingestDisc = %v, want the foreign-index refusal\n%s", err, e.log())
	}
	if !strings.Contains(err.Error(), "Nothing from this disc was staged") {
		t.Errorf("the refusal does not say nothing was staged: %v", err)
	}
	if after := stagedSet(t, enc); after != before {
		t.Fatalf("a refused disc changed staging:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// What the key a refused disc left behind used to cost. Where the staged
// public set's own key had not come across — its first disc's copy fails the
// hash that disc records — a disc of another public set, refused for its
// index, had already staged its key, and the staged set's next genuine disc
// was then refused as "two different public sets". The refusal comes first
// now, so the next disc goes in and brings the set's key with it.
func TestIngestRefusedDiscLeavesNoKeyToShutOutTheStagedSet(t *testing.T) {
	e := newEnv(t)
	staged := e.opts().stagedPublicIdentity()
	_, keyP := publicKey(t)
	_, keyQ := publicKey(t)
	indexP := []byte("set P's index\n")
	disc := func(n int, index, key []byte) string {
		t.Helper()
		mp := discWithIndex(t, n, index)
		if err := os.WriteFile(filepath.Join(mp, doc.PublicIdentityName), key, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
			t.Fatal(err)
		}
		return mp
	}

	p1 := disc(1, indexP, keyP)
	damaged := append(append([]byte{}, keyP...), "# a line added after the sums were taken\n"...)
	if err := os.WriteFile(filepath.Join(p1, doc.PublicIdentityName), damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	ig := &ingester{o: e.opts(), mountPoint: p1}
	if _, err := ig.ingestDisc(context.Background()); err != nil {
		t.Fatalf("set P's first disc: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(staged); err == nil || ig.incomplete != 1 {
		t.Fatalf("fixture: set P's damaged key was staged, or not counted (incomplete = %d)\n%s", ig.incomplete, e.log())
	}

	_, err := (&ingester{o: e.opts(), mountPoint: disc(1, []byte("set Q's index\n"), keyQ)}).ingestDisc(context.Background())
	if err == nil || !strings.Contains(err.Error(), "differs from the one already staged") {
		t.Fatalf("set Q's disc = %v, want the foreign-index refusal\n%s", err, e.log())
	}
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("the refused disc left its key in staging")
	}

	ingestOne(t, e, disc(2, indexP, keyP))
	if got, err := os.ReadFile(staged); err != nil || !bytes.Equal(got, keyP) {
		t.Fatalf("set P's key did not come across from its second disc (%v)\n%s", err, e.log())
	}
}

// A read error on the disc's own index says nothing about which set the disc
// belongs to, so the early check must not refuse over it: this reader has
// always staged the readable images of a damaged disc, and must go on doing
// so. The failure is still reported, when the copy loop reaches the index.
// brb.sh parts company here — with nothing on the disc recording the index's
// hash it refuses the same disc as another set's — and README.md's table of
// reader differences says so.
func TestIngestStillStagesADiscWhoseIndexCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	e := newEnv(t)
	real := []byte("the set's real index\n")
	ingestOne(t, e, discWithIndex(t, 1, real))
	mp := discWithIndex(t, 2, real)
	idx := filepath.Join(mp, dataDir, indexName)
	if err := os.Chmod(idx, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(idx, 0o644) })

	_, err := (&ingester{o: e.opts(), mountPoint: mp}).ingestDisc(context.Background())
	if err == nil {
		t.Fatalf("an unreadable index went unreported\n%s", e.log())
	}
	if strings.Contains(err.Error(), "differs from the one already staged") {
		t.Fatalf("an unreadable index was refused as another set's: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.cfg.Dirs().Enc, encName(2))); serr != nil {
		t.Fatalf("the disc's readable image was not staged: %v\n%s", serr, e.log())
	}
}

// stagedSet renders every name in enc with its digest, for comparing staging
// before and after something that must not touch it.
func stagedSet(t *testing.T, enc string) string {
	t.Helper()
	ents, err := os.ReadDir(enc)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, en := range ents {
		body, err := os.ReadFile(filepath.Join(enc, en.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %s\n", sha512Hex(t, body)[:16], en.Name())
	}
	return b.String()
}

// TestAltCopyNameNeverReusesATakenName pins km-RV-020. An alternate copy was
// named <dst>.copy<unix seconds>, whatever already sat there, and copyStream
// renames its finished copy over the name: two pressings read in one second —
// a clock stepped back, a script looping ingest over small images — had the
// second destroy the first, the copy par2 might need. A copy that failed
// removed its own unverified output, which then meant the earlier pressing.
//
// The sentinels sit at every name from a second ago to a few seconds ahead, so
// the clock ticking over during the test changes nothing. Every path that
// writes an alternate is driven, and every sentinel must come through intact.
func TestAltCopyNameNeverReusesATakenName(t *testing.T) {
	staged := []byte("the staged copy of the image\n")
	fromDisc := []byte("this disc's copy of the image, damaged otherwise\n")

	tests := []struct {
		name string
		want string
		// dir makes the source unreadable: a directory opens and fails to read.
		dir bool
	}{
		{name: "no recorded hash", want: ""},
		{name: "a recorded hash neither copy matches", want: strings.Repeat("ab", 64)},
		{name: "a copy that fails outright", want: strings.Repeat("ab", 64), dir: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			enc := e.cfg.Dirs().Enc
			dst := filepath.Join(enc, encName(1))
			if err := os.WriteFile(dst, staged, 0o600); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(e.dir, encName(1))
			if tc.dir {
				if err := os.Mkdir(src, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(src, fromDisc, 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			sentinels := map[string]string{}
			for s := now - 1; s <= now+5; s++ {
				p := fmt.Sprintf("%s.copy%d", dst, s)
				body := fmt.Sprintf("an earlier pressing, kept at %d\n", s)
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				sentinels[p] = body
			}

			_, err := e.opts().ingestFile(context.Background(), src, dst, tc.want)
			if tc.dir && err == nil {
				t.Fatalf("a source that cannot be read was copied\n%s", e.log())
			}
			for p, body := range sentinels {
				got, rerr := os.ReadFile(p)
				if rerr != nil {
					t.Errorf("%s is gone: %v", filepath.Base(p), rerr)
				} else if string(got) != body {
					t.Errorf("%s was overwritten with %q", filepath.Base(p), got)
				}
			}
			if !tc.dir {
				found := false
				for _, c := range altCopies(enc, encName(1)) {
					if b, _ := os.ReadFile(filepath.Join(enc, c)); string(b) == string(fromDisc) {
						found = true
					}
				}
				if !found {
					t.Errorf("this disc's copy was not kept under a name of its own\n%s", e.log())
				}
			}
		})
	}
}

// The search for a free name advances only while the name is TAKEN. A name the
// filesystem will not hold at all used to be returned for the copy to fail on,
// and still is: a loop that advanced on every error spun forever here, on a
// file name a hand-mastered disc can choose.
func TestAltCopyNameStopsOnANameTheFilesystemRefuses(t *testing.T) {
	e := newEnv(t)
	enc := e.cfg.Dirs().Enc
	// 244 bytes: NAME_MAX is 255, so the name itself fits and so does its
	// .mapfile, which is looked for first; its .copy<epoch> name does not.
	long := strings.Repeat("x", 231) + ".squashfs.age"
	dst := filepath.Join(enc, long)
	if err := os.WriteFile(dst, []byte("staged\n"), 0o600); err != nil {
		t.Skipf("this filesystem will not hold a %d-byte name: %v", len(long), err)
	}
	src := filepath.Join(e.dir, "src")
	if err := os.WriteFile(src, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.opts().ingestFile(context.Background(), src, dst, "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Skipf("this filesystem holds a %d-byte name, so there is no refusal here to stop on", len(long)+len(".copy0000000000"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ingestFile did not return: the free-name search is spinning on an error it can never get past")
	}
}

// TestIngestFileDoesNotKeepAnIdenticalBadCopy pins km-RV-083. A disc that
// returns the same wrong bytes every time used to leave one more
// byte-identical .copy per re-read, each a whole image of staging and one
// more par2 operand, with no new block in any of them.
func TestIngestFileDoesNotKeepAnIdenticalBadCopy(t *testing.T) {
	recorded := strings.Repeat("ab", 64) // matches no pressing here
	pressingA := []byte("the image, damaged one way\n")
	pressingB := []byte("the image, damaged another\n")

	setup := func(t *testing.T) (*env, string) {
		e := newEnv(t)
		dst := filepath.Join(e.cfg.Dirs().Enc, encName(1))
		return e, dst
	}
	read := func(t *testing.T, e *env, body []byte, want string) (bool, error) {
		t.Helper()
		src := filepath.Join(t.TempDir(), encName(1))
		if err := os.WriteFile(src, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return e.opts().ingestFile(context.Background(), src, filepath.Join(e.cfg.Dirs().Enc, encName(1)), want)
	}

	t.Run("the same pressing again, against the staged copy", func(t *testing.T) {
		e, _ := setup(t)
		if _, err := read(t, e, pressingA, recorded); !errors.Is(err, ErrIncompleteCopy) {
			t.Fatalf("first read = %v, want the wrong-data CopyProblem", err)
		}
		staged, err := read(t, e, pressingA, recorded)
		if !errors.Is(err, ErrIncompleteCopy) {
			t.Fatalf("second read = %v, want it reported as the still-unverified copy it is", err)
		}
		if staged {
			t.Error("a dropped duplicate was reported as staging something")
		}
		if alts := altCopies(e.cfg.Dirs().Enc, encName(1)); len(alts) != 0 {
			t.Fatalf("alternates = %v; a byte-identical copy of the staged image was kept", alts)
		}
		if !strings.Contains(err.Error(), "byte for byte the staged one") {
			t.Errorf("the reason does not say the copy was identical: %v", err)
		}
	})

	t.Run("a second pressing again, against its alternate", func(t *testing.T) {
		e, _ := setup(t)
		read(t, e, pressingA, recorded)
		if _, err := read(t, e, pressingB, recorded); err != nil {
			t.Fatalf("a differing pressing: %v\n%s", err, e.log())
		}
		if alts := altCopies(e.cfg.Dirs().Enc, encName(1)); len(alts) != 1 {
			t.Fatalf("alternates = %v, want the differing pressing kept", alts)
		}
		_, err := read(t, e, pressingB, recorded)
		if !errors.Is(err, ErrIncompleteCopy) || !strings.Contains(err.Error(), "staged from an earlier read") {
			t.Fatalf("re-reading the second pressing = %v, want it dropped as a copy already staged", err)
		}
		if alts := altCopies(e.cfg.Dirs().Enc, encName(1)); len(alts) != 1 {
			t.Fatalf("alternates = %v; the second pressing is now staged twice", alts)
		}
	})

	t.Run("no recorded hash: a second pressing again", func(t *testing.T) {
		e, dst := setup(t)
		if err := os.WriteFile(dst, pressingA, 0o600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := read(t, e, pressingB, ""); err != nil {
				t.Fatalf("read %d: %v\n%s", i+1, err, e.log())
			}
		}
		if alts := altCopies(e.cfg.Dirs().Enc, encName(1)); len(alts) != 1 {
			t.Fatalf("alternates = %v; the same pressing was kept once per read", alts)
		}
		if !strings.Contains(e.log(), "not kept a second time") {
			t.Errorf("the operator was not told the copy was a duplicate:\n%s", e.log())
		}
	})

	// Only complete copies are compared. An alternate with a map file beside
	// it is a salvage, whose unread regions are zeros the drive never
	// returned; a complete read that happens to hash the same is kept.
	t.Run("a salvage is not a pressing already staged", func(t *testing.T) {
		e, dst := setup(t)
		if err := os.WriteFile(dst, pressingA, 0o600); err != nil {
			t.Fatal(err)
		}
		salvage := dst + ".copy1700000000"
		if err := os.WriteFile(salvage, pressingB, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(salvage+mapfileExt, []byte("0x00000000  0x0000001b  -\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		staged, err := read(t, e, pressingB, recorded)
		if err != nil || !staged {
			t.Fatalf("a complete differing pressing = (%v, %v), want it kept\n%s", staged, err, e.log())
		}
		var copies []string
		for _, c := range altCopies(e.cfg.Dirs().Enc, encName(1)) {
			if !strings.HasSuffix(c, mapfileExt) {
				copies = append(copies, c)
			}
		}
		if len(copies) != 2 {
			t.Fatalf("copies = %v; a complete read was dropped as a copy of a salvage", copies)
		}
	})

	// A FIFO among the alternates would block the hash forever, and keep the
	// ingest waiting on a writer that never comes.
	t.Run("an alternate that is not a regular file is not read", func(t *testing.T) {
		e, dst := setup(t)
		if err := os.WriteFile(dst, pressingA, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(dst+".copy1700000000", 0o600); err != nil {
			t.Skipf("cannot make a FIFO here: %v", err)
		}
		src := filepath.Join(t.TempDir(), encName(1))
		if err := os.WriteFile(src, pressingB, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := e.opts().ingestFile(context.Background(), src, dst, recorded)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("a differing pressing beside a FIFO: %v\n%s", err, e.log())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ingestFile did not return: the FIFO among the alternates was opened to be hashed")
		}
	})
}

// TestIngestPublicIdentityBoundsTheReadItself pins the core of km-RV-016. The
// size gate judges what the open file's stat says, and a stat is a claim made
// before the read, not a limit on it: identity.txt grown after the stat — or
// served by a filesystem whose st_size understates it — went straight into an
// unbounded io.ReadAll. The read is bounded now, which is what the allocation
// check below measures: the file grows to 64 MiB (sparse, so it costs nothing
// to make), and reading it whole allocates at least that much whatever is
// said about it afterwards.
//
// Not parallel, and must not be: TotalAlloc counts the whole process.
func TestIngestPublicIdentityBoundsTheReadItself(t *testing.T) {
	e := newEnv(t)
	_, key := publicKey(t)
	mp := publicDisc(t, 1, key)
	p := filepath.Join(mp, doc.PublicIdentityName)
	const grown = 64 << 20
	testHookBeforeIdentityRead = func() {
		if err := os.Truncate(p, grown); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeIdentityRead = nil })

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	staged, err := e.opts().ingestPublicIdentity(discRoot(t, mp), mp, "")
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("reading an identity.txt grown to %d MiB allocated %d MiB; the read is not bounded", grown>>20, grew>>20)
	}
	var cp *CopyProblem
	if !errors.As(err, &cp) || !strings.Contains(err.Error(), "a public archive's key is a few hundred") {
		t.Fatalf("ingestPublicIdentity = %v, want the size refusal for a file that grew past the ceiling", err)
	}
	if staged {
		t.Error("a refused key was reported as staged")
	}
	if _, err := os.Stat(e.opts().stagedPublicIdentity()); err == nil {
		t.Fatal("a file past the ceiling was staged as a key")
	}
}

// The type is judged on the file actually open, not only on the name checked
// before it: a FIFO put at the name in between reports a size of 0 and used to
// pass the size gate. Reverting the check reads it instead, and the read is
// then refused for its size rather than its type — which is what this tells
// apart.
func TestIngestPublicIdentityJudgesTheFileItOpened(t *testing.T) {
	e := newEnv(t)
	_, key := publicKey(t)
	mp := publicDisc(t, 1, key)
	p := filepath.Join(mp, doc.PublicIdentityName)
	wrote := make(chan struct{})
	testHookBeforeIdentityOpen = func() {
		if err := os.Remove(p); err != nil {
			t.Error(err)
			return
		}
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Error(err)
			return
		}
		go func() {
			defer close(wrote)
			w, err := os.OpenFile(p, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			defer w.Close()
			w.Write(bytes.Repeat([]byte("x"), 128<<10))
		}()
	}
	t.Cleanup(func() { testHookBeforeIdentityOpen = nil })

	staged, err := e.opts().ingestPublicIdentity(discRoot(t, mp), mp, "")
	// Release a writer still waiting for a reader, whatever happened above.
	if r, oerr := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0); oerr == nil {
		r.Close()
	}
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Error("the FIFO's writer never finished")
	}
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ingestPublicIdentity = %v, want the opened FIFO refused as not a regular file", err)
	}
	if staged {
		t.Error("a refused key was reported as staged")
	}
}

// And the name is opened through the disc's root, as it was checked: a link
// swapped in between to a key off the disc is not read and not staged.
func TestIngestPublicIdentityOpensThroughTheDiscRoot(t *testing.T) {
	e := newEnv(t)
	_, key := publicKey(t)
	_, elsewhere := publicKey(t)
	mp := publicDisc(t, 1, key)
	outside := filepath.Join(t.TempDir(), "someone-elses-key.txt")
	if err := os.WriteFile(outside, elsewhere, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(mp, doc.PublicIdentityName)
	testHookBeforeIdentityOpen = func() {
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, p); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeIdentityOpen = nil })

	if _, err := e.opts().ingestPublicIdentity(discRoot(t, mp), mp, ""); err == nil {
		t.Fatal("a key read through a link off the disc was accepted")
	}
	if _, err := os.Stat(e.opts().stagedPublicIdentity()); err == nil {
		t.Fatal("a key from outside the disc was staged")
	}
}
