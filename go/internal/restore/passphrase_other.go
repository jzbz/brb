//go:build !linux

package restore

import "errors"

// readPassphrase needs termios control of /dev/tty, which is only wired up for
// Linux — the platform brb runs on. Anywhere else the passphrase-protected
// identity cannot be unlocked in-process; the caller's error message points at
// AGE_IDENTITY as the way out.
func readPassphrase(string) (string, error) {
	return "", errors.New("reading a passphrase without echo is only implemented on Linux")
}
