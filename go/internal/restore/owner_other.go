//go:build !unix

package restore

import "io/fs"

// fileOwner has no notion of a numeric owner to report on this platform, so
// secureDir's ownership check is skipped here. brb runs on Linux; this exists
// only so the package still compiles elsewhere.
func fileOwner(fs.FileInfo) (uid int, ok bool) {
	return 0, false
}
