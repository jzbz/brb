package fsx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDirBytesCountsOnlyFileData holds DirBytes to what `du -sb
// --apparent-size` reports for the same tree, which is what both callers size
// their decisions against: file data, once per file, and nothing for the
// directories or symlinks around it.
func TestDirBytesCountsOnlyFileData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 40), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.bin"), make([]byte, 20), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink is a name, not payload; it contributes nothing to an ISO.
	if err := os.Symlink(filepath.Join(dir, "a.bin"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := DirBytes(t.Context(), dir)
	if err != nil {
		t.Fatalf("DirBytes: %v", err)
	}
	if got != 60 {
		t.Errorf("DirBytes = %d, want 60", got)
	}

	if _, err := DirBytes(t.Context(), filepath.Join(dir, "nope")); err == nil {
		t.Error("DirBytes of a missing directory succeeded, want an error")
	}
}

// TestDirBytesStopsWhenCancelled: a walk of a multi-terabyte tree must not
// outlive a Ctrl-C.
func TestDirBytesStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DirBytes(ctx, dir); err == nil {
		t.Error("DirBytes ignored a cancelled context")
	}
}

func TestFreeSpace(t *testing.T) {
	t.Parallel()
	n, err := FreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if n <= 0 {
		t.Errorf("FreeSpace = %d, want a positive number of bytes", n)
	}
	if _, err := FreeSpace(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("FreeSpace of a missing directory succeeded, want an error")
	}
}
