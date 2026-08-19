package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSet builds a Set where the named tools are present and everything else is
// missing, without touching PATH.
func fakeSet(present ...string) *Set {
	var ts []Tool
	for _, n := range Known() {
		t := Tool{Name: n}
		if slices.Contains(present, n) {
			t.Found, t.Path = true, "/usr/bin/"+n
		}
		ts = append(ts, t)
	}
	return NewSet(ts)
}

func TestRequireReportsEveryMissingTool(t *testing.T) {
	tests := []struct {
		name        string
		present     []string
		require     []string
		wantMissing []string
	}{
		{
			name:    "nothing missing",
			present: []string{Mksquashfs, Unsquashfs, Par2, Xorriso},
			require: []string{Mksquashfs, Unsquashfs, Par2, Xorriso},
		},
		{
			name:        "one missing",
			present:     []string{Mksquashfs, Unsquashfs, Xorriso},
			require:     []string{Mksquashfs, Unsquashfs, Par2, Xorriso},
			wantMissing: []string{Par2},
		},
		{
			name:        "three missing, all reported at once",
			present:     []string{Unsquashfs},
			require:     []string{Mksquashfs, Unsquashfs, Par2, Xorriso},
			wantMissing: []string{Mksquashfs, Par2, Xorriso},
		},
		{
			name:        "everything missing",
			present:     nil,
			require:     []string{Mksquashfs, Par2},
			wantMissing: []string{Mksquashfs, Par2},
		},
		{
			name:    "requiring nothing succeeds",
			present: nil,
			require: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fakeSet(tc.present...)
			err := s.Require(tc.require...)
			if len(tc.wantMissing) == 0 {
				if err != nil {
					t.Fatalf("Require() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Require() = nil, want an error naming %v", tc.wantMissing)
			}
			if !errors.Is(err, ErrMissing) {
				t.Errorf("Require() error does not match ErrMissing: %v", err)
			}
			var me *MissingError
			if !errors.As(err, &me) {
				t.Fatalf("Require() error is not a *MissingError: %v", err)
			}
			if !slices.Equal(me.Names, tc.wantMissing) {
				t.Errorf("missing names = %v, want %v", me.Names, tc.wantMissing)
			}
			// The single-line message must name every missing tool, so an
			// operator can install them all in one go rather than one per run.
			msg := err.Error()
			for _, n := range tc.wantMissing {
				if !strings.Contains(msg, n) {
					t.Errorf("error message %q does not name %q", msg, n)
				}
			}
			wantPlural := len(tc.wantMissing) > 1
			gotPlural := strings.Contains(msg, "missing required tools:")
			if gotPlural != wantPlural {
				t.Errorf("error message %q: plural = %v, want %v", msg, gotPlural, wantPlural)
			}
		})
	}
}

func TestMissingErrorNamesThePackage(t *testing.T) {
	err := &MissingError{Names: []string{Par2}}
	if !strings.Contains(err.Error(), "par2cmdline") {
		t.Errorf("missing-tool error should hint at the package: %q", err.Error())
	}
}

func TestGetHasAll(t *testing.T) {
	s := fakeSet(Mksquashfs, Par2)

	if !s.Has(Mksquashfs) {
		t.Error("Has(mksquashfs) = false, want true")
	}
	if s.Has(Xorriso) {
		t.Error("Has(xorriso) = true, want false")
	}
	if got := s.Get(Mksquashfs); got.Path != "/usr/bin/mksquashfs" || !got.Found {
		t.Errorf("Get(mksquashfs) = %+v", got)
	}
	if got := s.Get("nosuchtool"); got.Found || got.Name != "nosuchtool" {
		t.Errorf("Get(nosuchtool) = %+v, want a zero Tool named nosuchtool", got)
	}

	all := s.All()
	if len(all) != len(Known()) {
		t.Fatalf("All() returned %d tools, want %d", len(all), len(Known()))
	}
	for i, n := range Known() {
		if all[i].Name != n {
			t.Errorf("All()[%d].Name = %q, want %q", i, all[i].Name, n)
		}
	}
}

func TestRequireOnAMissingToolFromARunner(t *testing.T) {
	// Every runner must refuse cleanly when its binary is absent rather than
	// trying to exec "".
	s := fakeSet()
	ctx := context.Background()

	checks := []struct {
		name string
		err  error
	}{
		{"BuildImage", s.BuildImage(ctx, MkOptions{SourceDir: "/tmp", Out: "/tmp/x.sq", Files: []string{"a"}})},
		{"Par2Create", s.Par2Create(ctx, Par2Options{Dir: "/tmp", File: "x"})},
		{"Par2Verify", s.Par2Verify(ctx, "/tmp", "x.par2", nil)},
		{"Par2Repair", s.Par2Repair(ctx, "/tmp", "x.par2", nil)},
		{"MakeISO", s.MakeISO(ctx, ISOOptions{Dir: "/tmp", Out: "/tmp/x.iso"})},
		{"ProbeISO", s.ProbeISO(ctx)},
		{"BurnISO", s.BurnISO(ctx, "/dev/sr0", "/tmp/x.iso", 4, nil)},
		{"Unsquashfs", s.Unsquashfs(ctx, UnsqOptions{Image: "/tmp/x.sq", Dest: "/tmp/d"})},
		{"UnsquashfsList", s.UnsquashfsList(ctx, "/tmp/x.sq", &strings.Builder{})},
	}
	for _, c := range checks {
		if !errors.Is(c.err, ErrMissing) {
			t.Errorf("%s with no binary: err = %v, want ErrMissing", c.name, c.err)
		}
	}
	if _, err := s.ImageStats(ctx, "/tmp/x.sq"); !errors.Is(err, ErrMissing) {
		t.Errorf("ImageStats with no binary: err = %v, want ErrMissing", err)
	}
}

func TestCapabilityProbesWithoutMksquashfs(t *testing.T) {
	s := fakeSet()
	ctx := context.Background()
	if s.MksquashfsHasCpioStyle0(ctx) {
		t.Error("MksquashfsHasCpioStyle0 = true with no mksquashfs")
	}
	if got := s.MksquashfsCompressors(ctx); got != nil {
		t.Errorf("MksquashfsCompressors = %v, want nil with no mksquashfs", got)
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"\n\n", ""},
		{"mksquashfs version 4.7.5 (2026/03/01)\ncopyright...", "mksquashfs version 4.7.5 (2026/03/01)"},
		{"\n  1.3.1  \nrest", "1.3.1"},
	}
	for _, tc := range tests {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLineContaining(t *testing.T) {
	pick := lineContaining("xorriso version")
	out := "xorriso 1.5.8.pl02 : RockRidge filesystem manipulator\n\nxorriso version   :  1.5.8.pl02\nVersion timestamp :  2026.05.22\n"
	if got, want := pick(out), "xorriso version   :  1.5.8.pl02"; got != want {
		t.Errorf("pick = %q, want %q", got, want)
	}
	if got, want := pick("something else\n"), "something else"; got != want {
		t.Errorf("fallback pick = %q, want %q", got, want)
	}
	if got := pick(""); got != "" {
		t.Errorf("pick(\"\") = %q, want \"\"", got)
	}
}

func TestDetectNeverFails(t *testing.T) {
	s := Detect(context.Background())
	if s == nil {
		t.Fatal("Detect returned nil")
	}
	if len(s.All()) != len(Known()) {
		t.Errorf("Detect found %d entries, want %d", len(s.All()), len(Known()))
	}
	for _, tool := range s.All() {
		if tool.Found && tool.Path == "" {
			t.Errorf("%s reported found with no path", tool.Name)
		}
		if !tool.Found && tool.Path != "" {
			t.Errorf("%s reported missing but has path %q", tool.Name, tool.Path)
		}
	}
}

// TestRemovePar2CleansUpBothNamingForms proves the cleanup that runs when a
// par2 create fails knows both shapes of name it can be given: the file that
// was protected, and a set named for itself. Getting the second one wrong would
// leave a half-written sidecars.par2 behind while hunting for the
// "sidecars.par2.par2" that never existed.
func TestRemovePar2CleansUpBothNamingForms(t *testing.T) {
	tests := []struct {
		name   string
		arg    string
		remove []string
	}{
		{
			name:   "a protected file",
			arg:    "disc01.squashfs.age",
			remove: []string{"disc01.squashfs.age.par2", "disc01.squashfs.age.vol000+30.par2"},
		},
		{
			name:   "a set named for itself",
			arg:    "sidecars.par2",
			remove: []string{"sidecars.par2", "sidecars.vol000+04.par2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			keep := []string{"disc01.squashfs.age", "disc01.squashfs.sha512", "index.tsv.gz.age"}
			for _, n := range append(append([]string{}, keep...), tc.remove...) {
				if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			removePar2(dir, tc.arg)
			for _, n := range tc.remove {
				if _, err := os.Stat(filepath.Join(dir, n)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s survived removePar2: %v", n, err)
				}
			}
			for _, n := range keep {
				if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
					t.Errorf("removePar2 deleted %s, which it does not own: %v", n, err)
				}
			}
		})
	}
}

// TestRemovePar2InADirectoryNamedLikeAGlob is the regression for the bug that
// motivated Par2VolumeNames: the directory used to be pasted into a
// filepath.Glob pattern, so a staging path containing '[' — here, literally
// "a[1]" — matched nothing and a failed create left its half-written volumes
// behind for the next run to trip over.
func TestRemovePar2InADirectoryNamedLikeAGlob(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a[1]")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	remove := []string{"disc01.squashfs.age.par2", "disc01.squashfs.age.vol000+30.par2"}
	keep := []string{"disc01.squashfs.age", "disc01.squashfs.age.sha512", "disc02.squashfs.age.par2"}
	for _, n := range append(append([]string{}, keep...), remove...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := Par2VolumeNames(dir, "disc01.squashfs.age"); !slices.Equal(got, remove) {
		t.Fatalf("Par2VolumeNames = %v, want %v", got, remove)
	}
	removePar2(dir, "disc01.squashfs.age")
	for _, n := range remove {
		if _, err := os.Stat(filepath.Join(dir, n)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived removePar2 in a directory named like a glob: %v", n, err)
		}
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("removePar2 deleted %s, which it does not own: %v", n, err)
		}
	}
}

// TestMksquashfsReadFailure pins the recogniser for the one mksquashfs message
// that means "I backed this file up as nothing and exited 0". The exact text
// is the one mksquashfs 4.7 prints; the partial shapes guard against a reword.
func TestMksquashfsReadFailure(t *testing.T) {
	tests := []struct {
		line string
		file string
		ok   bool
	}{
		{"Failed to read file locked, creating empty file", "locked", true},
		{"Failed to read file sub/dir/secret.txt, creating empty file\n", "sub/dir/secret.txt", true},
		{"Failed to read file odd, name.txt, creating empty file", "odd, name.txt", true},
		{"Failed to read file x", "x", true},
		{"file y, creating empty file", "file y", true},
		{"Parallel mksquashfs: Using 32 processors", "", false},
		{"Creating 4.0 filesystem on out.sqfs, block size 131072.", "", false},
		{"Number of files 2", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		file, ok := MksquashfsReadFailure(tc.line)
		if ok != tc.ok || file != tc.file {
			t.Errorf("MksquashfsReadFailure(%q) = (%q, %v), want (%q, %v)", tc.line, file, ok, tc.file, tc.ok)
		}
	}
}

// TestReadFailureWatchSplitsLinesItself feeds the watcher the way a writer that
// batches several lines per Write would, and one that dribbles a line across
// several Writes: it must find every failure either way, and forward the bytes
// untouched to the caller's log.
func TestReadFailureWatchSplitsLinesItself(t *testing.T) {
	var log strings.Builder
	w := &readFailureWatch{out: &log}
	chunks := []string{
		"Parallel mksquashfs: Using 4 processors\nFailed to read file a, creating empty file\nCreating 4.0",
		" filesystem\nFailed to read file dir/b, crea",
		"ting empty file\n",
		"Failed to read file c, creating empty file", // no trailing newline
	}
	for _, c := range chunks {
		if _, err := w.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	if got := w.failed(); !slices.Equal(got, []string{"a", "dir/b", "c"}) {
		t.Fatalf("failed = %v, want [a dir/b c]", got)
	}
	if got := log.String(); got != strings.Join(chunks, "") {
		t.Fatalf("log was altered:\n%q\nwant\n%q", got, strings.Join(chunks, ""))
	}
	if err := readFailureError(w.failed()); !strings.Contains(err.Error(), "EMPTY files") ||
		!strings.Contains(err.Error(), "dir/b") {
		t.Fatalf("readFailureError = %v, want it to name the files and the consequence", err)
	}
}
