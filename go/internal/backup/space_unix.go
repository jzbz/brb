//go:build unix

package backup

import (
	"fmt"

	"github.com/jzbz/brb/internal/fsx"
)

// freeSpace returns the bytes available on the filesystem holding path,
// reported as internal/fsx reports it: the blocks available to an unprivileged
// user, since the reserved-block margin most Linux filesystems keep back is not
// space a backup can use.
func freeSpace(path string) (int64, error) {
	n, err := fsx.FreeSpace(path)
	if err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	return n, nil
}
