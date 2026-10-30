package restore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// Burn writes a staged disc set to optical media. which is a disc number, a
// range ("7-20", "7-"), or "all" — the selection `brb iso` takes too (see
// iso.ParseRange). There is no shell counterpart: brb.sh is the reader only,
// and refuses burn by name.
//
// The ISOs are not assumed to exist. Under the default ISO_MODE=ondemand
// nothing has built them, and under KEEP_ISOS=0 each one is deleted again as
// soon as its disc is written — unless there is no disc directory to rebuild
// it from, when it is kept (see burnOne) — so what a set is made of is its
// disc DIRECTORIES: the queue comes from those, each disc's ISO is built at
// the moment it is needed, and gaps in the numbering are simply a shorter
// queue. Only a tree carried in as ISOs alone is queued from its ISOs.
//
// The disc count in the label and in "disc 3 of 20" comes from MANIFEST.txt for
// the same reason — a set half burned would otherwise renumber itself.
func Burn(ctx context.Context, o Options, which string) error {
	if err := o.check(); err != nil {
		return err
	}
	unlock, err := o.lockStaging()
	if err != nil {
		return err
	}
	defer unlock()
	// lockStaging secures the staging root and nothing under it, and burn
	// writes into iso/ — each image is built there, burned from there and
	// deleted there — and masters every image from discs/. Both are secured
	// here as backup's preflight and `brb iso` secure them: a symlinked iso/
	// used to have each ISO built, burned and deleted wherever the link
	// pointed, and a hand-made one kept whatever mode it was made with while
	// holding a second copy of every disc. A tree backup laid out is refused
	// nothing new; one carried in as ISOs only gains an empty discs/.
	//
	// Before the tool requirement, so the refusal is about the tree whatever
	// is installed, and a machine without xorriso can test it.
	if err := o.secureStaging(o.dirs().Discs, o.dirs().ISO); err != nil {
		return err
	}
	if err := o.Tools.Require(tools.Xorriso); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if o.Cfg.Burner == "" {
		return errors.New("restore: no BURNER configured")
	}

	dirs := o.dirs()
	nums, err := iso.DiscNumbers(dirs)
	if err != nil {
		return err
	}
	if len(nums) == 0 {
		return fmt.Errorf("restore: nothing to burn: no disc directories in %s and no ISOs in %s — "+
			"run 'brb backup' first", dirs.Discs, dirs.ISO)
	}
	rng, err := iso.ParseRange(which)
	if err != nil {
		return fmt.Errorf("restore: burn: %w", err)
	}
	total := iso.Total(o.Cfg.Staging, len(nums))

	var queue []int
	for _, n := range nums {
		if rng.Contains(n) {
			queue = append(queue, n)
		}
	}
	if len(queue) == 0 {
		return fmt.Errorf("restore: no discs matched %q in %s", which, dirs.Discs)
	}

	if o.UI.AssumeYes() && len(queue) > 1 {
		o.UI.Warn("--yes: all %d discs will be burned one after another without waiting for you to change the disc", len(queue))
	}
	burned := 0
	for i, n := range queue {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 {
			more, err := o.UI.Confirm(fmt.Sprintf("Continue to disc %d of %d?", n, total))
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}
			if !more {
				// Stopping is not the end of the report. Whatever was burned
				// before this still has to be counted and read back, so the
				// queue is cut to the discs actually offered and the loop is
				// left for the closing lines below — returning here used to
				// skip both, after real burns. "before" and this disc's number,
				// not "after" the previous one: that one may have been skipped.
				o.UI.Warn("stopped before disc %d", n)
				queue = queue[:i]
				break
			}
		}
		wrote, err := o.burnOne(ctx, n, total)
		if err != nil {
			return err
		}
		if wrote {
			burned++
		}
	}
	// The closing line is the one that ends up pasted into notes, so it must
	// count what reached the media, not what was offered: a declined per-disc
	// confirmation skips the burn and stays in the queue. A campaign stopped
	// early counts the discs it got to; the warning above says where it
	// stopped.
	if burned == len(queue) {
		o.UI.OK("burned %d disc(s)", burned)
	} else {
		o.UI.OK("burned %d of %d disc(s), %d skipped", burned, len(queue), len(queue)-burned)
	}
	// Worth repeating at the end, because the per-disc hints have scrolled by
	// under xorriso's output by now, and a set nobody read back is a set whose
	// first news of a bad drive arrives at restore time.
	if burned > 0 {
		o.UI.Step("verify what you burned:  brb verify-disc <n>  for each disc, with it in the drive")
	}
	return nil
}

// isoOptions bundles what internal/iso needs to build an image at burn time.
func (o Options) isoOptions() iso.Options {
	return iso.Options{Cfg: o.Cfg, UI: o.UI, Tools: o.Tools, Version: o.Version}
}

// burnOne builds disc n's ISO if it is not already there, confirms, burns it,
// and then — only then — drops the ISO again. burned reports whether anything
// actually reached the medium.
//
// A declined confirmation is not an error: it skips that disc, and the
// campaign moves on to the next.
func (o Options) burnOne(ctx context.Context, n, total int) (burned bool, err error) {
	path, err := o.isoOptions().Ensure(ctx, n, total)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	o.UI.Log("disc %d: insert a blank %s", n, o.Cfg.DiscType)
	yes, err := o.UI.Confirm(fmt.Sprintf("Burn %s (%s) to %s at %dx?",
		iso.Name(n), ui.HumanBytes(st.Size()), o.Cfg.Burner, o.Cfg.BurnSpeed))
	if err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	if !yes {
		o.UI.Warn("skipped disc %d", n)
		return false, nil
	}

	log := o.logWriter()
	defer log.Close()
	if err := o.Tools.BurnISO(ctx, o.Cfg.Burner, path, o.Cfg.BurnSpeed, log); err != nil {
		// Deliberately nothing is deleted here. A failed burn is precisely when
		// a retry needs the ISO still to be on disk.
		return false, fmt.Errorf("restore: burning disc %d: %w", n, err)
	}
	o.UI.OK("disc %d burned — label it: %s, disc %d of %d", n, o.Cfg.ArchiveName, n, total)
	// xorriso exiting 0 says the drive accepted the write, not that the medium
	// reads back. Nothing here reads it back — that is what verify-disc is for,
	// and it is the only thing that catches a drive or a blank that wrote badly
	// — so say so at the moment the disc is still in the tray and the ISO, on
	// the next line, may be about to be deleted.
	o.UI.Step("read it back before you trust it:  brb verify-disc %d", n)

	// Only here, and only on success: the bytes are on the medium, so the second
	// copy in staging has served its purpose. Every path above that did not burn
	// has already returned.
	if o.Cfg.KeepISOs {
		return true, nil
	}
	// The ISO is a cache only while its disc directory is there to build it
	// from again. In a staging tree carried here as ISOs and nothing else —
	// what `brb iso all` makes, and what iso.DiscNumbers falls back to — it is
	// the only copy of the disc this machine holds, and the disc just written
	// has not been read back. Deleting it then, with a promise that it could
	// be rebuilt from a directory that does not exist, left a disc that fails
	// verify-disc with nothing to burn again from.
	if !o.isoOptions().CanRebuild(n) {
		o.UI.Step("kept %s — it is the only copy here (there is no disc directory in %s to rebuild it "+
			"from); delete it yourself once the disc reads back", filepath.Base(path), o.dirs().Discs)
		return true, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.UI.Warn("could not remove %s after burning it: %v", filepath.Base(path), err)
		return true, nil
	}
	o.UI.Step("removed %s — rebuilt from %s if you burn this disc again (KEEP_ISOS=1 keeps them)",
		filepath.Base(path), o.dirs().Discs)
	return true, nil
}
