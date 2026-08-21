package pack

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/jzbz/brb/internal/scan"
)

// file builds a regular-file entry with a unique inode.
func file(rel string, size int64) scan.Entry {
	return scan.Entry{Rel: rel, Kind: scan.KindFile, Size: size, Inode: hashInode(rel), Nlink: 1}
}

// link builds a regular-file entry that shares an inode with its group.
func link(rel string, size int64, inode uint64) scan.Entry {
	return scan.Entry{Rel: rel, Kind: scan.KindFile, Size: size, Inode: inode, Nlink: 2}
}

// linkOn is link, on a chosen filesystem. An inode number means nothing
// without the device beside it, so a test about collisions has to say both.
func linkOn(rel string, size int64, dev, inode uint64) scan.Entry {
	e := link(rel, size, inode)
	e.Dev = dev
	return e
}

func dir(rel string) scan.Entry {
	return scan.Entry{Rel: rel, Kind: scan.KindDir, Inode: hashInode(rel), Nlink: 2}
}

func symlink(rel string) scan.Entry {
	return scan.Entry{Rel: rel, Kind: scan.KindSymlink, Inode: hashInode(rel), Nlink: 1}
}

// hashInode derives a stable, collision-unlikely inode from a path so test
// fixtures do not have to hand out numbers.
func hashInode(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h | 1
}

// packAll drains the packer, returning every committed bin.
func packAll(t *testing.T, p *Packer, budget int64) []*Bin {
	t.Helper()
	var bins []*Bin
	for i := 0; ; i++ {
		if i > 10000 {
			t.Fatal("packing did not terminate")
		}
		b, ok := p.Next(budget)
		if !ok {
			break
		}
		if err := p.Commit(b); err != nil {
			t.Fatalf("commit bin %d: %v", b.Index, err)
		}
		bins = append(bins, b)
	}
	return bins
}

func TestNewOrdersUnitsDeterministically(t *testing.T) {
	entries := []scan.Entry{
		file("b", 10), file("a", 10), file("c", 30), file("d", 20),
	}
	want := []string{"c", "d", "a", "b"} // size desc, ties by first path
	for run := 0; run < 5; run++ {
		p := New(entries)
		var got []string
		for _, u := range p.units {
			got = append(got, u.Paths[0])
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: unit order = %v, want %v", run, got, want)
		}
	}
}

func TestHardlinkGroupChargedOnceAndNeverSplit(t *testing.T) {
	const ino = 4242
	entries := []scan.Entry{
		link("z/three", 100, ino),
		link("a/one", 100, ino),
		link("m/two", 100, ino),
		file("filler", 100),
	}
	p := New(entries)

	if got, want := len(p.units), 2; got != want {
		t.Fatalf("units = %d, want %d", got, want)
	}
	units, files, bytes := p.Remaining()
	if units != 2 || files != 4 || bytes != 200 {
		t.Fatalf("Remaining() = (%d,%d,%d), want (2,4,200)", units, files, bytes)
	}

	// A budget of 150 fits exactly one of the two 100-byte units, and the
	// hardlink group must arrive whole.
	b, ok := p.Next(150)
	if !ok {
		t.Fatal("Next(150) returned ok=false")
	}
	if b.RawBytes != 100 {
		t.Fatalf("RawBytes = %d, want 100", b.RawBytes)
	}
	if len(b.Files) != 3 {
		t.Fatalf("Files = %v, want the whole 3-name hardlink group", b.Files)
	}
	want := []string{"a/one", "m/two", "z/three"}
	if !reflect.DeepEqual(b.Files, want) {
		t.Fatalf("Files = %v, want %v", b.Files, want)
	}
}

func TestSkeletonInEveryBin(t *testing.T) {
	entries := []scan.Entry{
		dir("sub"), symlink("sub/link"), dir("other"),
		file("sub/a", 10), file("sub/b", 10), file("other/c", 10),
	}
	p := New(entries)
	bins := packAll(t, p, 10) // one file per bin
	if len(bins) != 3 {
		t.Fatalf("bins = %d, want 3", len(bins))
	}
	want := []string{"other", "sub", "sub/link"}
	for _, b := range bins {
		if !reflect.DeepEqual(b.Skeleton, want) {
			t.Fatalf("bin %d skeleton = %v, want %v", b.Index, b.Skeleton, want)
		}
	}
}

func TestDeterminismAcrossRuns(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var entries []scan.Entry
	for i := 0; i < 200; i++ {
		entries = append(entries, file(fmt.Sprintf("f%03d", i), rng.Int63n(50)+1))
	}
	entries = append(entries, dir("d"), symlink("s"))

	var first [][]string
	for run := 0; run < 3; run++ {
		p := New(entries)
		var layout [][]string
		for _, b := range packAll(t, p, 300) {
			layout = append(layout, b.Files)
		}
		if run == 0 {
			first = layout
			continue
		}
		if !reflect.DeepEqual(layout, first) {
			t.Fatalf("run %d produced a different layout than run 0", run)
		}
	}
}

func TestOversized(t *testing.T) {
	entries := []scan.Entry{
		file("small", 10), file("huge", 5000), file("big", 1000),
	}
	p := New(entries)

	over := p.Oversized(900)
	if len(over) != 2 {
		t.Fatalf("Oversized(900) = %v, want 2 units", over)
	}
	if over[0].Paths[0] != "huge" || over[1].Paths[0] != "big" {
		t.Fatalf("Oversized not largest-first: %v", over)
	}
	if len(p.Oversized(5000)) != 0 {
		t.Fatalf("Oversized(5000) should be empty")
	}

	// Mutating the returned unit must not corrupt the packer.
	over[0].Paths[0] = "clobbered"
	if p.units[0].Paths[0] != "huge" {
		t.Fatal("Oversized returned aliased path slices")
	}

	// Nothing fits and nothing is assignable: Next must not spin.
	if _, ok := p.Next(5); ok {
		t.Fatal("Next(5) should report ok=false when nothing fits")
	}
}

func TestOversizedIgnoresAssignedUnits(t *testing.T) {
	entries := []scan.Entry{file("a", 100), file("b", 100)}
	p := New(entries)
	b, ok := p.Next(100)
	if !ok {
		t.Fatal("Next(100) ok=false")
	}
	if err := p.Commit(b); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := p.Oversized(50); len(got) != 1 {
		t.Fatalf("Oversized(50) = %v, want only the unassigned unit", got)
	}
}

func TestEmptyInput(t *testing.T) {
	p := New(nil)
	if b, ok := p.Next(1 << 40); ok {
		t.Fatalf("Next on empty packer returned %v", b)
	}
	if u, f, by := p.Remaining(); u != 0 || f != 0 || by != 0 {
		t.Fatalf("Remaining() = (%d,%d,%d), want zeroes", u, f, by)
	}
	if p.Committed() != 0 {
		t.Fatalf("Committed() = %d, want 0", p.Committed())
	}
}

func TestSkeletonOnlyInputProducesNoBins(t *testing.T) {
	p := New([]scan.Entry{dir("a"), symlink("a/l")})
	if _, ok := p.Next(1 << 40); ok {
		t.Fatal("a tree with no regular files must produce no bins")
	}
}

func TestSingleFileExactlyBudget(t *testing.T) {
	p := New([]scan.Entry{file("exact", 1000)})
	b, ok := p.Next(1000)
	if !ok {
		t.Fatal("a file exactly equal to the budget must fit")
	}
	if b.RawBytes != 1000 || !reflect.DeepEqual(b.Files, []string{"exact"}) {
		t.Fatalf("bin = %+v", b)
	}
	if err := p.Commit(b); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, ok := p.Next(1000); ok {
		t.Fatal("packer should be drained")
	}
}

func TestZeroSizedFilesAlwaysFit(t *testing.T) {
	p := New([]scan.Entry{file("empty1", 0), file("empty2", 0)})
	b, ok := p.Next(0)
	if !ok {
		t.Fatal("zero-byte files must fit a zero budget")
	}
	if len(b.Files) != 2 {
		t.Fatalf("Files = %v, want both empty files", b.Files)
	}
}

func TestNextIsPure(t *testing.T) {
	entries := []scan.Entry{
		file("a", 500), file("b", 400), file("c", 300), file("d", 200),
	}
	p := New(entries)

	first, _ := p.Next(1000)
	second, _ := p.Next(1000)
	if !reflect.DeepEqual(first.Files, second.Files) || first.Index != second.Index {
		t.Fatalf("repeated Next differed: %v vs %v", first, second)
	}
	if u, f, by := p.Remaining(); u != 4 || f != 4 || by != 1400 {
		t.Fatalf("Next mutated assignment state: Remaining() = (%d,%d,%d)", u, f, by)
	}

	// Re-plan the same disc with a smaller budget, as the shrink-retry loop does.
	small, ok := p.Next(500)
	if !ok {
		t.Fatal("Next(500) ok=false")
	}
	if !reflect.DeepEqual(small.Files, []string{"a"}) {
		t.Fatalf("re-plan Files = %v, want [a]", small.Files)
	}
	if small.Index != 1 {
		t.Fatalf("re-plan Index = %d, want 1", small.Index)
	}
	if err := p.Commit(small); err != nil {
		t.Fatalf("commit re-planned bin: %v", err)
	}
	if u, _, by := p.Remaining(); u != 3 || by != 900 {
		t.Fatalf("after commit Remaining() = (%d,_,%d), want (3,_,900)", u, by)
	}
}

func TestCommitStaleBin(t *testing.T) {
	entries := []scan.Entry{file("a", 100), file("b", 100), file("c", 100)}

	t.Run("superseded by a later Next", func(t *testing.T) {
		p := New(entries)
		stale, _ := p.Next(100)
		if _, ok := p.Next(200); !ok {
			t.Fatal("second Next failed")
		}
		if err := p.Commit(stale); !errors.Is(err, ErrStaleBin) {
			t.Fatalf("Commit(stale) error = %v, want ErrStaleBin", err)
		}
		if p.Committed() != 0 {
			t.Fatal("a rejected commit must not advance the bin index")
		}
	})

	t.Run("committed twice", func(t *testing.T) {
		p := New(entries)
		b, _ := p.Next(100)
		if err := p.Commit(b); err != nil {
			t.Fatalf("first commit: %v", err)
		}
		if err := p.Commit(b); !errors.Is(err, ErrStaleBin) {
			t.Fatalf("double Commit error = %v, want ErrStaleBin", err)
		}
		if u, _, _ := p.Remaining(); u != 2 {
			t.Fatalf("double commit changed state: %d units remain, want 2", u)
		}
	})

	t.Run("foreign bin", func(t *testing.T) {
		p := New(entries)
		if _, ok := p.Next(100); !ok {
			t.Fatal("Next failed")
		}
		if err := p.Commit(&Bin{Index: 1}); !errors.Is(err, ErrStaleBin) {
			t.Fatalf("Commit(foreign) error = %v, want ErrStaleBin", err)
		}
	})

	t.Run("nil bin", func(t *testing.T) {
		p := New(entries)
		if err := p.Commit(nil); !errors.Is(err, ErrStaleBin) {
			t.Fatalf("Commit(nil) error = %v, want ErrStaleBin", err)
		}
	})
}

func TestBinIndexesAreConsecutive(t *testing.T) {
	var entries []scan.Entry
	for i := 0; i < 10; i++ {
		entries = append(entries, file(fmt.Sprintf("f%d", i), 100))
	}
	p := New(entries)
	for i, b := range packAll(t, p, 250) {
		if b.Index != i+1 {
			t.Fatalf("bin %d has Index %d", i, b.Index)
		}
	}
	// Ten 100-byte files at a 250-byte budget: two per disc, five discs.
	if p.Committed() != 5 {
		t.Fatalf("Committed() = %d, want 5", p.Committed())
	}
}

// TestFullPackAssignsEveryFileExactlyOnce is the property that protects against
// silent data loss: over randomised trees, every regular file must appear on
// exactly one disc, no file may appear twice, and no bin may exceed its budget.
func TestFullPackAssignsEveryFileExactlyOnce(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%02d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			entries, wantFiles, wantSkeleton, wantBytes := randomTree(rng)

			budget := int64(rng.Intn(4000) + 1000)
			p := New(entries)
			if over := p.Oversized(budget); len(over) != 0 {
				// Raise the budget above the largest unit so a full pack is
				// possible; oversized behaviour is covered elsewhere.
				budget = over[0].Size
			}
			if _, _, remBytes := p.Remaining(); remBytes != wantBytes {
				t.Fatalf("Remaining bytes = %d, want %d", remBytes, wantBytes)
			}

			seen := make(map[string]int, len(wantFiles))
			var packed int64
			for _, b := range packAll(t, p, budget) {
				if b.RawBytes > budget {
					t.Fatalf("bin %d over budget: %d > %d", b.Index, b.RawBytes, budget)
				}
				if !sort.StringsAreSorted(b.Files) {
					t.Fatalf("bin %d files are not sorted", b.Index)
				}
				if !reflect.DeepEqual(b.Skeleton, wantSkeleton) {
					t.Fatalf("bin %d skeleton differs from the scan's non-file entries", b.Index)
				}
				for _, f := range b.Files {
					seen[f]++
				}
				packed += b.RawBytes
			}

			if len(seen) != len(wantFiles) {
				t.Fatalf("packed %d distinct files, scan had %d", len(seen), len(wantFiles))
			}
			for _, f := range wantFiles {
				switch seen[f] {
				case 1:
				case 0:
					t.Fatalf("file %q was never assigned to a disc", f)
				default:
					t.Fatalf("file %q was assigned to %d discs", f, seen[f])
				}
			}
			if packed != wantBytes {
				t.Fatalf("packed %d raw bytes, scan had %d", packed, wantBytes)
			}
			if u, f, by := p.Remaining(); u != 0 || f != 0 || by != 0 {
				t.Fatalf("Remaining() = (%d,%d,%d) after a full pack", u, f, by)
			}
		})
	}
}

// randomTree builds a random entry list, returning the expected file paths, the
// expected sorted skeleton, and the expected raw byte total (hard link groups
// charged once).
func randomTree(rng *rand.Rand) (entries []scan.Entry, files []string, skeleton []string, bytes int64) {
	nDirs := rng.Intn(6) + 1
	dirs := []string{""}
	for i := 0; i < nDirs; i++ {
		parent := dirs[rng.Intn(len(dirs))]
		name := fmt.Sprintf("d%d", i)
		if parent != "" {
			name = parent + "/" + name
		}
		dirs = append(dirs, name)
		entries = append(entries, dir(name))
		skeleton = append(skeleton, name)
	}

	nFiles := rng.Intn(60)
	var lastGroup uint64
	var groupSize int64
	for i := 0; i < nFiles; i++ {
		d := dirs[rng.Intn(len(dirs))]
		rel := fmt.Sprintf("f%03d", i)
		if d != "" {
			rel = d + "/" + rel
		}
		files = append(files, rel)

		switch {
		case lastGroup != 0 && rng.Intn(3) == 0:
			// Another name for the previous inode: charged nothing extra.
			entries = append(entries, link(rel, groupSize, lastGroup))
		case rng.Intn(4) == 0:
			// Start a new hard link group.
			lastGroup = uint64(i) + 1000
			groupSize = rng.Int63n(900) + 1
			bytes += groupSize
			entries = append(entries, link(rel, groupSize, lastGroup))
		default:
			size := rng.Int63n(900)
			bytes += size
			entries = append(entries, file(rel, size))
		}
	}

	nLinks := rng.Intn(5)
	for i := 0; i < nLinks; i++ {
		rel := fmt.Sprintf("s%d", i)
		entries = append(entries, symlink(rel))
		skeleton = append(skeleton, rel)
	}

	rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	sort.Strings(skeleton)
	return entries, files, skeleton, bytes
}

// Two filesystems number their inodes independently, so the same inode number
// on two devices is two unrelated files. Grouping on the inode alone merged
// them into one unit charged a single file's size, which under-estimates the
// disc: here the merged unit would report 100 bytes for 200 bytes of content,
// and the shortfall lands on a disc already planned to be full. scan sets Dev
// on every entry precisely so this cannot happen.
func TestHardlinkGroupsOnDifferentDevicesDoNotCollide(t *testing.T) {
	const ino = 4242 // the same number on both filesystems
	entries := []scan.Entry{
		linkOn("root/one", 100, 1, ino),
		linkOn("root/two", 100, 1, ino),
		linkOn("bindmount/one", 100, 2, ino),
		linkOn("bindmount/two", 100, 2, ino),
	}
	p := New(entries)

	if got, want := len(p.units), 2; got != want {
		t.Fatalf("units = %d, want %d: one group per device, not one group total", got, want)
	}
	units, files, bytes := p.Remaining()
	if units != 2 || files != 4 || bytes != 200 {
		t.Fatalf("Remaining() = (%d,%d,%d), want (2,4,200)", units, files, bytes)
	}

	// Each unit must hold the two names from its own device and neither of the
	// other's, or the group travels to a disc its hard links do not live on.
	for _, u := range p.units {
		var want []string
		switch u.Paths[0] {
		case "bindmount/one":
			want = []string{"bindmount/one", "bindmount/two"}
		case "root/one":
			want = []string{"root/one", "root/two"}
		default:
			t.Fatalf("unexpected unit %v", u.Paths)
		}
		if !reflect.DeepEqual(u.Paths, want) {
			t.Fatalf("unit = %v, want %v", u.Paths, want)
		}
		if u.Size != 100 {
			t.Fatalf("unit %v size = %d, want 100", u.Paths, u.Size)
		}
	}
}
