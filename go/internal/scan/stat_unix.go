//go:build unix

package scan

import (
	"os"
	"syscall"
)

// readable reports whether the regular file at path can be opened for reading,
// by opening and closing it. The walker calls it for regular files only, but
// the flags defend against the file having been swapped for something else
// between the lstat and the open: O_NONBLOCK so a fifo cannot stall the walk,
// O_NOFOLLOW so a symlink is not followed. Nothing is read; the open is the
// whole test, and it is the same open mksquashfs will attempt.
func readable(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

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
