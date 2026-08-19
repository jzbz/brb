package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// env is a self-contained staging area with a real age keypair. Nothing in it
// needs root, a network or any external binary.
type env struct {
	t          *testing.T
	dir        string
	cfg        *config.Config
	out        *bytes.Buffer
	ui         *ui.Printer
	tools      *tools.Set
	recipients []age.Recipient
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()

	id, err := agecrypt.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	idPath := filepath.Join(dir, "identity.txt")
	if err := agecrypt.WriteIdentityFile(idPath, id); err != nil {
		t.Fatalf("WriteIdentityFile: %v", err)
	}
	recPath := filepath.Join(dir, "recipients.txt")
	if err := agecrypt.AppendRecipient(recPath, id.Recipient().String()); err != nil {
		t.Fatalf("AppendRecipient: %v", err)
	}
	recs, err := agecrypt.ParseRecipientsFile(recPath)
	if err != nil {
		t.Fatalf("ParseRecipientsFile: %v", err)
	}

	cfg := config.Default()
	cfg.Staging = filepath.Join(dir, "staging")
	cfg.AgeIdentity = idPath
	cfg.AgeRecipientsFile = recPath
	cfg.Burner = ""

	out := &bytes.Buffer{}
	e := &env{
		t:          t,
		dir:        dir,
		cfg:        cfg,
		out:        out,
		ui:         ui.New(out, false),
		tools:      tools.NewSet(nil),
		recipients: recs,
	}
	for _, d := range []string{cfg.Dirs().Enc, cfg.Dirs().Restore} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	return e
}

func (e *env) opts() Options {
	return Options{Cfg: e.cfg, UI: e.ui, Tools: e.tools}
}

func (e *env) log() string { return e.out.String() }

// writeImage encrypts payload as disc n's image and writes both recorded
// hashes beside it, exactly as a backup does.
func (e *env) writeImage(n int, payload []byte) (encPath string, sums agecrypt.Sums) {
	e.t.Helper()
	base := strings.TrimSuffix(encName(n), ageExt)
	src := filepath.Join(e.dir, base+".plain")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		e.t.Fatalf("write payload: %v", err)
	}
	encPath = filepath.Join(e.cfg.Dirs().Enc, encName(n))
	sums, err := agecrypt.Encrypt(context.Background(), src, encPath, e.recipients, nil)
	if err != nil {
		e.t.Fatalf("Encrypt: %v", err)
	}
	if err := agecrypt.WriteSumFile(encPath+sumExt, sums.Cipher, encName(n)); err != nil {
		e.t.Fatalf("WriteSumFile: %v", err)
	}
	if err := agecrypt.WriteSumFile(filepath.Join(e.cfg.Dirs().Enc, base+sumExt), sums.Plain, base); err != nil {
		e.t.Fatalf("WriteSumFile: %v", err)
	}
	return encPath, sums
}

// corrupt flips bits in the middle of a file without changing its length.
func corrupt(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 8 {
		t.Fatalf("%s is too short to corrupt", path)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// scramble puts a non-hex byte in the middle of a sha512sum file's digest, the
// commonest shape of sidecar rot: the file still exists and is still the right
// length, but no longer parses.
func scramble(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 16 {
		t.Fatalf("%s is too short to scramble", path)
	}
	body[9] = 'X'
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakePar2 installs a shell script standing in for par2. body runs with the
// encrypted directory as its working directory. Using a script keeps the
// decision table testable on a machine with no par2 installed.
func fakePar2(t *testing.T, e *env, body string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh to build a par2 stand-in with")
	}
	path := filepath.Join(t.TempDir(), "par2")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write par2 stub: %v", err)
	}
	e.tools = tools.NewSet([]tools.Tool{{Name: tools.Par2, Path: path, Found: true}})
}

func TestPrepareImageHashGood(t *testing.T) {
	e := newEnv(t)
	payload := bytes.Repeat([]byte("squashfs pretend payload\n"), 400)
	enc, _ := e.writeImage(1, payload)

	got, err := PrepareImage(context.Background(), e.opts(), enc)
	if err != nil {
		t.Fatalf("PrepareImage: %v", err)
	}
	want := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("decrypted image does not match the payload")
	}
	// The returned value must be a path and nothing else.
	if strings.ContainsAny(got, "\n\r ") {
		t.Fatalf("path is contaminated: %q", got)
	}
}

// A plaintext already in the restore directory is reused only after it has
// been hashed against the recorded plaintext hash. Here it matches, so it is
// reused: the ciphertext is deleted first to prove nothing was decrypted.
func TestPrepareImageReusesAnAlreadyDecryptedImage(t *testing.T) {
	e := newEnv(t)
	payload := []byte("payload")
	enc, _ := e.writeImage(1, payload)

	plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	if err := os.WriteFile(plain, payload, 0o600); err != nil {
		t.Fatalf("seed plaintext: %v", err)
	}
	if err := os.Remove(enc); err != nil {
		t.Fatal(err)
	}
	got, err := PrepareImage(context.Background(), e.opts(), enc)
	if err != nil {
		t.Fatalf("PrepareImage: %v\n%s", err, e.log())
	}
	if got != plain {
		t.Fatalf("path = %q, want %q", got, plain)
	}
	for _, want := range []string{"verifying the decrypted disc01.squashfs", "reusing the decrypted " + plain} {
		if !strings.Contains(e.log(), want) {
			t.Errorf("the run did not say %q:\n%s", want, e.log())
		}
	}
}

// The restore directory is shared by every set that passes through the staging
// area, so a disc01.squashfs left there by some other archive — --keep-images,
// or a run that stopped early — must not be handed over as this set's disc 1.
// It fails the recorded plaintext hash, is discarded, and the image is
// decrypted afresh.
func TestPrepareImageDoesNotReuseAPlaintextThatFailsItsHash(t *testing.T) {
	e := newEnv(t)
	payload := bytes.Repeat([]byte("this set's image\n"), 100)
	enc, _ := e.writeImage(1, payload)

	plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	if err := os.WriteFile(plain, []byte("some other set's disc 1"), 0o600); err != nil {
		t.Fatalf("seed plaintext: %v", err)
	}
	got, err := PrepareImage(context.Background(), e.opts(), enc)
	if err != nil {
		t.Fatalf("PrepareImage: %v\n%s", err, e.log())
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("the stale plaintext was reused: %q", body)
	}
	if !strings.Contains(e.log(), "does not match the recorded plaintext hash") {
		t.Errorf("the discard was silent:\n%s", e.log())
	}
}

// With no plaintext sidecar there is nothing to check a leftover against, and
// an unchecked leftover is not reused either.
func TestPrepareImageDoesNotReuseAPlaintextWithoutASidecar(t *testing.T) {
	e := newEnv(t)
	payload := []byte("payload")
	enc, _ := e.writeImage(1, payload)
	if err := os.Remove(filepath.Join(e.cfg.Dirs().Enc, "disc01.squashfs"+sumExt)); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	if err := os.WriteFile(plain, []byte("unverifiable leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := PrepareImage(context.Background(), e.opts(), enc)
	if err != nil {
		t.Fatalf("PrepareImage: %v\n%s", err, e.log())
	}
	if body, _ := os.ReadFile(got); !bytes.Equal(body, payload) {
		t.Fatalf("the unverifiable leftover was reused: %q", body)
	}
}

func TestPrepareImageDecisionTable(t *testing.T) {
	payload := bytes.Repeat([]byte("data that must not be decrypted when damaged\n"), 200)

	tests := []struct {
		name string
		// setup damages the environment and returns nothing; it runs after the
		// image and its hashes have been written.
		setup func(t *testing.T, e *env, enc string)
		// wantErr, when non-empty, is a substring the error must contain.
		wantErr string
		// alsoErr, when non-empty, is a second substring it must contain.
		alsoErr string
		// wantLog, when non-empty, is a substring the printed output must
		// contain: the warning that stands in for a refusal.
		wantLog string
		// wantPlain says whether the decrypted image must exist afterwards.
		wantPlain bool
	}{
		{
			name:      "intact ciphertext decrypts",
			setup:     func(*testing.T, *env, string) {},
			wantPlain: true,
		},
		{
			name: "damaged ciphertext with no par2 is refused",
			setup: func(t *testing.T, e *env, enc string) {
				corrupt(t, enc)
			},
			wantErr: "has no par2 recovery data",
		},
		{
			name: "damaged ciphertext with par2 data but no par2 program",
			setup: func(t *testing.T, e *env, enc string) {
				corrupt(t, enc)
				if err := os.WriteFile(enc+par2Ext, []byte("par2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "needs par2 but it is not installed",
		},
		{
			name: "par2 repairs it and the hash matches again",
			setup: func(t *testing.T, e *env, enc string) {
				good := filepath.Join(t.TempDir(), "good.age")
				data, err := os.ReadFile(enc)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(good, data, 0o600); err != nil {
					t.Fatal(err)
				}
				corrupt(t, enc)
				if err := os.WriteFile(enc+par2Ext, []byte("par2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fakePar2(t, e, "cp '"+good+"' '"+enc+"'")
			},
			wantPlain: true,
		},
		{
			// HL-2. The ciphertext is untouched and par2 says so; only the
			// 150-byte sidecar recording its hash has rotted. brb.sh names the
			// sidecar as the corrupt party and carries on, because refusing
			// would throw away a provably correct image. The decrypted image is
			// still checked against disc01.squashfs.sha512 afterwards, which is
			// what keeps this safe.
			name: "par2 says the ciphertext is whole and only the sidecar disagrees",
			setup: func(t *testing.T, e *env, enc string) {
				wrong := strings.Repeat("ab", 64)
				if err := agecrypt.WriteSumFile(enc+sumExt, wrong, filepath.Base(enc)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(enc+par2Ext, []byte("par2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fakePar2(t, e, "exit 0")
			},
			wantLog:   "the sidecar is what is corrupt, not the image",
			wantPlain: true,
		},
		{
			// The other half of HL-2: continuing past a rotted sidecar must not
			// become continuing past a rotted image. par2 blesses a ciphertext
			// that really is damaged, and age's own authentication catches it.
			name: "par2 blesses a genuinely damaged ciphertext and the decryption still fails",
			setup: func(t *testing.T, e *env, enc string) {
				corrupt(t, enc)
				if err := os.WriteFile(enc+par2Ext, []byte("par2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fakePar2(t, e, "exit 0")
			},
			wantErr: "decrypting disc01.squashfs" + ageExt,
			wantLog: "the sidecar is what is corrupt, not the image",
		},
		{
			name: "par2 cannot repair it",
			setup: func(t *testing.T, e *env, enc string) {
				corrupt(t, enc)
				if err := os.WriteFile(enc+par2Ext, []byte("par2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fakePar2(t, e, "echo 'Repair is not possible.' >&2; exit 1")
			},
			wantErr: "par2 could not repair",
		},
		{
			name: "no recorded ciphertext hash and no par2 still decrypts",
			setup: func(t *testing.T, e *env, enc string) {
				if err := os.Remove(enc + sumExt); err != nil {
					t.Fatal(err)
				}
			},
			wantPlain: true,
		},
		{
			name: "decrypted image does not match its recorded hash",
			setup: func(t *testing.T, e *env, enc string) {
				base := strings.TrimSuffix(filepath.Base(enc), ageExt)
				wrong := strings.Repeat("ab", 64)
				if err := agecrypt.WriteSumFile(filepath.Join(filepath.Dir(enc), base+sumExt), wrong, base); err != nil {
					t.Fatal(err)
				}
			},
			// age authenticated the ciphertext on the way through, so the
			// 170-byte sidecar is as likely to be what rotted as the image.
			wantErr: "does not match the hash in disc01.squashfs" + sumExt,
			alsoErr: "par2 repair -- " + sidecarsPar2,
		},
		{
			// The likeliest shape of sidecar rot: the flipped byte lands in the
			// 128-character digest, so the file no longer parses at all. HL-2
			// again — an unreadable hash file says nothing about the ciphertext,
			// which age authenticates on the way through anyway, so this warns
			// and goes on rather than abandoning the image.
			name: "a rotted ciphertext sidecar that no longer parses",
			setup: func(t *testing.T, e *env, enc string) {
				scramble(t, enc+sumExt)
			},
			wantLog:   "cannot be read, so it is the sidecar that is corrupt",
			wantPlain: true,
		},
		{
			// The plaintext sidecar is the last check there is: age cannot
			// vouch for the image being the one that was backed up, only for the
			// ciphertext being unmodified. Losing this one is fatal, as it is in
			// brb.sh.
			name: "a rotted plaintext sidecar that no longer parses is still fatal",
			setup: func(t *testing.T, e *env, enc string) {
				base := strings.TrimSuffix(filepath.Base(enc), ageExt)
				scramble(t, filepath.Join(filepath.Dir(enc), base+sumExt))
			},
			wantErr: "that sidecar is damaged",
			alsoErr: "par2 repair -- " + sidecarsPar2,
		},
		{
			name: "unreadable age identity",
			setup: func(t *testing.T, e *env, enc string) {
				e.cfg.AgeIdentity = filepath.Join(e.dir, "nope.txt")
			},
			wantErr: "age identity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			enc, _ := e.writeImage(1, payload)
			tc.setup(t, e, enc)

			got, err := PrepareImage(context.Background(), e.opts(), enc)
			plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("PrepareImage succeeded (%q), want an error containing %q\n%s", got, tc.wantErr, e.log())
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if tc.alsoErr != "" && !strings.Contains(err.Error(), tc.alsoErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.alsoErr)
				}
			} else if err != nil {
				t.Fatalf("PrepareImage: %v\n%s", err, e.log())
			}
			if tc.wantLog != "" && !strings.Contains(e.log(), tc.wantLog) {
				t.Fatalf("output does not mention %q:\n%s", tc.wantLog, e.log())
			}

			_, statErr := os.Stat(plain)
			if tc.wantPlain {
				if statErr != nil {
					t.Fatalf("expected a decrypted image at %s: %v", plain, statErr)
				}
				body, err := os.ReadFile(plain)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(body, payload) {
					t.Fatal("decrypted image does not match the payload")
				}
			} else if statErr == nil {
				t.Fatalf("a plaintext image was left at %s after a failure", plain)
			}
			// Whatever happened, nothing may be left half-written.
			if _, err := os.Stat(plain + partExt); err == nil {
				t.Fatalf("a .part file was left behind at %s", plain+partExt)
			}
		})
	}
}

func TestPrepareImageRejectsNonAgePath(t *testing.T) {
	e := newEnv(t)
	if _, err := PrepareImage(context.Background(), e.opts(), filepath.Join(e.dir, "disc01.squashfs")); err == nil {
		t.Fatal("expected an error for a path that is not an .age file")
	}
	if _, err := PrepareImage(context.Background(), e.opts(), ""); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

func TestPrepareImageNeedsItsDependencies(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"no config", Options{UI: ui.New(&bytes.Buffer{}, false), Tools: tools.NewSet(nil)}},
		{"no printer", Options{Cfg: config.Default(), Tools: tools.NewSet(nil)}},
		{"no tools", Options{Cfg: config.Default(), UI: ui.New(&bytes.Buffer{}, false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PrepareImage(context.Background(), tc.o, "x.age"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPrepareImageCancellation(t *testing.T) {
	e := newEnv(t)
	enc, _ := e.writeImage(1, bytes.Repeat([]byte("x"), 4<<20))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareImage(ctx, e.opts(), enc); err == nil {
		t.Fatal("expected a cancelled context to abort PrepareImage")
	}
	plain := filepath.Join(e.cfg.Dirs().Restore, "disc01.squashfs")
	if _, err := os.Stat(plain); err == nil {
		t.Fatal("a plaintext image survived a cancelled run")
	}
}
