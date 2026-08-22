package restore

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// runTool bounds both of a child's streams. Which end each one keeps is not
// interchangeable: stdout is read with firstLine, so only the head is usable,
// and stderr is read for why a program failed, which it says on its way out.
// Getting them the wrong way round loses exactly the part that was wanted, and
// loses it silently.
func TestBoundedWritersKeepTheEndThatIsRead(t *testing.T) {
	t.Run("head keeps the beginning", func(t *testing.T) {
		b := &boundedHead{limit: 10}
		mustWriteAll(t, b, "0123456789abcdefghij")
		if got := b.String(); got != "0123456789" {
			t.Errorf("head = %q, want %q", got, "0123456789")
		}
	})

	t.Run("tail keeps the end", func(t *testing.T) {
		b := &boundedTail{limit: 10}
		mustWriteAll(t, b, "0123456789abcdefghij")
		if got := b.String(); got != "abcdefghij" {
			t.Errorf("tail = %q, want %q", got, "abcdefghij")
		}
	})

	// The child must never block because its output stopped being recorded, so
	// both writers report every byte as written however much they keep.
	t.Run("both accept every write once full", func(t *testing.T) {
		h := &boundedHead{limit: 4}
		t2 := &boundedTail{limit: 4}
		for _, w := range []interface {
			Write([]byte) (int, error)
		}{h, t2} {
			for i := 0; i < 3; i++ {
				n, err := w.Write([]byte("xxxxxxxx"))
				if err != nil || n != 8 {
					t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
				}
			}
		}
	})

	// ddrescue redraws one progress line with bare CRs for hours. The head is
	// what firstLine reads, and it must not grow with the redraws.
	t.Run("a long CR redraw does not grow the head", func(t *testing.T) {
		b := &boundedHead{limit: 64}
		mustWriteAll(t, b, "GNU ddrescue 1.27\n")
		for i := 0; i < 10000; i++ {
			mustWriteAll(t, b, "\rrescued: 1234 MB")
		}
		if len(b.buf) > 64 {
			t.Fatalf("head grew to %d bytes, want <= 64", len(b.buf))
		}
		if got := firstLine(b.String()); got != "GNU ddrescue 1.27" {
			t.Errorf("firstLine = %q, want the banner", got)
		}
	})
}

func mustWriteAll(t *testing.T, w interface {
	Write([]byte) (int, error)
}, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	_ = strings.TrimSpace(s)
}

// The types above are only correct if runTool wires each to the stream that is
// read from that end. Testing them in isolation cannot see a swap, so this runs
// the real thing: a child that prints a recognisable first line to stdout and a
// recognisable last line to stderr, with more than the limit in between.
func TestRunToolKeepsTheHeadOfStdoutAndTheTailOfStderr(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()

	t.Run("stdout head survives a flood", func(t *testing.T) {
		script := `printf 'BANNER\n'; i=0; while [ $i -lt 4000 ]; do printf 'x%.0s' 1 2 3 4 5 6 7 8 9 0; i=$((i+1)); done`
		out, err := runTool(ctx, sh, "-c", script)
		if err != nil {
			t.Fatalf("runTool: %v", err)
		}
		if got := firstLine(out); got != "BANNER" {
			t.Errorf("firstLine(stdout) = %q, want %q — stdout must keep its head", got, "BANNER")
		}
		if len(out) > tailLimit {
			t.Errorf("stdout kept %d bytes, want <= %d", len(out), tailLimit)
		}
	})

	t.Run("stderr tail survives a flood", func(t *testing.T) {
		script := `i=0; while [ $i -lt 4000 ]; do printf 'y%.0s' 1 2 3 4 5 6 7 8 9 0; i=$((i+1)); done >&2; printf '\nWHY-IT-FAILED\n' >&2; exit 3`
		_, err := runTool(ctx, sh, "-c", script)
		if err == nil {
			t.Fatal("runTool returned no error for a child that exited 3")
		}
		if !strings.Contains(err.Error(), "WHY-IT-FAILED") {
			t.Errorf("error does not carry the last line of stderr — stderr must keep its tail:\n%v", err)
		}
	})
}
