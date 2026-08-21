package tools

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// randomBytes returns n deterministic pseudo-random bytes.
//
// Deterministic so that a failure reproduces; pseudo-random because the par2
// tests below depend on no two data blocks of a fixture being byte-identical.
//
// That is not a stylistic preference. par2 identifies data blocks by hash, so
// a file whose blocks are all the same bytes gives it no way to tell which
// position a block it found belongs to. TestPar2CreateVerifyRepair used to
// build its payload as "recovery data test\n" repeated to 380000 bytes and ask
// for 40 blocks: 380000/40 is 9500, 9500/19 is exactly 500, and so all forty
// blocks were identical. par2cmdline 1.2.0 sees through that. 0.8.1 — which is
// what Debian and Ubuntu ship, and what CI runs — reports "found 40 of 40" on
// the damaged file, concludes that none of the recovery blocks are needed, and
// rewrites a file that is still wrong.
//
// Nothing brb writes can look like that: the payload is age ciphertext and the
// sidecars are hex digests. The fixtures were the thing out of step with the
// data they stand for, so they are generated the way the real thing looks.
func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.New(rand.NewSource(seed)).Read(b); err != nil {
		panic(err) // (*rand.Rand).Read never returns an error
	}
	return b
}

// realSet returns a Set detected from PATH, skipping the test unless every
// named binary is installed. None of these tests are required to pass on a
// machine without squashfs-tools, par2 or xorriso.
func realSet(t *testing.T, names ...string) *Set {
	t.Helper()
	s := Detect(t.Context())
	if err := s.Require(names...); err != nil {
		t.Skipf("skipping: %v", err)
	}
	return s
}

// makeTree writes a small source tree and returns its root plus the relative
// paths in it.
func makeTree(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"top.txt":        "top level\n",
		"docs/a.txt":     strings.Repeat("alpha\n", 500),
		"docs/b.txt":     strings.Repeat("beta\n", 500),
		"data/nested/c":  strings.Repeat("gamma\n", 500),
		"odd name .txt ": "spaces in the name\n",
	}
	var rel []string
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	return root, rel
}

func TestBuildImageAndExtract(t *testing.T) {
	s := realSet(t, Mksquashfs, Unsquashfs)
	ctx := t.Context()

	src, files := makeTree(t)
	work := t.TempDir()
	img := filepath.Join(work, "disc01.squashfs")

	var log bytes.Buffer
	if err := s.BuildImage(ctx, MkOptions{
		SourceDir:   src,
		Out:         img,
		Files:       files,
		Compression: "none",
		BlockSize:   "1M",
		Log:         &log,
	}); err != nil {
		t.Fatalf("BuildImage() = %v\nlog: %s", err, log.String())
	}

	st, err := os.Stat(img)
	if err != nil {
		t.Fatalf("no image produced: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("image is empty")
	}
	// The image path must never be contaminated by the tool's chatter.
	if strings.Contains(log.String(), img) && strings.Contains(img, "\n") {
		t.Error("image path looks like captured output")
	}

	stats, err := s.ImageStats(ctx, img)
	if err != nil {
		t.Fatalf("ImageStats() = %v", err)
	}
	if !strings.Contains(strings.ToLower(stats), "squashfs") {
		t.Errorf("ImageStats() does not look like a superblock summary: %q", stats)
	}

	dest := t.TempDir()
	if err := s.Unsquashfs(ctx, UnsqOptions{Image: img, Dest: dest, Force: true}); err != nil {
		t.Fatalf("Unsquashfs() = %v", err)
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("missing after extraction: %s (%v)", f, err)
		}
	}

	var listing bytes.Buffer
	if err := s.UnsquashfsList(ctx, img, &listing); err != nil {
		t.Fatalf("UnsquashfsList() = %v", err)
	}
	if !strings.Contains(listing.String(), "top.txt") {
		t.Errorf("listing does not mention top.txt:\n%s", listing.String())
	}
}

func TestBuildImageOnlySubset(t *testing.T) {
	s := realSet(t, Mksquashfs, Unsquashfs)
	ctx := t.Context()

	src, _ := makeTree(t)
	work := t.TempDir()
	img := filepath.Join(work, "disc01.squashfs")
	if err := s.BuildImage(ctx, MkOptions{
		SourceDir:   src,
		Out:         img,
		Files:       []string{"docs/a.txt", "docs/b.txt"},
		Compression: "none",
	}); err != nil {
		t.Fatalf("BuildImage() = %v", err)
	}

	dest := t.TempDir()
	if err := s.Unsquashfs(ctx, UnsqOptions{
		Image: img, Dest: dest, Force: true, Only: []string{"docs/a.txt"},
	}); err != nil {
		t.Fatalf("Unsquashfs() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "docs/a.txt")); err != nil {
		t.Errorf("requested path was not extracted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "docs/b.txt")); err == nil {
		t.Error("docs/b.txt was extracted although Only asked for docs/a.txt")
	}
}

func TestBuildImageRemovesAPartialImageOnFailure(t *testing.T) {
	s := realSet(t, Mksquashfs)
	src, files := makeTree(t)
	img := filepath.Join(t.TempDir(), "disc01.squashfs")

	// 7 is not a legal squashfs block size, so mksquashfs refuses.
	err := s.BuildImage(t.Context(), MkOptions{
		SourceDir: src, Out: img, Files: files, BlockSize: "7", Compression: "none",
	})
	if err == nil {
		t.Fatal("BuildImage() = nil, want a failure for an illegal block size")
	}
	if _, statErr := os.Stat(img); statErr == nil {
		t.Errorf("a partial image was left behind at %s", img)
	}
}

func TestBuildImageRejectsABadFileList(t *testing.T) {
	s := realSet(t, Mksquashfs)
	src := t.TempDir()
	img := filepath.Join(t.TempDir(), "x.squashfs")

	tests := []struct {
		name  string
		files []string
	}{
		{"empty list", nil},
		{"empty entry", []string{"ok", ""}},
		{"embedded NUL", []string{"bad\x00path"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.BuildImage(t.Context(), MkOptions{
				SourceDir: src, Out: img, Files: tc.files,
			}); err == nil {
				t.Fatal("BuildImage() = nil, want a validation error")
			}
		})
	}
}

// TestBuildImageRejectsARelativeOutput pins the guard against the one path in
// this package that is resolved against two different working directories.
// mksquashfs runs with SourceDir as its cwd and resolves the output path on its
// command line there; BuildImage's own Remove, cleanup defer and Stat resolve
// the same string in brb's cwd. A relative Out therefore names two files, and
// the dangerous half of that is silent: with a matching directory under
// SourceDir the tool writes a whole disc of plaintext where brb never looks,
// never cleans up and never encrypts.
func TestBuildImageRejectsARelativeOutput(t *testing.T) {
	s := realSet(t, Mksquashfs)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory the relative path would resolve into, so a missing guard
	// fails by writing the image in the wrong place rather than by erroring.
	if err := os.Mkdir(filepath.Join(src, "img"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := s.BuildImage(t.Context(), MkOptions{
		SourceDir: src, Out: filepath.Join("img", "disc01.squashfs"), Files: []string{"a"},
	})
	if err == nil {
		t.Fatal("BuildImage() = nil for a relative output path, want a rejection")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("BuildImage() = %v, want an error saying the output path must be absolute", err)
	}
	if _, statErr := os.Stat(filepath.Join(src, "img", "disc01.squashfs")); statErr == nil {
		t.Error("an image was written under SourceDir, where brb would never find or delete it")
	}
}

func TestMksquashfsCapabilities(t *testing.T) {
	s := realSet(t, Mksquashfs)
	ctx := t.Context()

	if !s.MksquashfsHasCpioStyle0(ctx) {
		t.Error("MksquashfsHasCpioStyle0 = false; squashfs-tools 4.5+ should have it")
	}
	comps := s.MksquashfsCompressors(ctx)
	if len(comps) == 0 {
		t.Fatal("MksquashfsCompressors returned nothing")
	}
	found := false
	for _, c := range comps {
		if c == "gzip" {
			found = true
		}
	}
	if !found {
		t.Errorf("compressors %v do not include gzip, which is always built in", comps)
	}
	// Cached: the second call must agree with the first.
	if got := s.MksquashfsCompressors(ctx); len(got) != len(comps) {
		t.Errorf("second call returned %v, want %v", got, comps)
	}
}

func TestPar2CreateVerifyRepair(t *testing.T) {
	s := realSet(t, Par2)
	ctx := t.Context()

	dir := t.TempDir()
	name := "disc01.squashfs.age"
	// 380000 bytes of high-entropy payload, as an encrypted image is. See
	// randomBytes: a repeated string here divides evenly into the block size
	// and makes every block identical, which par2cmdline 0.8.1 cannot repair.
	body := randomBytes(1, 380000)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	if err := s.Par2Create(ctx, Par2Options{
		Dir: dir, File: name, Redundancy: 20, Blocks: 40, MemoryMB: 64, Log: &log,
	}); err != nil {
		t.Fatalf("Par2Create() = %v\nlog: %s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, name+".par2")); err != nil {
		t.Fatalf("no par2 index produced: %v", err)
	}

	if err := s.Par2Verify(ctx, dir, name+".par2", nil); err != nil {
		t.Fatalf("Par2Verify() on an intact file = %v", err)
	}

	// Corrupt a chunk in the middle and prove verify notices, then repair fixes.
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0}, 4096), int64(len(body)/2)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Par2Verify(ctx, dir, name+".par2", nil); err == nil {
		t.Error("Par2Verify() = nil on a corrupted file; the exit status was swallowed")
	}
	if err := s.Par2Repair(ctx, dir, name+".par2", nil); err != nil {
		t.Fatalf("Par2Repair() = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("repaired file does not match the original")
	}
}

// TestPar2CreateProtectsSeveralFilesUnderOneSet covers the sidecars.par2 form:
// one recovery set over several small files, named for itself rather than for
// any of them. The set is then moved to a different directory before it is used
// — a recovery set that recorded anything but bare file names would not repair
// on the disc it was burned onto, which is the whole point of running par2 with
// the data directory as its working directory.
func TestPar2CreateProtectsSeveralFilesUnderOneSet(t *testing.T) {
	s := realSet(t, Par2)
	ctx := t.Context()

	dir := t.TempDir()
	// Shaped like the real files: 64 random bytes hex-encoded is 128 characters,
	// which is exactly a SHA-512 digest, and the index is encrypted gzip. The
	// previous spelling repeated a four-byte group, giving these a period that
	// divides the par2 block size — the duplicate-block trap randomBytes
	// describes, which this test only escaped by an accident of block sizing.
	members := map[string][]byte{
		"disc01.squashfs.age.sha512": []byte(hex.EncodeToString(randomBytes(2, 64)) + "  disc01.squashfs.age\n"),
		"disc01.squashfs.sha512":     []byte(hex.EncodeToString(randomBytes(3, 64)) + "  disc01.squashfs\n"),
		"index.tsv.gz.age":           randomBytes(4, 4800),
	}
	names := make([]string, 0, len(members))
	for name, body := range members {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var log bytes.Buffer
	if err := s.Par2Create(ctx, Par2Options{
		Dir: dir, File: "sidecars.par2", Inputs: names, Redundancy: 50, Blocks: 100, Log: &log,
	}); err != nil {
		t.Fatalf("Par2Create() = %v\nlog: %s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "sidecars.par2")); err != nil {
		t.Fatalf("no sidecars.par2 produced: %v", err)
	}
	vols, err := filepath.Glob(filepath.Join(dir, "sidecars.vol*.par2"))
	if err != nil || len(vols) == 0 {
		t.Fatalf("no sidecars.vol*.par2 produced (%v)", err)
	}
	// The set must not be named after one of the files it covers.
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name+".par2")); err == nil {
			t.Errorf("par2 also produced %s.par2; the set was not named for itself", name)
		}
	}

	// Move everything somewhere else, exactly as burning the disc does.
	moved := t.TempDir()
	all, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(moved, e.Name()), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Par2Verify(ctx, moved, "sidecars.par2", nil); err != nil {
		t.Fatalf("Par2Verify() on an intact, relocated set = %v", err)
	}

	// Rot one byte of the smallest file — the case the image's own parity
	// cannot help with — and prove the set notices and puts it back.
	target := filepath.Join(moved, "disc01.squashfs.sha512")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), body...)
	damaged[3] ^= 0xff
	if err := os.WriteFile(target, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Par2Verify(ctx, moved, "sidecars.par2", nil); err == nil {
		t.Error("Par2Verify() = nil on a corrupted sidecar; the exit status was swallowed")
	}
	if err := s.Par2Repair(ctx, moved, "sidecars.par2", nil); err != nil {
		t.Fatalf("Par2Repair() = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("repaired sidecar = %q, want %q", got, body)
	}
}

func TestPar2CreateRejectsAnAbsoluteFile(t *testing.T) {
	s := realSet(t, Par2)
	if err := s.Par2Create(t.Context(), Par2Options{
		Dir: t.TempDir(), File: "/etc/hostname",
	}); err == nil {
		t.Fatal("Par2Create() = nil, want a rejection of an absolute file name")
	}
}

func TestProbeISO(t *testing.T) {
	s := realSet(t, Xorriso)
	if err := s.ProbeISO(t.Context()); err != nil {
		t.Fatalf("ProbeISO() = %v", err)
	}
}

func TestMakeISO(t *testing.T) {
	s := realSet(t, Xorriso)
	ctx := t.Context()

	src, _ := makeTree(t)
	out := filepath.Join(t.TempDir(), "disc01.iso")
	var log bytes.Buffer
	if err := s.MakeISO(ctx, ISOOptions{
		Dir:     src,
		Out:     out,
		Label:   DiscLabel("BACKUP", 1, 3),
		AppID:   "brb 1.0.0",
		Publish: "home-2026-01-01",
		Log:     &log,
	}); err != nil {
		t.Fatalf("MakeISO() = %v\nlog: %s", err, log.String())
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("no ISO produced: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("ISO is empty")
	}
	for _, noisy := range []string{"UPDATE", "ISO image produced"} {
		if strings.Contains(log.String(), noisy) {
			t.Errorf("noisy line %q reached the log:\n%s", noisy, log.String())
		}
	}
}

func TestMakeISOHonoursTheExitStatus(t *testing.T) {
	s := realSet(t, Xorriso)
	src, _ := makeTree(t)
	// A destination whose parent does not exist: xorriso fails, and a
	// "| grep ... || true" pipeline would have reported success.
	out := filepath.Join(t.TempDir(), "no-such-dir", "disc01.iso")
	err := s.MakeISO(t.Context(), ISOOptions{Dir: src, Out: out, Label: "PROBE"})
	if err == nil {
		t.Fatal("MakeISO() = nil, want xorriso's failure to surface")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("a partial ISO was left behind at %s", out)
	}
}

func TestMakeISORejectsAnIllegalLabel(t *testing.T) {
	s := realSet(t, Xorriso)
	src, _ := makeTree(t)
	out := filepath.Join(t.TempDir(), "x.iso")
	if err := s.MakeISO(t.Context(), ISOOptions{
		Dir: src, Out: out, Label: "not a legal label",
	}); err == nil {
		t.Fatal("MakeISO() = nil, want a rejection of an unsanitised label")
	}
}

func TestBurnISORefusesAMissingOrEmptyISO(t *testing.T) {
	s := realSet(t, Xorriso)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.iso")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.BurnISO(t.Context(), "/dev/sr0", empty, 4, nil); err == nil {
		t.Error("BurnISO() = nil for an empty ISO")
	}
	if err := s.BurnISO(t.Context(), "/dev/sr0", filepath.Join(dir, "nope.iso"), 4, nil); err == nil {
		t.Error("BurnISO() = nil for a missing ISO")
	}
	if err := s.BurnISO(t.Context(), "", empty, 4, nil); err == nil {
		t.Error("BurnISO() = nil for an empty device")
	}
}

func TestDetectCapturesVersions(t *testing.T) {
	s := realSet(t, Mksquashfs)
	got := s.Get(Mksquashfs)
	if got.Version == "" {
		t.Error("Detect did not capture a version for mksquashfs")
	}
	if strings.Contains(got.Version, "\n") {
		t.Errorf("version string spans lines: %q", got.Version)
	}
}

// TestBuildImageFailsWhenMksquashfsEmptiesAFile runs the real tool over a file
// it cannot read. mksquashfs exits 0 and writes a zero-byte file in its place;
// BuildImage must not accept that as success, must name the file, and must not
// leave the image behind — an image that passes every hash while missing data
// is the worst artefact a backup tool can produce.
func TestBuildImageFailsWhenMksquashfsEmptiesAFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not apply")
	}
	s := realSet(t, Mksquashfs)
	src, files := makeTree(t)
	locked := filepath.Join(src, "docs", "b.txt")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	img := filepath.Join(t.TempDir(), "disc01.squashfs")
	var log bytes.Buffer
	err := s.BuildImage(t.Context(), MkOptions{
		SourceDir: src, Out: img, Files: files, Compression: "none", Log: &log,
	})
	if err == nil {
		t.Fatalf("BuildImage() = nil over an unreadable file, want a failure\nlog: %s", log.String())
	}
	if !strings.Contains(err.Error(), "docs/b.txt") || !strings.Contains(err.Error(), "EMPTY") {
		t.Errorf("error does not name the emptied file and the consequence: %v", err)
	}
	if _, statErr := os.Stat(img); statErr == nil {
		t.Errorf("an image with an emptied file in it was left behind at %s", img)
	}
	if !strings.Contains(log.String(), "creating empty file") {
		t.Errorf("mksquashfs's own message was not forwarded to the log:\n%s", log.String())
	}
}
