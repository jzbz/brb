// Package pack groups a scanned tree into disc-sized bins using first-fit
// decreasing packing.
//
// Two rules make this more than a textbook bin packer:
//
//   - Hard-linked files sharing an inode form a single indivisible [Unit]. The
//     unit is charged once — squashfs stores the extra names as real links —
//     and every name in the group travels to the same disc, because a link
//     whose target landed on another disc would be restored as a second full
//     copy at best and as nothing at all at worst.
//   - Every non-file entry (directory, symlink, fifo, socket, device node)
//     forms the skeleton, which is replicated into every bin, so that a single
//     disc shows the complete directory structure.
//
// The skeleton carries no file data, but it is not free: each member costs an
// inode in every image, plus a directory-table entry and a symlink its target
// string. That cost is charged to no budget — [Bin.RawBytes] counts the chosen
// file units only, and [Packer.Oversized] compares file units alone — so a tree
// with an enormous number of non-file entries overshoots every disc by the same
// fixed amount and is re-packed once per disc for it. Ordinary trees are
// nowhere near this; the shape that would reach it is millions of symlinks with
// long targets. If it is ever seen in the field the fix is to charge an
// estimated per-entry skeleton cost against the budget and to refuse up front
// when the skeleton alone exceeds it, because neither of the shrink loop's two
// pieces of advice can help once it does.
//
// The whole scan is held in memory and assignment is tracked in a bitset, so
// planning N discs costs one O(n log n) sort plus one O(n) sweep per disc,
// rather than re-reading and re-parsing the scan for every disc.
//
// A Packer is not safe for concurrent use.
package pack

import (
	"errors"
	"fmt"
	"sort"

	"github.com/jzbz/brb/internal/scan"
)

// Unit is one indivisible group of paths sharing a single inode's bytes. A
// unit with more than one path is a hard link group.
type Unit struct {
	// Size is the number of bytes the unit costs on a disc, charged once for
	// the whole group.
	Size int64
	// Paths holds one or more relative paths, sorted. They always travel
	// together.
	Paths []string
}

// Bin is one disc's worth of content.
type Bin struct {
	// Index is the 1-based disc number.
	Index int
	// Files are the data files assigned to this bin, sorted.
	Files []string
	// Skeleton is every non-file entry, sorted. It is identical for every bin
	// and shared between them: callers must not modify it.
	Skeleton []string
	// RawBytes is the uncompressed size of Files, hard links charged once.
	RawBytes int64
}

// ErrStaleBin is returned by [Packer.Commit] when the bin was not the one
// produced by the most recent call to [Packer.Next].
var ErrStaleBin = errors.New("pack: bin was not produced by the most recent Next")

// Packer assigns units to bins. Build one with [New].
type Packer struct {
	units    []Unit   // sorted by size descending, ties by first path
	skeleton []string // sorted, shared by every bin

	assigned  []bool // parallel to units
	committed int

	remUnits int
	remFiles int
	remBytes int64

	// pending is the bin returned by the most recent Next, and pendingIdx the
	// unit indices it covers. Commit only accepts pending.
	pending    *Bin
	pendingIdx []int
}

// linkGroup identifies one hard-linked inode uniquely across filesystems. It
// mirrors the key the scanner charges bytes with, so the two agree by
// construction about what "the same file" means.
type linkGroup struct {
	dev, ino uint64
}

// New builds units from entries.
//
// Regular files with a link count above one are grouped by device and inode;
// everything else becomes a one-path unit. Non-file entries become the
// skeleton. The result is deterministic: units are ordered by size descending
// with ties broken by their first path.
//
// Grouping keys on the device-and-inode pair, never on the inode alone. An
// inode number is unique only within one filesystem, so two unrelated link
// groups on two filesystems can share one. Collapsing them into a single unit
// loses no data — every name still travels together onto one disc — but the
// merged group is charged only the larger of the two sizes, and a disc packed
// against an under-estimate is one that may not fit the disc it was planned
// for. Keying on the pair cannot collide.
//
// This is not theoretical for a caller using scan.Options.OneFileSystem, which
// bounds a scan to the root's device at directories but deliberately lets a
// bind-mounted *file* from another filesystem through, exactly as find -xdev
// does. That file is the second device in the tree. It has to be hard-linked
// and share an inode number with a link group on the root device before the old
// key mattered — rare, silent when it happened, and free to rule out.
//
// (Nothing in the frozen on-disc format records an inode or a device, so the
// grouping key is an implementation choice, not a compatibility constraint.)
func New(entries []scan.Entry) *Packer {
	var skeleton []string
	var singles []Unit
	groups := make(map[linkGroup][]scan.Entry)

	for _, e := range entries {
		if e.Kind != scan.KindFile {
			skeleton = append(skeleton, e.Rel)
			continue
		}
		if e.Nlink > 1 && e.Inode != 0 {
			id := linkGroup{dev: e.Dev, ino: e.Inode}
			groups[id] = append(groups[id], e)
			continue
		}
		singles = append(singles, Unit{Size: e.Size, Paths: []string{e.Rel}})
	}

	units := singles
	for _, g := range groups {
		u := Unit{Paths: make([]string, 0, len(g))}
		for _, e := range g {
			u.Paths = append(u.Paths, e.Rel)
			// All names of one inode report the same size; take the largest
			// anyway so a racing truncation cannot under-charge the disc.
			if e.Size > u.Size {
				u.Size = e.Size
			}
		}
		sort.Strings(u.Paths)
		units = append(units, u)
	}

	sort.Slice(units, func(i, j int) bool {
		if units[i].Size != units[j].Size {
			return units[i].Size > units[j].Size
		}
		return units[i].Paths[0] < units[j].Paths[0]
	})
	sort.Strings(skeleton)

	p := &Packer{
		units:    units,
		skeleton: skeleton,
		assigned: make([]bool, len(units)),
		remUnits: len(units),
	}
	for _, u := range units {
		p.remFiles += len(u.Paths)
		p.remBytes += u.Size
	}
	return p
}

// Oversized returns the still-unassigned units that cannot fit on a disc of
// the given budget, largest first. A non-empty result means packing can never
// complete: the caller must exclude those files, use larger media, or split
// them by hand.
func (p *Packer) Oversized(budget int64) []Unit {
	var out []Unit
	for i := range p.units {
		if p.assigned[i] || p.units[i].Size <= budget {
			continue
		}
		u := p.units[i]
		paths := make([]string, len(u.Paths))
		copy(paths, u.Paths)
		out = append(out, Unit{Size: u.Size, Paths: paths})
	}
	// p.units is already sorted by size descending.
	return out
}

// Next computes the next candidate bin from the currently-unassigned units,
// taking the largest unit that still fits until none does.
//
// It does not change what is assigned, so it may be called repeatedly with a
// smaller budget to re-plan the same disc after a measurement — only [Commit]
// advances the packer. ok is false when nothing more can be assigned, either
// because every unit is committed or because no remaining unit fits the
// budget; callers should consult [Packer.Oversized] and [Packer.Remaining] to
// tell those apart.
func (p *Packer) Next(budget int64) (bin *Bin, ok bool) {
	p.pending, p.pendingIdx = nil, nil

	var chosen []int
	var files []string
	var total int64
	for i := range p.units {
		if p.assigned[i] {
			continue
		}
		// Written as a subtraction so a pathological size cannot overflow.
		if p.units[i].Size > budget-total {
			continue
		}
		chosen = append(chosen, i)
		files = append(files, p.units[i].Paths...)
		total += p.units[i].Size
	}
	if len(chosen) == 0 {
		return nil, false
	}
	sort.Strings(files)

	b := &Bin{
		Index:    p.committed + 1,
		Files:    files,
		Skeleton: p.skeleton,
		RawBytes: total,
	}
	p.pending, p.pendingIdx = b, chosen
	return b, true
}

// Commit accepts the bin returned by the most recent [Packer.Next], marking
// its files assigned and advancing the bin index. Committing any other bin
// returns an error wrapping [ErrStaleBin] and changes nothing.
func (p *Packer) Commit(b *Bin) error {
	if b == nil {
		return fmt.Errorf("pack: Commit(nil): %w", ErrStaleBin)
	}
	if p.pending == nil || p.pending != b {
		return fmt.Errorf("pack: commit of bin %d: %w", b.Index, ErrStaleBin)
	}
	for _, i := range p.pendingIdx {
		p.assigned[i] = true
		p.remUnits--
		p.remFiles -= len(p.units[i].Paths)
		p.remBytes -= p.units[i].Size
	}
	p.committed++
	p.pending, p.pendingIdx = nil, nil
	return nil
}

// Remaining reports what is still unassigned: the number of units, the number
// of file paths they cover, and their total raw size.
func (p *Packer) Remaining() (units int, files int, bytes int64) {
	return p.remUnits, p.remFiles, p.remBytes
}

// Committed returns the number of bins committed so far.
func (p *Packer) Committed() int { return p.committed }
