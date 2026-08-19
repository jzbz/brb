package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/fsx"
)

// stagingRunner is a runner with nothing set but the staging paths, which is
// all makeDirs and the index writer need.
func stagingRunner(t *testing.T, staging string) *runner {
	t.Helper()
	cfg := config.Default()
	cfg.Staging = staging
	return &runner{cfg: cfg, dirs: cfg.Dirs(), p: quiet()}
}

// STAGING defaults to a path under /var/tmp, which is world-writable, so a
// symlink planted at it — or at any of the directories under it — before the
// run would send every plaintext squashfs image wherever the planter chose.
// The reader has refused this since it grew secureDir; the writer accepted it
// and merely chmod'ed whatever the link pointed at.
func TestMakeDirsRefusesASymlinkedStagingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(d config.Dirs, staging string) string
	}{
		{"STAGING itself", func(_ config.Dirs, s string) string { return s }},
		{"the work directory", func(d config.Dirs, _ string) string { return d.Work }},
		{"the img directory", func(d config.Dirs, _ string) string { return d.Img }},
		{"the enc directory", func(d config.Dirs, _ string) string { return d.Enc }},
		{"the discs directory", func(d config.Dirs, _ string) string { return d.Discs }},
		{"the iso directory", func(d config.Dirs, _ string) string { return d.ISO }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			staging := filepath.Join(root, "staging")
			elsewhere := filepath.Join(root, "elsewhere")
			if err := os.Mkdir(elsewhere, 0o700); err != nil {
				t.Fatal(err)
			}
			r := stagingRunner(t, staging)
			link := tc.link(r.dirs, staging)
			// Everything above the link has to exist for it to be planted.
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}

			err := r.makeDirs()
			if err == nil || !strings.Contains(err.Error(), "is a symlink") ||
				!strings.Contains(err.Error(), link) {
				t.Fatalf("makeDirs = %v, want a refusal naming the symlink %s", err, link)
			}
			if !strings.HasPrefix(err.Error(), "backup: ") {
				t.Errorf("the refusal lost its command prefix: %v", err)
			}
			// And nothing was created on the far side of the link.
			ents, _ := os.ReadDir(elsewhere)
			if len(ents) != 0 {
				t.Errorf("makeDirs built %d entr(ies) through the symlink, in %s", len(ents), elsewhere)
			}
		})
	}
}

// A staging tree the operator made by hand under a 022 umask must end up 0700
// throughout, not only at the root: img/ holds plaintext images and work/
// holds the resume state that decides what a continued run skips.
func TestMakeDirsTightensEveryDirectory(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	r := stagingRunner(t, staging)
	for _, d := range []string{staging, r.dirs.Work, r.dirs.Img, r.dirs.Enc, r.dirs.Discs, r.dirs.ISO} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.makeDirs(); err != nil {
		t.Fatalf("makeDirs: %v", err)
	}
	for _, d := range []string{staging, r.dirs.Work, r.dirs.Img, r.dirs.Enc, r.dirs.Discs, r.dirs.ISO} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is %o, want 0700", d, fi.Mode().Perm())
		}
	}
}

// The whole point of the ownership rule on the writer's side: README.md
// recommends running the backup as root, and as root the chmod succeeds on
// anybody's directory, so ownership is the only check left that can fire. A
// root run that finds STAGING owned by another account must refuse rather than
// fill it with plaintext.
//
// Only root can build the fixture, so the test runs only as root — the same
// gate the reader's equivalent uses.
func TestMakeDirsRefusesStagingOwnedBySomeoneElse(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: only root can create a directory owned by another uid")
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	const nobody = 65534
	if err := os.Chown(staging, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	r := stagingRunner(t, staging)
	err := r.makeDirs()
	if err == nil || !strings.Contains(err.Error(), "is owned by uid 65534") {
		t.Fatalf("makeDirs = %v, want an ownership refusal", err)
	}
	if !strings.Contains(err.Error(), "chown") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
	// Refused, and nothing built inside it.
	if ents, _ := os.ReadDir(staging); len(ents) != 0 {
		t.Errorf("makeDirs built %d entr(ies) in a directory another account owns", len(ents))
	}
}

// The legitimate root case still works: a root run over a root-owned staging
// tree is the README's recommended configuration and must not be refused.
func TestMakeDirsAcceptsItsOwnStaging(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	r := stagingRunner(t, staging)
	if err := r.makeDirs(); err != nil {
		t.Fatalf("makeDirs over a directory of our own: %v", err)
	}
	// And again, over the tree it just made: staging is reused across a
	// resumed run, so the second pass must be as quiet as the first.
	if err := r.makeDirs(); err != nil {
		t.Fatalf("makeDirs over the tree it just built: %v", err)
	}
}

// createPart writes every image, ciphertext and copied file this program
// produces. A symlink planted at the .part path must not receive them: the
// bytes land under the intended name and the link's target is untouched.
// O_TRUNC, which this used to use, wrote straight through it.
func TestCreatePartDoesNotWriteThroughAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "disc01.squashfs")
	if err := os.Symlink(victim, dst+".part"); err != nil {
		t.Fatal(err)
	}

	f, finish, err := createPart(dst)
	if err != nil {
		t.Fatalf("createPart: %v", err)
	}
	if _, err := f.WriteString("plaintext image"); err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "theirs" {
		t.Fatalf("the planted symlink was followed: victim now holds %q", got)
	}
	if got, _ := os.ReadFile(dst); string(got) != "plaintext image" {
		t.Fatalf("%s holds %q, want the written bytes", dst, got)
	}
	if fi, err := os.Lstat(dst); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("Lstat(%s) = %v, %v; want a real file", dst, fi, err)
	}
}

// The index is the one staging file that is appended to rather than written
// once, so it cannot use the remove-then-O_EXCL of everything else. It must
// still refuse a planted symlink — the index is what a resume reconciles its
// state against, and what "brb index" prints back to the operator.
func TestAppendIndexDoesNotWriteThroughAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, indexFileName)
	if err := os.Symlink(victim, index); err != nil {
		t.Fatal(err)
	}
	err := appendIndex(index, 1, []string{"a.txt"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("appendIndex = %v, want a refusal naming the symlink", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "theirs" {
		t.Fatalf("the index was appended through the symlink: victim holds %q", got)
	}
}

// (That it still accumulates across discs — the reason it cannot use
// fsx.CreateFresh — is pinned by TestAppendIndexAccumulatesAcrossDiscs in
// index_test.go.)

// TestBackupRefusesToShareStagingWithAnotherRun. Every earlier guard against a
// second run caught one that had already finished; this is the one that
// notices a run in progress. The case it is really for is `burn` mastering an
// ISO from a disc directory a backup has not finished writing.
func TestBackupRefusesToShareStagingWithAnotherRun(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	cfg := integrationConfig(t, 2, 1<<20)
	keysFor(t, cfg)

	// Stand in for the run already under way: hold its lock, having secured
	// the tree exactly as its preflight would.
	if err := fsx.SecureStaging(cfg.Staging); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.LockStaging(cfg.Staging)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	err = Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set})
	if err == nil {
		t.Fatal("a second backup into the same staging directory was accepted")
	}
	if !strings.Contains(err.Error(), "another brb is using") {
		t.Fatalf("Run = %v, want the staging-in-use refusal", err)
	}

	// And the lock is not a one-way door: once the first run is done, the next
	// one proceeds. Without this the test would pass just as well against a
	// build that refused every backup.
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	free, err := fsx.LockStaging(cfg.Staging)
	if err != nil {
		t.Fatalf("staging stayed locked after the holder released: %v", err)
	}
	free.Release()
}

// TestAFailedPreflightReleasesTheStagingLock. preflight takes the lock partway
// through and can still refuse the run afterwards. Releasing only on the
// success path leaked it for the life of the process — invisible in a one-shot
// CLI, and fatal to any second Run in the same process, which is what every
// resume test here is. Caught by the suite, not by the unit tests, so it is
// pinned here.
func TestAFailedPreflightReleasesTheStagingLock(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	cfg := integrationConfig(t, 2, 1<<20)
	keysFor(t, cfg)

	// A refusal that happens AFTER makeDirs has locked: --resume with no state
	// to resume from. Any late preflight refusal would do.
	err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set, Resume: true})
	if err == nil {
		t.Fatal("--resume with no state was accepted")
	}
	if strings.Contains(err.Error(), "another brb is using") {
		t.Fatalf("the refusal was the lock itself, not the expected one: %v", err)
	}

	// The lock must be free again: nothing is holding it, so taking it here
	// must succeed. Before the fix this failed, and so did every later Run.
	lock, err := fsx.LockStaging(cfg.Staging)
	if err != nil {
		t.Fatalf("the failed preflight leaked the staging lock: %v", err)
	}
	lock.Release()
}
