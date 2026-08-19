//go:build unix

package restore

import (
	"io/fs"
	"syscall"
)

// fileOwner reports the uid that owns the file fi describes. ok is false when
// the platform's stat carries no such thing, in which case the ownership check
// in secureDir has nothing to compare and is skipped rather than failed.
func fileOwner(fi fs.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
