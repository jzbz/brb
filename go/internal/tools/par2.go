package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Par2Create writes recovery data for Par2Options.File, or — when
// Par2Options.Inputs is set — for every file in Inputs under the set name
// File. The run happens with Par2Options.Dir as its working directory and every
// file named relatively, so the .par2 volumes record bare names and stay valid
// after the set is copied onto a disc. A set built anywhere else records the
// path it was built from and will not repair on the disc.
//
// On failure or cancellation every .par2 volume produced so far is removed, so
// a later run cannot mistake a half-written set for a good one.
func (s *Set) Par2Create(ctx context.Context, o Par2Options) error {
	path, err := s.bin(Par2)
	if err != nil {
		return err
	}
	if o.Dir == "" {
		return errors.New("par2: no working directory given")
	}
	if o.File == "" {
		return errors.New("par2: no file given")
	}
	for _, name := range append([]string{o.File}, o.Inputs...) {
		if name == "" {
			return fmt.Errorf("par2: empty file name in the set for %s", o.File)
		}
		if filepath.IsAbs(name) {
			return fmt.Errorf("par2: file must be relative to %s, got %q", o.Dir, name)
		}
	}
	if o.Redundancy < 0 || o.Redundancy > 100 {
		return fmt.Errorf("par2: redundancy %d%% out of range (0-100)", o.Redundancy)
	}

	done := false
	defer func() {
		if !done {
			removePar2(o.Dir, o.File)
		}
	}()

	if err := run(ctx, runSpec{
		name: Par2,
		path: path,
		args: Par2CreateArgs(o),
		dir:  o.Dir,
		log:  o.Log,
	}); err != nil {
		return err
	}
	done = true
	return nil
}

// removePar2 deletes the recovery volumes belonging to file, ignoring errors:
// this runs on a failure path where there is nothing useful left to report.
//
// file may be either a protected file ("disc01.squashfs.age", whose set is
// disc01.squashfs.age.par2) or the set's own name ("sidecars.par2"); par2
// derives both from the same base, so the trailing ".par2" is stripped first
// and "sidecars.par2.par2" is never looked for.
func removePar2(dir, file string) {
	base := strings.TrimSuffix(file, ".par2")
	patterns := []string{base + ".par2", base + ".vol*.par2"}
	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			continue
		}
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// Par2Verify checks a file against its recovery set. A damaged file makes par2
// exit non-zero, which surfaces as an error here rather than being swallowed.
func (s *Set) Par2Verify(ctx context.Context, dir, par2 string, log io.Writer) error {
	path, err := s.bin(Par2)
	if err != nil {
		return err
	}
	if dir == "" || par2 == "" {
		return errors.New("par2 verify: need a directory and a .par2 file")
	}
	return run(ctx, runSpec{
		name: Par2,
		path: path,
		args: Par2VerifyArgs(par2),
		dir:  dir,
		log:  log,
	})
}

// Par2Repair repairs a damaged file from its recovery set. extras name further
// damaged copies of the same file, relative to dir; par2 draws on every copy it
// is told about, which is what makes a second burn of the set worth ingesting.
func (s *Set) Par2Repair(ctx context.Context, dir, par2 string, log io.Writer, extras ...string) error {
	path, err := s.bin(Par2)
	if err != nil {
		return err
	}
	if dir == "" || par2 == "" {
		return errors.New("par2 repair: need a directory and a .par2 file")
	}
	for _, x := range extras {
		if x == "" {
			return errors.New("par2 repair: empty extra file name")
		}
		if filepath.IsAbs(x) {
			return fmt.Errorf("par2 repair: extra file must be relative to %s, got %q", dir, x)
		}
	}
	return run(ctx, runSpec{
		name: Par2,
		path: path,
		args: Par2RepairArgs(par2, extras...),
		dir:  dir,
		log:  log,
	})
}
