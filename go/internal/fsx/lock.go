package fsx

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LockName is the file brb takes its staging lock on. It sits at the root of
// the staging directory and is empty apart from the pid of whoever holds it,
// which exists only so a refusal can name the other run.
//
// The file is deliberately NOT removed when the lock is released. Unlinking a
// file another process may be about to lock is how advisory locking is
// commonly got wrong: the second process locks an inode that is no longer at
// the path, a third creates a fresh one, and both then believe they hold it.
// The file is zero bytes and goes when staging is wiped.
const LockName = ".brb.lock"

// StagingLock is a held exclusive lock on a staging directory.
type StagingLock struct {
	f    *os.File
	path string
}

// LockStaging takes an exclusive lock on dir for as long as this process
// lives, so that two brb runs cannot write into one staging tree at once.
//
// WHY THIS EXISTS. Every guard against a second run was, until now, a guard
// against a second run that had already FINISHED: startFresh refuses leftover
// encrypted images, and a plain backup refuses a state file that records
// completed discs. Nothing noticed a run happening at that moment, and the
// tool actively invites one — a twenty-disc set takes days, ISO_MODE=ondemand
// means `burn` builds each ISO from the disc directory when the disc goes in
// the drive, and "start burning disc 1 while the rest builds" is an obvious
// thing to try. It is also the one arrangement in which `burn` reads a disc
// directory that `backup` has not finished writing.
//
// The damage is quiet, which is what makes it worth forty lines. Two runs
// building the same image write the same path; unsquashfs -stat then reads
// only the superblock, so a body that is a mix of the two can still look like
// a readable squashfs. The encrypt-and-hash pass records the digest of
// whatever it was handed, par2 protects those bytes faithfully, SHA512SUMS
// agrees with itself, and verify-disc passes. The disc is wrong, and says so
// for the first time at restore, years later.
//
// WHY flock(2) AND NOT A PID FILE. The kernel drops a flock when the holding
// process dies, however it dies. A pid file created with O_EXCL does not: a
// backup killed with SIGKILL would leave one behind and block the resume
// afterwards — and resume-after-kill is the feature this program is built
// around, so a lock that broke it would be worse than no lock at all. There is
// nothing to clean up here and no --force to add.
//
// The lock is advisory and same-machine. It stops the accident it is named
// for; it is not a defence against another user, which is what [SecureDir]'s
// ownership rule is for. Call it AFTER securing the directory, so the lock
// file is created somewhere this process has already proven it owns.
func LockStaging(dir string) (*StagingLock, error) {
	if dir == "" {
		return nil, fmt.Errorf("no STAGING directory to lock")
	}
	path := filepath.Join(filepath.Clean(dir), LockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the staging lock %s: %w", path, err)
	}
	locked, err := tryLockExclusive(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !locked {
		holder := lockHolder(f)
		f.Close()
		return nil, fmt.Errorf("another brb is using the staging directory %s%s — "+
			"wait for it to finish, or point STAGING at a directory of its own; "+
			"two runs sharing one staging tree can write the same image at the same time, "+
			"and the result verifies clean and does not restore", dir, holder)
	}
	// Best effort, and only ever read by the message above: a pid that could
	// not be written costs nothing, so it must not fail the run.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &StagingLock{f: f, path: path}, nil
}

// Release drops the lock. Closing the file is what releases it; the file
// itself stays, for the reason given on [LockName].
func (l *StagingLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}

// lockHolder renders " (pid N)" for the message when the file names one, and
// "" when it does not. It is advisory text: the pid may be stale by the time
// it is read, which is exactly why nothing is decided on it.
func lockHolder(f *os.File) string {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}
