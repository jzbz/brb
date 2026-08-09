package restore

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sha512Hex(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha512.Sum512(b)
	return hex.EncodeToString(sum[:])
}

func TestDiscNumberOf(t *testing.T) {
	tests := []struct {
		name, suffix string
		want         int
		ok           bool
	}{
		{"disc01.squashfs.age", ".squashfs.age", 1, true},
		{"disc07.squashfs.age", ".squashfs.age", 7, true},
		{"disc100.squashfs.age", ".squashfs.age", 100, true},
		{"disc1.iso", ".iso", 1, true},
		{"disc00.iso", ".iso", 0, false},
		{"disc.iso", ".iso", 0, false},
		{"discXX.iso", ".iso", 0, false},
		{"disc01.iso", ".squashfs.age", 0, false},
		{"index.tsv.gz.age", ".squashfs.age", 0, false},
		{"disc 1.iso", ".iso", 0, false},
		{"disc-1.iso", ".iso", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name+tc.suffix, func(t *testing.T) {
			got, ok := discNumberOf(tc.name, tc.suffix)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("discNumberOf(%q, %q) = (%d, %v), want (%d, %v)",
					tc.name, tc.suffix, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestListNumberedSortsByDiscNumber(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"disc10.iso", "disc2.iso", "disc1.iso", "notes.txt", "disc03.iso"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listNumbered(dir, ".iso")
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, g := range got {
		nums = append(nums, g.N)
	}
	want := []int{1, 2, 3, 10}
	if len(nums) != len(want) {
		t.Fatalf("got %v, want %v", nums, want)
	}
	for i := range want {
		if nums[i] != want[i] {
			t.Fatalf("got %v, want %v", nums, want)
		}
	}
	// A missing directory is empty, not an error.
	got, err = listNumbered(filepath.Join(dir, "nope"), ".iso")
	if err != nil || len(got) != 0 {
		t.Fatalf("missing directory: got %v, %v", got, err)
	}
}

func TestFilterIndex(t *testing.T) {
	const index = "1\tDocuments/tax/2024.pdf\n" +
		"1\tPhotos/2024/IMG_0001.JPG\n" +
		"2\tPhotos/2025/img_0002.jpg\n" +
		"3\tsrc/main.go\n"

	tests := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			name:    "no pattern passes everything through",
			pattern: "",
			want: []string{
				"1\tDocuments/tax/2024.pdf",
				"1\tPhotos/2024/IMG_0001.JPG",
				"2\tPhotos/2025/img_0002.jpg",
				"3\tsrc/main.go",
			},
		},
		{
			name:    "substring match",
			pattern: "Photos/2024",
			want:    []string{"1\tPhotos/2024/IMG_0001.JPG"},
		},
		{
			name:    "case insensitive",
			pattern: "img_0002.JPG",
			want:    []string{"2\tPhotos/2025/img_0002.jpg"},
		},
		{
			name:    "matches the disc number column too",
			pattern: "3\t",
			want:    []string{"3\tsrc/main.go"},
		},
		{
			name:    "no match",
			pattern: "nothing here",
			want:    nil,
		},
		{
			name:    "a dot is a literal dot, not any character",
			pattern: "main.go",
			want:    []string{"3\tsrc/main.go"},
		},
		{
			name:    "regexp metacharacters are literal",
			pattern: "main.*go",
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			n, err := filterIndex(context.Background(), strings.NewReader(index), tc.pattern, &out, false)
			if err != nil {
				t.Fatalf("filterIndex: %v", err)
			}
			if n != len(tc.want) {
				t.Fatalf("matched %d line(s), want %d (%q)", n, len(tc.want), out.String())
			}
			var got []string
			if s := strings.TrimSuffix(out.String(), "\n"); s != "" {
				got = strings.Split(s, "\n")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestFilterIndexHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if _, err := filterIndex(ctx, strings.NewReader("1\ta\n"), "", &out, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("filterIndex = %v, want context.Canceled", err)
	}
}

// TestFilterIndexKeepsATrailingCR pins the split behaviour that once lost a
// byte of a real filename: the index stores a CR raw (escaping covers only
// backslash, tab and newline), so the reader must hand it back byte for byte —
// bufio's default ScanLines silently ate it, and a file named "report.pdf\r"
// became selectively unrestorable while reporting success.
func TestFilterIndexKeepsATrailingCR(t *testing.T) {
	const index = "1\treport.pdf\r\n1\tplain.txt\n"
	var out bytes.Buffer
	n, err := filterIndex(context.Background(), strings.NewReader(index), "", &out, false)
	if err != nil {
		t.Fatalf("filterIndex: %v", err)
	}
	if n != 2 {
		t.Fatalf("matched %d line(s), want 2", n)
	}
	if got := out.String(); got != index {
		t.Fatalf("filterIndex rewrote the records: %q, want %q", got, index)
	}
}

// TestFilterIndexEscapesForATerminal: on a tty a filename's control bytes are
// rendered, not executed — an archive name is attacker-chosen input.
func TestFilterIndexEscapesForATerminal(t *testing.T) {
	const index = "1\tESC\x1b]0;PWNED\x07inject.txt\n1\tCR\rname.txt\n"
	var out bytes.Buffer
	if _, err := filterIndex(context.Background(), strings.NewReader(index), "", &out, true); err != nil {
		t.Fatalf("filterIndex: %v", err)
	}
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("raw control bytes reached the terminal: %q", got)
	}
	for _, want := range []string{`1\tESC\x1b]0;PWNED\x07inject.txt`, `1\tCR\rname.txt`} {
		want = strings.ReplaceAll(want, `\t`, "\t")
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
}

func TestMapfileMissing(t *testing.T) {
	const complete = `# Mapfile. Created by GNU ddrescue version 1.27
# Command line: ddrescue -n /dev/sr0 out map
# current_pos  current_status  current_pass
0x00000000     +               1
#      pos        size  status
0x00000000  0x00100000  +
`
	const damaged = `# Mapfile. Created by GNU ddrescue version 1.27
# current_pos  current_status
0x00001000     ?
#      pos        size  status
0x00000000  0x00001000  +
0x00001000  0x00000200  -
0x00001200  0x00000400  /
0x00001600  0x00000100  *
0x00001700  0x00000100  ?
0x00001800  0x00010000  +
`
	tests := []struct {
		name  string
		text  string
		write bool
		want  int64
		ok    bool
	}{
		{name: "no map file at all", write: false, want: 0, ok: false},
		{name: "everything recovered", text: complete, write: true, want: 0, ok: true},
		{name: "gaps of every kind are counted", text: damaged, write: true, want: 0x200 + 0x400 + 0x100 + 0x100, ok: true},
		{name: "only comments", text: "# nothing here\n", write: true, want: 0, ok: false},
		{name: "empty file", text: "", write: true, want: 0, ok: false},
		{
			name:  "decimal sizes are accepted too",
			text:  "0\t4096\t+\n4096\t512\t-\n",
			write: true,
			want:  512,
			ok:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.mapfile")
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := mapfileMissing(path)
			if err != nil {
				t.Fatalf("mapfileMissing: %v", err)
			}
			if got != tc.want || ok != tc.ok {
				t.Fatalf("mapfileMissing = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAssessSalvage(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	out := filepath.Join(dir, "out")
	mapfile := filepath.Join(dir, "out.mapfile")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("complete copy with no map file", func(t *testing.T) {
		if err := os.WriteFile(out, bytes.Repeat([]byte("x"), 1000), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := assessSalvage(src, out, mapfile)
		if err != nil || got != 0 {
			t.Fatalf("assessSalvage = (%d, %v), want (0, nil)", got, err)
		}
	})

	t.Run("short output is missing bytes", func(t *testing.T) {
		if err := os.WriteFile(out, bytes.Repeat([]byte("x"), 600), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := assessSalvage(src, out, mapfile)
		if err != nil || got != 400 {
			t.Fatalf("assessSalvage = (%d, %v), want (400, nil)", got, err)
		}
	})

	t.Run("full length but the map file reports gaps", func(t *testing.T) {
		if err := os.WriteFile(out, bytes.Repeat([]byte("x"), 1000), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mapfile, []byte("0\t900\t+\n900\t100\t-\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := assessSalvage(src, out, mapfile)
		if err != nil || got != 100 {
			t.Fatalf("assessSalvage = (%d, %v), want (100, nil)", got, err)
		}
	})

	t.Run("no output at all is an error", func(t *testing.T) {
		if _, err := assessSalvage(src, filepath.Join(dir, "missing"), mapfile); err == nil {
			t.Fatal("expected an error when ddrescue produced nothing")
		}
	})
}

func TestCopyStream(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	body := bytes.Repeat([]byte("blu-ray\n"), 100000)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var prog bytes.Buffer
	sum, err := copyStream(context.Background(), src, dst, &prog)
	if err != nil {
		t.Fatalf("copyStream: %v", err)
	}
	if sum != sha512Hex(t, body) {
		t.Fatal("copyStream returned the wrong hash")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("copy differs from the source")
	}
	if prog.Len() != len(body) {
		t.Fatalf("progress saw %d bytes, want %d", prog.Len(), len(body))
	}
	if _, err := os.Stat(dst + partExt); err == nil {
		t.Fatal("a .part file was left behind")
	}
}

func TestCopyStreamReportsAReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory opens but cannot be read as a stream, which is the closest a
	// test can get to a drive that fails mid-file.
	src := filepath.Join(dir, "adirectory")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	_, err := copyStream(context.Background(), src, dst, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var re *readError
	if !errors.As(err, &re) {
		t.Fatalf("error is not a *readError: %v", err)
	}
	for _, p := range []string{dst, dst + partExt} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was left behind after a failed copy", p)
		}
	}
}

func TestCopyStreamHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyStream(ctx, src, dst, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyStream = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dst + partExt); err == nil {
		t.Fatal("a .part file survived cancellation")
	}
}

// TestDdrescueSalvageIsJudgedByTheResultNotTheExitStatus is the counterpart to
// brb.sh's copy_file_robustly, which returns success unconditionally once
// ddrescue has run. ddrescue exits non-zero in cases where it recovered
// everything and exits zero in cases where it did not, so the map file and the
// output size are the only honest evidence.
func TestDdrescueSalvageIsJudgedByTheResultNotTheExitStatus(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build a ddrescue stand-in with")
	}

	tests := []struct {
		name string
		// salvaged is how many of the source's 1000 bytes the stand-in writes.
		salvaged int
		// mapfile is what the stand-in records.
		mapfile string
		// exit is the stand-in's exit status.
		exit int
		// want is the number of bytes ddrescueInto must report as missing.
		want int64
		// wantMapfile says whether the map file must be kept for a later pass.
		wantMapfile bool
	}{
		{
			name:     "complete salvage, zero exit",
			salvaged: 1000,
			mapfile:  "0\t1000\t+\n",
			exit:     0,
			want:     0,
		},
		{
			name:     "complete salvage but a non-zero exit is still complete",
			salvaged: 1000,
			mapfile:  "0\t1000\t+\n",
			exit:     1,
			want:     0,
		},
		{
			name:        "zero exit with unread regions is still incomplete",
			salvaged:    1000,
			mapfile:     "0\t900\t+\n900\t100\t-\n",
			exit:        0,
			want:        100,
			wantMapfile: true,
		},
		{
			name:        "short output is incomplete even with an empty map",
			salvaged:    600,
			mapfile:     "0\t600\t+\n",
			exit:        0,
			want:        400,
			wantMapfile: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 1000), 0o600); err != nil {
				t.Fatal(err)
			}
			partial := filepath.Join(dir, "partial")
			if err := os.WriteFile(partial, bytes.Repeat([]byte("x"), tc.salvaged), 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "dst")
			out := dst + partExt
			mapfile := dst + mapfileExt

			bin := filepath.Join(t.TempDir(), "ddrescue")
			script := "#!/bin/sh\ncp '" + partial + "' '" + out + "'\n" +
				"printf '%s' '" + tc.mapfile + "' > '" + mapfile + "'\n" +
				"exit " + strconv.Itoa(tc.exit) + "\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			missing, err := e.opts().ddrescueInto(context.Background(), bin, src, out, mapfile, dst)
			if err != nil {
				t.Fatalf("ddrescueInto: %v\n%s", err, e.log())
			}
			if missing != tc.want {
				t.Fatalf("missing = %d, want %d\n%s", missing, tc.want, e.log())
			}
			st, err := os.Stat(dst)
			if err != nil {
				t.Fatalf("the salvaged data must be kept for par2: %v", err)
			}
			if st.Size() != int64(tc.salvaged) {
				t.Fatalf("salvaged file is %d bytes, want %d", st.Size(), tc.salvaged)
			}
			_, mapErr := os.Stat(mapfile)
			if tc.wantMapfile && mapErr != nil {
				t.Fatal("the map file must be kept so another copy of the disc can fill the gaps")
			}
			if !tc.wantMapfile && mapErr == nil {
				t.Fatal("a complete salvage must not leave a map file behind")
			}
		})
	}
}

func TestParseProcMounts(t *testing.T) {
	const table = `proc /proc proc rw,nosuid 0 0
/dev/sda1 / ext4 rw,relatime 0 0
/dev/sr0 /run/media/jz/BACKUP_01_OF_03 iso9660 ro,nosuid 0 0
/dev/sdb1 /mnt/with\040space vfat rw 0 0
`
	tests := []struct {
		dev, want string
	}{
		{"/dev/sr0", "/run/media/jz/BACKUP_01_OF_03"},
		{"/dev/sda1", "/"},
		{"/dev/sdb1", "/mnt/with space"},
		{"/dev/sr1", ""},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.dev, func(t *testing.T) {
			if got := parseProcMounts(table, tc.dev); got != tc.want {
				t.Fatalf("parseProcMounts(%q) = %q, want %q", tc.dev, got, tc.want)
			}
		})
	}
}

func TestParseUdisksMount(t *testing.T) {
	tests := []struct {
		name, out, want string
	}{
		{"modern", "Mounted /dev/sr0 at /run/media/jz/DISC\n", "/run/media/jz/DISC"},
		{"with a full stop", "Mounted /dev/sr0 at /media/DISC.\n", "/media/DISC"},
		{"noise first", "warning\nMounted /dev/sr0 at /media/x\n", "/media/x"},
		{"already mounted error", "Error mounting /dev/sr0: already mounted\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUdisksMount(tc.out); got != tc.want {
				t.Fatalf("parseUdisksMount(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

func TestCopyProblemIsAnIncompleteCopy(t *testing.T) {
	err := error(&CopyProblem{Name: "disc01.squashfs.age", Missing: 2048, Reason: "ddrescue could not read the whole file"})
	if !errors.Is(err, ErrIncompleteCopy) {
		t.Fatal("a CopyProblem must satisfy errors.Is(err, ErrIncompleteCopy)")
	}
	if !strings.Contains(err.Error(), "2048") || !strings.Contains(err.Error(), "par2") {
		t.Fatalf("the message must say how much is missing and what to do: %v", err)
	}
	quiet := error(&CopyProblem{Name: "x", Missing: -1, Reason: "hash mismatch"})
	if strings.Contains(quiet.Error(), "-1") {
		t.Fatalf("an unknown shortfall must not be printed as a byte count: %v", quiet)
	}
}

func TestRecordedSum(t *testing.T) {
	dir := t.TempDir()
	digest := strings.Repeat("ab", 64)
	path := filepath.Join(dir, "disc01.squashfs.age.sha512")
	if err := os.WriteFile(path, []byte(digest+"  disc01.squashfs.age\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := recordedSum(path, "disc01.squashfs.age", "")
	if err != nil || !ok || got != digest {
		t.Fatalf("recordedSum = (%q, %v, %v)", got, ok, err)
	}
	// A file that says nothing about the name we asked for.
	if _, ok, err := recordedSum(path, "disc02.squashfs.age", ""); err != nil || ok {
		t.Fatalf("recordedSum for another name = (%v, %v), want (false, nil)", ok, err)
	}
	// A missing file is not an error.
	if _, ok, err := recordedSum(filepath.Join(dir, "nope"), "x", ""); err != nil || ok {
		t.Fatalf("recordedSum for a missing file = (%v, %v), want (false, nil)", ok, err)
	}
	// A malformed one is.
	bad := filepath.Join(dir, "bad.sha512")
	if err := os.WriteFile(bad, []byte("not a checksum line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = recordedSum(bad, "x", "")
	if err == nil {
		t.Fatal("a malformed checksum file must be an error, not a silently skipped check")
	}
	// And it must be distinguishable, so that the one caller with a better
	// authority to appeal to — par2 over the ciphertext — can go and ask it.
	if !errors.Is(err, errSidecarUnreadable) {
		t.Fatalf("error = %v, want it to wrap errSidecarUnreadable", err)
	}
}

func TestStepWriterSplitsLines(t *testing.T) {
	e := newEnv(t)
	w := e.opts().logWriter()
	if _, err := w.Write([]byte("first line\nsecond ")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("line\n\n  \nthird")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := e.log()
	for _, want := range []string{"first line", "second line", "third"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log is missing %q:\n%s", want, log)
		}
	}
	if strings.Count(log, "   .") != 3 {
		t.Fatalf("expected exactly three step lines:\n%s", log)
	}
}

// TestPassphraseErrorDistinguishesEmptyFromNoTerminal covers a message that
// used to be actively misleading: an empty passphrase — Enter pressed at a
// perfectly good prompt — was reported as "there is no terminal to ask on ...
// cannot be automated", which is not what happened and not what to do about it.
// Both still fail closed; only the diagnosis differs.
func TestPassphraseErrorDistinguishesEmptyFromNoTerminal(t *testing.T) {
	const path = "/keys/rescue-identity.txt.age"

	empty := passphraseError(path, ErrEmptyPassphrase).Error()
	if !strings.Contains(empty, "passphrase cannot be empty") {
		t.Errorf("empty passphrase error does not say so: %s", empty)
	}
	if strings.Contains(empty, "no terminal") || strings.Contains(empty, "cannot be automated") {
		t.Errorf("empty passphrase still blames a missing terminal: %s", empty)
	}

	noTTY := passphraseError(path, fmt.Errorf("%w: /dev/tty: no such device", ErrNoTerminal)).Error()
	if !strings.Contains(noTTY, "no terminal to ask on") || !strings.Contains(noTTY, "cannot be automated") {
		t.Errorf("missing-terminal error lost its diagnosis: %s", noTTY)
	}
	if strings.Contains(noTTY, "cannot be empty") {
		t.Errorf("missing-terminal error blames an empty passphrase: %s", noTTY)
	}

	other := passphraseError(path, errors.New("read: interrupted")).Error()
	if !strings.Contains(other, "could not be read") {
		t.Errorf("unclassified failure lost its wording: %s", other)
	}

	for _, s := range []string{empty, noTTY, other} {
		if !strings.HasPrefix(s, "restore: ") || !strings.Contains(s, path) {
			t.Errorf("error does not name the command and the file: %s", s)
		}
	}
}
