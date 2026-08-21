// Package indexfmt implements the escaping contract of brb's on-disc index.
//
// The index is the one artifact that answers "what did I just lose" after a
// disc is destroyed, so it has to be unambiguously parseable by whatever is at
// hand years from now — the on-disc README documents the whole format as
// `awk -F'\t'` over a two-column list. That only holds if a path can never
// introduce a field or a record separator of its own.
//
// Each path is therefore escaped before the row is written, by three
// substitutions in this order: a backslash becomes `\\`, a tab becomes `\t`, a
// newline becomes `\n`. The order matters — escaping the backslash last would
// turn a literal tab into `\\t`, which reads back as a backslash followed by
// 't'. The single-pass loop below produces byte-for-byte what those three
// sequential substitutions produce, and [UnescapePath] reverses them in one
// left-to-right pass, which is the only way to tell `\\t` (backslash, then
// 't') from `\t` (a tab).
//
// This package is the only writer of that form: index.tsv is built by the Go
// implementation alone (internal/backup/index.go), because brb.sh reads disc
// sets and refuses every writer command. The reader that has to agree with it
// is brb.sh's cmd_restore, which escapes a --only pattern with the same three
// substitutions in the same order, in bash parameter expansion, before
// matching it against the raw index field — see the `esc_only=` lines in
// brb.sh, under the comment "the same order the writer uses". Change the order
// here and that match silently stops finding paths containing a tab.
// TestEscapePathMatchesTheThreeSubstitutions holds this package to the three
// substitutions; xcompat-test.sh holds the two implementations to each other.
//
// The result is the guarantee the README states: exactly one row per file,
// always two tab-separated fields, and a path can never span two rows.
package indexfmt

import (
	"fmt"
	"strconv"
	"strings"
)

// EscapePath renders a path so that it occupies exactly one index row.
//
// Backslash, tab and newline become `\\`, `\t` and `\n`; every other byte,
// including any UTF-8 sequence, is passed through untouched. One pass over the
// bytes produces exactly what the format's three sequential substitutions
// produce — backslash, then tab, then newline, the order described on the
// package — and TestEscapePathMatchesTheThreeSubstitutions asserts that
// against a literal transcription of them rather than against prose.
func EscapePath(p string) string {
	if !strings.ContainsAny(p, "\\\t\n") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p) + 8)
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// UnescapePath reverses [EscapePath], recovering the original path bytes.
//
// It never fails. A backslash followed by anything other than `\`, `t` or `n`
// cannot come out of [EscapePath], so it is not a sequence this format defines;
// it is preserved verbatim rather than dropped, because a reader of a damaged
// index is better served by the bytes that are there than by a silent deletion.
func UnescapePath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		switch s[i+1] {
		case '\\':
			b.WriteByte('\\')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

// FormatLine returns one index record — the disc number, a tab, and the escaped
// path — without the terminating newline.
//
// The disc number is deliberately not zero-padded, unlike the image file names:
// the recovery recipe printed on every disc is `awk -F'\t' '$1==3'`.
func FormatLine(disc int, path string) string {
	return strconv.Itoa(disc) + "\t" + EscapePath(path)
}

// ParseLine splits one index record into its disc number and its unescaped
// path. The record must not include the terminating newline.
//
// A record that does not carry a disc number of 1 or more, a tab, and a
// non-empty path is corruption rather than an awkward filename: escaping means
// no path can produce one. Callers are expected to report it, not discard it.
func ParseLine(line string) (disc int, path string, err error) {
	tab := strings.IndexByte(line, '\t')
	if tab < 0 {
		return 0, "", fmt.Errorf("indexfmt: record has no tab: %q", line)
	}
	n, cerr := strconv.Atoi(line[:tab])
	if cerr != nil || n < 1 {
		return 0, "", fmt.Errorf("indexfmt: record has a bad disc number %q: %q", line[:tab], line)
	}
	esc := line[tab+1:]
	if esc == "" {
		return 0, "", fmt.Errorf("indexfmt: record has an empty path: %q", line)
	}
	return n, UnescapePath(esc), nil
}
