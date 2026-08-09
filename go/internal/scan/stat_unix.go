//go:build unix

package scan

import (
	"os"
	"syscall"
)

// fileIDs extracts the device number, inode number and hard link count from a
// FileInfo produced by lstat. It returns zeroes when the platform-specific
// stat structure is not available, which keeps the walker usable (hard links
// are then simply charged more than once) rather than making it panic.
func fileIDs(fi os.FileInfo) (dev, ino, nlink uint64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink)
}
