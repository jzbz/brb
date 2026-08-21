// Package fsx holds the filesystem operations brb performs outside any one
// stage of the pipeline, in one copy each, because a second implementation of
// any of them is a second chance to get it wrong.
//
// Two are measurements: how many bytes a directory tree holds, and how many
// are still free on the volume under it. Both sides of a disc-sizing decision
// use them — the backup checks a disc directory against the media, the ISO
// builder checks the same tree against the free space it needs to copy it.
//
// Most of the rest are the staging area's safety rules — [SecureStaging],
// [SecureDir], [CreateFresh], [OpenAppend], with [FileOwner] under them. The
// writer and the reader share one staging tree layout and one threat: its
// default lives under a world-writable /var/tmp, and it holds plaintext. These
// rules used to be written once per caller, and each rewrite lost a piece of
// them, which is why they are here and not there.
//
// One more is exclusion rather than a rule: [LockStaging] holds an flock on
// the staging root for as long as a run lasts, so two brb processes cannot
// build into one tree. It is the newest thing here and the least visible in
// its absence — see [LockStaging] for why a set built by two runs at once
// verifies clean and does not restore.
//
// This package is Unix-only, as internal/scan and internal/backup are: the
// rules above are made of flock(2), O_NOFOLLOW, fchmod(2) and statfs(2), and
// there is no useful way to fake any of them. There are no non-Unix stubs, on
// purpose — a build in which the staging lock quietly does nothing is worse
// than no build at all.
package fsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DirBytes sums the apparent size of every regular file beneath dir — the
// bytes the files contain, not the blocks they occupy. Directories, symlinks
// and device nodes count for nothing, matching what ends up in an ISO of the
// tree, which is what every caller is really asking about.
//
// ctx is checked once per entry, so a walk of a large tree stops promptly.
func DirBytes(ctx context.Context, dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measuring %s: %w", dir, err)
	}
	return total, nil
}
