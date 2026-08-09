// Package fsx holds the two filesystem measurements brb makes outside any one
// stage of the pipeline: how many bytes a directory tree holds, and how many
// are still free on the volume under it.
//
// They live together, and apart from their callers, because both sides of a
// disc-sizing decision use them — the backup checks a disc directory against
// the media, the ISO builder checks the same tree against the free space it
// needs to copy it — and a second implementation of either is a second chance
// to get that decision wrong.
package fsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DirBytes sums the apparent size of every regular file beneath dir, which is
// what brb.sh measures with `du -sb --apparent-size`. Directories, symlinks and
// device nodes count for nothing, matching what ends up in an ISO of the tree.
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
