package agecrypt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// maxSumLine bounds one line of a checksum file: 128 hex characters, a
// separator, and a name. A megabyte is far more than any path needs and is the
// same ceiling the reader puts on an index or a manifest line.
//
// Exactly: bufio.Scanner refuses a line whose bytes — a trailing carriage
// return included, the newline not — number maxSumLine or more, and that
// refuses the whole file, a comment line included even though a comment
// stores nothing. brb.sh's SUM_AWK applies the same test to every line, so a
// long line refuses the file in both readers.
const maxSumLine = 1 << 20

// maxSumBytes bounds the whole of a checksum file [ReadSumFile] will read, and
// it is the bound that actually holds the returned map down.
//
// The two caps beside it are per element — a megabyte per line, a million
// lines — and their product is a terabyte, so each was being mistaken for a
// total. Every parsed line is retained in the map, its key keeping the whole
// scanned line alive, so a SHA512SUMS of a few thousand long, distinct, valid
// lines took about twice its own size in heap: a few gigabytes of file, on a
// "disc" that is a directory or on a single BD-25, could have the process
// killed before the first hash. The file comes off a disc somebody else handed
// over, and verify-disc, ingest and restore all hand its path here with nothing
// in between looking at its size.
//
// Sixteen megabytes is unreachable rather than tight: a brb disc's SHA512SUMS
// lists on the order of ten files in a few kilobytes. It is counted in bytes
// actually read, never taken from a stat, for the three things a stat gets
// wrong: a symlink reports its own length rather than its target's, a procfs or
// FUSE file can report 0 and then stream without end, and counting scanned
// lines instead of bytes misses the carriage return ScanLines drops and invents
// a newline the last line may not have. brb.sh's MAX_SUM_BYTES is the same
// number counted the same way (sum_file_too_big: head -c, then wc -c), so a
// file one byte over refuses in both readers and one exactly at it passes in
// both.
const maxSumBytes = 16 << 20

// maxSumLines bounds how many lines of a checksum file [ReadSumFile] will read.
//
// It came first, and its comment used to say it held the map to a few hundred
// megabytes. It never did — a line may be a megabyte long — and [maxSumBytes]
// is what does that now. It stays as a second, independent ceiling: under the
// byte cap it can only be reached by a file made almost entirely of blank or
// comment lines, which store nothing, so the count is of lines read rather
// than entries kept. brb.sh mirrors it as MAX_SUM_LINES, and dropping it here
// alone would make the two readers disagree about exactly that file. A brb
// disc directory holds on the order of ten files, so a million is unreachable
// by anything brb writes.
const maxSumLines = 1 << 20

// ReadSumFile parses a file in GNU sha512sum format and returns a map from
// name to lowercase hex digest. Both the text-mode ("  ") and binary-mode
// (" *") separators are accepted, as are blank lines and "#" comments. A file
// larger than [maxSumBytes], longer than [maxSumLines] lines, or holding a line
// of [maxSumLine] bytes or more is refused rather than loaded.
func ReadSumFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agecrypt: open %s: %w", path, err)
	}
	defer f.Close()

	// lr runs dry only once maxSumBytes+1 bytes have been read, which is
	// exactly "larger than the cap", whatever the file says about itself.
	lr := &io.LimitedReader{R: f, N: maxSumBytes + 1}
	tooBig := func() error {
		return fmt.Errorf("agecrypt: %s is larger than %d bytes — refusing to read it. A brb disc's %s lists "+
			"a handful of files in a few kilobytes, so this one is damaged or was not written by brb",
			path, maxSumBytes, SumsName)
	}

	out := make(map[string]string)
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 64*1024), maxSumLine)
	for n := 1; sc.Scan(); n++ {
		// Before each line rather than once at the end: once lr has run dry,
		// the line in hand may be one the limit cut short, and its parse error
		// would name a symptom instead of the cause.
		if lr.N == 0 {
			return nil, tooBig()
		}
		if n > maxSumLines {
			return nil, fmt.Errorf("agecrypt: %s has more than %d lines — refusing to read it. A brb disc's %s lists "+
				"a handful of files, so this one is damaged or was not written by brb",
				path, maxSumLines, SumsName)
		}
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
	if lr.N == 0 {
		return nil, tooBig()
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("agecrypt: read %s: %w", path, err)
	}
	return out, nil
}

// VerifyDir hashes every file listed in the sha512sum-format file at sumPath,
// resolving names inside dir, and returns the names whose contents do not match
// — including names that could not be read at all, and names that are not a
// regular file reached without a symbolic link (see [hashInRoot]). A nil error
// with an empty bad list means the directory verified clean.
//
// A sums file that lists nothing is an error, not a clean directory. It
// vouches for no file, and "verified" off it is the integrity claim made
// without reading a byte: an empty or comment-only SHA512SUMS beside a corrupt
// image reported a clean disc. GNU `sha512sum -c` refuses the same file ("no
// properly formatted checksum lines found"), which is how brb.sh answered it
// when it delegated to that command, and brb.sh's own parser answers it the
// same way now.
//
// Each name in required must be listed too — matched with one leading "./"
// dropped from both, as hashInRoot resolves it — or the call is an error. It is
// for the file whose absence would leave the whole check meaningless; VerifyDisc
// passes the disc's image.
//
// Cancellation aborts with an error rather than reporting the remaining files
// as bad.
func VerifyDir(ctx context.Context, dir, sumPath string, required ...string) (bad []string, err error) {
	want, err := ReadSumFile(sumPath)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("agecrypt: %s lists no files, so it cannot vouch for any", sumPath)
	}
	for _, r := range required {
		if !listsName(want, r) {
			return nil, fmt.Errorf("agecrypt: %s records no hash for %s, so it cannot vouch for it", sumPath, r)
		}
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

// listsName reports whether want has an entry for name, comparing both with one
// leading "./" dropped — "./data/x" and "data/x" are the same file to hashInRoot,
// so they are the same entry here.
func listsName(want map[string]string, name string) bool {
	name = strings.TrimPrefix(name, "./")
	for k := range want {
		if strings.TrimPrefix(k, "./") == name {
			return true
		}
	}
	return false
}

// maxSumName is the length, in bytes and with one leading "./" dropped, at which
// a listed name stops being a path on the disc: Linux's PATH_MAX, which counts
// the terminating NUL. brb.sh resolves each name as a path relative to the disc
// root and the kernel refuses one that long, while an os.Root resolves a
// component at a time and has no such limit — so without this the two readers
// disagreed about a hand-made path that long, and brb.sh's walk over the
// components of a megabyte-long name would cost a megabyte per component.
const maxSumName = 4096

// hashInRoot hashes one entry of a checksum file. The name, with one leading
// "./" dropped, is held to the rule every disc brb writes already meets: it is
// shorter than [maxSumName], it climbs nowhere (no absolute path, no ".."
// component), no directory on the way to it is a symbolic link, and what it
// names is a regular file and not a link either. Anything else is an error,
// which VerifyDir reports as a mismatch.
//
// The os.Root alone did not enforce that, and brb.sh did not either. A Root
// confines a name to the disc, but it still follows a link whose target stays
// inside, lets "data/../x" through, and opens a FIFO — which blocks open(2)
// until a writer appears, so verify-disc hung, through the first Ctrl-C too,
// since that only cancels the context. brb.sh, for its part, handed the names
// to `sha512sum -c`, which followed a link anywhere at all, so "disc 1
// verified" could be said of /dev/null. Both readers now apply this one rule:
// brb.sh's sum_name_is_file walks the same components with [[ -L ]] and
// [[ -d ]] and ends on [[ -f ]]. Nothing brb writes can fail it, because
// sumCandidates lists regular files only, never through a link.
//
// Each directory on the way is Lstat'd and then entered as a Root of its own,
// so every check is one component deep and a name of many components costs one
// walk rather than one walk per prefix. The last component is Lstat'd, opened
// with O_NONBLOCK so that a FIFO swapped in after the Lstat cannot block the
// open either, and fstat'd: it must still be a regular file, and the same one.
// The windows between the checks and the opens are the check-then-act that
// restore's discEntry documents and accepts: closing them against somebody
// rewriting the disc during the run buys nothing, since they could simply put
// a real file there.
func hashInRoot(ctx context.Context, root *os.Root, name string) (string, error) {
	rel := strings.TrimPrefix(name, "./")
	if len(rel) >= maxSumName {
		return "", errors.New("too long to be a path on the disc")
	}
	if strings.HasPrefix(rel, "/") {
		return "", errors.New("an absolute path, not a file on the disc")
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == ".." {
			return "", errors.New("climbs out of its directory with ..")
		}
	}
	last := parts[len(parts)-1]
	if last == "" || last == "." {
		return "", errors.New("names a directory, not a file")
	}

	dir := root
	defer func() {
		if dir != root {
			dir.Close()
		}
	}()
	for _, p := range parts[:len(parts)-1] {
		if p == "" || p == "." {
			continue
		}
		fi, err := dir.Lstat(p)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symbolic link", p)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", p)
		}
		sub, err := dir.OpenRoot(p)
		if err != nil {
			return "", err
		}
		if dir != root {
			dir.Close()
		}
		dir = sub
	}

	lfi, err := dir.Lstat(last)
	if err != nil {
		return "", err
	}
	if !lfi.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file (%s)", lfi.Mode().Type())
	}
	f, err := dir.OpenFile(last, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ofi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !ofi.Mode().IsRegular() || !os.SameFile(lfi, ofi) {
		return "", errors.New("changed while it was being opened")
	}
	return hashReader(ctx, f)
}

// WriteSums walks dir and writes a sha512sum-format file at sumPath covering
// every regular file below it except SHA512SUMS itself. Names are recorded as
// "./relative/path" and sorted, which is what GNU sha512sum itself writes for
// "find . -type f | sort | xargs sha512sum" — so `sha512sum -c SHA512SUMS`, the
// check the on-disc README tells a stranger to run, resolves exactly the files
// that were hashed. brb.sh no longer delegates verify-disc to that command (it
// parses the file the way ReadSumFile does and hashes each name itself), but
// GNU compatibility is still the format. xcompat-test.sh pins that a disc
// written here checks out under both readers and under sha512sum alone.
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
	// Empty means never trust it in place of reading the file.
	Same string
	// Verify holds the file to Digest whenever it is hashed, for whatever
	// reason: because Same is empty, or because the inode check failed. A file
	// that then hashes to anything else is refused with a
	// [*DigestMismatchError] instead of being recorded as it now is.
	Verify bool
}

// DigestMismatchError is WriteSumsKnown's refusal to attest a file that no
// longer hashes to the digest its caller recorded for it with Verify set.
//
// It is a type rather than a sentence because only the caller knows what the
// file is, and so what can be done about it.
type DigestMismatchError struct {
	// Name is the "./relative/path" the file would have been recorded under.
	Name string
	// Got is what the file hashes to now; Want is the digest recorded for it.
	Got, Want string
}

func (e *DigestMismatchError) Error() string {
	return fmt.Sprintf("agecrypt: %s hashes to %s, not to the %s recorded for it", e.Name, e.Got, e.Want)
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
// from disc. The inode check keeps the shortcut from applying to any file that
// is not literally the one that was hashed, and a caller that wants the
// read-back (--verify-roundtrip) passes no Same at all.
//
// A read-back is only a check if its result is compared with something, and
// this one used not to be: it recorded whatever the bytes hashed to by then,
// which is to say it attested the corruption it was there to catch. Verify is
// the comparison. With it set, a file that is read — by the caller's choice or
// because its inode did not match — must hash to Digest, or nothing is
// written and the error is a [*DigestMismatchError].
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
			if k, ok := known[name]; ok && k.Verify && !strings.EqualFold(sum, k.Digest) {
				return &DigestMismatchError{Name: name, Got: sum, Want: k.Digest}
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
// fall-back: it means the caller has confused two files. It is checked here,
// before any hashing, for a digest that is only there to be compared with as
// well — finding that out after reading twenty gigabytes would be a poor time.
func knownSum(known map[string]KnownSum, name, path string) (string, error) {
	k, ok := known[name]
	if !ok || (k.Same == "" && !k.Verify) {
		return "", nil
	}
	if !isHex512(k.Digest) {
		return "", fmt.Errorf("agecrypt: recorded digest for %s is not a sha-512 hex digest", name)
	}
	if k.Same == "" {
		return "", nil
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

// partSuffix marks a file this package is in the middle of writing: every
// stream lands in "<name>.part" and is renamed into place once complete. See
// createPart.
const partSuffix = ".part"

// sumCandidates returns the slash-separated "./name" paths of every regular
// file below dir, excluding any file named SHA512SUMS and any file whose name
// ends in ".part". Symlinks, directories and other non-regular entries are
// skipped, matching find -type f.
//
// The ".part" exclusion is what keeps a disc verifiable after a crash. A run
// killed inside writeSums leaves "SHA512SUMS.part" beside the files it was
// hashing; the resumed run then hashed that remnant into the new SHA512SUMS as
// a phantom "./SHA512SUMS.part" entry, and the rename that installs the new
// sums file takes the .part name away — so the disc carried a checksum line
// for a file that is not on it, and every verify of it failed forever. Nothing
// legitimate on a disc ends in .part: it is this package's own in-progress
// marker, so any such file anywhere under the disc directory is either the
// remains of an interrupted write or about to be renamed away, and in neither
// case is it a file to attest.
func sumCandidates(ctx context.Context, dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !d.IsDir() && (d.Name() == SumsName || strings.HasSuffix(d.Name(), partSuffix)) {
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
