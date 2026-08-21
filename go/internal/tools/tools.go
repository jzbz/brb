// Package tools wraps the external programs brb drives: mksquashfs and
// unsquashfs for the images, par2 for parity, xorriso for the ISOs, plus the
// optional helpers used around a burn (age, ddrescue, udisksctl, eject,
// findmnt, pv).
//
// Every command line is assembled by a small pure function that returns a
// []string, so argument construction can be unit-tested on a machine where
// none of the binaries are installed. The runners layered on top of those
// functions all take a context.Context, terminate the child process when the
// context is cancelled, and delete partial output.
//
// Two habits that shell makes easy are deliberately not reproduced here: a
// subprocess's stdout is never conflated with a returned value, and an exit
// status is never discarded by piping the tool into a filter. Both cost a
// backup silently — see [(*Set).BuildImage] and [(*Set).MakeISO] for the shape
// of each failure. (brb.sh in this tree is the reader and runs none of these
// writer-side tools; the habits belong to the shell writer this package
// replaced, which is not in the repository.)
package tools

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Names of the external programs brb knows about.
const (
	Mksquashfs = "mksquashfs"
	Unsquashfs = "unsquashfs"
	Age        = "age"
	Par2       = "par2"
	Xorriso    = "xorriso"
	Ddrescue   = "ddrescue"
	Udisksctl  = "udisksctl"
	Eject      = "eject"
	Findmnt    = "findmnt"
	Pv         = "pv"
)

// Known returns every tool name Detect looks for, in report order.
func Known() []string {
	return []string{
		Mksquashfs, Unsquashfs, Age, Par2, Xorriso,
		Ddrescue, Udisksctl, Eject, Findmnt, Pv,
	}
}

// hints maps a tool to the distribution package that usually provides it, so a
// missing-tool error can tell the operator what to install.
var hints = map[string]string{
	Mksquashfs: "squashfs-tools",
	Unsquashfs: "squashfs-tools",
	Age:        "age",
	Par2:       "par2 / par2cmdline",
	Xorriso:    "xorriso",
	Ddrescue:   "gddrescue / ddrescue",
	Udisksctl:  "udisks2",
	Eject:      "util-linux / eject",
	Findmnt:    "util-linux",
	Pv:         "pv",
}

// ErrMissing is the sentinel behind every missing-tool error, so callers can
// branch with errors.Is(err, tools.ErrMissing).
var ErrMissing = errors.New("tool not available")

// MissingError reports every tool that could not be found on PATH. Require
// returns one of these naming all missing tools at once rather than stopping at
// the first.
type MissingError struct {
	// Names holds the missing tool names in the order they were requested.
	Names []string
}

// Error renders the whole missing set in a single line.
func (e *MissingError) Error() string {
	parts := make([]string, 0, len(e.Names))
	for _, n := range e.Names {
		if h := hints[n]; h != "" {
			parts = append(parts, n+" (package: "+h+")")
		} else {
			parts = append(parts, n)
		}
	}
	if len(parts) == 1 {
		return "missing required tool: " + parts[0]
	}
	return "missing required tools: " + strings.Join(parts, ", ")
}

// Unwrap reports ErrMissing so errors.Is works on the sentinel.
func (e *MissingError) Unwrap() error { return ErrMissing }

// Tool describes one external program: where it lives and, when it was cheap to
// ask, which version it reports.
type Tool struct {
	// Name is the program name as it appears on PATH, e.g. "mksquashfs".
	Name string
	// Path is the absolute path found by the lookup, empty when not found.
	Path string
	// Version is the first informative line of the program's version output,
	// or empty when it was not probed or the probe failed.
	Version string
	// Found reports whether the program was located on PATH.
	Found bool
}

// Set is the result of a tool lookup plus the cached answers to the capability
// probes. It is safe for concurrent use.
type Set struct {
	order []string

	mu    sync.Mutex
	tools map[string]Tool

	// mksquashfs help text, probed at most once.
	sqHelpDone bool
	sqHelp     string
}

// probeTimeout bounds each version or capability probe.
const probeTimeout = 10 * time.Second

// versionProbe describes how to ask one tool for its version.
type versionProbe struct {
	args []string
	pick func(string) string
}

var versionProbes = map[string]versionProbe{
	Mksquashfs: {[]string{"-version"}, firstLine},
	Unsquashfs: {[]string{"-version"}, firstLine},
	Age:        {[]string{"--version"}, firstLine},
	Par2:       {[]string{"-V"}, firstLine},
	Xorriso:    {[]string{"--version"}, lineContaining("xorriso version")},
	Ddrescue:   {[]string{"--version"}, firstLine},
	Udisksctl:  {[]string{"--version"}, firstLine},
	Eject:      {[]string{"--version"}, firstLine},
	Findmnt:    {[]string{"--version"}, firstLine},
	Pv:         {[]string{"--version"}, firstLine},
}

// firstLine returns the first non-blank line of s, trimmed.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

// lineContaining returns a picker selecting the first line containing sub
// (case-insensitively), falling back to the first non-blank line.
func lineContaining(sub string) func(string) string {
	low := strings.ToLower(sub)
	return func(s string) string {
		for _, ln := range strings.Split(s, "\n") {
			if strings.Contains(strings.ToLower(ln), low) {
				return strings.TrimSpace(ln)
			}
		}
		return firstLine(s)
	}
}

// Detect looks up every known tool on PATH and, for the ones it finds, captures
// a version string when that is cheap. It never fails: absent tools are simply
// reported as not found. Probes honour ctx and are bounded by a short timeout,
// so a wedged binary cannot stall startup indefinitely.
func Detect(ctx context.Context) *Set {
	names := Known()
	s := &Set{order: names, tools: make(map[string]Tool, len(names))}

	found := make([]Tool, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		t := Tool{Name: name}
		p, err := exec.LookPath(name)
		if err != nil {
			found[i] = t
			continue
		}
		t.Path, t.Found = p, true
		found[i] = t

		probe, ok := versionProbes[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(i int, path string, probe versionProbe) {
			defer wg.Done()
			found[i].Version = probeVersion(ctx, path, probe)
		}(i, p, probe)
	}
	wg.Wait()

	for _, t := range found {
		s.tools[t.Name] = t
	}
	return s
}

// probeVersion runs one version command and picks the informative line out of
// its combined output. A non-zero exit is ignored: several of these tools
// report a version and then exit non-zero.
func probeVersion(ctx context.Context, path string, probe versionProbe) string {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, _ := combinedOutput(pctx, path, probe.args...)
	return probe.pick(out)
}

// NewSet builds a Set from an explicit list of tools without touching PATH. It
// exists for tests and for callers that resolve paths themselves.
func NewSet(ts []Tool) *Set {
	s := &Set{tools: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		s.order = append(s.order, t.Name)
		s.tools[t.Name] = t
	}
	return s
}

// Get returns the named tool. An unknown or missing name yields a zero Tool
// with Found false.
func (s *Set) Get(name string) Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tools[name]
	if !ok {
		return Tool{Name: name}
	}
	return t
}

// Has reports whether the named tool was found.
func (s *Set) Has(name string) bool { return s.Get(name).Found }

// Require reports every one of names that is missing, in a single error. It
// never stops at the first missing tool. The error satisfies
// errors.Is(err, ErrMissing) and unwraps to *MissingError.
func (s *Set) Require(names ...string) error {
	var missing []string
	for _, n := range names {
		if !s.Has(n) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &MissingError{Names: missing}
}

// All returns every tool in the set, in the order it was detected.
func (s *Set) All() []Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Tool, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.tools[n])
	}
	return out
}

// bin resolves the path of a required tool, or returns a *MissingError.
func (s *Set) bin(name string) (string, error) {
	t := s.Get(name)
	if !t.Found || t.Path == "" {
		return "", &MissingError{Names: []string{name}}
	}
	return t.Path, nil
}

// mksquashfsHelp returns mksquashfs's help text, probed at most once.
//
// squashfs-tools 4.6 and newer split the help across sections: "-help" prints
// only a summary and "-help-all" prints everything. Probing with "-help" alone
// and grepping it for "-cpiostyle0" therefore reports a false negative on those
// versions — the flag is supported and the feature detection says it is not, so
// brb refuses to run on a perfectly good mksquashfs. Ask for "-help-all" first
// and fall back to "-help".
func (s *Set) mksquashfsHelp(ctx context.Context) string {
	s.mu.Lock()
	if s.sqHelpDone {
		defer s.mu.Unlock()
		return s.sqHelp
	}
	s.mu.Unlock()

	var help string
	if path, err := s.bin(Mksquashfs); err == nil {
		hctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		out, _ := combinedOutput(hctx, path, "-help-all")
		if !strings.Contains(out, "-cpiostyle0") {
			// Either an older mksquashfs that has no -help-all, or one whose
			// -help is already complete.
			if alt, _ := combinedOutput(hctx, path, "-help"); len(alt) > len(out) ||
				strings.Contains(alt, "-cpiostyle0") {
				out = alt
			}
		}
		help = out
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sqHelpDone {
		s.sqHelp, s.sqHelpDone = help, true
	}
	return s.sqHelp
}

// MksquashfsHasCpioStyle0 reports whether mksquashfs understands -cpiostyle0,
// which brb needs to feed a file list as NUL-delimited stdin. It requires
// squashfs-tools 4.5 or newer. The answer is cached; a missing mksquashfs
// reports false.
func (s *Set) MksquashfsHasCpioStyle0(ctx context.Context) bool {
	return strings.Contains(s.mksquashfsHelp(ctx), "-cpiostyle0")
}

// MksquashfsCompressors returns the compressor names this mksquashfs was built
// with, in the order it lists them. It returns nil when mksquashfs is missing
// or its help text could not be parsed.
func (s *Set) MksquashfsCompressors(ctx context.Context) []string {
	return ParseCompressors(s.mksquashfsHelp(ctx))
}
