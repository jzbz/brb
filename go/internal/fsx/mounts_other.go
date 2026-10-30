//go:build unix && !linux

package fsx

// MountPointsUnder is the Linux mountinfo check, which other systems do not
// offer in the same form. ok is always false here, so a caller has only
// [DeviceID] to go on: it catches another filesystem mounted at a directory,
// and misses a directory bind-mounted from the filesystem already there.
func MountPointsUnder(string) (mps []string, ok bool, err error) {
	return nil, false, nil
}
