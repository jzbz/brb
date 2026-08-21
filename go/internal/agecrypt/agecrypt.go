// Package agecrypt wraps age encryption and SHA-512 hashing for brb.
//
// It does in-process what brb.sh, the reader shipped on every disc, shells out
// to age(1) and sha512sum(1) for. Two properties matter above all others:
//
//   - Every stream is written to a "<name>.part" file that is fsynced and
//     renamed into place only after the whole operation succeeded. A partial
//     file is removed on any error, including context cancellation, so a
//     truncated ciphertext can never be mistaken for a complete one.
//   - Encrypt computes the plaintext and the ciphertext SHA-512 in a single
//     pass over the data. Hashing the plaintext, encrypting and hashing the
//     ciphertext as three separate steps would read a multi-gigabyte image
//     three times to learn the same two numbers.
//
// The checksum files written here are byte-compatible with GNU sha512sum,
// which is the program brb.sh verifies a disc with (`sha512sum -c --quiet
// --strict SHA512SUMS`, brb.sh:852). Only the Go build writes a disc;
// xcompat-test.sh pins that both readers accept what it wrote.
package agecrypt

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/fsx"
)

// copyBufSize is the chunk size of the streaming copies. Large enough that the
// per-chunk context check costs nothing, small enough to abort promptly.
const copyBufSize = 1 << 20

// ErrNoRecipients is returned when an encryption is attempted without any age
// recipient, which would produce a file nobody can decrypt.
var ErrNoRecipients = errors.New("agecrypt: no age recipients")

// ErrNoIdentities is returned when a decryption is attempted without any age
// identity.
var ErrNoIdentities = errors.New("agecrypt: no age identities")

// Sums holds the two SHA-512 digests of one encryption, lowercase hex.
type Sums struct {
	// Plain is the digest of the plaintext, before encryption.
	Plain string
	// Cipher is the digest of the age ciphertext, as written to disc.
	Cipher string
}

// ParseRecipientsFile reads an age recipients file: one public key per line,
// ignoring blank lines and lines starting with "#". It is an error for the file
// to contain no keys.
func ParseRecipientsFile(path string) ([]age.Recipient, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agecrypt: open recipients file: %w", err)
	}
	defer f.Close()
	recs, err := age.ParseRecipients(f)
	if err != nil {
		return nil, fmt.Errorf("agecrypt: parse recipients file %s: %w", path, err)
	}
	return recs, nil
}

// ParseIdentityFile reads an age identity file: one secret key per line,
// ignoring blank lines and lines starting with "#". It is an error for the file
// to contain no keys.
//
// A parse failure is reported in this package's own words and never in age's.
// age quotes the line it could not place straight back at the caller
// ("unknown identity type: %q"), and in an identity file that line is the
// secret key: a paste that carried one leading space, or a key round-tripped
// through something that lowercased it, is still a perfectly good key, and
// every caller prints the error it gets — onto the terminal, into the
// scrollback, into a CI log, into the bug report the operator then attaches
// the whole thing to. Nothing here may put key material in an error string.
// ParseRecipientsFile above deliberately keeps age's message: its lines are
// public keys, and the exact text is the fastest way to fix a typo in one.
func ParseIdentityFile(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agecrypt: open identity file: %w", err)
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, redactedIdentityError(path, f)
	}
	return ids, nil
}

// maxIdentityLine bounds one line of an identity file while [ParseIdentityFile]
// is locating the line that failed. An age secret key is 74 characters; a
// megabyte is a line no identity file has, and stops a rescan of a file that
// turned out to be a disc image from buffering it.
const maxIdentityLine = 1 << 20

// redactedIdentityError says which line of an identity file age refused,
// without reproducing any of it.
//
// The line number comes from re-scanning the file and offering each key line
// to age on its own, so the verdict is age's rather than a second copy of its
// dispatch here that would drift from it — and so the message age returns can
// be thrown away unread. A rescan that cannot single out one line (an empty
// file, a file of nothing but comments, a seek that failed) still yields an
// error naming the file, because returning nil here would turn an unparseable
// identity into a silent success.
func redactedIdentityError(path string, f *os.File) error {
	const advice = "its content is not quoted here because it is secret key material — " +
		"check that the key line is one unmodified AGE-SECRET-KEY-1... with no leading space and no change of case"
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 4<<10), maxIdentityLine)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if _, err := age.ParseIdentities(strings.NewReader(line)); err != nil {
				return fmt.Errorf("agecrypt: identity file %s: line %d is not an age identity — %s", path, n, advice)
			}
		}
	}
	return fmt.Errorf("agecrypt: identity file %s holds no age identity — %s", path, advice)
}

// ReadX25519IdentityFile reads an identity file that must hold exactly one
// X25519 secret key and returns it with its type intact, so the caller can ask
// it for its recipient. ParseIdentityFile erases the type behind age.Identity,
// which is right for decrypting but useless for the one place brb needs to
// prove a reloaded key is the key a set was encrypted to.
func ReadX25519IdentityFile(path string) (*age.X25519Identity, error) {
	ids, err := ParseIdentityFile(path)
	if err != nil {
		return nil, err
	}
	var found *age.X25519Identity
	for _, id := range ids {
		x, ok := id.(*age.X25519Identity)
		if !ok {
			return nil, fmt.Errorf("agecrypt: identity file %s: holds a key that is not X25519", path)
		}
		if found != nil {
			return nil, fmt.Errorf("agecrypt: identity file %s: holds more than one key", path)
		}
		found = x
	}
	if found == nil {
		return nil, fmt.Errorf("agecrypt: identity file %s: no key", path)
	}
	return found, nil
}

// GenerateIdentity generates a fresh X25519 age keypair.
func GenerateIdentity() (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("agecrypt: generate identity: %w", err)
	}
	return id, nil
}

// WriteIdentityFile writes id to path in the format age-keygen produces: a
// "# created:" comment, a "# public key:" comment, then the secret key. The
// file is created with mode 0400 and an existing file is never overwritten.
func WriteIdentityFile(path string, id *age.X25519Identity) error {
	return writeIdentityFile(path, id, 0o400)
}

func writeIdentityFile(path string, id *age.X25519Identity, mode os.FileMode) (err error) {
	if id == nil {
		return errors.New("agecrypt: nil identity")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("agecrypt: create key directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("agecrypt: create identity file %s: %w", path, err)
	}
	defer func() {
		cerr := f.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("agecrypt: close identity file %s: %w", path, cerr)
		}
		if err != nil {
			os.Remove(path)
		}
	}()

	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().Format(time.RFC3339), id.Recipient(), id)
	if _, err := f.WriteString(body); err != nil {
		return fmt.Errorf("agecrypt: write identity file %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("agecrypt: sync identity file %s: %w", path, err)
	}
	// The mode passed to OpenFile is masked by the umask; force it.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("agecrypt: chmod identity file %s: %w", path, err)
	}
	return nil
}

// WriteEncryptedIdentityFile writes id to path inside an age container
// encrypted under passphrase — the scrypt recipient, which is what `age -p`
// produces and what [age.NewScryptIdentity] unlocks on the restore side.
//
// The plaintext identity never becomes a file. It is rendered into the age
// writer and nowhere else, which is the whole point of the rescue key: the
// construction it stands in for is `age-keygen | age -p`, a pipe with nothing
// to shred afterwards, and shred can promise nothing on a copy-on-write,
// compressed or flash-translated filesystem anyway. This function is that
// pipe. What brb.sh does with the result is read it: find_identity (brb.sh:206)
// tries rescue-identity.txt.age beside the recipients file, which is why
// init-key writes it under that name and in that directory.
//
// The file is created with O_EXCL and mode 0400, and removed again if anything
// after the create fails, so a half-written container that nobody's passphrase
// opens is never left behind claiming to be a rescue key.
func WriteEncryptedIdentityFile(path string, id *age.X25519Identity, passphrase string) (err error) {
	if id == nil {
		return errors.New("agecrypt: nil identity")
	}
	if passphrase == "" {
		return errors.New("agecrypt: empty passphrase")
	}
	rec, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return fmt.Errorf("agecrypt: passphrase recipient: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("agecrypt: create key directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("agecrypt: create identity file %s: %w", path, err)
	}
	defer func() {
		cerr := f.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("agecrypt: close identity file %s: %w", path, cerr)
		}
		if err != nil {
			os.Remove(path)
		}
	}()

	w, err := age.Encrypt(f, rec)
	if err != nil {
		return fmt.Errorf("agecrypt: encrypt identity file %s: %w", path, err)
	}
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().Format(time.RFC3339), id.Recipient(), id)
	if _, err := io.WriteString(w, body); err != nil {
		return fmt.Errorf("agecrypt: write identity file %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("agecrypt: finish identity file %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("agecrypt: sync identity file %s: %w", path, err)
	}
	// The mode passed to OpenFile is masked by the umask; force it.
	if err := os.Chmod(path, 0o400); err != nil {
		return fmt.Errorf("agecrypt: chmod identity file %s: %w", path, err)
	}
	return nil
}

// AppendRecipient appends an age public key to the recipients file at path,
// creating the file (and its parent directory) if needed. The key is validated
// before anything is written, and a missing final newline in an existing file
// is repaired so the new key lands on a line of its own.
func AppendRecipient(path, pubkey string) (err error) {
	pubkey = strings.TrimSpace(pubkey)
	if pubkey == "" {
		return errors.New("agecrypt: empty recipient")
	}
	if strings.ContainsAny(pubkey, "\n\r") {
		return fmt.Errorf("agecrypt: recipient %q spans multiple lines", pubkey)
	}
	if _, err := age.ParseRecipients(strings.NewReader(pubkey)); err != nil {
		return fmt.Errorf("agecrypt: invalid recipient %q: %w", pubkey, err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("agecrypt: create recipients directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("agecrypt: open recipients file %s: %w", path, err)
	}
	defer func() {
		cerr := f.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("agecrypt: close recipients file %s: %w", path, cerr)
		}
	}()

	line := pubkey + "\n"
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("agecrypt: stat recipients file %s: %w", path, err)
	}
	if fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err != nil {
			return fmt.Errorf("agecrypt: read recipients file %s: %w", path, err)
		}
		if last[0] != '\n' {
			line = "\n" + line
		}
	}
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("agecrypt: append to recipients file %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("agecrypt: sync recipients file %s: %w", path, err)
	}
	return nil
}

// ProbeRecipients encrypts a few bytes to recs, in memory and to nowhere, and
// returns whatever age refuses with.
//
// It exists because parsing a recipients file proves each key well-formed and
// nothing at all about the set: age (1.3 on) refuses to encrypt to a set that
// mixes post-quantum (age1pq1...) and classic (age1...) recipients, while
// [ParseRecipientsFile] and [AppendRecipient] both accept such a file happily.
// The probe is the same call [Encrypt] makes, so anything age will object to
// about the set is objected to here instead, at the cost of one header.
func ProbeRecipients(recs []age.Recipient) error {
	if len(recs) == 0 {
		return ErrNoRecipients
	}
	w, err := age.Encrypt(io.Discard, recs...)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("brb")); err != nil {
		return err
	}
	return w.Close()
}

// CheckRecipientAddition reports whether appending pubkey to the recipients
// file at path would leave a set age is still willing to encrypt to.
//
// Call it before minting anything. init-key's failure without it is not a
// failed command but a broken archive: --rescue-key wrote the encrypted
// container, appended a classic key beside a post-quantum one, printed "discs
// built from now on are encrypted to both keys", and every later backup then
// died in preflight on a set age will not mix. The operator's only way out was
// to edit the recipients file by hand, and init-key refuses to re-mint a
// rescue key while the container exists — so the promised second way in was a
// file nothing was ever encrypted to.
//
// A recipients file that does not exist yet, or that this package cannot parse
// at all, is not this function's refusal to make: the first is the ordinary
// first run, and the second already fails loudly in backup preflight with a
// message about the file rather than about the key being added. Only a set age
// itself rejects is reported here.
func CheckRecipientAddition(path, pubkey string) error {
	existing, err := ParseRecipientsFile(path)
	if err != nil {
		return nil
	}
	added, err := age.ParseRecipients(strings.NewReader(strings.TrimSpace(pubkey)))
	if err != nil {
		return fmt.Errorf("agecrypt: invalid recipient %q: %w", pubkey, err)
	}
	if err := ProbeRecipients(append(existing, added...)); err != nil {
		return fmt.Errorf("agecrypt: adding %s to %s would make a set age refuses to encrypt to: %w — "+
			"a set may not mix post-quantum (age1pq1...) and classic (age1...) recipients; edit %s so it "+
			"holds one kind only, and re-run", pubkey, path, err, path)
	}
	return nil
}

// Encrypt streams the file at src into the age file at dst, encrypted to
// recipients, and returns the SHA-512 of both the plaintext and the ciphertext
// computed in the same single pass over the data.
//
// The ciphertext is written to dst+".part", fsynced and renamed onto dst only
// after the stream completed. The partial file is removed on any error and on
// context cancellation, so dst never exists in a half-written state.
//
// If prog is non-nil, plaintext bytes are also written to it as they are read,
// for progress accounting.
func Encrypt(ctx context.Context, src, dst string, recipients []age.Recipient, prog io.Writer) (sums Sums, err error) {
	if len(recipients) == 0 {
		return Sums{}, ErrNoRecipients
	}
	in, err := os.Open(src)
	if err != nil {
		return Sums{}, fmt.Errorf("agecrypt: open %s: %w", src, err)
	}
	defer in.Close()

	part, done, err := createPart(dst, partModePublic)
	if err != nil {
		return Sums{}, err
	}
	defer func() { err = done(err) }()

	// Both digests run beside the encryption rather than in front of it; see
	// asyncHash. The releases cover every path that leaves before Sum, so a
	// cancelled or failed encryption never leaks the two goroutines.
	plain := newAsyncHash()
	defer plain.release()
	cipher := newAsyncHash()
	defer cipher.release()

	aw, err := age.Encrypt(io.MultiWriter(part, cipher), recipients...)
	if err != nil {
		return Sums{}, fmt.Errorf("agecrypt: start encryption of %s: %w", src, err)
	}
	sinks := []io.Writer{aw, plain}
	if prog != nil {
		sinks = append(sinks, prog)
	}
	if _, err := copyCtx(ctx, io.MultiWriter(sinks...), in); err != nil {
		return Sums{}, fmt.Errorf("agecrypt: encrypt %s: %w", src, err)
	}
	// Close flushes the final STREAM chunk; it is part of the ciphertext and
	// therefore must be hashed before Cipher is read.
	if err := aw.Close(); err != nil {
		return Sums{}, fmt.Errorf("agecrypt: finish encryption of %s: %w", src, err)
	}
	plainSum, err := plain.Sum()
	if err != nil {
		return Sums{}, fmt.Errorf("agecrypt: hashing the plaintext of %s: %w", src, err)
	}
	cipherSum, err := cipher.Sum()
	if err != nil {
		return Sums{}, fmt.Errorf("agecrypt: hashing the ciphertext of %s: %w", src, err)
	}
	return Sums{Plain: plainSum, Cipher: cipherSum}, nil
}

// Decrypt streams the age file at src into the plaintext file at dst and
// returns the SHA-512 of the plaintext, lowercase hex. It uses the same
// ".part"-then-rename discipline as Encrypt: dst never exists half-written.
//
// If prog is non-nil, plaintext bytes are also written to it for progress
// accounting.
func Decrypt(ctx context.Context, src, dst string, ids []age.Identity, prog io.Writer) (sum string, err error) {
	if len(ids) == 0 {
		return "", ErrNoIdentities
	}
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("agecrypt: open %s: %w", src, err)
	}
	defer in.Close()

	r, err := age.Decrypt(in, ids...)
	if err != nil {
		return "", fmt.Errorf("agecrypt: decrypt %s: %w", src, err)
	}

	// Plaintext: the decrypted image, or the backup's round-trip copy of it.
	part, done, err := createPart(dst, partModePrivate)
	if err != nil {
		return "", err
	}
	defer func() { err = done(err) }()

	plain := newAsyncHash()
	defer plain.release()
	sinks := []io.Writer{part, plain}
	if prog != nil {
		sinks = append(sinks, prog)
	}
	if _, err := copyCtx(ctx, io.MultiWriter(sinks...), r); err != nil {
		return "", fmt.Errorf("agecrypt: decrypt %s: %w", src, err)
	}
	sum, err = plain.Sum()
	if err != nil {
		return "", fmt.Errorf("agecrypt: hashing the plaintext of %s: %w", src, err)
	}
	return sum, nil
}

// DecryptTo streams the age file at src into w. It is used where the plaintext
// is consumed in memory rather than stored, such as the encrypted index.
func DecryptTo(ctx context.Context, src string, w io.Writer, ids []age.Identity) error {
	if len(ids) == 0 {
		return ErrNoIdentities
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("agecrypt: open %s: %w", src, err)
	}
	defer in.Close()

	r, err := age.Decrypt(in, ids...)
	if err != nil {
		return fmt.Errorf("agecrypt: decrypt %s: %w", src, err)
	}
	if _, err := copyCtx(ctx, w, r); err != nil {
		return fmt.Errorf("agecrypt: decrypt %s: %w", src, err)
	}
	return nil
}

// SumFile returns the SHA-512 of the file at path as lowercase hex.
func SumFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("agecrypt: open %s: %w", path, err)
	}
	defer f.Close()
	sum, err := hashReader(ctx, f)
	if err != nil {
		return "", fmt.Errorf("agecrypt: hash %s: %w", path, err)
	}
	return sum, nil
}

// hashReader reads r to EOF and returns its SHA-512 as lowercase hex.
func hashReader(ctx context.Context, r io.Reader) (string, error) {
	var h hash.Hash = sha512.New()
	if _, err := copyCtx(ctx, h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyCtx copies src into dst in fixed-size chunks, checking ctx between
// chunks so a multi-gigabyte stream aborts promptly on cancellation.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// Modes for createPart. Ciphertext, checksum files and par2 volumes are copied
// onto the discs, where the settled format is 0644; plaintext is written into
// staging, where the default STAGING under /var/tmp is world-executable and
// every local account could otherwise read the whole decrypted archive for as
// long as a restore runs. brb.sh gets 0600 there the other way, with a
// `umask 077` around its staging writes (brb.sh:266).
const (
	partModePublic  os.FileMode = 0o644
	partModePrivate os.FileMode = 0o600
)

// createPart opens dst+".part" for writing with the given mode and returns it
// together with a finish function. Call finish with the operation's error: on
// success it fsyncs, closes and renames the partial file onto dst; on failure —
// including context cancellation — it closes and removes the partial file and
// returns the original error. The returned error of finish is the error to
// report.
//
// The .part is opened by [fsx.CreateFresh]: remove whatever is there, then
// O_EXCL, never O_TRUNC — because a symlink another local user planted in
// staging would otherwise be opened straight through, streaming a decrypted
// image into a file of their choosing. That rule and the reason for it live in
// one place on purpose (fsx/fsx.go: "each rewrite lost a piece of them"), so
// it is not restated here. The stale-.part case it covers is reached in this
// package by a backup that is --resumed and re-encrypts the disc it was killed
// on, the writer having reaped nothing beforehand.
func createPart(dst string, mode os.FileMode) (*os.File, func(error) error, error) {
	part := dst + ".part"
	f, err := fsx.CreateFresh(part, mode)
	if err != nil {
		return nil, nil, fmt.Errorf("agecrypt: %w", err)
	}
	finish := func(opErr error) error {
		if opErr != nil {
			f.Close()
			os.Remove(part)
			return opErr
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(part)
			return fmt.Errorf("agecrypt: sync %s: %w", part, err)
		}
		if err := f.Close(); err != nil {
			os.Remove(part)
			return fmt.Errorf("agecrypt: close %s: %w", part, err)
		}
		if err := os.Rename(part, dst); err != nil {
			os.Remove(part)
			return fmt.Errorf("agecrypt: rename %s: %w", part, err)
		}
		syncDir(filepath.Dir(dst))
		return nil
	}
	return f, finish, nil
}

// syncDir flushes a directory entry so a completed rename survives a crash.
// Failures are deliberately ignored: not every filesystem supports fsync on a
// directory, and the data itself is already durable at this point.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
