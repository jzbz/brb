package backup

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/config"
)

func TestSaveLoadStateRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")

	want := &State{
		Version:     StateVersion,
		Archive:     "home-2026-08-06",
		Source:      "/home/jz",
		Started:     time.Date(2026, 8, 6, 5, 21, 0, 0, time.UTC).Format(time.RFC3339),
		DiscsDone:   3,
		Assigned:    []string{"a/b.txt", "c d/e\tf.bin", "z"},
		PackRatio:   0.625,
		ScanRawSize: 1234567890,
	}
	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Version != want.Version || got.Archive != want.Archive || got.Source != want.Source ||
		got.Started != want.Started || got.DiscsDone != want.DiscsDone ||
		got.PackRatio != want.PackRatio || got.ScanRawSize != want.ScanRawSize {
		t.Fatalf("round trip changed the state:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Assigned) != len(want.Assigned) {
		t.Fatalf("Assigned length = %d, want %d", len(got.Assigned), len(want.Assigned))
	}
	for i := range want.Assigned {
		if got.Assigned[i] != want.Assigned[i] {
			t.Errorf("Assigned[%d] = %q, want %q", i, got.Assigned[i], want.Assigned[i])
		}
	}
}

func TestSaveStateOverwritesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	for i := 1; i <= 3; i++ {
		s := newState("arch", "/src", 1.0, time.Now())
		s.DiscsDone = i
		s.Assigned = []string{"f"}
		if err := SaveState(path, s); err != nil {
			t.Fatalf("SaveState %d: %v", i, err)
		}
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.DiscsDone != 3 {
		t.Errorf("DiscsDone = %d, want 3", got.DiscsDone)
	}
	// No temporary files may be left behind.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "state.json" {
		t.Errorf("staging directory holds %d entries, want just state.json", len(ents))
	}
}

func TestSaveStateFillsInTheVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, &State{Archive: "a", Source: "/s"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Version != StateVersion {
		t.Errorf("Version = %d, want %d", got.Version, StateVersion)
	}
}

func TestSaveStateRejectsNil(t *testing.T) {
	t.Parallel()
	if err := SaveState(filepath.Join(t.TempDir(), "state.json"), nil); err == nil {
		t.Fatal("SaveState(nil) succeeded, want an error")
	}
}

func TestLoadStateErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string // "" means: do not create the file
		wantIs  error
		wantSub string
	}{
		{name: "missing file", wantIs: fs.ErrNotExist},
		{name: "not json", content: "{", wantSub: "parsing resume state"},
		{
			name:    "wrong version",
			content: `{"version":99,"archive":"a","source":"/s"}`,
			wantSub: "version 99",
		},
		{
			name:    "negative discs",
			content: `{"version":1,"archive":"a","source":"/s","discs_done":-1}`,
			wantSub: "-1 completed discs",
		},
		{
			name:    "discs without files",
			content: `{"version":1,"archive":"a","source":"/s","discs_done":2,"assigned":[]}`,
			wantSub: "lists no files",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := LoadState(path)
			if err == nil {
				t.Fatal("LoadState succeeded, want an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestCheckResumeHintPastesIntoAConfig: the hint is a line to paste into a
// config file, and an archive name derived from a SOURCE_DIR such as
// "My Documents" carries a space. Printed bare, brb.sh's source ran the second
// word as a command and the Go parser refused the line.
func TestCheckResumeHintPastesIntoAConfig(t *testing.T) {
	t.Parallel()
	const was = "My Documents-2026-10-29"
	st := &State{Version: StateVersion, Archive: was, Source: "/home/u/My Documents"}
	err := st.checkResume("My Documents-2026-10-30", "/home/u/My Documents")
	if err == nil {
		t.Fatal("checkResume succeeded, want a mismatch error")
	}
	_, line, ok := strings.Cut(err.Error(), "(set ")
	line, _, ok2 := strings.Cut(line, " to continue that set)")
	if !ok || !ok2 {
		t.Fatalf("no ARCHIVE_NAME hint in %q", err)
	}
	vals, perr := config.Parse(line+"\n", "hint")
	if perr != nil {
		t.Fatalf("the hint %q does not parse as a config line: %v", line, perr)
	}
	if got := vals["ARCHIVE_NAME"].Scalar; got != was {
		t.Errorf("the hint %q sets ARCHIVE_NAME to %q, want %q", line, got, was)
	}
}

func TestCheckResume(t *testing.T) {
	t.Parallel()
	st := &State{Version: StateVersion, Archive: "home-2026-08-06", Source: "/home/jz"}

	tests := []struct {
		name           string
		archive, src   string
		wantErr        bool
		wantMentioning string
	}{
		{name: "match", archive: "home-2026-08-06", src: "/home/jz"},
		{
			name: "different source", archive: "home-2026-08-06", src: "/srv/photos",
			wantErr: true, wantMentioning: "/srv/photos",
		},
		{
			name: "different archive", archive: "home-2026-08-07", src: "/home/jz",
			wantErr: true, wantMentioning: "ARCHIVE_NAME=home-2026-08-06",
		},
		{
			name: "both differ", archive: "other", src: "/other",
			wantErr: true, wantMentioning: "source",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := st.checkResume(tc.archive, tc.src)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("checkResume: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkResume succeeded, want a mismatch error")
			}
			if !errors.Is(err, ErrStateMismatch) {
				t.Errorf("error %v does not wrap ErrStateMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.wantMentioning) {
				t.Errorf("error %q does not mention %q", err, tc.wantMentioning)
			}
		})
	}
}

// TestCheckGeometryRefusesAChangedSetting pins the mid-set geometry refusal:
// every setting that feeds the image budget is recorded when the set starts,
// and a resume that finds any of them changed stops in preflight, naming the
// setting and both values, instead of building discs to a different size and
// failing hours later at the size check.
func TestCheckGeometryRefusesAChangedSetting(t *testing.T) {
	t.Parallel()
	rec := Geometry{DiscType: "bd25", CapacityBytes: 25025314816, ReserveBytes: 104857600, Par2Redundancy: 10}
	st := &State{Version: StateVersion, DiscsDone: 3}
	st.setGeometry(rec)
	if err := st.checkGeometry(rec); err != nil {
		t.Fatalf("checkGeometry against the recorded geometry: %v", err)
	}

	tests := []struct {
		name string
		now  Geometry
		want []string
	}{
		{
			name: "disc type and capacity",
			now:  Geometry{DiscType: "bd50", CapacityBytes: 50050629632, ReserveBytes: 104857600, Par2Redundancy: 10},
			want: []string{"DISC_TYPE was bd25, is now bd50", "25025314816", "50050629632", "3 disc(s)"},
		},
		{
			name: "capacity override alone",
			now:  Geometry{DiscType: "bd25", CapacityBytes: 24000000000, ReserveBytes: 104857600, Par2Redundancy: 10},
			want: []string{"DISC_CAPACITY_BYTES", "25025314816", "24000000000"},
		},
		{
			name: "reserve",
			now:  Geometry{DiscType: "bd25", CapacityBytes: 25025314816, ReserveBytes: 209715200, Par2Redundancy: 10},
			want: []string{"RESERVE_BYTES was 104857600, is now 209715200"},
		},
		{
			name: "redundancy",
			now:  Geometry{DiscType: "bd25", CapacityBytes: 25025314816, ReserveBytes: 104857600, Par2Redundancy: 15},
			want: []string{"PAR2_REDUNDANCY was 10, is now 15"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := st.checkGeometry(tc.now)
			if err == nil {
				t.Fatal("checkGeometry accepted a changed geometry")
			}
			if !errors.Is(err, ErrStateMismatch) {
				t.Errorf("error %v does not wrap ErrStateMismatch", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			for _, w := range []string{"start over", "restore the recorded settings"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not offer %q", err, w)
				}
			}
		})
	}

	// A state file that recorded no geometry — written before the fields
	// existed — cannot be checked and must not be refused for it.
	old := &State{Version: StateVersion, DiscsDone: 1}
	if err := old.checkGeometry(rec); err != nil {
		t.Errorf("checkGeometry against a state without geometry: %v", err)
	}
}

// TestCheckRecipientsRefusesAChangedFile pins the recipients refusal: a key
// added (init-key --rescue-key mid-campaign) or removed splits the set between
// two recipient sets while the manifest attributes one set to every disc, so
// a resume that finds the file changed stops and says which keys the finished
// discs are encrypted to and which the file holds now.
func TestCheckRecipientsRefusesAChangedFile(t *testing.T) {
	t.Parallel()
	const (
		k1 = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq1"
		k2 = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq2"
		k3 = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq3"
	)
	st := &State{Version: StateVersion, DiscsDone: 2}
	st.setRecipients([]string{k2, k1})
	if got := st.Recipients; len(got) != 2 || got[0] != k1 || got[1] != k2 {
		t.Fatalf("setRecipients recorded %v, want them sorted", got)
	}

	// Same keys, either order: the file's line order is not part of the set.
	for _, same := range [][]string{{k1, k2}, {k2, k1}} {
		if err := st.checkRecipients(same); err != nil {
			t.Errorf("checkRecipients(%v) = %v, want nil", same, err)
		}
	}
	tests := []struct {
		name string
		now  []string
	}{
		{"a key appended", []string{k1, k2, k3}},
		{"a key removed", []string{k1}},
		{"a key replaced", []string{k1, k3}},
		{"the file emptied", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := st.checkRecipients(tc.now)
			if err == nil {
				t.Fatal("checkRecipients accepted a changed recipient set")
			}
			if !errors.Is(err, ErrStateMismatch) {
				t.Errorf("error %v does not wrap ErrStateMismatch", err)
			}
			for _, w := range append([]string{k1, k2, "2 disc(s)", "restore the recipients file", "start over"}, tc.now...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}

	// A public archive records its one key as PublicKey and no Recipients, and
	// so does a state file from before the field existed: nothing to compare.
	if err := (&State{Version: StateVersion, DiscsDone: 1}).checkRecipients([]string{k1}); err != nil {
		t.Errorf("checkRecipients against a state without recipients: %v", err)
	}
}

// TestStateRoundTripsRecipientsAndGeometry: the pins are only worth anything
// if they survive the trip through state.json.
func TestStateRoundTripsRecipientsAndGeometry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	st := newState("arch", "/src", 1.0, time.Now())
	st.DiscsDone, st.Assigned = 1, []string{"a"}
	st.setGeometry(Geometry{DiscType: "bdxl100", CapacityBytes: 100103356416, ReserveBytes: 5, Par2Redundancy: 7})
	st.setRecipients([]string{"age1zzz", "age1aaa"})
	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.DiscType != "bdxl100" || got.CapacityBytes != 100103356416 || got.ReserveBytes != 5 || got.Par2Redundancy != 7 {
		t.Errorf("geometry did not round-trip: %+v", got)
	}
	if len(got.Recipients) != 2 || got.Recipients[0] != "age1aaa" || got.Recipients[1] != "age1zzz" {
		t.Errorf("recipients did not round-trip: %v", got.Recipients)
	}
}

func TestAssignedSet(t *testing.T) {
	t.Parallel()
	st := &State{Assigned: []string{"a", "b", "a"}}
	got := st.assignedSet()
	if len(got) != 2 {
		t.Fatalf("assignedSet has %d entries, want 2", len(got))
	}
	if _, ok := got["a"]; !ok {
		t.Error(`assignedSet is missing "a"`)
	}
}

// TestStateRoundTripsPathsThatAreNotUTF8 pins the one thing the resume record
// exists to do: hand back the exact bytes it was given.
//
// A Unix filename is a byte string. JSON strings are text, and Go's encoder
// replaces every invalid UTF-8 sequence with U+FFFD, so a file named with a
// stray 0xFF used to come back under a name that matched nothing in the source
// tree. The resumed run then decided the file had been deleted, wrote it to a
// fresh disc, and did the same again on the next resume — one more disc, and
// one more index row naming a file that does not exist, every time.
func TestStateRoundTripsPathsThatAreNotUTF8(t *testing.T) {
	paths := []string{
		"plain.txt",
		"nonutf8-\xff\xfe.bin",
		"a\tb.txt",
		"c\nd.txt",
		"e\\f.txt",
		"ünïcøde.txt",
		"lone-continuation-\x80.bin",
	}
	skeleton := []string{"d dir-\xff", "l link-\x80", "o plain-fifo"}
	path := filepath.Join(t.TempDir(), "state.json")
	in := &State{
		Version: StateVersion, Archive: "a", Source: "/s", DiscsDone: 1,
		Assigned: append([]string(nil), paths...), PackRatio: 1,
		Skeleton: append([]string(nil), skeleton...),
	}
	if err := SaveState(path, in); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"assigned_raw"`, `"skeleton_raw"`} {
		if !strings.Contains(string(data), k) {
			t.Errorf("state.json carries no %s for lists that need one:\n%s", k, data)
		}
	}
	// The raw forms are the file's business. Left on the caller's State they
	// were a second copy of every path for the rest of the run.
	if in.AssignedRaw != "" || in.SkeletonRaw != "" {
		t.Errorf("SaveState left raw forms on the caller's State (%d and %d bytes)",
			len(in.AssignedRaw), len(in.SkeletonRaw))
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if out.AssignedRaw != "" || out.SkeletonRaw != "" {
		t.Errorf("LoadState kept raw forms after restoring from them (%d and %d bytes)",
			len(out.AssignedRaw), len(out.SkeletonRaw))
	}
	if !slices.Equal(out.Skeleton, skeleton) {
		t.Errorf("Skeleton = %q, want %q", out.Skeleton, skeleton)
	}
	if !slices.Equal(out.Assigned, paths) {
		for i := range paths {
			if i >= len(out.Assigned) || out.Assigned[i] != paths[i] {
				got := "<missing>"
				if i < len(out.Assigned) {
					got = out.Assigned[i]
				}
				t.Errorf("path %d: got %q, want %q", i, got, paths[i])
			}
		}
	}
	// The set is what resume consults, and it is looked up by exact bytes.
	if _, ok := out.assignedSet()["nonutf8-\xff\xfe.bin"]; !ok {
		t.Error("a resumed run would treat the file as no longer in the source tree")
	}
}

// TestStateRoundTripsASourceAndArchiveThatAreNotUTF8: the two names checkResume
// compares byte for byte with the configuration have to come back byte for
// byte too.
//
// They used to go through JSON as text, so a SOURCE_DIR named in Latin-1 came
// back with U+FFFD in place of each accented letter, and every resume of the
// set was refused as belonging to a different source — with the configuration
// unchanged and no setting that could satisfy the check. The default
// ARCHIVE_NAME is derived from the directory and inherited the same bytes.
func TestStateRoundTripsASourceAndArchiveThatAreNotUTF8(t *testing.T) {
	t.Parallel()
	latin1 := "/data/Fotos_\xe9t\xe9"
	started := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		archive, source string
	}{
		{"source with the derived archive name", config.ArchiveNameFor(latin1, started), latin1},
		{"archive name alone", "arch\xff", "/srv/photos"},
		{"source alone", "photos", latin1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			in := newState(tc.archive, tc.source, 1.0, started)
			in.DiscsDone, in.Assigned = 1, []string{"a"}
			if err := SaveState(path, in); err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			out, err := LoadState(path)
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			if out.Archive != tc.archive || out.Source != tc.source {
				t.Errorf("round trip gave archive %q, source %q; want %q, %q",
					out.Archive, out.Source, tc.archive, tc.source)
			}
			if err := out.checkResume(tc.archive, tc.source); err != nil {
				t.Errorf("a resume under the unchanged configuration was refused: %v", err)
			}
			if out.ArchiveRaw != "" || out.SourceRaw != "" {
				t.Error("LoadState kept the raw forms after restoring from them")
			}
		})
	}
}

// TestStateLeavesOrdinaryFilesAlone: the raw lists are a fallback, not a format
// change. A tree of ordinary names must produce the state file it always did.
func TestStateLeavesOrdinaryFilesAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{Version: StateVersion, Archive: "ärchive", Source: "/s/ünïcøde", DiscsDone: 1,
		Assigned: []string{"a.txt", "sub/ünïcøde.txt"}, Skeleton: []string{"d sub"}, PackRatio: 1}
	if err := SaveState(path, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"assigned_raw", "skeleton_raw", "archive_raw", "source_raw"} {
		if strings.Contains(string(data), k) {
			t.Errorf("state.json carries %s for a tree that does not need it:\n%s", k, data)
		}
	}
}

// TestLoadStateRefusesADisagreeingRawList: a raw form and the field it stands
// for are written together, so a raw form that does not decode, or does not
// hold as many entries as its list, means the file was edited or truncated.
// Guessing which one is right would silently leave files out of the archive,
// or skip the check that keeps the set restorable. So would a skeleton entry
// that names no kind.
func TestLoadStateRefusesADisagreeingRawList(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		name, fields, want string
	}{
		{"assigned_raw short", `"assigned":["a.txt","b.txt"],"assigned_raw":"` + b64("a.txt") + `"`, "assigned_raw lists 1"},
		{"skeleton_raw short", `"assigned":["a.txt"],"skeleton":["d x","d y"],"skeleton_raw":"` + b64("d x") + `"`, "skeleton_raw lists 1"},
		{"assigned_raw not base64", `"assigned":["a.txt"],"assigned_raw":"!!"`, "assigned_raw is not valid base64"},
		{"skeleton_raw not base64", `"assigned":["a.txt"],"skeleton":["d x"],"skeleton_raw":"!!"`, "skeleton_raw is not valid base64"},
		{"archive_raw not base64", `"assigned":["a.txt"],"archive_raw":"!!"`, "archive_raw is not valid base64"},
		{"source_raw not base64", `"assigned":["a.txt"],"source_raw":"!!"`, "source_raw is not valid base64"},
		{"skeleton entry with no kind", `"assigned":["a.txt"],"skeleton":["x what"]`, "skeleton entry 1"},
		{"skeleton entry with no path", `"assigned":["a.txt"],"skeleton":["d "]`, "skeleton entry 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			body := `{"version":1,"archive":"a","source":"/s","discs_done":1,` + tc.fields + `,"pack_ratio":1}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadState(path)
			if err == nil {
				t.Fatalf("LoadState accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestSaveStateBytesAreUnchanged pins the file format to exactly what
// json.MarshalIndent of the State produces, plus a newline. state.json never
// reaches a disc, but it decides which files a resumed run skips, so a brb
// that wrote it any other way — a hand-rolled streaming writer, say, the
// obvious way to cut what a save costs in memory — must still be read by the
// versions either side of it, and they by it.
func TestSaveStateBytesAreUnchanged(t *testing.T) {
	st := &State{
		Version:        StateVersion,
		Archive:        "an archive <with> &markup",
		Source:         "/srv/photos",
		Started:        "2026-08-07T01:00:00Z",
		DiscsDone:      2,
		Assigned:       []string{"a.txt", "dir/b\tb.txt", "\u00e9.bin"},
		PackRatio:      0.421,
		MeasuredRatios: []float64{0.4, 0.44},
		ScanRawSize:    1234567,
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want)+"\n" {
		t.Errorf("state.json is no longer what MarshalIndent produced\ngot:\n%s\nwant:\n%s\n", got, want)
	}
}
