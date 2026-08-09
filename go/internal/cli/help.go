package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/ui"
)

// helpText is everything that does not depend on the loaded configuration.
// The flag list here is generated from nothing — it is prose — so it is kept
// deliberately close to dispatch: every flag named below is accepted there, and
// nothing accepted there is missing here.
const helpText = `brb %s — independent, mountable, encrypted backup discs

USAGE
  brb [--yes|-y] [-c CONFIG] [--no-color] <command> [args]

GLOBAL FLAGS (before the command)
  -y, --yes              answer yes to every confirmation; never wait for input
  -c, --config PATH      configuration file to read
      --no-color         no ANSI colour, whatever the terminal says
  -h, --help             this text
      --version          print the version and exit

COMMANDS
  doctor                 check dependencies, versions, configuration and budgets
  init-key               generate an age keypair and a recipients file
      --rescue-key         also mint a second recipient whose identity is kept
                           encrypted under a passphrase asked for here, so the
                           set restores with either the key file or something
                           you remember. Written beside the recipients file as
                           rescue-identity.txt.age, mode 0400; the plaintext
                           never touches disk. Safe to run later on a set that
                           already exists — an existing identity.txt is left
                           alone, and discs already burned keep the keys they
                           were built with. Needs a terminal, so --yes is
                           refused
  plan                   scan and show the disc layout without building anything
  backup                 build, encrypt, protect and image every disc
      --resume             continue an interrupted run from <STAGING>/state.json
      --verify-roundtrip   decrypt every image back and compare hashes before the
                           plaintext is deleted (needs AGE_IDENTITY; doubles the
                           time spent per disc, and is the only way to prove the
                           set decrypts before the plaintext is gone)
  burn <n|n-m|n-|all>    burn discs, confirming before each; builds each ISO
                         first if it is not there, and removes it again once
                         the disc is written (KEEP_ISOS=1 keeps it)
  iso <n|n-m|n-|all>     build ISO images and stop, without burning
  verify-disc <n> [mount]
                         read a burned disc back and check every hash
  ingest [mount]         copy discs back onto disk (prompts for each)
  restore <dest>         repair, decrypt and extract into <dest>, OVERWRITING
                         what is already there: extraction replaces existing
                         files with the backup's versions, mode and mtime
                         included. A <dest> that is not empty is listed and
                         confirmed first — and --yes answers that confirmation,
                         so restoring into a live directory with --yes
                         overwrites it without asking. Restore into an empty
                         directory and merge by hand if that is not what you
                         want. --only and --disc overwrite just the same, in
                         the part of the tree they touch
      --only PATH          extract only this path inside the archive (repeatable).
                           PATH is relative to the archive root, with no leading
                           '/': it is the path as 'brb index' prints it, not the
                           path the file had on the machine that was backed up.
                           The encrypted index says which disc(s) hold it, and
                           only those are decrypted. Write --only=PATH for a
                           path that begins with a dash
      --disc N             only this disc
      --keep-images        keep each decrypted image instead of deleting it once
                           it has been extracted
  mount <n> <mountpoint> decrypt one disc's image and mount it read-only (root)
  list <n>               list the contents of one disc's image
  index [pattern]        which disc holds a given path
  version                print the version
  help                   this text

  Flags are recognised only where they are documented: global flags before the
  command, per-command flags after it. An unknown flag is an error rather than
  data. Everything after a bare "--" is a positional argument, so
  "brb index -- -y" searches the index for "-y".

  Exit status: 0 success, 1 failure, 2 usage error.
`

// packRatioText is brb.sh's usage() explanation of PACK_RATIO, which is the
// one setting that changes how full the discs come out and the one an operator
// is most likely to want to tune after a first run.
const packRatioText = `ABOUT PACK_RATIO
  Discs are packed by uncompressed size, so brb has to guess how well the
  content will compress. The default 1.00 assumes no compression at all, which
  is safe but leaves discs partly empty when the content is compressible. If
  your first run reports images compressing to, say, 0.62 of raw, set
  PACK_RATIO=0.65 and re-run for fuller discs. If an image overshoots its
  budget, brb measures the real ratio, re-packs that disc and continues on its
  own.

  You rarely have to: with PACK_RATIO_ADAPT=1 (the default) every finished disc
  feeds its measured ratio back, and the next disc is planned from the worst of
  the last PACK_RATIO_WINDOW discs plus PACK_RATIO_MARGIN. The estimate moves in
  both directions, so a stretch of incompressible files raises it again instead
  of packing the rest of the set to a ratio only the first discs achieved. Set
  PACK_RATIO_ADAPT=0 to hold the configured value fixed.
`

// writeHelp prints the full help text, with the configuration section filled
// in from the values actually in force.
func writeHelp(w io.Writer, cfg *config.Config, cfgPath string) {
	fmt.Fprintf(w, helpText, version)

	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}
	present := "not present; built-in defaults and the environment only"
	if _, err := os.Stat(cfgPath); err == nil {
		present = "in use"
	}

	fmt.Fprintf(w, "\nCONFIGURATION\n  Config file: %s  (%s)\n", cfgPath, present)
	fmt.Fprintf(w, "  Every setting below can also be given in the environment, which wins\n")
	fmt.Fprintf(w, "  over the file.\n\n")
	for _, l := range configLines(cfg) {
		fmt.Fprintf(w, "    %s\n", l)
	}

	fmt.Fprintf(w, "\n%s", packRatioText)

	fmt.Fprintf(w, `
TYPICAL RUN
  brb doctor                     # dependencies, budgets, and can you decrypt it
  brb init-key                   # then back the key up OFF these discs
  brb plan                       # how many discs, before committing to anything
  brb backup --verify-roundtrip
  brb burn all
  brb verify-disc 1
  brb restore /tmp/testrestore   # once, before you trust the set
  rm -rf %s
`, cfg.Staging)
}

// configLines renders the configuration as the assignments a config file would
// hold, with the values currently in force, each with the note that explains
// what it is for.
func configLines(cfg *config.Config) []string {
	capacity := ""
	if cfg.DiscCapacityBytes > 0 {
		capacity = fmt.Sprintf("%d", cfg.DiscCapacityBytes)
	}
	identity := cfg.AgeIdentity
	identityNote := "restore only"
	if identity == "" {
		identityNote = "restore only; defaults to " + defaultIdentityPath(cfg)
	}

	out := []string{
		assign("SOURCE_DIR", cfg.SourceDir, "the tree to back up"),
		assign("ARCHIVE_NAME", cfg.ArchiveName, ""),
		assign("STAGING", cfg.Staging, "working space; holds plaintext images"),
		assign("DISC_TYPE", string(cfg.DiscType), "bd25 | bd50 | bdxl100 | bdxl128"),
		assign("DISC_CAPACITY_BYTES", capacity, "override for unusual media"),
		assign("COMPRESSION", cfg.Compression, "zstd | xz | gzip | lz4 | lzo | none"),
		assign("COMPRESSION_LEVEL", fmt.Sprint(cfg.CompressionLevel), "zstd 1-22, gzip/lzo 1-9; ignored for xz, lz4, none"),
		assign("BLOCK_SIZE", cfg.BlockSize, "squashfs data block size"),
		assign("PACK_RATIO", fmt.Sprintf("%.2f", cfg.PackRatio), "expected compressed/raw; lower = fuller discs"),
		assign("PACK_RATIO_ADAPT", boolInt(cfg.PackRatioAdapt), "1 re-learns the ratio from the discs built so far"),
		assign("PACK_RATIO_WINDOW", fmt.Sprint(cfg.PackRatioWindow), "how many recent discs the estimate considers"),
		assign("PACK_RATIO_MARGIN", fmt.Sprintf("%.2f", cfg.PackRatioMargin), "safety factor over the measured worst case"),
		assign("PAR2_REDUNDANCY", fmt.Sprint(cfg.Par2Redundancy), "% recovery data over the ciphertext"),
		assign("PAR2_BLOCKS", fmt.Sprint(cfg.Par2Blocks), ""),
		assign("PAR2_MEMORY_MB", fmt.Sprint(cfg.Par2MemoryMB), ""),
		assign("RESERVE_BYTES", fmt.Sprint(cfg.ReserveBytes), ui.HumanBytes(cfg.ReserveBytes)+" held back on every disc"),
		assign("JOBS", fmt.Sprint(cfg.Jobs), "compressor threads; 0 = one per CPU"),
		assign("MAX_SHRINK_ATTEMPTS", fmt.Sprint(cfg.MaxShrinkAttempts), "re-packs allowed when an image overshoots"),
		assign("LABEL_PREFIX", cfg.LabelPrefix, "start of each ISO 9660 volume label"),
		assign("BURNER", cfg.Burner, ""),
		assign("BURN_SPEED", fmt.Sprint(cfg.BurnSpeed), ""),
		assign("ISO_MODE", cfg.ISOMode.String(), isoModeNote(cfg.ISOMode)),
		assign("KEEP_ISOS", boolInt(cfg.KeepISOs), "1 keeps each ISO after a successful burn"),
		assign("AGE_RECIPIENTS_FILE", cfg.AgeRecipientsFile, "public keys images are encrypted to"),
		assign("AGE_IDENTITY", identity, identityNote),
		assign("DIST_DIR", cfg.DistDir, distNote(cfg)),
	}
	out = append(out, arrayLines("PRUNE_DIRS", cfg.PruneDirs)...)
	out = append(out, arrayLines("EXCLUDE_MASKS", cfg.ExcludeMasks)...)
	return out
}

// isoModeNote explains ISO_MODE in one line, naming the other value so the
// choice is visible without reading the manual.
func isoModeNote(m config.ISOMode) string {
	if m.Eager() {
		return "backup builds every ISO up front: about 2.2x staging; ondemand does not"
	}
	return "burn builds each ISO as it needs it; eager builds them all up front"
}

// boolInt renders a 0/1 setting the way a config file writes it.
func boolInt(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// distNote explains DIST_DIR with the directory that would actually be used,
// since the setting is normally left empty and located automatically. The
// environment spelling is BRB_DIST_DIR; `brb doctor` lists what it found there.
func distNote(cfg *config.Config) string {
	if cfg.DistDir != "" {
		return "copies of brb for every disc (env: BRB_DIST_DIR)"
	}
	if dist, err := cfg.ResolveDistDir(); err == nil && dist != "" {
		return "copies of brb for every disc; found " + dist
	}
	return "copies of brb for every disc; none found, see ./build-dist.sh"
}

// assignColumn is where the explanatory comments start.
const assignColumn = 44

// assign renders one KEY=value with its comment lined up.
func assign(key, value, note string) string {
	s := key + "=" + value
	if note == "" {
		return s
	}
	if len(s) >= assignColumn {
		return s + "   # " + note
	}
	return s + strings.Repeat(" ", assignColumn-len(s)) + "# " + note
}

// arrayLines renders a list-valued setting as a shell array, wrapped so that no
// line runs off a terminal.
func arrayLines(key string, values []string) []string {
	if len(values) == 0 {
		return []string{key + "=()"}
	}
	const width = 72
	out := []string{key + "=("}
	line := "   "
	for _, v := range quoteAll(values) {
		if len(line)+1+len(v) > width {
			out = append(out, line)
			line = "   "
		}
		line += " " + v
	}
	if strings.TrimSpace(line) != "" {
		out = append(out, line)
	}
	return append(out, ")")
}

// quoteAll quotes list elements that contain a space, so the rendered array is
// something that can be pasted straight into a config file.
func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.ContainsAny(s, " \t\"'") {
			out = append(out, fmt.Sprintf("%q", s))
			continue
		}
		out = append(out, s)
	}
	return out
}
