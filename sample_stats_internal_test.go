package rtl2832u

import (
	"context"
	"errors"
	"math"
	"testing"
)

// errSampleReaderBoom is the sentinel for the propagation tests.
var errSampleReaderBoom = errors.New("sampleReader: boom")

// scriptedSampleReader emits a queued sequence of byte chunks, one
// per Read call. After the queue is exhausted Read returns 0 and
// the queueEmpty error so tests catch under-provisioning.
type scriptedSampleReader struct {
	chunks      [][]byte
	next        int
	err         error
	readCalls   int
	queueEmpty  error
	ctxObserved []context.Context //nolint:containedctx // test fixture; observes the ctx passed in.
}

func (s *scriptedSampleReader) Read(ctx context.Context, dst []byte) (int, error) {
	s.readCalls++
	s.ctxObserved = append(s.ctxObserved, ctx)

	if s.err != nil {
		return 0, s.err
	}

	if s.next >= len(s.chunks) {
		return 0, s.queueEmpty
	}

	chunk := s.chunks[s.next]
	s.next++

	return copy(dst, chunk), nil
}

func TestSampleStatsAccumulatorAllZeroSamples(t *testing.T) {
	t.Parallel()

	var acc sampleStatsAccumulator
	// 8 raw bytes = 4 I/Q samples all reading raw 0x80 (= 0 post-offset).
	acc.consume([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})

	got := acc.finalise()
	if got.Samples != 4 {
		t.Errorf("Samples = %d, want 4", got.Samples)
	}

	if got.DCI != 0 || got.DCQ != 0 {
		t.Errorf("DCI/DCQ = %v/%v, want 0/0", got.DCI, got.DCQ)
	}

	if got.RMS != 0 {
		t.Errorf("RMS = %v, want 0", got.RMS)
	}

	if got.Peak != 0 {
		t.Errorf("Peak = %v, want 0", got.Peak)
	}

	if got.SaturationFrac != 0 {
		t.Errorf("SaturationFrac = %v, want 0", got.SaturationFrac)
	}
}

func TestSampleStatsAccumulatorPeakAndRMS(t *testing.T) {
	t.Parallel()

	// Sample 1: I=+50, Q=-30  → |s|² = 2500+900 = 3400
	// Sample 2: I=+100, Q=+100 → |s|² = 20_000 (peak)
	// Sample 3: I=-20, Q=+40  → |s|² = 400+1600 = 2000
	chunk := []byte{
		128 + 50, 128 - 30,
		128 + 100, 128 + 100,
		128 - 20, 128 + 40,
	}

	var acc sampleStatsAccumulator
	acc.consume(chunk)

	got := acc.finalise()

	if got.Samples != 3 {
		t.Fatalf("Samples = %d, want 3", got.Samples)
	}

	// Peak should be sqrt(20_000).
	wantPeak := math.Sqrt(20_000)
	if math.Abs(got.Peak-wantPeak) > 1e-9 {
		t.Errorf("Peak = %v, want %v", got.Peak, wantPeak)
	}

	// RMS = sqrt(sum/N) = sqrt((3400+20000+2000)/3) = sqrt(25400/3).
	wantRMS := math.Sqrt((3400.0 + 20_000.0 + 2000.0) / 3)
	if math.Abs(got.RMS-wantRMS) > 1e-9 {
		t.Errorf("RMS = %v, want %v", got.RMS, wantRMS)
	}

	wantDCI := (50.0 + 100.0 - 20.0) / 3
	if math.Abs(got.DCI-wantDCI) > 1e-9 {
		t.Errorf("DCI = %v, want %v", got.DCI, wantDCI)
	}

	wantDCQ := (-30.0 + 100.0 + 40.0) / 3
	if math.Abs(got.DCQ-wantDCQ) > 1e-9 {
		t.Errorf("DCQ = %v, want %v", got.DCQ, wantDCQ)
	}
}

func TestSampleStatsAccumulatorSaturationCounting(t *testing.T) {
	t.Parallel()

	// 4 samples: two with one channel at a rail, one with both at
	// rails, one clean.
	chunk := []byte{
		0xFF, 128, // I high rail
		128, 0x00, // Q low rail
		0x00, 0xFF, // both rails
		128 + 10, 128 + 10, // clean
	}

	var acc sampleStatsAccumulator
	acc.consume(chunk)

	got := acc.finalise()
	if got.Samples != 4 {
		t.Fatalf("Samples = %d, want 4", got.Samples)
	}

	wantFrac := 3.0 / 4.0
	if math.Abs(got.SaturationFrac-wantFrac) > 1e-9 {
		t.Errorf("SaturationFrac = %v, want %v", got.SaturationFrac, wantFrac)
	}
}

func TestSampleStatsAccumulatorTrailingHalfSampleDropped(t *testing.T) {
	t.Parallel()

	// 5 bytes: 2 whole samples + 1 stray byte. The stray must not
	// land in the accumulator (no overflow into Q, no I-only sample).
	chunk := []byte{128 + 10, 128 + 10, 128 + 20, 128 + 20, 0xFF}

	var acc sampleStatsAccumulator
	acc.consume(chunk)

	got := acc.finalise()
	if got.Samples != 2 {
		t.Errorf("Samples = %d, want 2 (trailing half-sample byte must be ignored)", got.Samples)
	}
	// The stray 0xFF would have raised SaturationFrac if it were
	// consumed; assert it stays zero.
	if got.SaturationFrac != 0 {
		t.Errorf("SaturationFrac = %v, want 0", got.SaturationFrac)
	}
}

func TestSampleStatsAccumulatorEmptyChunkIsNoop(t *testing.T) {
	t.Parallel()

	var acc sampleStatsAccumulator
	acc.consume(nil)
	acc.consume([]byte{})

	got := acc.finalise()
	if got != (SampleStats{}) {
		t.Errorf("finalise() = %+v, want zero SampleStats", got)
	}
}

func TestSampleStatsAccumulatorByteCountTracksSamples(t *testing.T) {
	t.Parallel()

	var acc sampleStatsAccumulator
	if acc.byteCount() != 0 {
		t.Errorf("byteCount() at zero = %d, want 0", acc.byteCount())
	}

	acc.consume([]byte{128, 128, 128, 128, 128, 128})

	if acc.byteCount() != 6 {
		t.Errorf("byteCount() after 3 samples = %d, want 6", acc.byteCount())
	}
}

func TestReadSampleStatsLoopsUntilTargetReached(t *testing.T) {
	t.Parallel()

	// Four samples per chunk × three chunks = 12 samples total.
	chunk := []byte{128, 128, 128, 128, 128, 128, 128, 128}
	reader := &scriptedSampleReader{
		chunks:     [][]byte{chunk, chunk, chunk},
		queueEmpty: errSampleReaderBoom,
	}

	const target = 10 // requires at least 3 chunks (4+4+4 = 12 ≥ 10).

	got, err := readSampleStats(context.Background(), reader, target, len(chunk))
	if err != nil {
		t.Fatalf("readSampleStats: unexpected error: %v", err)
	}

	if got.Samples != 12 {
		t.Errorf("Samples = %d, want 12 (3 chunks × 4 samples)", got.Samples)
	}

	if reader.readCalls != 3 {
		t.Errorf("readCalls = %d, want 3", reader.readCalls)
	}
}

func TestReadSampleStatsWrapsReaderError(t *testing.T) {
	t.Parallel()

	reader := &scriptedSampleReader{err: errSampleReaderBoom}

	_, err := readSampleStats(context.Background(), reader, 1, 16)
	if err == nil {
		t.Fatal("readSampleStats: want error, got nil")
	}

	if !errors.Is(err, errSampleReaderBoom) {
		t.Errorf("error chain = %v, want errors.Is errSampleReaderBoom", err)
	}
}

func TestMagSqToBucketEndpoints(t *testing.T) {
	t.Parallel()

	// Zero magnitude lands in bucket 0.
	if got := magSqToBucket(0); got != 0 {
		t.Errorf("magSqToBucket(0) = %d, want 0", got)
	}

	// Max magnitude squared lands in the top bucket (clamp).
	maxMagSq := int64(128*128 + 128*128) // 32_768
	if got := magSqToBucket(maxMagSq); got != HistogramBuckets-1 {
		t.Errorf("magSqToBucket(maxMagSq) = %d, want %d", got, HistogramBuckets-1)
	}

	// Magnitude beyond the canonical max (theoretical-only) still
	// clamps to the top bucket rather than overflowing.
	if got := magSqToBucket(maxMagSq * 2); got != HistogramBuckets-1 {
		t.Errorf("magSqToBucket(2 * maxMagSq) = %d, want %d (clamp)", got, HistogramBuckets-1)
	}
}

func TestMagSqToBucketMonotonic(t *testing.T) {
	t.Parallel()

	// Sweep magnitudes from 0 to max and assert the bucket index
	// is non-decreasing.
	last := -1

	for mag := range 182 {
		got := magSqToBucket(int64(mag * mag))
		if got < last {
			t.Errorf("magSqToBucket non-monotonic at mag=%d: got %d, previous %d", mag, got, last)
		}

		last = got
	}
}

func TestSampleStatsAccumulatorHistogramBins(t *testing.T) {
	t.Parallel()

	// Three samples with deliberately distinct magnitudes so each
	// lands in a different bucket. Verify the histogram counts
	// match.
	chunk := []byte{
		0x80, 0x80, //   |s| = 0      → bucket 0
		128 + 100, 128 + 100, // |s| ≈ 141  → mid-upper bucket
		0xFF, 0xFF, //   |s| ≈ 180  → top-or-near-top bucket
	}

	var acc sampleStatsAccumulator
	acc.consume(chunk)

	got := acc.finalise()
	if got.Samples != 3 {
		t.Fatalf("Samples = %d, want 3", got.Samples)
	}

	if got.MagnitudeHistogram[0] != 1 {
		t.Errorf("bucket 0 count = %d, want 1 (the zero-magnitude sample)", got.MagnitudeHistogram[0])
	}

	if got.MagnitudeHistogram[HistogramBuckets-1] == 0 {
		t.Error("top bucket count = 0, want >= 1 (the rail-saturated sample)")
	}

	// Total of all bucket counts must equal Samples — every
	// sample lands in exactly one bucket.
	var total uint32

	for _, count := range got.MagnitudeHistogram {
		total += count
	}

	if total != uint32(got.Samples) { //nolint:gosec // got.Samples is from int64.
		t.Errorf("histogram total = %d, want %d (Samples)", total, got.Samples)
	}
}

func TestReadSampleStatsStopsAfterFirstSufficientChunk(t *testing.T) {
	t.Parallel()

	// A single chunk providing more samples than the target should
	// terminate the loop in one Read.
	chunk := []byte{128, 128, 128, 128} // 2 samples
	reader := &scriptedSampleReader{
		chunks:     [][]byte{chunk, chunk},
		queueEmpty: errSampleReaderBoom,
	}

	if _, err := readSampleStats(context.Background(), reader, 1, len(chunk)); err != nil {
		t.Fatalf("readSampleStats: unexpected error: %v", err)
	}

	if reader.readCalls != 1 {
		t.Errorf("readCalls = %d, want 1 (loop should stop after first sufficient chunk)", reader.readCalls)
	}
}
