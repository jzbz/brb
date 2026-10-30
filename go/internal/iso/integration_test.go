package iso

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// testOptions builds an Options over an empty staging directory. The tool set
// is whatever this machine has; the tests that need xorriso call needXorriso.
func testOptions(t *testing.T) Options {
	t.Helper()
	cfg := config.Default()
	cfg.Staging = filepath.Join(t.TempDir(), "staging")
	cfg.ArchiveName = "iso-test"
	cfg.LabelPrefix = "TEST"
	return Options{
		Cfg:     cfg,
		UI:      ui.New(&bytes.Buffer{}, false),
		Tools:   tools.Detect(t.Context()),
		Version: "test",
	}
}

// needXorriso skips a test on a machine without the one tool this package runs.
func needXorriso(t *testing.T, o Options) {
	t.Helper()
	if err := o.Tools.Require(tools.Xorriso); err != nil {
		t.Skipf("skipping: %v", err)
	}
}

// stageDiscs lays out disc directories numbered as given, each holding a file
// of payload bytes, and returns the Options over them. It is deliberately not a
// real backup: what is being tested is the imaging of a directory tree, and a
// fixture that takes a second to build is one the range cases can afford to
// repeat.
func stageDiscs(t *testing.T, o Options, nums ...int) {
	t.Helper()
	for _, n := range nums {
		data := filepath.Join(o.sourceDir(n), "data")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		body := bytes.Repeat([]byte{byte(n)}, 64<<10)
		if err := os.WriteFile(filepath.Join(data, "payload.bin"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(o.sourceDir(n), "README.md"),
			[]byte("disc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// isoNumbers lists the disc numbers actually present in the ISO directory.
func isoNumbers(t *testing.T, o Options) []int {
	t.Helper()
	nums, err := numbered(o.Cfg.Dirs().ISO, false, ".iso")
	if err != nil {
		t.Fatal(err)
	}
	return nums
}

// TestBuildRanges covers the whole of the selection syntax against real
// xorriso, over a set numbered with a gap in it — which is what a staging
// directory looks like once a disc has been re-made by hand.
func TestBuildRanges(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		spec string
		want []int
	}{
		{"all", []int{1, 2, 3, 5}},
		{"2", []int{2}},
		{"2-3", []int{2, 3}},
		{"3-", []int{3, 5}},
		{"4", nil},   // the gap itself: a well-formed selector matching nothing
		{"6-9", nil}, // past the end of the set
		{"5", []int{5}},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			o := testOptions(t)
			needXorriso(t, o)
			stageDiscs(t, o, 1, 2, 3, 5)

			err := Build(ctx, o, tc.spec)
			if len(tc.want) == 0 {
				if err == nil || !strings.Contains(err.Error(), "no discs matched") {
					t.Fatalf("Build(%q) = %v, want a complaint that nothing matched", tc.spec, err)
				}
				if got := isoNumbers(t, o); len(got) != 0 {
					t.Fatalf("Build(%q) built %v", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build(%q): %v", tc.spec, err)
			}
			got := isoNumbers(t, o)
			if len(got) != len(tc.want) {
				t.Fatalf("Build(%q) built %v, want %v", tc.spec, got, tc.want)
			}
			for i, n := range tc.want {
				if got[i] != n {
					t.Fatalf("Build(%q) built %v, want %v", tc.spec, got, tc.want)
				}
			}
			// Every image has to be a usable one, not merely a file that exists.
			for _, n := range got {
				fi, err := os.Stat(o.Path(n))
				if err != nil {
					t.Fatal(err)
				}
				src, err := fsx.DirBytes(ctx, o.sourceDir(n))
				if err != nil {
					t.Fatal(err)
				}
				if fi.Size() < src {
					t.Errorf("disc %d ISO is %d bytes, its source tree is %d", n, fi.Size(), src)
				}
			}
		})
	}
}

// TestBuildWithoutADiscSet refuses rather than producing an empty ISO
// directory an operator would then try to burn.
// TestBuildWithoutADiscSet also pins an ordering the securing pass must not
// disturb: "run 'brb backup' first" is a far better answer to an empty staging
// area than a permissions or lock error, so the cheap disc-set check keeps
// coming before fsx.SecureStaging and fsx.LockStaging.
//
// It no longer needs xorriso: Build asks for the tool after every refusal
// about the staging tree, this one included.
func TestBuildWithoutADiscSet(t *testing.T) {
	o := testOptions(t)
	err := Build(context.Background(), o, "all")
	if err == nil || !strings.Contains(err.Error(), "run 'brb backup' first") {
		t.Fatalf("Build over an empty staging area = %v, want a pointer at backup", err)
	}
}

// TestVolumeLabelCarriesTheManifestTotal is the trap this package exists to
// avoid: the label says "01 OF 20", and a burn run days after the backup has
// only MANIFEST.txt to learn the 20 from.
func TestVolumeLabelCarriesTheManifestTotal(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1, 2)

	if err := os.WriteFile(filepath.Join(o.Cfg.Staging, "MANIFEST.txt"),
		[]byte("archive name    : iso-test\ndiscs           : 20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, o, "1"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := volumeLabel(t, o.Path(1)), "TEST_01_OF_20"; got != want {
		t.Errorf("volume label = %q, want %q — the total did not come from MANIFEST.txt", got, want)
	}

	// Without a manifest it falls back to what staging holds, which is two.
	if err := os.Remove(filepath.Join(o.Cfg.Staging, "MANIFEST.txt")); err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, o, "2"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := volumeLabel(t, o.Path(2)), "TEST_02_OF_02"; got != want {
		t.Errorf("volume label = %q, want %q", got, want)
	}
}

// volumeLabel reads the ISO 9660 primary volume descriptor's volume identifier:
// 32 bytes at offset 0x8028. Reading it out of the file rather than asking
// xorriso keeps the assertion independent of the tool that wrote it.
func volumeLabel(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 32)
	if _, err := f.ReadAt(buf, 0x8028); err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(buf), " \x00")
}

// TestBuildOneRefusesAnISOLargerThanTheMedia proves the upper size assertion by
// declaring media far too small for the tree. Without it a disc that cannot
// possibly be burned is only discovered by the drive, one blank at a time.
func TestBuildOneRefusesAnISOLargerThanTheMedia(t *testing.T) {
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)
	o.Cfg.DiscCapacityBytes = 4096

	err := o.BuildOne(context.Background(), 1, 1)
	if err == nil || !strings.Contains(err.Error(), "larger than the") {
		t.Fatalf("BuildOne onto 4 KiB media = %v, want a size refusal", err)
	}
	// And the unusable file must not be left where burn would find it.
	if _, statErr := os.Stat(o.Path(1)); statErr == nil {
		t.Errorf("%s was left behind after the size check refused it", o.Path(1))
	}
}

// TestEnsureBuildsOnlyWhatIsMissing is the burn path's contract: an ISO that is
// there is reused untouched, one that is not is built from the disc directory,
// and a zero-length remnant of a dead run is replaced rather than burned.
func TestEnsureBuildsOnlyWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)

	path, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Second call: the file is there, so nothing is rebuilt. Removing the disc
	// directory is what proves it — a rebuild would now fail.
	if err := os.RemoveAll(o.sourceDir(1)); err != nil {
		t.Fatal(err)
	}
	again, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure over an existing ISO: %v", err)
	}
	second, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) || second.Size() != first.Size() {
		t.Error("Ensure rebuilt an ISO that was already there")
	}

	// An empty file is not an ISO. With the disc directory gone it cannot be
	// rebuilt either, and saying so beats handing 0 bytes to the burner.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Ensure(ctx, 1, 1); err == nil {
		t.Error("Ensure accepted a zero-length ISO")
	}
}

// TestEnsureRebuildsATruncatedISO is the power-loss case: xorriso died part-way
// through and left a file that is non-empty but shorter than the disc directory
// it was made from. Such a file passes every "is it there" test, and Ensure
// used to hand it straight to the burner. It must be rebuilt instead, to the
// same bound BuildOne holds a fresh image to.
func TestEnsureRebuildsATruncatedISO(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)

	path, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	whole, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	srcSize, err := fsx.DirBytes(ctx, o.sourceDir(1))
	if err != nil {
		t.Fatal(err)
	}
	// Cut it to well under the tree it was built from: the payload alone is
	// 64 KiB, so 4 KiB of ISO cannot be complete.
	if err := os.Truncate(path, 4096); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	o.UI = ui.New(&log, false)
	again, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure over a truncated ISO: %v", err)
	}
	rebuilt, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Size() < srcSize {
		t.Fatalf("Ensure returned a %d-byte ISO for a %d-byte disc directory: the truncated file was "+
			"handed back instead of rebuilt", rebuilt.Size(), srcSize)
	}
	if rebuilt.Size() != whole.Size() {
		t.Errorf("rebuilt ISO is %d bytes, the original was %d", rebuilt.Size(), whole.Size())
	}
	if !strings.Contains(log.String(), "truncated") {
		t.Errorf("the rebuild was not explained to the operator:\n%s", log.String())
	}
}

// TestBuildRefusesASymlinkedStagingRootBeforeTakingTheLock is the `iso`
// command's half of the staging rules, and it is about ORDER as much as about
// the refusal. Build used to go straight from "are there disc directories?" to
// fsx.LockStaging without securing the tree first; backup secures in preflight
// and burn/restore secure in their own lockStaging, and `brb iso` was the one
// staging-writing command that did not.
//
// Order is not observable in whether Build fails, which is what this test used
// to check and why it passed with the securing call deleted. LockStaging opens
// the lock O_NOFOLLOW and refuses a planted symlink on its own (fsx/lock.go),
// so a Build that reaches it still errors, still says "symlink", and still
// leaves the target whole. An earlier version of the comment here claimed the
// opposite — that LockStaging follows the link and only the securing above
// prevents it — and asserting on that belief tested nothing.
//
// What order does decide is WHICH refusal the operator gets. Secured first, the
// error names the staging root, which is the thing they have to fix. Unsecured,
// it names the lock file inside a root that should never have been used at all.
// So the assertion is on the wording of the two, and it fails if the securing
// call goes away.
func TestBuildRefusesASymlinkedStagingRootBeforeTakingTheLock(t *testing.T) {
	o := testOptions(t)
	stageDiscs(t, o, 1)

	victim := filepath.Join(t.TempDir(), "bashrc")
	const content = "# the operator's file, which must still be here afterwards\n"
	if err := os.WriteFile(victim, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(o.Cfg.Staging, fsx.LockName)); err != nil {
		t.Fatal(err)
	}
	// Point STAGING at the tree through a symlink, which is the first of
	// SecureDir's three rules and the one a test can arrange without a second
	// uid. Securing has to happen before the lock is opened, so a Build that
	// reaches LockStaging at all truncates the victim.
	link := filepath.Join(t.TempDir(), "staging-link")
	if err := os.Symlink(o.Cfg.Staging, link); err != nil {
		t.Fatal(err)
	}
	o.Cfg.Staging = link

	err := Build(context.Background(), o, "all")
	if err == nil {
		t.Fatal("Build over a symlinked STAGING = nil, want the same refusal backup and burn make")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Build over a symlinked STAGING = %v, want an error naming the symlink", err)
	}
	// The ordering assertion. SecureDir's refusal is the staging-root one;
	// LockStaging's names the lock file. Getting the second means the lock was
	// opened inside a root nothing had vetted, which is the defect this test is
	// named for.
	if !strings.Contains(err.Error(), "a staging directory must be a real directory") {
		t.Errorf("Build refused, but not as the staging root: %v", err)
	}
	if strings.Contains(err.Error(), "the staging lock") {
		t.Errorf("the lock was opened before the tree was secured; the refusal came from "+
			"LockStaging rather than from SecureDir: %v", err)
	}
	got, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("the symlink target is gone: %v", readErr)
	}
	if string(got) != content {
		t.Fatalf("the staging lock was opened before the tree was secured and truncated the "+
			"operator's file: %q", got)
	}
}

// TestBuildTightensALooseStagingRoot is the same defect with no attacker in it.
// BuildOne creates the ISO directory with os.MkdirAll, whose mode reaches only
// what it makes, so a staging tree the operator laid out by hand under a loose
// umask kept those permissions for the whole run — while holding a full second
// copy of every disc in the clear. Securing forces 0700 whether or not the
// directory was just created, which is precisely the rule MkdirAll does not
// have.
func TestBuildTightensALooseStagingRoot(t *testing.T) {
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)
	if err := os.MkdirAll(o.Cfg.Dirs().ISO, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(o.Cfg.Staging, 0o777); err != nil {
		t.Fatal(err)
	}

	if err := Build(context.Background(), o, "all"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, dir := range []string{o.Cfg.Staging, o.Cfg.Dirs().ISO} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s is %o after Build, want 0700 — it holds a full copy of every disc", dir, perm)
		}
	}
}

// TestEnsureRefusesAnISOTooLargeForTheMedia holds an ISO that is already on disk
// to the upper bound BuildOne holds a fresh one to. Ensure used to apply only
// the lower bound, so an image built for 100 GB BD-R XL was handed to the burner
// unchanged after DISC_TYPE was changed to a 25 GB bd-r — the operator found out
// from xorriso with a blank already in the tray. The existing file must survive
// the refusal: it is still exactly right for the media it was built for, and
// rebuilding it would only hit BuildOne's own capacity check hours later.
func TestEnsureRefusesAnISOTooLargeForTheMedia(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	needXorriso(t, o)
	stageDiscs(t, o, 1)

	path, err := o.Ensure(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	built, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Now the same staging tree, with media too small for what is in it.
	o.Cfg.DiscCapacityBytes = built.Size() - 1
	if _, err := o.Ensure(ctx, 1, 1); err == nil {
		t.Fatal("Ensure returned an ISO larger than the configured media")
	} else if !strings.Contains(err.Error(), "larger than the") {
		t.Errorf("Ensure = %v, want an error naming the media size", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the oversized ISO was deleted rather than refused: %v", err)
	}
	if after.Size() != built.Size() {
		t.Errorf("the existing ISO was rewritten: %d bytes, was %d", after.Size(), built.Size())
	}

	// Capacity 0 means "media size unknown", and the bound is not applied then.
	o.Cfg.DiscCapacityBytes = 0
	o.Cfg.DiscType = ""
	if got, err := o.Ensure(ctx, 1, 1); err != nil || got != path {
		t.Errorf("Ensure with no known capacity = %q, %v; want the existing ISO", got, err)
	}
}

// TestBuildRefusesASymlinkedDiscsDirectory is the disc directories' half of
// `brb iso`'s staging rules. Every image is mastered from discs/, so a discs/
// that is a symlink images whatever tree it points at and labels it as this
// set. burn and backup refuse it; Build used to secure only the root and iso/.
// The refusal comes before the tool requirement, so this runs without xorriso.
func TestBuildRefusesASymlinkedDiscsDirectory(t *testing.T) {
	o := testOptions(t)
	far := filepath.Join(t.TempDir(), "other-set")
	if err := os.MkdirAll(filepath.Join(far, dirName(1), "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(o.Cfg.Staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(far, o.Cfg.Dirs().Discs); err != nil {
		t.Fatal(err)
	}

	err := Build(context.Background(), o, "all")
	if err == nil || !strings.Contains(err.Error(), "is a symlink") || !strings.Contains(err.Error(), "discs") {
		t.Fatalf("Build over a symlinked discs/ = %v, want a refusal naming it", err)
	}
	if got := isoNumbers(t, o); len(got) != 0 {
		t.Errorf("Build imaged %v from a symlinked discs/", got)
	}
}

// TestBuildOneRefusesASymlinkedDiscDirectory is the level below discs/, which
// securing discs/ does not reach. CanRebuild used os.Stat, so a discs/disc01
// that was a link to a directory counted as the disc directory: BuildOne
// mastered the image from wherever it pointed and labelled it as this set,
// and burn measured an existing ISO against that tree and then deleted the
// ISO as rebuildable. DiscNumbers never counted such a link as a disc, and now
// nothing else does either. The refusal names the link, comes before the tool
// requirement, and builds nothing.
func TestBuildOneRefusesASymlinkedDiscDirectory(t *testing.T) {
	o := testOptions(t)
	far := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(far, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(far, "data", "payload.bin"), []byte("not this set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(o.Cfg.Dirs().Discs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(far, o.sourceDir(1)); err != nil {
		t.Fatal(err)
	}

	if o.CanRebuild(1) {
		t.Error("CanRebuild counts a symlink as a disc directory")
	}
	err := o.BuildOne(context.Background(), 1, 1)
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("BuildOne from a symlinked disc directory = %v, want a refusal naming the link", err)
	}
	if got := isoNumbers(t, o); len(got) != 0 {
		t.Errorf("BuildOne imaged %v through a symlinked disc directory", got)
	}
}

// TestEnsureRefusesAnOversizedISOWithoutItsDiscDirectory is the media bound in
// an ISO-only staging tree — the files `brb iso all` makes to carry to another
// machine, whose DISC_TYPE may name smaller blanks. complete used to return
// "complete" as soon as it found no disc directory, before the capacity check
// that needs nothing but the file and the configuration, so burn handed the
// burner an image that could not fit. It is refused now, the file is kept —
// it is the only copy — and the message does not suggest deleting it to be
// rebuilt, which with nothing to rebuild from would destroy it. Runs without
// xorriso: nothing is built.
func TestEnsureRefusesAnOversizedISOWithoutItsDiscDirectory(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	if err := os.MkdirAll(o.Cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	path := o.Path(1)
	if err := os.WriteFile(path, bytes.Repeat([]byte{'i'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	o.Cfg.DiscCapacityBytes = 4095
	_, err := o.Ensure(ctx, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "larger than the") {
		t.Fatalf("Ensure of a 4096-byte ISO onto 4095-byte media = %v, want the media refusal", err)
	}
	if strings.Contains(err.Error(), "rebuild") {
		t.Errorf("the refusal offers a rebuild with no disc directory to rebuild from: %v", err)
	}
	if fi, statErr := os.Stat(path); statErr != nil || fi.Size() != 4096 {
		t.Fatalf("the only copy did not survive the refusal: %v", statErr)
	}

	// Media it fits, and media of unknown size: both take the file as it is.
	o.Cfg.DiscCapacityBytes = 4096
	if got, err := o.Ensure(ctx, 1, 1); err != nil || got != path {
		t.Errorf("Ensure onto media the ISO fits = %q, %v; want the existing ISO", got, err)
	}
	o.Cfg.DiscCapacityBytes = 0
	o.Cfg.DiscType = ""
	if got, err := o.Ensure(ctx, 1, 1); err != nil || got != path {
		t.Errorf("Ensure with no known capacity = %q, %v; want the existing ISO", got, err)
	}
}

// TestEnsureOffersARebuildOnlyWhereOneCouldFit pins the advice on the media
// refusal when the disc directory IS there. Deleting the ISO and rebuilding is
// worth suggesting only when the tree itself fits the media; when it does not,
// a rebuild is certain to hit BuildOne's own capacity refusal, which deletes
// what it built. That case used to be offered the rebuild anyway, and when the
// ISO was also shorter than its tree it was rebuilt outright, at the cost of
// a whole build, before being refused. Runs without xorriso: nothing is built.
func TestEnsureOffersARebuildOnlyWhereOneCouldFit(t *testing.T) {
	ctx := context.Background()
	o := testOptions(t)
	stageDiscs(t, o, 1)
	srcSize, err := fsx.DirBytes(ctx, o.sourceDir(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(o.Cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(size int64) {
		t.Helper()
		if err := os.WriteFile(o.Path(1), bytes.Repeat([]byte{'i'}, int(size)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The tree fits; the ISO, built for something bigger, does not.
	write(srcSize + 10)
	o.Cfg.DiscCapacityBytes = srcSize + 5
	if _, err := o.Ensure(ctx, 1, 1); err == nil || !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("Ensure = %v, want the media refusal offering a rebuild", err)
	}

	// The tree itself is bigger than the media: no rebuild can fit, so none is
	// offered — and a file that is also shorter than its tree is refused and
	// left alone, not rebuilt. A rebuild would need xorriso, and where it is
	// installed it used to replace the file and then refuse what it built.
	for _, size := range []int64{srcSize + 10, srcSize - 10} {
		write(size)
		o.Cfg.DiscCapacityBytes = srcSize - 20
		_, err := o.Ensure(ctx, 1, 1)
		if err == nil || !strings.Contains(err.Error(), "larger than the") {
			t.Errorf("Ensure of %d bytes onto %d-byte media for a %d-byte tree = %v, want the media refusal",
				size, srcSize-20, srcSize, err)
		} else if strings.Contains(err.Error(), "rebuild") {
			t.Errorf("the refusal offers a rebuild that cannot fit: %v", err)
		}
		if fi, statErr := os.Stat(o.Path(1)); statErr != nil || fi.Size() != size {
			t.Errorf("the %d-byte ISO was not left as it was: %v", size, statErr)
		}
	}
}
