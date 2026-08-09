package ui

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a bytes.Buffer that tolerates concurrent writers.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// newTestPrinter returns a Printer writing to a buffer, with no terminal and no
// input, so nothing can block on /dev/tty during tests.
func newTestPrinter(color bool) (*Printer, *syncBuf) {
	buf := &syncBuf{}
	p := New(buf, color)
	p.ttyOpen = func() (*os.File, error) { return nil, errors.New("no controlling terminal in tests") }
	p.stdin = nil
	return p, buf
}

// TestHumanBytes pins the format against brb.sh's human(), whose output was
// captured from awk for each of these inputs.
func TestHumanBytes(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"one", 1, "1 B"},
		{"just under a kibibyte", 1023, "1023 B"},
		{"one kibibyte", 1024, "1.00 KiB"},
		{"one and a half kibibytes", 1536, "1.50 KiB"},
		{"one mebibyte", 1024 * 1024, "1.00 MiB"},
		{"reserve default", 104857600, "100.00 MiB"},
		{"one gibibyte", 1024 * 1024 * 1024, "1.00 GiB"},
		{"bd25 capacity", 25025314816, "23.31 GiB"},
		{"one tebibyte", 1099511627776, "1.00 TiB"},
		{"two tebibytes", 2199023255552, "2.00 TiB"},
		{"capped above tebibytes", 1125899906842624, "1024.00 TiB"},
		{"negative", -5, "-5 B"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HumanBytes(tc.in); got != tc.want {
				t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPrinterPrefixesWithoutColor(t *testing.T) {
	tests := []struct {
		name string
		call func(p *Printer)
		want string
	}{
		{"log", func(p *Printer) { p.Log("building %d", 3) }, "==> building 3\n"},
		{"ok", func(p *Printer) { p.OK("done") }, "  ok done\n"},
		{"warn", func(p *Printer) { p.Warn("careful") }, "warn careful\n"},
		{"fail", func(p *Printer) { p.Fail("broken") }, "fail broken\n"},
		{"step", func(p *Printer) { p.Step("detail") }, "   . detail\n"},
		{"raw", func(p *Printer) { p.Raw("plain %s", "text") }, "plain text\n"},
		{"raw empty", func(p *Printer) { p.Raw("") }, "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := newTestPrinter(false)
			tc.call(p)
			if got := buf.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrinterPrefixesWithColor(t *testing.T) {
	tests := []struct {
		name string
		call func(p *Printer)
		want string
	}{
		{"log", func(p *Printer) { p.Log("x") }, "\033[34m==>\033[0m x\n"},
		{"ok", func(p *Printer) { p.OK("x") }, "\033[32m  ok\033[0m x\n"},
		{"warn", func(p *Printer) { p.Warn("x") }, "\033[33mwarn\033[0m x\n"},
		{"fail", func(p *Printer) { p.Fail("x") }, "\033[31mfail\033[0m x\n"},
		{"step", func(p *Printer) { p.Step("x") }, "\033[2m   .\033[0m x\n"},
		{"raw stays plain", func(p *Printer) { p.Raw("x") }, "x\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := newTestPrinter(true)
			tc.call(p)
			if got := buf.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFailDoesNotExit is a guard against reintroducing brb.sh's die(), which
// exits. Reaching the line after Fail is the whole assertion.
func TestFailDoesNotExit(t *testing.T) {
	p, buf := newTestPrinter(false)
	p.Fail("this must not be fatal")
	p.OK("still running")
	if !strings.Contains(buf.String(), "still running") {
		t.Errorf("output = %q, want it to continue past Fail", buf.String())
	}
}

func TestColorEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if ColorEnabled(&bytes.Buffer{}) {
		t.Error("ColorEnabled(non-file) = true, want false")
	}
	dev, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer dev.Close()
	// os.DevNull is a character device, so it stands in for a terminal here.
	if !IsTerminal(dev) {
		t.Skip("this platform's null device is not a character device")
	}
	if !ColorEnabled(dev) {
		t.Error("ColorEnabled(char device) = false, want true")
	}
	t.Setenv("NO_COLOR", "1")
	if ColorEnabled(dev) {
		t.Error("ColorEnabled with NO_COLOR set = true, want false")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if ColorEnabled(dev) {
		t.Error("ColorEnabled with TERM=dumb = true, want false")
	}
}

func TestAssumeYesRoundTrip(t *testing.T) {
	p, _ := newTestPrinter(false)
	if p.AssumeYes() {
		t.Error("AssumeYes() = true on a fresh printer")
	}
	p.SetAssumeYes(true)
	if !p.AssumeYes() {
		t.Error("AssumeYes() = false after SetAssumeYes(true)")
	}
	p.SetAssumeYes(false)
	if p.AssumeYes() {
		t.Error("AssumeYes() = true after SetAssumeYes(false)")
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"y", "y\n", true},
		{"uppercase Y", "Y\n", true},
		{"yes", "yes\n", true},
		{"mixed case yes", "YeS\n", true},
		{"padded yes", "  y  \n", true},
		{"n", "n\n", false},
		{"no", "no\n", false},
		{"empty line defaults to no", "\n", false},
		{"garbage is no", "maybe\n", false},
		{"crlf yes", "y\r\n", true},
		{"eof is no", "", false},
		{"unterminated yes", "y", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := newTestPrinter(false)
			p.SetInput(strings.NewReader(tc.input))
			got, err := p.Confirm("proceed?")
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != tc.want {
				t.Errorf("Confirm(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if !strings.Contains(buf.String(), "proceed? [y/N]") {
				t.Errorf("prompt not shown, output = %q", buf.String())
			}
		})
	}
}

func TestConfirmAssumeYes(t *testing.T) {
	p, buf := newTestPrinter(false)
	p.SetAssumeYes(true)
	// Input that would say "no" must not even be read.
	p.SetInput(strings.NewReader("n\n"))
	got, err := p.Confirm("proceed?")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Error("Confirm under --yes = false, want true")
	}
	if want := "proceed? [auto-yes]\n"; buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}

func TestConfirmNonInteractive(t *testing.T) {
	p, _ := newTestPrinter(false)
	got, err := p.Confirm("proceed?")
	if err == nil {
		t.Fatalf("Confirm with no terminal = %v, nil; want an error", got)
	}
	if !errors.Is(err, ErrNonInteractive) {
		t.Errorf("error %v does not wrap ErrNonInteractive", err)
	}
	if got {
		t.Error("Confirm returned true alongside an error")
	}
}

func TestPromptEnter(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{"enter", "\n", nil},
		{"text then enter", "whatever\n", nil},
		{"eof", "", io.EOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := newTestPrinter(false)
			p.SetInput(strings.NewReader(tc.input))
			err := p.PromptEnter("press Enter: ")
			if !errors.Is(err, tc.wantErr) && !(err == nil && tc.wantErr == nil) {
				t.Fatalf("PromptEnter = %v, want %v", err, tc.wantErr)
			}
			if !strings.Contains(buf.String(), "press Enter:") {
				t.Errorf("prompt not shown, output = %q", buf.String())
			}
		})
	}
}

// TestPromptEnterLoopTerminates is the regression test for brb.sh's
// cmd_ingest, which loops forever under --yes because prompt_enter always
// succeeds. Both non-interactive paths must break the loop.
func TestPromptEnterLoopTerminates(t *testing.T) {
	t.Run("assume yes", func(t *testing.T) {
		p, _ := newTestPrinter(false)
		p.SetAssumeYes(true)
		err := p.PromptEnter("insert the next disc")
		if !errors.Is(err, ErrNonInteractive) {
			t.Fatalf("PromptEnter under --yes = %v, want ErrNonInteractive", err)
		}
	})
	t.Run("no terminal", func(t *testing.T) {
		p, _ := newTestPrinter(false)
		err := p.PromptEnter("insert the next disc")
		if !errors.Is(err, ErrNonInteractive) {
			t.Fatalf("PromptEnter with no terminal = %v, want ErrNonInteractive", err)
		}
	})
	t.Run("input exhausted", func(t *testing.T) {
		p, _ := newTestPrinter(false)
		p.SetInput(strings.NewReader("\n\n"))
		seen := 0
		for i := 0; i < 100; i++ {
			if err := p.PromptEnter(""); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("PromptEnter = %v, want io.EOF", err)
				}
				break
			}
			seen++
		}
		if seen != 2 {
			t.Errorf("loop ran %d times before EOF, want 2", seen)
		}
	})
}

func TestPrinterConcurrentWrites(t *testing.T) {
	p, buf := newTestPrinter(false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				p.Step("worker %d line %d", n, j)
			}
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 8*25 {
		t.Fatalf("got %d lines, want %d", len(lines), 8*25)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "   . worker ") {
			t.Fatalf("interleaved output: %q", l)
		}
	}
}

func TestCloseWithoutTTY(t *testing.T) {
	p, _ := newTestPrinter(false)
	if err := p.Close(); err != nil {
		t.Errorf("Close on a printer that never prompted: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0 s"},
		{-time.Second, "0 s"},
		{1500 * time.Millisecond, "2 s"},
		{59 * time.Second, "59 s"},
		{90 * time.Second, "1 min 30 s"},
		{45*time.Minute + 13*time.Second, "45 min 13 s"},
		{time.Hour, "1 h 00 min"},
		{2*time.Hour + 31*time.Minute, "2 h 31 min"},
	}
	for _, tc := range tests {
		if got := HumanDuration(tc.in); got != tc.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
