package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// setHome points HOME at a fixed value so that ~ and $HOME expansion is
// predictable, clears BRB_CONFIG so DefaultConfigPath is predictable, and
// clears every configuration key so that a variable exported in the developer's
// own shell cannot change what these tests observe.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("BRB_CONFIG", "")
	for _, k := range Keys() {
		t.Setenv(EnvName(k), "")
	}
}

func TestParseScalars(t *testing.T) {
	setHome(t, "/home/tester")

	tests := []struct {
		name string
		in   string
		key  string
		want string
	}{
		{"bare", `SOURCE_DIR=/data`, "SOURCE_DIR", "/data"},
		{"double quoted", `SOURCE_DIR="/data/my files"`, "SOURCE_DIR", "/data/my files"},
		{"single quoted", `SOURCE_DIR='/data/my files'`, "SOURCE_DIR", "/data/my files"},
		{"empty", `ARCHIVE_NAME=`, "ARCHIVE_NAME", ""},
		{"empty quoted", `ARCHIVE_NAME=""`, "ARCHIVE_NAME", ""},
		{"export prefix", `export STAGING=/var/tmp/brb`, "STAGING", "/var/tmp/brb"},
		{"export with tab", "export\tSTAGING=/x", "STAGING", "/x"},
		{"leading whitespace", "  \tSTAGING=/x", "STAGING", "/x"},
		{"trailing comment", `STAGING=/x   # where images live`, "STAGING", "/x"},
		{"hash inside word is literal", `LABEL_PREFIX=a#b`, "LABEL_PREFIX", "a#b"},
		{"hash inside quotes", `LABEL_PREFIX="a # b"`, "LABEL_PREFIX", "a # b"},
		{"quote concatenation", `LABEL_PREFIX="a"'b'c`, "LABEL_PREFIX", "abc"},
		{"tilde alone", `SOURCE_DIR=~`, "SOURCE_DIR", "/home/tester"},
		{"tilde path", `SOURCE_DIR=~/photos`, "SOURCE_DIR", "/home/tester/photos"},
		{"bare HOME", `SOURCE_DIR=$HOME/photos`, "SOURCE_DIR", "/home/tester/photos"},
		{"braced HOME", `SOURCE_DIR=${HOME}/photos`, "SOURCE_DIR", "/home/tester/photos"},
		{"HOME in double quotes", `SOURCE_DIR="$HOME/my photos"`, "SOURCE_DIR", "/home/tester/my photos"},
		{"HOME literal in single quotes", `SOURCE_DIR='$HOME/x'`, "SOURCE_DIR", "$HOME/x"},
		{"dollar paren literal in single quotes", `LABEL_PREFIX='$(date)'`, "LABEL_PREFIX", "$(date)"},
		{"backtick literal in single quotes", "LABEL_PREFIX='`date`'", "LABEL_PREFIX", "`date`"},
		{"escaped space", `LABEL_PREFIX=a\ b`, "LABEL_PREFIX", "a b"},
		{"escaped dollar in quotes", `LABEL_PREFIX="\$HOME"`, "LABEL_PREFIX", "$HOME"},
		{"trailing dollar is literal", `LABEL_PREFIX=x$`, "LABEL_PREFIX", "x$"},
		{"glob is literal", `LABEL_PREFIX=*.pyc`, "LABEL_PREFIX", "*.pyc"},
		{"crlf line ending", "STAGING=/x\r\n", "STAGING", "/x"},
		{"last assignment wins", "STAGING=/a\nSTAGING=/b\n", "STAGING", "/b"},
		{"line continuation", "STAGING=/a\\\nbc\n", "STAGING", "/abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vals, err := Parse(tc.in, "test")
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			v, ok := vals[tc.key]
			if !ok {
				t.Fatalf("Parse(%q): key %s missing, got %v", tc.in, tc.key, vals)
			}
			if v.IsArray {
				t.Fatalf("Parse(%q): %s came back as an array", tc.in, tc.key)
			}
			if v.Scalar != tc.want {
				t.Errorf("Parse(%q): %s = %q, want %q", tc.in, tc.key, v.Scalar, tc.want)
			}
		})
	}
}

func TestParseIgnoresBlanksAndComments(t *testing.T) {
	setHome(t, "/home/tester")
	in := "" +
		"# brb config\n" +
		"\n" +
		"   \t\n" +
		"   # indented comment with KEY=value inside\n" +
		"STAGING=/x\n"
	vals, err := Parse(in, "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(vals) != 1 {
		t.Fatalf("got %d values, want 1: %v", len(vals), vals)
	}
	if vals["STAGING"].Line != 5 {
		t.Errorf("STAGING recorded on line %d, want 5", vals["STAGING"].Line)
	}
}

func TestParseArrays(t *testing.T) {
	setHome(t, "/home/tester")

	tests := []struct {
		name string
		in   string
		key  string
		want []string
	}{
		{"one line", `PRUNE_DIRS=( .cache snap )`, "PRUNE_DIRS", []string{".cache", "snap"}},
		{"no inner spaces", `PRUNE_DIRS=(.cache snap)`, "PRUNE_DIRS", []string{".cache", "snap"}},
		{"empty", `PRUNE_DIRS=()`, "PRUNE_DIRS", []string{}},
		{"empty with space", `PRUNE_DIRS=(  )`, "PRUNE_DIRS", []string{}},
		{"quoted element with space", `PRUNE_DIRS=( "my docs" 'other dir' )`, "PRUNE_DIRS",
			[]string{"my docs", "other dir"}},
		{"tab separated", "PRUNE_DIRS=(\t.cache\tsnap\t)", "PRUNE_DIRS", []string{".cache", "snap"}},
		{"globs stay literal", `EXCLUDE_MASKS=( *.pyc *.pyo core .DS_Store )`, "EXCLUDE_MASKS",
			[]string{"*.pyc", "*.pyo", "core", ".DS_Store"}},
		{
			name: "spanning lines with comments",
			in: "PRUNE_DIRS=(\n" +
				"  .cache            # browser and build caches\n" +
				"  \".local/share/Trash\"\n" +
				"\n" +
				"  # a whole-line comment inside the array\n" +
				"  'go/pkg/mod'\n" +
				")\n",
			key:  "PRUNE_DIRS",
			want: []string{".cache", ".local/share/Trash", "go/pkg/mod"},
		},
		{
			name: "backslash continuation inside array",
			in:   "PRUNE_DIRS=( .cache \\\n  snap )\n",
			key:  "PRUNE_DIRS",
			want: []string{".cache", "snap"},
		},
		{
			name: "closing paren on the element's line",
			in:   "PRUNE_DIRS=(\n  .cache\n  snap)\n",
			key:  "PRUNE_DIRS",
			want: []string{".cache", "snap"},
		},
		{"home expansion in element", `PRUNE_DIRS=( $HOME/junk )`, "PRUNE_DIRS",
			[]string{"/home/tester/junk"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vals, err := Parse(tc.in, "test")
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			v, ok := vals[tc.key]
			if !ok {
				t.Fatalf("key %s missing, got %v", tc.key, vals)
			}
			if !v.IsArray {
				t.Fatalf("%s did not parse as an array: %+v", tc.key, v)
			}
			if !reflect.DeepEqual(v.Array, tc.want) {
				t.Errorf("%s = %q, want %q", tc.key, v.Array, tc.want)
			}
		})
	}
}

func TestParseArrayStatementFollowedByMore(t *testing.T) {
	setHome(t, "/home/tester")
	in := "PRUNE_DIRS=(\n  .cache\n)   # end of list\nSTAGING=/x\n"
	vals, err := Parse(in, "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := vals["STAGING"].Scalar; got != "/x" {
		t.Errorf("STAGING = %q, want /x", got)
	}
	if got := vals["STAGING"].Line; got != 4 {
		t.Errorf("STAGING line = %d, want 4", got)
	}
}

func TestParseRejects(t *testing.T) {
	setHome(t, "/home/tester")

	tests := []struct {
		name     string
		in       string
		wantLine string // substring the message must contain
	}{
		{"command substitution dollar paren", "ARCHIVE_NAME=$(date +%F)\n", ":1:"},
		{"command substitution backtick", "ARCHIVE_NAME=`date`\n", ":1:"},
		{"command substitution in quotes", "STAGING=/x\nARCHIVE_NAME=\"$(hostname)\"\n", ":2:"},
		{"backtick in double quotes", "ARCHIVE_NAME=\"`id`\"\n", ":1:"},
		{"unknown variable", "STAGING=$TMPDIR/brb\n", ":1:"},
		{"unknown braced variable", "STAGING=${TMPDIR}/brb\n", ":1:"},
		{"positional parameter", "STAGING=$1\n", ":1:"},
		{"pipeline", "STAGING=/x | tee /y\n", ":1:"},
		{"semicolon", "STAGING=/x; rm -rf /\n", ":1:"},
		{"redirection", "STAGING=/x > /y\n", ":1:"},
		{"background", "STAGING=/x & \n", ":1:"},
		{"two words", "STAGING=/x /y\n", ":1:"},
		{"not an assignment", "source /etc/other\n", ":1:"},
		{"conditional", "if [ -d /x ]; then STAGING=/x; fi\n", ":1:"},
		{"append", "PRUNE_DIRS+=( a )\n", ":1:"},
		{"unterminated array", "PRUNE_DIRS=( a\n", ":1:"},
		{"unterminated double quote", "STAGING=\"/x\n", ":1:"},
		{"unterminated single quote", "STAGING='/x\n", ":1:"},
		{"stray paren in scalar", "STAGING=/x)\n", ":1:"},
		{"tilde user", "STAGING=~root/x\n", ":1:"},
		{"function definition", "f() { echo hi; }\n", ":1:"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.in, "cfg")
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tc.in)
			}
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("error does not wrap ErrSyntax: %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "cfg"+tc.wantLine) {
				t.Errorf("error %q does not name the source and line (want %q)", msg, "cfg"+tc.wantLine)
			}
			if !strings.Contains(msg, "\"") {
				t.Errorf("error %q does not quote the offending line", msg)
			}
		})
	}
}

func TestParseErrorNamesOffendingText(t *testing.T) {
	setHome(t, "/home/tester")
	in := "STAGING=/x\n\nARCHIVE_NAME=$(date)\n"
	_, err := Parse(in, "brb.conf")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"brb.conf:3", "ARCHIVE_NAME=$(date)", "command substitution"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestParseHomeUnsetIsAnError(t *testing.T) {
	// With no HOME at all, expansion cannot be faked silently.
	t.Setenv("HOME", "")
	if homeDir() != "" {
		t.Skip("os.UserHomeDir still resolves a home directory here")
	}
	if _, err := Parse("SOURCE_DIR=$HOME/x\n", "test"); err == nil {
		t.Error("Parse with no HOME succeeded, want an error")
	}
}

func TestParseFile(t *testing.T) {
	setHome(t, "/home/tester")
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("STAGING=/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if vals["STAGING"].Scalar != "/x" {
		t.Errorf("STAGING = %q", vals["STAGING"].Scalar)
	}

	if _, err := ParseFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("ParseFile on a missing file succeeded, want an error")
	}
}

// realWorldConfig is the kind of file a user actually writes, and it must parse
// under both bash's `source` and ParseFile.
const realWorldConfig = `# ~/.config/brb/config — read by brb.sh and by brb(1)

SOURCE_DIR="$HOME"
STAGING=/var/tmp/brb            # put this on a LUKS volume
AGE_RECIPIENTS_FILE=~/.config/brb/recipients.txt

DISC_TYPE=bdxl100
COMPRESSION=zstd
COMPRESSION_LEVEL=19
BLOCK_SIZE=1M
PACK_RATIO=0.65                 # measured on the first run
PAR2_REDUNDANCY=10

export BURNER=/dev/sr0
BURN_SPEED=4
LABEL_PREFIX=ARCHIVE

PRUNE_DIRS=(
  ".cache"
  ".local/share/Trash"
  "VirtualBox VMs"              # big, and reproducible
  go/pkg/mod
)

EXCLUDE_MASKS=( "*.pyc" '*.pyo' core )
`

func TestParseRealWorldConfig(t *testing.T) {
	setHome(t, "/home/tester")
	vals, err := Parse(realWorldConfig, "config")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	wantScalars := map[string]string{
		"SOURCE_DIR":          "/home/tester",
		"STAGING":             "/var/tmp/brb",
		"AGE_RECIPIENTS_FILE": "/home/tester/.config/brb/recipients.txt",
		"DISC_TYPE":           "bdxl100",
		"COMPRESSION":         "zstd",
		"COMPRESSION_LEVEL":   "19",
		"BLOCK_SIZE":          "1M",
		"PACK_RATIO":          "0.65",
		"PAR2_REDUNDANCY":     "10",
		"BURNER":              "/dev/sr0",
		"BURN_SPEED":          "4",
		"LABEL_PREFIX":        "ARCHIVE",
	}
	for k, want := range wantScalars {
		v, ok := vals[k]
		if !ok {
			t.Errorf("%s missing", k)
			continue
		}
		if v.IsArray || v.Scalar != want {
			t.Errorf("%s = %+v, want scalar %q", k, v, want)
		}
	}

	wantPrune := []string{".cache", ".local/share/Trash", "VirtualBox VMs", "go/pkg/mod"}
	if got := vals["PRUNE_DIRS"]; !got.IsArray || !reflect.DeepEqual(got.Array, wantPrune) {
		t.Errorf("PRUNE_DIRS = %+v, want %q", got, wantPrune)
	}
	wantMasks := []string{"*.pyc", "*.pyo", "core"}
	if got := vals["EXCLUDE_MASKS"]; !got.IsArray || !reflect.DeepEqual(got.Array, wantMasks) {
		t.Errorf("EXCLUDE_MASKS = %+v, want %q", got, wantMasks)
	}

	// And the whole thing must survive Apply and Validate.
	c := Default()
	if err := c.Apply(vals); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c.SourceDir = t.TempDir() // the file's SOURCE_DIR does not exist on this machine
	c.ResolveDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.DiscType != "bdxl100" || c.PackRatio != 0.65 || c.BurnSpeed != 4 {
		t.Errorf("applied config is wrong: %+v", c)
	}
	if !reflect.DeepEqual(c.PruneDirs, wantPrune) {
		t.Errorf("PruneDirs = %q, want %q", c.PruneDirs, wantPrune)
	}
}

func TestExpandHomePath(t *testing.T) {
	setHome(t, "/home/tester")
	tests := []struct{ in, want string }{
		{"~", "/home/tester"},
		{"~/x", "/home/tester/x"},
		{"$HOME/x", "/home/tester/x"},
		{"${HOME}/x", "/home/tester/x"},
		{"$HOMEBREW/x", "$HOMEBREW/x"},
		{"/absolute/~", "/absolute/~"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := expandHomePath(tc.in); got != tc.want {
			t.Errorf("expandHomePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBashExpansionsThisParserWouldHaveKeptAsText pins the four forms that used
// to fall through readDollar's default branch and come out as literal text.
//
// brb.sh sources the same file, so a form bash expands and this parser kept was
// not a parse difference: it was one configuration file naming two different
// things. Measured against bash 5 on the same input before the fix:
//
//	ARCHIVE_NAME=A$'x'B    bash AxB        parser A$xB
//	ARCHIVE_NAME=A$"x"B    bash AxB        parser A$xB
//	ARCHIVE_NAME=A$-B      bash AhmtBcB    parser A$-B
//	ARCHIVE_NAME=A$[1+1]B  bash A2B        parser A$[1+1]B
//
// $- cannot be expanded faithfully by anything that is not the shell, so
// refusing is the only answer that cannot be silently wrong.
func TestBashExpansionsThisParserWouldHaveKeptAsText(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"ANSI-C quoting", `ARCHIVE_NAME=A$'x'B`},
		{"ANSI-C quoting with an escape", `ARCHIVE_NAME=A$'\t'B`},
		{"locale translation", `ARCHIVE_NAME=A$"x"B`},
		{"option flags", `ARCHIVE_NAME=A$-B`},
		{"option flags in double quotes", `ARCHIVE_NAME="A$-B"`},
		{"deprecated arithmetic", `ARCHIVE_NAME=A$[1+1]B`},
		{"deprecated arithmetic in double quotes", `ARCHIVE_NAME="A$[1+1]B"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Parse(tc.in, "test")
			if err == nil {
				t.Fatalf("Parse(%q) accepted it as %q; bash would expand it", tc.in, v["ARCHIVE_NAME"].Scalar)
			}
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("error does not wrap ErrSyntax: %v", err)
			}
		})
	}
}

// TestDollarFormsBashLeavesAloneAreStillAccepted is the companion that stops the
// refusals above from passing by rejecting every dollar sign.
//
// The quoting cases are the ones that matter: inside double or single quotes
// bash does NOT read $'...' as ANSI-C quoting, so both readers already agreed
// there, and refusing them would reject a file brb.sh accepts — trading one
// divergence for another. Each expectation below is what bash 5 produces.
func TestDollarFormsBashLeavesAloneAreStillAccepted(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"dollar quote inside double quotes", `ARCHIVE_NAME="A$'x'B"`, `A$'x'B`},
		{"dollar quote inside single quotes", `ARCHIVE_NAME='A$'"'"'x'"'"'B'`, `A$'x'B`},
		{"dollar dash inside single quotes", `ARCHIVE_NAME='A$-B'`, `A$-B`},
		{"dollar bracket inside single quotes", `ARCHIVE_NAME='A$[1+1]B'`, `A$[1+1]B`},
		{"dollar before an ordinary byte", `ARCHIVE_NAME=A$.B`, `A$.B`},
		{"dollar before a comma", `ARCHIVE_NAME=A$,B`, `A$,B`},
		{"trailing dollar", `ARCHIVE_NAME=AB$`, `AB$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Parse(tc.in, "test")
			if err != nil {
				t.Fatalf("Parse(%q) refused a form bash leaves alone: %v", tc.in, err)
			}
			if got := v["ARCHIVE_NAME"].Scalar; got != tc.want {
				t.Errorf("ARCHIVE_NAME = %q, want %q (what bash gives)", got, tc.want)
			}
		})
	}
}
