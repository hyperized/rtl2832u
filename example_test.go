package rtl2832u_test

import (
	"context"
	"fmt"

	"github.com/hyperized/rtl2832u"
)

// These examples are illustrative — they compile and are picked up
// by godoc as runnable snippets, but they do not include `// Output:`
// directives because executing them requires a real RTL-SDR dongle
// on the host running `go test`. CI runs on hosts without a dongle,
// so the examples are kept compile-only.

// Example shows the canonical happy-path use of the package: open a
// dongle with sensible defaults and a closed-loop gain search, read
// one chunk of IQ samples, then close.
func Example() {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithCenterFreq(rtl2832u.DefaultCenterFreqHz),
		rtl2832u.WithSampleRate(rtl2832u.DefaultSampleRateHz),
		rtl2832u.WithAutoGain(),
	)
	if err != nil {
		_, _ = fmt.Printf("open: %v\n", err)

		return
	}

	defer func() { _ = rcv.Close() }()

	buf := make([]byte, 32*1024)

	count, err := rcv.Read(context.Background(), buf)
	if err != nil {
		_, _ = fmt.Printf("read: %v\n", err)

		return
	}

	_, _ = fmt.Printf("got %d IQ bytes\n", count)
}

// ExampleWithGain shows the librtlsdr-compatible single-knob gain
// ladder: tens-of-dB target, with one VGA pin that matches what
// `rtl_sdr -g 49.6` produces under librtlsdr.
func ExampleWithGain() {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithGain(496), // 49.6 dB on the librtlsdr ladder
	)
	if err != nil {
		_, _ = fmt.Printf("open: %v\n", err)

		return
	}

	defer func() { _ = rcv.Close() }()
}

// ExampleWithLNAGain demonstrates per-stage gain control. The
// R820T / R860 has three independent stages; pin some, hand others
// back to the chip's AGC. Last write wins, so this lands at
// LNA=15, Mixer=AGC, VGA=+0.0 dB.
func ExampleWithLNAGain() {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithGain(rtl2832u.GainAGC),                 // start with all stages on AGC
		rtl2832u.WithLNAGain(rtl2832u.ManualGainStep(15)),   // pin LNA at max
		rtl2832u.WithVGAGain(rtl2832u.VGAStepForCentiDB(0)), // pin VGA at +0.0 dB
	)
	if err != nil {
		_, _ = fmt.Printf("open: %v\n", err)

		return
	}

	defer func() { _ = rcv.Close() }()
}

// ExampleWithBiasTee powers an external active LNA / SAW filter
// from the antenna coax. V3-class dongles wire bias-tee to GPIO0
// by default; for clones use WithBiasTeeGPIO instead.
func ExampleWithBiasTee() {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithAutoGain(),
		rtl2832u.WithBiasTee(true),
	)
	if err != nil {
		_, _ = fmt.Printf("open: %v\n", err)

		return
	}

	defer func() { _ = rcv.Close() }()
}

// ExampleWithFrequencyCorrection trims a drifty TCXO. The +ppm
// shift is applied to a single effective xtal value that flows
// into both the sample-rate divider and the R860 PLL, so a
// per-device calibration constant compensates the entire chain.
// Clamped to ±FrequencyCorrectionPPMMax (1000 ppm).
func ExampleWithFrequencyCorrection() {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithFrequencyCorrection(-37), // crystal runs ~37 ppm slow
	)
	if err != nil {
		_, _ = fmt.Printf("open: %v\n", err)

		return
	}

	defer func() { _ = rcv.Close() }()
}
