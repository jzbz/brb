package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/backup"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// backupTools are the external programs a backup cannot be made without. age is
// not among them: brb links filippo.io/age as a library, so encryption needs no
// binary on PATH at all.
var backupTools = []string{tools.Mksquashfs, tools.Unsquashfs, tools.Par2, tools.Xorriso}

// restoreTools are the external programs a restore cannot be done without.
// This is the short list that has to keep working for the lifetime of the
// discs, which is why doctor reports it separately.
var restoreTools = []string{tools.Unsquashfs, tools.Par2}

// optionalTools improve some paths but are never required.
var optionalTools = []string{tools.Age, tools.Ddrescue, tools.Udisksctl, tools.Eject, tools.Findmnt, tools.Pv}

// doctor checks everything that can be checked without building anything:
// dependencies, the mksquashfs feature and compressor set brb relies on, the
// resolved configuration and its computed disc budgets, and whether the archive
// this configuration would produce could ever be decrypted again.
//
// It returns an error — and so exits non-zero — when anything required is
// missing or wrong.
func doctor(ctx context.Context, cfg *config.Config, cfgPath string, p *ui.Printer, ts *tools.Set) error {
	problems := 0

	p.Log("checking dependencies (backup side)")
	var missing []string
	for _, name := range backupTools {
		t := ts.Get(name)
		switch {
		case t.Found:
			p.OK("%-11s %s", name, t.Path)
		default:
			p.Warn("%-11s MISSING", name)
			missing = append(missing, name)
		}
	}
	p.Step("age is built in: brb encrypts and decrypts with the age library, not a binary")
	if err := ts.Require(missing...); err != nil {
		p.Fail("%v", err)
		problems++
	}

	p.Raw("")
	p.Log("checking dependencies (restore side — this is the set that matters long term)")
	for _, name := range restoreTools {
		if ts.Has(name) {
			p.OK("%-11s %s", name, ts.Get(name).Path)
		} else {
			p.Warn("%-11s MISSING", name)
		}
	}
	p.Step("nothing else is needed to restore: no python, no mksquashfs, no age binary")
	p.Step("the kernel alone can mount an image: mount -o loop,ro disc01.squashfs /mnt")
	p.Step("par2 is only needed when a disc has actually rotted")

	p.Raw("")
	p.Log("optional tools")
	for _, name := range optionalTools {
		if ts.Has(name) {
			p.OK("%-11s %s  (optional)", name, ts.Get(name).Path)
		} else {
			p.Step("%-11s not found (optional)", name)
		}
	}

	p.Raw("")
	p.Log("squashfs capabilities")
	if ts.Has(tools.Mksquashfs) {
		if ts.MksquashfsHasCpioStyle0(ctx) {
			p.OK("-cpiostyle0 supported (squashfs-tools 4.5 or newer)")
		} else {
			p.Warn("this mksquashfs has no -cpiostyle0; squashfs-tools 4.5 or newer is required")
			p.Step("without it brb cannot feed mksquashfs a file list containing odd names")
			problems++
		}
		comps := ts.MksquashfsCompressors(ctx)
		switch {
		case len(comps) == 0:
			p.Step("could not read the compressor list out of mksquashfs -help")
		default:
			p.Step("compressors built in: %s", strings.Join(comps, ", "))
			if !tools.NoCompression(cfg.Compression) && !contains(comps, cfg.Compression) {
				p.Warn("COMPRESSION=%s is not in that list — this mksquashfs may not support it", cfg.Compression)
				problems++
			}
		}
	} else {
		p.Warn("mksquashfs is missing, so its capabilities could not be checked")
	}

	// brb.sh passes -Xcompression-level only for zstd and gzip and says nothing
	// when the setting cannot apply, so an operator who sets COMPRESSION=xz and
	// COMPRESSION_LEVEL=9 gets neither the level nor a word about it.
	switch {
	case tools.NoCompression(cfg.Compression):
		p.Warn("COMPRESSION=%s: COMPRESSION_LEVEL=%d is ignored, nothing is compressed",
			cfg.Compression, cfg.CompressionLevel)
	case !tools.LevelApplies(cfg.Compression):
		p.Warn("COMPRESSION_LEVEL=%d is ignored for %s: mksquashfs takes -Xcompression-level for zstd, gzip and lzo only",
			cfg.CompressionLevel, cfg.Compression)
		p.Step("to tune %s, set COMPRESSION=zstd instead, or accept its default level", cfg.Compression)
	}

	p.Raw("")
	p.Log("tool versions")
	for _, t := range ts.All() {
		if !t.Found {
			continue
		}
		v := t.Version
		if v == "" {
			v = "(version not reported)"
		}
		p.Step("%-11s %s", t.Name, v)
	}
	p.Step("%-11s %s", "brb", version+" ("+runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH+")")

	p.Raw("")
	p.Log("configuration")
	if _, err := os.Stat(cfgPath); err == nil {
		p.Step("config file     %s", cfgPath)
	} else {
		p.Step("config file     %s  (not present; defaults and environment only)", cfgPath)
	}
	p.Step("source          %s", cfg.SourceDir)
	p.Step("archive name    %s", cfg.ArchiveName)
	p.Step("staging         %s", cfg.Staging)
	d := cfg.Dirs()
	p.Step("  images        %s", d.Img)
	p.Step("  ciphertext    %s", d.Enc)
	p.Step("  disc trees    %s", d.Discs)
	p.Step("  isos          %s", d.ISO)
	p.Step("disc type       %s  (%s)", cfg.DiscType, ui.HumanBytes(cfg.Capacity()))
	if cfg.DiscCapacityBytes > 0 {
		p.Step("                capacity overridden by DISC_CAPACITY_BYTES=%d", cfg.DiscCapacityBytes)
	}

	if b, err := cfg.Budget(); err != nil {
		p.Fail("disc budget: %v", err)
		problems++
	} else {
		p.Step("usable per disc %s  (98%% of the media, ISO 9660 overhead)", ui.HumanBytes(b.Usable))
		p.Step("reserve         %s  (README, MANIFEST, SHA512SUMS, index, brb)", ui.HumanBytes(b.Reserve))
		p.Step("max image size  %s  (after %d%% par2 recovery data)", ui.HumanBytes(b.Image), cfg.Par2Redundancy)
		p.Step("raw per disc    %s  at PACK_RATIO %.2f", ui.HumanBytes(rawPerDisc(b.Image, cfg.PackRatio)), cfg.PackRatio)
		p.Step("ratio adapts    %s", packRatioAdaptNote(cfg))
	}
	p.Step("compression     %s level %d, block %s", cfg.Compression, cfg.CompressionLevel, cfg.BlockSize)
	p.Step("par2            %d%%, %d blocks, %d MB memory", cfg.Par2Redundancy, cfg.Par2Blocks, cfg.Par2MemoryMB)
	jobs := fmt.Sprint(cfg.Jobs)
	if cfg.Jobs == 0 {
		jobs = fmt.Sprintf("0 (one per CPU: %d)", runtime.NumCPU())
	}
	p.Step("compressor jobs %s", jobs)
	p.Step("burner          %s at %dx", cfg.Burner, cfg.BurnSpeed)
	p.Step("iso mode        %s  (%s)", cfg.ISOMode, isoModeNote(cfg.ISOMode))
	p.Step("keep isos       %s  (after a successful burn)", boolInt(cfg.KeepISOs))
	p.Step("label prefix    %s", cfg.LabelPrefix)
	p.Step("prune dirs      %s", summariseList(cfg.PruneDirs))
	p.Step("exclude masks   %s", summariseList(cfg.ExcludeMasks))

	if err := cfg.Validate(); err != nil {
		p.Raw("")
		p.Log("configuration problems")
		for _, line := range strings.Split(err.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				p.Fail("%s", line)
				problems++
			}
		}
	}

	p.Raw("")
	p.Log("disc payload (the tool itself, carried on every disc)")
	reportPayload(cfg, p)

	p.Raw("")
	p.Log("encryption")
	problems += checkKeys(ctx, cfg, p)

	p.Raw("")
	if problems > 0 {
		return fmt.Errorf("%d problem(s) above must be fixed before running a backup", problems)
	}
	p.OK("ready")
	return nil
}

// reportPayload says where the copies of brb that go onto every disc come from,
// and which of them are actually there.
//
// Never a problem, always at most a warning: a set burned without the payload is
// still a complete, restorable set — every README's restore recipe uses nothing
// but sha512sum, par2, age and the kernel. Failing doctor over it would train an
// operator to ignore the one command that catches the failures that do matter.
func reportPayload(cfg *config.Config, p *ui.Printer) {
	dist, err := cfg.ResolveDistDir()
	if err != nil {
		p.Warn("%v", err)
	}
	if dist == "" {
		p.Warn("no dist directory — discs will carry only %s, the binary running now",
			backup.SelfCopyName())
		p.Step("build one with ./build-dist.sh, or set BRB_DIST_DIR")
		return
	}
	p.Step("dist            %s", dist)
	for _, name := range backup.PayloadNames() {
		fi, err := os.Stat(filepath.Join(dist, name))
		if err != nil || !fi.Mode().IsRegular() {
			p.Warn("%-17s MISSING — run ./build-dist.sh", name)
			continue
		}
		p.OK("%-17s %s", name, ui.HumanBytes(fi.Size()))
	}
}

// checkKeys reports on the recipients file and, when a secret key is available,
// proves that data encrypted to those recipients can be decrypted again.
//
// This is the one failure mode with no recovery path: a recipients file copied
// from another machine, or a second keypair minted into a different directory,
// produces a whole set of perfectly written, permanently unreadable discs. It
// costs milliseconds to rule out.
func checkKeys(ctx context.Context, cfg *config.Config, p *ui.Printer) int {
	problems := 0

	recips, err := agecrypt.ParseRecipientsFile(cfg.AgeRecipientsFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.Warn("no recipients file at %s", cfg.AgeRecipientsFile)
		p.Step("run 'brb init-key' to make one")
		return problems + 1
	case err != nil:
		p.Fail("recipients file %s: %v", cfg.AgeRecipientsFile, err)
		return problems + 1
	}
	p.OK("recipients file has %d key(s): %s", len(recips), cfg.AgeRecipientsFile)

	// Whether the second way in exists, as brb.sh's doctor reports it. It is
	// the one thing about the key setup that cannot be inferred from the round
	// trip below, and the day it matters is the day nobody can run doctor to
	// find out.
	rescuePath := filepath.Join(filepath.Dir(cfg.AgeRecipientsFile), rescueIdentityName)
	if _, err := os.Stat(rescuePath); err == nil {
		p.OK("rescue key present: %s", rescuePath)
	} else {
		p.Step("no rescue key in %s   (add one with 'brb init-key --rescue-key')", filepath.Dir(cfg.AgeRecipientsFile))
	}

	idPath := defaultIdentityPath(cfg)
	if _, err := os.Stat(idPath); err != nil {
		p.Warn("no readable identity at %s — cannot prove this set will ever be decryptable", idPath)
		p.Step("set AGE_IDENTITY to the secret key that matches those recipients")
		if _, err := os.Stat(idPath + ".age"); err == nil {
			p.Step("%s.age exists: a passphrase-protected identity cannot be tested unattended", idPath)
		}
		return problems
	}

	ids, err := agecrypt.ParseIdentityFile(idPath)
	if err != nil {
		p.Fail("identity %s: %v", idPath, err)
		return problems + 1
	}
	if err := roundTrip(ctx, recips, ids); err != nil {
		p.Fail("%s cannot decrypt data encrypted to %s: %v", idPath, cfg.AgeRecipientsFile, err)
		p.Step("every disc in this set would be unreadable — fix the recipients file,")
		p.Step("or point AGE_IDENTITY at the key that matches it")
		return problems + 1
	}
	p.OK("age round-trip verified against %s", idPath)
	return problems
}

// roundTrip encrypts a token to recipients and decrypts it back with ids,
// through the same code path a real image takes.
func roundTrip(ctx context.Context, recips []age.Recipient, ids []age.Identity) error {
	const token = "brb-roundtrip"

	dir, err := os.MkdirTemp("", "brb-doctor-")
	if err != nil {
		return fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	plain := filepath.Join(dir, "probe")
	if err := os.WriteFile(plain, []byte(token), 0o600); err != nil {
		return fmt.Errorf("writing the probe: %w", err)
	}
	enc := filepath.Join(dir, "probe.age")
	if _, err := agecrypt.Encrypt(ctx, plain, enc, recips, nil); err != nil {
		return fmt.Errorf("encrypting the probe: %w", err)
	}
	back := filepath.Join(dir, "probe.out")
	if _, err := agecrypt.Decrypt(ctx, enc, back, ids, nil); err != nil {
		return fmt.Errorf("decrypting the probe: %w", err)
	}
	got, err := os.ReadFile(back)
	if err != nil {
		return fmt.Errorf("reading the probe back: %w", err)
	}
	if string(got) != token {
		return errors.New("the decrypted probe does not match what was encrypted")
	}
	return nil
}

// defaultIdentityPath resolves AGE_IDENTITY the way brb.sh's resolve_identity
// does: the configured path, else identity.txt beside the recipients file.
func defaultIdentityPath(cfg *config.Config) string {
	if cfg.AgeIdentity != "" {
		return cfg.AgeIdentity
	}
	return filepath.Join(filepath.Dir(cfg.AgeRecipientsFile), "identity.txt")
}

// rawPerDisc converts a compressed-image budget into the raw-content budget the
// packer works in, matching brb.sh's raw_budget and internal/backup.
func rawPerDisc(imageBudget int64, ratio float64) int64 {
	if !(ratio > 0) {
		ratio = 1
	}
	return int64(float64(imageBudget) / ratio)
}

// packRatioAdaptNote says in one line whether the pack ratio will be re-learned
// as the run measures discs, and from what. The number above it — raw per disc
// — is only the starting point when it will be, which is the default.
func packRatioAdaptNote(cfg *config.Config) string {
	if !cfg.PackRatioAdapt {
		return "no  (PACK_RATIO_ADAPT=0: the ratio above is used for every disc)"
	}
	return fmt.Sprintf("yes  (worst of the last %d disc(s) measured, x%.2f margin)",
		cfg.PackRatioWindow, cfg.PackRatioMargin)
}

// summariseList renders a configured list compactly, naming the entries when
// there are few and counting them when there are many.
func summariseList(in []string) string {
	if len(in) == 0 {
		return "(none)"
	}
	if len(in) <= 4 {
		return strings.Join(in, ", ")
	}
	return fmt.Sprintf("%d entries: %s, ...", len(in), strings.Join(in[:3], ", "))
}

// contains reports whether list holds want.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
