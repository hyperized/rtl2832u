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
// the user having to know: it sets all stages to max, asks the
// RTL2832U's IF AGC how it feels (datasheet §8.1.5
// if_agc_val readback), and only steps the LNA down when the IF
// AGC is severely demanding more attenuation than the VGA can
// provide.
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
	// SettleDelay is how long to wait after each gain change
	// before sampling the AGC. The chip's IF AGC loop with
	// loop_gain2 = 4 stabilises in about 200 ms; 500 ms is the
	// safe default.
	SettleDelay time.Duration

	// SampleCount is how many SignalStats reads to average for
	// each candidate gain config. ADS-B is bursty so the AGC
	// hunts; a single read is unreliable. 16 samples × 30 ms
	// covers most of an aircraft transmission cycle.
	SampleCount int

	// SampleInterval is the spacing between SignalStats reads
	// inside one window.
	SampleInterval time.Duration

	// OvergainedThreshold is the if_agc_val mean below which the
	// chip is considered "severely over-gained" and the LNA
	// should be dropped. Values close to zero indicate a
	// comfortable AGC; large negative means the chip is fighting
	// to attenuate. -3500 sits near halfway to the saturation
	// floor of -8192 — past this point the AGC has lost most of
	// its dynamic range.
	OvergainedThreshold int
}

func (o AutoTuneOptions) orDefaults() AutoTuneOptions {
	const (
		defaultSettle    = 500 * time.Millisecond
		defaultSamples   = 16
		defaultInterval  = 30 * time.Millisecond
		defaultThreshold = -3500
	)

	if o.SettleDelay <= 0 {
		o.SettleDelay = defaultSettle
	}

	if o.SampleCount <= 0 {
		o.SampleCount = defaultSamples
	}

	if o.SampleInterval <= 0 {
		o.SampleInterval = defaultInterval
	}

	if o.OvergainedThreshold == 0 {
		o.OvergainedThreshold = defaultThreshold
	}

	return o
}

// AutoTuneResult records the gain configuration auto-tune
// converged on plus the if_agc_val mean it observed at that
// config — useful for logging and for callers that want to
// re-decide on retune.
type AutoTuneResult struct {
	LNA        GainStage
	Mixer      GainStage
	VGA        GainStage
	FinalIFAGC int
	Iterations int
}

// signalSampler is the slice of Receiver / backend that AutoTuneGain
// needs. Defining it as a small interface keeps autoTuneGain
// callable from tests with a stub that returns canned stats.
type signalSampler interface {
	SignalStats() (SignalStats, error)
}

// autoTuneGain runs the gradient-descent search described at the
// top of the file. It pins Mixer and VGA at their maxima, then
// walks LNA from 15 downward until the IF AGC mean climbs above
// the over-gained threshold (or LNA hits zero, the saturation
// floor). The final Tuner state matches the returned result.
func autoTuneGain(
	ctx context.Context,
	tuner Tuner,
	sampler signalSampler,
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

		mean, err := sampleIFAGCMean(ctx, sampler, opts.SampleCount, opts.SampleInterval)
		if err != nil {
			return AutoTuneResult{}, fmt.Errorf("autotune: sample at LNA=%d: %w", lnaStep, err)
		}

		// topLNA - lnaStep + 1 = number of iterations completed (1..16).
		iterations := int(topLNA-lnaStep) + 1

		if mean > opts.OvergainedThreshold || lnaStep == 0 {
			return AutoTuneResult{
				LNA:        lna,
				Mixer:      mixer,
				VGA:        vga,
				FinalIFAGC: mean,
				Iterations: iterations,
			}, nil
		}
	}
}

// sampleIFAGCMean takes opts.SampleCount SignalStats readings at
// opts.SampleInterval apart and returns the arithmetic mean of
// IFAGCValue. Cancellation propagates through ctx.
func sampleIFAGCMean(
	ctx context.Context,
	sampler signalSampler,
	count int,
	interval time.Duration,
) (int, error) {
	sum := 0

	for sampleIdx := range count {
		stats, err := sampler.SignalStats()
		if err != nil {
			return 0, fmt.Errorf("sample %d/%d: %w", sampleIdx+1, count, err)
		}

		sum += int(stats.IFAGCValue)

		if sampleIdx == count-1 {
			break
		}

		if err := waitOrCancel(ctx, interval); err != nil {
			return 0, err
		}
	}

	return sum / count, nil
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
