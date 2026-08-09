//go:build unix

package fsx

import (
	"fmt"
	"syscall"
)

// FreeSpace returns the bytes available on the filesystem holding path.
//
// It reports the blocks available to an unprivileged user (statfs's f_bavail),
// not the total free blocks, because the reserved-block margin most Linux
// filesystems keep back is not space a backup can use. The directory must
// already exist.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := int64(st.Bsize)
	if bsize <= 0 {
		return 0, fmt.Errorf("statfs %s reported a block size of %d", path, bsize)
	}
	avail := int64(st.Bavail)
	if avail < 0 {
		return 0, fmt.Errorf("statfs %s reported %d available blocks", path, st.Bavail)
	}
	return avail * bsize, nil
}
