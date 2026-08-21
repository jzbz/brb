package restore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/tools"
)

// --only "." (or anything that names the archive root) passes covers() for
// every entry and makes unsquashfs extract nothing; it used to report success
// with an empty destination. It is refused up front.
func TestIsArchiveRoot(t *testing.T) {
	for _, p := range []string{".", "./", "/", "//", " . ", "sub/..", "./sub/../", ".."} {
		if !isArchiveRoot(p) {
			t.Errorf("isArchiveRoot(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"sub", "./sub", "/sub/", "sub/f.txt", "..sub", ".hidden", "sub/..x"} {
		if isArchiveRoot(p) {
			t.Errorf("isArchiveRoot(%q) = true, want false", p)
		}
	}
}

func TestRestoreOnlyRefusesTheArchiveRoot(t *testing.T) {
	e := newEnv(t)
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Unsquashfs, Path: "/bin/true", Found: true}})
	for _, p := range []string{".", "./", "/"} {
		err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: filepath.Join(e.dir, "d"), Only: []string{p}})
		if err == nil || !strings.Contains(err.Error(), "that is the whole tree — run restore without --only") {
			t.Errorf("Restore --only %q = %v, want the whole-tree refusal", p, err)
		}
	}
}

// listedDir has to find the path in an "unsquashfs -ll" line whatever the
// name holds, and answer for directories only.
func TestListedDir(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
		ok   bool
	}{
		{"drwxr-xr-x jz/jz                    96 2026-08-19 01:30 squashfs-root", "", false},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/emptydir", "emptydir", true},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/name with space", "name with space", true},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/sub/deep", "sub/deep", true},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/two  spaces", "two  spaces", true},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/ leading", " leading", true},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/trailing\r", "trailing\r", true},
		{"drwxr-xr-x 1000/1000            123456789 2026-08-19 01:30 squashfs-root/wide", "wide", true},
		{"-rw-r--r-- jz/jz                     3 2026-08-19 01:30 squashfs-root/sub/f.txt", "", false},
		{"lrwxrwxrwx jz/jz                     5 2026-08-19 01:30 squashfs-root/sub/link -> f.txt", "", false},
		{"Parallel unsquashfs: Using 8 processors", "", false},
		{"4 inodes (2 blocks) to write", "", false},
		{"", "", false},
		{"drwxr-xr-x", "", false},
	} {
		got, ok := listedDir(tc.line)
		if got != tc.want || ok != tc.ok {
			t.Errorf("listedDir(%q) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestExtractionTouches(t *testing.T) {
	for _, tc := range []struct {
		only []string
		dir  string
		want bool
	}{
		{nil, "anything", true},
		{[]string{"sub/f.txt"}, "sub", true},           // ancestor
		{[]string{"sub/deep/f.txt"}, "sub/deep", true}, // deeper ancestor
		{[]string{"sub"}, "sub", true},                 // the directory itself
		{[]string{"sub"}, "sub/deep", true},            // under it
		{[]string{"sub/"}, "sub/deep", true},
		{[]string{"sub/f.txt"}, "subdir", false}, // a sibling with a common prefix
		{[]string{"sub/f.txt"}, "other", false},
		{[]string{"a/x", "b/y"}, "b", true},
	} {
		if got := extractionTouches(tc.only, tc.dir); got != tc.want {
			t.Errorf("extractionTouches(%q, %q) = %v, want %v", tc.only, tc.dir, got, tc.want)
		}
	}
}

// symlinkSet stages a two-disc set whose first disc holds an empty directory,
// a directory with children, and a symlink of its own — everything the
// per-image symlink guard has to tell apart.
func symlinkSet(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.squashTools(t)
	src := filepath.Join(e.dir, "src01")
	if err := os.MkdirAll(filepath.Join(src, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "unrelated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f.txt", filepath.Join(src, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	e.makeDisc(1, map[string]string{"sub/f.txt": "one\n"})
	e.makeDisc(2, map[string]string{"sub/other.txt": "two\n"})
	e.writeManifest(manifestSaying(2))
	e.ui.SetAssumeYes(true)
	return e
}

// A symlink to a file planted where the archive has a directory: unsquashfs
// -f would apply the archive directory's mode and mtime through it to the
// target — reproduced against an empty directory, the case no index knows.
// The restore is refused before it starts and the target is untouched.
func TestRestoreRefusesASymlinkToAFileAtAnArchiveDirectory(t *testing.T) {
	for _, dir := range []string{"emptydir", "sub"} {
		t.Run(dir, func(t *testing.T) {
			e := symlinkSet(t)
			victim := filepath.Join(e.dir, "victim")
			if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(e.dir, "dest")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, filepath.Join(dest, dir)); err != nil {
				t.Fatal(err)
			}
			err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest})
			if err == nil || !strings.Contains(err.Error(), "THROUGH them") || !strings.Contains(err.Error(), filepath.Join(dest, dir)) {
				t.Fatalf("Restore = %v, want a refusal naming %s\n%s", err, filepath.Join(dest, dir), e.log())
			}
			fi, err := os.Stat(victim)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Fatalf("the target's mode was changed to %o through the symlink", fi.Mode().Perm())
			}
		})
	}
}

// A dangling symlink at an archive directory is refused too: unsquashfs
// would create through it wherever it points.
func TestRestoreRefusesADanglingSymlinkAtAnArchiveDirectory(t *testing.T) {
	e := symlinkSet(t)
	dest := filepath.Join(e.dir, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.dir, "does-not-exist"), filepath.Join(dest, "emptydir")); err != nil {
		t.Fatal(err)
	}
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest})
	if err == nil || !strings.Contains(err.Error(), "THROUGH them") {
		t.Fatalf("Restore = %v, want a refusal\n%s", err, e.log())
	}
}

// The legitimate multi-disc case: disc 1 extracts a symlink the archive holds
// as a symlink, and disc 2 extracts into the same tree. That link is at a path
// the archive holds as a leaf, not a directory, so it must not stop disc 2 —
// and a second full restore over the first must pass for the same reason.
func TestRestoreAcrossDiscsToleratesTheArchivesOwnSymlinks(t *testing.T) {
	e := symlinkSet(t)
	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	link := filepath.Join(dest, "sub", "link")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("disc 1's symlink was not restored: %v", err)
	}
	for _, rel := range []string{"sub/f.txt", "sub/other.txt", "emptydir"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not restored: %v", rel, err)
		}
	}
	// And again over the populated tree, link and all.
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("second Restore over the same tree: %v\n%s", err, e.log())
	}
}

// With --only, auditImage's symlink guard checks only the directories the
// extraction touches, so a symlink to a FILE somewhere else in a live tree
// does not block fetching one file back into it — but the same link is refused
// when the extraction would pass through it.
//
// Only that guard is narrowed. A symlink to a DIRECTORY is still refused
// wherever it sits, --only or not; TestRestoreOnlyDoesNotExemptADirectorySymlink
// is the other half of this pair.
func TestRestoreOnlyChecksOnlyTheDirectoriesItTouches(t *testing.T) {
	e := symlinkSet(t)
	victim := filepath.Join(e.dir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(e.dir, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dest, "unrelated")); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Only: []string{"sub/f.txt"}}); err != nil {
		t.Fatalf("Restore --only past an unrelated symlink: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "f.txt")); err != nil {
		t.Fatal("the requested file was not restored")
	}
	// The set has no index, so --only falls back to asking every image; disc
	// 1 is the one that holds "unrelated".
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Only: []string{"unrelated"}})
	if err == nil || !strings.Contains(err.Error(), "THROUGH them") {
		t.Fatalf("Restore --only through the symlink = %v, want a refusal\n%s", err, e.log())
	}
}

// The traversal guard is NOT narrowed by --only, and the comment on
// auditImage's symlink guard used to read as though it were. A symlink that
// resolves to a DIRECTORY is refused wherever it sits under the destination,
// even when the extraction would never go near it: refuseSymlinkedDirs runs
// once, before the first image, over the whole tree. That is what README.md's
// Limitations promise — "refused outright, --yes or not" — and it is the
// reason `brb restore ~ --only one/file` fails on a live $HOME that happens to
// contain a ~/tmp -> /var/tmp. An engineer who narrows that walk to the --only
// paths has to mirror it in brb.sh and in xcompat-test.sh's destination
// symlink assertions, which hold both readers to this refusal.
func TestRestoreOnlyDoesNotExemptADirectorySymlink(t *testing.T) {
	e := symlinkSet(t)
	elsewhere := filepath.Join(e.dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(e.dir, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing in the archive is named "attic", so extraction never touches it.
	if err := os.Symlink(elsewhere, filepath.Join(dest, "attic")); err != nil {
		t.Fatal(err)
	}
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Only: []string{"sub/f.txt"}})
	if err == nil || !strings.Contains(err.Error(), "symlink(s) to directories") {
		t.Fatalf("Restore --only with an untouched directory symlink = %v, want the whole-tree refusal\n%s", err, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "f.txt")); err == nil {
		t.Fatal("the run was refused, but it had already extracted")
	}
}

// fakeUnsquashfs installs a shell wrapper around the real unsquashfs that
// exits with status rc after a successful extraction, printing one line of
// complaint the way unsquashfs does when it could not restore an attribute.
// Listing modes pass straight through. It gives the exit-status handling a
// deterministic case on any machine.
func fakeUnsquashfs(t *testing.T, e *env, rc int) string {
	t.Helper()
	real := e.tools.Get(tools.Unsquashfs)
	if !real.Found {
		t.Skip("no unsquashfs to wrap")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build an unsquashfs stand-in with")
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in -l|-ll|-lls|-lln|-s) exec " + real.Path + " \"$@\";; esac\n" +
		real.Path + " \"$@\"; st=$?\n" +
		"[ $st -eq 0 ] || exit $st\n" +
		"echo 'write_xattr: could not write xattr user.brb.test for file x because Operation not supported' >&2\n" +
		"exit " + strconv.Itoa(rc) + "\n"
	path := filepath.Join(t.TempDir(), "unsquashfs")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Unsquashfs, Path: path, Found: true}})
	return path
}

// unsquashfs exit 2 — extracted everything, could not restore some attribute
// — is a warning that names the kept log, and the restore goes on to the next
// disc. brb.sh has behaved this way all along; the Go reader used to abort.
func TestRestoreContinuesPastUnsquashfsExitTwo(t *testing.T) {
	e := symlinkSet(t)
	fakeUnsquashfs(t, e, 2)
	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore = %v, want success past exit status 2\n%s", err, e.log())
	}
	for _, rel := range []string{"sub/f.txt", "sub/other.txt"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not restored: %v", rel, err)
		}
	}
	for _, n := range []string{"disc01.squashfs", "disc02.squashfs"} {
		logPath := filepath.Join(e.cfg.Dirs().Restore, "unsquashfs."+n+".log")
		if !strings.Contains(e.log(), "unsquashfs reported non-fatal errors on "+n+" — see "+logPath) {
			t.Errorf("the run did not warn about %s and name its log:\n%s", n, e.log())
		}
		body, err := os.ReadFile(logPath)
		if err != nil {
			t.Errorf("the log for %s was not kept: %v", n, err)
		} else if !strings.Contains(string(body), "write_xattr") {
			t.Errorf("the log for %s does not hold unsquashfs's output:\n%s", n, body)
		}
		if !strings.Contains(e.log(), n+" extracted") {
			t.Errorf("%s was not reported extracted:\n%s", n, e.log())
		}
	}
}

// Any other non-zero status is still fatal, and the error says where the
// output went. A clean run leaves no log behind.
func TestRestoreStopsOnUnsquashfsExitOne(t *testing.T) {
	e := symlinkSet(t)
	fakeUnsquashfs(t, e, 1)
	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest})
	logPath := filepath.Join(e.cfg.Dirs().Restore, "unsquashfs.disc01.squashfs.log")
	if err == nil || !strings.Contains(err.Error(), "extracting disc01.squashfs") || !strings.Contains(err.Error(), logPath) {
		t.Fatalf("Restore = %v, want a failure naming %s\n%s", err, logPath, e.log())
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("the log of the failed extraction was not kept: %v", err)
	}
	if strings.Contains(e.log(), "disc02.squashfs extracted") {
		t.Errorf("the restore went on past a fatal unsquashfs failure:\n%s", e.log())
	}
}

func TestRestoreLeavesNoLogAfterACleanExtraction(t *testing.T) {
	e := symlinkSet(t)
	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	ents, err := os.ReadDir(e.cfg.Dirs().Restore)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.HasPrefix(ent.Name(), "unsquashfs.") {
			t.Errorf("a clean extraction left %s behind", ent.Name())
		}
	}
}

// The real thing, where the host can produce it: on an SELinux system
// mksquashfs records security.selinux on every entry, and an unprivileged
// unsquashfs asked to restore every xattr extracts everything and exits 2.
// This pins that the exit status the wrapper classifies is the one unsquashfs
// really uses; hosts that cannot produce it skip.
func TestUnsquashfsExitTwoIsWhatARealNonFatalRunReturns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root can write every xattr, so unsquashfs exits 0")
	}
	e := newEnv(t)
	e.squashTools(t)
	src := filepath.Join(e.dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(e.dir, "img.squashfs")
	if err := e.tools.BuildImage(context.Background(), tools.MkOptions{
		SourceDir: src, Out: img, Files: []string{"a.txt"}, Compression: "none", BlockSize: "128K",
		Xattrs: true, // as a backup builds it: the labels ride along
	}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(e.dir, "dest")
	err := e.tools.Unsquashfs(context.Background(), tools.UnsqOptions{
		Image: img, Dest: dest, Force: true, Xattrs: true, // every namespace, root's setting
	})
	if err == nil {
		t.Skip("skipping: this host's unsquashfs restores every xattr as non-root, so there is no exit 2 to see")
	}
	if got := tools.ExitCode(err); got != 2 {
		t.Fatalf("ExitCode = %d for %v; want 2 — the non-fatal status the extract path treats as a warning", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.txt")); err != nil {
		t.Fatalf("exit 2 but the file was not extracted: %v", err)
	}
}

// A destination that is itself a symlink to a directory is refused, and so is
// the same destination spelled with a trailing slash: the slash makes the
// kernel resolve the link before Lstat sees it, so an uncleaned path would
// walk the target, find nothing to refuse, and write the archive outside the
// destination. brb.sh strips the slash; xcompat pins both readers on it.
func TestRestoreRefusesASymlinkedDestinationWithATrailingSlash(t *testing.T) {
	e := symlinkSet(t)
	victim := filepath.Join(e.dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(e.dir, "dest")
	if err := os.Symlink(victim, dest); err != nil {
		t.Fatal(err)
	}
	for _, spelled := range []string{dest, dest + "/"} {
		err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: spelled})
		if err == nil || !strings.Contains(err.Error(), "symlink(s) to directories") {
			t.Errorf("Restore into %q = %v, want the symlinked-directory refusal\n%s", spelled, err, e.log())
		}
		if _, err := os.Stat(filepath.Join(victim, "sub")); err == nil {
			t.Fatalf("Restore into %q wrote the archive through the destination link", spelled)
		}
	}
}
