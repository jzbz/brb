package backup

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/iso"
	"github.com/jzbz/brb/internal/pack"
	"github.com/jzbz/brb/internal/scan"
	"github.com/jzbz/brb/internal/ui"
)

// quiet is a printer that throws its output away.
func quiet() *ui.Printer { return ui.New(io.Discard, false) }

// recordingPrinter is a printer whose output can be read back, for the checks
// that assert on what the operator was actually told rather than only on what
// the function returned.
func recordingPrinter() (*ui.Printer, func() string) {
	var b bytes.Buffer
	return ui.New(&b, false), b.String
}

func TestRequiredSpace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		image      int64
		redundancy int
		want       int64
	}{
		// One plaintext image and, beside it, one ciphertext with its parity —
		// the two that exist at the same moment. The round-trip check adds
		// nothing: it hashes the decrypted stream and stores none of it.
		{name: "no parity", image: 1000, redundancy: 0, want: 2000},
		{name: "ten percent", image: 1000, redundancy: 10, want: 2100},
		{name: "hundred percent", image: 1000, redundancy: 100, want: 3000},
		{
			// A real bd25 budget at the shipped defaults.
			name: "bd25 default", image: 21999955782, redundancy: 10,
			want: 21999955782 + 21999955782*110/100,
		},
		{name: "zero budget", image: 0, redundancy: 10, want: 0},
		{name: "negative budget", image: -5, redundancy: 10, want: 0},
		{name: "negative redundancy is treated as zero", image: 1000, redundancy: -1, want: 2000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RequiredSpace(tc.image, tc.redundancy); got != tc.want {
				t.Errorf("RequiredSpace(%d, %d) = %d, want %d", tc.image, tc.redundancy, got, tc.want)
			}
		})
	}
}

func TestRequiredSpaceIsBelowTwoAndAHalfImages(t *testing.T) {
	t.Parallel()
	// A sanity bound: the requirement must stay proportional to the budget, so
	// a preflight failure always names a believable number. It must also stay
	// ABOVE two images, because the plaintext and the ciphertext really do
	// coexist and a floor that forgot one of them would let a run start that
	// cannot finish its first disc.
	const image = 21999955782
	got := RequiredSpace(image, 10)
	if got < 2*image || got > 5*image/2 {
		t.Errorf("RequiredSpace = %d, want between %d and %d", got, 2*image, 5*image/2)
	}
}

// TestRoundTripNeedsNoStagingSpace. The round-trip check wants one thing from
// the decrypted image: its SHA-512. It used to get that by decrypting to a
// temporary file in <STAGING>/work — an image-sized sequential write plus an
// fsync, up to 95 GB on BD-XL media — which nothing ever opened and which was
// unlinked a moment later.
//
// The fixture makes work/ read-only, so a run that writes there at all cannot
// finish. That is the only way to observe the difference after the fact: the
// old code deleted its temporary file too, so a check for leftovers passes
// either way.
func TestRoundTripNeedsNoStagingSpace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: root writes into a read-only directory, so the fixture proves nothing")
	}
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := runnerFor(t, nil)
	r.opts.VerifyRoundTrip = true
	r.identities = []age.Identity{id}
	for _, d := range []string{r.dirs.Work, r.dirs.Img, r.dirs.Enc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Content that does not compress, so the ciphertext is a real stream and
	// not a few bytes of age header.
	plain := filepath.Join(r.dirs.Img, imageName(1))
	body := bytes.Repeat([]byte("brb round-trip fixture\n"), 40_000)
	if err := os.WriteFile(plain, body, 0o600); err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(r.dirs.Enc, imageName(1)+".age")
	sums, err := agecrypt.Encrypt(ctx, plain, enc, []age.Recipient{id.Recipient()}, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if err := os.Chmod(r.dirs.Work, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.dirs.Work, 0o700) })

	if err := r.roundTrip(ctx, 1, enc, int64(len(body)), sums.Plain); err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	if left, err := os.ReadDir(r.dirs.Work); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("%s holds %d file(s) after the round-trip", r.dirs.Work, len(left))
	}

	// The point of the check survives the change: a digest that does not match
	// still stops the run before the plaintext is deleted.
	wrong := strings.Repeat("0", len(sums.Plain))
	err = r.roundTrip(ctx, 1, enc, int64(len(body)), wrong)
	if err == nil || !strings.Contains(err.Error(), "round-trip mismatch") {
		t.Fatalf("roundTrip against the wrong digest = %v, want the mismatch refusal", err)
	}
}

// TestWriteSumsTakesTheKnownDigestShortcut pins the single wiring point of the
// project's largest I/O saving: SHA512SUMS is written from the digests the run
// already measured while encrypting, instead of re-reading twenty to ninety
// gigabytes per disc to arrive at the same numbers.
//
// The shortcut is keyed by the exact "./data/<name>" strings agecrypt records,
// built by hand on this side (see knownSums) and by a directory walk on the
// other. Nothing fails when they stop matching: agecrypt hashes the file the
// long way and writes a correct SHA512SUMS, so the only symptom is a run that
// is quietly hours slower. That is why this test plants a digest that is
// deliberately WRONG for the bytes on disc — it is the one observation that
// distinguishes "the shortcut was taken" from "the file was hashed".
func TestWriteSumsTakesTheKnownDigestShortcut(t *testing.T) {
	ctx := context.Background()
	r := runnerFor(t, nil)
	data := filepath.Join(r.dirs.Discs, discDirName(1), "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.dirs.Enc, 0o700); err != nil {
		t.Fatal(err)
	}

	// The disc directory's copies are hard links to enc/, which is what lets
	// agecrypt's inode check accept a digest taken from the other name.
	planted := map[string]string{}
	for _, name := range []string{imageName(1) + ".age", indexName} {
		src := filepath.Join(r.dirs.Enc, name)
		if err := os.WriteFile(src, []byte("ciphertext of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(src, filepath.Join(data, name)); err != nil {
			t.Fatal(err)
		}
		sum := sha512.Sum512([]byte("a digest of something else entirely: " + name))
		planted["./data/"+name] = hex.EncodeToString(sum[:])
	}
	r.discCipher[1] = planted["./data/"+imageName(1)+".age"]
	r.indexCipher = planted["./data/"+indexName]

	if err := r.writeSums(ctx, 1); err != nil {
		t.Fatalf("writeSums: %v", err)
	}
	sums, err := os.ReadFile(filepath.Join(r.dirs.Discs, discDirName(1), agecrypt.SumsName))
	if err != nil {
		t.Fatal(err)
	}
	for name, digest := range planted {
		if !strings.Contains(string(sums), digest+"  "+name) {
			t.Errorf("SHA512SUMS does not carry the recorded digest for %s — the shortcut's key "+
				"no longer matches the name agecrypt records, so every disc re-reads its image:\n%s",
				name, sums)
		}
	}

	// The other half of the contract, and the reason the keys can be trusted:
	// --verify-roundtrip is the operator asking for every byte to be proved
	// twice, so nothing is shortcut — and what the read-back finds is held to
	// the digest recorded at encryption. A planted digest that is wrong for
	// the bytes is, from here, exactly what a ciphertext that changed in
	// staging after its round-trip looks like, so the run must refuse. It used
	// to record the bytes as they were by then, attesting the change.
	r.opts.VerifyRoundTrip = true
	index, ok := r.knownSum(indexName, r.indexCipher)
	if !ok {
		t.Fatal("knownSum has no entry for the index this run measured")
	}
	for name, k := range r.knownSums(1, &index) {
		if k.Same != "" {
			t.Errorf("knownSums under --verify-roundtrip offers %s for the shortcut", name)
		}
	}
	err = r.writeSums(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), "has changed in staging since") {
		t.Fatalf("writeSums under --verify-roundtrip with digests the bytes do not match = %v, "+
			"want a refusal naming the mismatch", err)
	}
	// Refused before anything was written over the previous SHA512SUMS.
	after, rerr := os.ReadFile(filepath.Join(r.dirs.Discs, discDirName(1), agecrypt.SumsName))
	if rerr != nil || !bytes.Equal(after, sums) {
		t.Errorf("the refused writeSums replaced %s (err %v)", agecrypt.SumsName, rerr)
	}
}

// sumsFixture is a runner whose disc 1 is laid out by hand the way
// buildDiscDirs lays it out: the image and the index in enc/, each with the
// sidecar its encryption wrote, and hard-linked into the disc's data/. It
// returns the true ciphertext digests by name.
func sumsFixture(t *testing.T) (*runner, map[string]string) {
	t.Helper()
	return sumsFixtureDiscs(t, 1)
}

// sumsFixtureDiscs is sumsFixture over discs 1 to total, each with its own
// image and every one with a link to the one index.
func sumsFixtureDiscs(t *testing.T, total int) (*runner, map[string]string) {
	t.Helper()
	r := runnerFor(t, nil)
	for _, d := range []string{r.dirs.Enc, r.dirs.Work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	digests := map[string]string{}
	for n := 1; n <= total; n++ {
		data := filepath.Join(r.dirs.Discs, discDirName(n), "data")
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{imageName(n) + ".age", indexName} {
			src := filepath.Join(r.dirs.Enc, name)
			if _, done := digests[name]; !done {
				body := []byte("ciphertext of " + name)
				if err := os.WriteFile(src, body, 0o644); err != nil {
					t.Fatal(err)
				}
				sum := sha512.Sum512(body)
				digests[name] = hex.EncodeToString(sum[:])
				if err := agecrypt.WriteSumFile(src+".sha512", digests[name], name); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Link(src, filepath.Join(data, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r, digests
}

// rot changes one byte of path in place — same inode, so every hard link to
// it changes too, which is what bad RAM or a failing SSD does to a file.
func rot(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
}

// TestWriteSumsHoldsTheReadBackToTheEncryptionDigest. Whenever writeSums
// reads a large file back rather than taking the shortcut, what it finds must
// be what was encrypted. Under --verify-roundtrip, for a disc an earlier run
// finished, and for a copy linkOrCopy could not link, it used to record
// whatever the bytes hashed to by then — so a change in staging after
// encryption was written into SHA512SUMS as correct, and verify-disc then
// passed the disc.
func TestWriteSumsHoldsTheReadBackToTheEncryptionDigest(t *testing.T) {
	ctx := context.Background()
	img := imageName(1) + ".age"

	t.Run("an unchanged set is recorded, read back under --verify-roundtrip", func(t *testing.T) {
		r, digests := sumsFixture(t)
		r.opts.VerifyRoundTrip = true
		r.discCipher[1], r.indexCipher = digests[img], digests[indexName]
		if err := r.writeSums(ctx, 1); err != nil {
			t.Fatalf("writeSums: %v", err)
		}
		sums := readFileString(t, filepath.Join(r.dirs.Discs, discDirName(1), agecrypt.SumsName))
		for name, d := range digests {
			if !strings.Contains(sums, d+"  ./data/"+name+"\n") {
				t.Errorf("SHA512SUMS lacks the digest of %s:\n%s", name, sums)
			}
		}
	})

	t.Run("an image changed after its round-trip is refused, with the way out", func(t *testing.T) {
		r, digests := sumsFixture(t)
		r.opts.VerifyRoundTrip = true
		r.discCipher[1], r.indexCipher = digests[img], digests[indexName]
		rot(t, filepath.Join(r.dirs.Enc, img))
		err := r.writeSums(ctx, 1)
		if err == nil {
			t.Fatal("writeSums recorded an image that changed after encryption")
		}
		for _, want := range []string{
			"./data/" + img, digests[img], img + ".sha512",
			"par2 repair -- " + img + ".par2", "brb backup --resume",
			// The way out when par2 finds the image intact and the record
			// is what changed, rather than only "start the set over".
			"sha512sum " + img + " > " + img + ".sha512",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q:\n%v", want, err)
			}
		}
		// The disc's file is a link to enc/, so there is no copy to blame.
		if strings.Contains(err.Error(), "a file of its own") {
			t.Errorf("the refusal blames a copy where the disc holds a link:\n%v", err)
		}
		if _, serr := os.Stat(filepath.Join(r.dirs.Discs, discDirName(1), agecrypt.SumsName)); serr == nil {
			t.Error("a SHA512SUMS was written for the changed image")
		}
	})

	t.Run("a record that changed is repaired the way the refusal says", func(t *testing.T) {
		r, digests := sumsFixture(t)
		// The image is intact and its record is not: a well-formed digest of
		// something else, which is what a flipped hex digit looks like.
		sidecar := filepath.Join(r.dirs.Enc, img+".sha512")
		other := sha512.Sum512([]byte("not the image"))
		if err := agecrypt.WriteSumFile(sidecar, hex.EncodeToString(other[:]), img); err != nil {
			t.Fatal(err)
		}
		if err := r.writeSums(ctx, 1); err == nil {
			t.Fatal("writeSums recorded an image its record disagrees with")
		}
		// What `sha512sum <img> > <img>.sha512`, run in enc/ as the refusal
		// says, writes: GNU's text-mode line. The resume that follows takes
		// the digest from that file again.
		if err := os.WriteFile(sidecar, []byte(digests[img]+"  "+img+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.writeSums(ctx, 1); err != nil {
			t.Fatalf("writeSums after the record was written again with sha512sum: %v", err)
		}
	})

	t.Run("a disc an earlier run finished is held to its sidecar", func(t *testing.T) {
		r, digests := sumsFixture(t)
		// Nothing measured by this invocation: both digests come from enc/.
		if err := r.writeSums(ctx, 1); err != nil {
			t.Fatalf("writeSums over an unchanged resumed disc: %v", err)
		}
		rot(t, filepath.Join(r.dirs.Enc, img))
		err := r.writeSums(ctx, 1)
		if err == nil || !strings.Contains(err.Error(), digests[img]) {
			t.Fatalf("writeSums over a resumed disc whose image changed = %v, want a refusal", err)
		}
	})

	t.Run("a copy that could not be linked is held to the digest too", func(t *testing.T) {
		r, digests := sumsFixture(t)
		r.discCipher[1], r.indexCipher = digests[img], digests[indexName]
		// What linkOrCopy leaves when os.Link fails: a separate inode, so the
		// shortcut's inode check declines and the file is read.
		discCopy := filepath.Join(r.dirs.Discs, discDirName(1), "data", img)
		if err := os.Remove(discCopy); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(discCopy, []byte("Xiphertext of "+img), 0o644); err != nil {
			t.Fatal(err)
		}
		err := r.writeSums(ctx, 1)
		if err == nil {
			t.Fatal("writeSums recorded a disc copy that differs from what was encrypted")
		}
		// enc/ is intact here, so par2 would find nothing wrong with it and
		// the record is fine too: the first thing to try is the resume that
		// copies the file again, not a repair of either.
		msg := err.Error()
		for _, want := range []string{"a file of its own", "copies it again", "brb backup --resume"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal for a copy does not say %q:\n%v", want, err)
			}
		}
		if resume, par2 := strings.Index(msg, "brb backup --resume"), strings.Index(msg, "par2 repair"); par2 >= 0 && par2 < resume {
			t.Errorf("the refusal for a copy sends the operator to par2 before the resume:\n%v", err)
		}
	})

	t.Run("an index that changed is re-encrypted by a resume, or the set started over", func(t *testing.T) {
		r, digests := sumsFixture(t)
		r.opts.VerifyRoundTrip = true
		r.discCipher[1], r.indexCipher = digests[img], digests[indexName]
		rot(t, filepath.Join(r.dirs.Enc, indexName))
		err := r.writeSums(ctx, 1)
		if err == nil || !strings.Contains(err.Error(), "brb backup --resume") ||
			!strings.Contains(err.Error(), indexFileName) {
			t.Fatalf("writeSums over a changed index = %v, want a refusal sending the operator to --resume", err)
		}
		r.indexBuilt = true
		err = r.writeSums(ctx, 1)
		if err == nil {
			t.Fatal("writeSums recorded a changed index with the plaintext gone")
		}
		// With nothing to encrypt it from again, the index can still be proved
		// sound by decrypting it, and then only its record needs writing again.
		for _, want := range []string{
			"gunzip -t", "sha512sum " + indexName + " > " + indexName + ".sha512", "start the set over",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for an index with the plaintext gone does not say %q:\n%v", want, err)
			}
		}
		if strings.Contains(err.Error(), "a file of its own") {
			t.Errorf("the refusal blames a copy where the disc holds a link:\n%v", err)
		}
		// And when the disc's index is a copy, the resume that copies it again
		// comes first.
		discCopy := filepath.Join(r.dirs.Discs, discDirName(1), "data", indexName)
		if err := os.Remove(discCopy); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(discCopy, []byte("a copy that differs"), 0o644); err != nil {
			t.Fatal(err)
		}
		err = r.writeSums(ctx, 1)
		if err == nil || !strings.Contains(err.Error(), "a file of its own") ||
			!strings.Contains(err.Error(), "gunzip -t") {
			t.Fatalf("writeSums over a copied index with the plaintext gone = %v, want the copy "+
				"named first and the record's repair after it", err)
		}
	})

	t.Run("the index's sidecar is read once for the whole set", func(t *testing.T) {
		r, _ := sumsFixtureDiscs(t, 3)
		p, out := recordingPrinter()
		r.p = p
		if err := os.WriteFile(filepath.Join(r.dirs.Enc, indexName+".sha512"), []byte("not a checksum line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.writeSums(ctx, 3); err != nil {
			t.Fatalf("writeSums with a rotted index sidecar: %v", err)
		}
		// The index is on every disc; its warning is about one file.
		if got := strings.Count(out(), "without being compared"); got != 1 {
			t.Errorf("the rotted index sidecar was warned about %d times over three discs, want once:\n%s",
				got, out())
		}
		for n := 1; n <= 3; n++ {
			sums := readFileString(t, filepath.Join(r.dirs.Discs, discDirName(n), agecrypt.SumsName))
			if !strings.Contains(sums, "./data/"+indexName+"\n") {
				t.Errorf("disc %d's SHA512SUMS does not cover the index:\n%s", n, sums)
			}
		}
	})

	t.Run("a sidecar that cannot be read is warned about, not refused", func(t *testing.T) {
		r, _ := sumsFixture(t)
		p, out := recordingPrinter()
		r.p = p
		if err := os.WriteFile(filepath.Join(r.dirs.Enc, img+".sha512"), []byte("not a checksum line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.writeSums(ctx, 1); err != nil {
			t.Fatalf("writeSums with a rotted sidecar: %v", err)
		}
		if !strings.Contains(out(), "without being compared") {
			t.Errorf("no warning that %s went uncompared:\n%s", img, out())
		}
	})
}

func TestFreeSpaceOfATempDir(t *testing.T) {
	t.Parallel()
	n, err := freeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("freeSpace: %v", err)
	}
	if n <= 0 {
		t.Errorf("freeSpace = %d, want a positive number of bytes", n)
	}
}

func TestFreeSpaceOfAMissingDirectory(t *testing.T) {
	t.Parallel()
	if _, err := freeSpace(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("freeSpace of a missing directory succeeded, want an error")
	}
}

func TestCheckSpace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	avail, err := freeSpace(dir)
	if err != nil {
		t.Fatalf("freeSpace: %v", err)
	}

	cfg := config.Default()
	cfg.Staging = dir
	cfg.Par2Redundancy = 10

	// A budget that comfortably fits.
	r := &runner{cfg: cfg, p: quiet(), budget: disc.Budget{Image: 1 << 10}}
	if err := r.checkSpace(); err != nil {
		t.Errorf("checkSpace with a tiny budget: %v", err)
	}

	// A budget that cannot possibly fit.
	big := avail // one image plus its ciphertext already exceeds what is available
	r = &runner{cfg: cfg, p: quiet(), budget: disc.Budget{Image: big}}
	err = r.checkSpace()
	if err == nil {
		t.Fatal("checkSpace with an impossible budget succeeded, want an error")
	}
	for _, want := range []string{"need", "have", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRawBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image int64
		ratio float64
		want  int64
	}{
		{name: "ratio one", image: 1000, ratio: 1.0, want: 1000},
		{name: "half", image: 1000, ratio: 0.5, want: 2000},
		{name: "truncates like awk printf %d", image: 1000, ratio: 0.3, want: 3333},
		{name: "above one shrinks the budget", image: 1000, ratio: 1.25, want: 800},
		{name: "zero ratio falls back to 1.0", image: 1000, ratio: 0, want: 1000},
		{name: "negative ratio falls back to 1.0", image: 1000, ratio: -2, want: 1000},
		{name: "NaN falls back to 1.0", image: 1000, ratio: math.NaN(), want: 1000},
		{name: "Inf falls back to 1.0", image: 1000, ratio: math.Inf(1), want: 1000},
		{name: "never returns zero", image: 1, ratio: 1000, want: 1},
		// A ratio small enough that budget/ratio leaves the int64 range. The
		// conversion is implementation-defined there — amd64 yields MinInt64 —
		// so without the floor this came back negative. doctor printed exactly
		// that, -9223372036854775808 B, until it stopped carrying its own copy
		// of this function.
		{name: "a ratio too small to convert", image: 21999955782, ratio: 1e-10, want: 1},
		{
			name: "bd25 default budget", image: 21999955782, ratio: 1.0, want: 21999955782,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RawBudget(tc.image, tc.ratio); got != tc.want {
				t.Errorf("RawBudget(%d, %v) = %d, want %d", tc.image, tc.ratio, got, tc.want)
			}
		})
	}
}

func TestMeasuredRatio(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		image, raw int64
		want       float64
	}{
		{name: "incompressible", image: 1000, raw: 1000, want: 1.0},
		{name: "compressed to two thirds", image: 2000, raw: 3000, want: 0.667},
		{name: "expanded", image: 1100, raw: 1000, want: 1.1},
		{name: "rounds to three places", image: 1234, raw: 10000, want: 0.123},
		{name: "empty bin", image: 500, raw: 0, want: 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := measuredRatio(tc.image, tc.raw); got != tc.want {
				t.Errorf("measuredRatio(%d, %d) = %v, want %v", tc.image, tc.raw, got, tc.want)
			}
		})
	}
}

func TestShrinkRatio(t *testing.T) {
	t.Parallel()
	// Two roundings, not one: the measured ratio is rounded to three decimals
	// and the margin is applied to THAT, so the ratio in the log is the ratio
	// the re-pack used. Collapsing them changes the budget in the last digit.
	tests := []struct {
		name       string
		image, raw int64
		want       float64
	}{
		{name: "no compression", image: 1000, raw: 1000, want: 1.05},
		{name: "measured 0.900", image: 900, raw: 1000, want: 0.945},
		{name: "measured 0.667", image: 2000, raw: 3000, want: 0.7},
		{name: "measured 0.620", image: 620, raw: 1000, want: 0.651},
		{name: "image bigger than raw", image: 1500, raw: 1000, want: 1.575},
		{name: "empty bin falls back to 1.0", image: 700, raw: 0, want: 1.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shrinkRatio(tc.image, tc.raw); got != tc.want {
				t.Errorf("shrinkRatio(%d, %d) = %v, want %v", tc.image, tc.raw, got, tc.want)
			}
		})
	}
}

func TestShrinkRetryShrinksTheBudget(t *testing.T) {
	t.Parallel()
	// The property the retry loop depends on: an image that overshot its budget
	// yields a raw budget strictly smaller than the raw content that produced
	// it, so the re-planned bin is genuinely smaller.
	const imageBudget = 1_000_000
	cases := []struct{ imageSize, rawBytes int64 }{
		{imageSize: 1_100_000, rawBytes: 1_000_000},
		{imageSize: 1_050_000, rawBytes: 2_000_000},
		{imageSize: 4_000_000, rawBytes: 4_000_000},
		{imageSize: 1_000_001, rawBytes: 1_000_000},
	}
	for _, c := range cases {
		ratio := shrinkRatio(c.imageSize, c.rawBytes)
		got := RawBudget(imageBudget, ratio)
		if got >= c.rawBytes {
			t.Errorf("image %d from %d raw: new raw budget %d is not below %d (ratio %v)",
				c.imageSize, c.rawBytes, got, c.rawBytes, ratio)
		}
	}
}

func TestNamingMatchesBrbSh(t *testing.T) {
	t.Parallel()
	if got := imageName(3); got != "disc03.squashfs" {
		t.Errorf("imageName(3) = %q, want disc03.squashfs", got)
	}
	if got := imageName(12); got != "disc12.squashfs" {
		t.Errorf("imageName(12) = %q, want disc12.squashfs", got)
	}
	if got := imageName(100); got != "disc100.squashfs" {
		t.Errorf("imageName(100) = %q, want disc100.squashfs", got)
	}
	if got := discDirName(7); got != "disc07" {
		t.Errorf("discDirName(7) = %q, want disc07", got)
	}
	if got := iso.Name(7); got != "disc07.iso" {
		t.Errorf("iso.Name(7) = %q, want disc07.iso", got)
	}
	if indexName != "index.tsv.gz.age" {
		t.Errorf("indexName = %q, want index.tsv.gz.age", indexName)
	}
}

// writeTree creates a directory holding files of the given sizes, named a, b, …
func writeTree(t *testing.T, sizes ...int64) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, sz := range sizes {
		name := filepath.Join(dir, "sub", string(rune('a'+i))+".bin")
		if err := os.WriteFile(name, make([]byte, sz), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// planCfg is a configuration whose disc budget is small enough to test with.
func planCfg(t *testing.T, src string, capacity int64) *config.Config {
	t.Helper()
	c := config.Default()
	c.SourceDir = src
	c.Staging = t.TempDir()
	c.ArchiveName = "test-archive"
	c.DiscCapacityBytes = capacity
	c.ReserveBytes = 0
	c.Par2Redundancy = 10
	c.PackRatio = 1.0
	c.PruneDirs = nil
	c.ExcludeMasks = nil
	return c
}

func TestPlan(t *testing.T) {
	t.Parallel()
	// capacity 1 MiB -> usable 1027604 -> image budget 925769, so three
	// 300000-byte files fit on a disc and the fourth does not.
	src := writeTree(t, 300000, 300000, 300000, 300000, 300000)
	cfg := planCfg(t, src, 1<<20)

	got, err := Plan(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Discs != 2 {
		t.Fatalf("Discs = %d, want 2 (%+v)", got.Discs, got.PerDisc)
	}
	if got.RawBytes != 1500000 {
		t.Errorf("RawBytes = %d, want 1500000", got.RawBytes)
	}
	if got.PackRatio != 1.0 {
		t.Errorf("PackRatio = %v, want 1", got.PackRatio)
	}
	if len(got.PerDisc) != 2 {
		t.Fatalf("PerDisc has %d entries, want 2", len(got.PerDisc))
	}
	if got.PerDisc[0].Index != 1 || got.PerDisc[0].Files != 3 || got.PerDisc[0].RawBytes != 900000 {
		t.Errorf("disc 1 = %+v, want {Index:1 Files:3 RawBytes:900000}", got.PerDisc[0])
	}
	if got.PerDisc[1].Index != 2 || got.PerDisc[1].Files != 2 || got.PerDisc[1].RawBytes != 600000 {
		t.Errorf("disc 2 = %+v, want {Index:2 Files:2 RawBytes:600000}", got.PerDisc[1])
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 300000, 200000, 300000, 100000, 250000)
	cfg := planCfg(t, src, 1<<20)

	first, err := Plan(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := Plan(context.Background(), Options{Cfg: cfg})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if again.Discs != first.Discs || len(again.PerDisc) != len(first.PerDisc) {
			t.Fatalf("run %d differs: %+v vs %+v", i, again, first)
		}
		for j := range first.PerDisc {
			if again.PerDisc[j] != first.PerDisc[j] {
				t.Errorf("run %d disc %d = %+v, want %+v", i, j, again.PerDisc[j], first.PerDisc[j])
			}
		}
	}
}

func TestPlanRejectsAFileLargerThanADisc(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 2_000_000)
	cfg := planCfg(t, src, 1<<20)

	_, err := Plan(context.Background(), Options{Cfg: cfg})
	if err == nil {
		t.Fatal("Plan succeeded with an oversized file, want an error")
	}
	for _, want := range []string{"larger than one disc", "a.bin", "EXCLUDE_MASKS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPlanHonoursThePackRatio(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 300000, 300000, 300000, 300000, 300000)
	cfg := planCfg(t, src, 1<<20)
	cfg.PackRatio = 0.5 // expect 2:1 compression, so twice as much fits

	got, err := Plan(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Discs != 1 {
		t.Errorf("Discs = %d at ratio 0.5, want 1 (%+v)", got.Discs, got.PerDisc)
	}
}

func TestPlanRejectsAnEmptyTree(t *testing.T) {
	t.Parallel()
	cfg := planCfg(t, writeTree(t), 1<<20)
	_, err := Plan(context.Background(), Options{Cfg: cfg})
	if err == nil {
		t.Fatal("Plan of a tree with no files succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "nothing to pack") {
		t.Errorf("error %q does not explain that there was nothing to pack", err)
	}
}

func TestPlanNeedsAConfiguration(t *testing.T) {
	t.Parallel()
	if _, err := Plan(context.Background(), Options{}); err == nil {
		t.Fatal("Plan without a configuration succeeded, want an error")
	}
}

func TestPlanAbortsOnCancellation(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 1000, 1000)
	cfg := planCfg(t, src, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Plan(ctx, Options{Cfg: cfg})
	if err == nil {
		t.Fatal("Plan succeeded with a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}

func TestResumeFilterDropsAssignedFiles(t *testing.T) {
	t.Parallel()
	res := &scan.Result{
		RawBytes: 300,
		Entries: []scan.Entry{
			{Rel: "sub", Kind: scan.KindDir},
			{Rel: "sub/a", Kind: scan.KindFile, Size: 100},
			{Rel: "sub/b", Kind: scan.KindFile, Size: 100},
			{Rel: "sub/c", Kind: scan.KindFile, Size: 100},
		},
	}
	r := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 300,
		Assigned: []string{"sub/a", "sub/c"},
	}}

	got, err := r.resumeFilter(res)
	if err != nil {
		t.Fatalf("resumeFilter: %v", err)
	}
	want := []string{"sub", "sub/b"}
	if len(got) != len(want) {
		t.Fatalf("kept %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Rel != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i].Rel, want[i])
		}
	}
}

func TestResumeFilterKeepsEverythingOnAFreshRun(t *testing.T) {
	t.Parallel()
	res := &scan.Result{
		RawBytes: 100,
		Entries:  []scan.Entry{{Rel: "a", Kind: scan.KindFile, Size: 100}},
	}
	r := &runner{p: quiet(), st: newState("arch", "/src", 1.0, time.Now())}
	got, err := r.resumeFilter(res)
	if err != nil {
		t.Fatalf("resumeFilter: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("kept %d entries, want 1", len(got))
	}
	if r.st.ScanRawSize != 100 {
		t.Errorf("ScanRawSize = %d, want 100", r.st.ScanRawSize)
	}
}

// TestResumeFilterRefusesAnAssignedFileThatBecameSomethingElse pins the refusal
// that stands between a resume and a set that cannot be restored.
//
// A path on a finished disc is a regular file there for good; the disc is
// written and may already be burned. If the source tree has since replaced it
// with a directory, the skeleton every later disc carries holds it as a
// directory, and unsquashfs refuses the mismatch part way through the restore
// with "failed to lstat ..., because Not a directory". Before this check, the
// assigned lookup happened only for KindFile entries, so the changed path was
// treated as new, the run exited 0 saying "backup complete", and the set was
// discovered to be unreadable only by someone restoring it.
func TestResumeFilterRefusesAnAssignedFileThatBecameSomethingElse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind scan.Kind
	}{
		{"directory", scan.KindDir},
		{"symlink", scan.KindSymlink},
		{"device node", scan.KindOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := &scan.Result{
				RawBytes: 200,
				Entries: []scan.Entry{
					{Rel: "keep", Kind: scan.KindFile, Size: 100},
					{Rel: "swapped", Kind: tc.kind},
					{Rel: "swapped/inner", Kind: scan.KindFile, Size: 100},
				},
			}
			r := &runner{p: quiet(), st: &State{
				Version: StateVersion, DiscsDone: 1, ScanRawSize: 200,
				Assigned: []string{"swapped"},
			}}

			got, err := r.resumeFilter(res)
			if err == nil {
				t.Fatalf("resumeFilter accepted a resume whose %s replaced an assigned file; kept %+v", tc.name, got)
			}
			if got != nil {
				t.Errorf("entries = %+v, want nil alongside the refusal", got)
			}
			// The operator has to find the path to put it back, so the message
			// has to name it and say what it is now.
			if !strings.Contains(err.Error(), "swapped") {
				t.Errorf("error does not name the offending path: %v", err)
			}
			if !strings.Contains(err.Error(), tc.kind.String()) {
				t.Errorf("error does not say what %q is now (%s): %v", "swapped", tc.kind, err)
			}
		})
	}
}

// TestResumeFilterAcceptsAnUnchangedKind is the companion that stops the check
// above from passing by refusing everything: a directory that was always a
// directory is not in the assigned set and must still travel in the skeleton.
func TestResumeFilterAcceptsAnUnchangedKind(t *testing.T) {
	t.Parallel()
	res := &scan.Result{
		RawBytes: 100,
		Entries: []scan.Entry{
			{Rel: "sub", Kind: scan.KindDir},
			{Rel: "sub/a", Kind: scan.KindFile, Size: 100},
		},
	}
	r := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 100,
		Assigned: []string{"sub/a"},
	}}

	got, err := r.resumeFilter(res)
	if err != nil {
		t.Fatalf("resumeFilter refused an unchanged tree: %v", err)
	}
	if len(got) != 1 || got[0].Rel != "sub" {
		t.Errorf("kept %+v, want just the skeleton entry sub", got)
	}
}

// TestResumeFilterRefusesAWrittenSkeletonPathThatChangedKind pins the other
// half of the refusal above: a path a finished disc holds as a directory, a
// symlink or a device node is that kind there for good, exactly as a written
// file is a file.
//
// Before the check, nothing recorded those kinds, so a directory that became a
// file — or the realistic one, a full disk answered by moving ~/Photos to a
// bigger volume and symlinking it back — was accepted, every later disc
// carried the path as its new kind, and the set could not be restored as a
// whole: no unsquashfs replaces a populated directory with a leaf, and a
// symlink where a disc holds a directory is what both readers' guards refuse.
//
// Every case leaves something to write, which is what the refusal guards (see
// TestResumeFilterOnlyWarnsWhenNothingIsLeftToWrite). In the cases where a
// path became a regular file, that file is the only thing left, so they also
// pin that it counts.
//
// Two sources of "what kind was it", both covered: the directories that hold
// an assigned file, which even a state file from before State.Skeleton can
// supply, and the recorded skeleton, which is the only way to know about an
// empty directory, a directory of symlinks, or a leaf.
func TestResumeFilterRefusesAWrittenSkeletonPathThatChangedKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		assigned []string
		skeleton []string
		now      []scan.Entry
		path     string
		was, is  scan.Kind
	}{
		{
			name:     "populated directory now a file, no skeleton recorded",
			assigned: []string{"keep", "tree/x"},
			now:      []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1}, {Rel: "tree", Kind: scan.KindFile, Size: 1}},
			path:     "tree", was: scan.KindDir, is: scan.KindFile,
		},
		{
			name:     "populated directory moved away and symlinked back",
			assigned: []string{"Photos/a.jpg", "Photos/b.jpg"},
			now:      []scan.Entry{{Rel: "Photos", Kind: scan.KindSymlink}, {Rel: "new", Kind: scan.KindFile, Size: 1}},
			path:     "Photos", was: scan.KindDir, is: scan.KindSymlink,
		},
		{
			name:     "an ancestor of an assigned file, not its parent",
			assigned: []string{"a/b/c/x"},
			now:      []scan.Entry{{Rel: "a", Kind: scan.KindDir}, {Rel: "a/b", Kind: scan.KindFile, Size: 1}},
			path:     "a/b", was: scan.KindDir, is: scan.KindFile,
		},
		{
			name:     "populated directory now a fifo",
			assigned: []string{"tree/x"},
			now:      []scan.Entry{{Rel: "new", Kind: scan.KindFile, Size: 1}, {Rel: "tree", Kind: scan.KindOther}},
			path:     "tree", was: scan.KindDir, is: scan.KindOther,
		},
		{
			name:     "empty directory now a file",
			assigned: []string{"keep"},
			skeleton: []string{"d empty"},
			now:      []scan.Entry{{Rel: "empty", Kind: scan.KindFile, Size: 1}, {Rel: "keep", Kind: scan.KindFile, Size: 1}},
			path:     "empty", was: scan.KindDir, is: scan.KindFile,
		},
		{
			name:     "directory of symlinks now a file",
			assigned: []string{"keep"},
			skeleton: []string{"d links", "l links/one"},
			now:      []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1}, {Rel: "links", Kind: scan.KindFile, Size: 1}},
			path:     "links", was: scan.KindDir, is: scan.KindFile,
		},
		{
			name:     "symlink now a directory",
			assigned: []string{"keep"},
			skeleton: []string{"l ln"},
			now: []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1},
				{Rel: "ln", Kind: scan.KindDir}, {Rel: "ln/new", Kind: scan.KindFile, Size: 1}},
			path: "ln", was: scan.KindSymlink, is: scan.KindDir,
		},
		{
			name:     "device node now a directory",
			assigned: []string{"keep"},
			skeleton: []string{"o dev/null"},
			now: []scan.Entry{{Rel: "dev", Kind: scan.KindDir}, {Rel: "dev/null", Kind: scan.KindDir},
				{Rel: "keep", Kind: scan.KindFile, Size: 1}, {Rel: "new", Kind: scan.KindFile, Size: 1}},
			path: "dev/null", was: scan.KindOther, is: scan.KindDir,
		},
		{
			// squashfs-tools 4.5 writes a regular file by opening whatever holds
			// its path, so the later disc's file goes through the earlier
			// disc's link.
			name:     "symlink now a regular file",
			assigned: []string{"keep"},
			skeleton: []string{"l ln"},
			now:      []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1}, {Rel: "ln", Kind: scan.KindFile, Size: 1}},
			path:     "ln", was: scan.KindSymlink, is: scan.KindFile,
		},
		{
			name:     "symlink now a fifo",
			assigned: []string{"keep"},
			skeleton: []string{"l p"},
			now: []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1},
				{Rel: "new", Kind: scan.KindFile, Size: 1}, {Rel: "p", Kind: scan.KindOther}},
			path: "p", was: scan.KindSymlink, is: scan.KindOther,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &runner{p: quiet(), st: &State{
				Version: StateVersion, DiscsDone: 2, ScanRawSize: 2,
				Assigned: tc.assigned, Skeleton: tc.skeleton,
			}}
			got, err := r.resumeFilter(&scan.Result{RawBytes: 2, Entries: tc.now})
			if err == nil {
				t.Fatalf("resumeFilter accepted %s; kept %+v", tc.name, got)
			}
			if got != nil {
				t.Errorf("entries = %+v, want nil alongside the refusal", got)
			}
			// Putting the path back is the repair, so the message has to name
			// it and say what it was and what it is now.
			line := fmt.Sprintf("%-7s -> %-7s  %q", tc.was, tc.is, tc.path)
			if !strings.Contains(err.Error(), line) {
				t.Errorf("error does not carry the line %q:\n%v", line, err)
			}
			if !strings.Contains(err.Error(), "2 finished disc(s)") {
				t.Errorf("error does not say how many discs are finished:\n%v", err)
			}
		})
	}
}

// TestResumeFilterAcceptsWhatTheFinishedDiscsCanAbsorb is the companion that
// stops the check above from passing by refusing too much. Deleting things is
// what --resume is documented to cope with, and a name that merely shares a
// prefix with a recorded directory is not inside it.
func TestResumeFilterAcceptsWhatTheFinishedDiscsCanAbsorb(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		assigned []string
		skeleton []string
		now      []scan.Entry
	}{
		{
			name:     "directory kept, its written file deleted",
			assigned: []string{"tree/x"},
			now:      []scan.Entry{{Rel: "tree", Kind: scan.KindDir}, {Rel: "tree/new", Kind: scan.KindFile, Size: 1}},
		},
		{
			name:     "directory removed outright",
			assigned: []string{"keep", "tree/x"},
			skeleton: []string{"d tree", "l tree/ln"},
			now:      []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1}},
		},
		{
			name:     "siblings that only share a prefix",
			assigned: []string{"tree/x"},
			skeleton: []string{"d tree"},
			now: []scan.Entry{{Rel: "tre", Kind: scan.KindFile, Size: 1}, {Rel: "tree", Kind: scan.KindDir},
				{Rel: "tree.x", Kind: scan.KindSymlink}, {Rel: "tree/x", Kind: scan.KindFile, Size: 1}},
		},
		{
			name:     "every recorded kind unchanged",
			assigned: []string{"d/f"},
			skeleton: []string{"d d", "d d/empty", "l d/ln", "o d/fifo"},
			now: []scan.Entry{{Rel: "d", Kind: scan.KindDir}, {Rel: "d/empty", Kind: scan.KindDir},
				{Rel: "d/f", Kind: scan.KindFile, Size: 1}, {Rel: "d/fifo", Kind: scan.KindOther},
				{Rel: "d/ln", Kind: scan.KindSymlink}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &runner{p: quiet(), st: &State{
				Version: StateVersion, DiscsDone: 1, ScanRawSize: 1,
				Assigned: tc.assigned, Skeleton: tc.skeleton,
			}}
			if _, err := r.resumeFilter(&scan.Result{RawBytes: 1, Entries: tc.now}); err != nil {
				t.Fatalf("resumeFilter refused %s: %v", tc.name, err)
			}
		})
	}
}

// TestResumeFilterReportsBothDirectionsInOneRefusal: a written file that became
// a directory and a written directory that became a file are one mistake as far
// as the operator is concerned, and a refusal that named only the first would
// send them back to fix it and then refuse again for the second.
func TestResumeFilterReportsBothDirectionsInOneRefusal(t *testing.T) {
	t.Parallel()
	r := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 2,
		Assigned: []string{"was-file", "was-dir/x"},
	}}
	_, err := r.resumeFilter(&scan.Result{RawBytes: 2, Entries: []scan.Entry{
		{Rel: "was-dir", Kind: scan.KindFile, Size: 1},
		{Rel: "was-file", Kind: scan.KindDir},
	}})
	if err == nil {
		t.Fatal("resumeFilter accepted two changed kinds")
	}
	for _, w := range []string{"2 path(s)", `file    -> dir      "was-file"`, `dir     -> file     "was-dir"`} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error does not say %q:\n%v", w, err)
		}
	}
}

// TestResumeFilterOnlyWarnsWhenNothingIsLeftToWrite: the refusal above guards
// the discs still to come, and a resume can have none — one interrupted after
// its last disc was recorded, in the index, the payload or the eager ISOs. No
// further disc then carries the new kind, the finished discs agree with one
// another, and refusing would only demand back the space that moving the
// directory away freed, or a finished set thrown away. The change is named in
// a warning instead, and is kept out of the skeleton record, since no disc
// will hold it.
//
// The one change that still refuses is a path that became a regular file: that
// file is itself left to write, and only a new disc could take it.
func TestResumeFilterOnlyWarnsWhenNothingIsLeftToWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		assigned []string
		skeleton []string
		now      []scan.Entry
		line     string
	}{
		{
			name:     "populated directory moved away and symlinked back",
			assigned: []string{"Photos/a.jpg", "Photos/b.jpg"},
			now:      []scan.Entry{{Rel: "Photos", Kind: scan.KindSymlink}},
			line:     `dir     -> symlink  "Photos"`,
		},
		{
			name:     "written file now a symlink",
			assigned: []string{"f", "keep"},
			now:      []scan.Entry{{Rel: "f", Kind: scan.KindSymlink}, {Rel: "keep", Kind: scan.KindFile, Size: 1}},
			line:     `file    -> symlink  "f"`,
		},
		{
			name:     "symlink now a fifo",
			assigned: []string{"keep"},
			skeleton: []string{"l p"},
			now:      []scan.Entry{{Rel: "keep", Kind: scan.KindFile, Size: 1}, {Rel: "p", Kind: scan.KindOther}},
			line:     `symlink -> other    "p"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, log := recordingPrinter()
			st := &State{
				Version: StateVersion, DiscsDone: 2, ScanRawSize: 2,
				Assigned: tc.assigned, Skeleton: tc.skeleton,
			}
			r := &runner{p: p, st: st}
			out, err := r.resumeFilter(&scan.Result{RawBytes: 2, Entries: tc.now})
			if err != nil {
				t.Fatalf("resumeFilter refused %s with nothing left to write: %v", tc.name, err)
			}
			for _, e := range out {
				if e.Kind == scan.KindFile {
					t.Errorf("left %q to write; the case is meant to have nothing", e.Rel)
				}
			}
			if got := log(); !strings.Contains(got, tc.line) {
				t.Errorf("the warning does not carry the line %q:\n%s", tc.line, got)
			}
			if !slices.Equal(st.Skeleton, tc.skeleton) {
				t.Errorf("the record became %q, want it left at %q: no disc will hold the new kind",
					st.Skeleton, tc.skeleton)
			}
		})
	}

	r := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 2, ScanRawSize: 2,
		Assigned: []string{"Photos/a.jpg"},
	}}
	_, err := r.resumeFilter(&scan.Result{RawBytes: 2, Entries: []scan.Entry{{Rel: "Photos", Kind: scan.KindFile, Size: 1}}})
	if err == nil || !strings.Contains(err.Error(), `dir     -> file     "Photos"`) {
		t.Errorf("a directory that became a file, with nothing else to write = %v, want the refusal naming it", err)
	}
}

// TestResumeFilterRecordsTheSkeletonItWillWrite: the record the refusal above
// checks against is only as good as what each run adds to it. A fresh run
// records its own skeleton; a resumed one keeps everything already recorded —
// an entry the tree has since lost is still on the discs that were written —
// adds the directories its assigned files imply, which is what lets a state
// file from before the field existed catch up, and adds its own scan's.
func TestResumeFilterRecordsTheSkeletonItWillWrite(t *testing.T) {
	t.Parallel()
	fresh := &runner{p: quiet(), st: newState("arch", "/src", 1.0, time.Now())}
	if _, err := fresh.resumeFilter(&scan.Result{RawBytes: 1, Entries: []scan.Entry{
		{Rel: "a", Kind: scan.KindDir}, {Rel: "a/f", Kind: scan.KindFile, Size: 1},
		{Rel: "a/ln", Kind: scan.KindSymlink}, {Rel: "p", Kind: scan.KindOther},
	}}); err != nil {
		t.Fatalf("resumeFilter on a fresh run: %v", err)
	}
	if want := []string{"d a", "l a/ln", "o p"}; !slices.Equal(fresh.st.Skeleton, want) {
		t.Errorf("a fresh run recorded %q, want %q", fresh.st.Skeleton, want)
	}

	resumed := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 1,
		Assigned: []string{"old/deep/f"},
		Skeleton: []string{"d gone", "l gone/ln"},
	}}
	if _, err := resumed.resumeFilter(&scan.Result{RawBytes: 1, Entries: []scan.Entry{
		{Rel: "new", Kind: scan.KindDir}, {Rel: "new/f", Kind: scan.KindFile, Size: 1},
		{Rel: "old", Kind: scan.KindDir}, {Rel: "old/deep", Kind: scan.KindDir},
		{Rel: "old/deep/f", Kind: scan.KindFile, Size: 1},
	}}); err != nil {
		t.Fatalf("resumeFilter on a resumed run: %v", err)
	}
	want := []string{"d gone", "d new", "d old", "d old/deep", "l gone/ln"}
	if !slices.Equal(resumed.st.Skeleton, want) {
		t.Errorf("a resumed run recorded %q, want %q", resumed.st.Skeleton, want)
	}
}

// TestResumeFilterRecordSurvivesTheStateFile is the wiring test for the record:
// what one run notes about its skeleton has to reach the next run through
// state.json, the only thing the two share. An empty directory is the case
// that proves it, because nothing but the record knows it was there.
func TestResumeFilterRecordSurvivesTheStateFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	first := &runner{p: quiet(), st: newState("arch", "/src", 1.0, time.Now())}
	if _, err := first.resumeFilter(&scan.Result{RawBytes: 2, Entries: []scan.Entry{
		{Rel: "empty-\xff", Kind: scan.KindDir}, {Rel: "f", Kind: scan.KindFile, Size: 1},
		{Rel: "g", Kind: scan.KindFile, Size: 1},
	}}); err != nil {
		t.Fatalf("resumeFilter on the first run: %v", err)
	}
	// What oneDisc does once disc 1 is finished.
	first.st.DiscsDone, first.st.Assigned = 1, []string{"f"}
	if err := SaveState(path, first.st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	second := &runner{p: quiet(), st: st}
	_, err = second.resumeFilter(&scan.Result{RawBytes: 2, Entries: []scan.Entry{
		{Rel: "empty-\xff", Kind: scan.KindFile, Size: 1}, {Rel: "f", Kind: scan.KindFile, Size: 1},
		{Rel: "g", Kind: scan.KindFile, Size: 1},
	}})
	if err == nil || !strings.Contains(err.Error(), `dir     -> file     "empty-\xff"`) {
		t.Errorf("a resume after the empty directory became a file = %v, want the refusal naming it", err)
	}
}

// TestResumeFilterRefusesADamagedSkeletonRecord: an entry that does not parse
// is not a kind anyone can check against, and skipping it would drop the check.
func TestResumeFilterRefusesADamagedSkeletonRecord(t *testing.T) {
	t.Parallel()
	r := &runner{p: quiet(), st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 1,
		Assigned: []string{"f"}, Skeleton: []string{"x what"},
	}}
	if _, err := r.resumeFilter(&scan.Result{RawBytes: 1, Entries: []scan.Entry{{Rel: "f", Kind: scan.KindFile, Size: 1}}}); err == nil {
		t.Fatal("resumeFilter accepted a skeleton entry with no kind it knows")
	}
}

// TestEmptyBinErrorNamesTheFileAndNotPackRatio. When an image overshoots, the
// shrink loop re-plans the disc at the ratio the content actually achieved —
// a SMALLER raw budget than the one buildDiscs checked for oversized files. A
// file between the two budgets therefore gets past the up-front check and is
// discovered here, with nothing left that fits.
//
// The message this path used to print told the operator to "lower PACK_RATIO
// manually". That is inert: the budget here comes from the measured ratio, so
// a re-run at any PACK_RATIO packs the same bin, measures the same overshoot
// and stops in exactly the same place. The operator has to be told which file
// it is and what actually works.
func TestEmptyBinErrorNamesTheFileAndNotPackRatio(t *testing.T) {
	t.Parallel()
	p := pack.New([]scan.Entry{
		{Rel: "media/one-big-mkv", Kind: scan.KindFile, Size: 900},
		{Rel: "media/notes.txt", Kind: scan.KindFile, Size: 10},
	})
	// The budget after a 1.05 shrink of a 1000-byte image budget: the 900-byte
	// file fitted the planned budget and does not fit the measured one.
	const rb = 800
	if _, ok := p.Next(rb); ok {
		// A bin containing only the small file is fine; the fixture is about
		// the big one, so make sure it really is impossible.
		if over := p.Oversized(rb); len(over) == 0 {
			t.Fatal("fixture: nothing is oversized at this budget")
		}
	}

	err := emptyBinError(7, p, rb, 1.052)
	if err == nil {
		t.Fatal("emptyBinError returned nil")
	}
	msg := err.Error()
	for _, want := range []string{"disc 7", "media/one-big-mkv", "measured ratio 1.052",
		"EXCLUDE_MASKS", "DISC_TYPE=bdxl100"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "PACK_RATIO") {
		t.Errorf("the refusal still sends the operator to PACK_RATIO, which cannot change "+
			"a budget derived from the measured ratio:\n%s", msg)
	}
}

func TestDirBytes(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, 10, 20, 30)
	got, err := dirBytes(context.Background(), dir)
	if err != nil {
		t.Fatalf("dirBytes: %v", err)
	}
	if got != 60 {
		t.Errorf("dirBytes = %d, want 60", got)
	}
}

func TestLinkOrCopyFallsBackToACopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "sub", "dst")

	// A hard link works within one directory tree; the copy path is exercised
	// by copyFile directly.
	if err := linkOrCopy(context.Background(), src, dst, 0o644); err != nil {
		t.Fatalf("linkOrCopy: %v", err)
	}
	if err := linkOrCopy(context.Background(), src, dst, 0o644); err != nil {
		t.Fatalf("linkOrCopy over an existing file: %v", err)
	}
	copied := filepath.Join(dir, "copied")
	if err := copyFile(context.Background(), src, copied, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	for _, p := range []string{dst, copied} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "payload" {
			t.Errorf("%s = %q, want \"payload\"", p, data)
		}
	}
	fi, err := os.Stat(copied)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("copied mode = %v, want 0755", fi.Mode().Perm())
	}
}

func TestFilesMatching(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"disc01.squashfs.age", "disc01.squashfs.age.par2",
		"disc01.squashfs.age.vol000+100.par2", "disc01.squashfs.sha512"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := filesMatching(dir, func(n string) bool {
		return strings.HasPrefix(n, "disc01.squashfs.age") && strings.HasSuffix(n, ".par2")
	})
	if err != nil {
		t.Fatalf("filesMatching: %v", err)
	}
	want := []string{"disc01.squashfs.age.par2", "disc01.squashfs.age.vol000+100.par2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	missing, err := filesMatching(filepath.Join(dir, "nope"), func(string) bool { return true })
	if err != nil {
		t.Errorf("filesMatching of a missing directory: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("filesMatching of a missing directory returned %v", missing)
	}
}

// TestSidecarNames pins down what sidecars.par2 covers, which is the part of
// this feature a reader is most likely to get wrong: every .sha512 sidecar and
// the encrypted index itself, in that order, and nothing else — not the
// image, not the image's own parity, and not a previous run's sidecars.par2.
func TestSidecarNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"disc01.squashfs.age",
		"disc01.squashfs.age.par2",
		"disc01.squashfs.age.vol000+20.par2",
		"disc01.squashfs.age.sha512",
		"disc01.squashfs.sha512",
		indexName,
		indexName + ".sha512",
		"sidecars.par2",
		"sidecars.vol000+04.par2",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := sidecarNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"disc01.squashfs.age.sha512",
		"disc01.squashfs.sha512",
		indexName + ".sha512",
		indexName,
	}
	if len(got) != len(want) {
		t.Fatalf("sidecarNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sidecarNames = %v, want %v", got, want)
		}
	}
}

// TestSidecarNamesWithoutAnIndex covers the disc directory of a run that has
// not written the encrypted index yet: the hashes are still protected, and the
// absent index is not named to par2 as a file that does not exist.
func TestSidecarNamesWithoutAnIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disc01.squashfs.sha512"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sidecarNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "disc01.squashfs.sha512" {
		t.Fatalf("sidecarNames = %v, want just the one sidecar", got)
	}

	// And an empty directory yields nothing rather than an error, so the caller
	// can warn instead of running par2 with no files at all.
	empty, err := sidecarNames(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("sidecarNames of an empty directory = %v", empty)
	}
}

// TestPlanWarnsAboutAConfigurationBackupWouldRefuse: cfg.Validate ran only in
// preflight, so a plan was happily produced under settings backup refuses and
// the operator learned about them at the start of the overnight run instead.
// Plan is a diagnostic, so it warns and keeps planning, as it already does for
// the reserve check.
func TestPlanWarnsAboutAConfigurationBackupWouldRefuse(t *testing.T) {
	t.Parallel()
	src := writeTree(t, 300000, 300000)
	cfg := planCfg(t, src, 1<<20)
	cfg.Compression = "notreal"
	cfg.MaxShrinkAttempts = -5

	var buf bytes.Buffer
	p := ui.New(&buf, false)
	p.SetAssumeYes(true)
	got, err := Plan(context.Background(), Options{Cfg: cfg, UI: p})
	if err != nil {
		t.Fatalf("Plan refused a configuration it should only have warned about: %v", err)
	}
	if got.Discs == 0 {
		t.Fatal("Plan produced no layout")
	}
	out := buf.String()
	for _, want := range []string{"COMPRESSION", "MAX_SHRINK_ATTEMPTS", "backup will refuse this configuration"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not mention %q:\n%s", want, out)
		}
	}

	// And a configuration Validate accepts is not accused of anything by it.
	// (The reserve check next to it may still speak; it is a separate warning
	// with its own test, and RESERVE_BYTES=0 here trips it deliberately.)
	clean := planCfg(t, src, 1<<20)
	var quiet bytes.Buffer
	q := ui.New(&quiet, false)
	q.SetAssumeYes(true)
	if _, err := Plan(context.Background(), Options{Cfg: clean, UI: q}); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, unwanted := range []string{"COMPRESSION", "MAX_SHRINK_ATTEMPTS"} {
		if strings.Contains(quiet.String(), unwanted) {
			t.Errorf("a valid configuration was accused of %s:\n%s", unwanted, quiet.String())
		}
	}
}

// TestCheckIndexCoversRefusesFilesTheIndexCannotList pins the resume refusal
// for a set whose encrypted index is already built: the plaintext index is
// gone, the encrypted one is kept as it is and copied onto every disc, so any
// file still to be written would land on a disc the index knows nothing about.
func TestCheckIndexCoversRefusesFilesTheIndexCannotList(t *testing.T) {
	t.Parallel()
	skeleton := []scan.Entry{{Rel: "sub", Kind: scan.KindDir}, {Rel: "sub/link", Kind: scan.KindSymlink}}
	files := []scan.Entry{{Rel: "sub/new1", Kind: scan.KindFile, Size: 5}, {Rel: "sub/new2", Kind: scan.KindFile, Size: 5}}

	r := &runner{p: quiet(), indexBuilt: true, st: &State{Version: StateVersion, DiscsDone: 3}}
	// Nothing but skeleton left: every file is on a disc, and the index covers
	// them all, so the resume may go on to lay the discs out.
	if err := r.checkIndexCovers(skeleton); err != nil {
		t.Fatalf("checkIndexCovers with no files pending: %v", err)
	}
	err := r.checkIndexCovers(append(skeleton, files...))
	if err == nil {
		t.Fatal("checkIndexCovers accepted 2 files the built index cannot list")
	}
	for _, w := range []string{"3 finished disc(s)", "2 file(s) are not on any disc", "cannot be extended", "start over"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	// A resume whose index is not built yet appends to the plaintext one, so
	// pending files are the normal case there.
	r.indexBuilt = false
	if err := r.checkIndexCovers(files); err != nil {
		t.Errorf("checkIndexCovers with the plaintext index still present: %v", err)
	}
}

// TestProbeRecipientsRefusesAMixedSet: age refuses to encrypt to a recipient
// set that mixes post-quantum and classic keys, and neither AppendRecipient
// nor ParseRecipientsFile objects to such a file — so without the probe the
// refusal arrived after disc 1's mksquashfs. loadRecipients must catch it in
// preflight, and must accept an ordinary set.
func TestProbeRecipientsRefusesAMixedSet(t *testing.T) {
	t.Parallel()
	classic, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pq, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.ProbeRecipients([]age.Recipient{classic.Recipient()}); err != nil {
		t.Fatalf("ProbeRecipients over one classic key: %v", err)
	}
	if err := agecrypt.ProbeRecipients([]age.Recipient{pq.Recipient()}); err != nil {
		t.Fatalf("ProbeRecipients over one post-quantum key: %v", err)
	}
	if err := agecrypt.ProbeRecipients([]age.Recipient{classic.Recipient(), pq.Recipient()}); err == nil {
		t.Fatal("ProbeRecipients accepted a set mixing post-quantum and classic keys; age's Encrypt will not")
	}

	// Through loadRecipients, from a file, the way preflight sees it.
	write := func(name string, keys ...string) *runner {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.AgeRecipientsFile = path
		return &runner{cfg: cfg, p: quiet()}
	}
	r := write("mixed.txt", classic.Recipient().String(), pq.Recipient().String())
	err = r.loadRecipients()
	if err == nil {
		t.Fatal("loadRecipients accepted a recipients file mixing post-quantum and classic keys")
	}
	for _, w := range []string{"post-quantum", "mixed.txt", "one kind only"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	if len(r.recipients) != 0 {
		t.Error("loadRecipients kept the recipients of a set it refused")
	}
	ok := write("plain.txt", classic.Recipient().String())
	if err := ok.loadRecipients(); err != nil {
		t.Fatalf("loadRecipients over an ordinary file: %v", err)
	}
	if len(ok.recipients) != 1 || len(ok.pubkeys) != 1 {
		t.Errorf("loadRecipients kept %d recipient(s) and %d key(s), want 1 and 1", len(ok.recipients), len(ok.pubkeys))
	}
}

// TestLoadRecipientsRecordsTheKeysAsWritten: the keys the manifest and the
// resume pin record are now re-encoded from the parsed recipients rather than
// copied from the file, and the manifest's job is to show a restorer a key
// they can compare with their own. So the re-encoding has to give back each
// key line exactly, around the comments, blank lines, CRLF endings and missing
// final newline a hand-edited file has, and for post-quantum keys too.
func TestLoadRecipientsRecordsTheKeysAsWritten(t *testing.T) {
	t.Parallel()
	c1, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pq, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	k1, k2, kpq := c1.Recipient().String(), c2.Recipient().String(), pq.Recipient().String()
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"classic, hand-edited", "# created by hand\r\n\r\n" + k1 + "\r\n# " + k2 + "\n\n" + k2, []string{k1, k2}},
		{"post-quantum", "# pq\n" + kpq + "\r\n", []string{kpq}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "recipients.txt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.AgeRecipientsFile = path
			r := &runner{cfg: cfg, p: quiet()}
			if err := r.loadRecipients(); err != nil {
				t.Fatalf("loadRecipients: %v", err)
			}
			if !slices.Equal(r.pubkeys, tc.want) {
				t.Errorf("recorded keys %q, want the file's key lines %q", r.pubkeys, tc.want)
			}
			if len(r.recipients) != len(r.pubkeys) {
				t.Errorf("%d recipient(s) to encrypt to, %d key(s) recorded", len(r.recipients), len(r.pubkeys))
			}
		})
	}
}

// TestLoadRecipientsRefusesMoreThanAgeCanDecrypt: age writes a file to any
// number of recipients and refuses to read one encrypted to more than
// agecrypt.MaxRecipients, so a recipients file one key too long used to make
// a backup that exited 0 and that no current reader could open. backup used to
// keep its own copy of the recipient probe, which a check added to agecrypt's
// would have missed, so this pins it from here, through the file preflight
// reads — and without the advice about mixing key types that age's own
// refusals get, which would send the operator after a problem the file does
// not have.
func TestLoadRecipientsRefusesMoreThanAgeCanDecrypt(t *testing.T) {
	t.Parallel()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	loadAs := func(n int, opts Options) (*runner, error) {
		path := filepath.Join(t.TempDir(), "recipients.txt")
		line := id.Recipient().String() + "\n"
		if err := os.WriteFile(path, []byte(strings.Repeat(line, n)), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.AgeRecipientsFile = path
		r := &runner{opts: opts, cfg: cfg, p: quiet()}
		return r, r.loadRecipients()
	}
	load := func(n int) (*runner, error) { return loadAs(n, Options{}) }

	if r, err := load(agecrypt.MaxRecipients); err != nil {
		t.Fatalf("loadRecipients over %d keys, as many as age reads: %v", agecrypt.MaxRecipients, err)
	} else if len(r.recipients) != agecrypt.MaxRecipients {
		t.Errorf("loadRecipients kept %d recipient(s), want %d", len(r.recipients), agecrypt.MaxRecipients)
	}

	// A resume cannot drop keys — checkRecipients holds it to the ones its
	// discs were encrypted to — so it is not told to.
	if _, err := loadAs(agecrypt.MaxRecipients+1, Options{Resume: true}); err == nil ||
		!strings.Contains(err.Error(), "start it over") || strings.Contains(err.Error(), "re-run") {
		t.Errorf("loadRecipients under --resume over %d keys = %v, want a refusal that says to start "+
			"the set over rather than to remove keys and re-run", agecrypt.MaxRecipients+1, err)
	}

	r, err := load(agecrypt.MaxRecipients + 1)
	if err == nil {
		t.Fatalf("loadRecipients accepted %d keys; age cannot decrypt a file encrypted to that many",
			agecrypt.MaxRecipients+1)
	}
	if !errors.Is(err, agecrypt.ErrTooManyRecipients) {
		t.Errorf("the refusal does not wrap agecrypt.ErrTooManyRecipients: %v", err)
	}
	if strings.Contains(err.Error(), "post-quantum") {
		t.Errorf("the refusal carries the key-mixing advice, which is not the problem: %v", err)
	}
	if !strings.Contains(err.Error(), "recipients.txt") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	if len(r.recipients) != 0 {
		t.Error("loadRecipients kept the recipients of a set it refused")
	}
}

// resumeRunner builds a runner around a saved state file, ready for
// prepareState with --resume: the state says one disc is done, and the
// plaintext index in work/ says the same, so the only thing that can refuse
// is the check under test.
func resumeRunner(t *testing.T, mutate func(*State)) *runner {
	t.Helper()
	cfg := config.Default()
	cfg.Staging = t.TempDir()
	cfg.SourceDir = t.TempDir()
	cfg.ArchiveName = "pinned"
	r := &runner{
		opts:      Options{Resume: true},
		cfg:       cfg,
		p:         quiet(),
		dirs:      cfg.Dirs(),
		statePath: filepath.Join(cfg.Staging, "state.json"),
		pubkeys:   []string{"age1recorded"},
	}
	for _, d := range []string{r.dirs.Work, r.dirs.Enc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st := newState(cfg.ArchiveName, cfg.SourceDir, 1.0, time.Now())
	st.DiscsDone, st.Assigned = 1, []string{"a"}
	st.setGeometry(r.geometry())
	st.setRecipients(r.pubkeys)
	mutate(st)
	if err := SaveState(r.statePath, st); err != nil {
		t.Fatal(err)
	}
	if err := appendIndex(filepath.Join(r.dirs.Work, indexFileName), 1, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestPrepareStateRefusesAResumeUnderChangedKeysOrGeometry is the wiring
// test for the two pins: prepareState must consult them, with --resume, once
// discs exist, and the state a fresh run starts from must carry them.
func TestPrepareStateRefusesAResumeUnderChangedKeysOrGeometry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	same := resumeRunner(t, func(*State) {})
	if err := same.prepareState(ctx); err != nil {
		t.Fatalf("prepareState with unchanged keys and geometry: %v", err)
	}

	keys := resumeRunner(t, func(s *State) { s.setRecipients([]string{"age1recorded", "age1addedlater"}) })
	err := keys.prepareState(ctx)
	if !errors.Is(err, ErrStateMismatch) || !strings.Contains(err.Error(), "age1addedlater") {
		t.Errorf("prepareState under a changed recipients file = %v, want ErrStateMismatch naming the keys", err)
	}

	geom := resumeRunner(t, func(s *State) { s.Par2Redundancy++ })
	err = geom.prepareState(ctx)
	if !errors.Is(err, ErrStateMismatch) || !strings.Contains(err.Error(), "PAR2_REDUNDANCY") {
		t.Errorf("prepareState under a changed PAR2_REDUNDANCY = %v, want ErrStateMismatch naming it", err)
	}

	// A fresh start records both, so the next resume has something to check.
	fresh := resumeRunner(t, func(*State) {})
	fresh.opts.Resume = false
	if err := os.Remove(fresh.statePath); err != nil {
		t.Fatal(err)
	}
	if err := fresh.prepareState(ctx); err != nil {
		t.Fatalf("prepareState for a fresh run: %v", err)
	}
	if got := fresh.st.Recipients; len(got) != 1 || got[0] != "age1recorded" {
		t.Errorf("a fresh state recorded recipients %v, want [age1recorded]", got)
	}
	if fresh.st.CapacityBytes != fresh.cfg.Capacity() || fresh.st.DiscType != "bd25" ||
		fresh.st.ReserveBytes != fresh.cfg.ReserveBytes || fresh.st.Par2Redundancy != fresh.cfg.Par2Redundancy {
		t.Errorf("a fresh state recorded geometry %+v, want the configuration's", fresh.st)
	}
}

// TestManifestPrunesCarriesTheSkippedMounts: the manifest's "excluded from
// this backup" list is the one record of a skipped mount point that outlives
// the terminal, so the mount points the scan reported must land there,
// marked as such, after the configured prunes.
func TestManifestPrunesCarriesTheSkippedMounts(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.PruneDirs = []string{".cache"}
	r := &runner{cfg: cfg}
	if got := r.manifestPrunes(); len(got) != 1 || got[0] != ".cache" {
		t.Fatalf("manifestPrunes without mounts = %v, want [.cache]", got)
	}
	r.skippedMounts = []string{"nas", "media/usb"}
	got := r.manifestPrunes()
	if len(got) != 3 || got[0] != ".cache" {
		t.Fatalf("manifestPrunes = %v, want the prune first and both mounts after it", got)
	}
	for i, m := range r.skippedMounts {
		if !strings.HasPrefix(got[i+1], m+" ") || !strings.Contains(got[i+1], "mount point") {
			t.Errorf("manifestPrunes[%d] = %q, want %q marked as a mount point", i+1, got[i+1], m)
		}
	}
	if len(cfg.PruneDirs) != 1 {
		t.Error("manifestPrunes appended to the configuration's own slice")
	}
}

// TestOversizedDiscAdviceIsTrue: the old message said "raise RESERVE_BYTES and
// re-run", which a --resume cannot act on — the images already built keep
// their size, and Usable ignores the reserve. The advice must say by how much,
// and that the set has to start over.
func TestOversizedDiscAdviceIsTrue(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.ReserveBytes = 1000
	r := &runner{cfg: cfg, budget: disc.Budget{Usable: 10000}}
	err := r.oversizedDisc(4, "files", 10250)
	for _, w := range []string{"disc 4", "RESERVE_BYTES=1000", "at least 1250", "start the set over", "--resume cannot"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	if strings.Contains(err.Error(), "and re-run") {
		t.Errorf("error %q still gives the advice a resume cannot act on", err)
	}
}

// TestFilesMatchingInADirectoryNamedLikeAGlob pins the sweep helper the
// stale-par2 removals in protect and protectSidecars rely on: it lists the
// directory and matches names, so a STAGING path holding '[' — here literally
// "a[1]" — still finds the set that filepath.Glob would have missed.
func TestFilesMatchingInADirectoryNamedLikeAGlob(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a[1]")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"disc01.squashfs.age", "disc01.squashfs.age.par2",
		"disc01.squashfs.age.vol000+30.par2", "sidecars.par2", "sidecars.vol00+10.par2"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := "disc01.squashfs"
	got, err := filesMatching(dir, func(nm string) bool {
		return strings.HasPrefix(nm, base+".age") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil || len(got) != 2 {
		t.Errorf("image par2 set in a glob-named directory = %v, %v; want 2 names", got, err)
	}
	got, err = filesMatching(dir, func(nm string) bool {
		return strings.HasPrefix(nm, "sidecars") && strings.HasSuffix(nm, ".par2")
	})
	if err != nil || len(got) != 2 {
		t.Errorf("sidecar par2 set in a glob-named directory = %v, %v; want 2 names", got, err)
	}
	// And the thing this replaced really does fail there.
	if m, _ := filepath.Glob(filepath.Join(dir, "sidecars*.par2")); len(m) != 0 {
		t.Logf("filepath.Glob matched %v in a directory named like a glob; the helper is belt and braces here", m)
	}
}

// TestResumeFilterWarnsWhenANewLinkJoinsWrittenData pins the report for the
// case pack cannot fix: a hard link added to an already-written inode between
// an interrupted run and its resume.
//
// The new name cannot join its group, because the disc holding the group is
// finished, so pack charges it a second full copy on a later disc and the
// restored tree gets two independent files where the source had one inode. It
// was silent: RawBytes counts a group once per inode, so the tree-size warning
// cannot fire for a new link, and the vanished-names warning counts only names
// that went away.
func TestResumeFilterWarnsWhenANewLinkJoinsWrittenData(t *testing.T) {
	res := &scan.Result{
		RawBytes: 300,
		Entries: []scan.Entry{
			// One inode, three names. a and b were written to disc 1; c is new.
			{Rel: "a.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 42, Nlink: 3},
			{Rel: "b.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 42, Nlink: 3},
			{Rel: "c.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 42, Nlink: 3},
			{Rel: "plain.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 99, Nlink: 1},
		},
	}
	p, log := recordingPrinter()
	r := &runner{p: p, st: &State{
		Version: StateVersion, DiscsDone: 1, ScanRawSize: 300,
		Assigned: []string{"a.bin", "b.bin"},
	}}

	out, err := r.resumeFilter(res)
	if err != nil {
		t.Fatalf("resumeFilter refused a resume it should only warn about: %v", err)
	}
	// The new link is still packed: warning, not refusal, and no byte is dropped.
	var kept []string
	for _, e := range out {
		kept = append(kept, e.Rel)
	}
	if len(kept) != 2 || kept[0] != "c.bin" || kept[1] != "plain.bin" {
		t.Fatalf("kept %v, want [c.bin plain.bin]", kept)
	}
	got := log()
	if !strings.Contains(got, "c.bin") {
		t.Errorf("the warning does not name the new link:\n%s", got)
	}
	// It has to say what c.bin duplicates, because the repair is to remove that
	// link and resume again, and nothing else in the run points at the pair.
	if !strings.Contains(got, "a.bin") && !strings.Contains(got, "b.bin") {
		t.Errorf("the warning does not name what the link duplicates:\n%s", got)
	}
	if strings.Contains(got, "plain.bin") {
		t.Errorf("an ordinary new file was reported as a relink:\n%s", got)
	}
}

// TestResumeFilterIsQuietAboutAnUnchangedLinkGroup is the companion that stops
// the check above from firing on every hard link it sees. A group wholly
// outstanding, or wholly assigned, is the ordinary case and says nothing.
func TestResumeFilterIsQuietAboutAnUnchangedLinkGroup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assigned []string
	}{
		{"group wholly outstanding", []string{"other.bin"}},
		{"group wholly assigned", []string{"a.bin", "b.bin", "other.bin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &scan.Result{
				RawBytes: 300,
				Entries: []scan.Entry{
					{Rel: "a.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 42, Nlink: 2},
					{Rel: "b.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 42, Nlink: 2},
					{Rel: "other.bin", Kind: scan.KindFile, Size: 100, Dev: 7, Inode: 99, Nlink: 1},
				},
			}
			p, log := recordingPrinter()
			r := &runner{p: p, st: &State{
				Version: StateVersion, DiscsDone: 1, ScanRawSize: 300,
				Assigned: tc.assigned,
			}}
			if _, err := r.resumeFilter(res); err != nil {
				t.Fatalf("resumeFilter: %v", err)
			}
			if got := log(); strings.Contains(got, "hard link") {
				t.Errorf("warned about an unchanged link group:\n%s", got)
			}
		})
	}
}

// TestWarnIfImageMayNotFit pins the notice that stands between an adaptive pack
// ratio and mksquashfs running the filesystem out mid-image.
//
// preflight sizes its refusal from the IMAGE budget, which is the right floor
// for content that compresses the way the ratio predicts and is not a bound on
// the plaintext mksquashfs writes. With adaptation on by default, one disc of
// compressible content can teach a ratio of 0.088, the raw budget becomes
// eleven times the image budget, and a following disc of incompressible content
// grows the image toward that raw size — past a free-space figure preflight
// called sufficient.
//
// It warns rather than refusing: requiring avail >= RawBytes of every disc
// refuses runs that complete today, because mksquashfs compresses as it writes.
func TestWarnIfImageMayNotFit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rawBytes int64
		wantWarn bool
	}{
		// t.TempDir is on a filesystem with far more than a kilobyte free, so
		// this is the ordinary case and must stay silent.
		{"a bin that comfortably fits", 1024, false},
		// Larger than any filesystem this can run on, so the check must fire.
		{"a bin larger than the filesystem", 1 << 62, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, log := recordingPrinter()
			r := &runner{p: p, cfg: &config.Config{Staging: t.TempDir()}, packRatio: 0.088}
			r.warnIfImageMayNotFit(&pack.Bin{RawBytes: tc.rawBytes}, 2)
			got := log()
			if tc.wantWarn && !strings.Contains(got, "disc 2 packs") {
				t.Errorf("no warning for a bin of %d bytes:\n%s", tc.rawBytes, got)
			}
			if !tc.wantWarn && strings.Contains(got, "disc 2 packs") {
				t.Errorf("warned about a bin that fits:\n%s", got)
			}
		})
	}
}

// TestDiagnosticsQuoteSourcePaths. The Printer escapes ESC and C1 in every
// message but keeps a newline, because brb's own messages span lines — so a
// path from the source tree pasted raw into one of them could end a line and
// start another that reads exactly like brb's. Anyone who can create a file in
// the tree being backed up names it, and with colour off "  ok backup
// complete" under a refusal is indistinguishable from the real thing.
// Every producer that puts a source path into a multi-line message is held to
// it here: the oversized-file refusal, the resume's relinked-name warning (both
// of its names), its changed-kind refusal, and the unreadable-path warning.
func TestDiagnosticsQuoteSourcePaths(t *testing.T) {
	t.Parallel()
	const forged = "big\n  ok no files were skipped\x1b[2J"
	var buf bytes.Buffer
	p := ui.New(&buf, false)

	p.Fail("%v", oversizedError([]pack.Unit{{Size: 1 << 40, Paths: []string{forged}}}, 1<<30))
	p.Warn("%s", relinkedDetail([]scan.Entry{{Rel: forged, Size: 5}}, map[string]string{forged: forged + "-old"}))
	p.Fail("%v", changedKindError([]kindChange{{rel: forged, was: scan.KindFile, now: scan.KindDir}}, 1))
	p.Step("%s", problemLine(scan.Problem{
		Path: "/src/" + forged,
		Err:  &fs.PathError{Op: "open", Path: "/src/" + forged, Err: fs.ErrPermission},
	}))

	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  ok") {
			t.Errorf("a file name forged a line of its own:\n%s", out)
			break
		}
	}
	// One escaped spelling per name printed: oversized, relinked (twice),
	// changed-kind, unreadable.
	if n := strings.Count(out, `\n  ok no files were skipped\x1b[2J`); n != 5 {
		t.Errorf("found %d escaped spelling(s) of the name, want 5:\n%s", n, out)
	}
	// The unreadable-path line keeps what failed, and names the path once.
	if !strings.Contains(out, `": open: permission denied`) || strings.Count(out, "/src/") != 1 {
		t.Errorf("the unreadable-path line should name the path once and say what failed:\n%s", out)
	}
}
