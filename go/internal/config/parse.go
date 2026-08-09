package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Value is one setting read from a configuration file. A setting is either a
// scalar (KEY=value) or an array (KEY=( a b c )); IsArray says which, and Line
// is the 1-based line the assignment started on, used in error messages.
type Value struct {
	// Scalar holds the value of a KEY=value assignment. It is empty when
	// IsArray is true.
	Scalar string
	// Array holds the elements of a KEY=( ... ) assignment. It is nil when
	// IsArray is false.
	Array []string
	// IsArray reports whether the assignment used the KEY=( ... ) form.
	IsArray bool
	// Line is the 1-based line number the assignment started on.
	Line int
}

// ErrSyntax is returned, wrapped, for every configuration-file syntax error.
// Callers that want to distinguish a malformed file from an unreadable one can
// test for it with errors.Is.
var ErrSyntax = errors.New("config syntax error")

// ParseFile parses a brb configuration file: the restricted subset of shell
// that brb.sh's `source` of the same file also accepts, so that one file can
// drive both implementations.
//
// Supported:
//
//	KEY=value | KEY="value" | KEY='value' | KEY=( a "b c" 'd' )
//	an optional `export ` prefix, # comments outside quotes, blank lines,
//	arrays spanning several lines, backslash line continuations, and
//	expansion of $HOME, ${HOME} and a leading ~.
//
// Everything else is rejected rather than guessed at: command substitution
// ($( ) or backticks), any variable other than HOME, and shell syntax such as
// pipelines, redirections, conditionals or function definitions. Every error
// names the file, the line number and the offending line, and wraps ErrSyntax.
func ParseFile(path string) (map[string]Value, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	return Parse(string(data), path)
}

// Parse parses configuration text that came from the named source. The name is
// used only in error messages; ParseFile passes the file's path.
func Parse(text, name string) (map[string]Value, error) {
	p := &parser{lines: splitLines(text), name: name, home: homeDir()}
	return p.parse()
}

// splitLines splits on newlines and drops a trailing carriage return, so that
// a CRLF file parses the same as a LF one.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	// A trailing newline produces a final empty element; harmless but drop it
	// so error messages never point past the end of the file.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// parser walks the configuration text one line at a time. li is the 0-based
// index of the current line and col the byte offset within it.
type parser struct {
	lines []string
	li    int
	col   int
	home  string
	name  string
}

func (p *parser) cur() string {
	if p.li < len(p.lines) {
		return p.lines[p.li]
	}
	return ""
}

func (p *parser) eol() bool { return p.col >= len(p.cur()) }

func (p *parser) peek() byte { return p.cur()[p.col] }

func (p *parser) skipSpace() {
	for !p.eol() {
		if c := p.peek(); c == ' ' || c == '\t' {
			p.col++
			continue
		}
		return
	}
}

// advanceLine moves to the next line, reporting whether one exists.
func (p *parser) advanceLine() bool {
	p.li++
	p.col = 0
	return p.li < len(p.lines)
}

// errf builds a syntax error naming the source, the line number and the text
// of that line.
func (p *parser) errf(line int, format string, a ...any) error {
	text := ""
	if line >= 1 && line <= len(p.lines) {
		text = p.lines[line-1]
	}
	return fmt.Errorf("%w: %s:%d: %s: %q",
		ErrSyntax, p.name, line, fmt.Sprintf(format, a...), text)
}

func (p *parser) parse() (map[string]Value, error) {
	out := make(map[string]Value)
	for p.li < len(p.lines) {
		p.col = 0
		p.skipSpace()
		if p.eol() || p.peek() == '#' {
			p.li++
			continue
		}
		start := p.li + 1
		key, err := p.readKey(start)
		if err != nil {
			return nil, err
		}
		v := Value{Line: start}
		if !p.eol() && p.peek() == '(' {
			p.col++
			items, err := p.readArray(start)
			if err != nil {
				return nil, err
			}
			v.IsArray = true
			v.Array = items
		} else {
			s, err := p.readWordOnly(start)
			if err != nil {
				return nil, err
			}
			v.Scalar = s
		}
		if err := p.endStatement(); err != nil {
			return nil, err
		}
		out[key] = v
		p.li++
	}
	return out, nil
}

// readKey consumes an optional `export ` prefix, the variable name and the
// `=`, leaving the position at the first byte of the value.
func (p *parser) readKey(start int) (string, error) {
	name := p.readName()
	if name == "export" && !p.eol() && (p.peek() == ' ' || p.peek() == '\t') {
		p.skipSpace()
		name = p.readName()
	}
	if name == "" {
		return "", p.errf(start, "unsupported shell syntax (expected KEY=value)")
	}
	if p.eol() || p.peek() != '=' {
		if !p.eol() && p.peek() == '+' {
			return "", p.errf(start, "appending with += is not supported")
		}
		return "", p.errf(start, "unsupported shell syntax (only KEY=value assignments are supported)")
	}
	p.col++
	return name, nil
}

// readName reads a shell variable name: a letter or underscore followed by
// letters, digits and underscores. It returns "" when the position is not on
// one.
func (p *parser) readName() string {
	begin := p.col
	line := p.cur()
	for p.col < len(line) {
		c := line[p.col]
		isAlpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isAlpha || (isDigit && p.col > begin) {
			p.col++
			continue
		}
		break
	}
	return line[begin:p.col]
}

// endStatement checks that nothing but whitespace and a comment follows a
// completed assignment.
func (p *parser) endStatement() error {
	p.skipSpace()
	if p.eol() || p.peek() == '#' {
		return nil
	}
	return p.errf(p.li+1, "unexpected text after the value")
}

// readArray reads the elements of a KEY=( ... ) assignment. The opening paren
// has already been consumed. Elements may be spread over any number of lines
// and separated by any whitespace; comments run to the end of their line.
func (p *parser) readArray(start int) ([]string, error) {
	items := []string{}
	for {
		// Skip whitespace, comments and line breaks until something real.
		for {
			p.skipSpace()
			if p.eol() {
				if !p.advanceLine() {
					return nil, p.errf(start, "unterminated array (no closing parenthesis)")
				}
				continue
			}
			if p.peek() == '#' {
				p.col = len(p.cur())
				continue
			}
			break
		}
		if p.peek() == ')' {
			p.col++
			return items, nil
		}
		w, ok, err := p.readWord(')', start)
		if err != nil {
			return nil, err
		}
		if ok {
			items = append(items, w)
		}
	}
}

// readWordOnly reads a scalar value: exactly one shell word, with no array
// terminator in play.
func (p *parser) readWordOnly(start int) (string, error) {
	w, _, err := p.readWord(0, start)
	return w, err
}

// readWord reads one shell word. term, when non-zero, is a byte that ends the
// word without being consumed (')' inside an array). ok is false when the
// input held no word at all — end of line, or a comment.
func (p *parser) readWord(term byte, start int) (string, bool, error) {
	var b strings.Builder
	started := false
	for {
		if p.eol() {
			return b.String(), started, nil
		}
		c := p.peek()
		switch {
		case c == ' ' || c == '\t':
			if started {
				return b.String(), true, nil
			}
			p.col++

		case term != 0 && c == term:
			return b.String(), started, nil

		case c == '#':
			if !started {
				p.col = len(p.cur())
				return "", false, nil
			}
			// A '#' inside a word is literal, exactly as in the shell.
			b.WriteByte('#')
			p.col++

		case c == '\'':
			started = true
			p.col++
			s, err := p.readSingleQuoted(p.li + 1)
			if err != nil {
				return "", false, err
			}
			b.WriteString(s)

		case c == '"':
			started = true
			p.col++
			s, err := p.readDoubleQuoted(p.li + 1)
			if err != nil {
				return "", false, err
			}
			b.WriteString(s)

		case c == '\\':
			p.col++
			if p.eol() {
				// Backslash-newline: a line continuation.
				if !p.advanceLine() {
					return "", false, p.errf(start, "line continuation at end of file")
				}
				continue
			}
			started = true
			b.WriteByte(p.peek())
			p.col++

		case c == '$':
			started = true
			s, err := p.readDollar()
			if err != nil {
				return "", false, err
			}
			b.WriteString(s)

		case c == '~' && !started:
			s, err := p.readTilde()
			if err != nil {
				return "", false, err
			}
			started = true
			b.WriteString(s)

		case c == '`':
			return "", false, p.errf(p.li+1, "command substitution is not supported")

		case c == ';' || c == '&' || c == '|' || c == '<' || c == '>' || c == '(' || c == ')':
			return "", false, p.errf(p.li+1, "unsupported shell syntax %q", string(c))

		default:
			started = true
			b.WriteByte(c)
			p.col++
		}
	}
}

// readSingleQuoted reads to the closing quote. Nothing is special inside single
// quotes, not even a backslash; embedded newlines are kept.
func (p *parser) readSingleQuoted(start int) (string, error) {
	var b strings.Builder
	for {
		if p.eol() {
			if !p.advanceLine() {
				return "", p.errf(start, "unterminated single quote")
			}
			b.WriteByte('\n')
			continue
		}
		c := p.peek()
		p.col++
		if c == '\'' {
			return b.String(), nil
		}
		b.WriteByte(c)
	}
}

// readDoubleQuoted reads to the closing quote, honouring the shell's escapes
// and expanding $HOME.
func (p *parser) readDoubleQuoted(start int) (string, error) {
	var b strings.Builder
	for {
		if p.eol() {
			if !p.advanceLine() {
				return "", p.errf(start, "unterminated double quote")
			}
			b.WriteByte('\n')
			continue
		}
		switch c := p.peek(); c {
		case '"':
			p.col++
			return b.String(), nil
		case '\\':
			p.col++
			if p.eol() {
				// Backslash-newline inside double quotes: continuation.
				if !p.advanceLine() {
					return "", p.errf(start, "unterminated double quote")
				}
				continue
			}
			n := p.peek()
			// Inside double quotes the shell only treats these as escapes.
			if n == '$' || n == '`' || n == '"' || n == '\\' {
				b.WriteByte(n)
			} else {
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			p.col++
		case '`':
			return "", p.errf(p.li+1, "command substitution is not supported")
		case '$':
			s, err := p.readDollar()
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		default:
			b.WriteByte(c)
			p.col++
		}
	}
}

// readDollar expands the one variable reference brb supports, $HOME, in either
// its bare or braced form. Everything else that can follow a '$' is rejected,
// because silently dropping an expansion would produce a wrong path.
func (p *parser) readDollar() (string, error) {
	line := p.li + 1
	p.col++ // consume '$'
	if p.eol() {
		return "$", nil // a trailing '$' is a literal dollar sign
	}
	switch c := p.peek(); {
	case c == '(':
		return "", p.errf(line, "command substitution is not supported")
	case c == '{':
		p.col++
		begin := p.col
		for !p.eol() && p.peek() != '}' {
			p.col++
		}
		if p.eol() {
			return "", p.errf(line, "unterminated ${...}")
		}
		name := p.cur()[begin:p.col]
		p.col++ // consume '}'
		if name != "HOME" {
			return "", p.errf(line, "variable expansion is not supported: ${%s} (only ${HOME} is)", name)
		}
		return p.homeOrErr(line)
	case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		name := p.readName()
		if name != "HOME" {
			return "", p.errf(line, "variable expansion is not supported: $%s (only $HOME is)", name)
		}
		return p.homeOrErr(line)
	case c >= '0' && c <= '9', c == '@', c == '*', c == '#', c == '?', c == '!', c == '$':
		return "", p.errf(line, "variable expansion is not supported: $%s", string(c))
	default:
		return "$", nil // '$' followed by something ordinary is a literal
	}
}

// readTilde expands a leading ~ that stands alone or introduces a path.
func (p *parser) readTilde() (string, error) {
	line := p.li + 1
	p.col++ // consume '~'
	if !p.eol() {
		switch p.peek() {
		case '/', ' ', '\t', ')', '#', '"', '\'':
			// A bare ~ or the start of a ~/path.
		default:
			return "", p.errf(line, "~user expansion is not supported (only a leading ~ or ~/ is)")
		}
	}
	return p.homeOrErr(line)
}

func (p *parser) homeOrErr(line int) (string, error) {
	if p.home == "" {
		return "", p.errf(line, "cannot expand HOME: it is not set in the environment")
	}
	return p.home, nil
}

// homeDir returns the user's home directory the way the shell would: $HOME
// first, falling back to the password database. It returns "" when neither is
// available, and callers turn that into an error only if expansion is asked for.
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// expandHomePath applies the same HOME expansion to a value that did not come
// from the configuration file — an environment variable, whose quoting the
// shell has already removed.
func expandHomePath(s string) string {
	home := homeDir()
	if home == "" {
		return s
	}
	switch {
	case s == "~":
		return home
	case strings.HasPrefix(s, "~/"):
		s = home + s[1:]
	}
	s = strings.ReplaceAll(s, "${HOME}", home)
	// Replace a bare $HOME only when it is not the prefix of a longer name.
	var b strings.Builder
	for {
		i := strings.Index(s, "$HOME")
		if i < 0 {
			b.WriteString(s)
			break
		}
		rest := s[i+len("$HOME"):]
		b.WriteString(s[:i])
		if rest != "" {
			c := rest[0]
			if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				b.WriteString("$HOME")
				s = rest
				continue
			}
		}
		b.WriteString(home)
		s = rest
	}
	return b.String()
}
