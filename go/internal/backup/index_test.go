package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/indexfmt"
)

func TestWriteIndexLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		disc  int
		paths []string
		want  string
	}{
		{name: "empty", disc: 1, paths: nil, want: ""},
		{name: "one", disc: 1, paths: []string{"a.txt"}, want: "1\ta.txt\n"},
		{
			name: "several", disc: 7,
			paths: []string{"a/b.txt", "c/d.bin"},
			want:  "7\ta/b.txt\n7\tc/d.bin\n",
		},
		{
			// The disc number is not zero-padded in the index, unlike the file
			// names. Both readers depend on it: brb.sh splits these lines with
			// `awk -F'\t'` and compares field 1 numerically (brb.sh:1392), and
			// the recipe printed in the on-disc README does the same.
			name: "disc numbers are not padded", disc: 3,
			paths: []string{"x"}, want: "3\tx\n",
		},
		{
			name: "spaces and unicode are literal", disc: 12,
			paths: []string{"My Documents/rés umé.pdf"},
			want:  "12\tMy Documents/rés umé.pdf\n",
		},
		{
			// The escaping is what keeps one file to one row. brb.sh substitutes
			// backslash, then tab, then newline, in that order.
			name: "tab in a path is escaped", disc: 2,
			paths: []string{"od\td"}, want: "2\tod\\td\n",
		},
		{
			name: "newline in a path is escaped", disc: 1,
			paths: []string{"new\nline.txt"}, want: "1\tnew\\nline.txt\n",
		},
		{
			name: "backslash in a path is escaped", disc: 1,
			paths: []string{`back\slash.txt`}, want: "1\tback\\\\slash.txt\n",
		},
		{
			// Escaping the backslash last would render this as `\\t`, which reads
			// back as a backslash followed by 't' rather than as a tab.
			name: "backslash before a tab", disc: 1,
			paths: []string{"a\\\tb"}, want: "1\ta\\\\\\tb\n",
		},
		{
			// The sharpest case: raw, this filename asserts a file on disc 9.
			name: "a filename cannot forge a row", disc: 1,
			paths: []string{"evil\n9\tphantom.txt"},
			want:  "1\tevil\\n9\\tphantom.txt\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := writeIndexLines(&buf, tc.disc, tc.paths); err != nil {
				t.Fatalf("writeIndexLines: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("index = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppendIndexAccumulatesAcrossDiscs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.tsv")
	if err := appendIndex(path, 1, []string{"a", "b"}); err != nil {
		t.Fatalf("appendIndex 1: %v", err)
	}
	if err := appendIndex(path, 2, []string{"c"}); err != nil {
		t.Fatalf("appendIndex 2: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\ta\n1\tb\n2\tc\n"
	if string(data) != want {
		t.Errorf("index = %q, want %q", data, want)
	}

	sum, err := readIndexSummary(context.Background(), path)
	if err != nil {
		t.Fatalf("readIndexSummary: %v", err)
	}
	if sum.Lines != 3 || sum.MaxDisc != 2 {
		t.Errorf("summary = %+v, want {Lines:3 MaxDisc:2}", sum)
	}
}

func TestReadIndexSummary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		wantLines int
		wantMax   int
		wantTrunc bool
		wantErr   string
	}{
		{name: "empty file"},
		{name: "one line", content: "1\tx\n", wantLines: 1, wantMax: 1},
		{
			// The tail of a record the interrupted run was still writing is not
			// a record yet, so it is reported rather than counted.
			name: "no trailing newline", content: "1\tx\n2\ty",
			wantLines: 1, wantMax: 1, wantTrunc: true,
		},
		{
			name: "escaped paths parse", content: "1\ta\\tb.txt\n2\tc\\nd.txt\n1\te\\\\f.txt\n",
			wantLines: 3, wantMax: 2,
		},
		{name: "out of order", content: "9\ta\n2\tb\n", wantLines: 2, wantMax: 9},
		{name: "no tab", content: "1 x\n", wantErr: "no tab"},
		{name: "bad number", content: "one\tx\n", wantErr: "bad disc number"},
		{name: "zero disc", content: "0\tx\n", wantErr: "bad disc number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "index.tsv")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			sum, err := readIndexSummary(context.Background(), path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("readIndexSummary succeeded, want an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readIndexSummary: %v", err)
			}
			if sum.Lines != tc.wantLines || sum.MaxDisc != tc.wantMax || sum.Truncated != tc.wantTrunc {
				t.Errorf("summary = %+v, want {Lines:%d MaxDisc:%d Truncated:%v}",
					sum, tc.wantLines, tc.wantMax, tc.wantTrunc)
			}
		})
	}
}

func TestReadIndexSummaryMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := readIndexSummary(context.Background(), filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("readIndexSummary of a missing file succeeded, want an error")
	}
}

func TestReconcileIndex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		maxDisc   int
		want      string // the file contents afterwards
		wantLines int
		wantMax   int
		wantWarn  bool
		wantErr   string
	}{
		{
			name: "already consistent", content: "1\ta\n1\tb\n2\tc\n", maxDisc: 2,
			want: "1\ta\n1\tb\n2\tc\n", wantLines: 3, wantMax: 2,
		},
		{
			// IDX-4: Go's own resume used to call these lines damage and delete
			// them. Escaped, they are ordinary records and must survive untouched.
			name:      "escaped awkward paths survive",
			content:   "1\ta\\tb.txt\n1\tc\\nd.txt\n1\te\\\\f.txt\n1\tevil\\n9\\tphantom.txt\n",
			maxDisc:   1,
			want:      "1\ta\\tb.txt\n1\tc\\nd.txt\n1\te\\\\f.txt\n1\tevil\\n9\\tphantom.txt\n",
			wantLines: 4, wantMax: 1,
		},
		{
			// A record that genuinely does not parse is corruption, and the index
			// is the map of what is on which disc: report it, never delete it.
			name:    "an unparseable record fails and is not dropped",
			content: "1\ta\n1\tb\nline.txt\n1\tc\n", maxDisc: 1,
			want: "1\ta\n1\tb\nline.txt\n1\tc\n", wantErr: "damaged",
		},
		{
			name:    "a bad disc number fails and is not dropped",
			content: "1\ta\n0\tb\n", maxDisc: 1,
			want: "1\ta\n0\tb\n", wantErr: "damaged",
		},
		{
			// The window between appending a disc's records and saving the
			// state that says the disc is done.
			name: "one disc ahead of the state", content: "1\ta\n2\tb\n", maxDisc: 1,
			want: "1\ta\n", wantLines: 1, wantMax: 1, wantWarn: true,
		},
		{
			name: "half-written trailing line", content: "1\ta\n1\tb", maxDisc: 1,
			want: "1\ta\n", wantLines: 1, wantMax: 1, wantWarn: true,
		},
		{
			name: "trailing line with no tab", content: "1\ta\n2", maxDisc: 1,
			want: "1\ta\n", wantLines: 1, wantMax: 1, wantWarn: true,
		},
		{
			name: "everything dropped", content: "3\ta\n", maxDisc: 1,
			want: "", wantLines: 0, wantMax: 0, wantWarn: true,
		},
		{
			name: "empty file stays empty", content: "", maxDisc: 0,
			want: "", wantLines: 0, wantMax: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "index.tsv")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			warned := 0
			sum, err := reconcileIndex(context.Background(), path, tc.maxDisc,
				func(string, ...any) { warned++ })
			switch {
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("reconcileIndex succeeded, want an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				got, rerr := os.ReadFile(path)
				if rerr != nil {
					t.Fatal(rerr)
				}
				if string(got) != tc.want {
					t.Errorf("a failed reconcile changed the index to %q, want %q left alone", got, tc.want)
				}
				if _, serr := os.Stat(path + ".part"); !os.IsNotExist(serr) {
					t.Error("the .part file was left behind")
				}
				return
			case err != nil:
				t.Fatalf("reconcileIndex: %v", err)
			}
			if sum.Lines != tc.wantLines || sum.MaxDisc != tc.wantMax {
				t.Errorf("summary = %+v, want {Lines:%d MaxDisc:%d}", sum, tc.wantLines, tc.wantMax)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("index = %q, want %q", got, tc.want)
			}
			if (warned > 0) != tc.wantWarn {
				t.Errorf("warned %d time(s), wantWarn = %v", warned, tc.wantWarn)
			}
			if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
				t.Error("the .part file was left behind")
			}
		})
	}
}

func TestReconcileIndexKeepsAppending(t *testing.T) {
	t.Parallel()
	// After a repair the file must still be a valid target for appendIndex.
	path := filepath.Join(t.TempDir(), "index.tsv")
	if err := os.WriteFile(path, []byte("1\ta\n2\tb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileIndex(context.Background(), path, 1, func(string, ...any) {}); err != nil {
		t.Fatalf("reconcileIndex: %v", err)
	}
	if err := appendIndex(path, 2, []string{"b", "c"}); err != nil {
		t.Fatalf("appendIndex: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1\ta\n2\tb\n2\tc\n"; string(got) != want {
		t.Errorf("index = %q, want %q", got, want)
	}
}

// TestWrittenIndexIsOneRowPerFile is IDX-1 and IDX-3 in miniature: whatever the
// filenames are, the written index has one line per file, every line has exactly
// two tab-separated fields, and every original path comes back out of it.
func TestWrittenIndexIsOneRowPerFile(t *testing.T) {
	t.Parallel()
	paths := []string{
		"plain.txt",
		"a\tb.txt",
		"c\nd.txt",
		`e\f.txt`,
		"sub/mix\t\\and\nnl.txt",
		"evil\n9\tphantom.txt",
		"rés umé.pdf",
	}
	var buf bytes.Buffer
	if err := writeIndexLines(&buf, 1, paths); err != nil {
		t.Fatalf("writeIndexLines: %v", err)
	}
	body := buf.String()
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("index does not end with a newline: %q", body)
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != len(paths) {
		t.Fatalf("index has %d line(s) for %d file(s): %q", len(lines), len(paths), body)
	}
	for i, line := range lines {
		if fields := strings.Split(line, "\t"); len(fields) != 2 {
			t.Errorf("line %d has %d tab-separated field(s), want 2: %q", i+1, len(fields), line)
		}
		disc, got, err := indexfmt.ParseLine(line)
		if err != nil {
			t.Fatalf("line %d does not parse: %v", i+1, err)
		}
		// IDX-2: no filename may name a disc outside the set.
		if disc != 1 {
			t.Errorf("line %d names disc %d, but the set has one disc: %q", i+1, disc, line)
		}
		if got != paths[i] {
			t.Errorf("line %d round-tripped to %q, want %q", i+1, got, paths[i])
		}
	}
}

func TestGzipFileRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "index.tsv")
	body := "1\ta\n1\tb\n2\tc\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "index.tsv.gz")
	if err := gzipFile(context.Background(), src, dst); err != nil {
		t.Fatalf("gzipFile: %v", err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("the output is not gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("decompressed %q, want %q", got, body)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Error("the .part file was left behind")
	}
}

func TestGzipFileRemovesThePartialOnCancellation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "index.tsv")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dst := filepath.Join(dir, "index.tsv.gz")
	if err := gzipFile(ctx, src, dst); err == nil {
		t.Fatal("gzipFile succeeded with a cancelled context")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a cancelled gzip left the destination behind")
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Error("a cancelled gzip left the .part file behind")
	}
}
