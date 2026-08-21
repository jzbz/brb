package restore

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// restorableXattrs returns the -xattrs-include regex matching the extended
// attributes the current user is actually able to write.
//
// Only root may write the security.* and system.* namespaces, and on any
// SELinux system (Fedora, RHEL, CentOS) every file carries a security.selinux
// label. Asking unsquashfs to restore those as an ordinary user makes it print
// a write_xattr error and exit non-zero even though it extracted every file,
// directory, symlink and hardlink correctly — which would fail the restore of
// a perfectly good archive. Restricting an unprivileged run to the user.*
// namespace keeps the xattrs that can be restored and drops only the ones that
// were never restorable without root in the first place.
func restorableXattrs() string {
	if os.Geteuid() == 0 {
		return "" // root: restore every namespace
	}
	return "^user."
}

// RestoreOptions selects what Restore extracts and what it leaves behind.
type RestoreOptions struct {
	// Dest is the directory to extract into; it is created if missing.
	Dest string
	// Only limits extraction to these paths inside the archive. Empty extracts
	// everything.
	Only []string
	// Disc restores just one disc; 0 restores every ingested disc.
	Disc int
	// KeepImages keeps each decrypted image in the staging restore directory
	// instead of deleting it once it has been extracted.
	KeepImages bool
}

// Restore repairs, decrypts and extracts ingested images into a destination
// directory.
//
// Each plaintext image is deleted as soon as it has been extracted, unless
// KeepImages says otherwise. brb.sh keeps every one of them, so restoring a
// 500 GB archive needs 500 GB of staging on top of the destination and stops
// halfway through with no space left.
//
// Two things are announced rather than assumed, because both are ways a restore
// can look like a success while handing back less than the operator asked for:
//
//   - A set with images missing. Every disc carries the whole directory
//     skeleton, so restoring two discs of a three-disc set produces a tree with
//     the right shape and the files silently absent. The disc count in
//     MANIFEST.txt is compared with what is in staging, the missing numbers are
//     named, and an interactive run has to confirm before it proceeds. Passing
//     Disc skips the check: restoring one named disc is deliberate.
//   - An Only path that is on none of the images. unsquashfs exits 0 having
//     created nothing, so each image is asked whether it holds the path before
//     it is extracted, and a path found nowhere fails the restore.
//
// Only is resolved through the encrypted index before any image is touched, so
// retrieving one file from a fifty-disc set prepares one disc and not fifty —
// see [Options.narrowByIndex]. A set with no index still restores the old way,
// with a warning.
func Restore(ctx context.Context, o Options, ro RestoreOptions) error {
	if err := o.check(); err != nil {
		return err
	}
	unlock, err := o.lockStaging()
	if err != nil {
		return err
	}
	defer unlock()
	if ro.Dest == "" {
		return errors.New("restore: no destination given")
	}
	// Cleaned before any check looks at it. A destination spelled with a
	// trailing slash — "dest/" — is resolved by the kernel before Lstat ever
	// sees it, so refuseSymlinkedDirs handed "dest/" walks the link's TARGET,
	// finds nothing to refuse, and the whole archive lands outside the
	// destination. brb.sh strips the slash first; so does this.
	ro.Dest = filepath.Clean(ro.Dest)
	if err := o.Tools.Require(tools.Unsquashfs); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	for i, p := range ro.Only {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("restore: --only path %d is empty", i+1)
		}
		// "." names the archive root, and so does "./", "/" or anything that
		// cleans to nothing. covers() treats that as matching every entry, so
		// the pre-check says every disc holds it, and unsquashfs handed "." as
		// an extraction operand extracts nothing at all — leaving a run that
		// reports success having restored not one file. Whoever typed it meant
		// the whole tree, and that is what a restore without --only does.
		if isArchiveRoot(p) {
			return fmt.Errorf("restore --only %s: that is the whole tree — run restore without --only", p)
		}
	}
	// The restore directory is about to receive every disc's plaintext, and
	// the enc directory is what the images, their hashes and a public set's
	// staged key are read from; both have to be real directories of ours
	// before anything is read from, reaped from or written into them.
	// PrepareImage checks again per image, cheaply.
	if err := o.secureStaging(o.dirs().Enc, o.dirs().Restore); err != nil {
		return err
	}
	// Fail on a missing key now rather than after hours of hashing, and hold on
	// to what was loaded: this command decrypts the index and then one image
	// per disc, and an identity that has to be unlocked must be unlocked once.
	ids, err := o.identities()
	if err != nil {
		return err
	}
	o.ids = ids
	if err := os.MkdirAll(ro.Dest, 0o755); err != nil {
		return fmt.Errorf("restore: creating %s: %w", ro.Dest, err)
	}
	if os.Geteuid() != 0 {
		o.UI.Warn("not running as root: ownership will not be restored")
	}
	// A hard kill mid-decrypt leaves an image-sized .part in the restore
	// directory that nothing ever reuses or lists; brb.sh reaps its own at
	// ingest, and this is the matching moment on the restore side.
	o.reapPartFiles(o.dirs().Restore)

	// unsquashfs is run with -f for every image, because discs 2..N extract
	// into a tree disc 1 already populated. Into a live $HOME that silently
	// overwrites current files with the backup's versions, mode and mtime
	// included — so a non-empty destination needs a yes first, exactly as
	// brb.sh requires one. It applies with --disc and --only too: -f overwrites
	// there just the same.
	if err := o.confirmNonEmptyDest(ro.Dest); err != nil {
		return err
	}
	// And -f follows a symlink that is already in the destination: a planted
	// "sub -> /anywhere" makes unsquashfs write the archive's sub/* files
	// outside the destination, at any depth, with this process's privileges —
	// which the README recommends be root's. That is a hard refusal, not a
	// question, so --yes cannot wave it through.
	if err := refuseSymlinkedDirs(ro.Dest); err != nil {
		return err
	}

	imgs, err := o.selectImages(ro.Disc)
	if err != nil {
		return err
	}

	// HL-1: the shape of the restored tree cannot reveal a missing disc, so the
	// gap is reported here or not at all. Only for a whole-set restore —
	// --disc N asks for one disc and gets one disc.
	var status setStatus
	if ro.Disc == 0 {
		status = o.checkComplete(imgs)
		if !status.Complete() {
			yes, err := o.UI.Confirm("Restore the partial set anyway?")
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}
			if !yes {
				return partialAbortedError(status)
			}
		}
	}

	// HL-3. --only used to prepare every image in the set — par2-verifying and
	// decrypting the lot, and writing every disc's plaintext into the staging
	// restore directory — and then filter the extraction. The index already
	// knows which disc holds the path, so ask it and prepare only those. The
	// completeness check above deliberately runs first, on the whole ingested
	// set, exactly as brb.sh orders it: what is missing from the set is a fact
	// about the set, not about the paths being asked for.
	sel, narrowed := imgs, false
	if len(ro.Only) > 0 {
		sel, narrowed, err = o.narrowByIndex(ctx, imgs, ro)
		if err != nil {
			return err
		}
	}

	o.UI.Log("restoring %d image(s) to %s", len(sel), ro.Dest)

	// Which of the requested paths have been extracted from some image. A path
	// that is on none of them is the failure IDX-6 is about, and it has to be
	// tracked per path: reporting "restore complete" after recovering two of
	// the three files someone asked for is the same lie in miniature.
	found := make(map[string]bool, len(ro.Only))
	extracted := 0

	for _, im := range sel {
		if err := ctx.Err(); err != nil {
			return err
		}
		o.UI.Step("preparing %s", filepath.Base(im.Path))
		plain, err := PrepareImage(ctx, o, im.Path)
		if err != nil {
			return err
		}
		name := filepath.Base(plain)

		// With --only, ask the image what it holds before extracting. Skipping
		// the discs that hold none of the requested paths is also what lets a
		// genuine unsquashfs failure below stay a failure.
		here := ro.Only
		if len(ro.Only) > 0 {
			here, err = o.pathsPresent(ctx, plain, ro.Only)
			if err != nil {
				return err
			}
			if len(here) == 0 {
				o.UI.Step("%s is not on %s", quoteVisible(ro.Only), name)
				o.discardImage(plain, ro.KeepImages)
				continue
			}
		}

		// The destination may already hold a symlink at a path this image
		// holds as a directory — planted, or an honest one under a live $HOME
		// — and unsquashfs -f applies the archive directory's mode, owner and
		// times through it to whatever it points at. Refused per image,
		// against this image's own directory list.
		if err := o.refuseSymlinksAtImageDirs(ctx, plain, ro.Dest, here); err != nil {
			return err
		}

		err = o.extractImage(ctx, plain, ro.Dest, here)
		switch {
		case err == nil:
			extracted++
			if len(ro.Only) > 0 {
				// The listing said yes, but unsquashfs exits 0 whether or not it
				// created anything, so believe only the destination: a path that
				// is not there was not extracted, whatever the listing claimed.
				// This is what catches the names a line-based listing cannot
				// carry faithfully, and any future silent unsquashfs no-match.
				var present []string
				for _, p := range here {
					if pathRestored(ro.Dest, p) {
						found[p] = true
						present = append(present, p)
					} else {
						o.UI.Warn("%s was listed on %s but was not extracted", quoteVisible([]string{p}), name)
					}
				}
				if len(present) > 0 {
					o.UI.OK("%s extracted from %s", quoteVisible(present), name)
				}
			} else {
				o.UI.OK("%s extracted", name)
			}
		case ctx.Err() != nil:
			return err
		default:
			return fmt.Errorf("restore: extracting %s: %w", name, err)
		}
		o.discardImage(plain, ro.KeepImages)
	}

	if len(ro.Only) > 0 {
		var absent []string
		for _, p := range ro.Only {
			if !found[p] {
				absent = append(absent, p)
			}
		}
		if len(absent) > 0 {
			// Say what was actually searched. With --disc N only that one disc
			// was looked at, and "not on any of the 1 ingested disc(s)" reads
			// as though the whole set had been checked — which would send an
			// operator away believing the file is gone.
			where := fmt.Sprintf("any of the %d ingested disc(s)", len(imgs))
			switch {
			case ro.Disc > 0:
				where = fmt.Sprintf("disc %d, the only disc searched", ro.Disc)
			case narrowed:
				// Say which discs were opened. After the index has narrowed the
				// run, "any of the 3 ingested disc(s)" would claim a search
				// that did not happen, and would send an operator away
				// believing the file is gone from the whole set.
				where = fmt.Sprintf("disc(s) %s, the only ones the index named for it", discList(discNumbers(sel)))
			}
			return fmt.Errorf("restore: %s was not found on %s and nothing was restored for it — check 'brb index %s'",
				quoteAll(absent), where, absent[0])
		}
		o.UI.OK("extracted %d requested path(s) from %d of %d image(s)", len(ro.Only), extracted, len(sel))
	} else if extracted == 0 {
		return fmt.Errorf("restore: none of the %d image(s) could be extracted", len(sel))
	}

	o.reportOutcome(ro, status)
	return nil
}

// reportOutcome prints the closing summary. A restore of a set with discs
// missing must not end on a line that reads like an ordinary clean success: the
// tree it produced has the right shape and the wrong contents, and this is the
// last chance to say so.
func (o Options) reportOutcome(ro RestoreOptions, st setStatus) {
	if !st.Complete() {
		o.UI.Warn("this restore was PARTIAL: disc(s) %s of %d were not in the staging area", st.missingList(), st.Want)
		o.UI.Warn("files that lived only on those discs are NOT in %s, and the empty directories left behind do not show it", ro.Dest)
		o.UI.Warn("ingest the missing disc(s) and run the same restore again to fill them in")
		o.UI.OK("partial restore complete: %s", ro.Dest)
	} else {
		o.UI.OK("restore complete: %s", ro.Dest)
	}
	if ro.KeepImages {
		o.UI.Step("the decrypted images are still in %s — plaintext; remove them when you are done", o.dirs().Restore)
	}
}

// quoteAll renders paths for an error message, quoted the way brb.sh quotes the
// --only path so that a name with spaces in it is still readable.
func quoteAll(paths []string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = "'" + p + "'"
	}
	return strings.Join(q, ", ")
}

// quoteVisible renders archive paths for the UI's own status lines, with
// strconv.Quote so that a name carrying terminal control bytes — which an
// attacker who can plant one file in the backed-up tree chooses freely — is
// shown rather than executed by the operator's terminal. Status lines are
// diagnostics, not data, so unlike index output they are always escaped.
func quoteVisible(paths []string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = strconv.Quote(p)
	}
	return strings.Join(q, ", ")
}

// reapPartFiles removes stale ".part" files from a staging directory. Every
// writer cleans up its own on failure, but a kill -9 or power loss skips the
// deferred removal, and nothing ever resumes from a bare .part — brb.sh
// removes them at the same moments for the same staging layout. Failures are
// only warnings: dead weight on disk must not stop a restore.
func (o Options) reapPartFiles(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), partExt) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil {
			o.UI.Warn("could not remove the stale %s: %v", p, err)
			continue
		}
		o.UI.Step("removed the stale %s left by an interrupted run", p)
	}
}

// confirmNonEmptyDest mirrors brb.sh's guard on a destination that already has
// contents, message for message: warn what -f will do, show what is there, and
// require a yes. Under --yes the confirmation auto-answers, as it does in bash.
func (o Options) confirmNonEmptyDest(dest string) error {
	ents, err := os.ReadDir(dest)
	if err != nil {
		return fmt.Errorf("restore: reading %s: %w", dest, err)
	}
	if len(ents) == 0 {
		return nil
	}
	names := make([]string, 0, 5)
	for _, e := range ents[:min(len(ents), 5)] {
		names = append(names, e.Name())
	}
	o.UI.Warn("%s is not empty. unsquashfs -f will OVERWRITE existing files with the backup versions.", dest)
	o.UI.Warn("  existing entries: %s ...", strings.Join(names, " "))
	yes, err := o.UI.Confirm(fmt.Sprintf("Overwrite the current contents of %s with this backup?", dest))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if !yes {
		return errors.New("restore: aborted — restore into an empty directory and merge by hand")
	}
	return nil
}

// refuseSymlinkedDirs fails when anything already under dest is a symlink that
// resolves to a directory. unsquashfs -f traverses such a link — at any depth,
// not just the top level — and writes the archive's files through it, outside
// the destination. A symlink to a file, or a dangling one, is left alone here:
// at a path the archive holds as a file unsquashfs unlinks and replaces it as
// an entry, which is safe, and at a path the archive holds as a directory it
// is caught per image by refuseSymlinksAtImageDirs, which knows which paths
// those are. Within one run this needs checking only before the first image:
// the skeleton on every disc makes a path either a directory or a leaf across
// the whole set, so nothing a disc extracts turns into a traversal for the
// next one.
func refuseSymlinkedDirs(dest string) error {
	var bad []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		st, serr := os.Stat(p)
		if serr != nil || !st.IsDir() {
			return nil
		}
		target, _ := os.Readlink(p)
		bad = append(bad, fmt.Sprintf("%s -> %s", p, target))
		if len(bad) >= 5 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("restore: checking %s for symlinks: %w", dest, err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("restore: %s contains symlink(s) to directories (%s); unsquashfs -f would follow them and "+
			"write the backup's files OUTSIDE the destination — remove them, or restore into an empty directory and merge by hand",
			dest, strings.Join(bad, ", "))
	}
	return nil
}

// isArchiveRoot reports whether a --only path names the archive root rather
// than something in it: ".", "./", "/", "" and anything that cleans to one of
// those. covers() treats such a path as matching every entry, so it passes the
// index and the per-image pre-check, and unsquashfs handed "." as an extraction
// operand extracts nothing — the combination that once reported success with
// an empty destination.
func isArchiveRoot(p string) bool {
	return path.Clean("/"+strings.TrimSpace(p)) == "/"
}

// unsquashfsLogName is the file the restore staging directory keeps
// unsquashfs's output in while an image is extracted, and afterwards when it
// exited non-fatally: "unsquashfs.disc03.squashfs.log", brb.sh's name for the
// same file, so an operator told to look at one finds it under either reader.
func unsquashfsLogName(image string) string {
	return "unsquashfs." + filepath.Base(image) + ".log"
}

// extractImage runs unsquashfs over one decrypted image, keeping its output in
// a log file under the restore staging directory as well as on the terminal.
//
// unsquashfs distinguishes two failures. Exit 1 means it aborted; exit 2 means
// it extracted everything and could not restore some attribute — an owner as
// non-root, an xattr on NFS, CIFS or exFAT, a mode a filesystem cannot hold —
// which is routine and leaves every file present and correct. This used to
// treat both as fatal, throwing away a multi-disc restore over the second, and
// brb.sh does not: it warns, keeps the log, and goes on to the next disc. So
// does this. On a clean exit the log is removed; on exit 2 it is kept and the
// warning names it; on anything else it is kept and the error names it, since
// unsquashfs's own words are the diagnosis.
func (o Options) extractImage(ctx context.Context, image, dest string, only []string) error {
	name := filepath.Base(image)
	logPath := filepath.Join(o.dirs().Restore, unsquashfsLogName(image))
	lf, err := fsx.CreateFresh(logPath, 0o600)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	steps := o.logWriter()
	err = o.Tools.Unsquashfs(ctx, tools.UnsqOptions{
		Image:         image,
		Dest:          dest,
		Only:          only,
		Force:         true,
		Xattrs:        true,
		XattrsInclude: restorableXattrs(),
		Log:           io.MultiWriter(lf, steps),
	})
	steps.Close()
	if cerr := lf.Close(); cerr != nil && err == nil {
		o.UI.Warn("could not finish writing %s: %v", logPath, cerr)
	}
	switch {
	case err == nil:
		os.Remove(logPath)
		return nil
	case ctx.Err() != nil:
		os.Remove(logPath)
		return err
	case tools.ExitCode(err) == 2:
		o.UI.Warn("unsquashfs reported non-fatal errors on %s — see %s", name, logPath)
		return nil
	default:
		return fmt.Errorf("%w (its output is in %s)", err, logPath)
	}
}

// listedDir turns one line of an "unsquashfs -ll" listing into an
// archive-relative directory path, or ok=false for any other line — a file, a
// symlink, the archive root, or the chatter some versions print first.
//
// The long listing's line is "<mode> <user>/<group> <size> <date> <time>
// <path>", with the size right-aligned in a padded field and exactly one space
// before the path — so the path is everything after the fifth field's single
// trailing space, whatever it contains. Nothing is trimmed off the end: a
// trailing '\r' is part of a name. A name holding '\n' cannot survive a
// line-based listing; the second half of it will fail to parse and be skipped.
func listedDir(line string) (string, bool) {
	if !strings.HasPrefix(line, "d") {
		return "", false
	}
	rest := line
	for i := 0; i < 5; i++ {
		rest = strings.TrimLeft(rest, " ")
		j := strings.IndexByte(rest, ' ')
		if j < 0 {
			return "", false
		}
		rest = rest[j:]
	}
	if len(rest) < 2 {
		return "", false
	}
	return archivePath(rest[1:])
}

// extractionTouches reports whether unsquashfs, asked for only these paths,
// will create or re-attribute the archive directory dir: it does when dir is
// one of them, is under one of them, or is an ancestor it has to pass through
// — unsquashfs sets attributes on every directory it descends into, not only
// on the ones named. An empty only extracts everything and touches every one.
func extractionTouches(only []string, dir string) bool {
	if len(only) == 0 {
		return true
	}
	for _, p := range only {
		if covers(p, dir) || strings.HasPrefix(strings.Trim(p, "/"), dir+"/") {
			return true
		}
	}
	return false
}

// refuseSymlinksAtImageDirs fails when the destination already holds a symlink
// — to anything, or to nothing — at a path this image holds as a directory
// and is about to extract.
//
// refuseSymlinkedDirs catches a link that resolves to a directory, which is the
// traversal case. This is the other case: a link that resolves to a file, or
// dangles. unsquashfs -f finds the path taken, carries on, and at the end of
// the directory applies the archive's mode, owner, mtime and xattrs to the
// path — through the link, onto whatever it points at. Reproduced: a planted
// "Documents -> /etc/shadow" under a restore run as root left /etc/shadow
// world-readable with the backup's timestamp. Where the archive directory has
// children unsquashfs aborts instead, which is safe but no better a message
// than this one.
//
// The image itself is asked which paths are directories, via its long listing,
// because a directory that is empty in the archive — the empty skeleton
// directories every disc carries, most of all — is in no index and casts no
// shadow in the destination until it is created. It is checked per image
// rather than once, and only against the archive's directories: the symlinks
// disc 1 itself extracted, at paths the archive holds as symlinks or files,
// are the very thing that must not stop disc 2. With --only, this guard checks
// only the directories extraction will actually touch, so the links to files
// and the dangling links a live $HOME accumulates elsewhere do not block
// fetching one file back into it.
//
// The narrowing stops there, and reading it as a statement about restore would
// be wrong: refuseSymlinkedDirs runs once before the first image, takes no
// only argument, and walks the whole destination, so a link resolving to a
// DIRECTORY anywhere under dest refuses the run whatever --only says. That is
// deliberate and ledgered — README.md's Limitations: "A destination holding a
// symlink to a directory is refused outright, --yes or not" — because
// unsquashfs -f follows such a link and writes outside the destination, and
// narrowing it safely would mean covering every ancestor of every --only path
// as well as everything under it. TestRestoreOnlyDoesNotExemptADirectorySymlink
// pins it.
func (o Options) refuseSymlinksAtImageDirs(ctx context.Context, image, dest string, only []string) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := o.Tools.UnsquashfsList(ctx, image, pw)
		pw.CloseWithError(err)
		done <- err
	}()

	var bad []string
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64<<10), maxIndexLine)
	sc.Split(scanLinesKeepCR)
	for i := 0; sc.Scan(); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				pr.CloseWithError(err)
				<-done
				return err
			}
		}
		rel, ok := listedDir(sc.Text())
		if !ok || !extractionTouches(only, rel) {
			continue
		}
		p := filepath.Join(dest, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		target, _ := os.Readlink(p)
		bad = append(bad, fmt.Sprintf("%s -> %s", p, target))
		if len(bad) >= 5 {
			break
		}
	}
	// Drain and close so the child is never left writing into a full pipe, then
	// take its exit status: a listing that failed has vouched for nothing.
	_, _ = io.Copy(io.Discard, pr)
	pr.Close()
	if err := <-done; err != nil {
		return fmt.Errorf("restore: listing the directories of %s: %w", filepath.Base(image), err)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("restore: reading the listing of %s: %w", filepath.Base(image), err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("restore: %s holds symlink(s) where %s has directories (%s); unsquashfs -f would apply the "+
			"backup's directory mode, owner and times THROUGH them to whatever they point at — "+
			"remove them, or restore into an empty directory and merge by hand",
			dest, filepath.Base(image), strings.Join(bad, ", "))
	}
	return nil
}

// pathRestored reports whether a requested archive path now exists under dest.
// Lstat, not Stat: a restored symlink is a restored path, wherever it points.
func pathRestored(dest, want string) bool {
	want = strings.Trim(want, "/")
	if want == "" || want == "." {
		want = "."
	}
	_, err := os.Lstat(filepath.Join(dest, want))
	return err == nil
}

// squashfsRoot is the directory unsquashfs's listings are rooted at. It is the
// default extraction directory name and appears whatever -d would have said,
// because a listing never extracts anything.
const squashfsRoot = "squashfs-root"

// pathsPresent returns, in the order given, those of want that the image
// actually holds — as an exact entry or as a directory with entries under it.
//
// unsquashfs is asked rather than the extracted tree, for two reasons: a
// destination that already had the file would answer yes without the archive
// having contributed anything, and the answer is needed before the extraction
// rather than after it.
//
// The listing is streamed. A full archive listing of a 22 GB disc is large
// enough that reading it into memory to answer a yes/no question would be a
// poor trade.
func (o Options) pathsPresent(ctx context.Context, image string, want []string) ([]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	hit := make(map[string]bool, len(want))
	// A name containing '\n' (or a bare '\r') is one a line-based listing
	// cannot carry faithfully, so the pre-check would answer "not here" about a
	// file that is. Those paths skip it: -no-wildcards makes the extraction
	// operand literal, so unsquashfs is handed the real name, and the caller's
	// post-extraction check judges whether it appeared.
	for _, p := range want {
		if strings.ContainsAny(p, "\n\r") {
			hit[p] = true
		}
	}
	if len(hit) == len(want) {
		return append([]string(nil), want...), nil
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := o.Tools.UnsquashfsNames(ctx, image, pw)
		pw.CloseWithError(err)
		done <- err
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64<<10), maxIndexLine)
	sc.Split(scanLinesKeepCR)
	for i := 0; sc.Scan(); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				pr.CloseWithError(err)
				<-done
				return nil, err
			}
		}
		entry, ok := archivePath(sc.Text())
		if !ok {
			continue
		}
		for _, p := range want {
			if !hit[p] && covers(p, entry) {
				hit[p] = true
			}
		}
		if len(hit) == len(want) {
			break
		}
	}
	// Drain and close so the child is never left writing into a full pipe, then
	// take its exit status: a listing that failed says nothing about the image.
	_, _ = io.Copy(io.Discard, pr)
	pr.Close()
	if err := <-done; err != nil {
		return nil, fmt.Errorf("restore: listing %s: %w", filepath.Base(image), err)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("restore: reading the listing of %s: %w", filepath.Base(image), err)
	}

	out := make([]string, 0, len(hit))
	for _, p := range want {
		if hit[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

// archivePath turns one line of an unsquashfs listing into an archive-relative
// path. The archive root itself, and anything not under it, yield ok=false.
//
// Nothing is trimmed off the end: unsquashfs on Linux terminates listing lines
// with '\n' alone, so a '\r' here is part of a real file's name, and stripping
// it once made "restore --only" report success while extracting nothing.
func archivePath(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, squashfsRoot+"/")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// scanLinesKeepCR is bufio.ScanLines without its dropCR step: tokens end at
// '\n' and keep a preceding '\r'. Every reader in this package splits records
// with it, because in both streams it reads — the index, where '\n' is escaped
// precisely so that records are unambiguously newline-terminated, and
// unsquashfs listings, which never emit CRLF — a '\r' before the terminator is
// a byte of a file's name, not line-ending decoration.
func scanLinesKeepCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// covers reports whether the archive entry satisfies a requested path: the
// entry itself, or something inside it when the request names a directory.
func covers(want, entry string) bool {
	want = strings.Trim(want, "/")
	if want == "" || want == "." {
		return true
	}
	return entry == want || strings.HasPrefix(entry, want+"/")
}

// discardImage removes a decrypted image once it has served its purpose. A
// failure to remove it is a warning, not an error: the data is already restored
// and the operator can delete the staging directory.
func (o Options) discardImage(plain string, keep bool) {
	if keep {
		return
	}
	if err := os.Remove(plain); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.UI.Warn("could not remove the decrypted %s: %v", plain, err)
		return
	}
	o.UI.Step("removed the decrypted %s", filepath.Base(plain))
}

// List writes the contents of one disc's image to w, in unsquashfs's long
// listing format. The image is decrypted first and left in the staging restore
// directory, so listing several discs in a row does not decrypt the same image
// twice.
func List(ctx context.Context, o Options, n int, w io.Writer) error {
	if err := o.check(); err != nil {
		return err
	}
	unlock, err := o.lockStaging()
	if err != nil {
		return err
	}
	defer unlock()
	if w == nil {
		return errors.New("restore: no output writer given")
	}
	if n <= 0 {
		return fmt.Errorf("restore: disc number must be 1 or more, got %d", n)
	}
	if err := o.Tools.Require(tools.Unsquashfs); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	imgs, err := o.selectImages(n)
	if err != nil {
		return err
	}
	plain, err := PrepareImage(ctx, o, imgs[0].Path)
	if err != nil {
		return err
	}
	// The listing carries attacker-chosen filenames; on a terminal their
	// control bytes are escaped rather than executed, and piped output is
	// byte-faithful — the same rule the index follows.
	lw := w
	var esc *escapingWriter
	if ui.IsTerminal(w) {
		esc = newEscapingWriter(w)
		lw = esc
	}
	listErr := o.Tools.UnsquashfsList(ctx, plain, lw)
	// Closed whatever happened, and before the error is reported: the escaped
	// stream is buffered, so a listing that died half way through would
	// otherwise print nothing at all rather than the part that was read.
	if esc != nil {
		if err := esc.Close(); err != nil && listErr == nil {
			listErr = err
		}
	}
	if listErr != nil {
		return fmt.Errorf("restore: listing %s: %w", filepath.Base(plain), listErr)
	}
	o.UI.Step("the decrypted %s is still in %s — plaintext; remove it when you are done",
		filepath.Base(plain), o.dirs().Restore)
	return nil
}

// Index writes the encrypted "which disc holds which file" map to w, optionally
// filtered. Lines are "<disc number>\t<path>", exactly as brb.sh records them.
//
// pattern is matched case-insensitively as a plain substring, not as a regular
// expression: brb.sh pipes the index through grep, where a path containing a
// dot or a bracket quietly means something other than what the operator typed.
//
// It reads nothing but the staging tree, and it opens the way every other
// command in this package does, for two reasons that are not obvious for a
// command that only prints.
//
// The lock: checkIndexIntact reads the recorded hash and then, separately,
// hashes the index. A backup replaces those same two files one after the other
// at the end of its run (backup: the index, then its sidecar). An unlocked
// index run that lands between them compares the old hash against the new
// index and tells the operator their archive has rotted and to run par2 repair
// — on a staging tree a backup is still writing. The lock is non-blocking, so
// what they get instead is "another brb is using the staging directory", which
// is true.
//
// The guard: STAGING defaults under a world-writable /var/tmp, so enc/ may be
// a symlink or a directory belonging to somebody else, holding an identity and
// an index of their composition. Every other command refuses such a tree;
// index alone would decrypt the planted index with the planted key and print
// it as the operator's own map of which disc holds what.
func Index(ctx context.Context, o Options, pattern string, w io.Writer) error {
	if err := o.check(); err != nil {
		return err
	}
	if w == nil {
		return errors.New("restore: no output writer given")
	}
	unlock, err := o.lockStaging()
	if err != nil {
		return err
	}
	defer unlock()
	if err := o.secureStaging(o.dirs().Enc); err != nil {
		return err
	}
	ids, err := o.identities()
	if err != nil {
		return err
	}
	idx := filepath.Join(o.dirs().Enc, indexName)
	if _, err := os.Stat(idx); err != nil {
		return fmt.Errorf("restore: no index at %s — run 'brb ingest' first: %w", idx, err)
	}
	if err := o.checkIndexIntact(ctx, idx); err != nil {
		return err
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(agecrypt.DecryptTo(ctx, idx, pw, ids))
	}()
	defer pr.Close()

	// From here on a failure means the bytes are not what was written: the
	// index is gzip inside age, so neither layer tolerates a flipped bit, and
	// both report it rather than returning something plausible.
	// The index is on every disc and in every disc's sidecars set, so any of
	// them can put it back: disc 0 asks for whichever one is to hand.
	hint := o.sidecarRepairHint(0)
	gz, err := gzip.NewReader(pr)
	if err != nil {
		return fmt.Errorf("restore: reading %s (%s): %w", idx, hint, err)
	}
	defer gz.Close()

	// Terminal-only: ui.IsTerminal is true for any character device (including
	// /dev/null, where quoting is harmless), and piped output must stay
	// byte-faithful for the awk recipes and the cross-reader diff.
	n, err := filterIndex(ctx, gz, pattern, w, ui.IsTerminal(w))
	if err != nil {
		return fmt.Errorf("restore: reading %s (%s): %w", idx, hint, err)
	}
	if n > 0 {
		return nil
	}
	if pattern == "" {
		return fmt.Errorf("restore: the index in %s is empty", idx)
	}
	o.UI.Warn("no match for %q", pattern)
	return fmt.Errorf("restore: %w: %q", ErrNoMatch, pattern)
}

// checkIndexIntact compares the encrypted index with the hash recorded beside
// it, before anything tries to read it.
//
// The index is the file whose damage is least visible: gzip inside age, where
// one flipped bit costs the entire map of which disc holds what, and nothing
// notices until the day a disc is missing. It is covered by sidecars.par2, so
// a mismatch here is repairable on the spot rather than fatal to the archive.
//
// A missing sidecar is not an error: age's authentication still stands between
// a corrupted index and a plausible-looking wrong answer.
func (o Options) checkIndexIntact(ctx context.Context, idx string) error {
	name := filepath.Base(idx)
	hint := o.sidecarRepairHint(0)
	want, ok, err := recordedSum(idx+sumExt, name, hint)
	if err != nil {
		return err
	}
	if !ok {
		o.UI.Warn("no recorded hash for %s; age's own authentication is the only check performed", name)
		return nil
	}
	got, err := agecrypt.SumFile(ctx, idx)
	if err != nil {
		return fmt.Errorf("restore: hashing %s: %w", name, err)
	}
	if strings.EqualFold(got, want) {
		o.UI.Step("%s matches its recorded hash", name)
		return nil
	}
	return fmt.Errorf("restore: %s does not match the hash in %s — one of the two has rotted; "+
		"%s, then retry", name, name+sumExt, hint)
}

// maxIndexLine bounds one index line. Paths are long, but not this long, and a
// bound keeps a corrupted index from being read into memory without limit.
const maxIndexLine = 1 << 20

// filterIndex copies the index lines matching pattern to w and returns how many
// there were. An empty pattern matches everything.
//
// escape says the output is going to a terminal, where a filename's control
// bytes would otherwise be executed as escape sequences — retitling the
// window, clearing warnings off the screen, or worse on terminals with OSC 52
// on. Piped output stays byte-faithful, which is what lets the two readers'
// index output be diffed against each other.
func filterIndex(ctx context.Context, r io.Reader, pattern string, w io.Writer, escape bool) (int, error) {
	needle := strings.ToLower(pattern)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxIndexLine)
	sc.Split(scanLinesKeepCR)
	out := bufio.NewWriter(w)
	n := 0
	for i := 0; sc.Scan(); i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return n, err
			}
		}
		// The scanner's own bytes, not sc.Text(): one row per file in the set
		// means a $HOME-sized index runs this loop a million times, and a
		// string is materialised only on the branches that need one — the
		// case-folded match and the terminal escaping. The record and its
		// newline go to the bufio.Writer separately rather than through a
		// concatenation that copies the whole row to append one byte.
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if needle != "" || escape {
			s := string(line)
			if needle != "" && !strings.Contains(strings.ToLower(s), needle) {
				continue
			}
			if escape {
				s = escapeControls(s)
			}
			if _, err := out.WriteString(s); err != nil {
				return n, err
			}
		} else if _, err := out.Write(line); err != nil {
			return n, err
		}
		if err := out.WriteByte('\n'); err != nil {
			return n, err
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	if err := out.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

// escapeControls renders C0 control bytes and DEL visibly, sparing the tab
// that separates an index record's fields. The escapes are the C-style ones an
// operator can feed back through printf; everything printable passes through
// untouched, multi-byte characters included.
func escapeControls(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\t':
			b.WriteByte(c)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\n':
			b.WriteString(`\n`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// escapingWriter escapes control bytes line by line on the way to a terminal.
// It buffers the trailing partial line; Close flushes it, and Close must be
// called or the tail of the listing is lost.
//
// The destination is os.Stdout, which is unbuffered, and a disc packed with
// small files lists a million entries — one write(2) each, where the sibling
// index path has always batched its output through a bufio.Writer. The buffer
// is a chunk of the child's own pipe writes, so the operator sees the listing
// arrive at the same moments they did before; what goes is the syscall per
// line, not the streaming.
type escapingWriter struct {
	w    *bufio.Writer
	part []byte
}

// newEscapingWriter wraps w. os/exec copies a child's stdout in 32 KiB chunks,
// so a buffer of the same size turns each of those into about one write(2)
// instead of one per line.
func newEscapingWriter(w io.Writer) *escapingWriter {
	return &escapingWriter{w: bufio.NewWriterSize(w, 32<<10)}
}

// Write implements io.Writer over whole lines.
func (e *escapingWriter) Write(b []byte) (int, error) {
	e.part = append(e.part, b...)
	for {
		i := bytes.IndexByte(e.part, '\n')
		if i < 0 {
			break
		}
		line := escapeControls(string(e.part[:i]))
		e.part = e.part[i+1:]
		if _, err := e.w.WriteString(line); err != nil {
			return len(b), err
		}
		if err := e.w.WriteByte('\n'); err != nil {
			return len(b), err
		}
	}
	return len(b), nil
}

// Close flushes a trailing line that never got its newline, and then the
// buffer. It is safe to call more than once, so a caller that is already
// failing can flush what was listed before reporting why the rest is missing.
func (e *escapingWriter) Close() error {
	if len(e.part) > 0 {
		part := e.part
		e.part = nil
		if _, err := e.w.WriteString(escapeControls(string(part))); err != nil {
			return err
		}
	}
	return e.w.Flush()
}
