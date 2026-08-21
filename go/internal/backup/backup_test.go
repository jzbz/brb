package backup

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/pack"
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
		// One plaintext image and, beside it, one ciphertext with its parity —
		// the two that exist at the same moment. The round-trip check adds
		// nothing: it hashes the decrypted stream and stores none of it.
		{name: "no parity", image: 1000, redundancy: 0, want: 2000},
		{name: "ten percent", image: 1000, redundancy: 10, want: 2100},
		{name: "hundred percent", image: 1000, redundancy: 100, want: 3000},
		{
			// A real bd25 budget at the shipped defaults.
			name: "bd25 default", image: 21999955782, redundancy: 10,
			want: 21999955782 + 21999955782*110/100,
		},
		{name: "zero budget", image: 0, redundancy: 10, want: 0},
		{name: "negative budget", image: -5, redundancy: 10, want: 0},
		{name: "negative redundancy is treated as zero", image: 1000, redundancy: -1, want: 2000},
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

func TestRequiredSpaceIsBelowTwoAndAHalfImages(t *testing.T) {
	t.Parallel()
	// A sanity bound: the requirement must stay proportional to the budget, so
	// a preflight failure always names a believable number. It must also stay
	// ABOVE two images, because the plaintext and the ciphertext really do
	// coexist and a floor that forgot one of them would let a run start that
	// cannot finish its first disc.
	const image = 21999955782
	got := RequiredSpace(image, 10)
	if got < 2*image || got > 5*image/2 {
		t.Errorf("RequiredSpace = %d, want between %d and %d", got, 2*image, 5*image/2)
	}
}

// TestRoundTripNeedsNoStagingSpace. The round-trip check wants one thing from
// the decrypted image: its SHA-512. It used to get that by decrypting to a
// temporary file in <STAGING>/work — an image-sized sequential write plus an
// fsync, up to 95 GB on BD-XL media — which nothing ever opened and which was
// unlinked a moment later.
//
// The fixture makes work/ read-only, so a run that writes there at all cannot
// finish. That is the only way to observe the difference after the fact: the
// old code deleted its temporary file too, so a check for leftovers passes
// either way.
func TestRoundTripNeedsNoStagingSpace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root writes into a read-only directory, so the fixture proves nothing")
	}
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := runnerFor(t, nil)
	r.opts.VerifyRoundTrip = true
	r.identities = []age.Identity{id}
	for _, d := range []string{r.dirs.Work, r.dirs.Img, r.dirs.Enc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Content that does not compress, so the ciphertext is a real stream and
	// not a few bytes of age header.
	plain := filepath.Join(r.dirs.Img, imageName(1))
	body := bytes.Repeat([]byte("brb round-trip fixture\n"), 40_000)
	if err := os.WriteFile(plain, body, 0o600); err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(r.dirs.Enc, imageName(1)+".age")
	sums, err := agecrypt.Encrypt(ctx, plain, enc, []age.Recipient{id.Recipient()}, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if err := os.Chmod(r.dirs.Work, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.dirs.Work, 0o700) })

	if err := r.roundTrip(ctx, 1, enc, int64(len(body)), sums.Plain); err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	if left, err := os.ReadDir(r.dirs.Work); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("%s holds %d file(s) after the round-trip", r.dirs.Work, len(left))
	}

	// The point of the check survives the change: a digest that does not match
	// still stops the run before the plaintext is deleted.
	wrong := strings.Repeat("0", len(sums.Plain))
	err = r.roundTrip(ctx, 1, enc, int64(len(body)), wrong)
	if err == nil || !strings.Contains(err.Error(), "round-trip mismatch") {
		t.Fatalf("roundTrip against the wrong digest = %v, want the mismatch refusal", err)
	}
}

// TestWriteSumsTakesTheKnownDigestShortcut pins the single wiring point of the
// project's largest I/O saving: SHA512SUMS is written from the digests the run
// already measured while encrypting, instead of re-reading twenty to ninety
// gigabytes per disc to arrive at the same numbers.
//
// The shortcut is keyed by the exact "./data/<name>" strings agecrypt records,
// built by hand on this side (see knownSums) and by a directory walk on the
// other. Nothing fails when they stop matching: agecrypt hashes the file the
// long way and writes a correct SHA512SUMS, so the only symptom is a run that
// is quietly hours slower. That is why this test plants a digest that is
// deliberately WRONG for the bytes on disc — it is the one observation that
// distinguishes "the shortcut was taken" from "the file was hashed".
func TestWriteSumsTakesTheKnownDigestShortcut(t *testing.T) {
	ctx := context.Background()
	r := runnerFor(t, nil)
	data := filepath.Join(r.dirs.Discs, discDirName(1), "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.dirs.Enc, 0o700); err != nil {
		t.Fatal(err)
	}

	// The disc directory's copies are hard links to enc/, which is what lets
	// agecrypt's inode check accept a digest taken from the other name.
	planted := map[string]string{}
	for _, name := range []string{imageName(1) + ".age", indexName} {
		src := filepath.Join(r.dirs.Enc, name)
		if err := os.WriteFile(src, []byte("ciphertext of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(src, filepath.Join(data, name)); err != nil {
			t.Fatal(err)
		}
		sum := sha512.Sum512([]byte("a digest of something else entirely: " + name))
		planted["./data/"+name] = hex.EncodeToString(sum[:])
	}
	r.discCipher[1] = planted["./data/"+imageName(1)+".age"]
	r.indexCipher = planted["./data/"+indexName]

	if err := r.writeSums(ctx, 1); err != nil {
		t.Fatalf("writeSums: %v", err)
	}
	sums, err := os.ReadFile(filepath.Join(r.dirs.Discs, discDirName(1), agecrypt.SumsName))
	if err != nil {
		t.Fatal(err)
	}
	for name, digest := range planted {
		if !strings.Contains(string(sums), digest+"  "+name) {
			t.Errorf("SHA512SUMS does not carry the recorded digest for %s — the shortcut's key "+
				"no longer matches the name agecrypt records, so every disc re-reads its image:\n%s",
				name, sums)
		}
	}

	// The other half of the contract, and the reason the keys can be trusted:
	// --verify-roundtrip is the operator asking for every byte to be proved
	// twice, so nothing is shortcut and SHA512SUMS holds the real digests.
	r.opts.VerifyRoundTrip = true
	if got := r.knownSums(1); got != nil {
		t.Errorf("knownSums under --verify-roundtrip = %v, want nothing shortcut", got)
	}
	if err := r.writeSums(ctx, 1); err != nil {
		t.Fatalf("writeSums: %v", err)
	}
	sums, err = os.ReadFile(filepath.Join(r.dirs.Discs, discDirName(1), agecrypt.SumsName))
	if err != nil {
		t.Fatal(err)
	}
	for name, digest := range planted {
		if strings.Contains(string(sums), digest) {
			t.Errorf("SHA512SUMS carries the recorded digest for %s under --verify-roundtrip", name)
		}
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
	big := avail // one image plus its ciphertext already exceeds what is available
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
	// Two roundings, not one: the measured ratio is rounded to three decimals
	// and the margin is applied to THAT, so the ratio in the log is the ratio
	// the re-pack used. Collapsing them changes the budget in the last digit.
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

// TestEmptyBinErrorNamesTheFileAndNotPackRatio. When an image overshoots, the
// shrink loop re-plans the disc at the ratio the content actually achieved —
// a SMALLER raw budget than the one buildDiscs checked for oversized files. A
// file between the two budgets therefore gets past the up-front check and is
// discovered here, with nothing left that fits.
//
// The message this path used to print told the operator to "lower PACK_RATIO
// manually". That is inert: the budget here comes from the measured ratio, so
// a re-run at any PACK_RATIO packs the same bin, measures the same overshoot
// and stops in exactly the same place. The operator has to be told which file
// it is and what actually works.
func TestEmptyBinErrorNamesTheFileAndNotPackRatio(t *testing.T) {
	t.Parallel()
	p := pack.New([]scan.Entry{
		{Rel: "media/one-big-mkv", Kind: scan.KindFile, Size: 900},
		{Rel: "media/notes.txt", Kind: scan.KindFile, Size: 10},
	})
	// The budget after a 1.05 shrink of a 1000-byte image budget: the 900-byte
	// file fitted the planned budget and does not fit the measured one.
	const rb = 800
	if _, ok := p.Next(rb); ok {
		// A bin containing only the small file is fine; the fixture is about
		// the big one, so make sure it really is impossible.
		if over := p.Oversized(rb); len(over) == 0 {
			t.Fatal("fixture: nothing is oversized at this budget")
		}
	}

	err := emptyBinError(7, p, rb, 1.052)
	if err == nil {
		t.Fatal("emptyBinError returned nil")
	}
	msg := err.Error()
	for _, want := range []string{"disc 7", "media/one-big-mkv", "measured ratio 1.052",
		"EXCLUDE_MASKS", "DISC_TYPE=bdxl100"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "PACK_RATIO") {
		t.Errorf("the refusal still sends the operator to PACK_RATIO, which cannot change "+
			"a budget derived from the measured ratio:\n%s", msg)
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
// the encrypted index itself, in that order, and nothing else — not the
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

// TestCheckIndexCoversRefusesFilesTheIndexCannotList pins the resume refusal
// for a set whose encrypted index is already built: the plaintext index is
// gone, the encrypted one is kept as it is and copied onto every disc, so any
// file still to be written would land on a disc the index knows nothing about.
func TestCheckIndexCoversRefusesFilesTheIndexCannotList(t *testing.T) {
	t.Parallel()
	skeleton := []scan.Entry{{Rel: "sub", Kind: scan.KindDir}, {Rel: "sub/link", Kind: scan.KindSymlink}}
	files := []scan.Entry{{Rel: "sub/new1", Kind: scan.KindFile, Size: 5}, {Rel: "sub/new2", Kind: scan.KindFile, Size: 5}}

	r := &runner{p: quiet(), indexBuilt: true, st: &State{Version: StateVersion, DiscsDone: 3}}
	// Nothing but skeleton left: every file is on a disc, and the index covers
	// them all, so the resume may go on to lay the discs out.
	if err := r.checkIndexCovers(skeleton); err != nil {
		t.Fatalf("checkIndexCovers with no files pending: %v", err)
	}
	err := r.checkIndexCovers(append(skeleton, files...))
	if err == nil {
		t.Fatal("checkIndexCovers accepted 2 files the built index cannot list")
	}
	for _, w := range []string{"3 finished disc(s)", "2 file(s) are not on any disc", "cannot be extended", "start over"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	// A resume whose index is not built yet appends to the plaintext one, so
	// pending files are the normal case there.
	r.indexBuilt = false
	if err := r.checkIndexCovers(files); err != nil {
		t.Errorf("checkIndexCovers with the plaintext index still present: %v", err)
	}
}

// TestProbeRecipientsRefusesAMixedSet: age refuses to encrypt to a recipient
// set that mixes post-quantum and classic keys, and neither AppendRecipient
// nor ParseRecipientsFile objects to such a file — so without the probe the
// refusal arrived after disc 1's mksquashfs. loadRecipients must catch it in
// preflight, and must accept an ordinary set.
func TestProbeRecipientsRefusesAMixedSet(t *testing.T) {
	t.Parallel()
	classic, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pq, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := probeRecipients([]age.Recipient{classic.Recipient()}); err != nil {
		t.Fatalf("probeRecipients over one classic key: %v", err)
	}
	if err := probeRecipients([]age.Recipient{pq.Recipient()}); err != nil {
		t.Fatalf("probeRecipients over one post-quantum key: %v", err)
	}
	if err := probeRecipients([]age.Recipient{classic.Recipient(), pq.Recipient()}); err == nil {
		t.Fatal("probeRecipients accepted a set mixing post-quantum and classic keys; age's Encrypt will not")
	}

	// Through loadRecipients, from a file, the way preflight sees it.
	write := func(name string, keys ...string) *runner {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.AgeRecipientsFile = path
		return &runner{cfg: cfg, p: quiet()}
	}
	r := write("mixed.txt", classic.Recipient().String(), pq.Recipient().String())
	err = r.loadRecipients()
	if err == nil {
		t.Fatal("loadRecipients accepted a recipients file mixing post-quantum and classic keys")
	}
	for _, w := range []string{"post-quantum", "mixed.txt", "one kind only"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	if len(r.recipients) != 0 {
		t.Error("loadRecipients kept the recipients of a set it refused")
	}
	ok := write("plain.txt", classic.Recipient().String())
	if err := ok.loadRecipients(); err != nil {
		t.Fatalf("loadRecipients over an ordinary file: %v", err)
	}
	if len(ok.recipients) != 1 || len(ok.pubkeys) != 1 {
		t.Errorf("loadRecipients kept %d recipient(s) and %d key(s), want 1 and 1", len(ok.recipients), len(ok.pubkeys))
	}
}

// resumeRunner builds a runner around a saved state file, ready for
// prepareState with --resume: the state says one disc is done, and the
// plaintext index in work/ says the same, so the only thing that can refuse
// is the check under test.
func resumeRunner(t *testing.T, mutate func(*State)) *runner {
	t.Helper()
	cfg := config.Default()
	cfg.Staging = t.TempDir()
	cfg.SourceDir = t.TempDir()
	cfg.ArchiveName = "pinned"
	r := &runner{
		opts:      Options{Resume: true},
		cfg:       cfg,
		p:         quiet(),
		dirs:      cfg.Dirs(),
		statePath: filepath.Join(cfg.Staging, "state.json"),
		pubkeys:   []string{"age1recorded"},
	}
	for _, d := range []string{r.dirs.Work, r.dirs.Enc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st := newState(cfg.ArchiveName, cfg.SourceDir, 1.0, time.Now())
	st.DiscsDone, st.Assigned = 1, []string{"a"}
	st.setGeometry(r.geometry())
	st.setRecipients(r.pubkeys)
	mutate(st)
	if err := SaveState(r.statePath, st); err != nil {
		t.Fatal(err)
	}
	if err := appendIndex(filepath.Join(r.dirs.Work, indexFileName), 1, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestPrepareStateRefusesAResumeUnderChangedKeysOrGeometry is the wiring
// test for the two pins: prepareState must consult them, with --resume, once
// discs exist, and the state a fresh run starts from must carry them.
func TestPrepareStateRefusesAResumeUnderChangedKeysOrGeometry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	same := resumeRunner(t, func(*State) {})
	if err := same.prepareState(ctx); err != nil {
		t.Fatalf("prepareState with unchanged keys and geometry: %v", err)
	}

	keys := resumeRunner(t, func(s *State) { s.setRecipients([]string{"age1recorded", "age1addedlater"}) })
	err := keys.prepareState(ctx)
	if !errors.Is(err, ErrStateMismatch) || !strings.Contains(err.Error(), "age1addedlater") {
		t.Errorf("prepareState under a changed recipients file = %v, want ErrStateMismatch naming the keys", err)
	}

	geom := resumeRunner(t, func(s *State) { s.Par2Redundancy++ })
	err = geom.prepareState(ctx)
	if !errors.Is(err, ErrStateMismatch) || !strings.Contains(err.Error(), "PAR2_REDUNDANCY") {
		t.Errorf("prepareState under a changed PAR2_REDUNDANCY = %v, want ErrStateMismatch naming it", err)
	}

	// A fresh start records both, so the next resume has something to check.
	fresh := resumeRunner(t, func(*State) {})
	fresh.opts.Resume = false
	if err := os.Remove(fresh.statePath); err != nil {
		t.Fatal(err)
	}
	if err := fresh.prepareState(ctx); err != nil {
		t.Fatalf("prepareState for a fresh run: %v", err)
	}
	if got := fresh.st.Recipients; len(got) != 1 || got[0] != "age1recorded" {
		t.Errorf("a fresh state recorded recipients %v, want [age1recorded]", got)
	}
	if fresh.st.CapacityBytes != fresh.cfg.Capacity() || fresh.st.DiscType != "bd25" ||
		fresh.st.ReserveBytes != fresh.cfg.ReserveBytes || fresh.st.Par2Redundancy != fresh.cfg.Par2Redundancy {
		t.Errorf("a fresh state recorded geometry %+v, want the configuration's", fresh.st)
	}
}

// TestManifestPrunesCarriesTheSkippedMounts: the manifest's "excluded from
// this backup" list is the one record of a skipped mount point that outlives
// the terminal, so the mount points the scan reported must land there,
// marked as such, after the configured prunes.
func TestManifestPrunesCarriesTheSkippedMounts(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.PruneDirs = []string{".cache"}
	r := &runner{cfg: cfg}
	if got := r.manifestPrunes(); len(got) != 1 || got[0] != ".cache" {
		t.Fatalf("manifestPrunes without mounts = %v, want [.cache]", got)
	}
	r.skippedMounts = []string{"nas", "media/usb"}
	got := r.manifestPrunes()
	if len(got) != 3 || got[0] != ".cache" {
		t.Fatalf("manifestPrunes = %v, want the prune first and both mounts after it", got)
	}
	for i, m := range r.skippedMounts {
		if !strings.HasPrefix(got[i+1], m+" ") || !strings.Contains(got[i+1], "mount point") {
			t.Errorf("manifestPrunes[%d] = %q, want %q marked as a mount point", i+1, got[i+1], m)
		}
	}
	if len(cfg.PruneDirs) != 1 {
		t.Error("manifestPrunes appended to the configuration's own slice")
	}
}

// TestOversizedDiscAdviceIsTrue: the old message said "raise RESERVE_BYTES and
// re-run", which a --resume cannot act on — the images already built keep
// their size, and Usable ignores the reserve. The advice must say by how much,
// and that the set has to start over.
func TestOversizedDiscAdviceIsTrue(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.ReserveBytes = 1000
	r := &runner{cfg: cfg, budget: disc.Budget{Usable: 10000}}
	err := r.oversizedDisc(4, "files", 10250)
	for _, w := range []string{"disc 4", "RESERVE_BYTES=1000", "at least 1250", "start the set over", "--resume cannot"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	if strings.Contains(err.Error(), "and re-run") {
		t.Errorf("error %q still gives the advice a resume cannot act on", err)
	}
}

// TestFilesMatchingInADirectoryNamedLikeAGlob pins the sweep helper the
// stale-par2 removals in protect and protectSidecars rely on: it lists the
// directory and matches names, so a STAGING path holding '[' — here literally
// "a[1]" — still finds the set that filepath.Glob would have missed.
func TestFilesMatchingInADirectoryNamedLikeAGlob(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a[1]")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"disc01.squashfs.age", "disc01.squashfs.age.par2",
		"disc01.squashfs.age.vol000+30.par2", "sidecars.par2", "sidecars.vol00+10.par2"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := "disc01.squashfs"
	got, err := filesMatching(dir, func(nm string) bool {
		return strings.HasPrefix(nm, base+".age") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil || len(got) != 2 {
		t.Errorf("image par2 set in a glob-named directory = %v, %v; want 2 names", got, err)
	}
	got, err = filesMatching(dir, func(nm string) bool {
		return strings.HasPrefix(nm, "sidecars") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil || len(got) != 2 {
		t.Errorf("sidecar par2 set in a glob-named directory = %v, %v; want 2 names", got, err)
	}
	// And the thing this replaced really does fail there.
	if m, _ := filepath.Glob(filepath.Join(dir, "sidecars*.par2")); len(m) != 0 {
		t.Logf("filepath.Glob matched %v in a directory named like a glob; the helper is belt and braces here", m)
	}
}
