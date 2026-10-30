//go:build linux

package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/fsx"
)

// mountedDiscDirEnv and mountKindEnv carry the disc directory, and what to
// mount there, to the copy of the test binary that
// TestFreshDiscDirOverARealMount starts in namespaces of its own.
const (
	mountedDiscDirEnv = "BRB_TEST_MOUNTED_DISC_DIR"
	mountKindEnv      = "BRB_TEST_MOUNT_KIND"
)

// TestFreshDiscDirOverARealMount is TestBuildDiscDirsRefusesAMountedDiscDirectory
// against something that really is mounted at the disc directory, where the
// system lets an unprivileged process mount: the test binary runs itself again
// in a new user and mount namespace, mounts there, puts one file in the
// mount, and calls freshDiscDir on it. Without the refusal RemoveAll deleted
// that file and then failed with EBUSY. Two mounts, because they are caught
// by different tests: a tmpfs, which has a device of its own, and a directory
// bind-mounted from the filesystem discs/ is on, which has the device discs/
// has and is seen only in the kernel's list of mount points.
//
// It is skipped where user namespaces, or mounting in them, are not allowed,
// which is the case on some CI runners; the deviceOf and mountPointsUnder
// cases of the other test cover the same rules everywhere.
func TestFreshDiscDirOverARealMount(t *testing.T) {
	if dd := os.Getenv(mountedDiscDirEnv); dd != "" {
		fmt.Println(mountedDiscDirChild(dd, os.Getenv(mountKindEnv)))
		return
	}
	for _, kind := range []string{"tmpfs", "bind"} {
		t.Run(kind, func(t *testing.T) { realMountCase(t, kind) })
	}
}

// realMountCase runs the namespaced half for one kind of mount and reports
// what it said.
func realMountCase(t *testing.T, kind string) {
	dd := filepath.Join(t.TempDir(), "discs", discDirName(1))
	if err := os.MkdirAll(dd, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary to run it again: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestFreshDiscDirOverARealMount$", "-test.count=1")
	cmd.Env = append(os.Environ(), mountedDiscDirEnv+"="+dd, mountKindEnv+"="+kind)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Skipf("cannot start a process in a user namespace here: %v", err)
	}
	result := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "mount-test: ") {
			result = strings.TrimPrefix(line, "mount-test: ")
		}
	}
	t.Logf("namespaced run: %s", result)
	switch {
	case strings.HasPrefix(result, "skip: "):
		t.Skip(strings.TrimPrefix(result, "skip: "))
	case result == "":
		t.Fatalf("the namespaced run reported nothing (%v):\n%s", err, out)
	case result != "refused, file kept":
		t.Fatalf("freshDiscDir over a %s mounted at the disc directory: %s", kind, result)
	}
}

// mountedDiscDirChild is the namespaced half: it mounts a tmpfs at dd, or
// bind-mounts a directory beside discs/ there, puts a file in the mount, runs
// freshDiscDir, and says on one line what became of both.
func mountedDiscDirChild(dd, kind string) string {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return "mount-test: skip: cannot make the mount namespace private: " + err.Error()
	}
	switch kind {
	case "tmpfs":
		if err := syscall.Mount("tmpfs", dd, "tmpfs", 0, "size=1m"); err != nil {
			return "mount-test: skip: cannot mount a tmpfs in a user namespace here: " + err.Error()
		}
	case "bind":
		src := filepath.Join(filepath.Dir(filepath.Dir(dd)), "bind-source")
		if err := os.Mkdir(src, 0o700); err != nil {
			return "mount-test: making the bind source: " + err.Error()
		}
		if err := syscall.Mount(src, dd, "", syscall.MS_BIND, ""); err != nil {
			return "mount-test: skip: cannot bind-mount in a user namespace here: " + err.Error()
		}
	default:
		return "mount-test: unknown mount kind " + strconv.Quote(kind)
	}
	defer syscall.Unmount(dd, syscall.MNT_DETACH)
	if kind == "bind" {
		// Both conditions are what make this case worth having: without
		// the first it tests nothing the tmpfs case does not, and without
		// the second it cannot pass.
		ddFI, err1 := os.Lstat(dd)
		discsFI, err2 := os.Lstat(filepath.Dir(dd))
		if err := errors.Join(err1, err2); err != nil {
			return "mount-test: " + err.Error()
		}
		ddDev, _ := fsx.DeviceID(ddFI)
		discsDev, _ := fsx.DeviceID(discsFI)
		if ddDev != discsDev {
			return "mount-test: the bind mount has a device of its own, so it is not a same-filesystem bind"
		}
		if _, ok, err := fsx.MountPointsUnder(dd); err != nil || !ok {
			return fmt.Sprintf("mount-test: skip: /proc/self/mountinfo cannot be read here (%v), "+
				"so a same-filesystem bind mount cannot be recognised", err)
		}
	}
	precious := filepath.Join(dd, "precious.txt")
	if err := os.WriteFile(precious, []byte("in the mount"), 0o600); err != nil {
		return "mount-test: writing into the mount: " + err.Error()
	}
	err := freshDiscDir(dd, filepath.Join(dd, "data"))
	_, kept := os.Stat(precious)
	switch {
	case err != nil && strings.Contains(err.Error(), dd+" is ") && strings.Contains(err.Error(), "mount point") && kept == nil:
		return "mount-test: refused, file kept"
	case kept != nil:
		return fmt.Sprintf("mount-test: the file in the mount is gone (freshDiscDir: %v)", err)
	default:
		return fmt.Sprintf("mount-test: not refused (freshDiscDir: %v)", err)
	}
}
