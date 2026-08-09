package indexfmt

import (
	"math/rand"
	"strings"
	"testing"
)

// FuzzRoundTrip: the escaper is the only thing standing between a filename and
// the index format, so it must survive arbitrary bytes, not just plausible
// ones.
func FuzzRoundTrip(f *testing.F) {
	for _, s := range []string{"", "a\tb", "c\nd", `e\f`, `\`, `\\\`, `\t`, `\\t`,
		"\xff\xfe", "ünïcøde", "a\\\tb", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		esc := EscapePath(p)
		if strings.ContainsAny(esc, "\t\n") {
			t.Fatalf("EscapePath(%q) = %q: still holds a tab or a newline", p, esc)
		}
		if got := UnescapePath(esc); got != p {
			t.Fatalf("round trip: %q -> %q -> %q", p, esc, got)
		}
		// And the same through a whole record.
		d, back, err := ParseLine(FormatLine(7, p))
		if p == "" {
			return // an empty path is not a record
		}
		if err != nil {
			t.Fatalf("ParseLine(FormatLine(7, %q)): %v", p, err)
		}
		if d != 7 || back != p {
			t.Fatalf("record round trip: %q -> (%d, %q)", p, d, back)
		}
	})
}

// TestRoundTripRandomBytes hammers the same property with random byte strings,
// including sequences that are not valid UTF-8 at all.
func TestRoundTripRandomBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte{'\\', '\t', '\n', 't', 'n', 'a', '/', 0xff, 0xfe, 0x80, 0x00}
	for i := 0; i < 200000; i++ {
		b := make([]byte, rng.Intn(12))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		p := string(b)
		esc := EscapePath(p)
		if strings.ContainsAny(esc, "\t\n") {
			t.Fatalf("EscapePath(%q) = %q: still holds a tab or a newline", p, esc)
		}
		if got := UnescapePath(esc); got != p {
			t.Fatalf("round trip: %q -> %q -> %q", p, esc, got)
		}
	}
}
