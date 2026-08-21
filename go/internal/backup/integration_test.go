package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// realTools returns a detected tool set, skipping the test when anything a
// backup needs is missing. No test in this package requires root or a network.
func realTools(t *testing.T, ctx context.Context) *tools.Set {
	t.Helper()
	set := tools.Detect(ctx)
	if err := set.Require(tools.Mksquashfs, tools.Unsquashfs, tools.Par2, tools.Xorriso); err != nil {
		t.Skipf("skipping: %v", err)
	}
	if !set.MksquashfsHasCpioStyle0(ctx) {
		t.Skip("skipping: this mksquashfs has no -cpiostyle0")
	}
	return set
}

// integrationConfig builds a source tree of n files of the given size and a
// configuration whose disc budget forces the layout the caller wants.
//
// RESERVE_BYTES has to cover the copy of this program that goes onto every
// disc — here that is the test binary, which is several megabytes.
func integrationConfig(t *testing.T, files int, size int64) *config.Config {
	t.Helper()
	src := t.TempDir()
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < files; i++ {
		buf := make([]byte, size)
		if _, err := rnd.Read(buf); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(src, "file"+string(rune('a'+i))+".bin")
		if err := os.WriteFile(name, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := config.Default()
	c.SourceDir = src
	c.Staging = t.TempDir()
	c.ArchiveName = "integration-archive"
	c.DiscCapacityBytes = 32 << 20
	c.ReserveBytes = 12 << 20
	c.Compression = "none"
	c.CompressionLevel = 0
	c.BlockSize = "128K"
	c.Par2Redundancy = 10
	c.Par2Blocks = 20
	c.Par2MemoryMB = 64
	c.LabelPrefix = "TEST"
	c.PruneDirs = nil
	c.ExcludeMasks = nil
	return c
}

// keysFor writes an age keypair and points the configuration at it.
func keysFor(t *testing.T, c *config.Config) {
	t.Helper()
	dir := t.TempDir()
	id, err := agecrypt.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	c.AgeIdentity = filepath.Join(dir, "identity.txt")
	c.AgeRecipientsFile = filepath.Join(dir, "recipients.txt")
	if err := agecrypt.WriteIdentityFile(c.AgeIdentity, id); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.AppendRecipient(c.AgeRecipientsFile, id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
}

// enoughSpace skips the test when staging cannot hold the run.
func enoughSpace(t *testing.T, c *config.Config) {
	t.Helper()
	b, err := c.Budget()
	if err != nil {
		t.Fatal(err)
	}
	avail, err := freeSpace(c.Staging)
	if err != nil {
		t.Fatal(err)
	}
	if need := 4 * RequiredSpace(b.Image, c.Par2Redundancy); avail < need {
		t.Skipf("skipping: %s has %d bytes free, this test wants %d", c.Staging, avail, need)
	}
}

func yesPrinter() *ui.Printer {
	p := ui.New(io.Discard, false)
	p.SetAssumeYes(true)
	return p
}

// TestRunEndToEnd exercises the whole pipeline against the real tools: the
// shrink-retry loop (PACK_RATIO is deliberately optimistic, so the first image
// overshoots), encryption, par2, the round-trip verification, the disc layout,
// the manifest, the sums and the ISOs.
func TestRunEndToEnd(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	// No payload here, whatever this machine has installed: the disc payload
	// has its own tests, and this one asserts on the self-copy.
	noSystemDist(t)

	cfg := integrationConfig(t, 6, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	// 24 MiB of incompressible data against a ~17 MiB image budget, told to
	// expect 2:1 compression: disc 1 is planned oversized and must be re-packed
	// from the measured ratio.
	cfg.PackRatio = 0.5
	// The default is ondemand, which builds nothing;
	// TestOnDemandLeavesNoISOs covers that. This run asserts on the images, so
	// it asks for them.
	cfg.ISOMode = config.ISOEager

	if err := Run(ctx, Options{
		Cfg: cfg, UI: yesPrinter(), Tools: set, VerifyRoundTrip: true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	dirs := cfg.Dirs()
	total := discCount(t, dirs.Discs)
	if total < 2 {
		t.Fatalf("the set has %d disc(s), want at least 2", total)
	}

	// No plaintext image may survive.
	if left, err := os.ReadDir(dirs.Img); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("%s still holds %d plaintext image(s)", dirs.Img, len(left))
	}

	for n := 1; n <= total; n++ {
		dd := filepath.Join(dirs.Discs, discDirName(n))
		base := imageName(n)
		for _, want := range []string{
			"README.md", "MANIFEST.txt", agecrypt.SumsName, SelfCopyName(),
			filepath.Join("data", base+".age"),
			filepath.Join("data", base+".age.sha512"),
			filepath.Join("data", base+".sha512"),
			filepath.Join("data", base+".age.par2"),
			filepath.Join("data", indexName),
			filepath.Join("data", indexName+".sha512"),
		} {
			if _, err := os.Stat(filepath.Join(dd, want)); err != nil {
				t.Errorf("disc %d: %v", n, err)
			}
		}
		bad, err := agecrypt.VerifyDir(ctx, dd, filepath.Join(dd, agecrypt.SumsName))
		if err != nil {
			t.Fatalf("disc %d: verifying %s: %v", n, agecrypt.SumsName, err)
		}
		if len(bad) != 0 {
			t.Errorf("disc %d: %d file(s) failed their checksum: %v", n, len(bad), bad)
		}
		assertSidecarParity(t, ctx, set, n, dd)
		fi, err := os.Stat(filepath.Join(dirs.ISO, iso.Name(n)))
		if err != nil {
			t.Errorf("disc %d ISO: %v", n, err)
		} else if fi.Size() == 0 {
			t.Errorf("disc %d ISO is empty", n)
		}
	}

	// A completed run leaves no resume state: the file exists to say "this set is
	// half built", and keeping it would make staging claim an interruption that
	// never happened, so the next plain backup here would refuse to start.
	// What the state records mid-run is covered by the resume tests, which
	// interrupt a run for real.
	if _, err := os.Stat(filepath.Join(cfg.Staging, "state.json")); !os.IsNotExist(err) {
		t.Errorf("the finished run left its resume state behind (stat err: %v)", err)
	}

	// The index must decrypt to one line per file, mentioning every disc.
	ids, err := agecrypt.ParseIdentityFile(cfg.AgeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := agecrypt.DecryptTo(ctx, filepath.Join(dirs.Enc, indexName), &raw, ids); err != nil {
		t.Fatalf("decrypting the index: %v", err)
	}
	zr, err := gzip.NewReader(&raw)
	if err != nil {
		t.Fatalf("the index is not gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("the index has %d line(s), want 6:\n%s", len(lines), body)
	}
	discs := map[string]bool{}
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".bin") {
			t.Errorf("malformed index line %q", line)
			continue
		}
		discs[parts[0]] = true
	}
	if len(discs) != total {
		t.Errorf("the index mentions %d disc(s), the set has %d", len(discs), total)
	}

	// Finally: a finished set cannot be resumed, because there is nothing left
	// half built. Deleting the ISOs by accident is recovered with `brb iso all`,
	// not by resuming — a resume would have to rebuild images that are already
	// there and correct.
	if err := os.RemoveAll(dirs.ISO); err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set, Resume: true})
	if err == nil {
		t.Fatal("--resume over a finished set succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "no state to resume from") {
		t.Errorf("error %q does not explain that the set is already finished", err)
	}
}

// assertSidecarParity holds one finished disc to what sidecars.par2 is for: the
// set exists with at least one recovery volume, par2 itself verifies it from
// inside data/ (which is the only place it will ever be used from), and both
// files are in SHA512SUMS — which they can only be if the parity was built
// before the sums were written.
func assertSidecarParity(t *testing.T, ctx context.Context, set *tools.Set, n int, dd string) {
	t.Helper()
	data := filepath.Join(dd, "data")

	if _, err := os.Stat(filepath.Join(data, sidecarsPar2Name)); err != nil {
		t.Errorf("disc %d: %v", n, err)
		return
	}
	vols, err := filepath.Glob(filepath.Join(data, "sidecars.vol*.par2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) == 0 {
		t.Errorf("disc %d: sidecars.par2 has no recovery volumes", n)
	}

	// par2 is run from data/, exactly as a restorer sitting on the burned disc
	// would run it. A set built anywhere else would fail here.
	if err := set.Par2Verify(ctx, data, sidecarsPar2Name, nil); err != nil {
		t.Errorf("disc %d: par2 verify -- %s in %s = %v", n, sidecarsPar2Name, data, err)
	}

	sums, err := agecrypt.ReadSumFile(filepath.Join(dd, agecrypt.SumsName))
	if err != nil {
		t.Fatalf("disc %d: %v", n, err)
	}
	want := []string{"./data/" + sidecarsPar2Name, "./data/" + filepath.Base(vols[0])}
	for _, nm := range want {
		if _, ok := sums[nm]; !ok {
			t.Errorf("disc %d: %s is not in %s; the parity was written after the sums",
				n, nm, agecrypt.SumsName)
		}
	}

	// Every .sha512 sidecar and the encrypted index must be covered, not just
	// whichever ones happened to exist first.
	covered, err := sidecarNames(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, nm := range []string{imageName(n) + ".sha512", imageName(n) + ".age.sha512",
		indexName + ".sha512", indexName} {
		if !slices.Contains(covered, nm) {
			t.Errorf("disc %d: %s is not covered by %s", n, nm, sidecarsPar2Name)
		}
	}
}

// TestSidecarParityFailureIsOnlyAWarning replaces par2 with a wrapper that
// refuses the sidecar set and passes everything else through. The set has lost
// redundancy over its small files, which is worth a warning and nothing more:
// the images, their own parity, the manifest, the sums and the ISOs must all
// still be there.
func TestSidecarParityFailureIsOnlyAWarning(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	real := set.Get(tools.Par2)
	script := filepath.Join(t.TempDir(), "par2")
	body := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = \"" + sidecarsPar2Name + "\" ]; then\n" +
		"    echo 'par2: no.' >&2\n    exit 3\n  fi\ndone\nexec " + real.Path + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var replaced []tools.Tool
	for _, tl := range set.All() {
		if tl.Name == tools.Par2 {
			tl.Path = script
		}
		replaced = append(replaced, tl)
	}

	cfg := integrationConfig(t, 4, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05
	cfg.ISOMode = config.ISOEager

	var log bytes.Buffer
	p := ui.New(&log, false)
	p.SetAssumeYes(true)
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: tools.NewSet(replaced)}); err != nil {
		t.Fatalf("Run with a failing sidecar par2: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "could not protect the sidecar files on disc 1") {
		t.Errorf("the failure was not reported:\n%s", log.String())
	}
	// And reported again at the end. A real run prints hours of output between
	// the warning and the summary, and the disc is burned on the strength of
	// "backup complete": a loss of recovery data that scrolled away is a loss
	// the operator never learns about until a restore fifteen years later.
	out := log.String()
	i := strings.LastIndex(out, "backup complete")
	if i < 0 {
		t.Fatalf("the run never reported completion:\n%s", out)
	}
	tail := out[i:]
	if !strings.Contains(tail, "no sidecar recovery data on disc(s) 1") {
		t.Errorf("the closing summary does not name the disc that lost its sidecar parity:\n%s", tail)
	}

	dirs := cfg.Dirs()
	total := discCount(t, dirs.Discs)
	if total < 1 {
		t.Fatal("no discs were built")
	}
	for n := 1; n <= total; n++ {
		dd := filepath.Join(dirs.Discs, discDirName(n))
		// No half-written set may be left behind for SHA512SUMS to record.
		left, err := filepath.Glob(filepath.Join(dd, "data", "sidecars*.par2"))
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("disc %d kept %v after a failed sidecar par2", n, left)
		}
		// And the disc's own README must stop promising what is not there.
		// This is the end of the chain the operator's warning began: the
		// warning reaches whoever ran the backup, but the README is what
		// reaches the person holding the disc in fifteen years, and it is the
		// only one of the two that cannot be corrected afterwards.
		readme, err := os.ReadFile(filepath.Join(dd, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(readme)
		for _, bad := range []string{
			"sidecars.par2                    par2 index",
			"sidecars.vol*.par2",
			"par2 repair -- sidecars.par2",
		} {
			if strings.Contains(text, bad) {
				t.Errorf("disc %d README promises %q after its sidecar par2 failed", n, bad)
			}
		}
		if !strings.Contains(text, "no parity of their own") {
			t.Errorf("disc %d README does not say the small files are unprotected", n)
		}
		// The disc is otherwise complete and internally consistent.
		for _, want := range []string{agecrypt.SumsName, "MANIFEST.txt", "README.md",
			filepath.Join("data", imageName(n)+".age"),
			filepath.Join("data", imageName(n)+".age.par2")} {
			if _, err := os.Stat(filepath.Join(dd, want)); err != nil {
				t.Errorf("disc %d: %v", n, err)
			}
		}
		bad, err := agecrypt.VerifyDir(ctx, dd, filepath.Join(dd, agecrypt.SumsName))
		if err != nil {
			t.Fatalf("disc %d: %v", n, err)
		}
		if len(bad) != 0 {
			t.Errorf("disc %d: %d file(s) failed their checksum: %v", n, len(bad), bad)
		}
		if _, err := os.Stat(filepath.Join(dirs.ISO, iso.Name(n))); err != nil {
			t.Errorf("disc %d ISO: %v", n, err)
		}
	}
}

// TestOnDemandLeavesNoISOs is the default path, and the whole reason ISO_MODE
// exists: an ISO is a full second copy of its disc directory, so a backup that
// builds them all peaks at about 2.2x the compressed set. Under ondemand the
// backup must build none, must not tell the operator it has, and `brb iso all`
// must then produce exactly one per disc from the disc directories alone.
func TestOnDemandLeavesNoISOs(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	cfg := integrationConfig(t, 6, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05 // two discs, no shrink retry

	if cfg.ISOMode != config.ISOOnDemand {
		t.Fatalf("ISO_MODE defaults to %q, want %q", cfg.ISOMode, config.ISOOnDemand)
	}

	var log bytes.Buffer
	p := ui.New(&log, false)
	p.SetAssumeYes(true)
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: set}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	dirs := cfg.Dirs()
	total := discCount(t, dirs.Discs)
	if total < 2 {
		t.Fatalf("the set has %d disc(s), want at least 2", total)
	}

	// Nothing at all, not even an empty file: the ISO directory is created by
	// preflight, so "no ISOs" has to be asserted on its contents.
	ents, err := os.ReadDir(dirs.ISO)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("ISO_MODE=ondemand left %d file(s) in %s", len(ents), dirs.ISO)
	}

	// And the closing summary must not send the operator looking for them.
	out := log.String()
	if strings.Contains(out, "ISOs    : "+dirs.ISO) {
		t.Errorf("the summary claims the ISOs are in %s:\n%s", dirs.ISO, out)
	}
	for _, want := range []string{"built on demand", "brb iso all"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, out)
		}
	}

	// `brb iso all` is what materialises them, from the disc directories.
	if err := iso.Build(ctx, isoOpts(cfg, p, set), "all"); err != nil {
		t.Fatalf("iso all: %v\n%s", err, log.String())
	}
	for n := 1; n <= total; n++ {
		fi, err := os.Stat(filepath.Join(dirs.ISO, iso.Name(n)))
		if err != nil {
			t.Errorf("after 'iso all': %v", err)
			continue
		}
		src, err := dirBytes(ctx, filepath.Join(dirs.Discs, discDirName(n)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() < src {
			t.Errorf("disc %d ISO is %d bytes, its source tree is %d", n, fi.Size(), src)
		}
	}
}

// isoOpts bundles what internal/iso needs, as backup.Run does.
func isoOpts(cfg *config.Config, p *ui.Printer, set *tools.Set) iso.Options {
	return iso.Options{Cfg: cfg, UI: p, Tools: set, Version: Version}
}

// TestRunCarriesTheDistPayload builds two real sets: one with a dist directory
// holding all four artifacts, one with no dist directory at all. Every disc of
// the first has to carry the payload, with the cross-built binary winning over
// the one that is running, covered by SHA512SUMS and listed in the README; the
// second has to succeed anyway and say nothing about files it does not have.
func TestRunCarriesTheDistPayload(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	dist := t.TempDir()
	for _, name := range PayloadNames() {
		body := "cross-built " + name + strings.Repeat(" payload", 64)
		path := filepath.Join(dist, name)
		if err := os.WriteFile(path, []byte(body), PayloadMode(name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, PayloadMode(name)); err != nil {
			t.Fatal(err)
		}
	}

	cfg := integrationConfig(t, 6, 4<<20)
	cfg.DistDir = dist
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05 // two discs, no shrink retry

	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run with a payload: %v", err)
	}
	dirs := cfg.Dirs()
	total := discCount(t, dirs.Discs)
	if total < 2 {
		t.Fatalf("the set has %d disc(s), want at least 2", total)
	}

	for n := 1; n <= total; n++ {
		dd := filepath.Join(dirs.Discs, discDirName(n))
		sums, err := agecrypt.ReadSumFile(filepath.Join(dd, agecrypt.SumsName))
		if err != nil {
			t.Fatalf("disc %d: %v", n, err)
		}
		for _, name := range PayloadNames() {
			path := filepath.Join(dd, name)
			fi, err := os.Stat(path)
			if err != nil {
				t.Errorf("disc %d: %v", n, err)
				continue
			}
			if got := fi.Mode().Perm(); got != PayloadMode(name) {
				t.Errorf("disc %d: %s has mode %o, want %o", n, name, got, PayloadMode(name))
			}
			// The payload is a release artifact cross-built for both
			// architectures; the binary that happens to be running must not
			// have overwritten it.
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(body), "cross-built "+name) {
				t.Errorf("disc %d: %s is not the file from the dist directory", n, name)
			}
			// writeSums runs after the payload, so the hashes cover it.
			if _, ok := sums["./"+name]; !ok {
				t.Errorf("disc %d: %s is not in %s", n, name, agecrypt.SumsName)
			}
		}
		bad, err := agecrypt.VerifyDir(ctx, dd, filepath.Join(dd, agecrypt.SumsName))
		if err != nil {
			t.Fatalf("disc %d: verifying %s: %v", n, agecrypt.SumsName, err)
		}
		if len(bad) != 0 {
			t.Errorf("disc %d: %d file(s) failed their checksum: %v", n, len(bad), bad)
		}
		assertREADMEMatchesDisc(t, n, dd)
	}

	// And now the same thing with nothing to carry. A missing payload must
	// never fail a backup, and the README must not describe one.
	noSystemDist(t)
	bare := integrationConfig(t, 4, 4<<20)
	keysFor(t, bare)
	enoughSpace(t, bare)
	bare.PackRatio = 1.05

	if err := Run(ctx, Options{Cfg: bare, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run without a dist directory: %v", err)
	}
	bareDirs := bare.Dirs()
	bareTotal := discCount(t, bareDirs.Discs)
	if bareTotal < 1 {
		t.Fatalf("the payload-less set has %d disc(s)", bareTotal)
	}
	for n := 1; n <= bareTotal; n++ {
		dd := filepath.Join(bareDirs.Discs, discDirName(n))
		// Only the self-copy is there, and it is not called plain "brb".
		if _, err := os.Stat(filepath.Join(dd, SelfCopyName())); err != nil {
			t.Errorf("disc %d: %v", n, err)
		}
		for _, name := range []string{"brb", "brb.sh", "brb-src.tar.gz"} {
			if _, err := os.Stat(filepath.Join(dd, name)); err == nil {
				t.Errorf("disc %d carries %s with no dist directory", n, name)
			}
		}
		assertREADMEMatchesDisc(t, n, dd)
	}
}

// assertREADMEMatchesDisc holds the README to the disc it is on: every copy of
// brb in the root is listed, and no copy that is not there is mentioned
// anywhere in the document. A restorer with one disc and a bad day should not
// be sent looking for a file that was never written.
func assertREADMEMatchesDisc(t *testing.T, n int, dd string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dd, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(body)

	seen := map[string]bool{}
	for _, name := range append(PayloadNames(), SelfCopyName()) {
		if seen[name] {
			continue
		}
		seen[name] = true
		_, statErr := os.Stat(filepath.Join(dd, name))
		switch present, listed := statErr == nil, strings.Contains(readme, name); {
		case present && !listed:
			t.Errorf("disc %d carries %s but its README does not list it", n, name)
		case !present && listed:
			t.Errorf("disc %d does not carry %s but its README names it", n, name)
		}
	}
	if strings.Contains(readme, "{{") || strings.Contains(readme, "<no value>") {
		t.Errorf("disc %d README did not render:\n%s", n, readme)
	}
}

// TestRunRefusesToRestartOverAFinishedSet proves the guard that keeps a second
// backup from scribbling over the first one's staging directory.
func TestRunRefusesToRestartOverAFinishedSet(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	cfg := integrationConfig(t, 2, 1<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)

	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set})
	if err == nil {
		t.Fatal("a second Run without --resume succeeded, want an error")
	}
	// The set is FINISHED, so its state file is gone and there is nothing to
	// resume. Telling the operator to pass --resume here would send them to
	// resume a run that already completed; the useful advice is that staging
	// already holds a set and they should clear it or point somewhere else.
	if !strings.Contains(err.Error(), "no run to resume") {
		t.Errorf("error %q does not explain that the set is already finished", err)
	}
	if !strings.Contains(err.Error(), cfg.Staging) {
		t.Errorf("error %q does not name the staging directory to clear", err)
	}

	// And --resume over a finished set is refused too: there is no state left.
	err = Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set, Resume: true})
	if err == nil {
		t.Fatal("--resume over a finished set succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "no state to resume from") {
		t.Errorf("error %q does not explain that there is no state to resume from", err)
	}
}

// TestResumeContinuesAtTheNextDisc rewinds a finished set to the state an
// interruption after disc 1 would have left, then resumes it. The packer must
// re-seed from the recorded assignment and build exactly the missing disc.
func TestResumeContinuesAtTheNextDisc(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	cfg := integrationConfig(t, 6, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05 // ~17 MiB budget, 24 MiB of data: two discs, no retry

	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dirs := cfg.Dirs()
	if got := discCount(t, dirs.Discs); got != 2 {
		t.Fatalf("the reference set has %d disc(s), want 2", got)
	}
	index := readIndex(t, ctx, cfg)

	// Rewind to "disc 1 finished, then the machine went down".
	var firstDisc []string
	var firstLines []string
	for _, line := range index {
		if strings.HasPrefix(line, "1\t") {
			firstDisc = append(firstDisc, strings.TrimPrefix(line, "1\t"))
			firstLines = append(firstLines, line)
		}
	}
	if len(firstDisc) == 0 || len(firstDisc) == len(index) {
		t.Fatalf("disc 1 holds %d of %d file(s); the fixture is wrong", len(firstDisc), len(index))
	}
	for _, dir := range []string{dirs.Discs, dirs.ISO} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, nm := range []string{indexName, indexName + ".sha512"} {
		if err := os.Remove(filepath.Join(dirs.Enc, nm)); err != nil {
			t.Fatal(err)
		}
	}
	second, err := filesMatching(dirs.Enc, func(n string) bool { return strings.HasPrefix(n, "disc02.") })
	if err != nil {
		t.Fatal(err)
	}
	if len(second) == 0 {
		t.Fatal("no disc02 artefacts to remove; the fixture is wrong")
	}
	for _, nm := range second {
		if err := os.Remove(filepath.Join(dirs.Enc, nm)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirs.Work, indexFileName),
		[]byte(strings.Join(firstLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Build the state an interrupted run would have left. It is constructed
	// rather than read back from the finished run, because a completed run
	// removes its state file — there is nothing to load.
	if err := SaveState(filepath.Join(cfg.Staging, "state.json"), &State{
		Version:   StateVersion,
		Archive:   cfg.ArchiveName,
		Source:    cfg.SourceDir,
		DiscsDone: 1,
		Assigned:  firstDisc,
		PackRatio: cfg.PackRatio,
	}); err != nil {
		t.Fatal(err)
	}

	// Resuming must rebuild only what is missing.
	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set, Resume: true}); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if got := discCount(t, dirs.Discs); got != 2 {
		t.Errorf("after resuming the set has %d disc(s), want 2", got)
	}
	if got := readIndex(t, ctx, cfg); len(got) != len(index) {
		t.Errorf("after resuming the index has %d line(s), want %d", len(got), len(index))
	}
	// The resume completed the set, so its state file must be gone: leaving it
	// would make staging claim an interruption that is over.
	if _, err := os.Stat(filepath.Join(cfg.Staging, "state.json")); !os.IsNotExist(err) {
		t.Errorf("the completed resume left its state behind (stat err: %v)", err)
	}
}

// readIndex decrypts and decompresses the on-disc index, returning its lines.
func readIndex(t *testing.T, ctx context.Context, cfg *config.Config) []string {
	t.Helper()
	ids, err := agecrypt.ParseIdentityFile(cfg.AgeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := agecrypt.DecryptTo(ctx, filepath.Join(cfg.Dirs().Enc, indexName), &raw, ids); err != nil {
		t.Fatalf("decrypting the index: %v", err)
	}
	zr, err := gzip.NewReader(&raw)
	if err != nil {
		t.Fatalf("the index is not gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// discCount counts the discNN directories in a staging discs directory.
func discCount(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "disc") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return len(names)
}

// ---------------------------------------------------------------------------
// Adaptive PACK_RATIO
// ---------------------------------------------------------------------------

// ratioTree writes n files of the given size into dir.
//
// With compressible set, every 4 KiB chunk is 1 KiB of random bytes followed by
// 3 KiB of zeros, which zstd takes to roughly a quarter of its size — the shape
// of a real tree of documents and source code, rather than the 1000:1 of a file
// of zeros, which would tell us nothing except that the clamp floor works.
// Without it the files are random and nothing compresses them at all.
func ratioTree(t *testing.T, dir string, n int, size int64, compressible bool) {
	t.Helper()
	rnd := rand.New(rand.NewSource(7))
	buf := make([]byte, size)
	for i := 0; i < n; i++ {
		if _, err := rnd.Read(buf); err != nil {
			t.Fatal(err)
		}
		if compressible {
			for off := 0; off < len(buf); off += 4096 {
				end := off + 4096
				if end > len(buf) {
					end = len(buf)
				}
				for j := off + 1024; j < end; j++ {
					buf[j] = 0
				}
			}
		}
		name := filepath.Join(dir, fmt.Sprintf("file%02d.bin", i))
		if err := os.WriteFile(name, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ratioConfig is a configuration over an existing source tree, with a disc
// budget small enough that a handful of megabytes needs several discs. Each run
// gets its own staging directory, so two runs can be compared over one tree.
func ratioConfig(t *testing.T, src string) *config.Config {
	t.Helper()
	c := config.Default()
	c.SourceDir = src
	c.Staging = t.TempDir()
	c.ArchiveName = "ratio-archive"
	c.DiscCapacityBytes = 32 << 20
	// Room for the copy of this program that goes onto every disc, which here
	// is the test binary.
	c.ReserveBytes = 12 << 20
	c.Compression = "zstd"
	c.CompressionLevel = 3
	c.BlockSize = "128K"
	c.Par2Redundancy = 10
	c.Par2Blocks = 20
	c.Par2MemoryMB = 64
	c.LabelPrefix = "TEST"
	c.PruneDirs = nil
	c.ExcludeMasks = nil
	keysFor(t, c)
	enoughSpace(t, c)
	return c
}

// capturingPrinter returns a printer that answers every prompt yes and keeps
// what it printed. The Printer serialises its own writes, and the buffer is
// only read once the run has returned.
func capturingPrinter() (*ui.Printer, *bytes.Buffer) {
	var buf bytes.Buffer
	p := ui.New(&buf, false)
	p.SetAssumeYes(true)
	return p, &buf
}

// TestAdaptingTheRatioNeedsFewerDiscs is the whole point of the feature, run
// twice over one tree: PACK_RATIO starts at the safe 1.00 both times, and the
// only difference is whether each finished disc is allowed to correct it.
//
// Without adaptation every disc after the first is planned as if nothing
// compresses, so the set is padded out with discs that are three quarters
// empty. With it, disc 1's measurement plans disc 2, and the same bytes fit on
// strictly fewer discs — without a single over-budget rebuild.
func TestAdaptingTheRatioNeedsFewerDiscs(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	src := t.TempDir()
	ratioTree(t, src, 32, 2<<20, true)

	fixed := ratioConfig(t, src)
	fixed.PackRatioAdapt = false
	fixedUI, fixedLog := capturingPrinter()
	if err := Run(ctx, Options{Cfg: fixed, UI: fixedUI, Tools: set}); err != nil {
		t.Fatalf("PACK_RATIO_ADAPT=0 run: %v", err)
	}
	fixedDiscs := discCount(t, fixed.Dirs().Discs)

	adaptive := ratioConfig(t, src)
	adaptiveUI, adaptiveLog := capturingPrinter()
	if !adaptive.PackRatioAdapt {
		t.Fatal("PACK_RATIO_ADAPT is not on by default")
	}
	if err := Run(ctx, Options{Cfg: adaptive, UI: adaptiveUI, Tools: set}); err != nil {
		t.Fatalf("adaptive run: %v", err)
	}
	adaptiveDiscs := discCount(t, adaptive.Dirs().Discs)

	t.Logf("PACK_RATIO_ADAPT=0: %d disc(s); PACK_RATIO_ADAPT=1: %d disc(s)", fixedDiscs, adaptiveDiscs)
	if fixedDiscs < 2 {
		t.Fatalf("the fixed-ratio run needed %d disc(s); this test cannot show an improvement", fixedDiscs)
	}
	if adaptiveDiscs >= fixedDiscs {
		t.Errorf("adapting the ratio used %d disc(s), the fixed ratio used %d; want strictly fewer",
			adaptiveDiscs, fixedDiscs)
	}

	// The fixed run must not have adapted, and must have kept the ratio it was
	// given: the only thing that may move it is the shrink-retry, which needs
	// an overshoot, and there is none here.
	if strings.Contains(fixedLog.String(), "pack ratio 1.000 ->") {
		t.Errorf("PACK_RATIO_ADAPT=0 adapted anyway:\n%s", fixedLog.String())
	}
	// The state file is gone by now: a completed run removes it, so that staging
	// never claims an interruption that did not happen. What the run learned is
	// asserted from what it reported instead.
	if strings.Contains(fixedLog.String(), "disc(s) measured") {
		t.Errorf("PACK_RATIO_ADAPT=0 recorded measurements:\n%s", fixedLog.String())
	}

	// The adaptive run must say what it did, and end below the ratio it
	// started from.
	out := adaptiveLog.String()
	if !strings.Contains(out, "pack ratio 1.000 ->") {
		t.Errorf("the adaptive run never reported a change of ratio:\n%s", out)
	}
	// An adaptation that overshot would cost a full rebuild of a multi-gigabyte
	// image on a real set, which is exactly what the margin is for.
	if strings.Contains(out, "over the") || strings.Contains(out, "re-packing") {
		t.Errorf("the adaptive run overshot a disc budget:\n%s", out)
	}
	// The ratio it moved TO must be below the 1.000 it started from, which is the
	// whole point: a "change" that went up would use more discs, not fewer.
	m := regexp.MustCompile(`pack ratio 1\.000 -> ([0-9.]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not read the new ratio out of the log:\n%s", out)
	}
	if v, err := strconv.ParseFloat(m[1], 64); err != nil || !(v < 1.0) {
		t.Errorf("the adaptive run moved the ratio to %q, want a value below 1.0", m[1])
	}

	// Fewer discs must still mean all of the data. Both sets index the same 32
	// files, and every disc of the adaptive set verifies against its own sums.
	for _, c := range []*config.Config{fixed, adaptive} {
		if lines := readIndex(t, ctx, c); len(lines) != 32 {
			t.Errorf("%s: the index has %d line(s), want 32", c.Staging, len(lines))
		}
	}
	for n := 1; n <= adaptiveDiscs; n++ {
		dd := filepath.Join(adaptive.Dirs().Discs, discDirName(n))
		bad, err := agecrypt.VerifyDir(ctx, dd, filepath.Join(dd, agecrypt.SumsName))
		if err != nil {
			t.Fatalf("disc %d: verifying %s: %v", n, agecrypt.SumsName, err)
		}
		if len(bad) != 0 {
			t.Errorf("disc %d: %d file(s) failed their checksum: %v", n, len(bad), bad)
		}
	}
}

// TestIncompressibleContentHoldsTheRatioAtTheTop is the other direction, and
// the one that would have been catastrophic to get wrong: content that does
// not compress must leave the estimate at the top of its range. An estimate
// that collapsed to the clamp floor here — the bash bug — would plan fifty
// disc-budgets of files onto the next disc, build the image, reject it and
// rebuild it.
//
// "The top" is measured*margin and not 1.000. A squashfs of incompressible
// content is slightly LARGER than the bytes that went into it (stored blocks,
// plus the inode, directory and fragment tables, plus padding), so planning
// the next disc at exactly 1.000 plans it to overshoot, and an overshoot costs
// a second full mksquashfs pass over the whole disc. The margin is what keeps
// that from happening once per disc for a set of photos or video.
func TestIncompressibleContentHoldsTheRatioAtTheTop(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	src := t.TempDir()
	ratioTree(t, src, 16, 2<<20, false)

	cfg := ratioConfig(t, src)
	p, log := capturingPrinter()
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: set}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	total := discCount(t, cfg.Dirs().Discs)
	if total < 2 {
		t.Fatalf("the set has %d disc(s), want at least 2 so a second disc is planned from the first", total)
	}
	// A completed run removes its state file, so what each disc actually measured
	// is read back from what the run reported: one "compressed to X of raw" per
	// disc, every one of them near 1.0 because none of this content compresses.
	out := log.String()
	ms := regexp.MustCompile(`compressed to ([0-9.]+) of raw`).FindAllStringSubmatch(out, -1)
	if len(ms) != total {
		t.Errorf("the run reported %d measurement(s) for %d disc(s):\n%s", len(ms), total, out)
	}
	for _, m := range ms {
		if v, err := strconv.ParseFloat(m[1], 64); err != nil || v < 0.9 {
			t.Errorf("measured ratio %q on incompressible content", m[1])
		}
	}
	// Every move the estimator makes must be upward. Downward on this content
	// is the bash bug, and 1.000 exactly is the clamp that used to discard the
	// shrink retry's correction and buy a rebuild on every disc.
	for _, m := range regexp.MustCompile(`pack ratio [0-9.]+ -> ([0-9.]+)`).FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err != nil || v <= 1.0 {
			t.Errorf("the estimate moved to %q on incompressible content, want a ratio above 1.0:\n%s", m[1], out)
		}
	}
	// No overshoot, therefore no rebuild: the estimate never planned more onto
	// a disc than fitted.
	if strings.Contains(out, "over the") || strings.Contains(out, "re-packing") {
		t.Errorf("a disc overshot its budget:\n%s", out)
	}
}

// TestAResumedRunKeepsWhatItLearned. The measurements are the estimator's whole
// memory: a set continued days later must plan its next disc from them, not
// from the configured guess it has already disproved.
func TestAResumedRunKeepsWhatItLearned(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	src := t.TempDir()
	ratioTree(t, src, 24, 2<<20, true)

	cfg := ratioConfig(t, src)

	// Interrupt the run for real rather than resuming a finished set: a completed
	// run removes its state file, so "resume a set that is already done" is not a
	// thing an operator can do, and a test that did it proved nothing about the
	// path that matters.
	runCtx, cancel := context.WithCancel(ctx)
	p, _ := capturingPrinter()
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, Options{Cfg: cfg, UI: p, Tools: set}) }()

	statePath := filepath.Join(cfg.Staging, "state.json")
	var st *State
	for i := 0; i < 4000; i++ {
		if s, err := LoadState(statePath); err == nil && s.DiscsDone >= 1 {
			st = s
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the run finished before it could be interrupted: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if st == nil {
		t.Fatal("never saw a completed disc to interrupt after")
	}
	if len(st.MeasuredRatios) == 0 {
		t.Fatal("the interrupted run recorded no measurements")
	}

	// What the resume carries in is the estimator's whole memory: a set continued
	// days later must plan from what it measured, not from the guess it disproved.
	resumeUI, resumeLog := capturingPrinter()
	if err := Run(ctx, Options{Cfg: cfg, UI: resumeUI, Tools: set, Resume: true}); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	out := resumeLog.String()
	want := fmt.Sprintf("carrying forward %d measured ratio(s)", len(st.MeasuredRatios))
	if !strings.Contains(out, want) {
		t.Errorf("the resume does not report %q:\n%s", want, out)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("the completed resume left its state file behind at %s", statePath)
	}
}

// loadStateFor reads the resume state a run left behind.
func loadStateFor(t *testing.T, cfg *config.Config) *State {
	t.Helper()
	st, err := LoadState(filepath.Join(cfg.Staging, "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return st
}

// TestResumeOverStaleRecoveryData covers the window a kill lands in most often
// on real media: par2 over a 25 GB image runs for the better part of an hour,
// and the state file is only written after it. A run killed in there leaves a
// complete recovery set for a disc the state does not yet count as done, and
// par2 refuses to write over one — so the resume redid the whole mksquashfs and
// encrypt and then failed at the last step, self-healing only because the failed
// Par2Create swept the set away on its way out.
func TestResumeOverStaleRecoveryData(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)

	cfg := integrationConfig(t, 6, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05 // ~17 MiB budget, 24 MiB of data: two discs, no retry

	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dirs := cfg.Dirs()
	if got := discCount(t, dirs.Discs); got != 2 {
		t.Fatalf("the reference set has %d disc(s), want 2", got)
	}
	index := readIndex(t, ctx, cfg)

	var firstDisc, firstLines []string
	for _, line := range index {
		if strings.HasPrefix(line, "1\t") {
			firstDisc = append(firstDisc, strings.TrimPrefix(line, "1\t"))
			firstLines = append(firstLines, line)
		}
	}
	if len(firstDisc) == 0 || len(firstDisc) == len(index) {
		t.Fatalf("disc 1 holds %d of %d file(s); the fixture is wrong", len(firstDisc), len(index))
	}
	// Rewind to "disc 2 was interrupted inside its par2 pass": the disc
	// directories and the encrypted index are gone, but every disc02 artefact in
	// enc/ — the ciphertext AND its recovery volumes — is still there.
	for _, dir := range []string{dirs.Discs, dirs.ISO} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, nm := range []string{indexName, indexName + ".sha512"} {
		if err := os.Remove(filepath.Join(dirs.Enc, nm)); err != nil {
			t.Fatal(err)
		}
	}
	stale, err := filesMatching(dirs.Enc, func(n string) bool {
		return strings.HasPrefix(n, "disc02.squashfs.age") && strings.HasSuffix(n, ".par2")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) == 0 {
		t.Fatal("the finished run left no disc02 recovery volumes; the fixture is wrong")
	}
	if err := os.WriteFile(filepath.Join(dirs.Work, indexFileName),
		[]byte(strings.Join(firstLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(filepath.Join(cfg.Staging, "state.json"), &State{
		Version:   StateVersion,
		Archive:   cfg.ArchiveName,
		Source:    cfg.SourceDir,
		DiscsDone: 1,
		Assigned:  firstDisc,
		PackRatio: cfg.PackRatio,
	}); err != nil {
		t.Fatal(err)
	}

	// The first resume must finish. Before the sweep it failed here with
	// "par2 failed: exit status 3 / Par2 file already exists".
	if err := Run(ctx, Options{Cfg: cfg, UI: yesPrinter(), Tools: set, Resume: true}); err != nil {
		t.Fatalf("resuming over stale recovery data: %v", err)
	}
	if got := discCount(t, dirs.Discs); got != 2 {
		t.Errorf("after resuming the set has %d disc(s), want 2", got)
	}
	if got := readIndex(t, ctx, cfg); len(got) != len(index) {
		t.Errorf("after resuming the index has %d line(s), want %d", len(got), len(index))
	}
}

// TestRunPublicArchive covers PUBLIC_ARCHIVE end to end: the set is encrypted
// to a keypair minted for it alone, that key is written onto every disc, and
// the images decrypt with nothing but what the disc carries.
//
// The point of the mode is that the archive keeps no secret, so the assertions
// are about the key being present and usable rather than protected.
func TestRunPublicArchive(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	cfg := integrationConfig(t, 4, 4<<20)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05
	cfg.PublicArchive = true
	// Deliberately not keysFor(t, cfg): a public archive must not need a
	// recipients file to exist, and must not read one if it does.
	cfg.AgeRecipientsFile = filepath.Join(t.TempDir(), "no-such-recipients.txt")

	var log bytes.Buffer
	p := ui.New(&log, false)
	p.SetAssumeYes(true)
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: set}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	if _, err := os.Lstat(cfg.AgeRecipientsFile); err == nil {
		t.Error("the run created a recipients file; a public archive must not need one")
	}
	if !strings.Contains(log.String(), "NOT be confidential") {
		t.Errorf("the run did not warn that the set is not confidential:\n%s", log.String())
	}

	dirs := cfg.Dirs()
	total := discCount(t, dirs.Discs)
	if total < 1 {
		t.Fatalf("no discs were produced")
	}

	var first string
	for n := 1; n <= total; n++ {
		dd := filepath.Join(dirs.Discs, discDirName(n))
		key := filepath.Join(dd, doc.PublicIdentityName)
		st, err := os.Stat(key)
		if err != nil {
			t.Fatalf("disc %d carries no %s: %v", n, doc.PublicIdentityName, err)
		}
		// World-readable on purpose: 0400 would imply a confidentiality this
		// file does not have, and it is about to be pressed onto a disc.
		if got := st.Mode().Perm(); got != 0o644 {
			t.Errorf("disc %d: %s has mode %04o, want 0644", n, doc.PublicIdentityName, got)
		}
		if n == 1 {
			first = dd
		}
	}

	// The same key must appear in all three places, so that one of them
	// rotting does not cost the reader the archive.
	keyRe := regexp.MustCompile(`AGE-SECRET-KEY-1[A-Z0-9]{20,}`)
	fromFile := keyRe.FindString(readFileString(t, filepath.Join(first, doc.PublicIdentityName)))
	if fromFile == "" {
		t.Fatal("no secret key in the on-disc identity file")
	}
	for _, name := range []string{"MANIFEST.txt", "README.md"} {
		if got := keyRe.FindString(readFileString(t, filepath.Join(first, name))); got != fromFile {
			t.Errorf("%s carries key %q, want the same one as %s (%q)",
				name, got, doc.PublicIdentityName, fromFile)
		}
	}

	// And the README must not still tell the reader the key is elsewhere.
	readme := readFileString(t, filepath.Join(first, "README.md"))
	if strings.Contains(readme, "never will be") {
		t.Error("the public README still claims the key is not on the disc")
	}

	// The whole point: decrypt using only the disc's own key.
	ids, err := agecrypt.ParseIdentityFile(filepath.Join(first, doc.PublicIdentityName))
	if err != nil {
		t.Fatalf("the on-disc identity does not parse: %v", err)
	}
	img := filepath.Join(first, "data", imageName(1)+".age")
	out := filepath.Join(t.TempDir(), "disc01.squashfs")
	if _, err := agecrypt.Decrypt(ctx, img, out, ids, nil); err != nil {
		t.Fatalf("decrypting with the disc's own key: %v", err)
	}
}

// TestRunOrdinaryArchivePublishesNoKey is the other half of the public-archive
// contract: with the mode off, nothing about a set changes. Without this, the
// assertions above could be satisfied by code that published a key always.
func TestRunOrdinaryArchivePublishesNoKey(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	cfg := integrationConfig(t, 4, 4<<20)
	keysFor(t, cfg)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05
	if cfg.PublicArchive {
		t.Fatal("PublicArchive defaults to true; it must be opt-in")
	}

	var log bytes.Buffer
	p := ui.New(&log, false)
	p.SetAssumeYes(true)
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: set}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}

	dirs := cfg.Dirs()
	keyRe := regexp.MustCompile(`AGE-SECRET-KEY-1[A-Z0-9]{20,}`)
	for n := 1; n <= discCount(t, dirs.Discs); n++ {
		dd := filepath.Join(dirs.Discs, discDirName(n))
		if _, err := os.Lstat(filepath.Join(dd, doc.PublicIdentityName)); err == nil {
			t.Errorf("disc %d carries %s but PUBLIC_ARCHIVE was off", n, doc.PublicIdentityName)
		}
		for _, name := range []string{"MANIFEST.txt", "README.md"} {
			if got := keyRe.FindString(readFileString(t, filepath.Join(dd, name))); got != "" {
				t.Errorf("disc %d: %s leaks a secret key (%q)", n, name, got)
			}
		}
	}
}

// readFileString reads a file the test cannot proceed without.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestPublicArchiveSurvivesInterruptionAndResume is the regression test for a
// critical defect: the minted key used to live only in process memory until the
// end of a completed run, so a public backup interrupted mid-set took its key
// with it, and the resume minted a new one, encrypted the rest of the set to
// that, and stamped it onto every disc — including the discs whose images were
// encrypted to the dead key. Those verified clean and were undecryptable
// forever. The run here is interrupted for real, after disc 1 is protected and
// state is saved, exactly the window the defect lived in.
func TestPublicArchiveSurvivesInterruptionAndResume(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	src := t.TempDir()
	ratioTree(t, src, 24, 2<<20, false) // incompressible: several discs, slow enough to catch
	cfg := ratioConfig(t, src)
	cfg.PublicArchive = true
	cfg.AgeRecipientsFile = filepath.Join(t.TempDir(), "no-such-recipients.txt")
	cfg.AgeIdentity = ""

	runCtx, cancel := context.WithCancel(ctx)
	p, _ := capturingPrinter()
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, Options{Cfg: cfg, UI: p, Tools: set}) }()

	statePath := filepath.Join(cfg.Staging, "state.json")
	var st *State
	for i := 0; i < 8000; i++ {
		if s, err := LoadState(statePath); err == nil && s.DiscsDone >= 1 {
			st = s
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the run finished before it could be interrupted: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if st == nil {
		t.Fatal("never saw a completed disc to interrupt after")
	}

	// What the defect hinged on: at this moment the key must already be on
	// disk, and the state must know the set is public and which key it is.
	if !st.PublicArchive || st.PublicKey == "" {
		t.Fatalf("interrupted state does not record the public archive: %+v", st)
	}
	keyPath := filepath.Join(cfg.Dirs().Enc, doc.PublicIdentityName)
	keyA, err := agecrypt.ReadX25519IdentityFile(keyPath)
	if err != nil {
		t.Fatalf("the interrupted run left no readable key at %s: %v", keyPath, err)
	}
	if keyA.Recipient().String() != st.PublicKey {
		t.Fatalf("staging key %s does not match state %s", keyA.Recipient(), st.PublicKey)
	}

	// The two ways a resume could change its mind, both refused. Resuming
	// without the flag: this state is public. Both must leave staging alone.
	cfgOff := *cfg
	cfgOff.PublicArchive = false
	keysFor(t, &cfgOff)
	if err := Run(ctx, Options{Cfg: &cfgOff, UI: yesPrinter(), Tools: set, Resume: true}); err == nil {
		t.Fatal("resuming a public set without --public-archive was accepted")
	} else if !errors.Is(err, ErrStateMismatch) || !strings.Contains(err.Error(), "--public-archive") {
		t.Fatalf("resume without the flag: got %v, want an ErrStateMismatch naming --public-archive", err)
	}

	// Now the real resume, with the flag. It must reload key A, not mint.
	resumeUI, resumeLog := capturingPrinter()
	if err := Run(ctx, Options{Cfg: cfg, UI: resumeUI, Tools: set, Resume: true}); err != nil {
		t.Fatalf("resumed Run: %v\n%s", err, resumeLog.String())
	}
	if !strings.Contains(resumeLog.String(), "resumed with its recorded key") {
		t.Errorf("the resume did not report reloading the key:\n%s", resumeLog.String())
	}
	if strings.Contains(resumeLog.String(), "a keypair was generated") {
		t.Errorf("the resume minted a new keypair:\n%s", resumeLog.String())
	}

	// Every disc — the ones from before the interruption and the ones after —
	// must decrypt with the identity.txt that disc itself carries, and all
	// those files must be key A.
	dirs := cfg.Dirs()
	total := discCount(t, dirs.Discs)
	if total < 2 {
		t.Fatalf("the set has %d disc(s); the interruption did not split it", total)
	}
	keyRe := regexp.MustCompile(`AGE-SECRET-KEY-1[A-Z0-9]{20,}`)
	for n := 1; n <= total; n++ {
		dd := filepath.Join(dirs.Discs, discDirName(n))
		onDisc, err := agecrypt.ReadX25519IdentityFile(filepath.Join(dd, doc.PublicIdentityName))
		if err != nil {
			t.Fatalf("disc %d: %v", n, err)
		}
		if onDisc.String() != keyA.String() {
			t.Errorf("disc %d carries a different key than the one the set was started with", n)
		}
		for _, name := range []string{"MANIFEST.txt", "README.md"} {
			if got := keyRe.FindString(readFileString(t, filepath.Join(dd, name))); got != keyA.String() {
				t.Errorf("disc %d: %s prints key %q, want the set's key", n, name, got)
			}
		}
		img := filepath.Join(dd, "data", imageName(n)+".age")
		out := filepath.Join(t.TempDir(), imageName(n))
		if _, err := agecrypt.Decrypt(ctx, img, out, []age.Identity{onDisc}, nil); err != nil {
			t.Errorf("disc %d does not decrypt with the key it carries: %v", n, err)
		}
	}
	// A finished set removes its state; the key stays in staging with the
	// rest of the ciphertext until the operator wipes it.
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("the completed resume left its state behind (stat err: %v)", err)
	}
}

// TestOrdinaryArchiveRefusesToTurnPublicOnResume is the mirror: adding the flag
// to a set that began ordinary would encrypt the remaining discs to a minted
// key and publish that key onto discs it cannot open.
func TestOrdinaryArchiveRefusesToTurnPublicOnResume(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	src := t.TempDir()
	ratioTree(t, src, 24, 2<<20, false)
	cfg := ratioConfig(t, src)

	runCtx, cancel := context.WithCancel(ctx)
	p, _ := capturingPrinter()
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, Options{Cfg: cfg, UI: p, Tools: set}) }()
	statePath := filepath.Join(cfg.Staging, "state.json")
	seen := false
	for i := 0; i < 8000 && !seen; i++ {
		if s, err := LoadState(statePath); err == nil && s.DiscsDone >= 1 {
			seen = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the run finished before it could be interrupted: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !seen {
		t.Fatal("never saw a completed disc to interrupt after")
	}

	pub := *cfg
	pub.PublicArchive = true
	err := Run(ctx, Options{Cfg: &pub, UI: yesPrinter(), Tools: set, Resume: true})
	if err == nil {
		t.Fatal("resuming an ordinary set with --public-archive was accepted")
	}
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("got %v, want ErrStateMismatch", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Dirs().Enc, doc.PublicIdentityName)); !os.IsNotExist(err) {
		t.Errorf("the refused resume left a key in staging (stat err: %v)", err)
	}
}

// TestPublicArchiveRoundTripUsesTheMintedKey: --verify-roundtrip used to load
// the operator's AGE_IDENTITY, which is not a recipient of a public archive, so
// the combination failed on disc 1 after mksquashfs, encrypt and par2 — or, with
// no operator identity at all, was refused in preflight. The only key that can
// round-trip a public set is the archive's own.
func TestPublicArchiveRoundTripUsesTheMintedKey(t *testing.T) {
	ctx := context.Background()
	set := realTools(t, ctx)
	noSystemDist(t)

	cfg := integrationConfig(t, 4, 4<<20)
	enoughSpace(t, cfg)
	cfg.PackRatio = 1.05
	cfg.PublicArchive = true
	// No operator identity anywhere: a public archive must not need one, even
	// for the round-trip.
	cfg.AgeRecipientsFile = filepath.Join(t.TempDir(), "no-such-recipients.txt")
	cfg.AgeIdentity = filepath.Join(t.TempDir(), "no-such-identity.txt")

	p, log := capturingPrinter()
	p.SetAssumeYes(true)
	if err := Run(ctx, Options{Cfg: cfg, UI: p, Tools: set, VerifyRoundTrip: true}); err != nil {
		t.Fatalf("Run with --verify-roundtrip on a public archive: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "using the archive's own key") {
		t.Errorf("round-trip did not use the minted key:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "round-trip") {
		t.Errorf("no evidence the round-trip ran at all:\n%s", log.String())
	}
}
