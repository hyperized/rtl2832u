package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// hzPerMHz scales a sample-rate field from Hz to MHz for the
// spectrum pane title and axis labels.
const hzPerMHz = 1_000_000.0

// spectrumCentreFraction maps the chart's centre column to the
// half-width position. Kept symbolic for the few uses below so
// the magic-number lint stays happy.
const spectrumCentreFraction = 0.5

// spectrumFFTSize is the FFT length the TUI uses. 512 points
// gives ~4.7 kHz frequency resolution at 2.4 MS/s, easily
// downsampled to a typical 80-column terminal. Powers of two
// only — fftInPlace panics otherwise.
const spectrumFFTSize = 512

// Spectrum is the magnitude-in-dB array produced by one FFT
// window plus the centre-bin index used to mark DC (centre
// frequency) on the display. Bins are in fftshift order: index 0
// = most-negative frequency, len/2 = DC, len-1 = most-positive.
type Spectrum struct {
	BinDB     []float64
	CentreBin int
}

// computeSpectrum runs Welch's method over the input I/Q buffer:
// many overlapping windowed FFTs are averaged in the power
// (magnitude-squared) domain before conversion to dB. This is the
// difference between "the spectrum bounces wildly every frame
// because we sampled one ADS-B burst" and "the spectrum settles
// to a stable shape because we averaged ~256 short FFTs across
// the whole 54 ms sample window."
//
// Output BinDB is fftshift-applied so the DC bin sits at len/2
// — natural reading order for an SDR baseband display, where the
// centre column on screen corresponds to the tuned centre
// frequency.
//
//nolint:varnamelen // FFT scratch buffer follows the canonical 'x' name.
func computeSpectrum(raw []byte) Spectrum {
	const (
		bytesPerSample = 2
		// hopFraction = 2 → 50% overlap between successive FFT
		// windows. Standard for Welch with Hann; gives more
		// segments per frame for better averaging without
		// losing too much independent information.
		hopFraction = 2
	)

	samples := len(raw) / bytesPerSample
	if samples < spectrumFFTSize {
		return Spectrum{}
	}

	hop := spectrumFFTSize / hopFraction
	numSegments := (samples-spectrumFFTSize)/hop + 1

	const dcOffset = 128

	x := make([]complex128, spectrumFFTSize)
	powerSum := make([]float64, spectrumFFTSize)
	halfN := spectrumFFTSize / 2

	for seg := range numSegments {
		startSample := seg * hop

		for i := range x {
			rawIdx := (startSample + i) * bytesPerSample
			iVal := float64(raw[rawIdx]) - dcOffset
			qVal := float64(raw[rawIdx+1]) - dcOffset
			window := hannWindow(i, spectrumFFTSize)
			x[i] = complex(iVal*window, qVal*window)
		}

		fftInPlace(x)

		// Accumulate |x|² per bin, applying fftshift in the
		// same step (fft bin i → display bin (i+N/2) mod N).
		for fftBin, value := range x {
			displayBin := (fftBin + halfN) % spectrumFFTSize
			powerSum[displayBin] += real(value)*real(value) + imag(value)*imag(value)
		}
	}

	return Spectrum{
		BinDB:     powerToDB(powerSum, numSegments),
		CentreBin: halfN,
	}
}

// powerToDB converts the accumulated power-per-bin into the
// dB-per-bin slice the renderer expects. Averages over the
// segment count and applies the canonical 10*log10 (power, not
// amplitude — Welch sums squared magnitudes).
func powerToDB(powerSum []float64, numSegments int) []float64 {
	const (
		dbFloor       = -200.0
		dbCoefficient = 10.0 // 10*log10 for power; we already summed |x|², not |x|.
	)

	bins := make([]float64, len(powerSum))
	denom := float64(numSegments)

	for binIdx, summedPower := range powerSum {
		avgPower := summedPower / denom
		if avgPower <= 0 {
			bins[binIdx] = dbFloor

			continue
		}

		bins[binIdx] = dbCoefficient * math.Log10(avgPower)
	}

	return bins
}

// renderSpectrum draws the magnitude-in-dB spectrum as a vertical
// bar chart sized to the pane. Layout matches the histogram:
//
//   - Left Y-axis gutter showing dB labels (relative to the
//     supplied displayTop = top of the chart).
//   - Bottom axis with frequency tick labels in MHz offset from
//     the centre, plus a '▲' carrier-frequency marker.
//   - Bars are colour-coded by where they sit in the displayed
//     dB range: bottom third green (noise floor), middle yellow,
//     top third red (real peaks).
//
// displayTop is supplied by the caller (typically a slow-decay
// scale tracker) so the chart's normalisation doesn't rescale
// violently when individual bin magnitudes flicker. span is fixed
// at spectrumDynamicRangeDB.
//
// Empty spectrum produces a placeholder grid so the layout
// doesn't shift as the first frame arrives.
func renderSpectrum(spec Spectrum, displayTop, baselineDB float64, width, height int) string {
	const axisRows = 2

	if width <= histogramYMarginWidth || height <= axisRows {
		return ""
	}

	chartWidth := width - histogramYMarginWidth
	chartHeight := height - axisRows

	if len(spec.BinDB) == 0 {
		return renderEmptySpectrum(chartWidth, chartHeight, width)
	}

	cols := downsampleSpectrum(spec.BinDB, chartWidth)
	span := spectrumDynamicRangeDB

	const subSteps = 8

	fullScale := uint32(chartHeight * subSteps) //nolint:gosec // chartHeight bounded by terminal size.

	heights := make([]uint32, len(cols))

	for idx, dbVal := range cols {
		normalised := (dbVal - (displayTop - span)) / span

		if normalised < 0 {
			normalised = 0
		}

		if normalised > 1 {
			normalised = 1
		}

		heights[idx] = uint32(normalised * float64(fullScale))
	}

	chart := paintSpectrumColumns(heights, chartHeight, subSteps)

	centreCol := spec.CentreBin * chartWidth / len(spec.BinDB)
	chart = overlayCentreLine(chart, centreCol, chartWidth)

	baselineRow := baselineRowFor(baselineDB, displayTop, chartHeight)
	chart = overlayBaseline(chart, baselineRow, chartWidth)

	chart = prefixSpectrumYAxisLabels(chart, displayTop, span, chartHeight)

	return chart + "\n" + renderSpectrumAxis(width, chartWidth, centreCol)
}

// overlayCentreLine draws a vertical guide ('│') up the chart at
// the tuned-frequency column. The line is drawn through every
// row regardless of whether a bar fills the cell — when the
// carrier sits on the tuned frequency the line cuts through the
// bar; when it doesn't, the line is visible against the noise
// floor and the bar appears offset from it. Either way the
// operator has a fixed visual anchor for "this is where I tuned".
//
// Lines may contain tview colour markup like [red]…[-]; we use
// runeIndexForDisplayCol so the overlay lands on the right
// visual cell instead of a markup character (overwriting inside
// a tag would corrupt the colour state and break the layout).
func overlayCentreLine(chart string, centreCol, chartWidth int) string {
	if centreCol < 0 || centreCol >= chartWidth {
		return chart
	}

	lines := strings.Split(chart, "\n")
	for rowIdx, line := range lines {
		runes := []rune(line)

		idx := runeIndexForDisplayCol(runes, centreCol)
		if idx < 0 {
			continue
		}

		runes[idx] = '│'
		lines[rowIdx] = string(runes)
	}

	return strings.Join(lines, "\n")
}

// runeIndexForDisplayCol scans a rune slice that may contain
// tview colour markup (`[colour]` / `[-]` etc.) and returns the
// rune index of the displayCol-th visible cell. Markup blocks
// occupy zero display cells. Returns -1 if displayCol points
// past the visible end of the line.
func runeIndexForDisplayCol(runes []rune, displayCol int) int {
	displayed := 0
	idx := 0

	for idx < len(runes) {
		if runes[idx] == '[' {
			// Skip the entire `[...]` markup block. tview's
			// chart output never embeds literal '[' in the
			// visible content of the spectrum, so naive
			// bracket-matching is safe here.
			closing := idx + 1
			for closing < len(runes) && runes[closing] != ']' {
				closing++
			}

			idx = closing + 1

			continue
		}

		if displayed == displayCol {
			return idx
		}

		displayed++
		idx++
	}

	return -1
}

// baselineRowFor maps a dB value (the long-term noise floor) into
// the chart row index it should be drawn on. Returns -1 if the
// baseline falls outside the displayed dB range so the overlay
// can skip drawing.
func baselineRowFor(baselineDB, displayTop float64, chartHeight int) int {
	if math.IsInf(baselineDB, 0) || math.IsNaN(baselineDB) {
		return -1
	}

	if chartHeight <= 0 {
		return -1
	}

	// fraction = 0 → bottom of chart, 1 → top.
	fraction := (baselineDB - (displayTop - spectrumDynamicRangeDB)) / spectrumDynamicRangeDB
	if fraction < 0 || fraction > 1 {
		return -1
	}

	// rowFromBottom = chartHeight-1 places the baseline at the
	// top row; 0 at the bottom row. paintSpectrumColumns emits
	// row chartHeight-1 first (top), so the returned index here
	// is "rows from the top".
	rowFromBottom := int(fraction * float64(chartHeight-1))

	return chartHeight - 1 - rowFromBottom
}

// overlayBaseline draws a horizontal dashed line at baselineRow.
// The dashes only fill empty cells; bar cells keep their block
// character so the operator can see bars *crossing* the line.
// baselineRow of -1 leaves the chart untouched.
//
// Lines may contain tview colour markup like [red]…[-]; we walk
// the row with runeIndexForDisplayCol so the overlay lands on
// the right visual cell instead of corrupting a markup block.
func overlayBaseline(chart string, baselineRow, chartWidth int) string {
	if baselineRow < 0 {
		return chart
	}

	lines := strings.Split(chart, "\n")
	if baselineRow >= len(lines) {
		return chart
	}

	runes := []rune(lines[baselineRow])

	for col := range chartWidth {
		idx := runeIndexForDisplayCol(runes, col)
		if idx < 0 {
			break
		}

		// Empty cells get the dash; block / partial-block
		// cells stay intact so a bar that exceeds the
		// baseline remains visible.
		if runes[idx] == ' ' {
			runes[idx] = '╌'
		}
	}

	lines[baselineRow] = string(runes)

	return strings.Join(lines, "\n")
}

// spectrumDynamicRangeDB is the dB span of the chart vertical
// axis. Top of the chart = the caller's displayTop, bottom =
// displayTop - spectrumDynamicRangeDB. 40 dB lines up well with
// the noise-floor → burst-peak spread of a typical RTL-SDR chain.
const spectrumDynamicRangeDB = 40.0

// paintSpectrumColumns draws the bars with row-based "VU meter"
// colouring: bars in the top third of the chart show green
// (signal worth attention), middle third yellow, bottom third
// red (noise floor / nothing of interest). Each cell's colour is
// determined by its row position, not the column's dB value —
// so a tall bar climbs red → yellow → green and the operator
// can read peak strength at a glance from the colour distribution.
//
// Empty cells above a bar stay uncoloured. Colour tags use
// tview's [colour]…[-] markup; the entire bar portion of a row
// shares a single open/close tag pair.
func paintSpectrumColumns(heights []uint32, height, subSteps int) string {
	var builder strings.Builder

	subStepsU := uint32(subSteps) //nolint:gosec // subSteps is the constant 8.

	for row := height - 1; row >= 0; row-- {
		rowFloorU := uint32(row) * subStepsU //nolint:gosec // row >= 0.
		rowCeilU := rowFloorU + subStepsU
		rowColor := colorForSpectrumRow(row, height)

		writeRowBarsInColor(&builder, heights, rowColor, rowFloorU, rowCeilU)

		if row > 0 {
			builder.WriteByte('\n')
		}
	}

	return builder.String()
}

// colorForSpectrumRow returns the tview colour for the given row
// index. Row 0 is the bottom of the chart; row height-1 is the
// top. Tier boundaries are at 1/3 and 2/3 of the chart height.
func colorForSpectrumRow(row, height int) string {
	if height <= 1 {
		return colorGreen
	}

	const (
		yellowMinFraction = 1.0 / 3.0
		greenMinFraction  = 2.0 / 3.0
	)

	fraction := float64(row) / float64(height-1)

	switch {
	case fraction < yellowMinFraction:
		return colorRed
	case fraction < greenMinFraction:
		return colorYellow
	default:
		return colorGreen
	}
}

// writeRowBarsInColor emits one row's cells: bar cells get the
// row's colour; empty cells stay uncoloured. Run-length encoded:
// the bar portions of the row share one open/close tag pair.
func writeRowBarsInColor(
	builder *strings.Builder,
	heights []uint32,
	rowColor string,
	rowFloorU, rowCeilU uint32,
) {
	currentColor := ""

	for _, colHeight := range heights {
		char := cellRuneForHeight(colHeight, rowFloorU, rowCeilU)

		wantColor := ""
		if char != ' ' {
			wantColor = rowColor
		}

		currentColor = switchColor(builder, currentColor, wantColor)
		builder.WriteRune(char)
	}

	if currentColor != "" {
		builder.WriteString("[-]")
	}
}

// switchColor emits the tview tags needed to transition the
// current open colour to the desired one (closing the previous,
// opening the next). Returns the new current colour. Empty
// string means "no open colour".
func switchColor(builder *strings.Builder, current, desired string) string {
	if desired == current {
		return current
	}

	if current != "" {
		builder.WriteString("[-]")
	}

	if desired != "" {
		builder.WriteByte('[')
		builder.WriteString(desired)
		builder.WriteByte(']')
	}

	return desired
}

// cellRuneForHeight is the rune-picker for one cell of a column.
// Empty rows above the bar return space; rows fully inside the
// bar return a full block; the partial row at the top of the bar
// returns the eighth-of-a-row block matching the bar height.
func cellRuneForHeight(colHeight, rowFloorU, rowCeilU uint32) rune {
	switch {
	case colHeight <= rowFloorU:
		return ' '
	case colHeight >= rowCeilU:
		return '█'
	default:
		return histogramBlocks[colHeight-rowFloorU]
	}
}

// spectrumFloorPercentile is the percentile of bins used as the
// frame's noise-floor estimate. The 25th percentile is robust
// to a few real peaks dragging the value up — even a wideband
// signal covering a quarter of the band leaves it unaffected.
const spectrumFloorPercentile = 0.25

// estimateFloorDB returns the spectrumFloorPercentile percentile
// of the bin slice. Used as input to the long-term baseline
// tracker. Returns -∞ on empty input — same convention as
// spectrumPeakDB.
func estimateFloorDB(cols []float64) float64 {
	if len(cols) == 0 {
		return math.Inf(-1)
	}

	sorted := make([]float64, len(cols))
	copy(sorted, cols)
	slices.Sort(sorted)

	idx := int(float64(len(sorted)) * spectrumFloorPercentile)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}

	return sorted[idx]
}

// spectrumBaselineTracker holds the long-time-constant EMA of
// the per-frame noise-floor estimate. The chart overlays this as
// a horizontal line so the operator can read "anything sticking
// out above the line is a signal; everything at the line is the
// chain's natural floor".
type spectrumBaselineTracker struct {
	baseline    float64
	initialised bool
}

// update folds the latest floor estimate into the tracker and
// returns the new baseline. tauSeconds is the EMA time constant
// (~30 s — much slower than spectrumScaleTracker, since we want
// the baseline to track the chain's *quiescent* state, not
// transients).
func (t *spectrumBaselineTracker) update(currentFloor, dtSeconds float64) float64 {
	const tauSeconds = 30.0

	if math.IsInf(currentFloor, -1) {
		return t.baseline
	}

	if !t.initialised {
		t.baseline = currentFloor
		t.initialised = true

		return t.baseline
	}

	if dtSeconds <= 0 {
		return t.baseline
	}

	alpha := dtSeconds / (tauSeconds + dtSeconds)
	t.baseline = alpha*currentFloor + (1-alpha)*t.baseline

	return t.baseline
}

// renderSpectrumTitle is the pane title — kept symmetric with
// renderHistogramTitle so both panes share visual style.
func renderSpectrumTitle(sampleRateHz uint32) string {
	mhz := float64(sampleRateHz) / hzPerMHz

	return fmt.Sprintf(" spectrum dB ±%.2f MHz ", mhz*spectrumCentreFraction)
}

// renderEmptySpectrum is the blank-frame placeholder.
func renderEmptySpectrum(chartWidth, chartHeight, width int) string {
	var builder strings.Builder

	emptyRow := strings.Repeat(" ", histogramYMarginWidth+chartWidth)

	for row := range chartHeight {
		builder.WriteString(emptyRow)

		if row < chartHeight-1 {
			builder.WriteByte('\n')
		}
	}

	builder.WriteByte('\n')

	const halfChart = 2

	builder.WriteString(renderSpectrumAxis(width, chartWidth, chartWidth/halfChart))

	return builder.String()
}

// downsampleSpectrum reduces BinDB to exactly width columns by
// max-pooling within each block. Max is the right summary for a
// frequency-domain magnitude display — averaging would smear
// narrow peaks like a carrier into the noise.
func downsampleSpectrum(bins []float64, width int) []float64 {
	out := make([]float64, width)

	if width <= 0 {
		return out
	}

	for col := range width {
		start := col * len(bins) / width
		end := (col + 1) * len(bins) / width

		if end <= start {
			end = start + 1
		}

		if end > len(bins) {
			end = len(bins)
		}

		peak := bins[start]
		for _, dbVal := range bins[start+1 : end] {
			if dbVal > peak {
				peak = dbVal
			}
		}

		out[col] = peak
	}

	return out
}

// spectrumPeakDB returns the largest dB value across the bin
// slice. Callers use this as the input to spectrumScaleTracker
// so the chart's displayTop tracks signal peaks with a
// slow-decay rule instead of resetting every frame. Returns
// math.Inf(-1) when bins is empty so the caller can detect "no
// data yet" without a separate ok flag.
func spectrumPeakDB(bins []float64) float64 {
	if len(bins) == 0 {
		return math.Inf(-1)
	}

	peak := bins[0]

	for _, dbVal := range bins[1:] {
		if dbVal > peak {
			peak = dbVal
		}
	}

	return peak
}

// spectrumScaleTracker holds the slow-decay display-top value the
// spectrum chart normalises against. New peaks push the top up
// immediately so emerging signals are instantly visible; absent
// new peaks the top decays back down at a fixed dB/second rate so
// the chart isn't constantly rescaling on small fluctuations.
type spectrumScaleTracker struct {
	top         float64
	initialised bool
}

// update folds the latest peak dB into the running display top
// and returns the new top. dtSeconds is the wall-clock interval
// since the previous update — used to scale the decay so the
// behaviour is independent of redraw rate.
func (s *spectrumScaleTracker) update(currentPeak, dtSeconds float64) float64 {
	// Decay rate in dB/second when the current peak is below
	// the running top. 5 dB/s means an 8-second fade for a
	// 40 dB transient — enough to keep the operator's eye
	// pinned to the peak shape, short enough that the chart
	// follows real gain changes.
	const decayDBPerSecond = 5.0

	if math.IsInf(currentPeak, -1) {
		// "No data" — leave the top alone.
		return s.top
	}

	if !s.initialised {
		s.top = currentPeak
		s.initialised = true

		return s.top
	}

	if currentPeak > s.top {
		s.top = currentPeak

		return s.top
	}

	next := s.top - decayDBPerSecond*dtSeconds
	if next < currentPeak {
		next = currentPeak
	}

	s.top = next

	return s.top
}

// prefixSpectrumYAxisLabels prefixes each chart row with a Y-axis
// dB label. Top row shows the peak dB; descending rows step down
// by dbRange/chartHeight.
func prefixSpectrumYAxisLabels(chart string, dbMax, dbRange float64, chartHeight int) string {
	lines := strings.Split(chart, "\n")

	for rowIdx := range lines {
		// Top of row 0 represents dbMax; bottom of last row
		// represents dbMax - dbRange.
		fraction := float64(rowIdx) / float64(chartHeight)
		dbAtRowTop := dbMax - fraction*dbRange
		lines[rowIdx] = formatSpectrumYLabel(dbAtRowTop) + lines[rowIdx]
	}

	return strings.Join(lines, "\n")
}

// formatSpectrumYLabel builds the 6-rune Y-axis label, matching
// histogramYMarginWidth so the chart rows align with the bottom
// axis decoration. The "dB" unit lives in the pane title, not
// every label — otherwise the prefix is 7 runes wide and tview
// wraps every chart line, breaking the layout.
func formatSpectrumYLabel(db float64) string {
	return fmt.Sprintf("%4d ┤", int(math.Round(db)))
}

// renderSpectrumAxis builds the two-row bottom axis: a tick line
// and a frequency-offset labels row in MHz from the centre
// frequency. Centre column is 0 MHz; left edge is -Fs/2, right
// edge is +Fs/2. Sample rate isn't known here so we use the
// SDR-canonical 2.4 MS/s for label arithmetic — operators
// running at a different rate can mentally scale or change the
// constant if needed.
//
// The unit caption goes at the right edge if there's room. The
// centreCol parameter marks the carrier-frequency column with a
// '▲' on the tick row so the operator can compare the position of
// the spectrum peak against the carrier without a vertical line
// cutting through the bars.
func renderSpectrumAxis(totalWidth, chartWidth, centreCol int) string {
	const spectrumTickStride = 16

	ticks := make([]rune, totalWidth)
	labels := make([]rune, totalWidth)

	for idx := range totalWidth {
		ticks[idx] = ' '
		labels[idx] = ' '
	}

	writeSpectrumYCorner(ticks, totalWidth)
	writeSpectrumXTicks(ticks, labels, totalWidth, chartWidth, spectrumTickStride)
	writeSpectrumCentreMarker(ticks, totalWidth, centreCol)
	writeSpectrumCaption(labels, totalWidth)

	return string(ticks) + "\n" + string(labels)
}

// writeSpectrumCentreMarker overlays a '▲' on the tick row at the
// chart's centre column. Operator reads "is the peak above the
// arrow?" — yes means tuned, offset means PPM is off or the
// signal is off-carrier.
func writeSpectrumCentreMarker(ticks []rune, totalWidth, centreCol int) {
	pos := histogramYMarginWidth + centreCol
	if pos >= 0 && pos < totalWidth {
		ticks[pos] = '▲'
	}
}

// writeSpectrumYCorner paints the "0dB └" Y-axis terminator into
// the gutter portion of the tick row.
func writeSpectrumYCorner(ticks []rune, totalWidth int) {
	corner := "  -∞ └"
	writeRunesAt(ticks, corner, 0, totalWidth)
}

// writeSpectrumXTicks fills the chart-area portion of the tick
// row with ─ between ticks and ┬ at each stride boundary, plus
// the MHz-offset tick label below each ┬.
func writeSpectrumXTicks(ticks, labels []rune, totalWidth, chartWidth, stride int) {
	for idx := histogramYMarginWidth; idx < totalWidth; idx++ {
		ticks[idx] = '─'
	}

	// Tick label is the frequency offset in MHz from centre.
	// Assume sample rate 2.4 MS/s — most SDR setups use this
	// default; spectrumDisplaySampleRateMHz is the canonical
	// constant. A different rate will mislabel the ticks but
	// not affect the shape.
	const spectrumDisplaySampleRateMHz = 2.4

	for col := 0; col < chartWidth; col += stride {
		ticks[histogramYMarginWidth+col] = '┬'

		fraction := 0.0
		if chartWidth > 1 {
			fraction = float64(col)/float64(chartWidth-1) - spectrumCentreFraction
		}

		mhz := fraction * spectrumDisplaySampleRateMHz
		labelText := strconv.FormatFloat(mhz, 'f', 1, 64)

		writeRunesAt(labels, labelText, histogramYMarginWidth+col, totalWidth)
	}
}

// writeSpectrumCaption appends the unit caption to the labels row
// when there's room.
func writeSpectrumCaption(labels []rune, totalWidth int) {
	caption := " MHz "

	const captionMinWidth = 16
	if totalWidth < captionMinWidth {
		return
	}

	writeRunesAt(labels, caption, totalWidth-len(caption), totalWidth)
}
