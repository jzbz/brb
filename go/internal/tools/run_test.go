package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shell returns the path to /bin/sh, skipping the test when there is none.
func shell(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no /bin/sh available")
	}
	return p
}

func TestRunReportsExitStatus(t *testing.T) {
	sh := shell(t)
	err := run(context.Background(), runSpec{
		name: "sh", path: sh,
		args: []string{"-c", "echo bad things happened >&2; exit 3"},
	})
	if err == nil {
		t.Fatal("run() = nil, want an error for exit status 3")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("run() error is not an *exec.ExitError: %v", err)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
	if !strings.Contains(err.Error(), "bad things happened") {
		t.Errorf("error should carry the tool's output: %v", err)
	}
}

func TestRunFiltersOutputButKeepsStatus(t *testing.T) {
	sh := shell(t)
	var log bytes.Buffer
	err := run(context.Background(), runSpec{
		name: "sh", path: sh,
		args: []string{"-c", `printf 'xorriso : UPDATE : busy\nxorriso : FAILURE : nope\n'; exit 1`},
		log:  &log,
		// This is the shape of the "tool | grep -v ... || true" bug: filtering
		// must not cost the exit status.
		filter: KeepISOLine,
	})
	if err == nil {
		t.Fatal("run() = nil, want the child's non-zero exit to survive filtering")
	}
	if got := log.String(); strings.Contains(got, "UPDATE") {
		t.Errorf("filtered line reached the log: %q", got)
	} else if !strings.Contains(got, "FAILURE") {
		t.Errorf("kept line missing from the log: %q", got)
	}
}

func TestRunSeparatesStdoutFromTheLog(t *testing.T) {
	sh := shell(t)
	var out, log bytes.Buffer
	err := run(context.Background(), runSpec{
		name: "sh", path: sh,
		args:   []string{"-c", `printf 'payload\n'; printf 'chatter\n' >&2`},
		stdout: &out,
		log:    &log,
	})
	if err != nil {
		t.Fatalf("run() = %v", err)
	}
	if got := out.String(); got != "payload\n" {
		t.Errorf("stdout = %q, want %q", got, "payload\n")
	}
	if got := log.String(); !strings.Contains(got, "chatter") {
		t.Errorf("log = %q, want it to contain the stderr chatter", got)
	}
	if strings.Contains(log.String(), "payload") {
		t.Errorf("stdout leaked into the log: %q", log.String())
	}
}

func TestRunFeedsALargeStdinWithoutDeadlocking(t *testing.T) {
	sh := shell(t)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "list")

	// Far more than any pipe buffer: if the feed were not concurrent with the
	// child, this would deadlock and the test would time out.
	const n = 200000
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), runSpec{
			name: "sh", path: sh,
			args: []string{"-c", "cat > " + outFile},
			stdin: func(w io.Writer) error {
				for i := range n {
					if _, err := fmt.Fprintf(w, "path/to/file-%d\x00", i); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run() deadlocked feeding a large stdin")
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	recs := bytes.Split(data, []byte{0})
	// A trailing NUL leaves one empty final element.
	if got := len(recs) - 1; got != n {
		t.Fatalf("child received %d NUL-delimited records, want %d", got, n)
	}
	for _, i := range []int{0, n / 2, n - 1} {
		want := "path/to/file-" + strconv.Itoa(i)
		if got := string(recs[i]); got != want {
			t.Errorf("record %d = %q, want %q", i, got, want)
		}
	}
}

func TestRunToleratesAChildThatIgnoresStdin(t *testing.T) {
	sh := shell(t)
	err := run(context.Background(), runSpec{
		name: "sh", path: sh,
		args: []string{"-c", "exit 0"},
		stdin: func(w io.Writer) error {
			for range 100000 {
				if _, err := w.Write(bytes.Repeat([]byte("x"), 1024)); err != nil {
					return err
				}
			}
			return nil
		},
	})
	// The child never reads: the writes fail with EPIPE, which is not a
	// backup failure.
	if err != nil {
		t.Fatalf("run() = %v, want nil (a broken stdin pipe on success is benign)", err)
	}
}

func TestRunKillsTheChildOnCancellation(t *testing.T) {
	sh := shell(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := run(ctx, runSpec{
		name: "sh", path: sh,
		args: []string{"-c", "trap '' INT; sleep 120"},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("run() = nil, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("run() error = %v, want it to wrap context.Canceled", err)
	}
	// SIGINT is ignored by that shell, so this only returns promptly if the
	// WaitDelay escalation to SIGKILL works.
	if elapsed > 30*time.Second {
		t.Errorf("run() took %v to abort; the child was not killed", elapsed)
	}
}

func TestRunStartFailure(t *testing.T) {
	err := run(context.Background(), runSpec{
		name: "nope", path: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err == nil {
		t.Fatal("run() = nil, want a start failure")
	}
	if !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("run() error = %v, want it to mention the start failure", err)
	}
}

func TestLineWriterHandlesPartialAndUnterminatedLines(t *testing.T) {
	var out bytes.Buffer
	w := &lineWriter{out: &out, tail: tailBuffer{limit: tailLimit}}
	for _, chunk := range []string{"one", " and a half\ntw", "o\r\n", "three (no newline)"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := out.String(), "one and a half\ntwo\n"; got != want {
		t.Errorf("before flush: %q, want %q", got, want)
	}
	w.flush()
	if got, want := out.String(), "one and a half\ntwo\nthree (no newline)\n"; got != want {
		t.Errorf("after flush: %q, want %q", got, want)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := tailBuffer{limit: 10}
	tb.writeString("0123456789abcdef")
	if got, want := tb.String(), "6789abcdef"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

func TestIsPipeClosed(t *testing.T) {
	if !isPipeClosed(io.ErrClosedPipe) {
		t.Error("io.ErrClosedPipe should count as a closed pipe")
	}
	if !isPipeClosed(fmt.Errorf("wrapped: %w", os.ErrClosed)) {
		t.Error("os.ErrClosed should count as a closed pipe")
	}
	if isPipeClosed(errors.New("disk on fire")) {
		t.Error("an unrelated error must not count as a closed pipe")
	}
}

func TestSplitLogSegment(t *testing.T) {
	// Feed the whole input through the splitter the way lineWriter does and
	// collect the segments that would actually be logged.
	drain := func(in string) []string {
		var got []string
		buf := []byte(in)
		for {
			seg, rest, ok := SplitLogSegment(buf)
			if !ok {
				return got
			}
			buf = rest
			if seg != nil {
				got = append(got, string(seg))
			}
		}
	}
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain lines", "one\ntwo\n", []string{"one", "two"}},
		{"crlf is a line ending", "one\r\ntwo\r\n", []string{"one", "two"}},
		{"bare cr progress is dropped", "10%\r20%\r30%\r", nil},
		{
			name: "progress then a real line",
			in:   "Scanning: 99.9%\rTarget: \"x\" - found.\nRepair complete.\n",
			want: []string{"Target: \"x\" - found.", "Repair complete."},
		},
		{"incomplete line is withheld", "partial", nil},
		{"trailing cr is withheld pending crlf", "done\r", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := drain(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("segments = %q, want %q", got, tc.want)
			}
		})
	}
}

// ExitCode is what lets a caller tell unsquashfs's "finished with non-fatal
// errors" (2) from "aborted" (1). It has to read the status out of run()'s
// wrapped error, and answer -1 for every error that carries no status — a
// child that never started, one killed by cancellation — so that no caller
// can mistake "no status" for the one it is looking for.
func TestExitCode(t *testing.T) {
	sh := shell(t)
	for _, want := range []int{1, 2, 3} {
		err := run(context.Background(), runSpec{
			name: "sh", path: sh, args: []string{"-c", "exit " + strconv.Itoa(want)},
		})
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(exit %d) = %d", want, got)
		}
	}
	if got := ExitCode(nil); got != -1 {
		t.Errorf("ExitCode(nil) = %d, want -1", got)
	}
	if got := ExitCode(errors.New("something else")); got != -1 {
		t.Errorf("ExitCode(plain error) = %d, want -1", got)
	}
	err := run(context.Background(), runSpec{name: "nope", path: filepath.Join(t.TempDir(), "missing")})
	if got := ExitCode(err); got != -1 {
		t.Errorf("ExitCode(start failure) = %d, want -1", got)
	}
	// A cancelled child exits by signal, and the error reports the cause, not
	// a status: -1 again, never something a caller would act on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = run(ctx, runSpec{name: "sh", path: sh, args: []string{"-c", "sleep 30"}})
	if got := ExitCode(err); got != -1 {
		t.Errorf("ExitCode(cancelled) = %d, want -1", got)
	}
}
