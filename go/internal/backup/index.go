package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/indexfmt"
)

// indexFileName is the plaintext index accumulated in the work directory while
// a run is in progress. It is kept until the whole run succeeds so that a
// resumed run can carry on appending to it.
const indexFileName = "index.tsv"

// writeIndexLines serialises one disc's file list in the on-disc index format:
// the disc number, a tab, the escaped path relative to the source directory,
// and a newline. This one IS frozen — brb.sh parses these lines to answer
// "which disc holds this file", and xcompat-test.sh holds the two readers to
// the same answers — so it changes only when the on-disc format does.
//
// The path is escaped by [indexfmt.EscapePath], so a tab or a newline in a
// filename can neither add a field nor split one file over two rows. Writing
// such a path raw is what let a filename forge an index row naming a disc that
// was never burned, and cost the real name of every file it happened to.
func writeIndexLines(w io.Writer, discNum int, paths []string) error {
	bw := bufio.NewWriterSize(w, 128<<10)
	for _, p := range paths {
		if _, err := bw.WriteString(indexfmt.FormatLine(discNum, p)); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// appendIndex appends one disc's file list to the index file and flushes it to
// disk, so that an interrupted run resumes with an index that already covers
// every disc it completed.
//
// It is the one file in staging that accumulates rather than being written
// once, so [fsx.CreateFresh]'s remove-then-O_EXCL — used everywhere else here
// — would throw away every earlier disc's lines. [fsx.OpenAppend] gives it the
// same refusal to open through a planted symlink without discarding what is
// already in the file.
func appendIndex(path string, discNum int, paths []string) (err error) {
	f, err := fsx.OpenAppend(path, 0o600)
	if err != nil {
		return fmt.Errorf("backup: index: %w", err)
	}
	defer func() {
		cerr := f.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("backup: closing index %s: %w", path, cerr)
		}
	}()
	if err := writeIndexLines(f, discNum, paths); err != nil {
		return fmt.Errorf("backup: writing index %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("backup: syncing index %s: %w", path, err)
	}
	return nil
}

// indexSummary is what an existing index file records.
type indexSummary struct {
	// Lines is the number of records in the file.
	Lines int
	// MaxDisc is the highest disc number mentioned, or 0 when empty.
	MaxDisc int
	// Truncated records that the file ends without a newline, which means the
	// last record was only half written when the run was interrupted.
	Truncated bool
}

// readIndexSummary scans an existing index file. It is used on resume to prove
// the index on disk agrees with the state file before more lines are appended
// to it.
//
// A final line with no newline is the tail of a record the interrupted run was
// still writing: it is reported through [indexSummary.Truncated] and left
// uncounted, not treated as corruption. Any complete line that does not parse
// is corruption — with escaping in place no filename can produce one — and is
// returned as an error so the caller can refuse to touch the file.
func readIndexSummary(ctx context.Context, path string) (indexSummary, error) {
	var sum indexSummary
	f, err := os.Open(path)
	if err != nil {
		return sum, fmt.Errorf("backup: opening index %s: %w", path, err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return sum, fmt.Errorf("backup: reading index %s: %w", path, err)
		}
		line, err := br.ReadString('\n')
		switch {
		case line == "":
			// nothing to account for
		case !strings.HasSuffix(line, "\n"):
			sum.Truncated = true
		default:
			n, _, perr := indexfmt.ParseLine(strings.TrimSuffix(line, "\n"))
			if perr != nil {
				return sum, fmt.Errorf("backup: index %s: %w", path, perr)
			}
			sum.Lines++
			if n > sum.MaxDisc {
				sum.MaxDisc = n
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sum, nil
			}
			return sum, fmt.Errorf("backup: reading index %s: %w", path, err)
		}
	}
}

// reconcileIndex makes the index on disk agree with a state file that records
// maxDisc completed discs.
//
// A run is interrupted between appending a disc's index records and saving the
// state that says the disc is done, so an index one disc ahead of the state is
// an ordinary, recoverable condition rather than a corrupt staging directory:
// the records for discs beyond maxDisc — and any half-written trailing line —
// are dropped, and the disc is simply built again. The common case reads the
// file once and rewrites nothing.
//
// A record that does not parse is the one thing that is never dropped. Every
// path is escaped on the way in, so no filename can produce an unparseable
// line; one here means the file itself has been damaged, and the index is the
// map of which disc holds which file. Rewriting it without those rows would
// destroy the only record that the files they name exist at all, so this fails
// and leaves the file untouched for the operator to look at.
func reconcileIndex(ctx context.Context, path string, maxDisc int, warn func(string, ...any)) (indexSummary, error) {
	sum, err := readIndexSummary(ctx, path)
	if err == nil && sum.MaxDisc <= maxDisc && !sum.Truncated {
		return sum, nil
	}
	switch {
	case err != nil:
		return indexSummary{}, fmt.Errorf("the index is damaged and has NOT been altered "+
			"(it is the map of which disc holds which file, so no record is ever dropped from it); "+
			"salvage it by hand or start the set over: %w", err)
	case sum.Truncated:
		warn("the index %s ends mid-record; dropping the half-written tail", path)
	default:
		warn("the index %s holds records for disc %d but only %d disc(s) completed; dropping them",
			path, sum.MaxDisc, maxDisc)
	}
	return rewriteIndex(ctx, path, maxDisc)
}

// rewriteIndex copies the index, keeping the records for discs up to maxDisc,
// and atomically replaces the original with the result. Records for later discs
// are dropped because those discs are about to be built again; a half-written
// trailing line is dropped for the same reason.
//
// It refuses to run over a record it cannot parse. reconcileIndex has already
// rejected such a file, so reaching one here would mean the index changed under
// us — and dropping it would delete the only note of a file's existence.
func rewriteIndex(ctx context.Context, path string, maxDisc int) (sum indexSummary, err error) {
	in, err := os.Open(path)
	if err != nil {
		return sum, fmt.Errorf("backup: opening index %s: %w", path, err)
	}
	defer in.Close()

	out, finish, err := createPart(path)
	if err != nil {
		return sum, err
	}
	defer func() { err = finish(err) }()

	br := bufio.NewReaderSize(in, 128<<10)
	bw := bufio.NewWriterSize(out, 128<<10)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return indexSummary{}, fmt.Errorf("backup: rewriting index %s: %w", path, cerr)
		}
		line, rerr := br.ReadString('\n')
		// Only the last line can lack its newline, and that is the tail of the
		// record the interrupted run was mid-way through writing.
		if complete := strings.HasSuffix(line, "\n"); complete {
			text := strings.TrimSuffix(line, "\n")
			n, _, perr := indexfmt.ParseLine(text)
			if perr != nil {
				return indexSummary{}, fmt.Errorf("backup: rewriting index %s, "+
					"which has NOT been altered: %w", path, perr)
			}
			if n <= maxDisc {
				if _, werr := bw.WriteString(text + "\n"); werr != nil {
					return indexSummary{}, fmt.Errorf("backup: rewriting index %s: %w", path, werr)
				}
				sum.Lines++
				if n > sum.MaxDisc {
					sum.MaxDisc = n
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return indexSummary{}, fmt.Errorf("backup: reading index %s: %w", path, rerr)
		}
	}
	if ferr := bw.Flush(); ferr != nil {
		return indexSummary{}, fmt.Errorf("backup: rewriting index %s: %w", path, ferr)
	}
	return sum, nil
}

// gzipFile compresses src to dst at maximum compression. The output is written
// to dst+".part" and renamed, so dst never exists half-written.
//
// That it is gzip at all is part of the on-disc format: brb.sh reads the index
// with `gunzip -c`. The LEVEL is not — a decompressor cannot tell — so -9 is
// only a choice about a file of a few hundred kilobytes.
//
// gzip rather than zstd, and deliberately so. The disc images use zstd because
// the KERNEL decompresses them on mount, so it costs no userspace dependency.
// The index is the opposite case: it is what a person reads when a disc has been
// lost, possibly from a rescue USB, and gunzip has been on every Unix since 1992
// where zstd dates from 2016. On a text file this small the size difference is a
// rounding error, and it is not a trade worth making in the one code path that
// runs after something has already gone wrong.
func gzipFile(ctx context.Context, src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("backup: opening %s: %w", src, err)
	}
	defer in.Close()

	out, finish, err := createPart(dst)
	if err != nil {
		return err
	}
	defer func() { err = finish(err) }()

	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("backup: gzip %s: %w", src, err)
	}
	if _, err := fsx.CopyCtx(ctx, zw, in); err != nil {
		return fmt.Errorf("backup: compressing %s: %w", src, err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("backup: finishing gzip of %s: %w", src, err)
	}
	return nil
}
