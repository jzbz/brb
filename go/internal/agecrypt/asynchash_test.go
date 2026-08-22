package agecrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/fsx"
)

// TestAsyncHashMatchesSynchronous is the whole contract in one line: the same
// bytes through the goroutine must give the digest a plain sha512.New() would.
func TestAsyncHashMatchesSynchronous(t *testing.T) {
	t.Parallel()
	// Sizes either side of the buffer size, so a payload that spans several
	// recycled buffers and one that does not are both covered.
	for _, n := range []int{0, 1, 1000, asyncHashSize - 1, asyncHashSize, asyncHashSize + 7, 5*asyncHashSize + 3} {
		data := make([]byte, n)
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("rand: %v", err)
		}
		want := sha512.Sum512(data)

		a := newAsyncHash()
		// Written in odd-sized pieces, the way age's STREAM writer feeds the
		// ciphertext digest: 64 KiB chunks, not the copy buffer's 1 MiB.
		for off := 0; off < len(data); off += 65552 {
			end := min(off+65552, len(data))
			got, err := a.Write(data[off:end])
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got != end-off {
				t.Fatalf("Write returned %d, want %d", got, end-off)
			}
		}
		sum, err := a.Sum()
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if sum != hex.EncodeToString(want[:]) {
			t.Fatalf("%d bytes: async digest %s, synchronous %s", n, sum, hex.EncodeToString(want[:]))
		}
	}
}

// TestAsyncHashReleaseIsIdempotent covers the error paths, which release the
// hasher without ever asking for the digest.
func TestAsyncHashReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	a := newAsyncHash()
	if _, err := a.Write([]byte("some bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	a.release()
	a.release()
	if _, err := a.Sum(); err != nil {
		t.Fatalf("Sum after release: %v", err)
	}
}

// TestAsyncHashReportsAShortHash proves the guard the digest's credibility
// rests on: if the goroutine somehow consumed fewer bytes than were written,
// Sum must fail rather than hand back a digest of the wrong data.
func TestAsyncHashReportsAShortHash(t *testing.T) {
	t.Parallel()
	a := newAsyncHash()
	if _, err := a.Write(bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	a.release()
	<-a.dead
	a.written += 512 // as if half a chunk had been dropped on the way in
	if _, err := a.Sum(); err == nil {
		t.Fatal("Sum accepted a digest over fewer bytes than were written")
	}
}

// encryptSynchronous is Encrypt as it was before the digests moved onto their
// own goroutines: two sha512 sinks behind one io.MultiWriter. It exists so the
// benchmark below measures the change rather than asserting it, and so
// TestEncryptMatchesTheSynchronousPipeline can prove the bytes are identical.
func encryptSynchronous(ctx context.Context, src, dst string, recipients []age.Recipient) (Sums, error) {
	in, err := os.Open(src)
	if err != nil {
		return Sums{}, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return Sums{}, err
	}
	defer out.Close()

	plain := sha512.New()
	cipher := sha512.New()
	aw, err := age.Encrypt(io.MultiWriter(out, cipher), recipients...)
	if err != nil {
		return Sums{}, err
	}
	if _, err := fsx.CopyCtx(ctx, io.MultiWriter(aw, plain), in); err != nil {
		return Sums{}, err
	}
	if err := aw.Close(); err != nil {
		return Sums{}, err
	}
	return Sums{
		Plain:  hex.EncodeToString(plain.Sum(nil)),
		Cipher: hex.EncodeToString(cipher.Sum(nil)),
	}, nil
}

// TestEncryptMatchesTheSynchronousPipeline pins the property that makes the
// change safe to land: only the order in which cores compute the digests
// changed, so the plaintext digest and the decrypted bytes must be what the old
// pipeline produced. (The ciphertext digest cannot be compared directly — age
// picks a fresh file key every time.)
func TestEncryptMatchesTheSynchronousPipeline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "plain.bin")
	data := make([]byte, 3*asyncHashSize+12345)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	recs := []age.Recipient{id.Recipient()}
	ctx := context.Background()

	old, err := encryptSynchronous(ctx, src, filepath.Join(dir, "old.age"), recs)
	if err != nil {
		t.Fatalf("synchronous encrypt: %v", err)
	}
	got, err := Encrypt(ctx, src, filepath.Join(dir, "new.age"), recs, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got.Plain != old.Plain {
		t.Errorf("plaintext digest %s, synchronous pipeline gave %s", got.Plain, old.Plain)
	}
	want := sha512.Sum512(data)
	if got.Plain != hex.EncodeToString(want[:]) {
		t.Errorf("plaintext digest %s, sha512 of the file is %s", got.Plain, hex.EncodeToString(want[:]))
	}
	// And the ciphertext digest must describe the file that was written.
	onDisc, err := SumFile(ctx, filepath.Join(dir, "new.age"))
	if err != nil {
		t.Fatalf("SumFile: %v", err)
	}
	if onDisc != got.Cipher {
		t.Errorf("Cipher = %s, the file on disc hashes to %s", got.Cipher, onDisc)
	}

	back, err := Decrypt(ctx, filepath.Join(dir, "new.age"), filepath.Join(dir, "back.bin"), []age.Identity{id}, nil)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if back != got.Plain {
		t.Errorf("round trip hashed to %s, want %s", back, got.Plain)
	}
}

// benchFixture writes n bytes of incompressible data and returns its path.
func benchFixture(b *testing.B, n int64) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.bin")
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("fixture: %v", err)
	}
	defer f.Close()
	if _, err := io.CopyN(f, rand.Reader, n); err != nil {
		b.Fatalf("fixture: %v", err)
	}
	return path
}

// benchBytes is the fixture size for the two benchmarks below. A gigabyte is
// the smallest size at which the pipeline's steady state dominates the setup;
// a real disc image is twenty times this.
const benchBytes = 1 << 30

// BenchmarkEncryptSynchronous measures the pipeline as it was: both digests and
// the AEAD on one core, through io.MultiWriter.
func BenchmarkEncryptSynchronous(b *testing.B) {
	src := benchFixture(b, benchBytes)
	id, err := GenerateIdentity()
	if err != nil {
		b.Fatalf("identity: %v", err)
	}
	recs := []age.Recipient{id.Recipient()}
	dst := filepath.Join(b.TempDir(), "out.age")
	ctx := context.Background()

	b.SetBytes(benchBytes)
	b.ResetTimer()
	for b.Loop() {
		if _, err := encryptSynchronous(ctx, src, dst, recs); err != nil {
			b.Fatalf("encrypt: %v", err)
		}
	}
}

// BenchmarkEncrypt measures the shipped pipeline, with a goroutine per digest.
func BenchmarkEncrypt(b *testing.B) {
	src := benchFixture(b, benchBytes)
	id, err := GenerateIdentity()
	if err != nil {
		b.Fatalf("identity: %v", err)
	}
	recs := []age.Recipient{id.Recipient()}
	dst := filepath.Join(b.TempDir(), "out.age")
	ctx := context.Background()

	b.SetBytes(benchBytes)
	b.ResetTimer()
	for b.Loop() {
		if _, err := Encrypt(ctx, src, dst, recs, nil); err != nil {
			b.Fatalf("encrypt: %v", err)
		}
	}
}
