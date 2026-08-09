package restore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
)

func TestStagedSidecarName(t *testing.T) {
	for _, tc := range []struct {
		name string
		disc int
		want string
	}{
		// The two names a disc's sidecars set is actually made of.
		{"sidecars.par2", 3, "sidecars-disc03.par2"},
		{"sidecars.vol00+40.par2", 3, "sidecars-disc03.vol00+40.par2"},
		{"sidecars.par2", 12, "sidecars-disc12.par2"},
		// Everything else on a disc is already per-disc or genuinely shared.
		{"disc01.squashfs.age", 1, "disc01.squashfs.age"},
		{"disc01.squashfs.age.par2", 1, "disc01.squashfs.age.par2"},
		{"index.tsv.gz.age", 1, "index.tsv.gz.age"},
		{"sidecars.par2.sha512", 1, "sidecars.par2.sha512"},
		// An unnumbered disc keeps the flat name: renaming it to disc00 would
		// invent a disc that is not in the set.
		{"sidecars.par2", 0, "sidecars.par2"},
	} {
		if got := stagedSidecarName(tc.name, tc.disc); got != tc.want {
			t.Errorf("stagedSidecarName(%q, %d) = %q, want %q", tc.name, tc.disc, got, tc.want)
		}
	}
}

func TestDiscOfDataFiles(t *testing.T) {
	if n := discOfDataFiles([]string{"index.tsv.gz.age", "sidecars.par2", "disc07.squashfs.age"}); n != 7 {
		t.Fatalf("disc = %d, want 7", n)
	}
	if n := discOfDataFiles([]string{"index.tsv.gz.age", "sidecars.par2"}); n != 0 {
		t.Fatalf("disc = %d, want 0", n)
	}
}

// TestSidecarRepairHintNamesWhereTheParityIs covers the second half of HL-2:
// the advice used to send the operator to $STAGING/enc unconditionally, which
// is right after an ingest and wrong after a backup, where sidecars.par2 only
// ever exists in the disc directory.
func TestSidecarRepairHintNamesWhereTheParityIs(t *testing.T) {
	e := newEnv(t)
	enc := e.cfg.Dirs().Enc

	// Nothing staged: the disc is the only place the parity is.
	hint := e.opts().sidecarRepairHint(3)
	if strings.Contains(hint, enc) {
		t.Fatalf("hint points at staging when nothing is staged: %s", hint)
	}
	if !strings.Contains(hint, "par2 repair -- sidecars.par2 in disc 3's data/ directory") {
		t.Fatalf("hint does not name the disc: %s", hint)
	}

	// Disc 3's own set, staged under its per-disc name.
	if err := os.WriteFile(filepath.Join(enc, "sidecars-disc03.par2"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	hint = e.opts().sidecarRepairHint(3)
	if !strings.Contains(hint, "par2 repair -- sidecars-disc03.par2 in "+enc) {
		t.Fatalf("hint does not name the staged per-disc parity: %s", hint)
	}
	// Another disc's set is no use for disc 3's hashes and must not be offered.
	if err := os.WriteFile(filepath.Join(enc, "sidecars-disc01.par2"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hint := e.opts().sidecarRepairHint(3); strings.Contains(hint, "sidecars-disc01.par2") {
		t.Fatalf("hint offers another disc's parity: %s", hint)
	}
	// The index is on every disc, so any disc's set will do for it.
	if hint := e.opts().sidecarRepairHint(0); !strings.Contains(hint, "sidecars-disc01.par2 in "+enc) {
		t.Fatalf("hint for the index does not name a staged set: %s", hint)
	}

	// A staging area filled by brb.sh holds one set under the flat on-disc name.
	e2 := newEnv(t)
	if err := os.WriteFile(filepath.Join(e2.cfg.Dirs().Enc, sidecarsPar2), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hint := e2.opts().sidecarRepairHint(1); !strings.Contains(hint, "sidecars.par2 in "+e2.cfg.Dirs().Enc) {
		t.Fatalf("hint does not name a bash-written staged set: %s", hint)
	}
}

// fakeDisc lays out one disc of a set the way a burned disc reads: a data
// directory with the numbered image, its two hashes, the shared index, and a
// real sidecars.par2 over the small files, plus the disc's own SHA512SUMS.
func fakeDisc(t *testing.T, n int) string {
	t.Helper()
	mp := t.TempDir()
	data := filepath.Join(mp, dataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("disc%02d.squashfs", n)
	write := func(name string, body []byte) {
		if err := os.WriteFile(filepath.Join(data, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(base+ageExt, []byte(strings.Repeat(fmt.Sprintf("image %d\n", n), 40)))
	// The index and its hash are byte-identical on every disc of a real set, so
	// they must be here too: the point of the test is the one file that is not.
	write(indexName, []byte(strings.Repeat("shared index\n", 40)))
	perDisc := strings.Repeat(fmt.Sprintf("%x", n%16), 128)
	for nm, digest := range map[string]string{
		base + ageExt: perDisc,
		base:          perDisc,
		indexName:     strings.Repeat("ab", 64),
	} {
		if err := agecrypt.WriteSumFile(filepath.Join(data, nm+sumExt), digest, nm); err != nil {
			t.Fatal(err)
		}
	}

	inputs := []string{base + ageExt + sumExt, base + sumExt, indexName + sumExt, indexName}
	cmd := exec.Command("par2", append([]string{"create", "-q", "-r50", "-n1", "-b100", "--", sidecarsPar2}, inputs...)...)
	cmd.Dir = data
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create on disc %d: %v\n%s", n, err, out)
	}
	if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
		t.Fatal(err)
	}
	return mp
}

// TestIngestKeepsEveryDiscsSidecarParity is HL-5. Three discs, each with its
// own sidecars.par2 under the same on-disc name, ingested into one flat staging
// area: all three sets must survive, and each must still verify the hashes of
// the disc it came from.
func TestIngestKeepsEveryDiscsSidecarParity(t *testing.T) {
	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 is not installed")
	}
	e := newEnv(t)
	e.tools = tools.NewSet(nil)
	enc := e.cfg.Dirs().Enc

	for n := 1; n <= 3; n++ {
		ig := &ingester{o: e.opts(), mountPoint: fakeDisc(t, n)}
		if _, err := ig.ingestDisc(context.Background()); err != nil {
			t.Fatalf("ingesting disc %d: %v\n%s", n, err, e.log())
		}
	}

	for n := 1; n <= 3; n++ {
		set := fmt.Sprintf("sidecars-disc%02d.par2", n)
		if _, err := os.Stat(filepath.Join(enc, set)); err != nil {
			t.Fatalf("disc %d's sidecar parity did not survive the ingest: %v\n%s", n, err, e.log())
		}
		cmd := exec.Command("par2", "verify", "-q", "--", set)
		cmd.Dir = enc
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("par2 verify %s: %v\n%s", set, err, out)
		}
		want := fmt.Sprintf("disc%02d.squashfs.age.sha512", n)
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s does not cover %s:\n%s", set, want, out)
		}
	}
	// And the flat name must not be left lying about pretending to cover a disc
	// it does not.
	if _, err := os.Stat(filepath.Join(enc, sidecarsPar2)); err == nil {
		t.Fatal("a flat sidecars.par2 was staged as well as the per-disc sets")
	}
}
