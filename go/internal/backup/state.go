package backup

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/scan"
	"github.com/jzbz/brb/internal/ui"
)

// StateVersion is the schema version written into state.json. LoadState
// refuses a file written by a different version rather than guessing at its
// meaning: the file decides which files a resumed run will skip, so a
// misreading of it would silently leave data out of the archive.
const StateVersion = 1

// ErrStateMismatch is returned when a resume is attempted against a state file
// that describes a different archive or a different source directory.
var ErrStateMismatch = errors.New("backup: resume state does not match the configuration")

// State is the resume record written to <Staging>/state.json after every
// completed disc.
type State struct {
	// Version is the schema version; see StateVersion.
	Version int `json:"version"`
	// Archive is the archive name the set is being built under.
	Archive string `json:"archive"`
	// Source is the directory being backed up.
	Source string `json:"source"`
	// ArchiveRaw and SourceRaw carry Archive and Source byte for byte whenever
	// they are not valid UTF-8, as AssignedRaw does for Assigned (see there):
	// base64 of the exact bytes, absent otherwise. checkResume compares both
	// with the configuration byte for byte, so a SOURCE_DIR named in, say,
	// Latin-1 came back from state.json with U+FFFD where its accented letters
	// had been, and every resume of that set was refused as belonging to
	// another source — with the configuration unchanged, and no setting that
	// could satisfy it. A derived ARCHIVE_NAME inherits the directory's bytes
	// and failed the same way.
	ArchiveRaw string `json:"archive_raw,omitempty"`
	SourceRaw  string `json:"source_raw,omitempty"`
	// Started is when the first disc of the set began, RFC 3339.
	Started string `json:"started"`
	// DiscsDone is the number of discs fully written, encrypted and protected.
	DiscsDone int `json:"discs_done"`
	// Assigned holds every relative path already written to a disc.
	Assigned []string `json:"assigned"`
	// AssignedRaw carries the same list, byte for byte, whenever Assigned
	// cannot: base64 of the paths joined by NUL, which no path may contain.
	//
	// A Unix filename is a byte string, not text. JSON strings are text, and
	// Go's encoder substitutes U+FFFD for every byte sequence that is not
	// valid UTF-8 — so a file named with, say, a stray 0xFF came back from
	// state.json under a name that matches nothing in the source tree, and
	// every resumed run wrote it to yet another disc, forever. It is written
	// only when it is needed, so a state file for an ordinary tree is
	// unchanged and stays readable at a glance.
	//
	// Like every *Raw field it exists in the file and nowhere else: SaveState
	// computes it on the copy it encodes, and LoadState folds it back into
	// Assigned and clears it. Kept on the live State it was a second copy of
	// every assigned path — 4/3 of their bytes, over a hundred megabytes for a
	// million ordinary-length paths once one of them held a stray byte — held
	// for the rest of the run with nothing reading it, since every save
	// rebuilds it anyway.
	AssignedRaw string `json:"assigned_raw,omitempty"`
	// Skeleton records every non-file entry a finished disc holds, as a kind
	// letter, a space and the path: "d Photos" for a directory, "l
	// Photos/latest" for a symlink, and "o" for the fifos, sockets and device
	// nodes the scan does not tell apart. It is the union over every run that
	// finished a disc, because each disc carries the whole skeleton of the
	// scan that built it, and an entry a later scan no longer sees is still on
	// the discs an earlier one wrote. resumeFilter refuses a resume that would
	// give one of these paths a different kind, as Assigned lets it do for the
	// regular files; resumeFilter says why. Sorted, so the same tree writes the
	// same file. SkeletonRaw is to it what AssignedRaw is to Assigned.
	//
	// Absent from a state file written before the field existed, and from one
	// rewritten since by a brb that predates it: StateVersion is unchanged, so
	// such a brb resumes the set, ignores the field and drops it from its next
	// save. Either way resumeFilter can still work out the directories that
	// hold an assigned file, which are the ones a change is most likely to
	// break, and nothing else of the earlier discs; from the first disc a
	// newer brb finishes, its own scan's skeleton is recorded alongside them.
	Skeleton    []string `json:"skeleton,omitempty"`
	SkeletonRaw string   `json:"skeleton_raw,omitempty"`
	// PackRatio is the compressed/raw ratio in force, which the shrink-retry
	// loop may have raised and the adaptive estimator may have lowered.
	PackRatio float64 `json:"pack_ratio"`
	// MeasuredRatios holds the ratio every finished disc achieved, oldest
	// first. It is the adaptive estimator's whole memory: without it a resumed
	// run would plan its next disc from the configured guess again, having
	// already measured the answer. Absent from a state file written before the
	// estimator existed, which is why a resume falls back to PackRatio alone.
	MeasuredRatios []float64 `json:"measured_ratios,omitempty"`
	// ScanRawSize is the raw byte total the scan reported, recorded so a
	// resumed run can notice the source tree changed size underneath it.
	ScanRawSize int64 `json:"scan_raw_size"`
	// PublicArchive records that the set is being built under PUBLIC_ARCHIVE,
	// and PublicKey is the age recipient it is encrypted to. Both exist so a
	// resume can refuse to change its mind: the mode is a per-invocation flag,
	// and a resume that dropped it would silently encrypt the rest of the set
	// to the operator's ordinary recipients while the finished discs stay on
	// the archive's own key — half a set unlabelled and the other half
	// unopenable. PublicKey additionally lets the resume prove the key file it
	// reloads is the one the finished discs were actually encrypted to.
	// Absent from an ordinary set's state file, and from every state file
	// written before the mode existed, which reads correctly as "not public".
	PublicArchive bool   `json:"public_archive,omitempty"`
	PublicKey     string `json:"public_key,omitempty"`
	// Recipients holds the age public keys of AGE_RECIPIENTS_FILE as they were
	// when the set was started, sorted. A resume re-reads that file, and a
	// file that changed in between — `init-key --rescue-key` appending a key
	// mid-campaign is the likely way — would split the set: discs 1..N open
	// with the old keys only, discs N+1.. with the new, while MANIFEST.txt
	// attributes the current keys to every disc. Recording them lets the resume
	// notice and refuse. Absent for a public archive, whose single recipient
	// is PublicKey, and from every state file written before the field
	// existed, which a resume then cannot check and does not pretend to.
	Recipients []string `json:"recipients,omitempty"`
	// DiscType, CapacityBytes, ReserveBytes and Par2Redundancy pin the disc
	// geometry the finished discs were packed to. Every one of them feeds the
	// image budget, and a resume that silently accepted a change went on to
	// build the remaining discs to a different size — a DISC_TYPE=bd50 disc in
	// a bd25 set is discovered by the drive, and a raised PAR2_REDUNDANCY fails
	// hours later at the disc-directory size check with no way back. Recorded
	// as CapacityBytes rather than DISC_CAPACITY_BYTES so a set built with an
	// override and one built with the equivalent DISC_TYPE compare equal, which
	// is the only equality that matters. CapacityBytes is always positive when
	// the four were recorded, so it doubles as the "was recorded" flag for a
	// state file written before they existed.
	DiscType       string `json:"disc_type,omitempty"`
	CapacityBytes  int64  `json:"capacity_bytes,omitempty"`
	ReserveBytes   int64  `json:"reserve_bytes,omitempty"`
	Par2Redundancy int    `json:"par2_redundancy,omitempty"`
}

// Geometry is the disc geometry a State pins; see State.DiscType. It is a
// value type so the runner can build one from its configuration and the state
// can compare it with the one it recorded.
type Geometry struct {
	DiscType       string
	CapacityBytes  int64
	ReserveBytes   int64
	Par2Redundancy int
}

// setGeometry records the geometry a fresh set is packed to.
func (s *State) setGeometry(g Geometry) {
	s.DiscType, s.CapacityBytes = g.DiscType, g.CapacityBytes
	s.ReserveBytes, s.Par2Redundancy = g.ReserveBytes, g.Par2Redundancy
}

// checkGeometry rejects a resume whose disc geometry differs from the one the
// finished discs were packed to. Every difference is named with both values,
// because the fix is to put the setting back, not to guess which one moved.
// A state file that recorded no geometry (CapacityBytes == 0: written before
// the fields existed) cannot be checked and is accepted.
func (s *State) checkGeometry(g Geometry) error {
	if s.CapacityBytes == 0 {
		return nil
	}
	var diffs []string
	if s.DiscType != g.DiscType {
		diffs = append(diffs, fmt.Sprintf("DISC_TYPE was %s, is now %s", s.DiscType, g.DiscType))
	}
	if s.CapacityBytes != g.CapacityBytes {
		diffs = append(diffs, fmt.Sprintf("disc capacity was %d bytes, is now %d bytes "+
			"(DISC_TYPE or DISC_CAPACITY_BYTES changed)", s.CapacityBytes, g.CapacityBytes))
	}
	if s.ReserveBytes != g.ReserveBytes {
		diffs = append(diffs, fmt.Sprintf("RESERVE_BYTES was %d, is now %d", s.ReserveBytes, g.ReserveBytes))
	}
	if s.Par2Redundancy != g.Par2Redundancy {
		diffs = append(diffs, fmt.Sprintf("PAR2_REDUNDANCY was %d, is now %d", s.Par2Redundancy, g.Par2Redundancy))
	}
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the disc geometry changed since this set was started (%s); the %d disc(s) "+
		"already written were packed to the recorded settings and cannot be re-sized, so the set "+
		"cannot be continued under the new ones — restore the recorded settings to resume, or start over",
		ErrStateMismatch, strings.Join(diffs, "; "), s.DiscsDone)
}

// setRecipients records the sorted public keys a fresh ordinary set is
// encrypted to; see State.Recipients.
func (s *State) setRecipients(pubkeys []string) {
	s.Recipients = sortedStrings(pubkeys)
}

// checkRecipients rejects a resume whose AGE_RECIPIENTS_FILE no longer holds
// exactly the keys the finished discs were encrypted to. The comparison is
// order-insensitive — the file's line order is not part of the set — but a key
// added or removed is: either way the discs still to be written would open
// with a different set of keys from the ones already on the shelf, while the
// manifest lists one set for all of them. A state that recorded no keys (a
// public archive, or a file written before the field existed) is accepted.
func (s *State) checkRecipients(pubkeys []string) error {
	if len(s.Recipients) == 0 {
		return nil
	}
	now := sortedStrings(pubkeys)
	if strings.Join(now, "\n") == strings.Join(s.Recipients, "\n") {
		return nil
	}
	return fmt.Errorf("%w: AGE_RECIPIENTS_FILE changed since this set was started; the %d disc(s) already "+
		"written are encrypted to:\n    %s\n  and the file now holds:\n    %s\n  continuing would encrypt the "+
		"remaining discs to a different set of keys while MANIFEST.txt attributes the current keys to every "+
		"disc; restore the recipients file to the recorded keys and resume, or start over",
		ErrStateMismatch, s.DiscsDone, strings.Join(s.Recipients, "\n    "), joinOrNone(now))
}

// joinOrNone renders a key list for a message, saying so when it is empty.
func joinOrNone(keys []string) string {
	if len(keys) == 0 {
		return "(no keys)"
	}
	return strings.Join(keys, "\n    ")
}

// newState returns the state a fresh run starts from.
func newState(archive, source string, ratio float64, started time.Time) *State {
	return &State{
		Version:   StateVersion,
		Archive:   archive,
		Source:    source,
		Started:   started.Format(time.RFC3339),
		Assigned:  []string{},
		PackRatio: ratio,
	}
}

// LoadState reads a state file. A missing file is reported as an error
// wrapping fs.ErrNotExist, so callers can tell "nothing to resume" from "the
// state is unreadable".
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: reading resume state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("backup: parsing resume state %s: %w", path, err)
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("backup: resume state %s has version %d, this brb writes version %d",
			path, s.Version, StateVersion)
	}
	if s.DiscsDone < 0 {
		return nil, fmt.Errorf("backup: resume state %s reports %d completed discs", path, s.DiscsDone)
	}
	if s.DiscsDone > 0 && len(s.Assigned) == 0 {
		return nil, fmt.Errorf("backup: resume state %s reports %d completed disc(s) but lists no files",
			path, s.DiscsDone)
	}
	if err := s.restoreRaw(); err != nil {
		return nil, fmt.Errorf("backup: resume state %s: %w", path, err)
	}
	// Parsed here as well as where it is used, so a damaged entry is reported
	// as a damaged state file before anything is decided from it.
	if _, err := s.skeletonKinds(); err != nil {
		return nil, fmt.Errorf("backup: resume state %s: %w", path, err)
	}
	return &s, nil
}

// rawSep separates the entries of a raw list. NUL is the one byte a Unix path
// cannot hold, which is what makes the encoding unambiguous.
const rawSep = "\x00"

// rawList is the value of a *Raw field for list: base64 of its entries joined
// by NUL when at least one of them is not valid UTF-8, and "" otherwise, so a
// state file for an ordinary tree carries no raw field at all.
func rawList(list []string) string {
	for _, p := range list {
		if !utf8.ValidString(p) {
			return base64.StdEncoding.EncodeToString([]byte(strings.Join(list, rawSep)))
		}
	}
	return ""
}

// rawText is rawList for a single name: one entry joins to itself, so the
// encoding is the same, and restoreRawText decodes it without splitting.
func rawText(s string) string { return rawList([]string{s}) }

// restoreRaw replaces each field that has a byte-exact form recorded with that
// form, then clears the raw fields, so the live State holds one copy of every
// path and not two. A file that disagrees with itself is refused rather than
// guessed at: Assigned decides which files a resumed run skips, Skeleton which
// changes it refuses, and Archive and Source whether the state belongs to this
// set at all, so misreading any of them leaves data out of the archive.
func (s *State) restoreRaw() error {
	var err error
	if s.Assigned, err = restoreRawList("assigned", s.Assigned, s.AssignedRaw); err != nil {
		return err
	}
	if s.Skeleton, err = restoreRawList("skeleton", s.Skeleton, s.SkeletonRaw); err != nil {
		return err
	}
	if s.Archive, err = restoreRawText("archive", s.Archive, s.ArchiveRaw); err != nil {
		return err
	}
	if s.Source, err = restoreRawText("source", s.Source, s.SourceRaw); err != nil {
		return err
	}
	s.AssignedRaw, s.SkeletonRaw, s.ArchiveRaw, s.SourceRaw = "", "", "", ""
	return nil
}

// restoreRawList returns the list a raw form records, or list itself when no
// raw form was written. The two are written together from one slice, so a raw
// form holding a different number of entries means the file was edited or
// truncated.
func restoreRawList(field string, list []string, raw string) ([]string, error) {
	if raw == "" {
		return list, nil
	}
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s_raw is not valid base64: %w", field, err)
	}
	out := strings.Split(string(blob), rawSep)
	if len(out) != len(list) {
		return nil, fmt.Errorf("%s_raw lists %d name(s) but %s lists %d", field, len(out), field, len(list))
	}
	return out, nil
}

// restoreRawText is restoreRawList for a single name.
func restoreRawText(field, text, raw string) (string, error) {
	if raw == "" {
		return text, nil
	}
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("%s_raw is not valid base64: %w", field, err)
	}
	return string(blob), nil
}

// skeletonTag is the letter a Skeleton entry records k under; see
// State.Skeleton. A regular file has none, because Assigned records those.
func skeletonTag(k scan.Kind) (byte, bool) {
	switch k {
	case scan.KindDir:
		return 'd', true
	case scan.KindSymlink:
		return 'l', true
	case scan.KindOther:
		return 'o', true
	}
	return 0, false
}

// skeletonKind is skeletonTag the other way round.
func skeletonKind(tag byte) (scan.Kind, bool) {
	switch tag {
	case 'd':
		return scan.KindDir, true
	case 'l':
		return scan.KindSymlink, true
	case 'o':
		return scan.KindOther, true
	}
	return 0, false
}

// skeletonKinds parses Skeleton into a lookup from path to kind. An entry that
// does not parse is refused, not skipped: skipping it would quietly drop the
// check that stands between a resume and a set that cannot be restored.
func (s *State) skeletonKinds() (map[string]scan.Kind, error) {
	m := make(map[string]scan.Kind, len(s.Skeleton))
	for i, ent := range s.Skeleton {
		k, ok := scan.Kind(0), false
		if len(ent) > 2 && ent[1] == ' ' {
			k, ok = skeletonKind(ent[0])
		}
		if !ok {
			return nil, fmt.Errorf("skeleton entry %d (%q) is not a kind letter, a space and a path", i+1, ent)
		}
		m[ent[2:]] = k
	}
	return m, nil
}

// writtenKinds is the kind every non-file path on a finished disc has there:
// the recorded Skeleton, plus every directory that holds an assigned file.
// Those directories are in the Skeleton of whichever run wrote the file, so
// for a current state file they add nothing; for one written before Skeleton
// existed they are all there is to check against. They cost about one lookup
// per assigned file, because a directory already present has its ancestors
// present too.
func (s *State) writtenKinds() (map[string]scan.Kind, error) {
	m, err := s.skeletonKinds()
	if err != nil {
		return nil, err
	}
	for _, p := range s.Assigned {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if _, ok := m[d]; ok {
				break
			}
			m[d] = scan.KindDir
		}
	}
	return m, nil
}

// recordSkeleton sets Skeleton to kinds — what the finished discs hold, from
// writtenKinds, or nil on a fresh run — plus every non-file entry of a scan
// whose skeleton is about to go onto another disc. kinds is used up: each path
// the scan names is deleted from it as the scan's entry is added, so what is
// left is what the discs hold and the tree has since lost. Where both name a
// path they agree, because resumeFilter records nothing until it has refused
// every case where they do not.
func (s *State) recordSkeleton(kinds map[string]scan.Kind, entries []scan.Entry) {
	out := make([]string, 0, len(kinds))
	for _, e := range entries {
		if tag, ok := skeletonTag(e.Kind); ok {
			out = append(out, string(tag)+" "+e.Rel)
			delete(kinds, e.Rel)
		}
	}
	for p, k := range kinds {
		if tag, ok := skeletonTag(k); ok {
			out = append(out, string(tag)+" "+p)
		}
	}
	sort.Strings(out)
	s.Skeleton = out
}

// SaveState writes a state file atomically: a temporary file in the same
// directory is fsynced and then renamed over the target, so an interrupted
// write can never leave a truncated state behind.
func SaveState(path string, s *State) (err error) {
	if s == nil {
		return errors.New("backup: SaveState: no state given")
	}
	if s.Version == 0 {
		s.Version = StateVersion
	}
	// The raw forms go on a shallow copy, which shares every slice with s, so
	// the caller's State never holds them; see State.AssignedRaw. Computing
	// them afresh from the lists is also what keeps a stale one from outliving
	// a change to its list.
	onDisk := *s
	onDisk.AssignedRaw, onDisk.SkeletonRaw = rawList(s.Assigned), rawList(s.Skeleton)
	onDisk.ArchiveRaw, onDisk.SourceRaw = rawText(s.Archive), rawText(s.Source)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("backup: creating temporary state file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	// This is not streamed, whatever an Encoder looks like. Encode marshals
	// the whole State into a buffer, re-indents that into a second one for
	// SetIndent, and only then hands the writer the finished document in a
	// single Write — in Go 1.26 and in 1.27's jsonv2-backed encoder alike. So
	// it goes straight to the file: a bufio.Writer in between batched nothing,
	// and only made the code look as if it streamed. Assigned grows by a
	// disc's worth of paths after every disc and the whole list is rewritten
	// each time, so on a million-path set each save holds a few hundred
	// megabytes of transient heap, on the order of json.MarshalIndent. An
	// encoder that truly streamed would have to reproduce these bytes exactly
	// (TestSaveStateBytesAreUnchanged), which is more change than the saving
	// has yet been worth.
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err = enc.Encode(&onDisk); err != nil {
		return fmt.Errorf("backup: writing resume state %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("backup: syncing %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("backup: closing %s: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("backup: installing %s: %w", path, err)
	}
	fsx.SyncDir(dir)
	return nil
}

// checkResume rejects a state file that belongs to a different backup. Skipping
// files because some other archive already contains them would produce a set
// that silently omits data, so the two identifying fields must match exactly.
func (s *State) checkResume(archive, source string) error {
	if s.Source != source {
		return fmt.Errorf("%w: state was recorded for source %q, configuration says %q",
			ErrStateMismatch, s.Source, source)
	}
	if s.Archive != archive {
		// The hint is pasted into a config file, so the name goes through
		// ShellWord as every other pasted line does: a name derived from a
		// SOURCE_DIR such as "My Documents" printed bare had brb.sh's source
		// run the second word as a command.
		return fmt.Errorf("%w: state was recorded for archive %q, configuration says %q "+
			"(set ARCHIVE_NAME=%s to continue that set)",
			ErrStateMismatch, s.Archive, archive, ui.ShellWord(s.Archive))
	}
	return nil
}

// checkPublicMode rejects a resume that would change whether the set is a
// public archive. Either direction splits the set between two recipient sets:
// see the PublicArchive field. Kept apart from checkResume so the message can
// name the flag to pass rather than the config key to edit.
func (s *State) checkPublicMode(public bool) error {
	switch {
	case s.PublicArchive && !public:
		return fmt.Errorf("%w: this set was started as a PUBLIC archive (key %s) but this run is not; "+
			"resume it with --public-archive (or PUBLIC_ARCHIVE=1), or start over",
			ErrStateMismatch, s.PublicKey)
	case !s.PublicArchive && public:
		return fmt.Errorf("%w: this set was started as an ordinary archive, encrypted to AGE_RECIPIENTS_FILE; "+
			"--public-archive cannot be added to a set mid-way, because the discs already written would "+
			"not open with the key it publishes; drop the flag to resume, or start over",
			ErrStateMismatch)
	}
	return nil
}

// assignedSet returns the recorded paths as a lookup set.
func (s *State) assignedSet() map[string]struct{} {
	m := make(map[string]struct{}, len(s.Assigned))
	for _, p := range s.Assigned {
		m[p] = struct{}{}
	}
	return m
}
