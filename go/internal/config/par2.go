package config

// par2 block-count bounds.
//
// par2 splits the file it protects into a fixed number of equally sized blocks
// and Reed-Solomon recovers whole blocks, so one bad byte costs a whole block.
// The block COUNT is therefore what decides how much real-world damage the
// parity survives, and it has to scale with the file.
//
// A fixed 3000 blocks looks harmless on a small image and is quietly disastrous
// on a large one: a 22 GB disc image becomes 3000 blocks of about 7.5 MiB, with
// only 300 recovery blocks at 10% redundancy. 301 scattered pinholes — roughly
// 0.0005% of the disc — exhaust the parity completely, while the README promises
// that "roughly 10% of it can be destroyed and still rebuilt". Scattered
// single-sector rot is exactly the damage BD-R actually suffers, so the fixed
// count fails against the one failure mode the parity exists for.
//
// Sizing the blocks at about 1 MiB instead gives that same image ~22000 blocks
// and ~2200 recovery blocks: 7.5x more independent damage sites survived, and
// the README's claim becomes true.
const (
	// par2MinBlocks keeps small images from getting uselessly coarse blocks.
	par2MinBlocks = 3000
	// par2MaxBlocks is par2's hard ceiling — it rejects -b40000 outright.
	par2MaxBlocks = 32768
	// par2TargetBlockSize is the block size the count is derived from.
	par2TargetBlockSize = 1 << 20 // 1 MiB
)

// Par2BlockCount returns how many blocks par2 should split a file of size bytes
// into, aiming for roughly 1 MiB per block and clamped to the range par2
// accepts.
//
// The geometry is recorded in MANIFEST.txt and the .par2 files themselves carry
// it, so a repair years from now needs nothing from this function — but a
// re-created recovery set has to match the one on the disc, which is why the
// rule lives in one place rather than at the call site.
//
// A size of zero or less yields the minimum, which keeps the caller from having
// to special-case an empty file.
func Par2BlockCount(size int64) int {
	if size <= 0 {
		return par2MinBlocks
	}
	n := size / par2TargetBlockSize
	if n < par2MinBlocks {
		return par2MinBlocks
	}
	if n > par2MaxBlocks {
		return par2MaxBlocks
	}
	return int(n)
}

// Par2Geometry describes the recovery set brb is about to build, for the
// progress line and the manifest.
type Par2Geometry struct {
	// Blocks is the number of source blocks the file is split into.
	Blocks int
	// BlockSize is the size of each block in bytes, rounded up.
	BlockSize int64
	// RecoveryBlocks is how many blocks of parity are generated.
	RecoveryBlocks int
}

// Par2GeometryFor reports the geometry for a file of size bytes at the given
// redundancy. Pass blocks as configured: zero means "size it automatically".
func Par2GeometryFor(size int64, blocks, redundancy int) Par2Geometry {
	if blocks <= 0 {
		blocks = Par2BlockCount(size)
	}
	g := Par2Geometry{Blocks: blocks, RecoveryBlocks: blocks * redundancy / 100}
	if blocks > 0 {
		g.BlockSize = (size + int64(blocks) - 1) / int64(blocks)
	}
	return g
}
