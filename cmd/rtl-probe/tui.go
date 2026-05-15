package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/hyperized/rtl2832u"
	"github.com/rivo/tview"
)

// tuiSampleTarget is the per-probe I/Q sample target the TUI's
// sampling goroutine asks ReadSampleStats for. 128 KiB samples ≈
// 54 ms at 2.4 MS/s — enough for the histogram and saturation
// reading to stabilise across burst variance while keeping the
// goroutine responsive to ctx cancellation.
const tuiSampleTarget = 128 * 1024

// tuiRefreshInterval is the lower bound between successive probes.
// ReadSampleStats already blocks for ~54 ms per call at the
// default sample rate, so this acts as a floor rather than a
// metronome; the actual frame rate is ~5–18 Hz depending on USB
// jitter.
const tuiRefreshInterval = 50 * time.Millisecond

// tuiHistoryWindow bounds the strip-chart ring buffer. At a ~10 Hz
// effective probe rate this holds ~30 s of metric history.
const tuiHistoryWindow = 300

// tuiHealthAverageWindow is how many recent SampleStats samples we
// average before grading. The advice and status banners read the
// smoothed values so they don't flip every frame as per-probe
// SaturationFrac swings on bursty traffic. At ~5 Hz this is ~4 s
// of history — long enough to dampen flicker, short enough that
// the operator sees the result of a gain change quickly.
const tuiHealthAverageWindow = 20

// spectrumEMAAlpha is the blending weight for the latest FFT
// frame in the per-bin exponential moving average. computeSpectrum
// already does heavy intra-frame smoothing via Welch averaging
// (~256 short FFTs per frame), so we only need light inter-frame
// EMA to dampen residual flicker. 0.5 gives a ~3-frame (~600 ms
// at 5 Hz) time constant — responsive to tuning changes, still
// quiets the final display.
const spectrumEMAAlpha = 0.5

// rawSampler is the slice of *rtl2832u.Receiver the TUI sampler
// needs. We use Read directly (not ReadSampleStats) so the
// sampler can derive both SampleStats *and* the spectrum FFT from
// the same buffer without competing reads on the bulk endpoint.
type rawSampler interface {
	Read(ctx context.Context, p []byte) (int, error)
}

// tuiModel holds the latest sample-stats reading plus a ring
// buffer of recent ones for the strip chart. Accessed from both
// the sampling goroutine (writer) and the UI thread (reader), so
// every field is guarded by mu.
type tuiModel struct {
	mu             sync.RWMutex
	latest         rtl2832u.SampleStats
	latestSpectrum Spectrum
	history        []rtl2832u.SampleStats // ring buffer, len ≤ tuiHistoryWindow
	frames         uint64
	lastErr        error
}

// tuiSnapshot is the value-copy of tuiModel state taken under a
// single read lock. Grouped into a struct so the rendering path
// reads one field at a time without holding the model lock.
type tuiSnapshot struct {
	latest         rtl2832u.SampleStats
	latestSpectrum Spectrum
	history        []rtl2832u.SampleStats
	frames         uint64
	lastErr        error
}

// update is called by the sampler goroutine each time a fresh
// SampleStats + Spectrum is available. Pushes the stats value
// onto the ring buffer, evicting the oldest entry when the
// window is full; the spectrum is single-valued (latest only).
func (m *tuiModel) update(stats rtl2832u.SampleStats, spec Spectrum) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.latest = stats
	m.latestSpectrum = spec
	m.frames++

	if len(m.history) == tuiHistoryWindow {
		m.history = append(m.history[:0], m.history[1:]...)
	}

	m.history = append(m.history, stats)
}

// recordError is called by the sampler goroutine if ReadSampleStats
// returns an error. The UI thread surfaces it in the footer.
func (m *tuiModel) recordError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastErr = err
}

// snapshot returns a value copy of the model state under a single
// read lock. Avoids fine-grained locking on the UI render path —
// one lock, copy out, render outside the lock.
func (m *tuiModel) snapshot() tuiSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	history := make([]rtl2832u.SampleStats, len(m.history))
	copy(history, m.history)

	// Spectrum's BinDB is a slice we have to copy out so the
	// renderer reads a stable view while the sampler writes the
	// next frame.
	bins := make([]float64, len(m.latestSpectrum.BinDB))
	copy(bins, m.latestSpectrum.BinDB)

	return tuiSnapshot{
		latest:         m.latest,
		latestSpectrum: Spectrum{BinDB: bins, CentreBin: m.latestSpectrum.CentreBin},
		history:        history,
		frames:         m.frames,
		lastErr:        m.lastErr,
	}
}

// runTUI opens the device with the configured options, launches a
// sampling goroutine, and renders a live histogram + strip-chart +
// spectrum view until the user quits or ctx is cancelled. Returns
// exitOK on clean shutdown or exitProbeFailed if the tview app
// fails to start.
func runTUI(ctx context.Context, rcv rawSampler, sampleRateHz uint32, stderr io.Writer) int {
	model := &tuiModel{
		history: make([]rtl2832u.SampleStats, 0, tuiHistoryWindow),
	}

	app := tview.NewApplication()

	header := tview.NewTextView().SetDynamicColors(true)
	status := tview.NewTextView().SetDynamicColors(true)
	adviceLine := tview.NewTextView().SetDynamicColors(true)
	histogram := tview.NewTextView().SetDynamicColors(true)
	histogram.SetBorder(true).SetTitle(" magnitude histogram ")

	strip := tview.NewTextView().SetDynamicColors(true)
	strip.SetBorder(true).SetTitle(" strip chart (last ~30 s) ")

	spectrum := tview.NewTextView().SetDynamicColors(true)
	spectrum.SetBorder(true).SetTitle(renderSpectrumTitle(sampleRateHz))

	footer := tview.NewTextView().SetDynamicColors(true)

	rightColumn := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(strip, 0, 1, false).
		AddItem(spectrum, 0, 2, false)

	body := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(histogram, 0, 1, false).
		AddItem(rightColumn, 0, 1, false)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 1, 0, false).
		AddItem(status, 1, 0, false).
		AddItem(adviceLine, 1, 0, false).
		AddItem(body, 0, 1, false).
		AddItem(footer, 1, 0, false)

	app.SetRoot(root, true).SetInputCapture(tuiInputCapture(app))

	samplerCtx, cancelSampler := context.WithCancel(ctx)
	defer cancelSampler()

	go runSampler(samplerCtx, rcv, model)
	go runRedraw(samplerCtx, app, model, redrawPanes{
		header:    header,
		status:    status,
		advice:    adviceLine,
		histogram: histogram,
		strip:     strip,
		spectrum:  spectrum,
		footer:    footer,
	})

	if err := app.Run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "rtl-probe: tui: %v\n", err)

		return exitProbeFailed
	}

	return exitOK
}

// tuiInputCapture returns the keypress handler: q or Ctrl-C stops
// the app, every other key passes through to the focused primitive
// (currently none — the TUI is read-only).
func tuiInputCapture(app *tview.Application) func(*tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC || event.Key() == tcell.KeyEsc {
			app.Stop()

			return nil
		}

		if event.Rune() == 'q' || event.Rune() == 'Q' {
			app.Stop()

			return nil
		}

		return event
	}
}

// runSampler is the goroutine that pulls raw I/Q chunks from the
// device, derives both SampleStats and the spectrum FFT from each
// frame, applies temporal smoothing to the FFT, and publishes
// the result to the model. Errors land in model.lastErr; the
// loop keeps trying so a transient USB hiccup doesn't tear the
// TUI down.
//
// One Read call per chunk: 16 KiB at 2.4 MS/s is ~3.4 ms of
// samples — plenty for spectrumFFTSize (512 samples = 1 KiB) and
// far more than required by SampleStats. We accumulate a few
// chunks into a buffer so the SampleStats reading covers
// tuiSampleTarget samples, then feed the head of that buffer to
// the FFT.
func runSampler(ctx context.Context, rcv rawSampler, model *tuiModel) {
	const samplerReadChunk = 16 * 1024

	ticker := time.NewTicker(tuiRefreshInterval)
	defer ticker.Stop()

	const bytesPerSample = 2

	frameBuf := make([]byte, 0, tuiSampleTarget*bytesPerSample)
	scratch := make([]byte, samplerReadChunk)

	// smoothed holds the running per-bin EMA across FFT frames.
	// Nil until the first frame arrives.
	var smoothed Spectrum

	for {
		frameBuf = frameBuf[:0]

		if err := readFrame(ctx, rcv, scratch, &frameBuf, tuiSampleTarget*bytesPerSample); err != nil {
			if ctx.Err() != nil {
				return
			}

			model.recordError(err)
		} else {
			stats := rtl2832u.ComputeSampleStats(frameBuf)
			fresh := computeSpectrum(frameBuf)
			smoothed = blendSpectrum(smoothed, fresh, spectrumEMAAlpha)
			model.update(stats, smoothed)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// blendSpectrum applies an exponential moving average to the
// per-bin dB values, biasing toward the running history with
// weight (1 - alpha) and toward the new frame with weight alpha.
// Empty inputs short-circuit to the other side; mismatched lengths
// (e.g., first frame of a re-tuned receiver) reset the smoothed
// state to the new frame.
func blendSpectrum(prev, fresh Spectrum, alpha float64) Spectrum {
	if len(fresh.BinDB) == 0 {
		return prev
	}

	if len(prev.BinDB) != len(fresh.BinDB) {
		bins := make([]float64, len(fresh.BinDB))
		copy(bins, fresh.BinDB)

		return Spectrum{BinDB: bins, CentreBin: fresh.CentreBin}
	}

	bins := make([]float64, len(prev.BinDB))
	for i, prevValue := range prev.BinDB {
		bins[i] = alpha*fresh.BinDB[i] + (1-alpha)*prevValue
	}

	return Spectrum{BinDB: bins, CentreBin: fresh.CentreBin}
}

// readFrame reads chunks from the bulk endpoint into frameBuf
// until it holds at least targetBytes. Pulled out of runSampler
// so the loop stays inside revive's complexity budget.
func readFrame(
	ctx context.Context,
	rcv rawSampler,
	scratch []byte,
	frameBuf *[]byte,
	targetBytes int,
) error {
	for len(*frameBuf) < targetBytes {
		count, err := rcv.Read(ctx, scratch)
		if err != nil {
			return err //nolint:wrapcheck // sampler-internal; caller decides ctx-cancellation policy.
		}

		*frameBuf = append(*frameBuf, scratch[:count]...)
	}

	return nil
}

// runRedraw is the goroutine that asks tview to redraw the UI at
// the sampler's rate. We funnel the redraw through QueueUpdateDraw
// so tview owns all writes to its primitives — concurrent direct
// SetText calls from multiple goroutines would race tview's
// internal state.
// redrawPanes groups the tview text views runRedraw mutates so
// the function's signature stays inside revive's parameter-count
// limit when new panes (advice row, spectrum) are added.
type redrawPanes struct {
	header    *tview.TextView
	status    *tview.TextView
	advice    *tview.TextView
	histogram *tview.TextView
	strip     *tview.TextView
	spectrum  *tview.TextView
	footer    *tview.TextView
}

func runRedraw(
	ctx context.Context,
	app *tview.Application,
	model *tuiModel,
	panes redrawPanes,
) {
	ticker := time.NewTicker(tuiRefreshInterval)
	defer ticker.Stop()

	var (
		scale    spectrumScaleTracker
		baseline spectrumBaselineTracker
		lastTick = time.Now()
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()
		dtSeconds := now.Sub(lastTick).Seconds()
		lastTick = now

		snap := model.snapshot()
		displayTop := scale.update(spectrumPeakDB(snap.latestSpectrum.BinDB), dtSeconds)
		baselineDB := baseline.update(estimateFloorDB(snap.latestSpectrum.BinDB), dtSeconds)

		app.QueueUpdateDraw(func() {
			_, _, histW, histH := panes.histogram.GetInnerRect()
			_, _, stripW, stripH := panes.strip.GetInnerRect()
			_, _, specW, specH := panes.spectrum.GetInnerRect()

			// Status / advice read from the smoothed average so
			// they don't flip every frame on bursty traffic;
			// header / histogram / strip / spectrum stay on the
			// raw latest so the operator can see real-time
			// fluctuation.
			smoothed := averageStats(snap.history, tuiHealthAverageWindow)

			panes.header.SetText(renderHeader(snap.latest, snap.frames))
			panes.status.SetText(renderStatusBanner(smoothed))
			panes.advice.SetText(renderAdviceBanner(smoothed))
			panes.histogram.SetTitle(renderHistogramTitle(snap.latest.MagnitudeHistogram))
			panes.histogram.SetText(renderHistogram(snap.latest.MagnitudeHistogram, histW, histH))
			panes.strip.SetText(renderStripChart(snap.history, stripW, stripH))
			panes.spectrum.SetText(renderSpectrum(snap.latestSpectrum, displayTop, baselineDB, specW, specH))
			panes.footer.SetText(renderFooter(snap.lastErr))
		})
	}
}

// renderHeader formats the latest sample-stats numbers and the
// running frame count into a single line.
func renderHeader(stats rtl2832u.SampleStats, frames uint64) string {
	const percent = 100.0

	return fmt.Sprintf(
		"[::b]rtl-probe[::-] frame=%d samples=%d rms=%.1f peak=%.1f sat=%.2f%% dc=%+.1f,%+.1f",
		frames,
		stats.Samples,
		stats.RMS,
		stats.Peak,
		stats.SaturationFrac*percent,
		stats.DCI,
		stats.DCQ,
	)
}

// renderFooter draws the keybindings line and any sampler error.
// Errors are coloured red so a hung USB shows up at a glance.
func renderFooter(lastErr error) string {
	if lastErr != nil {
		return fmt.Sprintf("[red]sampler error: %v[-] · [::b]q[::-] quit", lastErr)
	}

	return "[::b]q[::-] quit · [::b]esc[::-] quit · live histogram + strip chart at ~5 Hz"
}

// histogramBlocks is the eight-level vertical block ramp used to
// draw partial bar heights. histogramBlocks[0] is empty;
// histogramBlocks[8] is a full block.
//
//nolint:gochecknoglobals // rune array literal can't be a const; used as a lookup table.
var histogramBlocks = [9]rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// histogramYMarginWidth is the fixed-width left gutter reserved
// for Y-axis labels of the form "NNN% ┤". Six characters: three
// digit columns, a '%', a space, and the tick character.
const histogramYMarginWidth = 6

// histogramAxisRows is how many rows the axis decoration consumes
// below the chart: one tick row carrying the └─┬─┬─… line plus
// the 0% Y label, and one magnitude-labels row.
const histogramAxisRows = 2

// renderHistogram converts a SampleStats.MagnitudeHistogram into a
// width×height block-character rendering with axis decoration:
//
//   - Left gutter: percentage Y-axis labels at each row boundary,
//     so the operator can read a bar's height as a fraction of
//     the maximum bucket count.
//   - Bottom: a tick line (└─┬─…) plus magnitude-tick labels and
//     a unit caption identifying what the X-axis values represent
//     (8-bit |I+jQ| sample magnitude after 128-offset removal).
//
// The maximum bucket count appears in the pane title (set by the
// caller via renderHistogramTitle) so the percentages on the left
// have a numeric anchor.
func renderHistogram(hist [rtl2832u.HistogramBuckets]uint32, width, height int) string {
	if width <= histogramYMarginWidth || height <= histogramAxisRows {
		return ""
	}

	chartWidth := width - histogramYMarginWidth
	chartHeight := height - histogramAxisRows

	cols := mapHistogramToColumns(hist, chartWidth)
	maxCount := uint32(0)

	for _, count := range cols {
		if count > maxCount {
			maxCount = count
		}
	}

	if maxCount == 0 {
		return renderEmptyHistogram(chartWidth, chartHeight)
	}

	const subSteps = 8

	fullScale := uint32(chartHeight * subSteps) //nolint:gosec // chartHeight bounded by terminal size.

	heights := make([]uint32, len(cols))
	for i, count := range cols {
		heights[i] = (count * fullScale) / maxCount
	}

	chart := paintHistogramColumns(heights, chartWidth, chartHeight, subSteps)
	chart = prefixYAxisLabels(chart, chartHeight)

	return chart + "\n" + renderHistogramAxis(width, chartWidth)
}

// paintHistogramColumns is paintColumns + per-column colour
// coding based on each column's magnitude position on the X
// axis. Both edges (far left = under-gain, far right = clipping)
// are red; the healthy mid-range is green; transition bands are
// yellow. The colour assignment is purely positional (no
// per-frame heuristics), so the visual cue is stable.
func paintHistogramColumns(heights []uint32, width, height, subSteps int) string {
	var builder strings.Builder

	colColors := make([]string, width)
	for col := range width {
		colColors[col] = colorForHistogramColumn(col, width)
	}

	subStepsU := uint32(subSteps) //nolint:gosec // subSteps is the constant 8.

	for row := height - 1; row >= 0; row-- {
		rowFloorU := uint32(row) * subStepsU //nolint:gosec // row >= 0.
		rowCeilU := rowFloorU + subStepsU

		writeHistogramBarsInColor(&builder, heights, colColors, rowFloorU, rowCeilU)

		if row > 0 {
			builder.WriteByte('\n')
		}
	}

	return builder.String()
}

// writeHistogramBarsInColor emits one row's cells with per-column
// colour. Cells inside a bar take the column's tier colour;
// empty cells stay uncoloured. Run-length encoded: a run of
// same-colour columns shares one open/close pair.
func writeHistogramBarsInColor(
	builder *strings.Builder,
	heights []uint32,
	colColors []string,
	rowFloorU, rowCeilU uint32,
) {
	currentColor := ""

	for colIdx, colHeight := range heights {
		char := cellRuneForHeight(colHeight, rowFloorU, rowCeilU)

		wantColor := ""
		if char != ' ' {
			wantColor = colColors[colIdx]
		}

		currentColor = switchColor(builder, currentColor, wantColor)
		builder.WriteRune(char)
	}

	if currentColor != "" {
		builder.WriteString("[-]")
	}
}

// colorForHistogramColumn returns the tier colour for a chart
// column based on its magnitude midpoint. Hard cut-offs are
// preferable to thresholds here — the operator wants to read
// "what gain regime are my samples in" at a glance, and the
// boundaries don't shift with chain conditions.
func colorForHistogramColumn(col, totalCols int) string {
	if totalCols <= 0 {
		return ""
	}

	// Magnitude at the middle of the column.
	const halfStep = 0.5

	maxMag := rtl2832u.MaxSampleMagnitude
	mag := (float64(col) + halfStep) * maxMag / float64(totalCols)

	const (
		undergainedMaxMag = 12.0
		marginalLowMaxMag = 25.0
		healthyMaxMag     = 130.0
		hotBurstsMaxMag   = 160.0
	)

	switch {
	case mag < undergainedMaxMag:
		return colorRed
	case mag < marginalLowMaxMag:
		return colorYellow
	case mag < healthyMaxMag:
		return colorGreen
	case mag < hotBurstsMaxMag:
		return colorYellow
	default:
		return colorRed
	}
}

// renderHistogramTitle builds the pane title that holds the max-
// count anchor for the percentage Y-axis. Empty input renders a
// placeholder so the title isn't blank before the first sample.
func renderHistogramTitle(hist [rtl2832u.HistogramBuckets]uint32) string {
	maxCount := uint32(0)

	for _, count := range hist {
		if count > maxCount {
			maxCount = count
		}
	}

	if maxCount == 0 {
		return " magnitude histogram "
	}

	return fmt.Sprintf(" magnitude histogram (max=%d) ", maxCount)
}

// renderEmptyHistogram is what we draw when no samples have
// landed: a blank chart of the right shape plus the axis, so the
// layout doesn't shift as the first frame arrives.
func renderEmptyHistogram(chartWidth, chartHeight int) string {
	var builder strings.Builder

	emptyRow := strings.Repeat(" ", histogramYMarginWidth+chartWidth)

	for row := range chartHeight {
		builder.WriteString(emptyRow)

		if row < chartHeight-1 {
			builder.WriteByte('\n')
		}
	}

	builder.WriteByte('\n')
	builder.WriteString(renderHistogramAxis(histogramYMarginWidth+chartWidth, chartWidth))

	return builder.String()
}

// prefixYAxisLabels inserts a left-gutter label on each chart row.
// Labels are percentages of max; the top row reads 100%, the
// bottom row reads the row floor.
func prefixYAxisLabels(chart string, chartHeight int) string {
	lines := strings.Split(chart, "\n")

	const percentScale = 100

	for rowIdx := range lines {
		// rowIdx 0 = top → 100%; rowIdx chartHeight-1 = bottom
		// → step above 0%. Each row covers (1/chartHeight) of
		// the range; label the row by its *upper* edge so the
		// top row reads 100%.
		pct := percentScale - (rowIdx * percentScale / chartHeight)

		lines[rowIdx] = formatYLabel(pct) + lines[rowIdx]
	}

	return strings.Join(lines, "\n")
}

// formatYLabel produces the 6-char fixed-width Y-axis label for a
// percentage. Right-aligned digits, '%', space, '┤' tick.
func formatYLabel(pct int) string {
	return fmt.Sprintf("%3d%% ┤", pct)
}

// renderHistogramAxis builds the two-row bottom axis: a tick line
// (`  0% └─┬───┬─…`) and a magnitude-tick label row that includes
// the unit caption "(|I+jQ|)" if there's room at the right.
func renderHistogramAxis(totalWidth, chartWidth int) string {
	ticks := make([]rune, totalWidth)
	labels := make([]rune, totalWidth)

	for idx := range totalWidth {
		ticks[idx] = ' '
		labels[idx] = ' '
	}

	writeYAxisCorner(ticks, totalWidth)
	writeXAxisTicks(ticks, labels, totalWidth, chartWidth)
	writeXAxisRightEdge(labels, totalWidth)

	return string(ticks) + "\n" + string(labels)
}

// writeYAxisCorner paints the "  0% └" Y-axis terminator into the
// gutter portion of the tick row.
func writeYAxisCorner(ticks []rune, totalWidth int) {
	zeroLabel := "  0% └"

	for offset, char := range zeroLabel {
		if offset < totalWidth {
			ticks[offset] = char
		}
	}
}

// writeXAxisTicks fills the chart-area portion of the tick row
// with ─ between ticks and ┬ at each stride boundary, plus the
// magnitude tick label below each ┬.
func writeXAxisTicks(ticks, labels []rune, totalWidth, chartWidth int) {
	const histogramTickStride = 16

	for idx := histogramYMarginWidth; idx < totalWidth; idx++ {
		ticks[idx] = '─'
	}

	maxMag := rtl2832u.MaxSampleMagnitude

	for col := 0; col < chartWidth; col += histogramTickStride {
		ticks[histogramYMarginWidth+col] = '┬'

		fraction := 0.0
		if chartWidth > 1 {
			fraction = float64(col) / float64(chartWidth-1)
		}

		writeRunesAt(labels, strconv.Itoa(int(fraction*maxMag)), histogramYMarginWidth+col, totalWidth)
	}
}

// writeXAxisRightEdge places the rightmost magnitude reference
// (~181) and the unit caption at the far right of the labels row,
// if width allows.
func writeXAxisRightEdge(labels []rune, totalWidth int) {
	maxMag := rtl2832u.MaxSampleMagnitude
	rightLabel := strconv.Itoa(int(maxMag))
	caption := " |I+jQ|"

	rightEnd := totalWidth
	if rightEnd-len(caption) >= histogramYMarginWidth+len(rightLabel) {
		writeRunesAt(labels, caption, rightEnd-len(caption), totalWidth)

		rightEnd -= len(caption)
	}

	if rightEnd-len(rightLabel) > histogramYMarginWidth {
		writeRunesAt(labels, rightLabel, rightEnd-len(rightLabel), totalWidth)
	}
}

// writeRunesAt copies the runes of s into the dst rune buffer
// starting at start, stopping at totalWidth or end of string.
func writeRunesAt(dst []rune, s string, start, totalWidth int) {
	for offset, char := range s {
		pos := start + offset
		if pos >= totalWidth {
			return
		}

		dst[pos] = char
	}
}

// mapHistogramToColumns resamples the fixed HistogramBuckets bin
// array to exactly `width` columns. When width >=
// HistogramBuckets the buckets are stretched (each bucket fills
// width/HistogramBuckets columns); when width < HistogramBuckets
// the buckets are summed into the destination column.
func mapHistogramToColumns(hist [rtl2832u.HistogramBuckets]uint32, width int) []uint32 {
	cols := make([]uint32, width)

	for srcIdx, count := range hist {
		// dstIdx = floor(srcIdx * width / HistogramBuckets);
		// integer math avoids float drift on small widths.
		dstIdx := srcIdx * width / rtl2832u.HistogramBuckets
		if dstIdx >= width {
			dstIdx = width - 1
		}

		cols[dstIdx] += count
	}

	return cols
}

// health is the per-metric judgment used to colour the value
// column and to roll up an overall chain-health status.
type health int

const (
	healthInfo health = iota // ungraded; rendered neutrally
	healthGood
	healthMarginal
	healthBad
)

// Health / spectrum colour names. tview accepts named colours via
// [name]…[-] markup; centralising them avoids the goconst lint
// firing on the per-tier rendering paths and keeps the palette
// consistent across the TUI.
const (
	colorGreen  = "green"
	colorYellow = "yellow"
	colorRed    = "red"
)

// healthColor returns the tview colour-tag name corresponding to a
// health grade. healthInfo gets a sentinel "-" which tview treats
// as "reset to default colours".
func healthColor(grade health) string {
	switch grade {
	case healthGood:
		return colorGreen
	case healthMarginal:
		return colorYellow
	case healthBad:
		return colorRed
	case healthInfo:
		return "-"
	}

	return "-"
}

// healthLabel is the short human-readable name for a health grade,
// used in the overall-status banner.
func healthLabel(grade health) string {
	switch grade {
	case healthGood:
		return "GOOD"
	case healthMarginal:
		return "MARGINAL"
	case healthBad:
		return "BAD"
	case healthInfo:
		return "—"
	}

	return "—"
}

// stripSeriesDef describes one metric row in the strip chart:
// a 4-char label, an extractor that pulls the metric out of a
// SampleStats, a scale (the expected upper bound; values above
// clamp to the top of the row), a unit suffix for the per-row
// scale annotation, a signed flag (DC channels can be negative,
// so we plot |value| and show the sign in the numeric readout
// next to the bar), a grade function that judges the current
// reading, and a hint string explaining what "good" looks like.
type stripSeriesDef struct {
	label   string
	extract func(rtl2832u.SampleStats) float64
	scale   float64
	unit    string
	signed  bool
	grade   func(value float64) health
	hint    string
}

// stripSeries lists the metrics renderStripChart traces, in order
// from top of the pane to bottom. Each series gets one row.
//
//nolint:gochecknoglobals // pure-data table; equivalent of a typed enum, no runtime mutation.
var stripSeries = []stripSeriesDef{
	{
		label: "rms ", scale: stripScaleRMS, unit: "",
		extract: func(s rtl2832u.SampleStats) float64 { return s.RMS },
		grade:   gradeRMS,
		hint:    "good 5-50",
	},
	{
		label: "sat ", scale: stripScaleSat, unit: "%",
		extract: func(s rtl2832u.SampleStats) float64 { return s.SaturationFrac * stripPercent },
		grade:   gradeSaturationPercent,
		hint:    "good <1%",
	},
	{
		label: "peak", scale: rtl2832u.MaxSampleMagnitude, unit: "",
		extract: func(s rtl2832u.SampleStats) float64 { return s.Peak },
		// peak is informational: it pins at ~181 whenever any
		// burst lands, so it doesn't have a useful good/bad
		// threshold of its own.
		hint: "info only",
	},
	{
		label: "dcI ", scale: stripScaleDC, unit: "", signed: true,
		extract: func(s rtl2832u.SampleStats) float64 { return s.DCI },
		grade:   gradeDC,
		hint:    "good |dc|<1",
	},
	{
		label: "dcQ ", scale: stripScaleDC, unit: "", signed: true,
		extract: func(s rtl2832u.SampleStats) float64 { return s.DCQ },
		grade:   gradeDC,
		hint:    "good |dc|<1",
	},
}

// gradeRMS grades the noise-floor RMS. Below 5 LSB means the chip
// is essentially muted (chain unpowered or LNA dead); above 80
// means the front-end is heading into continuous compression
// (decoder yield collapses). The 5–50 sweet spot leaves room for
// bursts to push above the noise floor without saturating.
func gradeRMS(value float64) health {
	const (
		rmsMutedBelow      = 5.0
		rmsGoodUpper       = 50.0
		rmsCompressedAbove = 80.0
	)

	if value < rmsMutedBelow || value > rmsCompressedAbove {
		return healthBad
	}

	if value > rmsGoodUpper {
		return healthMarginal
	}

	return healthGood
}

// gradeSaturationPercent grades SaturationFrac (already scaled to
// percent at the extractor). Brief burst clipping is fine; >1%
// indicates either heavy traffic (still ok-ish) or chain
// overload; >5% guarantees decoder yield damage.
func gradeSaturationPercent(value float64) health {
	const (
		satGoodUpper     = 1.0
		satMarginalUpper = 5.0
	)

	if value > satMarginalUpper {
		return healthBad
	}

	if value > satGoodUpper {
		return healthMarginal
	}

	return healthGood
}

// gradeDC grades the (signed) DC offset on a single channel.
// Healthy chains stay within ±1 LSB; ±1 to ±2 is drift worth
// noting; beyond ±2 the chip's DC cancellation is overwhelmed.
func gradeDC(value float64) health {
	const (
		dcGoodAbs     = 1.0
		dcMarginalAbs = 2.0
	)

	abs := math.Abs(value)
	if abs > dcMarginalAbs {
		return healthBad
	}

	if abs > dcGoodAbs {
		return healthMarginal
	}

	return healthGood
}

const (
	// stripScaleRMS caps the RMS strip at 100 (well above any
	// realistic ADC noise floor RMS — the 8-bit range maxes the
	// metric near sqrt(2) × 128 ≈ 181).
	stripScaleRMS = 100.0
	// stripScaleSat caps saturation% at 20 — anything beyond
	// that is "saturated everywhere", and visualising the
	// difference between 30% and 50% saturation is not useful.
	stripScaleSat = 20.0
	// stripScaleDC caps |DC| at 3 LSB. Healthy chains stay
	// within ±1–2 so this preserves visibility of small
	// drift; chains with broken DC cancellation will pin the
	// row at the top.
	stripScaleDC = 3.0
	// stripPercent is the percent multiplier reused inside the
	// extractor for the saturation row.
	stripPercent = 100.0
)

// renderStripChart draws the last len(history) samples of each
// series as a row of block characters. Each row is laid out as:
//
//	{label:4} {value:>+7.2f}{unit} {bar...} {scale-annotation}
//
// The numeric value (sign included for signed series) makes small
// DC values readable even though the bar itself can only show
// |value|; the scale annotation at the right edge tells the
// operator what a full bar means.
func renderStripChart(history []rtl2832u.SampleStats, width, height int) string {
	if width <= 0 || height <= 0 || len(history) == 0 {
		return ""
	}

	// labelW = 4, space, valueW = 8 (sign + 6 digits or .xx),
	// unitW up to 1, space, scaleAnnotation up to len(" max=NNN%").
	const (
		labelW              = 4
		valueW              = 8
		scaleAnnotationW    = 10
		minBar              = 4
		prefixAndAnnotation = labelW + 1 + valueW + 1 + scaleAnnotationW + 1
	)

	chartWidth := width - prefixAndAnnotation
	if chartWidth < minBar {
		return ""
	}

	var builder strings.Builder

	rows := min(len(stripSeries), height)
	for rowIdx := range rows {
		series := stripSeries[rowIdx]
		latest := series.extract(history[len(history)-1])

		writeSeriesRow(&builder, series, latest, history, chartWidth)

		if rowIdx < rows-1 {
			builder.WriteByte('\n')
		}
	}

	return builder.String()
}

// writeSeriesRow renders one row of the strip chart: label, the
// latest numeric value (signed if applicable, colour-coded by
// health grade), the bar trace, and a right-side annotation
// holding the operator-facing health hint plus the full-bar scale.
func writeSeriesRow(
	builder *strings.Builder,
	series stripSeriesDef,
	latest float64,
	history []rtl2832u.SampleStats,
	chartWidth int,
) {
	builder.WriteString(series.label)
	builder.WriteByte(' ')

	grade := healthInfo
	if series.grade != nil {
		grade = series.grade(latest)
	}

	colour := healthColor(grade)
	valueFmt := "%7.2f%-1s"

	if series.signed {
		valueFmt = "%+7.2f%-1s"
	}

	_, _ = fmt.Fprintf(builder, "["+colour+"]"+valueFmt+"[-] ", latest, series.unit)

	builder.WriteString(traceSeries(history, series, chartWidth))
	_, _ = fmt.Fprintf(builder, " [grey]%s, max=%g%s[-]", series.hint, series.scale, series.unit)
}

// averageStats returns a SampleStats whose RMS / Peak /
// SaturationFrac / DC fields are the arithmetic mean over the
// last `window` entries of history (or all of history if shorter).
// Samples carries the latest entry's count as a marker that data
// has arrived; the magnitude histogram is left at the zero value
// because averaging bucket counts loses the shape that makes the
// histogram useful — the per-frame histogram from the latest
// snapshot is the right input for that visualisation.
func averageStats(history []rtl2832u.SampleStats, window int) rtl2832u.SampleStats {
	if len(history) == 0 {
		return rtl2832u.SampleStats{}
	}

	start := max(len(history)-window, 0)
	slice := history[start:]
	count := float64(len(slice))

	var avg rtl2832u.SampleStats

	for _, sample := range slice {
		avg.RMS += sample.RMS
		avg.Peak += sample.Peak
		avg.SaturationFrac += sample.SaturationFrac
		avg.DCI += sample.DCI
		avg.DCQ += sample.DCQ
	}

	avg.RMS /= count
	avg.Peak /= count
	avg.SaturationFrac /= count
	avg.DCI /= count
	avg.DCQ /= count
	avg.Samples = slice[len(slice)-1].Samples

	return avg
}

// overallHealth combines the per-series grades into a single
// chain-wide judgment: the worst component wins. Series flagged
// healthInfo (peak) don't participate. Returns the rolled-up
// grade plus the labels of the series that drove it (so the
// banner can name what's wrong).
func overallHealth(latest rtl2832u.SampleStats) (health, []string) {
	worst := healthGood
	worstLabels := []string{}

	for _, series := range stripSeries {
		if series.grade == nil {
			continue
		}

		grade := series.grade(series.extract(latest))
		if grade > worst {
			worst = grade
			worstLabels = []string{strings.TrimSpace(series.label)}
		} else if grade == worst && grade != healthGood {
			worstLabels = append(worstLabels, strings.TrimSpace(series.label))
		}
	}

	if worst == healthGood {
		return worst, nil
	}

	return worst, worstLabels
}

// renderStatusBanner builds the one-line chain-health header
// shown above the strip chart: a coloured GOOD / MARGINAL / BAD
// token plus the contributing series labels when the grade is not
// GOOD. Empty when no sample has landed yet.
func renderStatusBanner(latest rtl2832u.SampleStats) string {
	if latest.Samples == 0 {
		return "[grey]waiting for first sample…[-]"
	}

	grade, labels := overallHealth(latest)
	colour := healthColor(grade)
	label := healthLabel(grade)

	if len(labels) == 0 {
		return fmt.Sprintf("chain: [%s::b]%s[-:-:-]", colour, label)
	}

	return fmt.Sprintf("chain: [%s::b]%s[-:-:-]  (%s)", colour, label, strings.Join(labels, ", "))
}

// advice carries one actionable hint from the diagnose ruleset.
// severity decides the colour the renderer paints the message in.
type advice struct {
	severity health
	message  string
}

// Diagnostic thresholds calibrated against an empirical ADS-B
// chain on radio (192.168.1.159), 2026-05. The rules look at the
// *combination* of metrics so the advice points at a specific
// physical cause rather than restating the per-series grades.
const (
	// adviceMutedRMS / adviceMutedPeak: below both thresholds
	// the chip is essentially silent; suppress every other rule
	// and tell the operator to look at the chain itself.
	adviceMutedRMS  = 5.0
	adviceMutedPeak = 30.0

	// adviceCompressionRMS / adviceClippingRMS: distinguishes
	// "RMS itself is in compression" from "bursts are clipping
	// but noise floor is healthy" — the fixes differ.
	adviceCompressionRMS = 80.0
	adviceClippingRMS    = 30.0

	// adviceUnderRMS: noise floor below ADC quantisation noise.
	// We require a visible Peak so we don't trigger on the
	// chain-muted case (which has its own rule).
	adviceUnderRMS  = 5.0
	adviceUnderPeak = 60.0

	// advicePercent is the scale factor matching the strip chart's
	// extractor for sat. Kept symbolic for readability.
	advicePercent = 100.0
)

// diagnose walks an ordered ruleset against the latest stats and
// returns operator-facing advice items. The list is empty (callers
// render "chain healthy") when no rule fires.
//
// Rules are independent except for the muted-chain check, which
// short-circuits: if the chip is muted, the other readings can't
// be interpreted reliably and would just emit noise advice.
func diagnose(stats rtl2832u.SampleStats) []advice {
	if stats.Samples == 0 {
		return nil
	}

	if stats.RMS < adviceMutedRMS && stats.Peak < adviceMutedPeak {
		return []advice{{
			severity: healthBad,
			message:  "chain muted: check LNA power, USB connection, --gain setting",
		}}
	}

	var out []advice

	if stats.RMS > adviceCompressionRMS {
		out = append(out, advice{
			severity: healthBad,
			message:  "front-end compressed: lower --gain, drop LNA step, or add an in-line attenuator",
		})
	} else if stats.SaturationFrac*advicePercent > 5 && stats.RMS > adviceClippingRMS {
		out = append(out, advice{
			severity: healthMarginal,
			message:  "ADC clipping on bursts: reduce --gain by a few steps if decoder yield is poor",
		})
	}

	if stats.RMS < adviceUnderRMS && stats.Peak > adviceUnderPeak {
		out = append(out, advice{
			severity: healthMarginal,
			message:  "noise floor below quantisation: increase --gain so weak preambles clear the noise",
		})
	}

	if gradeDC(stats.DCI) == healthBad || gradeDC(stats.DCQ) == healthBad {
		out = append(out, advice{
			severity: healthBad,
			message:  "DC offset large: check PPM correction, tuner init, or chain grounding",
		})
	}

	return out
}

// renderAdviceBanner formats diagnose's output for the TUI advice
// row. Multiple hints join with " · "; each is coloured by its
// severity. Empty input renders the green "looks healthy" line so
// the row is never blank once samples are flowing.
func renderAdviceBanner(latest rtl2832u.SampleStats) string {
	if latest.Samples == 0 {
		return ""
	}

	hints := diagnose(latest)
	if len(hints) == 0 {
		return "[green]chain healthy — no changes recommended[-]"
	}

	parts := make([]string, 0, len(hints))

	for _, hint := range hints {
		parts = append(parts, fmt.Sprintf("[%s]%s[-]", healthColor(hint.severity), hint.message))
	}

	return strings.Join(parts, "  ·  ")
}

// traceSeries renders one series of history samples as a row of
// block characters of length width. Latest sample lands at the
// right edge; if history is shorter than width, the left side is
// padded with spaces. Negative values (for signed series like DC)
// are plotted as |value| since a 1-row bipolar bar has too little
// vertical resolution to encode sign — the numeric column already
// shows the sign.
//
// Each cell is coloured by series.grade(raw value) so the
// operator can see when each metric was good / marginal / bad
// across the visible history window. Ungraded series (e.g. peak)
// stay uncoloured.
func traceSeries(history []rtl2832u.SampleStats, series stripSeriesDef, width int) string {
	var builder strings.Builder

	pad := max(width-len(history), 0)

	for range pad {
		builder.WriteByte(' ')
	}

	start := max(len(history)-(width-pad), 0)
	stepCount := float64(len(histogramBlocks) - 1)

	currentColor := ""

	for _, sample := range history[start:] {
		rawValue := series.extract(sample)
		normalised := math.Abs(rawValue) / series.scale

		if normalised > 1 {
			normalised = 1
		}

		step := int(normalised * stepCount)
		char := histogramBlocks[step]

		wantColor := ""
		if series.grade != nil && char != ' ' {
			wantColor = healthColor(series.grade(rawValue))
		}

		currentColor = switchColor(&builder, currentColor, wantColor)
		builder.WriteRune(char)
	}

	if currentColor != "" {
		builder.WriteString("[-]")
	}

	return builder.String()
}
