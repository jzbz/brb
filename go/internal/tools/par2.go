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
//
// The directory is listed and each base name matched on its own, rather than
// handing dir+pattern to filepath.Glob: Glob treats the whole string as a
// pattern, so a staging directory whose path holds '[', ']', '*' or '?' —
// "/mnt/backup [2026]" is not far-fetched — made every pattern match nothing
// and the half-written set was left behind. Only the volume names are ever
// patterns here; the directory never is.
func removePar2(dir, file string) {
	base := strings.TrimSuffix(file, ".par2")
	for _, name := range Par2VolumeNames(dir, base) {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// Par2VolumeNames lists the entries of dir that belong to the par2 set with the
// given base name: "<base>.par2" and "<base>.vol*.par2". base is a bare file
// name and is matched literally except for that one wildcard, and dir is never
// interpreted as a pattern — see removePar2 for why that matters. A directory
// that cannot be read yields nothing.
func Par2VolumeNames(dir, base string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if n == base+".par2" || isPar2Volume(n, base) {
			out = append(out, n)
		}
	}
	return out
}

// isPar2Volume reports whether name is a recovery volume of the set base:
// "<base>.vol<anything>.par2", the shape par2cmdline writes ("vol000+30").
func isPar2Volume(name, base string) bool {
	rest, ok := strings.CutPrefix(name, base+".vol")
	return ok && strings.HasSuffix(rest, ".par2")
}

// Par2Verify checks a file against its recovery set. A damaged file makes par2
// exit non-zero, which surfaces as an error here rather than being swallowed.
//
// NO COMMAND CALLS THIS TODAY, and that is deliberate rather than an oversight
// left lying around. The restore path never wants verify-without-repair: `par2
// repair` verifies first and only rewrites what is broken, so calling verify
// ahead of it would read the whole recovery set twice for no extra information.
// `brb verify-disc` does not use it either — it checks SHA512SUMS, which needs
// nothing but sha512sum, and adding a par2 verify would make par2 a hard
// dependency of the one command an operator runs on a strange machine to find
// out whether a disc is readable at all. brb.sh's verify-disc would then have
// to match it, or the two readers would disagree about what "verified" means.
//
// It is kept because it is the honest tools-layer counterpart of Par2Repair,
// it is covered against real par2 by TestPar2CreateVerifyRepair, and wiring it
// into a read-only check is a decision about the reader contract rather than
// about this package. Delete it if that decision goes the other way; do not
// call it from a repair path.
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
