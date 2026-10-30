package restore

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A restore into a directory that already holds files must ask first, because
// unsquashfs -f replaces them with the backup's versions. brb.sh refuses in
// exactly this shape and this is the reader-parity assertion for it.
func TestConfirmNonEmptyDestRefusesWithoutAYes(t *testing.T) {
	e := newEnv(t)
	dest := filepath.Join(e.dir, "live")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "top.txt"), []byte("MY-CURRENT-WORK"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.ui.SetInput(strings.NewReader("n\n"))
	err := e.opts().confirmNonEmptyDest(dest)
	if err == nil || !strings.Contains(err.Error(), "restore into an empty directory and merge by hand") {
		t.Fatalf("confirmNonEmptyDest = %v, want brb.sh's refusal", err)
	}
	for _, want := range []string{"is not empty", "OVERWRITE", "existing entries: top.txt"} {
		if !strings.Contains(e.log(), want) {
			t.Errorf("log does not contain %q:\n%s", want, e.log())
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "top.txt")); err != nil {
		t.Fatalf("the refusal removed the live file: %v", err)
	}
}

// An empty destination is the ordinary case and must not ask anything, and
// --yes proceeds — both the way brb.sh behaves.
func TestConfirmNonEmptyDestPassesWhenEmptyOrForced(t *testing.T) {
	e := newEnv(t)
	dest := filepath.Join(e.dir, "empty")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.opts().confirmNonEmptyDest(dest); err != nil {
		t.Fatalf("an empty destination was refused: %v", err)
	}
	if strings.Contains(e.log(), "is not empty") {
		t.Fatalf("an empty destination warned:\n%s", e.log())
	}

	if err := os.WriteFile(filepath.Join(dest, "top.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ui.SetAssumeYes(true)
	if err := e.opts().confirmNonEmptyDest(dest); err != nil {
		t.Fatalf("--yes was refused: %v", err)
	}
}

// unsquashfs -f writes through a symlinked directory already in the
// destination, putting the archive's files outside it. The refusal has to reach
// every depth: a top-level-only check leaves "dest/a/b -> /elsewhere" open.
func TestRefuseSymlinkedDirsAtEveryDepth(t *testing.T) {
	e := newEnv(t)
	victim := filepath.Join(e.dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, link string }{
		{"top level", "sub"},
		{"depth 2", filepath.Join("a", "b")},
		{"depth 3", filepath.Join("a", "b2", "c")},
	} {
		dest := filepath.Join(e.dir, "dest-"+strings.ReplaceAll(tc.name, " ", "-"))
		link := filepath.Join(dest, tc.link)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		err := refuseSymlinkedDirs(dest)
		if err == nil || !strings.Contains(err.Error(), "OUTSIDE the destination") {
			t.Errorf("%s: refuseSymlinkedDirs = %v, want a refusal naming the escape", tc.name, err)
		}
		if err != nil && !strings.Contains(err.Error(), victim) {
			t.Errorf("%s: the refusal does not name the target: %v", tc.name, err)
		}
	}
}

// A symlink to a file is not an escape: unsquashfs unlinks the entry and
// replaces it, so refusing over one would block ordinary re-restores.
func TestRefuseSymlinkedDirsAllowsFileSymlinks(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(e.dir, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(e.dir, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dest, "top.txt")); err != nil {
		t.Fatal(err)
	}
	// A dangling link resolves to nothing and is equally not a traversal.
	if err := os.Symlink(filepath.Join(e.dir, "gone"), filepath.Join(dest, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := refuseSymlinkedDirs(dest); err != nil {
		t.Fatalf("refuseSymlinkedDirs = %v, want nil for file symlinks", err)
	}
}

// The identity search order is brb.sh's find_identity, byte for byte: the
// configured identity, its age container, then the rescue key. Both readers
// must pick the same file or a set restores under one and not the other.
func TestIdentityCandidatesMatchFindIdentity(t *testing.T) {
	e := newEnv(t)
	keydir := filepath.Dir(e.cfg.AgeRecipientsFile)

	got := identityCandidates(e.cfg)
	want := []string{
		e.cfg.AgeIdentity,
		e.cfg.AgeIdentity + ageExt,
		filepath.Join(keydir, "rescue-identity.txt"+ageExt),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("identityCandidates(configured) = %q, want %q", got, want)
	}

	unset := *e.cfg
	unset.AgeIdentity = ""
	got = identityCandidates(&unset)
	want = []string{
		filepath.Join(keydir, "identity.txt"),
		filepath.Join(keydir, "identity.txt"+ageExt),
		filepath.Join(keydir, "rescue-identity.txt"+ageExt),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("identityCandidates(default) = %q, want %q", got, want)
	}
}

// findIdentity takes the first candidate that is actually there, so a set whose
// plaintext identity was shredded still restores from the rescue key.
func TestFindIdentityFallsBackToTheRescueKey(t *testing.T) {
	e := newEnv(t)
	keydir := filepath.Dir(e.cfg.AgeRecipientsFile)
	rescue := filepath.Join(keydir, "rescue-identity.txt"+ageExt)
	if err := os.WriteFile(rescue, []byte("age-encryption.org/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, ok := findIdentity(e.cfg); !ok || got != e.cfg.AgeIdentity {
		t.Fatalf("findIdentity = %q, %v; want the plaintext identity", got, ok)
	}
	if err := os.Remove(e.cfg.AgeIdentity); err != nil {
		t.Fatal(err)
	}
	got, ok := findIdentity(e.cfg)
	if !ok || got != rescue {
		t.Fatalf("findIdentity = %q, %v; want the rescue key %q", got, ok, rescue)
	}
	enc, err := identityIsEncrypted(rescue)
	if err != nil || !enc {
		t.Fatalf("identityIsEncrypted(%s) = %v, %v; want true", rescue, enc, err)
	}
}

// fifoReleasedAtCleanup makes a named pipe at path and arranges for any reader
// still blocked opening it to be released when the test ends, so that a
// regression that opens it hangs one goroutine for the length of the test and
// no longer.
func fifoReleasedAtCleanup(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot make a FIFO here: %v", err)
	}
	t.Cleanup(func() {
		// A writer's open completes a blocked reader's; with no reader
		// waiting, O_NONBLOCK makes it fail at once instead of hanging.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
}

// findIdentityOrTimeout runs findIdentity with a deadline: a candidate that
// blocks in open is exactly the failure some of these tests look for.
func findIdentityOrTimeout(t *testing.T, e *env) (string, bool) {
	t.Helper()
	type result struct {
		path string
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		p, ok := findIdentity(e.cfg)
		ch <- result{p, ok}
	}()
	select {
	case r := <-ch:
		return r.path, r.ok
	case <-time.After(5 * time.Second):
		t.Fatalf("findIdentity has not returned after 5s; a candidate blocked it")
		return "", false
	}
}

// findIdentity takes only a regular file, as brb.sh's `[[ -f && -r ]]` does.
// An AGE_IDENTITY naming a directory, a FIFO or a device used to end the
// search there — "is a directory", a hang in open, or a sniff of /dev/zero
// that runs out of memory — while brb.sh skipped it for <AGE_IDENTITY>.age or
// the rescue key, so the same set restored under one reader and not the other.
// Both fallbacks are checked for each kind.
func TestFindIdentitySkipsAnIdentityThatIsNotARegularFile(t *testing.T) {
	kinds := map[string]func(t *testing.T, path string){
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": fifoReleasedAtCleanup,
	}
	for kind, mk := range kinds {
		for _, fallback := range []string{"container", "rescue"} {
			t.Run(kind+" falls back to the "+fallback, func(t *testing.T) {
				e := newEnv(t)
				keydir := filepath.Dir(e.cfg.AgeRecipientsFile)
				e.cfg.AgeIdentity = filepath.Join(keydir, "odd-identity")
				mk(t, e.cfg.AgeIdentity)
				want := e.cfg.AgeIdentity + ageExt
				if fallback == "rescue" {
					want = filepath.Join(keydir, "rescue-identity.txt"+ageExt)
				}
				if err := os.WriteFile(want, []byte("age-encryption.org/v1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if got, ok := findIdentityOrTimeout(t, e); !ok || got != want {
					t.Fatalf("findIdentity = %q, %v; want %q", got, ok, want)
				}
			})
		}
	}

	// A character device, the case that ran the sniff out of memory. /dev/null
	// stands in for /dev/zero so a regression fails rather than allocates.
	if fi, err := os.Stat(os.DevNull); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		e := newEnv(t)
		keydir := filepath.Dir(e.cfg.AgeRecipientsFile)
		e.cfg.AgeIdentity = os.DevNull
		rescue := filepath.Join(keydir, "rescue-identity.txt"+ageExt)
		if err := os.WriteFile(rescue, []byte("age-encryption.org/v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := findIdentityOrTimeout(t, e); !ok || got != rescue {
			t.Fatalf("findIdentity with AGE_IDENTITY=%s = %q, %v; want the rescue key %q", os.DevNull, got, ok, rescue)
		}
	}

	// And a symlink to a real identity still counts, since `-f` follows links.
	e := newEnv(t)
	link := filepath.Join(e.dir, "linked-identity.txt")
	if err := os.Symlink(e.cfg.AgeIdentity, link); err != nil {
		t.Fatal(err)
	}
	e.cfg.AgeIdentity = link
	if got, ok := findIdentityOrTimeout(t, e); !ok || got != link {
		t.Fatalf("findIdentity with a symlinked AGE_IDENTITY = %q, %v; want %q", got, ok, link)
	}
}

// The public archive's staged key counts only as a regular file, as brb.sh's
// `[[ -f "$pub" ]]` has it. Anything else at that name is ignored by both
// readers: the configured identity is used, and nothing hangs or fails.
func TestIdentitiesIgnoresAStagedKeyThatIsNotARegularFile(t *testing.T) {
	for kind, mk := range map[string]func(t *testing.T, path string){
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": fifoReleasedAtCleanup,
	} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			mk(t, e.opts().stagedPublicIdentity())
			type result struct {
				n   int
				err error
			}
			ch := make(chan result, 1)
			go func() {
				ids, err := e.opts().identities()
				ch <- result{len(ids), err}
			}()
			select {
			case r := <-ch:
				if r.err != nil || r.n != 1 {
					t.Fatalf("identities() = %d key(s), %v; want the configured identity alone", r.n, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("identities() has not returned after 5s; the staged key blocked it")
			}
		})
	}
}

// --yes promises an unattended run, so a passphrase-protected identity has to
// say so rather than block on a prompt nobody will answer.
func TestUnlockIdentityRefusesUnderAssumeYes(t *testing.T) {
	e := newEnv(t)
	e.ui.SetAssumeYes(true)
	path := filepath.Join(e.dir, "identity.txt"+ageExt)
	if _, err := e.opts().unlockIdentity(path); err == nil ||
		!strings.Contains(err.Error(), "nobody to type the passphrase") {
		t.Fatalf("unlockIdentity under --yes = %v, want a refusal", err)
	}
}

// A kill -9 mid-copy leaves an image-sized .part that nothing resumes from and
// no listing shows. brb.sh reaps these at ingest; so does this.
func TestReapPartFiles(t *testing.T) {
	e := newEnv(t)
	dir := e.cfg.Dirs().Enc
	keep := filepath.Join(dir, "disc01.squashfs"+ageExt)
	stale := filepath.Join(dir, "disc01.squashfs"+ageExt+partExt)
	for _, p := range []string{keep, stale} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.opts().reapPartFiles(dir)
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the stale %s survived", partExt)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("reaping removed the finished file: %v", err)
	}
	if !strings.Contains(e.log(), "removed the stale") {
		t.Errorf("the reap was silent:\n%s", e.log())
	}
}

// altCopies is what feeds par2 the second pressing of a damaged disc. It reads
// the directory rather than globbing, so a staging path with glob
// metacharacters in it cannot hide a copy par2 needs.
func TestAltCopiesFindsEveryPressing(t *testing.T) {
	dir := t.TempDir()
	name := "disc01.squashfs" + ageExt
	for _, nm := range []string{name, name + ".copy1", name + ".copy2", name + par2Ext, "disc02.squashfs.age.copy9"} {
		if err := os.WriteFile(filepath.Join(dir, nm), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := altCopies(dir, name)
	want := []string{name + ".copy1", name + ".copy2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("altCopies = %q, want %q", got, want)
	}
}
