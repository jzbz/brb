package indexfmt

import (
	"strings"
	"testing"
)

func TestEscapePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty"},
		{name: "plain", in: "a/b.txt", want: "a/b.txt"},
		{name: "spaces and unicode are literal", in: "My Documents/rés umé.pdf", want: "My Documents/rés umé.pdf"},
		{name: "tab", in: "a\tb.txt", want: `a\tb.txt`},
		{name: "newline", in: "c\nd.txt", want: `c\nd.txt`},
		{name: "backslash", in: `e\f.txt`, want: `e\\f.txt`},
		// The order is the whole point. Escaping the backslash last would turn
		// this into `\\t`, which reads back as a backslash and a 't'.
		{name: "backslash then tab", in: "\\\t", want: `\\\t`},
		{name: "a literal backslash-t is not a tab", in: `a\tb`, want: `a\\tb`},
		{name: "carriage return is not special", in: "a\rb", want: "a\rb"},
		{
			name: "everything at once",
			in:   "sub/mix\t\\and\nnl.txt",
			want: `sub/mix\t\\and\nnl.txt`,
		},
		{
			name: "the forged row", in: "evil\n9\tphantom.txt",
			want: `evil\n9\tphantom.txt`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := EscapePath(tc.in); got != tc.want {
				t.Errorf("EscapePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEscapePathMatchesSequentialSubstitution pins the single pass to the three
// ordered substitutions brb.sh performs, which are the definition of the format.
func TestEscapePathMatchesSequentialSubstitution(t *testing.T) {
	t.Parallel()
	bash := func(p string) string {
		p = strings.ReplaceAll(p, `\`, `\\`)
		p = strings.ReplaceAll(p, "\t", `\t`)
		p = strings.ReplaceAll(p, "\n", `\n`)
		return p
	}
	for _, p := range awkwardPaths() {
		if got, want := EscapePath(p), bash(p); got != want {
			t.Errorf("EscapePath(%q) = %q, brb.sh writes %q", p, got, want)
		}
	}
}

func TestEscapedPathIsOneRow(t *testing.T) {
	t.Parallel()
	for _, p := range awkwardPaths() {
		esc := EscapePath(p)
		if strings.ContainsAny(esc, "\t\n") {
			t.Errorf("EscapePath(%q) = %q still holds a tab or a newline", p, esc)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for _, p := range awkwardPaths() {
		if got := UnescapePath(EscapePath(p)); got != p {
			t.Errorf("round trip of %q gave %q", p, got)
		}
	}
}

func TestUnescapePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty"},
		{name: "plain", in: "a/b.txt", want: "a/b.txt"},
		{name: "tab", in: `a\tb`, want: "a\tb"},
		{name: "newline", in: `a\nb`, want: "a\nb"},
		{name: "backslash", in: `a\\b`, want: `a\b`},
		{name: "escaped backslash then t", in: `a\\tb`, want: `a\tb`},
		{name: "backslash then tab", in: `\\\t`, want: "\\\t"},
		// Neither of these can come out of EscapePath. Damaged input keeps its
		// bytes rather than losing them.
		{name: "an undefined escape is kept verbatim", in: `a\qb`, want: `a\qb`},
		{name: "a trailing backslash is kept", in: `a\`, want: `a\`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := UnescapePath(tc.in); got != tc.want {
				t.Errorf("UnescapePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatLine(t *testing.T) {
	t.Parallel()
	// Unpadded, because the on-disc recipe is awk -F'\t' '$1==3'.
	if got, want := FormatLine(3, "x"), "3\tx"; got != want {
		t.Errorf("FormatLine(3, \"x\") = %q, want %q", got, want)
	}
	if got, want := FormatLine(1, "evil\n9\tphantom.txt"), `1`+"\t"+`evil\n9\tphantom.txt`; got != want {
		t.Errorf("FormatLine = %q, want %q", got, want)
	}
}

func TestParseLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		line     string
		wantDisc int
		wantPath string
		wantErr  string
	}{
		{name: "plain", line: "1\ta", wantDisc: 1, wantPath: "a"},
		{name: "many discs", line: "42\tsome/path", wantDisc: 42, wantPath: "some/path"},
		{name: "escaped", line: `1` + "\t" + `a\tb.txt`, wantDisc: 1, wantPath: "a\tb.txt"},
		{name: "no tab", line: "1 a", wantErr: "no tab"},
		{name: "empty", line: "", wantErr: "no tab"},
		{name: "no disc field", line: "\ta", wantErr: "bad disc number"},
		{name: "zero", line: "0\ta", wantErr: "bad disc number"},
		{name: "negative", line: "-1\ta", wantErr: "bad disc number"},
		{name: "not a number", line: "x\ta", wantErr: "bad disc number"},
		{name: "empty path", line: "1\t", wantErr: "empty path"},
		// Only the first tab separates the fields, and after escaping there is
		// never a second one; a stray one is still not a reason to lose the row.
		{name: "a second raw tab stays in the path", line: "1\ta\tb", wantDisc: 1, wantPath: "a\tb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			disc, path, err := ParseLine(tc.line)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseLine(%q) succeeded, want an error mentioning %q", tc.line, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLine(%q): %v", tc.line, err)
			}
			if disc != tc.wantDisc || path != tc.wantPath {
				t.Errorf("ParseLine(%q) = (%d, %q), want (%d, %q)",
					tc.line, disc, path, tc.wantDisc, tc.wantPath)
			}
		})
	}
}

func TestFormatLineParseLineRoundTrip(t *testing.T) {
	t.Parallel()
	for _, p := range awkwardPaths() {
		if p == "" {
			continue // an empty path is not a file
		}
		line := FormatLine(7, p)
		if n := strings.Count(line, "\t"); n != 1 {
			t.Errorf("FormatLine(7, %q) = %q has %d tabs, want exactly 2 fields", p, line, n)
		}
		if strings.Contains(line, "\n") {
			t.Errorf("FormatLine(7, %q) = %q spans more than one row", p, line)
		}
		disc, got, err := ParseLine(line)
		if err != nil {
			t.Fatalf("ParseLine(%q): %v", line, err)
		}
		if disc != 7 || got != p {
			t.Errorf("round trip of %q gave (%d, %q)", p, disc, got)
		}
	}
}

// awkwardPaths is the set of names the index has to survive, from the
// cross-compatibility probes.
func awkwardPaths() []string {
	return []string{
		"",
		"plain.txt",
		"a\tb.txt",
		"c\nd.txt",
		`e\f.txt`,
		"back\\slash.txt",
		"sub/mix\t\\and\nnl.txt",
		"evil\n9\tphantom.txt",
		"\\",
		"\\\\",
		`\t`,
		`\n`,
		`\\t`,
		"\t\t\n\n\\\\",
		"My Documents/rés umé.pdf",
		"trailing\\",
		"lead\ting\\tab",
		strings.Repeat("a\t\n\\", 64),
	}
}
