//go:build unix

package fsx

import "syscall"

// oNoFollow makes an open refuse when the final path component is a symlink.
// The os package offers no portable spelling of it, so it is named here per
// platform rather than at the call site.
const oNoFollow = syscall.O_NOFOLLOW
