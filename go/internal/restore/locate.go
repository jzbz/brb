package restore

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/indexfmt"
)

// errNoIndex marks a staging area that has no encrypted index in it, so that
// the caller can fall back to searching every image rather than refusing.
var errNoIndex = errors.New("restore: no encrypted index")

// indexDiscs asks the encrypted index which disc(s) hold each of the requested
// archive paths. The result is keyed by the path exactly as it was requested,
// and each value is ascending and free of duplicates. A path the index says
// nothing about is absent from the map rather than present and empty.
//
// Matching is done on the parsed, unescaped path — not on the raw line — for
// the reason [indexfmt] exists: a filename may legitimately contain a tab or a
// backslash, and the index stores those escaped. Comparing against the raw
// bytes would miss exactly the files whose names made escaping necessary, and
// would also match a path against the disc-number field.
//
// It is deliberately stricter than brb.sh, which greps the index
// case-insensitively for a substring: here a request matches an entry only when
// it names it exactly or names a directory containing it, which is the same
// question [Options.pathsPresent] asks the image itself. Anything looser would
// have the index and the image disagree about what --only means, and would send
// a restore off to decrypt a disc because some other file's name contained the
// requested one.
func (o Options) indexDiscs(ctx context.Context, want []string) (map[string][]int, error) {
	if len(want) == 0 {
		return nil, nil
	}
	hits := make(map[string]map[int]bool, len(want))
	bad, err := o.scanIndex(ctx, func(disc int, p string) {
		for _, w := range want {
			if covers(w, p) {
				if hits[w] == nil {
					hits[w] = map[int]bool{}
				}
				hits[w][disc] = true
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if bad > 0 {
		o.UI.Warn("%d record(s) in %s do not parse and were ignored; %s if a path seems to be missing",
			bad, indexName, o.sidecarRepairHint(0))
	}

	out := make(map[string][]int, len(hits))
	for w, set := range hits {
		out[w] = sortedDiscs(set)
	}
	return out, nil
}

// scanIndex decrypts the staged index and hands every record it holds to fn as
// a disc number and an unescaped path. It reports how many records did not
// parse, so each caller can decide what an incomplete answer is worth to it.
//
// One reader, because the read side now asks the index two questions — "which
// disc(s) hold this path", for --only, and "exactly which paths does this disc
// hold", for the per-image cross-check in [Options.auditImage] — and a second
// walk written for the second question would be a second set of decisions
// about unescaping, about a bare CR in a name, and about a record that will not
// parse. The two answers would then drift apart precisely where a forged disc
// wants them to: the guard compares an image against the index, and it is worth
// nothing if its idea of what the index says differs from the one the rest of
// the restore uses.
//
// Errors keep the shape [Options.indexDiscs] established: [errNoIndex] wrapped
// when the staging area simply has no index, so callers can fall back rather
// than refuse, and anything else means the index is there and would not read.
func (o Options) scanIndex(ctx context.Context, fn func(disc int, path string)) (bad int, err error) {
	encDir := o.dirs().Enc
	idx := filepath.Join(encDir, indexName)
	if _, err := os.Stat(idx); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%w in %s", errNoIndex, encDir)
		}
		return 0, fmt.Errorf("restore: %s: %w", idx, err)
	}
	// The identities are loaded once per command and shared, so reading the
	// index here does not cost a second passphrase prompt before the images are
	// decrypted. See Options.ids.
	ids, err := o.identities()
	if err != nil {
		return 0, err
	}
	if err := o.checkIndexIntact(ctx, idx); err != nil {
		return 0, err
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(agecrypt.DecryptTo(ctx, idx, pw, ids))
	}()
	defer pr.Close()

	hint := o.sidecarRepairHint(0)
	gz, err := gzip.NewReader(pr)
	if err != nil {
		return 0, fmt.Errorf("restore: reading %s (%s): %w", idx, hint, err)
	}
	defer gz.Close()

	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64<<10), maxIndexLine)
	// The default ScanLines drops a trailing '\r', and the index stores a CR in
	// a filename raw (escaping is for the bytes that could break the row —
	// backslash, tab, newline — and CR cannot). Splitting must not eat name
	// bytes, or the file becomes unfindable by its own real name.
	sc.Split(scanLinesKeepCR)
	for i := 0; sc.Scan(); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return bad, err
			}
		}
		line := sc.Text()
		if line == "" {
			continue
		}
		disc, p, err := indexfmt.ParseLine(line)
		if err != nil {
			// Escaping means no filename can produce an unparseable record, so
			// this is corruption the hash check above did not catch. It is
			// counted and reported rather than fatal: the other million records
			// are still an answer, and the caller can always fall back to
			// searching every disc.
			bad++
			continue
		}
		fn(disc, p)
	}
	if err := sc.Err(); err != nil {
		return bad, fmt.Errorf("restore: reading %s (%s): %w", idx, hint, err)
	}
	return bad, nil
}

// discRows is what the index says one disc holds: every regular-file path it
// puts on that disc, and — when the comparison in [Options.auditImage] cannot
// be made exact — the reason why.
//
// The writer indexes regular files only. Directories, symlinks and specials are
// the skeleton, which is replicated onto every disc of the set on purpose (see
// pack.Bin.Skeleton), so they are in no index and are not comparable this way.
// That is why the cross-check looks at the image's files and nothing else: a
// guard that compared directories would refuse every legitimate restore of
// every set brb has ever written.
type discRows struct {
	// paths holds the archive paths the index puts on this disc.
	paths map[string]bool
	// inexact, when non-empty, says in operator-facing terms why this disc's
	// image must not be judged against paths. It is a degradation, never a
	// refusal: see [Options.indexRowsForDisc].
	inexact string
}

// indexRowsForDisc reads the index and collects what it says disc n holds.
//
// The index is read once per disc rather than once per restore. A full-set
// restore of a $HOME-sized archive has a million records in it, and holding
// every disc's rows at once to save a few seconds of gunzip would cost hundreds
// of megabytes on the machine least able to spare it — while the pass being
// saved is nothing beside par2-verifying and decrypting the 25 GB image the
// rows are about to be compared against.
//
// Two conditions come back as [discRows.inexact] rather than as an error,
// because a backup tool that cannot restore a legitimate set is worse than one
// that misses an exotic attack:
//
//   - A path on this disc whose name contains a newline. brb deliberately
//     supports those — it is why the index has an escaping contract at all —
//     but "unsquashfs -ll" is line-based, so such a file arrives as two lines
//     of which only the first fragment parses, and the comparison would report
//     a file the image does not hold and one it does not list. See listedDir.
//   - Records that do not parse. The rows read off a damaged index are not the
//     whole of what the disc holds, so every unread row would look like a file
//     missing from the image.
func (o Options) indexRowsForDisc(ctx context.Context, n int) (*discRows, error) {
	rows := &discRows{paths: map[string]bool{}}
	newlines := 0
	bad, err := o.scanIndex(ctx, func(disc int, p string) {
		if disc != n {
			return
		}
		if strings.Contains(p, "\n") {
			newlines++
		}
		rows.paths[p] = true
	})
	if err != nil {
		return nil, err
	}
	switch {
	case newlines > 0:
		rows.inexact = fmt.Sprintf("the index puts %d path(s) whose name contains a newline on this disc, "+
			"and 'unsquashfs -ll' is line-based, so the image's file list cannot be read back exactly", newlines)
	case bad > 0:
		rows.inexact = fmt.Sprintf("%d record(s) in %s do not parse, so the index's list for this disc is incomplete (%s)",
			bad, indexName, o.sidecarRepairHint(n))
	}
	return rows, nil
}

// sortedDiscs renders a disc-number set as an ascending slice.
func sortedDiscs(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

// discList renders disc numbers the way brb.sh prints them: ascending, space
// separated.
func discList(ds []int) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, " ")
}

// discNumbers lists the disc numbers of a selection of images, in the order
// they will be worked through.
func discNumbers(imgs []discFile) []int {
	out := make([]int, len(imgs))
	for i, im := range imgs {
		out[i] = im.N
	}
	return out
}

// containsDisc reports whether ds names disc n.
func containsDisc(ds []int, n int) bool {
	for _, d := range ds {
		if d == n {
			return true
		}
	}
	return false
}

// narrowByIndex is HL-3: it resolves the --only paths through the encrypted
// index and returns just the images that actually hold them.
//
// Without it --only par2-verifies and decrypts every image in the set to
// retrieve one file — on a 50-disc BD25 set roughly 1.2 TB of hashing and
// decryption, and every disc's plaintext written into the staging restore
// directory on the way, which is precisely the exposure --only exists to
// avoid. brb.sh carries the same fix, with a comment recording why.
//
// narrowed reports whether the returned selection is smaller than the ingested
// set because of the index, so the caller can say what was actually searched if
// the path turns out not to be there.
//
// Two things it deliberately does not do:
//
//   - It never fails because the index is absent or unreadable. An old set, or
//     a single disc ingested on its own, still restores the way it did before;
//     the fallback is announced, because a --only that quietly decrypts fifty
//     discs is a surprise worth one warning.
//   - It never adds a disc the operator did not ask for. With --disc N the
//     selection stays disc N; the index only contributes what it knows about
//     that choice, up to refusing early when it is certain the path is
//     somewhere else.
func (o Options) narrowByIndex(ctx context.Context, imgs []discFile, ro RestoreOptions) (sel []discFile, narrowed bool, err error) {
	encDir := o.dirs().Enc
	hits, err := o.indexDiscs(ctx, ro.Only)
	if err != nil {
		if errors.Is(err, errNoIndex) {
			o.UI.Warn("no %s in %s, so there is nothing to say which disc(s) hold %s",
				indexName, encDir, quoteAll(ro.Only))
		} else {
			o.UI.Warn("the encrypted index could not be read: %v", err)
		}
		if ro.Disc == 0 {
			o.UI.Warn("every one of the %d ingested image(s) will be decrypted to look for it; "+
				"pass --disc N to restrict that to one disc", len(imgs))
		}
		return imgs, false, nil
	}

	if ro.Disc > 0 {
		// --only and --disc are both honoured. The index cannot widen the
		// operator's choice of disc, but it can say the path is not on it,
		// which saves decrypting a whole image to discover the same thing.
		anyKnown, anyHere := false, false
		var elsewhere []int
		for _, p := range ro.Only {
			ds := hits[p]
			switch {
			case len(ds) == 0:
				o.UI.Warn("the index does not list %s; searching disc %d anyway, because --disc names it",
					quoteAll([]string{p}), ro.Disc)
			case containsDisc(ds, ro.Disc):
				anyKnown, anyHere = true, true
				o.UI.Step("the index puts %s on disc(s) %s", quoteAll([]string{p}), discList(ds))
			default:
				anyKnown = true
				elsewhere = append(elsewhere, ds...)
				o.UI.Warn("the index puts %s on disc(s) %s, not on disc %d",
					quoteAll([]string{p}), discList(ds), ro.Disc)
			}
		}
		if anyKnown && !anyHere {
			ds := sortedDiscs(discSet(elsewhere))
			return nil, false, fmt.Errorf("restore: the index puts %s on disc(s) %s, not on disc %d — "+
				"drop --disc, or pass --disc %d", quoteAll(ro.Only), discList(ds), ro.Disc, ds[0])
		}
		return imgs, false, nil
	}

	var absent []string
	union := map[int]bool{}
	for _, p := range ro.Only {
		ds := hits[p]
		if len(ds) == 0 {
			absent = append(absent, p)
			continue
		}
		// A path may legitimately be on several discs — a directory the packer
		// split, or a name that is both — so every disc it names is collected,
		// never just the first.
		o.UI.Step("the index puts %s on disc(s) %s", quoteAll([]string{p}), discList(ds))
		for _, d := range ds {
			union[d] = true
		}
	}
	if len(absent) > 0 {
		return nil, false, fmt.Errorf("restore: %s is not in the index — check 'brb index %s', or pass "+
			"--disc N explicitly. Paths are relative to the archive root, with no leading '/'",
			quoteAll(absent), absent[0])
	}

	have := make(map[int]discFile, len(imgs))
	for _, im := range imgs {
		have[im.N] = im
	}
	for _, d := range sortedDiscs(union) {
		im, ok := have[d]
		if !ok {
			// Not fatal on its own: the rest of the request may still be
			// recoverable from the discs that are here. Name the disc, because
			// "ingest disc 7" is the whole of the remedy.
			o.UI.Warn("%s is partly on disc %d, whose image is not in %s",
				quoteAll(pathsOnDisc(ro.Only, hits, d)), d, encDir)
			continue
		}
		sel = append(sel, im)
	}
	if len(sel) == 0 {
		return nil, false, fmt.Errorf("restore: %s is on disc(s) %s, none of which have been ingested — "+
			"ingest one of them and retry", quoteAll(ro.Only), discList(sortedDiscs(union)))
	}
	o.UI.Step("the index narrows this restore to %d of %d ingested image(s)", len(sel), len(imgs))
	return sel, len(sel) < len(imgs), nil
}

// discSet collects disc numbers into a set.
func discSet(ds []int) map[int]bool {
	set := make(map[int]bool, len(ds))
	for _, d := range ds {
		set[d] = true
	}
	return set
}

// pathsOnDisc returns those of want that the index puts on disc d, in the order
// they were requested.
func pathsOnDisc(want []string, hits map[string][]int, d int) []string {
	var out []string
	for _, p := range want {
		if containsDisc(hits[p], d) {
			out = append(out, p)
		}
	}
	return out
}
