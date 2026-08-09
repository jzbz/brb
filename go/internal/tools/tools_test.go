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
