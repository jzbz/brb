package restore

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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// maxPublicIdentityBytes bounds the one file brb reads off a disc whole rather
// than streaming it past a hash. The identity file the writer renders is a
// header comment, one AGE-SECRET-KEY- line and a "# created:" line — under
// 300 bytes. 64 KiB leaves room for a key someone has annotated by hand, or
// re-wrapped, or given a paragraph of provenance, and is still small enough
// that reading it costs nothing on the smallest machine that can run a
// restore. Anything past it is not a key, whatever the disc calls it.
const maxPublicIdentityBytes = 64 << 10

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
	unlock, err := o.lockStaging()
	if err != nil {
		return err
	}
	defer unlock()
	encDir := o.dirs().Enc
	if err := o.secureStaging(encDir); err != nil {
		return err
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

	// Everything below reads a disc somebody else may have mastered, so the
	// names are resolved through a root confined to the mount point. See
	// [discEntry] for what that buys and what it does not.
	discRoot, err := os.OpenRoot(mp)
	if err != nil {
		return false, fmt.Errorf("restore: opening %s: %w", mp, err)
	}
	defer discRoot.Close()

	data := filepath.Join(mp, dataDir)
	switch fi, derr := discEntry(discRoot, dataDir); {
	case derr != nil && !errors.Is(derr, fs.ErrNotExist):
		return false, fmt.Errorf("restore: %s: %w", mp, derr)
	case derr != nil || !fi.IsDir():
		return false, fmt.Errorf("restore: %s has no %s/ directory — is this one of ours?", mp, dataDir)
	}
	o.UI.Log("ingesting %s", mp)

	sums := o.discSums(discRoot, mp)
	names, err := dataFiles(data)
	if err != nil {
		return false, err
	}
	if len(names) == 0 {
		return false, fmt.Errorf("restore: %s is empty", data)
	}
	names = o.withoutStagingNames(names)

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

	// The index first, and before anything at all is written: a disc whose
	// index disagrees with the one already pinned is refused whole, so it
	// leaves nothing in staging — not its images, not a public archive's key.
	// brb.sh's refuse_foreign_index runs at the same point, ahead of its own
	// key check, so the two readers leave the same staging area behind such a
	// disc (refuseForeignIndex says which disc they still treat differently).
	// The index is judged by the same witness the copy loop below uses for it;
	// a damaged sidecar is left for that loop to report, so it is reported
	// once.
	encDir := o.dirs().Enc
	early, judged, err := o.refuseForeignIndex(ctx, mp, names, o.discWant(discRoot, mp, sums, indexName, true))
	if err != nil {
		return false, err
	}

	// A public archive's key comes off the disc root, ahead of the data files:
	// a disc that belongs to a different public set than the one already in
	// staging is refused whole, before any of its images land beside the other
	// set's.
	switch st, err := o.ingestPublicIdentity(discRoot, mp, sums[doc.PublicIdentityName]); {
	case err == nil:
		staged = staged || st
	case errors.Is(err, ErrIncompleteCopy):
		o.UI.Warn("%v", err)
		ig.incomplete++
	default:
		return staged, err
	}

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return staged, err
		}
		want := o.discWant(discRoot, mp, sums, name, false)
		dst := filepath.Join(encDir, stagedSidecarName(name, disc))
		var st bool
		if name == indexName && judged {
			// Both copies were hashed before anything was staged, and nothing
			// since has written to the staged index; reading them again would
			// only reach the same verdict.
			st, err = o.settleIndex(name, dst, early)
		} else {
			st, err = o.ingestFile(ctx, filepath.Join(data, name), dst, want)
		}
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

	if err := o.copyManifest(ctx, discRoot, mp); err != nil {
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
//
// Keying by base name collapses "./data/disc01.squashfs.age" onto
// "disc01.squashfs.age", which is what lets a staged copy be checked against a
// path recorded relative to the disc root. No disc either implementation
// masters can collide under that collapse — the disc root and data/ share no
// file name — so two entries with the same base name mean a hand-mastered,
// merged or damaged sums file. Letting the last one the map iterator happened
// to visit win would make the check answer differently on two runs over the
// same disc, which reads like failing hardware; the ambiguous name is dropped
// instead, so that copy is reported as unverifiable and every other name on
// the disc is still checked.
func (o Options) discSums(root *os.Root, mp string) map[string]string {
	path := filepath.Join(mp, agecrypt.SumsName)
	switch fi, err := discEntry(root, agecrypt.SumsName); {
	case errors.Is(err, fs.ErrNotExist):
		o.UI.Warn("no %s on this disc; copies cannot be checked as they are made", agecrypt.SumsName)
		return nil
	case err != nil:
		o.UI.Warn("could not read %s: %v", path, err)
		return nil
	case !fi.Mode().IsRegular():
		o.UI.Warn("%s on this disc is not a regular file (%s); copies cannot be checked as they are made",
			agecrypt.SumsName, fi.Mode().Type())
		return nil
	}
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
	ambiguous := map[string]bool{}
	for name, hex := range sums {
		base := filepath.Base(name)
		if prev, seen := out[base]; seen && !strings.EqualFold(prev, hex) {
			ambiguous[base] = true
			continue
		}
		out[base] = hex
	}
	for base := range ambiguous {
		delete(out, base)
		o.UI.Warn("%s records two different hashes for %s; that copy cannot be checked as it is made — "+
			"run 'brb verify-disc' against this disc, and re-master it from a good copy of the set if the sums file is damaged",
			agecrypt.SumsName, base)
	}
	return out
}

// discOfDataFiles reports which disc of the set a data directory belongs to,
// read from the numbered image it carries. 0 means it could not be told, which
// is the only case where a sidecars.par2 still has to be staged flat.
func discOfDataFiles(names []string) int {
	for _, nm := range names {
		if n, ok := disc.NumberOf(nm, ".squashfs"+ageExt); ok {
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

// discEntry looks at one name at the root of a mounted disc before anything
// reads it, and is the one place the rule about a disc's own names lives: brb
// wrote them, so none of them is ever a symbolic link, and following one reads
// a file that is not on the disc at all.
//
// The two halves do different jobs. [os.Root] confines the name to the mount,
// so no component resolves outside it; but a Root deliberately still follows a
// link whose target stays inside, and permits a device file, so Lstat here
// refuses the link at the final component and the callers require a regular
// file. Together they are what brb.sh gets from `find -P` declining to descend
// a symlinked start point and from `[[ -f ]]` declining anything that is not a
// regular file, which is the point: the two readers refuse the same discs.
//
// Like brb.sh's, this is a check followed by an open, and the window between
// them is not closed. Closing it would buy little against an attacker who can
// rewrite the mount while the ingest runs: the same window is open for every
// name a disc is read by, and through MANIFEST.txt and data/ (a device
// streamed into staging) such an attacker can already fill the staging disk
// without winning any race. SHA512SUMS is no such route any more:
// [agecrypt.ReadSumFile] stops at its byte cap however the file is fed.
// What the check buys is everything against the case that actually occurs, a
// disc that already carries a link at one of these names when it is handed
// over. The one exception taken is identity.txt, the only file read whole into
// memory: [Options.ingestPublicIdentity] judges the type again on the file it
// opened and bounds the read itself, because there a swap in the window
// otherwise turns a 64 KiB ceiling into none at all.
func discEntry(root *os.Root, name string) (fs.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, rerr := root.Readlink(name)
		if rerr != nil {
			target = "unreadable"
		}
		return nil, fmt.Errorf("%s on this disc is a symbolic link (-> %s); a disc brb wrote "+
			"carries no links at its own names, so this one was mastered by hand or by somebody "+
			"else, and reading through it would take a file that is not on the disc", name, target)
	}
	return fi, nil
}

// dataFiles lists the regular files in a disc's data directory, sorted. Both
// callers, ingestDisc and VerifyDisc, prove the directory itself is not a link
// before this runs (see [discEntry]); within it, os.ReadDir reports the type of
// each dirent rather than of what it points at, so a symlinked entry is not
// regular and is skipped here — the same answer `find -type f` gives brb.sh,
// which tests the link too.
//
// Only the top level is listed. The format's data/ is flat — brb never writes a
// subdirectory there — and a file nested in one has no name in staging that
// would not collide with a top-level one. brb.sh's ingest used to run its find
// without -maxdepth and stage data/sub/disc01.squashfs.age as disc01's image,
// which this reader never did; it now lists the same one level.
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

// stagingOwnName reports whether a name in a disc's data/ is one that staging
// gives a meaning of its own, beyond "a file this disc carries". Ingest stages
// every data file under its own name in one flat enc/ directory, and these are
// the names that directory already uses for something else:
//
//   - *.part is a copy still being written, reaped by the next run.
//   - *.mapfile is a ddrescue map. The Go reader takes enc/<image>.mapfile as
//     the record of a salvage in progress and resumes it: ddrescue then writes
//     into the staged image in place, from whichever disc is in the drive, and
//     with no recorded hash nothing checks the result. A disc carrying
//     data/disc01.squashfs.age.mapfile marking the whole image unread used to
//     be staged under that name and so had the staged image overwritten with
//     its own bytes on the next ingest — the one thing reconcileExisting
//     promises never happens.
//   - <image>.copy* is another pressing's copy of an image, which both readers
//     hand to par2 at restore beside the primary.
//   - identity.txt is where a public archive's key is staged. It comes off the
//     disc ROOT, past the key contract in [Options.ingestPublicIdentity]; a
//     data/identity.txt staged under the same name went round that contract,
//     and with a recorded hash could replace the staged key outright.
//   - sidecars-disc* is where each disc's sidecar parity is staged under its
//     number (see stagedSidecarName), so a disc carrying one could take the
//     place of another disc's set.
//
// A disc brb wrote carries none of these in data/ — its names are frozen
// format, and none of them is one of these — so such a file is skipped and
// said so, not staged. brb.sh's ingest loop skips the same five patterns with
// the same warning.
func stagingOwnName(name string) bool {
	switch {
	case strings.HasSuffix(name, partExt), strings.HasSuffix(name, mapfileExt):
		return true
	case strings.Contains(name, ".squashfs"+ageExt+".copy"):
		return true
	case name == doc.PublicIdentityName:
		return true
	case strings.HasPrefix(name, sidecarsStem+"-disc"):
		return true
	}
	return false
}

// withoutStagingNames drops the names [stagingOwnName] keeps for staging's own
// use from a disc's data files, warning about each one it drops.
func (o Options) withoutStagingNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if stagingOwnName(name) {
			o.UI.Warn("%s on this disc has a name staging keeps for its own bookkeeping, and no disc brb wrote "+
				"carries one in %s/ — not staging it", name, dataDir)
			continue
		}
		out = append(out, name)
	}
	return out
}

// discWant is the hash this disc records for one of its data files: its
// SHA512SUMS entry, or, when SHA512SUMS has none, the digest the file's own
// per-file sidecar beside it records ("data/disc03.squashfs.age.sha512" for
// the image, "data/index.tsv.gz.age.sha512" for the index). "" means the disc
// records nothing usable for it, and the copy goes unchecked.
//
// The sidecar is the second witness brb.sh has consulted since 2ad5f0c, and
// for a disc whose SHA512SUMS is missing or rotted it is the only one left
// for the image and the index. Without it the two readers disagreed about
// the same disc: an image that did not match its sidecar was counted bad by
// brb.sh and staged as fine here, and an index whose copy had rotted, on a
// disc whose sidecar still vouched for the staged one, was ingested by
// brb.sh as damage and refused here as another set's. Adding the witness
// rather than dropping it from brb.sh is the direction that keeps a check.
//
// A sidecar that is a link, or that does not parse, or that records two
// digests for the name, is no witness — reported unless quiet, and then
// treated exactly as an absent one, which is how restore treats the same
// sidecar once it is staged (recordedCipherSum).
func (o Options) discWant(root *os.Root, mp string, sums map[string]string, name string, quiet bool) string {
	if want := sums[name]; want != "" {
		return want
	}
	want, err := discSidecarSum(root, mp, name)
	if err != nil && !quiet {
		o.UI.Warn("%v", err)
	}
	return want
}

// discSidecarSum reads the digest data/<name>.sha512 records for name, through
// the disc root and by the same rules as every other name ingest reads off a
// disc: no link at the name ([discEntry]), a regular file, and the
// [recordedSum] parse, under which the sidecar records a digest but does not
// get to choose which file it is for. brb.sh's disc_sidecar_sum makes the same
// three checks with -L, -f and recorded_sum.
func discSidecarSum(root *os.Root, mp, name string) (string, error) {
	rel := filepath.Join(dataDir, name+sumExt)
	switch fi, err := discEntry(root, rel); {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("%v; %s cannot be checked against it", err, name)
	case !fi.Mode().IsRegular():
		return "", nil
	}
	want, ok, err := recordedSum(filepath.Join(mp, rel), name, "")
	if err != nil {
		return "", fmt.Errorf("%s%s on this disc is damaged, so %s cannot be checked against it either", name, sumExt, name)
	}
	if !ok {
		return "", nil
	}
	return want, nil
}

// ingestFile copies one file off the disc, or explains why it did not. staged
// reports whether anything new reached the staging area — a fresh copy, a
// resumed salvage, a verified replacement or an alternate copy — as opposed to
// "already have", which stages nothing.
//
// want is the hash the disc records for it — its SHA512SUMS entry, or failing
// that its per-file sidecar ([Options.discWant]) — or "" when the disc records
// nothing usable. When it is known the copy is checked against it for free,
// because copyStream hashes as it writes.
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

	// The index is the one file whose staged copy is authoritative rather than
	// merely first, so it is decided before the ordinary rules below get to it.
	if isIndexName(dst) {
		return o.reconcileIndex(ctx, src, dst, want)
	}

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
		return o.replaceOrKeepBoth(ctx, src, dst, want, got)
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
		sum, err := o.copyRobustly(ctx, src, alt)
		if err != nil {
			return true, err
		}
		// It differs from the primary, but it may be a pressing read before:
		// the same disc ingested twice is the same bytes twice, and a second
		// copy of them is a whole image of staging that gives par2 nothing.
		if same, err := o.sameAltCopy(ctx, dst, alt, sum); err != nil {
			return true, err
		} else if same != "" {
			o.removeUnverified(alt)
			o.removeStaleMapfile(alt)
			o.UI.Step("this disc's copy of %s is byte for byte %s, staged from an earlier read, so it was not kept a second time", name, same)
			return false, nil
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

// isIndexName reports whether a staged path is the set's encrypted index, the
// one file whose staged copy pins what the whole set is (see reconcileIndex).
func isIndexName(dst string) bool {
	return filepath.Base(dst) == indexName
}

// reconcileIndex decides what to do about an index that is already staged, and
// refuses the disc when the two do not agree. It is half of the mitigation for
// RVW-008; [Options.auditImage] is the other half, and neither is worth
// anything alone.
//
// The writer copies ONE index file onto every disc of a set, so two discs of
// one set carry it byte for byte. A difference therefore means one of three
// things, and none of them is a disc to go on ingesting:
//
//   - The discs of two different sets are being ingested into one staging area.
//     Their images would end up interleaved in one enc/ under one index that
//     describes half of them, and a restore would extract both sets into the
//     destination while reporting one.
//   - The staged copy has rotted, or this disc's has.
//   - One of the two discs is not from the operator's set at all. age encrypts
//     to a PUBLIC key that MANIFEST.txt prints on every disc, so anyone holding
//     one disc can master another that decrypts and verifies clean — see the
//     RVW-008 note on [Options.auditImage]. Pinning the index is what forces
//     such a disc to ship the GENUINE index, which is the only reason the
//     image cross-check at restore can catch it: the attacker cannot read the
//     index they were made to carry, so they cannot make their image agree
//     with it.
//
// This is deliberately NOT the keep-both treatment images get. Two partially
// rotted copies of an image are raw material par2 combines into a whole one, on
// purpose; two different indexes are two different answers to "what is in this
// set", and keeping both would just mean choosing one silently.
//
// The refusal is modelled on the public-archive key contract a few functions
// down (ingestPublicIdentity): same shape, same reasoning, same remedy of
// finishing or clearing the staging area before the other set goes into it.
//
// Rot is told apart from a foreign index by what the disc itself records for
// its index — its SHA512SUMS entry, or failing that the index's own sidecar
// ([Options.discWant]), the two witnesses brb.sh's refuse_foreign_index asks in
// the same order — and only where that can settle it:
//
//   - The staged copy is what this disc records the index should be, and the
//     copy beside it on the medium is not. The disc agrees with the staged
//     index and has simply lost its own copy of it; that is a warning, and the
//     ingest carries on. This is the case the plain-file rules already handled
//     ("already have %s, and it matches the hash on this disc"), and turning it
//     into a failure would fail an honest disc for nothing.
//   - Neither copy matches what the disc records. The disc contradicts itself,
//     so it cannot be believed about anything, and it is reported as the
//     damaged copy it is.
//
// A forged disc reaches neither: its sums file agrees with its index, because
// the attacker wrote both. That is what leaves the refusal below for the case
// where two self-consistent discs disagree with each other.
//
// The same verdict is reached once before this, by [Options.refuseForeignIndex],
// ahead of anything the disc stages. A disc's image sorts before its index
// ("disc07.squashfs.age" before "index.tsv.gz.age"), so when this function was
// the only check, a refused disc had already left its image, its sidecars and
// a public archive's key in enc/ — inert, and refused again at restore by the
// cross-check, but beside the other set's files, and contrary to what brb.sh
// does. When that early look reaches a verdict, the copy loop reports it
// through [Options.settleIndex] without coming here, so the index is read once
// per disc, not twice. The loop comes here only when the early look could not
// hash the two copies, and the refusal in settleIndex is then reached only if
// this second look can.
func (o Options) reconcileIndex(ctx context.Context, src, dst, want string) (staged bool, err error) {
	name := ingestName(src, dst)
	verdict, err := judgeIndex(ctx, name, src, dst, want)
	if err != nil {
		return false, err
	}
	return o.settleIndex(name, dst, verdict)
}

// settleIndex reports what an [indexVerdict] means for the disc: the index
// staged at dst stays as it is whatever the verdict, and only a foreign one
// fails the disc outright. See [Options.reconcileIndex] for the reasoning.
func (o Options) settleIndex(name, dst string, verdict indexVerdict) (staged bool, err error) {
	switch verdict {
	case indexSame:
		o.UI.Step("already have %s, and this disc's copy is identical — this disc belongs to the staged set", name)
		return false, nil
	case indexVouchedFor:
		// This disc vouches for the staged index and has lost its own copy of
		// it. Nothing is wrong with the set, and nothing here needs
		// restaging.
		o.UI.Warn("already have %s, and it matches the hash on this disc — but this disc's copy is damaged "+
			"and was not staged; %s if you need this disc's copy back", name, o.sidecarRepairHint(0))
		return false, nil
	case indexSelfContradicting:
		// The disc contradicts itself, so it cannot be believed about which
		// set it belongs to either. Nothing is staged: the pinned index stays.
		return false, &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason: "this disc's copy of the index matches neither the hash this disc itself records for it nor the " +
				"index already staged, so this disc's copy is damaged; the staged index was kept — " +
				o.sidecarRepairHint(0),
		}
	}
	// Reached only when refuseForeignIndex could not hash the two copies and
	// reconcileIndex could, so what sorted before the index may be staged
	// already.
	return false, o.foreignIndexError(dst, "Nothing from this disc was added to the index")
}

// indexVerdict is what a disc's copy of the index says about the disc, set
// against the index already staged. See [Options.reconcileIndex] for the
// reasoning behind each.
type indexVerdict int

const (
	// indexSame: the two copies are identical, as every disc of one set's are.
	indexSame indexVerdict = iota
	// indexVouchedFor: the disc records the staged index's hash and has lost
	// its own copy of it. Rot on this disc, not another set.
	indexVouchedFor
	// indexSelfContradicting: neither copy matches what the disc records. Rot
	// on this disc, which therefore cannot be believed about its set either.
	indexSelfContradicting
	// indexForeign: the disc vouches for an index that is not the staged one,
	// or records nothing to judge by. Another set, or not a set at all.
	indexForeign
)

// judgeIndex hashes both copies of the index and says which [indexVerdict]
// they amount to. It reads and reports nothing else, so it can be asked before
// a disc has staged anything. want is the hash the disc records for its index,
// or "".
func judgeIndex(ctx context.Context, name, src, dst, want string) (indexVerdict, error) {
	staged, err := agecrypt.SumFile(ctx, dst)
	if err != nil {
		return 0, fmt.Errorf("restore: hashing the staged %s: %w", name, err)
	}
	onDisc, err := agecrypt.SumFile(ctx, src)
	if err != nil {
		return 0, fmt.Errorf("restore: hashing %s on the disc: %w", name, err)
	}
	switch {
	case strings.EqualFold(staged, onDisc):
		return indexSame, nil
	case want == "":
		return indexForeign, nil
	case strings.EqualFold(staged, want):
		return indexVouchedFor, nil
	case !strings.EqualFold(onDisc, want):
		return indexSelfContradicting, nil
	}
	return indexForeign, nil
}

// refuseForeignIndex is reconcileIndex's refusal, made before a disc stages
// anything: when an index is already pinned and this disc carries a different
// one that it vouches for (or records nothing about), the disc is refused here
// and leaves staging exactly as it found it. brb.sh's refuse_foreign_index
// runs at the same point, ahead of the published key as this does, so for a
// disc whose index both readers can read the two refuse the same discs, by the
// same witnesses in the same order. Any other verdict is returned, with judged
// set, for the copy loop to report when it reaches the index
// ([Options.settleIndex]) instead of hashing both copies a second time.
//
// An index this disc cannot even be read back from gets no verdict here, and
// that is deliberate; it is also where the two readers part. A read error
// hashing the disc's copy says nothing about which set the disc belongs to, so
// this reader does not refuse over it: the disc's readable images are staged,
// as this reader has always staged them for a damaged disc, and the read error
// fails the disc when the copy loop reaches the index (reconcileIndex). brb.sh
// cannot read the copy either, but its cmp then reads as a difference: with
// nothing on the disc recording the index's hash, it refuses the disc as
// another set's with nothing staged; with something recording it, it takes the
// copy for rot and ingests the rest of the disc. README.md's table of reader
// differences records it.
func (o Options) refuseForeignIndex(ctx context.Context, mp string, names []string, want string) (verdict indexVerdict, judged bool, err error) {
	if !slices.Contains(names, indexName) {
		return 0, false, nil
	}
	dst := filepath.Join(o.dirs().Enc, indexName)
	if _, err := os.Stat(dst); err != nil {
		// Nothing pinned yet: this disc's index becomes the pinned one. (Any
		// other failure is the copy loop's to report, where it stats dst too.)
		return 0, false, nil
	}
	verdict, err = judgeIndex(ctx, indexName, filepath.Join(mp, dataDir, indexName), dst, want)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return 0, false, cerr
		}
		return 0, false, nil
	}
	if verdict == indexForeign {
		return 0, false, o.foreignIndexError(dst, "Nothing from this disc was staged")
	}
	return verdict, true, nil
}

// foreignIndexError is the refusal of a disc whose index is not the one pinned
// at dst. kept says what the refusal left alone, which depends on how far the
// disc got before it.
func (o Options) foreignIndexError(dst, kept string) error {
	return fmt.Errorf("restore: this disc carries an index that differs from the one already staged at %s — "+
		"every disc of one set carries the identical index, so this disc belongs to a different set, or one of the two "+
		"indexes is not the set's at all. %s, and it must not be ingested "+
		"beside the other set's images: finish or clear %s before ingesting the other set. If both discs are meant to be "+
		"from one set, one of them is not what it claims to be, and the images already staged cannot be told apart from "+
		"here — ingest each disc into an empty staging directory and compare",
		dst, kept, o.Cfg.Staging)
}

// altCopyName is where a further pressing's copy of an already-staged file
// goes: the same ".copy<unixtime>" convention brb.sh uses, in the same
// directory, so either reader's par2 pass finds the other's copies — each
// takes every name with that prefix, so any number after it will do.
//
// It is the first such name at or after now that is FREE. It used to be now,
// whatever was there: copyStream renames its finished copy over the name and
// brb.sh's cp truncates it, so two pressings read within one second — a
// clock stepped back, a machine with no RTC that boots to the same second, a
// script looping ingest over small images — had the second destroy the first,
// the very copy par2 might need, without a word. A failed copy that removes
// its own unverified output then deleted the earlier pressing outright.
//
// The search advances only while Lstat finds the name taken, and stops on any
// other answer: a name too long for the filesystem, or a staging disk that
// cannot be read, is returned for the copy to fail on and report, as it always
// was, rather than searched past forever. That is the same test brb.sh's
// alt_copy_name makes with [[ -e || -L ]], which is false for those too. It
// ends because a directory holds finitely many names, and the staging lock
// keeps any other run of either reader from taking one in between.
func altCopyName(dst string) string {
	for t := time.Now().Unix(); ; t++ {
		alt := fmt.Sprintf("%s.copy%d", dst, t)
		if _, err := os.Lstat(alt); err != nil {
			return alt
		}
	}
}

// sameAltCopy names the alternate copy of dst, already staged, that holds
// exactly the bytes a new copy at alt hashed to (sum), or "" when none does.
//
// Only a complete pressing's copy is a candidate: a regular file under the
// .copy name, and not the new copy itself, a salvage's map file, a copy still
// being written, or an incomplete salvage — an alternate with a map file
// beside it, whose unread regions are zeros the drive never returned, so a
// complete read matches it only by accident and hashing it is a read of
// staging spent for nothing. A regular file, not merely a name that is not a
// directory as altCopies lists them, because a FIFO left in staging would
// block the hash forever.
//
// Each candidate is hashed, which is one read of staging per alternate — and
// only on the path that has just read the same number of bytes off an optical
// disc, which is far slower. A candidate that cannot be hashed is passed over,
// so the new copy is kept: keeping is the side to err on. brb.sh's
// same_alt_copy makes the same comparison over the same names, its -L before
// -f standing in for the Lstat here.
func (o Options) sameAltCopy(ctx context.Context, dst, alt, sum string) (string, error) {
	dir := filepath.Dir(dst)
	for _, c := range altCopies(dir, filepath.Base(dst)) {
		p := filepath.Join(dir, c)
		if p == alt || strings.HasSuffix(c, mapfileExt) || strings.HasSuffix(c, partExt) {
			continue
		}
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if _, err := os.Lstat(p + mapfileExt); err == nil {
			continue
		}
		got, err := agecrypt.SumFile(ctx, p)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return "", cerr
			}
			continue
		}
		if strings.EqualFold(got, sum) {
			return c, nil
		}
	}
	return "", nil
}

// replaceOrKeepBoth handles a staged copy that fails the hash this disc
// records. The disc's version is read to a sibling name and hashed on the way;
// only a copy proven whole is renamed over the staged one — the staged bytes
// are never destroyed ahead of that proof. A copy that is damaged too is kept
// under the .copy name for par2 to combine at restore time, which is the one
// way two bad burns still add up to a whole image.
//
// have is the staged copy's own hash, which the caller has just taken. A
// damaged copy is only worth keeping when it is DIFFERENT damage: a disc that
// returns the same wrong bytes every time — burned wrong, read through a
// faulty drive or bridge, or carrying a sums line that was wrong from the
// start — used to leave one more byte-identical .copy per re-read, each a
// whole image of staging and one more operand for par2 to scan, with not one
// new block among them — and both readers' advice after a bad copy invited
// exactly those re-reads. A copy identical to the staged one,
// or to an alternate already staged, is dropped and reported as the still
// unverified copy it is: a *CopyProblem, as brb.sh counts the same file bad.
func (o Options) replaceOrKeepBoth(ctx context.Context, src, dst, want, have string) (staged bool, err error) {
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
		same := ""
		if strings.EqualFold(sum, have) {
			same = "the staged one"
		} else if c, err := o.sameAltCopy(ctx, dst, alt, sum); err != nil {
			return true, err
		} else if c != "" {
			same = c + ", staged from an earlier read"
		}
		if same != "" {
			o.removeUnverified(alt)
			o.removeStaleMapfile(alt)
			return false, &CopyProblem{
				Name:    name,
				Missing: -1,
				Reason: "this disc's copy is byte for byte " + same + ", and does not match the hash this disc records " +
					"for it either, so it was not kept a second time — reading this disc again will not help; ingest " +
					"another pressing, or let par2 repair it during 'brb restore'",
			}
		}
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
func (o Options) copyManifest(ctx context.Context, root *os.Root, mp string) error {
	src := filepath.Join(mp, manifestName)
	switch fi, err := discEntry(root, manifestName); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("restore: %s: %w", src, err)
	case !fi.Mode().IsRegular():
		// A device node here used to be copied, which meant streaming it: the
		// hash is taken as the bytes go past, so nothing judged the file until
		// it ended, and /dev/zero does not end.
		return fmt.Errorf("restore: %s on this disc is not a regular file (%s), so it was not copied",
			manifestName, fi.Mode().Type())
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

// ingestPublicIdentity stages a public archive's key. A set built with
// --public-archive carries its secret key as identity.txt at the root of every
// disc, and until the key is somewhere a restore looks, the set cannot be
// restored by brb without the operator finding that file and setting
// AGE_IDENTITY to it by hand. So ingest copies it to <STAGING>/enc/identity.txt
// — the very path the writer leaves its own copy of the key at while it
// builds the set — and identities() picks it up from there. brb.sh does the
// same, to the same path, so a staging area is interchangeable between the two
// readers.
//
// The copy is checked against the disc's SHA512SUMS entry for ./identity.txt
// like every other file taken off a disc: a key with a rotted character
// decrypts nothing, and it is better to hear that at ingest, with the disc in
// hand and the same key printed in MANIFEST.txt and README.md, than at restore.
// A mismatch is reported the way a damaged data file is, as an incomplete
// copy, and nothing is staged: a wrong key beside the images would only send
// the restore down the wrong path. This is the one file read into memory
// whole rather than streamed past its hash — it has to be parsed as a key, not
// copied — so its size is judged first, against [maxPublicIdentityBytes], and
// an implausible one is refused the same way; the read is then bounded by the
// same ceiling, so a file that is not what the stat said cannot outrun it.
//
// A staged key that already exists is compared, not overwritten. Identical
// contents are the ordinary case — every disc of a public set carries the same
// key, and so does the writer's leftover. Different contents mean the discs of
// two different public archives are being ingested into one staging area, and
// their images would end up interleaved in one enc/ under one key that opens
// half of them; that disc is refused rather than staged.
//
// A disc without an identity.txt is a private set, and nothing happens. want
// is the digest the disc's SHA512SUMS records for the file, or "" when the
// disc carries no sums.
func (o Options) ingestPublicIdentity(root *os.Root, mp, want string) (staged bool, err error) {
	src := filepath.Join(mp, doc.PublicIdentityName)
	name := doc.PublicIdentityName
	// Before the open, and before the size gate below: that gate reads
	// fi.Size(), which is 0 for a device node, so a character device here slid
	// under it and io.ReadAll took the process out through the allocator —
	// exactly the death the gate was written to prevent.
	switch fi, derr := discEntry(root, name); {
	case errors.Is(derr, fs.ErrNotExist):
		return false, nil
	case derr != nil:
		return false, fmt.Errorf("restore: reading %s: %w", src, derr)
	case !fi.Mode().IsRegular():
		return false, &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason: fmt.Sprintf("is not a regular file on this disc (%s); a public archive's key is a "+
				"few hundred bytes of text, so this is not one and it was not read or staged", fi.Mode().Type()),
		}
	}
	if testHookBeforeIdentityOpen != nil {
		testHookBeforeIdentityOpen()
	}
	// Through the root, like the Lstat above, rather than by plain path: a
	// name re-resolved from the top could leave the disc altogether.
	f, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("restore: reading %s: %w", src, err)
	}
	defer f.Close()

	// The size is judged before a byte is read, and from the open file rather
	// than a second stat, because this is the one file taken off a disc that
	// is read whole into memory instead of streamed past a hash in 1 MiB
	// chunks. Everything that would reveal an absurd file — the disc's
	// SHA512SUMS entry, the age parse — happens after the read, so without
	// this an inserted disc naming a 40 GB file identity.txt takes the process
	// out through the allocator before any check runs, and the operator's
	// diagnosis is an out-of-memory kill rather than "that file is not a key".
	// A public set cannot be ingested at all until they work out which file
	// did it.
	//
	// The type is judged again here, on the descriptor, because the check
	// above was made on a name and this is the file actually open. A device
	// or a FIFO put at the name since reports a size of 0, and would slide
	// under the size gate just as a device node there did before that check
	// was added.
	tooBig := func(size string) error {
		return &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason: fmt.Sprintf("is %s bytes on this disc; a public archive's key is a few hundred, so this file is not one "+
				"and it was not read or staged — the key is printed in the disc's MANIFEST.txt and README.md, "+
				"and AGE_IDENTITY can be pointed at a copy typed out from there", size),
		}
	}
	if fi, err := f.Stat(); err != nil {
		return false, fmt.Errorf("restore: reading %s: %w", src, err)
	} else if !fi.Mode().IsRegular() {
		return false, &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason: fmt.Sprintf("is not a regular file on this disc (%s); a public archive's key is a "+
				"few hundred bytes of text, so this is not one and it was not read or staged", fi.Mode().Type()),
		}
	} else if fi.Size() > maxPublicIdentityBytes {
		return false, tooBig(strconv.FormatInt(fi.Size(), 10))
	}
	if testHookBeforeIdentityRead != nil {
		testHookBeforeIdentityRead()
	}
	// And the read itself is bounded, whatever the stat said. A size is a
	// claim made before the read, not a limit on it: a file that grows after
	// the stat, or a filesystem whose st_size understates what it serves (a
	// FUSE mount with direct_io, say), sails past the gate above and into an
	// unbounded io.ReadAll. One byte past the ceiling is enough to know it is
	// past the ceiling.
	body, err := io.ReadAll(io.LimitReader(f, maxPublicIdentityBytes+1))
	if err != nil {
		return false, fmt.Errorf("restore: reading %s: %w", src, err)
	}
	if len(body) > maxPublicIdentityBytes {
		return false, tooBig(fmt.Sprintf("more than %d", maxPublicIdentityBytes))
	}
	if want != "" {
		sum := sha512.Sum512(body)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
			return false, &CopyProblem{
				Name:    name,
				Missing: -1,
				Reason: "the public archive's key on this disc does not match the hash this disc records for it, " +
					"so it was not staged; the same key is printed in the disc's MANIFEST.txt and README.md — " +
					"check it against those, and set AGE_IDENTITY to a corrected copy",
			}
		}
	}

	// This disc's copy has to be a usable identity before it is compared with
	// anything or staged: a file that merely passed the disc's hash could still
	// be an empty or foreign file if the set was mastered by hand.
	thisKey, err := parseX25519Identity(body)
	if err != nil {
		return false, &CopyProblem{
			Name:    name,
			Missing: -1,
			Reason:  fmt.Sprintf("the file on this disc is not an age identity (%v); the same key is printed in the disc's MANIFEST.txt and README.md", err),
		}
	}

	dst := o.stagedPublicIdentity()
	switch have, err := os.ReadFile(dst); {
	case err == nil:
		// "The same key" is judged by the key, not by the bytes. The writer
		// stamps every identity file it renders with a "# created:" line, so a
		// set whose discs were laid out across a second boundary can carry the
		// same key under different bytes; brb.sh's ingest compares the
		// AGE-SECRET-KEY- line for the same reason. Two different KEYS, on the
		// other hand, really are two different sets.
		haveKey, perr := parseX25519Identity(have)
		if perr == nil && haveKey.String() == thisKey.String() {
			o.UI.Step("already have the public archive's key %s, and this disc's matches it", name)
			return false, nil
		}
		if perr != nil {
			return false, fmt.Errorf("restore: the staged public archive key %s is not an age identity (%v); "+
				"remove it and ingest again", dst, perr)
		}
		return false, fmt.Errorf("restore: this disc carries a public archive's key that differs from the one already staged at %s — "+
			"two different public sets are being ingested into one staging area, and their images would end up "+
			"under one key that opens only some of them; finish or clear %s before ingesting the other set",
			dst, o.Cfg.Staging)
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("restore: %s: %w", dst, err)
	}

	// Written .part-then-rename like every other staged file, so an ingest
	// interrupted here leaves no half-written key for identities() to choke on.
	if err := writeStagedFile(dst, body, 0o600); err != nil {
		return true, fmt.Errorf("restore: staging the public archive's key: %w", err)
	}
	if want != "" {
		o.UI.Step("%s copied and verified — the public archive's key is now in %s", name, filepath.Dir(dst))
	} else {
		o.UI.Step("%s copied — the public archive's key is now in %s", name, filepath.Dir(dst))
	}
	return true, nil
}

// testHookBeforeIdentityOpen and testHookBeforeIdentityRead run in
// [Options.ingestPublicIdentity]'s two windows: between the check of the name
// and the open, and between the size gate on the open file and the read. Both
// are nil in every build but the tests that change the file in those windows;
// an interleaving is not something a test can schedule from the outside, and
// the claim of the code between them is that the change makes no difference.
var (
	testHookBeforeIdentityOpen func()
	testHookBeforeIdentityRead func()
)

// writeStagedFile writes body to dst through a ".part" sibling that is
// created fresh (see fsx.CreateFresh — never through a symlink planted at the
// name), fsynced, and renamed into place, so dst is either whole or absent.
func writeStagedFile(dst string, body []byte, mode os.FileMode) error {
	part := dst + partExt
	f, err := fsx.CreateFresh(part, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(part)
		return fmt.Errorf("writing %s: %w", part, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(part)
		return fmt.Errorf("syncing %s: %w", part, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return fmt.Errorf("closing %s: %w", part, err)
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return fmt.Errorf("renaming %s: %w", part, err)
	}
	return nil
}

// parseX25519Identity reads the one X25519 identity an identity-file body
// holds, in the format age-keygen and brb write: comment lines beginning "#",
// blank lines, and exactly one AGE-SECRET-KEY-1... line.
func parseX25519Identity(body []byte) (*age.X25519Identity, error) {
	ids, err := age.ParseIdentities(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var found *age.X25519Identity
	for _, id := range ids {
		x, ok := id.(*age.X25519Identity)
		if !ok {
			return nil, errors.New("holds a key that is not X25519")
		}
		if found != nil {
			return nil, errors.New("holds more than one key")
		}
		found = x
	}
	if found == nil {
		return nil, errors.New("holds no key")
	}
	return found, nil
}
