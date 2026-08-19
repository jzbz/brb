//go:build unix

package fsx

import (
	"errors"
	"os"
	"syscall"
)

// tryLockExclusive takes a non-blocking exclusive flock. It reports false, and
// no error, when the lock is already held: that is the ordinary case this
// exists to detect, not a failure to ask.
func tryLockExclusive(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK): // EAGAIN, same value
		return false, nil
	default:
		return false, err
	}
}
