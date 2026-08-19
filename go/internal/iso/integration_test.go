package iso

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// testOptions builds an Options over an empty staging directory. The tool set
// is whatever this machine has; the tests that need xorriso call needXorriso.
func testOptions(t *testing.T) Options {
	t.Helper()
	cfg := config.Default()
	cfg.Staging = filepath.Join(t.TempDir(), "staging")
	cfg.ArchiveName = "iso-test"
	cfg.LabelPrefix = "TEST"
	return Options{
		Cfg:     cfg,
		UI:      ui.New(&bytes.Buffer{}, false),
		Tools:   tools.Detect(t.Context()),
		Version: "test",
	}
}

// needXorriso skips a test on a machine without the one tool this package runs.
func needXorriso(t *testing.T, o Options) {
	t.Helper()
	if err := o.Tools.Require(tools.Xorriso); err != nil {
		t.Skipf("skipping: %v", err)
	}
}

// stageDiscs lays out disc directories numbered as given, each holding a file
// of payload bytes, and returns the Options over them. It is deliberately not a
// real backup: what is being tested is the imaging of a directory tree, and a
// fixture that takes a second to build is one the range cases can afford to
// repeat.
func stageDiscs(t *testing.T, o Options, nums ...int) {
	t.Helper()
	for _, n := range nums {
		data := filepath.Join(o.sourceDir(n), "data")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		body := bytes.Repeat([]byte{byte(n)}, 64<<10)
		if err := os.WriteFile(filepath.Join(data, "payload.bin"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(o.sourceDir(n), "README.md"),
			[]byte("disc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// isoNumbers lists the disc numbers actually present in the ISO directory.
func isoNumbers(t *testing.T, o Options) []int {
	t.Helper()
	nums, err := numbered(o.Cfg.Dirs().ISO, false, ".iso")
	if err != nil {
		t.Fatal(err)
	}
	return nums
}

// TestBuildRanges covers the whole of the selection syntax against real
// xorriso, over a set numbered with a gap in it — which is what a staging
// directory looks like once a disc has been re-made by hand.
func TestBuildRanges(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		spec string
		want []int
	}{
		{"all", []int{1, 2, 3, 5}},
		{"2", []int{2}},
		{"2-3", []int{2, 3}},
		{"3-", []int{3, 5}},
		{"4", nil},   // the gap itself: a well-formed selector matching nothing
		{"6-9", nil}, // past the end of the set
		{"5", []int{5}},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			o := testOptions(t)
			needXorriso(t, o)
			stageDiscs(t, o, 1, 2, 3, 5)

			err := Build(ctx, o, tc.spec)
			if len(tc.want) == 0 {
				if err == nil || !strings.Contains(err.Error(), "no discs matched") {
					t.Fatalf("Build(%q) = %v, want a complaint that nothing matched", tc.spec, err)
				}
				if got := isoNumbers(t, o); len(got) != 0 {
					t.Fatalf("Build(%q) built %v", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build(%q): %v", tc.spec, err)
			}
			got := isoNumbers(t, o)
			if len(got) != len(tc.want) {
				t.Fatalf("Build(%q) built %v, want %v", tc.spec, got, tc.want)
			}
			for i, n := range tc.want {
				if got[i] != n {
					t.Fatalf("Build(%q) built %v, want %v", tc.spec, got, tc.want)
				}
			}
			// Every image has to be a usable one, not merely a file that exists.
			for _, n := range got {
				fi, err := os.Stat(o.Path(n))
				if err != nil {
					t.Fatal(err)
				}
				src, err := fsx.DirBytes(ctx, o.sourceDir(n))
				if err != nil {
					t.Fatal(err)
				}
				if fi.Size() < src {
					t.Errorf("disc %d ISO is %d bytes, its source tree is %d", n, fi.Size(), src)
				}
			}
		})
	}
}

// TestBuildWithoutADiscSet refuses rather than producing an empty ISO
// directory an operator would then try to burn.
func TestBuildWithoutADiscSet(t *testing.T) {
	o := testOptions(t)
	needXorriso(t, o)
	err := Build(context.Background(), o, "all")
	if err == nil || !strings.Contains(err.Error(), "run 'brb backup' first") {
		t.Fatalf("Build over an empty staging area = %v, want a pointer at backup", err)
	}
}

// TestVolumeLabelCarriesTheManifestTotal is the trap this package exists to
// avoid: the label says "01 OF 20", and a burn run days after the backup has
// only MANIFEST.txt to learn the 20 from.
func TestVolumeLabelCarriesTheManifestTotal(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1, 2)

	if err := os.WriteFile(filepath.Join(o.Cfg.Staging, "MANIFEST.txt"),
		[]byte("archive name    : iso-test\ndiscs           : 20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, o, "1"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := volumeLabel(t, o.Path(1)), "TEST_01_OF_20"; got != want {
		t.Errorf("volume label = %q, want %q — the total did not come from MANIFEST.txt", got, want)
	}

	// Without a manifest it falls back to what staging holds, which is two.
	if err := os.Remove(filepath.Join(o.Cfg.Staging, "MANIFEST.txt")); err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, o, "2"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := volumeLabel(t, o.Path(2)), "TEST_02_OF_02"; got != want {
		t.Errorf("volume label = %q, want %q", got, want)
	}
}

// volumeLabel reads the ISO 9660 primary volume descriptor's volume identifier:
// 32 bytes at offset 0x8028. Reading it out of the file rather than asking
// xorriso keeps the assertion independent of the tool that wrote it.
func volumeLabel(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 32)
	if _, err := f.ReadAt(buf, 0x8028); err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(buf), " \x00")
}

// TestBuildOneRefusesAnISOLargerThanTheMedia proves the upper size assertion by
// declaring media far too small for the tree. Without it a disc that cannot
// possibly be burned is only discovered by the drive, one blank at a time.
func TestBuildOneRefusesAnISOLargerThanTheMedia(t *testing.T) {
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)
	o.Cfg.DiscCapacityBytes = 4096

	err := o.BuildOne(context.Background(), 1, 1)
	if err == nil || !strings.Contains(err.Error(), "larger than the") {
		t.Fatalf("BuildOne onto 4 KiB media = %v, want a size refusal", err)
	}
	// And the unusable file must not be left where burn would find it.
	if _, statErr := os.Stat(o.Path(1)); statErr == nil {
		t.Errorf("%s was left behind after the size check refused it", o.Path(1))
	}
}

// TestEnsureBuildsOnlyWhatIsMissing is the burn path's contract: an ISO that is
// there is reused untouched, one that is not is built from the disc directory,
// and a zero-length remnant of a dead run is replaced rather than burned.
func TestEnsureBuildsOnlyWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)

	path, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Second call: the file is there, so nothing is rebuilt. Removing the disc
	// directory is what proves it — a rebuild would now fail.
	if err := os.RemoveAll(o.sourceDir(1)); err != nil {
		t.Fatal(err)
	}
	again, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure over an existing ISO: %v", err)
	}
	second, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) || second.Size() != first.Size() {
		t.Error("Ensure rebuilt an ISO that was already there")
	}

	// An empty file is not an ISO. With the disc directory gone it cannot be
	// rebuilt either, and saying so beats handing 0 bytes to the burner.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Ensure(ctx, 1, 1); err == nil {
		t.Error("Ensure accepted a zero-length ISO")
	}
}

// TestEnsureRebuildsATruncatedISO is the power-loss case: xorriso died part-way
// through and left a file that is non-empty but shorter than the disc directory
// it was made from. Such a file passes every "is it there" test, and Ensure
// used to hand it straight to the burner. It must be rebuilt instead, to the
// same bound BuildOne holds a fresh image to.
func TestEnsureRebuildsATruncatedISO(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)

	path, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	whole, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	srcSize, err := fsx.DirBytes(ctx, o.sourceDir(1))
	if err != nil {
		t.Fatal(err)
	}
	// Cut it to well under the tree it was built from: the payload alone is
	// 64 KiB, so 4 KiB of ISO cannot be complete.
	if err := os.Truncate(path, 4096); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	o.UI = ui.New(&log, false)
	again, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure over a truncated ISO: %v", err)
	}
	rebuilt, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Size() < srcSize {
		t.Fatalf("Ensure returned a %d-byte ISO for a %d-byte disc directory: the truncated file was "+
			"handed back instead of rebuilt", rebuilt.Size(), srcSize)
	}
	if rebuilt.Size() != whole.Size() {
		t.Errorf("rebuilt ISO is %d bytes, the original was %d", rebuilt.Size(), whole.Size())
	}
	if !strings.Contains(log.String(), "truncated") {
		t.Errorf("the rebuild was not explained to the operator:\n%s", log.String())
	}
}
