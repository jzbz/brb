package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/doc"
)

// publicKey generates a fresh keypair and returns it with the bytes of the
// identity file a public archive carries on its discs.
func publicKey(t *testing.T) (*age.X25519Identity, []byte) {
	t.Helper()
	id, err := agecrypt.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), doc.PublicIdentityName)
	if err := agecrypt.WriteIdentityFile(p, id); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return id, body
}

// stagePublicKey drops a public archive's key where ingest would leave it.
func (e *env) stagePublicKey(body []byte) string {
	e.t.Helper()
	p := e.opts().stagedPublicIdentity()
	if err := os.WriteFile(p, body, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// noPrimaryIdentity removes every place findIdentity looks, leaving a
// configuration with no key of its own — a public archive's operator.
func (e *env) noPrimaryIdentity() {
	e.t.Helper()
	for _, p := range identityCandidates(e.cfg) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.t.Fatal(err)
		}
	}
}

// opens reports whether ids can decrypt something encrypted to rec.
func opens(t *testing.T, ids []age.Identity, rec age.Recipient) bool {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rec)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	w.Close()
	_, err = age.Decrypt(&buf, ids...)
	return err == nil
}

// A public archive is the set that has no key beside the recipients file: the
// disc brought the only key there is, ingest staged it at enc/identity.txt,
// and identities() must use it alone — no error, no passphrase prompt.
func TestIdentitiesUsesTheStagedPublicKeyAlone(t *testing.T) {
	e := newEnv(t)
	e.noPrimaryIdentity()
	pub, body := publicKey(t)
	staged := e.stagePublicKey(body)

	ids, err := e.opts().identities()
	if err != nil {
		t.Fatalf("identities() = %v, want the staged public key\n%s", err, e.log())
	}
	if len(ids) != 1 || !opens(t, ids, pub.Recipient()) {
		t.Fatalf("identities() returned %d identity(ies) that do not open the public set", len(ids))
	}
	if !strings.Contains(e.log(), "using the public archive's key "+staged) {
		t.Errorf("the run did not say which key it used:\n%s", e.log())
	}
	if strings.Contains(e.log(), "passphrase") {
		t.Errorf("a public key must never prompt:\n%s", e.log())
	}
}

// With a primary identity present the staged key is added, not substituted:
// age tries each, so a private set and a public set both open from one
// staging area.
func TestIdentitiesAppendsTheStagedPublicKey(t *testing.T) {
	e := newEnv(t)
	pub, body := publicKey(t)
	staged := e.stagePublicKey(body)
	own, err := agecrypt.ReadX25519IdentityFile(e.cfg.AgeIdentity)
	if err != nil {
		t.Fatal(err)
	}

	ids, err := e.opts().identities()
	if err != nil {
		t.Fatalf("identities() = %v\n%s", err, e.log())
	}
	if len(ids) != 2 {
		t.Fatalf("identities() returned %d identities, want the primary and the staged key", len(ids))
	}
	if !opens(t, ids, own.Recipient()) {
		t.Error("the primary identity no longer opens its own set")
	}
	if !opens(t, ids, pub.Recipient()) {
		t.Error("the staged public key was not added")
	}
	if !strings.Contains(e.log(), "also using the public archive's key "+staged) {
		t.Errorf("the run did not announce the second key:\n%s", e.log())
	}
	// The primary comes first: a set encrypted to it must not cost a failed
	// trial of the public key on every file.
	if !opens(t, ids[:1], own.Recipient()) {
		t.Error("the primary identity is not first in the list")
	}
}

// Without the staged key, and without a primary, the error has to send the
// operator to both places a key can come from.
func TestIdentitiesWithNoKeyAnywhereNamesTheStagedPath(t *testing.T) {
	e := newEnv(t)
	e.noPrimaryIdentity()
	_, err := e.opts().identities()
	if err == nil {
		t.Fatal("identities() = nil, want no-key error")
	}
	for _, want := range []string{"no age identity found", e.opts().stagedPublicIdentity(), "ingest a disc of a public set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A staged key that does not parse is the key to whatever was ingested;
// ignoring it would turn "cannot decrypt" into a mystery. It is an error that
// names the file, even when a primary identity would otherwise do.
func TestIdentitiesRejectsAnUnparseableStagedKey(t *testing.T) {
	e := newEnv(t)
	staged := e.stagePublicKey([]byte("this is not an age identity\n"))
	_, err := e.opts().identities()
	if err == nil || !strings.Contains(err.Error(), staged) || !strings.Contains(err.Error(), "not a usable age identity") {
		t.Fatalf("identities() = %v, want an error naming %s", err, staged)
	}
	e.noPrimaryIdentity()
	if _, err := e.opts().identities(); err == nil || !strings.Contains(err.Error(), staged) {
		t.Fatalf("identities() without a primary = %v, want an error naming %s", err, staged)
	}
}

// AGE_IDENTITY pointed at the staged key itself is one key, not two.
func TestIdentitiesDoesNotLoadTheStagedKeyTwice(t *testing.T) {
	e := newEnv(t)
	_, body := publicKey(t)
	e.cfg.AgeIdentity = e.stagePublicKey(body)
	ids, err := e.opts().identities()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("identities() returned %d identities, want 1", len(ids))
	}
}

// publicDisc lays out one disc of a public set the way it reads: a data
// directory with a numbered image, identity.txt at the root, and a SHA512SUMS
// covering both. Nothing here needs par2.
func publicDisc(t *testing.T, n int, key []byte) string {
	t.Helper()
	mp := t.TempDir()
	data := filepath.Join(mp, dataDir)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, encName(n)), bytes.Repeat([]byte("image bytes\n"), 50), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mp, doc.PublicIdentityName), key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.WriteSums(context.Background(), mp, filepath.Join(mp, agecrypt.SumsName)); err != nil {
		t.Fatal(err)
	}
	return mp
}

// Ingesting a public set's disc stages its key at enc/identity.txt, checked
// against the disc's SHA512SUMS; a second disc of the same set matches and is
// fine; a disc of a different public set is refused whole, before its images
// land beside the first set's.
func TestIngestStagesThePublicArchivesKey(t *testing.T) {
	e := newEnv(t)
	_, keyA := publicKey(t)
	_, keyB := publicKey(t)
	staged := e.opts().stagedPublicIdentity()

	ig := &ingester{o: e.opts(), mountPoint: publicDisc(t, 1, keyA)}
	if _, err := ig.ingestDisc(context.Background()); err != nil {
		t.Fatalf("ingesting disc 1: %v\n%s", err, e.log())
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("the key was not staged: %v\n%s", err, e.log())
	}
	if !bytes.Equal(got, keyA) {
		t.Fatalf("staged key differs from the disc's")
	}
	if !strings.Contains(e.log(), doc.PublicIdentityName+" copied and verified") {
		t.Errorf("the copy was not announced as verified:\n%s", e.log())
	}
	// And identities() now opens the set with nothing else configured.
	e.noPrimaryIdentity()
	if ids, err := e.opts().identities(); err != nil || len(ids) != 1 {
		t.Fatalf("identities() after ingest = %d, %v; want the staged key", len(ids), err)
	}

	// Disc 2 of the same set: the SAME KEY, but not the same bytes. Sets
	// mastered before the writer copied one canonical file onto every disc
	// carry a different "# created:" line per disc, and a reader that
	// compared bytes refused disc 2 of its own set as a foreign one. Same key
	// means same key.
	keyA2 := bytes.Replace(keyA, []byte("# created:"), []byte("# created: a second later,"), 1)
	if bytes.Equal(keyA2, keyA) {
		t.Fatal("fixture: could not vary the created line; the identity file format changed")
	}
	ig = &ingester{o: e.opts(), mountPoint: publicDisc(t, 2, keyA2)}
	if _, err := ig.ingestDisc(context.Background()); err != nil {
		t.Fatalf("ingesting disc 2 (same key, different bytes): %v\n%s", err, e.log())
	}
	if !strings.Contains(e.log(), "already have the public archive's key") {
		t.Errorf("the second disc's identical key was not recognised:\n%s", e.log())
	}
	if got, _ := os.ReadFile(staged); !bytes.Equal(got, keyA) {
		t.Fatal("disc 2 rewrote the staged key; the first copy staged must stay")
	}

	// A disc from another public set: refused, its image never staged.
	ig = &ingester{o: e.opts(), mountPoint: publicDisc(t, 3, keyB)}
	_, err = ig.ingestDisc(context.Background())
	if err == nil || !strings.Contains(err.Error(), "two different public sets") {
		t.Fatalf("ingesting a different set's disc = %v, want a refusal", err)
	}
	if got, _ := os.ReadFile(staged); !bytes.Equal(got, keyA) {
		t.Fatal("the staged key was overwritten by the other set's")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Enc, encName(3))); err == nil {
		t.Fatal("the other set's image was staged beside the first set's")
	}
}

// A key whose bytes do not match the disc's SHA512SUMS entry decrypts nothing;
// it is reported as an incomplete copy and not staged.
func TestIngestRefusesAKeyThatFailsTheDiscsHash(t *testing.T) {
	e := newEnv(t)
	_, key := publicKey(t)
	mp := publicDisc(t, 1, key)
	// Rot one character after the sums were taken.
	rotted := append([]byte(nil), key...)
	rotted[len(rotted)-2] ^= 0x01
	if err := os.WriteFile(filepath.Join(mp, doc.PublicIdentityName), rotted, 0o644); err != nil {
		t.Fatal(err)
	}

	ig := &ingester{o: e.opts(), mountPoint: mp}
	if _, err := ig.ingestDisc(context.Background()); err != nil {
		t.Fatalf("ingestDisc: %v", err)
	}
	if ig.incomplete != 1 {
		t.Fatalf("incomplete = %d, want the key counted as an incomplete copy\n%s", ig.incomplete, e.log())
	}
	if _, err := os.Stat(e.opts().stagedPublicIdentity()); err == nil {
		t.Fatal("a key that failed the disc's hash was staged anyway")
	}
	if !strings.Contains(e.log(), "does not match the hash this disc records") {
		t.Errorf("the mismatch was not explained:\n%s", e.log())
	}
	// The data files were still ingested: a wrong key is not a wrong disc.
	if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Enc, encName(1))); err != nil {
		t.Errorf("the image was not staged: %v", err)
	}
}

// identity.txt is the one file taken off a disc that is read into memory
// whole, and it is read before the disc's SHA512SUMS entry or the age parse
// can say anything about it. A disc that names a huge file identity.txt used
// to be answered by allocating all of it: on a small machine the ingest run
// dies in the allocator, and the operator's diagnosis is an out-of-memory kill
// rather than the name of the file that caused it. The size is judged first
// now, and the disc is reported the way a damaged copy is — the rest of it
// still ingests.
func TestIngestRefusesAnAbsurdlyLargePublicKey(t *testing.T) {
	e := newEnv(t)
	_, key := publicKey(t)
	mp := publicDisc(t, 1, key)

	// Sparse, so the test does not write a megabyte: what matters is the size
	// the reader sees. A real disc would carry gigabytes here.
	f, err := os.OpenFile(filepath.Join(mp, doc.PublicIdentityName), os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxPublicIdentityBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ig := &ingester{o: e.opts(), mountPoint: mp}
	if _, err := ig.ingestDisc(context.Background()); err != nil {
		t.Fatalf("ingestDisc: %v\n%s", err, e.log())
	}
	if ig.incomplete != 1 {
		t.Fatalf("incomplete = %d, want the key counted as an incomplete copy\n%s", ig.incomplete, e.log())
	}
	if _, err := os.Stat(e.opts().stagedPublicIdentity()); err == nil {
		t.Fatal("a file too large to be a key was staged as one")
	}
	// The refusal must name the size, not the hash: reaching the hash means
	// the whole file was read first, which is the failure being prevented.
	if !strings.Contains(e.log(), "a public archive's key is a few hundred") {
		t.Fatalf("the size was not what was refused:\n%s", e.log())
	}
	// And the disc is still a disc: its image comes off as usual.
	if _, err := os.Stat(filepath.Join(e.cfg.Dirs().Enc, encName(1))); err != nil {
		t.Errorf("the image was not staged: %v", err)
	}
}

// A private set's disc has no identity.txt, and ingest must not invent one.
func TestIngestOfAPrivateDiscStagesNoKey(t *testing.T) {
	e := newEnv(t)
	mp := publicDisc(t, 1, []byte("x"))
	if err := os.Remove(filepath.Join(mp, doc.PublicIdentityName)); err != nil {
		t.Fatal(err)
	}
	ig := &ingester{o: e.opts(), mountPoint: mp}
	if _, err := ig.ingestDisc(context.Background()); err != nil {
		t.Fatalf("ingestDisc: %v\n%s", err, e.log())
	}
	if _, err := os.Stat(e.opts().stagedPublicIdentity()); err == nil {
		t.Fatal("a key was staged from a disc that carries none")
	}
}
