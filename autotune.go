package rtl2832u

import (
	"context"
	"fmt"
	"time"
)

// Auto-tune gain
// ==============
//
// The R820T/R860 has three independently-programmable gain stages
// (LNA, Mixer, VGA). For ADS-B reception the empirically-best
// configuration is:
//
//   - VGA pinned at maximum (step 15 = +40.5 dB) to give weak
//     preambles room to push above the noise floor.
//   - Mixer pinned at maximum (step 15) for the same reason.
//   - LNA pinned at maximum on antennas without external pre-amp,
//     dropped one or more steps when an external LNA upstream is
//     loud enough that the chip's own LNA compresses.
//
// Empirically (uConsole + RTL2838 + passive antenna) all-stages-
// max gave 50× more frames in 30 seconds than librtlsdr's
// canonical --gain 49.6 ladder. With a SAW + LNA pre-amp inline
// the librtlsdr ladder catches up because the chip's LNA needs
// less of the gain budget.
//
// Auto-tune resolves "do I have an external LNA or not?" without
// the user having to know: it sets all stages to max, samples the
// dongle's I/Q stream, and only steps the LNA down when the ADC
// is clipping (overload from too much gain in front of it).
//
// Metric: SampleStats.SaturationFrac (fraction of raw samples
// landing at ADC rail 0x00 / 0xFF). Earlier iterations of this
// code used SignalStats.IFAGCValue, but init_chip.go disables the
// chip's demod-side RF/IF AGC loops on the R820T2 path to avoid a
// feedback fight with the tuner's VGA — leaving those registers
// effectively static and unable to drive any search. The host-
// side saturation count is independent of the chip's AGC state
// and always reflects current ADC conditions.
//
// The algorithm is intentionally one-dimensional. Pinning Mixer
// and VGA at their maxima isolates the trade-off to LNA, which
// matches the front-end-saturation problem we're solving and
// keeps the search space at most 16 steps. Most chains converge
// in 1–3 iterations; the loop bounds are spelled out in
// AutoTuneOptions.

// AutoTuneOptions controls the auto-tune algorithm. Zero values
// pick conservative defaults; a caller can override individual
// fields without losing the others.
type AutoTuneOptions struct {
	// SettleDelay is how long to wait after each LNA change
	// before sampling. Two effects gate the lower bound: the
	// tuner's analogue settle time (sub-millisecond on the
	// R820T2) and the URB ring's drop-oldest buffering (~110 ms
	// of pre-change samples sit in the ring until the producer
	// overwrites them). 500 ms is the safe default — well past
	// both.
	SettleDelay time.Duration

	// SampleTarget is the minimum number of I/Q pairs to read
	// per probe. ReadSampleStats consumes whole URBs, so the
	// actual count rounds up to the nearest URB boundary
	// (16 KiB bytes = 8 KiB samples at the driver's default
	// URB length).
	//
	// Why 128 K (≈54 ms at 2.4 MS/s): ADS-B is bursty.
	// Aggregate traffic is ~100 frames/sec across all aircraft;
	// each frame is ~120 µs. Per-window saturation count is
	// dominated by how many bursts the probe happens to catch.
	// Shorter windows (3.4 ms with 8 K samples) had a ~3×
	// burst-count swing between probes, dragging the LNA walk
	// all the way to the floor on hot probes. 128 K samples
	// averages ~5 bursts per window so SaturationFrac
	// stabilises.
	SampleTarget int

	// SaturationThreshold is the maximum SaturationFrac the
	// algorithm tolerates at the converged LNA step.
	//
	// Default 0.05 (5%) — calibrated for ADS-B's bursty signal
	// model. Brief ADC clipping during a valid burst doesn't
	// hurt decode: the bit slicer cares about half-bit timing,
	// not absolute amplitude. True chain overload (intermod or
	// out-of-band emitters) shows up as *continuous* saturation
	// in the 10–50% range, comfortably above the threshold.
	//
	// Tuned empirically on radio (192.168.1.159) with a SAW →
	// USB-LNA → RTL-SDR v3 chain in 2026-05: the manual
	// gain-200 sweep optimum reports ~4% saturation in the
	// probe-stats window and decodes ~24 DF17/min; the 5%
	// threshold lands the autotune in the same operating
	// region. Tighter thresholds (0.5% / 2%) walked LNA too
	// low, losing 50% of the DF17 yield.
	SaturationThreshold float64
}

func (o AutoTuneOptions) orDefaults() AutoTuneOptions {
	const (
		defaultSettle    = 500 * time.Millisecond
		defaultSamples   = 128 * 1024
		defaultThreshold = 0.05
	)

	if o.SettleDelay <= 0 {
		o.SettleDelay = defaultSettle
	}

	if o.SampleTarget <= 0 {
		o.SampleTarget = defaultSamples
	}

	if o.SaturationThreshold <= 0 {
		o.SaturationThreshold = defaultThreshold
	}

	return o
}

// AutoTuneResult records the gain configuration auto-tune
// converged on, plus the SampleStats it observed at that config —
// useful for logging and for callers that want to re-decide on a
// retune. FinalStats.SaturationFrac is the metric the algorithm
// optimised against.
type AutoTuneResult struct {
	LNA        GainStage
	Mixer      GainStage
	VGA        GainStage
	FinalStats SampleStats
	Iterations int
}

// autoTuneGain runs the gradient-descent search described at the
// top of the file. It pins Mixer and VGA at their maxima, then
// walks LNA from 15 downward until ReadSampleStats reports
// SaturationFrac at or below the threshold (or LNA hits zero, the
// saturation floor). The final Tuner state matches the returned
// result.
func autoTuneGain(
	ctx context.Context,
	tuner Tuner,
	reader sampleReader,
	opts AutoTuneOptions,
) (AutoTuneResult, error) {
	opts = opts.orDefaults()

	const (
		topLNA   = r860GainStepCount - 1 // 15
		topMixer = r860GainStepCount - 1
		topVGA   = r860GainStepCount - 1
	)

	mixer := ManualGainStep(topMixer)
	vga := ManualGainStep(topVGA)

	if err := tuner.SetMixerGain(mixer); err != nil {
		return AutoTuneResult{}, fmt.Errorf("autotune: pin mixer at max: %w", err)
	}

	if err := tuner.SetVGAGain(vga); err != nil {
		return AutoTuneResult{}, fmt.Errorf("autotune: pin VGA at max: %w", err)
	}

	for lnaStep := topLNA; ; lnaStep-- {
		lna := ManualGainStep(lnaStep)
		if err := tuner.SetLNAGain(lna); err != nil {
			return AutoTuneResult{}, fmt.Errorf("autotune: set LNA step=%d: %w", lnaStep, err)
		}

		if err := waitOrCancel(ctx, opts.SettleDelay); err != nil {
			return AutoTuneResult{}, err
		}

		stats, err := readSampleStats(ctx, reader, opts.SampleTarget, sampleStatsReadBuf)
		if err != nil {
			return AutoTuneResult{}, fmt.Errorf("autotune: sample at LNA=%d: %w", lnaStep, err)
		}

		// topLNA - lnaStep + 1 = number of iterations completed (1..16).
		iterations := int(topLNA-lnaStep) + 1

		if stats.SaturationFrac <= opts.SaturationThreshold || lnaStep == 0 {
			return AutoTuneResult{
				LNA:        lna,
				Mixer:      mixer,
				VGA:        vga,
				FinalStats: stats,
				Iterations: iterations,
			}, nil
		}
	}
}

// waitOrCancel sleeps for d unless ctx is cancelled first, in
// which case it returns the context's error. Pulled out so the
// auto-tune loop reads as a sequence of intent rather than
// repeating the same select pattern.
func waitOrCancel(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("autotune: %w", ctx.Err())
	}
}
