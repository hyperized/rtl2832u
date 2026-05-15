package rtl2832u

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// SampleStats summarises a window of raw I/Q samples taken from
// the dongle's bulk endpoint. Computed host-side from the same
// bytes that feed the demodulator, so these values are unaffected
// by the demod-side AGC loops being disabled on the R820T2 path
// (see init_chip.go's disableRFIFAGC for why that path is taken).
//
// SampleStats is the right input for any gain or chain-quality
// diagnostic on this driver — SignalStats's AGC register reads
// return effectively static values once the demod's RF/IF AGC
// loops are off.
//
// Magnitude fields are in 8-bit ADC units after the canonical
// 128-offset removal: I and Q each span [-128, +127], so a single-
// sample magnitude maxes at sqrt(128² + 128²) ≈ 181.
type SampleStats struct {
	// Samples is the number of I/Q sample pairs aggregated. Two
	// bytes from the bulk stream per sample.
	Samples int

	// DCI / DCQ are the arithmetic means of the I and Q channels
	// after offset removal. A healthy chain reports values near
	// zero; sustained drift indicates the demod's DC cancellation
	// is overwhelmed or the chip is in a bad configuration.
	DCI, DCQ float64

	// RMS is the root-mean-square sample magnitude over the
	// window. Tracks input signal power: doubles for every 6 dB
	// of additional gain in front of the ADC, until saturation
	// starts clipping the peaks.
	RMS float64

	// Peak is the maximum single-sample magnitude in the window.
	// Pinning at the ADC rail (~181) indicates the front-end is
	// in compression — more gain than the ADC can swing.
	Peak float64

	// SaturationFrac is the fraction of samples where either I or
	// Q landed at the ADC rail (raw byte 0x00 or 0xFF). Above ~1%
	// suggests overload; above ~10% guarantees intermod and
	// decoder yield collapse.
	SaturationFrac float64

	// MagnitudeHistogram bins per-sample magnitude into
	// HistogramBuckets equal-width buckets covering [0,
	// MaxSampleMagnitude]. Bucket index = floor(|sample| /
	// (MaxSampleMagnitude / HistogramBuckets)); the topmost
	// bucket also catches the (theoretically impossible)
	// |sample| == MaxSampleMagnitude case.
	//
	// Used by live diagnostic UIs to visualise the gain regime:
	// a healthy chain shows a bell-shaped peak near the noise
	// floor with a thin tail; under-gained chains pile up near
	// bucket 0; over-gained chains spike at the topmost bucket
	// (clipping). The aggregate Sample/RMS/Peak/SaturationFrac
	// fields summarise the same data — this is the raw shape.
	MagnitudeHistogram [HistogramBuckets]uint32
}

// HistogramBuckets is the bin count of SampleStats.MagnitudeHistogram.
// 64 covers [0, MaxSampleMagnitude] with ~2.83-unit-wide bins, which
// renders cleanly into a single TUI pane while preserving enough
// shape to distinguish noise from burst tails.
const HistogramBuckets = 64

// MaxSampleMagnitude is the upper bound of a single I/Q sample's
// magnitude after 128-offset removal, i.e. sqrt(128² + 128²). Used
// as the histogram's full-scale range.
const MaxSampleMagnitude = 181.019335983756 // sqrt(128*128 + 128*128)

// ErrInvalidSampleTarget fires when ReadSampleStats was asked for
// zero or negative samples. Exposed as a sentinel so callers can
// distinguish programmer error from a stream/IO failure.
var ErrInvalidSampleTarget = errors.New("rtl2832u: ReadSampleStats target sample count must be positive")

// sampleStatsReadBuf is the read-side scratch buffer size used by
// ReadSampleStats. Matches the URB length used by the streaming
// ring (bulk_linux.go's streamURBLen) so a single Read consumes a
// whole URB and the loop iteration count tracks chunk arrivals.
const sampleStatsReadBuf = 16 * 1024

// ReadSampleStats reads at least targetSamples I/Q pairs from the
// dongle and returns aggregate magnitude statistics. The call
// issues one or more Read invocations against the bulk endpoint
// until the target is met or ctx is cancelled.
//
// targetSamples must be > 0. The returned Samples count may be
// slightly larger than the target, since a Read returns a whole
// URB; values around 8 KiB samples (≈3.4 ms at 2.4 MS/s) are
// convenient for autotune steps and ~128 KiB samples (≈54 ms)
// for one-shot diagnostics.
//
// ReadSampleStats competes with any other consumer of the bulk
// stream — the dongle is single-producer — so call it during a
// dedicated probe window, not concurrently with the demodulator.
func (r *Receiver) ReadSampleStats(ctx context.Context, targetSamples int) (SampleStats, error) {
	if targetSamples <= 0 {
		return SampleStats{}, ErrInvalidSampleTarget
	}

	return readSampleStats(ctx, r, targetSamples, sampleStatsReadBuf)
}

// sampleReader is the slice of Receiver / backend that
// readSampleStats needs. Defining it as a small interface keeps
// the function callable from tests with a stub that returns canned
// chunk bytes, mirroring the signalSampler pattern used by
// autotune.
type sampleReader interface {
	Read(ctx context.Context, p []byte) (int, error)
}

// ComputeSampleStats folds a raw I/Q byte buffer through the same
// magnitude accumulator ReadSampleStats uses and returns the
// finalised SampleStats. The input is interleaved unsigned 8-bit
// I/Q (I, Q, I, Q, …) — the format the bulk endpoint emits.
//
// Useful when a caller already has the raw bytes in hand (replay
// mode, a shared buffer that also feeds an FFT, etc.) and would
// rather not perform a redundant Read pass through
// ReadSampleStats. A trailing half-sample byte is dropped: the
// bulk endpoint emits whole I/Q pairs, but defending against a
// partial chunk keeps the function total.
func ComputeSampleStats(raw []byte) SampleStats {
	var acc sampleStatsAccumulator

	acc.consume(raw)

	return acc.finalise()
}

// readSampleStats is the testable core of ReadSampleStats: the
// public method validates input and supplies the production read
// buffer size, this function drives the read loop and accumulator.
// Pulled out as an unexported function so internal tests can
// exercise the loop without an open device.
func readSampleStats(
	ctx context.Context,
	reader sampleReader,
	targetSamples, bufBytes int,
) (SampleStats, error) {
	buf := make([]byte, bufBytes)
	targetBytes := targetSamples * 2

	var acc sampleStatsAccumulator

	for acc.byteCount() < targetBytes {
		n, err := reader.Read(ctx, buf)
		if err != nil {
			return SampleStats{}, fmt.Errorf("rtl2832u: ReadSampleStats: read chunk: %w", err)
		}

		acc.consume(buf[:n])
	}

	return acc.finalise(), nil
}

// sampleStatsAccumulator collects running totals across one or
// more URB chunks. Separated from readSampleStats so the math is
// unit-testable without an io path, and so a future caller that
// already holds raw bytes (e.g. replay-mode probing) can feed them
// in directly.
type sampleStatsAccumulator struct {
	samples   int64
	sumI      int64
	sumQ      int64
	sumMagSq  int64
	peakMagSq int64
	saturated int64
	histogram [HistogramBuckets]uint32
}

// byteCount returns how many sample bytes have been consumed so
// far. Pulled out so readSampleStats reads as a clean "until we
// have enough" loop.
func (a *sampleStatsAccumulator) byteCount() int {
	const bytesPerSample = 2

	return int(a.samples) * bytesPerSample
}

// consume folds a chunk of raw I/Q bytes into the running totals.
// A trailing odd byte (half a sample) is dropped — the bulk
// endpoint emits whole I/Q pairs in practice, but defending
// against a partial Read keeps the accumulator total-function.
func (a *sampleStatsAccumulator) consume(chunk []byte) {
	const dcOffset = 128

	// &^ 1 rounds down to the nearest even count: any trailing
	// half-sample byte gets dropped.
	end := len(chunk) &^ 1

	for i := 0; i < end; i += 2 {
		rawI := chunk[i]
		//nolint:gosec // i is even and < end ≤ len(chunk), so i+1 is in range.
		rawQ := chunk[i+1]

		// Range after 128-offset: [-128, +127]; product fits in
		// int comfortably on both 32-bit (uConsole arm) and
		// 64-bit hosts (max single-sample magnitude squared is
		// 2 × 128² = 32_768).
		iVal := int(rawI) - dcOffset
		qVal := int(rawQ) - dcOffset

		a.samples++
		a.sumI += int64(iVal)
		a.sumQ += int64(qVal)

		magSq := int64(iVal*iVal + qVal*qVal)
		a.sumMagSq += magSq

		if magSq > a.peakMagSq {
			a.peakMagSq = magSq
		}

		if isRail(rawI) || isRail(rawQ) {
			a.saturated++
		}

		a.histogram[magSqToBucket(magSq)]++
	}
}

// magSqToBucket maps a squared sample magnitude to its histogram
// bucket index. Bucket b covers magnitude [b * bucketWidth, (b+1) *
// bucketWidth); the topmost bucket clamps the |s| ==
// MaxSampleMagnitude edge.
func magSqToBucket(magSq int64) int {
	const (
		lastBucket  = HistogramBuckets - 1
		bucketWidth = MaxSampleMagnitude / HistogramBuckets
	)

	idx := int(math.Sqrt(float64(magSq)) / bucketWidth)
	if idx > lastBucket {
		return lastBucket
	}

	return idx
}

// isRail reports whether a raw ADC byte sits at the converter's
// minimum or maximum code, i.e. the sample is clipped against the
// rails. Pulled out for clarity at the call-site of consume.
func isRail(b byte) bool {
	return b == 0x00 || b == 0xFF
}

// finalise converts the running totals into a SampleStats value.
// A zero-sample accumulator returns the zero SampleStats so the
// caller does not need to special-case the "no chunks arrived"
// path — though readSampleStats's loop only finalises after the
// target byte count is reached, so this is mostly a guard for
// tests that bypass the loop.
func (a *sampleStatsAccumulator) finalise() SampleStats {
	if a.samples == 0 {
		return SampleStats{}
	}

	samples := float64(a.samples)

	return SampleStats{
		Samples:            int(a.samples),
		DCI:                float64(a.sumI) / samples,
		DCQ:                float64(a.sumQ) / samples,
		RMS:                math.Sqrt(float64(a.sumMagSq) / samples),
		Peak:               math.Sqrt(float64(a.peakMagSq)),
		SaturationFrac:     float64(a.saturated) / samples,
		MagnitudeHistogram: a.histogram,
	}
}
