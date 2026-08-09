package agecrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// sha512 of the fixed inputs used below, as reported by GNU sha512sum.
const (
	sumHelloWorld = "db3974a97f2407b7cae1ae637c0030687a11913274d578492558e39c16c017de84eacdc8c62fe34ee4e12b4b1428817f09b6a2760c3f8a664ceae94d2434a593"
	sumEmpty      = "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"
	sumPayload    = "d64446d19541544b1ff4b689a0b1bcd2ed3e17cfebd0bebd07fc7d43ee94ff0063ccf8988478e11bb390b89b26f3c4c3f529883ea298a5bbf3599ecc9db08f60"
)

func mustIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	return id
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSumFileKnownVectors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		data string
		want string
	}{
		{"hello world", "hello world\n", sumHelloWorld},
		{"empty", "", sumEmpty},
		{"payload", "brb test payload", sumPayload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
			writeFile(t, p, []byte(tc.data))
			got, err := SumFile(t.Context(), p)
			if err != nil {
				t.Fatalf("SumFile: %v", err)
			}
			if got != tc.want {
				t.Errorf("SumFile = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSumFileLargeInput(t *testing.T) {
	// Exercises the multi-chunk path of copyCtx.
	dir := t.TempDir()
	buf := make([]byte, copyBufSize*2+12345)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	p := filepath.Join(dir, "big")
	writeFile(t, p, buf)

	got, err := SumFile(t.Context(), p)
	if err != nil {
		t.Fatalf("SumFile: %v", err)
	}
	want, err := hashReader(t.Context(), bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("hashReader: %v", err)
	}
	if got != want {
		t.Errorf("SumFile = %s, want %s", got, want)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)

	plainData := []byte("brb test payload")
	src := filepath.Join(dir, "disc01.squashfs")
	enc := filepath.Join(dir, "disc01.squashfs.age")
	writeFile(t, src, plainData)

	var prog bytes.Buffer
	sums, err := Encrypt(t.Context(), src, enc, []age.Recipient{id.Recipient()}, &prog)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if sums.Plain != sumPayload {
		t.Errorf("Sums.Plain = %s, want %s", sums.Plain, sumPayload)
	}
	if prog.Len() != len(plainData) {
		t.Errorf("progress saw %d bytes, want %d", prog.Len(), len(plainData))
	}
	if _, err := os.Stat(enc + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial file survived a successful encryption: %v", err)
	}

	// The recorded ciphertext hash must be the hash of the file on disc.
	onDisc, err := SumFile(t.Context(), enc)
	if err != nil {
		t.Fatalf("SumFile: %v", err)
	}
	if onDisc != sums.Cipher {
		t.Errorf("Sums.Cipher = %s, but file hashes to %s", sums.Cipher, onDisc)
	}

	out := filepath.Join(dir, "back.squashfs")
	got, err := Decrypt(t.Context(), enc, out, []age.Identity{id}, nil)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != sums.Plain {
		t.Errorf("Decrypt sum = %s, want %s", got, sums.Plain)
	}
	back, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(back, plainData) {
		t.Errorf("round trip changed the data: %q", back)
	}
}

func TestEncryptLargeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)
	data := make([]byte, copyBufSize+7)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	src := filepath.Join(dir, "big")
	writeFile(t, src, data)

	enc := filepath.Join(dir, "big.age")
	sums, err := Encrypt(t.Context(), src, enc, []age.Recipient{id.Recipient()}, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	out := filepath.Join(dir, "big.back")
	got, err := Decrypt(t.Context(), enc, out, []age.Identity{id}, nil)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != sums.Plain {
		t.Errorf("plaintext hash changed across the round trip")
	}
	back, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(back, data) {
		t.Error("round trip changed the data")
	}
}

func TestEncryptMultipleRecipients(t *testing.T) {
	dir := t.TempDir()
	ids := []*age.X25519Identity{mustIdentity(t), mustIdentity(t), mustIdentity(t)}
	recs := make([]age.Recipient, 0, len(ids))
	for _, id := range ids {
		recs = append(recs, id.Recipient())
	}
	src := filepath.Join(dir, "in")
	writeFile(t, src, []byte("hello world\n"))
	enc := filepath.Join(dir, "in.age")

	sums, err := Encrypt(t.Context(), src, enc, recs, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if sums.Plain != sumHelloWorld {
		t.Errorf("Sums.Plain = %s, want %s", sums.Plain, sumHelloWorld)
	}
	for i, id := range ids {
		var buf bytes.Buffer
		if err := DecryptTo(t.Context(), enc, &buf, []age.Identity{id}); err != nil {
			t.Fatalf("recipient %d could not decrypt: %v", i, err)
		}
		if buf.String() != "hello world\n" {
			t.Errorf("recipient %d got %q", i, buf.String())
		}
	}

	// An unrelated identity must not be able to read it.
	stranger := mustIdentity(t)
	if err := DecryptTo(t.Context(), enc, io.Discard, []age.Identity{stranger}); err == nil {
		t.Error("an unlisted identity decrypted the file")
	}
}

func TestEncryptNoRecipients(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in")
	writeFile(t, src, []byte("x"))
	if _, err := Encrypt(t.Context(), src, filepath.Join(dir, "out.age"), nil, nil); !errors.Is(err, ErrNoRecipients) {
		t.Fatalf("Encrypt without recipients: %v, want ErrNoRecipients", err)
	}
	if _, err := Decrypt(t.Context(), src, filepath.Join(dir, "out"), nil, nil); !errors.Is(err, ErrNoIdentities) {
		t.Fatalf("Decrypt without identities: %v, want ErrNoIdentities", err)
	}
}

func TestEncryptCancelledContextLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)
	src := filepath.Join(dir, "in")
	// Several chunks, so cancellation lands mid-stream.
	writeFile(t, src, bytes.Repeat([]byte("a"), copyBufSize*3))
	enc := filepath.Join(dir, "in.age")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel as soon as the first chunk has been written.
	prog := writerFunc(func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	})

	_, err := Encrypt(ctx, src, enc, []age.Recipient{id.Recipient()}, prog)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Encrypt: %v, want context.Canceled", err)
	}
	for _, p := range []string{enc, enc + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after cancellation (err=%v)", p, err)
		}
	}
}

func TestDecryptCancelledContextLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)
	src := filepath.Join(dir, "in")
	writeFile(t, src, bytes.Repeat([]byte("b"), copyBufSize*3))
	enc := filepath.Join(dir, "in.age")
	if _, err := Encrypt(t.Context(), src, enc, []age.Recipient{id.Recipient()}, nil); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prog := writerFunc(func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	})
	out := filepath.Join(dir, "out")
	if _, err := Decrypt(ctx, enc, out, []age.Identity{id}, prog); !errors.Is(err, context.Canceled) {
		t.Fatalf("Decrypt: %v, want context.Canceled", err)
	}
	for _, p := range []string{out, out + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after cancellation (err=%v)", p, err)
		}
	}
}

func TestDecryptTruncatedCiphertext(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)
	src := filepath.Join(dir, "in")
	writeFile(t, src, bytes.Repeat([]byte("c"), 200000))
	enc := filepath.Join(dir, "in.age")
	if _, err := Encrypt(t.Context(), src, enc, []age.Recipient{id.Recipient()}, nil); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	data, err := os.ReadFile(enc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	writeFile(t, enc, data[:len(data)-64])

	out := filepath.Join(dir, "out")
	if _, err := Decrypt(t.Context(), enc, out, []age.Identity{id}, nil); err == nil {
		t.Fatal("a truncated ciphertext decrypted without error")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("truncated decryption left %s behind (err=%v)", out, err)
	}
}

func TestIdentityAndRecipientFiles(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)
	keyPath := filepath.Join(dir, "keys", "identity.txt")

	if err := WriteIdentityFile(keyPath, id); err != nil {
		t.Fatalf("WriteIdentityFile: %v", err)
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o400 {
		t.Errorf("identity mode = %o, want 400", got)
	}
	if err := WriteIdentityFile(keyPath, id); err == nil {
		t.Error("WriteIdentityFile overwrote an existing file")
	}

	ids, err := ParseIdentityFile(keyPath)
	if err != nil {
		t.Fatalf("ParseIdentityFile: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("got %d identities, want 1", len(ids))
	}
	parsed, ok := ids[0].(*age.X25519Identity)
	if !ok {
		t.Fatalf("identity has type %T, want *age.X25519Identity", ids[0])
	}
	if parsed.String() != id.String() {
		t.Error("round-tripped identity differs")
	}

	recPath := filepath.Join(dir, "keys", "recipients.txt")
	if err := AppendRecipient(recPath, id.Recipient().String()); err != nil {
		t.Fatalf("AppendRecipient: %v", err)
	}
	// A file with no trailing newline must not glue two keys together.
	writeFile(t, recPath, []byte(id.Recipient().String()))
	second := mustIdentity(t)
	if err := AppendRecipient(recPath, second.Recipient().String()); err != nil {
		t.Fatalf("AppendRecipient: %v", err)
	}
	recs, err := ParseRecipientsFile(recPath)
	if err != nil {
		t.Fatalf("ParseRecipientsFile: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d recipients, want 2", len(recs))
	}

	if err := AppendRecipient(recPath, "not-an-age-key"); err == nil {
		t.Error("AppendRecipient accepted a non-key")
	}
	if err := AppendRecipient(recPath, "age1foo\nage1bar"); err == nil {
		t.Error("AppendRecipient accepted a multi-line value")
	}
}

func TestParseRecipientsFileIgnoresCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	id := mustIdentity(t)
	p := filepath.Join(dir, "recipients.txt")
	writeFile(t, p, []byte("# a comment\n\n"+id.Recipient().String()+"\n\n"))
	recs, err := ParseRecipientsFile(p)
	if err != nil {
		t.Fatalf("ParseRecipientsFile: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recipients, want 1", len(recs))
	}

	empty := filepath.Join(dir, "empty.txt")
	writeFile(t, empty, []byte("# nothing here\n"))
	if _, err := ParseRecipientsFile(empty); err == nil {
		t.Error("a recipients file with no keys was accepted")
	}
	if _, err := ParseRecipientsFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("a missing recipients file was accepted")
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestWriteEncryptedIdentityFile covers the rescue key's storage: a container
// only the passphrase opens, mode 0400, and — the property the whole design
// exists for — no plaintext identity anywhere on disk.
func TestWriteEncryptedIdentityFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rescue-identity.txt.age")
	id := mustIdentity(t)
	const pass = "correct horse battery staple"

	if err := WriteEncryptedIdentityFile(path, id, pass); err != nil {
		t.Fatalf("WriteEncryptedIdentityFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o400 {
		t.Errorf("mode = %04o, want 0400", perm)
	}

	// Nothing in the directory holds the secret in the clear — not the
	// container, not a stray temporary file beside it.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if bytes.Contains(b, []byte("AGE-SECRET-KEY-")) {
			t.Errorf("%s holds a plaintext identity", e.Name())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/v1")) {
		t.Errorf("container does not start with the age header: %q", raw[:min(len(raw), 32)])
	}

	// The passphrase, and only the passphrase, opens it.
	sid, err := age.NewScryptIdentity(pass)
	if err != nil {
		t.Fatal(err)
	}
	r, err := age.Decrypt(bytes.NewReader(raw), sid)
	if err != nil {
		t.Fatalf("decrypt with the passphrase: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := age.ParseIdentities(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("what came out is not an age identity: %v", err)
	}
	if len(ids) != 1 || ids[0].(*age.X25519Identity).String() != id.String() {
		t.Errorf("the identity that came back is not the one written")
	}
	if !bytes.Contains(got, []byte("# public key: "+id.Recipient().String())) {
		t.Errorf("container body is not in age-keygen's format:\n%s", got)
	}

	wrong, err := age.NewScryptIdentity("not the passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := age.Decrypt(bytes.NewReader(raw), wrong); err == nil {
		t.Error("the wrong passphrase decrypted the rescue key")
	}
}

// TestWriteEncryptedIdentityFileRefusals pins the two ways it must not write:
// over a file that is already there, and without a passphrase.
func TestWriteEncryptedIdentityFileRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rescue-identity.txt.age")
	if err := os.WriteFile(path, []byte("do not touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteEncryptedIdentityFile(path, mustIdentity(t), "pw"); err == nil {
		t.Error("overwrote an existing rescue identity")
	}
	if b, _ := os.ReadFile(path); string(b) != "do not touch\n" {
		t.Errorf("the existing file was modified: %q", b)
	}

	fresh := filepath.Join(dir, "fresh.age")
	if err := WriteEncryptedIdentityFile(fresh, mustIdentity(t), ""); err == nil {
		t.Error("wrote a rescue identity with an empty passphrase")
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed write left %s behind", fresh)
	}
}
