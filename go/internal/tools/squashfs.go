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
	err = run(ctx, runSpec{
		name: Mksquashfs,
		path: path,
		args: MksquashfsArgs(o),
		dir:  o.SourceDir,
		log:  o.Log,
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
