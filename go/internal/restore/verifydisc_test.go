package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/agecrypt"
)

// verifyDiscDir lays out the smallest directory VerifyDisc accepts as disc 1:
// the numbered image, a README beside it, and a SHA512SUMS the writer's own
// function made over both.
func verifyDiscDir(t *testing.T) string {
	t.Helper()
	mp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mp, dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, dataDir, encName(1)), []byte("ciphertext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "README.md"), []byte("# disc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
		t.Fatal(err)
	}
	return mp
}

// verifyWithin runs VerifyDisc with a deadline, so a regression that blocks in
// open(2) on a FIFO fails the test instead of hanging the suite: on timeout the
// FIFO is opened for writing, which releases the blocked open.
func verifyWithin(t *testing.T, o Options, mp, fifo string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- VerifyDisc(context.Background(), o, 1, mp) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		<-done
		t.Fatalf("VerifyDisc blocked opening the FIFO at %s", fifo)
		return nil
	}
}

// TestVerifyDiscRefusesANonRegularSums: a FIFO named SHA512SUMS passed the
// existence check and then blocked ReadSumFile's open until a writer came — a
// hang a cancelled context could not end, so the first Ctrl-C did nothing.
// brb.sh's [[ -f ]] refused the same disc at once. A link is refused whatever
// it points at, as brb.sh's -L refuses it: one to a regular file off the disc
// had that file's hashes vouch for the disc, and the disc was called verified.
func TestVerifyDiscRefusesANonRegularSums(t *testing.T) {
	e := newEnv(t)

	mp := verifyDiscDir(t)
	sums := filepath.Join(mp, agecrypt.SumsName)
	if err := os.Remove(sums); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(sums, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := verifyWithin(t, e.opts(), mp, sums); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("VerifyDisc over a FIFO SHA512SUMS = %v, want it refused as not a regular file", err)
	}

	mp = verifyDiscDir(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	sums = filepath.Join(mp, agecrypt.SumsName)
	if err := os.Remove(sums); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, sums); err != nil {
		t.Fatal(err)
	}
	if err := verifyWithin(t, e.opts(), mp, fifo); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("VerifyDisc over a SHA512SUMS linked to a FIFO = %v, want it refused as a link", err)
	}

	mp = verifyDiscDir(t)
	sums = filepath.Join(mp, agecrypt.SumsName)
	elsewhere := filepath.Join(t.TempDir(), "sums")
	if err := os.Rename(sums, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, sums); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDisc(context.Background(), e.opts(), 1, mp); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("VerifyDisc over a SHA512SUMS linked to a regular file off the disc = %v, want it refused as a link", err)
	}
}

// TestVerifyDiscStepsPastANonRegularManifest: the archive-name advice opened
// MANIFEST.txt with no type check, and config.Load always fills in an archive
// name, so a FIFO there hung every verify-disc. brb.sh's [[ -f ]] steps past it
// and verifies the disc; so must this.
func TestVerifyDiscStepsPastANonRegularManifest(t *testing.T) {
	e := newEnv(t)
	e.cfg.ArchiveName = "xcompat"
	mp := verifyDiscDir(t)
	manifest := filepath.Join(mp, manifestName)
	if err := syscall.Mkfifo(manifest, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := verifyWithin(t, e.opts(), mp, manifest); err != nil {
		t.Fatalf("VerifyDisc with a FIFO MANIFEST.txt: %v\n%s", err, e.log())
	}
}

// TestVerifyDiscRefusesASymlinkedDataDir: os.ReadDir follows a symlinked data/
// wherever it points, so the identity gate saw an image that is not on the disc
// and the disc verified, while brb.sh's `find -P` will not descend the link and
// calls the disc unrecognised. Both halves are covered: data pointing inside the
// disc with the image's real digest listed, and data pointing off it with a
// SHA512SUMS that does not mention the image at all.
func TestVerifyDiscRefusesASymlinkedDataDir(t *testing.T) {
	e := newEnv(t)
	for _, inside := range []bool{true, false} {
		mp := t.TempDir()
		real := filepath.Join(mp, "real")
		if !inside {
			real = filepath.Join(t.TempDir(), "real")
		}
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		img := filepath.Join(real, encName(1))
		if err := os.WriteFile(img, []byte("ciphertext\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(mp, dataDir)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mp, manifestName), []byte("brb\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		listed := "./" + manifestName
		file := filepath.Join(mp, manifestName)
		if inside {
			listed, file = "./"+dataDir+"/"+encName(1), img
		}
		digest, err := agecrypt.SumFile(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		if err := agecrypt.WriteSumFile(filepath.Join(mp, agecrypt.SumsName), digest, listed); err != nil {
			t.Fatal(err)
		}
		err = VerifyDisc(context.Background(), e.opts(), 1, mp)
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("inside=%v: VerifyDisc over a symlinked data/ = %v, want a refusal naming the link", inside, err)
		}
	}
}

// TestVerifyDiscRequiresTheImageLine: verify-disc checked only the names the
// SHA512SUMS listed, so a corrupt image beside a sums file that listed nothing,
// or listed only the README, was reported "disc 1 verified" without a byte of
// the image being read. The image is the one file the identity gate has already
// named, and every disc the writer produces lists it.
func TestVerifyDiscRequiresTheImageLine(t *testing.T) {
	e := newEnv(t)
	for name, body := range map[string]func(mp string) string{
		"empty":         func(string) string { return "" },
		"comments only": func(string) string { return "# nothing here\n\n" },
		"README only": func(mp string) string {
			d, err := agecrypt.SumFile(context.Background(), filepath.Join(mp, "README.md"))
			if err != nil {
				t.Fatal(err)
			}
			return d + "  ./README.md\n"
		},
	} {
		mp := verifyDiscDir(t)
		if err := os.WriteFile(filepath.Join(mp, dataDir, encName(1)), []byte("corrupt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mp, agecrypt.SumsName), []byte(body(mp)), 0o600); err != nil {
			t.Fatal(err)
		}
		err := VerifyDisc(context.Background(), e.opts(), 1, mp)
		if err == nil {
			t.Errorf("%s: VerifyDisc reported a disc with a corrupt, unlisted image as verified", name)
			continue
		}
		if errors.Is(err, ErrVerifyFailed) {
			t.Errorf("%s: VerifyDisc = %v; a sums file that cannot vouch for the image is not a hash mismatch", name, err)
		}
	}
}
