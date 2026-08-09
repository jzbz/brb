package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// payloadNames are the artifacts build-dist.sh produces and every disc carries:
// the tool as a portable bash script, as a static binary for each of the two
// architectures worth caring about, and as complete source with its
// dependencies vendored.
//
// None of it is needed to restore — every README's restore recipe uses nothing
// but sha512sum, par2, age and the kernel — but a static binary needs no
// interpreter and no shared libraries, which makes it a better bet fifteen
// years out than anything the reader has to install first.
var payloadNames = []string{
	"brb.sh",
	"brb-linux-amd64",
	"brb-linux-aarch64",
	"brb-src.tar.gz",
}

// PayloadNames returns the files brb looks for in the dist directory, in the
// order they are reported and listed.
func PayloadNames() []string { return append([]string(nil), payloadNames...) }

// PayloadMode is the mode a payload file is given on the disc: the source
// tarball is data, everything else is something a restorer runs.
func PayloadMode(name string) fs.FileMode {
	if strings.HasSuffix(name, ".tar.gz") {
		return 0o644
	}
	return 0o755
}

// writePayload copies the dist payload into every disc directory.
//
// Nothing about a missing payload fails a backup: a set without it is still a
// complete, restorable set. A dist directory that cannot be found, or a file
// missing from one that can, is reported and stepped over. Only a genuine
// failure to place a file that is there stops the run, which is what brb.sh
// does too.
//
// It runs after buildDiscDirs, so the disc directories exist, and before
// writeReadmes, so that the cross-built brb-linux-<arch> from the payload wins
// over the copy of the running binary and each README lists what is really on
// its disc.
func (r *runner) writePayload(ctx context.Context, total int) error {
	r.p.Log("disc payload (the tool itself, carried on every disc)")

	dist, err := r.cfg.ResolveDistDir()
	if err != nil {
		r.p.Warn("%v", err)
	}
	if dist == "" {
		r.p.Warn("no dist directory found — discs will carry only %s, the binary running now",
			SelfCopyName())
		r.p.Step("build one with ./build-dist.sh, or set BRB_DIST_DIR")
		return nil
	}
	r.p.Step("payload from %s", dist)

	present := make([]string, 0, len(payloadNames))
	for _, name := range payloadNames {
		fi, err := os.Stat(filepath.Join(dist, name))
		if err != nil || !fi.Mode().IsRegular() {
			r.p.Warn("payload missing: %s", name)
			continue
		}
		present = append(present, name)
	}
	if len(present) < len(payloadNames) {
		r.p.Step("run ./build-dist.sh to produce the full payload")
	}
	if len(present) == 0 {
		r.p.Warn("no payload files were found in %s", dist)
		return nil
	}

	for n := 1; n <= total; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("backup: aborted: %w", err)
		}
		dd := filepath.Join(r.dirs.Discs, discDirName(n))
		for _, name := range present {
			src := filepath.Join(dist, name)
			if err := placePayload(ctx, src, filepath.Join(dd, name), PayloadMode(name)); err != nil {
				return fmt.Errorf("backup: disc %d: %w", n, err)
			}
		}
	}
	r.p.OK("%d payload file(s) on every disc", len(present))
	return nil
}

// placePayload puts one payload file into a disc directory with the mode it
// must have on the disc.
//
// A hard link is used only when the source already has that mode, so that
// staging does not carry a second multi-megabyte copy per disc. When the mode
// differs the file is copied and the copy is chmodded instead: a link shares an
// inode with the file in the operator's dist directory, so a chmod through it
// would silently rewrite their copy — or fail outright when the dist directory
// is read-only, which a packaged /usr/share/brb is. The dist directory is never
// written to.
func placePayload(ctx context.Context, src, dst string, mode fs.FileMode) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("backup: payload %s: %w", src, err)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: replacing %s: %w", dst, err)
	}
	if fi.Mode().Perm() == mode.Perm() {
		if err := os.Link(src, dst); err == nil {
			return nil
		}
	}
	return copyFile(ctx, src, dst, mode)
}

// discToolArtifacts lists the copies of brb actually present in the root of one
// disc directory.
//
// The README is rendered from this rather than from a fixed list, so it can
// never promise a restorer a file that is not on the disc in front of them.
func discToolArtifacts(dir string) []string {
	candidates := append(PayloadNames(), SelfCopyName())
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if seen[name] {
			continue
		}
		seen[name] = true
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			out = append(out, name)
		}
	}
	return out
}
