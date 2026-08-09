package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/agecrypt"
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

func TestDiscSumsKeysByBaseName(t *testing.T) {
	e := newEnv(t)
	mp := t.TempDir()
	lines := "" +
		strings.Repeat("a", 128) + "  ./data/disc01.squashfs.age\n" +
		strings.Repeat("b", 128) + "  ./README.md\n"
	if err := os.WriteFile(filepath.Join(mp, agecrypt.SumsName), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	sums := e.opts().discSums(mp)
	if got := sums["disc01.squashfs.age"]; got != strings.Repeat("a", 128) {
		t.Fatalf("disc01.squashfs.age = %q", got)
	}
	if got := sums["README.md"]; got != strings.Repeat("b", 128) {
		t.Fatalf("README.md = %q", got)
	}
}

func TestDiscSumsWithoutASumsFile(t *testing.T) {
	e := newEnv(t)
	if sums := e.opts().discSums(t.TempDir()); sums != nil {
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
	if err := e.opts().copyManifest(context.Background(), mp); err != nil {
		t.Fatalf("copyManifest with no manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mp, manifestName), []byte("manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.opts().copyManifest(context.Background(), mp); err != nil {
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
