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
