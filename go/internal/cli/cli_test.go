package cli

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/backup"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
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

// newTestPrinter returns an uncoloured Printer writing to w, for calling the
// pieces of the CLI directly.
func newTestPrinter(w io.Writer) *ui.Printer { return ui.New(w, false) }

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

// TestExplicitConfigPathMustExist pins brb.sh's rule for -c: a path that names
// nothing is a mistyped path, not a first run, and carrying on with the
// defaults would silently point a restore at /var/tmp/brb — an empty staging
// directory and a report of no discs at all. The default location, by
// contrast, is allowed to be absent.
func TestExplicitConfigPathMustExist(t *testing.T) {
	home := isolate(t)
	missing := filepath.Join(home, "no-such-config")

	for _, args := range [][]string{
		{"-c", missing, "doctor"},
		{"--config=" + missing, "plan"},
	} {
		status, _, errOut := runMain(t, args...)
		if status != exitError {
			t.Fatalf("%q exit = %d, want %d: a -c that names no file must not run on defaults\n%s", args, status, exitError, errOut)
		}
		if !strings.Contains(errOut, "config file not found: "+missing) {
			t.Errorf("%q stderr = %q, want it to name the missing file", args, errOut)
		}
		if !strings.Contains(errOut, "-c") {
			t.Errorf("%q stderr = %q, want it to say the path came from -c", args, errOut)
		}
	}

	// help must still print — it is what an operator reaches for to find out
	// what went wrong — and the default path may be absent without complaint.
	if status, out, _ := runMain(t, "-c", missing, "help"); status != exitOK || !strings.Contains(out, "USAGE") {
		t.Errorf("help with a missing -c exit = %d; help must always print", status)
	}
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCE_DIR", src)
	if status, _, errOut := runMain(t, "plan"); status != exitOK {
		t.Errorf("plan with no config at the default path exit = %d, want %d — only an explicit -c is required to exist\n%s", status, exitOK, errOut)
	}
}

// TestSymlinkedSourceDirIsFollowed covers a SOURCE_DIR that is itself a
// symbolic link to a directory. Validate stats through the link, so doctor
// said ready; the scanner lstats its root and refused it as "not a directory"
// — hours later, if the operator did not run plan first. The link is resolved
// once, when the configuration is loaded, and the operator is told.
func TestSymlinkedSourceDirIsFollowed(t *testing.T) {
	home := isolate(t)
	real := filepath.Join(home, "real-photos")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "photos")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	writeConfig(t, home, "SOURCE_DIR="+link+"\n")

	var errOut bytes.Buffer
	cfg, _, err := loadConfig("", newTestPrinter(&errOut))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := resolveSourceDir(cfg, newTestPrinter(&errOut)); err != nil {
		t.Fatalf("resolveSourceDir: %v", err)
	}
	if cfg.SourceDir != real {
		t.Errorf("SourceDir = %q, want the link's target %q", cfg.SourceDir, real)
	}
	if want := "SOURCE_DIR " + link + " is a symlink to " + real + "; using " + real; !strings.Contains(errOut.String(), want) {
		t.Errorf("the resolution was not reported: stderr = %q, want it to contain %q", errOut.String(), want)
	}
	// The archive is still named after what the operator wrote.
	if !strings.HasPrefix(cfg.ArchiveName, "photos-") {
		t.Errorf("ArchiveName = %q, want it derived from the link name %q", cfg.ArchiveName, "photos")
	}

	// End to end: plan walks the tree, and used to die on the link; doctor
	// reports the resolved source. Restore-side commands never look at
	// SOURCE_DIR and say nothing about it.
	status, _, stderr := runMain(t, "plan")
	if status != exitOK {
		t.Fatalf("plan on a symlinked SOURCE_DIR exit = %d, want %d\n%s", status, exitOK, stderr)
	}
	if strings.Contains(stderr, "not a directory") {
		t.Errorf("plan still refused the symlinked SOURCE_DIR:\n%s", stderr)
	}
	if !strings.Contains(stderr, "is a symlink to "+real) {
		t.Errorf("plan did not say it followed the link:\n%s", stderr)
	}
	if _, _, stderr := runMain(t, "doctor"); !strings.Contains(stderr, "source          "+real) {
		t.Errorf("doctor does not report the resolved source:\n%s", stderr)
	}
	if _, _, stderr := runMain(t, "index"); strings.Contains(stderr, "symlink") {
		t.Errorf("a restore-side command mentioned SOURCE_DIR's symlink:\n%s", stderr)
	}

	// A link to a file is left for Validate to reject as what it is, and a
	// dangling link is an error that names both ends.
	file := filepath.Join(home, "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(home, "file-link")
	if err := os.Symlink(file, fileLink); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.SourceDir = fileLink
	if err := resolveSourceDir(c, newTestPrinter(&errOut)); err != nil || c.SourceDir != fileLink {
		t.Errorf("a link to a file: err = %v, SourceDir = %q; want no error and the path left alone for Validate", err, c.SourceDir)
	}
	dangling := filepath.Join(home, "dangling")
	if err := os.Symlink(filepath.Join(home, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	c.SourceDir = dangling
	if err := resolveSourceDir(c, newTestPrinter(&errOut)); err == nil || !strings.Contains(err.Error(), dangling) {
		t.Errorf("a dangling link: err = %v, want an error naming %s", err, dangling)
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
		// --disc 0 used to parse: 0 is RestoreOptions.Disc's sentinel for
		// "every disc", so it restored the whole set over the destination
		// tree — with -y, without a confirmation — where the operator had
		// asked for one disc. brb.sh refuses it too.
		{"disc zero", []string{"restore", "/dest", "--disc", "0"}, `restore: --disc: "0" is not a disc number`},
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

// TestDoctorUnderPublicArchive pins that doctor knows the mode. A public
// archive mints its own keypair and never reads AGE_RECIPIENTS_FILE, so doctor
// used to fail a correctly configured public set over a missing file the run
// would not have opened — and told the operator to run 'brb init-key', minting
// the long-lived key the mode exists to avoid needing. It also said nothing
// about the set not being confidential.
func TestDoctorUnderPublicArchive(t *testing.T) {
	home := isolate(t)
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, home, "SOURCE_DIR="+src+"\nSTAGING="+filepath.Join(home, "staging")+
		"\nPUBLIC_ARCHIVE=1\nAGE_RECIPIENTS_FILE="+filepath.Join(home, "nope", "recipients.txt")+"\n")

	_, _, errOut := runMain(t, "doctor")
	for _, unwanted := range []string{"no recipients file", "brb init-key"} {
		if strings.Contains(errOut, unwanted) {
			t.Errorf("doctor complains %q about a public archive, which never reads that file:\n%s",
				unwanted, errOut)
		}
	}
	for _, want := range []string{
		"PUBLIC_ARCHIVE=1: this set will NOT be confidential",
		"public archive  1",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("doctor output does not contain %q:\n%s", want, errOut)
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

// TestDoctorAcceptsAPassphraseProtectedIdentity: the README says an identity
// encrypted with `age -p` works anywhere a plain one does, and the restore
// side honours that. Doctor used to feed the container to the identity
// parser, get "no secret keys found", and count it as a problem to fix before
// a backup. Both container spellings — binary and ASCII-armored — are
// recognised, reported as present and passphrase-protected, and are not a
// failure; a file that is neither still is.
func TestDoctorAcceptsAPassphraseProtectedIdentity(t *testing.T) {
	home := isolate(t)
	keys := filepath.Join(home, "keys")
	recips := filepath.Join(keys, "recipients.txt")
	id, err := agecrypt.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.AppendRecipient(recips, id.Recipient().String()); err != nil {
		t.Fatal(err)
	}

	containers := map[string]func(path string){
		"binary": func(path string) {
			if err := agecrypt.WriteEncryptedIdentityFile(path, id, "correct horse"); err != nil {
				t.Fatal(err)
			}
		},
		"armored": func(path string) {
			body := "-----BEGIN AGE ENCRYPTED FILE-----\nYWdlLWVuY3J5cHRpb24ub3JnL3YxCg==\n-----END AGE ENCRYPTED FILE-----\n"
			if err := os.WriteFile(path, []byte(body), 0o400); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range containers {
		t.Run(name, func(t *testing.T) {
			idPath := filepath.Join(home, name+"-identity.txt.age")
			write(idPath)
			cfg := config.Default()
			cfg.AgeRecipientsFile = recips
			cfg.AgeIdentity = idPath

			var errOut bytes.Buffer
			problems := checkKeys(context.Background(), cfg, newTestPrinter(&errOut))
			if problems != 0 {
				t.Errorf("checkKeys counted %d problem(s) for a passphrase-protected identity; want 0\n%s", problems, errOut.String())
			}
			out := errOut.String()
			if !strings.Contains(out, "  ok identity "+idPath+" is passphrase-protected") {
				t.Errorf("the identity was not reported as present and passphrase-protected:\n%s", out)
			}
			if strings.Contains(out, "fail ") {
				t.Errorf("doctor reported a failure for a passphrase-protected identity:\n%s", out)
			}
		})
	}

	// The full command agrees, when the tools it also checks are installed.
	if err := tools.Detect(context.Background()).Require(backupTools...); err == nil {
		idPath := filepath.Join(home, "binary-identity.txt.age")
		src := filepath.Join(home, "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, home, "SOURCE_DIR="+src+"\nAGE_RECIPIENTS_FILE="+recips+"\nAGE_IDENTITY="+idPath+"\n")
		status, _, errOut := runMain(t, "doctor")
		if status != exitOK || !strings.Contains(errOut, "ready") {
			t.Errorf("doctor exit = %d with a passphrase-protected AGE_IDENTITY, want %d and ready\n%s", status, exitOK, errOut)
		}
	}

	// A file that is neither a key nor a container is still a real problem.
	junk := filepath.Join(home, "junk-identity.txt")
	if err := os.WriteFile(junk, []byte("this is not a key\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AgeRecipientsFile = recips
	cfg.AgeIdentity = junk
	var errOut bytes.Buffer
	if problems := checkKeys(context.Background(), cfg, newTestPrinter(&errOut)); problems != 1 {
		t.Errorf("checkKeys counted %d problem(s) for a junk identity; want 1\n%s", problems, errOut.String())
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

// TestInitKeyLeavesOtherPeoplesDirectoriesAlone pins which directories
// init-key makes 0700: one it created itself, and brb's default key directory.
// AGE_IDENTITY=~/identity.txt used to get $HOME chmodded to 0700 unasked; now
// the directory is left as it is and the operator is warned when it is open
// to others.
func TestInitKeyLeavesOtherPeoplesDirectoriesAlone(t *testing.T) {
	dirMode := func(t *testing.T, path string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}

	t.Run("identity in the home directory", func(t *testing.T) {
		home := isolate(t)
		if err := os.Chmod(home, 0o755); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, home, "AGE_IDENTITY="+filepath.Join(home, "identity.txt")+"\n")

		status, _, errOut := runMain(t, "init-key")
		if status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if got := dirMode(t, home); got != 0o755 {
			t.Errorf("$HOME mode = %04o after init-key, want 0755 left alone", got)
		}
		if !strings.Contains(errOut, "warn "+home+" is accessible to others (mode 0755)") {
			t.Errorf("no warning that the identity's directory is open to others:\n%s", errOut)
		}
		if !strings.Contains(errOut, "chmod 700 "+home) {
			t.Errorf("the warning does not say how to tighten it:\n%s", errOut)
		}
		if got := dirMode(t, filepath.Join(home, ".config", "brb")); got != 0o700 {
			t.Errorf("the default recipients directory was created with mode %04o, want 0700", got)
		}
	})

	t.Run("private directory draws no warning", func(t *testing.T) {
		home := isolate(t)
		private := filepath.Join(home, "private")
		if err := os.Mkdir(private, 0o700); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, home, "AGE_IDENTITY="+filepath.Join(private, "identity.txt")+"\n")
		status, _, errOut := runMain(t, "init-key")
		if status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if strings.Contains(errOut, "accessible to others") {
			t.Errorf("a 0700 directory was warned about:\n%s", errOut)
		}
	})

	t.Run("default key directory is made private", func(t *testing.T) {
		home := isolate(t)
		def := filepath.Join(home, ".config", "brb")
		if err := os.MkdirAll(def, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := dirMode(t, def); got != 0o755 {
			t.Skipf("umask left the fixture at %04o", got)
		}
		status, _, errOut := runMain(t, "init-key")
		if status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if got := dirMode(t, def); got != 0o700 {
			t.Errorf("default key directory mode = %04o after init-key, want 0700", got)
		}
		if strings.Contains(errOut, "accessible to others") {
			t.Errorf("brb's own directory was warned about instead of tightened:\n%s", errOut)
		}
	})

	t.Run("directory init-key creates is private", func(t *testing.T) {
		home := isolate(t)
		keys := filepath.Join(home, "keys", "nested")
		writeConfig(t, home, "AGE_RECIPIENTS_FILE="+filepath.Join(keys, "recipients.txt")+"\n")
		if status, _, errOut := runMain(t, "init-key"); status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if got := dirMode(t, keys); got != 0o700 {
			t.Errorf("created key directory mode = %04o, want 0700", got)
		}
	})

	// The rescue identity is written beside the RECIPIENTS file, never beside
	// AGE_IDENTITY. When the two are in different directories the recipients
	// one used to get a bare MkdirAll: no chmod of brb's own default, and no
	// warning about a directory open to others — so the directory that ends up
	// holding a rescue container was the one prepareKeyDir's promise skipped.
	t.Run("recipients directory elsewhere gets the same treatment", func(t *testing.T) {
		home := isolate(t)
		private := filepath.Join(home, "private")
		if err := os.Mkdir(private, 0o700); err != nil {
			t.Fatal(err)
		}
		shared := filepath.Join(home, "shared-keys")
		if err := os.Mkdir(shared, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := dirMode(t, shared); got != 0o755 {
			t.Skipf("umask left the fixture at %04o", got)
		}
		writeConfig(t, home, "AGE_IDENTITY="+filepath.Join(private, "identity.txt")+"\n"+
			"AGE_RECIPIENTS_FILE="+filepath.Join(shared, "recipients.txt")+"\n")

		status, _, errOut := runMain(t, "init-key")
		if status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if got := dirMode(t, shared); got != 0o755 {
			t.Errorf("the recipients directory mode = %04o, want 0755 left alone", got)
		}
		if !strings.Contains(errOut, "warn "+shared+" is accessible to others (mode 0755)") {
			t.Errorf("no warning that the directory the rescue key would land in is open to others:\n%s", errOut)
		}
	})

	t.Run("default recipients directory is made private from elsewhere", func(t *testing.T) {
		home := isolate(t)
		private := filepath.Join(home, "private")
		if err := os.Mkdir(private, 0o700); err != nil {
			t.Fatal(err)
		}
		def := filepath.Join(home, ".config", "brb")
		if err := os.MkdirAll(def, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := dirMode(t, def); got != 0o755 {
			t.Skipf("umask left the fixture at %04o", got)
		}
		writeConfig(t, home, "AGE_IDENTITY="+filepath.Join(private, "identity.txt")+"\n")

		status, _, errOut := runMain(t, "init-key")
		if status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if got := dirMode(t, def); got != 0o700 {
			t.Errorf("the default recipients directory mode = %04o after init-key, want 0700", got)
		}
	})

	t.Run("existing non-default recipients directory is left alone", func(t *testing.T) {
		home := isolate(t)
		shared := filepath.Join(home, "shared-keys")
		if err := os.Mkdir(shared, 0o750); err != nil {
			t.Fatal(err)
		}
		if got := dirMode(t, shared); got != 0o750 {
			t.Skipf("umask left the fixture at %04o", got)
		}
		writeConfig(t, home, "AGE_RECIPIENTS_FILE="+filepath.Join(shared, "recipients.txt")+"\n")
		status, _, errOut := runMain(t, "init-key")
		if status != exitOK {
			t.Fatalf("init-key exit = %d\n%s", status, errOut)
		}
		if got := dirMode(t, shared); got != 0o750 {
			t.Errorf("shared key directory mode = %04o after init-key, want 0750 left alone", got)
		}
		if !strings.Contains(errOut, "warn "+shared+" is accessible to others (mode 0750)") {
			t.Errorf("no warning about the group-accessible directory:\n%s", errOut)
		}
	})
}

// TestInitKeyRefusesToBuildARecipientSetAgeWontEncryptTo: age refuses to
// encrypt to a set that mixes post-quantum and classic recipients, init-key
// can only ever mint classic ones, and AppendRecipient validates a key in
// isolation. So init-key against a post-quantum recipients file used to exit
// 0, print the key it had recorded, and hand the operator an archive whose
// every backup dies in preflight — with an identity file a second init-key
// then refuses to overwrite. The refusal has to come before anything is
// written, and it has to leave the recipients file byte-identical so the
// operator can fix it and retry.
func TestInitKeyRefusesToBuildARecipientSetAgeWontEncryptTo(t *testing.T) {
	home := isolate(t)
	keys := filepath.Join(home, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	recips := filepath.Join(keys, "recipients.txt")
	writeConfig(t, home, "AGE_RECIPIENTS_FILE="+recips+"\n")

	pq, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	before := pq.Recipient().String() + "\n"
	if err := os.WriteFile(recips, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	status, _, errOut := runMain(t, "init-key")
	if status != exitError {
		t.Fatalf("init-key exit = %d against a post-quantum recipients file, want %d\n%s", status, exitError, errOut)
	}
	if !strings.Contains(errOut, "post-quantum") {
		t.Errorf("the refusal does not say what is wrong with the set:\n%s", errOut)
	}
	after, err := os.ReadFile(recips)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("the recipients file was changed by a run that refused:\n%s", after)
	}
	if _, err := os.Lstat(filepath.Join(keys, "identity.txt")); !os.IsNotExist(err) {
		t.Errorf("an identity was written despite the refusal (%v) — a later init-key would refuse to replace it", err)
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

// TestDoctorReportsTheBudgetTheRunWillUse pins doctor's per-disc figure to the
// packer's own function rather than to a second copy of the arithmetic.
//
// doctor used to carry its own rawPerDisc, whose comment claimed it was "the
// same arithmetic internal/backup plans with — doctor's number has to be the
// number the run will use, or it is worse than none". It guarded only a
// non-positive ratio, with no infinity check and no floor, so PACK_RATIO=1e-10
// printed "raw per disc -9223372036854775808 B" while plan reported 1 B from
// the same config and the same budget.
func TestDoctorReportsTheBudgetTheRunWillUse(t *testing.T) {
	cfg := config.Default()
	b, err := cfg.Budget()
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	for _, ratio := range []float64{1.0, 0.5, 1e-10, 0, math.NaN(), math.Inf(1)} {
		if got := backup.RawBudget(b.Image, ratio); got < 1 {
			t.Errorf("RawBudget(%v) = %d; doctor prints this number, and it must never "+
				"be one an operator cannot act on", ratio, got)
		}
	}
}
