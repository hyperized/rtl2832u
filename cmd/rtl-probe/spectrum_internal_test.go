package main

import (
	"math"
	"strings"
	"testing"
)

func TestComputeSpectrumShortInputReturnsEmpty(t *testing.T) {
	t.Parallel()

	tiny := make([]byte, 10)
	if got := computeSpectrum(tiny); got.BinDB != nil {
		t.Errorf("short input produced non-empty spectrum: %+v", got)
	}
}

func TestComputeSpectrumDCSampleHasPeakAtCentre(t *testing.T) {
	t.Parallel()

	// Constant I=Q after offset-removal (raw 128 maps to 0). Hann
	// window will modulate, but the DC bin (centre after fftshift)
	// should still dominate.
	raw := make([]byte, spectrumFFTSize*2)
	for i := range raw {
		raw[i] = 128 + 30
	}

	spec := computeSpectrum(raw)
	if len(spec.BinDB) != spectrumFFTSize {
		t.Fatalf("BinDB len = %d, want %d", len(spec.BinDB), spectrumFFTSize)
	}

	peakBin := 0

	for i, dbVal := range spec.BinDB {
		if dbVal > spec.BinDB[peakBin] {
			peakBin = i
		}
	}

	if peakBin != spec.CentreBin {
		t.Errorf("peak bin = %d, centre = %d (DC sample should peak at centre after fftshift)",
			peakBin, spec.CentreBin)
	}
}

func TestDownsampleSpectrumLowerCountUsesMaxPool(t *testing.T) {
	t.Parallel()

	bins := []float64{-10, -20, -30, -40, -50, -60, -70, -80}

	cols := downsampleSpectrum(bins, 4)
	// Each output column max-pools 2 input bins.
	want := []float64{-10, -30, -50, -70}

	for idx, wantValue := range want {
		if math.Abs(cols[idx]-wantValue) > 1e-9 {
			t.Errorf("col[%d] = %v, want %v", idx, cols[idx], wantValue)
		}
	}
}

func TestSpectrumPeakDB(t *testing.T) {
	t.Parallel()

	bins := []float64{-60, -50, -10, -30, -90}
	if got := spectrumPeakDB(bins); got != -10 {
		t.Errorf("peak = %v, want -10", got)
	}
}

func TestSpectrumPeakDBEmptyReturnsNegInf(t *testing.T) {
	t.Parallel()

	if got := spectrumPeakDB(nil); !math.IsInf(got, -1) {
		t.Errorf("empty peak = %v, want -Inf", got)
	}
}

func TestSpectrumScaleTrackerSnapsUpDecaysDown(t *testing.T) {
	t.Parallel()

	var scale spectrumScaleTracker

	// First update initialises to the value.
	if got := scale.update(-10, 0.05); got != -10 {
		t.Errorf("first update = %v, want -10", got)
	}

	// A higher peak snaps the top up.
	if got := scale.update(-5, 0.05); got != -5 {
		t.Errorf("snap-up = %v, want -5", got)
	}

	// A lower peak triggers decay (5 dB/s × 1 s = 5 dB drop,
	// but only down to the new peak).
	got := scale.update(-30, 1.0)
	want := -10.0 // -5 - 5 = -10

	if got != want {
		t.Errorf("decay = %v, want %v", got, want)
	}
}

func TestSpectrumScaleTrackerDecayClampedToCurrentPeak(t *testing.T) {
	t.Parallel()

	var scale spectrumScaleTracker

	scale.update(-10, 0.05)

	// A long elapsed time would decay past the current peak,
	// but the tracker clamps to the floor (= current peak).
	got := scale.update(-30, 60.0)
	if got != -30 {
		t.Errorf("over-decayed top = %v, want -30 (clamped to current peak)", got)
	}
}

func TestSpectrumScaleTrackerIgnoresNegInfPeak(t *testing.T) {
	t.Parallel()

	var scale spectrumScaleTracker

	scale.update(-10, 0.05)

	// Empty spectrum (peak = -Inf) shouldn't disturb the top.
	got := scale.update(math.Inf(-1), 1.0)
	if got != -10 {
		t.Errorf("no-data update changed top = %v, want -10", got)
	}
}

func TestColorForSpectrumRowTiers(t *testing.T) {
	t.Parallel()

	const height = 9 // chartHeight; tier boundaries at row 3 and row 6.

	cases := []struct {
		name string
		row  int
		want string
	}{
		{"bottom_row_red", 0, colorRed},
		{"red_second_row", 1, colorRed},
		{"yellow_lower_edge", 3, colorYellow},
		{"yellow_middle", 4, colorYellow},
		{"green_upper_third", 6, colorGreen},
		{"top_row_green", 8, colorGreen},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := colorForSpectrumRow(testCase.row, height); got != testCase.want {
				t.Errorf("colorForSpectrumRow(%d, %d) = %q, want %q",
					testCase.row, height, got, testCase.want)
			}
		})
	}
}

func TestEstimateFloorDBPercentile(t *testing.T) {
	t.Parallel()

	// 8 bins: most at the noise floor, a few real peaks. 25th
	// percentile (index 2 after sort) lands on the floor.
	cols := []float64{-50, -50, -50, -50, -45, -20, -10, -5}

	if got := estimateFloorDB(cols); got != -50 {
		t.Errorf("floor estimate = %v, want -50", got)
	}
}

func TestEstimateFloorDBEmptyReturnsNegInf(t *testing.T) {
	t.Parallel()

	if got := estimateFloorDB(nil); !math.IsInf(got, -1) {
		t.Errorf("empty floor = %v, want -Inf", got)
	}
}

func TestSpectrumBaselineTrackerEMAConverges(t *testing.T) {
	t.Parallel()

	var tracker spectrumBaselineTracker

	// First sample initialises.
	tracker.update(-60, 0.1)

	// Many small steps at -50; baseline should converge toward
	// -50 with tau ≈ 30 s. 120 s at 100 ms/step = 4 time
	// constants → ~98% convergence → within 0.5 dB.
	for range 1200 {
		tracker.update(-50, 0.1)
	}

	const tolerance = 0.5
	if math.Abs(tracker.baseline-(-50)) > tolerance {
		t.Errorf("baseline = %v, want within %v of -50", tracker.baseline, tolerance)
	}
}

func TestSpectrumBaselineTrackerIgnoresNegInf(t *testing.T) {
	t.Parallel()

	var tracker spectrumBaselineTracker

	tracker.update(-50, 0.1)
	// Empty-spectrum update must not corrupt the running value.
	got := tracker.update(math.Inf(-1), 1.0)

	if got != -50 {
		t.Errorf("NegInf update changed baseline = %v, want -50", got)
	}
}

func TestRuneIndexForDisplayColSkipsMarkup(t *testing.T) {
	t.Parallel()

	// "   [red]██[-]   " — 8 visible cells (3 spaces, 2 blocks,
	// 3 spaces). Display columns 3 and 4 are the bars; the rune
	// indices that correspond to them sit after the "[red]"
	// markup, not at runes 3-4.
	runes := []rune("   [red]██[-]   ")

	idx := runeIndexForDisplayCol(runes, 3)
	if idx < 0 || runes[idx] != '█' {
		t.Errorf("display col 3 = rune %q at idx %d, want '█'", runes[idx], idx)
	}

	idx = runeIndexForDisplayCol(runes, 4)
	if idx < 0 || runes[idx] != '█' {
		t.Errorf("display col 4 = rune %q at idx %d, want '█'", runes[idx], idx)
	}

	// Empty cell after the bar.
	idx = runeIndexForDisplayCol(runes, 5)
	if idx < 0 || runes[idx] != ' ' {
		t.Errorf("display col 5 = rune %q at idx %d, want ' '", runes[idx], idx)
	}

	// Past the end of the visible row.
	if got := runeIndexForDisplayCol(runes, 100); got != -1 {
		t.Errorf("out-of-range display col = %d, want -1", got)
	}
}

func TestBaselineRowForOutsideRangeReturnsNegOne(t *testing.T) {
	t.Parallel()

	if got := baselineRowFor(math.Inf(-1), -10, 6); got != -1 {
		t.Errorf("inf baseline = %d, want -1", got)
	}

	// Baseline below the displayed range.
	if got := baselineRowFor(-100, -10, 6); got != -1 {
		t.Errorf("below-range baseline = %d, want -1", got)
	}
}

func TestBaselineRowForTopAndBottom(t *testing.T) {
	t.Parallel()

	// displayTop = 0, chartHeight = 6. Baseline at displayTop
	// lands at row 0 (top); at displayTop - spectrumDynamicRangeDB
	// at row 5 (bottom).
	if got := baselineRowFor(0, 0, 6); got != 0 {
		t.Errorf("baseline at displayTop = row %d, want 0 (top)", got)
	}

	if got := baselineRowFor(-spectrumDynamicRangeDB, 0, 6); got != 5 {
		t.Errorf("baseline at displayBottom = row %d, want 5 (bottom)", got)
	}
}

func TestRenderSpectrumIncludesAxisAndMarker(t *testing.T) {
	t.Parallel()

	raw := make([]byte, spectrumFFTSize*2)
	for i := range raw {
		raw[i] = 128 + 30
	}

	spec := computeSpectrum(raw)

	out := renderSpectrum(spec, spectrumPeakDB(spec.BinDB), math.Inf(-1), 60, 8)
	if !strings.Contains(out, "MHz") {
		t.Errorf("spectrum render missing MHz unit caption: %q", out)
	}

	if !strings.Contains(out, "▲") {
		t.Errorf("spectrum render missing centre-frequency marker (▲): %q", out)
	}

	// The dB unit lives in the pane title (renderSpectrumTitle),
	// not in the Y-axis labels themselves — keeping each label
	// 6 runes wide so chart rows don't overflow the pane and
	// trigger tview line-wrap.
	if !strings.Contains(out, "┤") {
		t.Errorf("spectrum render missing Y-axis tick characters: %q", out)
	}
}

func TestRenderSpectrumEmptyInputRendersPlaceholder(t *testing.T) {
	t.Parallel()

	got := renderSpectrum(Spectrum{}, 0, math.Inf(-1), 60, 8)
	if !strings.Contains(got, "MHz") {
		t.Errorf("empty spectrum should still render the axis: %q", got)
	}
}

func TestRenderSpectrumTitleIncludesSampleRate(t *testing.T) {
	t.Parallel()

	got := renderSpectrumTitle(2_400_000)
	if !strings.Contains(got, "1.20") {
		t.Errorf("title missing ±sample-rate/2 (MHz): %q", got)
	}
}

func TestRenderSpectrumTooNarrowRendersEmpty(t *testing.T) {
	t.Parallel()

	if got := renderSpectrum(Spectrum{BinDB: []float64{-30}}, -30, math.Inf(-1), 5, 8); got != "" {
		t.Errorf("too-narrow render = %q, want empty", got)
	}
}
