package rtl2832u

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

// fastTuneOptions returns AutoTuneOptions with SettleDelay zeroed
// so tests don't sleep. SampleTarget is set small enough that one
// canned chunk satisfies a probe.
func fastTuneOptions() AutoTuneOptions {
	return AutoTuneOptions{
		SettleDelay:         1 * time.Nanosecond,
		SampleTarget:        4,
		SaturationThreshold: 0.005,
	}
}

// cleanChunk builds a byte slice of `samples` I/Q pairs all at raw
// 0x80 (midrange), so SaturationFrac is 0.
func cleanChunk(samples int) []byte {
	const dcMid = 0x80

	out := make([]byte, samples*2)
	for i := range out {
		out[i] = dcMid
	}

	return out
}

// saturatedChunk builds a byte slice of `samples` I/Q pairs all at
// raw 0xFF, so SaturationFrac is 1.0.
func saturatedChunk(samples int) []byte {
	out := make([]byte, samples*2)
	for i := range out {
		out[i] = 0xFF
	}

	return out
}

func TestAutoTuneGainStaysAtMaxOnPassiveAntenna(t *testing.T) {
	t.Parallel()

	// Passive antenna ≈ ADC sees no saturation at LNA=15 →
	// algorithm converges at the top step immediately.
	reader := &scriptedSampleReader{
		chunks:     [][]byte{cleanChunk(4)},
		queueEmpty: errSampleReaderBoom,
	}
	tuner := &recordingTuner{}

	result, err := autoTuneGain(t.Context(), tuner, reader, fastTuneOptions())
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

	if result.FinalStats.SaturationFrac != 0 {
		t.Errorf("FinalStats.SaturationFrac = %v, want 0", result.FinalStats.SaturationFrac)
	}
}

func TestAutoTuneGainDescendsWhenOvergained(t *testing.T) {
	t.Parallel()

	// Hot RF chain: at LNA=15 the ADC saturates; at LNA=14 it
	// recovers. Algorithm should walk down one step.
	reader := &scriptedSampleReader{
		chunks: [][]byte{
			saturatedChunk(4), // LNA=15: 100% saturation, drop
			cleanChunk(4),     // LNA=14: 0% saturation, accept
		},
		queueEmpty: errSampleReaderBoom,
	}
	tuner := &recordingTuner{}

	result, err := autoTuneGain(t.Context(), tuner, reader, fastTuneOptions())
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

	// Pathologically hot input: every LNA step still reports
	// 100% saturation. Algorithm must terminate at the floor
	// without underflowing the uint8 step counter.
	const stepCount = 16

	chunks := make([][]byte, stepCount)
	for i := range chunks {
		chunks[i] = saturatedChunk(4)
	}

	reader := &scriptedSampleReader{
		chunks:     chunks,
		queueEmpty: errSampleReaderBoom,
	}
	tuner := &recordingTuner{}

	result, err := autoTuneGain(t.Context(), tuner, reader, fastTuneOptions())
	if err != nil {
		t.Fatalf("autoTuneGain: %v", err)
	}

	if result.LNA.Step != 0 {
		t.Errorf("converged LNA = %d, want 0 (saturation floor)", result.LNA.Step)
	}

	if result.Iterations != stepCount {
		t.Errorf("iterations = %d, want %d", result.Iterations, stepCount)
	}

	if result.FinalStats.SaturationFrac != 1.0 {
		t.Errorf("FinalStats.SaturationFrac = %v, want 1.0 at LNA floor", result.FinalStats.SaturationFrac)
	}
}

func TestAutoTuneGainContextCancelDuringSettle(t *testing.T) {
	t.Parallel()

	tuner := &recordingTuner{}
	// Long settle so cancellation lands in the time.After branch.
	opts := AutoTuneOptions{
		SettleDelay:         1 * time.Hour,
		SampleTarget:        1,
		SaturationThreshold: 0.005,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := autoTuneGain(ctx, tuner, &scriptedSampleReader{}, opts); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAutoTuneGainPropagatesReaderError(t *testing.T) {
	t.Parallel()

	tuner := &recordingTuner{}
	reader := &scriptedSampleReader{err: errSampleReaderBoom}

	if _, err := autoTuneGain(t.Context(), tuner, reader, fastTuneOptions()); !errors.Is(err, errSampleReaderBoom) {
		t.Errorf("err = %v, want wrapping errSampleReaderBoom", err)
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

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			reader := &scriptedSampleReader{
				chunks:     [][]byte{cleanChunk(4)},
				queueEmpty: errSampleReaderBoom,
			}

			_, err := autoTuneGain(t.Context(), testCase.mk(), reader, fastTuneOptions())
			if !errors.Is(err, errFakeTuner) {
				t.Errorf("err = %v, want wrapping errFakeTuner", err)
			}
		})
	}
}

func TestAutoTuneOptionsOrDefaults(t *testing.T) {
	t.Parallel()

	opts := AutoTuneOptions{}.orDefaults()

	if opts.SettleDelay <= 0 {
		t.Errorf("SettleDelay = %v, want positive default", opts.SettleDelay)
	}

	if opts.SampleTarget <= 0 {
		t.Errorf("SampleTarget = %d, want positive default", opts.SampleTarget)
	}

	if opts.SaturationThreshold <= 0 {
		t.Errorf("SaturationThreshold = %v, want positive default", opts.SaturationThreshold)
	}

	// Pre-set values are preserved.
	custom := AutoTuneOptions{
		SettleDelay:         42 * time.Millisecond,
		SampleTarget:        99,
		SaturationThreshold: 0.01,
	}.orDefaults()

	if custom.SettleDelay != 42*time.Millisecond ||
		custom.SampleTarget != 99 ||
		custom.SaturationThreshold != 0.01 {
		t.Errorf("preset overrides not preserved: %+v", custom)
	}
}
