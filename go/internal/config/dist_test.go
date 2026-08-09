package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkdirs creates directories under a temporary root and returns the root.
func mkdirs(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveDistDirPrecedence(t *testing.T) {
	// Every candidate exists, so each case can only be picked for the right
	// reason: explicit beats the executable's neighbour, which beats
	// /usr/local, which beats /usr/share.
	root := mkdirs(t, "explicit", "exe/dist", "usr-local", "usr-share")
	at := func(n string) string { return filepath.Join(root, n) }
	system := []string{at("usr-local"), at("usr-share")}

	tests := []struct {
		name     string
		explicit string
		exeDir   string
		system   []string
		want     string
	}{
		{"DIST_DIR wins", at("explicit"), at("exe"), system, at("explicit")},
		{"then dist beside the program", "", at("exe"), system, at("exe/dist")},
		{"then /usr/local/share/brb", "", at("nowhere"), system, at("usr-local")},
		{"then /usr/share/brb", "", at("nowhere"), system[1:], at("usr-share")},
		{"nothing found is not an error", "", at("nowhere"), nil, ""},
		{"no executable path just drops that candidate", "", "", system, at("usr-local")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDistDir(tt.explicit, tt.exeDir, tt.system)
			if err != nil {
				t.Fatalf("resolveDistDir: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveDistDir = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveDistDirExplicitMissing is the case worth being loud about. An
// operator who set DIST_DIR meant it; silently falling through to /usr/share or
// to no payload at all would only be discovered after twenty discs were burned.
func TestResolveDistDirExplicitMissing(t *testing.T) {
	root := mkdirs(t, "exe/dist", "usr-share")
	at := func(n string) string { return filepath.Join(root, n) }

	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		explicit string
		want     string
	}{
		{"does not exist", at("typo"), "does not exist"},
		{"is a file", file, "is not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDistDir(tt.explicit, at("exe"), []string{at("usr-share")})
			if err == nil {
				t.Fatalf("resolveDistDir(%q) = %q, want an error", tt.explicit, got)
			}
			if got != "" {
				t.Errorf("resolveDistDir returned %q alongside its error", got)
			}
			msg := err.Error()
			for _, want := range []string{"DIST_DIR", "BRB_DIST_DIR", tt.explicit, tt.want, "build-dist.sh"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			// And it must not quietly hand back one of the fallbacks instead.
			if strings.Contains(msg, at("exe/dist")) {
				t.Errorf("error %q suggests the search fell through", msg)
			}
		})
	}
}

// TestDistDirFromFileAndEnvironment covers the two ways the setting arrives:
// DIST_DIR in the configuration file both implementations read, BRB_DIST_DIR in
// the environment. A bare DIST_DIR in the environment is ignored, exactly as
// brb.sh's DIST_DIR="${BRB_DIST_DIR:-}" ignores it.
func TestDistDirFromFileAndEnvironment(t *testing.T) {
	if got := EnvName("DIST_DIR"); got != "BRB_DIST_DIR" {
		t.Errorf("EnvName(DIST_DIR) = %q, want BRB_DIST_DIR", got)
	}
	if got := EnvName("STAGING"); got != "STAGING" {
		t.Errorf("EnvName(STAGING) = %q, want it unchanged", got)
	}

	setHome(t, "/home/tester")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte("DIST_DIR=/from/file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DistDir != "/from/file" {
		t.Errorf("DistDir = %q, want /from/file", c.DistDir)
	}

	t.Setenv("DIST_DIR", "/ignored")
	c, err = Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DistDir != "/from/file" {
		t.Errorf("DistDir = %q; a bare DIST_DIR in the environment must be ignored", c.DistDir)
	}

	t.Setenv("BRB_DIST_DIR", "/from/env")
	c, err = Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DistDir != "/from/env" {
		t.Errorf("DistDir = %q, want the environment to win over the file", c.DistDir)
	}
}

// TestResolveDistDirUsesTheConfiguredValue proves the method wires the field to
// the search, and that the default configuration asks for nothing in
// particular.
func TestResolveDistDirUsesTheConfiguredValue(t *testing.T) {
	dir := t.TempDir()
	c := &Config{DistDir: dir}
	got, err := c.ResolveDistDir()
	if err != nil {
		t.Fatalf("ResolveDistDir: %v", err)
	}
	if got != dir {
		t.Errorf("ResolveDistDir = %q, want %q", got, dir)
	}
	if Default().DistDir != "" {
		t.Errorf("Default().DistDir = %q, want it empty so the search applies", Default().DistDir)
	}
}
