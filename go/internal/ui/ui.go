// Package ui provides brb's terminal output: the prefixed, optionally
// coloured status lines, the yes/no and press-Enter prompts, byte formatting
// that matches brb.sh's human() exactly, and a rate-limited progress bar.
//
// The line prefixes and colours are a deliberate port of brb.sh's log/ok/warn/
// die/step helpers so that operators see the same output from both
// implementations. Nothing here ever terminates the process: Fail prints and
// returns, leaving the decision to the caller.
//
// Every message goes out with its terminal control bytes escaped (see
// visible), so a file name or a subprocess line that carries an escape
// sequence is shown to the operator rather than executed by their terminal.
package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrNonInteractive is returned when a prompt is needed but no terminal is
// available to answer it, and when PromptEnter is called while --yes is in
// effect (there is nobody to press Enter, and spinning forever is a bug).
var ErrNonInteractive = errors.New("no terminal available")

// ANSI escapes, matching brb.sh's C_* variables.
const (
	cRed   = "\033[31m"
	cYel   = "\033[33m"
	cGrn   = "\033[32m"
	cBlu   = "\033[34m"
	cDim   = "\033[2m"
	cOff   = "\033[0m"
	clrEOL = "\r\033[2K" // return to column 0 and erase the line
)

// Printer writes brb's status output to a single writer, serialising messages
// against any progress bar that is currently on screen. A Printer is safe for
// concurrent use.
type Printer struct {
	mu        sync.Mutex
	w         io.Writer
	color     bool
	tty       bool // w is a character device, so cursor control works
	assumeYes bool

	// Input for Confirm/PromptEnter, resolved lazily and then cached.
	in      *bufio.Reader
	ttyFile *os.File
	ttyOpen func() (*os.File, error) // overridable in tests
	stdin   *os.File                 // overridable in tests

	prog *Progress // the progress bar currently drawn, if any
}

// New returns a Printer writing to w. color enables the ANSI escapes; callers
// normally pass ColorEnabled(w), which is true only for a terminal with
// NO_COLOR unset.
func New(w io.Writer, color bool) *Printer {
	return &Printer{
		w:       w,
		color:   color,
		tty:     IsTerminal(w),
		ttyOpen: openControllingTTY,
		stdin:   os.Stdin,
	}
}

// IsTerminal reports whether w is an open character device, brb's stdlib-only
// approximation of isatty. Note that this is true for /dev/null and other
// character devices that are not terminals; it is deliberately dependency-free.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isTerminalFile(f)
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// ColorEnabled reports whether colour should be used for w: only when w is a
// terminal, NO_COLOR is unset or empty, and TERM is not "dumb".
func ColorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(w)
}

// Color reports whether this Printer emits ANSI escapes.
func (p *Printer) Color() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.color
}

// Log prints "==> msg" (blue prefix): a major step starting.
func (p *Printer) Log(format string, a ...any) { p.emit("==>", cBlu, format, a...) }

// OK prints "  ok msg" (green prefix): a step that succeeded.
func (p *Printer) OK(format string, a ...any) { p.emit("  ok", cGrn, format, a...) }

// Warn prints "warn msg" (yellow prefix): something the operator should know
// about that does not stop the run.
func (p *Printer) Warn(format string, a ...any) { p.emit("warn", cYel, format, a...) }

// Fail prints "fail msg" (red prefix). It only prints — it never exits and
// never panics; the caller decides what to do about the failure.
func (p *Printer) Fail(format string, a ...any) { p.emit("fail", cRed, format, a...) }

// Step prints "   . msg" (dim prefix): detail under the current Log line.
func (p *Printer) Step(format string, a ...any) { p.emit("   .", cDim, format, a...) }

// Raw prints the formatted message with no prefix and no colour, followed by a
// newline.
func (p *Printer) Raw(format string, a ...any) {
	msg := visible(fmt.Sprintf(format, a...))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hideProgressLocked()
	fmt.Fprintln(p.w, msg)
}

func (p *Printer) emit(prefix, color, format string, a ...any) {
	msg := visible(fmt.Sprintf(format, a...))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hideProgressLocked()
	if p.color {
		fmt.Fprintf(p.w, "%s%s%s %s\n", color, prefix, cOff, msg)
	} else {
		fmt.Fprintf(p.w, "%s %s\n", prefix, msg)
	}
}

// write emits text with no trailing newline, used for prompts.
func (p *Printer) write(s string) {
	s = visible(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hideProgressLocked()
	fmt.Fprint(p.w, s)
}

// visible renders a message so that nothing in it can drive the terminal.
//
// Every message the Printer emits passes through here, whatever it was built
// from. Most are brb's own prose, but a great many carry text brb did not
// write: file names from the scanned tree in scan-problem and oversized-file
// reports, subprocess output forwarded line by line (mksquashfs prints
// "Failed to read file <name>" with the name's bytes intact), disc labels and
// archive names read back from media, and the destination path echoed in a
// Confirm prompt. Anyone who can plant one file in a tree that gets backed up
// picks its name freely, and a name holding ESC ] 0 ; ... BEL retitles the
// operator's window, ESC [ 2 J wipes what they were reading, and worse is
// possible on terminals that answer queries. Escaping in the Printer, rather
// than at each call site, means a caller cannot forget.
//
// What is escaped: every C0 control byte except newline and tab, DEL, and the
// C1 controls U+0080..U+009F (which xterm-compatible terminals honour when
// they arrive UTF-8 encoded, exactly as ESC-bracket sequences). Newline is
// kept because callers deliberately emit multi-line messages through one
// call; tab is kept because it cannot move the cursor anywhere but along the
// current line and subprocess output is often tab-aligned. Bytes that are not
// valid UTF-8 pass through: a terminal shows them as replacement characters,
// which is honest, and rewriting them would misreport the name.
//
// The rendering is the C-style one restore's escapeControls uses for index
// lines — \r for CR, \xNN for anything else, one escape per byte — so an
// operator sees the same spelling for the same name whichever command printed
// it, and can feed it back through printf to reproduce the bytes. The colour
// codes the Printer wraps around a message are added after this runs and are
// never touched.
func visible(s string) string {
	if !needsEscaping(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\n' || c == '\t':
			b.WriteByte(c)
			i++
		case c == '\r':
			b.WriteString(`\r`)
			i++
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
			i++
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r >= 0x80 && r <= 0x9f {
				// A C1 control, UTF-8 encoded: spell out both bytes so the
				// escape round-trips through printf like every other one.
				for _, cb := range []byte(s[i : i+size]) {
					fmt.Fprintf(&b, `\x%02x`, cb)
				}
			} else {
				b.WriteString(s[i : i+size])
			}
			i += size
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// needsEscaping is visible's fast path: almost every message is clean, and
// the printer is on the hot path of every forwarded subprocess line.
func needsEscaping(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			return true
		}
		// 0xC2 is the lead byte of every UTF-8 encoded C1 control.
		if c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f {
			return true
		}
	}
	return false
}

// hideProgressLocked erases a drawn progress bar so a message can take the
// line. The bar redraws on its next update. p.mu must be held.
func (p *Printer) hideProgressLocked() {
	if p.prog != nil && p.prog.shown {
		fmt.Fprint(p.w, clrEOL)
		p.prog.shown = false
	}
}

// SetAssumeYes enables or disables non-interactive mode (--yes). When set,
// Confirm answers yes without asking and PromptEnter refuses rather than
// blocking.
func (p *Printer) SetAssumeYes(yes bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.assumeYes = yes
}

// AssumeYes reports whether non-interactive mode is in effect.
func (p *Printer) AssumeYes() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.assumeYes
}

// SetInput overrides where Confirm and PromptEnter read answers from. By
// default they read the controlling terminal (/dev/tty), falling back to
// standard input when it is a terminal. Passing a reader here makes the
// Printer interactive regardless of terminal state; it is intended for tests
// and for callers that own the input stream themselves.
func (p *Printer) SetInput(r io.Reader) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r == nil {
		p.in = nil
		return
	}
	p.in = bufio.NewReader(r)
}

// Close releases the controlling terminal if this Printer opened one. It is
// safe to call more than once, and on a Printer that never prompted.
func (p *Printer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ttyFile == nil {
		return nil
	}
	f := p.ttyFile
	p.ttyFile = nil
	p.in = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", f.Name(), err)
	}
	return nil
}

func openControllingTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// reader resolves and caches the input source, mirroring brb.sh's order:
// /dev/tty first, then stdin when stdin is a terminal.
func (p *Printer) reader() (*bufio.Reader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.in != nil {
		return p.in, nil
	}
	if p.ttyOpen != nil {
		if f, err := p.ttyOpen(); err == nil && f != nil {
			p.ttyFile = f
			p.in = bufio.NewReader(f)
			return p.in, nil
		}
	}
	if isTerminalFile(p.stdin) {
		p.in = bufio.NewReader(p.stdin)
		return p.in, nil
	}
	return nil, ErrNonInteractive
}

// readLine reads one line, stripping the trailing newline. It reports io.EOF
// only when the stream ended before any bytes of this line were read.
func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && s == "" {
			return "", io.EOF
		}
		if !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading answer: %w", err)
		}
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// Confirm asks a yes/no question, defaulting to no. A "no" answer, or input
// that ends without an answer, returns (false, nil). Under --yes it prints the
// question and returns (true, nil) without reading. With no terminal available
// and --yes not in effect it returns an error wrapping ErrNonInteractive.
func (p *Printer) Confirm(prompt string) (bool, error) {
	if p.AssumeYes() {
		p.Raw("%s [auto-yes]", prompt)
		return true, nil
	}
	r, err := p.reader()
	if err != nil {
		return false, fmt.Errorf("cannot confirm %q, re-run with --yes if you mean it: %w", prompt, err)
	}
	p.write(prompt + " [y/N] ")
	line, err := readLine(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Input ended without an answer: the default, no.
			p.Raw("")
			return false, nil
		}
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// PromptEnter prints prompt and waits for the operator to press Enter. It
// returns io.EOF when the input stream ends, which callers use to terminate
// interactive loops. Under --yes it returns ErrNonInteractive immediately,
// again so that a loop cannot spin forever.
func (p *Printer) PromptEnter(prompt string) error {
	if p.AssumeYes() {
		return fmt.Errorf("cannot wait for Enter at %q under --yes: %w", prompt, ErrNonInteractive)
	}
	r, err := p.reader()
	if err != nil {
		return fmt.Errorf("cannot prompt %q: %w", prompt, err)
	}
	if prompt != "" {
		p.write(prompt)
	}
	if _, err := readLine(r); err != nil {
		if errors.Is(err, io.EOF) {
			p.Raw("")
			return io.EOF
		}
		return err
	}
	return nil
}

// HumanBytes formats a byte count with binary units, exactly as brb.sh's
// human() does: "%.0f B" below 1024, "%.2f" with KiB/MiB/GiB/TiB above,
// dividing by 1024 each step and capping at TiB.
func HumanBytes(n int64) string {
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB"}
	b := float64(n)
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.2f %s", b, units[i])
}

// HumanDuration formats an elapsed time for an operator planning the rest of a
// campaign. Go's own String() renders three quarters of an hour as
// "45m13.478s", which is precision nobody can use when the number that matters
// is "about three quarters of an hour, times twenty discs".
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%d min %02d s", int(d/time.Minute), int(d%time.Minute/time.Second))
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%d h %02d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}
