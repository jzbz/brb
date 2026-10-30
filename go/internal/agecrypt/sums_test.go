package agecrypt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestReadSumFileRefusesAnEnormousChecksumFile: SHA512SUMS comes off a disc
// somebody else supplied — restore mounts it and hands the path to VerifyDir
// with nothing in between looking at its size — and every parsed line is kept
// in the returned map. The per-line buffer was capped and the line count was
// not, so a checksum file of distinct valid lines turned file bytes into heap
// until the process was killed. A real disc lists about ten files.
//
// The oversized file here is made of comment lines, which store nothing: that
// is the point of counting lines read rather than entries kept. Without the
// cap this file parses to an empty map and no error at all, which is exactly
// the unbounded scan a hostile file wants.
func TestReadSumFileRefusesAnEnormousChecksumFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, SumsName)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for i := 0; i <= maxSumLines; i++ {
		if _, err := w.WriteString("#\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSumFile(p)
	if err == nil {
		t.Fatalf("ReadSumFile read a file of %d lines, returning %d entries", maxSumLines+1, len(got))
	}
	if !strings.Contains(err.Error(), "refusing to read it") || !strings.Contains(err.Error(), p) {
		t.Errorf("the refusal does not name the file and say it was refused:\n%v", err)
	}

	// The cap must be unreachable by anything brb writes: a disc-sized file
	// still parses, entry for entry.
	small := filepath.Join(dir, "small")
	var b strings.Builder
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&b, "%s  ./data/disc%02d.squashfs.age\n", sumEmpty, i)
	}
	writeFile(t, small, []byte(b.String()))
	sums, err := ReadSumFile(small)
	if err != nil {
		t.Fatalf("ReadSumFile on an ordinary checksum file: %v", err)
	}
	if len(sums) != 64 {
		t.Errorf("got %d entries, want 64", len(sums))
	}
}

// sumFileOfSize writes a SHA512SUMS of exactly size bytes made of valid,
// distinct entries — the shape that is retained in the map line for line, which
// a file of comments is not — each line ending in eol and the last one without
// it when unterminated is set. Names run up to about a megabyte, inside
// maxSumLine, so a few lines reach the byte cap.
func sumFileOfSize(t *testing.T, path string, size int, eol string, unterminated bool) {
	t.Helper()
	const nameLen = 1_000_000
	var b strings.Builder
	for i := 0; ; i++ {
		head := fmt.Sprintf("%s  ./%08d-", sumEmpty, i)
		left := size - b.Len()
		full := len(head) + nameLen + len(eol)
		if left <= full {
			// The last line takes exactly what is left.
			tail := left - len(head)
			if !unterminated {
				tail -= len(eol)
			}
			if tail < 1 {
				t.Fatalf("size %d leaves no room for a last name", size)
			}
			b.WriteString(head + strings.Repeat("n", tail))
			if !unterminated {
				b.WriteString(eol)
			}
			break
		}
		b.WriteString(head + strings.Repeat("n", nameLen) + eol)
	}
	if b.Len() != size {
		t.Fatalf("built %d bytes, want %d", b.Len(), size)
	}
	writeFile(t, path, []byte(b.String()))
}

// TestReadSumFileRefusesMoreThanMaxSumBytes: the line cap and the per-line cap
// were each a bound on one element, and every valid line is kept in the map, so
// a file of a few thousand megabyte-long entries became gigabytes of heap. The
// cap is on bytes READ, and these boundaries are the ones brb.sh's
// sum_file_too_big (head -c, then wc -c) draws: one byte over refuses whatever
// its line endings, exactly at the cap parses whether or not the last line
// ends in a newline. A count of scanned lines plus one per newline gets the
// CRLF file and the unterminated one wrong in opposite directions, which is how
// a first draft of this disagreed with the bash reader.
func TestReadSumFileRefusesMoreThanMaxSumBytes(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name         string
		size         int
		eol          string
		unterminated bool
		refused      bool
	}{
		{"one byte over", maxSumBytes + 1, "\n", false, true},
		{"one byte over, CRLF", maxSumBytes + 1, "\r\n", false, true},
		{"one byte over, last line unterminated", maxSumBytes + 1, "\n", true, true},
		{"exactly at the cap", maxSumBytes, "\n", false, false},
		{"exactly at the cap, CRLF", maxSumBytes, "\r\n", false, false},
		{"exactly at the cap, last line unterminated", maxSumBytes, "\n", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, SumsName)
			sumFileOfSize(t, p, tc.size, tc.eol, tc.unterminated)
			got, err := ReadSumFile(p)
			if !tc.refused {
				if err != nil {
					t.Fatalf("ReadSumFile refused a file of exactly %d bytes: %v", tc.size, err)
				}
				if len(got) < 16 {
					t.Errorf("got %d entries from a file of ~1 MB lines filling the cap", len(got))
				}
				return
			}
			if err == nil {
				t.Fatalf("ReadSumFile read %d bytes, returning %d entries", tc.size, len(got))
			}
			if !strings.Contains(err.Error(), "refusing to read it") || !strings.Contains(err.Error(), "larger than") ||
				!strings.Contains(err.Error(), p) {
				t.Errorf("the refusal does not name the file and say it was too large:\n%v", err)
			}
		})
	}
}

// TestReadSumFileCountsBytesItReadNotBytesAStatReports: a FIFO, like a procfs
// or FUSE file, reports a size of 0 and then streams. ReadSumFile reads through
// whatever it is handed — VerifyDisc and discSums are what refuse a FIFO by
// name — so the cap has to be on what the reads return. Here a writer streams
// comment lines, which store nothing, well past the cap.
func TestReadSumFileCountsBytesItReadNotBytesAStatReports(t *testing.T) {
	p := filepath.Join(t.TempDir(), SumsName)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	go func() {
		w, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer w.Close()
		line := []byte("#" + strings.Repeat("x", 4094) + "\n")
		for written := 0; written <= 2*maxSumBytes; written += len(line) {
			if _, err := w.Write(line); err != nil {
				return // the reader stopped, which is the point
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := ReadSumFile(p)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("ReadSumFile on a stream past the cap: %v, want the size refusal", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ReadSumFile did not stop reading a stream past the cap")
	}
}

// TestReadSumFileLineLengthBoundary pins where the per-line cap falls, because
// brb.sh's SUM_AWK reproduces it by hand (length($0) >= 1048576 refuses): a line
// of maxSumLine-1 bytes is an ordinary entry, and one of maxSumLine bytes
// refuses the whole file even as a comment.
func TestReadSumFileLineLengthBoundary(t *testing.T) {
	dir := t.TempDir()
	head := sumEmpty + "  ./"
	ok := filepath.Join(dir, "ok")
	writeFile(t, ok, []byte(head+strings.Repeat("n", maxSumLine-1-len(head))+"\n"))
	if got, err := ReadSumFile(ok); err != nil || len(got) != 1 {
		t.Fatalf("a line of %d bytes: %d entries, %v", maxSumLine-1, len(got), err)
	}
	for _, unterminated := range []bool{false, true} {
		long := filepath.Join(dir, "long")
		body := "#" + strings.Repeat("c", maxSumLine-1)
		if !unterminated {
			body += "\n"
		}
		writeFile(t, long, []byte(sumEmpty+"  ./a\n"+body))
		if got, err := ReadSumFile(long); err == nil {
			t.Errorf("a comment line of %d bytes (unterminated=%v) was read: %d entries", maxSumLine, unterminated, len(got))
		}
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

// TestVerifyDirRefusesASumsFileThatListsNothing: an empty or comment-only
// SHA512SUMS beside a corrupt image used to come back clean, because the loop
// over its entries had nothing to do. GNU sha512sum -c refuses the same files,
// and brb.sh with it; a "\r\n" line trims to blank, so it lists nothing too.
func TestVerifyDirRefusesASumsFileThatListsNothing(t *testing.T) {
	for name, body := range map[string]string{
		"empty":             "",
		"comments only":     "# nothing\n\n",
		"a blank CRLF line": "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := buildDiscDir(t)
			sumPath := filepath.Join(dir, SumsName)
			writeFile(t, sumPath, []byte(body))
			bad, err := VerifyDir(t.Context(), dir, sumPath)
			if err == nil || !strings.Contains(err.Error(), "lists no files") {
				t.Fatalf("VerifyDir = %v, %v; want a refusal of a sums file that lists nothing", bad, err)
			}
		})
	}
}

// TestVerifyDirRequiresTheNamedFiles: the identity gate in VerifyDisc reads the
// image's name, and nothing made the sums file vouch for it, so a SHA512SUMS
// that listed only the manifest verified a disc whose image was corrupt.
func TestVerifyDirRequiresTheNamedFiles(t *testing.T) {
	dir := buildDiscDir(t)
	sumPath := filepath.Join(dir, SumsName)
	writeFile(t, sumPath, []byte(sumPayload+"  ./MANIFEST.txt\n"))
	writeFile(t, filepath.Join(dir, "data", "disc01.squashfs.age"), []byte("corrupt\n"))
	if bad, err := VerifyDir(t.Context(), dir, sumPath, "data/disc01.squashfs.age"); err == nil {
		t.Fatalf("VerifyDir = %v, nil; a sums file without the required name must be refused", bad)
	}
	// Without the requirement the same file still verifies: it is the caller's
	// rule, not a change to what a sums file may list.
	if bad, err := VerifyDir(t.Context(), dir, sumPath); err != nil || len(bad) != 0 {
		t.Fatalf("VerifyDir without a requirement = %v, %v", bad, err)
	}
	// Either spelling of the name satisfies it, as either resolves to the file.
	writeFile(t, filepath.Join(dir, "data", "disc01.squashfs.age"), []byte("hello world\n"))
	for _, listed := range []string{"./data/disc01.squashfs.age", "data/disc01.squashfs.age"} {
		writeFile(t, sumPath, []byte(sumHelloWorld+"  "+listed+"\n"))
		if bad, err := VerifyDir(t.Context(), dir, sumPath, "data/disc01.squashfs.age"); err != nil || len(bad) != 0 {
			t.Errorf("listed as %q: VerifyDir = %v, %v", listed, bad, err)
		}
	}
}

// TestVerifyDirRefusesLinksAndNonRegularEntries is the rule hashInRoot and
// brb.sh's verify_disc_sums share: no symbolic link anywhere on the way to a
// listed name, nothing climbing out, and a regular file at the end. Every name
// below carries the right digest for what it would read if followed, so only
// the rule can put it in the bad list — and the plain spellings at the end must
// stay out of it, or the rule is refusing more than it says.
func TestVerifyDirRefusesLinksAndNonRegularEntries(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "host"), []byte("hello world\n"))
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x"), []byte("hello world\n"))
	if err := os.MkdirAll(filepath.Join(dir, "data", "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "data", "real", "x"), []byte("hello world\n"))
	for link, target := range map[string]string{
		"lnk":          "x",
		"hostlink":     filepath.Join(outside, "host"),
		"data/sublink": "real",
		"nulllink":     os.DevNull,
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	long := strings.Repeat("a", maxSumName)
	var b strings.Builder
	for _, n := range []string{
		"./lnk", "./hostlink", "./data/sublink/x", "./fifo", "./data", "./data/../x",
		"./data/real/x/", "./" + long,
		"./x", "x", "./data//real/x", "./data/./real/x",
	} {
		b.WriteString(sumHelloWorld + "  " + n + "\n")
	}
	b.WriteString(sumEmpty + "  ./nulllink\n")
	sumPath := filepath.Join(t.TempDir(), SumsName)
	writeFile(t, sumPath, []byte(b.String()))

	type result struct {
		bad []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		bad, err := VerifyDir(t.Context(), dir, sumPath)
		done <- result{bad, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		// Unblock the open so the failure is reported rather than hung.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Fatal("VerifyDir blocked opening a listed FIFO")
	}
	if r.err != nil {
		t.Fatalf("VerifyDir: %v", r.err)
	}
	want := []string{
		"./" + long, "./data", "./data/../x", "./data/real/x/", "./data/sublink/x",
		"./fifo", "./hostlink", "./lnk", "./nulllink",
	}
	if !reflect.DeepEqual(r.bad, want) {
		t.Errorf("bad =\n  %q\nwant\n  %q", r.bad, want)
	}
}

// TestVerifyDirRefusesANameLongerThanPathMax: an os.Root resolves one component
// at a time, so it opens a real path of any length, while brb.sh resolves each
// name relative to the disc root and the kernel stops it at PATH_MAX. A file at
// the bottom of a deep enough tree, carrying its right digest, verified in one
// reader and was a mismatch in the other.
func TestVerifyDirRefusesANameLongerThanPathMax(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	comp := strings.Repeat("d", 200)
	var rel []string
	cur := root
	for len(strings.Join(rel, "/")) < maxSumName {
		if err := cur.Mkdir(comp, 0o755); err != nil {
			t.Fatalf("mkdir at depth %d: %v", len(rel), err)
		}
		next, err := cur.OpenRoot(comp)
		if err != nil {
			t.Fatal(err)
		}
		if cur != root {
			cur.Close()
		}
		cur = next
		rel = append(rel, comp)
	}
	f, err := cur.Create("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hello world\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cur.Close()
	name := "./" + strings.Join(rel, "/") + "/x"
	sumPath := filepath.Join(t.TempDir(), SumsName)
	writeFile(t, sumPath, []byte(sumHelloWorld+"  "+name+"\n"))
	bad, err := VerifyDir(t.Context(), dir, sumPath)
	if err != nil {
		t.Fatalf("VerifyDir: %v", err)
	}
	if len(bad) != 1 {
		t.Errorf("a real file %d bytes deep verified: bad = %d names, want it refused", len(name), len(bad))
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

// TestWriteSumsKnownVerifiesWhatItReads. Verify holds a file to its recorded
// digest whenever it is read rather than shortcut — because the caller gave no
// Same, or because the inode check declined. Without it the read-back recorded
// whatever the bytes hashed to by then, which attests a change rather than
// catching it. The shortcut itself is untouched: a hard link to the measured
// file is still believed without being read.
func TestWriteSumsKnownVerifiesWhatItReads(t *testing.T) {
	dir := buildDiscDir(t)
	sumPath := filepath.Join(t.TempDir(), SumsName)
	wrong := strings.Repeat("a", 128)

	// Read, and matching: recorded as usual.
	if err := WriteSumsKnown(t.Context(), dir, sumPath, map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: sumHelloWorld, Verify: true},
	}); err != nil {
		t.Fatalf("WriteSumsKnown over a matching file: %v", err)
	}
	before, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatal(err)
	}

	// Read, and not matching: refused, with what was found and what was
	// expected, and nothing written.
	err = WriteSumsKnown(t.Context(), dir, sumPath, map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: wrong, Verify: true},
	})
	var mm *DigestMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("WriteSumsKnown over a file that does not match its digest = %v, want *DigestMismatchError", err)
	}
	if mm.Name != "./data/disc01.squashfs.age" || mm.Got != sumHelloWorld || mm.Want != wrong {
		t.Errorf("mismatch = %+v", mm)
	}
	if after, _ := os.ReadFile(sumPath); !bytes.Equal(after, before) {
		t.Error("the refused WriteSumsKnown replaced the previous checksum file")
	}

	// The inode check declined (a decoy with the same bytes elsewhere), so the
	// file is read — and held to the digest.
	decoy := filepath.Join(t.TempDir(), "disc01.squashfs.age")
	writeFile(t, decoy, []byte("hello world\n"))
	if err := WriteSumsKnown(t.Context(), dir, sumPath, map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: wrong, Same: decoy, Verify: true},
	}); !errors.As(err, &mm) {
		t.Errorf("a file read because its inode did not match was not held to its digest: %v", err)
	}

	// The shortcut: the same inode is believed, not read, so there is nothing
	// to compare and the recorded digest is what goes in.
	linked := filepath.Join(t.TempDir(), "disc01.squashfs.age")
	if err := os.Link(filepath.Join(dir, "data", "disc01.squashfs.age"), linked); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := WriteSumsKnown(t.Context(), dir, sumPath, map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: wrong, Same: linked, Verify: true},
	}); err != nil {
		t.Fatalf("WriteSumsKnown with the shortcut available: %v", err)
	}
	if got, _ := ReadSumFile(sumPath); got["./data/disc01.squashfs.age"] != wrong {
		t.Error("Verify disabled the shortcut for the very inode the digest was taken from")
	}

	// A malformed digest given only for comparison is refused before any
	// file is read, as one given for the shortcut always was.
	if err := WriteSumsKnown(t.Context(), dir, sumPath, map[string]KnownSum{
		"./data/disc01.squashfs.age": {Digest: "nonsense", Verify: true},
	}); err == nil || errors.As(err, &mm) {
		t.Errorf("WriteSumsKnown with a malformed digest to verify against = %v, want a refusal of the digest", err)
	}
}
