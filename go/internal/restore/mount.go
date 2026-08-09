package restore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
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
func (o Options) mountDisc(ctx context.Context, mountPoint string) (string, func(), error) {
	nop := func() {}

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
	if err != nil {
		o.UI.Step("udisksctl could not mount %s: %v", dev, err)
	}
	mp := o.findMount(ctx, dev)
	if mp == "" {
		mp = parseUdisksMount(out)
	}
	if mp == "" {
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

// VerifyDisc reads a burned disc back and checks every file on it against the
// SHA512SUMS written at backup time. A disc that does not verify is reported
// with the failing names and an error wrapping ErrVerifyFailed; par2 may still
// be able to repair it during a restore, which the message says.
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

	sumPath := filepath.Join(mp, agecrypt.SumsName)
	if _, err := os.Stat(sumPath); err != nil {
		return fmt.Errorf("restore: no %s at %s — is this one of ours?: %w", agecrypt.SumsName, mp, err)
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
	// so this can only ever be advice: warn when a loaded name disagrees with
	// the disc, exactly as brb.sh does.
	if o.Cfg.ArchiveName != "" {
		if marc := manifestArchiveName(mp); marc != "" && marc != o.Cfg.ArchiveName {
			o.UI.Warn("this disc belongs to archive '%s', not '%s'", marc, o.Cfg.ArchiveName)
		}
	}

	o.UI.Log("verifying disc %d at %s", n, mp)
	bad, err := agecrypt.VerifyDir(ctx, mp, sumPath)
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
// MANIFEST.txt, or "" when there is no manifest or no such field.
func manifestArchiveName(mp string) string {
	f, err := os.Open(filepath.Join(mp, manifestName))
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
	if os.Geteuid() != 0 {
		return errors.New("restore: mounting a squashfs image requires root")
	}
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
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("restore: creating %s: %w", mountPoint, err)
	}
	if _, err := runTool(ctx, mountBin, "-o", "loop,ro", "--", plain, mountPoint); err != nil {
		return fmt.Errorf("restore: mounting %s at %s: %w", plain, mountPoint, err)
	}
	o.UI.OK("disc %d mounted read-only at %s", n, mountPoint)
	o.UI.Step("unmount with: umount %s", mountPoint)
	o.UI.Step("the decrypted image is at %s; remove it once you have unmounted", plain)
	return nil
}
