package rtl2832u

import (
	"errors"
	"testing"
)

func TestSetIFBandwidthWritesBothRegisters(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	priorR10 := tuner.shadow[regR860Filter1]
	priorR11 := tuner.shadow[regR860Filter2]

	if err := tuner.applyIFBandwidth(0b10, 0b1010); err != nil {
		t.Fatalf("applyIFBandwidth: %v", err)
	}

	// Two writes: R11 (FILT_BW masked), R10 (FILT_CODE masked).
	writes := 0

	for _, busOp := range bus.ops {
		if busOp.kind == opWrite && busOp.addr == r860I2CAddr {
			writes++
		}
	}

	if writes != 2 {
		t.Errorf("write count = %d, want 2", writes)
	}

	// Verify shadow updates carry the masked-merged values.
	wantR10 := (priorR10 &^ maskR860FILTCode) | 0b1010
	if got := tuner.shadow[regR860Filter1]; got != wantR10 {
		t.Errorf("R10 shadow = %#x, want %#x", got, wantR10)
	}

	wantR11 := (priorR11 &^ maskR860FILTBW) | (0b10 << r860FILTBWShift)
	if got := tuner.shadow[regR860Filter2]; got != wantR11 {
		t.Errorf("R11 shadow = %#x, want %#x", got, wantR11)
	}
}

func TestSetIFBandwidthRejectsOutOfRangeCoarse(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.applyIFBandwidth(r860FILTBWCount, 0); !errors.Is(err, ErrR860FilterRange) {
		t.Errorf("err = %v, want wrapping ErrR860FilterRange", err)
	}
}

func TestSetIFBandwidthRejectsOutOfRangeFine(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.applyIFBandwidth(0, r860FilterStepCount); !errors.Is(err, ErrR860FilterRange) {
		t.Errorf("err = %v, want wrapping ErrR860FilterRange", err)
	}
}

func TestSetIFBandwidthBusErrors(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	bus.writeErr = errFakeControlOut

	if err := tuner.applyIFBandwidth(0, 0); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestSetIFBandwidthFineWriteFailureSurfaces(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	bus.failWritesEnabled = true
	bus.failWritesAfter = 1 // R11 (coarse) succeeds; R10 (fine) fails

	if err := tuner.applyIFBandwidth(0, 0); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut on fine write", err)
	}
}

func TestSetIFHighPass(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	prior := tuner.shadow[regR860Filter2]

	if err := tuner.applyIFHighPass(R860HPF1MHz); err != nil {
		t.Fatalf("applyIFHighPass: %v", err)
	}

	wantR11 := (prior &^ maskR860HPF) | R860HPF1MHz
	if got := tuner.shadow[regR860Filter2]; got != wantR11 {
		t.Errorf("R11 shadow = %#x, want %#x", got, wantR11)
	}

	writes := 0

	for _, busOp := range bus.ops {
		if busOp.kind == opWrite && busOp.addr == r860I2CAddr {
			writes++
		}
	}

	if writes != 1 {
		t.Errorf("write count = %d, want 1", writes)
	}
}

func TestSetIFHighPassRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	if err := tuner.applyIFHighPass(r860FilterStepCount); !errors.Is(err, ErrR860FilterRange) {
		t.Errorf("err = %v, want wrapping ErrR860FilterRange", err)
	}
}

func TestSetIFHighPassWrapsBusErr(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	bus.writeErr = errFakeControlOut

	if err := tuner.applyIFHighPass(0); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestSetFilterExtSetsBit(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)
	prior := tuner.shadow[regR860FilterExt]

	if err := tuner.applyFilterExt(true); err != nil {
		t.Fatalf("applyFilterExt(true): %v", err)
	}

	want := prior | maskR860FilterExt
	if got := tuner.shadow[regR860FilterExt]; got != want {
		t.Errorf("R30 shadow = %#x, want %#x", got, want)
	}
}

func TestSetFilterExtClearsBit(t *testing.T) {
	t.Parallel()

	tuner, _ := gainTunerOnFakeBus(t)

	// Force it on first.
	if err := tuner.applyFilterExt(true); err != nil {
		t.Fatalf("applyFilterExt(true): %v", err)
	}

	if err := tuner.applyFilterExt(false); err != nil {
		t.Fatalf("applyFilterExt(false): %v", err)
	}

	if got := tuner.shadow[regR860FilterExt] & maskR860FilterExt; got != 0 {
		t.Errorf("R30 FILTER_EXT bit = %#x, want 0", got)
	}
}

func TestSetFilterExtWrapsBusErr(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)
	bus.writeErr = errFakeControlOut

	if err := tuner.applyFilterExt(true); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// Public-method tests cover the withRepeater wrappers on R860,
// which the apply* internal-method tests above don't.

func TestPublicSetIFBandwidthOpensRepeater(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.SetIFBandwidth(0, 1); err != nil {
		t.Fatalf("SetIFBandwidth: %v", err)
	}

	if first, last := bus.ops[0].kind, bus.ops[len(bus.ops)-1].kind; first != opEnable || last != opDisable {
		t.Errorf("repeater bracket = %q ... %q, want %q ... %q",
			first, last, opEnable, opDisable)
	}
}

func TestPublicSetIFHighPassOpensRepeater(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.SetIFHighPass(R860HPF1MHz); err != nil {
		t.Fatalf("SetIFHighPass: %v", err)
	}

	if first, last := bus.ops[0].kind, bus.ops[len(bus.ops)-1].kind; first != opEnable || last != opDisable {
		t.Errorf("repeater bracket = %q ... %q, want %q ... %q",
			first, last, opEnable, opDisable)
	}
}

func TestPublicSetFilterExtOpensRepeater(t *testing.T) {
	t.Parallel()

	tuner, bus := gainTunerOnFakeBus(t)

	if err := tuner.SetFilterExt(true); err != nil {
		t.Fatalf("SetFilterExt: %v", err)
	}

	if first, last := bus.ops[0].kind, bus.ops[len(bus.ops)-1].kind; first != opEnable || last != opDisable {
		t.Errorf("repeater bracket = %q ... %q, want %q ... %q",
			first, last, opEnable, opDisable)
	}
}
