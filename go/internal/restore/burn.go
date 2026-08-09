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
// range ("7-20", "7-"), or "all", exactly as brb.sh accepts them.
//
// The ISOs are not assumed to exist. Under the default ISO_MODE=ondemand
// nothing has built them, and under KEEP_ISOS=0 each one is deleted again as
// soon as its disc is written, so what a set is made of is its disc
// DIRECTORIES: the queue comes from those, each disc's ISO is built at the
// moment it is needed, and gaps in the numbering are simply a shorter queue.
//
// The disc count in the label and in "disc 3 of 20" comes from MANIFEST.txt for
// the same reason — a set half burned would otherwise renumber itself.
func Burn(ctx context.Context, o Options, which string) error {
	if err := o.check(); err != nil {
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
				o.UI.Warn("stopped after disc %d", queue[i-1])
				return nil
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
	// confirmation skips the burn and stays in the queue.
	if burned == len(queue) {
		o.UI.OK("burned %d disc(s)", burned)
	} else {
		o.UI.OK("burned %d of %d disc(s), %d skipped", burned, len(queue), len(queue)-burned)
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
// A declined confirmation is not an error: it skips that disc, exactly as
// brb.sh does.
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

	// Only here, and only on success: the bytes are on the medium, so the second
	// copy in staging has served its purpose. Every path above that did not burn
	// has already returned.
	if o.Cfg.KeepISOs {
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
