package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// usageError marks a command line brb could not make sense of. Main turns one
// into exit status 2; every other error is exit status 1.
type usageError struct{ msg string }

// Error implements error.
func (e *usageError) Error() string { return e.msg }

// usagef builds a usageError.
func usagef(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}

// opt is one recognised flag. takesValue distinguishes "--resume" from
// "--disc 3"; apply receives the value ("" for a boolean).
type opt struct {
	takesValue bool
	apply      func(string) error
}

// cmdFlags is a strict command-line parser for one command.
//
// It is deliberately not the standard flag package: brb needs flags to be
// recognised anywhere after their command (brb.sh accepts "restore /dest
// --only x", which flag.Parse would treat as three positional arguments), and
// it needs an unknown flag to be a hard error naming the flag rather than
// something silently passed through as data. What it does NOT do is brb.sh's
// trick of stripping flags from anywhere on the line, which makes
// "brb index -- -y" search for something other than "-y".
type cmdFlags struct {
	cmd  string
	opts map[string]*opt
	pos  []string
}

// newFlags returns a parser for the named command.
func newFlags(cmd string) *cmdFlags {
	return &cmdFlags{cmd: cmd, opts: make(map[string]*opt)}
}

// add registers one option under every one of its names.
func (f *cmdFlags) add(o *opt, names ...string) {
	for _, n := range names {
		f.opts[n] = o
	}
}

// names returns every flag name registered so far, sorted, so a test can
// check that each one is documented.
func (f *cmdFlags) names() []string {
	out := make([]string, 0, len(f.opts))
	for n := range f.opts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Bool registers a boolean flag that sets *p when present.
func (f *cmdFlags) Bool(p *bool, names ...string) {
	f.add(&opt{apply: func(string) error { *p = true; return nil }}, names...)
}

// StringList registers a repeatable flag that appends each value to *p.
func (f *cmdFlags) StringList(p *[]string, names ...string) {
	f.add(&opt{takesValue: true, apply: func(v string) error {
		if v == "" {
			return fmt.Errorf("value must not be empty")
		}
		*p = append(*p, v)
		return nil
	}}, names...)
}

// DiscNum registers a flag taking a 1-based disc number, parsed by exactly the
// rule the positional disc numbers use.
//
// Zero is the point of the shared rule. RestoreOptions.Disc uses 0 as the
// sentinel for "every disc", so a `restore /home/me --disc 0` accepted as a
// number is indistinguishable from --disc having been left off: it decrypts and
// extracts the whole set over the destination tree, and with --yes there is no
// confirmation to catch it. `brb list 0` and brb.sh both refuse that spelling;
// so does this.
func (f *cmdFlags) DiscNum(p *int, names ...string) {
	f.add(&opt{takesValue: true, apply: func(v string) error {
		n, err := parseDiscNumber(v)
		if err != nil {
			return err
		}
		*p = n
		return nil
	}}, names...)
}

// parse splits args into flags and positional arguments. A bare "--" ends the
// flags: everything after it is positional, so a pattern or path that begins
// with a dash can still be passed. A lone "-" is positional too.
//
// A "--" written where a flag's value belongs is refused rather than consumed
// as that value. getopt would swallow it, but the shape it swallows is exactly
// the one the escape above teaches ("restore --only -- -weird"), and the
// resulting "unknown flag -weird" reads as though the file were not in the
// archive. The refusal names the two spellings that work.
func (f *cmdFlags) parse(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.pos = append(f.pos, args[i+1:]...)
			return nil
		}
		if len(a) < 2 || !strings.HasPrefix(a, "-") {
			f.pos = append(f.pos, a)
			continue
		}
		name, val := a, ""
		hasVal := false
		if j := strings.IndexByte(a, '='); j > 0 {
			name, val, hasVal = a[:j], a[j+1:], true
		}
		o := f.opts[name]
		if o == nil {
			return usagef("%s: unknown flag %s", f.cmd, name)
		}
		if !o.takesValue {
			if hasVal {
				return usagef("%s: flag %s takes no value", f.cmd, name)
			}
			if err := o.apply(""); err != nil {
				return usagef("%s: %s: %v", f.cmd, name, err)
			}
			continue
		}
		if !hasVal {
			i++
			if i >= len(args) {
				return usagef("%s: flag %s needs a value", f.cmd, name)
			}
			if args[i] == "--" {
				return usagef("%s: flag %s needs a value; write %s=VALUE if the value begins "+
					"with a dash, or put it before the --", f.cmd, name, name)
			}
			val = args[i]
		}
		if err := o.apply(val); err != nil {
			return usagef("%s: %s: %v", f.cmd, name, err)
		}
	}
	return nil
}

// need checks the positional argument count. max of -1 means "any number".
// form is the usage line shown when the count is wrong.
func (f *cmdFlags) need(min, max int, form string) error {
	n := len(f.pos)
	if n < min {
		return usagef("%s: not enough arguments\nusage: brb %s", f.cmd, form)
	}
	if max >= 0 && n > max {
		return usagef("%s: unexpected argument %q\nusage: brb %s", f.cmd, f.pos[max], form)
	}
	return nil
}

// parseDiscNumber is the one rule for a disc number anywhere on the command
// line: a decimal integer of at least 1. Discs are numbered from 1 on the
// media, in MANIFEST.txt and in every message, so 0 is not a disc.
func parseDiscNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a disc number", s)
	}
	return n, nil
}

// discNumber parses a positional disc number, naming the command in the error.
func discNumber(cmd, s string) (int, error) {
	n, err := parseDiscNumber(s)
	if err != nil {
		return 0, usagef("%s: %v", cmd, err)
	}
	return n, nil
}
