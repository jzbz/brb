//go:build linux

package restore

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// readPassphrase prompts on the controlling terminal and reads one line with
// echo turned off, the way age(1) itself asks. It deliberately refuses to read
// from anything but /dev/tty: a passphrase that can arrive on a pipe is a
// passphrase that ends up in shell history and process listings, and teaching
// operators to feed it to whatever asks is how secrets leak.
//
// The terminal is driven with raw termios ioctls rather than a dependency:
// echo off is one flag, and this module vendors nothing it does not need.
func readPassphrase(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("%w: /dev/tty: %v", ErrNoTerminal, err)
	}
	defer tty.Close()
	fd := tty.Fd()

	var old syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &old); err != nil {
		return "", fmt.Errorf("%w: /dev/tty is not a terminal: %v", ErrNoTerminal, err)
	}
	noEcho := old
	noEcho.Lflag &^= syscall.ECHO
	if err := ioctlTermios(fd, syscall.TCSETS, &noEcho); err != nil {
		return "", fmt.Errorf("turning echo off: %w", err)
	}
	// Echo is restored whatever happens below; a terminal left silent after a
	// failed prompt is a support call.
	defer func() { _ = ioctlTermios(fd, syscall.TCSETS, &old) }()

	fmt.Fprint(tty, prompt)
	line, err := bufio.NewReader(tty).ReadString('\n')
	// The operator's Enter was not echoed either; supply the newline so the
	// next output does not land on the prompt line.
	fmt.Fprintln(tty)
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the passphrase: %w", err)
	}
	pass := strings.TrimRight(line, "\r\n")
	if pass == "" {
		return "", ErrEmptyPassphrase
	}
	return pass, nil
}

// ioctlTermios wraps the get/set-attributes ioctl pair.
func ioctlTermios(fd uintptr, req uint, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}
