package restore

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
	"strconv"
	"strings"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
)

// copyBufSize is the chunk size of a robust copy. Large enough that the
// per-chunk context check costs nothing, small enough to abort promptly.
const copyBufSize = 1 << 20

// CopyProblem reports a copy that did not reproduce its source. It is returned
// instead of a success whenever ddrescue had to fill unreadable regions with
// zeros, or the copy did not match the hash the disc records for it.
//
// brb.sh returns success unconditionally once ddrescue has run, so a disc with
// unreadable sectors is silently ingested as if it were fine. It is not fine:
// the zeros are wrong bytes, and par2 has to repair them before the image can
// be decrypted.
type CopyProblem struct {
	// Name is the file that could not be copied faithfully.
	Name string
	// Missing is the number of bytes that could not be read, or -1 when the
	// shortfall is known to exist but not its size.
	Missing int64
	// Reason describes what went wrong, in operator-facing terms.
	Reason string
}

// Error renders the problem, including the byte count when it is known.
func (e *CopyProblem) Error() string {
	if e.Missing > 0 {
		return fmt.Sprintf("%s: %s (%d bytes are zeros, not data; par2 must repair them)", e.Name, e.Reason, e.Missing)
	}
	return fmt.Sprintf("%s: %s", e.Name, e.Reason)
}

// Unwrap reports ErrIncompleteCopy so callers can branch on the sentinel.
func (e *CopyProblem) Unwrap() error { return ErrIncompleteCopy }

// readError marks a failure that happened while reading the source, which is
// the only condition worth falling back to ddrescue for. A write failure means
// the staging filesystem is full or broken, and no amount of re-reading the
// disc will help.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

// copyStream copies src to dst through a ".part" file, returning the SHA-512 of
// what it wrote. The hash costs nothing extra because it is computed in the same
// pass, which lets the caller check the copy against the hash the disc records
// without reading either file again.
//
// A failure to read the source is reported as a *readError. Any partial output
// is removed, so dst never exists half-written.
func copyStream(ctx context.Context, src, dst string, prog io.Writer) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", &readError{fmt.Errorf("opening %s: %w", src, err)}
	}
	defer in.Close()

	part := dst + partExt
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", part, err)
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(part)
		}
	}()

	h := sha512.New()
	sinks := []io.Writer{out, h}
	if prog != nil {
		sinks = append(sinks, prog)
	}
	w := io.MultiWriter(sinks...)

	buf := make([]byte, copyBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return "", fmt.Errorf("writing %s: %w", part, werr)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", &readError{fmt.Errorf("reading %s: %w", src, rerr)}
		}
	}
	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("syncing %s: %w", part, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", part, err)
	}
	if err := os.Rename(part, dst); err != nil {
		return "", fmt.Errorf("renaming %s: %w", part, err)
	}
	ok = true
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyRobustly copies one file off a disc, falling back to ddrescue when the
// drive reports a read error. It returns the SHA-512 of the copy.
//
// When ddrescue could not read everything, the salvaged file is kept — par2
// needs it — but a *CopyProblem is returned so no caller can mistake the result
// for a faithful copy.
func (o Options) copyRobustly(ctx context.Context, src, dst string) (string, error) {
	name := filepath.Base(dst)
	var total int64
	if st, err := os.Stat(src); err == nil {
		total = st.Size()
	}

	prog := o.UI.NewProgress("copying "+name, total)
	sum, err := copyStream(ctx, src, dst, prog.Writer())
	prog.Done()
	if err == nil {
		return sum, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", err
	}
	var re *readError
	if !errors.As(err, &re) {
		return "", fmt.Errorf("restore: copying %s: %w", name, err)
	}

	o.UI.Warn("read error on %s: %v", name, re.err)
	dd := o.Tools.Get(tools.Ddrescue)
	if !dd.Found {
		o.UI.Warn("install gddrescue (ddrescue) to salvage partially readable discs")
		return "", fmt.Errorf("restore: copying %s: %w", name, err)
	}
	o.UI.Warn("falling back to ddrescue: unreadable areas become ZEROS, not data — par2 must repair them before the image can be decrypted")

	missing, err := o.ddrescueCopy(ctx, dd.Path, src, dst)
	if err != nil {
		return "", err
	}
	if missing != 0 {
		return "", &CopyProblem{Name: name, Missing: missing, Reason: "ddrescue could not read the whole file"}
	}
	o.UI.OK("ddrescue recovered all of %s", name)
	sum, err = agecrypt.SumFile(ctx, dst)
	if err != nil {
		return "", fmt.Errorf("restore: hashing the salvaged %s: %w", name, err)
	}
	return sum, nil
}

// ddrescueCopy runs the two ddrescue passes brb.sh uses — a fast pass that
// skips over damage, then a scraping pass that retries it — and then decides
// for itself whether the result is complete. ddrescue's own exit status is only
// logged: it exits non-zero in cases where it still salvaged nearly everything,
// and the map file plus the file size are the honest answer.
//
// It returns the number of bytes that could not be read.
func (o Options) ddrescueCopy(ctx context.Context, bin, src, dst string) (int64, error) {
	part := dst + partExt
	mapfile := dst + mapfileExt
	// A stale map file describes a file that no longer exists; resuming against
	// it would leave regions unread and marked done.
	for _, p := range []string{part, mapfile} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("restore: removing %s: %w", p, err)
		}
	}
	return o.ddrescueInto(ctx, bin, src, part, mapfile, dst)
}

// ddrescueResume continues an earlier salvage: the partially copied file and
// its map file are kept, so only the regions ddrescue never managed to read are
// tried again.
func (o Options) ddrescueResume(ctx context.Context, bin, src, dst string) (int64, error) {
	return o.ddrescueInto(ctx, bin, src, dst, dst+mapfileExt, dst)
}

// ddrescueInto runs both passes writing into out, then assesses the result and
// puts it in place at dst. out and dst may be the same file, which is what a
// resume does.
func (o Options) ddrescueInto(ctx context.Context, bin, src, out, mapfile, dst string) (int64, error) {
	log := o.logWriter()
	defer log.Close()

	for _, pass := range [][]string{
		{"-n", "--", src, out, mapfile},
		{"-d", "-r3", "--", src, out, mapfile},
	} {
		text, err := runTool(ctx, bin, pass...)
		if s := firstLine(text); s != "" {
			o.UI.Step("ddrescue: %s", s)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, fmt.Errorf("restore: ddrescue aborted: %w", ctxErr)
			}
			o.UI.Warn("ddrescue exited with an error (the map file decides what was salvaged): %v", err)
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}

	missing, err := assessSalvage(src, out, mapfile)
	if err != nil {
		return 0, err
	}
	if out != dst {
		if err := os.Rename(out, dst); err != nil {
			return 0, fmt.Errorf("restore: renaming %s: %w", out, err)
		}
	}
	if missing == 0 {
		if err := os.Remove(mapfile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			o.UI.Warn("could not remove %s: %v", mapfile, err)
		}
	} else {
		o.UI.Warn("%s of %s could not be read; the map file is kept at %s so another copy of the disc can fill the gaps",
			plural(missing, "byte"), filepath.Base(dst), mapfile)
	}
	return missing, nil
}

// plural renders a count with its unit, pluralised.
func plural(n int64, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// assessSalvage decides how many bytes of src are missing from out, using both
// the file sizes and ddrescue's map file and taking the worse of the two. A
// short file and a map file full of unread regions are independent symptoms and
// either one on its own means the copy is incomplete.
func assessSalvage(src, out, mapfile string) (int64, error) {
	si, err := os.Stat(src)
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	oi, err := os.Stat(out)
	if err != nil {
		return 0, fmt.Errorf("restore: ddrescue produced no output: %w", err)
	}
	missing := si.Size() - oi.Size()
	if missing < 0 {
		missing = 0
	}
	bad, ok, err := mapfileMissing(mapfile)
	if err != nil {
		return 0, err
	}
	if ok && bad > missing {
		missing = bad
	}
	return missing, nil
}

// mapfileMissing sums the regions a ddrescue map file reports as anything other
// than finished. ok is false when there is no map file at all.
//
// The format is a header of "#" comments, a status line of two or three fields
// where the second is a status character, and then data lines of
// "<pos> <size> <status>" with numbers in hex. Only '+' means the region was
// read successfully; '-', '?', '*' and '/' are all unread to some degree.
func mapfileMissing(path string) (bytes int64, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("restore: reading %s: %w", path, err)
	}
	seen := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		size, err := strconv.ParseInt(f[1], 0, 64)
		if err != nil {
			// The "current position / current status" line, whose second field
			// is a status character rather than a number.
			continue
		}
		if _, err := strconv.ParseInt(f[0], 0, 64); err != nil {
			continue
		}
		if len(f[2]) != 1 {
			continue
		}
		seen = true
		if f[2] != "+" && size > 0 {
			bytes += size
		}
	}
	return bytes, seen, nil
}
