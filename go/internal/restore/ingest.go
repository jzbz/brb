package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// Ingest copies burned discs back into the staging area so that they can be
// repaired, decrypted and extracted. Discs may be presented in any order, and a
// disc that has already been ingested is recognised and not copied again.
//
// The loop always terminates, which the bash version's does not:
//
//   - Under --yes there is nobody to swap discs, so the disc that is in the
//     drive now is ingested exactly once and the loop stops. brb.sh's
//     prompt_enter and confirm both return success immediately under --yes, so
//     it re-copies the same disc forever.
//   - End of input at the prompt stops the loop, so a piped stdin ends cleanly.
//   - Without a terminal and without --yes it stops with an error saying which
//     of the two to supply, rather than blocking.
func Ingest(ctx context.Context, o Options, mountPoint string) error {
	if err := o.check(); err != nil {
		return err
	}
	o.secureStaging()
	encDir := o.dirs().Enc
	if err := os.MkdirAll(encDir, 0o700); err != nil {
		return fmt.Errorf("restore: creating %s: %w", encDir, err)
	}
	// A .part from an interrupted copy must never be mistaken for a finished
	// file, and nothing resumes from one — ddrescue keys its resume off the
	// map file with the final name in place. brb.sh clears these at the same
	// moment.
	o.reapPartFiles(encDir)
	ig := &ingester{o: o, mountPoint: mountPoint}
	ig.one = ig.ingestDisc
	return ig.run(ctx)
}

// ingester holds one Ingest run. The per-disc step is a field so the loop's
// termination rules can be tested without a drive, a disc or a mount; it
// reports whether the pass staged anything, so a re-read of a disc that is
// already fully staged does not advance the disc count.
type ingester struct {
	o          Options
	mountPoint string
	one        func(context.Context) (staged bool, err error)

	// prevDisc is the disc number the previous pass read, for spotting a tray
	// that never opened. 0 between runs and after an unrecognisable disc.
	prevDisc int

	discs      int
	incomplete int
	failed     int
}

// run drives the interactive disc-swapping loop.
func (ig *ingester) run(ctx context.Context) error {
	o := ig.o

	if o.UI.AssumeYes() {
		// One disc, no prompting, no repeating.
		o.UI.Log("--yes: ingesting the disc that is in the drive now, once")
		staged, err := ig.one(ctx)
		if err != nil {
			return err
		}
		if staged {
			ig.discs++
		}
		return ig.finish()
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		o.UI.Log("insert the next disc (any order), then press Enter")
		if err := o.UI.PromptEnter(""); err != nil {
			if errors.Is(err, io.EOF) {
				o.UI.Step("input ended — stopping")
				break
			}
			if errors.Is(err, ui.ErrNonInteractive) {
				return fmt.Errorf("restore: ingest needs a terminal to prompt for disc changes; re-run with --yes to ingest the disc that is in the drive now: %w", err)
			}
			return fmt.Errorf("restore: %w", err)
		}

		if staged, err := ig.one(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return err
			}
			// One bad disc must not throw away the discs already ingested, and
			// the operator is standing right there: report it and let them
			// decide whether to continue. The run still fails at the end.
			o.UI.Fail("%v", err)
			ig.failed++
		} else if staged {
			// A pass that staged nothing new was a re-read — most often a tray
			// that never opened — and counting it would let the summary drift
			// away from what is actually on disk.
			ig.discs++
		}

		more, err := o.UI.Confirm("Another disc?")
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if !more {
			break
		}
	}
	return ig.finish()
}

// finish prints the summary and turns incomplete copies into a failure. A run
// that salvaged only part of a disc has not succeeded, whatever brb.sh's
// copy_file_robustly reports.
func (ig *ingester) finish() error {
	o := ig.o
	encDir := o.dirs().Enc
	imgs, err := listNumbered(encDir, ".squashfs"+ageExt)
	if err == nil {
		o.UI.OK("ingested %d disc(s); %d encrypted image(s) now in %s", ig.discs, len(imgs), encDir)
		// Say now which discs are still outstanding, while the operator is
		// standing at the drive with the box of discs. brb.sh reports the same
		// thing here, and for the same reason it is never fatal.
		o.checkComplete(imgs)
	} else {
		o.UI.OK("ingested %d disc(s) into %s", ig.discs, encDir)
	}
	if ig.incomplete > 0 {
		o.UI.Warn("%d file(s) could not be copied completely — the gaps are zeros", ig.incomplete)
		o.UI.Step("ingest another copy of the damaged disc, or let par2 repair it during 'brb restore'")
	}
	if ig.failed > 0 || ig.incomplete > 0 {
		return fmt.Errorf("restore: ingest finished with %d unreadable disc(s) and %d incomplete file(s)", ig.failed, ig.incomplete)
	}
	return nil
}

// ingestDisc mounts one disc, copies its data directory into staging and
// unmounts it again. staged reports whether anything new reached the staging
// area.
func (ig *ingester) ingestDisc(ctx context.Context) (staged bool, err error) {
	o := ig.o
	mp, release, err := o.mountDisc(ctx, ig.mountPoint)
	if err != nil {
		return false, err
	}
	var once sync.Once
	unmount := func() { once.Do(release) }
	defer unmount()

	data := filepath.Join(mp, dataDir)
	fi, err := os.Stat(data)
	if err != nil || !fi.IsDir() {
		return false, fmt.Errorf("restore: %s has no %s/ directory — is this one of ours?", mp, dataDir)
	}
	o.UI.Log("ingesting %s", mp)

	sums := o.discSums(mp)
	names, err := dataFiles(data)
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		return false, fmt.Errorf("restore: %s is empty", data)
	}

	// HL-5: the sidecar parity is the one thing on a disc whose name is the same
	// on every disc of the set, so it needs the disc number to survive being
	// copied into one flat staging directory.
	disc := discOfDataFiles(names)
	if disc == 0 {
		o.UI.Warn("%s carries no numbered image, so its %s is staged under the flat name and another disc's may already be there",
			mp, sidecarsPar2)
	}

	// eject failures are deliberately silent, and a desktop can re-mount the
	// disc that never left the tray. The operator believes they swapped discs;
	// the drive knows better, and only the disc number can say so. Two
	// pressings of the same disc are indistinguishable here — bash's
	// disc_identity has the same blind spot — which is why this asks rather
	// than refuses.
	if disc != 0 && disc == ig.prevDisc {
		o.UI.Warn("this is the same disc as last time (%s) — the tray may not have opened", encName(disc))
		again, err := o.UI.Confirm("Read it again anyway?")
		if err != nil {
			return false, fmt.Errorf("restore: %w", err)
		}
		if !again {
			unmount()
			if ig.mountPoint == "" {
				o.eject(ctx)
			}
			return false, nil
		}
	}
	ig.prevDisc = disc

	encDir := o.dirs().Enc
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return staged, err
		}
		st, err := o.ingestFile(ctx, filepath.Join(data, name), filepath.Join(encDir, stagedSidecarName(name, disc)), sums[name])
		staged = staged || st
		switch {
		case err == nil:
		case errors.Is(err, ErrIncompleteCopy):
			o.UI.Warn("%v", err)
			ig.incomplete++
		default:
			return staged, err
		}
	}

	if err := o.copyManifest(ctx, mp); err != nil {
		o.UI.Warn("%v", err)
	}
	unmount()
	if ig.mountPoint == "" {
		// Only drive the tray when we were the ones driving the drive.
		o.eject(ctx)
	}
	return staged, nil
}

// discSums reads the disc's own SHA512SUMS so that every copy can be checked
// against it as it is made, keyed by base name. A disc without one is usable
// but unverifiable, and says so.
func (o Options) discSums(mp string) map[string]string {
	path := filepath.Join(mp, agecrypt.SumsName)
	sums, err := agecrypt.ReadSumFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			o.UI.Warn("could not read %s: %v", path, err)
		} else {
			o.UI.Warn("no %s on this disc; copies cannot be checked as they are made", agecrypt.SumsName)
		}
		return nil
	}
	out := make(map[string]string, len(sums))
	for name, hex := range sums {
		out[filepath.Base(name)] = hex
	}
	return out
}

// discOfDataFiles reports which disc of the set a data directory belongs to,
// read from the numbered image it carries. 0 means it could not be told, which
// is the only case where a sidecars.par2 still has to be staged flat.
func discOfDataFiles(names []string) int {
	for _, nm := range names {
		if n, ok := discNumberOf(nm, ".squashfs"+ageExt); ok {
			return n
		}
	}
	return 0
}

// ingestName renders one file for the operator: what it is called on the disc,
// and, when ingest stages it under a different name, that name too. Only the
// per-disc sidecar parity is renamed, and an operator who has just been told to
// run par2 against sidecars-disc03.par2 needs to have seen where it came from.
func ingestName(src, dst string) string {
	s, d := filepath.Base(src), filepath.Base(dst)
	if s == d {
		return s
	}
	return s + " (staged as " + d + ")"
}

// dataFiles lists the regular files in a disc's data directory, sorted.
func dataFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("restore: reading %s: %w", dir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !e.Type().IsRegular() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// ingestFile copies one file off the disc, or explains why it did not. staged
// reports whether anything new reached the staging area — a fresh copy, a
// resumed salvage, a verified replacement or an alternate copy — as opposed to
// "already have", which stages nothing.
//
// want is the hash the disc's own SHA512SUMS records for it, or "" when the
// disc carries no sums. When it is known the copy is checked against it for
// free, because copyStream hashes as it writes.
func (o Options) ingestFile(ctx context.Context, src, dst, want string) (staged bool, err error) {
	name := ingestName(src, dst)
	switch _, err := os.Stat(dst); {
	case err == nil:
		return o.reconcileExisting(ctx, src, dst, want)
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("restore: %s: %w", dst, err)
	}

	o.UI.Step("copying %s", name)
	sum, err := o.copyRobustly(ctx, src, dst)
	if err != nil {
		return true, err
	}
	if want != "" && !strings.EqualFold(sum, want) {
		return true, &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason:  "the copy does not match the hash this disc records for it, so the drive returned wrong data without reporting an error",
		}
	}
	// The copy is proven whole, so a map file from an interrupted salvage of an
	// earlier pressing has nothing left to describe — left behind, it would
	// brand this good copy an incomplete salvage forever.
	o.removeStaleMapfile(dst)
	if want != "" {
		o.UI.Step("%s copied and verified", name)
	} else {
		o.UI.Step("%s copied", name)
	}
	return true, nil
}

// removeStaleMapfile drops the ddrescue map file next to a copy that has just
// been proven complete, so a leftover from an interrupted salvage cannot
// outvote the verified bytes on the next ingest.
func (o Options) removeStaleMapfile(dst string) {
	if err := os.Remove(dst + mapfileExt); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.UI.Warn("could not remove %s: %v", dst+mapfileExt, err)
	}
}

// reconcileExisting decides what to do about a file that is already in staging.
//
// Two rules, both learned the hard way. A staged copy is never overwritten
// unless its replacement is already proven better — never removed first and
// re-copied after, because the moment between is where a power cut lands. And
// a differing copy of an image is never thrown away: two pressings of the same
// disc, each rotted past its own par2 redundancy, can still hold every block
// between them, but only if both copies are on disk for par2 to combine.
func (o Options) reconcileExisting(ctx context.Context, src, dst, want string) (staged bool, err error) {
	name := ingestName(src, dst)

	if want != "" {
		// The hash speaks first: a staged copy that matches is done, whatever a
		// leftover map file claims — an interrupted salvage's map survives a
		// later clean re-ingest and would otherwise brand it incomplete forever.
		got, err := agecrypt.SumFile(ctx, dst)
		if err != nil {
			return false, fmt.Errorf("restore: hashing the staged %s: %w", name, err)
		}
		if strings.EqualFold(got, want) {
			o.removeStaleMapfile(dst)
			o.UI.Step("already have %s, and it matches the hash on this disc", name)
			return false, nil
		}
		// Not whole. If a map file says the staged copy is a partial salvage,
		// continue it: only the unread regions are retried.
		if missing, ok, err := mapfileMissing(dst + mapfileExt); err != nil {
			return false, err
		} else if ok && missing > 0 {
			return true, o.resumeSalvage(ctx, src, dst, want, missing)
		}
		return o.replaceOrKeepBoth(ctx, src, dst, want)
	}

	// No recorded hash. A partial salvage is still resumable — the map file is
	// the only witness there is.
	if missing, ok, err := mapfileMissing(dst + mapfileExt); err != nil {
		return false, err
	} else if ok && missing > 0 {
		return true, o.resumeSalvage(ctx, src, dst, want, missing)
	}

	// Compare the two files themselves, cheaply first.
	si, err := os.Stat(src)
	if err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	differ := si.Size() != di.Size()
	if differ {
		o.UI.Warn("already have %s, and it differs in size from the copy on this disc (staged %s, disc %s); keeping the staged copy",
			name, ui.HumanBytes(di.Size()), ui.HumanBytes(si.Size()))
	} else {
		a, err := agecrypt.SumFile(ctx, dst)
		if err != nil {
			return false, fmt.Errorf("restore: hashing the staged %s: %w", name, err)
		}
		b, err := agecrypt.SumFile(ctx, src)
		if err != nil {
			return false, fmt.Errorf("restore: hashing %s on the disc: %w", name, err)
		}
		if strings.EqualFold(a, b) {
			o.UI.Step("already have %s, byte for byte", name)
			return false, nil
		}
		differ = true
		o.UI.Warn("already have %s, and it differs from the copy on this disc; keeping the staged copy", name)
	}
	if differ && isImageName(dst) {
		// Neither copy can be judged without a recorded hash, so keep both: the
		// disc's version is staged under the .copy name par2 combines from.
		alt := altCopyName(dst)
		o.UI.Step("ingesting this disc's copy as %s for par2 to combine during 'brb restore'", filepath.Base(alt))
		if _, err := o.copyRobustly(ctx, src, alt); err != nil {
			return true, err
		}
		return true, nil
	}
	o.UI.Step("delete %s and ingest this disc again if you want the disc's copy instead", dst)
	return false, nil
}

// isImageName reports whether a staged path is an encrypted disc image, the
// only kind of file whose alternate copies par2 can combine.
func isImageName(dst string) bool {
	return strings.HasSuffix(filepath.Base(dst), ".squashfs"+ageExt)
}

// altCopyName is where a further pressing's copy of an already-staged file
// goes: the same ".copy<unixtime>" convention brb.sh uses, in the same
// directory, so either reader's par2 pass finds the other's copies.
func altCopyName(dst string) string {
	return fmt.Sprintf("%s.copy%d", dst, time.Now().Unix())
}

// replaceOrKeepBoth handles a staged copy that fails the hash this disc
// records. The disc's version is read to a sibling name and hashed on the way;
// only a copy proven whole is renamed over the staged one — the staged bytes
// are never destroyed ahead of that proof. A copy that is damaged too is kept
// under the .copy name for par2 to combine at restore time, which is the one
// way two bad burns still add up to a whole image.
func (o Options) replaceOrKeepBoth(ctx context.Context, src, dst, want string) (staged bool, err error) {
	name := ingestName(src, dst)
	o.UI.Warn("the staged %s does not match the hash on this disc; reading this disc's copy", name)
	alt := altCopyName(dst)
	sum, err := o.copyRobustly(ctx, src, alt)
	if err != nil {
		if errors.Is(err, ErrIncompleteCopy) && isImageName(dst) {
			// The salvage — and its map file — stay under the .copy name: zeros
			// and all, it is more raw material for par2.
			o.UI.Step("keeping the partial copy as %s for par2 to combine during 'brb restore'", filepath.Base(alt))
			return true, err
		}
		o.removeUnverified(alt)
		o.removeStaleMapfile(alt)
		return false, err
	}
	if strings.EqualFold(sum, want) {
		if err := os.Rename(alt, dst); err != nil {
			return true, fmt.Errorf("restore: replacing %s: %w", dst, err)
		}
		o.removeStaleMapfile(dst)
		o.UI.Step("replaced the staged %s with this disc's verified copy", name)
		return true, nil
	}
	if isImageName(dst) {
		o.UI.Warn("this disc's copy of %s does not match the recorded hash either; keeping both — par2 will combine them during 'brb restore'", name)
		return true, nil
	}
	// Not an image: a second bad copy is no use to par2 under this name, and
	// two unverifiable sidecars are no better than one.
	o.removeUnverified(alt)
	return false, &CopyProblem{
		Name:    name,
		Missing: -1,
		Reason:  "neither the staged copy nor this disc's matches the hash this disc records for it",
	}
}

// resumeSalvage continues an interrupted ddrescue copy from another pressing of
// the same disc: the regions the first copy could not read are retried, and
// only those.
func (o Options) resumeSalvage(ctx context.Context, src, dst, want string, missing int64) error {
	name := ingestName(src, dst)
	o.UI.Warn("the staged %s is an incomplete salvage (%s missing); retrying those regions from this disc", name, ui.HumanBytes(missing))
	dd := o.Tools.Get(tools.Ddrescue)
	if !dd.Found {
		o.UI.Warn("install gddrescue (ddrescue) to continue an interrupted salvage")
		return &CopyProblem{Name: name, Missing: missing, Reason: "an earlier copy was incomplete and ddrescue is not installed to finish it"}
	}
	still, err := o.ddrescueResume(ctx, dd.Path, src, dst)
	if err != nil {
		return err
	}
	if still != 0 {
		return &CopyProblem{Name: name, Missing: still, Reason: "this copy of the disc could not fill every gap either"}
	}
	o.UI.OK("%s is now complete", name)
	if want == "" {
		return nil
	}
	got, err := agecrypt.SumFile(ctx, dst)
	if err != nil {
		return fmt.Errorf("restore: hashing the salvaged %s: %w", name, err)
	}
	if !strings.EqualFold(got, want) {
		return &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason:  "every region was read, but the result does not match the hash this disc records for it",
		}
	}
	o.UI.Step("%s matches the hash on this disc", name)
	return nil
}

// copyManifest refreshes the staging copy of MANIFEST.txt from the disc. It
// describes the whole set, so any disc's copy will do; a failure is reported
// but does not fail the ingest, since nothing in a restore depends on it.
func (o Options) copyManifest(ctx context.Context, mp string) error {
	src := filepath.Join(mp, manifestName)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("restore: %s: %w", src, err)
	}
	// No removal first: copyStream writes dst+".part" and renames over dst
	// atomically, so a disc whose manifest cannot be read leaves the previous
	// staged copy — and the partial-set announcement it feeds — intact.
	dst := filepath.Join(o.Cfg.Staging, manifestName)
	if _, err := copyStream(ctx, src, dst, nil); err != nil {
		return fmt.Errorf("restore: copying %s: %w", manifestName, err)
	}
	o.UI.Step("%s copied to %s", manifestName, dst)
	return nil
}
