package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/hyperized/rtl2832u"
)

// errTUITest is the static sentinel for TUI tests that need to
// assert errors propagate through model.recordError /
// renderFooter. Reused across tests so err113 stays happy.
var errTUITest = errors.New("rtl-probe tui test error")

func TestTUIModelRingBufferEviction(t *testing.T) {
	t.Parallel()

	model := &tuiModel{history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow)}

	for i := range tuiHistoryWindow + 5 {
		model.update(rtl2832u.SampleStats{Samples: i}, Spectrum{})
	}

	snap := model.snapshot()
	if len(snap.history) != tuiHistoryWindow {
		t.Errorf("history len = %d, want %d (ring should cap)", len(snap.history), tuiHistoryWindow)
	}

	// Oldest entry should be the first one not evicted: index 5
	// of the original sequence.
	if snap.history[0].Samples != 5 {
		t.Errorf("oldest entry = %d, want 5 (5 oldest entries evicted)", snap.history[0].Samples)
	}

	// Newest entry must be the last one we pushed.
	if snap.history[len(snap.history)-1].Samples != tuiHistoryWindow+4 {
		t.Errorf("newest entry = %d, want %d",
			snap.history[len(snap.history)-1].Samples, tuiHistoryWindow+4)
	}
}

func TestTUIModelSnapshotIsValueCopy(t *testing.T) {
	t.Parallel()

	model := &tuiModel{history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow)}
	model.update(rtl2832u.SampleStats{Samples: 100}, Spectrum{})

	snap := model.snapshot()

	// Mutating the snapshot must not affect the model's
	// internal state.
	snap.history[0].Samples = 999

	again := model.snapshot()
	if again.history[0].Samples != 100 {
		t.Errorf("mutation of snapshot leaked back into model: got %d, want 100", again.history[0].Samples)
	}
}

func TestTUIModelRecordError(t *testing.T) {
	t.Parallel()

	model := &tuiModel{history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow)}

	model.recordError(errTUITest)

	snap := model.snapshot()
	if !errors.Is(snap.lastErr, errTUITest) {
		t.Errorf("lastErr = %v, want %v", snap.lastErr, errTUITest)
	}
}

func TestRenderHistogramZeroDimensionsRendersEmpty(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	for _, dim := range []struct{ w, h int }{{0, 5}, {10, 0}, {-1, 5}, {10, -1}} {
		if got := renderHistogram(hist, dim.w, dim.h); got != "" {
			t.Errorf("renderHistogram(%dx%d) = %q, want empty", dim.w, dim.h, got)
		}
	}
}

func TestRenderHistogramTitleEmptyHistogram(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32
	if got := renderHistogramTitle(hist); !strings.Contains(got, "magnitude histogram") {
		t.Errorf("title missing identifier: %q", got)
	}
}

func TestRenderHistogramTitleIncludesMaxCount(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	hist[10] = 9999

	got := renderHistogramTitle(hist)
	if !strings.Contains(got, "max=9999") {
		t.Errorf("title missing max count: %q", got)
	}
}

func TestColorForHistogramColumnTiers(t *testing.T) {
	t.Parallel()

	const totalCols = 64

	cases := []struct {
		name string
		col  int
		want string
	}{
		{"under_gained_left_edge", 0, colorRed},
		{"marginal_low", 6, colorYellow},
		{"healthy_centre", 32, colorGreen},
		{"hot_bursts", 53, colorYellow},
		{"clipping_right_edge", 63, colorRed},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := colorForHistogramColumn(testCase.col, totalCols); got != testCase.want {
				t.Errorf("colorForHistogramColumn(%d, %d) = %q, want %q",
					testCase.col, totalCols, got, testCase.want)
			}
		})
	}
}

func TestRenderHistogramHasYAxisPercentages(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	hist[5] = 100

	got := renderHistogram(hist, 60, 8)
	// chartHeight = 6 → top row = 100%, then 83, 66, 50, 33, 16.
	if !strings.Contains(got, "100%") {
		t.Errorf("histogram missing top-row 100%% label: %q", got)
	}

	if !strings.Contains(got, "0% └") {
		t.Errorf("histogram missing 0%% axis label: %q", got)
	}
}

func TestRenderHistogramHasXAxisUnitCaption(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	hist[5] = 100

	got := renderHistogram(hist, 80, 8)
	if !strings.Contains(got, "|I+jQ|") {
		t.Errorf("histogram X-axis missing unit caption: %q", got)
	}
}

func TestRenderHistogramEmptyDataStillRendersAxis(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	got := renderHistogram(hist, 32, 5)
	if !strings.Contains(got, "181") {
		t.Errorf("empty-histogram render missing rightmost axis label 181: %q", got)
	}

	if !strings.Contains(got, "0") {
		t.Errorf("empty-histogram render missing leftmost axis label 0: %q", got)
	}
}

func TestRenderHistogramSingleBucketProducesBar(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	hist[0] = 1000

	const (
		width  = 32
		height = 5
	)

	got := renderHistogram(hist, width, height)
	lines := strings.Split(got, "\n")

	if len(lines) != height {
		t.Errorf("rows = %d, want %d", len(lines), height)
	}

	// The bottom *chart* row (above the two axis rows) should
	// have the full block in the column immediately after the
	// Y-axis label gutter — bucket 0 had all the counts. The
	// chart row carries tview colour markup, so we resolve the
	// display column via runeIndexForDisplayCol rather than a
	// raw rune index.
	const (
		axisRows       = 2
		yLabelGutterCh = 6
	)

	bottomChart := lines[height-axisRows-1]
	runes := []rune(bottomChart)

	idx := runeIndexForDisplayCol(runes, yLabelGutterCh)
	if idx < 0 || runes[idx] != '█' {
		t.Errorf("first chart cell not full block: %q", bottomChart)
	}
}

func TestRenderHistogramAllRowsAreFullWidth(t *testing.T) {
	t.Parallel()

	var hist [rtl2832u.HistogramBuckets]uint32

	for i := range hist {
		hist[i] = uint32(i + 1) //nolint:gosec // small constant test data.
	}

	const (
		width  = 64
		height = 6
	)

	out := renderHistogram(hist, width, height)
	if out == "" {
		t.Fatal("renderHistogram returned empty for non-empty histogram")
	}

	lines := strings.Split(out, "\n")
	if len(lines) != height {
		t.Fatalf("rows = %d, want %d (4 chart rows + 2 axis rows)", len(lines), height)
	}

	for lineIdx, line := range lines {
		// Chart rows carry tview colour markup ("[red]", "[-]")
		// which inflates the rune count. We care about the
		// visible cell count.
		visible := visibleCellCount([]rune(line))
		if visible != width {
			t.Errorf("line %d visible width = %d, want %d", lineIdx, visible, width)
		}
	}
}

// visibleCellCount returns the count of cells a rune slice
// occupies on screen after stripping tview's [tag] markup
// blocks. Mirrors runeIndexForDisplayCol's parsing logic.
func visibleCellCount(runes []rune) int {
	count := 0
	idx := 0

	for idx < len(runes) {
		if runes[idx] == '[' {
			closing := idx + 1
			for closing < len(runes) && runes[closing] != ']' {
				closing++
			}

			idx = closing + 1

			continue
		}

		count++
		idx++
	}

	return count
}

func TestRenderStripChartEmptyHistory(t *testing.T) {
	t.Parallel()

	if got := renderStripChart(nil, 40, 4); got != "" {
		t.Errorf("empty-history render = %q, want empty", got)
	}
}

func TestRenderStripChartProducesOneRowPerSeriesWithValues(t *testing.T) {
	t.Parallel()

	history := []rtl2832u.SampleStats{
		{RMS: 50, SaturationFrac: 0.05, Peak: 100, DCI: 0.5, DCQ: -0.5},
		{RMS: 55, SaturationFrac: 0.06, Peak: 110, DCI: 0.3, DCQ: -0.3},
	}

	out := renderStripChart(history, 80, 10)
	lines := strings.Split(out, "\n")

	if len(lines) != len(stripSeries) {
		t.Errorf("rows = %d, want %d (one per series)", len(lines), len(stripSeries))
	}

	// Each row starts with its series label.
	for idx, series := range stripSeries {
		if !strings.HasPrefix(lines[idx], series.label) {
			t.Errorf("row %d does not start with %q: %q", idx, series.label, lines[idx])
		}
	}

	// DC row prints sign + magnitude in the value column.
	dcQRow := lines[len(lines)-1]
	if !strings.Contains(dcQRow, "-0.30") {
		t.Errorf("dcQ row missing signed value -0.30: %q", dcQRow)
	}

	// Scale annotation lands at the right.
	rmsRow := lines[0]
	if !strings.Contains(rmsRow, "max=100") {
		t.Errorf("rms row missing scale annotation: %q", rmsRow)
	}
}

func TestRenderStripChartTooNarrowRendersEmpty(t *testing.T) {
	t.Parallel()

	history := []rtl2832u.SampleStats{{RMS: 50}}

	// New layout needs labelW(4) + 1 + valueW(8) + 1 + scaleAnnotationW(10) + 1 + minBar(4) = 29 minimum.
	if got := renderStripChart(history, 20, 4); got != "" {
		t.Errorf("too-narrow render = %q, want empty", got)
	}
}

func TestRenderHeaderIncludesFrameCount(t *testing.T) {
	t.Parallel()

	stats := rtl2832u.SampleStats{Samples: 1024, RMS: 12.3, Peak: 45.6, SaturationFrac: 0.123}

	got := renderHeader(stats, 7)
	if !strings.Contains(got, "frame=7") {
		t.Errorf("header missing frame count: %q", got)
	}

	if !strings.Contains(got, "samples=1024") {
		t.Errorf("header missing samples: %q", got)
	}

	if !strings.Contains(got, "sat=12.30%") {
		t.Errorf("header missing saturation %%: %q", got)
	}
}

func TestAverageStatsEmptyHistoryReturnsZero(t *testing.T) {
	t.Parallel()

	got := averageStats(nil, 10)
	if got != (rtl2832u.SampleStats{}) {
		t.Errorf("averageStats(nil) = %+v, want zero", got)
	}
}

func TestAverageStatsWindowSmallerThanHistory(t *testing.T) {
	t.Parallel()

	history := []rtl2832u.SampleStats{
		{RMS: 100, SaturationFrac: 1.0},
		{RMS: 200, SaturationFrac: 0.0},
		{RMS: 30, SaturationFrac: 0.1},
		{RMS: 40, SaturationFrac: 0.1},
	}

	// window=2 → average last 2: (30+40)/2 = 35
	got := averageStats(history, 2)
	if got.RMS != 35 {
		t.Errorf("RMS = %v, want 35 (avg of last 2)", got.RMS)
	}
}

func TestAverageStatsWindowLargerThanHistory(t *testing.T) {
	t.Parallel()

	history := []rtl2832u.SampleStats{
		{RMS: 10, SaturationFrac: 0.1},
		{RMS: 20, SaturationFrac: 0.2},
	}

	// window=100 → average all
	got := averageStats(history, 100)
	if got.RMS != 15 {
		t.Errorf("RMS = %v, want 15 (avg of all entries)", got.RMS)
	}
}

func TestAverageStatsSmoothsTransientSpike(t *testing.T) {
	t.Parallel()

	// 19 clean samples + one big spike. Window=20 → spike's
	// contribution to the mean is 1/20, so saturation lands at
	// ~0.05% not 1% — well within the "good" band.
	history := make([]rtl2832u.SampleStats, 0, 20)
	for range 19 {
		history = append(history, rtl2832u.SampleStats{RMS: 30, SaturationFrac: 0.001})
	}

	history = append(history, rtl2832u.SampleStats{RMS: 30, SaturationFrac: 0.20})

	got := averageStats(history, 20)
	if got.SaturationFrac > 0.02 {
		t.Errorf("SaturationFrac = %v, want < 0.02 (spike should be diluted)", got.SaturationFrac)
	}
}

func TestGradeRMSThresholds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value float64
		want  health
	}{
		{"muted", 1.0, healthBad},
		{"good_low", 10.0, healthGood},
		{"good_mid", 30.0, healthGood},
		{"good_high", 49.0, healthGood},
		{"marginal_low", 51.0, healthMarginal},
		{"marginal_high", 79.0, healthMarginal},
		{"compressed", 85.0, healthBad},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := gradeRMS(testCase.value); got != testCase.want {
				t.Errorf("gradeRMS(%v) = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
}

func TestGradeSaturationPercentThresholds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value float64
		want  health
	}{
		{"clean", 0.5, healthGood},
		{"good_edge", 1.0, healthGood},
		{"marginal", 3.0, healthMarginal},
		{"marginal_edge", 5.0, healthMarginal},
		{"bad", 10.0, healthBad},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := gradeSaturationPercent(testCase.value); got != testCase.want {
				t.Errorf("gradeSaturationPercent(%v) = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
}

func TestGradeDCAbsoluteValueAndSignInvariance(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value float64
		want  health
	}{
		{"zero", 0.0, healthGood},
		{"good_pos", 0.5, healthGood},
		{"good_neg", -0.5, healthGood},
		{"marginal_pos", 1.5, healthMarginal},
		{"marginal_neg", -1.5, healthMarginal},
		{"bad_pos", 3.0, healthBad},
		{"bad_neg", -3.0, healthBad},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := gradeDC(testCase.value); got != testCase.want {
				t.Errorf("gradeDC(%v) = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
}

func TestOverallHealthWorstWins(t *testing.T) {
	t.Parallel()

	// All clean → GOOD, no contributing labels.
	good, labels := overallHealth(rtl2832u.SampleStats{
		Samples: 1, RMS: 20, SaturationFrac: 0.001, DCI: 0.1, DCQ: -0.1,
	})
	if good != healthGood || len(labels) != 0 {
		t.Errorf("clean sample: got grade=%v labels=%v, want GOOD/empty", good, labels)
	}

	// One marginal series should pull the overall to MARGINAL
	// and name the offender.
	mid, labels := overallHealth(rtl2832u.SampleStats{
		Samples: 1, RMS: 20, SaturationFrac: 0.03, DCI: 0.0, DCQ: 0.0,
	})
	if mid != healthMarginal {
		t.Errorf("marginal sat: got %v, want MARGINAL", mid)
	}

	if len(labels) != 1 || labels[0] != "sat" {
		t.Errorf("marginal sat labels = %v, want [sat]", labels)
	}

	// Any BAD series beats MARGINAL.
	bad, _ := overallHealth(rtl2832u.SampleStats{
		Samples: 1, RMS: 20, SaturationFrac: 0.03, DCI: 3.0, DCQ: 0.0,
	})
	if bad != healthBad {
		t.Errorf("bad DC: got %v, want BAD", bad)
	}
}

func TestRenderStatusBannerNoSamples(t *testing.T) {
	t.Parallel()

	got := renderStatusBanner(rtl2832u.SampleStats{Samples: 0})
	if !strings.Contains(got, "waiting") {
		t.Errorf("zero-sample banner = %q, want a 'waiting' placeholder", got)
	}
}

func TestRenderStatusBannerNamesOffenders(t *testing.T) {
	t.Parallel()

	got := renderStatusBanner(rtl2832u.SampleStats{
		Samples: 1, RMS: 20, SaturationFrac: 0.10, DCI: 0, DCQ: 0,
	})
	if !strings.Contains(got, "BAD") {
		t.Errorf("banner missing BAD: %q", got)
	}

	if !strings.Contains(got, "sat") {
		t.Errorf("banner missing offender 'sat': %q", got)
	}
}

func TestRenderStripChartColoursValueByGrade(t *testing.T) {
	t.Parallel()

	history := []rtl2832u.SampleStats{
		{RMS: 90, SaturationFrac: 0.001, Peak: 100, DCI: 0, DCQ: 0}, // rms bad
	}

	out := renderStripChart(history, 80, 10)
	if !strings.Contains(out, "[red]") {
		t.Errorf("strip chart missing [red] tag for bad rms: %q", out)
	}

	if !strings.Contains(out, "[green]") {
		t.Errorf("strip chart missing [green] tag for good sat: %q", out)
	}
}

func TestDiagnoseMutedChainShortCircuits(t *testing.T) {
	t.Parallel()

	hints := diagnose(rtl2832u.SampleStats{Samples: 1, RMS: 1, Peak: 10, SaturationFrac: 0, DCI: 5, DCQ: 5})
	if len(hints) != 1 {
		t.Fatalf("muted-chain advice should short-circuit; got %d hints: %+v", len(hints), hints)
	}

	if !strings.Contains(hints[0].message, "chain muted") {
		t.Errorf("muted advice missing diagnostic phrase: %q", hints[0].message)
	}
}

func TestDiagnoseCompressionAdvice(t *testing.T) {
	t.Parallel()

	hints := diagnose(rtl2832u.SampleStats{Samples: 1, RMS: 90, Peak: 181, SaturationFrac: 0.1})
	if len(hints) == 0 {
		t.Fatal("compressed chain produced no advice")
	}

	found := false

	for _, hint := range hints {
		if strings.Contains(hint.message, "front-end compressed") {
			found = true
		}
	}

	if !found {
		t.Errorf("compression advice missing: %+v", hints)
	}
}

func TestDiagnoseClippingAdvice(t *testing.T) {
	t.Parallel()

	// RMS healthy, but saturation high → bursts clip but chain
	// itself ok. Should suggest reducing gain (marginal severity).
	hints := diagnose(rtl2832u.SampleStats{Samples: 1, RMS: 45, Peak: 181, SaturationFrac: 0.08})

	found := false

	for _, hint := range hints {
		if strings.Contains(hint.message, "ADC clipping") {
			found = true
		}
	}

	if !found {
		t.Errorf("clipping advice missing: %+v", hints)
	}
}

func TestDiagnoseUnderGainedAdvice(t *testing.T) {
	t.Parallel()

	// Noise floor below quantisation but peaks visible → tell
	// the operator to bump gain.
	hints := diagnose(rtl2832u.SampleStats{Samples: 1, RMS: 3, Peak: 120, SaturationFrac: 0})

	found := false

	for _, hint := range hints {
		if strings.Contains(hint.message, "noise floor below quantisation") {
			found = true
		}
	}

	if !found {
		t.Errorf("under-gained advice missing: %+v", hints)
	}
}

func TestDiagnoseDCOffsetAdvice(t *testing.T) {
	t.Parallel()

	hints := diagnose(rtl2832u.SampleStats{Samples: 1, RMS: 25, Peak: 100, DCI: 3.0, DCQ: 0})

	found := false

	for _, hint := range hints {
		if strings.Contains(hint.message, "DC offset large") {
			found = true
		}
	}

	if !found {
		t.Errorf("DC advice missing: %+v", hints)
	}
}

func TestDiagnoseHealthyChainReturnsEmpty(t *testing.T) {
	t.Parallel()

	hints := diagnose(rtl2832u.SampleStats{Samples: 1, RMS: 25, Peak: 120, SaturationFrac: 0.005, DCI: 0, DCQ: 0})
	if len(hints) != 0 {
		t.Errorf("healthy chain emitted advice: %+v", hints)
	}
}

func TestRenderAdviceBannerHealthyShowsGreen(t *testing.T) {
	t.Parallel()

	got := renderAdviceBanner(rtl2832u.SampleStats{
		Samples: 1, RMS: 25, Peak: 120, SaturationFrac: 0.005, DCI: 0, DCQ: 0,
	})
	if !strings.Contains(got, "[green]") || !strings.Contains(got, "healthy") {
		t.Errorf("healthy banner = %q, want green 'healthy' marker", got)
	}
}

func TestRenderAdviceBannerJoinsMultipleHints(t *testing.T) {
	t.Parallel()

	// Compressed + DC bad → two hints joined by separator.
	got := renderAdviceBanner(rtl2832u.SampleStats{
		Samples: 1, RMS: 90, Peak: 181, SaturationFrac: 0.05, DCI: 3, DCQ: 0,
	})
	if !strings.Contains(got, "·") {
		t.Errorf("multi-hint banner missing separator: %q", got)
	}
}

func TestBlendSpectrumEmptyFresh(t *testing.T) {
	t.Parallel()

	prev := Spectrum{BinDB: []float64{-10, -20}, CentreBin: 1}
	got := blendSpectrum(prev, Spectrum{}, 0.2)

	if &got != &prev && len(got.BinDB) != len(prev.BinDB) {
		t.Errorf("empty fresh should pass prev through, got len=%d", len(got.BinDB))
	}
}

func TestBlendSpectrumLengthMismatchResets(t *testing.T) {
	t.Parallel()

	prev := Spectrum{BinDB: []float64{-10, -20}, CentreBin: 1}
	fresh := Spectrum{BinDB: []float64{-30, -40, -50, -60}, CentreBin: 2}

	got := blendSpectrum(prev, fresh, 0.2)
	if len(got.BinDB) != len(fresh.BinDB) {
		t.Fatalf("len=%d, want %d (reset to fresh)", len(got.BinDB), len(fresh.BinDB))
	}

	for i, value := range got.BinDB {
		if value != fresh.BinDB[i] {
			t.Errorf("bin %d = %v, want %v (length mismatch should reset to fresh)", i, value, fresh.BinDB[i])
		}
	}
}

func TestBlendSpectrumEMAFormula(t *testing.T) {
	t.Parallel()

	prev := Spectrum{BinDB: []float64{-10, -20}, CentreBin: 1}
	fresh := Spectrum{BinDB: []float64{-30, -40}, CentreBin: 1}
	alpha := 0.25

	got := blendSpectrum(prev, fresh, alpha)

	// EMA: out = alpha*fresh + (1-alpha)*prev
	want0 := alpha*(-30) + (1-alpha)*(-10) // = -15
	want1 := alpha*(-40) + (1-alpha)*(-20) // = -25

	if math.Abs(got.BinDB[0]-want0) > 1e-9 {
		t.Errorf("bin 0 = %v, want %v", got.BinDB[0], want0)
	}

	if math.Abs(got.BinDB[1]-want1) > 1e-9 {
		t.Errorf("bin 1 = %v, want %v", got.BinDB[1], want1)
	}
}

func TestRenderFooterErrorBranch(t *testing.T) {
	t.Parallel()

	state := gainState{lnaStep: 15, mixerStep: 15, vgaStep: 15}

	clean := renderFooter(state, nil, nil)
	if !strings.Contains(clean, "quit") {
		t.Errorf("clean footer missing quit hint: %q", clean)
	}

	if !strings.Contains(clean, "LNA=15") {
		t.Errorf("clean footer missing gain state: %q", clean)
	}

	withSamplerErr := renderFooter(state, errTUITest, nil)
	if !strings.Contains(withSamplerErr, "sampler error: rtl-probe tui test error") {
		t.Errorf("sampler-error footer missing diagnostic: %q", withSamplerErr)
	}

	withControlErr := renderFooter(state, nil, errTUITest)
	if !strings.Contains(withControlErr, "last control: rtl-probe tui test error") {
		t.Errorf("control-error footer missing diagnostic: %q", withControlErr)
	}
}

func TestRunSamplerStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	model := &tuiModel{history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow)}
	// Payload large enough that the sampler's readFrame hits
	// targetBytes in one Read pass and emits a frame quickly.
	stub := &stubRawSampler{payload: make([]byte, tuiSampleTarget*2)}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		runSampler(ctx, stub, model)
		close(done)
	}()

	// Give the loop a chance to update at least once, then cancel.
	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSampler did not return within 1s of cancellation")
	}
}

// stubRawSampler returns a fixed byte payload / error on every
// Read call. Suitable for exercising the sampler loop without an
// open device.
type stubRawSampler struct {
	payload []byte
	err     error
}

func (s *stubRawSampler) Read(_ context.Context, dst []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}

	return copy(dst, s.payload), nil
}

// stubTUIReceiver is a minimal tuiReceiver for exercising the
// keybind / control path without an open device.
type stubTUIReceiver struct {
	stubRawSampler

	lna, mixer, vga uint8
	lnaCalls        int
	mixerCalls      int
	vgaCalls        int
	biasOn          bool
	biasCalls       int
	forceSetErr     error
}

func (s *stubTUIReceiver) SetLNAGain(step uint8) error {
	s.lna = step
	s.lnaCalls++

	return s.forceSetErr
}

func (s *stubTUIReceiver) SetMixerGain(step uint8) error {
	s.mixer = step
	s.mixerCalls++

	return s.forceSetErr
}

func (s *stubTUIReceiver) SetVGAGain(step uint8) error {
	s.vga = step
	s.vgaCalls++

	return s.forceSetErr
}

func (s *stubTUIReceiver) SetBiasTee(enable bool) error {
	s.biasOn = enable
	s.biasCalls++

	return s.forceSetErr
}

func TestHandleGainKeyLNAUpDown(t *testing.T) {
	t.Parallel()

	rcv := &stubTUIReceiver{}
	model := &tuiModel{
		history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow),
		gain:    gainState{lnaStep: 5, mixerStep: 10, vgaStep: 12},
	}

	if !handleGainKey(rcv, model, 'l') {
		t.Fatal("'l' key was not consumed")
	}

	if rcv.lna != 6 || rcv.lnaCalls != 1 {
		t.Errorf("after 'l': lna=%d calls=%d, want 6 / 1", rcv.lna, rcv.lnaCalls)
	}

	if !handleGainKey(rcv, model, 'L') {
		t.Fatal("'L' key was not consumed")
	}

	if rcv.lna != 5 {
		t.Errorf("after 'L' from 6: lna=%d, want 5", rcv.lna)
	}
}

func TestHandleGainKeyClampsAtBounds(t *testing.T) {
	t.Parallel()

	rcv := &stubTUIReceiver{}
	model := &tuiModel{
		history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow),
		gain:    gainState{lnaStep: 0},
	}

	// Walk down from 0 should stay at 0 (no uint8 underflow).
	handleGainKey(rcv, model, 'L')

	if got := model.snapshot().gain.lnaStep; got != 0 {
		t.Errorf("LNA underflow not clamped: got %d, want 0", got)
	}

	// Walk up past max should stay at maxR860Step.
	model.setGain(gainState{lnaStep: maxR860Step})
	handleGainKey(rcv, model, 'l')

	if got := model.snapshot().gain.lnaStep; got != maxR860Step {
		t.Errorf("LNA overflow not clamped: got %d, want %d", got, maxR860Step)
	}
}

func TestHandleGainKeyBiasTeeToggles(t *testing.T) {
	t.Parallel()

	rcv := &stubTUIReceiver{}
	model := &tuiModel{
		history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow),
	}

	handleGainKey(rcv, model, 'b')

	if !rcv.biasOn || !model.snapshot().gain.biasOn {
		t.Error("first 'b' did not turn bias on")
	}

	handleGainKey(rcv, model, 'b')

	if rcv.biasOn || model.snapshot().gain.biasOn {
		t.Error("second 'b' did not turn bias off")
	}
}

func TestHandleGainKeyUnrelatedKeyNotConsumed(t *testing.T) {
	t.Parallel()

	rcv := &stubTUIReceiver{}
	model := &tuiModel{history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow)}

	if handleGainKey(rcv, model, 'x') {
		t.Error("unrelated key 'x' was consumed, want false")
	}

	if rcv.lnaCalls+rcv.mixerCalls+rcv.vgaCalls+rcv.biasCalls != 0 {
		t.Error("unrelated key triggered receiver call")
	}
}

func TestHandleGainKeyRecordsError(t *testing.T) {
	t.Parallel()

	rcv := &stubTUIReceiver{forceSetErr: errTUITest}
	model := &tuiModel{
		history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow),
	}

	handleGainKey(rcv, model, 'l')

	snap := model.snapshot()
	if !errors.Is(snap.lastControlErr, errTUITest) {
		t.Errorf("control error not recorded: got %v, want %v", snap.lastControlErr, errTUITest)
	}
}
