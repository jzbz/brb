package agecrypt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWriteSumFileFormat(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		digest  string
		file    string
		want    string
		wantErr bool
	}{
		{
			name:   "plain name",
			digest: sumHelloWorld,
			file:   "disc01.squashfs",
			want:   sumHelloWorld + "  disc01.squashfs\n",
		},
		{
			name:   "relative name",
			digest: sumEmpty,
			file:   "./data/disc01.squashfs.age",
			want:   sumEmpty + "  ./data/disc01.squashfs.age\n",
		},
		{
			name:   "name with spaces",
			digest: sumPayload,
			file:   "./a file with spaces",
			want:   sumPayload + "  ./a file with spaces\n",
		},
		{
			name:   "name with backslash is escaped",
			digest: sumPayload,
			file:   `./back\slash`,
			want:   "\\" + sumPayload + `  ./back\\slash` + "\n",
		},
		{
			name:   "name with newline is escaped",
			digest: sumPayload,
			file:   "./two\nlines",
			want:   "\\" + sumPayload + `  ./two\nlines` + "\n",
		},
		{name: "short digest", digest: "abcd", file: "x", wantErr: true},
		{name: "uppercase digest", digest: strings.ToUpper(sumEmpty), file: "x", wantErr: true},
		{name: "empty name", digest: sumEmpty, file: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, "sum")
			err := WriteSumFile(p, tc.digest, tc.file)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteSumFile: %v", err)
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			// Whatever we write, we must read back identically.
			back, err := ReadSumFile(p)
			if err != nil {
				t.Fatalf("ReadSumFile: %v", err)
			}
			if back[tc.file] != tc.digest {
				t.Errorf("round trip: got %v, want %s -> %s", back, tc.file, tc.digest)
			}
		})
	}
}

func TestReadSumFile(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "text mode",
			body: sumEmpty + "  a.txt\n",
			want: map[string]string{"a.txt": sumEmpty},
		},
		{
			name: "binary mode star",
			body: sumEmpty + " *a.txt\n",
			want: map[string]string{"a.txt": sumEmpty},
		},
		{
			name: "single space",
			body: sumEmpty + " a.txt\n",
			want: map[string]string{"a.txt": sumEmpty},
		},
		{
			name: "crlf and comments",
			body: "# header\r\n\r\n" + sumEmpty + "  a.txt\r\n",
			want: map[string]string{"a.txt": sumEmpty},
		},
		{
			name: "uppercase digest normalised",
			body: strings.ToUpper(sumEmpty) + "  a.txt\n",
			want: map[string]string{"a.txt": sumEmpty},
		},
		{
			name: "several entries",
			body: sumEmpty + "  ./a\n" + sumPayload + "  ./b\n",
			want: map[string]string{"./a": sumEmpty, "./b": sumPayload},
		},
		{
			name: "escaped name",
			body: "\\" + sumEmpty + `  ./two\nlines` + "\n",
			want: map[string]string{"./two\nlines": sumEmpty},
		},
		{
			name: "duplicate identical entry",
			body: sumEmpty + "  a\n" + sumEmpty + "  a\n",
			want: map[string]string{"a": sumEmpty},
		},
		{name: "duplicate conflicting entry", body: sumEmpty + "  a\n" + sumPayload + "  a\n", wantErr: true},
		{name: "truncated line", body: "abc  a.txt\n", wantErr: true},
		{name: "no name", body: sumEmpty + "  \n", wantErr: true},
		{name: "no separator", body: sumEmpty + "a.txt\n", wantErr: true},
		{name: "not hex", body: strings.Repeat("z", 128) + "  a.txt\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "SHA512SUMS")
			writeFile(t, p, []byte(tc.body))
			got, err := ReadSumFile(p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadSumFile: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// buildDiscDir lays out a directory shaped like one of ours.
func buildDiscDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "README.md"), []byte("hello world\n"))
	writeFile(t, filepath.Join(dir, "MANIFEST.txt"), []byte("brb test payload"))
	writeFile(t, filepath.Join(dir, "brb"), []byte(""))
	writeFile(t, filepath.Join(dir, "data", "disc01.squashfs.age"), []byte("hello world\n"))
	writeFile(t, filepath.Join(dir, "data", "index.tsv.gz.age"), []byte("brb test payload"))
	return dir
}

func TestWriteSumsOrderingAndPrefixes(t *testing.T) {
	dir := buildDiscDir(t)
	sumPath := filepath.Join(dir, SumsName)
	// A stale SHA512SUMS must be excluded from its own successor.
	writeFile(t, sumPath, []byte("stale\n"))

	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	got, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := strings.Join([]string{
		sumPayload + "  ./MANIFEST.txt",
		sumHelloWorld + "  ./README.md",
		sumEmpty + "  ./brb",
		sumHelloWorld + "  ./data/disc01.squashfs.age",
		sumPayload + "  ./data/index.tsv.gz.age",
		"",
	}, "\n")
	if string(got) != want {
		t.Errorf("SHA512SUMS mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}

	// Determinism: a second run over the same tree is byte-identical.
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums (second): %v", err)
	}
	again, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(again) != string(got) {
		t.Error("WriteSums is not deterministic")
	}
}

func TestWriteSumsSkipsNonRegularFiles(t *testing.T) {
	dir := buildDiscDir(t)
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sumPath := filepath.Join(dir, SumsName)
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	sums, err := ReadSumFile(sumPath)
	if err != nil {
		t.Fatalf("ReadSumFile: %v", err)
	}
	if _, ok := sums["./link"]; ok {
		t.Error("a symlink was hashed; find -type f would have skipped it")
	}
	if len(sums) != 5 {
		t.Errorf("got %d entries, want 5: %v", len(sums), sums)
	}
}

// TestWriteSumsSkipsPartFiles is the crash-recovery case: a run killed inside
// writeSums leaves SHA512SUMS.part behind, and the resumed run used to hash it
// into the new SHA512SUMS as a phantom "./SHA512SUMS.part" — a line for a file
// the install rename then takes away, so the disc never verifies. Any .part
// file, at any depth, is this package's own in-progress marker and must never
// be attested. VerifyDir over the result is the proof: it must pass, which it
// cannot if a phantom line names a file that is not there.
func TestWriteSumsSkipsPartFiles(t *testing.T) {
	dir := buildDiscDir(t)
	sumPath := filepath.Join(dir, SumsName)
	stale := []string{
		filepath.Join(dir, SumsName+".part"),
		filepath.Join(dir, "data", "disc01.squashfs.age.part"),
	}
	for _, p := range stale {
		if err := os.WriteFile(p, []byte("half-written remains of a dead run\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	sums, err := ReadSumFile(sumPath)
	if err != nil {
		t.Fatalf("ReadSumFile: %v", err)
	}
	for name := range sums {
		if strings.HasSuffix(name, ".part") {
			t.Errorf("SHA512SUMS attests %q, an in-progress remnant that is not a file on the disc", name)
		}
	}
	if len(sums) != 5 {
		t.Errorf("got %d entries, want 5: %v", len(sums), sums)
	}
	// The stale sums remnant is gone (writeFileAtomic wrote over and renamed
	// it); the other one is still lying there, and the disc must still verify.
	if _, err := os.Stat(stale[0]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s survived the install rename: %v", stale[0], err)
	}
	if bad, err := VerifyDir(t.Context(), dir, sumPath); err != nil || len(bad) != 0 {
		t.Errorf("VerifyDir over sums written beside .part remnants: bad=%v err=%v", bad, err)
	}
}

func TestWriteSumsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	sumPath := filepath.Join(dir, SumsName)
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	body, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("want an empty file, got %q", body)
	}
}

func TestWriteSumsCancelled(t *testing.T) {
	dir := buildDiscDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sumPath := filepath.Join(dir, SumsName)
	if err := WriteSums(ctx, dir, sumPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteSums: %v, want context.Canceled", err)
	}
	for _, p := range []string{sumPath, sumPath + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after cancellation (err=%v)", p, err)
		}
	}
}

func TestVerifyDir(t *testing.T) {
	dir := buildDiscDir(t)
	sumPath := filepath.Join(dir, SumsName)
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}

	bad, err := VerifyDir(t.Context(), dir, sumPath)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("clean directory reported bad files: %v", bad)
	}

	// Flip one byte in one file: only that file must be reported.
	writeFile(t, filepath.Join(dir, "data", "disc01.squashfs.age"), []byte("hello worlD\n"))
	bad, err = VerifyDir(t.Context(), dir, sumPath)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if !reflect.DeepEqual(bad, []string{"./data/disc01.squashfs.age"}) {
		t.Errorf("bad = %v, want [./data/disc01.squashfs.age]", bad)
	}

	// A missing file counts as bad, not as a hard error.
	if err := os.Remove(filepath.Join(dir, "brb")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	bad, err = VerifyDir(t.Context(), dir, sumPath)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	want := []string{"./brb", "./data/disc01.squashfs.age"}
	if !reflect.DeepEqual(bad, want) {
		t.Errorf("bad = %v, want %v", bad, want)
	}
}

func TestVerifyDirRejectsEscapingNames(t *testing.T) {
	dir := t.TempDir()
	sumPath := filepath.Join(dir, "escape.sha512")
	writeFile(t, sumPath, []byte(sumEmpty+"  ../outside\n"))
	writeFile(t, filepath.Join(filepath.Dir(dir), "outside"), []byte(""))

	bad, err := VerifyDir(t.Context(), dir, sumPath)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if !reflect.DeepEqual(bad, []string{"../outside"}) {
		t.Errorf("bad = %v, want [../outside]; a checksum file must not reach outside the disc", bad)
	}
}

func TestVerifyDirCancelled(t *testing.T) {
	dir := buildDiscDir(t)
	sumPath := filepath.Join(dir, SumsName)
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := VerifyDir(ctx, dir, sumPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyDir: %v, want context.Canceled", err)
	}
}

// TestSha512sumCompatibility checks our output against the real coreutils tool
// when it is installed, since a disc must verify with either implementation.
func TestSha512sumCompatibility(t *testing.T) {
	bin, err := exec.LookPath("sha512sum")
	if err != nil {
		t.Skip("sha512sum not installed")
	}
	dir := buildDiscDir(t)
	sumPath := filepath.Join(dir, SumsName)
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), bin, "-c", "--quiet", SumsName)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sha512sum -c rejected our SHA512SUMS: %v\n%s", err, out)
	}

	// And the reverse: a file coreutils wrote must parse for us.
	ref := filepath.Join(dir, "ref.sums")
	f, err := os.Create(ref)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gen := exec.CommandContext(t.Context(), bin, "README.md", "data/disc01.squashfs.age")
	gen.Dir = dir
	gen.Stdout = f
	if err := gen.Run(); err != nil {
		f.Close()
		t.Fatalf("sha512sum: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := ReadSumFile(ref)
	if err != nil {
		t.Fatalf("ReadSumFile: %v", err)
	}
	want := map[string]string{
		"README.md":                sumHelloWorld,
		"data/disc01.squashfs.age": sumHelloWorld,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	bad, err := VerifyDir(t.Context(), dir, ref)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("coreutils-written sums failed our verification: %v", bad)
	}
}

// TestSumsRoundTripACarriageReturn is the defect in one test: escapeName wrote
// a CR raw, ReadSumFile's CRLF tolerance ate it, and VerifyDir then reported a
// clean directory as corrupt. GNU coreutils 9.x escapes it for the same reason.
func TestSumsRoundTripACarriageReturn(t *testing.T) {
	dir := t.TempDir()
	name := "file-with-cr\r"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("hello world\n"), 0o644); err != nil {
		t.Skipf("filesystem rejects %q: %v", name, err)
	}
	sumPath := filepath.Join(t.TempDir(), SumsName)
	if err := WriteSums(t.Context(), dir, sumPath); err != nil {
		t.Fatalf("WriteSums: %v", err)
	}
	body, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), `\`) || !strings.Contains(string(body), `./file-with-cr\r`) {
		t.Errorf("line is not escaped the way sha512sum escapes it:\n%q", body)
	}
	got, err := ReadSumFile(sumPath)
	if err != nil {
		t.Fatalf("ReadSumFile: %v", err)
	}
	if _, ok := got["./"+name]; !ok {
		t.Fatalf("the name did not survive the round trip: %q", got)
	}
	bad, err := VerifyDir(t.Context(), dir, sumPath)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("VerifyDir called an untouched directory corrupt: %q", bad)
	}
}

// TestWriteSumsKnownSkipsOnlyTheSameInode covers both halves of the shortcut:
// the digest is trusted for the hard link it was measured from, and a file that
// merely shares its name is hashed the long way.
func TestWriteSumsKnownSkipsOnlyTheSameInode(t *testing.T) {
	dir := buildDiscDir(t)
	enc := t.TempDir()

	// The real layout: the disc directory's image is a hard link to enc/'s.
	linked := filepath.Join(enc, "disc01.squashfs.age")
	if err := os.Link(filepath.Join(dir, "data", "disc01.squashfs.age"), linked); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	// And a decoy: same name, same contents, different inode.
	decoy := filepath.Join(enc, "index.tsv.gz.age")
	writeFile(t, decoy, []byte("brb test payload"))

	// A deliberately wrong digest for each, so the test can see which one was
	// believed and which one was recomputed.
	wrong := strings.Repeat("a", 128)
	known := map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: wrong, Same: linked},
		"./data/index.tsv.gz.age":    {Digest: wrong, Same: decoy},
	}
	sumPath := filepath.Join(t.TempDir(), SumsName)
	if err := WriteSumsKnown(t.Context(), dir, sumPath, known); err != nil {
		t.Fatalf("WriteSumsKnown: %v", err)
	}
	got, err := ReadSumFile(sumPath)
	if err != nil {
		t.Fatalf("ReadSumFile: %v", err)
	}
	if got["./data/disc01.squashfs.age"] != wrong {
		t.Errorf("the recorded digest for the hard link was ignored: %s", got["./data/disc01.squashfs.age"])
	}
	if got["./data/index.tsv.gz.age"] != sumPayload {
		t.Errorf("a digest was trusted for a file that is not the same inode: %s",
			got["./data/index.tsv.gz.age"])
	}
	// Everything else is hashed exactly as before.
	if got["./README.md"] != sumHelloWorld {
		t.Errorf("./README.md = %s, want %s", got["./README.md"], sumHelloWorld)
	}
	// A malformed recorded digest is a caller bug, not something to hash around.
	if err := WriteSumsKnown(t.Context(), dir, sumPath, map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: "nonsense", Same: linked},
	}); err == nil {
		t.Error("WriteSumsKnown accepted a digest that is not a sha-512")
	}
}
