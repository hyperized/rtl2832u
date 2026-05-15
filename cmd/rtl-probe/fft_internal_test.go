package main

import (
	"math"
	"math/cmplx"
	"testing"
)

func TestFFTInPlaceDCInput(t *testing.T) {
	t.Parallel()

	// DC input (all 1s) → bin 0 should hold N; all other bins
	// should be ~zero.
	const size = 8

	x := make([]complex128, size) //nolint:varnamelen // FFT convention.
	for i := range x {
		x[i] = complex(1, 0)
	}

	fftInPlace(x)

	if cmplx.Abs(x[0]-complex(size, 0)) > 1e-9 {
		t.Errorf("DC bin = %v, want %v+0i", x[0], size)
	}

	for i := 1; i < size; i++ {
		if cmplx.Abs(x[i]) > 1e-9 {
			t.Errorf("bin %d = %v, want ~0 (DC input concentrates at bin 0)", i, x[i])
		}
	}
}

func TestFFTInPlaceSinusoidConcentratesEnergy(t *testing.T) {
	t.Parallel()

	// Complex exponential at bin k=2: x[n] = exp(j*2π*k*n/N)
	const (
		size   = 16
		binIdx = 2
	)

	x := make([]complex128, size) //nolint:varnamelen // FFT convention.
	for n := range x {
		angle := 2 * math.Pi * float64(binIdx) * float64(n) / float64(size)
		x[n] = complex(math.Cos(angle), math.Sin(angle))
	}

	fftInPlace(x)

	// Energy should be concentrated at bin k.
	mags := make([]float64, size)
	for i, value := range x {
		mags[i] = cmplx.Abs(value)
	}

	peakBin := 0

	for i, magnitude := range mags {
		if magnitude > mags[peakBin] {
			peakBin = i
		}
	}

	if peakBin != binIdx {
		t.Errorf("peak bin = %d, want %d", peakBin, binIdx)
	}

	if mags[peakBin] < float64(size)-1e-6 {
		t.Errorf("peak magnitude = %v, want ~%d", mags[peakBin], size)
	}
}

func TestFFTInPlaceLengthOnePassThrough(t *testing.T) {
	t.Parallel()

	x := []complex128{complex(42, 7)}
	fftInPlace(x)

	if x[0] != complex(42, 7) {
		t.Errorf("length-1 FFT modified the input: %v", x[0])
	}
}

func TestFFTInPlaceNonPowerOfTwoPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("fftInPlace did not panic on length-3 input")
		}
	}()

	fftInPlace(make([]complex128, 3))
}

func TestHannWindowEndpointsAreZero(t *testing.T) {
	t.Parallel()

	const size = 16

	if got := hannWindow(0, size); math.Abs(got) > 1e-9 {
		t.Errorf("hannWindow(0) = %v, want 0", got)
	}

	if got := hannWindow(size-1, size); math.Abs(got) > 1e-9 {
		t.Errorf("hannWindow(last) = %v, want 0", got)
	}
}

func TestHannWindowMidpointIsOne(t *testing.T) {
	t.Parallel()

	const size = 17 // odd → exact midpoint exists

	mid := size / 2
	if got := hannWindow(mid, size); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("hannWindow(mid) = %v, want 1", got)
	}
}

func TestHannWindowDegenerate(t *testing.T) {
	t.Parallel()

	if got := hannWindow(0, 1); got != 1 {
		t.Errorf("hannWindow(0, 1) = %v, want 1 (degenerate one-point window)", got)
	}
}
