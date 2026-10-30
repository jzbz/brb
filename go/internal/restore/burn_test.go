package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/tools"
)

// burnRig is a staging area with disc directories and a stubbed burner.
//
// There is no optical drive on the machines these tests run on, so xorriso is
// wrapped: an "-as mkisofs" run is passed through to the real program, so the
// ISOs are genuinely built, while an "-as cdrecord" run records what it was
// asked to burn and exits with whatever status the test wants. That is the only
// way to drive burn's real path — build a missing image, write it, then decide
// whether to drop it — without a blank disc in a tray.
type burnRig struct {
	e       *env
	burnLog string
}

func newBurnRig(t *testing.T, exitStatus int, discs ...int) *burnRig {
	t.Helper()
	e := newEnv(t)
	real := realTools(t, tools.Xorriso)
	e.cfg.Burner = "/dev/sr0"
	e.cfg.ArchiveName = "burn-test"
	e.cfg.LabelPrefix = "TEST"
	layDiscs(t, e, discs...)

	burnLog := filepath.Join(e.dir, "burned.log")
	// $2 is the personality: "cdrecord" is the burn, anything else (mkisofs,
	// --version) is a real xorriso run. The size is recorded as proof the ISO
	// existed and was complete at the moment the burner was handed it.
	script := "#!/bin/sh\n" +
		"if [ \"$2\" = cdrecord ]; then\n" +
		"  for a in \"$@\"; do last=\"$a\"; done\n" +
		"  printf '%s %s\\n' \"$last\" \"$(wc -c < \"$last\")\" >> " + burnLog + "\n" +
		"  exit " + strconv.Itoa(exitStatus) + "\n" +
		"fi\n" +
		"exec " + real.Get(tools.Xorriso).Path + " \"$@\"\n"
	path := filepath.Join(e.dir, "xorriso")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var set []tools.Tool
	for _, tl := range real.All() {
		if tl.Name == tools.Xorriso {
			tl.Path = path
		}
		set = append(set, tl)
	}
	e.tools = tools.NewSet(set)
	e.ui.SetAssumeYes(true)
	return &burnRig{e: e, burnLog: burnLog}
}

// layDiscs lays out a disc directory for each of discs, each holding a 32 KiB
// payload.
func layDiscs(t *testing.T, e *env, discs ...int) {
	t.Helper()
	for _, n := range discs {
		data := filepath.Join(e.cfg.Dirs().Discs, discDirName(n), dataDir)
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		body := strings.Repeat("payload ", 4096)
		if err := os.WriteFile(filepath.Join(data, "payload.bin"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stubISOSize is the size of every image the stand-in xorriso of
// newStubBurnRig "builds": larger than any disc directory layDiscs makes, so
// it passes BuildOne's lower bound, and different from every file a test
// plants, so the burner's record says which of the two it was handed.
const stubISOSize = 256 << 10

// newStubBurnRig is newBurnRig with no xorriso at all behind it, for the tests
// whose subject is which file burn picks, keeps or refuses rather than the
// image inside it. The stand-in's "-as mkisofs" writes stubISOSize zero bytes
// to its -o path and its "-as cdrecord" records what it was handed, exactly as
// newBurnRig's does, so these run on a machine without xorriso, where every
// newBurnRig test skips.
func newStubBurnRig(t *testing.T, discs ...int) *burnRig {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build an xorriso stand-in with")
	}
	e := newEnv(t)
	e.cfg.Burner = "/dev/sr0"
	e.cfg.ArchiveName = "burn-test"
	e.cfg.LabelPrefix = "TEST"
	layDiscs(t, e, discs...)

	burnLog := filepath.Join(e.dir, "burned.log")
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"cdrecord)\n" +
		"  for a in \"$@\"; do last=\"$a\"; done\n" +
		"  printf '%s %s\\n' \"$last\" \"$(wc -c < \"$last\")\" >> " + burnLog + "\n" +
		"  exit 0 ;;\n" +
		"mkisofs)\n" +
		"  prev=; for a in \"$@\"; do [ \"$prev\" = -o ] && out=\"$a\"; prev=\"$a\"; done\n" +
		"  dd if=/dev/zero of=\"$out\" bs=1024 count=" + strconv.Itoa(stubISOSize>>10) + " 2>/dev/null\n" +
		"  exit $? ;;\n" +
		"esac\n" +
		"exit 1\n"
	path := filepath.Join(e.dir, "xorriso")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Xorriso, Path: path, Found: true}})
	e.ui.SetAssumeYes(true)
	return &burnRig{e: e, burnLog: burnLog}
}

// burned returns the lines the stub burner recorded, one per disc written.
func (r *burnRig) burned(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(r.burnLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// discDirName is the per-disc staging directory name, e.g. "disc03".
func discDirName(n int) string { return fmt.Sprintf("disc%02d", n) }

// TestBurnBuildsAMissingISOAndDropsItAfterwards is the whole of ISO_MODE=
// ondemand seen from the burn side: nothing has built the image, burn builds it
// from the disc directory, writes it, and only then removes it again.
func TestBurnBuildsAMissingISOAndDropsItAfterwards(t *testing.T) {
	r := newBurnRig(t, 0, 1, 2)
	e := r.e

	// Nothing has built anything yet.
	if _, err := os.Stat(e.cfg.Dirs().ISO); err == nil {
		if ents, _ := os.ReadDir(e.cfg.Dirs().ISO); len(ents) != 0 {
			t.Fatalf("the fixture already holds ISOs: %v", ents)
		}
	}

	if err := Burn(context.Background(), e.opts(), "all"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}

	lines := r.burned(t)
	if len(lines) != 2 {
		t.Fatalf("the burner was given %d disc(s), want 2:\n%v\n%s", len(lines), lines, e.log())
	}
	for i, n := range []int{1, 2} {
		want := iso.Name(n)
		if !strings.Contains(lines[i], want) {
			t.Errorf("burn %d wrote %q, want %s", i+1, lines[i], want)
		}
		// The second field is the ISO's size at the moment it was burned: an
		// empty or half-written image would show up here and nowhere else.
		fields := strings.Fields(lines[i])
		if len(fields) != 2 {
			t.Fatalf("unparsable burn record %q", lines[i])
		}
		if size, err := strconv.Atoi(fields[1]); err != nil || size < 64<<10 {
			t.Errorf("disc %d was burned at %s bytes; it was not a complete ISO", n, fields[1])
		}
		// And it is gone again: KEEP_ISOS defaults to 0.
		if _, err := os.Stat(e.opts().isoOptions().Path(n)); err == nil {
			t.Errorf("disc %d ISO survived a successful burn under KEEP_ISOS=0", n)
		}
	}
	if !strings.Contains(e.log(), "removed "+iso.Name(1)) {
		t.Errorf("the removal was not reported:\n%s", e.log())
	}
	// The disc directories are untouched, so the disc can be burned again.
	for _, n := range []int{1, 2} {
		if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Discs, discDirName(n))); err != nil {
			t.Errorf("disc %d directory: %v", n, err)
		}
	}
}

// TestBurnKeepsTheISOAfterAFailure is the rule that matters most here: a failed
// burn is exactly when the retry needs the image still to be on disk.
func TestBurnKeepsTheISOAfterAFailure(t *testing.T) {
	r := newBurnRig(t, 1, 1)
	e := r.e

	err := Burn(context.Background(), e.opts(), "1")
	if err == nil || !strings.Contains(err.Error(), "burning disc 1") {
		t.Fatalf("Burn = %v, want the failure to be reported", err)
	}
	path := e.opts().isoOptions().Path(1)
	fi, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("the ISO was removed after a FAILED burn, so a retry has to rebuild it: %v", statErr)
	}
	if fi.Size() == 0 {
		t.Error("the ISO left behind after a failed burn is empty")
	}
}

// TestBurnKeepsTheISOWhenAsked covers KEEP_ISOS=1, for an operator burning two
// copies of the same set.
func TestBurnKeepsTheISOWhenAsked(t *testing.T) {
	r := newBurnRig(t, 0, 1)
	e := r.e
	e.cfg.KeepISOs = true

	if err := Burn(context.Background(), e.opts(), "1"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(e.opts().isoOptions().Path(1)); err != nil {
		t.Fatalf("KEEP_ISOS=1 did not keep the ISO: %v", err)
	}
	if strings.Contains(e.log(), "removed ") {
		t.Errorf("the log claims a removal under KEEP_ISOS=1:\n%s", e.log())
	}
}

// TestBurnCountsDiscDirectoriesNotISOs pins the queue to the disc directories.
// Under ondemand there are no ISOs to count, and after the first disc is burned
// under KEEP_ISOS=0 there is one fewer — a queue taken from them would shrink
// as it ran.
func TestBurnCountsDiscDirectoriesNotISOs(t *testing.T) {
	r := newBurnRig(t, 0, 1, 2, 5)
	e := r.e

	// The manifest says twenty, and that is what the labels must say too.
	if err := os.WriteFile(filepath.Join(e.cfg.Staging, manifestName),
		[]byte("archive name    : burn-test\ndiscs           : 20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Burn(context.Background(), e.opts(), "2-"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	// A gap in the numbering is a shorter queue, not an error: 2 and 5.
	if lines := r.burned(t); len(lines) != 2 ||
		!strings.Contains(lines[0], iso.Name(2)) || !strings.Contains(lines[1], iso.Name(5)) {
		t.Fatalf("burned %v, want disc02 and disc05", lines)
	}
	if !strings.Contains(e.log(), "disc 5 of 20") {
		t.Errorf("the label instruction does not carry the manifest's total:\n%s", e.log())
	}
}

// TestBurnRefusesASymlinkedISODirectory is burn's half of the staging rules.
// lockStaging secures only the root, and burn never secured iso/ itself,
// although it builds every image there, burns it from there and deletes it
// there: a symlinked iso/ had each ISO built, burned and removed wherever the
// link pointed. Backup and `brb iso` both refuse that tree already.
func TestBurnRefusesASymlinkedISODirectory(t *testing.T) {
	r := newStubBurnRig(t, 1)
	e := r.e
	far := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(far, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(far, e.cfg.Dirs().ISO); err != nil {
		t.Fatal(err)
	}

	err := Burn(context.Background(), e.opts(), "all")
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("Burn through a symlinked iso/ = %v, want a refusal naming the link\n%s", err, e.log())
	}
	if lines := r.burned(t); len(lines) != 0 {
		t.Errorf("the burner was handed %v through a symlinked iso/", lines)
	}
	if ents, _ := os.ReadDir(far); len(ents) != 0 {
		t.Errorf("burn wrote into the link's target: %v", ents)
	}
}

// TestBurnRefusesASymlinkedDiscsDirectory is the other directory burn reads
// from. Every image is mastered from discs/, so a symlinked one burns whatever
// tree it points at, labelled as this set.
func TestBurnRefusesASymlinkedDiscsDirectory(t *testing.T) {
	r := newStubBurnRig(t)
	e := r.e
	far := filepath.Join(t.TempDir(), "other-set")
	if err := os.MkdirAll(filepath.Join(far, discDirName(1), dataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(far, e.cfg.Dirs().Discs); err != nil {
		t.Fatal(err)
	}

	err := Burn(context.Background(), e.opts(), "all")
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("Burn through a symlinked discs/ = %v, want a refusal naming the link\n%s", err, e.log())
	}
	if lines := r.burned(t); len(lines) != 0 {
		t.Errorf("the burner was handed %v from a symlinked discs/", lines)
	}
}

// TestBurnRefusesASymlinkedISOItCannotRebuild is the file half of the same
// finding, which securing the directories does not reach: Ensure used os.Stat,
// so a disc01.iso that was a link to any readable file was taken for the ISO,
// and in a tree carried in as ISOs alone, with no disc directory to measure it
// against, nothing else stood between that file and the burner. It must be
// refused before the burner is reached, with the file it named untouched.
func TestBurnRefusesASymlinkedISOItCannotRebuild(t *testing.T) {
	r := newStubBurnRig(t)
	e := r.e
	const secret = "the host file, which is no business of a blank disc\n"
	host := filepath.Join(t.TempDir(), "hostfile")
	if err := os.WriteFile(host, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, e.opts().isoOptions().Path(1)); err != nil {
		t.Fatal(err)
	}

	err := Burn(context.Background(), e.opts(), "all")
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("Burn of a symlinked ISO with no disc directory = %v, want a refusal\n%s", err, e.log())
	}
	if lines := r.burned(t); len(lines) != 0 {
		t.Errorf("the burner was handed %v: a symlinked ISO was burned", lines)
	}
	if got, readErr := os.ReadFile(host); readErr != nil || string(got) != secret {
		t.Errorf("the link's target changed: %q, %v", got, readErr)
	}
}

// TestBurnRebuildsASymlinkedISOInsteadOfBurningIt is the same link with the
// disc directory present. A link to a big enough file passed both of Ensure's
// bounds and was burned as the disc; now it is replaced by an image built in
// iso/ — which is what `brb iso` already did with it — and the file it named
// is left exactly as it was.
func TestBurnRebuildsASymlinkedISOInsteadOfBurningIt(t *testing.T) {
	r := newStubBurnRig(t, 1)
	e := r.e
	big := filepath.Join(t.TempDir(), "another-image")
	body := strings.Repeat("O", 2*stubISOSize)
	if err := os.WriteFile(big, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	isoPath := e.opts().isoOptions().Path(1)
	if err := os.Symlink(big, isoPath); err != nil {
		t.Fatal(err)
	}

	if err := Burn(context.Background(), e.opts(), "all"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	want := fmt.Sprintf("%s %d", isoPath, stubISOSize)
	if lines := r.burned(t); len(lines) != 1 || lines[0] != want {
		t.Fatalf("the burner was handed %q, want %q — a freshly built image, not the link's target", lines, want)
	}
	if got, err := os.ReadFile(big); err != nil || string(got) != body {
		t.Errorf("the link's target was changed (%d bytes, %v)", len(got), err)
	}
	if !strings.Contains(e.log(), "is a symlink") {
		t.Errorf("replacing the link was not reported:\n%s", e.log())
	}
}

// TestBurnKeepsTheOnlyCopyOfAnISO is the tree `brb iso all` makes to carry to
// another machine: ISOs and nothing else. Under the default KEEP_ISOS=0 burn
// used to delete each one after the burn, saying it could be rebuilt from a
// discs/ that does not exist there — the only copy, gone, before the disc had
// been read back. With its disc directory present the ISO is a cache and still
// goes, which the second half holds to.
func TestBurnKeepsTheOnlyCopyOfAnISO(t *testing.T) {
	r := newStubBurnRig(t)
	e := r.e
	if err := os.MkdirAll(e.cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	isoPath := e.opts().isoOptions().Path(1)
	body := strings.Repeat("I", 300<<10)
	if err := os.WriteFile(isoPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Burn(context.Background(), e.opts(), "all"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	if lines := r.burned(t); len(lines) != 1 || !strings.Contains(lines[0], iso.Name(1)) {
		t.Fatalf("burned %v, want disc01.iso once", lines)
	}
	if got, err := os.ReadFile(isoPath); err != nil || string(got) != body {
		t.Fatalf("the only copy of disc 1 did not survive its burn (%d bytes, %v)", len(got), err)
	}
	if log := e.log(); !strings.Contains(log, "kept "+iso.Name(1)) ||
		strings.Contains(log, "removed ") || strings.Contains(log, "rebuilt from") {
		t.Errorf("the log does not say the ISO was kept, or still promises a rebuild:\n%s", log)
	}

	// The control: a disc directory to rebuild from makes the ISO a cache.
	r = newStubBurnRig(t, 1)
	e = r.e
	if err := Burn(context.Background(), e.opts(), "all"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(e.opts().isoOptions().Path(1)); err == nil {
		t.Error("a rebuildable ISO survived a successful burn under KEEP_ISOS=0")
	}
	if !strings.Contains(e.log(), "removed "+iso.Name(1)) {
		t.Errorf("the removal was not reported:\n%s", e.log())
	}
}

// TestBurnKeepsAnISOWhoseDiscDirectoryIsASymlink is the same ISO-only tree
// with a discs/disc01 that is a link to a directory somewhere else. CanRebuild
// used os.Stat and took the link for the disc directory, so the only copy was
// deleted after its burn as though it could be rebuilt — from a tree that is
// not in staging, and that a rebuild would have labelled as this set.
func TestBurnKeepsAnISOWhoseDiscDirectoryIsASymlink(t *testing.T) {
	r := newStubBurnRig(t)
	e := r.e
	far := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(far, dataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{e.cfg.Dirs().Discs, e.cfg.Dirs().ISO} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(far, filepath.Join(e.cfg.Dirs().Discs, discDirName(1))); err != nil {
		t.Fatal(err)
	}
	isoPath := e.opts().isoOptions().Path(1)
	body := strings.Repeat("I", 300<<10)
	if err := os.WriteFile(isoPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Burn(context.Background(), e.opts(), "all"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	if lines := r.burned(t); len(lines) != 1 || lines[0] != fmt.Sprintf("%s %d", isoPath, len(body)) {
		t.Fatalf("burned %v, want the ISO in staging, once and as it was", lines)
	}
	if got, err := os.ReadFile(isoPath); err != nil || string(got) != body {
		t.Fatalf("the only copy of disc 1 did not survive its burn (%d bytes, %v)", len(got), err)
	}
	if !strings.Contains(e.log(), "kept "+iso.Name(1)) {
		t.Errorf("the log does not say the ISO was kept:\n%s", e.log())
	}
}

// TestBurnStoppedEarlyStillReports is declining "Continue to disc N?". That
// used to return on the spot, so a campaign stopped after real burns printed
// neither the closing count — the line that ends up in the operator's notes —
// nor the reminder to read the discs back, and its "stopped after disc N"
// named the previous disc even when that one had been skipped.
func TestBurnStoppedEarlyStillReports(t *testing.T) {
	tests := []struct {
		name, answers string
		burned        int
		want, notWant []string
	}{
		{"after burning disc 1", "y\nn\n", 1,
			[]string{"stopped before disc 2", "burned 1 disc(s)", "verify what you burned"},
			[]string{"stopped after"}},
		{"after skipping disc 1", "n\nn\n", 0,
			[]string{"skipped disc 1", "stopped before disc 2", "burned 0 of 1 disc(s), 1 skipped"},
			[]string{"stopped after", "verify what you burned"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newStubBurnRig(t, 1, 2, 3)
			e := r.e
			e.ui.SetAssumeYes(false)
			e.ui.SetInput(strings.NewReader(tc.answers))

			if err := Burn(context.Background(), e.opts(), "all"); err != nil {
				t.Fatalf("Burn: %v\n%s", err, e.log())
			}
			if got := len(r.burned(t)); got != tc.burned {
				t.Fatalf("the burner was handed %d disc(s), want %d\n%s", got, tc.burned, e.log())
			}
			for _, w := range tc.want {
				if !strings.Contains(e.log(), w) {
					t.Errorf("the log does not say %q:\n%s", w, e.log())
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(e.log(), w) {
					t.Errorf("the log says %q:\n%s", w, e.log())
				}
			}
		})
	}
}
