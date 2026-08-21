package iso

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
)

func TestParseRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		spec string
		want Range
	}{
		{"all", Range{1, rangeMax}},
		{"ALL", Range{1, rangeMax}},
		{" all ", Range{1, rangeMax}},
		{"1", Range{1, 1}},
		{"7", Range{7, 7}},
		{"100", Range{100, 100}},
		{"7-20", Range{7, 20}},
		{"7-7", Range{7, 7}},
		{"7-", Range{7, rangeMax}},
	}
	for _, tc := range tests {
		got, err := ParseRange(tc.spec)
		if err != nil {
			t.Errorf("ParseRange(%q) = %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRange(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}

	// Everything that is not one of those forms has to be refused rather than
	// interpreted: "burn 0" or "burn -3" must not quietly become "burn all".
	for _, spec := range []string{"", "   ", "some", "0", "-1", "-", "1.5", "1-2-3", "1_3", "0-4", "7-3", "all-1"} {
		if got, err := ParseRange(spec); err == nil {
			t.Errorf("ParseRange(%q) = %+v, want an error", spec, got)
		}
	}
}

func TestRangeContains(t *testing.T) {
	t.Parallel()
	r := Range{From: 3, To: 5}
	for n, want := range map[int]bool{1: false, 2: false, 3: true, 4: true, 5: true, 6: false} {
		if got := r.Contains(n); got != want {
			t.Errorf("Range{3,5}.Contains(%d) = %v, want %v", n, got, want)
		}
	}
}

// TestDiscNumbersPrefersDirectoriesAndTolerandGaps pins the rule that makes
// ISO_MODE=ondemand work at all: the disc directories are the set, the ISO
// directory is only a fallback, and a set numbered 1, 2, 5 is three discs
// rather than an error.
func TestDiscNumbersPrefersDirectoriesAndToleratesGaps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := config.Dirs{Discs: filepath.Join(dir, "discs"), ISO: filepath.Join(dir, "iso")}

	// Nothing at all: no entries, and no error for the missing directories.
	got, err := DiscNumbers(d)
	if err != nil || len(got) != 0 {
		t.Fatalf("DiscNumbers of an empty staging area = %v, %v", got, err)
	}

	if err := os.MkdirAll(d.ISO, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"disc03.iso", "disc01.iso", "disc10.iso", "notadisc.iso", "disc.iso"} {
		if err := os.WriteFile(filepath.Join(d.ISO, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// With no disc directories, the ISOs answer — sorted numerically, so 10
	// comes after 3 rather than after 1.
	if got, err := DiscNumbers(d); err != nil || !reflect.DeepEqual(got, []int{1, 3, 10}) {
		t.Fatalf("DiscNumbers from the ISO fallback = %v, %v; want [1 3 10]", got, err)
	}

	if err := os.MkdirAll(d.Discs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"disc01", "disc02", "disc05", "scratch", "disc0x"} {
		if err := os.MkdirAll(filepath.Join(d.Discs, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file named like a disc directory is not one.
	if err := os.WriteFile(filepath.Join(d.Discs, "disc09"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := DiscNumbers(d); err != nil || !reflect.DeepEqual(got, []int{1, 2, 5}) {
		t.Fatalf("DiscNumbers = %v, %v; want [1 2 5] from the directories, gap and all", got, err)
	}
}

// TestTotalComesFromTheManifest is the reason a burn started days later still
// labels disc 3 of 20 correctly: counting what is in staging would renumber a
// half-burned set, since KEEP_ISOS=0 deletes each ISO as its disc is written.
func TestTotalComesFromTheManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if got := Total(dir, 7); got != 7 {
		t.Errorf("Total with no manifest = %d, want the fallback 7", got)
	}

	manifest := "brb backup manifest\n\narchive name    : photos\ndiscs           : 20\ndisc type       : bd25\n"
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST.txt"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Total(dir, 3); got != 20 {
		t.Errorf("Total = %d, want 20 from the manifest", got)
	}

	// A hand-edited or truncated manifest must never stop a burn.
	for _, body := range []string{"discs           :\n", "discs : zero\n", "discs: -4\n", "", "junk\n"} {
		if err := os.WriteFile(filepath.Join(dir, "MANIFEST.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Total(dir, 5); got != 5 {
			t.Errorf("Total of manifest %q = %d, want the fallback 5", body, got)
		}
	}
}

func TestRoomFor(t *testing.T) {
	t.Parallel()
	const tree = 100 << 20
	if RoomFor(tree, tree) {
		t.Error("RoomFor allowed an ISO with no headroom at all")
	}
	if RoomFor(tree+slack, tree) {
		t.Error("RoomFor allowed exactly the tree plus the slack; the headroom must be strict")
	}
	if !RoomFor(tree+slack+1, tree) {
		t.Error("RoomFor refused the tree plus the slack plus a byte")
	}
	if RoomFor(1<<62, 1<<62) {
		t.Error("RoomFor wrapped around on a size that would overflow")
	}
}

func TestNamesMatchTheStagingLayout(t *testing.T) {
	t.Parallel()
	if got := Name(7); got != "disc07.iso" {
		t.Errorf("Name(7) = %q, want disc07.iso", got)
	}
	if got := Name(100); got != "disc100.iso" {
		t.Errorf("Name(100) = %q, want disc100.iso", got)
	}
	if got := dirName(7); got != "disc07" {
		t.Errorf("dirName(7) = %q, want disc07", got)
	}
}

// TestBuildOneRefusesWithoutADiscDirectory is the message an operator gets for
// asking to image a disc that was never built.
func TestBuildOneRefusesWithoutADiscDirectory(t *testing.T) {
	t.Parallel()
	o := testOptions(t)
	err := o.BuildOne(t.Context(), 4, 4)
	if err == nil || !strings.Contains(err.Error(), "run 'brb backup' first") {
		t.Fatalf("BuildOne without a disc directory = %v, want a pointer at backup", err)
	}
}
