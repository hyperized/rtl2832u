package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

// readVCOFineTuneOK returns the 5-byte response a passing
// SetFreq read needs. Wire byte at index 4 = 0x04, which
// bit-reverses to 0x20 — bits [5:4] = 0b10 = vcoFineTune of 2,
// matching r860VCOPowerRef so divNum is left as picked.
func readVCOFineTuneOK() []byte {
	return []byte{0x00, 0x00, 0x00, 0x00, 0x04}
}

// newR860ForSetFreq constructs a tuner that has already passed
// detect+init. The fake's read queue is seeded with the chip-ID
// response then a placeholder for SetFreq's own probe; the test
// drains both before asserting.
//
// intFreqHz is forced to 0 so SetFreq tests reason about the
// pure rfHz → LO mapping (Zero-IF). The real-IF offset added in
// production (rfHz + intFreqHz) is exercised separately in
// TestSetFreqHonoursIntFreqOffset; mixing both would force every
// PLL-arithmetic assertion through the offset, which obscures
// the math under test.
func newR860ForSetFreq(t *testing.T) (*R860, *fakeI2C) {
	t.Helper()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	tuner.intFreqHz = 0

	// Reset the captured ops so the SetFreq test sees a clean
	// timeline starting at the open-repeater call.
	bus.ops = nil

	return tuner, bus
}

func TestSetFreq1090MHzWires(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)

	// SetFreq does its own 5-byte read for vcoFineTune; queue the
	// passing response.
	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())

	if err := tuner.SetFreq(1_090_000_000); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}

	// Expected I2C op sequence. Repeater open, then setMux's six
	// front-end writes (band 588+ for 1090 MHz: open_d=0,
	// rf_mux_ploy=0x40, tf_c=0, plus xtal/IMR-mem clears), then
	// the PLL preamble, the 5-byte fineTune probe, the PLL
	// settings register-by-register, the autotune-fast
	// finalisation, repeater close.
	//
	// Shadow values come from r860InitValues at construction:
	//   0x08=0xc0, 0x09=0x40, 0x10=0x6c, 0x12=0x80, 0x14=0x0f,
	//   0x15=0x00, 0x16=0xc0, 0x17=0x30, 0x1a=0x60, 0x1b=0x00.
	// setMux mutates 0x10 (clear bits 0,1,3) and 0x1a; the PLL
	// pass then operates on the post-setMux shadow.
	want := []fakeI2COp{
		{kind: opEnable},
		// setMux writes (band 588+):
		// open_d=0, mask 0x08: shadow=0x30 → 0x30.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860OpenDrain, 0x30}},
		// rf_mux_ploy=0x40, mask 0xc3: shadow=0x60 → 0x60 & 0x3c | 0x40 = 0x60.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860RFMux, 0x60}},
		// tf_c=0, full byte.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860TrackingFilt, 0x00}},
		// xtal cap clear, mask 0x0b: shadow=0x6c → 0x6c & 0xf4 = 0x64.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860XtalCap, 0x64}},
		// IMR mem 1 clear, mask 0x3f: shadow=0xc0 → 0xc0.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860IMRMem1, 0xc0}},
		// IMR mem 2 clear, mask 0x3f: shadow=0x40 → 0x40.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860IMRMem2, 0x40}},
		// PLL preamble:
		// autotune slow: shadow=0x60, mask=0x0c, value=0 → 0x60.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860Autotune, 0x60}},
		// VCO current: shadow=0x80, mask=0xe0, value=0x80 → 0x80.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860VCOCurrent, 0x80}},
		// fineTune probe: write read-pointer, then 5-byte read.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860ProbeStart}},
		// The mock captures bytes BEFORE readRegisters bit-reverses
		// them in place, so opRead sees the wire form (0x04 at
		// index 4 → bitrevs to 0x20 → vcoFineTune = 2).
		{kind: opRead, addr: r860I2CAddr, data: []byte{0x00, 0x00, 0x00, 0x00, 0x04}},
		// divNum=0 << 5: shadow=0x64 (after setMux), mask=0xe0 → 0x04.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860DivNum, 0x04}},
		// ni=6, si=0 → full byte 0x06.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860NiSi, 0x06}},
		// pwSDM: sdm != 0 so value=0; shadow=0x80, mask=0x08 → 0x80.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860VCOCurrent, 0x80}},
		// SDM high = 0xD8 (full byte).
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860SDMHigh, 0xD8}},
		// SDM low = 0xE4 (full byte).
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860SDMLow, 0xE4}},
		// autotune fast: shadow=0x60, mask=0x08, value=0x08 → 0x68.
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860Autotune, 0x68}},
		{kind: opDisable},
	}

	if !reflect.DeepEqual(bus.ops, want) {
		t.Errorf("ops:\n got %#v\nwant %#v", bus.ops, want)
	}
}

func TestSetFreqIntegerNTunesPwSDMBit(t *testing.T) {
	t.Parallel()

	// At an integer-N tuning point (sdm==0) the pwSDM bit must be
	// SET, not cleared. Pick rfHz so the residual is zero: a
	// frequency where rfHz * mixDiv is an exact multiple of 2*xtal.
	//
	// 2 * 28.8 MHz = 57.6 MHz. With mixDiv=2, rfHz must be a
	// multiple of 28.8 MHz (so vco = rfHz * 2 = N * 57.6 MHz). The
	// VCO must also fall in [1.77, 3.54) GHz, so N ∈ [31, 61].
	// Pick N=37 → vco = 2.1312 GHz → rfHz = 1.0656 GHz.
	const rfHz uint32 = 1_065_600_000

	tuner, bus := newR860ForSetFreq(t)
	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())

	if err := tuner.SetFreq(rfHz); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}

	// Find the SECOND write to regR860VCOCurrent (the pwSDM toggle).
	// The first is the VCO-current preamble; the second carries the
	// pwSDM bit.
	var pwSDMByte uint8

	hits := 0

	for _, entry := range bus.ops {
		if entry.kind != opWrite || len(entry.data) != 2 || entry.data[0] != regR860VCOCurrent {
			continue
		}

		hits++
		if hits == 2 {
			pwSDMByte = entry.data[1]

			break
		}
	}

	if hits < 2 {
		t.Fatalf("expected two writes to regR860VCOCurrent, got %d", hits)
	}

	if pwSDMByte&maskR860PwSDM == 0 {
		t.Errorf("pwSDM toggle = %#x; bit %#x should be SET for integer-N tuning",
			pwSDMByte, maskR860PwSDM)
	}
}

func TestSetFreqRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)
	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())

	if err := tuner.SetFreq(3_000_000_000); !errors.Is(err, errR860FreqOutOfRange) {
		t.Errorf("err = %v, want wrapping errR860FreqOutOfRange", err)
	}

	// The repeater must still be closed — withRepeater's defer
	// fires on every error path.
	if last := bus.ops[len(bus.ops)-1]; last.kind != opDisable {
		t.Errorf("last op = %q, want %q (deferred close did not fire)", last.kind, opDisable)
	}
}

func TestSetFreqWrapsControllerError(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)
	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())
	bus.writeErr = errFakeControlOut

	if err := tuner.SetFreq(1_090_000_000); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestSetFreqVCOFineTuneReadFailure hits the readVCOFineTune
// error path: setMux + autotune-slow + VCO-current succeed; then
// the 5-byte fineTune probe fails on the read side.
func TestSetFreqVCOFineTuneReadFailure(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)
	bus.readErr = errFakeControlIn

	if err := tuner.SetFreq(1_090_000_000); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

// TestSetFreqRejectsRangeAfterReadingFineTune covers the
// out-of-range path AFTER vcoFineTune is read — distinct from
// the earlier rejection that errors before any I/O. With a
// successful probe and a too-high RF, computePLLSettings returns
// errR860FreqOutOfRange and SetFreq surfaces it.
func TestSetFreqRejectsRangeAfterReadingFineTune(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)
	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())

	if err := tuner.SetFreq(3_000_000_000); !errors.Is(err, errR860FreqOutOfRange) {
		t.Errorf("err = %v, want wrapping errR860FreqOutOfRange", err)
	}
}

// TestSetFreqInnerFailsAtEachStage exercises every error wrap in
// setFreqInner one at a time. Each stage value picks a different
// write to fail at:
//
//	0: autotune slow         (regR860Autotune)
//	1: VCO current           (regR860VCOCurrent)
//	2: read-pointer write    (the 0x00 byte before the 5-byte read)
//	3: divNum                (regR860DivNum)
//	4: ni|si                 (regR860NiSi)
//	5: pwSDM                 (regR860VCOCurrent again)
//	6: SDM high              (regR860SDMHigh)
//	7: SDM low               (regR860SDMLow)
//	8: autotune fast         (regR860Autotune)
//
// Calls setFreqInner directly so the setMux path doesn't consume
// the early stages.
func TestSetFreqInnerFailsAtEachStage(t *testing.T) {
	t.Parallel()

	const setFreqInnerWrites = 9

	for stage := range setFreqInnerWrites {
		t.Run("after_"+string(rune('0'+stage)), func(t *testing.T) {
			t.Parallel()

			tuner, bus := newR860ForSetFreq(t)

			if err := bus.enableI2CRepeater(); err != nil {
				t.Fatalf("enableI2CRepeater: %v", err)
			}

			bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())
			bus.failWritesEnabled = true
			bus.failWritesAfter = stage
			bus.writeCount = 0

			if err := tuner.setFreqInner(1_090_000_000); !errors.Is(err, errFakeControlOut) {
				t.Errorf("stage %d: err = %v, want wrapping errFakeControlOut", stage, err)
			}
		})
	}
}

// TestSetFreqInnerNintOutOfRange covers the
// computePLLSettings-error branch INSIDE setFreqInner: the
// preamble succeeds but the PLL math returns out-of-range. We
// trigger this with a too-high RF that still gets past setMux
// (which doesn't validate frequency).
func TestSetFreqInnerNintOutOfRange(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)

	if err := bus.enableI2CRepeater(); err != nil {
		t.Fatalf("enableI2CRepeater: %v", err)
	}

	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())

	if err := tuner.setFreqInner(3_000_000_000); !errors.Is(err, errR860FreqOutOfRange) {
		t.Errorf("err = %v, want wrapping errR860FreqOutOfRange", err)
	}
}

func TestSetFreqShadowReflectsWrites(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)
	bus.readResponses = append(bus.readResponses, readVCOFineTuneOK())

	if err := tuner.SetFreq(1_090_000_000); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}

	// Spot-check the shadow values match the new computed bytes,
	// confirming writeRegister and writeRegisterMasked maintain
	// the in-memory mirror.
	checks := []struct {
		reg  uint8
		want uint8
	}{
		// divNum's shadow is 0x04 after setMux clears 0x10's lower
		// bits (init 0x6c → 0x64) and the PLL pass leaves divNum=0
		// in bits [7:5] (0x64 & ~0xe0 = 0x04).
		{regR860DivNum, 0x04},
		{regR860NiSi, 0x06},
		{regR860SDMHigh, 0xD8},
		{regR860SDMLow, 0xE4},
		{regR860Autotune, 0x68},
	}

	for _, c := range checks {
		if got := tuner.shadow[c.reg]; got != c.want {
			t.Errorf("shadow[%#x] = %#x, want %#x", c.reg, got, c.want)
		}
	}
}

func TestR860BitRevRoundTrips(t *testing.T) {
	t.Parallel()

	for _, val := range []uint8{0x00, 0x69, 0x96, 0xff, 0x55, 0xAA, 0x12, 0x34} {
		if got := r860BitRev(r860BitRev(val)); got != val {
			t.Errorf("r860BitRev(r860BitRev(%#x)) = %#x, want %#x", val, got, val)
		}
	}
}

func TestR860BitRevKnownValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   uint8
		want uint8
	}{
		{"chipID 0x69", 0x69, 0x96},
		{"all bits set", 0xff, 0xff},
		{"all bits clear", 0x00, 0x00},
		{"low nibble", 0x0f, 0xf0},
		{"high nibble", 0xf0, 0x0f},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := r860BitRev(c.in); got != c.want {
				t.Errorf("r860BitRev(%#x) = %#x, want %#x", c.in, got, c.want)
			}
		})
	}
}
