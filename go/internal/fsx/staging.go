package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// SecureStaging makes a staging root, and the subdirectories the caller is
// about to write into, safe to hold plaintext.
//
// The root is secured before the subdirectories, and the order matters: a
// symlinked root would make every check below it a check on the link's target
// rather than on anything in the operator's staging tree.
//
// Each directory gets [SecureDir]'s three rules. The caller wraps the error
// with its own prefix ("backup: ", "restore: ").
func SecureStaging(root string, subs ...string) error {
	if root == "" {
		return errors.New("no STAGING directory configured")
	}
	dirs := append([]string{root}, subs...)
	for _, d := range dirs {
		if err := SecureDir(d); err != nil {
			return err
		}
	}
	return nil
}

// SecureDir applies brb's staging rules to one directory, failing rather than
// warning when it cannot.
//
// This is the single implementation of "secure the staging directory" for the
// whole program. It used to exist once per caller, and every rewrite lost a
// piece of it: the reader had all three rules below, the writer had only the
// chmod. Both sides need all three. The writer's tree holds unencrypted
// squashfs images until each one is encrypted and verified, and the resume
// state that decides which files a continued run is allowed to skip; the
// reader's holds every image it decrypts. Neither is a place to be relaxed
// about who else can write.
//
// It fails rather than warns for one reason: the README's default STAGING
// lives under /var/tmp, which is world-writable, and every local account can
// create things there ahead of the operator.
//
// The three rules, in this order:
//
//   - The directory must not be a symlink. Images are written into these
//     directories by name, and a symlink planted at "img", "enc" or "restore"
//     before the run sends every one of them wherever the planter chose.
//   - It is created if missing, and its mode is forced to 0700 whether or not
//     it was just created. Mkdir applies the mode only to what it makes, so a
//     directory the operator created by hand keeps whatever the umask gave it.
//     A chmod that fails is fatal: for as long as a run lasts this tree holds
//     the archive in the clear, and "could not lock the door" is not something
//     to proceed past.
//   - It must belong to the user running this process. A directory another
//     account owns is one that account can rename, replace or fill at will,
//     between any check made here and any write made later.
//
// The ownership rule is not redundant with the chmod rule, and it is the one
// that matters most to the writer: chmod fails on a foreign directory only for
// an unprivileged process. Under root it succeeds on anybody's directory, so
// the chmod check cannot fire — and root is exactly what README.md recommends
// for a full capture, because only root can read every file and record real
// ownership. Without this rule the writer's guard was vacuous in precisely its
// recommended configuration.
//
// WHY THE RULES ARE APPLIED THROUGH A DESCRIPTOR. Checking a path and then
// operating on the same path is two lookups, and in a world-writable /var/tmp
// somebody else chooses what the second one finds. This function used to do
// five: an Lstat, then MkdirAll, Chmod and Stat by name. A local user looping
// `ln -s /etc <STAGING>` against a run that arrives just after the Lstat got
// MkdirAll to accept the link's target as an existing directory, chmod 0700 to
// land on /etc, and the ownership rule to pass — because under root the target
// is root's too. Every image of the run then went to the planter's directory.
// So the path is used exactly once, by [openDirNoFollow], and the mode, the
// type and the owner are all settled on the descriptor it returns with fchmod
// and fstat. Those refer to one inode that nothing can substitute; the
// ownership rule is then a rule about the directory this run is really using
// rather than about whatever the name resolved to last.
func SecureDir(dir string) error {
	// Clean first, and specifically to strip a trailing slash: lstat(2) resolves
	// "link/" — the trailing slash is a statement that the path is a directory,
	// so the kernel follows the link to check — and os.Lstat inherits that. A
	// STAGING written as /var/tmp/brb/ would therefore walk straight past the
	// refusal below and report on the link's TARGET, which is the one thing this
	// function exists to refuse. Everything after this point is about the link
	// itself, so the whole check depends on this line.
	dir = filepath.Clean(dir)
	// This Lstat is for the message, not for the guarantee: it can name the
	// link and where it points, which ELOOP from the open below cannot. The
	// open is what actually refuses.
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return symlinkRefusal(dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if testHookAfterLstat != nil {
		testHookAfterLstat()
	}
	// MkdirAll for the parents and Mkdir for the directory itself, rather than
	// MkdirAll for the lot: MkdirAll starts with a Stat and returns nil when
	// the path already resolves to a directory, which is how a symlink planted
	// after the Lstat used to be adopted silently. mkdir(2) does not follow a
	// symlink at the final component — it fails with EEXIST — so the only
	// judgement made about that component is the open below.
	if parent := filepath.Dir(dir); parent != dir {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	d, err := openDirNoFollow(dir)
	if err != nil {
		// The open has already refused; all that is left is to say which of the
		// two things went wrong. Linux answers O_DIRECTORY|O_NOFOLLOW over a
		// symlink with ENOTDIR rather than ELOOP — the same errno a plain file
		// in the way gives — so the two are told apart by an Lstat here. This
		// one is safe to trust in a way the one at the top of the function is
		// not: nothing is done with its answer except choose a sentence.
		switch {
		case isSymlinkLoop(err):
			return symlinkRefusal(dir)
		case isNotDir(err):
			if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
				return symlinkRefusal(dir)
			}
			return fmt.Errorf("%s exists and is not a directory — remove it, or point STAGING elsewhere", dir)
		}
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	defer d.Close()
	// fchmod and fstat on the descriptor: the inode the checks below describe
	// is the inode the run will write into, whatever happens to the name.
	if err := d.Chmod(0o700); err != nil {
		return fmt.Errorf("securing %s, which will hold plaintext: %w — "+
			"fix its permissions, or point STAGING at a directory you own", dir, err)
	}
	fi, err := d.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if !fi.IsDir() {
		// O_DIRECTORY already refused this; kept so the rule is not silently
		// resting on a flag one platform might ignore.
		return fmt.Errorf("%s exists and is not a directory — remove it, or point STAGING elsewhere", dir)
	}
	owner, ok := FileOwner(fi)
	if ok && owner != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not by this process (uid %d); "+
			"whoever owns it can replace anything under it while plaintext is being written there — "+
			"%s", dir, owner, os.Geteuid(), ownershipAdvice(dir, owner))
	}
	return nil
}

// testHookAfterLstat runs in the gap between [SecureDir]'s symlink check and
// the create-and-open that follows it. It is nil in every build but the test
// that plants a symlink into that gap: the whole claim of the descriptor above
// is that the gap is harmless, and an interleaving is not something a test can
// schedule from the outside.
var testHookAfterLstat func()

// symlinkRefusal is the one wording for "STAGING is a symlink", used both by
// the Lstat that sees it up front and by the open that refuses it when it was
// planted a moment later. Readlink is best effort: a link that has already
// been swapped for something else still gets refused, just without the arrow.
func symlinkRefusal(dir string) error {
	target, _ := os.Readlink(dir)
	return fmt.Errorf("%s is a symlink (-> %s); a staging directory must be a real directory, "+
		"because plaintext images are written into it by name and would follow the link — "+
		"remove the link, or point STAGING at the directory itself", dir, target)
}

// ownershipAdvice says what to do about a staging directory somebody else
// owns. The advice differs by who "somebody else" is, because the three cases
// have three different fixes and only one of them is suspicious.
func ownershipAdvice(dir string, owner int) string {
	if os.Geteuid() == 0 {
		// Running the backup under sudo is normal and supported — the README
		// recommends it — and the commonest way to arrive here is entirely
		// innocent: the operator ran brb as themselves once, which created
		// STAGING under their own uid, and is now running it under sudo.
		// SUDO_UID identifies that account, so say so by name.
		//
		// It is still refused, and deliberately. Root is the worst case for
		// this, not the safe one: a root run reads files no ordinary user can,
		// and writes them in the clear into a directory the invoking account
		// can still rename or replace underneath it. Handing the tree to root
		// is one command; guessing that the operator meant it is not
		// recoverable.
		if sudo := os.Getenv("SUDO_UID"); sudo != "" {
			if uid, err := strconv.Atoi(sudo); err == nil && uid == owner {
				return fmt.Sprintf("that is the account you ran sudo from, and a root run must not write "+
					"plaintext where it can be replaced from below: chown -R root %s to hand the tree to "+
					"this run, or point STAGING at a directory root owns", dir)
			}
		}
		return fmt.Sprintf("chown -R root %s if it is yours, or point STAGING at a directory root owns", dir)
	}
	if owner == 0 {
		return "run this command as root, or point STAGING at a directory you own"
	}
	return "remove it if it is not yours, or point STAGING at a directory you own"
}

// CreateFresh creates path for writing, removing whatever was there first.
//
// It is the one way brb opens a file it is about to fill in the staging area —
// a .part, a log — and it never uses O_TRUNC. O_TRUNC opens straight through
// whatever is already at the path, and in a staging directory whose default
// lives under /var/tmp "whatever is already there" can be a symlink another
// local user planted: opening through it would stream a plaintext image, or a
// ciphertext, into a file of that user's choosing with this process's
// privileges. O_EXCL refuses to open through a symlink at all, dangling or
// not; the kernel guarantees that, with no window between a check and the open.
//
// The stale-file case O_TRUNC used to cover silently — a run killed mid-write,
// then repeated — is handled by removing the leftover first. Removing a path
// removes the link itself and never what it points to, so that step is safe on
// a symlink too, and anything planted between the Remove and the open still
// meets O_EXCL.
func CreateFresh(path string, mode os.FileMode) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("removing the stale %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	return f, nil
}

// OpenAppend opens path for appending, creating it if it does not exist, and
// refuses to open through a symlink.
//
// It exists for the one staging file that genuinely accumulates rather than
// being written once — the index, to which each finished disc appends its file
// list so an interrupted run resumes with an index covering every disc it
// completed. [CreateFresh]'s remove-then-O_EXCL would throw that history away,
// so the protection here is O_NOFOLLOW instead: it refuses when the final
// component is a symlink, with the same no-check-then-open window as O_EXCL.
// The Lstat below exists only to turn the kernel's ELOOP into a sentence an
// operator can act on; the flag, not the check, is what makes the guarantee.
func OpenAppend(path string, mode os.FileMode) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		return nil, fmt.Errorf("%s is a symlink (-> %s); it is written by this run and must be a real "+
			"file — remove the link, or point STAGING at a directory you own", path, target)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|oNoFollow, mode)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return f, nil
}
