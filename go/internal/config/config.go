// Package config loads brb's configuration.
//
// Three layers are combined, each overriding the one before it: the built-in
// defaults (byte-identical to brb.sh's), an optional configuration file in the
// restricted shell subset both implementations understand, and the process
// environment. The file is the same file brb.sh sources, so one config drives
// both implementations; see ParseFile for exactly which shell syntax is
// accepted, and note that anything outside that subset is an error rather than
// something quietly ignored.
//
// PRUNE_DIRS and EXCLUDE_MASKS set anywhere replace the defaults rather than
// extending them, matching brb.sh, where the defaults are only applied if the
// variable is unset.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jzbz/brb/internal/disc"
)

// Config is the complete set of knobs brb exposes. Every field corresponds to
// one upper-case shell variable of the same meaning in brb.sh; Keys lists the
// names.
type Config struct {
	// SourceDir is the tree to back up (SOURCE_DIR).
	SourceDir string
	// ArchiveName names the archive (ARCHIVE_NAME). Load fills it in with
	// "<base(SourceDir)>-YYYY-MM-DD" when it is left empty.
	ArchiveName string
	// Staging is the working directory holding plaintext images while a
	// backup runs (STAGING).
	Staging string
	// AgeRecipientsFile lists the age public keys images are encrypted to
	// (AGE_RECIPIENTS_FILE).
	AgeRecipientsFile string
	// AgeIdentity is the age secret key, needed only to restore (AGE_IDENTITY).
	AgeIdentity string
	// DiscType selects the media capacity (DISC_TYPE).
	DiscType disc.Type
	// DiscCapacityBytes overrides DiscType for unusual media
	// (DISC_CAPACITY_BYTES). 0 means use DiscType.
	DiscCapacityBytes int64
	// Compression is the squashfs compressor (COMPRESSION).
	Compression string
	// CompressionLevel is the compressor level (COMPRESSION_LEVEL). It is
	// only meaningful for zstd, gzip and lzo.
	CompressionLevel int
	// BlockSize is the squashfs data block size (BLOCK_SIZE), e.g. "1M".
	BlockSize string
	// PackRatio is the assumed compressed/raw ratio used to pack discs
	// (PACK_RATIO). 1.00 assumes no compression at all.
	PackRatio float64
	// PackRatioAdapt lets a backup re-estimate PackRatio from the discs it has
	// actually built, in both directions (PACK_RATIO_ADAPT). Off holds the
	// configured value, which is only ever corrected upward by the shrink-retry.
	PackRatioAdapt bool
	// PackRatioWindow is how many recent discs the estimate considers
	// (PACK_RATIO_WINDOW). The worst of a window rather than the last disc
	// alone, so one compressible disc cannot plan an incompressible one.
	PackRatioWindow int
	// PackRatioMargin is the safety factor applied to the measured worst case
	// before it becomes the new ratio (PACK_RATIO_MARGIN).
	PackRatioMargin float64
	// Par2Redundancy is the par2 recovery percentage (PAR2_REDUNDANCY).
	Par2Redundancy int
	// Par2Blocks is par2's block count (PAR2_BLOCKS). Zero means "size the
	// blocks from the image", which is almost always what you want — see
	// [Par2BlockCount].
	Par2Blocks int
	// Par2MemoryMB caps par2's memory use (PAR2_MEMORY_MB). Zero omits -m and
	// leaves par2 its own default, which is half of physical memory.
	Par2MemoryMB int
	// Burner is the optical device to burn to (BURNER).
	Burner string
	// BurnSpeed is the burn speed multiplier (BURN_SPEED).
	BurnSpeed int
	// LabelPrefix begins each disc's ISO 9660 volume label (LABEL_PREFIX).
	LabelPrefix string
	// MaxShrinkAttempts bounds the re-pack loop when an image overshoots its
	// budget (MAX_SHRINK_ATTEMPTS).
	MaxShrinkAttempts int
	// ReserveBytes is the space held back on every disc for README.md,
	// MANIFEST.txt, SHA512SUMS, the index and the brb binary (RESERVE_BYTES).
	ReserveBytes int64
	// ISOMode decides when the burnable ISO images are built (ISO_MODE).
	ISOMode ISOMode
	// KeepISOs keeps a burned disc's ISO in staging instead of deleting it
	// once the bytes are on the medium (KEEP_ISOS).
	KeepISOs bool
	// PruneDirs are directories, relative to SourceDir, not to descend into
	// (PRUNE_DIRS). Setting it replaces the defaults.
	PruneDirs []string
	// ExcludeMasks are filename glob patterns to skip (EXCLUDE_MASKS).
	// Setting it replaces the defaults.
	ExcludeMasks []string
	// Jobs is the number of compressor threads (JOBS). 0 means one per CPU.
	Jobs int
	// DistDir holds the copies of brb that are written onto every disc — the
	// bash script, a static binary per architecture and the source tarball, as
	// produced by build-dist.sh (DIST_DIR in the file, BRB_DIST_DIR in the
	// environment). Empty means "look in the usual places"; see ResolveDistDir.
	DistDir string
}

// Dirs are the working subdirectories under Staging.
type Dirs struct {
	// Work holds scratch state: the scan, the index, resume state.
	Work string
	// Img holds plaintext squashfs images, briefly.
	Img string
	// Enc holds encrypted images, their hashes and their par2 data.
	Enc string
	// Discs holds one directory per disc, exactly as it will be burned.
	Discs string
	// ISO holds the finished ISO 9660 images.
	ISO string
	// Restore holds images decrypted during a restore.
	Restore string
}

// ISOMode says when brb materialises the burnable ISO images of a finished
// set. It is a named type rather than a string so that a value that never came
// through ParseISOMode is visible at a glance.
type ISOMode string

// The two ISO modes, spelled as brb.sh spells them in a config file.
const (
	// ISOOnDemand builds each ISO at the moment its disc goes into the drive
	// and drops it again once the disc is written. It is the default: an ISO
	// is a full second copy of its disc directory, so holding twenty of them
	// for the length of a burn campaign keeps staging at roughly 2.2x the
	// compressed set, for days or weeks.
	ISOOnDemand ISOMode = "ondemand"
	// ISOEager builds every ISO at the end of the backup.
	ISOEager ISOMode = "eager"
)

// ISOModes returns the accepted ISO_MODE values, in the order the help text
// and the error message name them.
func ISOModes() []ISOMode { return []ISOMode{ISOOnDemand, ISOEager} }

// ParseISOMode reads an ISO_MODE value. A typo here is not cosmetic: brb.sh
// tests for "eager" and treats everything else as ondemand, so an unrecognised
// value would silently skip the ISO build the operator asked for.
func ParseISOMode(s string) (ISOMode, error) {
	m := ISOMode(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range ISOModes() {
		if m == known {
			return known, nil
		}
	}
	return "", fmt.Errorf("unknown ISO_MODE %q (expected %s or %s)", s, ISOOnDemand, ISOEager)
}

// String implements fmt.Stringer.
func (m ISOMode) String() string { return string(m) }

// Eager reports whether every ISO is built at the end of the backup.
func (m ISOMode) Eager() bool { return m == ISOEager }

// defaultPrune is brb.sh's DEFAULT_PRUNE array, in order.
var defaultPrune = []string{
	".cache",
	".local/share/Trash",
	".local/share/Steam",
	".thumbnails",
	".var/app",
	"snap",
	".npm/_cacache",
	".cargo/registry",
	".rustup/toolchains",
	".gradle/caches",
	".m2/repository",
	"go/pkg/mod",
	".local/share/containers",
	".local/share/docker",
	".vagrant.d/boxes",
}

// defaultExclude is brb.sh's DEFAULT_EXCLUDE array, in order.
// The core-dump pattern is deliberately "core.[0-9]*" rather than a bare
// "core": a mask named "core" would drop every file simply called core, which
// in a Go, Drupal or kernel tree is ordinary source rather than a crash dump.
var defaultExclude = []string{"*.pyc", "*.pyo", "core.[0-9]*", ".DS_Store"}

// DefaultPruneDirs returns a copy of brb.sh's DEFAULT_PRUNE list.
func DefaultPruneDirs() []string { return append([]string(nil), defaultPrune...) }

// DefaultExcludeMasks returns a copy of brb.sh's DEFAULT_EXCLUDE list.
func DefaultExcludeMasks() []string { return append([]string(nil), defaultExclude...) }

// Compressions returns the squashfs compressors brb accepts, in the order
// brb.sh documents them.
func Compressions() []string {
	return []string{"zstd", "xz", "gzip", "lz4", "lzo", "none"}
}

// Default returns the built-in configuration. Every value matches brb.sh's
// defaults exactly. ArchiveName is left empty; Load, or ResolveDefaults,
// derives it from SourceDir and today's date.
func Default() *Config {
	home := homeDir()
	return &Config{
		SourceDir:         home,
		ArchiveName:       "",
		Staging:           "/var/tmp/brb",
		AgeRecipientsFile: filepath.Join(home, ".config", "brb", "recipients.txt"),
		AgeIdentity:       "",
		DiscType:          disc.BD25,
		DiscCapacityBytes: 0,
		Compression:       "zstd",
		CompressionLevel:  19,
		BlockSize:         "1M",
		PackRatio:         1.00,
		PackRatioAdapt:    true,
		PackRatioWindow:   3,
		PackRatioMargin:   1.05,
		Par2Redundancy:    10,
		Par2Blocks:        0, // auto: ~1 MiB blocks, see Par2BlockCount
		Par2MemoryMB:      0, // omit -m; par2's own default is half of RAM
		Burner:            "/dev/sr0",
		BurnSpeed:         4,
		LabelPrefix:       "BACKUP",
		MaxShrinkAttempts: 4,
		ReserveBytes:      104857600,
		ISOMode:           ISOOnDemand,
		KeepISOs:          false,
		PruneDirs:         DefaultPruneDirs(),
		ExcludeMasks:      DefaultExcludeMasks(),
		Jobs:              0,
	}
}

// DefaultConfigPath returns $BRB_CONFIG if set, else $HOME/.config/brb/config,
// matching brb.sh's CONFIG_FILE.
func DefaultConfigPath() string {
	if p := os.Getenv("BRB_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".config", "brb", "config")
}

// Dirs returns the working subdirectories of Staging.
func (c *Config) Dirs() Dirs {
	return Dirs{
		Work:    filepath.Join(c.Staging, "work"),
		Img:     filepath.Join(c.Staging, "img"),
		Enc:     filepath.Join(c.Staging, "enc"),
		Discs:   filepath.Join(c.Staging, "discs"),
		ISO:     filepath.Join(c.Staging, "iso"),
		Restore: filepath.Join(c.Staging, "restore"),
	}
}

// Capacity returns the raw media capacity in bytes: DiscCapacityBytes when it
// is set, otherwise the capacity of DiscType.
func (c *Config) Capacity() int64 {
	if c.DiscCapacityBytes > 0 {
		return c.DiscCapacityBytes
	}
	return c.DiscType.Capacity()
}

// Budget apportions one disc between ISO overhead, the reserved plaintext
// files, par2 recovery data and the squashfs image.
func (c *Config) Budget() (disc.Budget, error) {
	b, err := disc.Compute(c.Capacity(), c.ReserveBytes, c.Par2Redundancy)
	if err != nil {
		return b, fmt.Errorf("disc budget for %s: %w", c.DiscType, err)
	}
	return b, nil
}

// ArchiveNameFor derives brb.sh's default archive name for a source directory
// and a date: the directory's base name, a hyphen, and YYYY-MM-DD.
func ArchiveNameFor(sourceDir string, t time.Time) string {
	base := filepath.Base(strings.TrimRight(sourceDir, string(filepath.Separator)))
	switch base {
	case "", ".", "..", string(filepath.Separator):
		base = "backup"
	}
	return base + "-" + t.Format("2006-01-02")
}

// ResolveDefaults fills in values that can only be derived once every layer has
// been applied. It is idempotent, and Load calls it for you.
func (c *Config) ResolveDefaults() {
	if c.ArchiveName == "" {
		c.ArchiveName = ArchiveNameFor(c.SourceDir, time.Now())
	}
}

// Load starts from Default, applies the configuration file when it exists, then
// the environment, which wins. A missing file is not an error; an unreadable or
// malformed one is. Pass "" for path to use DefaultConfigPath.
func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		path = DefaultConfigPath()
	}
	fi, err := os.Stat(path)
	switch {
	case err == nil && fi.IsDir():
		return nil, fmt.Errorf("config file %s is a directory", path)
	case err == nil:
		vals, err := ParseFile(path)
		if err != nil {
			return nil, err
		}
		if err := c.Apply(vals); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// No configuration file: defaults and environment only.
	default:
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	if err := c.ApplyEnv(os.Getenv); err != nil {
		return nil, err
	}
	c.ResolveDefaults()
	return c, nil
}

// Keys returns every configuration key recognised in the file and the
// environment, sorted, so that `doctor` and the tests can enumerate them.
func Keys() []string {
	return []string{
		"AGE_IDENTITY",
		"AGE_RECIPIENTS_FILE",
		"ARCHIVE_NAME",
		"BLOCK_SIZE",
		"BURNER",
		"BURN_SPEED",
		"COMPRESSION",
		"COMPRESSION_LEVEL",
		"DISC_CAPACITY_BYTES",
		"DISC_TYPE",
		"DIST_DIR",
		"EXCLUDE_MASKS",
		"ISO_MODE",
		"JOBS",
		"KEEP_ISOS",
		"LABEL_PREFIX",
		"MAX_SHRINK_ATTEMPTS",
		"PACK_RATIO",
		"PACK_RATIO_ADAPT",
		"PACK_RATIO_MARGIN",
		"PACK_RATIO_WINDOW",
		"PAR2_BLOCKS",
		"PAR2_MEMORY_MB",
		"PAR2_REDUNDANCY",
		"PRUNE_DIRS",
		"RESERVE_BYTES",
		"SOURCE_DIR",
		"STAGING",
	}
}

// isArrayKey reports whether a key holds a list rather than a scalar.
func isArrayKey(key string) bool {
	return key == "PRUNE_DIRS" || key == "EXCLUDE_MASKS"
}

// EnvName returns the environment variable that carries a configuration key.
//
// It is the key itself for everything but DIST_DIR, which brb.sh reads from
// BRB_DIST_DIR — the name is generic enough that an exported DIST_DIR would as
// likely belong to something else on the machine, and brb.sh's
// DIST_DIR="${BRB_DIST_DIR:-}" ignores it. The file key stays DIST_DIR, so one
// config file still drives both implementations.
func EnvName(key string) string {
	if key == "DIST_DIR" {
		return "BRB_DIST_DIR"
	}
	return key
}

// Apply merges parsed configuration values into c. Keys are applied in sorted
// order so that a file with several problems always reports the same one first.
// An unrecognised key is an error naming it and its line.
func (c *Config) Apply(vals map[string]Value) error {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := c.set(k, vals[k]); err != nil {
			return err
		}
	}
	return nil
}

// ApplyEnv applies the same key names from the process environment, which takes
// precedence over the configuration file. An empty or unset variable is
// ignored, exactly as brb.sh's ${VAR:-default} treats it. Pass os.Getenv, or a
// stub in tests; nil means os.Getenv.
//
// The shell has already stripped quoting from an environment variable, so only
// two conveniences are applied: a leading ~ and $HOME/${HOME} are expanded, and
// a value for PRUNE_DIRS or EXCLUDE_MASKS that is written as an array literal —
// "( a b )" — is parsed as one. Any other value of those two is a single
// element, matching what brb.sh sees.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, k := range Keys() {
		name := EnvName(k)
		raw := getenv(name)
		if raw == "" {
			continue
		}
		v, err := envValue(k, raw)
		if err != nil {
			return fmt.Errorf("environment variable %s: %w", name, err)
		}
		if err := c.set(k, v); err != nil {
			return fmt.Errorf("environment variable %s: %w", name, err)
		}
	}
	return nil
}

// envValue turns one environment string into a Value.
func envValue(key, raw string) (Value, error) {
	if isArrayKey(key) && strings.HasPrefix(strings.TrimLeft(raw, " \t"), "(") {
		vals, err := Parse(key+"="+raw, "environment")
		if err != nil {
			return Value{}, err
		}
		v, ok := vals[key]
		if !ok || !v.IsArray {
			return Value{}, fmt.Errorf("%w: malformed array literal %q", ErrSyntax, raw)
		}
		v.Line = 0
		return v, nil
	}
	if isArrayKey(key) {
		return Value{Array: []string{raw}, IsArray: true}, nil
	}
	return Value{Scalar: expandHomePath(raw)}, nil
}

// set applies one key.
func (c *Config) set(key string, v Value) error {
	switch key {
	case "SOURCE_DIR":
		return v.str(key, &c.SourceDir)
	case "ARCHIVE_NAME":
		return v.str(key, &c.ArchiveName)
	case "STAGING":
		return v.str(key, &c.Staging)
	case "AGE_RECIPIENTS_FILE":
		return v.str(key, &c.AgeRecipientsFile)
	case "AGE_IDENTITY":
		return v.str(key, &c.AgeIdentity)
	case "DISC_TYPE":
		s, err := v.scalar(key)
		if err != nil {
			return err
		}
		t, err := disc.ParseType(s)
		if err != nil {
			return fmt.Errorf("%sDISC_TYPE: %w", v.where(), err)
		}
		c.DiscType = t
		return nil
	case "DISC_CAPACITY_BYTES":
		return v.i64(key, &c.DiscCapacityBytes)
	case "COMPRESSION":
		s, err := v.scalar(key)
		if err != nil {
			return err
		}
		c.Compression = strings.ToLower(strings.TrimSpace(s))
		return nil
	case "COMPRESSION_LEVEL":
		return v.int(key, &c.CompressionLevel)
	case "BLOCK_SIZE":
		return v.str(key, &c.BlockSize)
	case "PACK_RATIO":
		return v.f64(key, &c.PackRatio)
	case "PACK_RATIO_ADAPT":
		return v.boolInt(key, &c.PackRatioAdapt)
	case "PACK_RATIO_WINDOW":
		return v.int(key, &c.PackRatioWindow)
	case "PACK_RATIO_MARGIN":
		return v.f64(key, &c.PackRatioMargin)
	case "PAR2_REDUNDANCY":
		return v.int(key, &c.Par2Redundancy)
	case "PAR2_BLOCKS":
		return v.int(key, &c.Par2Blocks)
	case "PAR2_MEMORY_MB":
		return v.int(key, &c.Par2MemoryMB)
	case "BURNER":
		return v.str(key, &c.Burner)
	case "BURN_SPEED":
		return v.int(key, &c.BurnSpeed)
	case "LABEL_PREFIX":
		return v.str(key, &c.LabelPrefix)
	case "MAX_SHRINK_ATTEMPTS":
		return v.int(key, &c.MaxShrinkAttempts)
	case "RESERVE_BYTES":
		return v.i64(key, &c.ReserveBytes)
	case "ISO_MODE":
		s, err := v.scalar(key)
		if err != nil {
			return err
		}
		m, err := ParseISOMode(s)
		if err != nil {
			// Rejected here rather than in Validate: brb.sh dies in load_config,
			// and a mode that only fails later would first let a whole backup run.
			return fmt.Errorf("%s%w", v.where(), err)
		}
		c.ISOMode = m
		return nil
	case "KEEP_ISOS":
		return v.boolInt(key, &c.KeepISOs)
	case "JOBS":
		return v.int(key, &c.Jobs)
	case "DIST_DIR":
		return v.str(key, &c.DistDir)
	case "PRUNE_DIRS":
		c.PruneDirs = v.list()
		return nil
	case "EXCLUDE_MASKS":
		c.ExcludeMasks = v.list()
		return nil
	default:
		return fmt.Errorf("%sunknown configuration key %q", v.where(), key)
	}
}

// where renders "line N: " for a value that came from a file, and "" for one
// that came from the environment.
func (v Value) where() string {
	if v.Line > 0 {
		return fmt.Sprintf("line %d: ", v.Line)
	}
	return ""
}

// scalar returns the value of a setting that must not be an array.
func (v Value) scalar(key string) (string, error) {
	if v.IsArray {
		return "", fmt.Errorf("%s%s does not take an array value", v.where(), key)
	}
	return v.Scalar, nil
}

// list returns the elements of an array setting. A scalar becomes a
// single-element list, or an empty one when it is blank — which is what brb.sh
// effectively does, since it skips empty prune and mask entries.
func (v Value) list() []string {
	if v.IsArray {
		out := make([]string, 0, len(v.Array))
		for _, s := range v.Array {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if strings.TrimSpace(v.Scalar) == "" {
		return []string{}
	}
	return []string{v.Scalar}
}

func (v Value) str(key string, dst *string) error {
	s, err := v.scalar(key)
	if err != nil {
		return err
	}
	*dst = s
	return nil
}

func (v Value) int(key string, dst *int) error {
	s, err := v.scalar(key)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%s%s: invalid integer %q", v.where(), key, s)
	}
	*dst = n
	return nil
}

// boolInt applies a setting brb.sh writes as 0 or 1.
//
// Numbers only, and anything else is an error: brb.sh tests these with
// (( ! VAR )), which is shell arithmetic, where a word like "yes" is an unset
// variable name and evaluates to 0 — the exact opposite of what it says. One
// config file drives both implementations, so a value that would mean two
// different things is refused rather than interpreted.
func (v Value) boolInt(key string, dst *bool) error {
	s, err := v.scalar(key)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%s%s: expected 0 or 1, got %q", v.where(), key, s)
	}
	*dst = n != 0
	return nil
}

// f64 applies a setting written as a decimal number.
func (v Value) f64(key string, dst *float64) error {
	s, err := v.scalar(key)
	if err != nil {
		return err
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fmt.Errorf("%s%s: invalid number %q", v.where(), key, s)
	}
	*dst = f
	return nil
}

func (v Value) i64(key string, dst *int64) error {
	s, err := v.scalar(key)
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return fmt.Errorf("%s%s: invalid integer %q", v.where(), key, s)
	}
	*dst = n
	return nil
}

// Validate reports everything wrong with the configuration at once, so a
// mistake is not discovered one run at a time. The returned error is a join of
// the individual problems.
func (c *Config) Validate() error {
	var errs []error

	if c.SourceDir == "" {
		errs = append(errs, errors.New("SOURCE_DIR is empty"))
	} else if fi, err := os.Stat(c.SourceDir); err != nil {
		errs = append(errs, fmt.Errorf("SOURCE_DIR: source directory does not exist: %s: %w", c.SourceDir, err))
	} else if !fi.IsDir() {
		errs = append(errs, fmt.Errorf("SOURCE_DIR: not a directory: %s", c.SourceDir))
	}

	if c.Staging == "" {
		errs = append(errs, errors.New("STAGING is empty"))
	}
	if c.AgeRecipientsFile == "" {
		errs = append(errs, errors.New("AGE_RECIPIENTS_FILE is empty"))
	}
	if c.LabelPrefix == "" {
		errs = append(errs, errors.New("LABEL_PREFIX is empty"))
	}
	// ARCHIVE_NAME is copied verbatim into MANIFEST.txt and every disc's
	// README.md — documents whose whole purpose is to be read by a stranger in
	// fifteen years — and it identifies the set in state.json. A newline pasted
	// in from a terminal splits the README's title line and injects a second
	// heading; an ESC byte is invisible in the config and repaints the reader's
	// terminal. LABEL_PREFIX survives the same treatment only because
	// tools.SanitiseLabel scrubs it on the way to xorriso; nothing scrubs this.
	// Refuse it before the run, not after every disc carries it.
	if err := checkArchiveName(c.ArchiveName); err != nil {
		errs = append(errs, err)
	}

	if _, err := disc.ParseType(string(c.DiscType)); err != nil {
		errs = append(errs, fmt.Errorf("DISC_TYPE: %w", err))
	}
	// Load rejects a bad ISO_MODE outright; this catches a Config assembled in
	// code, where the zero value of the field is an empty string.
	if _, err := ParseISOMode(string(c.ISOMode)); err != nil {
		errs = append(errs, err)
	}
	if c.DiscCapacityBytes < 0 {
		errs = append(errs, fmt.Errorf("DISC_CAPACITY_BYTES must not be negative, got %d", c.DiscCapacityBytes))
	}
	if c.ReserveBytes < 0 {
		errs = append(errs, fmt.Errorf("RESERVE_BYTES must not be negative, got %d", c.ReserveBytes))
	}

	if !(c.PackRatio > 0) || math.IsInf(c.PackRatio, 0) || math.IsNaN(c.PackRatio) {
		errs = append(errs, fmt.Errorf("PACK_RATIO must be a positive number, got %g", c.PackRatio))
	}
	// A window of zero is the shape of the bug this feature exists to avoid: an
	// empty window has no worst case, so an estimate taken from it is not a
	// measurement at all. Refuse it by name rather than silently disable the
	// adaptation the operator asked for.
	if c.PackRatioWindow < 1 {
		errs = append(errs, fmt.Errorf("PACK_RATIO_WINDOW must be at least 1, got %d "+
			"(set PACK_RATIO_ADAPT=0 to keep PACK_RATIO fixed instead)", c.PackRatioWindow))
	}
	// Below 1.0 the margin plans every disc to come out over budget, and each
	// overshoot costs a full mksquashfs pass over multiple gigabytes.
	if !(c.PackRatioMargin >= 1) || math.IsInf(c.PackRatioMargin, 0) {
		errs = append(errs, fmt.Errorf("PACK_RATIO_MARGIN is a safety factor over the measured "+
			"ratio and must be at least 1.0, got %g", c.PackRatioMargin))
	}

	if err := c.validateCompression(); err != nil {
		errs = append(errs, err)
	}
	if err := validateBlockSize(c.BlockSize); err != nil {
		errs = append(errs, err)
	}

	if c.Par2Redundancy < 1 || c.Par2Redundancy > 100 {
		errs = append(errs, fmt.Errorf("PAR2_REDUNDANCY must be between 1 and 100, got %d", c.Par2Redundancy))
	}
	if c.Par2Blocks < 0 {
		errs = append(errs, fmt.Errorf("PAR2_BLOCKS must not be negative, got %d", c.Par2Blocks))
	}
	if c.Par2Blocks > par2MaxBlocks {
		errs = append(errs, fmt.Errorf("PAR2_BLOCKS must be at most %d, got %d (par2 rejects more)",
			par2MaxBlocks, c.Par2Blocks))
	}
	if c.Par2MemoryMB < 0 {
		errs = append(errs, fmt.Errorf("PAR2_MEMORY_MB must not be negative, got %d", c.Par2MemoryMB))
	}
	if c.BurnSpeed < 0 {
		errs = append(errs, fmt.Errorf("BURN_SPEED must not be negative, got %d", c.BurnSpeed))
	}
	if c.MaxShrinkAttempts < 0 {
		errs = append(errs, fmt.Errorf("MAX_SHRINK_ATTEMPTS must not be negative, got %d", c.MaxShrinkAttempts))
	}
	if c.Jobs < 0 {
		errs = append(errs, fmt.Errorf("JOBS must not be negative, got %d", c.Jobs))
	}

	// Only worth computing once the inputs it depends on are sane.
	if len(errs) == 0 {
		if _, err := c.Budget(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// compressionLevelRange gives the level range a compressor accepts, and
// whether the level is used at all. brb.sh only passes -Xcompression-level for
// zstd and gzip; lzo takes one too, and xz and lz4 ignore it.
func compressionLevelRange(comp string) (lo, hi int, used bool) {
	switch comp {
	case "zstd":
		return 1, 22, true
	case "gzip":
		return 1, 9, true
	case "lzo":
		return 1, 9, true
	}
	return 0, 0, false
}

// checkArchiveName rejects an ARCHIVE_NAME that cannot be written into a
// document and read back as itself: any C0 control byte (newline, tab, CR,
// ESC), DEL, and '/' — the last is not used as a path component today, but a
// name that could not become one is cheap insurance.
func checkArchiveName(name string) error {
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' {
			return fmt.Errorf("ARCHIVE_NAME must not contain %q: it is copied verbatim "+
				"into MANIFEST.txt and README.md on every disc", r)
		}
	}
	return nil
}

func (c *Config) validateCompression() error {
	known := false
	for _, n := range Compressions() {
		if n == c.Compression {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("COMPRESSION %q is not one of %s",
			c.Compression, strings.Join(Compressions(), ", "))
	}
	lo, hi, used := compressionLevelRange(c.Compression)
	if used && (c.CompressionLevel < lo || c.CompressionLevel > hi) {
		return fmt.Errorf("COMPRESSION_LEVEL %d is out of range for %s (%d-%d)",
			c.CompressionLevel, c.Compression, lo, hi)
	}
	if !used && c.CompressionLevel < 0 {
		return fmt.Errorf("COMPRESSION_LEVEL must not be negative, got %d", c.CompressionLevel)
	}
	return nil
}

// BlockSizeBytes converts BLOCK_SIZE ("4K", "1M", "131072") to bytes.
func BlockSizeBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("BLOCK_SIZE is empty")
	}
	mult := int64(1)
	switch t[len(t)-1] {
	case 'K', 'k':
		mult, t = 1024, t[:len(t)-1]
	case 'M', 'm':
		mult, t = 1024*1024, t[:len(t)-1]
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("BLOCK_SIZE %q is not a size like 4K, 128K or 1M", s)
	}
	return n * mult, nil
}

// validateBlockSize enforces what mksquashfs enforces: a power of two between
// 4 KiB and 1 MiB. Catching it here saves discovering it hours into a run.
func validateBlockSize(s string) error {
	n, err := BlockSizeBytes(s)
	if err != nil {
		return err
	}
	if n < 4096 || n > 1024*1024 || n&(n-1) != 0 {
		return fmt.Errorf("BLOCK_SIZE %q must be a power of two between 4K and 1M", s)
	}
	return nil
}
