// Package disc describes the optical media brb writes to and the byte budget
// arithmetic that decides how large a single squashfs image may be.
//
// The capacities and the budget formula are frozen: they decide how a tree is
// sliced across discs, so a change to either re-plans every set built after it,
// and an operator adding a disc to an existing set would get one filled to a
// different mark. The arithmetic is integer arithmetic and the order of the
// multiplications and divisions is part of the constant, because every division
// truncates — see Compute and TestComputeOrderOfOperations.
//
// (These are writer-side numbers, so brb.sh has nothing to compare them
// against: it reads finished discs and never plans one. Nothing in this file
// has a counterpart in the shell script, in this revision or any earlier one.)
package disc

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Type identifies a Blu-ray media format.
type Type string

// The media types brb understands. The string values are the ones accepted in
// the configuration file's DISC_TYPE setting.
//
// These name capacities, not brands. M-DISC BD-R is ordinary BD-R as far as the
// format and the capacity are concerned — it differs in the recording layer,
// which is inorganic rather than an organic dye — so an M-DISC set uses exactly
// the same DISC_TYPE as its conventional equivalent, and any Blu-ray writer
// burns it. M-DISC exists for the first three; 128 GB quad-layer is
// conventional BD-R XL only.
const (
	// BD25 is single-layer Blu-ray, 25 GB nominal. M-DISC available.
	BD25 Type = "bd25"
	// BD50 is dual-layer Blu-ray, 50 GB nominal. M-DISC available.
	BD50 Type = "bd50"
	// BDXL100 is triple-layer BDXL, 100 GB nominal. M-DISC available.
	BDXL100 Type = "bdxl100"
	// BDXL128 is quad-layer BDXL, 128 GB nominal. No M-DISC equivalent.
	BDXL128 Type = "bdxl128"
)

// Byte capacities, in bytes the media actually holds. Do not adjust one to
// squeeze in a little more: every future set is planned against these, so a
// disc added to an existing set would be filled to a different mark than its
// siblings.
const (
	capBD25    int64 = 25025314816
	capBD50    int64 = 50050629632
	capBDXL100 int64 = 100103356416
	capBDXL128 int64 = 128001769472
)

// ErrUnknownType is returned by ParseType for an unrecognised media name.
var ErrUnknownType = errors.New("unknown disc type")

// Types returns every supported media type, smallest capacity first.
func Types() []Type {
	return []Type{BD25, BD50, BDXL100, BDXL128}
}

// ParseType converts a configuration string such as "bd50" into a Type.
// Surrounding whitespace and letter case are ignored. An unrecognised name
// yields an error wrapping ErrUnknownType that names the valid choices.
func ParseType(s string) (Type, error) {
	t := Type(strings.ToLower(strings.TrimSpace(s)))
	switch t {
	case BD25, BD50, BDXL100, BDXL128:
		return t, nil
	}
	names := make([]string, 0, len(Types()))
	for _, v := range Types() {
		names = append(names, string(v))
	}
	return "", fmt.Errorf("%w %q (expected %s)", ErrUnknownType, s, strings.Join(names, ", "))
}

// Capacity returns the raw media capacity in bytes, or 0 for a type that is
// not one of the four known constants.
func (t Type) Capacity() int64 {
	switch t {
	case BD25:
		return capBD25
	case BD50:
		return capBD50
	case BDXL100:
		return capBDXL100
	case BDXL128:
		return capBDXL128
	}
	return 0
}

// String returns the configuration name of the media type.
func (t Type) String() string { return string(t) }

// Budget is the result of apportioning one disc's raw capacity between
// filesystem overhead, the plaintext files carried on every disc, the par2
// recovery data, and the encrypted squashfs image itself.
type Budget struct {
	// Capacity is the raw media size in bytes.
	Capacity int64
	// Usable is Capacity*98/100, leaving 2% for ISO 9660 overhead.
	Usable int64
	// Reserve is the space set aside on every disc for the plaintext files
	// (README.md, MANIFEST.txt, SHA512SUMS, the brb binary, the index).
	Reserve int64
	// Image is the maximum size of one .squashfs image, before encryption.
	Image int64
}

// Compute applies the frozen budget formula:
//
//	usable = capacity * 98 / 100
//	image  = (usable - reserve) * 100 / (100 + par2Redundancy + 1)
//
// All arithmetic is integer arithmetic, evaluated in that order; each division
// truncates toward zero. The 98/100 is the 2% left for ISO 9660 overhead, and
// the extra "+1" in the divisor is slack for the par2 index file.
//
// It returns an error when the arithmetic cannot yield a usable image: a
// non-positive capacity, a negative redundancy, a divisor that is not
// positive, a reserve at or above the usable size, or a reserve and parity
// overhead that between them consume the whole disc.
func Compute(capacity, reserve int64, par2Redundancy int) (Budget, error) {
	if capacity <= 0 {
		return Budget{}, fmt.Errorf("disc capacity must be positive, got %d", capacity)
	}
	if reserve < 0 {
		return Budget{}, fmt.Errorf("reserve must not be negative, got %d", reserve)
	}
	if par2Redundancy < 0 {
		return Budget{}, fmt.Errorf("par2 redundancy must not be negative, got %d", par2Redundancy)
	}
	div := int64(100) + int64(par2Redundancy) + 1
	if div <= 0 {
		return Budget{}, fmt.Errorf("par2 redundancy %d yields a non-positive divisor", par2Redundancy)
	}
	if capacity > math.MaxInt64/98 {
		return Budget{}, fmt.Errorf("disc capacity %d is implausibly large", capacity)
	}
	usable := capacity * 98 / 100
	// Both ends of (usable-reserve)*100 have to be guarded, not just the
	// positive one. Nothing bounds RESERVE_BYTES from above — Config.Validate
	// checks only that it is not negative — so a reserve at or above
	// 92233744893356279 drives the subtraction below -(MaxInt64/100) and the
	// multiplication wraps back to a positive number. With RESERVE_BYTES set to
	// math.MaxInt64 on a BD25 that wrap yields Image=22094422090, which sails
	// past the `image <= 0` test at the bottom: Compute would return no error
	// and hand back a budget that ignores the reserve entirely, filling the disc
	// to capacity with no room for the plaintext files the reserve exists for. A
	// reserve at or above usable can never yield an image anyway, so refuse it
	// before multiplying, with the same message the truncating path already
	// gives for a reserve that merely eats the disc.
	if reserve >= usable {
		return Budget{Capacity: capacity, Usable: usable, Reserve: reserve},
			fmt.Errorf("disc capacity too small for the configured reserve and parity: "+
				"usable %d bytes, reserve %d bytes, par2 %d%%", usable, reserve, par2Redundancy)
	}
	if usable-reserve > math.MaxInt64/100 {
		return Budget{}, fmt.Errorf("disc capacity %d is implausibly large", capacity)
	}
	image := (usable - reserve) * 100 / div
	b := Budget{Capacity: capacity, Usable: usable, Reserve: reserve, Image: image}
	if image <= 0 {
		return b, fmt.Errorf("disc capacity too small for the configured reserve and parity: "+
			"usable %d bytes, reserve %d bytes, par2 %d%%", usable, reserve, par2Redundancy)
	}
	return b, nil
}
