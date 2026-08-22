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
	"github.com/jzbz/brb/internal/disc"
	"github.com/jzbz/brb/internal/fsx"
	"github.com/jzbz/brb/internal/tools"
	"github.com/jzbz/brb/internal/ui"
)

// slack is the headroom demanded on top of the source tree before an ISO is
// built: an ISO is slightly larger than the tree it is made from, and a staging
// filesystem that fills up mid-write leaves a truncated image behind.
//
// The 64 MiB figure is inherited from the shell writer this package replaced.
// That writer is not in this tree — brb.sh here is the READER only (brb.sh:3-8)
// — so nothing cross-checks the number and it is now this build's own
// definition. It is not part of the on-disc format: it only has to cover the
// ISO 9660 metadata a tree of any realistic size adds.
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

// Name returns one disc's ISO file name, e.g. "disc07.iso". The zero-padded
// "disc%02d" stem is the numbering the whole staging layout uses — the reader
// looks for disc07.squashfs.age and sidecars-disc07.par2 by the same rule — but
// the ISO is a burn-time artefact that never reaches a disc, so no reader ever
// names this file.
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
// The four checks below are the ones a shell pipeline around xorriso cannot
// make, and each of them is here because its absence is silent. There is room
// for the image before xorriso is started; xorriso's exit status is checked
// rather than swallowed by a pipeline; a partial ISO is removed on failure; and
// the finished file is held to both bounds that catch a truncated write — an
// ISO is never smaller than the tree it was built from, and never larger than
// the media it is going onto.
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
// srcSize bytes, with [slack]'s 64 MiB of headroom on top. It is exported so
// the rule is testable without filling a filesystem up.
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
// A file that is there is held to the same two bounds BuildOne holds a fresh
// one to: an ISO is never smaller than the disc directory it was built from,
// and never larger than the media this run is configured for. A zero-length
// file, or one that is shorter than its tree, is the
// remains of a run that died while writing it — power loss part-way through a
// 22 GiB xorriso run leaves 6 GiB that passes every "is it there" test — and
// rebuilding it is strictly better than burning it. It used to be enough for
// the file to be non-empty, which is how a truncated ISO got burned.
//
// When the disc directory is gone the bound cannot be measured and the ISO
// cannot be rebuilt either; a non-empty file is then taken as it is, since
// refusing would only leave the operator with nothing at all.
func (o Options) Ensure(ctx context.Context, n, total int) (string, error) {
	if err := o.check(); err != nil {
		return "", err
	}
	path := o.Path(n)
	switch st, err := os.Stat(path); {
	case err == nil && st.Mode().IsRegular() && st.Size() > 0:
		complete, err := o.complete(ctx, n, st.Size())
		if err != nil {
			return "", err
		}
		if complete {
			return path, nil
		}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("iso: %s: %w", path, err)
	}
	if err := o.BuildOne(ctx, n, total); err != nil {
		return "", err
	}
	return path, nil
}

// complete reports whether an existing ISO of size bytes passes both bounds
// BuildOne applies to a fresh one. A missing disc directory counts as complete
// — see Ensure — and a short file is reported, with why it will be rebuilt.
//
// The upper bound is an error rather than a rebuild, which is the one place
// this differs from BuildOne. An ISO that is too big for the media was built
// for larger media: rebuilding it from the same disc directory would spend the
// whole build only to hit BuildOne's own capacity refusal, and that refusal
// deletes the image, throwing away an ISO that is still perfectly good for the
// blanks it was made for. Refusing here instead keeps the file and says which
// setting to change. Without it, Ensure applied only the lower bound and burn
// handed the oversized image straight to xorriso — the operator found out with
// a blank already in the tray.
func (o Options) complete(ctx context.Context, n int, size int64) (bool, error) {
	src := o.sourceDir(n)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return true, nil
	}
	srcSize, err := fsx.DirBytes(ctx, src)
	if err != nil {
		return false, fmt.Errorf("iso: %w", err)
	}
	if size < srcSize {
		o.UI.Warn("%s is %s but its disc directory holds %s — the file is truncated (a run died while "+
			"writing it); rebuilding it rather than burning it", Name(n), ui.HumanBytes(size), ui.HumanBytes(srcSize))
		return false, nil
	}
	if capacity := o.Cfg.Capacity(); capacity > 0 && size > capacity {
		return false, fmt.Errorf("iso: disc %d ISO is %s, larger than the %s media this run is "+
			"configured for — it was built for larger media, so set DISC_TYPE (or "+
			"DISC_CAPACITY_BYTES) to match the blanks you are burning, or delete %s and let "+
			"this run rebuild it", n, ui.HumanBytes(size), ui.HumanBytes(capacity), o.Path(n))
	}
	return true, nil
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
	// Secure before locking, because the lock file is created inside the tree
	// and [fsx.LockStaging] says so in its own contract: it opens .brb.lock
	// with a plain O_RDWR|O_CREATE and then truncates it, so a symlink planted
	// at that name is followed and its target is destroyed. Nothing stops that
	// but [fsx.SecureDir] having already refused a staging root that is a
	// symlink or belongs to another account — and the README's default STAGING
	// lives under a world-writable /var/tmp, where any local account can create
	// the tree ahead of the operator.
	//
	// `brb iso` was the one staging-writing command that skipped this; backup
	// secures in preflight and burn/restore secure in their own lockStaging, so
	// the refusal here is the same one those commands already make rather than
	// new behaviour. The ISO directory is secured alongside the root because
	// BuildOne creates it with os.MkdirAll, whose mode reaches only what it
	// makes: an iso/ the operator made by hand keeps whatever the umask gave
	// it, and it holds a full second copy of every disc.
	if err := fsx.SecureStaging(o.Cfg.Staging, o.Cfg.Dirs().ISO); err != nil {
		return fmt.Errorf("iso: %w", err)
	}
	// Locked here rather than at the top, and the order is the point twice
	// over. There is nowhere to put a lock file until staging exists, and
	// "cannot open .brb.lock" is a poor way to be told there is no disc set to
	// image — so the cheap checks above answer first. This is also the command
	// entry point: BuildAll and Ensure do not lock, because their callers (a
	// backup run, and burn) hold it already, and one process taking an flock
	// twice on two descriptors would refuse itself.
	lock, err := fsx.LockStaging(o.Cfg.Staging)
	if err != nil {
		return fmt.Errorf("iso: %w", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			o.UI.Warn("could not release the staging lock: %v", err)
		}
	}()
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
		n, ok := disc.NumberOf(e.Name(), suffix)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
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

// rangeMax stands in for "no upper bound". The value is inherited from the
// shell writer this package replaced and is not a limit on anything: it only
// has to exceed any disc count a set could plausibly have, and nothing on a
// disc records it.
const rangeMax = 9999

// ParseRange reads the disc selection the `iso` and `burn` commands take:
// "all", a single number "7", a closed range "7-20", or an open one "7-"
// meaning "seven onwards".
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
