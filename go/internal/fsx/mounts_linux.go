//go:build linux

package fsx

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// MountPointsUnder returns every mount point at dir or below it in this
// process's mount namespace, as paths under dir as it was given, dir itself
// first when it is one. It reads the kernel's own list, /proc/self/mountinfo,
// so it also names a directory bind-mounted from the filesystem dir is on,
// which has the device of dir's parent and so cannot be told from an ordinary
// directory by [DeviceID]. The kernel's other answer, statx's
// STATX_ATTR_MOUNT_ROOT, exists only from Linux 5.8, and calling it means
// either spelling the syscall out per architecture or importing
// golang.org/x/sys/unix, which would make the vendored source in the release
// tarball ten times larger. mountinfo has been there since 2.6.26.
//
// ok is false when the list cannot be opened, as where /proc is not mounted
// in a sandbox or chroot, and a caller then has only the device to go on. A
// dir that does not exist holds no mount points.
func MountPointsUnder(dir string) (mps []string, ok bool, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, false, err
	}
	// mountinfo names a mount point by its path with every symlink resolved,
	// so dir is compared in the same terms: a staging tree reached through a
	// symlink, such as a ~/.cache that links to /tmp, would otherwise match
	// nothing.
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, false, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// A line holds two paths of up to PATH_MAX each, every byte of which may
	// be escaped to four, and the mount's options; a megabyte covers it.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		// The fifth field is the mount point. The kernel writes a space, tab,
		// newline or backslash in it as an octal escape, so splitting the line
		// on spaces cannot cut a path in two.
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		rel, err := filepath.Rel(real, unescapeMountinfo(fields[4]))
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		mps = append(mps, filepath.Join(dir, rel))
	}
	if err := sc.Err(); err != nil {
		return nil, false, fmt.Errorf("reading /proc/self/mountinfo: %w", err)
	}
	// Byte order puts dir, whose rel is ".", before anything below it.
	sort.Strings(mps)
	return mps, true, nil
}

// unescapeMountinfo undoes the kernel's escaping of a mountinfo path: a
// backslash and three octal digits stand for the byte they spell.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
