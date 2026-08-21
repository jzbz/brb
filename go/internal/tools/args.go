package tools

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// MkOptions describes one mksquashfs run.
type MkOptions struct {
	// SourceDir is the working directory the run happens in; every path in
	// Files is relative to it.
	SourceDir string
	// Out is the squashfs image to create.
	Out string
	// Files are the relative paths to include, fed to mksquashfs as
	// NUL-delimited stdin under -cpiostyle0.
	Files []string
	// Compression names the mksquashfs compressor. "" or "none" selects
	// -no-compression.
	Compression string
	// Level is the -Xcompression-level value. It is applied for zstd, gzip and
	// lzo only; see LevelApplies.
	Level int
	// BlockSize is the squashfs data block size, e.g. "1M". Empty leaves the
	// mksquashfs default.
	BlockSize string
	// Processors caps the compressor thread count; 0 leaves it to mksquashfs.
	Processors int
	// Xattrs stores extended attributes when true.
	Xattrs bool
	// Log receives the tool's output line by line; nil discards it.
	Log io.Writer
}

// LevelApplies reports whether a configured compression level actually reaches
// mksquashfs for the given compressor.
//
// mksquashfs accepts -Xcompression-level for zstd, gzip and lzo only. xz and
// lz4 are tuned through entirely different options (-Xdict-size / -Xbcj and
// -Xhc), and "none" takes no options at all. mksquashfs neither applies the
// level nor complains, so a COMPRESSION_LEVEL set beside one of those
// compressors vanishes without trace; callers of this package are expected to
// warn rather than let it go silently, which is what README's Limitations
// section promises.
func LevelApplies(compression string) bool {
	switch strings.ToLower(strings.TrimSpace(compression)) {
	case "zstd", "gzip", "lzo":
		return true
	default:
		return false
	}
}

// NoCompression reports whether compression names "no compression at all".
func NoCompression(compression string) bool {
	c := strings.ToLower(strings.TrimSpace(compression))
	return c == "" || c == "none"
}

// MksquashfsArgs builds the mksquashfs argument list, excluding the program
// name. The source is "-", meaning "read the file list from stdin", so the run
// must supply MkOptions.Files as NUL-delimited stdin and use SourceDir as its
// working directory.
func MksquashfsArgs(o MkOptions) []string {
	a := []string{"-", o.Out, "-cpiostyle0", "-no-progress", "-quiet"}
	if o.BlockSize != "" {
		a = append(a, "-b", o.BlockSize)
	}
	if o.Xattrs {
		a = append(a, "-xattrs")
	} else {
		a = append(a, "-no-xattrs")
	}
	a = append(a, "-no-exports")
	if o.Processors > 0 {
		a = append(a, "-processors", strconv.Itoa(o.Processors))
	}
	if NoCompression(o.Compression) {
		a = append(a, "-no-compression")
		return a
	}
	comp := strings.ToLower(strings.TrimSpace(o.Compression))
	a = append(a, "-comp", comp)
	if o.Level != 0 && LevelApplies(comp) {
		a = append(a, "-Xcompression-level", strconv.Itoa(o.Level))
	}
	return a
}

// Par2Options describes one par2 run.
type Par2Options struct {
	// Dir is the working directory; File and Inputs are relative to it.
	Dir string
	// File is the file to protect, or — when Inputs is set — the name of the
	// recovery set itself, e.g. "sidecars.par2".
	File string
	// Inputs are the files the recovery set covers. Leave it empty for the
	// usual "protect this one file" run, where par2 derives the set's name
	// from File. Set it to protect several files with one set, which is how
	// the small files on a disc (the .sha512 sidecars and the encrypted index)
	// are covered by a single sidecars.par2.
	Inputs []string
	// Redundancy is the recovery percentage; 0 leaves the par2 default.
	Redundancy int
	// Blocks is the recovery block count; 0 leaves the par2 default.
	Blocks int
	// MemoryMB caps par2's memory use; 0 leaves the par2 default.
	MemoryMB int
	// Log receives the tool's output line by line; nil discards it.
	Log io.Writer
}

// Par2CreateArgs builds the argument list for "par2 create", excluding the
// program name. The run must use Par2Options.Dir as its working directory,
// because par2 records the file name it was given and brb needs it relative.
//
// With Par2Options.Inputs set the first operand names the recovery set and the
// rest are the files it protects, which is par2's multi-file form:
//
//	par2 create -q -r50 -n1 -b100 -- sidecars.par2 a.sha512 b.sha512
func Par2CreateArgs(o Par2Options) []string {
	a := []string{"create", "-q"}
	if o.Redundancy > 0 {
		a = append(a, "-r"+strconv.Itoa(o.Redundancy))
	}
	a = append(a, "-n1")
	if o.Blocks > 0 {
		a = append(a, "-b"+strconv.Itoa(o.Blocks))
	}
	if o.MemoryMB > 0 {
		a = append(a, "-m"+strconv.Itoa(o.MemoryMB))
	}
	a = append(a, "--", o.File)
	return append(a, o.Inputs...)
}

// Par2VerifyArgs builds the argument list for "par2 verify".
func Par2VerifyArgs(par2 string) []string { return []string{"verify", "--", par2} }

// Par2RepairArgs builds the argument list for "par2 repair". extras are
// additional damaged copies of the protected file — the ".copy<time>" files a
// second ingest of the same disc leaves beside the staged image. par2 can
// combine two differently damaged copies into one whole file, but only when
// they are named on its command line; brb.sh passes them the same way.
func Par2RepairArgs(par2 string, extras ...string) []string {
	return append([]string{"repair", "--", par2}, extras...)
}

// ISOOptions describes one ISO build.
type ISOOptions struct {
	// Dir is the directory tree to turn into an ISO.
	Dir string
	// Out is the .iso file to write.
	Out string
	// Label is the ISO 9660 volume ID, already run through SanitiseLabel.
	Label string
	// AppID is the application ID (-A); empty omits it.
	AppID string
	// Publish is the publisher string (-p); empty omits it.
	Publish string
	// Log receives the tool's interesting output; nil discards it.
	Log io.Writer
}

// MkisofsArgs builds the xorriso argument list for one ISO, excluding the
// program name.
//
// There is deliberately no -udf: xorriso/libisofs cannot write UDF, and the
// on-disc README must not claim otherwise. -iso-level 3 enables ISO 9660
// multi-extent, which is what permits image files larger than 4 GiB.
func MkisofsArgs(o ISOOptions) []string {
	a := []string{"-as", "mkisofs", "-quiet", "-iso-level", "3", "-r", "-J", "-joliet-long"}
	if o.Label != "" {
		a = append(a, "-V", o.Label)
	}
	if o.AppID != "" {
		a = append(a, "-A", o.AppID)
	}
	if o.Publish != "" {
		a = append(a, "-p", o.Publish)
	}
	return append(a, "-o", o.Out, o.Dir)
}

// CdrecordArgs builds the xorriso argument list for burning one ISO. A speed of
// zero or less leaves the drive to choose.
func CdrecordArgs(dev, iso string, speed int) []string {
	a := []string{"-as", "cdrecord", "-v", "dev=" + dev}
	if speed > 0 {
		a = append(a, "speed="+strconv.Itoa(speed))
	}
	return append(a, "-eject", iso)
}

// UnsqOptions describes one unsquashfs extraction.
type UnsqOptions struct {
	// Image is the squashfs image to read.
	Image string
	// Dest is the extraction destination.
	Dest string
	// Only lists paths inside the image to extract; empty extracts everything.
	// These must follow the image on the command line, never precede it.
	Only []string
	// Force overwrites an existing destination (-f).
	Force bool
	// Xattrs restores extended attributes.
	Xattrs bool
	// XattrsInclude is a POSIX regular expression limiting which extended
	// attributes are restored ("-xattrs-include"). It is ignored unless Xattrs
	// is set. An unprivileged restore must set "^user." because the security.*
	// and system.* namespaces are writable only by root: without it unsquashfs
	// reports a write_xattr error and exits non-zero on any SELinux-labelled
	// tree, even though every file extracted correctly.
	XattrsInclude string
	// Log receives the tool's output line by line; nil discards it.
	Log io.Writer
}

// UnsquashfsArgs builds the unsquashfs argument list, excluding the program
// name.
//
// unsquashfs's syntax is "unsquashfs [options] FILESYSTEM [paths]", so the path
// filter must come after the image. Passing it before makes unsquashfs treat it
// as the image and extract everything instead of the requested subset.
//
// -no-wildcards is not optional. By default unsquashfs reads each extraction
// path as a wildcard pattern in which `\` escapes the next character and `*`,
// `?` and `[` are metacharacters, so asking for the real file `e\f.txt` matches
// nothing at all — and unsquashfs then exits 0 having created nothing, which is
// indistinguishable from a successful extraction. A path is a path here: it
// came from the operator or from the index, both of which name files literally.
func UnsquashfsArgs(o UnsqOptions) []string {
	a := []string{"-d", o.Dest, "-no-progress", "-no-wildcards"}
	if o.Force {
		a = append(a, "-f")
	}
	if o.Xattrs {
		a = append(a, "-xattrs")
		if o.XattrsInclude != "" {
			a = append(a, "-xattrs-include", o.XattrsInclude)
		}
	} else {
		a = append(a, "-no-xattrs")
	}
	a = append(a, o.Image)
	return append(a, o.Only...)
}

// maxLabel is the ISO 9660 volume identifier length limit.
const maxLabel = 32

// SanitiseLabel turns an arbitrary string into a legal ISO 9660 volume ID: it
// uppercases, replaces everything outside [A-Z0-9_] with '_', and truncates to
// 32 characters.
//
// It works a byte at a time rather than a rune at a time, so one multi-byte
// character becomes one '_' per byte and the result is always ASCII. That is
// what ISO 9660 requires, and it is what keeps the 32 in [maxLabel] a byte
// count: the volume identifier field is 32 bytes wide, not 32 characters.
func SanitiseLabel(s string) string {
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-'a'+'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
		if len(out) == maxLabel {
			break
		}
	}
	return string(out)
}

// DiscLabel builds one disc's ISO 9660 volume ID: "<prefix>_<nn>_OF_<nn>",
// sanitised and held to the 32-byte limit.
//
// The PREFIX is what gets cut when the whole thing will not fit, never the
// "_07_OF_20" suffix, and that asymmetry is the whole point of the function.
// Sanitising the concatenation and letting [SanitiseLabel] truncate would eat
// the tail first, because the disc number is at the end: a 30-byte
// LABEL_PREFIX left every disc from 1 to 9 with the identical volume ID
// "..._0", and a 31-byte one gave all twenty discs the same ID, so the label
// that exists precisely to say "disc 07 of 20" identified nothing — silently,
// permanently, on the media. The prefix is the operator's decoration; the disc
// number is the information, so the information survives.
//
// The suffix width is measured rather than assumed, because a set of more than
// 99 discs renders "_100_OF_120" and a hardcoded bound would be wrong there.
// Nothing reads this label back — neither reader parses it — so trimming the
// prefix costs the operator some of their chosen name and nothing else.
func DiscLabel(prefix string, n, total int) string {
	suffix := fmt.Sprintf("_%02d_OF_%02d", n, total)
	p := SanitiseLabel(prefix)
	if room := maxLabel - len(suffix); len(p) > room {
		if room < 0 {
			room = 0
		}
		p = p[:room]
	}
	return SanitiseLabel(p + suffix)
}

// isoNoise matches the xorriso chatter that carries no information about
// success or failure. The obvious way to drop it — piping xorriso through
// "grep -v ... || true" — throws the exit status away along with the noise, so
// KeepISOLine does the same filtering in process and leaves the child's status
// to be checked; see [(*Set).MakeISO].
var isoNoise = regexp.MustCompile(`(?i)xorriso : (UPDATE|NOTE)|^Media |^Added to ISO|^ISO image produced|^Written to medium|completed successfully`)

// KeepISOLine reports whether a line of xorriso output is worth logging.
// Blank lines and routine progress chatter are dropped; anything else,
// including every failure message, is kept.
func KeepISOLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	return !isoNoise.MatchString(line)
}

// compressorLine matches a bare compressor name in mksquashfs's help output,
// optionally marked as the default.
var compressorLine = regexp.MustCompile(`^([a-z][a-z0-9]*)(\s+\(default\))?$`)

// ParseCompressors extracts the compressor names from mksquashfs help text. It
// looks for the "Compressors available" heading and collects the indented bare
// names beneath it, stopping at the first line that returns to column zero.
// Names are de-duplicated, preserving first-seen order.
func ParseCompressors(help string) []string {
	var out []string
	seen := make(map[string]bool)
	collecting := false
	for _, raw := range strings.Split(help, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if !collecting {
			if strings.Contains(strings.ToLower(trimmed), "compressors available") {
				collecting = true
			}
			continue
		}
		if trimmed == "" {
			continue
		}
		if line == trimmed {
			// Back at column zero: the compressor block has ended.
			collecting = false
			continue
		}
		m := compressorLine.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		if name := m[1]; !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
