package restore

import (
	"fmt"

	"github.com/jzbz/brb/internal/fsx"
)

// secureStaging makes the staging directory, and the subdirectories this
// command is about to write into, safe to hold plaintext.
//
// The rules, and the reasoning behind each of them, live in [fsx.SecureDir]:
// the writer's preflight secures its own staging tree with the same three
// checks, and the two used to disagree about which of them mattered. All this
// adds is the "restore: " prefix the operator sees on everything this package
// refuses.
func (o Options) secureStaging(subs ...string) error {
	if err := fsx.SecureStaging(o.Cfg.Staging, subs...); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// lockStaging takes the staging lock for the length of one command, so that a
// reader cannot write into a tree a backup is still building — the case that
// matters is `burn`, which under ISO_MODE=ondemand masters its ISO from a disc
// directory the writer may not have finished. See [fsx.LockStaging].
//
// The returned function is always safe to call, including after an error, so
// callers can defer it unconditionally.
func (o Options) lockStaging() (func(), error) {
	// The lock file lives in the staging root, so the root has to exist and be
	// ours before there is anywhere to put it. Securing it here rather than
	// relying on the caller having done so keeps this safe to call first: a
	// reader's very first command finds no staging directory at all, and
	// "opening the staging lock: no such file or directory" is a poor way to
	// learn that. secureStaging is idempotent, so the later per-image calls
	// still do their own work.
	if err := o.secureStaging(); err != nil {
		return func() {}, err
	}
	lock, err := fsx.LockStaging(o.Cfg.Staging)
	if err != nil {
		return func() {}, fmt.Errorf("restore: %w", err)
	}
	return func() {
		if err := lock.Release(); err != nil {
			o.UI.Warn("could not release the staging lock: %v", err)
		}
	}, nil
}
