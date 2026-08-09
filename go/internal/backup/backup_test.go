package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/scan"
	"github.com/jzbz/brb/internal/ui"
)

// quiet is a printer that throws its output away.
func quiet() *ui.Printer { return ui.New(io.Discard, false) }

func TestRequiredSpace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		image      int64
		redundancy int
		want       int64
	}{
		// Two plaintext-sized copies (the image and its round-trip check) plus
		// one ciphertext with its parity.
		{name: "no parity", image: 1000, redundancy: 0, want: 3000},
		{name: "ten percent", image: 1000, redundancy: 10, want: 3100},
		{name: "hundred percent", image: 1000, redundancy: 100, want: 4000},
		{
			// A real bd25 budget at the shipped defaults.
			name: "bd25 default", image: 21999955782, redundancy: 10,
			want: 2*21999955782 + 21999955782*110/100,
		},
		{name: "zero budget", image: 0, redundancy: 10, want: 0},
		{name: "negative budget", image: -5, redundancy: 10, want: 0},
		{name: "negative redundancy is treated as zero", image: 1000, redundancy: -1, want: 3000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RequiredSpace(tc.image, tc.redundancy); got != tc.want {
				t.Errorf("RequiredSpace(%d, %d) = %d, want %d", tc.image, tc.redundancy, got, tc.want)
			}
		})
	}
}

func TestRequiredSpaceIsBelowThreeAndAHalfImages(t *testing.T) {
	t.Parallel()
	// A sanity bound: the requirement must stay proportional to the budget, so
	// a preflight failure always names a believable number.
	const image = 21999955782
	got := RequiredSpace(image, 10)
	if got < 3*image || got > 4*image {
		t.Errorf("RequiredSpace = %d, want between %d and %d", got, 3*image, 4*image)
	}
}

func TestFreeSpaceOfATempDir(t *testing.T) {
	t.Parallel()
	n, err := freeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("freeSpace: %v", err)
	}
	if n <= 0 {
		t.Errorf("freeSpace = %d, want a positive number of bytes", n)
	}
}

func TestFreeSpaceOfAMissingDirectory(t *testing.T) {
	t.Parallel()
	if _, err := freeSpace(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("freeSpace of a missing directory succeeded, want an error")
	}
}

func TestCheckSpace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	avail, err := freeSpace(dir)
	if err != nil {
		t.Fatalf("freeSpace: %v", err)
	}

	cfg := config.Default()
	cfg.Staging = dir
	cfg.Par2Redundancy = 10

	// A budget that comfortably fits.
	r := &runner{cfg: cfg, p: quiet(), budget: disc.Budget{Image: 1 << 10}}
	if err := r.checkSpace(); err != nil {
		t.Errorf("checkSpace with a tiny budget: %v", err)
	}

	// A budget that cannot possibly fit.
	big := avail // 2*big alone already exceeds what is available
	r = &runner{cfg: cfg, p: quiet(), budget: disc.Budget{Image: big}}
	err = r.checkSpace()
	if err == nil {
		t.Fatal("checkSpace with an impossible budget succeeded, want an error")
	}
	for _, want := range []string{"need", "have", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRawBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image int64
		ratio float64
		want  int64
	}{
		{name: "ratio one", image: 1000, ratio: 1.0, want: 1000},
		{name: "half", image: 1000, ratio: 0.5, want: 2000},
		{name: "truncates like awk printf %d", image: 1000, ratio: 0.3, want: 3333},
		{name: "above one shrinks the budget", image: 1000, ratio: 1.25, want: 800},
		{name: "zero ratio falls back to 1.0", image: 1000, ratio: 0, want: 1000},
		{name: "negative ratio falls back to 1.0", image: 1000, ratio: -2, want: 1000},
		{name: "NaN falls back to 1.0", image: 1000, ratio: math.NaN(), want: 1000},
		{name: "Inf falls back to 1.0", image: 1000, ratio: math.Inf(1), want: 1000},
		{name: "never returns zero", image: 1, ratio: 1000, want: 1},
		{
			name: "bd25 default budget", image: 21999955782, ratio: 1.0, want: 21999955782,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rawBudget(tc.image, tc.ratio); got != tc.want {
				t.Errorf("rawBudget(%d, %v) = %d, want %d", tc.image, tc.ratio, got, tc.want)
			}
		})
	}
}

func TestMeasuredRatio(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		image, raw int64
		want       float64
	}{
		{name: "incompressible", image: 1000, raw: 1000, want: 1.0},
		{name: "compressed to two thirds", image: 2000, raw: 3000, want: 0.667},
		{name: "expanded", image: 1100, raw: 1000, want: 1.1},
		{name: "rounds to three places", image: 1234, raw: 10000, want: 0.123},
		{name: "empty bin", image: 500, raw: 0, want: 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := measuredRatio(tc.image, tc.raw); got != tc.want {
				t.Errorf("measuredRatio(%d, %d) = %v, want %v", tc.image, tc.raw, got, tc.want)
			}
		})
	}
}

func TestShrinkRatio(t *testing.T) {
	t.Parallel()
	// brb.sh computes  ratio = printf "%.3f" (image/raw)  and then
	// PACK_RATIO = printf "%.3f" (ratio * 1.05). Both roundings are reproduced,
	// so the two implementations re-pack an over-budget disc identically.
	tests := []struct {
		name       string
		image, raw int64
		want       float64
	}{
		{name: "no compression", image: 1000, raw: 1000, want: 1.05},
		{name: "measured 0.900", image: 900, raw: 1000, want: 0.945},
		{name: "measured 0.667", image: 2000, raw: 3000, want: 0.7},
		{name: "measured 0.620", image: 620, raw: 1000, want: 0.651},
		{name: "image bigger than raw", image: 1500, raw: 1000, want: 1.575},
		{name: "empty bin falls back to 1.0", image: 700, raw: 0, want: 1.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shrinkRatio(tc.image, tc.raw); got != tc.want {
				t.Errorf("shrinkRatio(%d, %d) = %v, want %v", tc.image, tc.raw, got, tc.want)
			}
		})
	}
}

func TestShrinkRetryShrinksTheBudget(t *testing.T) {
	t.Parallel()
	// The property the retry loop depends on: an image that overshot its budget
	// yields a raw budget strictly smaller than the raw content that produced
	// it, so the re-planned bin is genuinely smaller.
	const imageBudget = 1_000_000
	cases := []struct{ imageSize, rawBytes int64 }{
		{imageSize: 1_100_000, rawBytes: 1_000_000},
		{imageSize: 1_050_000, rawBytes: 2_000_000},
		{imageSize: 4_000_000, rawBytes: 4_000_000},
		{imageSize: 1_000_001, rawBytes: 1_000_000},
	}
	for _, c := range cases {
		ratio := shrinkRatio(c.imageSize, c.rawBytes)
		got := rawBudget(imageBudget, ratio)
		if got >= c.rawBytes {
			t.Errorf("image %d from %d raw: new raw budget %d is not below %d (ratio %v)",
				c.imageSize, c.rawBytes, got, c.rawBytes, ratio)
		}
	}
}

func TestNamingMatchesBrbSh(t *testing.T) {
	t.Parallel()
	if got := imageName(3); got != "disc03.squashfs" {
		t.Errorf("imageName(3) = %q, want disc03.squashfs", got)
	}
	if got := imageName(12); got != "disc12.squashfs" {
		t.Errorf("imageName(12) = %q, want disc12.squashfs", got)
	}
	if got := imageName(100); got != "disc100.squashfs" {
		t.Errorf("imageName(100) = %q, want disc100.squashfs", got)
	}
	if got := discDirName(7); got != "disc07" {
		t.Errorf("discDirName(7) = %q, want disc07", got)
	}
	if got := iso.Name(7); got != "disc07.iso" {
		t.Errorf("iso.Name(7) = %q, want disc07.iso", got)
	}
	if indexName != "index.tsv.gz.age" {
		t.Errorf("indexName = %q, want index.tsv.gz.age", indexName)
	}
}

// writeTree creates a directory holding files of the given sizes, named a, b, …
func writeTree(t *testing.T, sizes ...int64) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, sz := range sizes {
		name := filepath.Join(dir, "sub", string(rune('a'+i))+".bin")
		if err := os.WriteFile(name, make([]byte, sz), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// planCfg is a configuration whose disc budget is small enough to test with.
func planCfg(t *testing.T, src string, capacity int64) *config.Config {
	t.Helper()
	c := config.Default()
	c.SourceDir = src
	c.Staging = t.TempDir()
	c.ArchiveName = "test-archive"
	c.DiscCapacityBytes = capacity
	c.ReserveBytes = 0
	c.Par2Redundancy = 10
	c.PackRatio = 1.0
	c.PruneDirs = nil
	c.ExcludeMasks = nil
	return c
}

func TestPlan(t *testing.T) {
	t.Parallel()
	// capacity 1 MiB -> usable 1027604 -> image budget 925769, so three
	// 300000-byte files fit on a disc and the fourth does not.
	src := writeTree(t, 300000, 300000, 300000, 300000, 300000)
	cfg := planCfg(t, src, 1<<20)

	got, err := Plan(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Discs != 2 {
		t.Fatalf("Discs = %d, want 2 (%+v)", got.Discs, got.PerDisc)
	}
	if got.RawBytes != 1500000 {
		t.Errorf("RawBytes = %d, want 1500000", got.RawBytes)
	}
	if got.PackRatio != 1.0 {
		t.Errorf("PackRatio = %v, want 1", got.PackRatio)
	}
	if len(got.PerDisc) != 2 {
		t.Fatalf("PerDisc has %d entries, want 2", len(got.PerDisc))
	}
	if got.PerDisc[0].Index != 1 || got.PerDisc[0].Files != 3 || got.PerDisc[0].RawBytes != 900000 {
		t.Errorf("disc 1 = %+v, want {Index:1 Files:3 RawBytes:900000}", got.PerDisc[0])
	}
	if got.PerDisc[1].Index != 2 || got.PerDisc[1].Files != 2 || got.PerDisc[1].RawBytes != 600000 {
		t.Errorf("disc 2 = %+v, want {Index:2 Files:2 RawBytes:600000}", got.PerDisc[1])
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 300000, 200000, 300000, 100000, 250000)
	cfg := planCfg(t, src, 1<<20)

	first, err := Plan(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := Plan(context.Background(), Options{Cfg: cfg})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if again.Discs != first.Discs || len(again.PerDisc) != len(first.PerDisc) {
			t.Fatalf("run %d differs: %+v vs %+v", i, again, first)
		}
		for j := range first.PerDisc {
			if again.PerDisc[j] != first.PerDisc[j] {
				t.Errorf("run %d disc %d = %+v, want %+v", i, j, again.PerDisc[j], first.PerDisc[j])
			}
		}
	}
}

func TestPlanRejectsAFileLargerThanADisc(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 2_000_000)
	cfg := planCfg(t, src, 1<<20)

	_, err := Plan(context.Background(), Options{Cfg: cfg})
	if err == nil {
		t.Fatal("Plan succeeded with an oversized file, want an error")
	}
	for _, want := range []string{"larger than one disc", "a.bin", "EXCLUDE_MASKS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPlanHonoursThePackRatio(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 300000, 300000, 300000, 300000, 300000)
	cfg := planCfg(t, src, 1<<20)
	cfg.PackRatio = 0.5 // expect 2:1 compression, so twice as much fits

	got, err := Plan(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Discs != 1 {
		t.Errorf("Discs = %d at ratio 0.5, want 1 (%+v)", got.Discs, got.PerDisc)
	}
}

func TestPlanRejectsAnEmptyTree(t *testing.T) {
	t.Parallel()
	cfg := planCfg(t, writeTree(t), 1<<20)
	_, err := Plan(context.Background(), Options{Cfg: cfg})
	if err == nil {
		t.Fatal("Plan of a tree with no files succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "nothing to pack") {
		t.Errorf("error %q does not explain that there was nothing to pack", err)
	}
}

func TestPlanNeedsAConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := Plan(context.Background(), Options{}); err == nil {
		t.Fatal("Plan without a configuration succeeded, want an error")
	}
}

func TestPlanAbortsOnCancellation(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 1000, 1000)
	cfg := planCfg(t, src, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Plan(ctx, Options{Cfg: cfg})
	if err == nil {
		t.Fatal("Plan succeeded with a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}

func TestResumeFilterDropsAssignedFiles(t *testing.T) {
	t.Parallel()
	res := &scan.Result{
		RawBytes: 300,
		Entries: []scan.Entry{
			{Rel: "sub", Kind: scan.KindDir},
			{Rel: "sub/a", Kind: scan.KindFile, Size: 100},
			{Rel: "sub/b", Kind: scan.KindFile, Size: 100},
			{Rel: "sub/c", Kind: scan.KindFile, Size: 100},
		},
	}
	r := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 300,
		Assigned: []string{"sub/a", "sub/c"},
	}}

	got := r.resumeFilter(res)
	want := []string{"sub", "sub/b"}
	if len(got) != len(want) {
		t.Fatalf("kept %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Rel != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i].Rel, want[i])
		}
	}
}

func TestResumeFilterKeepsEverythingOnAFreshRun(t *testing.T) {
	t.Parallel()
	res := &scan.Result{
		RawBytes: 100,
		Entries:  []scan.Entry{{Rel: "a", Kind: scan.KindFile, Size: 100}},
	}
	r := &runner{p: quiet(), st: newState("arch", "/src", 1.0, time.Now())}
	got := r.resumeFilter(res)
	if len(got) != 1 {
		t.Fatalf("kept %d entries, want 1", len(got))
	}
	if r.st.ScanRawSize != 100 {
		t.Errorf("ScanRawSize = %d, want 100", r.st.ScanRawSize)
	}
}

func TestDirBytes(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, 10, 20, 30)
	got, err := dirBytes(context.Background(), dir)
	if err != nil {
		t.Fatalf("dirBytes: %v", err)
	}
	if got != 60 {
		t.Errorf("dirBytes = %d, want 60", got)
	}
}

func TestLinkOrCopyFallsBackToACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "sub", "dst")

	// A hard link works within one directory tree; the copy path is exercised
	// by copyFile directly.
	if err := linkOrCopy(context.Background(), src, dst, 0o644); err != nil {
		t.Fatalf("linkOrCopy: %v", err)
	}
	if err := linkOrCopy(context.Background(), src, dst, 0o644); err != nil {
		t.Fatalf("linkOrCopy over an existing file: %v", err)
	}
	copied := filepath.Join(dir, "copied")
	if err := copyFile(context.Background(), src, copied, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	for _, p := range []string{dst, copied} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "payload" {
			t.Errorf("%s = %q, want \"payload\"", p, data)
		}
	}
	fi, err := os.Stat(copied)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("copied mode = %v, want 0755", fi.Mode().Perm())
	}
}

func TestFilesMatching(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"disc01.squashfs.age", "disc01.squashfs.age.par2",
		"disc01.squashfs.age.vol000+100.par2", "disc01.squashfs.sha512"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := filesMatching(dir, func(n string) bool {
		return strings.HasPrefix(n, "disc01.squashfs.age") && strings.HasSuffix(n, ".par2")
	})
	if err != nil {
		t.Fatalf("filesMatching: %v", err)
	}
	want := []string{"disc01.squashfs.age.par2", "disc01.squashfs.age.vol000+100.par2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	missing, err := filesMatching(filepath.Join(dir, "nope"), func(string) bool { return true })
	if err != nil {
		t.Errorf("filesMatching of a missing directory: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("filesMatching of a missing directory returned %v", missing)
	}
}

// TestSidecarNames pins down what sidecars.par2 covers, which is the part of
// this feature a reader is most likely to get wrong: every .sha512 sidecar and
// the encrypted index itself, in brb.sh's order, and nothing else — not the
// image, not the image's own parity, and not a previous run's sidecars.par2.
func TestSidecarNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"disc01.squashfs.age",
		"disc01.squashfs.age.par2",
		"disc01.squashfs.age.vol000+20.par2",
		"disc01.squashfs.age.sha512",
		"disc01.squashfs.sha512",
		indexName,
		indexName + ".sha512",
		"sidecars.par2",
		"sidecars.vol000+04.par2",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := sidecarNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"disc01.squashfs.age.sha512",
		"disc01.squashfs.sha512",
		indexName + ".sha512",
		indexName,
	}
	if len(got) != len(want) {
		t.Fatalf("sidecarNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sidecarNames = %v, want %v", got, want)
		}
	}
}

// TestSidecarNamesWithoutAnIndex covers the disc directory of a run that has
// not written the encrypted index yet: the hashes are still protected, and the
// absent index is not named to par2 as a file that does not exist.
func TestSidecarNamesWithoutAnIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disc01.squashfs.sha512"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sidecarNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "disc01.squashfs.sha512" {
		t.Fatalf("sidecarNames = %v, want just the one sidecar", got)
	}

	// And an empty directory yields nothing rather than an error, so the caller
	// can warn instead of running par2 with no files at all.
	empty, err := sidecarNames(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("sidecarNames of an empty directory = %v", empty)
	}
}

// TestPlanWarnsAboutAConfigurationBackupWouldRefuse: cfg.Validate ran only in
// preflight, so a plan was happily produced under settings backup refuses and
// the operator learned about them at the start of the overnight run instead.
// Plan is a diagnostic, so it warns and keeps planning, as it already does for
// the reserve check.
func TestPlanWarnsAboutAConfigurationBackupWouldRefuse(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 300000, 300000)
	cfg := planCfg(t, src, 1<<20)
	cfg.Compression = "notreal"
	cfg.MaxShrinkAttempts = -5

	var buf bytes.Buffer
	p := ui.New(&buf, false)
	p.SetAssumeYes(true)
	got, err := Plan(context.Background(), Options{Cfg: cfg, UI: p})
	if err != nil {
		t.Fatalf("Plan refused a configuration it should only have warned about: %v", err)
	}
	if got.Discs == 0 {
		t.Fatal("Plan produced no layout")
	}
	out := buf.String()
	for _, want := range []string{"COMPRESSION", "MAX_SHRINK_ATTEMPTS", "backup will refuse this configuration"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not mention %q:\n%s", want, out)
		}
	}

	// And a configuration Validate accepts is not accused of anything by it.
	// (The reserve check next to it may still speak; it is a separate warning
	// with its own test, and RESERVE_BYTES=0 here trips it deliberately.)
	clean := planCfg(t, src, 1<<20)
	var quiet bytes.Buffer
	q := ui.New(&quiet, false)
	q.SetAssumeYes(true)
	if _, err := Plan(context.Background(), Options{Cfg: clean, UI: q}); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, unwanted := range []string{"COMPRESSION", "MAX_SHRINK_ATTEMPTS"} {
		if strings.Contains(quiet.String(), unwanted) {
			t.Errorf("a valid configuration was accused of %s:\n%s", unwanted, quiet.String())
		}
	}
}
