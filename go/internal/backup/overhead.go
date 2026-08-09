package backup

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jzbz/brb/internal/ui"
)

// discOverhead is what a finished disc carries besides the image, its parity
// and the encrypted index — the files RESERVE_BYTES exists to cover.
type discOverhead struct {
	// Tool is the copy of brb at the disc root: the dist payload when there is
	// one, otherwise this binary copying itself.
	Tool int64
	// Docs is a generous allowance for README.md, MANIFEST.txt and SHA512SUMS,
	// which are text and never large.
	Docs int64
	// Names is what the tool bytes are made of, for the diagnostics.
	Names []string
}

// Total is the number of bytes a disc needs on top of its data directory.
func (d discOverhead) Total() int64 { return d.Tool + d.Docs }

// docsAllowance covers README.md, MANIFEST.txt and SHA512SUMS. The manifest
// grows with the disc count and the README is a few kilobytes; 2 MiB is far
// more than any real set needs and keeps the check from being the thing that
// fails a backup.
const docsAllowance = 2 << 20

// measureDiscOverhead reports what each disc will carry besides its data.
//
// This is the whole point of HL-4: brb.sh's self-copy is a ~114 KB shell script
// and this program's is a ~8 MB static binary, so the same
// (DISC_CAPACITY_BYTES, RESERVE_BYTES) pair can be comfortable for one
// implementation and impossible for the other. Discovering that after building
// every image — which is where checkDiscSizes sits — wastes hours. Measure it
// before anything is built instead.
//
// Errors are never fatal: a payload we cannot stat is simply not counted, and
// the late check still catches anything this misses.
func (r *runner) measureDiscOverhead() discOverhead {
	d := discOverhead{Docs: docsAllowance}

	// The dist payload, when there is one, is what actually lands on the disc.
	if dist, err := r.cfg.ResolveDistDir(); err == nil && dist != "" {
		for _, name := range payloadNames {
			if fi, serr := os.Stat(filepath.Join(dist, name)); serr == nil && !fi.IsDir() {
				d.Tool += fi.Size()
				d.Names = append(d.Names, fmt.Sprintf("%s %s", name, ui.HumanBytes(fi.Size())))
			}
		}
	}

	// Without a payload the run copies this binary in under its own name. With
	// one, it only does so when the payload did not already supply that
	// architecture, so counting it again would double-count.
	self := SelfCopyName()
	if !containsName(d.Names, self) {
		if exe, err := selfPath(); err == nil {
			if fi, serr := os.Stat(exe); serr == nil {
				d.Tool += fi.Size()
				d.Names = append(d.Names, fmt.Sprintf("%s %s", self, ui.HumanBytes(fi.Size())))
			}
		}
	}
	return d
}

// containsName reports whether any "name size" entry starts with name.
func containsName(entries []string, name string) bool {
	for _, e := range entries {
		if len(e) > len(name) && e[:len(name)] == name && e[len(name)] == ' ' {
			return true
		}
	}
	return false
}

// checkReserve fails when RESERVE_BYTES cannot cover what every disc carries
// besides its data. It is called from the plan path and from preflight, so a
// plan that reports N discs is a plan that will actually finish.
//
// It deliberately does NOT adjust the image budget to make room. That budget is
// what decides how files are packed into discs, and changing it here would make
// this implementation lay out a set differently from brb.sh for the same input.
// The operator raises RESERVE_BYTES (or the capacity) instead, and both
// implementations then agree.
func (r *runner) checkReserve() error {
	d := r.measureDiscOverhead()
	if d.Total() <= r.cfg.ReserveBytes {
		return nil
	}
	need := d.Total()
	return fmt.Errorf("backup: every disc carries %s of tool and documentation "+
		"(%v) but RESERVE_BYTES is only %s. Set RESERVE_BYTES=%d (or larger) and re-run.\n"+
		"  Note this program's copy of itself is a static binary of a few megabytes, where "+
		"brb.sh's is a shell script of a few hundred kilobytes, so a configuration that "+
		"fits the shell version can still be too tight for this one",
		ui.HumanBytes(d.Total()), d.Names, ui.HumanBytes(r.cfg.ReserveBytes), need)
}
