package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/tools"
)

// fakeDrive stands in for udisksctl and findmnt on a burner that is not
// mounted until udisksctl has been asked to mount it. udisksctl's mount marks
// the drive mounted and then hangs, the way it does waiting on a slow disc, so
// a test can cancel in the middle of it; its unmount is recorded in a log.
// findmnt reports the mount point only once the drive is marked mounted.
func fakeDrive(t *testing.T, e *env) (mountedMark, unmountLog string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build udisksctl and findmnt stand-ins with")
	}
	dir := t.TempDir()
	mountedMark = filepath.Join(dir, "mounted")
	unmountLog = filepath.Join(dir, "unmounts")
	mp := filepath.Join(dir, "mnt")
	udisks := filepath.Join(dir, "udisksctl")
	findmnt := filepath.Join(dir, "findmnt")
	if err := os.WriteFile(udisks, []byte("#!/bin/sh\ncase \"$1\" in\n"+
		"mount) : > '"+mountedMark+"'; exec sleep 30 ;;\n"+
		"unmount) echo unmount >> '"+unmountLog+"'; rm -f '"+mountedMark+"' ;;\n"+
		"esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findmnt, []byte("#!/bin/sh\n[ -e '"+mountedMark+"' ] && echo '"+mp+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.tools = tools.NewSet([]tools.Tool{
		{Name: tools.Udisksctl, Path: udisks, Found: true},
		{Name: tools.Findmnt, Path: findmnt, Found: true},
	})
	e.cfg.Burner = "/dev/brb-test-burner"
	return mountedMark, unmountLog
}

// INC-5: Ctrl-C at ingest's "press Enter" prompt, then Enter, used to send a
// dead context to udisksctl, and the cancellation came back as "could not
// mount /dev/sr0 — mount it yourself and pass the mount point". It is an
// interruption, and must say so.
func TestMountDiscReportsACancelledRunAsAnInterruption(t *testing.T) {
	e := newEnv(t)
	fakeDrive(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, release, err := e.opts().mountDisc(ctx, "")
	release()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mountDisc = %v, want the cancellation", err)
	}
	if strings.Contains(err.Error(), "mount it yourself") || strings.Contains(e.log(), "could not mount") {
		t.Fatalf("a cancelled run was reported as a drive that would not mount: %v\n%s", err, e.log())
	}
}

// An operator's own mount point is no different: a run already told to stop
// does not go on to read the disc there.
func TestMountDiscWithAMountPointHonoursCancellation(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mp, _, err := e.opts().mountDisc(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || mp != "" {
		t.Fatalf("mountDisc = (%q, %v), want no mount point and the cancellation", mp, err)
	}
}

// udisksctl only asks the udisks daemon to mount, and the daemon can finish
// after the client is interrupted. A disc that ends up mounted by a cancelled
// run is that run's to unmount — leaving it mounted is exactly what makes the
// next run fail, and the tray refuse to open.
func TestMountDiscUndoesAMountTheInterruptionLeft(t *testing.T) {
	e := newEnv(t)
	mounted, unmounts := fakeDrive(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(mounted); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	mp, release, err := e.opts().mountDisc(ctx, "")
	release()
	if !errors.Is(err, context.Canceled) || mp != "" {
		t.Fatalf("mountDisc = (%q, %v), want no mount point and the cancellation\n%s", mp, err, e.log())
	}
	log, rerr := os.ReadFile(unmounts)
	if rerr != nil || !strings.Contains(string(log), "unmount") {
		t.Fatalf("the disc the interrupted mount left mounted was not unmounted (%v)\n%s", rerr, e.log())
	}
}
