package ui

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTTYPrinter returns a Printer that believes its writer is a terminal, so
// the progress bar draws into the buffer.
func newTTYPrinter(color bool) (*Printer, *syncBuf) {
	p, buf := newTestPrinter(color)
	p.tty = true
	return p, buf
}

func TestProgressNoOpWithoutTerminal(t *testing.T) {
	p, buf := newTestPrinter(false)
	pr := p.NewProgress("encrypting", 1000)
	for i := 0; i < 100; i++ {
		pr.Add(10)
	}
	pr.Set(1000)
	pr.Done()
	if buf.String() != "" {
		t.Errorf("progress wrote %q to a non-terminal, want nothing", buf.String())
	}
	if pr.Current() != 1000 {
		t.Errorf("Current() = %d, want 1000 (counting must still work)", pr.Current())
	}
}

func TestProgressRendersOnTerminal(t *testing.T) {
	p, buf := newTTYPrinter(false)
	pr := p.NewProgress("encrypting", 25025314816)
	pr.Set(25025314816)
	pr.Done()
	out := buf.String()
	if !strings.Contains(out, "encrypting") {
		t.Errorf("output %q missing the label", out)
	}
	if !strings.Contains(out, "23.31 GiB / 23.31 GiB") {
		t.Errorf("output %q missing the byte counts", out)
	}
	if !strings.Contains(out, "100.0%") {
		t.Errorf("output %q missing the percentage", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Done did not end the line: %q", out)
	}
}

func TestProgressUnknownTotal(t *testing.T) {
	p, buf := newTTYPrinter(false)
	pr := p.NewProgress("hashing", 0)
	pr.Set(1536)
	pr.Done()
	out := buf.String()
	if !strings.Contains(out, "1.50 KiB") {
		t.Errorf("output %q missing the running count", out)
	}
	if strings.Contains(out, "%") {
		t.Errorf("output %q shows a percentage without a known total", out)
	}
}

// TestProgressRateLimited checks the ~10 renders/sec cap: a tight loop of
// updates must not produce a redraw per update.
func TestProgressRateLimited(t *testing.T) {
	p, buf := newTTYPrinter(false)
	pr := p.NewProgress("copying", 1<<20)
	for i := 0; i < 5000; i++ {
		pr.Add(64)
	}
	tight := strings.Count(buf.String(), clrEOL)
	if tight == 0 {
		t.Fatal("no render at all")
	}
	if tight > 4 {
		t.Errorf("%d renders for 5000 updates in a tight loop; the rate limit is not working", tight)
	}
	time.Sleep(2 * renderInterval)
	pr.Add(64)
	if after := strings.Count(buf.String(), clrEOL); after <= tight {
		t.Errorf("no render after waiting %v: %d then %d", 2*renderInterval, tight, after)
	}
	pr.Done()
}

func TestProgressWriterCounts(t *testing.T) {
	p, _ := newTTYPrinter(false)
	pr := p.NewProgress("streaming", 0)
	w := pr.Writer()
	n, err := io.Copy(w, strings.NewReader(strings.Repeat("x", 4096)))
	if err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if n != 4096 {
		t.Fatalf("io.Copy reported %d bytes, want 4096", n)
	}
	if pr.Current() != 4096 {
		t.Errorf("Current() = %d, want 4096", pr.Current())
	}
	pr.Done()
}

func TestProgressDoneIsIdempotent(t *testing.T) {
	p, buf := newTTYPrinter(false)
	pr := p.NewProgress("x", 10)
	pr.Done()
	first := buf.String()
	pr.Done()
	pr.Add(5) // updates after Done must not redraw
	if buf.String() != first {
		t.Errorf("output changed after the first Done: %q then %q", first, buf.String())
	}
}

// TestProgressYieldsToMessages checks that a log line erases the drawn bar
// rather than being appended to it.
func TestProgressYieldsToMessages(t *testing.T) {
	p, buf := newTTYPrinter(false)
	pr := p.NewProgress("working", 100)
	buf.Reset()
	p.OK("finished a step")
	out := buf.String()
	if !strings.HasPrefix(out, clrEOL) {
		t.Errorf("message %q did not erase the bar first", out)
	}
	if !strings.HasSuffix(out, "  ok finished a step\n") {
		t.Errorf("message %q malformed", out)
	}
	pr.Done()
}

func TestProgressNewSupersedesPrevious(t *testing.T) {
	p, buf := newTTYPrinter(false)
	first := p.NewProgress("first", 10)
	second := p.NewProgress("second", 10)
	if strings.Count(buf.String(), "\n") != 1 {
		t.Errorf("starting a second bar did not close the first: %q", buf.String())
	}
	second.Done()
	first.Add(1) // must be inert
	if !strings.Contains(buf.String(), "second") {
		t.Errorf("second bar never drawn: %q", buf.String())
	}
}

func TestProgressConcurrentAdd(t *testing.T) {
	p, _ := newTTYPrinter(false)
	pr := p.NewProgress("parallel", 8*1000*64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				pr.Add(64)
			}
		}()
	}
	wg.Wait()
	pr.Done()
	if got, want := pr.Current(), int64(8*1000*64); got != want {
		t.Errorf("Current() = %d, want %d", got, want)
	}
}

func TestNilProgressIsSafe(t *testing.T) {
	var pr *Progress
	pr.Add(1)
	pr.Set(2)
	pr.Done()
	if pr.Current() != 0 {
		t.Error("nil Progress reported a non-zero count")
	}
}
