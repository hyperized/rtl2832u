package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

func TestBandRangeForFreq(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rfHz     uint32
		wantBand r860FreqRange
	}{
		{
			name:     "below table — clamps to first row",
			rfHz:     10_000_000, // 10 MHz, < first row's 0 MHz boundary still picks row 0
			wantBand: r860FreqRanges[0],
		},
		{
			name:     "exactly 50 MHz lands in 50-row",
			rfHz:     50_000_000,
			wantBand: r860FreqRanges[1],
		},
		{
			name:     "75 MHz transitions open_d to 0 (row 6)",
			rfHz:     75_000_000,
			wantBand: r860FreqRanges[6],
		},
		{
			name:     "1090 MHz lands in highest row (588 MHz+)",
			rfHz:     1_090_000_000,
			wantBand: r860FreqRanges[len(r860FreqRanges)-1],
		},
		{
			name:     "above 588 MHz still uses last row",
			rfHz:     1_700_000_000,
			wantBand: r860FreqRanges[len(r860FreqRanges)-1],
		},
		{
			name:     "rounds-down sub-MHz: 49.999 MHz lands in row 0",
			rfHz:     49_999_999,
			wantBand: r860FreqRanges[0],
		},
	}

	for _, band := range tests {
		t.Run(band.name, func(t *testing.T) {
			t.Parallel()

			got := bandRangeForFreq(band.rfHz)
			if got != band.wantBand {
				t.Errorf("bandRangeForFreq(%d) = %+v, want %+v", band.rfHz, got, band.wantBand)
			}
		})
	}
}

func TestSetMux1090MHzWires(t *testing.T) {
	t.Parallel()

	// setMux requires the repeater open; tests of setMux in isolation
	// open it themselves (rather than via SetFreq's withRepeater).
	tuner, bus := newR860ForSetFreq(t)

	if err := bus.enableI2CRepeater(); err != nil {
		t.Fatalf("enableI2CRepeater: %v", err)
	}

	if err := tuner.setMux(1_090_000_000); err != nil {
		t.Fatalf("setMux: %v", err)
	}

	if err := bus.disableI2CRepeater(); err != nil {
		t.Fatalf("disableI2CRepeater: %v", err)
	}

	// Band 588+: open_d=0, rf_mux_ploy=0x40, tf_c=0.
	want := []fakeI2COp{
		{kind: opEnable},
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860OpenDrain, 0x30}},    // mask 0x08, value 0
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860RFMux, 0x60}},        // mask 0xc3, value 0x40
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860TrackingFilt, 0x00}}, // full byte
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860XtalCap, 0x64}},      // mask 0x0b, value 0
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860IMRMem1, 0xc0}},      // mask 0x3f, value 0
		{kind: opWrite, addr: r860I2CAddr, data: []byte{regR860IMRMem2, 0x40}},      // mask 0x3f, value 0
		{kind: opDisable},
	}

	if !reflect.DeepEqual(bus.ops, want) {
		t.Errorf("ops:\n got %#v\nwant %#v", bus.ops, want)
	}
}

func TestSetMux50MHzPicksLowerBand(t *testing.T) {
	t.Parallel()

	// 50 MHz lands in row 1 (open_d=0x08, rf_mux_ploy=0x02, tf_c=0xbe).
	// Verify the tracking-filter byte changes with band, not just
	// 1090 MHz's all-zeros case.
	tuner, bus := newR860ForSetFreq(t)

	if err := bus.enableI2CRepeater(); err != nil {
		t.Fatalf("enableI2CRepeater: %v", err)
	}

	if err := tuner.setMux(50_000_000); err != nil {
		t.Fatalf("setMux: %v", err)
	}

	// The third write (after enable, openDrain, rfMux) is the
	// full-byte tracking-filter write. For 50 MHz it should be 0xbe.
	const tfWriteIndex = 3 // enable + 2 masked writes before tracking-filt

	if got := bus.ops[tfWriteIndex]; got.kind != opWrite || len(got.data) != 2 {
		t.Fatalf("ops[%d] = %#v, want a 2-byte write", tfWriteIndex, got)
	}

	const wantTF uint8 = 0xbe
	if got := bus.ops[tfWriteIndex].data[1]; got != wantTF {
		t.Errorf("tracking-filter byte = %#x, want %#x for 50 MHz band", got, wantTF)
	}
}

func TestSetMuxWrapsControllerError(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForSetFreq(t)

	if err := bus.enableI2CRepeater(); err != nil {
		t.Fatalf("enableI2CRepeater: %v", err)
	}

	bus.writeErr = errFakeControlOut

	if err := tuner.setMux(1_090_000_000); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestSetMuxFailsAtEachStage walks through setMux's six masked
// writes one at a time, injecting an I2C failure after N
// successes. Each value of N exercises the error wrap of a
// different write — covering branches that the single
// "everything fails" test does not.
func TestSetMuxFailsAtEachStage(t *testing.T) {
	t.Parallel()

	const setMuxWrites = 6

	for stage := range setMuxWrites {
		t.Run("after_"+string(rune('0'+stage)), func(t *testing.T) {
			t.Parallel()

			tuner, bus := newR860ForSetFreq(t)

			if err := bus.enableI2CRepeater(); err != nil {
				t.Fatalf("enableI2CRepeater: %v", err)
			}

			bus.failWritesEnabled = true
			bus.failWritesAfter = stage
			bus.writeCount = 0 // reset; the enableI2CRepeater above
			// did not flow through this fake's i2cWrite, so the
			// counter is already zero, but pinning to zero makes
			// the test independent of any helper that bumps it.

			if err := tuner.setMux(1_090_000_000); !errors.Is(err, errFakeControlOut) {
				t.Errorf("stage %d: err = %v, want wrapping errFakeControlOut", stage, err)
			}
		})
	}
}
