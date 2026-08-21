package backup

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/pack"
	"github.com/jzbz/brb/internal/scan"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// toolLog forwards a subprocess's output to the printer, one step line per
// line. It never reports an error: losing a log line must not fail a build.
type toolLog struct{ p *ui.Printer }

func (t toolLog) Write(b []byte) (int, error) {
	line := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(line) != "" {
		t.p.Step("%s", line)
	}
	return len(b), nil
}

// Run performs a complete backup:
//
//	preflight -> scan -> per disc { pack, build image, size check with
//	shrink-retry, prove the image is readable, encrypt and hash in one pass,
//	par2, optional round-trip verification, delete the plaintext, record the
//	index, save state } -> encrypted index -> disc directories, each with
//	sidecars.par2 over its small files -> manifest -> dist payload -> README
//	and a copy of this program -> SHA512SUMS -> ISOs, under ISO_MODE=eager only.
//
// The order of the last few steps matters: sidecars.par2 is built while the
// disc directories are laid out, so SHA512SUMS covers it, and the README is
// rendered after the payload so it lists what is actually on its disc.
//
// Nothing here builds an ISO under the default ISO_MODE=ondemand; `burn` builds
// each one as its disc goes into the drive, and `iso` materialises them on
// request.
//
// With Options.DryRun it stops after planning, like the `plan` command. With
// Options.Resume it continues an interrupted set from <Staging>/state.json.
func Run(ctx context.Context, o Options) error {
	r, err := newRunner(o)
	if err != nil {
		return err
	}
	if o.DryRun {
		res, err := r.scan(ctx)
		if err != nil {
			return err
		}
		_, err = r.layout(ctx, res)
		return err
	}
	if r.tools == nil {
		r.tools = tools.Detect(ctx)
	}
	// Deferred BEFORE preflight, not after: preflight takes the staging lock
	// partway through and can still refuse the run after that (a resume whose
	// state disagrees, too little space, a declined confirmation). Releasing
	// only on the success path leaked the lock for the life of the process,
	// which a one-shot CLI hides and a second Run in the same process does not.
	defer r.releaseStaging()
	if err := r.preflight(ctx); err != nil {
		return err
	}

	res, err := r.scan(ctx)
	if err != nil {
		return err
	}
	entries := r.resumeFilter(res)
	if err := r.checkIndexCovers(entries); err != nil {
		return err
	}

	total, err := r.buildDiscs(ctx, entries)
	if err != nil {
		return err
	}
	if err := r.buildIndex(ctx); err != nil {
		return err
	}
	if err := r.buildDiscDirs(ctx, total); err != nil {
		return err
	}
	if err := r.writeManifest(ctx, total); err != nil {
		return err
	}
	// Before the READMEs: the payload's cross-built brb-linux-<arch> must win
	// over the copy of the running binary, and each README lists what is
	// actually in the root of its disc.
	if err := r.writePayload(ctx, total); err != nil {
		return err
	}
	if err := r.writeReadmes(ctx, total); err != nil {
		return err
	}
	if err := r.writeSums(ctx, total); err != nil {
		return err
	}
	if err := r.checkDiscSizes(ctx, total); err != nil {
		return err
	}
	if err := r.buildISOs(ctx, total); err != nil {
		return err
	}
	r.finish(total)
	return nil
}

// resumeFilter drops the files a previous run already wrote to a disc, which
// re-seeds the packer with what is left. Filtering the scan is equivalent to
// seeding an assigned set: every name of a hard link group travels to the same
// disc, so a group is either wholly assigned or wholly outstanding.
func (r *runner) resumeFilter(res *scan.Result) []scan.Entry {
	if r.st.ScanRawSize == 0 {
		r.st.ScanRawSize = res.RawBytes
	} else if r.st.ScanRawSize != res.RawBytes {
		r.p.Warn("the source tree measured %s when this set was started and %s now; "+
			"files added since will be included, files removed since are simply absent",
			ui.HumanBytes(r.st.ScanRawSize), ui.HumanBytes(res.RawBytes))
	}
	if r.st.DiscsDone == 0 {
		return res.Entries
	}

	assigned := r.st.assignedSet()
	out := make([]scan.Entry, 0, len(res.Entries))
	done, todo := 0, 0
	for _, e := range res.Entries {
		if e.Kind == scan.KindFile {
			if _, ok := assigned[e.Rel]; ok {
				done++
				continue
			}
			todo++
		}
		out = append(out, e)
	}
	if missing := len(assigned) - done; missing > 0 {
		r.p.Warn("%d file(s) recorded on an earlier disc are no longer in the source tree; "+
			"they remain on the discs already written", missing)
	}
	r.p.Step("resume: %d file(s) already on disc, %d still to write", done, todo)
	return out
}

// checkIndexCovers refuses a resume that would build discs the encrypted index
// can never list. A run that got as far as buildIndex deleted the plaintext
// index, and prepareState marks such a resume indexBuilt: the encrypted index
// is kept as it is and copied onto every disc. That is only right if there is
// nothing left to write. If the source tree grew since — files added under
// SOURCE_DIR, or files that were unreadable then and are readable now — the
// scan hands buildDiscs those files, it packs them onto NEW discs, and the
// index on every disc, including the new ones, says nothing about them: a
// restore of the whole set would never look for them, and `restore --only`
// could not name them. There is no way to extend a finished index short of
// rebuilding it, and the plaintext it was built from is gone, so the honest
// answer is to refuse.
//
// entries is the scan after resumeFilter, so every KindFile left in it is a
// file no finished disc holds.
func (r *runner) checkIndexCovers(entries []scan.Entry) error {
	if !r.indexBuilt {
		return nil
	}
	pending := 0
	for _, e := range entries {
		if e.Kind == scan.KindFile {
			pending++
		}
	}
	if pending == 0 {
		return nil
	}
	return fmt.Errorf("backup: --resume: the encrypted index was already built for the %d finished disc(s), "+
		"but %d file(s) are not on any disc (the source tree changed since); the index cannot be "+
		"extended — start over", r.st.DiscsDone, pending)
}

// buildDiscs runs the per-disc loop until every file is on a disc.
func (r *runner) buildDiscs(ctx context.Context, entries []scan.Entry) (int, error) {
	p := pack.New(entries)
	base := r.st.DiscsDone
	r.p.Log("building images (image budget %s per disc)", ui.HumanBytes(r.budget.Image))

	for {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("backup: aborted: %w", err)
		}
		rb := rawBudget(r.budget.Image, r.packRatio)
		if over := p.Oversized(rb); len(over) > 0 {
			return 0, oversizedError(over, rb)
		}
		bin, ok := p.Next(rb)
		if !ok {
			break
		}
		if err := r.oneDisc(ctx, p, bin, base+p.Committed()+1); err != nil {
			return 0, err
		}
	}

	total := base + p.Committed()
	if total == 0 {
		return 0, fmt.Errorf("backup: nothing was packed — is %s empty after pruning?", r.cfg.SourceDir)
	}
	r.p.OK("%d image(s) built", total)
	return total, nil
}

// oneDisc builds, protects and records a single disc. Nothing is marked
// assigned until every step of it has succeeded, so an interrupted disc is
// simply rebuilt by the next run.
func (r *runner) oneDisc(ctx context.Context, p *pack.Packer, bin *pack.Bin, n int) error {
	img := filepath.Join(r.dirs.Img, imageName(n))

	bin, size, err := r.buildImage(ctx, p, bin, n, img)
	if err != nil {
		return err
	}
	if err := r.protect(ctx, n, img, size); err != nil {
		return err
	}
	if err := appendIndex(filepath.Join(r.dirs.Work, indexFileName), n, bin.Files); err != nil {
		return err
	}
	if err := p.Commit(bin); err != nil {
		return fmt.Errorf("backup: disc %d: %w", n, err)
	}

	r.st.DiscsDone = n
	r.st.Assigned = append(r.st.Assigned, bin.Files...)
	r.st.PackRatio = r.packRatio
	r.st.MeasuredRatios = append(r.st.MeasuredRatios[:0], r.est.measured...)
	if err := SaveState(r.statePath, r.st); err != nil {
		return err
	}
	r.p.Step("state saved: %d disc(s) done", n)
	return nil
}

// buildImage builds one disc's squashfs image, re-planning the bin from the
// measured compression ratio when the image overshoots its budget.
//
// The returned bin is the one the image was actually built from, which is not
// necessarily the one passed in.
func (r *runner) buildImage(ctx context.Context, p *pack.Packer, bin *pack.Bin, n int, img string) (*pack.Bin, int64, error) {
	for attempt := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("backup: aborted: %w", err)
		}
		r.p.Step("disc %d: packing %d files, %s raw", n, len(bin.Files), ui.HumanBytes(bin.RawBytes))

		files := make([]string, 0, len(bin.Skeleton)+len(bin.Files))
		files = append(files, bin.Skeleton...)
		files = append(files, bin.Files...)
		if err := r.tools.BuildImage(ctx, tools.MkOptions{
			SourceDir:   r.cfg.SourceDir,
			Out:         img,
			Files:       files,
			Compression: r.cfg.Compression,
			Level:       r.cfg.CompressionLevel,
			BlockSize:   r.cfg.BlockSize,
			Processors:  r.cfg.Jobs,
			Xattrs:      true,
			Log:         toolLog{r.p},
		}); err != nil {
			return nil, 0, fmt.Errorf("backup: disc %d: %w", n, err)
		}

		fi, err := os.Stat(img)
		if err != nil {
			return nil, 0, fmt.Errorf("backup: disc %d image: %w", n, err)
		}
		size := fi.Size()
		if size <= r.budget.Image {
			ratio := measuredRatio(size, bin.RawBytes)
			r.p.OK("disc %d image: %s (compressed to %.3f of raw)", n, ui.HumanBytes(size), ratio)
			// Correct the guess from what this disc actually did. buildDiscs
			// re-derives the raw budget from r.packRatio before it plans the next
			// bin, so the correction takes effect on the very next disc.
			r.adapt(ratio)
			return bin, size, nil
		}

		attempt++
		if attempt > r.cfg.MaxShrinkAttempts {
			// Upward, not downward: the raw budget is imageBudget/PACK_RATIO, so a
			// HIGHER ratio packs LESS raw content per disc, which is the only way
			// out of a persistent overshoot. The figure carries the same 5% margin
			// the shrink loop itself re-packs with, because at exactly the measured
			// ratio the disc only fits by integer truncation.
			return nil, 0, fmt.Errorf("backup: disc %d is still %s after %d shrink attempt(s), "+
				"over the %s budget; set PACK_RATIO to at least %.3f and re-run",
				n, ui.HumanBytes(size), r.cfg.MaxShrinkAttempts, ui.HumanBytes(r.budget.Image),
				shrinkRatio(size, bin.RawBytes))
		}
		r.p.Warn("disc %d came out at %s, over the %s budget",
			n, ui.HumanBytes(size), ui.HumanBytes(r.budget.Image))

		// The measured ratio is the truth; re-plan this bin with it, plus margin.
		r.packRatio = shrinkRatio(size, bin.RawBytes)
		rb := rawBudget(r.budget.Image, r.packRatio)
		r.p.Step("re-packing disc %d with measured ratio %.3f (raw budget %s)",
			n, r.packRatio, ui.HumanBytes(rb))

		if err := os.Remove(img); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, 0, fmt.Errorf("backup: removing oversized image %s: %w", img, err)
		}
		next, ok := p.Next(rb)
		if !ok || len(next.Files) == 0 {
			return nil, 0, emptyBinError(n, p, rb, r.packRatio)
		}
		bin = next
	}
}

// emptyBinError explains a re-pack that has nothing left to put on the disc.
//
// This is almost always the "file larger than one disc" case discovered late.
// buildDiscs ran pack.Oversized against the budget derived from the ratio it
// GUESSED; the shrink loop has since replaced that with the ratio the content
// actually achieved, which is smaller, so a unit that sat between the two
// budgets got past the up-front check and is only now impossible. Naming it,
// with the three ways out oversizedError already gives that case up front, is
// the whole point.
//
// The message this replaced said "lower PACK_RATIO manually", which cannot
// work: rb here is imageBudget divided by the MEASURED ratio, and PACK_RATIO
// only seeds the first attempt of a disc. A re-run at any PACK_RATIO packs the
// same first bin, measures the same overshoot, shrinks to the same budget and
// stops in the same place — the operator would have been sent round that loop
// for as long as they were willing.
func emptyBinError(n int, p *pack.Packer, rb int64, ratio float64) error {
	if over := p.Oversized(rb); len(over) > 0 {
		return fmt.Errorf("backup: disc %d: at the compression this content actually achieves "+
			"(measured ratio %.3f) one disc holds %s of raw content, and %s",
			n, ratio, ui.HumanBytes(rb), oversizedDetail(over, rb))
	}
	// Not reachable from a tree that has anything left in it, since a unit that
	// does not fit is by definition oversized at this budget. Kept as a message
	// rather than a panic: an unexplained abort hours into a run is worse than
	// a sentence that says what was being attempted.
	return fmt.Errorf("backup: re-packing disc %d produced an empty bin at a raw budget of %s "+
		"(measured ratio %.3f) with nothing oversized — exclude the largest remaining files via "+
		"EXCLUDE_MASKS, or use larger media (DISC_TYPE=bdxl100)", n, ui.HumanBytes(rb), ratio)
}

// protect proves the image is readable, encrypts and hashes it in one pass,
// generates the par2 recovery data, optionally decrypts it back to compare the
// plaintext hash, and only then removes the plaintext.
func (r *runner) protect(ctx context.Context, n int, img string, size int64) error {
	base := imageName(n)
	enc := filepath.Join(r.dirs.Enc, base+".age")

	// Before anything destructive: a squashfs that cannot be read back is not a
	// backup, and this is the last moment at which the source is still intact.
	stats, err := r.tools.ImageStats(ctx, img)
	if err != nil {
		return fmt.Errorf("backup: disc %d image is not a readable squashfs: %w", n, err)
	}
	if line := firstLine(stats); line != "" {
		r.p.Step("%s", line)
	}

	r.p.Step("encrypting and hashing %s", base)
	prog := r.p.NewProgress("encrypt "+base, size)
	sums, err := agecrypt.Encrypt(ctx, img, enc, r.recipients, prog.Writer())
	prog.Done()
	if err != nil {
		return fmt.Errorf("backup: disc %d: %w", n, err)
	}
	if err := agecrypt.WriteSumFile(filepath.Join(r.dirs.Enc, base+".sha512"), sums.Plain, base); err != nil {
		return fmt.Errorf("backup: disc %d: %w", n, err)
	}
	if err := agecrypt.WriteSumFile(filepath.Join(r.dirs.Enc, base+".age.sha512"), sums.Cipher, base+".age"); err != nil {
		return fmt.Errorf("backup: disc %d: %w", n, err)
	}
	r.discCipher[n] = sums.Cipher

	// Size the recovery geometry from the ciphertext actually written, not from
	// the plaintext image: par2 protects the .age file, and a fixed block count
	// on a multi-gigabyte image collapses the damage tolerance the parity
	// exists to provide. See config.Par2BlockCount.
	encSize := size
	if fi, serr := os.Stat(enc); serr == nil {
		encSize = fi.Size()
	}
	geom := config.Par2GeometryFor(encSize, r.cfg.Par2Blocks, r.cfg.Par2Redundancy)
	r.p.Step("generating %d%% recovery data (%d blocks of %s, %d recovery blocks)",
		r.cfg.Par2Redundancy, geom.Blocks, ui.HumanBytes(geom.BlockSize), geom.RecoveryBlocks)
	// par2 refuses to write over an existing recovery set, and a run killed
	// anywhere in this disc's par2/round-trip window left one behind with
	// state.json still short of this disc — so the resume that rebuilds it would
	// redo the whole mksquashfs and encrypt only to fail here. Sweep first, as
	// protectSidecars does for its own set. Only discs being rebuilt reach
	// protect(), so this can never delete a finished disc's parity. Unlike the
	// sidecar set, this one is mandatory: a removal that fails is fatal.
	//
	// The names are matched one by one, never through filepath.Glob with the
	// directory in the pattern: a STAGING path holding '[', '*' or '?' would
	// make such a glob match nothing, and the stale set would survive to make
	// par2 refuse. The predicate is the same one discPayload uses to collect
	// the set later, so nothing this sweep leaves can be picked up as payload.
	stale, err := filesMatching(r.dirs.Enc, func(nm string) bool {
		return strings.HasPrefix(nm, base+".age") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil {
		return fmt.Errorf("backup: disc %d: looking for stale recovery data: %w", n, err)
	}
	for _, f := range stale {
		if err := os.Remove(filepath.Join(r.dirs.Enc, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("backup: disc %d: removing stale %s: %w", n, f, err)
		}
	}
	par2Started := time.Now()
	if err := r.tools.Par2Create(ctx, tools.Par2Options{
		Dir:        r.dirs.Enc,
		File:       base + ".age",
		Redundancy: r.cfg.Par2Redundancy,
		Blocks:     geom.Blocks,
		MemoryMB:   r.cfg.Par2MemoryMB,
		Log:        toolLog{r.p},
	}); err != nil {
		return fmt.Errorf("backup: disc %d: %w", n, err)
	}
	// par2 is normally the longest step of a disc by a wide margin, and preflight
	// can only guess at it. Report what it actually cost, so from disc 1 onward
	// the operator is planning a twenty-disc campaign from a measurement rather
	// than from an estimate.
	r.p.Step("recovery data for disc %d took %s", n, ui.HumanDuration(time.Since(par2Started)))

	if err := r.roundTrip(ctx, n, enc, size, sums.Plain); err != nil {
		return err
	}

	if err := os.Remove(img); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: removing plaintext image %s: %w", img, err)
	}
	r.p.OK("%s encrypted, protected, plaintext removed", base)
	return nil
}

// roundTrip decrypts a freshly written image back and compares its hash with
// the plaintext hash taken while encrypting, so the plaintext is never deleted
// with nothing having proved the ciphertext decrypts at all.
//
// The plaintext is hashed as it streams and never stored. Only the digest is
// wanted here, and writing the decrypted image to a temporary file — which is
// what this did until the file was noticed to have no reader — cost an
// image-sized sequential write and an fsync per disc, up to 95 GB on BD-XL
// media, for bytes that were unlinked moments later. It is also what made
// [RequiredSpace] charge staging for a third image-sized file.
func (r *runner) roundTrip(ctx context.Context, n int, enc string, size int64, want string) error {
	if !r.opts.VerifyRoundTrip || len(r.identities) == 0 {
		return nil
	}

	r.p.Step("verifying disc %d decrypts back to the same bytes", n)
	prog := r.p.NewProgress("verify disc"+fmt.Sprintf("%02d", n), size)
	h := sha512.New()
	derr := agecrypt.DecryptTo(ctx, enc, io.MultiWriter(h, prog.Writer()), r.identities)
	prog.Done()
	got := hex.EncodeToString(h.Sum(nil))
	if derr != nil {
		return fmt.Errorf("backup: disc %d round-trip: %w", n, derr)
	}
	if got != want {
		return fmt.Errorf("backup: disc %d round-trip mismatch: the decrypted image hashes to %s, "+
			"the original hashed to %s — refusing to delete the plaintext", n, got, want)
	}
	r.p.OK("disc %d round-trip verified", n)
	return nil
}

// buildIndex compresses and encrypts the "which disc holds what" index. It is
// copied onto every disc, so it is written once, at the end, when every disc's
// contents are known.
func (r *runner) buildIndex(ctx context.Context) error {
	r.p.Log("building encrypted index")
	out := filepath.Join(r.dirs.Enc, indexName)
	if r.indexBuilt {
		for _, nm := range []string{out, out + ".sha512"} {
			if _, err := os.Stat(nm); err != nil {
				return fmt.Errorf("backup: index %s: %w", nm, err)
			}
		}
		r.p.Step("index already built, keeping it")
		return nil
	}

	idx := filepath.Join(r.dirs.Work, indexFileName)
	fi, err := os.Stat(idx)
	if err != nil {
		return fmt.Errorf("backup: index %s: %w", idx, err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("backup: index %s is empty", idx)
	}

	gz := idx + ".gz"
	if err := gzipFile(ctx, idx, gz); err != nil {
		return err
	}
	defer func() { _ = os.Remove(gz) }()

	sums, err := agecrypt.Encrypt(ctx, gz, out, r.recipients, nil)
	if err != nil {
		return fmt.Errorf("backup: encrypting the index: %w", err)
	}
	if err := agecrypt.WriteSumFile(filepath.Join(r.dirs.Enc, indexName+".sha512"), sums.Cipher, indexName); err != nil {
		return fmt.Errorf("backup: index: %w", err)
	}
	r.indexCipher = sums.Cipher
	st, err := os.Stat(out)
	if err != nil {
		return fmt.Errorf("backup: index %s: %w", out, err)
	}
	r.p.OK("index: %s (encrypted, on every disc)", ui.HumanBytes(st.Size()))
	return nil
}

// discPayload lists the files that belong in one disc's data/ directory.
func (r *runner) discPayload(n int) ([]string, error) {
	base := imageName(n)
	required := []string{base + ".age", base + ".age.sha512", base + ".sha512",
		indexName, indexName + ".sha512"}
	for _, nm := range required {
		if _, err := os.Stat(filepath.Join(r.dirs.Enc, nm)); err != nil {
			return nil, fmt.Errorf("backup: disc %d is missing %s: %w", n, nm, err)
		}
	}
	par2s, err := filesMatching(r.dirs.Enc, func(nm string) bool {
		return strings.HasPrefix(nm, base+".age") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil {
		return nil, err
	}
	if len(par2s) == 0 {
		return nil, fmt.Errorf("backup: disc %d has no par2 recovery files in %s", n, r.dirs.Enc)
	}
	return append(required, par2s...), nil
}

// buildDiscDirs lays out one directory per disc, exactly as it will be burned.
// Each artefact is hard-linked so staging does not hold a second copy of every
// image; a copy is only made when the link cannot be created.
func (r *runner) buildDiscDirs(ctx context.Context, total int) error {
	r.p.Log("laying out discs")
	for n := 1; n <= total; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("backup: aborted: %w", err)
		}
		data := filepath.Join(r.dirs.Discs, discDirName(n), "data")
		if err := os.MkdirAll(data, 0o755); err != nil {
			return fmt.Errorf("backup: creating %s: %w", data, err)
		}
		names, err := r.discPayload(n)
		if err != nil {
			return err
		}
		for _, nm := range names {
			src := filepath.Join(r.dirs.Enc, nm)
			if err := linkOrCopy(ctx, src, filepath.Join(data, nm), 0o644); err != nil {
				return err
			}
		}
		// Before the size check, so the parity is counted against the media,
		// and long before writeSums, so SHA512SUMS covers it.
		r.protectSidecars(ctx, n, data)

		// Likewise before writeSums, so the published key is hashed with
		// everything else rather than sitting on the disc unaccounted for.
		if err := r.writePublicIdentity(n); err != nil {
			return err
		}

		size, err := dirBytes(ctx, data)
		if err != nil {
			return err
		}
		if size > r.budget.Usable {
			return r.oversizedDisc(n, "data/ payload", size)
		}
	}
	r.p.OK("%d disc directory(ies)", total)
	return nil
}

// oversizedDisc is the refusal for a disc directory that has outgrown the
// media, with advice that is actually true. The old advice was "raise
// RESERVE_BYTES and re-run", which cannot help a resume: Usable is
// capacity*98/100 and ignores the reserve, and the images already built were
// sized for the old reserve, so nothing about them shrinks. Raising the
// reserve does work — a larger reserve makes a smaller image budget, and the
// next disc built under it leaves that much more room for the files every
// disc carries — but only for images built after the change, which means
// starting the set over. Say so, and say how much: the reserve has to grow
// by at least the overshoot.
func (r *runner) oversizedDisc(n int, what string, size int64) error {
	over := size - r.budget.Usable
	return fmt.Errorf("backup: disc %d needs %s for its %s but the media holds about %s (%s over); "+
		"the files every disc carries outgrew RESERVE_BYTES=%d — set RESERVE_BYTES to at least %d "+
		"and start the set over: the images already built were sized for the old reserve, so "+
		"--resume cannot shrink them", n, ui.HumanBytes(size), what, ui.HumanBytes(r.budget.Usable),
		ui.HumanBytes(over), r.cfg.ReserveBytes, r.cfg.ReserveBytes+over)
}

// The published key lives at the disc root rather than in data/, beside
// README.md, which is where that README's manual restore recipe points
// (age -d -i /mnt/identity.txt). Both readers' ingest copy it from there into
// <staging>/enc/identity.txt — the same place this writer keeps it — and their
// identity search uses that file in addition to any configured identity, so a
// public set opens with nothing configured on either reader.
//
// Being outside data/ also means it is not a member of sidecars.par2 — par2
// will not take a path above its own working directory. It is protected
// instead by being written three times per disc: as doc.PublicIdentityName,
// in MANIFEST.txt and in README.md. An age secret key is 74 bech32 characters
// with a checksum, so a surviving copy can be retyped and verified even if the
// other two rot.

// publicIdentityText is the archive's secret key as a string under
// PUBLIC_ARCHIVE, and "" otherwise. Every public-archive passage in the
// manifest and the on-disc READMEs is conditioned on it, so those documents
// cannot describe a published key for a set that does not carry one.
func (r *runner) publicIdentityText() string {
	if r.publicIdentity == nil {
		return ""
	}
	return r.publicIdentity.String()
}

// writePublicIdentity puts the archive's secret key on disc n. It is a no-op
// unless PUBLIC_ARCHIVE minted one.
func (r *runner) writePublicIdentity(n int) error {
	if r.publicIdentity == nil {
		return nil
	}
	path := filepath.Join(r.dirs.Discs, discDirName(n), doc.PublicIdentityName)
	// The disc's copy is the staging copy's bytes, not a fresh rendering.
	// WriteIdentityFile stamps "# created:" with the moment it runs, so
	// rendering per disc gave every disc a different file for the same key
	// whenever the layout crossed a second boundary — and a reader that had
	// staged disc 1's copy could not tell disc 2's from a second public set's.
	// Copying the one persisted file makes the key genuinely one file on every
	// disc, which is also what SHA512SUMS, MANIFEST.txt and README.md say it is.
	canonical, err := os.ReadFile(r.publicIdentityPath())
	if err != nil {
		return fmt.Errorf("backup: disc %d: reading the set's key: %w", n, err)
	}
	// A resumed run reaches discs an earlier run already laid out. Keep the
	// file only if it is byte-for-byte the set's key: a run killed between the
	// create and the write leaves a truncated one, and keeping that would put
	// a disc on the shelf whose "key" is an empty file.
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, canonical) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: disc %d: replacing %s: %w", n, path, err)
	}
	if err := os.WriteFile(path, canonical, 0o644); err != nil {
		return fmt.Errorf("backup: disc %d: writing %s: %w", n, path, err)
	}
	// The mode passed to WriteFile is masked by the umask; the disc copy is
	// meant to be world-readable, that being the point of a public archive.
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("backup: disc %d: chmod %s: %w", n, path, err)
	}
	return nil
}

// The recovery set over a disc's small files. The parameters are fixed rather
// than configurable: these files total a few kilobytes, so there is nothing
// here worth tuning and a knob would only be a way to get it wrong.
const (
	// sidecarsPar2Name is the base name of the set. It is part of the on-disc
	// format: a restorer reads it out of the README and types it at par2.
	sidecarsPar2Name = "sidecars.par2"
	// SidecarRedundancy is the recovery percentage over the small files.
	SidecarRedundancy = 50
	// sidecarPar2Blocks is the recovery block count.
	sidecarPar2Blocks = 100
)

// sidecarNames lists the files in a disc's data directory that sidecars.par2
// protects: every .sha512 sidecar, and the encrypted index itself rather than
// only its hash.
//
// The membership is part of the on-disc format, not an internal choice: the
// README tells a restorer to run `par2 repair -- sidecars.par2`, and brb.sh
// prints that same instruction (brb.sh:611, :1709), so a file left out of this
// set is a file the printed repair cannot recover.
func sidecarNames(data string) ([]string, error) {
	names, err := filesMatching(data, func(nm string) bool {
		return strings.HasSuffix(nm, ".sha512")
	})
	if err != nil {
		return nil, err
	}
	idx := filepath.Join(data, indexName)
	switch _, err := os.Stat(idx); {
	case err == nil:
		names = append(names, indexName)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("backup: %s: %w", idx, err)
	}
	return names, nil
}

// protectSidecars writes sidecars.par2 over one disc's small files.
//
// The image's own par2 set covers the ciphertext and nothing else, so one
// rotted byte in a 170-byte .sha512 sidecar is enough to make a restore reject
// an image par2 itself says is perfect. The index is worse: gzip inside age,
// where a single flipped bit destroys the whole map of which disc holds what.
// Those files together are a few kilobytes, so 50% parity over them is free.
//
// Failure is a warning and never fatal: without sidecars.par2 the set loses
// redundancy over its smallest files, which is not a reason to throw away a
// backup that is otherwise complete.
//
// It is remembered, though. A twenty-disc run prints hours of output, and a
// warning on disc 14 is gone off the top of the terminal long before the run
// ends; the disc is then burned, shelved, and read for the first time in
// fifteen years by someone following its README. So every failure is recorded
// here and listed again by finish(), where the operator is still looking.
func (r *runner) protectSidecars(ctx context.Context, n int, data string) {
	if err := r.sidecarParity(ctx, n, data); err != nil {
		r.p.Warn("could not protect the sidecar files on disc %d: %v", n, err)
		r.sidecarFailures = append(r.sidecarFailures, n)
	}
}

// sidecarFailed reports whether disc n went without its sidecar parity. The
// READMEs are rendered after every disc is built, so this answers for the whole
// set by then; asking earlier would report a disc as protected because its par2
// run had not been attempted yet.
func (r *runner) sidecarFailed(n int) bool {
	for _, f := range r.sidecarFailures {
		if f == n {
			return true
		}
	}
	return false
}

// sidecarParity is protectSidecars' work, with the failures returned rather
// than warned about, so there is one place that decides what a failure means.
func (r *runner) sidecarParity(ctx context.Context, n int, data string) error {
	names, err := sidecarNames(data)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("there are no sidecar files in %s to protect", data)
	}
	// A resumed run finds the previous run's set here, and par2 will not
	// overwrite one. Removing it first also means a set that is left behind is
	// always one that matches the files beside it. Matched by name, not by a
	// glob over the full path, so a STAGING holding '[' or '*' still sweeps.
	stale, err := filesMatching(data, func(nm string) bool {
		return strings.HasPrefix(nm, "sidecars") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil {
		return err
	}
	for _, f := range stale {
		if err := os.Remove(filepath.Join(data, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing the previous %s: %w", f, err)
		}
	}

	r.p.Step("disc %d: %d%% recovery data over %d sidecar file(s)", n, SidecarRedundancy, len(names))
	return r.tools.Par2Create(ctx, tools.Par2Options{
		Dir:        data,
		File:       sidecarsPar2Name,
		Inputs:     names,
		Redundancy: SidecarRedundancy,
		Blocks:     sidecarPar2Blocks,
		// No log: par2 announces every one of these four tiny files by name on
		// every disc, which buries the run's real output on a twenty-disc set.
		// A failure still carries the tail of that output in its error.
	})
}

// writeManifest renders MANIFEST.txt, which describes the whole set, and puts
// the same file on every disc.
func (r *runner) writeManifest(ctx context.Context, total int) error {
	discFiles := make(map[int][]doc.FileEntry, total)
	for n := 1; n <= total; n++ {
		data := filepath.Join(r.dirs.Discs, discDirName(n), "data")
		ents, err := os.ReadDir(data)
		if err != nil {
			return fmt.Errorf("backup: reading %s: %w", data, err)
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				return fmt.Errorf("backup: %s/%s: %w", data, e.Name(), err)
			}
			discFiles[n] = append(discFiles[n], doc.FileEntry{Name: e.Name(), Size: fi.Size()})
		}
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	text := doc.RenderManifest(doc.ManifestData{
		Archive:        r.cfg.ArchiveName,
		Created:        r.started.Format(time.RFC3339),
		Host:           host,
		Source:         r.cfg.SourceDir,
		Total:          total,
		DiscType:       r.cfg.DiscType.String(),
		Compression:    r.cfg.Compression,
		Level:          r.cfg.CompressionLevel,
		LevelApplies:   tools.LevelApplies(r.cfg.Compression),
		BlockSize:      r.cfg.BlockSize,
		Redundancy:     r.cfg.Par2Redundancy,
		Version:        Version,
		ToolVersions:   r.toolVersions(),
		Recipients:     r.pubkeys,
		PublicIdentity: r.publicIdentityText(),
		DiscFiles:      discFiles,
		PruneDirs:      r.manifestPrunes(),
		ExcludeMasks:   r.cfg.ExcludeMasks,
	})

	mf := filepath.Join(r.cfg.Staging, "MANIFEST.txt")
	if err := os.WriteFile(mf, []byte(text), 0o644); err != nil {
		return fmt.Errorf("backup: writing %s: %w", mf, err)
	}
	for n := 1; n <= total; n++ {
		dst := filepath.Join(r.dirs.Discs, discDirName(n), "MANIFEST.txt")
		if err := linkOrCopy(ctx, mf, dst, 0o644); err != nil {
			return err
		}
	}
	r.p.OK("manifest on every disc")
	return nil
}

// manifestPrunes is the "excluded from this backup" list the manifest prints
// under "prune:": the configured PRUNE_DIRS, followed by every mount point the
// scan refused to cross, each marked as such. A mount point is not a
// configured prune, but the effect on the set is identical — the directory is
// there and everything under it is not — and the manifest is the one document
// that outlives the terminal the warning was printed to.
func (r *runner) manifestPrunes() []string {
	if len(r.skippedMounts) == 0 {
		return r.cfg.PruneDirs
	}
	out := append([]string(nil), r.cfg.PruneDirs...)
	for _, m := range r.skippedMounts {
		out = append(out, m+"  (mount point: not crossed, its contents are not on this set)")
	}
	return out
}

// toolVersions collects the version banners of the programs that built this
// set, for the manifest. age has no banner: it is compiled in.
func (r *runner) toolVersions() []string {
	var out []string
	for _, name := range []string{tools.Mksquashfs, tools.Unsquashfs, tools.Par2, tools.Xorriso} {
		if t := r.tools.Get(name); t.Found && t.Version != "" {
			out = append(out, t.Version)
		}
	}
	return append(out, "age  filippo.io/age, compiled into brb (no external binary used)")
}

// writeReadmes puts a copy of this program and a per-disc README.md on every
// disc, in that order: the README lists the copies of brb that are actually in
// the root of its disc, so they all have to be there before it is rendered.
func (r *runner) writeReadmes(ctx context.Context, total int) error {
	self, err := r.copySelf(ctx, total)
	if err != nil {
		return err
	}

	date := r.started.Format(time.RFC3339)
	for n := 1; n <= total; n++ {
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		text := doc.RenderDiscREADME(doc.DiscData{
			Archive:           r.cfg.ArchiveName,
			Disc:              n,
			Total:             total,
			Date:              date,
			Source:            r.cfg.SourceDir,
			Redundancy:        r.cfg.Par2Redundancy,
			SidecarRedundancy: SidecarRedundancy,
			SidecarParity:     !r.sidecarFailed(n),
			Version:           Version,
			Tools:             discToolArtifacts(dd),
			PublicIdentity:    r.publicIdentityText(),
		})
		if err := os.WriteFile(filepath.Join(dd, "README.md"), []byte(text), 0o644); err != nil {
			return fmt.Errorf("backup: writing %s/README.md: %w", dd, err)
		}
	}

	if self == "" {
		r.p.OK("README.md on every disc")
		return nil
	}
	r.p.OK("README.md and %s on every disc", self)
	return nil
}

// copySelf puts a copy of the running program on every disc that is not already
// carrying one under that name, and returns the name it used ("" when the
// program could not be located, which is reported but does not fail the run).
//
// The program is copied into staging once and hard-linked from there, so a
// twenty-disc set does not carry twenty independent copies in staging.
func (r *runner) copySelf(ctx context.Context, total int) (string, error) {
	self, err := selfPath()
	if err != nil {
		r.p.Warn("could not locate this program to copy onto the discs: %v", err)
		return "", nil
	}
	// Never call it plain "brb". Every disc already carries brb.sh, the shell
	// reader, and this is 8 MB of machine code for one architecture; two files
	// called "brb" on the same disc, one runnable anywhere and one runnable on
	// a CPU the restorer may not have, is the worst possible naming. Name it
	// for what it is, the way build-dist.sh names the release artifacts, so
	// `uname -m` tells a restorer which file to reach for.
	name := SelfCopyName()
	staged := filepath.Join(r.cfg.Staging, name)
	if err := copyFile(ctx, self, staged, 0o755); err != nil {
		return "", err
	}
	for n := 1; n <= total; n++ {
		dst := filepath.Join(r.dirs.Discs, discDirName(n), name)
		// The dist payload, when there is one, already carries a properly named
		// binary for every architecture. Copying ourselves over it would replace
		// a cross-built release artifact with whatever happens to be running.
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := linkOrCopy(ctx, staged, dst, 0o755); err != nil {
			return "", err
		}
	}
	return name, nil
}

// SelfCopyName is the filename this binary is written under on a disc. Go spells
// 64-bit ARM "arm64"; Linux and `uname -m` spell it "aarch64", and the person
// reading the disc will be typing uname, so the disc uses their spelling.
func SelfCopyName() string {
	arch := runtime.GOARCH
	if arch == "arm64" {
		arch = "aarch64"
	}
	return "brb-" + runtime.GOOS + "-" + arch
}

// selfPath returns the absolute path of the running program, resolving
// symlinks: what lands on the disc must be the binary itself and not a copy of
// a symlink's contents, and os.Executable can hand back the link.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("backup: locating this program: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// writeSums writes a sha512sum-compatible SHA512SUMS covering every file on
// each disc.
func (r *runner) writeSums(ctx context.Context, total int) error {
	for n := 1; n <= total; n++ {
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		if err := agecrypt.WriteSumsKnown(ctx, dd, filepath.Join(dd, agecrypt.SumsName), r.knownSums(n)); err != nil {
			return fmt.Errorf("backup: disc %d: %w", n, err)
		}
	}
	r.p.OK("%s on every disc", agecrypt.SumsName)
	return nil
}

// knownSums lists the digests this run already measured for disc n's two large
// files, so SHA512SUMS does not read them again: the disc directory's copies
// are hard links to the ones in enc/, whose SHA-512 was computed on the way
// through the encryption.
//
// It returns nothing under --verify-roundtrip. That flag is the operator asking
// for every byte to be proved twice, and this pass is the only one that reads
// the ciphertext back off the filesystem after it was written — the one thing
// that would catch a staging directory that corrupted it in between. See
// agecrypt.WriteSumsKnown, which checks the inode before trusting any of this.
func (r *runner) knownSums(n int) map[string]agecrypt.KnownSum {
	if r.opts.VerifyRoundTrip {
		return nil
	}
	out := make(map[string]agecrypt.KnownSum, 2)
	base := imageName(n) + ".age"
	if sum, ok := r.discCipher[n]; ok {
		out["./data/"+base] = agecrypt.KnownSum{Digest: sum, Same: filepath.Join(r.dirs.Enc, base)}
	}
	if r.indexCipher != "" {
		out["./data/"+indexName] = agecrypt.KnownSum{
			Digest: r.indexCipher,
			Same:   filepath.Join(r.dirs.Enc, indexName),
		}
	}
	return out
}

// checkDiscSizes confirms every finished disc directory still fits the media,
// now that the documentation, the sums and the program copy are on it too.
func (r *runner) checkDiscSizes(ctx context.Context, total int) error {
	for n := 1; n <= total; n++ {
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		size, err := dirBytes(ctx, dd)
		if err != nil {
			return err
		}
		if size > r.budget.Usable {
			return r.oversizedDisc(n, "files", size)
		}
		r.p.Step("%s: %s / %s", discDirName(n), ui.HumanBytes(size), ui.HumanBytes(r.budget.Usable))
	}
	return nil
}

// isoOptions bundles what internal/iso needs to build this set's images.
func (r *runner) isoOptions() iso.Options {
	return iso.Options{Cfg: r.cfg, UI: r.p, Tools: r.tools, Version: Version}
}

// buildISOs turns every disc directory into a burnable ISO 9660 image — but
// only under ISO_MODE=eager.
//
// Under the default, ondemand, the ISOs are burn's problem, one at a time. Each
// one is a full second copy of its disc directory, so building all of them here
// holds staging at roughly 2.2x the compressed set until the last disc is
// burned, which on a twenty-disc set is weeks.
func (r *runner) buildISOs(ctx context.Context, total int) error {
	if !r.cfg.ISOMode.Eager() {
		return nil
	}
	return iso.BuildAll(ctx, r.isoOptions(), total)
}

// finish removes the working index and prints what to do next.
func (r *runner) finish(total int) {
	idx := filepath.Join(r.dirs.Work, indexFileName)
	if err := os.Remove(idx); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.p.Warn("could not remove the plaintext index %s: %v", idx, err)
	}
	// The state file exists to say "this set is half built". Leaving it behind
	// after a completed run makes staging claim an interruption that never
	// happened, and the next plain backup into the same directory refuses to
	// start until the operator works out why.
	if err := os.Remove(r.statePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.p.Warn("could not remove the resume state %s: %v", r.statePath, err)
	}
	mins := int(time.Since(r.started).Round(time.Minute) / time.Minute)
	r.p.Raw("")
	r.p.OK("backup complete in %d minute(s)", mins)
	// Said again here because the disc is about to be burned. Without
	// sidecars.par2 the .sha512 files and the encrypted index on that disc have
	// no recovery data of their own, and a single rotted byte in the index is
	// the map of the whole set — the image's parity does not cover it.
	if len(r.sidecarFailures) > 0 {
		r.p.Warn("no sidecar recovery data on disc(s) %s: their small files (the .sha512 "+
			"sidecars and the encrypted index) are on the disc but unprotected. Re-run the "+
			"backup for those discs, or accept it and rely on the other copies of the index",
			joinInts(r.sidecarFailures))
	}
	r.p.Raw("")
	r.p.Raw("  Archive : %s", r.cfg.ArchiveName)
	r.p.Raw("  Discs   : %d", total)
	// Saying "ISOs: <dir>" when nothing has built them would send the operator
	// looking for files that do not exist and are not meant to.
	if r.cfg.ISOMode.Eager() {
		r.p.Raw("  ISOs    : %s", r.dirs.ISO)
	} else {
		r.p.Raw("  ISOs    : built on demand — 'burn' images each disc as it goes into")
		r.p.Raw("            the drive and removes the ISO once it is written")
		r.p.Raw("            (KEEP_ISOS=1 keeps them). To materialise the whole set")
		r.p.Raw("            as files, to burn them elsewhere:  brb iso all")
	}
	r.p.Raw("")
	r.p.Raw("  Next:")
	r.p.Raw("    brb burn all")
	r.p.Raw("    brb verify-disc 1")
	r.p.Raw("    brb restore /tmp/testrestore    # do this once before trusting the set")
	r.p.Raw("")
	r.p.Raw("  Then wipe staging:  rm -rf %s", r.cfg.Staging)
	r.p.Raw("")
}

// joinInts renders disc numbers for a message: "3", or "3, 7 and 14".
func joinInts(ns []int) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, strconv.Itoa(n))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
