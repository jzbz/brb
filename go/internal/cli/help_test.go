package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
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
func TestHelpConfigListingRoundTrips(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(home, "My Photos")
	cfg := config.Default()
	cfg.SourceDir = src
	cfg.ArchiveName = "photos 2026 #1 $HOME's ~tilde"
	cfg.LabelPrefix = "FAMILY PHOTOS"
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

// TestShellWord pins the rendering rule value by value: bare when nothing in
// it means anything to either reader, single-quoted otherwise, empty left as
// KEY= for both readers to take as the default.
func TestShellWord(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"zstd", "zstd"},
		{"/var/tmp/brb", "/var/tmp/brb"},
		{"1.05", "1.05"},
		{"bd25", "bd25"},
		{"a b", "'a b'"},
		{"*.pyc", "'*.pyc'"},
		{"core.[0-9]*", "'core.[0-9]*'"},
		{"~/photos", "'~/photos'"},
		{"$HOME/x", "'$HOME/x'"},
		{"a#b", "'a#b'"},
		{"it's", `'it'\''s'`},
		{"tab\there", "'tab\there'"},
		{"esc\x1bhere", "'esc\x1bhere'"},
	}
	for _, tc := range tests {
		if got := shellWord(tc.in); got != tc.want {
			t.Errorf("shellWord(%q) = %s, want %s", tc.in, got, tc.want)
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
