package restore

import "testing"

// escapeControls has to cover the C1 controls in both of their spellings. A
// terminal decoding UTF-8 acts on U+009B as CSI; one that is not decoding UTF-8
// acts on the bare 0x9b byte exactly the same way. Escaping one spelling and
// not the other leaves the attack working on half the terminals in use, which
// is why ui.go's note insists the two escapers move together.
//
// The case that keeps this honest is the last one. U+4E9B encodes as E4 B8 9B,
// so its final byte is 0x9b — a continuation byte, not a control. Escaping it
// would mangle CJK filenames in every listing, so a byte-at-a-time escaper is
// not good enough here and the test says so.
//
// The tricky values are built from code points rather than written as literals:
// this file stays plain ASCII, and what is under test cannot be quietly altered
// by an editor normalising the source.
func TestEscapeControlsCoversC1InBothSpellings(t *testing.T) {
	var (
		csiUTF8 = string(rune(0x9b))   // U+009B CSI, encoded C2 9B
		oscUTF8 = string(rune(0x9d))   // U+009D OSC, encoded C2 9D
		rawCSI  = string([]byte{0x9b}) // the same control, raw, invalid UTF-8
		rawLow  = string([]byte{0x80}) // bottom of the C1 block, raw
		highRaw = string([]byte{0xff}) // above C1: not a control, left alone
		eAcute  = string(rune(0xe9))   // an ordinary multi-byte rune
		cjk     = string(rune(0x4e9b)) // E4 B8 9B - contains 0x9b as a tail
	)

	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{"plain passes through", "notes.txt", "notes.txt"},
		{"tab is spared: it separates index fields", "a\tb", "a\tb"},
		{"CR and LF get C-style escapes", "a\r\nb", `a\r\nb`},
		{"ESC and BEL", "\x1b]0;PWNED\x07", `\x1b]0;PWNED\x07`},
		{"DEL", "a\x7fb", `a\x7fb`},

		{"C1 as UTF-8: U+009B CSI", "a" + csiUTF8 + "b", `a\xc2\x9bb`},
		{"C1 as UTF-8: U+009D OSC", "a" + oscUTF8 + "b", `a\xc2\x9db`},
		{"C1 raw: bare 0x9b", "a" + rawCSI + "b", `a\x9bb`},
		{"C1 raw: bare 0x80", "a" + rawLow + "b", `a\x80b`},

		{"a raw byte above C1 is left alone", "a" + highRaw + "b", "a" + highRaw + "b"},
		{"an ordinary multi-byte rune survives", "caf" + eAcute + ".txt", "caf" + eAcute + ".txt"},
		{"0x9b inside a valid rune is not a control", "keep" + cjk + "me", "keep" + cjk + "me"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeControls(tt.in); got != tt.want {
				t.Errorf("escapeControls(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}
