package rtl2832u

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errScriptedSamplerExhausted fires when a test under-provisions
// the scriptedSampler queue; signals it as a sentinel rather than
// a dynamic ad-hoc error.
var errScriptedSamplerExhausted = errors.New("scriptedSampler: exhausted")

// errSamplerBoom is the sentinel for the propagation test below.
var errSamplerBoom = errors.New("sampler boom")

// scriptedSampler returns a queued sequence of IFAGC values, one
// per SignalStats call. Lets tests drive the auto-tune search
// through a deterministic AGC trajectory without timing dependencies.
type scriptedSampler struct {
	ifValues []int
	next     int
	err      error
}

func (s *scriptedSampler) SignalStats() (SignalStats, error) {
	if s.err != nil {
		return SignalStats{}, s.err
	}

	if s.next >= len(s.ifValues) {
		return SignalStats{}, errScriptedSamplerExhausted
	}

	value := s.ifValues[s.next]
	s.next++

	return SignalStats{IFAGCValue: int16(value)}, nil //nolint:gosec // test fixture; values fit int16.
}

// recordingTuner remembers the most recent gain steps requested
// per stage so tests can assert which configuration auto-tune
// converged on.
type recordingTuner struct {
	lna   GainStage
	mixer GainStage
	vga   GainStage

	lnaSets   []GainStage
	mixerSets []GainStage
	vgaSets   []GainStage

	setLNAErr   error
	setMixerErr error
	setVGAErr   error
}

func (*recordingTuner) Name() string                                   { return "recording" }
func (*recordingTuner) SetFreq(uint32) error                           { return nil }
func (*recordingTuner) SetIFBandwidth(uint8, uint8) error              { return nil }
func (*recordingTuner) SetIFHighPass(uint8) error                      { return nil }
func (*recordingTuner) SetFilterExt(bool) error                        { return nil }
func (*recordingTuner) InitializeForSampleRate(uint32) (uint32, error) { return 0, nil }

func (t *recordingTuner) SetLNAGain(stage GainStage) error {
	t.lna = stage
	t.lnaSets = append(t.lnaSets, stage)

	return t.setLNAErr
}

func (t *recordingTuner) SetMixerGain(stage GainStage) error {
	t.mixer = stage
	t.mixerSets = append(t.mixerSets, stage)

	return t.setMixerErr
}

func (t *recordingTuner) SetVGAGain(stage GainStage) error {
	t.vga = stage
	t.vgaSets = append(t.vgaSets, stage)

	return t.setVGAErr
}

// fastTuneOptions returns AutoTuneOptions with the wait/sample
// durations zeroed so tests don't actually sleep. SettleDelay and
// SampleInterval default to small values via orDefaults; passing
// 1ns lets the time.After fire immediately.
func fastTuneOptions() AutoTuneOptions {
	return AutoTuneOptions{
		SettleDelay:    1 * time.Nanosecond,
		SampleCount:    4,
		SampleInterval: 1 * time.Nanosecond,
	}
}

func TestAutoTuneGainStaysAtMaxOnPassiveAntenna(t *testing.T) {
	t.Parallel()

	// Passive antenna ≈ chip is happy at all-stages-max: AGC
	// returns mild values (well above the over-gained threshold).
	// Returning constant -300 across all 4 samples means mean
	// stays > -3500 → algorithm exits at LNA=15 immediately.
	sampler := &scriptedSampler{ifValues: []int{-300, -300, -300, -300}}
	tuner := &recordingTuner{}

	result, err := autoTuneGain(t.Context(), tuner, sampler, fastTuneOptions())
	if err != nil {
		t.Fatalf("autoTuneGain: %v", err)
	}

	if result.LNA.Step != 15 {
		t.Errorf("converged LNA = %d, want 15 (no descent needed)", result.LNA.Step)
	}

	if result.Mixer.Step != 15 || result.VGA.Step != 15 {
		t.Errorf("Mixer=%d VGA=%d, want both 15", result.Mixer.Step, result.VGA.Step)
	}

	if result.Iterations != 1 {
		t.Errorf("iterations = %d, want 1", result.Iterations)
	}

	if result.FinalIFAGC != -300 {
		t.Errorf("FinalIFAGC = %d, want -300", result.FinalIFAGC)
	}
}

func TestAutoTuneGainDescendsWhenOvergained(t *testing.T) {
	t.Parallel()

	// Hot RF chain: at LNA=15 the chip is severely over-gained
	// (mean -7000); dropping to LNA=14 brings it into the
	// deadband (-2000). 4 samples per iteration × 2 iterations.
	sampler := &scriptedSampler{ifValues: []int{
		-7000, -7000, -7000, -7000, // LNA=15: mean -7000, drop
		-2000, -2000, -2000, -2000, // LNA=14: mean -2000, accept
	}}
	tuner := &recordingTuner{}

	result, err := autoTuneGain(t.Context(), tuner, sampler, fastTuneOptions())
	if err != nil {
		t.Fatalf("autoTuneGain: %v", err)
	}

	if result.LNA.Step != 14 {
		t.Errorf("converged LNA = %d, want 14", result.LNA.Step)
	}

	if result.Iterations != 2 {
		t.Errorf("iterations = %d, want 2", result.Iterations)
	}

	if got := len(tuner.lnaSets); got != 2 {
		t.Errorf("LNA writes = %d, want 2", got)
	}

	if tuner.lnaSets[0].Step != 15 || tuner.lnaSets[1].Step != 14 {
		t.Errorf("LNA trajectory = %v %v, want 15 then 14",
			tuner.lnaSets[0], tuner.lnaSets[1])
	}
}

func TestAutoTuneGainBottomsOutAtLNAZero(t *testing.T) {
	t.Parallel()

	// Pathologically hot input: AGC stays severely over-gained
	// even at LNA=0. Algorithm must terminate at the floor
	// without underflowing the uint8 step counter.
	const stepCount = 16

	values := make([]int, stepCount*4) // 4 samples per LNA step
	for i := range values {
		values[i] = -8000
	}

	sampler := &scriptedSampler{ifValues: values}
	tuner := &recordingTuner{}

	result, err := autoTuneGain(t.Context(), tuner, sampler, fastTuneOptions())
	if err != nil {
		t.Fatalf("autoTuneGain: %v", err)
	}

	if result.LNA.Step != 0 {
		t.Errorf("converged LNA = %d, want 0 (saturation floor)", result.LNA.Step)
	}

	if result.Iterations != stepCount {
		t.Errorf("iterations = %d, want %d", result.Iterations, stepCount)
	}
}

func TestAutoTuneGainContextCancelDuringSettle(t *testing.T) {
	t.Parallel()

	tuner := &recordingTuner{}
	// Long settle so cancellation lands in the time.After branch.
	opts := AutoTuneOptions{
		SettleDelay:    1 * time.Hour,
		SampleCount:    1,
		SampleInterval: 1 * time.Nanosecond,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := autoTuneGain(ctx, tuner, &scriptedSampler{}, opts); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAutoTuneGainPropagatesSamplerError(t *testing.T) {
	t.Parallel()

	tuner := &recordingTuner{}
	sampler := &scriptedSampler{err: errSamplerBoom}

	if _, err := autoTuneGain(t.Context(), tuner, sampler, fastTuneOptions()); !errors.Is(err, errSamplerBoom) {
		t.Errorf("err = %v, want wrapping errSamplerBoom", err)
	}
}

func TestAutoTuneGainPropagatesTunerError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mk   func() *recordingTuner
	}{
		{"mixer", func() *recordingTuner { return &recordingTuner{setMixerErr: errFakeTuner} }},
		{"vga", func() *recordingTuner { return &recordingTuner{setVGAErr: errFakeTuner} }},
		{"lna", func() *recordingTuner { return &recordingTuner{setLNAErr: errFakeTuner} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sampler := &scriptedSampler{ifValues: []int{0, 0, 0, 0}}
			if _, err := autoTuneGain(t.Context(), tc.mk(), sampler, fastTuneOptions()); !errors.Is(err, errFakeTuner) {
				t.Errorf("err = %v, want wrapping errFakeTuner", err)
			}
		})
	}
}

func TestSampleIFAGCMeanSingleSample(t *testing.T) {
	t.Parallel()

	// count=1 hits the early-break-after-first-sample path (no
	// inter-sample wait), which the multi-iteration tests above
	// don't reach because they all use count >= 4.
	sampler := &scriptedSampler{ifValues: []int{-1234}}

	mean, err := sampleIFAGCMean(t.Context(), sampler, 1, 1*time.Nanosecond)
	if err != nil {
		t.Fatalf("sampleIFAGCMean: %v", err)
	}

	if mean != -1234 {
		t.Errorf("mean = %d, want -1234", mean)
	}
}

func TestSampleIFAGCMeanCancelDuringInterval(t *testing.T) {
	t.Parallel()

	sampler := &scriptedSampler{ifValues: []int{0, 0}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := sampleIFAGCMean(ctx, sampler, 2, 1*time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAutoTuneOptionsOrDefaults(t *testing.T) {
	t.Parallel()

	opts := AutoTuneOptions{}.orDefaults()

	if opts.SettleDelay <= 0 {
		t.Errorf("SettleDelay = %v, want positive default", opts.SettleDelay)
	}

	if opts.SampleCount <= 0 {
		t.Errorf("SampleCount = %d, want positive default", opts.SampleCount)
	}

	if opts.SampleInterval <= 0 {
		t.Errorf("SampleInterval = %v, want positive default", opts.SampleInterval)
	}

	if opts.OvergainedThreshold == 0 {
		t.Error("OvergainedThreshold = 0, want non-zero default")
	}

	// Pre-set values are preserved.
	custom := AutoTuneOptions{
		SettleDelay:         42 * time.Millisecond,
		SampleCount:         99,
		SampleInterval:      7 * time.Millisecond,
		OvergainedThreshold: -1234,
	}.orDefaults()

	if custom.SettleDelay != 42*time.Millisecond ||
		custom.SampleCount != 99 ||
		custom.SampleInterval != 7*time.Millisecond ||
		custom.OvergainedThreshold != -1234 {
		t.Errorf("preset overrides not preserved: %+v", custom)
	}
}
