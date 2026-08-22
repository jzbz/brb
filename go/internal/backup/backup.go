// Package backup turns a source tree into a set of independent, encrypted,
// mountable Blu-ray discs.
//
// The pipeline:
//
//	bin-pack the tree into disc-sized groups
//	  -> mksquashfs (one self-contained image per disc)
//	    -> age (encrypt the image)
//	      -> par2 (recovery data over the ciphertext)
//	        -> xorriso (one ISO per disc)
//
// This is the only writer. brb.sh, which ships on every disc, reads a set and
// never builds one (see its header), so nothing outside this package observes
// how discs are packed — only the on-disc format the reader depends on is
// frozen. Where a comment below says something is fixed, it says which of the
// two reasons it means.
//
// The plaintext image is never deleted until the ciphertext has been hashed,
// the image has been proven to be a readable squashfs, and — when
// [Options.VerifyRoundTrip] is set — the ciphertext has been decrypted back
// and compared with the plaintext hash. And the run is resumable: after every
// completed disc a state file is written to <Staging>/state.json so that an
// interrupted twenty-disc set can be continued with Run(Resume: true) instead
// of started over.
//
// This package is Unix-only; it reads the free space of the staging filesystem
// with statfs(2).
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/pack"
	"github.com/jzbz/brb/internal/scan"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// Version is the brb version recorded in MANIFEST.txt, in each disc's README
// and in the ISO application id. It must stay equal to brb.sh's VERSION
// (brb.sh:61): both are printed on the same disc, and a restorer comparing
// them must not be told the reader and the writer are different releases.
const Version = "0.1.1"

// shrinkMargin is the safety factor applied to a measured compression ratio
// before an over-budget disc is re-packed with it: at exactly the measured
// ratio the disc only fits by integer truncation.
const shrinkMargin = 1.05

// Options configures [Plan] and [Run].
type Options struct {
	// Cfg is the loaded configuration. Required.
	Cfg *config.Config
	// UI receives progress and diagnostics. A nil printer discards everything,
	// which also means no operator confirmation can be obtained: Run then needs
	// a printer with AssumeYes set.
	UI *ui.Printer
	// Tools is the detected external tool set. Nil makes Run detect them.
	Tools *tools.Set
	// Resume continues an interrupted run from <Staging>/state.json.
	Resume bool
	// VerifyRoundTrip decrypts every image back after encrypting it and
	// compares the plaintext hash before the plaintext is deleted. It needs a
	// readable AGE_IDENTITY and doubles the time spent on each image.
	VerifyRoundTrip bool
	// DryRun plans the disc layout and reports it without building anything.
	DryRun bool
}

// PlanResult is the disc layout [Plan] computed.
type PlanResult struct {
	// Discs is the number of discs the layout needs.
	Discs int
	// RawBytes is the uncompressed size of everything that would be written,
	// hard links charged once.
	RawBytes int64
	// PerDisc describes each disc in order.
	PerDisc []PlanDisc
	// PackRatio is the assumed compressed/raw ratio the layout used.
	PackRatio float64
}

// PlanDisc is one disc of a [PlanResult].
type PlanDisc struct {
	// Index is the 1-based disc number.
	Index int
	// Files is the number of data files on the disc.
	Files int
	// RawBytes is their uncompressed size, hard links charged once.
	RawBytes int64
}

// RequiredSpace returns the number of free bytes the staging filesystem must
// have before a backup may start: room for one plaintext image and, beside it,
// one ciphertext with its par2 recovery data.
//
// That pair is the real peak of a disc. The plaintext image is not removed
// until the ciphertext has been written, hashed, protected and — under
// [Options.VerifyRoundTrip] — decrypted back, so both exist at once; nothing
// else in the pipeline is image-sized. The round-trip check used to add a
// third image-sized file here, because it decrypted to a temporary file it
// never read back; it hashes the plaintext as it streams now and writes
// nothing, so the requirement no longer charges for a copy that does not
// exist.
//
// It is a floor, not the total a full run consumes. Every finished disc leaves
// its ciphertext and parity in <Staging>/enc, and its ISO in <Staging>/iso,
// until the operator clears staging, so a set of N discs eventually needs
// roughly N times this much.
func RequiredSpace(imageBudget int64, par2Redundancy int) int64 {
	if imageBudget <= 0 {
		return 0
	}
	if par2Redundancy < 0 {
		par2Redundancy = 0
	}
	// A disc budget is at most a few hundred gigabytes, so the multiplication
	// cannot overflow; guard anyway rather than return a negative requirement.
	scaled := imageBudget * int64(100+par2Redundancy) / 100
	if scaled < imageBudget {
		return math.MaxInt64
	}
	return imageBudget + scaled
}

// rawBudget converts a compressed-size budget into the raw-content budget the
// packer works in: integer truncation of budget/ratio, with a non-positive or
// non-finite ratio treated as 1.0. Truncating rather than rounding is the safe
// direction — it plans slightly less content per disc, never slightly more.
func rawBudget(imageBudget int64, ratio float64) int64 {
	if !(ratio > 0) || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		ratio = 1.0
	}
	b := int64(float64(imageBudget) / ratio)
	if b < 1 {
		b = 1
	}
	return b
}

// round3 rounds to three decimal places, which is the precision every pack
// ratio is carried and printed at, so the number in the log is the number the
// budget was derived from.
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// measuredRatio is the compressed/raw ratio an image actually achieved.
func measuredRatio(imageSize, rawBytes int64) float64 {
	if rawBytes <= 0 {
		return 1.0
	}
	return round3(float64(imageSize) / float64(rawBytes))
}

// shrinkRatio returns the pack ratio to re-plan an over-budget disc with: the
// ratio the image actually achieved, plus a 5% margin.
//
// Both roundings are deliberate: the ratio the operator is shown, the ratio
// recorded in state.json and the ratio the next raw budget is computed from
// are then the same three-decimal number, so a re-plan is reproducible from
// what the log says. Nothing outside this package reads it — the pack ratio is
// not part of the on-disc format — so the arithmetic is free to change.
func shrinkRatio(imageSize, rawBytes int64) float64 {
	return round3(measuredRatio(imageSize, rawBytes) * shrinkMargin)
}

// imageName is the plaintext image name for a disc, e.g. "disc03.squashfs".
func imageName(n int) string { return fmt.Sprintf("disc%02d.squashfs", n) }

// discDirName is the per-disc staging directory name, e.g. "disc03".
func discDirName(n int) string { return fmt.Sprintf("disc%02d", n) }

// indexName is the file name of the encrypted index carried on every disc.
const indexName = "index.tsv.gz.age"

// runner holds everything one Plan or Run needs. It exists so the pipeline
// reads as a sequence of small steps rather than one function.
type runner struct {
	opts   Options
	cfg    *config.Config
	p      *ui.Printer
	tools  *tools.Set
	dirs   config.Dirs
	budget disc.Budget

	statePath string
	st        *State
	packRatio float64
	// est learns packRatio back from the discs this run measures; see ratio.go.
	est     *ratioEstimator
	started time.Time

	// indexBuilt records that a resumed run found the encrypted index already
	// written, so the plaintext index it was built from is legitimately gone.
	indexBuilt bool

	// skippedMounts holds the mount points the scan stopped at, relative to
	// SOURCE_DIR. They are warned about when the scan reports them and listed
	// again in MANIFEST.txt's "excluded from this backup" section, so a disc
	// read years later still says what was under the tree and not on the set.
	skippedMounts []string

	// unreadable holds the paths the scan could not open, relative to
	// SOURCE_DIR, capped at maxNamedUnreadable; unreadableCount is the true
	// total. They are warned about when the scan reports them and listed again
	// in MANIFEST.txt for the same reason skippedMounts are: a file that could
	// not be read is not on any disc, the terminal that said so is long gone by
	// the time anyone reads the discs, and a restore that quietly lacks a file
	// looks exactly like a backup that never had it.
	unreadable      []string
	unreadableCount int

	// sidecarFailures lists the discs whose sidecars.par2 could not be written.
	// That is a warning rather than an error (see protectSidecars), but it is a
	// warning about a disc that will be burned and shelved, so it is repeated
	// in the closing summary instead of being left hours up the scrollback.
	sidecarFailures []int

	// discCipher and indexCipher hold the ciphertext digests this run measured
	// while encrypting, so SHA512SUMS does not re-read twenty gigabytes per
	// disc to arrive at the same numbers. A disc finished by an earlier run is
	// simply absent here and is hashed the long way. See writeSums.
	discCipher  map[int]string
	indexCipher string

	recipients []age.Recipient
	pubkeys    []string
	identities []age.Identity

	// lock is the staging lock this run holds, or nil before preflight has
	// taken it (and for a dry run, which writes nothing).
	lock *fsx.StagingLock

	// publicIdentity is the archive's own secret key under PUBLIC_ARCHIVE, and
	// nil otherwise. It is minted in preflight and written onto every disc as
	// identity.txt; see mintPublicIdentity. Its presence is what the manifest
	// and the on-disc README key their public-archive wording off, so that a
	// set can never say it carries its key without actually carrying it.
	publicIdentity *age.X25519Identity
}

// newRunner validates the options common to Plan and Run.
func newRunner(o Options) (*runner, error) {
	if o.Cfg == nil {
		return nil, errors.New("backup: no configuration given")
	}
	p := o.UI
	if p == nil {
		p = ui.New(io.Discard, false)
	}
	b, err := o.Cfg.Budget()
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	ratio := o.Cfg.PackRatio
	if !(ratio > 0) || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		ratio = 1.0
	}
	return &runner{
		opts:       o,
		cfg:        o.Cfg,
		p:          p,
		tools:      o.Tools,
		dirs:       o.Cfg.Dirs(),
		budget:     b,
		statePath:  filepath.Join(o.Cfg.Staging, "state.json"),
		packRatio:  ratio,
		est:        newRatioEstimator(o.Cfg),
		started:    time.Now(),
		discCipher: make(map[int]string),
	}, nil
}

// Plan scans the source tree and computes the disc layout without building,
// encrypting or writing anything. It needs none of the external tools.
func Plan(ctx context.Context, o Options) (*PlanResult, error) {
	r, err := newRunner(o)
	if err != nil {
		return nil, err
	}
	res, err := r.scan(ctx)
	if err != nil {
		return nil, err
	}
	return r.layout(ctx, res)
}

// layout packs an already-scanned tree into bins, reporting each one.
func (r *runner) layout(ctx context.Context, res *scan.Result) (*PlanResult, error) {
	p := pack.New(res.Entries)
	rb := rawBudget(r.budget.Image, r.packRatio)

	// Same reason, one level up: preflight is the only caller of Validate, so
	// without this a plan is happily produced under settings backup refuses —
	// and the refusal then arrives at the start of the overnight run instead.
	// Warn rather than fail, so plan keeps working as the diagnostic it is.
	if err := r.cfg.Validate(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			r.p.Warn("%s", line)
		}
		r.p.Warn("backup will refuse this configuration; plan continues so you can see the layout")
	}

	// A plan that reports N discs should be a plan that finishes, so say up
	// front when the per-disc tool copy will not fit the reserve. It warns
	// rather than fails: plan is a dry run, and a hard error here would mask
	// the far more useful "this file is larger than one disc" and "nothing was
	// packed" diagnostics below. Preflight turns the same condition into a
	// refusal, before any image is built.
	if err := r.checkReserve(); err != nil {
		r.p.Warn("%v", err)
		r.p.Warn("backup will refuse this configuration; plan continues so you can see the layout")
	}

	r.p.Log("planning disc layout (dry run, nothing is built)")
	r.p.Step("raw content budget per disc: %s  (image budget %s / ratio %.3f)",
		ui.HumanBytes(rb), ui.HumanBytes(r.budget.Image), r.packRatio)

	out := &PlanResult{PackRatio: r.packRatio}
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("backup: planning aborted: %w", err)
		}
		if over := p.Oversized(rb); len(over) > 0 {
			return nil, oversizedError(over, rb)
		}
		bin, ok := p.Next(rb)
		if !ok {
			break
		}
		if err := p.Commit(bin); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		out.Discs++
		out.RawBytes += bin.RawBytes
		out.PerDisc = append(out.PerDisc, PlanDisc{
			Index: bin.Index, Files: len(bin.Files), RawBytes: bin.RawBytes,
		})
		r.p.Step("disc %02d: %6d files, %s raw", bin.Index, len(bin.Files), ui.HumanBytes(bin.RawBytes))
	}
	if out.Discs == 0 {
		return nil, fmt.Errorf("backup: nothing to pack — is %s empty after pruning?", r.cfg.SourceDir)
	}
	r.p.OK("%d disc(s) at pack ratio %.3f, %s of raw content",
		out.Discs, r.packRatio, ui.HumanBytes(out.RawBytes))
	// A plan is made before anything has been compressed, so this number is only
	// ever an upper bound when the content compresses — say which of the two
	// this run is, rather than promising an adaptation that is switched off.
	if r.cfg.PackRatioAdapt {
		r.p.Step("actual disc count depends on real compression; backup re-estimates the ratio " +
			"from every finished disc, so a compressible tree usually needs fewer")
	} else {
		r.p.Step("actual disc count depends on real compression; with PACK_RATIO_ADAPT=0 this " +
			"ratio is used for every disc, corrected only if one overshoots")
	}
	return out, nil
}

// scan walks the source tree and reports what it found.
func (r *runner) scan(ctx context.Context) (*scan.Result, error) {
	r.p.Log("scanning %s", r.cfg.SourceDir)
	res, err := scan.Walk(ctx, scan.Options{
		Root:          r.cfg.SourceDir,
		PruneDirs:     r.cfg.PruneDirs,
		ExcludeMasks:  r.cfg.ExcludeMasks,
		OneFileSystem: true,
	})
	if err != nil {
		return nil, fmt.Errorf("backup: scanning %s: %w", r.cfg.SourceDir, err)
	}
	if len(res.Entries) == 0 {
		return nil, fmt.Errorf("backup: scan of %s produced nothing — is it readable?", r.cfg.SourceDir)
	}
	r.p.OK("%d entries, %s of file data before compression",
		len(res.Entries), ui.HumanBytes(res.RawBytes))

	if n := len(res.Errors); n > 0 {
		r.p.Warn("%d path(s) could not be read and were skipped — they will NOT be on any disc", n)
		for i, e := range res.Errors {
			if i == 10 {
				r.p.Step("... and %d more", n-10)
				break
			}
			r.p.Step("%s", e.Error())
		}
		// Carried into the manifest as well. A tree can hold more unreadable
		// paths than a document should list, so the manifest names the first
		// few and then says how many there were: the count is the part that
		// tells a restorer to go looking, and it is never truncated away.
		r.unreadableCount = n
		for i, e := range res.Errors {
			if i == maxNamedUnreadable {
				break
			}
			r.unreadable = append(r.unreadable, e.Path)
		}
	}
	// A mount point under SOURCE_DIR is kept as an empty directory and its whole
	// subtree left out, because the scan does not descend into a mounted
	// directory (scan.Options.OneFileSystem). That is deliberate — a backup of
	// /home must not swallow the NAS mounted under it — but it used to be
	// silent, and a silent omission of an entire subtree is a data-loss report
	// waiting to happen. Say so, name the paths, and carry them into the
	// manifest.
	r.skippedMounts = res.SkippedMounts
	if n := len(res.SkippedMounts); n > 0 {
		r.p.Warn("%d mounted subtree(s) under %s are NOT included — brb does not descend into a "+
			"mounted directory; back each one up as its own SOURCE_DIR if you want it:", n, r.cfg.SourceDir)
		reportPaths(r.p, res.SkippedMounts)
	}
	// Two groups, because only one of them is handled: the index escapes tab and
	// newline, and passes every other control byte through verbatim. Saying
	// "tab or newline" about a name holding a carriage return would be a false
	// reassurance about the one byte that then makes `--only` unable to name it.
	// A name holding both lands in the raw group, which is the warning that is
	// true of it; see [scan.HasIndexEscape].
	var escaped, raw []string
	for _, p := range res.OddPaths {
		if scan.HasIndexEscape(p) {
			escaped = append(escaped, p)
		} else {
			raw = append(raw, p)
		}
	}
	if n := len(escaped); n > 0 {
		r.p.Warn("%d path(s) contain a tab or a newline; the on-disc index escapes them as "+
			`\t and \n so each still occupies exactly one row. The files themselves are `+
			"backed up normally.", n)
		reportPaths(r.p, escaped)
	}
	if n := len(raw); n > 0 {
		r.p.Warn("%d path(s) contain a control character the index does not escape (a carriage "+
			"return, most likely); the name is stored verbatim and will not display as itself, "+
			"so retrieving it with 'restore --only' means typing the control byte. The files "+
			"themselves are backed up normally.", n)
		reportPaths(r.p, raw)
	}
	return res, nil
}

// reportPaths lists odd paths under a warning, %q so a control byte is shown as
// an escape rather than acted on by the terminal, and truncated so a tree full
// of them cannot bury the rest of the run's output.
func reportPaths(p *ui.Printer, paths []string) {
	for i, s := range paths {
		if i == 20 {
			p.Step("... and %d more", len(paths)-20)
			return
		}
		p.Step("%q", s)
	}
}

// oversizedError reports units that can never fit a disc, largest first.
func oversizedError(over []pack.Unit, budget int64) error {
	return errors.New("backup: " + oversizedDetail(over, budget))
}

// oversizedDetail is oversizedError's message without the package prefix, so
// that buildImage can say what the measured ratio did to the budget first and
// still give the operator the same list and the same three ways out.
func oversizedDetail(over []pack.Unit, budget int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) are larger than one disc can hold (%s raw budget):",
		len(over), ui.HumanBytes(budget))
	for i, u := range over {
		if i == 20 {
			fmt.Fprintf(&b, "\n  ... and %d more", len(over)-20)
			break
		}
		fmt.Fprintf(&b, "\n  %14d  %s", u.Size, u.Paths[0])
	}
	b.WriteString("\n  exclude them via EXCLUDE_MASKS, use larger media (DISC_TYPE=bdxl100), " +
		"or split them yourself before backing up")
	return b.String()
}

// sortedStrings returns a sorted copy of in.
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// dirBytes sums the apparent size of every regular file beneath dir — apparent
// size, not blocks, because that is what will be written into an ISO of the
// tree. The ISO builder measures the same trees for its own space check, so the
// walk itself lives in internal/fsx and this only names the failure.
func dirBytes(ctx context.Context, dir string) (int64, error) {
	n, err := fsx.DirBytes(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	return n, nil
}
