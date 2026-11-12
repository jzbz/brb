package restore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// procMounts is where the kernel lists the current mounts. It is the fallback
// for finding a drive's mount point when findmnt is not installed.
const procMounts = "/proc/self/mounts"

// mountDisc resolves where the burned disc can be read from.
//
// An explicitly supplied mount point always wins and is never unmounted: it
// belongs to the operator. Otherwise the burner's existing mount is looked up,
// and only if it is not mounted at all is udisksctl asked to mount it — in
// which case the returned release function unmounts it again.
//
// release is never nil and is safe to call more than once. It runs even when
// the context has already been cancelled, because leaving a disc mounted after
// a Ctrl-C is exactly the state that makes the next run fail. That is done with
// a detached context rather than a signal handler: this is a library, and a
// library must not install process-wide handlers.
//
// A cancelled context is reported as the interruption it is, never as a disc
// that would not mount. Ctrl-C at ingest's "press Enter" prompt, then Enter,
// used to reach udisksctl with the context already gone, and the operator was
// told "could not mount /dev/sr0 — mount it yourself and pass the mount point"
// for a drive that was fine.
func (o Options) mountDisc(ctx context.Context, mountPoint string) (string, func(), error) {
	nop := func() {}
	if err := ctx.Err(); err != nil {
		return "", nop, fmt.Errorf("restore: interrupted before mounting: %w", err)
	}

	if mountPoint != "" {
		fi, err := os.Stat(mountPoint)
		if err != nil {
			return "", nop, fmt.Errorf("restore: mount point %s: %w", mountPoint, err)
		}
		if !fi.IsDir() {
			return "", nop, fmt.Errorf("restore: mount point %s is not a directory", mountPoint)
		}
		return mountPoint, nop, nil
	}

	dev := o.Cfg.Burner
	if dev == "" {
		return "", nop, errors.New("restore: no BURNER configured and no mount point given")
	}
	if mp := o.findMount(ctx, dev); mp != "" {
		o.UI.Step("%s is already mounted at %s", dev, mp)
		return mp, nop, nil
	}

	u := o.Tools.Get(tools.Udisksctl)
	if !u.Found {
		return "", nop, fmt.Errorf("restore: %s is not mounted and udisksctl is not installed — mount it yourself and pass the mount point", dev)
	}
	o.UI.Step("mounting %s with udisksctl", dev)
	out, err := runTool(ctx, u.Path, "mount", "-b", dev)
	if err != nil && ctx.Err() == nil {
		o.UI.Step("udisksctl could not mount %s: %v", dev, err)
	}
	// Looked up on a context of its own, so that it still runs after a
	// cancel: udisksctl only asks the udisks daemon to mount, and the daemon
	// can finish the job after the client was interrupted. Whether the disc is
	// mounted now decides whether there is a mount of ours to undo.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), unmountGrace)
	mp := o.findMount(lctx, dev)
	lcancel()
	if mp == "" {
		mp = parseUdisksMount(out)
	}
	if mp == "" {
		if cerr := ctx.Err(); cerr != nil {
			return "", nop, fmt.Errorf("restore: interrupted while mounting %s: %w", dev, cerr)
		}
		return "", nop, fmt.Errorf("restore: could not mount %s — mount it yourself and pass the mount point", dev)
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			// Detached from ctx: an unmount must still happen after a cancelled
			// operation.
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unmountGrace)
			defer cancel()
			if _, err := runTool(uctx, u.Path, "unmount", "-b", dev); err != nil {
				o.UI.Warn("could not unmount %s: %v", dev, err)
				return
			}
			o.UI.Step("unmounted %s", dev)
		})
	}
	if cerr := ctx.Err(); cerr != nil {
		// Mounted, but by a run that has been told to stop. It was not mounted
		// before udisksctl was asked, so the mount is this run's to undo, and
		// nobody is going to read the disc now.
		release()
		return "", nop, fmt.Errorf("restore: interrupted while mounting %s: %w", dev, cerr)
	}
	return mp, release, nil
}

// findMount reports where dev is currently mounted, preferring findmnt (which
// is what brb.sh uses) and falling back to the kernel's own mount table.
func (o Options) findMount(ctx context.Context, dev string) string {
	if f := o.Tools.Get(tools.Findmnt); f.Found {
		out, err := runTool(ctx, f.Path, "-n", "-o", "TARGET", "--source", dev)
		if err == nil {
			if mp := firstLine(out); mp != "" {
				return mp
			}
		}
	}
	data, err := os.ReadFile(procMounts)
	if err != nil {
		return ""
	}
	return parseProcMounts(string(data), dev)
}

// parseProcMounts returns the first mount point of dev in the kernel's mount
// table, decoding the octal escapes the kernel uses for spaces and tabs.
func parseProcMounts(table, dev string) string {
	if dev == "" {
		return ""
	}
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if unescapeMount(f[0]) != dev {
			continue
		}
		return unescapeMount(f[1])
	}
	return ""
}

// unescapeMount decodes the \0NN octal escapes /proc/self/mounts uses for
// space, tab, newline and backslash.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// parseUdisksMount extracts the mount point from udisksctl's report, which
// reads "Mounted /dev/sr0 at /run/media/user/LABEL". Older versions end the
// line with a full stop.
func parseUdisksMount(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Mounted ") {
			continue
		}
		i := strings.Index(line, " at ")
		if i < 0 {
			continue
		}
		mp := strings.TrimSpace(line[i+len(" at "):])
		mp = strings.TrimSuffix(mp, ".")
		if mp != "" {
			return mp
		}
	}
	return ""
}

// eject opens the drive tray, ignoring every failure: an operator with a
// slot-loading drive or no eject binary is not having an error.
func (o Options) eject(ctx context.Context) {
	dev := o.Cfg.Burner
	e := o.Tools.Get(tools.Eject)
	if dev == "" || !e.Found {
		return
	}
	if _, err := runTool(ctx, e.Path, dev); err != nil {
		o.UI.Step("could not eject %s: %v", dev, err)
	}
}

// VerifyDisc reads a burned disc back and checks every file its SHA512SUMS
// lists against the hash recorded at backup time. A file on the disc that the
// SHA512SUMS does not list is not read — the format attests what the writer
// listed, and the writer lists every regular file — but the disc's own image
// must be listed, or there is nothing here to verify: a SHA512SUMS that left it
// out (or listed nothing at all) used to report "disc N verified" without
// reading a byte of the image. brb.sh refuses the same discs.
//
// A disc that does not verify is reported with the failing names and an error
// wrapping ErrVerifyFailed; par2 may still be able to repair it during a
// restore, which the message says.
func VerifyDisc(ctx context.Context, o Options, n int, mountPoint string) error {
	if err := o.check(); err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("restore: disc number must be 1 or more, got %d", n)
	}
	mp, release, err := o.mountDisc(ctx, mountPoint)
	if err != nil {
		return err
	}
	defer release()

	discRoot, err := os.OpenRoot(mp)
	if err != nil {
		return fmt.Errorf("restore: opening %s: %w", mp, err)
	}
	defer discRoot.Close()

	// SHA512SUMS is held to discEntry's rule, as ingest holds it (discSums), and
	// as brb.sh's verify-disc holds it with -L before -f. A link used to be
	// followed here, so a disc whose SHA512SUMS pointed at a file off the
	// disc — one the operator's machine supplies — had its hashes taken from
	// there and was called verified. Anything else that is not a regular file is
	// refused too: a FIFO blocks open(2) until a writer appears, and
	// ReadSumFile's open is not one a cancelled context can interrupt, so
	// verify-disc hung through the first Ctrl-C.
	sumPath := filepath.Join(mp, agecrypt.SumsName)
	sfi, err := discEntry(discRoot, agecrypt.SumsName)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restore: no %s at %s — is this one of ours?: %w", agecrypt.SumsName, mp, err)
	}
	if err != nil {
		return fmt.Errorf("restore: %s: %w", mp, err)
	}
	if !sfi.Mode().IsRegular() {
		return fmt.Errorf("restore: %s at %s is not a regular file (%s) — is this one of ours?",
			agecrypt.SumsName, mp, sfi.Mode().Type())
	}

	// data/ itself is held to the same rule before dataFiles lists it, as
	// ingest holds it: os.ReadDir follows a symlinked data/ wherever it points,
	// so a disc whose data is a link to a directory of the operator's, holding a
	// file named like the image, passed the identity gate below and verified,
	// while brb.sh — whose `find -P` will not descend a symlinked start point —
	// refused it.
	if _, err := discEntry(discRoot, dataDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restore: %s: %w", mp, err)
	}

	// SHA512SUMS is per-disc and self-consistent, so ANY disc of the set passes
	// against its own copy. Without this, a tray that did not open lets disc 3
	// be recorded as "disc 7 verified" and disc 7 is never read again — eject
	// failures are deliberately silent, so nothing else in the flow notices.
	// brb.sh carries the same gate, for the same reason.
	names, err := dataFiles(filepath.Join(mp, dataDir))
	got := 0
	if err == nil {
		got = discOfDataFiles(names)
	}
	if got != n {
		holds := "an unrecognised disc"
		if got > 0 {
			holds = encName(got)
		}
		return fmt.Errorf("restore: the drive holds %s, not disc %d — insert disc %d", holds, n, n)
	}
	// The archive name is a writer-side setting a reader config may not carry,
	// so this can only ever be advice: warn when the name disagrees with the
	// disc. It is not quite brb.sh's rule. brb.sh compares only an ARCHIVE_NAME
	// the config or the environment sets, while this always has one, because
	// config.Load derives <basename SOURCE_DIR>-<today> when none is set, so a
	// reader config that names no archive gets the warning on nearly every
	// disc. The README's table of reader differences lists it.
	if o.Cfg.ArchiveName != "" {
		if marc := manifestArchiveName(mp); marc != "" && marc != o.Cfg.ArchiveName {
			o.UI.Warn("this disc belongs to archive '%s', not '%s'", marc, o.Cfg.ArchiveName)
		}
	}

	o.UI.Log("verifying disc %d at %s", n, mp)
	bad, err := agecrypt.VerifyDir(ctx, mp, sumPath, dataDir+"/"+encName(n))
	if err != nil {
		return fmt.Errorf("restore: verifying disc %d: %w", n, err)
	}
	if len(bad) > 0 {
		for _, name := range bad {
			o.UI.Warn("mismatch: %s", name)
		}
		o.UI.Warn("par2 may still recover this disc: run 'brb ingest' and then 'brb restore'")
		return fmt.Errorf("restore: disc %d has %d file(s) that do not match their recorded hash: %w", n, len(bad), ErrVerifyFailed)
	}
	o.UI.OK("disc %d verified", n)
	return nil
}

// manifestArchiveName reads the "archive name" field from a disc's
// MANIFEST.txt, or "" when there is no manifest, no such field, or something
// other than a regular file under that name. The last is brb.sh's
// [[ -f MANIFEST.txt ]], and it is not only parity: a FIFO there blocked the
// open below until a second signal, and config.Load always fills in an archive
// name, so every verify-disc reached it.
func manifestArchiveName(mp string) string {
	path := filepath.Join(mp, manifestName)
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4<<10), maxManifestLine)
	for sc.Scan() {
		if v, ok := manifestField(sc.Text(), "archive name"); ok {
			return v
		}
	}
	return ""
}

// Mount decrypts one disc's image and mounts it read-only, which is the whole
// point of the format: after this, the backup is an ordinary directory tree
// that any program can read.
//
// It needs root, because mounting does. The decrypted image stays on disk for
// as long as it is mounted; the caller is told where it is so it can be removed
// after unmounting.
//
// The image is a backup, not this host's system, so it is mounted nosuid and
// nodev — see [mountArgs]. brb.sh's cmd_mount makes the same mount with the
// same options, refuses the same symlinked mount point and creates the same
// 0700 directories, so the two readers mount a disc the same way.
func Mount(ctx context.Context, o Options, n int, mountPoint string) error {
	if err := o.check(); err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("restore: disc number must be 1 or more, got %d", n)
	}
	if mountPoint == "" {
		return errors.New("restore: no mount point given")
	}
	// A mount point that is itself a symlink is refused, because mount(8) run
	// as root resolves the whole path first and mounts on the link's TARGET: a
	// link left at /shared/browse pointing at /etc gets the image mounted over
	// /etc, hiding it until someone thinks to unmount it, and MkdirAll below
	// takes the link for an existing directory without a word. It is the rule
	// the staging directories and the restore destination already follow.
	//
	// dirPath first, for the reason SecureDir cleans STAGING: "link/" and
	// "link/." are resolved through the link before Lstat sees them, so the
	// check has to look at the spelling without them — and that spelling is
	// the one handed to MkdirAll and mount(8), so the check and the act are
	// about one path. It is a check followed by an act all the same: a link
	// planted in between is still followed, and nothing in either reader can
	// close that while mount(8) takes a path (README, Limitations).
	//
	// The checks come in the order brb.sh's cmd_mount makes them — the
	// arguments, this, root, then staging — so that the two readers refuse a
	// run for the same reason. Before the root check, so the refusal is the
	// same for everyone and can be tested without root. Before lockStaging,
	// which creates and secures STAGING and takes its lock: with it first, a
	// non-root operator against the default root-owned /var/tmp/brb, or anyone
	// whose STAGING is itself a symlink, was refused over staging by this
	// reader and over the mount point or root by the other, and a run on its
	// way to being refused had already made a staging tree. And before
	// PrepareImage, so a run that is going to be refused does not first
	// decrypt a disc's worth of plaintext.
	mountPoint = dirPath(mountPoint)
	if fi, err := os.Lstat(mountPoint); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, _ := os.Readlink(mountPoint)
		return fmt.Errorf("restore: mount point %s is a symlink (-> %s); mount(8) follows it and "+
			"would mount the image over whatever it points at — pass the directory itself",
			mountPoint, target)
	}
	if geteuid() != 0 {
		return errors.New("restore: mounting a squashfs image requires root")
	}
	unlock, err := o.lockStaging()
	if err != nil {
		return err
	}
	defer unlock()
	mountBin, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("restore: mount(8) not found on PATH: %w", err)
	}
	imgs, err := o.selectImages(n)
	if err != nil {
		return err
	}
	plain, err := PrepareImage(ctx, o, imgs[0].Path)
	if err != nil {
		return err
	}
	// 0700, which is what brb.sh's mkdir -p makes under the umask 077 its
	// secure_staging sets for the rest of the command; the Go build used to
	// make 0755, so the two readers left different directories behind. The
	// mode decides more than tidiness. Once mounted, the leaf takes the image
	// root's mode, but every parent made here decides whether another local
	// account can reach the decrypted tree at all, and a directory made for
	// root to mount a backup on has no business being open to everybody by
	// default. A directory that already exists keeps its mode: an operator who
	// wants the tree shared mounts it somewhere that already is.
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return fmt.Errorf("restore: creating %s: %w", mountPoint, err)
	}
	if _, err := runTool(ctx, mountBin, mountArgs(plain, mountPoint)...); err != nil {
		return fmt.Errorf("restore: mounting %s at %s: %w", plain, mountPoint, err)
	}
	o.UI.OK("disc %d mounted read-only at %s", n, mountPoint)
	o.UI.Step("unmount with: umount %s", ui.ShellWord(mountPoint))
	o.UI.Step("the decrypted image is at %s; remove it once you have unmounted", plain)
	return nil
}

// mountArgs is mount(8)'s argument list for one decrypted image, and brb.sh's
// cmd_mount passes the same one.
//
// ro alone is not enough, because it implies neither nosuid nor nodev: the
// kernel's defaults are suid and dev, and -o loop,ro sets MS_RDONLY and nothing
// else. The format keeps setuid bits, file capabilities (as xattrs) and device
// nodes, which is right for a faithful restore of / and wrong for a browse
// mount on the machine doing the reading. A backup of / taken as root, as the
// README recommends, carries that day's setuid-root binaries — an old sudo or
// pkexec with a published local root bug, decades on — and any local account
// that can reach the mount could run them with their archived privilege. A
// device node in the image opens the host device its numbers name, and a
// read-only filesystem does not make a device read-only. A forged disc can
// carry anything at all. nosuid and nodev switch both off and cost browsing
// nothing: stat still reports the mode bits and st_rdev, so copying out with
// cp -a, tar or rsync keeps them, and restore goes through unsquashfs, which
// never sees these options.
//
// noexec is deliberately not added. It closes no privilege boundary — a
// program that is not setuid runs as the caller either way, and ld.so or an
// interpreter steps around it — and it would stop a program in the backup
// from being run straight off the mount.
//
// "--" ends mount(8)'s options, so a mount point spelled with a leading dash
// is a mount point rather than a flag. brb.sh used to leave it out and read
// such a name as options; it passes it now too.
func mountArgs(plain, mountPoint string) []string {
	return []string{"-o", "loop,ro,nosuid,nodev", "--", plain, mountPoint}
}

// geteuid is os.Geteuid, held in a variable so that a test can get past
// Mount's root check and drive the rest of it against a stand-in mount(8).
// Nothing else shows which arguments the real command is given or what mode
// the directory made for it gets, and a test that needs root is a test that
// never runs.
var geteuid = os.Geteuid

// dirPath is brb.sh's dir_path, rule for rule: the spelling of a directory
// path that lstat answers about the directory ITSELF. Every trailing "/" and
// "/." comes off, repeatedly, because the kernel resolves "link/" and "link/."
// THROUGH a symlink before lstat sees them; "/" survives as "/", and an empty
// argument stays empty.
//
// It is deliberately not filepath.Clean. Clean also collapses ".." lexically,
// and the kernel does not: with a/link pointing at ../v, "a/link/../b" is the
// b beside v to mkdir, to mount(8) and to brb.sh, and "a/b" to Clean. The path
// goes on to MkdirAll and mount(8) after the check, so a reader that cleaned it
// would mount somewhere other than where the other reader mounts — and where
// either of them mounted before this check existed. Stripping only the
// trailing components leaves both readers handing the kernel byte-identical
// paths, and an interior "//" or "/./" names the same file to every lookup, so
// it can hide nothing.
func dirPath(d string) string {
	for d != "" && d != "/" {
		switch {
		case strings.HasSuffix(d, "/"):
			d = strings.TrimSuffix(d, "/")
		case strings.HasSuffix(d, "/."):
			d = strings.TrimSuffix(d, "/.")
		default:
			return d
		}
		if d == "" {
			d = "/"
		}
	}
	return d
}
