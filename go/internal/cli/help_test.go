package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/ui"
)

// TestHelpListsEveryConfigKey pins the README's promise that `brb help` is the
// authoritative list of settings: every key the parser accepts is rendered as
// an assignment in the CONFIGURATION section. PUBLIC_ARCHIVE was accepted for
// a whole release without a line here, which left the README pointing at a
// list that did not have it.
func TestHelpListsEveryConfigKey(t *testing.T) {
	isolate(t)
	_, out, _ := runMain(t, "help")
	listing := configListingLines(t, out)
	for _, k := range config.Keys() {
		found := false
		for _, l := range listing {
			if strings.HasPrefix(l, k+"=") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("help's CONFIGURATION section has no line for %s, which the parser accepts", k)
		}
	}
}

// TestHelpDocumentsEveryBackupFlag reads the flags `brb backup` registers back
// out of the one place they are registered and looks each up in helpText.
// A flag added to backupFlags without a help entry fails here rather than
// shipping undocumented, which is how --public-archive went missing.
func TestHelpDocumentsEveryBackupFlag(t *testing.T) {
	names := backupFlags(&backupOptions{}).names()
	if len(names) < 3 {
		t.Fatalf("backup registers %d flag(s) %q; expected at least --resume, --verify-roundtrip and --public-archive", len(names), names)
	}
	for _, n := range names {
		// The help lays each flag out as "      --name" on its own line, so
		// the match is anchored to that shape and not to a mention in prose.
		if !strings.Contains(helpText, "\n      "+n+" ") {
			t.Errorf("backup accepts %s but helpText has no entry for it", n)
		}
	}
	for _, want := range []string{"--resume", "--verify-roundtrip", "--public-archive"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("backup no longer accepts %s; the help and README both promise it", want)
		}
	}
	// And the setting the flag mirrors is named right beside it.
	if !strings.Contains(helpText, "PUBLIC_ARCHIVE=1") {
		t.Errorf("the --public-archive entry does not name PUBLIC_ARCHIVE=1, its config-file spelling")
	}
}

// TestHelpConfigListingRoundTrips feeds the CONFIGURATION section exactly as
// `brb help` prints it back through the config parser and, when bash is on
// the machine, through bash — the two readers of a config file — and checks
// that both recover the values that were in force. It uses values that need
// quoting (a space, a single quote, a literal $HOME and #, globs) and values
// that render empty (DISC_CAPACITY_BYTES=, DIST_DIR=), which are the two ways
// the rendered sample used to be rejected by the parser that printed it.
//
// The two ratios are deliberately values that %.2f cannot hold: PACK_RATIO=0.625
// is the shape the README's own advice produces ("set PACK_RATIO=0.65"), and
// PACK_RATIO_MARGIN=1.004 rounds to exactly 1.00, which is the margin switched
// off. With the defaults in place — 1.00 and 1.05 — this test passed with the
// rounding intact, so it pinned nothing.
func TestHelpConfigListingRoundTrips(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(home, "My Photos")
	cfg := config.Default()
	cfg.SourceDir = src
	cfg.ArchiveName = "photos 2026 #1 $HOME's ~tilde"
	cfg.LabelPrefix = "FAMILY PHOTOS"
	cfg.PackRatio = 0.625
	cfg.PackRatioMargin = 1.004
	cfg.PruneDirs = []string{".cache", "Old Stuff", "it's"}
	cfg.ExcludeMasks = []string{"*.pyc", "core.[0-9]*", "*~"}
	cfg.ResolveDefaults()

	var out bytes.Buffer
	writeHelp(&out, cfg, "")
	listing := configListingLines(t, out.String())
	text := strings.Join(listing, "\n") + "\n"

	// The Go reader.
	vals, err := config.Parse(text, "brb help")
	if err != nil {
		t.Fatalf("the config listing `brb help` prints does not parse: %v\n%s", err, text)
	}
	got := config.Default()
	if err := got.Apply(vals); err != nil {
		t.Fatalf("the config listing `brb help` prints does not apply: %v\n%s", err, text)
	}
	got.ResolveDefaults()
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("the listing loaded back as a different configuration\n got %+v\nwant %+v\nlisting:\n%s", got, cfg, text)
	}

	// The bash reader: brb.sh sources the same file.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; the parser half of the round trip passed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	// Run in an empty directory so that an unquoted glob, if one slipped
	// through, could not happen to match nothing and pass by luck.
	script := `set -eu; source "$1"
printf '%s\n' "$SOURCE_DIR" "$ARCHIVE_NAME" "$LABEL_PREFIX" "$DISC_CAPACITY_BYTES" "$DIST_DIR"
printf '%s\n' "${PRUNE_DIRS[@]}" "${EXCLUDE_MASKS[@]}"`
	cmd := exec.Command(bash, "-c", script, "bash", file)
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash rejected the config listing `brb help` prints: %v\n%s\nlisting:\n%s", err, raw, text)
	}
	want := append([]string{src, cfg.ArchiveName, cfg.LabelPrefix, "", ""}, cfg.PruneDirs...)
	want = append(want, cfg.ExcludeMasks...)
	if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); !reflect.DeepEqual(got, want) {
		t.Errorf("bash read the listing back as %q, want %q", got, want)
	}
}

// TestHelpDecimalsAreExact pins that a ratio prints as the number in force.
// %.2f rendered a measured PACK_RATIO=0.625 as 0.62 in the listing the README
// calls authoritative, and a PACK_RATIO_MARGIN=1.004 as 1.00 — the safety
// factor gone — while anything under 0.005 printed as 0.00, which Validate
// then refuses. The defaults still print the way the README's sample writes
// them.
func TestHelpDecimalsAreExact(t *testing.T) {
	tests := []struct{ in, want string }{
		{"1", "1.00"},
		{"1.05", "1.05"},
		{"0.625", "0.625"},
		{"1.004", "1.004"},
		{"0.001", "0.001"},
	}
	for _, tc := range tests {
		f, err := strconv.ParseFloat(tc.in, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := decimal(f); got != tc.want {
			t.Errorf("decimal(%v) = %s, want %s", f, got, tc.want)
		}
		back, err := strconv.ParseFloat(decimal(f), 64)
		if err != nil || back != f {
			t.Errorf("decimal(%v) = %s, which loads back as %v", f, decimal(f), back)
		}
	}
}

// TestHelpEscapesConfigValuesForATerminal pins the rule writeHelp applies to
// values it did not write. A config file is data — the README sends operators
// to `brb help` for the key list and tells them the Go build treats a hostile
// config as "an error message, not an execution" — and config.Parse copies an
// ESC byte into a value untouched, with Validate never running on this path.
// Single-quoting makes such a value a shell word again but does not stop a
// terminal from acting on it.
func TestHelpEscapesConfigValuesForATerminal(t *testing.T) {
	isolate(t)
	cfg := config.Default()
	cfg.LabelPrefix = "\x1b[2Jgotcha"
	cfg.ArchiveName = "bell\x07"
	cfg.ExcludeMasks = []string{"esc\x1bmask"}
	cfg.ResolveDefaults()

	// The choice of escaper is made by where the listing is going. A character
	// device stands in for a terminal here, as it does in ui's own tests.
	if dev, err := os.Open(os.DevNull); err != nil {
		t.Logf("cannot open %s, skipping the terminal half: %v", os.DevNull, err)
	} else {
		defer dev.Close()
		if ui.IsTerminal(dev) {
			if got := helpEscaper(dev)("a\x1bb"); got != `a\x1bb` {
				t.Errorf("help to a terminal renders %q as %q, want it escaped", "a\x1bb", got)
			}
		}
	}
	if got := helpEscaper(&bytes.Buffer{})("a\x1bb"); got != "a\x1bb" {
		t.Errorf("help to a buffer renders %q as %q, want it byte-exact", "a\x1bb", got)
	}

	// To a terminal: nothing that can drive it survives.
	for _, l := range configLines(cfg, ui.Visible) {
		if strings.ContainsAny(l, "\x1b\x07\x00\x7f") {
			t.Errorf("terminal listing line %q still carries a control byte", l)
		}
	}
	joined := strings.Join(configLines(cfg, ui.Visible), "\n")
	for _, want := range []string{`\x1b[2Jgotcha`, `bell\x07`, `esc\x1bmask`} {
		if !strings.Contains(joined, want) {
			t.Errorf("terminal listing does not spell out %s:\n%s", want, joined)
		}
	}

	// To anything else — a pipe, a file, this test — the bytes are exact, so
	// the listing still pastes back as the configuration in force.
	var out bytes.Buffer
	writeHelp(&out, cfg, "")
	if !strings.Contains(out.String(), "\x1b[2Jgotcha") {
		t.Errorf("piped listing lost the raw value it must reproduce:\n%s", out.String())
	}
}

// TestConfigWord pins the one rule the configuration listing adds to
// ui.ShellWord: an empty value stays bare, KEY=, for both readers to take as
// the default. Everything else is rendered exactly as ShellWord renders it;
// the value-by-value table lives beside ShellWord in internal/ui.
func TestConfigWord(t *testing.T) {
	if got := configWord(""); got != "" {
		t.Errorf("configWord(%q) = %s, want it bare so the line reads KEY=", "", got)
	}
	for _, s := range []string{"zstd", "/var/tmp/brb", "a b", "*.pyc", "~/photos", "it's", "esc\x1bhere"} {
		if got, want := configWord(s), ui.ShellWord(s); got != want {
			t.Errorf("configWord(%q) = %s, want ShellWord's %s", s, got, want)
		}
	}
}

// helpCleanupLine returns the last line of TYPICAL RUN, the rm -rf that wipes
// staging, with its indent removed.
func helpCleanupLine(t *testing.T, cfg *config.Config) string {
	t.Helper()
	var out bytes.Buffer
	writeHelp(&out, cfg, "")
	_, typical, ok := strings.Cut(out.String(), "\nTYPICAL RUN\n")
	if !ok {
		t.Fatalf("help has no TYPICAL RUN section:\n%s", out.String())
	}
	lines := strings.Split(strings.TrimRight(typical, "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// TestHelpCleanupCommandIsOneShellWord pins the rm -rf that closes TYPICAL
// RUN. STAGING used to be pasted into it bare, so a valid STAGING of
// "/srv/My Staging" came out as `rm -rf /srv/My Staging` — /srv/My and a
// ./Staging wherever the operator pasted it, with the staging tree left where
// it was — two screens below a CONFIGURATION listing that quoted the same value
// correctly. The default must still read the way the README prints it.
func TestHelpCleanupCommandIsOneShellWord(t *testing.T) {
	isolate(t)
	for _, tc := range []struct{ staging, want string }{
		{"/var/tmp/brb", "rm -rf /var/tmp/brb"},
		{"/srv/My Staging", "rm -rf '/srv/My Staging'"},
		{"/srv/a'b", `rm -rf '/srv/a'\''b'`},
		{"/srv/brb*", "rm -rf '/srv/brb*'"},
		{"/srv/$HOME", "rm -rf '/srv/$HOME'"},
	} {
		cfg := config.Default()
		cfg.Staging = tc.staging
		cfg.ResolveDefaults()
		if got := helpCleanupLine(t, cfg); got != tc.want {
			t.Errorf("STAGING=%q: help ends with %q, want %q", tc.staging, got, tc.want)
		}
	}

	// And bash reads each rendering back as exactly one word, the path itself.
	// The glob case gets a sibling for an unquoted * to match, and the run
	// happens in a directory holding a "Staging" for a split path to reach, so
	// neither can pass by luck.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; the rendered strings were checked above")
	}
	dir := t.TempDir()
	for _, sub := range []string{"brbX", "Staging"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, staging := range []string{
		filepath.Join(dir, "My Staging"),
		filepath.Join(dir, "a'b c"),
		filepath.Join(dir, "brb*"),
		filepath.Join(dir, "$HOME x"),
		filepath.Join(dir, "tab\there"),
	} {
		cfg := config.Default()
		cfg.Staging = staging
		cfg.ResolveDefaults()
		line := helpCleanupLine(t, cfg)
		words, ok := strings.CutPrefix(line, "rm -rf ")
		if !ok {
			t.Fatalf("STAGING=%q: help ends with %q, not an rm -rf", staging, line)
		}
		cmd := exec.Command(bash, "-c", `eval "set -- $1" || exit 3; printf '%s\n' "$#"; printf '%s' "$1"`, "bash", words)
		cmd.Dir = dir
		raw, err := cmd.Output()
		if err != nil {
			t.Errorf("STAGING=%q: bash could not read %q back: %v", staging, words, err)
			continue
		}
		n, word, _ := strings.Cut(string(raw), "\n")
		if n != "1" || word != staging {
			t.Errorf("STAGING=%q: bash reads %q as %s word(s), first %q; want exactly the path", staging, words, n, word)
		}
	}
}

// configListingLines extracts the assignment lines of the CONFIGURATION
// section from a full help rendering: the lines indented by four spaces
// between the section header and ABOUT PACK_RATIO, with that indent removed.
func configListingLines(t *testing.T, help string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(help, "\nCONFIGURATION\n")
	if !ok {
		t.Fatalf("help has no CONFIGURATION section:\n%s", help)
	}
	body, _, ok := strings.Cut(rest, "\nABOUT PACK_RATIO\n")
	if !ok {
		t.Fatalf("help's CONFIGURATION section does not end at ABOUT PACK_RATIO:\n%s", help)
	}
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "    ") {
			lines = append(lines, l[4:])
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no assignment lines found in the CONFIGURATION section:\n%s", body)
	}
	return lines
}
