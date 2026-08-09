package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/tools"
)

// isolate points HOME and BRB_CONFIG at a temporary directory and clears every
// configuration variable, so that whatever the developer running the tests has
// exported cannot change the result.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRB_CONFIG", filepath.Join(home, "config"))
	t.Setenv("NO_COLOR", "1")
	for _, k := range config.Keys() {
		t.Setenv(config.EnvName(k), "")
	}
	return home
}

// writeConfig writes a configuration file and returns its path.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return p
}

// runMain runs one command line and returns its status and streams.
func runMain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	status := Main(context.Background(), args, &out, &errOut)
	return status, out.String(), errOut.String()
}

func TestMainHelp(t *testing.T) {
	isolate(t)
	status, out, _ := runMain(t, "help")
	if status != exitOK {
		t.Fatalf("help exit = %d, want %d", status, exitOK)
	}
	// Every command must appear in the help, or the help is a lie.
	for _, want := range []string{
		"doctor", "init-key", "plan", "backup", "burn", "verify-disc", "ingest",
		"restore", "mount", "list", "index", "version", "help",
		"--resume", "--verify-roundtrip", "--only", "--disc", "--keep-images",
		"--yes", "--config", "--no-color",
		"ABOUT PACK_RATIO", "PACK_RATIO=0.65",
		"Exit status: 0 success, 1 failure, 2 usage error.",
		"brb " + version,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output does not mention %q", want)
		}
	}
	if strings.Contains(out, "python") {
		t.Errorf("help still mentions python, which the Go port does not use")
	}
}

func TestMainHelpSurvivesABrokenConfigFile(t *testing.T) {
	home := isolate(t)
	writeConfig(t, home, "this is not a config file at all\n")

	status, out, _ := runMain(t, "help")
	if status != exitOK {
		t.Fatalf("help exit = %d, want %d — help must work when the config is broken", status, exitOK)
	}
	if !strings.Contains(out, "SOURCE_DIR=") {
		t.Errorf("help fell back to no configuration at all:\n%s", out)
	}

	// A real command, though, must refuse to run on a config it cannot parse.
	status, _, errOut := runMain(t, "doctor")
	if status != exitError {
		t.Fatalf("doctor exit = %d, want %d on a broken config", status, exitError)
	}
	if !strings.Contains(errOut, "config") {
		t.Errorf("doctor error does not mention the config file:\n%s", errOut)
	}
}

func TestMainVersion(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"version"}, {"--version"}} {
		status, out, _ := runMain(t, args...)
		if status != exitOK {
			t.Fatalf("%q exit = %d, want %d", args, status, exitOK)
		}
		if got, want := strings.TrimSpace(out), "brb "+version; got != want {
			t.Errorf("%q printed %q, want %q", args, got, want)
		}
	}
}

func TestMainWithNoArgumentsShowsHelp(t *testing.T) {
	isolate(t)
	status, out, _ := runMain(t)
	if status != exitOK {
		t.Fatalf("exit = %d, want %d", status, exitOK)
	}
	if !strings.Contains(out, "USAGE") {
		t.Errorf("no-argument output is not the help text:\n%s", out)
	}
}

func TestMainUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"unknown global flag", []string{"--frobnicate", "doctor"}, "unknown flag --frobnicate"},
		{"unknown command flag", []string{"backup", "--fast"}, "backup: unknown flag --fast"},
		{"global flag in the wrong place", []string{"doctor", "--yes"}, "doctor: unknown flag --yes"},
		{"extra argument", []string{"plan", "now"}, `plan: unexpected argument "now"`},
		{"missing argument", []string{"burn"}, "burn: not enough arguments"},
		{"bad disc number", []string{"list", "one"}, `list: "one" is not a disc number`},
		{"bad burn selector", []string{"burn", "some"}, `burn: expected a number, a range like 7-20, or 'all' (got "some")`},
		{"bad iso selector", []string{"iso", "7-3"}, `iso: range "7-3" ends before it starts`},
		{"iso without a selector", []string{"iso"}, "iso: not enough arguments"},
		{"restore without a destination", []string{"restore"}, "restore: not enough arguments"},
		{"restore with two destinations", []string{"restore", "/a", "/b"}, `restore: unexpected argument "/b"`},
		{"mount without a mount point", []string{"mount", "1"}, "mount: not enough arguments"},
		{"verify-disc with too many arguments", []string{"verify-disc", "1", "/mnt", "extra"}, "unexpected argument"},
		{"index with two patterns", []string{"index", "a", "b"}, `index: unexpected argument "b"`},
		{"version takes no arguments", []string{"version", "extra"}, "version: unexpected argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			status, _, errOut := runMain(t, tc.args...)
			if status != exitUsage {
				t.Fatalf("%q exit = %d, want %d\nstderr:\n%s", tc.args, status, exitUsage, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("%q stderr = %q, want it to contain %q", tc.args, errOut, tc.want)
			}
		})
	}
}

// TestIndexPatternIsNotAFlag pins the bug this parser exists to avoid: brb.sh
// strips -y from anywhere on the line, so "brb index -- -y" searches for
// nothing and silently turns on --yes instead.
func TestIndexPatternIsNotAFlag(t *testing.T) {
	g, err := parseGlobals([]string{"index", "--", "-y"})
	if err != nil {
		t.Fatalf("parseGlobals: %v", err)
	}
	if g.assumeYes {
		t.Errorf("-y after the command was taken as the global --yes flag")
	}
	if g.cmd != "index" {
		t.Fatalf("command = %q, want index", g.cmd)
	}
	f := newFlags("index")
	if err := f.parse(g.args); err != nil {
		t.Fatalf("index parse: %v", err)
	}
	if len(f.pos) != 1 || f.pos[0] != "-y" {
		t.Errorf("index pattern = %q, want [-y]", f.pos)
	}
}

func TestDoctorWithoutARecipientsFile(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, home, "SOURCE_DIR="+src+"\nSTAGING="+filepath.Join(home, "staging")+"\n")

	status, _, errOut := runMain(t, "doctor")
	if status != exitError {
		t.Fatalf("doctor exit = %d, want %d without a recipients file\n%s", status, exitError, errOut)
	}
	for _, want := range []string{
		"checking dependencies (backup side)",
		"checking dependencies (restore side",
		"no recipients file",
		"brb init-key",
		"max image size",
		"raw per disc",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("doctor output does not mention %q:\n%s", want, errOut)
		}
	}
}

func TestDoctorWarnsAboutAnInapplicableCompressionLevel(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, home, "SOURCE_DIR="+src+"\nCOMPRESSION=xz\nCOMPRESSION_LEVEL=9\n")

	_, _, errOut := runMain(t, "doctor")
	if !strings.Contains(errOut, "COMPRESSION_LEVEL=9 is ignored for xz") {
		t.Errorf("doctor did not warn that the level is ignored for xz:\n%s", errOut)
	}
}

func TestDoctorIsHappyWithAFreshKey(t *testing.T) {
	home := isolate(t)
	if err := tools.Detect(context.Background()).Require(backupTools...); err != nil {
		t.Skipf("external tools missing: %v", err)
	}
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, home, "SOURCE_DIR="+src+"\nSTAGING="+filepath.Join(home, "staging")+"\n")

	if status, _, errOut := runMain(t, "init-key"); status != exitOK {
		t.Fatalf("init-key exit = %d\n%s", status, errOut)
	}
	status, _, errOut := runMain(t, "doctor")
	if status != exitOK {
		t.Fatalf("doctor exit = %d, want %d\n%s", status, exitOK, errOut)
	}
	if !strings.Contains(errOut, "age round-trip verified") {
		t.Errorf("doctor did not prove the key decrypts what it encrypts:\n%s", errOut)
	}
	if !strings.Contains(errOut, "ready") {
		t.Errorf("doctor did not report ready:\n%s", errOut)
	}
}

func TestInitKey(t *testing.T) {
	home := isolate(t)
	keys := filepath.Join(home, "keys")
	writeConfig(t, home, "AGE_RECIPIENTS_FILE="+filepath.Join(keys, "recipients.txt")+"\n")

	status, _, errOut := runMain(t, "init-key")
	if status != exitOK {
		t.Fatalf("init-key exit = %d\n%s", status, errOut)
	}

	idPath := filepath.Join(keys, "identity.txt")
	fi, err := os.Stat(idPath)
	if err != nil {
		t.Fatalf("identity file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o400 {
		t.Errorf("identity mode = %04o, want 0400", perm)
	}
	recips, err := os.ReadFile(filepath.Join(keys, "recipients.txt"))
	if err != nil {
		t.Fatalf("recipients file: %v", err)
	}
	pub := strings.TrimSpace(string(recips))
	if !strings.HasPrefix(pub, "age1") || strings.Contains(pub, "\n") {
		t.Errorf("recipients file = %q, want exactly one age1 key", pub)
	}
	if !strings.Contains(errOut, pub) {
		t.Errorf("init-key did not print the public key it recorded:\n%s", errOut)
	}
	if !strings.Contains(errOut, "Losing the secret identity means losing every backup") {
		t.Errorf("init-key did not warn about losing the key:\n%s", errOut)
	}

	// A second run must not replace the key that every burned disc depends on.
	status, _, errOut = runMain(t, "init-key")
	if status != exitError {
		t.Fatalf("second init-key exit = %d, want %d", status, exitError)
	}
	if !strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("second init-key error = %q, want a refusal to overwrite", errOut)
	}
	after, err := os.ReadFile(filepath.Join(keys, "recipients.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(recips) {
		t.Errorf("the failed second run changed the recipients file")
	}
}

func TestMainReportsCancellation(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, home, "SOURCE_DIR="+src+"\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut bytes.Buffer
	status := Main(ctx, []string{"plan"}, &out, &errOut)
	if status != exitError {
		t.Fatalf("plan on a cancelled context exit = %d, want %d", status, exitError)
	}
	if !strings.Contains(errOut.String(), "interrupted") {
		t.Errorf("cancellation was not reported:\n%s", errOut.String())
	}
}

func TestMainToleratesNilWriters(t *testing.T) {
	isolate(t)
	if status := Main(nil, []string{"version"}, nil, nil); status != exitOK {
		t.Fatalf("exit = %d with nil writers and context, want %d", status, exitOK)
	}
}

// TestInitKeyRescueRefusesUnderYes pins the guard that keeps the rescue key
// honest: it has to ask for a passphrase on the terminal, and --yes promises a
// run that never asks. The refusal names the flag, the way the restore side
// names it when it meets a passphrase-protected identity under --yes, and
// nothing is written.
//
// The tests deliberately never reach the prompt itself: a test that opens
// /dev/tty would hang for whoever runs `go test` from a terminal. The
// interactive path is exercised by hand under script(1).
func TestInitKeyRescueRefusesUnderYes(t *testing.T) {
	home := isolate(t)
	keys := filepath.Join(home, "keys")
	writeConfig(t, home, "AGE_RECIPIENTS_FILE="+filepath.Join(keys, "recipients.txt")+"\n")

	status, _, errOut := runMain(t, "--yes", "init-key", "--rescue-key")
	if status != exitError {
		t.Fatalf("init-key --rescue-key --yes exit = %d, want %d\n%s", status, exitError, errOut)
	}
	for _, want := range []string{"--rescue-key", "--yes", "passphrase"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(keys, rescueIdentityName)); !os.IsNotExist(err) {
		t.Errorf("a rescue identity was written despite the refusal: %v", err)
	}
	// The primary key is generated before the rescue half is even attempted,
	// so it is there — and it is a whole, usable key, not a fragment.
	recips, err := os.ReadFile(filepath.Join(keys, "recipients.txt"))
	if err != nil {
		t.Fatalf("recipients file: %v", err)
	}
	if n := len(strings.Fields(string(recips))); n != 1 {
		t.Errorf("recipients file has %d key(s), want 1 — the rescue key must not be recorded:\n%s", n, recips)
	}
}

// TestInitKeyRescueLeavesAnExistingIdentityAlone is the "add one later" case
// from the README: on a set that already exists, --rescue-key must not be the
// refusal a bare init-key is. It stops at the --yes guard, which is after the
// point a bare init-key would already have failed.
func TestInitKeyRescueLeavesAnExistingIdentityAlone(t *testing.T) {
	home := isolate(t)
	keys := filepath.Join(home, "keys")
	writeConfig(t, home, "AGE_RECIPIENTS_FILE="+filepath.Join(keys, "recipients.txt")+"\n")

	if status, _, errOut := runMain(t, "init-key"); status != exitOK {
		t.Fatalf("init-key exit = %d\n%s", status, errOut)
	}
	idPath := filepath.Join(keys, "identity.txt")
	before, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatal(err)
	}

	_, _, errOut := runMain(t, "--yes", "init-key", "--rescue-key")
	if strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("--rescue-key refused an existing identity instead of adding to it:\n%s", errOut)
	}
	if !strings.Contains(errOut, "leaving it untouched") {
		t.Errorf("--rescue-key did not say it was leaving the existing identity alone:\n%s", errOut)
	}
	after, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the existing identity was modified")
	}
}

// TestInitKeyRescueFlagIsRecognised is the regression this whole item is: the
// flag README documents was not in the Go build at all, so it came back as
// "unknown flag --rescue-key" with exit 2.
func TestInitKeyRescueFlagIsRecognised(t *testing.T) {
	home := isolate(t)
	writeConfig(t, home, "AGE_RECIPIENTS_FILE="+filepath.Join(home, "keys", "recipients.txt")+"\n")

	status, _, errOut := runMain(t, "--yes", "init-key", "--rescue-key")
	if status == exitUsage || strings.Contains(errOut, "unknown flag") {
		t.Fatalf("init-key --rescue-key is not recognised (exit %d):\n%s", status, errOut)
	}
}

// TestHelpDocumentsWhatRestoreAndTheRescueKeyDo covers the two help gaps: that
// restore overwrites its destination, and what --only's PATH is relative to.
// Help that omits either sends an operator into a live $HOME.
func TestHelpDocumentsWhatRestoreAndTheRescueKeyDo(t *testing.T) {
	isolate(t)
	_, out, _ := runMain(t, "help", "restore")
	for _, want := range []string{
		"OVERWRITING",
		"relative to the archive root",
		"--rescue-key",
		"rescue-identity.txt.age",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not mention %q", want)
		}
	}
}
