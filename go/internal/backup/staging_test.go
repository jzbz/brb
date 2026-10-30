package backup

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
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

// layoutRunner is a stagingRunner able to lay out, document and checksum disc
// directories without the external tools: no par2 (protectSidecars warns and
// carries on), and a budget no test fixture can outgrow.
func layoutRunner(t *testing.T, staging string) *runner {
	t.Helper()
	r := stagingRunner(t, staging)
	r.tools = tools.NewSet(nil)
	r.budget = disc.Budget{Usable: 1 << 40}
	r.discCipher = map[int]string{}
	return r
}

// plantEncArtefacts puts into enc/ what discPayload requires of disc 1 — the
// ciphertext, both sidecars, the index and its sidecar, and one par2 file — as
// an earlier run would have left them. The ciphertext sidecars hold the real
// digests, because writeSums holds a disc this run did not encrypt to the
// digest its sidecar recorded.
func plantEncArtefacts(t *testing.T, r *runner) {
	t.Helper()
	base := imageName(1)
	bodies := map[string]string{
		base + ".age":      "ciphertext of disc 1",
		base + ".age.par2": "parity over it",
		indexName:          "the encrypted index",
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(r.dirs.Enc, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{base + ".age", indexName} {
		sum := sha512.Sum512([]byte(bodies[name]))
		if err := agecrypt.WriteSumFile(filepath.Join(r.dirs.Enc, name+".sha512"), hex.EncodeToString(sum[:]), name); err != nil {
			t.Fatal(err)
		}
	}
	plain := sha512.Sum512([]byte("plaintext of disc 1"))
	if err := agecrypt.WriteSumFile(filepath.Join(r.dirs.Enc, base+".sha512"), hex.EncodeToString(plain[:]), base); err != nil {
		t.Fatal(err)
	}
}

// writeCanary creates a file outside staging whose bytes must survive the
// run, and returns its path.
func writeCanary(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("PRECIOUS "+name), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertCanary(t *testing.T, p string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("canary %s: %v", p, err)
	}
	if want := "PRECIOUS " + filepath.Base(p); string(got) != want {
		t.Errorf("%s was written through a planted link: it now holds %d byte(s) beginning %q",
			p, len(got), firstLine(string(got)))
	}
}

func assertRegular(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Errorf("%s: %v", p, err)
		return
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("%s is %v, want a regular file this run wrote", p, fi.Mode().Type())
	}
}

// TestDiscLayoutDoesNotTrustInheritedEntries. makeDirs secures the staging
// root and its five subdirectories, and nothing below them; a staging tree is
// reused across a resume, so whatever an earlier period left inside it was
// taken as found. In a tree another account could once write — made g+w or
// o+w by hand, or adopted with chown -R — that was a way to have a backup,
// run as root as the README recommends, write wherever the planter chose, and
// to put files of the planter's choosing on the discs as though brb had.
func TestDiscLayoutDoesNotTrustInheritedEntries(t *testing.T) {
	ctx := context.Background()

	// Documents: a symlink at STAGING/MANIFEST.txt (the staging root is never
	// emptied), and symlinks at a disc's README.md and identity.txt. The
	// O_TRUNC writes the first two went through truncated the link's target
	// and filled it with manifest or README text. identity.txt was removed
	// before it was written, and is here so that it stays safe.
	//
	// Leftovers: plain files a disc directory held from before, at names this
	// run does not overwrite. With no dist payload, copySelf kept any file
	// already at brb-linux-<arch> — the planted one here, or an older brb an
	// interrupted run had copied — and a planted brb.sh, never written by a
	// payload-less run, stayed too. Both went into SHA512SUMS, and the README
	// offered them to a restorer as copies of the tool.
	t.Run("planted links and leftovers", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "staging")
		outside := filepath.Join(root, "outside")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		r := layoutRunner(t, staging)
		dd := filepath.Join(r.dirs.Discs, discDirName(1))
		if err := os.MkdirAll(filepath.Join(dd, "data"), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dd, 0o777); err != nil {
			t.Fatal(err)
		}
		manifestCanary := writeCanary(t, outside, "manifest-canary")
		readmeCanary := writeCanary(t, outside, "readme-canary")
		keyCanary := writeCanary(t, outside, "key-canary")
		for link, target := range map[string]string{
			filepath.Join(staging, "MANIFEST.txt"): manifestCanary,
			filepath.Join(dd, "README.md"):         readmeCanary,
			filepath.Join(dd, "identity.txt"):      keyCanary,
		} {
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range map[string]string{
			"brb.sh":                              "#!/bin/sh\necho planted\n",
			SelfCopyName():                        "OLD BRB BINARY",
			filepath.Join("data", "extra.sha512"): "planted sidecar",
		} {
			if err := os.WriteFile(filepath.Join(dd, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		if err := r.makeDirs(); err != nil {
			t.Fatalf("makeDirs: %v", err)
		}
		plantEncArtefacts(t, r)
		// A public archive, so identity.txt is one of the documents written.
		id, err := agecrypt.GenerateIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if err := agecrypt.WriteIdentityFile(r.publicIdentityPath(), id); err != nil {
			t.Fatal(err)
		}
		r.publicIdentity = id

		if err := r.buildDiscDirs(ctx, 1); err != nil {
			t.Fatalf("buildDiscDirs: %v", err)
		}
		if err := r.writeManifest(ctx, 1); err != nil {
			t.Fatalf("writeManifest: %v", err)
		}
		if err := r.writeReadmes(ctx, 1); err != nil {
			t.Fatalf("writeReadmes: %v", err)
		}
		if err := r.writeSums(ctx, 1); err != nil {
			t.Fatalf("writeSums: %v", err)
		}

		for _, c := range []string{manifestCanary, readmeCanary, keyCanary} {
			assertCanary(t, c)
		}
		for _, p := range []string{
			filepath.Join(staging, "MANIFEST.txt"),
			filepath.Join(dd, "MANIFEST.txt"),
			filepath.Join(dd, "README.md"),
			filepath.Join(dd, "identity.txt"),
		} {
			assertRegular(t, p)
		}
		if fi, err := os.Stat(filepath.Join(dd, "identity.txt")); err == nil && fi.Mode().Perm() != 0o644 {
			t.Errorf("identity.txt has mode %04o, want 0644", fi.Mode().Perm())
		}

		for _, gone := range []string{"brb.sh", filepath.Join("data", "extra.sha512")} {
			if _, err := os.Lstat(filepath.Join(dd, gone)); err == nil {
				t.Errorf("the planted %s is still on the disc", gone)
			}
		}
		self, err := os.ReadFile(filepath.Join(dd, SelfCopyName()))
		if err != nil {
			t.Fatal(err)
		}
		if string(self) == "OLD BRB BINARY" {
			t.Errorf("the disc still carries the %s it held before this run, not the running program",
				SelfCopyName())
		}
		sums := readFileString(t, filepath.Join(dd, agecrypt.SumsName))
		for _, name := range []string{"./brb.sh", "./data/extra.sha512"} {
			if strings.Contains(sums, "  "+name+"\n") {
				t.Errorf("%s vouches for the planted %s", agecrypt.SumsName, name)
			}
		}
		if readme := readFileString(t, filepath.Join(dd, "README.md")); strings.Contains(readme, "brb.sh") {
			t.Error("the README offers a restorer the planted brb.sh")
		}

		// And the directories are staging directories now, not whatever mode
		// they were inherited with.
		for _, d := range []string{dd, filepath.Join(dd, "data")} {
			fi, err := os.Lstat(d)
			if err != nil {
				t.Errorf("%s: %v", d, err)
			} else if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Errorf("%s is %v, want a directory of mode 0700", d, fi.Mode())
			}
		}
	})

	// A symlink at the disc directory itself sent every artefact of that disc,
	// the documents and a copy of this program outside staging. It is refused,
	// and the link is left for the operator to see rather than quietly removed.
	t.Run("a symlinked disc directory", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "staging")
		outside := filepath.Join(root, "outside")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		r := layoutRunner(t, staging)
		if err := r.makeDirs(); err != nil {
			t.Fatalf("makeDirs: %v", err)
		}
		dd := filepath.Join(r.dirs.Discs, discDirName(1))
		if err := os.Symlink(outside, dd); err != nil {
			t.Fatal(err)
		}
		plantEncArtefacts(t, r)

		err := r.buildDiscDirs(ctx, 1)
		if err == nil || !strings.Contains(err.Error(), "is a symlink") || !strings.Contains(err.Error(), dd) {
			t.Fatalf("buildDiscDirs = %v, want a refusal naming the symlink %s", err, dd)
		}
		if ents, _ := os.ReadDir(outside); len(ents) != 0 {
			t.Errorf("buildDiscDirs wrote %d entr(ies) through the symlink into %s", len(ents), outside)
		}
		if fi, err := os.Lstat(dd); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the refused link is gone (Lstat: %v, %v); it should be left for the operator", fi, err)
		}
	})

	// A hard link at the manifest's name shares its inode with a file
	// elsewhere, so a truncating write rewrote that file's contents too. Only
	// a link to a file the planter could already write survives
	// fs.protected_hardlinks, but that is still a file the victim did not mean
	// to touch.
	t.Run("a hard link at the manifest", func(t *testing.T) {
		root := t.TempDir()
		staging := filepath.Join(root, "staging")
		r := layoutRunner(t, staging)
		if err := r.makeDirs(); err != nil {
			t.Fatalf("makeDirs: %v", err)
		}
		canary := writeCanary(t, root, "hardlink-canary")
		if err := os.Link(canary, filepath.Join(staging, "MANIFEST.txt")); err != nil {
			t.Skipf("cannot make a hard link here: %v", err)
		}
		plantEncArtefacts(t, r)
		if err := r.buildDiscDirs(ctx, 1); err != nil {
			t.Fatalf("buildDiscDirs: %v", err)
		}
		if err := r.writeManifest(ctx, 1); err != nil {
			t.Fatalf("writeManifest: %v", err)
		}
		assertCanary(t, canary)
		if !strings.HasPrefix(readFileString(t, filepath.Join(staging, "MANIFEST.txt")), "brb (Blu-ray Backup) manifest") {
			t.Error("STAGING/MANIFEST.txt does not hold the manifest")
		}
	})
}

// TestBuildDiscDirsKeepsTheDiscCopyWhenEncIsShort. The disc directory is
// emptied only once discPayload has found everything it needs in enc/. On a
// staging tree that spans filesystems the disc directory's artefacts are
// copies rather than links, so a resume that found one missing from enc/ and
// emptied the disc directory first would delete what may be the last copy,
// and then fail anyway.
func TestBuildDiscDirsKeepsTheDiscCopyWhenEncIsShort(t *testing.T) {
	r := layoutRunner(t, filepath.Join(t.TempDir(), "staging"))
	if err := r.makeDirs(); err != nil {
		t.Fatal(err)
	}
	plantEncArtefacts(t, r)
	if err := r.buildDiscDirs(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	img := imageName(1) + ".age"
	discCopy := filepath.Join(r.dirs.Discs, discDirName(1), "data", img)
	if err := os.Remove(filepath.Join(r.dirs.Enc, img)); err != nil {
		t.Fatal(err)
	}
	err := r.buildDiscDirs(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("buildDiscDirs with %s gone from enc/ = %v, want the missing-artefact refusal", img, err)
	}
	if _, err := os.Stat(discCopy); err != nil {
		t.Errorf("the refusal took the disc directory's copy with it: %v", err)
	}
}

// TestBuildDiscDirsRefusesAMountedDiscDirectory. Emptying a disc directory is
// a RemoveAll, and RemoveAll walks into a filesystem mounted at the disc
// directory or below it: it deleted everything on that volume and only then
// failed, EBUSY, on the mount point. A mount there is refused before anything
// is removed, as a symlink there is.
//
// Mounting a filesystem takes a privilege an ordinary test run does not have,
// so this stands a mount in for one directory through deviceOf (another
// filesystem) or mountPointsUnder (a bind mount from the same one), and
// everything else about the run is real. The mounts themselves are exercised,
// where the system allows it, by TestFreshDiscDirOverARealMount. Not
// parallel: deviceOf and mountPointsUnder are package state.
func TestBuildDiscDirsRefusesAMountedDiscDirectory(t *testing.T) {
	ctx := context.Background()
	// otherDevice makes every directory for which on reports true read as
	// being on a device of its own, and leaves the rest on their real one.
	otherDevice := func(t *testing.T, on func(p string) bool) {
		t.Helper()
		was := deviceOf
		t.Cleanup(func() { deviceOf = was })
		deviceOf = func(p string, fi os.FileInfo) (uint64, bool) {
			dev, ok := was(p, fi)
			if ok && on(p) {
				dev++
			}
			return dev, ok
		}
	}
	// mountedAt adds mp to the kernel's list of mount points and leaves its
	// device alone, which is what a bind mount from the same filesystem
	// looks like.
	mountedAt := func(t *testing.T, mp string) {
		t.Helper()
		was := mountPointsUnder
		t.Cleanup(func() { mountPointsUnder = was })
		mountPointsUnder = func(dir string) ([]string, bool, error) {
			mps, ok, err := was(dir)
			if mp == dir || strings.HasPrefix(mp, dir+string(os.PathSeparator)) {
				mps = append(mps, mp)
			}
			return mps, ok, err
		}
	}
	// laidOut is a staging tree whose disc 1 has been laid out once, with a
	// file of the operator's added to the disc directory and to its data/.
	laidOut := func(t *testing.T) (r *runner, dd string, canaries []string) {
		t.Helper()
		r = layoutRunner(t, filepath.Join(t.TempDir(), "staging"))
		if err := r.makeDirs(); err != nil {
			t.Fatal(err)
		}
		plantEncArtefacts(t, r)
		if err := r.buildDiscDirs(ctx, 1); err != nil {
			t.Fatalf("buildDiscDirs: %v", err)
		}
		dd = filepath.Join(r.dirs.Discs, discDirName(1))
		return r, dd, []string{
			writeCanary(t, dd, "on-the-disc-volume"),
			writeCanary(t, filepath.Join(dd, "data"), "on-the-data-volume"),
		}
	}

	for _, tc := range []struct{ name, below string }{
		{"a filesystem mounted at the disc directory", ""},
		{"a filesystem mounted at its data/", "data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, dd, canaries := laidOut(t)
			mp := filepath.Join(dd, tc.below)
			otherDevice(t, func(p string) bool { return p == mp || strings.HasPrefix(p, mp+string(os.PathSeparator)) })

			err := r.buildDiscDirs(ctx, 1)
			if err == nil || !strings.Contains(err.Error(), "different filesystem") || !strings.Contains(err.Error(), mp) {
				t.Fatalf("buildDiscDirs = %v, want a refusal naming the mount point %s", err, mp)
			}
			for _, c := range canaries {
				assertCanary(t, c)
			}
		})
	}

	// A bind mount from the filesystem discs/ is on has the device discs/ has,
	// so only the kernel's list of mount points shows it.
	for _, tc := range []struct{ name, below string }{
		{"a same-filesystem bind mount at the disc directory", ""},
		{"a same-filesystem bind mount at its data/", "data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, dd, canaries := laidOut(t)
			mp := filepath.Join(dd, tc.below)
			mountedAt(t, mp)

			err := r.buildDiscDirs(ctx, 1)
			if err == nil || !strings.Contains(err.Error(), mp+" is a mount point") {
				t.Fatalf("buildDiscDirs = %v, want a refusal naming the mount point %s", err, mp)
			}
			for _, c := range canaries {
				assertCanary(t, c)
			}
		})
	}

	// The way to give the discs a volume of their own is to mount it at
	// discs/ as a whole, and that must keep working: everything below it is
	// then on the one device, and discs/ is the only mount root.
	t.Run("discs/ itself on another filesystem", func(t *testing.T) {
		r, dd, canaries := laidOut(t)
		discs := r.dirs.Discs
		otherDevice(t, func(p string) bool { return p == discs || strings.HasPrefix(p, discs+string(os.PathSeparator)) })
		mountedAt(t, discs)

		if err := r.buildDiscDirs(ctx, 1); err != nil {
			t.Fatalf("buildDiscDirs: %v", err)
		}
		for _, c := range canaries {
			if _, err := os.Lstat(c); err == nil {
				t.Errorf("%s survived; the disc directory was not emptied", c)
			}
		}
		assertRegular(t, filepath.Join(dd, "data", imageName(1)+".age"))
	})
}
