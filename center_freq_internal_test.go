package rtl2832u

import (
	"errors"
	"testing"
)

// errFakeTuner is the static sentinel a fakeTuner returns when
// configured to fail. err113 forbids ad-hoc errors.New in tests.
var errFakeTuner = errors.New("fake tuner failure")

// fakeTuner implements the Tuner interface for tests. It captures
// the requested centre frequency so per-test assertions can check
// that delegation occurred with the right argument; setting setErr
// non-nil makes SetFreq fail without touching the controller.
type fakeTuner struct {
	name       string
	lastFreqHz uint32
	calls      int
	setFreqErr error
}

func (t *fakeTuner) Name() string { return t.name }

func (t *fakeTuner) SetFreq(rfHz uint32) error {
	t.calls++
	t.lastFreqHz = rfHz

	return t.setFreqErr
}

// SetLNAGain / SetMixerGain / SetVGAGain / SetIFBandwidth /
// SetIFHighPass / SetFilterExt are no-ops in the fake; the
// existing center-freq tests only exercise SetFreq delegation.
// Adding the methods here keeps fakeTuner compatible with the
// extended Tuner interface without ballooning every center-freq
// test with stage-call assertions.
func (*fakeTuner) SetLNAGain(GainStage) error        { return nil }
func (*fakeTuner) SetMixerGain(GainStage) error      { return nil }
func (*fakeTuner) SetVGAGain(GainStage) error        { return nil }
func (*fakeTuner) SetIFBandwidth(uint8, uint8) error { return nil }
func (*fakeTuner) SetIFHighPass(uint8) error         { return nil }
func (*fakeTuner) SetFilterExt(bool) error           { return nil }

func TestSetCenterFreqDelegatesToTuner(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	tuner := &fakeTuner{name: "fake"}

	if err := chip.SetCenterFreq(1_090_000_000, tuner); err != nil {
		t.Fatalf("SetCenterFreq: %v", err)
	}

	if tuner.calls != 1 {
		t.Errorf("tuner.calls = %d, want 1", tuner.calls)
	}

	if tuner.lastFreqHz != 1_090_000_000 {
		t.Errorf("tuner.lastFreqHz = %d, want 1_090_000_000", tuner.lastFreqHz)
	}

	// SetCenterFreq must not touch the chip's controller; with
	// Zero-IF mode the demod has nothing to reprogram.
	if len(mock.calls) != 0 {
		t.Errorf("controller.calls = %d, want 0 (chip stays in Zero-IF)", len(mock.calls))
	}
}

func TestSetCenterFreqNilTunerReturnsErrNoTuner(t *testing.T) {
	t.Parallel()

	mock := &mockController{}
	chip := newRTL2832U(mock)

	err := chip.SetCenterFreq(1_090_000_000, nil)
	if !errors.Is(err, ErrNoTuner) {
		t.Errorf("err = %v, want ErrNoTuner", err)
	}
}

func TestSetCenterFreqWrapsTunerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{}
	chip := newRTL2832U(mock)

	tuner := &fakeTuner{name: "fake", setFreqErr: errFakeTuner}

	err := chip.SetCenterFreq(1_090_000_000, tuner)
	if !errors.Is(err, errFakeTuner) {
		t.Errorf("err = %v, want wrapping errFakeTuner", err)
	}
}
