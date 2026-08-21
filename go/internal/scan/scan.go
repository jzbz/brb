// Package scan walks a source tree natively, covering the same ground as a
// `find -xdev … -prune` invocation would. (Do not go looking for that
// invocation in brb.sh: the shell script in this tree has been reader-only
// since the first commit and contains no scanner to compare against.)
//
// The walk never follows symbolic links, records unreadable directories and
// unreadable files as problems instead of failing (and keeps the unreadable
// files out of the entry list, so nothing downstream backs them up as empty),
// and charges hard-linked files exactly once towards the raw byte total. Paths
// are kept as native Go strings end-to-end, so a name containing a tab or a
// newline cannot corrupt the pipeline by being re-split out of a delimited
// intermediate file. Such names are still a hazard for the tab-separated
// on-disc index, so every path holding a control byte is reported in
// [Result.OddPaths].
//
// This package is Unix-only: it reads the inode, link count and device number
// from the underlying stat structure.
package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind classifies a directory entry. Only [KindFile] entries carry data; every
// other kind is part of the "skeleton" that is replicated onto every disc.
type Kind uint8

// The entry kinds recognised by the walker.
const (
	// KindFile is a regular file.
	KindFile Kind = iota
	// KindDir is a directory.
	KindDir
	// KindSymlink is a symbolic link (never followed).
	KindSymlink
	// KindOther is a fifo, socket, or device node.
	KindOther
)

// String returns a short lowercase name for the kind.
func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "dir"
	case KindSymlink:
		return "symlink"
	case KindOther:
		return "other"
	default:
		return "unknown"
	}
}

// Entry is one item found beneath the scan root.
type Entry struct {
	// Rel is the slash-separated path relative to the root. It is never empty
	// and never absolute; the root itself is not reported as an entry.
	Rel string
	// Kind classifies the entry.
	Kind Kind
	// Size is the apparent size in bytes for regular files, and 0 otherwise.
	Size int64
	// Dev is the device number of the filesystem the entry lives on. It is
	// only meaningful paired with Inode: an inode number identifies a file
	// within one filesystem and nowhere else, so two unrelated files on two
	// filesystems can carry the same Inode. Anything grouping hard links must
	// key on the pair, never on Inode alone. A scan can hold more than one
	// device even under Options.OneFileSystem — see the note there about a
	// bind-mounted file.
	Dev uint64
	// Inode is the entry's inode number. Unique only within Dev.
	Inode uint64
	// Nlink is the entry's hard link count.
	Nlink uint64
}

// Problem is a non-fatal failure encountered during the walk, such as a
// directory that could not be read.
type Problem struct {
	// Path is the filesystem path that failed, as passed to the OS.
	Path string
	// Err is the underlying error.
	Err error
}

// Error implements the error interface.
func (p Problem) Error() string { return p.Path + ": " + p.Err.Error() }

// Unwrap returns the underlying error.
func (p Problem) Unwrap() error { return p.Err }

// Result is the outcome of a walk.
type Result struct {
	// Root is the cleaned root the walk started from.
	Root string
	// Entries holds every kept entry in pre-order (a directory precedes its
	// contents, siblings are ordered by name).
	Entries []Entry
	// Files is the number of regular files in Entries.
	Files int
	// RawBytes is the sum of Size over regular files, with hard links counted
	// once per inode.
	RawBytes int64
	// Skeleton is the number of non-file entries in Entries.
	Skeleton int
	// Errors holds the non-fatal problems encountered, unless
	// Options.OnError was supplied.
	Errors []Problem
	// OddPaths holds relative paths containing a C0 control byte or DEL. Tab
	// and newline are a hazard for the tab-separated on-disc index, which
	// escapes them so each name still occupies one row; every other control
	// byte — a carriage return above all — is stored verbatim and will not
	// display as itself, so a restorer cannot type the name back. Both must be
	// reported. Use [HasIndexEscape] to tell the two groups apart: a name
	// holding both kinds, "sheet\tone\r" say, belongs to the raw group, since
	// escaping its tab does nothing about its carriage return.
	OddPaths []string
	// SkippedMounts holds the relative paths of the directories that
	// Options.OneFileSystem stopped the walk at: mount points beneath the
	// root, each of which is reported as an empty directory while everything
	// underneath it is left out. They are listed here, in walk order, so a
	// caller can say so out loud. Without this the omission was silent — no
	// problem, no warning — and a NAS mounted under the source tree simply
	// never made it onto a disc. Directories only: see Options.OneFileSystem
	// for the one thing this list does not cover.
	SkippedMounts []string
}

// Options configures [Walk].
type Options struct {
	// Root is the directory to walk. Required.
	Root string
	// PruneDirs are paths relative to Root, matched against an entry's Rel
	// path. A match is skipped entirely and, if it is a directory, its whole
	// subtree is skipped too. A pattern containing '*', '?' or '[' is matched
	// with filepath.Match instead of compared literally; note that unlike
	// find's -path, '*' does not cross a '/' separator.
	PruneDirs []string
	// ExcludeMasks are filepath.Match patterns tested against an entry's base
	// name. A match drops that entry only when it is not a directory: it never
	// prunes a directory and never stops descent, because the mask "core" would
	// otherwise erase every core/ source directory from the backup without
	// leaving so much as a skeleton entry behind. Use PruneDirs for whole
	// subtrees; the full rationale is in walker.dir.
	ExcludeMasks []string
	// OneFileSystem stops the walk from descending into a directory whose
	// device differs from the root's, like find -xdev. The directory itself is
	// still reported, so the mount point survives into the skeleton, and its
	// path is added to Result.SkippedMounts so the caller can report what was
	// left behind.
	//
	// The boundary is enforced at directories only. A NON-directory on another
	// device — in practice a file bind mount, since symlinks are never followed
	// — is an ordinary entry, charged to RawBytes, backed up with its contents,
	// and absent from SkippedMounts.
	//
	// That is deliberate rather than overlooked, and it is what every tool
	// spelling this option does: find -xdev, tar --one-file-system, rsync -x
	// and cp -x all descend no further at a mounted directory and all copy a
	// bind-mounted file's bytes. Excluding it instead would drop a file that is
	// plainly there in ls out of a backup, silently — the worse failure for
	// this program, and one no other tool would lead a user to expect. The
	// danger the option exists to stop is an unbounded subtree, a NAS or an
	// external drive; a bind-mounted file is one entry and cannot pull in a
	// second. Container runtimes mount /etc/resolv.conf and /etc/hosts this
	// way, so a backup of /etc from inside a container depends on it.
	//
	// The cost is that a scan holds more than one device even here, which is
	// why Entry carries Dev and why anything grouping by inode must key on the
	// pair. See TestOneFileSystemDoesNotStopAtANonDirectory for the case that
	// pins the behaviour.
	OneFileSystem bool
	// OnEntry, when non-nil, is called for every kept entry in walk order.
	OnEntry func(Entry)
	// OnError, when non-nil, receives non-fatal problems; they are then not
	// collected into Result.Errors.
	OnError func(path string, err error)
}

// ErrNoRoot is returned by [Walk] when Options.Root is empty.
var ErrNoRoot = errors.New("scan: root directory not set")

// cancelCheckInterval is how many entries are processed between checks of the
// context's done channel. Checking every entry would dominate the walk on a
// warm cache; a few hundred keeps cancellation latency imperceptible.
const cancelCheckInterval = 256

// Walk scans opts.Root, never following symbolic links.
//
// Unreadable directories are recorded as problems and skipped rather than
// aborting the walk. So is every regular file that cannot be opened for
// reading: it is recorded as a problem and left out of Entries, rather than
// reported as a file and left for the image builder to trip over. That is not
// an optimisation but a correctness guard — mksquashfs exits 0 on a source
// file it cannot open and writes a zero-byte file in its place, so a file that
// reached it unreadable was silently backed up as nothing. Every other kind of
// entry is never opened: opening a fifo would block the walk and opening a
// device node could have side effects. Walk returns an error only when the
// root itself cannot be used or when ctx is cancelled; the error then wraps
// ctx.Err().
func Walk(ctx context.Context, opts Options) (*Result, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, ErrNoRoot
	}
	root := filepath.Clean(opts.Root)

	prunePats, err := cleanPatterns(opts.PruneDirs, "PruneDirs")
	if err != nil {
		return nil, err
	}
	prunes := compilePrunes(prunePats)
	maskPats, err := cleanPatterns(opts.ExcludeMasks, "ExcludeMasks")
	if err != nil {
		return nil, err
	}
	masks := compileMasks(maskPats)

	fi, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("scan: root %s: %w", root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("scan: root %s is not a directory", root)
	}
	rootDev, _, _ := statIDs(fi)

	w := &walker{
		opts:    opts,
		prunes:  prunes,
		masks:   masks,
		rootDev: rootDev,
		linked:  make(map[fileID]struct{}),
		res:     &Result{Root: root},
	}
	if err := w.dir(ctx, root, "", false); err != nil {
		return nil, fmt.Errorf("scan: walking %s: %w", root, err)
	}
	return w.res, nil
}

// fileID identifies an inode uniquely across mounted filesystems.
type fileID struct {
	dev, ino uint64
}

// statIDs is how the walker reads a device number, inode and link count. It is
// a variable purely so a test can pretend a directory sits on another device:
// a real mount point under a temp dir needs root, and the OneFileSystem
// behaviour would otherwise be untestable. Nothing outside the tests assigns
// it.
var statIDs = fileIDs

type walker struct {
	opts    Options
	prunes  prunes
	masks   []mask
	rootDev uint64
	linked  map[fileID]struct{}
	res     *Result
	seen    int
}

// dir reads one directory and recurses. It returns an error only for context
// cancellation; everything else is recorded as a problem.
//
// relOdd must say whether rel already holds a control byte, so the check for
// [Result.OddPaths] can be made once per base name instead of once per full
// path. Scanning e.Rel whole re-read every ancestor's bytes once for each of
// its descendants — O(entries × depth × name length) on a tree the walk sees
// 10^4..10^6 times. Inheriting the answer is exact because the only byte
// concatenation adds is the '/' separator (0x2f), which is not a control byte:
// a path holds one iff its parent's path does or its own base name does.
func (w *walker) dir(ctx context.Context, abs, rel string, relOdd bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		w.problem(abs, err)
		// ReadDir returns the entries it managed to read alongside the error,
		// so fall through and use whatever we got.
	}
	for _, de := range ents {
		w.seen++
		if w.seen%cancelCheckInterval == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}

		name := de.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		if w.pruned(childRel) {
			continue
		}

		info, err := de.Info()
		if err != nil {
			w.problem(filepath.Join(abs, name), err)
			continue
		}
		dev, ino, nlink := statIDs(info)

		e := Entry{Rel: childRel, Kind: kindOf(info), Dev: dev, Inode: ino, Nlink: nlink}
		if e.Kind == KindFile {
			e.Size = info.Size()
		}

		// An exclude mask drops a matching entry only when it is not a
		// directory, and never stops descent. Excluding a directory here
		// would erase everything beneath it from the backup without leaving
		// so much as a skeleton entry behind, so a restored tree would show
		// no trace of the omission: the mask "core" would take out every
		// core/ source directory in a Go, Drupal or kernel tree. Pruning a
		// subtree is what PruneDirs is for, and it is anchored at the root
		// rather than matching a base name at any depth.
		if e.Kind != KindDir && w.excluded(name) {
			continue
		}
		// A regular file the walker cannot open is a problem, not an entry.
		// See Walk: mksquashfs would otherwise replace it with an empty file
		// and exit 0, and nothing downstream could tell. Only regular files
		// are opened — see readable.
		if e.Kind == KindFile {
			if err := readable(filepath.Join(abs, name)); err != nil {
				w.problem(filepath.Join(abs, name), err)
				continue
			}
		}
		// Computed here rather than at the top of the loop so an entry a mask
		// or an unreadable open dropped is never charged for it.
		childOdd := relOdd || hasControl(name)
		w.emit(e, childOdd)

		if e.Kind == KindDir {
			if w.opts.OneFileSystem && dev != w.rootDev {
				// Like find -xdev: report the mount point, do not descend —
				// and remember it, so the caller can say what was left out.
				w.res.SkippedMounts = append(w.res.SkippedMounts, childRel)
				continue
			}
			if err := w.dir(ctx, filepath.Join(abs, name), childRel, childOdd); err != nil {
				return err
			}
		}
	}
	return nil
}

// emit records a kept entry and updates the running totals. odd says whether
// e.Rel holds a control byte; see [walker.dir] for why the caller computes it.
func (w *walker) emit(e Entry, odd bool) {
	w.res.Entries = append(w.res.Entries, e)
	if e.Kind == KindFile {
		w.res.Files++
		if w.chargeable(e) {
			w.res.RawBytes += e.Size
		}
	} else {
		w.res.Skeleton++
	}
	if odd {
		w.res.OddPaths = append(w.res.OddPaths, e.Rel)
	}
	if w.opts.OnEntry != nil {
		w.opts.OnEntry(e)
	}
}

// chargeable reports whether e's bytes should be added to the raw total. A
// hard-linked inode is charged the first time one of its names is seen and
// never again.
func (w *walker) chargeable(e Entry) bool {
	if e.Nlink <= 1 {
		return true
	}
	id := fileID{dev: e.Dev, ino: e.Inode}
	if _, dup := w.linked[id]; dup {
		return false
	}
	w.linked[id] = struct{}{}
	return true
}

// problem reports a non-fatal failure.
func (w *walker) problem(path string, err error) {
	if w.opts.OnError != nil {
		w.opts.OnError(path, err)
		return
	}
	w.res.Errors = append(w.res.Errors, Problem{Path: path, Err: err})
}

// prunes is the compiled PruneDirs list. Whether a pattern holds a
// metacharacter depends on the pattern alone, but pruned() runs before the
// stat for every entry the walk sees — directories included, unlike the mask
// path — so asking strings.ContainsAny once per pattern per entry re-derived a
// fixed answer 15 times over for the default configuration, on 10^4..10^6
// entries. This is the same redundancy compileMasks below was written to
// remove, and the same shape of fix.
type prunes struct {
	// lit holds every pattern, glob or not: a pattern is also a literal name,
	// so a directory actually called "core.[0-9]*" still prunes itself.
	lit map[string]struct{}
	// glob holds only the patterns carrying a metacharacter. Under the default
	// PRUNE_DIRS it is empty, so the whole per-entry cost is one map lookup.
	glob []string
}

// compilePrunes splits already-cleaned patterns into the literal set and the
// glob subset. See [prunes] for why.
func compilePrunes(pats []string) prunes {
	p := prunes{lit: make(map[string]struct{}, len(pats))}
	for _, s := range pats {
		p.lit[s] = struct{}{}
		if isPattern(s) {
			p.glob = append(p.glob, s)
		}
	}
	return p
}

// pruned reports whether rel matches any prune pattern.
func (w *walker) pruned(rel string) bool {
	if _, ok := w.prunes.lit[rel]; ok {
		return true
	}
	for _, g := range w.prunes.glob {
		if ok, err := filepath.Match(g, rel); err == nil && ok {
			return true
		}
	}
	return false
}

// mask is one compiled exclude pattern. filepath.Match re-parses its pattern on
// every call, and the walk calls it once per mask per file: on a 200k-entry
// tree the four default masks are about a fifth of the whole walk. The two
// shapes that cover every default — "*.suffix" and a bare literal — are decided
// once here instead, which is exact rather than approximate for a base name
// (base names hold no '/', so Match("*.pyc", n) accepts precisely the names
// ending ".pyc", including ".pyc" itself).
type mask struct {
	kind maskKind
	pat  string // the suffix, the literal, or the glob
}

type maskKind uint8

const (
	maskGlob maskKind = iota
	maskSuffix
	maskLiteral
)

// compileMasks classifies already-cleaned patterns. Anything with a backslash
// keeps the general path: a backslash is filepath.Match's escape character, so
// the pattern does not mean what its bytes say.
func compileMasks(pats []string) []mask {
	out := make([]mask, 0, len(pats))
	for _, p := range pats {
		switch {
		case strings.Contains(p, `\`):
			out = append(out, mask{maskGlob, p})
		case !isPattern(p):
			out = append(out, mask{maskLiteral, p})
		case strings.HasPrefix(p, "*") && !isPattern(p[1:]):
			out = append(out, mask{maskSuffix, p[1:]})
		default:
			out = append(out, mask{maskGlob, p})
		}
	}
	return out
}

// excluded reports whether a base name matches any exclude mask.
func (w *walker) excluded(name string) bool {
	for _, m := range w.masks {
		switch m.kind {
		case maskLiteral:
			if name == m.pat {
				return true
			}
		case maskSuffix:
			if strings.HasSuffix(name, m.pat) {
				return true
			}
		default:
			if ok, err := filepath.Match(m.pat, name); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// isControl reports whether r is a C0 control character or DEL. Carriage
// return is the one that matters most and was missed the longest: indexfmt
// escapes only backslash, tab and newline, so a CR travels into the index raw,
// where it overwrites the start of its own line on any terminal.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// hasControl is the byte-wise form of isControl, applied to a whole string. It
// accepts exactly the strings strings.IndexFunc(s, isControl) >= 0 accepts:
// every rune isControl admits is below 0x80 and so a single byte, and no byte
// of a multi-byte UTF-8 sequence is below 0x80, so no valid encoding can be
// misread. Nor can an invalid one — IndexFunc turns a stray byte into U+FFFD,
// which isControl rejects, and this loop rejects it too. The walk calls this
// once per entry, so the rune decode and the indirect call through a func value
// that IndexFunc pays for every byte are worth not paying.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// HasIndexEscape reports whether a path from [Result.OddPaths] is one the
// on-disc index can still render as a single readable row: it holds a tab or a
// newline, which the index escapes, and no other control byte.
//
// The second half is what makes the answer usable as a classification. A name
// carrying both a tab and a carriage return would satisfy a plain "contains a
// tab or a newline" test, and the caller would then tell the operator that the
// index escapes what the name contains and each such name still occupies one
// row — true of the tab, false of the CR, which travels into the index raw and
// is the byte that leaves the name untypable. Answering false here puts that
// name in the raw group instead, where the warning is true of it. Nothing about
// the escaping contract changes: indexfmt still escapes backslash, tab and
// newline and nothing else, and every one of these names is backed up.
func HasIndexEscape(rel string) bool {
	if !strings.ContainsAny(rel, "\n\t") {
		return false
	}
	return strings.IndexFunc(rel, isUnescapedControl) < 0
}

// isUnescapedControl reports whether r is a control byte the index does not
// escape — every C0 byte and DEL except the tab and the newline it does.
func isUnescapedControl(r rune) bool { return isControl(r) && r != '\t' && r != '\n' }

// kindOf maps a FileInfo to a Kind.
func kindOf(fi os.FileInfo) Kind {
	m := fi.Mode()
	switch {
	case m.IsRegular():
		return KindFile
	case m.IsDir():
		return KindDir
	case m&os.ModeSymlink != 0:
		return KindSymlink
	default:
		return KindOther
	}
}

// isPattern reports whether s contains filepath.Match metacharacters.
func isPattern(s string) bool { return strings.ContainsAny(s, "*?[") }

// cleanPatterns drops empty patterns, trims surrounding slashes, and rejects
// malformed globs up front so a typo cannot silently exclude nothing.
func cleanPatterns(in []string, field string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p == "" || p == "." {
			continue
		}
		if _, err := filepath.Match(p, "x"); err != nil {
			return nil, fmt.Errorf("scan: %s: bad pattern %q: %w", field, p, err)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
