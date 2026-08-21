package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// MakeISO builds one ISO 9660 image from ISOOptions.Dir.
//
// xorriso is chatty enough that the natural thing to write is
// "xorriso ... | grep -v ... || true" — and that discards its exit status twice
// over: the pipeline reports grep's status, and the "|| true" that stops grep's
// own "no lines matched" from failing the build throws even that away. A failed
// burn image then looks exactly like a successful one. Here the noise is
// filtered line by line inside this process (see KeepISOLine) while the child's
// real exit status is checked, and the output is confirmed to be a non-empty
// file. A partial ISO is removed when the build fails or the context is
// cancelled.
func (s *Set) MakeISO(ctx context.Context, o ISOOptions) error {
	path, err := s.bin(Xorriso)
	if err != nil {
		return err
	}
	if o.Dir == "" {
		return errors.New("xorriso: no source directory given")
	}
	if o.Out == "" {
		return errors.New("xorriso: no output path given")
	}
	if st, err := os.Stat(o.Dir); err != nil {
		return fmt.Errorf("xorriso: source directory %s: %w", o.Dir, err)
	} else if !st.IsDir() {
		return fmt.Errorf("xorriso: source %s is not a directory", o.Dir)
	}
	if o.Label != SanitiseLabel(o.Label) {
		return fmt.Errorf("xorriso: volume label %q is not a legal ISO 9660 volume id", o.Label)
	}
	if err := os.Remove(o.Out); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("xorriso: removing stale ISO %s: %w", o.Out, err)
	}

	done := false
	defer func() {
		if !done {
			_ = os.Remove(o.Out)
		}
	}()

	if err := run(ctx, runSpec{
		name:   Xorriso,
		path:   path,
		args:   MkisofsArgs(o),
		log:    o.Log,
		filter: KeepISOLine,
	}); err != nil {
		return err
	}

	st, err := os.Stat(o.Out)
	if err != nil {
		return fmt.Errorf("xorriso: ISO %s: %w", o.Out, err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("xorriso produced an empty ISO: %s", o.Out)
	}
	done = true
	return nil
}

// ProbeISO builds a throwaway ISO in a temporary directory to prove the option
// set this package uses is accepted by the installed xorriso. Running it during
// preflight turns "a bad option" into a five-second failure instead of one
// discovered after hours of image building.
func (s *Set) ProbeISO(ctx context.Context) error {
	if _, err := s.bin(Xorriso); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "brb-isoprobe-")
	if err != nil {
		return fmt.Errorf("xorriso probe: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o700); err != nil {
		return fmt.Errorf("xorriso probe: %w", err)
	}
	if err := os.WriteFile(filepath.Join(src, "probe.txt"), []byte("probe\n"), 0o600); err != nil {
		return fmt.Errorf("xorriso probe: %w", err)
	}
	if err := s.MakeISO(ctx, ISOOptions{
		Dir:   src,
		Out:   filepath.Join(dir, "probe.iso"),
		Label: "PROBE",
	}); err != nil {
		return fmt.Errorf("xorriso rejected the ISO options brb uses (try: xorriso -as mkisofs -help): %w", err)
	}
	return nil
}

// BurnISO writes an ISO to an optical drive and ejects it. A speed of zero or
// less leaves the drive to choose.
func (s *Set) BurnISO(ctx context.Context, dev, iso string, speed int, log io.Writer) error {
	path, err := s.bin(Xorriso)
	if err != nil {
		return err
	}
	if dev == "" {
		return errors.New("xorriso: no burner device given")
	}
	st, err := os.Stat(iso)
	if err != nil {
		return fmt.Errorf("xorriso: ISO %s: %w", iso, err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("xorriso: refusing to burn an empty ISO: %s", iso)
	}
	return run(ctx, runSpec{
		name: Xorriso,
		path: path,
		args: CdrecordArgs(dev, iso, speed),
		log:  log,
	})
}
