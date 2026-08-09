package restore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxManifestLine bounds one MANIFEST.txt line. The manifest lists every file
// on every disc, so a line can be long, but a bound keeps a corrupted or
// truncated manifest from being read into memory without limit.
const maxManifestLine = 1 << 20

// setStatus is what a staging area's encrypted images say about the disc set
// they came from: how many discs the set is supposed to have, how many are
// here, and which numbers are absent.
//
// It exists because of a deliberate property of the on-disc format: every disc
// carries the whole directory skeleton, so restoring two discs of a three-disc
// set produces a tree that looks complete, with empty directories where the
// missing files belong. Nothing about the restored tree reveals the gap, which
// is why the gap has to be announced while the restore is running.
type setStatus struct {
	// Want is the disc count recorded in MANIFEST.txt; 0 when Known is false.
	Want int
	// Have is how many encrypted images were found in the staging area.
	Have int
	// Missing lists, ascending, the disc numbers the manifest names that have
	// no image in staging. It stops at maxNamedMissing entries; MissingCount
	// is the true total.
	Missing []int
	// MissingCount is how many of the discs the manifest names have no image
	// in staging, whether or not they are all listed in Missing.
	MissingCount int
	// Known reports whether a disc count could be read from the manifest at
	// all. When it is false nothing about completeness can be concluded, and
	// Complete answers true so that a restore is never blocked by a manifest
	// that was lost or hand-edited.
	Known bool
}

// maxNamedMissing bounds how many disc numbers a warning spells out.
//
// MANIFEST.txt is read off a disc that may have rotted or been hand-edited,
// and "discs : 3" is one flipped byte away from "discs : 30000000". Naming
// every absent disc of a set that size cost 1.6 GB of memory and a single
// 168 MB warning line; a restore must not be brought down by the bookkeeping
// file it only consults for advice. Past this many, the count is the news.
const maxNamedMissing = 64

// Complete reports whether every disc the manifest names has an image in
// staging. An unreadable manifest counts as complete: a restore must not be
// held up by a missing MANIFEST.txt.
func (s setStatus) Complete() bool { return s.MissingCount == 0 }

// missingList renders Missing the way brb.sh does, space separated, saying so
// rather than going on forever when the manifest claims an implausible set.
func (s setStatus) missingList() string {
	parts := make([]string, len(s.Missing))
	for i, n := range s.Missing {
		parts[i] = strconv.Itoa(n)
	}
	out := strings.Join(parts, " ")
	if rest := s.MissingCount - len(s.Missing); rest > 0 {
		out += fmt.Sprintf(" ... and %d more", rest)
	}
	return out
}

// expectedDiscs returns the disc count recorded in the staging MANIFEST.txt
// that ingest copies off every disc, and whether it could be read at all.
//
// It is deliberately never an error. A set whose manifest was lost, truncated
// or hand-edited must still restore; the caller warns and carries on. The
// parsing matches brb.sh's expected_discs(): the first line beginning "discs",
// optional blanks, a colon, optional blanks, and then a value that must be
// digits and nothing else.
func expectedDiscs(staging string) (int, bool) {
	f, err := os.Open(filepath.Join(staging, manifestName))
	if err != nil {
		return 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4<<10), maxManifestLine)
	for sc.Scan() {
		value, ok := manifestField(sc.Text(), "discs")
		if !ok {
			continue
		}
		// The first "discs" line decides, as brb.sh's `head -1` does: a second
		// one further down is not a fallback but a sign of a damaged manifest.
		if !allDigits(value) {
			return 0, false
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// manifestField splits "name<blanks>:<blanks>value" and returns value. The
// name must start the line, so a file listing that happens to contain the word
// is not mistaken for the field.
func manifestField(line, name string) (string, bool) {
	rest, ok := strings.CutPrefix(line, name)
	if !ok {
		return "", false
	}
	rest = strings.TrimLeft(rest, " \t")
	rest, ok = strings.CutPrefix(rest, ":")
	if !ok {
		return "", false
	}
	return strings.TrimLeft(rest, " \t"), true
}

// allDigits reports whether s is one or more ASCII digits and nothing else,
// which is the test brb.sh applies before believing the field.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkComplete compares the images in the staging area with the disc count
// MANIFEST.txt records, reports what it found, and returns it.
//
// It never fails. A missing or malformed manifest is a warning — the restore
// proceeds, because the alternative is refusing to give back data over a
// bookkeeping file — and an incomplete set is a warning naming exactly which
// disc numbers are absent, plus the consequence in plain words. What the
// caller does about an incomplete set is the caller's decision.
func (o Options) checkComplete(imgs []discFile) setStatus {
	st := setStatus{Have: len(imgs)}
	want, ok := expectedDiscs(o.Cfg.Staging)
	if !ok {
		// brb.sh's wording, except that "no MANIFEST.txt" would be untrue of a
		// manifest that is there and unreadable — a distinction worth making,
		// because one of the two is fixed by ingesting any disc of the set.
		if _, err := os.Stat(filepath.Join(o.Cfg.Staging, manifestName)); err == nil {
			o.UI.Warn("the %s in %s records no usable disc count — cannot tell how many discs this set has",
				manifestName, o.Cfg.Staging)
		} else {
			o.UI.Warn("no %s in %s — cannot tell how many discs this set has", manifestName, o.Cfg.Staging)
		}
		return st
	}
	st.Want, st.Known = want, true

	present := make(map[int]bool, len(imgs))
	inRange := 0
	for _, im := range imgs {
		if im.N >= 1 && im.N <= want && !present[im.N] {
			inRange++
		}
		present[im.N] = true
	}
	st.MissingCount = want - inRange
	// Bounded by the images actually in staging plus maxNamedMissing, so a
	// manifest claiming a billion discs costs a bounded amount of work.
	for n := 1; n <= want && len(st.Missing) < maxNamedMissing; n++ {
		if !present[n] {
			st.Missing = append(st.Missing, n)
		}
	}
	if st.MissingCount == 0 {
		o.UI.OK("all %d disc image(s) present", want)
		return st
	}
	// Word for word what brb.sh prints, so an operator who has read one
	// implementation's output recognises the other's.
	o.UI.Warn("MANIFEST says %d discs; %d present. MISSING: %s", want, len(imgs), st.missingList())
	o.UI.Warn("files on those discs will NOT be restored")
	return st
}

// partialAbortedError is what a declined "restore the partial set anyway?"
// turns into, so the message says what was declined rather than just "aborted".
func partialAbortedError(st setStatus) error {
	return fmt.Errorf("restore: aborted: disc(s) %s of %d are not in the staging area; "+
		"ingest them and retry, or pass --yes to restore what is here and accept that "+
		"the files on those discs will be absent", st.missingList(), st.Want)
}
