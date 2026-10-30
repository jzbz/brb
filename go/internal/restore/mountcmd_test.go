package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// stubMount puts a stand-in mount(8) first on PATH that records its arguments,
// one per line, and lets Mount past its root check, so that the whole of Mount
// runs unprivileged and nothing is ever really mounted. It returns the file the
// arguments land in, which does not exist until the stand-in has been run.
//
// The stand-in is installed even by the tests that expect a refusal before
// mount(8) is reached. Run as root with the guard under test deleted, those
// would otherwise hand the real mount(8) a path chosen to be the wrong one.
func stubMount(t *testing.T) (argvFile string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build a mount(8) stand-in with")
	}
	bin := t.TempDir()
	argvFile = filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argvFile + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "mount"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	saved := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = saved })
	return argvFile
}

// TestMountHandsMountTheHardenedArguments pins what reaches mount(8), argument
// for argument, which is the only place the next three properties exist:
//
//   - nosuid and nodev. ro does not imply either, so a setuid-root binary from
//     a backup of /, or a device node in the image, used to act on the host for
//     any account that could reach the mount.
//   - "--" before the operands, so a mount point with a leading dash is a mount
//     point. brb.sh used to leave it out and read such a name as options.
//   - the mount point as dirPath spells it: the trailing slashes are gone, the
//     same bytes brb.sh hands over.
//
// And it pins the mode of what Mount creates on the way: 0700 for the leaf and
// every missing parent, as brb.sh's mkdir -p makes them under its umask 077.
// The Go build used to make them 0755. The umask is set to the usual 022 for
// the run, because under a 077 one 0755 and 0700 come out the same and the
// check would prove nothing.
//
// xcompat-test.sh holds both readers to the same argument list.
func TestMountHandsMountTheHardenedArguments(t *testing.T) {
	argvFile := stubMount(t)
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	e := newEnv(t)
	e.writeImage(1, bytes.Repeat([]byte("pretend squashfs\n"), 512))
	mnt := filepath.Join(e.dir, "mnt")
	leaf := filepath.Join(mnt, "new", "-leaf")

	if err := Mount(context.Background(), e.opts(), 1, leaf+"//"); err != nil {
		t.Fatalf("Mount: %v\n%s", err, e.log())
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("mount(8) was never run: %v\n%s", err, e.log())
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	want := []string{"-o", "loop,ro,nosuid,nodev", "--", plain, leaf}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mount(8) was given\n %q\nwant\n %q", got, want)
	}
	for _, d := range []string{mnt, filepath.Join(mnt, "new"), leaf} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s was created %o, want 0700 — the mode brb.sh gives it", d, perm)
		}
	}
	if !strings.Contains(e.log(), "mounted read-only at "+leaf+"\n") {
		t.Errorf("the report does not name the mount point as it was mounted:\n%s", e.log())
	}
}

// TestMountRefusesASymlinkedMountPoint is the mount point's half of the rule
// the staging directories and the restore destination already follow. mount(8)
// run as root resolves the path and mounts on the link's target, so a link left
// where root is about to mount puts the image over whatever the link names —
// /etc, say — and MkdirAll used to take the link for an existing directory.
//
// Every spelling is one the kernel resolves THROUGH the link before lstat sees
// it unless it is stripped first; "link/." and "link/./" are the two a strip of
// trailing slashes alone missed, which in brb.sh would have let them through to
// mount over the target while Go refused them. xcompat-test.sh runs the same
// spellings through both readers.
//
// The refusal must come before the image is decrypted, and before mount(8):
// nothing may be written into the restore directory or the link's target.
func TestMountRefusesASymlinkedMountPoint(t *testing.T) {
	argvFile := stubMount(t)
	e := newEnv(t)
	e.writeImage(1, bytes.Repeat([]byte("pretend squashfs\n"), 512))
	victim := filepath.Join(e.dir, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.dir, "browse")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	for _, spelling := range []string{"", "/", "//", "/.", "/./"} {
		err := Mount(context.Background(), e.opts(), 1, link+spelling)
		if err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Errorf("Mount at %q = %v, want a refusal naming the symlink", "browse"+spelling, err)
		}
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Error("mount(8) was run over a symlinked mount point")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")); err == nil {
		t.Error("the image was decrypted for a mount that was going to be refused")
	}
	if ents, _ := os.ReadDir(victim); len(ents) != 0 {
		t.Errorf("the link's target was written into: %v", ents)
	}
}

// TestMountRefusesBeforeTouchingStaging pins the order of Mount's refusals to
// cmd_mount's — the arguments, the symlinked mount point, root, and only then
// staging — so that the two readers refuse a run for the same reason. Mount
// used to take the staging lock first, which creates and secures STAGING: a
// symlinked mount point under a STAGING that was itself a link was reported as
// the staging link by Go and as the mount point by brb.sh, a non-root run
// against the default root-owned /var/tmp/brb was refused over staging by one
// reader and for want of root by the other, and a run on its way to a refusal
// had already made a staging tree.
func TestMountRefusesBeforeTouchingStaging(t *testing.T) {
	stubMount(t)
	ctx := context.Background()
	e := newEnv(t)
	victim := filepath.Join(e.dir, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.dir, "browse")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	// A STAGING that lockStaging refuses, because it is a link, does not get
	// to decide which refusal a symlinked mount point gets.
	real := filepath.Join(e.dir, "real-staging")
	if err := os.Rename(e.cfg.Staging, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, e.cfg.Staging); err != nil {
		t.Fatal(err)
	}
	if err := Mount(ctx, e.opts(), 1, link); err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Errorf("Mount at a symlinked mount point, under a symlinked STAGING = %v, want the mount point refused", err)
	}

	// A STAGING that does not exist yet still does not exist after a refusal,
	// whether the refusal is the mount point's or root's.
	e.cfg.Staging = filepath.Join(e.dir, "fresh-staging")
	if err := Mount(ctx, e.opts(), 1, link); err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Errorf("Mount at a symlinked mount point = %v, want the mount point refused", err)
	}
	geteuid = func() int { return 1000 } // stubMount's cleanup puts it back
	mp := filepath.Join(e.dir, "mp")
	if err := Mount(ctx, e.opts(), 1, mp); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Errorf("Mount as a non-root user = %v, want the root refusal", err)
	}
	if _, err := os.Lstat(e.cfg.Staging); err == nil {
		t.Error("a STAGING was created for runs that were refused before needing one")
	}
	if _, err := os.Lstat(mp); err == nil {
		t.Error("the mount point was created for a run refused for want of root")
	}
}

// TestMountLeavesDotDotToTheKernel is why the mount point goes through dirPath
// and not filepath.Clean. With a/link pointing at ../v, the kernel resolves
// "a/link/../b" through the link to the b beside v, and so do mkdir, mount(8)
// and brb.sh; Clean would collapse it to "a/b" and mount somewhere neither the
// other reader nor this one before the check existed would.
func TestMountLeavesDotDotToTheKernel(t *testing.T) {
	argvFile := stubMount(t)
	e := newEnv(t)
	e.writeImage(1, bytes.Repeat([]byte("pretend squashfs\n"), 512))
	for _, d := range []string{"a", "v"} {
		if err := os.Mkdir(filepath.Join(e.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../v", filepath.Join(e.dir, "a", "link")); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(e.dir, "a") + "/link/../b"

	if err := Mount(context.Background(), e.opts(), 1, mp); err != nil {
		t.Fatalf("Mount: %v\n%s", err, e.log())
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("mount(8) was never run: %v", err)
	}
	argv := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if got := argv[len(argv)-1]; got != mp {
		t.Errorf("mount(8) was handed %q, want %q exactly as given", got, mp)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "b")); err != nil {
		t.Errorf("the directory the kernel names was not the one created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "a", "b")); err == nil {
		t.Error("a/b was created: the path was collapsed lexically, as brb.sh does not")
	}
}

// TestDirPathIsBrbShDirPath pins dirPath to brb.sh's dir_path, rule for rule.
// The two hand mount(8) the mount point after the symlink check, so any input
// they spell differently is a disc mounted in two different places.
//
// The ".." row is the reason this is not filepath.Clean: Clean would make it
// "a/b", and the kernel, mount(8) and brb.sh all resolve the link first. The
// newline rows are bytes like any other: a trailing newline is part of the
// name, and nothing strips it — brb.sh's caller used to, by way of $(...).
func TestDirPathIsBrbShDirPath(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":              "",
		"/":             "/",
		"//":            "/",
		"/.":            "/",
		"/./":           "/",
		".":             ".",
		"./":            ".",
		"mnt":           "mnt",
		"mnt/":          "mnt",
		"mnt//":         "mnt",
		"mnt/.":         "mnt",
		"mnt/./":        "mnt",
		"mnt/././/./":   "mnt",
		"/mnt/browse/":  "/mnt/browse",
		"a//b/./c":      "a//b/./c",
		"a/link/../b":   "a/link/../b",
		"a/link/../b/.": "a/link/../b",
		"a/..":          "a/..",
		"-mp/":          "-mp",
		"mnt\n":         "mnt\n",
		"mnt/\n":        "mnt/\n",
	} {
		if got := dirPath(in); got != want {
			t.Errorf("dirPath(%q) = %q, want %q", in, got, want)
		}
	}
}
