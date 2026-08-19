package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jzbz/brb/internal/fsx"
)

// copyBufSize is the chunk size of the context-aware copy loop. It also sets
// how often a long copy checks for cancellation.
const copyBufSize = 1 << 20

// copyCtx copies src to dst in bounded chunks, checking ctx between them.
//
// It is a hand-written loop rather than io.Copy so that no ReadFrom or WriteTo
// shortcut can copy gigabytes without ever looking at the context.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w != n {
				return total, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// createPart opens dst+".part" for writing and returns the file together with a
// finish function.
//
// finish(err) with a non-nil error closes and removes the partial file and
// returns the error unchanged; finish(nil) fsyncs, closes and renames it onto
// dst. Callers use it as `defer func() { err = finish(err) }()` with a named
// return so that every failure path — including cancellation — cleans up
// identically and dst is never observed half-written.
//
// The .part is opened by [fsx.CreateFresh], which removes any leftover and
// then uses O_EXCL. This used to be O_WRONLY|O_CREATE|O_TRUNC, which opens
// straight through a symlink sitting at the path — and everything that goes
// through here is written inside staging, whose default lives under a
// world-writable /var/tmp. The reader had already stopped doing this in two
// places for exactly that reason; the writer had not.
func createPart(dst string) (*os.File, func(error) error, error) {
	if dst == "" {
		return nil, nil, errors.New("backup: no destination path given")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, nil, fmt.Errorf("backup: creating %s: %w", filepath.Dir(dst), err)
	}
	part := dst + ".part"
	f, err := fsx.CreateFresh(part, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: %w", err)
	}
	finish := func(cause error) error {
		if cause != nil {
			_ = f.Close()
			_ = os.Remove(part)
			return cause
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(part)
			return fmt.Errorf("backup: syncing %s: %w", part, err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(part)
			return fmt.Errorf("backup: closing %s: %w", part, err)
		}
		if err := os.Rename(part, dst); err != nil {
			_ = os.Remove(part)
			return fmt.Errorf("backup: installing %s: %w", dst, err)
		}
		syncDir(filepath.Dir(dst))
		return nil
	}
	return f, finish, nil
}

// copyFile copies src to dst with the given mode, atomically.
func copyFile(ctx context.Context, src, dst string, mode fs.FileMode) (err error) {
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

	if _, err := copyCtx(ctx, out, in); err != nil {
		return fmt.Errorf("backup: copying %s to %s: %w", src, dst, err)
	}
	if err := out.Chmod(mode); err != nil {
		return fmt.Errorf("backup: setting mode on %s: %w", dst, err)
	}
	return nil
}

// linkOrCopy places src at dst as a hard link, falling back to a copy when the
// two are on different filesystems (or the filesystem has no links at all).
//
// Hard linking is what keeps staging from holding a second copy of every
// encrypted image per disc directory; the copy is only there so a staging
// directory spanning filesystems still works.
func linkOrCopy(ctx context.Context, src, dst string, mode fs.FileMode) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: replacing %s: %w", dst, err)
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(ctx, src, dst, mode)
}

// filesMatching returns the names of the regular files in dir for which keep
// reports true, sorted. A missing directory yields no names and no error.
func filesMatching(dir string, keep func(name string) bool) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("backup: reading %s: %w", dir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !keep(e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	return sortedStrings(out), nil
}
