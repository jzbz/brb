//go:build !unix

package fsx

import "os"

// tryLockExclusive cannot take a lock on this platform, so it reports success
// and leaves the caller unprotected. brb runs on Linux; this exists only so
// the package still compiles elsewhere. Claiming the lock was taken is the
// right lie here: the alternative is refusing to run at all on a platform the
// program does not otherwise support.
func tryLockExclusive(*os.File) (bool, error) { return true, nil }
