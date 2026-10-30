//go:build unix

package fsx

import (
	"io/fs"
	"syscall"
)

// FileOwner reports the uid that owns the file fi describes. ok is false when
// the platform's stat carries no such thing, in which case the ownership check
// in [SecureDir] has nothing to compare and is skipped rather than failed.
func FileOwner(fi fs.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// DeviceID reports the device that holds the file fi describes, which is what
// tells a mount point from the directory it is mounted on. ok is false when the
// platform's stat carries no such thing, and a caller then has nothing to
// compare and skips its check, as [SecureDir] skips the ownership one. The
// conversion is for darwin, whose Dev is an int32.
func DeviceID(fi fs.FileInfo) (dev uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
