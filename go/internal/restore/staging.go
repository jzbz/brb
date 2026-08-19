package restore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// secureStaging makes the staging directory, and the subdirectories this
// command is about to write into, safe to hold plaintext. It replaces the
// warn-and-carry-on version this package used to have, and it fails rather
// than warns, for one reason: the README's default STAGING lives under
// /var/tmp, which is world-writable, and every local account can create
// things there ahead of the operator.
//
// For each of Staging and subs, in that order:
//
//   - It must not be a symlink. A restore writes decrypted images into these
//     directories by name, and a symlink planted at "restore" or "enc" before
//     the run sends every one of them wherever the planter chose. Lstat sees
//     the link itself, whatever it points at, and dangling or not.
//   - It is created if missing, and its mode is forced to 0700 whether or not
//     it was just created. MkdirAll applies the mode only to what it makes, so
//     a directory the operator created by hand keeps whatever the umask gave
//     it. A chmod that fails is fatal, as it is in the writer's makeDirs: for
//     as long as a restore runs, this tree holds the whole archive in the
//     clear, and "could not lock the door" is not something to proceed past.
//   - It must belong to the user running this process. A directory another
//     account owns is one that account can rename, replace or fill at will,
//     between any check made here and any write made later — the ownership
//     check is what turns the two checks above from a race into a guarantee.
//
// The subdirectories are checked after the root because a symlinked root
// would make every check below it a check on the link's target.
func (o Options) secureStaging(subs ...string) error {
	if o.Cfg.Staging == "" {
		return errors.New("restore: no STAGING directory configured")
	}
	dirs := append([]string{o.Cfg.Staging}, subs...)
	for _, d := range dirs {
		if err := secureDir(d); err != nil {
			return err
		}
	}
	return nil
}

// secureDir applies secureStaging's rules to one directory.
func secureDir(dir string) error {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(dir)
		return fmt.Errorf("restore: %s is a symlink (-> %s); a staging directory must be a real directory, "+
			"because decrypted images are written into it by name and would follow the link — "+
			"remove the link, or point STAGING at the directory itself", dir, target)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restore: %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("restore: creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restore: securing %s, which will hold plaintext: %w — "+
			"fix its permissions, or point STAGING at a directory you own", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("restore: %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("restore: %s exists and is not a directory — remove it, or point STAGING elsewhere", dir)
	}
	owner, ok := fileOwner(fi)
	if ok && owner != os.Geteuid() {
		return fmt.Errorf("restore: %s is owned by uid %d, not by this process (uid %d); "+
			"whoever owns it can replace anything under it while a restore is writing plaintext there — "+
			"%s", dir, owner, os.Geteuid(), ownershipAdvice(dir, owner))
	}
	return nil
}

// createFresh creates path for writing, removing whatever was there first.
//
// It is the one way this package opens a file it is about to fill in the
// staging area — a .part, a log — and it never uses O_TRUNC. O_TRUNC opens
// straight through whatever is already at the path, and in a staging directory
// whose default lives under /var/tmp "whatever is already there" can be a
// symlink another local user planted: opening through it would stream a
// decrypted image, or a ciphertext, into a file of that user's choosing with
// this process's privileges. O_EXCL refuses to open through a symlink at all,
// dangling or not; the kernel guarantees that, with no window between a check
// and the open.
//
// The stale-file case O_TRUNC used to cover silently — a run killed mid-write,
// then repeated — is handled by removing the leftover first. Removing a path
// removes the link itself and never what it points to, so that step is safe on
// a symlink too, and anything planted between the Remove and the open still
// meets O_EXCL.
func createFresh(path string, mode os.FileMode) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("removing the stale %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	return f, nil
}

// ownershipAdvice says what to do about a staging directory somebody else
// owns. Under sudo the likeliest story is a directory the operator made
// earlier without it, in which case chown is the fix; otherwise the fix is a
// directory of one's own.
func ownershipAdvice(dir string, owner int) string {
	if os.Geteuid() == 0 {
		return fmt.Sprintf("chown -R root %s if it is yours, or point STAGING at a directory root owns", dir)
	}
	if owner == 0 {
		return "run this command as root, or point STAGING at a directory you own"
	}
	return "remove it if it is not yours, or point STAGING at a directory you own"
}
