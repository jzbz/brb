//go:build !unix

package fsx

import "io/fs"

// FileOwner has no notion of a numeric owner to report on this platform, so
// [SecureDir]'s ownership check is skipped here. brb runs on Linux; this
// exists only so the package still compiles elsewhere.
func FileOwner(fs.FileInfo) (uid int, ok bool) {
	return 0, false
}
