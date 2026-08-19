// Package restore implements the read side of brb: copying a burned disc set
// back onto disk (Ingest), proving a disc still reads (VerifyDisc), burning the
// ISOs (Burn), and turning encrypted images back into files (Restore, Mount,
// List, Index).
//
// Everything here is built on PrepareImage, which is the one operation that
// stands between a damaged disc and a silently wrong restore. It checks the
// ciphertext against its recorded hash, repairs it with par2 when the hash does
// not match, re-checks, and refuses to decrypt anything that still does not
// match. It returns a path and nothing else: brb.sh had to smuggle that path
// through a global variable because par2's chatter would otherwise end up
// inside it, and that whole class of mistake is avoided here by not capturing
// subprocess output into values that are used as paths.
//
// Two behaviours deliberately differ from brb.sh, both because the bash version
// cannot finish otherwise:
//
//   - Restore deletes each decrypted image once it has been extracted, so a
//     500 GB archive does not need 500 GB of staging on top of the destination.
//     RestoreOptions.KeepImages restores the old behaviour.
//   - Ingest always terminates. Under --yes it ingests the disc that is in the
//     drive now, exactly once; bash's prompt and confirmation both auto-succeed,
//     so "brb --yes ingest" re-copies the same disc forever.
package restore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// File and directory names shared by both implementations. These are part of
// the on-disc format and must not be changed.
const (
	// dataDir is the subdirectory of a disc holding the payload.
	dataDir = "data"
	// manifestName is the whole-set manifest carried on every disc.
	manifestName = "MANIFEST.txt"
	// indexName is the encrypted "which disc holds what" index.
	indexName = "index.tsv.gz.age"
	// partExt marks a file that is still being written.
	partExt = ".part"
	// mapfileExt is where ddrescue records what it managed to read.
	mapfileExt = ".mapfile"
	// sumExt is the extension of a single-entry sha512sum file.
	sumExt = ".sha512"
	// par2Ext is the extension of a par2 index file.
	par2Ext = ".par2"
	// ageExt is the extension of an age-encrypted file.
	ageExt = ".age"
	// sidecarsStem is the stem par2 pairs a sidecars set's index and volume
	// files by: "sidecars.par2" and "sidecars.vol00+40.par2".
	sidecarsStem = "sidecars"
	// sidecarsPar2 is the recovery set a disc carries over its small files:
	// every .sha512 sidecar and the encrypted index. The image's own par2 set
	// covers the ciphertext and nothing else, so this is the only thing that
	// can put a rotted hash file or a rotted index back.
	//
	// This name is on-disc format and is frozen. In the staging area the same
	// file is stored per disc — see stagedSidecarName.
	sidecarsPar2 = sidecarsStem + par2Ext
)

// isSidecarParity reports whether a file in a disc's data directory belongs to
// that disc's sidecars recovery set: the index file itself, or one of the
// volume files par2 pairs with it.
func isSidecarParity(name string) bool {
	return strings.HasPrefix(name, sidecarsStem+".") && strings.HasSuffix(name, par2Ext)
}

// stagedSidecarName maps a disc's sidecar parity file to the name it is stored
// under in the staging area.
//
// HL-5. Every disc of a set carries a sidecars.par2 of its own, covering that
// disc's own .sha512 files as well as the shared index. Staging is one flat
// directory, so copied in under the on-disc name the N sets collide and N discs
// of recovery data become one: brb.sh keeps the first disc's copy, this
// program used to keep the last, and either way the sidecars of every other
// disc are left with no parity at all. Combined with a restore that will not
// proceed past a rotted sidecar, that decides whether a disc is restorable.
//
// The fix is a staging-only rename — "sidecars.par2" becomes
// "sidecars-disc03.par2" and "sidecars.vol00+40.par2" becomes
// "sidecars-disc03.vol00+40.par2". par2 finds a set's volume files by the index
// file's own stem, so the renamed set repairs exactly as the original did. What
// is written to a disc is untouched: the name there stays sidecars.par2, and
// ingest maps from it, so a disc written by brb.sh reads the same as one
// written here.
//
// disc of 0 means the disc number could not be told; the name is left alone.
func stagedSidecarName(name string, disc int) string {
	if disc <= 0 || !isSidecarParity(name) {
		return name
	}
	return fmt.Sprintf("%s-disc%02d%s", sidecarsStem, disc, strings.TrimPrefix(name, sidecarsStem))
}

// stagedSidecarSets lists the sidecar parity index files present in the staging
// area that cover disc n's small files, most specific first.
//
// n of 0 asks for a set covering the encrypted index rather than one disc's
// hashes: the index is on every disc and in every disc's set, so any of them
// will do. For a numbered disc only that disc's own set is named — another
// disc's parity knows nothing about discNN.squashfs.age.sha512, and sending an
// operator to run a repair that cannot work is worse than saying nothing.
//
// The flat name is always considered last: a staging area filled by brb.sh, or
// by an older version of this program, holds one set under the on-disc name.
func stagedSidecarSets(encDir string, n int) []string {
	var want []string
	if n > 0 {
		want = append(want, stagedSidecarName(sidecarsPar2, n))
	} else if ents, err := os.ReadDir(encDir); err == nil {
		for _, e := range ents {
			nm := e.Name()
			if strings.HasPrefix(nm, sidecarsStem+"-disc") && strings.HasSuffix(nm, par2Ext) &&
				!strings.Contains(nm, ".vol") {
				want = append(want, nm)
				break // every disc's set covers the index; one is enough
			}
		}
	}
	want = append(want, sidecarsPar2)

	var out []string
	for _, nm := range want {
		if _, err := os.Stat(filepath.Join(encDir, nm)); err == nil {
			out = append(out, nm)
		}
	}
	return out
}

// sidecarRepairHint is the advice to give when the small file is what rotted
// rather than the image.
//
// brb.sh always says "repair it from the disc", which on a bash-written set is
// true twice over: sidecars.par2 is on the disc and its ingest copies it into
// staging under the same name. Here the two places genuinely differ — a backup
// writes the set into the disc directory and never into enc/, and an ingest
// stores it per disc — so pointing at only one of them sends the operator to a
// file that is not there. Name every place the parity actually is, and always
// end with the disc itself, which is where it can always be found.
//
// disc is the disc whose sidecars the file belongs to, or 0 when any disc's set
// will do.
func (o Options) sidecarRepairHint(disc int) string {
	encDir := o.dirs().Enc
	var where []string
	for _, nm := range stagedSidecarSets(encDir, disc) {
		where = append(where, fmt.Sprintf("par2 repair -- %s in %s", nm, encDir))
	}
	from := "the disc"
	if disc > 0 {
		from = fmt.Sprintf("disc %d", disc)
	}
	where = append(where, fmt.Sprintf("par2 repair -- %s in %s's %s/ directory", sidecarsPar2, from, dataDir))
	return "repair it from parity: " + strings.Join(where, ", or ")
}

// discOfImage reports which disc an image base name such as "disc07.squashfs"
// belongs to. 0 means it could not be told, which only happens for a name this
// program did not produce.
func discOfImage(base string) int {
	n, _ := discNumberOf(base, ".squashfs")
	return n
}

// killGrace is how long a child process gets to exit after a cancellation
// before it is killed outright.
const killGrace = 5 * time.Second

// unmountGrace bounds an unmount that runs after the context was cancelled.
const unmountGrace = 30 * time.Second

// ErrVerifyFailed is the sentinel behind a disc that did not match its
// SHA512SUMS.
var ErrVerifyFailed = errors.New("disc verification failed")

// ErrNoMatch is returned by Index when no line matched the pattern.
var ErrNoMatch = errors.New("no match in the index")

// ErrIncompleteCopy is the sentinel behind every copy that did not reproduce
// its source byte for byte. brb.sh's copy_file_robustly returns success
// unconditionally once ddrescue has run; here an incomplete salvage is always
// reported as such.
var ErrIncompleteCopy = errors.New("copy incomplete")

// Options carries the dependencies every command in this package needs.
type Options struct {
	// Cfg is the loaded configuration; required.
	Cfg *config.Config
	// UI receives progress and status output; required.
	UI *ui.Printer
	// Tools is the detected set of external programs; required.
	Tools *tools.Set
	// Version is this binary's brb version. burn stamps it into the
	// application id of any ISO it has to build, so an image built at burn
	// time carries the same provenance as one built during the backup. Empty
	// leaves the id off, which costs nothing else.
	Version string

	// ids caches the age identities once they have been loaded, so that a
	// single command that decrypts several things — a --only restore reads the
	// encrypted index and then one image per disc — resolves the secret key
	// once. Options is copied by value, so a caller that fills this in shares
	// the loaded keys with everything it hands the copy to.
	//
	// Unlocking a key is not always free: an identity that is protected by a
	// passphrase has to be unlocked by the person at the terminal, and asking
	// them for it once per decryption is how an operator learns to type the
	// passphrase to whatever asks. One load, one prompt.
	ids []age.Identity
}

// check reports a missing dependency rather than letting a nil pointer panic
// somewhere deep in a multi-hour operation.
func (o Options) check() error {
	switch {
	case o.Cfg == nil:
		return errors.New("restore: no configuration")
	case o.UI == nil:
		return errors.New("restore: no printer")
	case o.Tools == nil:
		return errors.New("restore: no tool set")
	}
	return nil
}

// dirs returns the staging subdirectories.
func (o Options) dirs() config.Dirs { return o.Cfg.Dirs() }

// discFile is one numbered per-disc file found in the staging area: an
// encrypted image in the enc directory, or an ISO in the iso directory.
type discFile struct {
	// N is the disc number the file belongs to.
	N int
	// Path is the file's path.
	Path string
}

// encName returns the on-disc name of one encrypted image, e.g.
// "disc07.squashfs.age". The format is shared with brb.sh.
func encName(n int) string { return fmt.Sprintf("disc%02d.squashfs%s", n, ageExt) }

// discNumberOf extracts the disc number from a name of the form
// "disc<digits><suffix>". It accepts any number of digits so that a set with
// more than 99 discs still sorts and selects correctly.
func discNumberOf(name, suffix string) (int, bool) {
	if !strings.HasPrefix(name, "disc") || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	digits := name[len("disc") : len(name)-len(suffix)]
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// listNumbered returns every regular file in dir whose name is "disc<n><suffix>",
// sorted by disc number. A missing directory yields no entries and no error:
// the caller reports "nothing ingested yet" more helpfully than a stat error
// would.
func listNumbered(dir, suffix string) ([]discFile, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("restore: reading %s: %w", dir, err)
	}
	var out []discFile
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n, ok := discNumberOf(e.Name(), suffix)
		if !ok {
			continue
		}
		out = append(out, discFile{N: n, Path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// selectImages resolves which encrypted images a command should work on: one
// disc when n is positive, otherwise every image in the staging area.
func (o Options) selectImages(n int) ([]discFile, error) {
	encDir := o.dirs().Enc
	if n > 0 {
		p := filepath.Join(encDir, encName(n))
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("restore: no image for disc %d in %s: %w", n, encDir, err)
		}
		return []discFile{{N: n, Path: p}}, nil
	}
	imgs, err := listNumbered(encDir, ".squashfs"+ageExt)
	if err != nil {
		return nil, err
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("restore: no images in %s — run 'brb ingest' first", encDir)
	}
	return imgs, nil
}

// identityCandidates lists where the age secret key may live, in the order
// brb.sh's find_identity tries them: the configured identity, then its
// age-encrypted container, and last the passphrase-protected rescue identity
// that init-key's advice ("age -p ... && shred -u identity.txt") leaves as the
// only key there is. Without the fallbacks, the disaster-recovery scenario the
// rescue key exists for is exactly the one this program cannot handle.
func identityCandidates(c *config.Config) []string {
	d := filepath.Dir(c.AgeRecipientsFile)
	var cands []string
	if c.AgeIdentity != "" {
		cands = []string{c.AgeIdentity, c.AgeIdentity + ageExt}
	} else {
		cands = []string{filepath.Join(d, "identity.txt"), filepath.Join(d, "identity.txt"+ageExt)}
	}
	return append(cands, filepath.Join(d, "rescue-identity.txt"+ageExt))
}

// findIdentity returns the first identity candidate that exists and can be
// opened, mirroring brb.sh's `-f && -r` test.
func findIdentity(c *config.Config) (string, bool) {
	for _, cand := range identityCandidates(c) {
		f, err := os.Open(cand)
		if err != nil {
			continue
		}
		f.Close()
		return cand, true
	}
	return "", false
}

// identityIsEncrypted reports whether the file is an age container rather than
// a plaintext identity. Both container formats announce themselves on their
// first line; an age-keygen identity starts with "# created:" or
// "AGE-SECRET-KEY-".
func identityIsEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	first, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return strings.HasPrefix(first, "age-encryption.org/v1") ||
		strings.HasPrefix(first, "-----BEGIN AGE ENCRYPTED FILE-----"), nil
}

// stagedPublicIdentity is where a public archive's key sits in the staging
// area: <STAGING>/enc/identity.txt. The writer leaves the key it minted there
// (backup's publicIdentityPath), and ingest copies the identity.txt from a
// public set's disc root to the same place, so a public set is restorable by
// either route without the operator being asked to set AGE_IDENTITY to a file
// on a disc. brb.sh's find_identity looks in the same place.
func (o Options) stagedPublicIdentity() string {
	return filepath.Join(o.dirs().Enc, doc.PublicIdentityName)
}

// identities loads the age identities used to decrypt. It is called before any
// expensive work so that a missing key fails in a second rather than after an
// hour of hashing. A passphrase-protected identity is unlocked here, once —
// see the ids field for why once matters.
//
// Two sources are consulted. The primary identity is found exactly as before:
// AGE_IDENTITY, its age container, then the rescue key, and a
// passphrase-protected one is unlocked. Then, if the staging area holds a
// public archive's key (see stagedPublicIdentity), that key is added to the
// list — age tries each identity in turn, so a set encrypted to either opens.
// When there is no primary identity at all but the staged key is there, it is
// used alone, with no error and no passphrase prompt: a public archive is
// exactly the set that has no key beside the recipients file, and the disc
// itself brought the only one there is. A staged key that exists but does not
// parse is an error naming the file: it is the key to whatever was ingested,
// and silently ignoring it would turn "cannot decrypt" into a mystery.
//
// A copy of Options that already carries loaded identities reuses them; see the
// ids field.
func (o Options) identities() ([]age.Identity, error) {
	if len(o.ids) > 0 {
		return o.ids, nil
	}
	staged := o.stagedPublicIdentity()
	var pub []age.Identity
	switch _, err := os.Stat(staged); {
	case err == nil:
		pub, err = agecrypt.ParseIdentityFile(staged)
		if err != nil {
			return nil, fmt.Errorf("restore: %s is not a usable age identity — it should be the public archive's key, "+
				"copied from the disc root by ingest; re-ingest a disc of the set, or copy identity.txt from any of its discs there: %w",
				staged, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("restore: %s: %w", staged, err)
	}

	path, found := findIdentity(o.Cfg)
	if !found {
		if len(pub) > 0 {
			o.UI.Step("using the public archive's key %s", staged)
			return pub, nil
		}
		want := o.Cfg.AgeIdentity
		if want == "" {
			want = "identity.txt"
		}
		return nil, fmt.Errorf("restore: no age identity found: looked for %s, %s%s and rescue-identity.txt.age near %s, "+
			"and for a public archive's key at %s (set AGE_IDENTITY=/path/to/identity.txt, or ingest a disc of a public set)",
			want, want, ageExt, o.Cfg.AgeRecipientsFile, staged)
	}
	// Say so when the file used is not the one asked for: falling back to the
	// rescue key is a decision the operator should see, not discover from a
	// passphrase prompt.
	if path != o.Cfg.AgeIdentity {
		o.UI.Step("using identity %s", path)
	}
	var ids []age.Identity
	if path == staged {
		// AGE_IDENTITY was pointed at the staged key itself; it is already
		// loaded, and loading it twice would only ask age to try it twice.
		ids = pub
		pub = nil
	} else {
		enc, err := identityIsEncrypted(path)
		if err != nil {
			return nil, fmt.Errorf("restore: age identity %s is not usable (set AGE_IDENTITY=/path/to/identity.txt): %w", path, err)
		}
		if enc {
			ids, err = o.unlockIdentity(path)
		} else {
			ids, err = agecrypt.ParseIdentityFile(path)
			if err != nil {
				err = fmt.Errorf("restore: age identity %s is not usable (set AGE_IDENTITY=/path/to/identity.txt): %w", path, err)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if len(pub) > 0 {
		o.UI.Step("also using the public archive's key %s", staged)
		ids = append(ids, pub...)
	}
	return ids, nil
}

// passphraseError turns a failed prompt into the sentence that fits it. Three
// failures, three diagnoses: every one of them used to be reported as "there is
// no terminal to ask on ... this cannot be automated", which is a wrong answer
// when the operator is sitting at a terminal and merely pressed Enter — it
// sends them hunting for a tty that was already there. All three fail closed;
// only the wording differs.
func passphraseError(path string, err error) error {
	switch {
	case errors.Is(err, ErrEmptyPassphrase):
		return fmt.Errorf("restore: %s is passphrase-protected and the passphrase was empty — "+
			"a passphrase cannot be empty. Run the command again and type it, "+
			"or point AGE_IDENTITY at an unencrypted identity", path)
	case errors.Is(err, ErrNoTerminal):
		return fmt.Errorf("restore: %s is passphrase-protected and there is no terminal to ask on. "+
			"The passphrase is read from /dev/tty and never from a pipe, so this cannot be automated — "+
			"run it from an interactive shell, or point AGE_IDENTITY at an unencrypted identity: %w", path, err)
	default:
		return fmt.Errorf("restore: %s is passphrase-protected and the passphrase could not be read — "+
			"run it from an interactive shell, or point AGE_IDENTITY at an unencrypted identity: %w", path, err)
	}
}

// unlockIdentity decrypts a passphrase-protected identity with a passphrase
// asked for on the terminal, once. The plaintext identity lives only in this
// process's memory: it is never written to a file and never appears on a
// command line, which is the property the encrypted container exists for.
func (o Options) unlockIdentity(path string) ([]age.Identity, error) {
	// Refuse under --yes rather than prompting: --yes promises an unattended
	// run, and a run that stops to ask for a passphrase is not one.
	if o.UI.AssumeYes() {
		return nil, fmt.Errorf("restore: %s is passphrase-protected and --yes is in effect; "+
			"there is nobody to type the passphrase — run without --yes, or point AGE_IDENTITY at an unencrypted identity", path)
	}
	o.UI.Warn("identity %s is passphrase-protected", path)
	pass, err := readPassphrase(fmt.Sprintf("enter the passphrase for %s: ", filepath.Base(path)))
	if err != nil {
		return nil, passphraseError(path, err)
	}
	sid, err := age.NewScryptIdentity(pass)
	if err != nil {
		return nil, fmt.Errorf("restore: unlocking %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("restore: unlocking %s: %w", path, err)
	}
	defer f.Close()
	var in io.Reader = f
	// age -d accepts both container formats, so both are accepted here.
	br := bufio.NewReader(f)
	if first, _ := br.Peek(len(armorHeader)); strings.HasPrefix(string(first), armorHeader) {
		in = armor.NewReader(br)
	} else {
		in = br
	}
	r, err := age.Decrypt(in, sid)
	if err != nil {
		return nil, fmt.Errorf("restore: could not unlock %s — wrong passphrase, or that file is not a passphrase-protected age identity: %w", path, err)
	}
	ids, err := age.ParseIdentities(r)
	if err != nil {
		return nil, fmt.Errorf("restore: %s decrypted, but what came out is not an age identity: %w", path, err)
	}
	o.UI.OK("identity unlocked — this command will not ask again")
	return ids, nil
}

// armorHeader is the first line of an ASCII-armored age file.
const armorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"

// PrepareImage makes one disc's plaintext squashfs image available on disk and
// returns its path.
//
// The sequence is deliberate, and every step of it exists because skipping it
// would let corrupted data reach a restored file tree:
//
//  1. An already decrypted image in the restore directory is reused — but
//     only after it has been hashed and matched against the recorded
//     plaintext hash. Anything else there is discarded and decrypted afresh.
//  2. The ciphertext is hashed and compared with the hash recorded beside it.
//  3. On a mismatch par2 repairs it — and the hash is checked again. If it
//     still does not match, this refuses to decrypt garbage rather than
//     handing unsquashfs an image that will extract silently wrong files.
//  4. The image is decrypted, and the plaintext hash is compared with the one
//     recorded at backup time. A mismatch removes the plaintext again.
//
// The returned string is a path and only a path. No subprocess output can reach
// it, which is the bug brb.sh worked around with a global variable.
func PrepareImage(ctx context.Context, o Options, encPath string) (string, error) {
	if err := o.check(); err != nil {
		return "", err
	}
	if encPath == "" {
		return "", errors.New("restore: no encrypted image given")
	}
	name := filepath.Base(encPath)
	base := strings.TrimSuffix(name, ageExt)
	if base == name {
		return "", fmt.Errorf("restore: %s is not an %s file", encPath, ageExt)
	}
	encDir := filepath.Dir(encPath)

	// Both staging directories this touches have to be real directories of
	// ours: restore/ receives the plaintext, and enc/ is where a par2 repair
	// rewrites the ciphertext — and where a planted link could substitute
	// ciphertext of somebody else's choosing for this set's.
	restoreDir := o.dirs().Restore
	if err := o.secureStaging(o.dirs().Enc, restoreDir); err != nil {
		return "", err
	}
	plain := filepath.Join(restoreDir, base)

	switch reused, err := o.reuseDecrypted(ctx, encDir, base, plain); {
	case err != nil:
		return "", err
	case reused:
		return plain, nil
	}

	st, err := os.Stat(encPath)
	if err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}

	ids, err := o.identities()
	if err != nil {
		return "", err
	}

	// The ciphertext check and the decryption both have to read the whole
	// multi-gigabyte file, so on the common, clean path they are fused into one
	// pass: the ciphertext is hashed as age reads it, and the plaintext is only
	// promoted once the digest has matched the recorded one. Only when the
	// digest does not match does this fall back to the slow path — par2 repair,
	// re-hash, decrypt again — which costs one wasted read on a disc that is
	// actually damaged, the rare case.
	cipherWant, haveCipherSum, err := o.recordedCipherSum(encDir, base)
	if err != nil {
		return "", err
	}

	var sum string
	if !haveCipherSum {
		if err := o.checkCiphertextNoSum(ctx, encDir, base); err != nil {
			return "", err
		}
		o.UI.Step("decrypting %s", name)
		prog := o.UI.NewProgress("decrypting "+base, st.Size())
		sum, err = agecrypt.Decrypt(ctx, encPath, plain, ids, prog.Writer())
		prog.Done()
		if err != nil {
			return "", fmt.Errorf("restore: decrypting %s: %w", name, err)
		}
	} else {
		o.UI.Step("checking %s against its recorded hash", name)
		o.UI.Step("decrypting %s", name)
		prog := o.UI.NewProgress("decrypting "+base, st.Size())
		sum, err = o.decryptVerifying(ctx, encPath, plain, cipherWant, ids, prog.Writer())
		prog.Done()
		switch {
		case err == nil:
			o.UI.Step("%s matches its recorded hash", name)
		case errors.Is(err, errCiphertextDamaged):
			// The refusal already happened: decryptVerifying never promoted the
			// plaintext. Repair the ciphertext — or let HL-2 bless it — and only
			// then decrypt again, the two-pass way.
			o.UI.Warn("%s does not match its recorded hash", name)
			if err := o.repairCiphertext(ctx, encDir, base, cipherWant); err != nil {
				return "", err
			}
			o.UI.Step("decrypting %s", name)
			prog = o.UI.NewProgress("decrypting "+base, st.Size())
			sum, err = agecrypt.Decrypt(ctx, encPath, plain, ids, prog.Writer())
			prog.Done()
			if err != nil {
				return "", fmt.Errorf("restore: decrypting %s: %w", name, err)
			}
		case ctx.Err() != nil:
			return "", err
		default:
			return "", fmt.Errorf("restore: decrypting %s: %w", name, err)
		}
	}

	want, ok, err := recordedSum(filepath.Join(encDir, base+sumExt), base, o.sidecarRepairHint(discOfImage(base)))
	if err != nil {
		// The plaintext is on disk and nothing has checked it. Leaving it there
		// would let the "already decrypted, therefore already verified" branch
		// above hand it over unchecked on the very next run — the run made
		// straight after repairing the sidecar this error is about.
		o.removeUnverified(plain)
		return "", err
	}
	switch {
	case !ok:
		o.UI.Warn("no recorded hash for the decrypted %s; age's own authentication is the only check performed", base)
	case !strings.EqualFold(want, sum):
		o.removeUnverified(plain)
		// age authenticates as it decrypts, so the ciphertext is intact and the
		// two candidates are the image that was encrypted and the 170-byte
		// sidecar recording its hash. The sidecar is the one with its own
		// parity, so say how to put it back before concluding the worst.
		return "", fmt.Errorf("restore: decrypted image %s does not match the hash in %s; "+
			"the ciphertext decrypted cleanly, so either the image is not what was backed up "+
			"or that sidecar has rotted — %s, then retry",
			base, base+sumExt, o.sidecarRepairHint(discOfImage(base)))
	default:
		o.UI.Step("%s matches its recorded plaintext hash", base)
	}
	return plain, nil
}

// reuseDecrypted decides whether a plaintext image already sitting at plain
// can stand in for decrypting encDir/base.age again. It can only when it
// hashes to the plaintext digest recorded in encDir/base.sha512.
//
// This used to trust anything at that path, on the strength of every failed
// check removing what it had decrypted. That reasoning covered this program's
// own leftovers and nothing else: the restore directory is shared by every set
// that passes through this staging area, so a disc02.squashfs left by an
// earlier restore of a different archive — KeepImages, or a run that stopped
// before it deleted its images — was handed over as this set's disc 2 and
// extracted over the destination without a byte of it being checked. Hashing
// costs one read of the image, which is what the decryption it saves would
// have cost anyway.
//
// A mismatch is not an error and neither is a sidecar that is missing or
// unreadable: in every such case the leftover is removed and the caller
// decrypts afresh, and the fresh plaintext meets the same sidecar and the same
// verdict PrepareImage has always given it.
func (o Options) reuseDecrypted(ctx context.Context, encDir, base, plain string) (bool, error) {
	switch _, err := os.Lstat(plain); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("restore: %s: %w", plain, err)
	}
	o.UI.Step("verifying the decrypted %s already in %s", base, filepath.Dir(plain))
	want, ok, err := recordedSum(filepath.Join(encDir, base+sumExt), base, "")
	if err != nil || !ok {
		o.UI.Warn("no usable recorded plaintext hash to check the existing %s against; discarding it and decrypting again", base)
		o.removeUnverified(plain)
		return false, nil
	}
	got, err := agecrypt.SumFile(ctx, plain)
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		o.UI.Warn("could not hash the existing %s (%v); discarding it and decrypting again", base, err)
		o.removeUnverified(plain)
		return false, nil
	}
	if !strings.EqualFold(got, want) {
		o.UI.Warn("the existing %s does not match the recorded plaintext hash of this set's %s — "+
			"it is left over from something else; discarding it and decrypting again", base, base+ageExt)
		o.removeUnverified(plain)
		return false, nil
	}
	o.UI.Step("reusing the decrypted %s — it matches its recorded plaintext hash", plain)
	return true, nil
}

// removeUnverified deletes a decrypted image that no check has vouched for.
// Every path that fails after the decryption must actually do so, so that
// nothing unverified is left where the next run finds it — reuseDecrypted
// re-hashes whatever it finds, but a plaintext known to be wrong has no
// business waiting around for that.
func (o Options) removeUnverified(plain string) {
	if err := os.Remove(plain); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.UI.Warn("could not remove the unverified %s: %v", plain, err)
	}
}

// recordedCipherSum reads the ciphertext hash recorded beside the image at
// backup time. haveSum is false when nothing usable was recorded — including
// when the sidecar exists but no longer parses, which is HL-2 in its likeliest
// form: the flipped byte landed in the digest and says nothing about the
// image. Refusing over that would throw away a multi-gigabyte ciphertext over
// 150 bytes that have their own parity, so it warns and falls through exactly
// as if no hash had been written: par2 becomes the authority on the
// ciphertext, and the decrypted image is still checked against its own .sha512
// afterwards.
func (o Options) recordedCipherSum(encDir, base string) (want string, haveSum bool, err error) {
	name := base + ageExt
	hint := o.sidecarRepairHint(discOfImage(base))
	want, haveSum, err = recordedSum(filepath.Join(encDir, name)+sumExt, name, hint)
	if errors.Is(err, errSidecarUnreadable) {
		o.UI.Warn("%s cannot be read, so it is the sidecar that is corrupt, not %s", name+sumExt, name)
		o.UI.Warn("  (%s)", hint)
		return "", false, nil
	}
	return want, haveSum, err
}

// checkCiphertextNoSum decides what to do about an image with no recorded
// ciphertext hash. brb.sh treats "no recorded hash" the same as "damaged" and
// refuses to continue without par2. Saying a file is damaged when nothing has
// checked it is wrong, so instead: use par2 when it is there, and otherwise
// proceed, because age's authenticated encryption will fail loudly on a
// corrupted ciphertext anyway.
func (o Options) checkCiphertextNoSum(ctx context.Context, encDir, base string) error {
	name := base + ageExt
	o.UI.Warn("no recorded hash for %s", name)
	if _, err := os.Stat(filepath.Join(encDir, name) + par2Ext); err != nil {
		o.UI.Warn("no par2 data either; relying on age's authentication to detect damage")
		return nil
	}
	if !o.Tools.Has(tools.Par2) {
		o.UI.Warn("par2 is not installed to check it either; relying on age's authentication to detect damage")
		return nil
	}
	o.UI.Step("checking %s with par2 instead", name)
	return o.par2Repair(ctx, encDir, name)
}

// repairCiphertext handles an image whose ciphertext did not match its
// recorded hash: par2 repairs it, and the hash is checked again.
func (o Options) repairCiphertext(ctx context.Context, encDir, base, want string) error {
	name := base + ageExt
	encPath := filepath.Join(encDir, name)
	if _, err := os.Stat(encPath + par2Ext); err != nil {
		return fmt.Errorf("restore: %s is damaged and has no par2 recovery data in %s; ingest another copy of that disc and retry", name, encDir)
	}
	if err := o.par2Repair(ctx, encDir, name); err != nil {
		return err
	}
	got, err := agecrypt.SumFile(ctx, encPath)
	if err != nil {
		return fmt.Errorf("restore: hashing %s after repair: %w", name, err)
	}
	if !strings.EqualFold(got, want) {
		// HL-2. par2 has just checked the ciphertext against its own recovery
		// data and pronounced it whole, so the file the hash comes from is the
		// suspect: a 150-byte sidecar rots exactly as readily as a 22 GB image,
		// and unlike the image it is covered by sidecars.par2. Failing here
		// would abandon a provably byte-for-byte correct image over a hash file
		// — brb.sh warns and continues, and so does this.
		//
		// Nothing is decrypted on trust. age authenticates the ciphertext as it
		// reads it, and the plaintext is compared with discNN.squashfs.sha512
		// below, so a genuinely wrong image is still caught and removed.
		o.UI.Warn("%s passes par2 but not its %s sidecar — the sidecar is what is corrupt, not the image", name, sumExt)
		o.UI.Warn("  (%s)", o.sidecarRepairHint(discOfImage(base)))
		return nil
	}
	o.UI.OK("repaired %s", name)
	return nil
}

// errCiphertextDamaged marks a fused decrypt that refused itself because the
// ciphertext's digest did not match the recorded one. The caller falls back to
// the repair path; nothing half-decrypted survives.
var errCiphertextDamaged = errors.New("ciphertext does not match its recorded hash")

// decryptVerifying decrypts encPath into dst while hashing the ciphertext in
// the same read, so a clean restore reads each image once instead of twice.
// The plaintext goes to dst+".part" and is renamed into place only after the
// ciphertext digest has matched wantCipher; on a mismatch — whether age failed
// mid-stream or decrypted cleanly against a wrong recording — the partial
// plaintext is removed and errCiphertextDamaged comes back, so nothing is ever
// promoted ahead of the check the two-pass path would have made first.
func (o Options) decryptVerifying(ctx context.Context, encPath, dst, wantCipher string, ids []age.Identity, prog io.Writer) (plainSum string, err error) {
	if len(ids) == 0 {
		return "", agecrypt.ErrNoIdentities
	}
	in, err := os.Open(encPath)
	if err != nil {
		return "", fmt.Errorf("restore: open %s: %w", encPath, err)
	}
	defer in.Close()

	cipher := sha512.New()
	tee := io.TeeReader(in, cipher)
	r, ageErr := age.Decrypt(tee, ids...)

	part := dst + partExt
	var copyErr error
	plain := sha512.New()
	promoted := false
	var out *os.File
	if ageErr == nil {
		// 0600: this is the decrypted image, in a staging directory whose
		// default lives under /var/tmp. The ciphertext, its sidecars and the
		// par2 volumes stay 0644 — those modes end up on a disc.
		//
		// createFresh, never O_TRUNC: a symlink another local user planted at
		// the .part path would otherwise have the plaintext streamed into a
		// file of their choosing with this process's privileges.
		out, err = createFresh(part, 0o600)
		if err != nil {
			return "", fmt.Errorf("restore: %w", err)
		}
		defer func() {
			if !promoted {
				out.Close()
				os.Remove(part)
			}
		}()
		sinks := []io.Writer{out, plain}
		if prog != nil {
			sinks = append(sinks, prog)
		}
		copyErr = copyChunks(ctx, io.MultiWriter(sinks...), r)
	}

	if ageErr != nil || copyErr != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// age gave up — a damaged header, or a failed authentication mid-stream.
		// Finish hashing what it never read: the digest is what tells damage,
		// which par2 can fix, from a key that does not open this file, which it
		// cannot. Repairing a 20 GB image to rediscover a wrong key would be a
		// long way round to the same error.
		if err := copyChunks(ctx, io.Discard, tee); err != nil {
			return "", fmt.Errorf("restore: reading %s: %w", encPath, err)
		}
		if !strings.EqualFold(hex.EncodeToString(cipher.Sum(nil)), wantCipher) {
			return "", errCiphertextDamaged
		}
		if ageErr != nil {
			return "", ageErr
		}
		return "", copyErr
	}

	// age stops at the end of its own framing; any trailing bytes are part of
	// the file on disk and belong in its digest.
	if err := copyChunks(ctx, io.Discard, tee); err != nil {
		return "", fmt.Errorf("restore: reading %s: %w", encPath, err)
	}
	if !strings.EqualFold(hex.EncodeToString(cipher.Sum(nil)), wantCipher) {
		return "", errCiphertextDamaged
	}

	if err := out.Sync(); err != nil {
		return "", fmt.Errorf("restore: syncing %s: %w", part, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("restore: closing %s: %w", part, err)
	}
	if err := os.Rename(part, dst); err != nil {
		return "", fmt.Errorf("restore: renaming %s: %w", part, err)
	}
	promoted = true
	return hex.EncodeToString(plain.Sum(nil)), nil
}

// copyChunks copies src into dst in fixed-size chunks, checking ctx between
// chunks so a multi-gigabyte stream aborts promptly on cancellation.
func copyChunks(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, copyBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// par2Repair runs par2 over one file's recovery set. par2 verifies before it
// repairs, so this doubles as the integrity check when no hash was recorded.
//
// Any ".copy<time>" files ingest staged from further pressings of the same
// disc are passed along as extra operands: par2 can combine two differently
// damaged copies into a whole file, but only ones named on its command line.
// This is the archive's last line of defence — two burns that each rotted past
// their own redundancy may still hold every block between them.
func (o Options) par2Repair(ctx context.Context, encDir, name string) error {
	if err := o.Tools.Require(tools.Par2); err != nil {
		return fmt.Errorf("restore: %s needs par2 but it is not installed: %w", name, err)
	}
	o.UI.Warn("attempting par2 repair of %s", name)
	extras := altCopies(encDir, name)
	if len(extras) > 0 {
		o.UI.Step("naming %d alternate copy(ies) for par2 to combine", len(extras))
	}
	log := o.logWriter()
	defer log.Close()
	if err := o.Tools.Par2Repair(ctx, encDir, name+par2Ext, log, extras...); err != nil {
		return fmt.Errorf("restore: par2 could not repair %s; if you burned a second copy of the set, ingest that disc into %s too and retry: %w", name, encDir, err)
	}
	return nil
}

// altCopies lists the "<name>.copy<time>" files ingest staged beside a file,
// relative to encDir, sorted. Read with ReadDir rather than a glob so that a
// staging path containing glob metacharacters cannot hide them.
func altCopies(encDir, name string) []string {
	ents, err := os.ReadDir(encDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), name+".copy") {
			out = append(out, e.Name())
		}
	}
	return out
}

// errSidecarUnreadable marks a .sha512 file that exists but cannot be parsed,
// so that a caller with something better to appeal to — par2 over the
// ciphertext — can go on to ask it instead of failing outright.
var errSidecarUnreadable = errors.New("sidecar unreadable")

// recordedSum reads a single-entry sha512sum file and returns the digest it
// records for name. ok is false when the file does not exist, or when it exists
// but says nothing about that name. A file that cannot be parsed is an error:
// silently ignoring it would turn a checked restore into an unchecked one.
//
// This is the likeliest way a rotted sidecar shows itself. One flipped byte
// usually lands in the 128-character digest and leaves a file that no longer
// parses at all, rather than one that parses and disagrees — so the way out
// belongs here as much as in the mismatch paths, and hint carries it.
func recordedSum(sumPath, name, hint string) (hex string, ok bool, err error) {
	sums, err := agecrypt.ReadSumFile(sumPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		if hint == "" {
			hint = "repair it from parity: par2 repair -- " + sidecarsPar2
		}
		return "", false, fmt.Errorf("restore: reading %s: that sidecar is damaged — %s, then retry: %w: %w",
			sumPath, hint, errSidecarUnreadable, err)
	}
	for k, v := range sums {
		if filepath.Base(k) == name {
			return v, true, nil
		}
	}
	return "", false, nil
}

// logWriter returns a writer that turns a subprocess's output into dim step
// lines. The caller must Close it to flush a trailing partial line.
func (o Options) logWriter() *stepWriter { return &stepWriter{p: o.UI} }

// stepWriter forwards whole lines written to it to a Printer as step output.
// It is safe for concurrent use because a subprocess's stdout and stderr may
// share one.
type stepWriter struct {
	mu   sync.Mutex
	p    *ui.Printer
	part []byte
}

// Write implements io.Writer. It never reports an error: losing a log line must
// not fail an operation.
func (w *stepWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part = append(w.part, b...)
	for {
		seg, rest, ok := tools.SplitLogSegment(w.part)
		if !ok {
			break
		}
		w.part = rest
		if seg != nil {
			w.emit(string(seg))
		}
	}
	// Guard against a tool that reports progress without ever ending a line.
	if len(w.part) > tailLimit {
		w.part = w.part[:0]
	}
	return len(b), nil
}

// Close flushes a trailing line that never got its newline.
func (w *stepWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.part) > 0 {
		w.emit(string(bytes.TrimRight(w.part, "\r")))
		w.part = w.part[:0]
	}
	return nil
}

// emit prints one line, dropping blanks. The caller holds w.mu.
func (w *stepWriter) emit(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.p.Step("%s", line)
}

// tailLimit bounds how much of a failed command's output an error carries.
const tailLimit = 2 << 10

// runTool executes one short-lived helper program, returning its standard
// output. The context is honoured: the child is interrupted and then killed.
// The exit status is always checked, never swallowed.
func runTool(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = killGrace
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %w%s", filepath.Base(path), err, tail(errb.String()))
	}
	return out.String(), nil
}

// tail renders the last of a failed command's output for an error message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > tailLimit {
		s = s[len(s)-tailLimit:]
	}
	return ": " + s
}

// firstLine returns the first non-blank line of s, trimmed.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}
