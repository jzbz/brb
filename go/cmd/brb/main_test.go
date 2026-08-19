package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// helperEnv turns the test binary into a stand-in for a stuck brb: it sets up
// the signal context exactly as main does, reports when the first signal has
// cancelled it, and then never returns — the way a child process ignoring
// SIGINT or a write that will not come back would keep the real program
// alive. Only a signal taking its default action ends it.
const helperEnv = "BRB_TEST_SIGNAL_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		ctx, _ := signalContext()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("cancelled")
		select {}
	}
	os.Exit(m.Run())
}

// TestSecondSignalKillsTheProcess pins what main's comment promises: the
// first Ctrl-C cancels the context, the second kills brb outright. Before
// signalContext released the signals on cancellation, NotifyContext kept
// catching them, the second Ctrl-C was swallowed, and a brb stuck behind an
// unresponsive child could not be interrupted from the keyboard at all. Here
// that would show as the helper never exiting.
func TestSecondSignalKillsTheProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Whatever happens below, do not leave the helper behind.
	defer func() { _ = cmd.Process.Kill() }()

	lines := bufio.NewScanner(out)
	expect := func(want string) {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("helper ended before printing %q: %v", want, lines.Err())
		}
		if got := lines.Text(); got != want {
			t.Fatalf("helper printed %q, want %q", got, want)
		}
	}
	expect("ready")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	expect("cancelled")

	// The helper is now where a stuck brb would be: context cancelled, still
	// running. A second SIGINT has to end it. Give the goroutine that hands
	// the signal back its moment, and give the kernel a moment to deliver.
	deadline := time.After(5 * time.Second)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("helper exited normally (%v); it can only end by a signal taking its default action", err)
			}
			ws, ok := ee.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
				t.Fatalf("helper ended with %v, want death by SIGINT", err)
			}
			return
		case <-time.After(200 * time.Millisecond):
			// Not yet: the second signal may have raced the release. Again.
		case <-deadline:
			t.Fatal("the second SIGINT was swallowed: the helper is still running 5s after it, so a stuck brb could not be interrupted")
		}
	}
}
