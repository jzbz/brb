package ui

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// renderInterval caps redraws at roughly ten per second. A backup writes tens
// of gigabytes through Add; redrawing per chunk would cost more than the work.
const renderInterval = 100 * time.Millisecond

// barWidth is the number of cells in the drawn bar.
const barWidth = 24

// Progress is a single-line byte counter drawn on the Printer's writer. Add
// and Set are safe to call concurrently. When the writer is not a terminal the
// bar draws nothing at all: Add still counts, but no output is produced, so
// piping brb's output to a file or a log stays readable.
type Progress struct {
	p     *Printer
	label string
	total int64
	on    bool // writer is a terminal
	cur   atomic.Int64

	// Guarded by p.mu.
	last  time.Time
	shown bool
	done  bool
}

// NewProgress returns a progress bar labelled label. total is the expected
// number of bytes; pass 0 when it is unknown, and only the running count is
// shown. Exactly one bar is tracked by a Printer at a time — creating a new
// one finishes the previous one's line. Call Done when the operation ends.
func (p *Printer) NewProgress(label string, total int64) *Progress {
	p.mu.Lock()
	prev := p.prog
	p.mu.Unlock()
	if prev != nil {
		prev.Done()
	}
	pr := &Progress{p: p, label: label, total: total}
	p.mu.Lock()
	pr.on = p.tty
	p.prog = pr
	p.mu.Unlock()
	pr.render(true)
	return pr
}

// Add increases the byte count by n and redraws if enough time has passed.
func (pr *Progress) Add(n int64) {
	if pr == nil {
		return
	}
	pr.cur.Add(n)
	pr.render(false)
}

// Set replaces the byte count with n and redraws if enough time has passed.
func (pr *Progress) Set(n int64) {
	if pr == nil {
		return
	}
	pr.cur.Store(n)
	pr.render(false)
}

// Current returns the number of bytes counted so far.
func (pr *Progress) Current() int64 {
	if pr == nil {
		return 0
	}
	return pr.cur.Load()
}

// Done finishes the bar, drawing its final state on a line of its own. It is
// idempotent, so `defer pr.Done()` alongside an explicit call is harmless.
func (pr *Progress) Done() {
	if pr == nil {
		return
	}
	p := pr.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr.done {
		return
	}
	pr.done = true
	if p.prog == pr {
		p.prog = nil
	}
	if !pr.on {
		return
	}
	fmt.Fprint(p.w, clrEOL+pr.line(pr.cur.Load())+"\n")
	pr.shown = false
}

// Writer returns an io.Writer that discards its input and counts the bytes
// into the bar. It is the sink handed to streaming operations that report
// progress by writing to a tee.
func (pr *Progress) Writer() io.Writer { return progressWriter{pr} }

type progressWriter struct{ pr *Progress }

func (w progressWriter) Write(b []byte) (int, error) {
	w.pr.Add(int64(len(b)))
	return len(b), nil
}

func (pr *Progress) render(force bool) {
	if !pr.on {
		return
	}
	p := pr.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr.done {
		return
	}
	now := time.Now()
	if !force && pr.shown && now.Sub(pr.last) < renderInterval {
		return
	}
	pr.last = now
	fmt.Fprint(p.w, clrEOL+pr.line(pr.cur.Load()))
	pr.shown = true
}

// line renders the bar's text without any cursor control.
func (pr *Progress) line(cur int64) string {
	var b strings.Builder
	if pr.p.color {
		b.WriteString(cDim)
	}
	b.WriteString("   . ")
	b.WriteString(pr.label)
	if pr.p.color {
		b.WriteString(cOff)
	}
	b.WriteString(" ")
	if pr.total > 0 {
		frac := float64(cur) / float64(pr.total)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		filled := int(frac * barWidth)
		fmt.Fprintf(&b, "[%s%s] %5.1f%%  %s / %s",
			strings.Repeat("=", filled),
			strings.Repeat(" ", barWidth-filled),
			frac*100,
			HumanBytes(cur), HumanBytes(pr.total))
	} else {
		b.WriteString(HumanBytes(cur))
	}
	return b.String()
}
