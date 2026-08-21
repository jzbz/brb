package fsx

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockStagingRefusesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := LockStaging(dir)
	if err != nil {
		t.Fatalf("first LockStaging: %v", err)
	}
	second, err := LockStaging(dir)
	if err == nil {
		second.Release()
		t.Fatal("LockStaging succeeded twice on one directory")
	}
	if !strings.Contains(err.Error(), "another brb is using") {
		t.Errorf("LockStaging = %v, want the in-use refusal", err)
	}
	// The refusal names the holder, which is what makes it actionable.
	if want := "(pid " + strconv.Itoa(os.Getpid()) + ")"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not name the holder %s", err, want)
	}
	// Releasing hands it on rather than leaving the directory unusable.
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	third, err := LockStaging(dir)
	if err != nil {
		t.Fatalf("LockStaging after Release: %v", err)
	}
	third.Release()
}

// The lock file survives the lock. Unlinking it would let a second run create
// a fresh inode at the same path and lock that instead, which is the classic
// way to end up with two holders that both believe they are alone.
func TestLockStagingLeavesItsFileBehind(t *testing.T) {
	dir := t.TempDir()
	l, err := LockStaging(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, LockName)); err != nil {
		t.Errorf("the lock file was removed on release: %v", err)
	}
}

// A lock held by a process that is gone must not block the next run: brb is
// built around being killed and resumed, so a stale lock would break the
// feature it is meant to protect. flock gives this for free — the kernel drops
// it when the last descriptor closes, however the process ended — and this
// pins that we depend on flock and not on a pid file.
func TestLockStagingIsNotBlockedByADeadHolder(t *testing.T) {
	dir := t.TempDir()
	// Simulate the killed run: its descriptor goes away with no Release call,
	// exactly as it would when the kernel closes it on process exit.
	l, err := LockStaging(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.f.Close(); err != nil { // no Release: the "process died" path
		t.Fatal(err)
	}
	l.f = nil
	again, err := LockStaging(dir)
	if err != nil {
		t.Fatalf("a lock left by a dead holder blocked the next run: %v", err)
	}
	again.Release()
}

// The lock file is opened O_RDWR|O_CREAT and then truncated, so a symlink
// planted at <STAGING>/.brb.lock is a request to empty whatever it points at,
// with this run's privileges — root's, under the sudo run the README
// recommends. STAGING's default lives under a world-writable /var/tmp, so the
// planter needs no privilege of their own.
//
// LockStaging does not assume its caller secured the directory first: the
// refusal has to come from the open itself. iso.Build reached this function
// without a SecureStaging in front of it, and "every caller remembers" is not
// a property a file-truncating open should depend on.
func TestLockStagingRefusesASymlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	const contents = "the operator's file, which this run has no business touching"
	if err := os.WriteFile(victim, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, LockName)); err != nil {
		t.Fatal(err)
	}

	l, err := LockStaging(dir)
	if err == nil {
		l.Release()
		t.Fatal("LockStaging opened through a symlink planted at the lock path")
	}
	if !strings.Contains(err.Error(), "is a symlink") || !strings.Contains(err.Error(), victim) {
		t.Errorf("LockStaging = %v, want a refusal naming the link and its target", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != contents {
		t.Fatalf("the planted symlink was followed: the victim now holds %q", got)
	}
}
