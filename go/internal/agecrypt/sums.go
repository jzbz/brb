package agecrypt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SumsName is the name of the checksum file written on every disc.
const SumsName = "SHA512SUMS"

// hexLen is the length of a SHA-512 digest in hexadecimal.
const hexLen = 128

// WriteSumFile writes a single-entry checksum file in GNU sha512sum format:
// the lowercase hex digest, two spaces, the name, a newline. Names containing
// a backslash, a newline or a carriage return are escaped exactly as sha512sum
// escapes them.
func WriteSumFile(sumPath, hex, name string) error {
	line, err := sumLine(hex, name)
	if err != nil {
		return err
	}
	return writeFileAtomic(sumPath, []byte(line))
}

// ReadSumFile parses a file in GNU sha512sum format and returns a map from
// name to lowercase hex digest. Both the text-mode ("  ") and binary-mode
// (" *") separators are accepted, as are blank lines and "#" comments.
func ReadSumFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agecrypt: open %s: %w", path, err)
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		digest, name, err := parseSumLine(line)
		if err != nil {
			return nil, fmt.Errorf("agecrypt: %s line %d: %w", path, n, err)
		}
		if prev, dup := out[name]; dup {
			if prev != digest {
				return nil, fmt.Errorf("agecrypt: %s line %d: %q listed twice with different digests", path, n, name)
			}
			continue
		}
		out[name] = digest
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("agecrypt: read %s: %w", path, err)
	}
	return out, nil
}

// VerifyDir hashes every file listed in the sha512sum-format file at sumPath,
// resolving names inside dir, and returns the names whose contents do not match
// — including names that could not be read at all. A nil error with an empty
// bad list means the directory verified clean. Cancellation aborts with an
// error rather than reporting the remaining files as bad.
func VerifyDir(ctx context.Context, dir, sumPath string) (bad []string, err error) {
	want, err := ReadSumFile(sumPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("agecrypt: open %s: %w", dir, err)
	}
	defer root.Close()

	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return bad, fmt.Errorf("agecrypt: verify %s: %w", dir, err)
		}
		got, err := hashInRoot(ctx, root, name)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return bad, fmt.Errorf("agecrypt: verify %s: %w", dir, ctxErr)
			}
			bad = append(bad, name)
			continue
		}
		if !strings.EqualFold(got, want[name]) {
			bad = append(bad, name)
		}
	}
	return bad, nil
}

// hashInRoot hashes one entry of a checksum file, refusing to follow a name out
// of the directory being verified.
func hashInRoot(ctx context.Context, root *os.Root, name string) (string, error) {
	f, err := root.Open(filepath.FromSlash(strings.TrimPrefix(name, "./")))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f)
}

// WriteSums walks dir and writes a sha512sum-format file at sumPath covering
// every regular file below it except SHA512SUMS itself. Names are recorded as
// "./relative/path" and sorted, matching what the bash implementation's
// "find . -type f | sort | xargs sha512sum" produces, so a disc written by
// either implementation verifies with either.
func WriteSums(ctx context.Context, dir, sumPath string) error {
	return WriteSumsKnown(ctx, dir, sumPath, nil)
}

// KnownSum is a digest the caller already computed for a file it holds under
// another name.
type KnownSum struct {
	// Digest is the lowercase hex SHA-512.
	Digest string
	// Same is the path the digest was taken from. The digest is trusted only
	// when the entry in the directory being summed is that very inode, so a
	// stale or mismatched entry costs one stat and is then hashed normally.
	Same string
}

// WriteSumsKnown is WriteSums with an escape hatch for files whose digest is
// already known: keys are the "./relative/path" names WriteSums itself records.
//
// What it buys: a disc directory's image is a hard link to the ciphertext in
// enc/, whose SHA-512 was computed while it was being encrypted, so hashing it
// again re-reads twenty gigabytes to learn a number brb already wrote into the
// .sha512 beside it.
//
// What it gives up, and why the guard matters: the full pass is an independent
// read-back of the exact bytes that will be burned, so it would catch
// corruption introduced in staging *after* encryption — bad RAM, a failing SSD.
// Taking the shortcut means SHA512SUMS attests a digest that was never re-read
// from disc. Callers therefore pass nothing when the operator asked for extra
// assurance (--verify-roundtrip), and the inode check keeps the shortcut from
// applying to any file that is not literally the one that was hashed.
func WriteSumsKnown(ctx context.Context, dir, sumPath string, known map[string]KnownSum) error {
	names, err := sumCandidates(ctx, dir)
	if err != nil {
		return err
	}
	sort.Strings(names)

	var buf strings.Builder
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("agecrypt: hash %s: %w", dir, err)
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		sum, err := knownSum(known, name, path)
		if err != nil {
			return err
		}
		if sum == "" {
			sum, err = SumFile(ctx, path)
			if err != nil {
				return err
			}
		}
		line, err := sumLine(sum, name)
		if err != nil {
			return err
		}
		buf.WriteString(line)
	}
	return writeFileAtomic(sumPath, []byte(buf.String()))
}

// knownSum returns the recorded digest for name when one was supplied and path
// is the same inode it was taken from, and "" when the file must be hashed. A
// recorded digest that is not a valid SHA-512 is an error rather than a silent
// fall-back: it means the caller has confused two files.
func knownSum(known map[string]KnownSum, name, path string) (string, error) {
	k, ok := known[name]
	if !ok || k.Same == "" {
		return "", nil
	}
	if !isHex512(k.Digest) {
		return "", fmt.Errorf("agecrypt: recorded digest for %s is not a sha-512 hex digest", name)
	}
	here, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("agecrypt: stat %s: %w", path, err)
	}
	there, err := os.Stat(k.Same)
	if err != nil || !os.SameFile(here, there) {
		return "", nil
	}
	return k.Digest, nil
}

// sumCandidates returns the slash-separated "./name" paths of every regular
// file below dir, excluding any file named SHA512SUMS. Symlinks, directories
// and other non-regular entries are skipped, matching find -type f.
func sumCandidates(ctx context.Context, dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.Name() == SumsName && !d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("relative path of %s: %w", path, err)
		}
		names = append(names, "./"+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agecrypt: walk %s: %w", dir, err)
	}
	return names, nil
}

// sumLine renders one GNU sha512sum line, validating the digest first so a
// malformed hash can never be recorded as authoritative.
func sumLine(digest, name string) (string, error) {
	if !isHex512(digest) {
		return "", fmt.Errorf("agecrypt: %q is not a sha-512 hex digest", digest)
	}
	if name == "" {
		return "", errors.New("agecrypt: empty name in checksum file")
	}
	escaped, needsPrefix := escapeName(name)
	if needsPrefix {
		return "\\" + digest + "  " + escaped + "\n", nil
	}
	return digest + "  " + escaped + "\n", nil
}

// parseSumLine splits one checksum line into its digest and name. It accepts
// the text-mode two-space separator, the binary-mode " *" separator and a
// leading backslash marking an escaped name.
func parseSumLine(line string) (digest, name string, err error) {
	escaped := false
	if strings.HasPrefix(line, "\\") {
		escaped = true
		line = line[1:]
	}
	if len(line) < hexLen+2 {
		return "", "", errors.New("line too short for a sha-512 checksum")
	}
	digest = strings.ToLower(line[:hexLen])
	if !isHex512(digest) {
		return "", "", errors.New("line does not start with a sha-512 hex digest")
	}
	rest := line[hexLen:]
	if rest[0] != ' ' && rest[0] != '\t' {
		return "", "", errors.New("missing separator after the digest")
	}
	rest = rest[1:]
	// GNU sha512sum writes two spaces for text mode and " *" for binary mode.
	if rest != "" && (rest[0] == ' ' || rest[0] == '*') {
		rest = rest[1:]
	}
	if rest == "" {
		return "", "", errors.New("missing file name")
	}
	if escaped {
		rest = unescapeName(rest)
	}
	return digest, rest, nil
}

// escapeName applies GNU coreutils' checksum-file quoting: a name containing a
// backslash, a newline or a carriage return is escaped and its line is prefixed
// with a backslash.
//
// The carriage return is not decoration. ReadSumFile trims a trailing CR so it
// tolerates a CRLF file, so a raw CR in a name came back one byte shorter than
// it went in and VerifyDir then reported a clean directory as corrupt. GNU
// coreutils 9.x escapes it for the same reason; builds older than 9.0 do not
// understand \r when checking, which only ever matters for a name brb never
// writes onto a disc.
func escapeName(name string) (string, bool) {
	if !strings.ContainsAny(name, "\\\n\r") {
		return name, false
	}
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), true
}

// unescapeName reverses escapeName, scanning left to right so that an escaped
// backslash is never re-interpreted as the start of another escape.
func unescapeName(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isHex512 reports whether s is exactly 128 lowercase hexadecimal digits.
func isHex512(s string) bool {
	if len(s) != hexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// writeFileAtomic writes data to a ".part" file, fsyncs it and renames it into
// place, so a checksum file is never observed truncated.
func writeFileAtomic(path string, data []byte) error {
	f, finish, err := createPart(path, partModePublic)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr != nil {
		werr = fmt.Errorf("agecrypt: write %s: %w", path, werr)
	}
	return finish(werr)
}
