// Package cli is brb's command-line front end: it parses the command line,
// loads the configuration, and dispatches to internal/backup and
// internal/restore.
//
// Parsing is strict and position-aware. Global flags (--yes, --config,
// --no-color) come before the command; per-command flags come after it. An
// unrecognised flag is a usage error naming the flag, and everything after a
// bare "--" is data. brb.sh instead strips -y and -c from anywhere on the line,
// including out of user data, so "brb index -- -y" searches for the wrong
// thing there and the right thing here.
//
// Main is the only exported symbol; cmd/brb is a wrapper that gives it a
// signal-cancelled context and exits with the status it returns. Exit status is
// 0 for success, 1 for a failure, 2 for a usage error.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jzbz/brb/internal/backup"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/restore"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// Exit statuses. They are part of brb's interface: scripts branch on them.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// version is the brb version reported by `brb version`, written into every
// MANIFEST.txt and README.md, and recorded in each ISO's application id. It
// comes from internal/backup so that one constant drives all of them.
const version = backup.Version

// globals are the flags accepted before the command name.
type globals struct {
	assumeYes  bool
	noColor    bool
	configPath string
	cmd        string
	args       []string
}

// Main runs one brb command line and returns the process exit status.
//
// args is the argument list without the program name, normally os.Args[1:].
// stdout carries command output that a caller might pipe or redirect — the
// decrypted index, an image listing, the help text; stderr carries progress and
// diagnostics, exactly as brb.sh splits them. Main never panics and never calls
// os.Exit; it aborts promptly when ctx is cancelled.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	g, err := parseGlobals(args)
	if err != nil {
		fmt.Fprintf(stderr, "fail %v\n", err)
		fmt.Fprintf(stderr, "run 'brb help' for usage\n")
		return exitUsage
	}

	p := ui.New(stderr, !g.noColor && ui.ColorEnabled(stderr))
	p.SetAssumeYes(g.assumeYes)
	defer func() { _ = p.Close() }()

	status := run(ctx, g, p, stdout)

	// A cancelled context is how SIGINT reaches us; say so rather than leaving
	// the operator wondering which of the messages above was the real failure.
	if status != exitOK && ctx.Err() != nil {
		p.Warn("interrupted — staging may hold partial output; 'brb backup --resume' continues where this stopped")
	}
	return status
}

// run does the work Main reports the status of.
func run(ctx context.Context, g globals, p *ui.Printer, stdout io.Writer) int {
	// help and version must work even when the configuration file is broken:
	// they are what an operator reaches for to find out why.
	switch g.cmd {
	case "help":
		f := newFlags("help")
		if err := f.parse(g.args); err != nil {
			return fail(p, err)
		}
		writeHelp(stdout, bestEffortConfig(g.configPath), g.configPath)
		return exitOK
	case "version":
		f := newFlags("version")
		if err := f.parse(g.args); err != nil {
			return fail(p, err)
		}
		if err := f.need(0, 0, "version"); err != nil {
			return fail(p, err)
		}
		fmt.Fprintf(stdout, "brb %s\n", version)
		return exitOK
	}

	cfg, cfgPath, err := loadConfig(g.configPath, p)
	if err != nil {
		return fail(p, err)
	}
	// ASSUME_YES can only be read once the config is loaded, which is after the
	// --yes flag was applied. It is OR'd in, never assigned: a config saying 0
	// must not undo a --yes the operator typed on this command line, and only
	// this direction can be right for a setting whose whole meaning is "do not
	// stop and ask me".
	if cfg.AssumeYes {
		p.SetAssumeYes(true)
	}

	if err := dispatch(ctx, g, cfg, cfgPath, p, stdout); err != nil {
		return fail(p, err)
	}
	return exitOK
}

// fail prints an error and maps it to an exit status.
func fail(p *ui.Printer, err error) int {
	var ue *usageError
	if errors.As(err, &ue) {
		p.Fail("%v", err)
		p.Step("run 'brb help' for usage")
		return exitUsage
	}
	p.Fail("%v", err)
	return exitError
}

// loadConfig reads the configuration file and reports which one was used.
//
// path is what -c named, or "" for the default location. The two are not
// treated alike when the file is missing: no file at the default path is the
// ordinary first run, but a -c that names nothing is a mistyped path, and
// carrying on with the defaults would silently point a restore at
// /var/tmp/brb — an empty staging directory and a report of no discs at all —
// or a backup at $HOME. brb.sh dies on the same condition with the same words.
func loadConfig(path string, p *ui.Printer) (*config.Config, string, error) {
	explicit := path != ""
	if !explicit {
		path = config.DefaultConfigPath()
	}
	if explicit {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			// The advice to drop -c is only advice when there is somewhere to
			// drop back to: DefaultConfigPath gives up when HOME is unset
			// rather than naming a path relative to the current directory.
			fallback := "drop -c to use " + config.DefaultConfigPath()
			if config.DefaultConfigPath() == "" {
				fallback = "set HOME or BRB_CONFIG if you meant the default location"
			}
			return nil, path, fmt.Errorf("config file not found: %s (named by -c; "+
				"check the path, or %s)", path, fallback)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, path, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		p.Step("config: %s", path)
	}
	return cfg, path, nil
}

// resolveSourceDir replaces a SOURCE_DIR that is itself a symbolic link with
// the directory it points at. It is called by the commands that read the
// source tree — doctor, plan and backup — and by nothing on the restore side,
// where SOURCE_DIR is unused and the message would only be noise.
//
// Validate and doctor stat the path, which follows the link, so a symlinked
// SOURCE_DIR passed every check and then failed at the scan, which lstats its
// root and reported "not a directory" — accurate, unhelpful, and hours after
// doctor said ready. The scanner is right not to follow links inside the tree,
// but the root is the operator's choice of what to back up, and a link there
// can only ever have meant its target. Only the last component is resolved,
// which is exactly the case the scanner rejects; symlinked parents were always
// fine (EvalSymlinks resolves those too, which changes nothing the scanner
// cares about). A backup resumed after this change cannot be surprised by the
// new path: a symlinked SOURCE_DIR never got as far as writing state.
//
// The original spelling is kept for the message and for ARCHIVE_NAME, which
// Load has already derived from it — the operator named the link, so the
// archive is named after the link.
func resolveSourceDir(cfg *config.Config, p *ui.Printer) error {
	if cfg.SourceDir == "" {
		return nil
	}
	fi, err := os.Lstat(cfg.SourceDir)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return nil // missing or ordinary: Validate reports the former
	}
	target, err := filepath.EvalSymlinks(cfg.SourceDir)
	if err != nil {
		return fmt.Errorf("SOURCE_DIR %s is a symlink that cannot be followed: %w", cfg.SourceDir, err)
	}
	tfi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("SOURCE_DIR %s is a symlink to %s: %w", cfg.SourceDir, target, err)
	}
	if !tfi.IsDir() {
		// Leave it: Validate's "not a directory" is the right complaint.
		return nil
	}
	p.Step("SOURCE_DIR %s is a symlink to %s; using %s", cfg.SourceDir, target, target)
	cfg.SourceDir = target
	return nil
}

// bestEffortConfig loads the configuration for the help text, falling back to
// the built-in defaults when the file cannot be read. Help that refuses to
// print because the config file has a typo helps nobody.
func bestEffortConfig(path string) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		cfg = config.Default()
		cfg.ResolveDefaults()
	}
	return cfg
}

// dispatch runs one command. Every command reports its own progress through p;
// only data output goes to stdout.
func dispatch(ctx context.Context, g globals, cfg *config.Config, cfgPath string, p *ui.Printer, stdout io.Writer) error {
	switch g.cmd {
	case "doctor":
		f := newFlags("doctor")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(0, 0, "doctor"); err != nil {
			return err
		}
		if err := resolveSourceDir(cfg, p); err != nil {
			return err
		}
		return doctor(ctx, cfg, cfgPath, p, tools.Detect(ctx))

	case "init-key":
		var rescue bool
		f := newFlags("init-key")
		f.Bool(&rescue, "--rescue-key")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(0, 0, "init-key [--rescue-key]"); err != nil {
			return err
		}
		return initKey(cfg, p, rescue)

	case "plan":
		f := newFlags("plan")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(0, 0, "plan"); err != nil {
			return err
		}
		if err := resolveSourceDir(cfg, p); err != nil {
			return err
		}
		_, err := backup.Plan(ctx, backup.Options{Cfg: cfg, UI: p, DryRun: true})
		return err

	case "backup":
		var bo backupOptions
		f := backupFlags(&bo)
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(0, 0,
			"backup [--resume] [--verify-roundtrip] [--public-archive]"); err != nil {
			return err
		}
		if err := resolveSourceDir(cfg, p); err != nil {
			return err
		}
		// The flag turns it on; the config file can too. Neither can turn the
		// other off, so a set is public only if something said so explicitly.
		if bo.public {
			cfg.PublicArchive = true
		}
		return backup.Run(ctx, backup.Options{
			Cfg:             cfg,
			UI:              p,
			Tools:           tools.Detect(ctx),
			Resume:          bo.resume,
			VerifyRoundTrip: bo.roundTrip,
		})

	case "burn":
		f := newFlags("burn")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(1, 1, "burn <n|n-m|n-|all>"); err != nil {
			return err
		}
		if err := checkRange("burn", f.pos[0]); err != nil {
			return err
		}
		return restore.Burn(ctx, restoreOpts(ctx, cfg, p), f.pos[0])

	case "iso":
		f := newFlags("iso")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(1, 1, "iso <n|n-m|n-|all>"); err != nil {
			return err
		}
		if err := checkRange("iso", f.pos[0]); err != nil {
			return err
		}
		return iso.Build(ctx, iso.Options{
			Cfg: cfg, UI: p, Tools: tools.Detect(ctx), Version: version,
		}, f.pos[0])

	case "verify-disc":
		f := newFlags("verify-disc")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(1, 2, "verify-disc <disc-number> [mount-point]"); err != nil {
			return err
		}
		n, err := discNumber("verify-disc", f.pos[0])
		if err != nil {
			return err
		}
		mp := ""
		if len(f.pos) == 2 {
			mp = f.pos[1]
		}
		return restore.VerifyDisc(ctx, restoreOpts(ctx, cfg, p), n, mp)

	case "ingest":
		f := newFlags("ingest")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(0, 1, "ingest [mount-point]"); err != nil {
			return err
		}
		mp := ""
		if len(f.pos) == 1 {
			mp = f.pos[0]
		}
		return restore.Ingest(ctx, restoreOpts(ctx, cfg, p), mp)

	case "restore":
		var ro restore.RestoreOptions
		// Seeded from the config so KEEP_IMAGES=1 works without the flag, as it
		// has always worked for brb.sh. --keep-images then sets it again and
		// cannot unset it, which matches the flag's own meaning: it is how one
		// command asks to keep the images, never how it asks not to.
		ro.KeepImages = cfg.KeepImages
		f := newFlags("restore")
		f.StringList(&ro.Only, "--only")
		f.DiscNum(&ro.Disc, "--disc")
		f.Bool(&ro.KeepImages, "--keep-images")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(1, 1, "restore <destination> [--only PATH]... [--disc N] [--keep-images]"); err != nil {
			return err
		}
		ro.Dest = f.pos[0]
		return restore.Restore(ctx, restoreOpts(ctx, cfg, p), ro)

	case "mount":
		f := newFlags("mount")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(2, 2, "mount <disc-number> <mount-point>"); err != nil {
			return err
		}
		n, err := discNumber("mount", f.pos[0])
		if err != nil {
			return err
		}
		return restore.Mount(ctx, restoreOpts(ctx, cfg, p), n, f.pos[1])

	case "list":
		f := newFlags("list")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(1, 1, "list <disc-number>"); err != nil {
			return err
		}
		n, err := discNumber("list", f.pos[0])
		if err != nil {
			return err
		}
		return restore.List(ctx, restoreOpts(ctx, cfg, p), n, stdout)

	case "index":
		f := newFlags("index")
		if err := f.parse(g.args); err != nil {
			return err
		}
		if err := f.need(0, 1, "index [pattern]"); err != nil {
			return err
		}
		pattern := ""
		if len(f.pos) == 1 {
			pattern = f.pos[0]
		}
		return restore.Index(ctx, restoreOpts(ctx, cfg, p), pattern, stdout)
	}

	return usagef("unknown command %q", g.cmd)
}

// backupOptions are the per-command flags of `brb backup`.
type backupOptions struct {
	resume    bool
	roundTrip bool
	public    bool
}

// backupFlags registers every flag `brb backup` accepts. It is the one list:
// dispatch parses with it, and TestHelpDocumentsEveryBackupFlag reads the
// names back out of it and looks each one up in helpText — so a flag added
// here without a line in the help fails the tests instead of shipping as an
// undocumented switch, which is how --public-archive went missing once.
func backupFlags(o *backupOptions) *cmdFlags {
	f := newFlags("backup")
	f.Bool(&o.resume, "--resume")
	f.Bool(&o.roundTrip, "--verify-roundtrip")
	f.Bool(&o.public, "--public-archive")
	return f
}

// restoreOpts builds the dependency bundle every restore-side command takes.
func restoreOpts(ctx context.Context, cfg *config.Config, p *ui.Printer) restore.Options {
	return restore.Options{Cfg: cfg, UI: p, Tools: tools.Detect(ctx), Version: version}
}

// checkRange rejects a disc selection that is not "all", a number or a range,
// here rather than inside the command, so that a typo exits 2 like every other
// bad command line and not 1 like a failure to burn.
func checkRange(cmd, spec string) error {
	if _, err := iso.ParseRange(spec); err != nil {
		return usagef("%s: %v", cmd, err)
	}
	return nil
}

// parseGlobals reads the flags that precede the command name and stops at the
// first argument that is not one of them. That argument is the command; the
// rest belong to it untouched.
func parseGlobals(args []string) (globals, error) {
	var g globals
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if len(a) < 2 || !strings.HasPrefix(a, "-") {
			break
		}
		name, val := a, ""
		hasVal := false
		if j := strings.IndexByte(a, '='); j > 0 {
			name, val, hasVal = a[:j], a[j+1:], true
		}
		switch name {
		case "-y", "--yes":
			if hasVal {
				return g, usagef("flag %s takes no value", name)
			}
			g.assumeYes = true
		case "--no-color", "--no-colour":
			if hasVal {
				return g, usagef("flag %s takes no value", name)
			}
			g.noColor = true
		case "-c", "--config":
			if !hasVal {
				i++
				if i >= len(args) {
					return g, usagef("flag %s needs a path", name)
				}
				val = args[i]
			}
			if val == "" {
				return g, usagef("flag %s needs a path", name)
			}
			g.configPath = val
		case "-h", "--help":
			g.cmd = "help"
			g.args = nil
			return g, nil
		case "--version":
			g.cmd = "version"
			g.args = nil
			return g, nil
		default:
			return g, usagef("unknown flag %s", name)
		}
	}
	if i >= len(args) {
		// No command at all: show the help text, as brb.sh does.
		g.cmd = "help"
		return g, nil
	}
	// Whatever stopped the loop is the command name, verbatim: after a bare
	// "--" even something that looks like a flag is taken as the command, and
	// reported as an unknown command rather than an unknown flag.
	g.cmd = args[i]
	g.args = args[i+1:]
	return g, nil
}
