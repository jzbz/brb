package backup

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
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
	AssignedRaw string `json:"assigned_raw,omitempty"`
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
	if err := s.restoreRawPaths(); err != nil {
		return nil, fmt.Errorf("backup: resume state %s: %w", path, err)
	}
	return &s, nil
}

// rawSep separates paths inside AssignedRaw. NUL is the one byte a Unix path
// cannot hold, which is what makes the encoding unambiguous.
const rawSep = "\x00"

// encodeRawPaths fills AssignedRaw when at least one assigned path is not
// valid UTF-8, and clears it otherwise so the field never lingers with stale
// contents after the list changes.
func (s *State) encodeRawPaths() {
	need := false
	for _, p := range s.Assigned {
		if !utf8.ValidString(p) {
			need = true
			break
		}
	}
	if !need {
		s.AssignedRaw = ""
		return
	}
	s.AssignedRaw = base64.StdEncoding.EncodeToString([]byte(strings.Join(s.Assigned, rawSep)))
}

// restoreRawPaths replaces Assigned with the byte-exact list when one was
// recorded. A file that disagrees with itself is refused rather than guessed
// at: the list decides which files a resumed run will skip, so misreading it
// leaves data out of the archive.
func (s *State) restoreRawPaths() error {
	if s.AssignedRaw == "" {
		return nil
	}
	blob, err := base64.StdEncoding.DecodeString(s.AssignedRaw)
	if err != nil {
		return fmt.Errorf("assigned_raw is not valid base64: %w", err)
	}
	raw := strings.Split(string(blob), rawSep)
	if len(raw) != len(s.Assigned) {
		return fmt.Errorf("assigned_raw lists %d path(s) but assigned lists %d",
			len(raw), len(s.Assigned))
	}
	s.Assigned = raw
	return nil
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
	s.encodeRawPaths()

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
	// Streamed, not marshalled into memory first. Assigned grows by a disc's
	// worth of paths after every disc and the whole list is rewritten each time,
	// so on a multi-million-file tree the last disc of a set was serialising a
	// couple of hundred megabytes into a []byte before touching the file.
	// Encoding straight into the temp file costs one buffer instead.
	w := bufio.NewWriterSize(tmp, 1<<16)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err = enc.Encode(s); err != nil {
		return fmt.Errorf("backup: encoding resume state: %w", err)
	}
	if err = w.Flush(); err != nil {
		return fmt.Errorf("backup: writing %s: %w", tmpName, err)
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
	syncDir(dir)
	return nil
}

// syncDir flushes a directory entry. Not every filesystem permits it, and a
// failure here does not make the rename any less durable in practice, so the
// error is deliberately ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
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
		return fmt.Errorf("%w: state was recorded for archive %q, configuration says %q "+
			"(set ARCHIVE_NAME=%s to continue that set)",
			ErrStateMismatch, s.Archive, archive, s.Archive)
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
