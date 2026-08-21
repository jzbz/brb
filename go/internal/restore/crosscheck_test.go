package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/indexfmt"
)

// The tests in this file cover RVW-008 as a pair, because the two halves are
// only worth anything together.
//
// age encrypts to a PUBLIC key, and MANIFEST.txt prints the recipients on every
// disc, so anyone who gets hold of one disc can master another that decrypts,
// verifies against its own SHA512SUMS, passes par2 and extracts. What they
// cannot do is READ the set's index, because that needs the private key.
//
//   - Pinning the index at ingest (reconcileIndex) forces such a disc to carry
//     the genuine index — copying ciphertext needs no key — or be refused for
//     carrying a different one.
//   - Cross-checking each image against the index at restore (auditImage) then
//     catches it, because it cannot make an image agree with an index it cannot
//     read.
//
// Neither half alone stops the attack, so each has a test here, and the third
// test pins the degradation that keeps the second from refusing legitimate
// sets whose filenames a line-based listing cannot carry.

// discWithIndex lays out one disc of a set the way a burned disc reads —
// data/ holding the numbered image, its two hash sidecars and the set's shared
// index, plus the disc's own SHA512SUMS over all of it. Unlike fakeDisc it
// needs no par2, and the index body is the caller's, which is the whole point.
func discWithIndex(t *testing.T, n int, index []byte) string {
	t.Helper()
	mp := t.TempDir()
	data := filepath.Join(mp, dataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("disc%02d.squashfs", n)
	write := func(name string, body []byte) {
		if err := os.WriteFile(filepath.Join(data, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(base+ageExt, []byte(strings.Repeat(fmt.Sprintf("image %d\n", n), 40)))
	write(indexName, index)
	digest := strings.Repeat("ab", 64)
	for _, nm := range []string{base + ageExt, base, indexName} {
		if err := agecrypt.WriteSumFile(filepath.Join(data, nm+sumExt), digest, nm); err != nil {
			t.Fatal(err)
		}
	}
	if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
		t.Fatal(err)
	}
	return mp
}

// TestIngestPinsTheIndexOfTheFirstDisc is the ingest half of RVW-008.
//
// Every disc of one set carries the identical index file, because the writer
// copies one file onto all of them. A second disc whose index differs is
// therefore either from another set or not from any set the operator burned,
// and it is refused rather than warned about — which is what makes an attacker
// carry the genuine index onto their forged disc, where the restore-side
// cross-check can catch them with it.
//
// Reverting the guard (dropping the isIndexName dispatch at the top of
// reconcileExisting) makes the second disc's index either be kept with a
// warning or, worse, silently replace the staged one under the disc's own
// recorded hash — both of which fail here.
func TestIngestPinsTheIndexOfTheFirstDisc(t *testing.T) {
	real := []byte("the set's real index, encrypted\n")

	t.Run("a second disc of the same set is ordinary", func(t *testing.T) {
		e := newEnv(t)
		ingestOne(t, e, discWithIndex(t, 1, real))
		ingestOne(t, e, discWithIndex(t, 2, real))
		if !strings.Contains(e.log(), "this disc belongs to the staged set") {
			t.Errorf("the second disc's identical index was not recognised:\n%s", e.log())
		}
	})

	t.Run("a disc carrying a different index is refused", func(t *testing.T) {
		e := newEnv(t)
		ingestOne(t, e, discWithIndex(t, 1, real))

		forged := []byte("an index the operator never wrote!\n")
		_, err := (&ingester{o: e.opts(), mountPoint: discWithIndex(t, 2, forged)}).ingestDisc(context.Background())
		if err == nil {
			t.Fatalf("the disc was ingested; want a refusal\n%s", e.log())
		}
		if !strings.Contains(err.Error(), "differs from the one already staged") {
			t.Fatalf("error = %v\nwant a refusal saying the index differs from the staged one", err)
		}
		if errors.Is(err, ErrIncompleteCopy) {
			t.Errorf("a foreign index was reported as a damaged copy: %v", err)
		}
		// The pinned index is the whole point: it must still be the one the
		// first disc brought, or the forged map of the set has won.
		body, rerr := os.ReadFile(filepath.Join(e.cfg.Dirs().Enc, indexName))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(body) != string(real) {
			t.Fatalf("the staged index is now %q; the disc's copy replaced the pinned one", body)
		}
	})

	t.Run("a rotted copy of the right index is damage, not another set", func(t *testing.T) {
		e := newEnv(t)
		ingestOne(t, e, discWithIndex(t, 1, real))

		// This disc's SHA512SUMS still records the set's real index, so the
		// disc vouches for what is already staged and has merely lost its own
		// copy. Refusing it would fail an honest disc for nothing; a forged
		// disc cannot get here, because its sums file agrees with its index.
		mp := discWithIndex(t, 2, real)
		rotted := append([]byte(nil), real...)
		rotted[0] ^= 0xff
		if err := os.WriteFile(filepath.Join(mp, dataDir, indexName), rotted, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := (&ingester{o: e.opts(), mountPoint: mp}).ingestDisc(context.Background()); err != nil {
			t.Fatalf("ingestDisc = %v, want the disc to be read despite its rotted index\n%s", err, e.log())
		}
		log := e.log()
		if !strings.Contains(log, "this disc's copy is damaged") {
			t.Fatalf("a rotted index was not reported as damage:\n%s", log)
		}
		if strings.Contains(log, "differs from the one already staged") {
			t.Fatalf("a rotted index was reported as a second set:\n%s", log)
		}
		body, rerr := os.ReadFile(filepath.Join(e.cfg.Dirs().Enc, indexName))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(body) != string(real) {
			t.Fatalf("the staged index was replaced by the rotted copy: %q", body)
		}
	})
}

// ingestOne reads one prepared disc into the staging area and fails the test if
// it does not go in cleanly.
func ingestOne(t *testing.T, e *env, mountPoint string) {
	t.Helper()
	if _, err := (&ingester{o: e.opts(), mountPoint: mountPoint}).ingestDisc(context.Background()); err != nil {
		t.Fatalf("ingesting %s: %v\n%s", mountPoint, err, e.log())
	}
}

// forgeImage replaces disc n's staged image with one holding files of the
// caller's choosing, encrypted to the same recipients and with both recorded
// hashes rewritten to match — everything an attacker holding one disc of the
// set and the public key from its MANIFEST.txt can produce.
func (e *env) forgeImage(n int, files map[string]string) {
	e.t.Helper()
	if err := os.RemoveAll(filepath.Join(e.dir, fmt.Sprintf("src%02d", n))); err != nil {
		e.t.Fatal(err)
	}
	e.makeDisc(n, files)
}

// twoDiscSetWithIndex stages a genuine two-disc set and the encrypted index
// that describes it.
func twoDiscSetWithIndex(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"keep.txt": "one\n"})
	e.makeDisc(2, map[string]string{"lib/two.txt": "two\n", "lib/three.txt": "three\n"})
	e.writeManifest(manifestSaying(2))
	e.writeIndex(strings.Join([]string{
		indexfmt.FormatLine(1, "keep.txt"),
		indexfmt.FormatLine(2, "lib/three.txt"),
		indexfmt.FormatLine(2, "lib/two.txt"),
	}, "\n") + "\n")
	e.ui.SetAssumeYes(true)
	return e
}

// TestRestoreRefusesAnImageThatDisagreesWithTheIndex is the restore half of
// RVW-008: the forged disc that decrypts, verifies and par2s clean, and that
// nothing on the read path used to question.
//
// The genuine set restores first, so this fails just as loudly if the guard
// starts refusing honest discs. Then disc 2's image is replaced by one built
// from a different tree — which is all an attacker with the set's public
// recipient key needs to do — while the pinned index still says what disc 2
// holds. The image and the index no longer agree, and the disc is refused
// before unsquashfs -f writes a byte of the attacker's tree into $HOME.
//
// Reverting the guard (auditImage returning before the comparison, or the
// listing's file entries being ignored) lets "evil.txt" land in the
// destination, which is what the last assertion is watching for.
func TestRestoreRefusesAnImageThatDisagreesWithTheIndex(t *testing.T) {
	e := twoDiscSetWithIndex(t)
	dest := filepath.Join(e.dir, "dest")

	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("the genuine set did not restore: %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "matches the index: 2 file(s), exactly the ones it puts on disc 2") {
		t.Errorf("the cross-check did not report on the genuine disc 2:\n%s", e.log())
	}

	e.forgeImage(2, map[string]string{"evil.txt": "run me\n", "lib/two.txt": "two\n"})

	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest})
	if err == nil {
		t.Fatalf("the forged disc was extracted\n%s", e.log())
	}
	if !strings.Contains(err.Error(), "does not match the index") {
		t.Fatalf("error = %v\nwant the index cross-check refusal", err)
	}
	for _, want := range []string{`"evil.txt"`, `"lib/three.txt"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "evil.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the forged image's file reached the destination: %v", err)
	}
}

// --only narrows what is extracted, never what is compared: the image still
// holds the whole disc, so a forged image is refused even when the one file
// asked for is in both the image and the index.
func TestRestoreOnlyStillChecksTheWholeImageAgainstTheIndex(t *testing.T) {
	e := twoDiscSetWithIndex(t)
	e.forgeImage(2, map[string]string{"evil.txt": "run me\n", "lib/two.txt": "two\n"})

	dest := filepath.Join(e.dir, "dest")
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest, Only: []string{"lib/two.txt"}})
	if err == nil || !strings.Contains(err.Error(), "does not match the index") {
		t.Fatalf("Restore --only from a forged image = %v, want the cross-check refusal\n%s", err, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest, "lib", "two.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the forged image was extracted anyway: %v", err)
	}
}

// TestRestoreDegradesTheCrossCheckForANameHoldingANewline is the false-positive
// case, and it matters more than the attack: brb deliberately supports a
// newline in a filename — it is why the index has an escaping contract at all —
// but "unsquashfs -ll" is line-based and cannot hand such a name back whole.
// Comparing anyway would refuse a legitimate disc every time, so the guard
// stands down for that disc, says so in as many words, and restores.
//
// Reverting the degradation (dropping the newline case from indexRowsForDisc)
// turns this set into an unrestorable one, which is what the Restore error
// check below catches.
func TestRestoreDegradesTheCrossCheckForANameHoldingANewline(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	odd := "notes/two\nlines.txt"
	e.makeDisc(1, map[string]string{odd: "awkward\n", "plain.txt": "ordinary\n"})
	e.writeManifest(manifestSaying(1))
	e.writeIndex(strings.Join([]string{
		indexfmt.FormatLine(1, odd),
		indexfmt.FormatLine(1, "plain.txt"),
	}, "\n") + "\n")
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("a legitimate set was refused over a filename brb supports: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(odd))); err != nil {
		t.Fatalf("the awkward name was not restored: %v\n%s", err, e.log())
	}
	log := e.log()
	if !strings.Contains(log, "was NOT checked against the index") {
		t.Fatalf("the run did not say the cross-check had been given up on:\n%s", log)
	}
	if !strings.Contains(log, "newline") || !strings.Contains(log, "would not be caught") {
		t.Fatalf("the warning does not say why, or what it costs:\n%s", log)
	}
}

// A set with no index at all — one written before there was one, or a single
// disc ingested on its own — still restores, and is told plainly that nothing
// vouched for the disc.
func TestRestoreWithoutAnIndexSaysTheImageWasNotChecked(t *testing.T) {
	e := newEnv(t)
	e.squashTools(t)
	e.makeDisc(1, map[string]string{"keep.txt": "one\n"})
	e.writeManifest(manifestSaying(1))
	e.ui.SetAssumeYes(true)

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "was NOT checked against the index") {
		t.Fatalf("an unvouched-for disc restored in silence:\n%s", e.log())
	}
}

// listedFile answers for regular files and for nothing else, because every
// other kind of entry is skeleton the writer replicates onto every disc and
// leaves out of the index. Getting this wrong refuses every legitimate
// restore, so it is pinned line by line.
func TestListedFile(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
		ok   bool
	}{
		{"-rw-r--r-- jz/jz                     3 2026-08-19 01:30 squashfs-root/sub/f.txt", "sub/f.txt", true},
		{"-rwxr-xr-x jz/jz                    12 2026-08-19 01:30 squashfs-root/run.sh", "run.sh", true},
		{"-rw-r--r-- jz/jz                     3 2026-08-19 01:30 squashfs-root/name with space", "name with space", true},
		{"-rw-r--r-- 1000/1000          123456789 2026-08-19 01:30 squashfs-root/wide", "wide", true},
		{"-rw-r--r-- jz/jz                     3 2026-08-19 01:30 squashfs-root/trailing\r", "trailing\r", true},
		{"drwxr-xr-x jz/jz                     3 2026-08-19 01:30 squashfs-root/emptydir", "", false},
		{"lrwxrwxrwx jz/jz                     5 2026-08-19 01:30 squashfs-root/sub/link -> f.txt", "", false},
		{"crw-rw-rw- root/root                1,3 2026-08-19 01:30 squashfs-root/dev/null", "", false},
		{"Parallel unsquashfs: Using 8 processors", "", false},
		{"", "", false},
		{"-rw-r--r--", "", false},
	} {
		got, ok := listedFile(tc.line)
		if got != tc.want || ok != tc.ok {
			t.Errorf("listedFile(%q) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}
