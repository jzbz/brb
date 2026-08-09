// Package iso turns the disc directories a backup laid out into burnable
// ISO 9660 images, and answers the two questions every caller of that has to
// answer first: which discs does this staging area hold, and how many discs is
// the set supposed to have.
//
// It is its own package because an ISO is a full second copy of its disc
// directory. Materialising twenty of them at the end of a backup holds staging
// at roughly 2.2x the compressed set for the whole length of a burn campaign —
// days or weeks — which is why ISO_MODE defaults to ondemand and burn builds
// each image at the moment its disc goes into the drive. Three callers need
// exactly the same per-disc build (backup's eager mode, the iso command, and
// burn), so it lives in none of them.
//
// This package is Unix-only; it reads free space with statfs(2) through
// internal/fsx.
package iso

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// slack is the headroom demanded on top of the source tree before an ISO is
// built, matching brb.sh's 67108864: an ISO is slightly larger than the tree it
// is made from, and a staging filesystem that fills up mid-write leaves a
// truncated image behind.
const slack = 64 << 20

// Options carries the dependencies building an ISO needs.
type Options struct {
	// Cfg is the loaded configuration; required.
	Cfg *config.Config
	// UI receives progress and status output; required.
	UI *ui.Printer
	// Tools is the detected set of external programs; required.
	Tools *tools.Set
	// Version is stamped into each ISO's application id as "brb <version>".
	// Empty omits the id, which costs nothing but the provenance record.
	Version string
}

// check reports a missing dependency rather than letting a nil pointer panic
// part-way through writing an image.
func (o Options) check() error {
	switch {
	case o.Cfg == nil:
		return errors.New("iso: no configuration")
	case o.UI == nil:
		return errors.New("iso: no printer")
	case o.Tools == nil:
		return errors.New("iso: no tool set")
	}
	return nil
}

// Name returns one disc's ISO file name, e.g. "disc07.iso". It is part of the
// staging layout shared with brb.sh.
func Name(n int) string { return fmt.Sprintf("disc%02d.iso", n) }

// dirName returns one disc's staging directory name, e.g. "disc07".
func dirName(n int) string { return fmt.Sprintf("disc%02d", n) }

// Path returns where disc n's ISO lives.
func (o Options) Path(n int) string { return filepath.Join(o.Cfg.Dirs().ISO, Name(n)) }

// sourceDir returns the disc directory disc n's ISO is built from.
func (o Options) sourceDir(n int) string { return filepath.Join(o.Cfg.Dirs().Discs, dirName(n)) }

// BuildOne builds one disc's ISO from the directory already laid out for it,
// overwriting any ISO that is there.
//
// total is the set's disc count and appears in the volume label ("BACKUP_01_OF_
// 20"), so a burn or an iso run started days later must resolve it from
// MANIFEST.txt rather than from whatever is in staging; see Total.
//
// Every check brb.sh makes is made here. There is room for the image before
// xorriso is started; xorriso's exit status is checked rather than swallowed by
// a pipeline; a partial ISO is removed on failure; and the finished file is
// held to both bounds that catch a truncated write — an ISO is never smaller
// than the tree it was built from, and never larger than the media it is going
// onto.
func (o Options) BuildOne(ctx context.Context, n, total int) error {
	if err := o.check(); err != nil {
		return err
	}
	if n < 1 {
		return fmt.Errorf("iso: disc number %d is not a disc number", n)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("iso: aborted: %w", err)
	}
	if err := o.Tools.Require(tools.Xorriso); err != nil {
		return fmt.Errorf("iso: %w", err)
	}

	src := o.sourceDir(n)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return fmt.Errorf("iso: no disc directory at %s — run 'brb backup' first", src)
	}
	isoDir := o.Cfg.Dirs().ISO
	if err := os.MkdirAll(isoDir, 0o700); err != nil {
		return fmt.Errorf("iso: creating %s: %w", isoDir, err)
	}
	out := o.Path(n)

	srcSize, err := fsx.DirBytes(ctx, src)
	if err != nil {
		return fmt.Errorf("iso: %w", err)
	}
	avail, err := fsx.FreeSpace(isoDir)
	if err != nil {
		return fmt.Errorf("iso: %w", err)
	}
	if !RoomFor(avail, srcSize) {
		return fmt.Errorf("iso: not enough space in %s for the disc %d ISO (%s needed, %s free)",
			isoDir, n, ui.HumanBytes(srcSize+slack), ui.HumanBytes(avail))
	}

	label := tools.DiscLabel(o.Cfg.LabelPrefix, n, total)
	appID := ""
	if o.Version != "" {
		appID = "brb " + o.Version
	}
	if err := o.Tools.MakeISO(ctx, tools.ISOOptions{
		Dir:     src,
		Out:     out,
		Label:   label,
		AppID:   appID,
		Publish: o.Cfg.ArchiveName,
		Log:     o.logWriter(),
	}); err != nil {
		// MakeISO has already removed the partial file.
		return fmt.Errorf("iso: disc %d: %w", n, err)
	}

	st, err := os.Stat(out)
	if err != nil {
		return fmt.Errorf("iso: disc %d: %w", n, err)
	}
	size := st.Size()
	capacity := o.Cfg.Capacity()
	switch {
	case size < srcSize:
		// A non-empty file is not the same as a complete one: 6 GiB of a 22 GiB
		// image passes every "is it there" test there is.
		o.remove(out)
		return fmt.Errorf("iso: disc %d ISO is %s but its source tree is %s — truncated",
			n, ui.HumanBytes(size), ui.HumanBytes(srcSize))
	case capacity > 0 && size > capacity:
		o.remove(out)
		return fmt.Errorf("iso: disc %d ISO is %s, larger than the %s media",
			n, ui.HumanBytes(size), ui.HumanBytes(capacity))
	}
	o.UI.Step("%s: %s  label=%s", Name(n), ui.HumanBytes(size), label)
	return nil
}

// RoomFor reports whether avail bytes are enough to write an ISO of a tree of
// srcSize bytes, with brb.sh's 64 MiB of headroom on top. It is exported so the
// rule is testable without filling a filesystem up.
func RoomFor(avail, srcSize int64) bool {
	need := srcSize + slack
	if need < srcSize { // overflow; nothing is ever this large, but do not wrap
		return false
	}
	return avail > need
}

// remove deletes an ISO that failed a check. A file left there would be burned
// by the next run: burn builds only what is missing.
func (o Options) remove(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.UI.Warn("could not remove the unusable %s: %v", filepath.Base(path), err)
	}
}

// Ensure returns the path of disc n's ISO, building it first when it is not
// there. Under ISO_MODE=ondemand nothing has built it yet, and after an earlier
// successful burn it has been deleted again; either way the disc directory is
// still in staging, so this builds from that rather than refusing.
//
// A zero-length file counts as missing. It can only be the remains of a run
// that died between creating the file and writing to it, and rebuilding it is
// strictly better than burning it.
func (o Options) Ensure(ctx context.Context, n, total int) (string, error) {
	if err := o.check(); err != nil {
		return "", err
	}
	path := o.Path(n)
	switch st, err := os.Stat(path); {
	case err == nil && st.Mode().IsRegular() && st.Size() > 0:
		return path, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("iso: %s: %w", path, err)
	}
	if err := o.BuildOne(ctx, n, total); err != nil {
		return "", err
	}
	return path, nil
}

// BuildAll builds the ISOs of discs 1..total, which is what ISO_MODE=eager does
// at the end of a backup. Use Build for a set that is already on disk, where the
// disc numbers are whatever staging actually holds.
func BuildAll(ctx context.Context, o Options, total int) error {
	if err := o.check(); err != nil {
		return err
	}
	o.UI.Log("building ISO images")
	for n := 1; n <= total; n++ {
		if err := o.BuildOne(ctx, n, total); err != nil {
			return err
		}
	}
	o.UI.OK("ISOs in %s", o.Cfg.Dirs().ISO)
	return nil
}

// Build is the `iso` command: materialise the ISOs of the discs matching spec
// and stop, without burning anything. It is the whole of what ISO_MODE=eager
// does at the end of a backup, on demand instead — for an operator who wants
// files to take to another machine or another burner.
func Build(ctx context.Context, o Options, spec string) error {
	if err := o.check(); err != nil {
		return err
	}
	if err := o.Tools.Require(tools.Xorriso); err != nil {
		return fmt.Errorf("iso: %w", err)
	}
	nums, err := DiscNumbers(o.Cfg.Dirs())
	if err != nil {
		return err
	}
	if len(nums) == 0 {
		return fmt.Errorf("iso: no disc directories in %s — run 'brb backup' first", o.Cfg.Dirs().Discs)
	}
	rng, err := ParseRange(spec)
	if err != nil {
		return fmt.Errorf("iso: %w", err)
	}
	total := Total(o.Cfg.Staging, len(nums))

	o.UI.Log("building ISO images")
	built := 0
	for _, n := range nums {
		if !rng.Contains(n) {
			continue
		}
		if err := o.BuildOne(ctx, n, total); err != nil {
			return err
		}
		built++
	}
	if built == 0 {
		return fmt.Errorf("iso: no discs matched %q in %s", spec, o.Cfg.Dirs().Discs)
	}
	o.UI.OK("%d ISO(s) in %s", built, o.Cfg.Dirs().ISO)
	return nil
}

// DiscNumbers returns the disc numbers a staging area holds, in order.
//
// The disc DIRECTORIES are the source of truth, not the ISOs: under
// ISO_MODE=ondemand there may be no ISOs at all yet, and a burned disc's ISO is
// deleted again on the way out of a successful burn. The ISO directory is only
// a fallback, for a staging tree that was carried around as ISOs and nothing
// else. Numbers are reported as they are found, so a set with gaps in it — one
// disc directory deleted, one disc still to be re-made — is simply a shorter
// list rather than an error.
func DiscNumbers(d config.Dirs) ([]int, error) {
	nums, err := numbered(d.Discs, true, "")
	if err != nil {
		return nil, err
	}
	if len(nums) > 0 {
		return nums, nil
	}
	return numbered(d.ISO, false, ".iso")
}

// numbered lists the "disc<digits><suffix>" entries of one directory, sorted
// numerically and de-duplicated. A missing directory yields nothing and no
// error: the caller says "run backup first" more helpfully than a stat error.
func numbered(dir string, wantDir bool, suffix string) ([]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("iso: reading %s: %w", dir, err)
	}
	seen := make(map[int]bool, len(ents))
	var out []int
	for _, e := range ents {
		if e.IsDir() != wantDir {
			continue
		}
		n, ok := discNumberOf(e.Name(), suffix)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

// discNumberOf extracts the disc number from a name of the form
// "disc<digits><suffix>". Any number of digits is accepted so that a set of more
// than 99 discs still sorts and selects correctly.
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

// manifestDiscs matches MANIFEST.txt's "discs : N" line.
var manifestDiscs = regexp.MustCompile(`^discs[ \t]*:[ \t]*([0-9]+)`)

// Total resolves how many discs the set is supposed to have.
//
// The volume label reads "01 OF 20", so a burn or an iso run started in its own
// process — the normal case, days after the backup — needs the set's real size
// and cannot get it by counting what is in staging: a set half burned under
// KEEP_ISOS=0, or one disc directory removed by hand, would relabel the rest of
// the discs. MANIFEST.txt is where backup recorded it. Counting is the fallback
// for a staging tree whose manifest has been lost, and a missing or unreadable
// manifest is never an error: a wrong number on a label must not stop a burn.
func Total(staging string, fallback int) int {
	f, err := os.Open(filepath.Join(staging, "MANIFEST.txt"))
	if err != nil {
		return fallback
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := manifestDiscs.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
		break
	}
	return fallback
}

// Range is a parsed disc selection.
type Range struct {
	// From and To are inclusive.
	From, To int
}

// rangeMax stands in for "no upper bound", matching brb.sh's 9999.
const rangeMax = 9999

// ParseRange reads brb.sh's disc selection syntax: "all", a single number "7",
// a closed range "7-20", or an open one "7-" meaning "seven onwards".
//
// It only parses; nothing here knows which discs exist, so a range naming discs
// that are not in staging is not an error until the caller finds nothing in it.
// That is what lets a set with gaps in its numbering be selected by range.
func ParseRange(spec string) (Range, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return Range{}, errors.New("expected a disc number, a range like 7-20, or 'all'")
	}
	if strings.EqualFold(s, "all") {
		return Range{From: 1, To: rangeMax}, nil
	}
	bad := func() (Range, error) {
		return Range{}, fmt.Errorf("expected a number, a range like 7-20, or 'all' (got %q)", spec)
	}
	from, to := s, s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		from, to = s[:i], s[i+1:]
		if to == "" {
			to = strconv.Itoa(rangeMax)
		}
	}
	f, err := strconv.Atoi(from)
	if err != nil || f < 1 {
		return bad()
	}
	t, err := strconv.Atoi(to)
	if err != nil || t < 1 {
		return bad()
	}
	if t < f {
		return Range{}, fmt.Errorf("range %q ends before it starts", spec)
	}
	return Range{From: f, To: t}, nil
}

// Contains reports whether disc n is in the range.
func (r Range) Contains(n int) bool { return n >= r.From && n <= r.To }

// logWriter turns xorriso's output into dim step lines. internal/tools has
// already split it into whole lines and dropped the progress chatter, so there
// is nothing to buffer here.
func (o Options) logWriter() *stepWriter { return &stepWriter{p: o.UI} }

// stepWriter forwards a subprocess's output to a Printer, one step line per
// line.
type stepWriter struct{ p *ui.Printer }

// Write implements io.Writer. It never reports an error: losing a log line must
// not fail an ISO build.
func (w *stepWriter) Write(b []byte) (int, error) {
	line := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(line) != "" {
		w.p.Step("%s", line)
	}
	return len(b), nil
}
