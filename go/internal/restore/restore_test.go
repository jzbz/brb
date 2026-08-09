package restore

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/tools"
)

// writeIndex builds the encrypted index a backup leaves in the staging area.
func (e *env) writeIndex(lines string) {
	e.t.Helper()
	raw := filepath.Join(e.dir, "index.tsv.gz")
	f, err := os.Create(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(lines)); err != nil {
		e.t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
	if _, err := agecrypt.Encrypt(context.Background(), raw,
		filepath.Join(e.cfg.Dirs().Enc, indexName), e.recipients, nil); err != nil {
		e.t.Fatal(err)
	}
}

func TestIndex(t *testing.T) {
	e := newEnv(t)
	e.writeIndex("1\tPhotos/2024/IMG_0001.JPG\n2\tsrc/main.go\n")

	var out bytes.Buffer
	if err := Index(context.Background(), e.opts(), "img_0001", &out); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if got := out.String(); got != "1\tPhotos/2024/IMG_0001.JPG\n" {
		t.Fatalf("Index wrote %q", got)
	}

	out.Reset()
	if err := Index(context.Background(), e.opts(), "", &out); err != nil {
		t.Fatalf("Index with no pattern: %v", err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 2 {
		t.Fatalf("Index wrote %d line(s), want 2: %q", lines, out.String())
	}

	out.Reset()
	err := Index(context.Background(), e.opts(), "nothing matches this", &out)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Index = %v, want ErrNoMatch", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a failed search wrote %q", out.String())
	}
}

func TestIndexWithoutAnIndex(t *testing.T) {
	e := newEnv(t)
	var out bytes.Buffer
	err := Index(context.Background(), e.opts(), "", &out)
	if err == nil || !strings.Contains(err.Error(), "run 'brb ingest' first") {
		t.Fatalf("Index = %v, want a message pointing at ingest", err)
	}
}

func TestVerifyDiscWithAnExplicitMountPoint(t *testing.T) {
	e := newEnv(t)
	mp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mp, dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(mp, dataDir, "disc01.squashfs.age")
	if err := os.WriteFile(payload, []byte("ciphertext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, "README.md"), []byte("# disc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
		t.Fatal(err)
	}

	if err := VerifyDisc(context.Background(), e.opts(), 1, mp); err != nil {
		t.Fatalf("VerifyDisc: %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "disc 1 verified") {
		t.Fatalf("log does not report success:\n%s", e.log())
	}

	// Now damage it.
	if err := os.WriteFile(payload, []byte("ciphertext?\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := VerifyDisc(context.Background(), e.opts(), 1, mp)
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("VerifyDisc = %v, want ErrVerifyFailed", err)
	}
	if !strings.Contains(e.log(), "disc01.squashfs.age") {
		t.Fatalf("the failing file is not named:\n%s", e.log())
	}
}

func TestVerifyDiscRejectsSomethingElsesDisc(t *testing.T) {
	e := newEnv(t)
	err := VerifyDisc(context.Background(), e.opts(), 1, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), agecrypt.SumsName) {
		t.Fatalf("VerifyDisc = %v, want a complaint about the missing sums file", err)
	}
	if err := VerifyDisc(context.Background(), e.opts(), 0, t.TempDir()); err == nil {
		t.Fatal("expected an error for disc number 0")
	}
}

func TestBurnWithoutISOs(t *testing.T) {
	e := newEnv(t)
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Xorriso, Path: "/bin/true", Found: true}})
	e.cfg.Burner = "/dev/sr0"
	err := Burn(context.Background(), e.opts(), "all")
	if err == nil || !strings.Contains(err.Error(), "run 'brb backup' first") {
		t.Fatalf("Burn = %v, want a message pointing at backup", err)
	}
}

func TestBurnRejectsABadSelector(t *testing.T) {
	e := newEnv(t)
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Xorriso, Path: "/bin/true", Found: true}})
	e.cfg.Burner = "/dev/sr0"
	if err := os.MkdirAll(e.cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.Dirs().ISO, "disc01.iso"), []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"", "everything", "-1", "0", "1.5"} {
		if err := Burn(context.Background(), e.opts(), which); err == nil {
			t.Fatalf("Burn(%q) succeeded, want a usage error", which)
		}
	}
	// A well-formed selector naming a disc this set does not have is not a
	// parse error; it is an empty queue, and says so.
	if err := Burn(context.Background(), e.opts(), "9"); err == nil ||
		!strings.Contains(err.Error(), `no discs matched "9"`) {
		t.Fatalf("Burn(\"9\") = %v, want a complaint that nothing matched", err)
	}
}

func TestBurnSkipsWhenTheOperatorDeclines(t *testing.T) {
	e := newEnv(t)
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Xorriso, Path: "/bin/false", Found: true}})
	e.cfg.Burner = "/dev/sr0"
	e.ui.SetInput(strings.NewReader("n\n"))
	if err := os.MkdirAll(e.cfg.Dirs().ISO, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.Dirs().ISO, "disc01.iso"), []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The burner is /bin/false, so a burn that actually ran would fail.
	if err := Burn(context.Background(), e.opts(), "1"); err != nil {
		t.Fatalf("Burn: %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "skipped disc 1") {
		t.Fatalf("log does not record the skip:\n%s", e.log())
	}
	// The closing line is the one pasted into notes, so it must count what
	// reached the medium: "burned 1 disc(s)" here would claim a disc that was
	// declined.
	if !strings.Contains(e.log(), "burned 0 of 1 disc(s), 1 skipped") {
		t.Fatalf("the summary counts the declined disc as burned:\n%s", e.log())
	}
}

// realTools returns the detected tool set, skipping the test when any of names
// is missing. Nothing in this package's tests requires these to be installed.
func realTools(t *testing.T, names ...string) *tools.Set {
	t.Helper()
	s := tools.Detect(context.Background())
	if err := s.Require(names...); err != nil {
		t.Skipf("skipping: %v", err)
	}
	return s
}

// TestRestoreExtractsAndDeletesThePlaintext exercises the whole read path
// against real squashfs tools when they are installed.
func TestRestoreExtractsAndDeletesThePlaintext(t *testing.T) {
	e := newEnv(t)
	s := realTools(t, tools.Mksquashfs, tools.Unsquashfs)
	if !s.MksquashfsHasCpioStyle0(context.Background()) {
		t.Skip("skipping: this mksquashfs has no -cpiostyle0")
	}
	e.tools = s
	ctx := context.Background()

	src := filepath.Join(e.dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(e.dir, "disc01.squashfs")
	if err := s.BuildImage(ctx, tools.MkOptions{
		SourceDir:   src,
		Out:         img,
		Files:       []string{"a.txt", "sub", "sub/b.txt"},
		Compression: "none",
		BlockSize:   "128K",
	}); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	enc := filepath.Join(e.cfg.Dirs().Enc, encName(1))
	sums, err := agecrypt.Encrypt(ctx, img, enc, e.recipients, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSumFile(enc+sumExt, sums.Cipher, encName(1)); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSumFile(filepath.Join(e.cfg.Dirs().Enc, "disc01.squashfs"+sumExt),
		sums.Plain, "disc01.squashfs"); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(e.dir, "dest")
	if err := Restore(ctx, e.opts(), RestoreOptions{Dest: dest}); err != nil {
		t.Fatalf("Restore: %v\n%s", err, e.log())
	}
	for _, rel := range []string{"a.txt", "sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s was not restored: %v", rel, err)
		}
	}
	plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	if _, err := os.Stat(plain); err == nil {
		t.Fatal("the decrypted image was kept even though --keep-images was not given")
	}

	// --only, and --keep-images.
	dest2 := filepath.Join(e.dir, "dest2")
	if err := Restore(ctx, e.opts(), RestoreOptions{
		Dest: dest2, Only: []string{"sub/b.txt"}, KeepImages: true,
	}); err != nil {
		t.Fatalf("Restore --only: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(filepath.Join(dest2, "sub", "b.txt")); err != nil {
		t.Fatalf("the requested path was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest2, "a.txt")); err == nil {
		t.Fatal("--only extracted a path that was not asked for, so the filter did not follow the image")
	}
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("--keep-images did not keep the decrypted image: %v", err)
	}

	// List reads the same image.
	var out bytes.Buffer
	if err := List(ctx, e.opts(), 1, &out); err != nil {
		t.Fatalf("List: %v\n%s", err, e.log())
	}
	if !strings.Contains(out.String(), "b.txt") {
		t.Fatalf("List did not mention b.txt:\n%s", out.String())
	}
}

func TestRestoreNeedsADestinationAndAKey(t *testing.T) {
	e := newEnv(t)
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Unsquashfs, Path: "/bin/true", Found: true}})
	ctx := context.Background()

	if err := Restore(ctx, e.opts(), RestoreOptions{}); err == nil {
		t.Fatal("expected an error with no destination")
	}
	if err := Restore(ctx, e.opts(), RestoreOptions{Dest: filepath.Join(e.dir, "d"), Only: []string{"  "}}); err == nil {
		t.Fatal("expected an error for an empty --only path")
	}
	e.cfg.AgeIdentity = filepath.Join(e.dir, "gone.txt")
	err := Restore(ctx, e.opts(), RestoreOptions{Dest: filepath.Join(e.dir, "d")})
	if err == nil || !strings.Contains(err.Error(), "age identity") {
		t.Fatalf("Restore = %v, want a complaint about the identity", err)
	}
}

func TestRestoreWithNothingIngested(t *testing.T) {
	e := newEnv(t)
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Unsquashfs, Path: "/bin/true", Found: true}})
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: filepath.Join(e.dir, "d")})
	if err == nil || !strings.Contains(err.Error(), "run 'brb ingest' first") {
		t.Fatalf("Restore = %v, want a message pointing at ingest", err)
	}
}

func TestRestoreWithoutUnsquashfs(t *testing.T) {
	e := newEnv(t)
	err := Restore(context.Background(), e.opts(), RestoreOptions{Dest: filepath.Join(e.dir, "d")})
	if !errors.Is(err, tools.ErrMissing) {
		t.Fatalf("Restore = %v, want tools.ErrMissing", err)
	}
}

func TestMountNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: running as root, where this check does not apply")
	}
	e := newEnv(t)
	err := Mount(context.Background(), e.opts(), 1, filepath.Join(e.dir, "mp"))
	if err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("Mount = %v, want a complaint about root", err)
	}
	if err := Mount(context.Background(), e.opts(), 1, ""); err == nil {
		t.Fatal("expected an error with no mount point")
	}
}

// TestIndexPointsAtTheSidecarParity covers the file whose damage is otherwise
// invisible until the day it matters: the index is gzip inside age, so one
// flipped bit costs the whole map of which disc holds what. Whether the damage
// is caught by the recorded hash or only by age's own authentication, the
// operator has to be told about sidecars.par2, which is the only thing on the
// disc that can put it back.
func TestIndexPointsAtTheSidecarParity(t *testing.T) {
	tests := []struct {
		name    string
		withSum bool
	}{
		{"the recorded hash catches it", true},
		{"age's authentication catches it", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeIndex("1\tPhotos/2024/IMG_0001.JPG\n2\tsrc/main.go\n")
			idx := filepath.Join(e.cfg.Dirs().Enc, indexName)

			if tc.withSum {
				sum, err := agecrypt.SumFile(context.Background(), idx)
				if err != nil {
					t.Fatal(err)
				}
				if err := agecrypt.WriteSumFile(idx+sumExt, sum, indexName); err != nil {
					t.Fatal(err)
				}
			}

			// Rot one byte, as a disc does.
			body, err := os.ReadFile(idx)
			if err != nil {
				t.Fatal(err)
			}
			body[len(body)/2] ^= 0x40
			if err := os.WriteFile(idx, body, 0o600); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			err = Index(context.Background(), e.opts(), "", &out)
			if err == nil {
				t.Fatalf("Index succeeded on a damaged index and wrote %q", out.String())
			}
			if !strings.Contains(err.Error(), "par2 repair -- "+sidecarsPar2) {
				t.Errorf("error = %v\nwant it to name the sidecar parity", err)
			}
			if out.Len() != 0 {
				t.Errorf("a damaged index still produced output: %q", out.String())
			}
		})
	}
}

// TestIndexAcceptsAnIntactIndexWithItsRecordedHash is the other half: the check
// added for the damaged case must not reject a good index.
func TestIndexAcceptsAnIntactIndexWithItsRecordedHash(t *testing.T) {
	e := newEnv(t)
	e.writeIndex("1\tPhotos/2024/IMG_0001.JPG\n")
	idx := filepath.Join(e.cfg.Dirs().Enc, indexName)
	sum, err := agecrypt.SumFile(context.Background(), idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSumFile(idx+sumExt, sum, indexName); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Index(context.Background(), e.opts(), "", &out); err != nil {
		t.Fatalf("Index: %v\n%s", err, e.log())
	}
	if got := out.String(); got != "1\tPhotos/2024/IMG_0001.JPG\n" {
		t.Fatalf("Index wrote %q", got)
	}
}
