package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// tailLimit bounds how much of a subprocess's output an error message carries.
const tailLimit = 4 << 10

// killGrace is how long a cancelled child gets to exit on its own before Go
// kills it outright.
const killGrace = 5 * time.Second

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) writeString(s string) {
	t.buf = append(t.buf, s...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.buf)) }

// lineWriter splits a subprocess's output into lines, drops the ones keep
// rejects, forwards the rest to out and remembers the tail for error messages.
// It is safe for concurrent use because stdout and stderr may share one.
type lineWriter struct {
	mu   sync.Mutex
	part []byte
	keep func(string) bool
	out  io.Writer
	tail tailBuffer
}

// SplitLogSegment consumes one segment from buf.
//
// It reports ok=false when buf holds no complete segment yet. A segment ending
// in "\n" or "\r\n" is a real log line and is returned to be printed. A segment
// ending in a bare "\r" is a progress redraw — par2 and xorriso report percent
// complete by rewriting one terminal line hundreds of times without ever
// emitting a newline — so it is consumed and returned as nil. Logging those
// would turn a single repair into thousands of lines, or, buffered until the
// newline that never comes, one unreadable line megabytes long.
func SplitLogSegment(buf []byte) (seg, rest []byte, ok bool) {
	i := bytes.IndexAny(buf, "\n\r")
	if i < 0 {
		return nil, buf, false
	}
	if buf[i] == '\n' {
		return buf[:i], buf[i+1:], true
	}
	if i+1 >= len(buf) {
		// A trailing CR could still turn out to be CRLF; wait for one more byte.
		return nil, buf, false
	}
	if buf[i+1] == '\n' {
		return buf[:i], buf[i+2:], true
	}
	return nil, buf[i+1:], true
}

// Write implements io.Writer. It never reports an error: losing a log line must
// not fail a multi-hour build.
func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part = append(w.part, p...)
	for {
		seg, rest, ok := SplitLogSegment(w.part)
		if !ok {
			break
		}
		w.part = rest
		if seg != nil {
			w.emit(string(seg))
		}
	}
	// Guard against a tool that never emits a newline.
	if len(w.part) > tailLimit {
		w.emit(string(w.part))
		w.part = w.part[:0]
	}
	return len(p), nil
}

// flush emits any trailing partial line.
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.part) > 0 {
		w.emit(string(bytes.TrimRight(w.part, "\r")))
		w.part = w.part[:0]
	}
}

// emit records and forwards one line. The caller holds w.mu.
func (w *lineWriter) emit(line string) {
	if w.keep != nil && !w.keep(line) {
		return
	}
	w.tail.writeString(line + "\n")
	if w.out != nil {
		fmt.Fprintln(w.out, line)
	}
}

// runSpec is one subprocess invocation.
type runSpec struct {
	// name is the tool name used in error messages.
	name string
	// path is the executable, args the arguments after it.
	path string
	args []string
	// dir is the working directory; empty means the caller's.
	dir string
	// stdin, when set, is run in its own goroutine and feeds the child's stdin.
	// Writing concurrently with the child's execution is what keeps a very
	// large file list from deadlocking against a full pipe buffer.
	stdin func(io.Writer) error
	// stdout, when set, receives the child's stdout verbatim; otherwise stdout
	// is folded into the line log alongside stderr.
	stdout io.Writer
	// log receives the kept output lines; nil discards them.
	log io.Writer
	// filter decides which output lines are worth keeping; nil keeps all.
	filter func(string) bool
}

// run executes one subprocess, honouring ctx: on cancellation the child is
// signalled and then killed, and the error reports the context's cause. The
// exit status is always checked — it is never swallowed by a pipeline.
func run(ctx context.Context, spec runSpec) error {
	cmd := exec.CommandContext(ctx, spec.path, spec.args...)
	cmd.Dir = spec.dir
	// Ask politely first; Go escalates to Kill after WaitDelay.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = killGrace

	lw := &lineWriter{keep: spec.filter, out: spec.log, tail: tailBuffer{limit: tailLimit}}
	if spec.stdout != nil {
		cmd.Stdout = spec.stdout
	} else {
		cmd.Stdout = lw
	}
	cmd.Stderr = lw

	var in io.WriteCloser
	if spec.stdin != nil {
		var err error
		if in, err = cmd.StdinPipe(); err != nil {
			return fmt.Errorf("%s: stdin pipe: %w", spec.name, err)
		}
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: cannot start %s: %w", spec.name, spec.path, err)
	}

	feed := make(chan error, 1)
	if spec.stdin != nil {
		go func() {
			err := spec.stdin(in)
			// Closing signals end-of-list; the child needs it to finish.
			if cerr := in.Close(); err == nil {
				err = cerr
			}
			feed <- err
		}()
	} else {
		feed <- nil
	}

	waitErr := cmd.Wait()
	lw.flush()
	// Wait has closed the stdin pipe by now, so a blocked feeder is unblocked
	// and this receive cannot deadlock.
	feedErr := <-feed

	if waitErr != nil {
		if cerr := context.Cause(ctx); cerr != nil {
			return fmt.Errorf("%s: aborted: %w", spec.name, cerr)
		}
		return fmt.Errorf("%s failed: %w%s", spec.name, waitErr, tailSuffix(&lw.tail))
	}
	if feedErr != nil && !isPipeClosed(feedErr) {
		return fmt.Errorf("%s: feeding input: %w", spec.name, feedErr)
	}
	return nil
}

// tailSuffix renders captured output for an error message, or "" when there was
// none.
func tailSuffix(t *tailBuffer) string {
	s := t.String()
	if s == "" {
		return ""
	}
	return "\n" + s
}

// isPipeClosed reports whether err is the ordinary "the child stopped reading"
// condition rather than a real I/O failure.
func isPipeClosed(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}

// combinedOutput runs a short-lived probe and returns its stdout and stderr
// together. The exit status is returned but callers generally ignore it:
// several of these tools print a version and then exit non-zero.
func combinedOutput(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = time.Second
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
