package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/jzbz/brb/internal/agecrypt"
	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/doc"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// preflight proves the run can succeed before it starts costing hours: the
// tools exist and are new enough, the recipients file has a key, xorriso
// accepts the options this program uses, staging exists and has room, and the
// operator has been told that staging holds plaintext.
func (r *runner) preflight(ctx context.Context) error {
	r.p.Log("preflight")

	if err := r.cfg.Validate(); err != nil {
		return fmt.Errorf("backup: configuration: %w", err)
	}
	// Weigh the per-disc tool copy before anything is built, not after every
	// image is written. See checkReserve.
	if err := r.checkReserve(); err != nil {
		return err
	}
	if err := r.requireTools(ctx); err != nil {
		return err
	}
	if err := r.loadRecipients(); err != nil {
		return err
	}
	if err := r.tools.ProbeISO(ctx); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	r.p.Step("xorriso options verified")

	if err := r.makeDirs(); err != nil {
		return err
	}
	// After makeDirs, which has just proved this process owns the tree, and
	// before anything is written into it. Released by Run's defer.
	if err := r.lockStaging(); err != nil {
		return err
	}
	if err := r.checkSpace(); err != nil {
		return err
	}
	r.warnPar2Cost()
	if os.Geteuid() != 0 {
		// Precisely what happens: the scan opens every regular file, reports the
		// ones it cannot read, and leaves them out of the set. Nothing is
		// "skipped" quietly — mksquashfs would have written an empty file in
		// place of each and said nothing, which is why the scan checks first.
		r.p.Warn("not running as root: files you cannot read are reported by the scan and left " +
			"out of the set (they will NOT be on any disc), and ownership is recorded as yours")
	}
	if err := r.prepareState(ctx); err != nil {
		return err
	}
	// After prepareState: a public archive's key is minted for a fresh set and
	// reloaded for a resumed one, and only then can the round-trip identity be
	// chosen, because under PUBLIC_ARCHIVE it IS that key.
	if err := r.preparePublicIdentity(); err != nil {
		return err
	}
	if err := r.loadIdentity(); err != nil {
		return err
	}
	return r.confirmStaging()
}

// requireTools checks the external programs and the two capabilities brb
// depends on. age is not among them: it is used as a library.
func (r *runner) requireTools(ctx context.Context) error {
	if err := r.tools.Require(tools.Mksquashfs, tools.Unsquashfs, tools.Par2, tools.Xorriso); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if !r.tools.MksquashfsHasCpioStyle0(ctx) {
		return errors.New("backup: this mksquashfs has no -cpiostyle0; squashfs-tools 4.5 or newer is required")
	}
	if !tools.NoCompression(r.cfg.Compression) {
		if comps := r.tools.MksquashfsCompressors(ctx); len(comps) > 0 && !contains(comps, r.cfg.Compression) {
			r.p.Warn("compressor %q is not built into this mksquashfs (it offers: %s)",
				r.cfg.Compression, strings.Join(comps, ", "))
		}
	}
	// mksquashfs takes -Xcompression-level for zstd and gzip and not for the
	// others, and says nothing when the flag is absent — so a run configured
	// for xz at level 19 silently uses the default. Say so.
	if r.cfg.CompressionLevel != 0 && !tools.NoCompression(r.cfg.Compression) &&
		!tools.LevelApplies(r.cfg.Compression) {
		r.p.Warn("COMPRESSION_LEVEL=%d is ignored for %s: mksquashfs takes -Xcompression-level "+
			"for zstd, gzip and lzo only", r.cfg.CompressionLevel, r.cfg.Compression)
	}
	for _, t := range r.tools.All() {
		if t.Found && t.Version != "" {
			r.p.Step("%s", t.Version)
		}
	}
	return nil
}

// loadRecipients reads the age public keys every image will be encrypted to.
//
// Under PUBLIC_ARCHIVE it reads nothing: the recipient is the archive's own
// key, which preparePublicIdentity mints or reloads once prepareState has
// decided whether this run is fresh or resumed.
func (r *runner) loadRecipients() error {
	if r.cfg.PublicArchive {
		return nil
	}
	path := r.cfg.AgeRecipientsFile
	recs, err := agecrypt.ParseRecipientsFile(path)
	if err != nil {
		return fmt.Errorf("backup: recipients file %s: %w (run 'brb init-key' to create one)", path, err)
	}
	if len(recs) == 0 {
		return fmt.Errorf("backup: recipients file %s has no age1... keys", path)
	}
	keys, err := pubkeyStrings(path, recs)
	if err != nil {
		return err
	}
	// agecrypt.ProbeRecipients says why the set is probed at all. This used to
	// call a second copy of it kept here, so a check added to agecrypt's — the
	// recipient count age's decoder allows, agecrypt.MaxRecipients — would have
	// passed by the one path that writes discs.
	if err := agecrypt.ProbeRecipients(recs); err != nil {
		if errors.Is(err, agecrypt.ErrTooManyRecipients) {
			// Removing keys is the way out only for a set not yet under way.
			// A resume is held to the keys its discs were encrypted to
			// (checkRecipients), so for one that advice would only have
			// traded this refusal for that one.
			if r.opts.Resume {
				return fmt.Errorf("backup: recipients file %s: %w — a resumed set must keep the keys its "+
					"discs were encrypted to, so this one cannot go on: start it over with no more than %d "+
					"(a disc already encrypted to this file opens only with age 1.3.1 or earlier, which had "+
					"no such limit)", path, err, agecrypt.MaxRecipients)
			}
			return fmt.Errorf("backup: recipients file %s: %w — remove the keys you no longer need, "+
				"and re-run (a disc already encrypted to this file opens only with age 1.3.1 or "+
				"earlier, which had no such limit)", path, err)
		}
		return fmt.Errorf("backup: recipients file %s: age refuses to encrypt to this set of keys: %w — "+
			"a set may not mix post-quantum (age1pq1...) and classic (age1...) recipients; edit the file so "+
			"it holds one kind only, and re-run", path, err)
	}
	r.recipients, r.pubkeys = recs, keys
	r.p.OK("recipients: %d key(s) from %s", len(recs), path)
	return nil
}

// preparePublicIdentity establishes the keypair a public archive is encrypted
// to: minted for a fresh set, reloaded for a resumed one. It runs after
// prepareState, because which of the two it must do is exactly what
// prepareState decides.
//
// The key is generated for this one archive. AGE_RECIPIENTS_FILE is
// deliberately not consulted, and neither is AGE_IDENTITY: publishing a key the
// operator already uses would retroactively expose every other set encrypted to
// it, turning one flag into a disclosure of unrelated backups. A fresh key can
// only ever disclose the archive it was made for.
//
// THE KEY IS WRITTEN TO STAGING HERE, BEFORE ANY DISC IS BUILT. It used to live
// only in this process's memory until buildDiscDirs put it on the discs at the
// very end of a completed run — so a run interrupted mid-set took its key to
// the grave, and the resumed run minted a new one, encrypted the remaining
// discs to that, and stamped it onto every disc directory, including the ones
// whose images were encrypted to the dead key. Those discs verified clean and
// were permanently undecryptable. Persisting the key first, and recording the
// mode and public key in state.json so a resume can check both, is what
// closes that.
//
// Encrypting to a key that is then shipped in the clear is, in cryptographic
// terms, no encryption at all. That is the intent — the ciphertext, par2 layout
// and both readers stay exactly as they are, so a public set is an ordinary set
// that happens to carry its own key, rather than a second on-disc format.
func (r *runner) preparePublicIdentity() error {
	if !r.cfg.PublicArchive {
		return nil
	}
	path := r.publicIdentityPath()
	resumed := r.st != nil && r.st.DiscsDone > 0

	var id *age.X25519Identity
	if resumed {
		// prepareState has already refused a mode mismatch, so the state says
		// this is a public set. Its key must be exactly the persisted one:
		// discs 1..N are encrypted to it and nothing else can open them.
		var err error
		id, err = agecrypt.ReadX25519IdentityFile(path)
		if err != nil {
			return fmt.Errorf("backup: --resume: this set is a public archive but its key cannot be read "+
				"from %s: %w — the %d disc(s) already written are encrypted to that key and nothing else "+
				"can open them, so the set cannot be continued without it; start over", path, err, r.st.DiscsDone)
		}
		got := id.Recipient().String()
		if got != r.st.PublicKey {
			return fmt.Errorf("backup: --resume: %s holds the key for %s but the resume state recorded %s; "+
				"the file was replaced since the set was started, so continuing would encrypt the "+
				"remaining discs to a key that opens none of the finished ones; start over",
				path, got, r.st.PublicKey)
		}
		r.p.OK("public archive resumed with its recorded key %s", got)
	} else {
		var err error
		id, err = agecrypt.GenerateIdentity()
		if err != nil {
			return fmt.Errorf("backup: public archive: %w", err)
		}
		// startFresh removed any stale copy, so O_EXCL here can only trip on a
		// concurrent run in the same staging, which is a refusal worth having.
		if err := agecrypt.WriteIdentityFile(path, id); err != nil {
			return fmt.Errorf("backup: public archive: persisting the key: %w", err)
		}
		r.st.PublicArchive = true
		r.st.PublicKey = id.Recipient().String()
		r.p.Warn("PUBLIC_ARCHIVE: this set will NOT be confidential")
		r.p.Step("a keypair was generated for this archive alone, and its secret key")
		r.p.Step("is written to identity.txt on every disc, so anyone holding a disc")
		r.p.Step("can read it. Nothing here protects the contents.")
		r.p.Step("public key: %s", id.Recipient())
		r.p.Step("key kept in %s until the set is finished", path)
	}

	r.publicIdentity = id
	r.recipients = []age.Recipient{id.Recipient()}
	r.pubkeys = []string{id.Recipient().String()}
	return nil
}

// publicIdentityPath is where a public archive's key lives in staging for the
// life of the set: beside the ciphertext, under the same name it carries on
// the discs.
func (r *runner) publicIdentityPath() string {
	return filepath.Join(r.dirs.Enc, doc.PublicIdentityName)
}

// pubkeyStrings returns the public keys of recs as text, for the manifest and
// the resume state's pin. They are taken from the recipients encryption will
// use, never from a second read of the file: that is what this used to do,
// and a file changed between the two opens — init-key --rescue-key appending
// a key is the writer brb itself provides — left the images encrypted to one
// set of keys while MANIFEST.txt and state.json recorded another, so the
// manifest could name a rescue key as opening discs it cannot open, and every
// later resume checked against keys nothing had been encrypted to.
//
// Re-encoding a key gives back the very line it was parsed from, which is what
// the manifest needs: it tells a restorer which key opens the set, and a key
// spelled differently is one they cannot compare with their own. age accepts
// a recipient line only as canonical lowercase bech32 with nothing around it
// — mixed case, a stray space and a bad checksum are all refused at parse —
// and String() encodes the same bytes the same way, post-quantum keys
// included.
func pubkeyStrings(path string, recs []age.Recipient) ([]string, error) {
	keys := make([]string, len(recs))
	for i, rc := range recs {
		s, ok := rc.(fmt.Stringer)
		if !ok {
			// No age recipient type lacks one today; a future one is refused
			// here, in preflight, rather than recorded as nothing.
			return nil, fmt.Errorf("backup: recipients file %s: key %d (%T) has no printable form "+
				"to record in the manifest", path, i+1, rc)
		}
		keys[i] = s.String()
	}
	return keys, nil
}

// loadIdentity resolves the secret key used by the optional round-trip
// verification. A run that asked for the verification and cannot do it is an
// error: quietly skipping a check the operator requested is how a bad set gets
// burned.
func (r *runner) loadIdentity() error {
	if !r.opts.VerifyRoundTrip {
		return nil
	}
	// A public archive is encrypted to its own minted key and to nothing else,
	// so that is the only identity that can round-trip it. Reading the
	// operator's AGE_IDENTITY here made --verify-roundtrip fail on disc 1 —
	// after mksquashfs, encrypt and par2 — for the one mode whose premise is
	// that no pre-existing key is involved.
	if r.cfg.PublicArchive {
		if r.publicIdentity == nil {
			return errors.New("backup: --verify-roundtrip: public archive key not prepared (internal ordering error)")
		}
		r.identities = []age.Identity{r.publicIdentity}
		r.p.Step("round-trip verification enabled, using the archive's own key")
		return nil
	}
	path := r.cfg.AgeIdentity
	if path == "" {
		path = filepath.Join(filepath.Dir(r.cfg.AgeRecipientsFile), "identity.txt")
	}
	ids, err := agecrypt.ParseIdentityFile(path)
	if err != nil {
		return fmt.Errorf("backup: --verify-roundtrip needs a readable age identity: %s: %w "+
			"(set AGE_IDENTITY=/path/to/identity.txt)", path, err)
	}
	r.identities = ids
	r.p.Step("round-trip verification enabled, identity %s", path)
	return nil
}

// makeDirs creates the staging tree and secures every directory in it. The
// rules, and why each of them is there, are [fsx.SecureDir]'s; this decides
// what to point them at.
//
// The root goes first, and is the one that matters most: STAGING defaults to a
// path under /var/tmp, which is world-writable, so it is the one directory in
// the tree that another local account can create ahead of the operator. Once
// it is proven to be a real directory, mode 0700, owned by this process,
// nothing else can be planted below it.
//
// The five subdirectories are then secured too, rather than merely created,
// for two reasons. Staging is deliberately reused — a resumed run finds the
// whole tree already there — so these are as often inherited as made, and an
// inherited directory can have any mode, any owner, or be a symlink the
// operator pointed at a bigger volume; the reader refuses exactly that at enc/
// and restore/, and a writer that accepted it would be the same defect in the
// other implementation. And what they hold is worth the check on its own:
// img/ holds unencrypted squashfs images until each one is encrypted and
// verified, enc/ the ciphertext, and work/ the plaintext index. The resume
// state is in none of them: it is state.json in the root itself, one more
// reason the root goes first. Its Assigned list is what a continued run
// consults to decide which files it may skip, and a tampered state file does
// not corrupt a set loudly; it makes the set quietly incomplete.
//
// This runs before checkSpace, which measures the filesystem under STAGING,
// and before prepareState, which reads <STAGING>/state.json — so nothing is
// measured through a symlink and no resume decision is taken on a file another
// account could have written.
func (r *runner) makeDirs() error {
	if err := fsx.SecureStaging(r.cfg.Staging,
		r.dirs.Work, r.dirs.Img, r.dirs.Enc, r.dirs.Discs, r.dirs.ISO); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// lockStaging takes the staging lock for the life of this run, so a second brb
// cannot write into the same tree at the same time. See [fsx.LockStaging] for
// why that matters and why the lock is an flock rather than a pid file.
func (r *runner) lockStaging() error {
	lock, err := fsx.LockStaging(r.cfg.Staging)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	r.lock = lock
	return nil
}

// releaseStaging drops the staging lock. It is safe to call on a runner that
// never took one — a dry run, or a preflight that failed before locking.
func (r *runner) releaseStaging() {
	if r.lock == nil {
		return
	}
	if err := r.lock.Release(); err != nil {
		r.p.Warn("could not release the staging lock: %v", err)
	}
	r.lock = nil
}

// checkSpace refuses to start a multi-hour job that cannot finish one disc.
func (r *runner) checkSpace() error {
	avail, err := freeSpace(r.cfg.Staging)
	if err != nil {
		return err
	}
	need := RequiredSpace(r.budget.Image, r.cfg.Par2Redundancy)
	if avail < need {
		return fmt.Errorf("backup: not enough free space in %s: need %s, have %s "+
			"(one plaintext image and, beside it, one ciphertext with %d%% parity — "+
			"the plaintext is not removed until the ciphertext is written and verified)",
			r.cfg.Staging, ui.HumanBytes(need), ui.HumanBytes(avail), r.cfg.Par2Redundancy)
	}
	perDisc := r.budget.Image * int64(100+r.cfg.Par2Redundancy) / 100
	r.p.Step("staging free space: %s available, %s needed to start", ui.HumanBytes(avail), ui.HumanBytes(need))
	// An ISO is a second copy of its disc, so whether the mode builds them is
	// the difference between N and about 2.2N of staging for the whole campaign.
	if r.cfg.ISOMode.Eager() {
		r.p.Step("ISO_MODE=eager: each finished disc leaves about %s of ciphertext plus an ISO "+
			"of the same size in %s until you clear it", ui.HumanBytes(perDisc), r.cfg.Staging)
	} else {
		r.p.Step("each finished disc leaves about %s of ciphertext in %s until you clear it; "+
			"its ISO is built when you burn it and removed again afterwards",
			ui.HumanBytes(perDisc), r.cfg.Staging)
	}
	return nil
}

// warnPar2Cost says up front that the recovery data, not the imaging, is what
// will consume the night.
//
// Preflight's job is to prove the run can succeed before it starts costing
// hours, and it checked tools, keys, xorriso and free space while saying
// nothing about the step that is normally 75-85% of a disc's wall time — most
// of an hour per disc on BD-25 media, and hours over a twenty-disc set.
//
// It deliberately prints no duration. par2's cost is not the closed form it
// looks like: measured here at a fixed 3000 blocks, 60 MiB and 240 MiB of input
// took the same 16.7 s while 1 GiB took 39.5 s, so the block count dominates
// until the input outgrows par2's memory budget and then the input does — and
// the per-core speed of the machine sets the scale of both. Any formula cheap
// enough to run in preflight is wrong by a factor that would mislead rather
// than inform. The number the operator actually needs is printed by protect()
// after each real disc, from a stopwatch.
func (r *runner) warnPar2Cost() {
	geom := config.Par2GeometryFor(r.budget.Image, r.cfg.Par2Blocks, r.cfg.Par2Redundancy)
	r.p.Step("recovery data is usually the longest step of a disc by a wide margin: %d recovery "+
		"block(s) over %d block(s) of a %s image, and each finished disc reports what it took",
		geom.RecoveryBlocks, geom.Blocks, ui.HumanBytes(r.budget.Image))
	r.p.Step("PAR2_BLOCKS is the knob: halving it roughly halves that time, and halves the " +
		"number of independent damage sites the parity survives")
}

// prepareState loads or creates the resume record and reconciles it with what
// is actually in staging.
func (r *runner) prepareState(ctx context.Context) error {
	indexPath := filepath.Join(r.dirs.Work, indexFileName)

	old, err := LoadState(r.statePath)
	switch {
	case err == nil:
		// fall through to the checks below
	case errors.Is(err, fs.ErrNotExist):
		if r.opts.Resume {
			return fmt.Errorf("backup: --resume: no state to resume from at %s", r.statePath)
		}
		return r.startFresh(indexPath)
	default:
		return err
	}

	if !r.opts.Resume {
		if old.DiscsDone > 0 {
			return fmt.Errorf("backup: %s records %d completed disc(s): "+
				"re-run with --resume to continue that set, or remove %s to start over",
				r.statePath, old.DiscsDone, r.cfg.Staging)
		}
		return r.startFresh(indexPath)
	}

	if err := old.checkResume(r.cfg.ArchiveName, r.cfg.SourceDir); err != nil {
		return err
	}
	if old.DiscsDone == 0 {
		return r.startFresh(indexPath)
	}
	// Only once discs exist does the mode matter: before that nothing has been
	// encrypted to anything, and startFresh above simply begins again.
	if err := old.checkPublicMode(r.cfg.PublicArchive); err != nil {
		return err
	}
	// Likewise the keys and the geometry: both are fixed by the discs already
	// on the shelf, and neither can be changed for the ones still to come
	// without splitting the set. loadRecipients has already read the file this
	// run would encrypt to (r.pubkeys is empty for a public archive, whose
	// state recorded no Recipients, so that comparison is vacuous there).
	if err := old.checkRecipients(r.pubkeys); err != nil {
		return err
	}
	if err := old.checkGeometry(r.geometry()); err != nil {
		return err
	}
	if _, err := os.Stat(indexPath); errors.Is(err, fs.ErrNotExist) {
		// Only finish deletes the plaintext index, as the step before it
		// deletes this state file. A resume that finds the state and only the
		// encrypted index is one whose last run stopped between the two, or
		// could not remove the state, and only the later steps are left to do.
		// A run stopped anywhere earlier, even after buildIndex, still has its
		// plaintext index and resumes below, building the encrypted one again.
		if _, encErr := os.Stat(filepath.Join(r.dirs.Enc, indexName)); encErr != nil {
			return fmt.Errorf("backup: --resume: neither %s nor the encrypted index exists, "+
				"so the file-to-disc map is lost; staging is inconsistent, start over", indexPath)
		}
		r.indexBuilt = true
		r.st = old
		r.resumeRatio(old)
		r.p.OK("resuming after %d completed disc(s); the encrypted index is already built", old.DiscsDone)
		return nil
	}
	sum, err := reconcileIndex(ctx, indexPath, old.DiscsDone, r.p.Warn)
	if err != nil {
		return fmt.Errorf("backup: --resume: %w", err)
	}
	if sum.MaxDisc != old.DiscsDone || sum.Lines != len(old.Assigned) {
		return fmt.Errorf("backup: --resume: %s lists %d file(s) across %d disc(s) but %s records "+
			"%d file(s) across %d disc(s); staging is inconsistent, start over",
			indexPath, sum.Lines, sum.MaxDisc, r.statePath, len(old.Assigned), old.DiscsDone)
	}
	r.st = old
	r.resumeRatio(old)
	if old.Started != "" {
		r.p.Step("set started %s", old.Started)
	}
	r.p.OK("resuming after %d completed disc(s), %d file(s) already written, pack ratio %.3f",
		old.DiscsDone, len(old.Assigned), r.packRatio)
	return nil
}

// startFresh sets up the state for a run that begins at disc 1, refusing to
// scribble over a staging directory that already holds encrypted images.
func (r *runner) startFresh(indexPath string) error {
	ages, err := filesMatching(r.dirs.Enc, func(n string) bool { return strings.HasSuffix(n, ".age") })
	if err != nil {
		return err
	}
	if len(ages) > 0 {
		return fmt.Errorf("backup: %s already holds %d encrypted image(s) "+
			"but there is no run to resume; remove %s or point STAGING elsewhere",
			r.dirs.Enc, len(ages), r.cfg.Staging)
	}
	if err := os.Remove(indexPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: removing stale index %s: %w", indexPath, err)
	}
	// A key left by an earlier public set that never got as far as disc 1
	// (nothing was encrypted to it — the .age check above just proved so) must
	// not be picked up by this one, whichever mode this one is in.
	if err := os.Remove(r.publicIdentityPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: removing stale public-archive key %s: %w", r.publicIdentityPath(), err)
	}
	r.st = newState(r.cfg.ArchiveName, r.cfg.SourceDir, r.packRatio, r.started)
	// Pin what a resume must not be allowed to change: the disc geometry the
	// images are packed to, and — for an ordinary set — the keys they are
	// encrypted to. A public archive records its one key as PublicKey instead,
	// in preparePublicIdentity, which runs after this.
	r.st.setGeometry(r.geometry())
	if !r.cfg.PublicArchive {
		r.st.setRecipients(r.pubkeys)
	}
	return nil
}

// geometry is the disc geometry this run packs to, in the form the resume
// state records and checks. Capacity is the effective one (DISC_CAPACITY_BYTES
// when set, else the media type's), so an override and its equivalent media
// type compare equal.
func (r *runner) geometry() Geometry {
	return Geometry{
		DiscType:       r.cfg.DiscType.String(),
		CapacityBytes:  r.cfg.Capacity(),
		ReserveBytes:   r.cfg.ReserveBytes,
		Par2Redundancy: r.cfg.Par2Redundancy,
	}
}

// confirmStaging makes the operator acknowledge that staging holds plaintext.
func (r *runner) confirmStaging() error {
	r.p.Raw("")
	r.p.Warn("staging holds UNENCRYPTED squashfs images while the backup runs")
	r.p.Step("%s should be on an encrypted volume (LUKS), or wiped afterwards", r.cfg.Staging)
	r.p.Step("each image is deleted as soon as it is encrypted and verified")
	r.p.Raw("")
	yes, err := r.p.Confirm("Continue?")
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if !yes {
		return errors.New("backup: aborted")
	}
	return nil
}

// contains reports whether want is in list.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
