package config

import "testing"

func TestPar2BlockCount(t *testing.T) {
	const mib = 1 << 20
	const gib = 1 << 30

	tests := []struct {
		name string
		size int64
		want int
	}{
		{"empty file falls back to the minimum", 0, 3000},
		{"negative size falls back to the minimum", -1, 3000},
		{"a small image stays at the minimum", 100 * mib, 3000},
		// Below ~2.93 GiB, size/1MiB is under the 3000 floor.
		{"just under the floor", 2999 * mib, 3000},
		{"exactly at the floor", 3000 * mib, 3000},
		{"one block past the floor", 3001 * mib, 3001},
		// The case that matters: a real BD25 image.
		{"a 20 GiB image gets ~1 MiB blocks", 20 * gib, 20480},
		{"a 22 GB image gets ~1 MiB blocks", 22_000_000_000, 20980},
		// Above 32 GiB the ceiling binds, because par2 rejects more.
		{"exactly at the ceiling", 32768 * mib, 32768},
		{"a BDXL100 image is clamped to the ceiling", 90 * gib, 32768},
		{"an absurd size is still clamped", 1 << 50, 32768},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Par2BlockCount(tc.size); got != tc.want {
				t.Errorf("Par2BlockCount(%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}
}

// TestPar2BlockCountKeepsBlocksNearATarget is the property that actually
// protects the user: whenever the count is not clamped, a block stays close to
// 1 MiB no matter how large the image is. A fixed count would let the block size
// grow without bound, and one bad byte costs a whole block.
func TestPar2BlockCountKeepsBlocksNearATarget(t *testing.T) {
	const mib = 1 << 20
	for size := int64(3000 * mib); size <= 32768*mib; size += 1237 * mib {
		n := Par2BlockCount(size)
		blockSize := (size + int64(n) - 1) / int64(n)
		if blockSize > 2*mib {
			t.Fatalf("size %d: block size %d exceeds 2 MiB with %d blocks", size, blockSize, n)
		}
	}
}

// TestPar2BlockCountBeatsAFixedCount states the regression in the terms the
// finding was reported in: at BD25 scale, sizing the blocks survives far more
// independent damage sites than the fixed 3000 the Go port used to hardcode.
func TestPar2BlockCountBeatsAFixedCount(t *testing.T) {
	const image = 22_000_000_000 // a full BD25 image
	const redundancy = 10

	sized := Par2GeometryFor(image, 0, redundancy)
	fixed := Par2GeometryFor(image, 3000, redundancy)

	if sized.RecoveryBlocks <= fixed.RecoveryBlocks {
		t.Fatalf("sizing the blocks gave %d recovery blocks, the fixed count gave %d; "+
			"sizing must give more", sized.RecoveryBlocks, fixed.RecoveryBlocks)
	}
	if ratio := sized.RecoveryBlocks / fixed.RecoveryBlocks; ratio < 5 {
		t.Errorf("expected sizing to survive several times more damage sites, got %dx "+
			"(%d vs %d recovery blocks)", ratio, sized.RecoveryBlocks, fixed.RecoveryBlocks)
	}
	if fixed.BlockSize <= 4<<20 {
		t.Errorf("the fixed count should produce coarse blocks at this scale, got %d", fixed.BlockSize)
	}
	if sized.BlockSize > 2<<20 {
		t.Errorf("sizing should keep blocks near 1 MiB, got %d", sized.BlockSize)
	}
}

func TestPar2GeometryFor(t *testing.T) {
	const mib = 1 << 20

	t.Run("an explicit block count wins", func(t *testing.T) {
		g := Par2GeometryFor(10*mib, 500, 10)
		if g.Blocks != 500 {
			t.Errorf("Blocks = %d, want 500", g.Blocks)
		}
		if g.RecoveryBlocks != 50 {
			t.Errorf("RecoveryBlocks = %d, want 50", g.RecoveryBlocks)
		}
	})

	t.Run("zero means size it automatically", func(t *testing.T) {
		g := Par2GeometryFor(20*1024*mib, 0, 10)
		if g.Blocks != 20480 {
			t.Errorf("Blocks = %d, want 20480", g.Blocks)
		}
	})

	t.Run("block size rounds up so the blocks cover the file", func(t *testing.T) {
		g := Par2GeometryFor(3001, 3000, 10)
		if g.BlockSize != 2 {
			t.Errorf("BlockSize = %d, want 2", g.BlockSize)
		}
		if total := g.BlockSize * int64(g.Blocks); total < 3001 {
			t.Errorf("%d blocks of %d cover only %d bytes, need 3001", g.Blocks, g.BlockSize, total)
		}
	})
}

// TestPar2DefaultsAreAutomatic pins the defaults themselves. A non-zero default
// here is what caused the original defect: it silently overrode the sizing.
func TestPar2DefaultsAreAutomatic(t *testing.T) {
	c := Default()
	if c.Par2Blocks != 0 {
		t.Errorf("Par2Blocks default = %d, want 0 (auto)", c.Par2Blocks)
	}
	if c.Par2MemoryMB != 0 {
		t.Errorf("Par2MemoryMB default = %d, want 0 (omit -m, par2 uses half of RAM)", c.Par2MemoryMB)
	}
}

func TestValidateAcceptsAutomaticPar2Blocks(t *testing.T) {
	c := Default()
	c.SourceDir = t.TempDir()

	c.Par2Blocks = 0
	if err := c.Validate(); err != nil {
		t.Errorf("PAR2_BLOCKS=0 (auto) must be valid, got %v", err)
	}

	c.Par2Blocks = par2MaxBlocks + 1
	if err := c.Validate(); err == nil {
		t.Errorf("PAR2_BLOCKS above par2's ceiling of %d must be rejected", par2MaxBlocks)
	}

	c.Par2Blocks = -1
	if err := c.Validate(); err == nil {
		t.Error("a negative PAR2_BLOCKS must be rejected")
	}
}

// TestPar2RecoveryBlocksRoundUp pins the recovery-block count to what par2
// actually writes, which is the number this preview exists to predict.
//
// It divided by 100 and truncated, so it under-reported every count that is
// not an exact multiple and reported ZERO for PAR2_BLOCKS 1 to 9 at the default
// 10% redundancy — a disc described in MANIFEST.txt and at the progress line as
// carrying no parity, while par2 wrote a block regardless. Expectations below
// were measured against par2 0.8.1 on a 5 MB file: -b5 -r10 wrote 1000572 bytes
// of recovery volumes (one block of five), -b9 wrote 556208 (one of nine), -b15
// wrote two blocks and -b45 five.
func TestPar2RecoveryBlocksRoundUp(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		blocks, redundancy, want int
	}{
		{5, 10, 1},  // truncation said 0
		{9, 10, 1},  // truncation said 0
		{1, 10, 1},  // truncation said 0
		{15, 10, 2}, // truncation said 1
		{45, 10, 5}, // truncation said 4
		{10, 10, 1}, // exact multiple: unchanged
		{40, 10, 4}, // exact multiple: unchanged
		{3000, 10, 300},
		{100, 1, 1},
		{99, 1, 1}, // truncation said 0
	} {
		g := Par2GeometryFor(10*mib, tc.blocks, tc.redundancy)
		if g.RecoveryBlocks != tc.want {
			t.Errorf("Par2GeometryFor(-b%d -r%d).RecoveryBlocks = %d, want %d",
				tc.blocks, tc.redundancy, g.RecoveryBlocks, tc.want)
		}
	}
	// Zero redundancy means no recovery set at all, and must not be rounded up
	// into one: Par2CreateArgs omits -r entirely when it is not positive.
	if g := Par2GeometryFor(10*mib, 3000, 0); g.RecoveryBlocks != 0 {
		t.Errorf("with no redundancy, RecoveryBlocks = %d, want 0", g.RecoveryBlocks)
	}
}
