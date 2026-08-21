package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The staging directory's default lives under /var/tmp, which every local
// account can write. A symlink planted at STAGING, or at any subdirectory of
// it, would send every plaintext image written into it wherever the planter
// chose, so each of them is refused before anything is written.
func TestSecureDirRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "staging")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	err := SecureDir(link)
	if err == nil || !strings.Contains(err.Error(), "is a symlink") || !strings.Contains(err.Error(), link) {
		t.Fatalf("SecureDir = %v, want a refusal naming the symlink %s", err, link)
	}
	if !strings.Contains(err.Error(), elsewhere) {
		t.Errorf("the refusal does not say where the link points: %v", err)
	}

	// A dangling link is the same hazard: the open that follows it creates the
	// target. Lstat sees the link either way.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nothing-here"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := SecureDir(dangling); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("SecureDir(dangling link) = %v, want the symlink refusal", err)
	}
}

// A missing directory is created 0700, and one the operator made by hand under
// a loose umask is tightened to 0700 rather than left as it was found.
func TestSecureDirCreatesAndTightens(t *testing.T) {
	root := t.TempDir()

	fresh := filepath.Join(root, "fresh")
	if err := SecureDir(fresh); err != nil {
		t.Fatalf("SecureDir: %v", err)
	}
	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SecureDir(loose); err != nil {
		t.Fatalf("SecureDir: %v", err)
	}
	for _, d := range []string{fresh, loose} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is %o, want 0700", d, fi.Mode().Perm())
		}
	}
}

// A file where a staging directory should be is refused with a message that
// says so, rather than being written into as if it were one.
func TestSecureDirRefusesAFileInTheWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SecureDir(path)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("SecureDir over a file = %v, want a refusal", err)
	}
}

// SecureStaging secures the root before the subdirectories, and the order is
// load-bearing: a symlinked root would make every check below it a check on
// the link's target. The subdirectory here is named through the link, so if
// the root were not checked first the whole call would pass and the planter
// would own the tree.
func TestSecureStagingChecksTheRootFirst(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "staging")
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	err := SecureStaging(root, filepath.Join(root, "img"), filepath.Join(root, "enc"))
	if err == nil || !strings.Contains(err.Error(), "is a symlink") || !strings.Contains(err.Error(), root) {
		t.Fatalf("SecureStaging = %v, want the root's symlink refusal", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "img")); err == nil {
		t.Error("a subdirectory was created through the symlinked root")
	}
}

// An empty STAGING is a configuration mistake, not a request to work in the
// current directory.
func TestSecureStagingRefusesAnEmptyRoot(t *testing.T) {
	if err := SecureStaging(""); err == nil || !strings.Contains(err.Error(), "STAGING") {
		t.Fatalf("SecureStaging(\"\") = %v, want a refusal naming STAGING", err)
	}
}

// A staging directory this process cannot chmod is one it does not own, and
// "could not lock the door" is fatal, not a warning: for as long as a run
// lasts this tree holds the archive in the clear. As non-root that is what a
// root-owned directory looks like; as root chmod succeeds and the ownership
// check below takes over.
func TestSecureDirFailsWhenItCannotTighten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root can chmod anything, and must not chmod /proc")
	}
	// /proc is a directory root owns that no unprivileged process can chmod,
	// and that nothing here can damage by trying.
	if fi, err := os.Stat("/proc"); err != nil || !fi.IsDir() {
		t.Skip("skipping: no /proc to stand in for a directory somebody else owns")
	}
	err := SecureDir("/proc")
	if err == nil || !strings.Contains(err.Error(), "securing /proc") {
		t.Fatalf("SecureDir(/proc) = %v, want a fatal chmod failure", err)
	}
	if !strings.Contains(err.Error(), "point STAGING at a directory you own") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// Under root chmod succeeds on anything, so the ownership check is the only
// one of the three that can still fire — and root is the configuration
// README.md recommends for a full backup, so it is the one that matters most.
// Only root can build the fixture, so the test runs only as root.
func TestSecureDirRefusesADirectoryOwnedBySomeoneElse(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: only root can create a directory owned by another uid")
	}
	dir := filepath.Join(t.TempDir(), "theirs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const nobody = 65534
	if err := os.Chown(dir, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	err := SecureDir(dir)
	if err == nil || !strings.Contains(err.Error(), "is owned by uid 65534") {
		t.Fatalf("SecureDir = %v, want an ownership refusal", err)
	}
	if !strings.Contains(err.Error(), "chown") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// Running the backup under sudo is normal and supported, and a root run that
// finds staging owned by the account that invoked the sudo is the commonest
// innocent way to reach the ownership refusal. It is still a refusal — root
// writing plaintext into a directory an ordinary account can replace under it
// is worse, not better — but the advice names that account instead of guessing.
func TestOwnershipAdviceNamesTheSudoAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: the sudo branch of the advice is only reachable as root")
	}
	t.Setenv("SUDO_UID", "1000")
	got := ownershipAdvice("/var/tmp/brb", 1000)
	if !strings.Contains(got, "sudo") || !strings.Contains(got, "chown -R root /var/tmp/brb") {
		t.Errorf("advice for the sudo case = %q, want it to name sudo and the chown", got)
	}
	// A different owner is not the sudo story, and must not be described as one.
	if other := ownershipAdvice("/var/tmp/brb", 1001); strings.Contains(other, "sudo") {
		t.Errorf("advice for an unrelated owner = %q, want no sudo claim", other)
	}
}

// FileOwner is what the ownership check compares with, so it has to read the
// real uid: ours on our own file, root's on root's.
func TestFileOwnerReadsTheUid(t *testing.T) {
	own := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(own, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(own)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := FileOwner(fi); !ok || uid != os.Geteuid() {
		t.Fatalf("FileOwner(own file) = %d, %v; want %d, true", uid, ok, os.Geteuid())
	}
	if fi, err := os.Stat("/"); err == nil {
		if uid, ok := FileOwner(fi); !ok || uid != 0 {
			t.Fatalf("FileOwner(/) = %d, %v; want 0, true", uid, ok)
		}
	}
}

// CreateFresh is how every .part and log in staging is opened, on both sides
// of the program. A symlink planted at the path must not be followed: the
// plaintext lands under the intended name and the link's target is untouched.
// O_TRUNC would have written straight through it.
func TestCreateFreshNeverFollowsAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "disc01.squashfs.part")
	if err := os.Symlink(victim, part); err != nil {
		t.Fatal(err)
	}
	f, err := CreateFresh(part, 0o600)
	if err != nil {
		t.Fatalf("CreateFresh: %v", err)
	}
	if _, err := f.WriteString("plaintext"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, _ := os.ReadFile(victim); string(got) != "theirs" {
		t.Fatalf("the planted symlink was followed: victim now holds %q", got)
	}
	fi, err := os.Lstat(part)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the symlink is still at the .part path")
	}
	if got, _ := os.ReadFile(part); string(got) != "plaintext" {
		t.Fatalf(".part holds %q, want the plaintext", got)
	}
}

// A stale .part from a run that was killed mid-write is replaced, not appended
// to: that is the case O_TRUNC used to cover, and removing first covers it
// without opening through anything.
func TestCreateFreshReplacesAStaleFile(t *testing.T) {
	part := filepath.Join(t.TempDir(), "disc01.squashfs.part")
	if err := os.WriteFile(part, []byte("half an image from the run that died"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := CreateFresh(part, 0o600)
	if err != nil {
		t.Fatalf("CreateFresh: %v", err)
	}
	if _, err := f.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, _ := os.ReadFile(part); string(got) != "new" {
		t.Fatalf(".part holds %q, want only the new bytes", got)
	}
}

// OpenAppend keeps what is already in the file — that is why it exists — but
// still refuses to open through a symlink planted at the path.
func TestOpenAppendKeepsHistoryAndRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index.tsv")

	f, err := OpenAppend(index, 0o600)
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	f.WriteString("1\tdisc one\n")
	f.Close()
	f, err = OpenAppend(index, 0o600)
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	f.WriteString("2\tdisc two\n")
	f.Close()
	if got, _ := os.ReadFile(index); string(got) != "1\tdisc one\n2\tdisc two\n" {
		t.Fatalf("index holds %q, want both discs' lines", got)
	}

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "planted.tsv")
	if err := os.Symlink(victim, planted); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAppend(planted, 0o600); err == nil {
		t.Fatal("OpenAppend opened through a planted symlink")
	}
	if got, _ := os.ReadFile(victim); string(got) != "theirs" {
		t.Fatalf("the victim was written: %q", got)
	}
}

// TestSecureDirRefusesASymlinkSpelledWithATrailingSlash. lstat(2) resolves
// "link/" rather than reporting on the link, so without a Clean the refusal
// above it inspects the target and passes — the same hole brb.sh had, found by
// review. STAGING is operator-typed, and a trailing slash on a directory path
// is the most natural thing in the world to type.
func TestSecureDirRefusesASymlinkSpelledWithATrailingSlash(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "staging")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// The fixture is only meaningful if the bare spelling is refused, so that a
	// pass here cannot mean "SecureDir refuses nothing".
	if err := SecureDir(link); err == nil {
		t.Fatal("fixture: SecureDir accepted the symlink even without a trailing slash")
	}
	err := SecureDir(link + "/")
	if err == nil {
		t.Fatal("SecureDir accepted a symlinked staging directory spelled with a trailing slash")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("SecureDir = %v, want the symlink refusal", err)
	}
}

// TestSecureDirRefusesASymlinkPlantedAfterTheCheck is the reason SecureDir
// works through a descriptor rather than through the path.
//
// The refusal above is an Lstat, and an Lstat only describes what the name
// meant at the moment it ran. When STAGING is the documented default under a
// world-writable /var/tmp and does not exist yet — the state after every
// install, and after every "rm -rf staging" the tool itself advises — a local
// user can loop `ln -s /etc <STAGING>` and land the link in the gap. The old
// code then did three more lookups by name: MkdirAll accepted the link's
// target as an existing directory, chmod 0700 landed on that target, and the
// ownership rule passed anyway because under the recommended sudo run the
// target is root's too. The hook plants the link in exactly that gap, so the
// interleaving is a fact of the test rather than a race it has to win.
func TestSecureDirRefusesASymlinkPlantedAfterTheCheck(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(base, "staging")

	planted := false
	testHookAfterLstat = func() {
		if planted {
			return
		}
		planted = true
		if err := os.Symlink(victim, staging); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testHookAfterLstat = nil })

	err := SecureDir(staging)
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("SecureDir = %v, want the symlink refusal for a link planted after the Lstat", err)
	}
	// A refusal is worth little if the damage was done on the way to it: the
	// chmod through the link is what locks a directory nobody asked about.
	fi, serr := os.Stat(victim)
	if serr != nil {
		t.Fatal(serr)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's target is now %o: it was chmodded through the planted symlink", fi.Mode().Perm())
	}
}

// TestSecureDirCreatesMissingParents pins that splitting MkdirAll (the
// parents) from Mkdir (the directory itself) did not cost the ordinary case:
// STAGING is operator-typed and may name a path several levels below anything
// that exists.
func TestSecureDirCreatesMissingParents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "one", "two", "three")
	if err := SecureDir(dir); err != nil {
		t.Fatalf("SecureDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s is %v, want a 0700 directory", dir, fi.Mode())
	}
}
