package backup

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/indexfmt"
	"github.com/jzbz/brb/internal/ui"
)

// awkwardNames are the filenames the cross-compatibility probes used to break
// the index: a tab, a newline, a backslash, all three at once, and the one that
// forged a row for a disc that was never burned.
var awkwardNames = []string{
	"plain.txt",
	"a\tb.txt",
	"c\nd.txt",
	`e\f.txt`,
	"evil\n9\tphantom.txt",
	"sub/mix\t\\and\nnl.txt",
}

// namedSourceConfig builds a source tree holding exactly these relative paths,
// each of the given size, and a configuration pointed at it.
func namedSourceConfig(t *testing.T, names []string, size int64) *config.Config {
	t.Helper()
	src := t.TempDir()
	rnd := rand.New(rand.NewSource(2))
	for _, rel := range names {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, size)
		if _, err := rnd.Read(buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := config.Default()
	c.SourceDir = src
	c.Staging = t.TempDir()
	c.ArchiveName = "index-escaping-archive"
	c.DiscCapacityBytes = 32 << 20
	c.ReserveBytes = 12 << 20
	c.Compression = "none"
	c.CompressionLevel = 0
	c.BlockSize = "128K"
	c.Par2Redundancy = 10
	c.Par2Blocks = 20
	c.Par2MemoryMB = 64
	c.LabelPrefix = "TEST"
	c.PruneDirs = nil
	c.ExcludeMasks = nil
	return c
}

// TestIndexEscapesAwkwardNamesEndToEnd is IDX-1, IDX-2 and IDX-3 against the
// real tools: back up a tree whose filenames contain tabs, newlines and
// backslashes, then read the encrypted index off the disc the way the on-disc
// README says to read it.
func TestIndexEscapesAwkwardNamesEndToEnd(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	cfg := namedSourceConfig(t, awkwardNames, 1<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)

	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	discs := discCount(t, cfg.Dirs().Discs)
	lines := readIndex(t, ctx, cfg)

	// One row per file, no more and no fewer. Go used to emit 7 lines for 5
	// files, and the extra ones named files that did not exist.
	if len(lines) != len(awkwardNames) {
		t.Fatalf("index has %d line(s) for %d file(s):\n%q", len(lines), len(awkwardNames), lines)
	}

	var got []string
	for i, line := range lines {
		// The README's recipe is awk -F'\t', so every row must be two fields.
		if n := strings.Count(line, "\t"); n != 1 {
			t.Errorf("line %d has %d tab-separated field(s), want 2: %q", i+1, n+1, line)
		}
		disc, path, err := indexfmt.ParseLine(line)
		if err != nil {
			t.Fatalf("line %d does not parse: %v", i+1, err)
		}
		// IDX-2: a filename must not be able to assert a disc that is not in
		// the set.
		if disc < 1 || disc > discs {
			t.Errorf("line %d names disc %d, but the set has %d disc(s): %q", i+1, disc, discs, line)
		}
		got = append(got, path)
	}

	// IDX-3: every real name comes back out, exactly.
	want := append([]string(nil), awkwardNames...)
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("index paths = %q, want %q", got, want)
	}
}

// TestResumeKeepsEveryEscapedIndexRow is IDX-4: Go's own resume used to call
// Go's own index damaged and permanently delete the rows it could not parse.
// With the escaping in place there is nothing it cannot parse, so the index
// must come through a resume unchanged and unremarked.
func TestResumeKeepsEveryEscapedIndexRow(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	cfg := namedSourceConfig(t, awkwardNames, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05 // ~17 MiB budget, 24 MiB of data: two discs

	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dirs := cfg.Dirs()
	if got := discCount(t, dirs.Discs); got != 2 {
		t.Fatalf("the reference set has %d disc(s), want 2", got)
	}
	before := readIndex(t, ctx, cfg)

	// Rewind to "disc 1 finished, then the machine went down".
	var firstLines, firstPaths []string
	for _, line := range before {
		disc, path, err := indexfmt.ParseLine(line)
		if err != nil {
			t.Fatalf("the index this run just wrote does not parse: %v", err)
		}
		if disc == 1 {
			firstLines = append(firstLines, line)
			firstPaths = append(firstPaths, path)
		}
	}
	if len(firstPaths) == 0 || len(firstPaths) == len(before) {
		t.Fatalf("disc 1 holds %d of %d file(s); the fixture is wrong", len(firstPaths), len(before))
	}
	for _, dir := range []string{dirs.Discs, dirs.ISO} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, nm := range []string{indexName, indexName + ".sha512"} {
		if err := os.Remove(filepath.Join(dirs.Enc, nm)); err != nil {
			t.Fatal(err)
		}
	}
	second, err := filesMatching(dirs.Enc, func(n string) bool { return strings.HasPrefix(n, "disc02.") })
	if err != nil {
		t.Fatal(err)
	}
	for _, nm := range second {
		if err := os.Remove(filepath.Join(dirs.Enc, nm)); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(dirs.Work, indexFileName)
	if err := os.WriteFile(work, []byte(strings.Join(firstLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Constructed, not read back: a completed run removes its state file, so the
	// state an interrupted run would have left has to be written directly.
	statePath := filepath.Join(cfg.Staging, "state.json")
	if err := SaveState(statePath, &State{
		Version:   StateVersion,
		Archive:   cfg.ArchiveName,
		Source:    cfg.SourceDir,
		DiscsDone: 1,
		Assigned:  firstPaths,
		PackRatio: cfg.PackRatio,
	}); err != nil {
		t.Fatal(err)
	}

	var log strings.Builder
	p := ui.New(&log, false)
	p.SetAssumeYes(true)
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: set, Resume: true}); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if out := log.String(); strings.Contains(out, "damaged") || strings.Contains(out, "cannot parse") {
		t.Errorf("resume called its own index damaged:\n%s", out)
	}
	after := readIndex(t, ctx, cfg)
	if len(after) != len(before) {
		t.Fatalf("the index shrank across a resume: %d line(s) -> %d line(s)", len(before), len(after))
	}
	b := append([]string(nil), before...)
	a := append([]string(nil), after...)
	sort.Strings(a)
	sort.Strings(b)
	if !slices.Equal(a, b) {
		t.Errorf("the index changed across a resume:\nbefore %q\nafter  %q", b, a)
	}
}
