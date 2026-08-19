package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// BuildImage runs mksquashfs over MkOptions.Files, producing MkOptions.Out.
//
// The file list is written to the child's stdin as NUL-delimited paths from a
// goroutine that runs concurrently with the build, so an arbitrarily long list
// cannot deadlock against a full pipe buffer.
//
// It returns only an error. mksquashfs's stdout goes to MkOptions.Log and
// nowhere else: brb.sh's img="$(build_one_image ...)" captured the tool's
// chatter into what it then treated as a path, and that must never happen here.
// A partial image is removed when the build fails or the context is cancelled.
//
// The exit status is not the whole verdict. mksquashfs exits 0 on a source
// file it cannot open — it prints "Failed to read file X, creating empty file"
// and writes a zero-byte X into the image — so a build that "succeeded" can be
// missing data with nothing downstream able to tell: every hash is taken over
// the image as written. The scan refuses unreadable files before they get
// here; this watches the tool's own output for that message, so a file that
// became unreadable between the scan and the build fails the build instead of
// being backed up as nothing.
func (s *Set) BuildImage(ctx context.Context, o MkOptions) error {
	path, err := s.bin(Mksquashfs)
	if err != nil {
		return err
	}
	if o.SourceDir == "" {
		return errors.New("mksquashfs: no source directory given")
	}
	if o.Out == "" {
		return errors.New("mksquashfs: no output path given")
	}
	if len(o.Files) == 0 {
		return errors.New("mksquashfs: empty file list")
	}
	for i, f := range o.Files {
		if f == "" {
			return fmt.Errorf("mksquashfs: file list entry %d is empty", i)
		}
		if strings.ContainsRune(f, 0) {
			return fmt.Errorf("mksquashfs: file list entry %d contains NUL: %q", i, f)
		}
	}
	if err := os.Remove(o.Out); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mksquashfs: removing stale image %s: %w", o.Out, err)
	}

	done := false
	defer func() {
		if !done {
			_ = os.Remove(o.Out)
		}
	}()

	files := o.Files
	watch := &readFailureWatch{out: o.Log}
	err = run(ctx, runSpec{
		name: Mksquashfs,
		path: path,
		args: MksquashfsArgs(o),
		dir:  o.SourceDir,
		log:  watch,
		stdin: func(w io.Writer) error {
			bw := bufio.NewWriterSize(w, 128<<10)
			for _, f := range files {
				if _, err := bw.WriteString(f); err != nil {
					return err
				}
				if err := bw.WriteByte(0); err != nil {
					return err
				}
			}
			return bw.Flush()
		},
	})
	if err != nil {
		return err
	}
	watch.flush()
	if failed := watch.failed(); len(failed) > 0 {
		return readFailureError(failed)
	}

	st, err := os.Stat(o.Out)
	if err != nil {
		return fmt.Errorf("mksquashfs: image %s: %w", o.Out, err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("mksquashfs produced an empty image: %s", o.Out)
	}
	done = true
	return nil
}

// readFailurePrefix and readFailureSuffix frame the message mksquashfs prints
// when it cannot read a source file and substitutes an empty one — verbatim
// from mksquashfs.c, where it has read "Failed to read file %s, creating empty
// file" for as long as -cpiostyle0 has existed. The file name sits between the
// two, unquoted, so a name holding a comma is still recovered whole: the
// suffix is matched at the end of the line, not at the first comma.
const (
	readFailurePrefix = "Failed to read file "
	readFailureSuffix = ", creating empty file"
)

// MksquashfsReadFailure recognises the line mksquashfs prints when it has
// silently replaced a source file with an empty one, and returns the file's
// name as the tool printed it. It is exported so the recogniser is testable
// against a captured line without running mksquashfs.
//
// Two shapes are accepted. The complete message yields the name between its
// two fixed halves. A line that merely begins with the prefix, or ends with
// the suffix, is also a failure — a future mksquashfs may reword one half, and
// a build that continued past an unreadable file is a build with a hole in it
// whichever way it was phrased — but the name is then the best-effort remainder
// of the line rather than an exact extraction.
func MksquashfsReadFailure(line string) (file string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	hasPrefix := strings.HasPrefix(line, readFailurePrefix)
	hasSuffix := strings.HasSuffix(line, readFailureSuffix)
	switch {
	case hasPrefix && hasSuffix && len(line) >= len(readFailurePrefix)+len(readFailureSuffix):
		return line[len(readFailurePrefix) : len(line)-len(readFailureSuffix)], true
	case hasPrefix:
		return strings.TrimSpace(strings.TrimPrefix(line, readFailurePrefix)), true
	case hasSuffix:
		return strings.TrimSpace(strings.TrimSuffix(line, readFailureSuffix)), true
	}
	return "", false
}

// readFailureWatch sits between run's line splitter and the caller's log:
// every line is forwarded to out untouched, and the ones that announce a
// silently-emptied file are collected. It does its own line splitting rather
// than trusting each Write to be one line, so it stays correct if the writer
// upstream ever changes how it batches.
type readFailureWatch struct {
	out   io.Writer
	part  []byte
	files []string
}

// Write implements io.Writer. It never reports an error: losing a log line
// must not fail a build — but a line it did see and recognise is never lost,
// which is the one property BuildImage relies on.
func (w *readFailureWatch) Write(p []byte) (int, error) {
	if w.out != nil {
		_, _ = w.out.Write(p)
	}
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			break
		}
		w.check(string(w.part[:i]))
		w.part = w.part[i+1:]
	}
	return len(p), nil
}

// flush examines a trailing line that never got its newline.
func (w *readFailureWatch) flush() {
	if len(w.part) > 0 {
		w.check(string(w.part))
		w.part = w.part[:0]
	}
}

func (w *readFailureWatch) check(line string) {
	if f, ok := MksquashfsReadFailure(line); ok {
		w.files = append(w.files, f)
	}
}

// failed returns the files mksquashfs reported it could not read.
func (w *readFailureWatch) failed() []string { return w.files }

// readFailureError names every file mksquashfs emptied and says what to do:
// the scan already refuses unreadable files, so reaching here means a file
// changed between the scan and the build, and the honest fix is to make it
// readable or exclude it and run again — never to keep an image with holes.
func readFailureError(files []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "mksquashfs could not read %d source file(s) and wrote EMPTY files in their place; "+
		"the image was discarded because it would have been missing data while every hash still passed:",
		len(files))
	for i, f := range files {
		if i == 20 {
			fmt.Fprintf(&b, "\n  ... and %d more", len(files)-20)
			break
		}
		fmt.Fprintf(&b, "\n  %s", f)
	}
	b.WriteString("\n  make them readable (or exclude them via EXCLUDE_MASKS / PRUNE_DIRS) and re-run; " +
		"the scan checks readability up front, so these became unreadable after it ran")
	return errors.New(b.String())
}

// ImageStats returns the superblock summary printed by "unsquashfs -s". It is
// used to prove an image is a readable squashfs before the plaintext copy is
// deleted. This is the one place a tool's stdout is deliberately the return
// value, and it is a read-only query with no path in it.
func (s *Set) ImageStats(ctx context.Context, image string) (string, error) {
	path, err := s.bin(Unsquashfs)
	if err != nil {
		return "", err
	}
	if image == "" {
		return "", errors.New("unsquashfs: no image given")
	}
	var buf bytes.Buffer
	if err := run(ctx, runSpec{
		name:   Unsquashfs,
		path:   path,
		args:   []string{"-s", image},
		stdout: &buf,
	}); err != nil {
		return "", err
	}
	out := strings.TrimRight(buf.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("unsquashfs -s said nothing about %s", image)
	}
	return out, nil
}

// Unsquashfs extracts an image, or the subset named by UnsqOptions.Only, into
// UnsqOptions.Dest.
func (s *Set) Unsquashfs(ctx context.Context, o UnsqOptions) error {
	path, err := s.bin(Unsquashfs)
	if err != nil {
		return err
	}
	if o.Image == "" {
		return errors.New("unsquashfs: no image given")
	}
	if o.Dest == "" {
		return errors.New("unsquashfs: no destination given")
	}
	for i, p := range o.Only {
		if p == "" {
			return fmt.Errorf("unsquashfs: extraction path %d is empty", i)
		}
	}
	return run(ctx, runSpec{
		name: Unsquashfs,
		path: path,
		args: UnsquashfsArgs(o),
		log:  o.Log,
	})
}

// UnsquashfsList writes the long listing of an image ("unsquashfs -ll") to w.
func (s *Set) UnsquashfsList(ctx context.Context, image string, w io.Writer) error {
	return s.listing(ctx, "-ll", image, w)
}

// UnsquashfsNames writes the bare path listing of an image ("unsquashfs -l") to
// w, one entry per line, each rooted at "squashfs-root/".
//
// This is how a caller finds out whether an image holds a particular path
// before extracting it. unsquashfs exits 0 having created nothing when the
// requested path is not in the image, so without asking first there is no
// signal at all about the one file the operator came for.
func (s *Set) UnsquashfsNames(ctx context.Context, image string, w io.Writer) error {
	return s.listing(ctx, "-l", image, w)
}

// listing runs one of unsquashfs's read-only listing modes. The output is the
// point of the call, so it goes to w verbatim rather than through the line log.
func (s *Set) listing(ctx context.Context, flag, image string, w io.Writer) error {
	path, err := s.bin(Unsquashfs)
	if err != nil {
		return err
	}
	if image == "" {
		return errors.New("unsquashfs: no image given")
	}
	if w == nil {
		return errors.New("unsquashfs: no output writer given")
	}
	return run(ctx, runSpec{
		name:   Unsquashfs,
		path:   path,
		args:   []string{flag, image},
		stdout: w,
	})
}
