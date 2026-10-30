package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// needFloodTools skips a test that needs yes(1) and head(1) to build its floods.
func needFloodTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"yes", "head"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("no %s on PATH to build a flood with", name)
		}
	}
}

// TestCombinedOutputIsBounded pins the cap on a probe's captured output. A
// version or help probe is bounded in time by probeTimeout and used to be
// bounded in nothing else, so a broken tool answering -version with an endless
// stream filled a bytes.Buffer at gigabytes a second — and Detect probes every
// known tool on almost every command, an unused optional helper included.
//
// The finite flood runs first and stops the test if the cap is gone: the
// endless one after it would otherwise take the machine's memory with it.
func TestCombinedOutputIsBounded(t *testing.T) {
	sh := shell(t)
	needFloodTools(t)

	// A normal answer comes back byte for byte, stderr included.
	out, err := combinedOutput(context.Background(), sh, "-c", `printf 'tool 1.0\nsecond line\n'; printf 'on stderr\n' >&2`)
	if err != nil || out != "tool 1.0\nsecond line\non stderr\n" {
		t.Fatalf("combinedOutput of a short probe = %q, %v", out, err)
	}

	// Exactly the limit is not an overflow: kept whole, and no error.
	out, err = combinedOutput(context.Background(), sh, "-c", `yes x | head -c `+shNum(probeLimit))
	if err != nil || len(out) != probeLimit {
		t.Fatalf("combinedOutput of exactly %d bytes = %d bytes, %v; want all of it and no error", probeLimit, len(out), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, _ = combinedOutput(ctx, sh, "-c", `yes 'version 1.2.3' | head -c 16777216`)
	if len(out) > probeLimit {
		t.Fatalf("combinedOutput kept %d bytes of a 16 MiB flood; the cap is %d", len(out), probeLimit)
	}
	if got := firstLine(out); got != "version 1.2.3" {
		t.Errorf("firstLine of the capped flood = %q, want the version line", got)
	}

	// An endless flood ends at the cap, not at the timeout: the refused write
	// closes the pipe and the child dies of it.
	start := time.Now()
	out, _ = combinedOutput(ctx, sh, "-c", `exec yes 'version 1.2.3'`)
	if took := time.Since(start); took > probeTimeout/2 {
		t.Errorf("an endless flood took %v to stop; it should stop at the cap, long before the %v timeout", took, probeTimeout)
	}
	if len(out) > probeLimit || firstLine(out) != "version 1.2.3" {
		t.Errorf("endless flood: kept %d bytes, first line %q", len(out), firstLine(out))
	}
}

// floodingUnsquashfs installs a /bin/sh script as unsquashfs and returns a Set
// that uses it.
func floodingUnsquashfs(t *testing.T, body string) *Set {
	t.Helper()
	shell(t)
	path := filepath.Join(t.TempDir(), "unsquashfs")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewSet([]Tool{{Name: Unsquashfs, Path: path, Found: true}})
}

// TestImageStatsIsBounded pins the cap on "unsquashfs -s". Its stdout goes
// around the line writer and the backup's context has no deadline, so a broken
// unsquashfs flooding it grew memory until the backup was killed, hours in.
// Now a flood is an error that blames the tool, a real summary is returned
// untouched, and a summary that merely fills the buffer before an unrelated
// failure keeps that failure's own error.
func TestImageStatsIsBounded(t *testing.T) {
	needFloodTools(t)
	ctx := context.Background()

	summary := "Found a valid SQUASHFS 4:0 superblock on x.\nCreation or last append time Thu Jan  1 00:00:00 1970\nFilesystem size 4 Kbytes"
	s := floodingUnsquashfs(t, `printf '%s\n' '`+summary+`'`)
	if got, err := s.ImageStats(ctx, "x.sq"); err != nil || got != summary {
		t.Fatalf("ImageStats of a normal summary = %q, %v; want it unchanged", got, err)
	}

	// Finite first, for the same reason as in TestCombinedOutputIsBounded.
	s = floodingUnsquashfs(t, `yes superblock | head -c 16777216`)
	_, err := s.ImageStats(ctx, "x.sq")
	if err == nil || !strings.Contains(err.Error(), "printed more than "+shNum(probeLimit)+" bytes") {
		t.Fatalf("ImageStats of a 16 MiB flood = %v; want the overflow named", err)
	}

	s = floodingUnsquashfs(t, `head -c `+shNum(probeLimit)+` /dev/zero; echo 'something unrelated failed' >&2; exit 1`)
	_, err = s.ImageStats(ctx, "x.sq")
	if err == nil || strings.Contains(err.Error(), "printed more than") || !strings.Contains(err.Error(), "something unrelated failed") {
		t.Errorf("ImageStats of exactly %d bytes then a failure = %v; want the failure, not an overflow", probeLimit, err)
	}

	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s = floodingUnsquashfs(t, `exec yes superblock`)
	start := time.Now()
	_, err = s.ImageStats(dctx, "x.sq")
	if err == nil || !strings.Contains(err.Error(), "printed more than") {
		t.Errorf("ImageStats of an endless flood = %v; want the overflow named", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("an endless flood took %v to stop; it should stop at the cap", took)
	}
}

// shNum spells an int for a shell command line.
func shNum(n int) string { return strconv.Itoa(n) }
