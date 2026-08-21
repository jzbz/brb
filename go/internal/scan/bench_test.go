package scan

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jzbz/brb/internal/config"
)

// The benchmarks below exist to settle two claims that cannot be settled by
// reading: that hoisting the prune classification out of the per-entry loop
// (compilePrunes) and inheriting the control-byte answer down the recursion
// (walker.dir's relOdd) are worth the code they cost. Both remove CPU from a
// loop whose wall clock is dominated by the kernel — one lstat per entry, plus
// one open+close per regular file — so the honest question is not "is there
// less work" (there provably is) but "is any of it visible".
//
// Run them on tmpfs, where the syscall floor is lowest and the CPU delta is
// therefore most visible; a spinning disc or a cold page cache will bury the
// difference:
//
//	TMPDIR=/dev/shm go test ./internal/scan -run xxx -bench Walk \
//	    -benchtime=1x -count=10 -benchmem | tee after.txt
//	benchstat before.txt after.txt
//
// Discard the first run so the cache is warm. The shapes are chosen to isolate
// the two effects and to guard the regressions each risks:
//
//   - BenchmarkWalkDeep vs BenchmarkWalkFlat: the odd-path inheritance saves
//     work proportional to depth, so the deep tree should improve and the flat
//     one should be a wash. The flat tree is the guardrail: it must not
//     regress, and allocs/op must not move, because neither change allocates
//     per entry.
//   - BenchmarkWalkPruneCount: sweeps PRUNE_DIRS across 0, 15 (the shipped
//     default) and 60 patterns. The point is the shape of the curve, not one
//     number: before compilePrunes the cost per entry was linear in the pattern
//     count, after it the curve should be flat.
//   - BenchmarkWalkTiny: the startup guardrail. compilePrunes builds one map
//     per Walk, so a walk over an almost-empty tree pays a fixed allocation it
//     did not before. That must stay lost in the noise, and -benchmem must show
//     the map as one allocation per Walk rather than one per entry.

// benchTree materialises a synthetic tree of about n entries, fanning out
// breadth-wide at each of depth levels, with name lengths in the range a real
// source tree produces. Files are empty: the walk lstats and opens them, and
// reads nothing, so their contents would only slow the fixture down.
func benchTree(b *testing.B, depth, breadth, filesPerDir int) string {
	b.Helper()
	root := b.TempDir()
	var mk func(dir string, level int)
	mk = func(dir string, level int) {
		for i := 0; i < filesPerDir; i++ {
			p := filepath.Join(dir, "component_file_name_"+strconv.Itoa(i)+".txt")
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				b.Fatalf("write %s: %v", p, err)
			}
		}
		if level >= depth {
			return
		}
		for i := 0; i < breadth; i++ {
			sub := filepath.Join(dir, "component_directory_"+strconv.Itoa(i))
			if err := os.Mkdir(sub, 0o755); err != nil {
				b.Fatalf("mkdir %s: %v", sub, err)
			}
			mk(sub, level+1)
		}
	}
	mk(root, 0)
	return root
}

// benchWalk runs one Walk and fails the benchmark if it did not see the tree,
// so a fixture that silently failed to materialise cannot look like a win.
func benchWalk(b *testing.B, opts Options) {
	b.Helper()
	res, err := Walk(context.Background(), opts)
	if err != nil {
		b.Fatalf("Walk: %v", err)
	}
	if len(res.Entries) == 0 {
		b.Fatal("Walk found nothing: the fixture is wrong, not the code")
	}
}

// BenchmarkWalkDeep is the shape the odd-path inheritance is for: every entry's
// relative path carries ten ancestors' bytes, which the old whole-path scan
// re-read once per descendant.
func BenchmarkWalkDeep(b *testing.B) {
	root := benchTree(b, 10, 2, 8)
	opts := Options{
		Root:          root,
		PruneDirs:     config.DefaultPruneDirs(),
		ExcludeMasks:  config.DefaultExcludeMasks(),
		OneFileSystem: true,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchWalk(b, opts)
	}
}

// BenchmarkWalkFlat is the guardrail for the same change: one directory, short
// paths, nothing to inherit. It must not regress.
func BenchmarkWalkFlat(b *testing.B) {
	root := benchTree(b, 0, 0, 20000)
	opts := Options{
		Root:          root,
		PruneDirs:     config.DefaultPruneDirs(),
		ExcludeMasks:  config.DefaultExcludeMasks(),
		OneFileSystem: true,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchWalk(b, opts)
	}
}

// BenchmarkWalkPruneCount sweeps the prune-pattern count over one fixed tree.
// Before compilePrunes the per-entry cost grew with the number of patterns
// configured, so the three sub-benchmarks diverged; after it they should not.
func BenchmarkWalkPruneCount(b *testing.B) {
	root := benchTree(b, 6, 3, 6)
	for _, n := range []int{0, 15, 60} {
		b.Run(strconv.Itoa(n)+"patterns", func(b *testing.B) {
			opts := Options{
				Root:          root,
				PruneDirs:     benchPrunes(n),
				ExcludeMasks:  config.DefaultExcludeMasks(),
				OneFileSystem: true,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchWalk(b, opts)
			}
		})
	}
}

// BenchmarkWalkTiny is the startup guardrail: compilePrunes' map is built once
// per Walk, and on a ten-entry tree that fixed cost has nowhere to hide.
func BenchmarkWalkTiny(b *testing.B) {
	root := benchTree(b, 0, 0, 10)
	opts := Options{
		Root:          root,
		PruneDirs:     config.DefaultPruneDirs(),
		ExcludeMasks:  config.DefaultExcludeMasks(),
		OneFileSystem: true,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchWalk(b, opts)
	}
}

// benchPrunes returns n prune patterns: the shipped defaults first, so the
// 15-pattern case is the real configuration, then synthetic literals that match
// nothing — which is the interesting case, since a pattern that matches is
// tested once and then prunes a whole subtree, while one that does not is
// tested against every entry the walk sees.
func benchPrunes(n int) []string {
	out := config.DefaultPruneDirs()
	if n <= len(out) {
		return out[:n]
	}
	for i := len(out); i < n; i++ {
		out = append(out, "no_such_directory_"+strconv.Itoa(i))
	}
	return out
}
