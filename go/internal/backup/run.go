package backup

import (
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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/fsx"
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
	entries, err := r.resumeFilter(res)
	if err != nil {
		return err
	}
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
// re-seeds the packer with what is left. For a tree that has not changed since,
// filtering the scan is equivalent to seeding an assigned set: every name of a
// hard link group travels to the same disc, so a group is either wholly
// assigned or wholly outstanding.
//
// Two ways the tree can change between the runs break that equivalence, and
// they get different answers because their consequences differ.
//
// A path a finished disc holds that has changed kind since — a file that is
// now a directory, a directory that is now a symlink, a symlink that is now a
// file — is REFUSED. A finished disc cannot be rewritten and every later disc
// carries the whole skeleton, so such a set holds the path as one kind on the
// finished discs and as another on all the rest, and restoring it means
// extracting one over the other with unsquashfs -f. Whether that can work
// depends on which two kinds they are and on the order — a whole-set restore
// goes oldest disc first, and `restore --disc` extracts an older disc over a
// newer one as readily as the reverse:
//
//   - With a directory on either side, no order works. A file that became a
//     directory has the later disc's files under it, and unsquashfs abandons
//     the restore at the first of them ("failed to lstat ..., because Not a
//     directory"), as it does under a fifo or device node that became one. A
//     directory cannot be replaced by a leaf, because -f has to remove it
//     first, which no squashfs-tools does for a populated one and only 4.7
//     does for an empty one. And a symlink where a disc holds a directory is
//     what both readers' per-image guards refuse, because -f would write
//     through it.
//   - A regular file and a symlink, fifo or device node restore one way round
//     only. -f unlinks whatever holds a path before creating a symlink, a
//     fifo or a device node (not a socket: 4.5 and 4.6 create one without
//     unlinking anything), but squashfs-tools 4.5 writes a regular file by
//     opening the path with O_TRUNC, so a file extracted over the other is
//     written through the symlink, into the device, or blocks on the fifo —
//     and for a symlink that became a file, that is the order a whole-set
//     restore takes. (Read from 4.5.1's source, not run; 4.6 lstats the path
//     and unlinks it first.)
//   - Between two non-file kinds — a symlink that became a fifo — it would
//     usually restore either way, a socket on one side aside. It is refused
//     all the same: it is too rare to earn an exception, and without one the
//     rule fits in a sentence.
//
// That sentence: a path a finished disc holds keeps its kind for the rest of
// the set. Regular files are checked against State.Assigned, everything else
// against State.Skeleton (see State.writtenKinds for a state file written
// before that existed).
//
// The refusal guards the discs still to be written, so it applies only when
// there are some. A run interrupted after its last disc was recorded — in the
// index, the payload, SHA512SUMS or the eager ISOs — resumes with nothing left
// to pack, buildDiscs builds no further disc, and a path that has changed kind
// since reaches none: the finished discs agree with one another, and a restore
// gives the path back as they hold it. That is warned about rather than
// refused, because moving a directory off a full disk and symlinking it back
// is the natural answer to that interruption, and refusing would demand the
// space back or a finished set thrown away. A path that has become a regular
// file is refused even then. It is itself something left to write, which only
// a new disc could take, and leaving it out instead would let the run report a
// complete set that lacks a file the scan found.
//
// The refusal is here because the alternative was silence. The assigned lookup
// used to sit inside the KindFile branch, so a written file that had changed
// kind never matched the set at all, and a written directory had no record to
// match: either way the run finished reporting a complete set, and the only
// warning fired was the one below saying files were "no longer in the source
// tree" — which reads as reassurance.
//
// When there is a disc still to write, it also adds this scan's skeleton to
// State.Skeleton, the record the next resume checks against. Only in memory:
// the state is saved when a disc is finished, so a run interrupted before its
// first disc records nothing it did not write.
//
// A hard link added to an already-written inode is WARNED about and packed. It
// cannot join the rest of its group — the disc holding the group is finished,
// and nothing can add a name to a written disc — so pack.New sees the new name
// alone and charges it a second full copy on a later disc. The set grows, and
// the restored tree holds two independent files where the source had one inode
// with several names. That is a fidelity loss and a size surprise, not a broken
// set: every byte still restores. Adding a name to a tree between runs is what
// --resume is documented to cope with, so refusing would break the supported
// case to prevent a duplicate.
//
// That one was silent too. RawBytes counts a hard-link group once per inode
// (scan.Result), so a new link does not change the measured tree size and the
// warning above cannot fire; the one below counts only names that vanished.
// Detecting it needs nothing new in the state file: the scan carries Dev, Inode
// and Nlink for every entry, the already-assigned ones included, so one pass
// over the same result says which inodes a finished disc already holds. The one
// case that escapes is a group whose every previously-written name has since
// been deleted — its inode is then nowhere in the scan, so there is nothing to
// match, and those names are reported by the vanished-names warning instead.
func (r *runner) resumeFilter(res *scan.Result) ([]scan.Entry, error) {
	if r.st.ScanRawSize == 0 {
		r.st.ScanRawSize = res.RawBytes
	} else if r.st.ScanRawSize != res.RawBytes {
		r.p.Warn("the source tree measured %s when this set was started and %s now; "+
			"files added since will be included, files removed since are simply absent",
			ui.HumanBytes(r.st.ScanRawSize), ui.HumanBytes(res.RawBytes))
	}
	if r.st.DiscsDone == 0 {
		// Nothing is on a disc yet, so nothing constrains the tree, and the
		// skeleton this run is about to write is the whole record.
		r.st.recordSkeleton(nil, res.Entries)
		return res.Entries, nil
	}

	assigned := r.st.assignedSet()
	// The kind of every non-file path a finished disc holds.
	was, err := r.st.writtenKinds()
	if err != nil {
		return nil, fmt.Errorf("backup: --resume: %w", err)
	}

	// Which inodes a finished disc already holds, and under which name, so the
	// warning can say what the new link duplicates. Keyed on the pair, never on
	// the inode alone, for the reason scan.Entry.Dev gives.
	written := make(map[inodeKey]string)
	for _, e := range res.Entries {
		if e.Kind != scan.KindFile || e.Nlink <= 1 || e.Inode == 0 {
			continue
		}
		if _, ok := assigned[e.Rel]; ok {
			written[inodeKey{dev: e.Dev, ino: e.Inode}] = e.Rel
		}
	}

	out := make([]scan.Entry, 0, len(res.Entries))
	var changed []kindChange
	var relinked []scan.Entry
	firstName := map[string]string{}
	done, todo := 0, 0
	for _, e := range res.Entries {
		_, wasAssigned := assigned[e.Rel]
		if wasAssigned && e.Kind != scan.KindFile {
			changed = append(changed, kindChange{rel: e.Rel, was: scan.KindFile, now: e.Kind})
			continue
		}
		if k, ok := was[e.Rel]; ok && k != e.Kind {
			changed = append(changed, kindChange{rel: e.Rel, was: k, now: e.Kind})
			if e.Kind == scan.KindFile {
				// No disc holds this file, so it is still to write, and only a
				// new disc could take it: counting it makes the refusal below
				// fire even when it is the only thing left.
				todo++
			}
			continue
		}
		if e.Kind == scan.KindFile {
			if wasAssigned {
				done++
				continue
			}
			if e.Nlink > 1 && e.Inode != 0 {
				if name, ok := written[inodeKey{dev: e.Dev, ino: e.Inode}]; ok {
					relinked = append(relinked, e)
					firstName[e.Rel] = name
				}
			}
			todo++
		}
		out = append(out, e)
	}
	if len(changed) > 0 {
		if todo > 0 {
			return nil, changedKindError(changed, r.st.DiscsDone)
		}
		r.p.Warn("%s", changedKindDetail(changed, r.st.DiscsDone))
	}
	// With nothing to write, this scan's skeleton goes onto no disc, so it is
	// not recorded — nor would it be saved, since only a finished disc saves.
	if todo > 0 {
		r.st.recordSkeleton(was, res.Entries)
	}
	if len(relinked) > 0 {
		r.p.Warn("%s", relinkedDetail(relinked, firstName))
	}
	if missing := len(assigned) - done; missing > 0 {
		r.p.Warn("%d file(s) recorded on an earlier disc are no longer in the source tree; "+
			"they remain on the discs already written", missing)
	}
	r.p.Step("resume: %d file(s) already on disc, %d still to write", done, todo)
	return out, nil
}

// inodeKey identifies one file within one filesystem. It is the pair, never the
// inode alone, for the reason [scan.Entry].Dev gives: two unrelated files on two
// filesystems can carry the same inode number, and a scan can hold more than one
// device. pack.New groups hard links on the same pair.
type inodeKey struct {
	dev, ino uint64
}

// relinkedDetail is the warning for names that were hard-linked to
// already-written data between an interrupted run and its resume, largest
// first. It names what each duplicates, because the repair is to remove the new
// link and resume again, and the operator cannot do that without knowing which
// name is new. The names are %q, for the reason oversizedDetail gives.
func relinkedDetail(relinked []scan.Entry, firstName map[string]string) string {
	sort.Slice(relinked, func(i, j int) bool { return relinked[i].Size > relinked[j].Size })
	var extra int64
	for _, e := range relinked {
		extra += e.Size
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d new hard link(s) point at data an earlier disc already holds; the disc holding "+
		"their other names is written and cannot gain one, so each is packed again as a full copy "+
		"(%s more on later discs, and each restores as a separate file rather than as a link):",
		len(relinked), ui.HumanBytes(extra))
	for i, e := range relinked {
		if i == 20 {
			fmt.Fprintf(&b, "\n  ... and %d more", len(relinked)-20)
			break
		}
		fmt.Fprintf(&b, "\n  %14d  %q (already on a disc as %q)", e.Size, e.Rel, firstName[e.Rel])
	}
	b.WriteString("\n  remove the new link(s) and resume again to avoid the copies, or start the set over")
	return b.String()
}

// kindChange is one path whose kind on a finished disc is not its kind in the
// source tree now.
type kindChange struct {
	rel      string
	was, now scan.Kind
}

// changedKindError refuses a resume whose source tree has changed the kind of
// a path a finished disc holds, in either direction, in one message — a list
// per direction would let fixing the first refusal uncover the second. Naming
// each path with what it was and what it is now is the whole value of the
// message: putting them back is the one repair that does not throw the
// finished discs away, and nothing else in the run points at them.
func changedKindError(changed []kindChange, discsDone int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "backup: --resume: %d path(s) the %d finished disc(s) hold have changed kind "+
		"in the source tree since (was -> now):", len(changed), discsDone)
	writeKindChanges(&b, changed)
	b.WriteString("\n  a finished disc cannot be rewritten, and every later disc carries the whole " +
		"directory skeleton, so continuing would hold each of these as one kind on the finished discs " +
		"and as another on the rest. Where one of the two is a directory, no restore of the set can " +
		"reliably lay one over the other; where one is a regular file, squashfs-tools 4.5 writes it " +
		"through a symlink or into a device node the other disc left at that path, or blocks on a fifo " +
		"there; and a path that changed between two other kinds is refused so that the rule stays simple. " +
		"Put them back as they were, or start the set over")
	return errors.New(b.String())
}

// changedKindDetail is the warning for the same changes when nothing is left
// to write. It names them because the restored tree will not match the source
// at those paths, and an operator who sees a restore bring back a directory
// they had replaced with a symlink should already have been told why.
func changedKindDetail(changed []kindChange, discsDone int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d path(s) the %d finished disc(s) hold have changed kind in the source tree "+
		"since (was -> now):", len(changed), discsDone)
	writeKindChanges(&b, changed)
	b.WriteString("\n  nothing is left to write, so no further disc carries them and the set is unaffected: " +
		"a restore gives each back as the finished discs hold it, not as it is now")
	return b.String()
}

// writeKindChanges lists changes one per line, the first twenty of them. The
// paths are %q, for the reason oversizedDetail gives.
func writeKindChanges(b *strings.Builder, changed []kindChange) {
	for i, c := range changed {
		if i == 20 {
			fmt.Fprintf(b, "\n  ... and %d more", len(changed)-20)
			break
		}
		fmt.Fprintf(b, "\n  %-7s -> %-7s  %q", c.was, c.now, c.rel)
	}
}

// checkIndexCovers refuses a resume that would build discs the encrypted index
// can never list. Only finish deletes the plaintext index, as the step before
// it deletes the state file, so a resume that finds the encrypted index and no
// plaintext one is one whose last run stopped between the two (or could not
// remove the state), and prepareState marks it indexBuilt: the encrypted index
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

// warnIfImageMayNotFit says so before a disc is built when staging could not
// hold the image even in the worst case, rather than leaving mksquashfs to run
// the filesystem out part way through.
//
// preflight sizes its refusal from the IMAGE budget: one plaintext image plus
// one ciphertext with parity. That is the right floor for a disc whose content
// compresses the way the ratio predicts, and it is not a bound on the plaintext
// mksquashfs actually writes. With PACK_RATIO_ADAPT on by default, a disc of
// compressible content can teach the estimator a ratio of, say, 0.088; the raw
// budget is then imageBudget/0.088, eleven times the image budget, and if the
// NEXT disc's content turns out not to compress — text then photographs, the
// case the adaptation window exists for — the image grows toward that raw size
// and the run dies on "No space left on device" after preflight said the space
// was sufficient.
//
// This warns and does not refuse, deliberately. The bound it would have to
// enforce is avail >= bin.RawBytes, and requiring that of every disc refuses
// runs that complete today: mksquashfs compresses as it writes, so the
// plaintext only approaches the raw size when the estimate was wrong, and on
// bd25 at that ratio the demand would be about 240 GB of staging for a 21 GB
// image budget. Clamping the raw budget to the space available instead would
// complete, but by putting less on each disc — which is a decision about how
// many physical discs someone burns, and not one to make silently.
func (r *runner) warnIfImageMayNotFit(bin *pack.Bin, n int) {
	avail, err := freeSpace(r.cfg.Staging)
	if err != nil || avail >= bin.RawBytes {
		return
	}
	r.p.Warn("disc %d packs %s of raw content and %s has %s free; that is enough only because "+
		"the content is expected to compress to about %.3f of its size. If it does not, the image "+
		"grows past the free space and mksquashfs stops the run here — clear staging, or set "+
		"PACK_RATIO nearer 1.0 to pack less per disc",
		n, ui.HumanBytes(bin.RawBytes), r.cfg.Staging, ui.HumanBytes(avail), r.packRatio)
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
		rb := RawBudget(r.budget.Image, r.packRatio)
		if over := p.Oversized(rb); len(over) > 0 {
			return 0, oversizedError(over, rb)
		}
		bin, ok := p.Next(rb)
		if !ok {
			break
		}
		r.warnIfImageMayNotFit(bin, base+p.Committed()+1)
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
		rb := RawBudget(r.budget.Image, r.packRatio)
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
//
// Each disc directory is emptied and made again before it is laid out, so
// everything in it afterwards was put there by this run; see freshDiscDir.
func (r *runner) buildDiscDirs(ctx context.Context, total int) error {
	r.p.Log("laying out discs")
	for n := 1; n <= total; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("backup: aborted: %w", err)
		}
		// Before the directory is cleared, not after: a resume that finds an
		// artefact missing from enc/ must stop with the disc directory's copy
		// of it still there. On a staging tree that spans filesystems that copy
		// is a copy rather than a link, and it may be the only one left.
		names, err := r.discPayload(n)
		if err != nil {
			return err
		}
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		data := filepath.Join(dd, "data")
		if err := freshDiscDir(dd, data); err != nil {
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

// freshDiscDir empties the disc directory dd and creates it again, with its
// data/ subdirectory, under the rules [fsx.SecureStaging] applies to the rest
// of the staging tree: real directories, mode 0700, owned by this process.
//
// WHY EMPTY IT. makeDirs secures discs/ itself and nothing below it, and a
// staging tree is reused across a resume, so whatever an earlier run left
// inside discs/ was taken as found. A tree that another account could once
// write — one the operator made group- or world-writable by hand, or adopted
// with chown -R — could hold a symlink at discs/disc01, a symlink or a hard
// link at one of the document names, or a plain file at a name nothing in this
// run overwrites. The first sent the whole layout of that disc, images,
// documents and a copy of this program, wherever the link pointed. The second
// had os.WriteFile truncate the target of a link at README.md with the
// README's text. The third went onto the disc as though this run had put it
// there: hashed into SHA512SUMS, and — for a planted brb.sh or
// brb-linux-<arch> on a set with no dist payload, where copySelf kept
// whatever it found — listed by the README as a copy of the tool for a
// restorer to run. Securing the directory would
// have answered only the first. Emptying it answers all three, and costs
// nothing, because every entry a disc directory holds is regenerated on every
// run anyway: the data/ links from enc/, sidecars.par2, identity.txt, the
// manifest link, the payload, the self-copy, README.md and SHA512SUMS.
// linkOrCopy already removed and relinked each artefact rather than trust
// what it found.
//
// A symlink at dd is refused rather than removed. RemoveAll would only unlink
// it, so nothing outside staging is at risk either way; but a link there was
// either planted or made by an operator who pointed a disc at another volume
// for room, and both are worth hearing about rather than finding the link
// gone. discs/ is mode 0700 and this process's own by now, so nothing can be
// planted between the check and the removal.
//
// A mount point at dd, or anywhere below it, is refused too, and for a worse
// reason. It is the other way to point a disc at another volume for room, and
// RemoveAll does not stop at it: it walks into the mounted filesystem, deletes
// everything there, and only then fails on the mount point itself with EBUSY,
// so the run stopped with that volume already emptied. A bind mount does the
// same to the directory it shows, wherever that is. Before disc directories
// were emptied, a run wrote into such a mount without trouble, as it is meant
// to: linkOrCopy copies where it cannot link so that a staging tree spanning
// filesystems works. Mounting the other volume at discs/ itself is still fine,
// since nothing at or below dd is then a mount point. mountBelow says how a
// mount point is recognised, and which one goes unseen off Linux or without
// /proc: a bind mount from the filesystem discs/ is on, which gives a disc no
// room it did not have. A mount cannot appear between the check and the
// removal without privileges that make every other check here moot.
//
// The resume state (<STAGING>/state.json), the plaintext index in work/ and
// the ciphertext in enc/ cannot be treated this way — they are the set — so
// the README's security notes say what a shared staging tree costs there.
func freshDiscDir(dd, data string) error {
	if fi, err := os.Lstat(dd); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(dd)
		return fmt.Errorf("backup: %s is a symlink (-> %s); a disc directory is laid out by this run "+
			"and must be a real directory inside staging — remove the link and run "+
			"'brb backup --resume'", dd, target)
	}
	discs := filepath.Dir(dd)
	mp, why, err := mountBelow(dd)
	if err != nil {
		return err
	}
	if mp != "" {
		return fmt.Errorf("backup: %s %s; a disc directory is emptied and laid out again on every "+
			"run, which would delete everything on it — unmount it, or mount the other volume at %s "+
			"as a whole instead, and run 'brb backup --resume'", mp, why, discs)
	}
	if err := os.RemoveAll(dd); err != nil {
		return fmt.Errorf("backup: clearing %s: %w", dd, err)
	}
	if err := fsx.SecureStaging(dd, data); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// deviceOf is [fsx.DeviceID] and mountPointsUnder is [fsx.MountPointsUnder],
// held in variables so that a test can stand a mount in for one directory:
// mounting a filesystem takes a privilege an ordinary test run does not have.
var (
	deviceOf         = func(_ string, fi fs.FileInfo) (uint64, bool) { return fsx.DeviceID(fi) }
	mountPointsUnder = fsx.MountPointsUnder
)

// mountBelow returns the first directory at or below dd that is a mount point,
// with the reason to give for it, or "" when there is none, which includes a
// dd that does not exist yet.
//
// Two tests, because each misses something the other sees. The kernel's list
// of mount points, [fsx.MountPointsUnder], is the only one that sees a
// directory bind-mounted from the filesystem discs/ is on, which has the
// device discs/ has; it needs Linux and /proc. A device other than the one
// discs/ is on catches every other mount on every system, so where the list
// cannot be read, only that same-filesystem bind goes unrecognised. The device
// test also refuses a btrfs subvolume, which has a device of its own without
// being a mount point; emptying one empties it just as surely.
//
// Directories only, for the device test. A filesystem mounted on a file cannot
// lose its contents to an unlink, and the comparison would be wrong for files
// on overlayfs, which reports its own device for a directory but may report
// the one of the layer beneath for anything else. The list is not filtered
// that way, and a mount on a file below dd is refused as well; that only
// brings the failure forward, since RemoveAll cannot unlink a mount point
// either and stops there with EBUSY.
func mountBelow(dd string) (found, why string, err error) {
	mps, _, err := mountPointsUnder(dd)
	if err != nil {
		return "", "", fmt.Errorf("backup: looking for mount points under %s: %w", dd, err)
	}
	if len(mps) > 0 {
		return mps[0], "is a mount point", nil
	}
	parent := filepath.Dir(dd)
	pfi, err := os.Lstat(parent)
	if err != nil {
		return "", "", fmt.Errorf("backup: %w", err)
	}
	want, ok := deviceOf(parent, pfi)
	if !ok {
		return "", "", nil
	}
	err = filepath.WalkDir(dd, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if dev, ok := deviceOf(p, fi); ok && dev != want {
			found, why = p, "is on a different filesystem from "+parent+" (a mount point, or a subvolume)"
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("backup: looking for mount points under %s: %w", dd, err)
	}
	return found, why, nil
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
	// World-readable, that being the point of a public archive; the staging
	// copy it comes from is 0400. There is never an earlier copy to keep or
	// replace — freshDiscDir has just emptied the directory — and the write
	// goes through a .part and a rename, so a run killed part way leaves no
	// truncated "key" behind at this name for a later one to burn.
	return writeFileAtomic(path, canonical, 0o644)
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
// prints that same instruction (brb.sh's sidecar_repair_hint and usage), so a
// file left out of this set is a file the printed repair cannot recover.
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
	// No earlier set should be here any more. A resumed run used to find the
	// previous run's, but buildDiscDirs has just emptied data/ (freshDiscDir),
	// and nothing it links in from enc/ is named like this. The sweep stays as
	// the guarantee rather than the expectation: par2 refuses to write over an
	// existing set, and removing one first means a set that is left behind is
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
		Source:         docPath(r.cfg.SourceDir),
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
		ExcludeMasks:   docPaths(r.cfg.ExcludeMasks),
	})

	// The staging root is never emptied the way a disc directory is, so this
	// one is the write that has to replace whatever sits at the name rather
	// than open through it; see writeFileAtomic. The disc copies are links to
	// the file it installs.
	mf := filepath.Join(r.cfg.Staging, "MANIFEST.txt")
	if err := writeFileAtomic(mf, []byte(text), 0o644); err != nil {
		return err
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

// maxNamedUnreadable bounds how many unreadable paths the manifest names before
// it falls back to a count. A source tree can hold more of them than a document
// anyone will read should list — an unreadable directory of a million files is
// a million lines — and the count is the part that matters: it tells a restorer
// the set is short and by how much, which no amount of truncation can take
// away.
const maxNamedUnreadable = 64

// manifestPrunes is the "excluded from this backup" list the manifest prints
// under "prune:": the configured PRUNE_DIRS, followed by every mount point the
// scan refused to cross and every path it could not read, each marked as such.
// Neither is a configured prune, but the effect on the set is identical — the
// entry is missing and nothing says so once the run ends — and the manifest is
// the one document that outlives the terminal the warning was printed to.
//
// The distinction matters to whoever reads it: a mount point was left out on
// purpose and its contents are somewhere else, while an unreadable path was
// meant to be here and is not. Both are named as what they are, and both
// relative to SOURCE_DIR, like the prunes above them: a path inside the
// archive is the one a restorer can look for, and pass to --only.
//
// Every name goes through docPath. The mount points and unreadable paths come
// from the source tree, where anyone who can create a file chooses its name.
func (r *runner) manifestPrunes() []string {
	out := docPaths(r.cfg.PruneDirs)
	for _, m := range r.skippedMounts {
		out = append(out, docPath(m)+"  (mount point: not crossed, its contents are not on this set)")
	}
	for _, u := range r.unreadable {
		out = append(out, docPath(u)+"  (could not be read: NOT on any disc)")
	}
	if extra := r.unreadableCount - len(r.unreadable); extra > 0 {
		out = append(out, fmt.Sprintf("... and %d more path(s) that could not be read and are NOT on any disc", extra))
	}
	return out
}

// docPath renders a path, or a configured pattern, for MANIFEST.txt and the
// on-disc README: documents a stranger reads off a disc years from now, most
// likely by catting them to a terminal.
//
// A name holding a control character is quoted with strconv.Quote, which is
// what reportPaths' %q shows the operator for the same name on the terminal.
// Every other name is written exactly as it is. Raw, a newline in SOURCE_DIR
// started a line of its own ahead of the manifest's real "discs" line, and
// both readers' expected-disc check and iso.Total believe the first "discs"
// line they see: a source path holding "\ndiscs : 1\n" made a two-disc set
// with one disc staged read as complete. A newline in an unreadable file's
// name or in a mount point forged "prune:" and "mask :" records in the
// excluded section. And an ESC or a C1 control acted on the terminal of
// whoever read the file. ARCHIVE_NAME is refused outright for holding a C0
// control, for exactly that reason, while the path printed beside it went in
// verbatim.
//
// The class is the Printer's (ui.needsEscaping): C0, DEL, and C1 in both
// spellings — UTF-8 encoded, and as a raw 0x80..0x9F byte that is not part of
// a valid sequence — plus the newline and tab the Printer keeps for brb's own
// multi-line messages, which in a document can only forge.
//
// It is a rendering for people, not an encoding to parse back. It is not
// one-to-one: a name that is literally a quoted, escaped string reads the same
// as the quoted form of the name it spells. Nothing reads these lines back,
// and the quotes tell a person which of the two to suspect.
func docPath(s string) string {
	for i, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
			(r == utf8.RuneError && s[i] >= 0x80 && s[i] <= 0x9f) {
			return strconv.Quote(s)
		}
	}
	return s
}

// docPaths is docPath over a list, into a new slice: the configuration's own
// lists are not the manifest's to rewrite.
func docPaths(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, docPath(s))
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
			Source:            docPath(r.cfg.SourceDir),
			Redundancy:        r.cfg.Par2Redundancy,
			SidecarRedundancy: SidecarRedundancy,
			SidecarParity:     !r.sidecarFailed(n),
			Version:           Version,
			Tools:             discToolArtifacts(dd),
			PublicIdentity:    r.publicIdentityText(),
		})
		if err := writeFileAtomic(filepath.Join(dd, "README.md"), []byte(text), 0o644); err != nil {
			return err
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
		//
		// Anything at dst can only be that payload, placed by this run.
		// freshDiscDir emptied every disc directory before writePayload ran, so
		// the copy an earlier, interrupted run left here is gone. It used to be
		// kept: a resume after an upgrade or a rebuild then burned the old
		// binary, under a README and a manifest that both name the version
		// running now — and an earlier run that died part way through this loop
		// left one set carrying two different programs under one name.
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
	// The index is the same file on every disc, so its digest is looked up
	// once, here, rather than once per disc: a sidecar that cannot be read was
	// read again, and warned about again, for each of twenty discs.
	var index *agecrypt.KnownSum
	if k, ok := r.knownSum(indexName, r.indexCipher); ok {
		index = &k
	}
	for n := 1; n <= total; n++ {
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		if err := agecrypt.WriteSumsKnown(ctx, dd, filepath.Join(dd, agecrypt.SumsName), r.knownSums(n, index)); err != nil {
			var changed *agecrypt.DigestMismatchError
			if errors.As(err, &changed) {
				return r.changedSinceEncryption(n, changed)
			}
			return fmt.Errorf("backup: disc %d: %w", n, err)
		}
	}
	r.p.OK("%s on every disc", agecrypt.SumsName)
	return nil
}

// knownSums tells agecrypt.WriteSumsKnown what disc n's two large files, its
// image and the encrypted index, hashed to when they were encrypted. The one
// record does two jobs.
//
// In default mode it saves a read. The disc directory's copies are hard links
// to the ones in enc/, whose SHA-512 this run computed on the way through the
// encryption, and agecrypt trusts that digest only once it has checked the
// inode (Same) — so SHA512SUMS does not re-read twenty gigabytes per disc to
// learn a number brb already holds.
//
// And whenever a file IS read, the result is held to that digest (Verify),
// and a file that no longer matches it refuses the run. It is read under
// --verify-roundtrip, which is the operator asking for every byte to be proved
// twice, so nothing is shortcut; when linkOrCopy had to copy rather than link,
// so the inode check fails; and for a disc or an index an earlier, interrupted
// invocation encrypted, which this one never measured. For those last the
// digest comes from the sidecar written at encryption, enc/<name>.sha512.
//
// The comparison is what makes the read-back worth its time, and it used to
// be missing. Under --verify-roundtrip this returned nothing at all, so
// SHA512SUMS was filled with whatever the bytes hashed to by then: a
// ciphertext that changed in staging after its round-trip — bad RAM, a
// failing SSD — was attested as correct and verified clean, where default mode
// would at least have recorded the original digest for `brb verify` to catch.
// Every disc finished before a --resume was attested the same way in default
// mode.
//
// index is the index's entry, which writeSums looks up once for every disc;
// nil leaves the index to be hashed with nothing to compare against.
func (r *runner) knownSums(n int, index *agecrypt.KnownSum) map[string]agecrypt.KnownSum {
	out := make(map[string]agecrypt.KnownSum, 2)
	base := imageName(n) + ".age"
	if k, ok := r.knownSum(base, r.discCipher[n]); ok {
		out["./data/"+base] = k
	}
	if index != nil {
		out["./data/"+indexName] = *index
	}
	return out
}

// knownSum is one entry of knownSums. measured is the ciphertext digest this
// invocation took while encrypting name, or "" when an earlier one did.
//
// A sidecar that cannot be read, or that does not name the file, leaves the
// file to be hashed with nothing to compare against — which is what happened
// to every such file before the comparison existed — and says so. It is not a
// refusal: discPayload has already required the sidecar to exist, so this is
// a sidecar that rotted, and the readers' own checks will meet it on restore.
func (r *runner) knownSum(name, measured string) (agecrypt.KnownSum, bool) {
	enc := filepath.Join(r.dirs.Enc, name)
	if measured != "" {
		k := agecrypt.KnownSum{Digest: measured, Verify: true}
		if !r.opts.VerifyRoundTrip {
			k.Same = enc
		}
		return k, true
	}
	sidecar := enc + ".sha512"
	recorded, err := agecrypt.ReadSumFile(sidecar)
	if err != nil {
		r.p.Warn("%s is hashed for %s without being compared with the digest taken when it was "+
			"encrypted: %v", name, agecrypt.SumsName, err)
		return agecrypt.KnownSum{}, false
	}
	digest, ok := recorded[name]
	if !ok {
		r.p.Warn("%s is hashed for %s without being compared with the digest taken when it was "+
			"encrypted: %s has no line for it", name, agecrypt.SumsName, sidecar)
		return agecrypt.KnownSum{}, false
	}
	return agecrypt.KnownSum{Digest: digest, Verify: true}, true
}

// changedSinceEncryption is writeSums' refusal for a file that no longer hashes
// to the digest taken when it was encrypted. Either the file changed in
// staging since, or the record of it did, and from here the two cannot be told
// apart — so the message names both, and the way out, which is different for
// an image and for the index.
//
// A refusal and not a warning, because what it stops is the one thing that
// cannot be undone afterwards: a SHA512SUMS on the disc that attests the bytes
// as correct, so that verify-disc passes a disc whose image or index is no
// longer the one that was encrypted. Nothing is lost by stopping: every disc
// is already recorded in state.json, so a --resume after the repair redoes
// only the steps that follow the last image.
//
// Where the advice starts depends on what was read. When linkOrCopy had to
// copy, the file hashed was the disc directory's own copy and enc/ may be
// untouched, so the first thing to try is a resume, which lays the disc out
// again from enc/. Sending the operator to par2 first was wrong there: par2
// would find enc/ intact, and the advice for an intact image — suspect the
// record, decrypt twenty gigabytes to check it — sent them after a record
// that was fine, with "start the set over" as the only other way out. Only if
// the refusal comes back is enc/ or its record the suspect, and then the
// advice is the linked case's.
//
// That advice ends in a repair of the record rather than in "start the set
// over", wherever the bytes can be proved sound without it. Decrypting is that
// proof: age authenticates its header and every chunk of what it decrypts, so
// a ciphertext that decrypts without error is the one that was written, and
// for an image the plaintext digest says it is the right one. A record proved
// wrong that way is written again with sha512sum, whose output is the format
// the sidecar is in, and the resume then holds the file to the new record.
func (r *runner) changedSinceEncryption(n int, c *agecrypt.DigestMismatchError) error {
	name := strings.TrimPrefix(c.Name, "./data/")
	enc := filepath.Join(r.dirs.Enc, name)
	head := fmt.Sprintf("backup: disc %d: %s hashes to %s now but to %s when it was encrypted, so it, "+
		"or its record %s, has changed in staging since; refusing to write a %s that would vouch for it",
		n, c.Name, c.Got, c.Want, enc+".sha512", agecrypt.SumsName)
	rewrite := fmt.Sprintf("in %s, 'sha512sum %s > %s.sha512' writes the record again, and "+
		"'brb backup --resume' carries on", r.dirs.Enc, name, name)

	var fix string
	switch {
	case name == indexName && !r.indexBuilt:
		// The index has no parity in enc/ — sidecars.par2 is built per disc,
		// over whatever the index was at the time — but until finish() it has
		// its plaintext: a resume that finds work/index.tsv encrypts it again,
		// and lays every disc out again after it, so this one answer covers the
		// copy, the ciphertext and the record alike.
		return fmt.Errorf("%s — run 'brb backup --resume', which encrypts the index again from %s",
			head, filepath.Join(r.dirs.Work, indexFileName))
	case name == indexName:
		fix = fmt.Sprintf("the plaintext index it was encrypted from is already gone, so check whether %s "+
			"is sound: if it still decrypts, and 'gunzip -t' accepts what comes out, the record is what "+
			"changed — %s; if not, start the set over", enc, rewrite)
	default:
		fix = fmt.Sprintf("in %s, 'par2 repair -- %s.par2' restores the image if it is what changed, and "+
			"'brb backup --resume' then lays the discs out again. If par2 finds it intact, the record is "+
			"the suspect: decrypt the image and compare its sha512 with %s. If they match, the image is "+
			"sound — %s; if they do not, start the set over", r.dirs.Enc, name,
			strings.TrimSuffix(name, ".age")+".sha512", rewrite)
	}
	if r.separateCopy(n, name) {
		return fmt.Errorf("%s — the disc's copy is a file of its own, not a link to %s, so it may be the "+
			"copy alone that changed: run 'brb backup --resume', which copies it again from enc/. If the "+
			"refusal comes back, %s", head, enc, fix)
	}
	return fmt.Errorf("%s — %s", head, fix)
}

// separateCopy reports whether disc n's copy of the enc/ file name is a file
// of its own rather than a hard link to it: what linkOrCopy leaves when the
// link cannot be made. It is false when either cannot be looked at, which
// leaves changedSinceEncryption giving the advice that does not depend on it.
func (r *runner) separateCopy(n int, name string) bool {
	disc, err := os.Lstat(filepath.Join(r.dirs.Discs, discDirName(n), "data", name))
	if err != nil {
		return false
	}
	orig, err := os.Lstat(filepath.Join(r.dirs.Enc, name))
	if err != nil {
		return false
	}
	return !os.SameFile(disc, orig)
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
	// One shell word whatever STAGING holds: this is pasted, and a staging path
	// with a space in it would otherwise reach rm as two operands.
	r.p.Raw("  Then wipe staging:  rm -rf %s", ui.ShellWord(r.cfg.Staging))
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
