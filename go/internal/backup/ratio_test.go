package backup

import (
	"bytes"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
	"github.com/jzbz/brb/internal/ui"
)

// estimator builds one with the defaults a run uses, so a test that cares about
// a single knob only has to name that knob.
func estimator(t *testing.T, tweak func(*config.Config)) *ratioEstimator {
	t.Helper()
	c := config.Default()
	if tweak != nil {
		tweak(c)
	}
	return newRatioEstimator(c)
}

// TestASingleMeasurementIsNotAnEmptyWindow is the regression test for the bug
// the bash implementation shipped: taking "the last N" of a list shorter than N
// yielded nothing there, so the estimate collapsed to the clamp floor whatever
// had been measured. On content compressing to 0.9 that plans fifty
// disc-budgets of files onto the next disc, writes it, measures it, rejects it
// and rebuilds it — repeatedly, over multiple gigabytes.
//
// One measurement must produce min(1.0, measured)*margin, and never the floor.
func TestASingleMeasurementIsNotAnEmptyWindow(t *testing.T) {
	tests := []struct {
		name     string
		measured float64
		want     float64
	}{
		{"barely compressible content is not mistaken for 50:1", 0.9, 0.945},
		{"typical mixed content", 0.4, 0.42},
		{"content that does not compress at all still gets the margin", 1.0, 1.05},
		{"the margin is not eaten as the measurement approaches 1.0", 0.98, 1.029},
		{"a measurement above the ceiling is clamped, not wrapped", 1.4, 1.05},
		{"only genuinely extreme content reaches the floor", 0.005, ratioFloor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := estimator(t, nil)
			got, window, ok := e.observe(tc.measured)
			if !ok {
				t.Fatalf("observe(%v) declined to estimate", tc.measured)
			}
			if got != tc.want {
				t.Errorf("observe(%v) = %v, want %v", tc.measured, got, tc.want)
			}
			if len(window) != 1 {
				t.Fatalf("window = %v, want the one measurement taken so far", window)
			}
			if tc.want != ratioFloor && got == ratioFloor {
				t.Errorf("observe(%v) collapsed to the clamp floor", tc.measured)
			}
		})
	}
}

// TestTheEstimateFollowsTheWorstOfTheWindow covers the whole point of a window:
// it must not be talked down by one compressible disc, and it must come back
// down once an incompressible stretch has scrolled out of it. A running maximum
// would satisfy the first half and never the second.
func TestTheEstimateFollowsTheWorstOfTheWindow(t *testing.T) {
	e := estimator(t, func(c *config.Config) { c.PackRatioWindow = 3 })

	steps := []struct {
		measured float64
		want     float64
		why      string
	}{
		{0.20, 0.21, "the first disc sets the estimate on its own"},
		{0.30, 0.315, "a worse disc raises it"},
		{0.25, 0.315, "a better disc does not lower it while the worse one is in the window"},
		{0.90, 0.945, "a disc of JPEGs raises it immediately"},
		{0.10, 0.945, "the JPEGs still dominate the window"},
		{0.10, 0.945, "and still do"},
		{0.10, 0.105, "once they are out of the window the estimate falls again"},
	}
	for i, s := range steps {
		got, _, ok := e.observe(s.measured)
		if !ok {
			t.Fatalf("step %d: observe(%v) declined to estimate", i, s.measured)
		}
		if got != s.want {
			t.Errorf("step %d (%s): after %v the estimate is %v, want %v", i, s.why, s.measured, got, s.want)
		}
	}
	if n := len(e.recent()); n != 3 {
		t.Errorf("the window holds %d entries, want 3", n)
	}
}

// TestTheEstimatorDoesNotUndoAShrinkRetry. An image can come out larger than
// the raw bytes it was built from, and buildImage's shrink loop is the only
// thing that notices: it raises the pack ratio above 1.0 and rebuilds the
// disc, at the cost of a second full mksquashfs pass over multiple gigabytes.
// adapt runs immediately afterwards on the disc that finally fit, and while
// the estimate was clamped at 1.0 it handed the next disc back the very ratio
// that had just been measured to overshoot — so a set of already-compressed
// content (photos, video, archives) rebuilt every disc it built, and the log
// showed the estimator cancelling the correction one line after making it.
func TestTheEstimatorDoesNotUndoAShrinkRetry(t *testing.T) {
	r := runnerFor(t, func(c *config.Config) { c.PackRatio = 1.0 })

	// The numbers buildImage would have: an image 0.3% larger than its raw
	// input, which is what squashfs does to incompressible content.
	const raw, image = 2_000_000_000, 2_005_000_000
	measured := measuredRatio(image, raw)
	r.packRatio = shrinkRatio(image, raw) // what the shrink loop re-packs with
	if r.packRatio <= 1.0 {
		t.Fatalf("fixture: shrinkRatio(%d, %d) = %v, want a ratio above 1.0", image, raw, r.packRatio)
	}

	r.adapt(measured)

	if r.packRatio <= 1.0 {
		t.Fatalf("after adapt(%.3f) the pack ratio is %v: the shrink retry's correction was "+
			"discarded, so the next disc is planned to overshoot again", measured, r.packRatio)
	}
}

// TestAdaptationOffKeepsTheConfiguredRatio: PACK_RATIO_ADAPT=0 must not merely
// dampen the estimate, it must not make one at all.
func TestAdaptationOffKeepsTheConfiguredRatio(t *testing.T) {
	e := estimator(t, func(c *config.Config) { c.PackRatioAdapt = false })
	for _, m := range []float64{0.2, 0.9, 0.05} {
		if _, _, ok := e.observe(m); ok {
			t.Fatalf("observe(%v) estimated with PACK_RATIO_ADAPT=0", m)
		}
	}

	r := runnerFor(t, func(c *config.Config) {
		c.PackRatioAdapt = false
		c.PackRatio = 0.62
	})
	r.adapt(0.10)
	r.adapt(0.90)
	if r.packRatio != 0.62 {
		t.Errorf("pack ratio moved to %v with PACK_RATIO_ADAPT=0, want the configured 0.62", r.packRatio)
	}
}

// TestNonsenseMeasurementsAreRefused. An image of zero bytes, or a raw total of
// zero, would otherwise enter the window as a ratio of 0 and take the estimate
// straight to the clamp floor.
func TestNonsenseMeasurementsAreRefused(t *testing.T) {
	e := estimator(t, nil)
	for _, m := range []float64{0, -0.5, math.NaN(), math.Inf(1)} {
		if _, _, ok := e.observe(m); ok {
			t.Errorf("observe(%v) was accepted as a measurement", m)
		}
	}
	if len(e.measured) != 0 {
		t.Errorf("the window kept %v", e.measured)
	}
}

// TestAWindowShorterThanItsLimitIsTheWholeList pins recent() directly, since it
// is the function the bash implementation got wrong.
func TestAWindowShorterThanItsLimitIsTheWholeList(t *testing.T) {
	e := estimator(t, func(c *config.Config) { c.PackRatioWindow = 4 })
	e.measured = []float64{0.5, 0.6}
	if got := e.recent(); !reflect.DeepEqual(got, []float64{0.5, 0.6}) {
		t.Errorf("recent() = %v, want both measurements", got)
	}
	e.measured = []float64{0.1, 0.2, 0.3, 0.4, 0.5}
	if got := e.recent(); !reflect.DeepEqual(got, []float64{0.2, 0.3, 0.4, 0.5}) {
		t.Errorf("recent() = %v, want the last 4", got)
	}
}

// TestAnUnusableWindowFallsBackToTheDefault. Validate rejects PACK_RATIO_WINDOW
// below 1, so this can only be a Config built in code — but a window of zero is
// exactly the empty window this feature exists to avoid, so the estimator
// refuses it too rather than estimating from nothing.
func TestAnUnusableWindowFallsBackToTheDefault(t *testing.T) {
	for _, w := range []int{0, -3} {
		e := estimator(t, func(c *config.Config) { c.PackRatioWindow = w })
		if e.window < 1 {
			t.Fatalf("window %d survived into the estimator", e.window)
		}
		got, _, ok := e.observe(0.9)
		if !ok || got != 0.945 {
			t.Errorf("with PACK_RATIO_WINDOW=%d, observe(0.9) = (%v, %v), want (0.945, true)", w, got, ok)
		}
	}
	for _, m := range []float64{0, 0.5, -1, math.Inf(1)} {
		e := estimator(t, func(c *config.Config) { c.PackRatioMargin = m })
		if e.margin < 1 {
			t.Fatalf("margin %v survived into the estimator", e.margin)
		}
	}
}

// TestMarginIsApplied checks that the safety factor is the configured one, not
// a hard-coded 1.05.
func TestMarginIsApplied(t *testing.T) {
	e := estimator(t, func(c *config.Config) { c.PackRatioMargin = 1.5 })
	got, _, ok := e.observe(0.4)
	if !ok || got != 0.6 {
		t.Errorf("observe(0.4) at margin 1.5 = (%v, %v), want (0.6, true)", got, ok)
	}
}

// TestAdaptLogsTheTransitionAndTheNewBudget. The pack ratio decides how full
// every remaining disc comes out, so a change to it is not something to make
// silently.
func TestAdaptLogsTheTransitionAndTheNewBudget(t *testing.T) {
	var buf bytes.Buffer
	r := runnerFor(t, func(c *config.Config) { c.PackRatio = 1.0 })
	r.p = ui.New(&buf, false)

	r.adapt(0.400)
	if r.packRatio != 0.42 {
		t.Fatalf("pack ratio = %v, want 0.42", r.packRatio)
	}
	out := buf.String()
	for _, want := range []string{
		"pack ratio 1.000 -> 0.420",
		"worst of the last 1 disc(s) measured: 0.400",
		"raw content budget per disc:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q:\n%s", want, out)
		}
	}

	// An estimate that does not move must not print anything at all: on a
	// twenty-disc set of uniform content that would be twenty identical lines.
	buf.Reset()
	r.adapt(0.400)
	if out := buf.String(); out != "" {
		t.Errorf("an unchanged estimate logged:\n%s", out)
	}
	if r.packRatio != 0.42 {
		t.Errorf("pack ratio = %v, want it to have stayed at 0.42", r.packRatio)
	}
}

// TestAdaptRecomputesTheRawBudget proves the correction actually reaches the
// packer: a lower ratio must widen the raw budget the next bin is planned to.
func TestAdaptRecomputesTheRawBudget(t *testing.T) {
	r := runnerFor(t, func(c *config.Config) { c.PackRatio = 1.0 })
	before := RawBudget(r.budget.Image, r.packRatio)
	r.adapt(0.25)
	after := RawBudget(r.budget.Image, r.packRatio)
	if r.packRatio != 0.263 {
		t.Fatalf("pack ratio = %v, want 0.263 (0.25 x 1.05, to three decimals)", r.packRatio)
	}
	if after <= before {
		t.Errorf("raw budget went from %d to %d; a lower ratio must plan more raw content per disc",
			before, after)
	}
}

// TestResumeCarriesTheMeasurements. Without the measurements a resumed run
// starts with an empty window, so its first disc would set the ratio from that
// disc alone — the single-disc estimate the window exists to avoid.
func TestResumeCarriesTheMeasurements(t *testing.T) {
	var buf bytes.Buffer
	r := runnerFor(t, nil)
	r.p = ui.New(&buf, false)

	r.resumeRatio(&State{PackRatio: 0.315, MeasuredRatios: []float64{0.20, 0.30, 0.25}})
	if r.packRatio != 0.315 {
		t.Fatalf("pack ratio = %v, want the state's 0.315", r.packRatio)
	}
	if !strings.Contains(buf.String(), "carrying forward 3 measured ratio(s): 0.200 0.300 0.250") {
		t.Errorf("the log does not report the carried measurements:\n%s", buf.String())
	}

	// 0.30 is still the worst of the window, so one compressible disc must not
	// take the estimate down to its own level.
	got, window, ok := r.est.observe(0.10)
	if !ok {
		t.Fatal("observe declined to estimate after a resume")
	}
	if len(window) != 3 || got != 0.315 {
		t.Errorf("after resuming, observe(0.10) = %v from window %v, want 0.315 from the last 3", got, window)
	}
}

// TestResumeDropsUnusableMeasurements: state.json is a text file an operator can
// edit, and one negative entry would become the window's maximum, clamp to the
// floor and plan fifty disc-budgets onto the next disc.
func TestResumeDropsUnusableMeasurements(t *testing.T) {
	var buf bytes.Buffer
	r := runnerFor(t, nil)
	r.p = ui.New(&buf, false)

	r.resumeRatio(&State{PackRatio: 0.42, MeasuredRatios: []float64{-1, 0.40, 0}})
	if !reflect.DeepEqual(r.est.measured, []float64{0.40}) {
		t.Errorf("carried %v, want only the usable measurement", r.est.measured)
	}
	got, _, _ := r.est.observe(0.30)
	if got != 0.42 {
		t.Errorf("observe(0.30) = %v, want 0.42 from the surviving 0.40", got)
	}

	buf.Reset()
	r2 := runnerFor(t, nil)
	r2.p = ui.New(&buf, false)
	r2.resumeRatio(&State{PackRatio: 0.5, MeasuredRatios: []float64{-1, 0}})
	if len(r2.est.measured) != 0 {
		t.Errorf("carried %v, want nothing usable", r2.est.measured)
	}
	if !strings.Contains(buf.String(), "none of them is a usable number") {
		t.Errorf("an all-garbage list was carried silently:\n%s", buf.String())
	}
	if r2.packRatio != 0.5 {
		t.Errorf("pack ratio = %v, want the state's 0.5", r2.packRatio)
	}
}

// runnerFor builds a runner over a throwaway configuration, for the tests that
// only exercise the estimate and never touch a disk.
func runnerFor(t *testing.T, tweak func(*config.Config)) *runner {
	t.Helper()
	c := config.Default()
	c.SourceDir = t.TempDir()
	c.Staging = t.TempDir()
	c.ArchiveName = "ratio-test"
	if tweak != nil {
		tweak(c)
	}
	r, err := newRunner(Options{Cfg: c, UI: ui.New(io.Discard, false)})
	if err != nil {
		t.Fatalf("newRunner: %v", err)
	}
	return r
}
