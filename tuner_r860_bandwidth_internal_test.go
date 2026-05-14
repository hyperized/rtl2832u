package rtl2832u

import (
	"errors"
	"slices"
	"testing"
)

// newR860ForBandwidth constructs a tuner that has already passed
// detect+init, with the chip's I2C repeater pre-opened so the
// bandwidth call (which expects an open bridge per the doc
// contract) can run without going through withRepeater. Returns
// the tuner and the underlying fake bus with its ops slice
// reset to a clean timeline.
func newR860ForBandwidth(t *testing.T) (*R860, *fakeI2C) {
	t.Helper()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	if err := bus.enableI2CRepeater(); err != nil {
		t.Fatalf("enableI2CRepeater: %v", err)
	}

	// Reset op timeline so the bandwidth-write assertions see a
	// clean slate. The repeater is now open and SetBandwidthForSampleRate's
	// writeRegisterMasked calls will land directly on the bus.
	bus.ops = nil

	return tuner, bus
}

// findRegisterWrite scans bus.ops for the most recent write to reg
// and returns the value byte, or false if no such write exists.
// Useful because writeRegisterMasked emits a write with the
// 2-byte payload [reg, computed].
func findRegisterWrite(bus *fakeI2C, reg uint8) (uint8, bool) {
	for _, op := range slices.Backward(bus.ops) {
		if op.kind == opWrite && len(op.data) == 2 && op.data[0] == reg {
			return op.data[1], true
		}
	}

	return 0, false
}

// bwCase captures one row in the bandwidth-sweep table. wantReg0a
// and wantReg0b are the pre-mask "value" arguments the function
// hands to writeRegisterMasked — the test reconstructs the
// expected wire byte from initShadow + mask + value below.
type bwCase struct {
	name      string
	bwHz      uint32
	wantReg0a uint8
	wantReg0b uint8
	wantIF    uint32
}

// expectedRegWrite mirrors writeRegisterMasked's read-modify-write:
// the byte that lands on the wire is (shadow &^ mask) | (val & mask).
func expectedRegWrite(shadow, mask, val uint8) uint8 {
	return (shadow &^ mask) | (val & mask)
}

// assertBandwidthCase runs one bwCase against a fresh tuner and
// fails the test if any of: returned intFreq, mirrored intFreqHz,
// or the wire bytes for R0x0a / R0x0b diverge from expectations.
// Extracted from the table loop to keep cognitive complexity below
// revive's threshold.
func assertBandwidthCase(t *testing.T, testCase bwCase) {
	t.Helper()

	// Initial-shadow values after applyPostInit (see
	// expectedR860ShadowAfterInit). The wire byte is the
	// read-modify-write of these against the test's mask + value.
	const (
		initShadow0a uint8 = 0xd0
		mask0a       uint8 = 0x10
		initShadow0b uint8 = 0x6b
		mask0b       uint8 = 0xef
	)

	tuner, bus := newR860ForBandwidth(t)

	gotIF, err := tuner.SetBandwidthForSampleRate(testCase.bwHz)
	if err != nil {
		t.Fatalf("SetBandwidthForSampleRate: %v", err)
	}

	if gotIF != testCase.wantIF {
		t.Errorf("intFreq = %d, want %d", gotIF, testCase.wantIF)
	}

	if tuner.intFreqHz != testCase.wantIF {
		t.Errorf("tuner.intFreqHz = %d, want %d (must mirror return value)",
			tuner.intFreqHz, testCase.wantIF)
	}

	gotByte0a, found := findRegisterWrite(bus, 0x0a)
	if !found {
		t.Fatalf("no write to R0x0a in bus ops: %#v", bus.ops)
	}

	if want := expectedRegWrite(initShadow0a, mask0a, testCase.wantReg0a); gotByte0a != want {
		t.Errorf("R0x0a wire byte = %#02x, want %#02x (reg0a value %#02x masked into shadow %#02x)",
			gotByte0a, want, testCase.wantReg0a, initShadow0a)
	}

	gotByte0b, found := findRegisterWrite(bus, 0x0b)
	if !found {
		t.Fatalf("no write to R0x0b in bus ops: %#v", bus.ops)
	}

	if want := expectedRegWrite(initShadow0b, mask0b, testCase.wantReg0b); gotByte0b != want {
		t.Errorf("R0x0b wire byte = %#02x, want %#02x (reg0b value %#02x masked into shadow %#02x)",
			gotByte0b, want, testCase.wantReg0b, initShadow0b)
	}
}

// TestSetBandwidthForSampleRateCases pins (bwHz, reg0a, reg0b,
// intFreqHz) tuples for every reachable branch of
// r82xx_set_bandwidth as ported. Tuple values are derived by
// walking the function logic by hand:
//
//   - case > 7 MHz:                wide-band 8 MHz analogue path.
//   - case > 6 MHz:                wide-band 7 MHz analogue path.
//   - case > table[0]+hp1+hp2:     fixed 3.57 MHz IF, 6 MHz filter.
//   - default + first/second true: full HP1+HP2 subtraction, LP index 0.
//   - default + first true, second false: HP2 only, second else sets bit 0x40.
//   - default + first false, second true: HP1 only, first else sets bit 0x20.
//   - default + both false:        narrow path, both 0x20|0x40 OR-ed in.
//
// Each case asserts on the masked R0x0a / R0x0b values that landed
// on the bus and on the intFreqHz the tuner returns + stashes in
// its shadow.
func TestSetBandwidthForSampleRateCases(t *testing.T) {
	t.Parallel()

	cases := []bwCase{
		{
			name:      "above 7 MHz",
			bwHz:      8_000_000,
			wantReg0a: 0x10,
			wantReg0b: 0x0b,
			wantIF:    4_570_000,
		},
		{
			name:      "above 6 MHz",
			bwHz:      6_500_000,
			wantReg0a: 0x10,
			wantReg0b: 0x2a,
			wantIF:    4_570_000,
		},
		{
			name:      "above table[0]+hp1+hp2",
			bwHz:      3_000_000,
			wantReg0a: 0x10,
			wantReg0b: 0x6b,
			wantIF:    3_570_000,
		},
		{
			// bw = 2_400_000 → first-if true (>2_050_000), second-if
			// true (1_670_000>... wait bw=2_400_000-380_000=2_020_000,
			// then second-if 2_020_000>1_700_000 true → bw=1_670_000,
			// intFreq=3_030_000, realBw=730_000. Loop: 1_670_000 > 1_600_000
			// (table[1]) → break at idx=1, idx-- = 0. reg0b = 0x80 | 15 = 0x8f.
			// realBw=730_000+1_700_000=2_430_000. intFreq=3_030_000-1_215_000=1_815_000.
			name:      "default both-true",
			bwHz:      2_400_000,
			wantReg0a: 0x00,
			wantReg0b: 0x8f,
			wantIF:    1_815_000,
		},
		{
			// bw = 2_080_000 → first-if true: bw=1_700_000,
			// intFreq=2_680_000, realBw=380_000. Second-if 1_700_000>1_700_000
			// false → reg0b |= 0x40 → 0xc0. Loop: idx=1 trips (1_700_000>1_600_000),
			// break, idx-- = 0. reg0b |= (15-0)=15 → 0xcf.
			// realBw=380_000+1_700_000=2_080_000. intFreq=2_680_000-1_040_000=1_640_000.
			name:      "default first-true second-false",
			bwHz:      2_080_000,
			wantReg0a: 0x00,
			wantReg0b: 0xcf,
			wantIF:    1_640_000,
		},
		{
			// bw = 1_800_000 → first-if false: reg0b |= 0x20 → 0xa0.
			// Second-if 1_800_000>1_700_000 true: bw=1_450_000,
			// intFreq=2_650_000, realBw=350_000. Loop: 1_450_000 > 1_200_000
			// (table[4]) → break at idx=4, idx-- = 3. table[3]=1_450_000.
			// Wait 1_450_000 not > 1_450_000 so idx=4 first hits >1_200_000.
			// reg0b |= (15-3)=12 → 0xac. realBw=350_000+1_450_000=1_800_000.
			// intFreq=2_650_000-900_000=1_750_000.
			name:      "default first-false second-true",
			bwHz:      1_800_000,
			wantReg0a: 0x00,
			wantReg0b: 0xac,
			wantIF:    1_750_000,
		},
		{
			// bw = 500_000 → first-if false: reg0b |= 0x20 → 0xa0.
			// Second-if false: reg0b |= 0x40 → 0xe0. Loop walks until
			// idx=8 (table[8]=450_000): 500_000 > 450_000 → break,
			// idx-- = 7. table[7]=550_000.
			// reg0b |= (15-7)=8 → 0xe8. realBw=550_000.
			// intFreq=2_300_000-275_000=2_025_000.
			name:      "default both-false",
			bwHz:      500_000,
			wantReg0a: 0x00,
			wantReg0b: 0xe8,
			wantIF:    2_025_000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertBandwidthCase(t, tc)
		})
	}
}

// TestSetBandwidthForSampleRateR0x0aWriteFails covers the first
// writeRegisterMasked error wrap. Failing the first write produces
// "set bandwidth R0x0a (...): %w" wrapping errFakeControlOut.
func TestSetBandwidthForSampleRateR0x0aWriteFails(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForBandwidth(t)

	bus.failWritesEnabled = true
	bus.failWritesAfter = 0 // first write fails
	bus.writeCount = 0

	if _, err := tuner.SetBandwidthForSampleRate(2_400_000); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestSetBandwidthForSampleRateR0x0bWriteFails covers the second
// writeRegisterMasked error wrap. R0x0a succeeds (one write), R0x0b
// fails on the second.
func TestSetBandwidthForSampleRateR0x0bWriteFails(t *testing.T) {
	t.Parallel()

	tuner, bus := newR860ForBandwidth(t)

	bus.failWritesEnabled = true
	bus.failWritesAfter = 1 // first write OK, second (R0x0b) fails
	bus.writeCount = 0

	if _, err := tuner.SetBandwidthForSampleRate(2_400_000); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestInitializeForSampleRateBracketsRepeater verifies the public
// orchestrator entry point: it opens the I²C bridge, drives
// SetBandwidthForSampleRate, and closes the bridge — and returns
// the same intFreq the inner primitive computed for the rate.
func TestInitializeForSampleRateBracketsRepeater(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	// Reset op timeline; we want to see the open/close pair the
	// orchestrator emits independently of the detect+init brackets.
	bus.ops = nil

	const rateHz uint32 = 2_400_000

	intFreq, err := tuner.InitializeForSampleRate(rateHz)
	if err != nil {
		t.Fatalf("InitializeForSampleRate: %v", err)
	}

	// Same trajectory the bw=2_400_000 case in
	// TestSetBandwidthForSampleRateCases pins.
	const wantIntFreq uint32 = 1_815_000
	if intFreq != wantIntFreq {
		t.Errorf("intFreq = %d, want %d", intFreq, wantIntFreq)
	}

	if len(bus.ops) < 2 {
		t.Fatalf("ops = %#v, want at least open + close", bus.ops)
	}

	if bus.ops[0].kind != opEnable {
		t.Errorf("first op = %q, want %q (repeater open missing)", bus.ops[0].kind, opEnable)
	}

	if last := bus.ops[len(bus.ops)-1]; last.kind != opDisable {
		t.Errorf("last op = %q, want %q (repeater close missing)", last.kind, opDisable)
	}
}

// TestInitializeForSampleRatePropagatesError covers the err != nil
// branch in InitializeForSampleRate: failing the I²C bridge open
// surfaces the error rather than the zero intFreq.
func TestInitializeForSampleRatePropagatesError(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	bus.enableErr = errFakeControlOut

	intFreq, err := tuner.InitializeForSampleRate(2_400_000)
	if !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}

	if intFreq != 0 {
		t.Errorf("intFreq = %d, want 0 on error path", intFreq)
	}
}
