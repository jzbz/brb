package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The staging directory's default lives under /var/tmp, which every local
// account can write. A symlink planted at STAGING, or at enc/ or restore/
// under it, would send every decrypted image wherever the planter chose, so
// each of them is refused before anything is reaped from or written into it.
func TestSecureStagingRefusesASymlinkedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(e *env) string // which path to replace with a symlink
	}{
		{"STAGING itself", func(e *env) string { return e.cfg.Staging }},
		{"the enc directory", func(e *env) string { return e.cfg.Dirs().Enc }},
		{"the restore directory", func(e *env) string { return e.cfg.Dirs().Restore }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			elsewhere := filepath.Join(e.dir, "elsewhere")
			if err := os.MkdirAll(elsewhere, 0o700); err != nil {
				t.Fatal(err)
			}
			link := tc.link(e)
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}
			// The enc directory has to be reachable for the image to be staged
			// at all; when STAGING itself is the link that means through it.
			if err := os.MkdirAll(e.cfg.Dirs().Enc, 0o700); err != nil {
				t.Fatal(err)
			}
			err := e.opts().secureStaging(e.cfg.Dirs().Enc, e.cfg.Dirs().Restore)
			if err == nil || !strings.Contains(err.Error(), "is a symlink") || !strings.Contains(err.Error(), link) {
				t.Fatalf("secureStaging = %v, want a refusal naming the symlink %s", err, link)
			}
			// The rules live in internal/fsx and are shared with the writer;
			// what this package adds is the prefix the operator reads. Losing
			// it would leave a bare sentence with no command attached to it.
			if !strings.HasPrefix(err.Error(), "restore: ") {
				t.Errorf("the refusal lost its command prefix: %v", err)
			}
			// And the commands that write plaintext all go through it: PrepareImage
			// must refuse before decrypting a byte.
			enc, _ := e.writeImage(1, []byte("payload"))
			if _, err := PrepareImage(context.Background(), e.opts(), enc); err == nil ||
				!strings.Contains(err.Error(), "is a symlink") {
				t.Fatalf("PrepareImage = %v, want the symlink refusal", err)
			}
			filepath.WalkDir(elsewhere, func(p string, d os.DirEntry, err error) error {
				if err == nil && d.Name() == "disc01.squashfs" {
					t.Errorf("the plaintext was written through the symlink, to %s", p)
				}
				return nil
			})
		})
	}
}

// A file where a staging directory should be is refused with a message that
// says so, and a directory that is missing is created 0700.
func TestSecureStagingCreatesAndTightens(t *testing.T) {
	e := newEnv(t)
	if err := os.RemoveAll(e.cfg.Staging); err != nil {
		t.Fatal(err)
	}
	// Loosely made by hand, the way an operator's mkdir under a 022 umask
	// leaves it: it must end up 0700 all the same.
	if err := os.MkdirAll(e.cfg.Staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.opts().secureStaging(e.cfg.Dirs().Restore); err != nil {
		t.Fatalf("secureStaging: %v", err)
	}
	for _, d := range []string{e.cfg.Staging, e.cfg.Dirs().Restore} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is %o, want 0700", d, fi.Mode().Perm())
		}
	}

	if err := os.WriteFile(e.cfg.Dirs().Enc, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := e.opts().secureStaging(e.cfg.Dirs().Enc)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("secureStaging over a file = %v, want a refusal", err)
	}
}

// fsx.CreateFresh's guarantee, end to end through PrepareImage: a symlink
// planted at restore/discNN.squashfs.part before the run must not receive the
// decrypted image. This is the reproduced attack — attacker-owned staging,
// link planted after the reap — reduced to the one step that matters. The unit
// test of the guarantee itself lives beside the implementation, in internal/fsx.
func TestPrepareImageDoesNotDecryptThroughAPlantedSymlink(t *testing.T) {
	e := newEnv(t)
	payload := bytes.Repeat([]byte("secret image bytes\n"), 300)
	enc, _ := e.writeImage(1, payload)

	victim := filepath.Join(e.dir, "victim")
	if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs"+partExt)
	if err := os.Symlink(victim, part); err != nil {
		t.Fatal(err)
	}
	plain, err := PrepareImage(context.Background(), e.opts(), enc)
	if err != nil {
		t.Fatalf("PrepareImage: %v\n%s", err, e.log())
	}
	if got, _ := os.ReadFile(victim); string(got) != "theirs" {
		t.Fatal("the decrypted image was streamed through the planted symlink")
	}
	if got, _ := os.ReadFile(plain); !bytes.Equal(got, payload) {
		t.Fatal("the decrypted image did not land at the intended path")
	}
}

// copyStream — every ingest copy — has the same property.
func TestCopyStreamDoesNotWriteThroughAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("from the disc"), 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.Symlink(victim, dst+partExt); err != nil {
		t.Fatal(err)
	}
	if _, err := copyStream(context.Background(), src, dst, nil); err != nil {
		t.Fatalf("copyStream: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "theirs" {
		t.Fatal("the copy was written through the planted symlink")
	}
	if got, _ := os.ReadFile(dst); string(got) != "from the disc" {
		t.Fatalf("dst holds %q", got)
	}
}
