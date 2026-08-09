package backup

import (
	"fmt"
	"math"
	"strings"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/ui"
)

// The bounds the adaptive estimate is clamped to, matching brb.sh's awk.
//
// The ceiling is the safe assumption brb starts from: nothing compresses. The
// floor is there because the raw budget is the image budget divided by the
// ratio, so an estimate of 0.001 would plan a thousand disc-budgets of content
// into one disc and pay for it with a rebuild of every gigabyte of it. 0.02 is
// already a 50:1 estimate, which is past anything real content achieves.
const (
	ratioFloor = 0.02
	ratioCeil  = 1.0
)

// ratioEstimator learns the compressed/raw ratio from the discs a run has
// actually built.
//
// PACK_RATIO is a guess made before anything has been compressed, and until
// this existed the guess was only ever corrected upward: the shrink-retry
// raises it after an overshoot and nothing ever lowered it again. Starting from
// the safe default of 1.00 that means every disc after the first is planned as
// if nothing compresses, leaving them 25-35% empty and costing several extra
// discs across a large set.
//
// The estimate is taken from the WORST of the last few discs rather than from
// the last disc alone: a run of text followed by a disc of JPEGs would
// otherwise be planned as if the JPEGs compressed 3:1, and pay for it with a
// full multi-GB rebuild. A window still lets the estimate fall again once an
// incompressible stretch is behind us, which a running maximum never would.
type ratioEstimator struct {
	adapt  bool
	window int
	margin float64

	// measured holds the accepted discs' ratios, oldest first. It is the
	// estimator's whole memory, and travels through the resume state so a set
	// continued days later plans its next disc the way this run would have.
	measured []float64
}

// newRatioEstimator builds the estimator one run uses.
func newRatioEstimator(c *config.Config) *ratioEstimator {
	e := &ratioEstimator{
		adapt:  c.PackRatioAdapt,
		window: c.PackRatioWindow,
		margin: c.PackRatioMargin,
	}
	// Validate rejects both of these, so reaching them means a Config was
	// assembled in code rather than loaded. Fall back to the documented
	// defaults rather than to a window of zero, which has no worst case and
	// would collapse every estimate onto the clamp floor.
	if e.window < 1 {
		e.window = config.Default().PackRatioWindow
	}
	if !(e.margin >= 1) || math.IsInf(e.margin, 0) {
		e.margin = config.Default().PackRatioMargin
	}
	return e
}

// carry restores the measurements of an interrupted run.
//
// Anything that is not a usable ratio is dropped rather than carried: state.json
// is a plain text file an operator can edit, and a single negative entry would
// become the window's maximum, clamp to the floor, and plan fifty disc-budgets
// of content onto the next disc.
func (e *ratioEstimator) carry(measured []float64) []float64 {
	e.measured = e.measured[:0]
	for _, m := range measured {
		if !(m > 0) || math.IsInf(m, 0) || math.IsNaN(m) {
			continue
		}
		e.measured = append(e.measured, m)
	}
	return e.measured
}

// recent returns the measurements the estimate is taken from: the last window
// entries, or all of them when fewer than that exist.
//
// This is the one place the bash implementation got wrong, and it got it wrong
// in the direction that destroys a run rather than the direction that wastes a
// disc: its negative slice offset yielded NOTHING when the array was shorter
// than the window, so awk saw no input, and the estimate collapsed to the clamp
// floor regardless of what had been measured. On content compressing to 0.9
// that plans fifty disc-budgets of files onto one disc, and every one of them
// is written, measured, rejected and rebuilt. A window shorter than its limit
// must be the whole list.
func (e *ratioEstimator) recent() []float64 {
	if len(e.measured) > e.window {
		return e.measured[len(e.measured)-e.window:]
	}
	return e.measured
}

// observe records the ratio a disc actually achieved and returns the ratio the
// next disc should be planned with, along with the window it was taken from.
//
// ok is false when there is no estimate to make: adaptation is off, or the
// measurement is not a usable number. Only accepted discs are passed here — the
// ratio of an image that overshot its budget is already the shrink-retry's
// input, and feeding a rejected attempt in as well would let a disc that was
// never written decide how the rest of the set is packed.
func (e *ratioEstimator) observe(measured float64) (next float64, window []float64, ok bool) {
	if !e.adapt {
		return 0, nil, false
	}
	if !(measured > 0) || math.IsInf(measured, 0) || math.IsNaN(measured) {
		return 0, nil, false
	}
	e.measured = append(e.measured, measured)

	w := e.recent()
	if len(w) == 0 {
		// Unreachable: the measurement above was just appended. Kept as the
		// explicit refusal to estimate from nothing.
		return 0, nil, false
	}
	worst := w[0]
	for _, r := range w[1:] {
		if r > worst {
			worst = r
		}
	}
	next = worst * e.margin
	if next > ratioCeil {
		next = ratioCeil
	}
	if next < ratioFloor {
		next = ratioFloor
	}
	// brb.sh formats the result to three decimals; reproducing the rounding
	// keeps the two implementations planning identical discs.
	return round3(next), append([]float64(nil), w...), true
}

// adapt re-estimates the pack ratio from the disc just accepted and reports any
// change, so the operator can see the number that decides how full the
// remaining discs come out.
func (r *runner) adapt(measured float64) {
	next, window, ok := r.est.observe(measured)
	if !ok || next == r.packRatio {
		return
	}
	r.p.Step("pack ratio %.3f -> %.3f (worst of the last %d disc(s) measured: %s)",
		r.packRatio, next, len(window), formatRatios(window))
	r.packRatio = next
	r.p.Step("raw content budget per disc: %s  (image budget %s / ratio %.3f)",
		ui.HumanBytes(rawBudget(r.budget.Image, next)), ui.HumanBytes(r.budget.Image), next)
}

// resumeRatio restores what an interrupted run had learned: the pack ratio it
// had reached, and the measurements it reached it from.
//
// The ratio alone is not enough. With an empty estimator the first disc of the
// resumed run would set the ratio from that disc alone, which is precisely the
// single-disc estimate the window exists to avoid — one disc of JPEGs part way
// through a set of text would then plan the rest of it as if the text were
// JPEGs, or the reverse.
func (r *runner) resumeRatio(old *State) {
	if old.PackRatio > 0 {
		r.packRatio = old.PackRatio
	}
	if len(old.MeasuredRatios) == 0 {
		return
	}
	carried := r.est.carry(old.MeasuredRatios)
	if len(carried) == 0 {
		r.p.Warn("%s records %d measured ratio(s) and none of them is a usable number; "+
			"continuing from pack ratio %.3f", r.statePath, len(old.MeasuredRatios), r.packRatio)
		return
	}
	r.p.Step("carrying forward %d measured ratio(s): %s", len(carried), formatRatios(carried))
}

// formatRatios renders a window for the log, oldest first.
func formatRatios(w []float64) string {
	parts := make([]string, 0, len(w))
	for _, r := range w {
		parts = append(parts, fmt.Sprintf("%.3f", r))
	}
	return strings.Join(parts, " ")
}
