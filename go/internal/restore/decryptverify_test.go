package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/fsx"
)

// TestDecryptVerifyingVerdicts pins every verdict the fused verify-and-decrypt
// can reach, and what it leaves on disk for each, so that moving its digests
// onto their own goroutines could change only how fast they are computed. The
// three that matter most are the ones PrepareImage's decision table never
// reaches directly: a wrong key over an intact file is age's error and not
// "damaged" (repairing 20 GB to rediscover a wrong key is the waste the digest
// exists to avoid), bytes after age's framing are refused, and a cancelled run
// leaves neither the image nor its .part. The sizes straddle age's 64 KiB
// chunk and the hashers' 1 MiB buffers.
func TestDecryptVerifyingVerdicts(t *testing.T) {
	other, err := agecrypt.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	before := runtime.NumGoroutine()

	for _, size := range []int{0, 1, 65536, 65537, 3<<20 + 11} {
		e := newEnv(t)
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		enc, sums := e.writeImage(1, payload)
		ids, err := e.opts().identities()
		if err != nil {
			t.Fatal(err)
		}
		pristine, err := os.ReadFile(enc)
		if err != nil {
			t.Fatal(err)
		}
		sum := func(b []byte) string { d := sha512.Sum512(b); return hex.EncodeToString(d[:]) }
		flipped := bytes.Clone(pristine)
		flipped[len(flipped)-1] ^= 0x01
		trailing := append(bytes.Clone(pristine), "trailing bytes"...)
		truncated := pristine[:len(pristine)-1]
		dst := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")

		for _, tc := range []struct {
			name    string
			file    []byte
			want    string
			ids     []age.Identity
			ctx     context.Context
			verdict string // ok, damaged, age (an error that is not "damaged"), ctx
		}{
			{"clean", pristine, sums.Cipher, ids, context.Background(), "ok"},
			{"recorded digest in upper case", pristine, strings.ToUpper(sums.Cipher), ids, context.Background(), "ok"},
			{"recorded digest wrong", pristine, sum(trailing), ids, context.Background(), "damaged"},
			{"wrong key, recorded digest right", pristine, sums.Cipher, []age.Identity{other}, context.Background(), "age"},
			{"wrong key, recorded digest wrong", pristine, sum(flipped), []age.Identity{other}, context.Background(), "damaged"},
			{"last byte flipped, digest of the flipped file", flipped, sum(flipped), ids, context.Background(), "age"},
			{"last byte flipped, original digest", flipped, sums.Cipher, ids, context.Background(), "damaged"},
			{"trailing bytes, digest over them", trailing, sum(trailing), ids, context.Background(), "age"},
			{"trailing bytes, original digest", trailing, sums.Cipher, ids, context.Background(), "damaged"},
			{"truncated, digest of what is left", truncated, sum(truncated), ids, context.Background(), "age"},
			{"cancelled before it starts", pristine, sums.Cipher, ids, cancelled, "ctx"},
		} {
			t.Run(tc.name+"/"+strconv.Itoa(size)+"B", func(t *testing.T) {
				if err := os.WriteFile(enc, tc.file, 0o600); err != nil {
					t.Fatal(err)
				}
				_ = os.Remove(dst)
				got, err := e.opts().decryptVerifying(tc.ctx, enc, dst, tc.want, tc.ids, nil)
				switch tc.verdict {
				case "ok":
					if err != nil || got != sum(payload) {
						t.Fatalf("decryptVerifying = %q, %v; want the plaintext digest %s", got, err, sum(payload))
					}
					if back, err := os.ReadFile(dst); err != nil || !bytes.Equal(back, payload) {
						t.Errorf("the promoted image is not the plaintext (%v)", err)
					}
				case "damaged":
					if !errors.Is(err, errCiphertextDamaged) {
						t.Errorf("decryptVerifying = %v, want errCiphertextDamaged", err)
					}
				case "age":
					if err == nil || errors.Is(err, errCiphertextDamaged) || errors.Is(err, context.Canceled) {
						t.Errorf("decryptVerifying = %v, want age's own error: the digest matched, so this is not damage", err)
					}
				case "ctx":
					if !errors.Is(err, context.Canceled) {
						t.Errorf("decryptVerifying = %v, want context.Canceled", err)
					}
				}
				if tc.verdict != "ok" {
					if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("an image was promoted on a %s verdict (%v)", tc.verdict, err)
					}
				}
				if _, err := os.Lstat(dst + partExt); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the .part survived (%v)", err)
				}
			})
		}
	}

	// Every return path releases both hashers. Their goroutines exit on their
	// own once released, so give them a moment before counting.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("%d goroutine(s) before the matrix, %d after: a hasher leaked", before, n)
	}
}

// decryptVerifyingSynchronous is decryptVerifying as it was before its digests
// moved onto goroutines of their own: the ciphertext SHA-512 behind an
// io.TeeReader and the plaintext one inside an io.MultiWriter, both on the
// caller's goroutine with the AEAD between them. It exists so the benchmark
// pair below measures the change rather than asserting it, as
// encryptSynchronous does for Encrypt in agecrypt. Only the progress writer is
// left out; every verdict and every step on disk is the old one.
func decryptVerifyingSynchronous(ctx context.Context, encPath, dst, wantCipher string, ids []age.Identity) (string, error) {
	in, err := os.Open(encPath)
	if err != nil {
		return "", err
	}
	defer in.Close()

	cipher := sha512.New()
	tee := io.TeeReader(in, cipher)
	r, ageErr := age.Decrypt(tee, ids...)

	part := dst + partExt
	var copyErr error
	plain := sha512.New()
	promoted := false
	var out *os.File
	if ageErr == nil {
		out, err = fsx.CreateFresh(part, 0o600)
		if err != nil {
			return "", err
		}
		defer func() {
			if !promoted {
				out.Close()
				os.Remove(part)
			}
		}()
		copyErr = copyChunks(ctx, io.MultiWriter(out, plain), r)
	}

	if ageErr != nil || copyErr != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := copyChunks(ctx, io.Discard, tee); err != nil {
			return "", err
		}
		if !strings.EqualFold(hex.EncodeToString(cipher.Sum(nil)), wantCipher) {
			return "", errCiphertextDamaged
		}
		if ageErr != nil {
			return "", ageErr
		}
		return "", copyErr
	}

	if err := copyChunks(ctx, io.Discard, tee); err != nil {
		return "", err
	}
	if !strings.EqualFold(hex.EncodeToString(cipher.Sum(nil)), wantCipher) {
		return "", errCiphertextDamaged
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	promoted = true
	return hex.EncodeToString(plain.Sum(nil)), nil
}

// decryptBenchBytes is the plaintext size of the benchmark pair below: large
// beside the hashers' 1 MiB buffers and age's 64 KiB chunks, so the steady
// state of the pipeline is what gets timed, and small enough to keep a run
// short. The figures quoted on decryptVerifying came from a 1 GiB image; on
// the same machine, with TMPDIR on the same encrypted NVMe, this size gave
// 360-370 MB/s against 755-770, and 385-400 against 380-390 with -cpu 1.
// b.TempDir follows TMPDIR, and where /tmp is a tmpfs that takes the disk out
// of the measurement altogether.
const decryptBenchBytes = 64 << 20

// decryptBenchFixture encrypts decryptBenchBytes of random data to a fresh key,
// as a backup would, and returns the image, its recorded ciphertext digest,
// the key, and the plaintext digest every iteration has to come back with.
func decryptBenchFixture(b *testing.B) (encPath, wantCipher string, ids []age.Identity, wantPlain string) {
	b.Helper()
	dir := b.TempDir()
	src := filepath.Join(dir, "disc01.squashfs")
	f, err := os.Create(src)
	if err != nil {
		b.Fatalf("fixture: %v", err)
	}
	if _, err := io.CopyN(f, rand.Reader, decryptBenchBytes); err != nil {
		b.Fatalf("fixture: %v", err)
	}
	if err := f.Close(); err != nil {
		b.Fatalf("fixture: %v", err)
	}
	id, err := agecrypt.GenerateIdentity()
	if err != nil {
		b.Fatalf("identity: %v", err)
	}
	encPath = src + ageExt
	sums, err := agecrypt.Encrypt(context.Background(), src, encPath, []age.Recipient{id.Recipient()}, nil)
	if err != nil {
		b.Fatalf("encrypt: %v", err)
	}
	if err := os.Remove(src); err != nil {
		b.Fatalf("fixture: %v", err)
	}
	return encPath, sums.Cipher, []age.Identity{id}, sums.Plain
}

// BenchmarkDecryptVerifyingSynchronous measures the verified decryption as it
// was: both digests and the AEAD on one goroutine.
func BenchmarkDecryptVerifyingSynchronous(b *testing.B) {
	enc, want, ids, plain := decryptBenchFixture(b)
	dst := filepath.Join(b.TempDir(), "disc01.squashfs")
	ctx := context.Background()

	b.SetBytes(decryptBenchBytes)
	b.ResetTimer()
	for b.Loop() {
		if got, err := decryptVerifyingSynchronous(ctx, enc, dst, want, ids); err != nil || got != plain {
			b.Fatalf("decryptVerifyingSynchronous = %s, %v; want the plaintext digest %s", got, err, plain)
		}
	}
}

// BenchmarkDecryptVerifying measures the shipped one, with a goroutine per
// digest.
func BenchmarkDecryptVerifying(b *testing.B) {
	enc, want, ids, plain := decryptBenchFixture(b)
	dst := filepath.Join(b.TempDir(), "disc01.squashfs")
	ctx := context.Background()

	b.SetBytes(decryptBenchBytes)
	b.ResetTimer()
	for b.Loop() {
		if got, err := (Options{}).decryptVerifying(ctx, enc, dst, want, ids, nil); err != nil || got != plain {
			b.Fatalf("decryptVerifying = %s, %v; want the plaintext digest %s", got, err, plain)
		}
	}
}
