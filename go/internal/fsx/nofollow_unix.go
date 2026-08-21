//go:build unix

package fsx

import (
	"errors"
	"os"
	"syscall"
)

// oNoFollow makes an open refuse when the final path component is a symlink.
// The os package offers no portable spelling of it, so it is named here per
// platform rather than at the call site.
const oNoFollow = syscall.O_NOFOLLOW

// openDirNoFollow opens dir itself — never anything it might be a link to —
// and returns a descriptor to it.
//
// Everything [SecureDir] does after the directory exists is done through this
// descriptor rather than by name, because a name is re-resolved by every
// syscall that takes one. O_DIRECTORY refuses a non-directory, O_NOFOLLOW
// refuses a symlink at the final component, and both refusals happen inside
// the open: there is no window between deciding the path is safe and using it.
//
// The two refusals are not cleanly distinguishable from the errno. Linux
// answers a symlink under O_DIRECTORY|O_NOFOLLOW with ENOTDIR, not ELOOP, so
// [isNotDir] and [isSymlinkLoop] between them tell the caller only that the
// open was refused; SecureDir works out which sentence to print with an Lstat
// it makes no decision on.
func openDirNoFollow(dir string) (*os.File, error) {
	return os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|oNoFollow, 0)
}

// isSymlinkLoop reports whether err is the kernel refusing to follow a symlink
// (ELOOP), which under O_NOFOLLOW means the final component is one.
func isSymlinkLoop(err error) bool { return errors.Is(err, syscall.ELOOP) }

// isNotDir reports whether err is ENOTDIR, which under O_DIRECTORY means the
// path exists and is not a directory.
func isNotDir(err error) bool { return errors.Is(err, syscall.ENOTDIR) }
