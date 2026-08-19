package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mkTree materialises a small tree under a fresh temp dir. The spec strings are
// "kind:relpath[:payload]" with kind one of f (file), d (dir), l (symlink),
// h (hard link to payload), p (fifo).
func mkTree(t *testing.T, specs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, s := range specs {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) < 2 {
			t.Fatalf("bad spec %q", s)
		}
		kind, rel := parts[0], parts[1]
		payload := ""
		if len(parts) == 3 {
			payload = parts[2]
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %q: %v", rel, err)
		}
		switch kind {
		case "d":
			if err := os.MkdirAll(abs, 0o755); err != nil {
				t.Fatalf("mkdir %q: %v", rel, err)
			}
		case "f":
			if err := os.WriteFile(abs, []byte(payload), 0o644); err != nil {
				t.Fatalf("write %q: %v", rel, err)
			}
		case "l":
			if err := os.Symlink(payload, abs); err != nil {
				t.Fatalf("symlink %q: %v", rel, err)
			}
		case "h":
			if err := os.Link(filepath.Join(root, filepath.FromSlash(payload)), abs); err != nil {
				t.Fatalf("link %q: %v", rel, err)
			}
		case "p":
			if err := syscall.Mkfifo(abs, 0o644); err != nil {
				t.Skipf("mkfifo unsupported here: %v", err)
			}
		default:
			t.Fatalf("bad kind %q in spec %q", kind, s)
		}
	}
	return root
}

// rels returns the relative paths of a result, in walk order.
func rels(r *Result) []string {
	out := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		out = append(out, e.Rel)
	}
	return out
}

func mustWalk(t *testing.T, opts Options) *Result {
	t.Helper()
	res, err := Walk(context.Background(), opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected problems: %v", res.Errors)
	}
	return res
}

func TestWalkBasic(t *testing.T) {
	root := mkTree(t,
		"f:top.txt:hello",
		"d:sub",
		"f:sub/inner.bin:0123456789",
		"l:sub/link:inner.bin",
		"d:empty",
	)
	res := mustWalk(t, Options{Root: root})

	want := []string{"empty", "sub", "sub/inner.bin", "sub/link", "top.txt"}
	if got := rels(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	if res.Files != 2 {
		t.Fatalf("Files = %d, want 2", res.Files)
	}
	if res.Skeleton != 3 {
		t.Fatalf("Skeleton = %d, want 3", res.Skeleton)
	}
	if res.RawBytes != 15 {
		t.Fatalf("RawBytes = %d, want 15", res.RawBytes)
	}
	if res.Root != root {
		t.Fatalf("Root = %q, want %q", res.Root, root)
	}
	if len(res.OddPaths) != 0 {
		t.Fatalf("OddPaths = %v, want none", res.OddPaths)
	}

	byRel := map[string]Entry{}
	for _, e := range res.Entries {
		byRel[e.Rel] = e
	}
	for rel, want := range map[string]Kind{
		"top.txt":       KindFile,
		"sub":           KindDir,
		"sub/inner.bin": KindFile,
		"sub/link":      KindSymlink,
		"empty":         KindDir,
	} {
		if got := byRel[rel].Kind; got != want {
			t.Errorf("%s: kind = %v, want %v", rel, got, want)
		}
	}
	if byRel["sub/link"].Size != 0 {
		t.Errorf("symlink size = %d, want 0 (link must not be followed)", byRel["sub/link"].Size)
	}
	if byRel["top.txt"].Inode == 0 || byRel["top.txt"].Nlink != 1 {
		t.Errorf("stat data missing: %+v", byRel["top.txt"])
	}
}

func TestWalkNeverFollowsSymlinks(t *testing.T) {
	root := mkTree(t,
		"d:real",
		"f:real/a:xx",
		"l:loop:.",
		"l:toreal:real",
	)
	res := mustWalk(t, Options{Root: root})
	want := []string{"loop", "real", "real/a", "toreal"}
	if got := rels(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestWalkNonFileKinds(t *testing.T) {
	root := mkTree(t, "p:pipe")
	res := mustWalk(t, Options{Root: root})
	if len(res.Entries) != 1 || res.Entries[0].Kind != KindOther {
		t.Fatalf("entries = %+v, want one KindOther", res.Entries)
	}
	if res.Skeleton != 1 || res.Files != 0 {
		t.Fatalf("Skeleton=%d Files=%d, want 1 and 0", res.Skeleton, res.Files)
	}
}

func TestHardlinkCountedOnce(t *testing.T) {
	root := mkTree(t,
		"f:a:0123456789", // 10 bytes
		"h:b:a",          // same inode
		"d:sub",          //
		"h:sub/c:a",      // same inode again
		"f:other:01234",  // 5 bytes
	)
	res := mustWalk(t, Options{Root: root})
	if res.Files != 4 {
		t.Fatalf("Files = %d, want 4", res.Files)
	}
	if res.RawBytes != 15 {
		t.Fatalf("RawBytes = %d, want 15 (hardlinked inode charged once)", res.RawBytes)
	}
	var inode uint64
	for _, e := range res.Entries {
		switch e.Rel {
		case "a", "b", "sub/c":
			if e.Nlink != 3 {
				t.Errorf("%s: Nlink = %d, want 3", e.Rel, e.Nlink)
			}
			if inode == 0 {
				inode = e.Inode
			} else if e.Inode != inode {
				t.Errorf("%s: inode %d differs from %d", e.Rel, e.Inode, inode)
			}
		}
	}
	if inode == 0 {
		t.Fatal("no inode reported for the hardlink group")
	}
}

func TestPruneAndExclude(t *testing.T) {
	tree := []string{
		"f:keep.txt:a",
		"d:.cache",
		"f:.cache/junk:aaaa",
		"d:.cache/deep",
		"f:.cache/deep/more:aaaa",
		"d:.local/share/Trash",
		"f:.local/share/Trash/x:aa",
		"f:.local/share/keep:a",
		"d:src",
		"f:src/main.py:aa",
		"f:src/main.pyc:aaaa",
		"f:core:aaaaa",
		"d:node_modules",
		"f:node_modules/x:aaaa",
	}
	tests := []struct {
		name    string
		prune   []string
		exclude []string
		want    []string
	}{
		{
			name: "no filters",
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "core", "keep.txt", "node_modules",
				"node_modules/x", "src", "src/main.py", "src/main.pyc",
			},
		},
		{
			name:  "prune prunes the whole subtree",
			prune: []string{".cache"},
			want: []string{
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "core", "keep.txt", "node_modules",
				"node_modules/x", "src", "src/main.py", "src/main.pyc",
			},
		},
		{
			name:  "nested prune path",
			prune: []string{".local/share/Trash"},
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/keep", "core", "keep.txt",
				"node_modules", "node_modules/x", "src", "src/main.py", "src/main.pyc",
			},
		},
		{
			name:  "prune pattern with a wildcard",
			prune: []string{"node_*"},
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "core", "keep.txt", "src", "src/main.py",
				"src/main.pyc",
			},
		},
		{
			name:  "prune is anchored at the root, not the base name",
			prune: []string{"deep"},
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "core", "keep.txt", "node_modules",
				"node_modules/x", "src", "src/main.py", "src/main.pyc",
			},
		},
		{
			name:    "exclude masks match the base name anywhere",
			exclude: []string{"*.pyc", "core"},
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "keep.txt", "node_modules", "node_modules/x",
				"src", "src/main.py",
			},
		},
		{
			// A mask must never prune a subtree: the directory and everything
			// under it survive. Only PruneDirs removes a whole tree.
			name:    "exclude never prunes a matching directory's subtree",
			exclude: []string{"node_modules"},
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "core", "keep.txt", "node_modules",
				"node_modules/x", "src", "src/main.py", "src/main.pyc",
			},
		},
		{
			name:    "prune and exclude together",
			prune:   []string{".cache", ".local/share/Trash"},
			exclude: []string{"*.pyc", "core", "node_modules"},
			want: []string{
				".local", ".local/share", ".local/share/keep", "keep.txt",
				"node_modules", "node_modules/x", "src", "src/main.py",
			},
		},
		{
			name:  "empty patterns are ignored",
			prune: []string{"", "  ", "/"},
			want: []string{
				".cache", ".cache/deep", ".cache/deep/more", ".cache/junk",
				".local", ".local/share", ".local/share/Trash", ".local/share/Trash/x",
				".local/share/keep", "core", "keep.txt", "node_modules",
				"node_modules/x", "src", "src/main.py", "src/main.pyc",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := mkTree(t, tree...)
			res := mustWalk(t, Options{
				Root:         root,
				PruneDirs:    tc.prune,
				ExcludeMasks: tc.exclude,
			})
			if got := rels(res); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("entries =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

// A directory named "core" is ordinary source in Go, Drupal and kernel trees.
// Masking it must not take the subtree with it: doing so is silent data loss,
// because no skeleton entry survives to show a restorer that anything is gone.
func TestExcludeMaskKeepsSourceDirectoryNamedCore(t *testing.T) {
	root := mkTree(t,
		"d:project",
		"d:project/core",
		"d:project/core/src",
		"f:project/core/src/main.c:aaaa",
		"f:project/core.1234:aa", // a real core dump
		"f:project/keep.txt:a",
	)
	res := mustWalk(t, Options{
		Root:         root,
		ExcludeMasks: []string{"core", "core.[0-9]*"},
	})
	want := []string{
		"project", "project/core", "project/core/src",
		"project/core/src/main.c", "project/keep.txt",
	}
	if got := rels(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries =\n  %v\nwant\n  %v", got, want)
	}
}

// A mask still drops a plain *file* whose name matches.
func TestExcludeMaskDropsMatchingFile(t *testing.T) {
	root := mkTree(t, "f:core:aaaa", "f:keep.txt:a")
	res := mustWalk(t, Options{Root: root, ExcludeMasks: []string{"core"}})
	if got := rels(res); !reflect.DeepEqual(got, []string{"keep.txt"}) {
		t.Fatalf("entries = %v, want [keep.txt]", got)
	}
}

func TestPruneTrailingSlashIsTolerated(t *testing.T) {
	root := mkTree(t, "d:cache", "f:cache/x:a", "f:keep:a")
	res := mustWalk(t, Options{Root: root, PruneDirs: []string{"cache/"}})
	if got := rels(res); !reflect.DeepEqual(got, []string{"keep"}) {
		t.Fatalf("entries = %v, want [keep]", got)
	}
}

func TestBadPatternIsRejected(t *testing.T) {
	root := mkTree(t, "f:a:x")
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"prune", Options{Root: root, PruneDirs: []string{"[bad"}}},
		{"exclude", Options{Root: root, ExcludeMasks: []string{"[bad"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Walk(context.Background(), tc.opts); !errors.Is(err, filepath.ErrBadPattern) {
				t.Fatalf("err = %v, want filepath.ErrBadPattern", err)
			}
		})
	}
}

func TestOddPaths(t *testing.T) {
	root := t.TempDir()
	names := []string{"with\ttab", "with\nnewline", "plain"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Skipf("filesystem rejects %q: %v", n, err)
		}
	}
	res := mustWalk(t, Options{Root: root})
	got := append([]string(nil), res.OddPaths...)
	sort.Strings(got)
	want := []string{"with\ttab", "with\nnewline"} // '\t' (0x09) sorts before '\n' (0x0a)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OddPaths = %q, want %q", got, want)
	}
	if res.Files != 3 {
		t.Fatalf("Files = %d, want 3 (odd names are still backed up)", res.Files)
	}
}

func TestOddPathsInADirectoryName(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "od\td")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Skipf("filesystem rejects tabs in names: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	res := mustWalk(t, Options{Root: root})
	want := []string{"od\td", "od\td/child"}
	if !reflect.DeepEqual(res.OddPaths, want) {
		t.Fatalf("OddPaths = %q, want %q", res.OddPaths, want)
	}
}

func TestUnreadableDirectoryIsAProblemNotAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	root := mkTree(t, "f:visible:aa", "d:locked", "f:locked/hidden:aaaa")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, err := Walk(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Walk returned a fatal error: %v", err)
	}
	if got := rels(res); !reflect.DeepEqual(got, []string{"locked", "visible"}) {
		t.Fatalf("entries = %v, want [locked visible]", got)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly one", res.Errors)
	}
	if res.Errors[0].Path != locked {
		t.Fatalf("problem path = %q, want %q", res.Errors[0].Path, locked)
	}
	if !errors.Is(res.Errors[0], os.ErrPermission) {
		t.Fatalf("problem error = %v, want a permission error", res.Errors[0].Err)
	}
	if res.RawBytes != 2 {
		t.Fatalf("RawBytes = %d, want 2", res.RawBytes)
	}
}

func TestCallbacks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	root := mkTree(t, "f:a:x", "d:sub", "f:sub/b:xy", "d:locked", "f:locked/c:xyz")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	var seen []string
	var problems []string
	res, err := Walk(context.Background(), Options{
		Root:    root,
		OnEntry: func(e Entry) { seen = append(seen, e.Rel) },
		OnError: func(path string, err error) { problems = append(problems, path) },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !reflect.DeepEqual(seen, rels(res)) {
		t.Fatalf("OnEntry saw %v, entries are %v", seen, rels(res))
	}
	if !reflect.DeepEqual(problems, []string{locked}) {
		t.Fatalf("OnError saw %v, want [%s]", problems, locked)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none when OnError is set", res.Errors)
	}
}

func TestOneFileSystem(t *testing.T) {
	// Everything under a temp dir shares one device, so -xdev must change
	// nothing. Crossing a real mount point cannot be arranged without root.
	root := mkTree(t, "f:a:x", "d:sub", "f:sub/b:xy")
	with := mustWalk(t, Options{Root: root, OneFileSystem: true})
	without := mustWalk(t, Options{Root: root})
	if !reflect.DeepEqual(rels(with), rels(without)) {
		t.Fatalf("-xdev changed a single-filesystem walk: %v vs %v", rels(with), rels(without))
	}
	if len(with.SkippedMounts) != 0 {
		t.Fatalf("SkippedMounts = %v on a single-filesystem tree, want none", with.SkippedMounts)
	}
}

// TestOneFileSystemReportsTheMountPointsItSkipped fakes a mount point: the
// directory "mnt" is made to report a device number different from the root's,
// which is exactly what a real mount does. The walker must then leave "mnt" in
// the skeleton, drop everything under it, and NAME it in SkippedMounts — the
// silent version of this is how a NAS mounted under SOURCE_DIR was left off
// every disc without a word.
func TestOneFileSystemReportsTheMountPointsItSkipped(t *testing.T) {
	root := mkTree(t,
		"f:a:x",
		"d:mnt",
		"f:mnt/onthemount:12345",
		"d:mnt/deeper",
		"f:mnt/deeper/c:678",
		"d:sub",
		"f:sub/b:xy",
		"d:sub/mnt2",
		"f:sub/mnt2/d:9",
	)
	// The fake: any directory whose base name starts with "mnt" is on device
	// rootDev+1. os.FileInfo has no path, but the temp dir is ours, so the
	// name is enough to tell them apart.
	real := statIDs
	statIDs = func(fi os.FileInfo) (dev, ino, nlink uint64) {
		dev, ino, nlink = real(fi)
		if fi.IsDir() && strings.HasPrefix(fi.Name(), "mnt") {
			dev++
		}
		return dev, ino, nlink
	}
	t.Cleanup(func() { statIDs = real })

	res := mustWalk(t, Options{Root: root, OneFileSystem: true})
	want := []string{"a", "mnt", "sub", "sub/b", "sub/mnt2"}
	if got := rels(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v (mount points kept, subtrees dropped)", got, want)
	}
	if got := res.SkippedMounts; !reflect.DeepEqual(got, []string{"mnt", "sub/mnt2"}) {
		t.Fatalf("SkippedMounts = %v, want [mnt sub/mnt2]", got)
	}
	if res.RawBytes != 3 {
		t.Fatalf("RawBytes = %d, want 3 (nothing under a mount point is charged)", res.RawBytes)
	}

	// Without -xdev the same tree is walked whole and nothing is reported.
	all := mustWalk(t, Options{Root: root})
	if len(all.SkippedMounts) != 0 {
		t.Fatalf("SkippedMounts = %v without OneFileSystem, want none", all.SkippedMounts)
	}
	if len(all.Entries) != 9 {
		t.Fatalf("entries without OneFileSystem = %v, want all 9", rels(all))
	}
}

// TestUnreadableFileIsAProblemAndNotAnEntry pins the guard against silent data
// loss: mksquashfs exits 0 on a source file it cannot open and writes an empty
// file in its place, so an unreadable file must never reach it. The walker
// reports it as a problem and leaves it out of Entries, Files and RawBytes.
func TestUnreadableFileIsAProblemAndNotAnEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	root := mkTree(t, "f:visible:aa", "d:sub", "f:sub/secret:aaaa", "f:sub/ok:a")
	secret := filepath.Join(root, "sub", "secret")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })

	res, err := Walk(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Walk returned a fatal error: %v", err)
	}
	if got := rels(res); !reflect.DeepEqual(got, []string{"sub", "sub/ok", "visible"}) {
		t.Fatalf("entries = %v, want [sub sub/ok visible] — the unreadable file must not be listed", got)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly one", res.Errors)
	}
	if res.Errors[0].Path != secret {
		t.Fatalf("problem path = %q, want %q", res.Errors[0].Path, secret)
	}
	if !errors.Is(res.Errors[0], os.ErrPermission) {
		t.Fatalf("problem error = %v, want a permission error", res.Errors[0].Err)
	}
	if res.Files != 2 || res.RawBytes != 3 {
		t.Fatalf("Files=%d RawBytes=%d, want 2 and 3 (the unreadable file is not counted)", res.Files, res.RawBytes)
	}
}

// TestUnreadableFileGoesToOnError is the same guard through the callback path,
// which is what a caller that streams problems relies on.
func TestUnreadableFileGoesToOnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	root := mkTree(t, "f:a:x", "f:locked:xyz")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	var seen, problems []string
	res, err := Walk(context.Background(), Options{
		Root:    root,
		OnEntry: func(e Entry) { seen = append(seen, e.Rel) },
		OnError: func(path string, err error) { problems = append(problems, path) },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !reflect.DeepEqual(seen, []string{"a"}) {
		t.Fatalf("OnEntry saw %v, want [a]", seen)
	}
	if !reflect.DeepEqual(problems, []string{locked}) {
		t.Fatalf("OnError saw %v, want [%s]", problems, locked)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none when OnError is set", res.Errors)
	}
}

// TestNonRegularEntriesAreNeverOpened: the readability check opens regular
// files only. A fifo with no writer would block an open forever, and the walk
// with it, so it must be classified and reported without ever being opened.
// The test would hang, not fail, if that rule were broken — hence the timeout.
func TestNonRegularEntriesAreNeverOpened(t *testing.T) {
	root := mkTree(t, "p:pipe", "f:a:x")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan *Result, 1)
	go func() {
		res, err := Walk(ctx, Options{Root: root})
		if err != nil {
			t.Errorf("Walk: %v", err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil {
			return
		}
		if got := rels(res); !reflect.DeepEqual(got, []string{"a", "pipe"}) {
			t.Fatalf("entries = %v, want [a pipe]", got)
		}
		if len(res.Errors) != 0 {
			t.Fatalf("Errors = %v, want none", res.Errors)
		}
	case <-ctx.Done():
		t.Fatal("Walk blocked on a fifo: it must not open non-regular entries")
	}
}

func TestRootErrors(t *testing.T) {
	dirRoot := mkTree(t, "f:a:x")
	t.Run("empty", func(t *testing.T) {
		if _, err := Walk(context.Background(), Options{}); !errors.Is(err, ErrNoRoot) {
			t.Fatalf("err = %v, want ErrNoRoot", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		_, err := Walk(context.Background(), Options{Root: filepath.Join(dirRoot, "nope")})
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v, want os.ErrNotExist", err)
		}
	})
	t.Run("not a directory", func(t *testing.T) {
		_, err := Walk(context.Background(), Options{Root: filepath.Join(dirRoot, "a")})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("err = %v, want a 'not a directory' error", err)
		}
	})
}

func TestEmptyRootYieldsEmptyResult(t *testing.T) {
	res := mustWalk(t, Options{Root: t.TempDir()})
	if len(res.Entries) != 0 || res.Files != 0 || res.RawBytes != 0 || res.Skeleton != 0 {
		t.Fatalf("result = %+v, want all zero", res)
	}
}

func TestContextCancellation(t *testing.T) {
	specs := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		specs = append(specs, "f:d"+string(rune('a'+i%26))+"/f"+itoa(i)+":x")
	}
	root := mkTree(t, specs...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Walk(ctx, Options{Root: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// Cancelling partway through must also abort.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	n := 0
	_, err := Walk(ctx2, Options{
		Root: root,
		OnEntry: func(Entry) {
			n++
			if n == 300 {
				cancel2()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n >= 2000 {
		t.Fatalf("walk visited %d entries after cancellation", n)
	}
}

// itoa avoids pulling strconv into the fixture helpers above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		KindFile: "file", KindDir: "dir", KindSymlink: "symlink",
		KindOther: "other", Kind(200): "unknown",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// TestOddPathsReportsCarriageReturns is the one control byte that used to slip
// through: indexfmt escapes backslash, tab and newline, so a CR travels into
// the on-disc index raw and the operator was never told the file existed.
func TestOddPathsReportsCarriageReturns(t *testing.T) {
	root := t.TempDir()
	names := []string{"report.pdf\r", "esc\x1b[31mname", "del\x7fname", "plain.txt"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Skipf("filesystem rejects %q: %v", n, err)
		}
	}
	res := mustWalk(t, Options{Root: root})
	got := append([]string(nil), res.OddPaths...)
	sort.Strings(got)
	want := []string{"report.pdf\r", "del\x7fname", "esc\x1b[31mname"} // sorted by byte
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OddPaths = %q, want %q", got, want)
	}
	// And the caller must be able to tell the escaped group from the raw one,
	// because only one of the two warnings it prints is true of each.
	for _, p := range got {
		if HasIndexEscape(p) {
			t.Errorf("HasIndexEscape(%q) = true; the index escapes only tab and newline", p)
		}
	}
	if !HasIndexEscape("a\tb") || !HasIndexEscape("a\nb") {
		t.Error("HasIndexEscape misses a tab or a newline")
	}
}

// TestHasIndexEscapeClassifiesMixedNamesAsRaw covers the case a plain "contains
// a tab or a newline" test gets wrong: a name carrying both an escaped byte and
// an unescaped one. Calling it escaped tells the operator the index handled
// what the name contains, which is false of the carriage return in it — and the
// CR is the byte that makes the name untypable for 'restore --only'.
func TestHasIndexEscapeClassifiesMixedNamesAsRaw(t *testing.T) {
	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{"sheet\tone", true},
		{"line\nbreak", true},
		{"sheet\tone\r", false},
		{"line\nbreak\x1b[31m", false},
		{"report.pdf\r", false},
		{"del\x7fname", false},
		{`back\slash`, false}, // escaped by indexfmt, but not a control byte
		{"plain.txt", false},
	} {
		if got := HasIndexEscape(tc.rel); got != tc.want {
			t.Errorf("HasIndexEscape(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// TestCompiledMasksMatchFilepathMatch pins the fast paths to the semantics they
// replace: the classification is only worth having if it accepts exactly the
// set filepath.Match accepts for a base name.
func TestCompiledMasksMatchFilepathMatch(t *testing.T) {
	pats := []string{"*.pyc", "*.pyo", ".DS_Store", "core.[0-9]*", "*", "a*b", `a\*b`, "*.tar.gz"}
	names := []string{
		"x.pyc", ".pyc", "pyc", "x.pyo", ".DS_Store", ".ds_store", "DS_Store",
		"core.1", "core.12345", "core.x", "core", "ab", "axb", "a*b", "a.b",
		"x.tar.gz", ".tar.gz", "", "x", "*",
	}
	for _, p := range pats {
		w := &walker{masks: compileMasks([]string{p})}
		for _, n := range names {
			want, err := filepath.Match(p, n)
			if err != nil {
				t.Fatalf("Match(%q, %q): %v", p, n, err)
			}
			if got := w.excluded(n); got != want {
				t.Errorf("mask %q against %q: excluded = %v, filepath.Match = %v", p, n, got, want)
			}
		}
	}
}
