package main

import "math"

// In-place radix-2 Cooley-Tukey FFT, used by the TUI spectrum
// pane. Pure stdlib, ~70 lines — easier to keep close than to
// pull in a DSP dependency for a single display.
//
// Length must be a power of two. The function panics on bad input
// (length 0 or not a power of two) — callers are spectrum.go's
// internals, which control the length.

// fftInPlace computes the discrete Fourier transform of x in
// place. Output ordering is the natural fftshift-not-yet-applied
// order: bin 0 is DC, bins 1..N/2-1 are positive frequencies,
// bins N/2..N-1 are negative frequencies in ascending order.
// Callers that want a centred spectrum apply a half-rotation
// afterwards.
//
//nolint:varnamelen // FFT math uses canonical single-letter names (n, x, w, k, j).
func fftInPlace(x []complex128) {
	n := len(x)
	if n <= 1 {
		return
	}

	if n&(n-1) != 0 {
		panic("fft: input length must be a power of two")
	}

	bitReversePermute(x)
	cooleyTukeyButterflies(x)
}

// bitReversePermute reorders x so the FFT can run iteratively
// without recursion. Each element i moves to position
// bit-reverse(i, log2(n)).
//
//nolint:varnamelen // FFT math uses canonical single-letter names (n, x, i, j).
func bitReversePermute(x []complex128) {
	n := len(x)
	j := 0

	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}

		j ^= bit

		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
}

// cooleyTukeyButterflies runs the iterative Cooley-Tukey loops
// over the already bit-reverse-permuted input.
//
//nolint:varnamelen // FFT math uses canonical single-letter names (n, x, w, wn, k, i).
func cooleyTukeyButterflies(x []complex128) {
	n := len(x)
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wn := complex(math.Cos(angle), math.Sin(angle))
		halfLength := length / 2

		for i := 0; i < n; i += length {
			w := complex(1, 0) //nolint:mnd // unit twiddle starts each butterfly.

			for k := range halfLength {
				upper := x[i+k]
				lower := x[i+k+halfLength] * w
				x[i+k] = upper + lower
				x[i+k+halfLength] = upper - lower
				w *= wn
			}
		}
	}
}

// hannWindow returns the n-point Hann window value for index i.
// w(i) = 0.5 - 0.5 * cos(2π * i / (n-1)). Used to taper the IQ
// input before FFT so the spectrum doesn't pick up edge-induced
// spectral leakage.
//
//nolint:varnamelen // i/n are the conventional window-function arg names.
func hannWindow(i, n int) float64 {
	if n <= 1 {
		return 1
	}

	const half = 0.5

	return half - half*math.Cos(2*math.Pi*float64(i)/float64(n-1))
}
