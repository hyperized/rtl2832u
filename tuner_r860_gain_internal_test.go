package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

// gainTunerOnFakeBus builds an *R860 wired to a fakeI2C with the
// shadow seeded the way an actual init() pass would leave it. The
// gain registers (R5/R7/R12) start at their datasheet defaults
// (matching r860InitValues), so masked writes can be reasoned about
// against a deterministic baseline.
func gainTunerOnFakeBus(t *testing.T) (*R860, *fakeI2C) {
	t.Helper()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	// Drop the bus ops captured during construction; tests want to
	// assert only against the gain-call writes.
	bus.ops = nil

	return tuner, bus
}

// expectMaskedWrite asserts that `bus` saw exactly one I²C write
// at gain-register `reg` carrying the masked value `wantValue`
// merged into the shadow's prior value. Since writeRegisterMasked
// reads the shadow first, this also verifies the shadow update is
// consistent.
func expectMaskedWrite(t *testing.T, bus *fakeI2C, reg, wantValue, mask, priorShadow uint8) {
	t.Helper()

	wantPayload := []byte{reg, (priorShadow &^ mask) | (wantValue & mask)}

	writes := 0

	for _, busOp := range bus.ops {
		if busOp.kind != opWrite || busOp.addr != r860I2CAddr {
			continue
		}

		writes++

		if !reflect.DeepEqual(busOp.data, wantPayload) {
			t.Errorf("write payload = %#v, want %#v", busOp.data, wantPayload)
		}
	}

	if writes != 1 {
		t.Errorf("write count = %d, want 1", writes)
	}
}

func TestSetLNAGainManualWritesMaskedRegister(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	prior := tuner.shadow[regR860LNAGain]

	if err := tuner.setLNAGainManual(0x0a); err != nil {
		t.Fatalf("setLNAGainManual: %v", err)
	}

	// Manual mode bit set, gain code = 0x0a.
	want := maskR860LNAGainMode | uint8(0x0a)
	mask := maskR860LNAGainMode | maskR860GainCode

	expectMaskedWrite(t, bus, regR860LNAGain, want, mask, prior)

	if got := tuner.shadow[regR860LNAGain] & maskR860GainCode; got != 0x0a {
		t.Errorf("shadow gain bits = %#x, want 0x0a", got)
	}

	if got := tuner.shadow[regR860LNAGain] & maskR860LNAGainMode; got == 0 {
		t.Error("shadow manual-mode bit not set")
	}
}

func TestSetLNAGainManualRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.setLNAGainManual(r860GainStepCount); !errors.Is(err, ErrR860GainStepRange) {
		t.Errorf("err = %v, want wrapping ErrR860GainStepRange", err)
	}
}

func TestSetLNAGainAutoClearsModeBit(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	// Force manual first so the mode bit is high.
	if err := tuner.setLNAGainManual(0x0f); err != nil {
		t.Fatalf("setLNAGainManual: %v", err)
	}

	bus.ops = nil
	prior := tuner.shadow[regR860LNAGain]

	if err := tuner.setLNAGainAuto(); err != nil {
		t.Fatalf("setLNAGainAuto: %v", err)
	}

	expectMaskedWrite(t, bus, regR860LNAGain, 0, maskR860LNAGainMode, prior)

	if got := tuner.shadow[regR860LNAGain] & maskR860LNAGainMode; got != 0 {
		t.Errorf("shadow mode bit = %#x, want 0 (auto)", got)
	}
}

func TestSetMixerGainManualClearsModeBitInverse(t *testing.T) {
	t.Parallel()

	// Mixer's MIXGAIN_MODE has inverse polarity to LNA: 0 = manual.
	tuner, bus := gainTunerOnFakeBus(t)
	prior := tuner.shadow[regR860MixerGain]

	if err := tuner.setMixerGainManual(0x07); err != nil {
		t.Fatalf("setMixerGainManual: %v", err)
	}

	want := uint8(0x07) // mode bit cleared, gain code 0x07
	mask := maskR860MixerGainMode | maskR860GainCode

	expectMaskedWrite(t, bus, regR860MixerGain, want, mask, prior)

	if got := tuner.shadow[regR860MixerGain] & maskR860MixerGainMode; got != 0 {
		t.Errorf("shadow mode bit = %#x, want 0 (manual)", got)
	}
}

func TestSetMixerGainManualRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.setMixerGainManual(99); !errors.Is(err, ErrR860GainStepRange) {
		t.Errorf("err = %v, want wrapping ErrR860GainStepRange", err)
	}
}

func TestSetMixerGainAutoSetsModeBit(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.setMixerGainManual(0x05); err != nil {
		t.Fatalf("setMixerGainManual: %v", err)
	}

	bus.ops = nil
	prior := tuner.shadow[regR860MixerGain]

	if err := tuner.setMixerGainAuto(); err != nil {
		t.Fatalf("setMixerGainAuto: %v", err)
	}

	expectMaskedWrite(t, bus, regR860MixerGain, maskR860MixerGainMode, maskR860MixerGainMode, prior)
}

func TestSetVGAGainManualClearsModeBit(t *testing.T) {
	t.Parallel()

	// VGA_MODE polarity: 0 = I²C VGA_CODE controls (manual),
	// 1 = IF_AGC pin controls (delegated to demod's DAGC).
	tuner, bus := gainTunerOnFakeBus(t)
	prior := tuner.shadow[regR860VGAGain]

	if err := tuner.setVGAGainManual(0x0c); err != nil {
		t.Fatalf("setVGAGainManual: %v", err)
	}

	want := uint8(0x0c) // mode bit cleared, code 0x0c
	mask := maskR860VGAMode | maskR860GainCode

	expectMaskedWrite(t, bus, regR860VGAGain, want, mask, prior)
}

func TestSetVGAGainManualRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.setVGAGainManual(20); !errors.Is(err, ErrR860GainStepRange) {
		t.Errorf("err = %v, want wrapping ErrR860GainStepRange", err)
	}
}

// TestSetVGAGainAutoMatchesLibrtlsdr verifies the AGC branch
// writes 0x0b in mask 0x9f — the exact pattern librtlsdr's
// r82xx_set_gain emits for "auto" gain mode. Bit 4 cleared puts
// the VGA on its register-driven gain path; the 0x0b code is the
// mid-band entry point the LNA+Mixer AGC loops ride above.
func TestSetVGAGainAutoMatchesLibrtlsdr(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.setVGAGainManual(0x05); err != nil {
		t.Fatalf("setVGAGainManual: %v", err)
	}

	bus.ops = nil
	prior := tuner.shadow[regR860VGAGain]

	if err := tuner.setVGAGainAuto(); err != nil {
		t.Fatalf("setVGAGainAuto: %v", err)
	}

	const (
		wantVal  uint8 = 0x0b
		wantMask uint8 = 0x9f
	)

	expectMaskedWrite(t, bus, regR860VGAGain, wantVal, wantMask, prior)
}

func TestSetGainHelpersWrapBusErrors(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	bus.writeErr = errFakeControlOut

	cases := []struct {
		name string
		call func() error
	}{
		{"LNAManual", func() error { return tuner.setLNAGainManual(0) }},
		{"LNAAuto", func() error { return tuner.setLNAGainAuto() }},
		{"MixerManual", func() error { return tuner.setMixerGainManual(0) }},
		{"MixerAuto", func() error { return tuner.setMixerGainAuto() }},
		{"VGAManual", func() error { return tuner.setVGAGainManual(0) }},
		{"VGAAuto", func() error { return tuner.setVGAGainAuto() }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := tc.call(); !errors.Is(err, errFakeControlOut) {
				t.Errorf("err = %v, want wrapping errFakeControlOut", err)
			}
		})
	}
}

func TestSetLNAGainPublicAuto(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.SetLNAGain(AutoGain); err != nil {
		t.Fatalf("SetLNAGain(AutoGain): %v", err)
	}

	// Public method wraps with withRepeater (enable + ... + disable).
	if first := bus.ops[0].kind; first != opEnable {
		t.Errorf("ops[0] = %q, want %q", first, opEnable)
	}

	if last := bus.ops[len(bus.ops)-1].kind; last != opDisable {
		t.Errorf("ops[last] = %q, want %q", last, opDisable)
	}
}

func TestSetLNAGainPublicManual(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.SetLNAGain(ManualGainStep(7)); err != nil {
		t.Fatalf("SetLNAGain manual: %v", err)
	}

	// Manual mode bit set in shadow.
	if got := tuner.shadow[regR860LNAGain] & maskR860LNAGainMode; got == 0 {
		t.Error("shadow LNA mode bit not set")
	}

	if got := tuner.shadow[regR860LNAGain] & maskR860GainCode; got != 7 {
		t.Errorf("shadow LNA gain bits = %#x, want 7", got)
	}

	if len(bus.ops) < 3 {
		t.Errorf("ops count = %d, want at least 3 (enable + write + disable)", len(bus.ops))
	}
}

func TestSetMixerGainPublic(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.SetMixerGain(ManualGainStep(11)); err != nil {
		t.Fatalf("SetMixerGain: %v", err)
	}

	if err := tuner.SetMixerGain(AutoGain); err != nil {
		t.Fatalf("SetMixerGain(AutoGain): %v", err)
	}
}

func TestSetVGAGainPublic(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.SetVGAGain(VGAStepForCentiDB(2000)); err != nil {
		t.Fatalf("SetVGAGain: %v", err)
	}

	if err := tuner.SetVGAGain(AutoGain); err != nil {
		t.Fatalf("SetVGAGain(AutoGain): %v", err)
	}
}

func TestManualGainStepClampsHigh(t *testing.T) {
	t.Parallel()

	if got := ManualGainStep(99); got.Step != 15 {
		t.Errorf("ManualGainStep(99) = %+v, want Step=15", got)
	}
}

func TestVGAStepForCentiDBClampsBelowMin(t *testing.T) {
	t.Parallel()

	got := VGAStepForCentiDB(-9999)
	if got.Step != 0 {
		t.Errorf("VGAStepForCentiDB(-9999) = %+v, want Step=0", got)
	}
}

func TestVGAStepForCentiDBClampsAboveMax(t *testing.T) {
	t.Parallel()

	got := VGAStepForCentiDB(10_000)
	if got.Step != 15 {
		t.Errorf("VGAStepForCentiDB(10_000) = %+v, want Step=15", got)
	}
}

func TestLibrtlsdrGainStepsSaturatesAtMax(t *testing.T) {
	t.Parallel()

	// Above the cumulative table maximum; both indices should pin at 15.
	lna, mixer := librtlsdrGainSteps(99_999)
	if lna != 15 || mixer != 15 {
		t.Errorf("librtlsdrGainSteps(99999) = (%d, %d), want (15, 15)", lna, mixer)
	}
}

func TestLibrtlsdrGainStepsZeroTargetReturnsZero(t *testing.T) {
	t.Parallel()

	// A target of 0 means "no manual gain" — first iteration's
	// total >= target check exits immediately.
	lna, mixer := librtlsdrGainSteps(0)
	if lna != 0 || mixer != 0 {
		t.Errorf("librtlsdrGainSteps(0) = (%d, %d), want (0, 0)", lna, mixer)
	}
}

func TestVGAGainCentiForStep(t *testing.T) {
	t.Parallel()

	cases := []struct {
		step uint8
		want int
	}{
		{0, -1200},           // 0000b → -12.00 dB
		{1, -1200 + 350},     // 0001b → -8.50 dB
		{8, -1200 + 8*350},   // 1000b → +16.00 dB
		{15, -1200 + 15*350}, // 1111b → +40.50 dB
	}

	for _, tc := range cases {
		if got := vgaGainCentiForStep(tc.step); got != tc.want {
			t.Errorf("step=%d: centi-dB = %d, want %d", tc.step, got, tc.want)
		}
	}
}
