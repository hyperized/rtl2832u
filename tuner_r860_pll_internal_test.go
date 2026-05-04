package rtl2832u

import (
	"errors"
	"testing"
)

// xtalHzDefault is the reference clock for every test in this file.
// 28.8 MHz matches both the chip-level constant and the dongles
// targeted by this project.
const xtalHzDefault uint32 = 28_800_000

// TestComputeSDM1090MHz hand-traces the iterative SDM algorithm
// for the residual fraction left when locking to 1090 MHz with a
// 28.8 MHz reference.
//
// Trace:
//
//	vco_fra = 2_180_000_000 - 57_600_000 * 37 = 48_800_000 Hz = 48_800 kHz
//	2*xtal in kHz = 57_600
//
//	  nSDM step  | step= 2*xtal/nSDM | residual decision  | sdm += 65536/nSDM
//	  -----------+-------------------+--------------------+-------------------
//	    2 (28800)| 28800             | 48800 > 28800: take| 32768  -> 32768
//	    4 (14400)| 14400             | 20000 > 14400: take| 16384  -> 49152
//	    8 (7200) |  7200             |  5600 < 7200: skip |   -    -> 49152
//	   16 (3600) |  3600             |  5600 > 3600: take |  4096  -> 53248
//	   32 (1800) |  1800             |  2000 > 1800: take |  2048  -> 55296
//	   64 (900)  |   900             |   200 < 900: skip  |   -    -> 55296
//	  128 (450)  |   450             |   200 < 450: skip  |   -    -> 55296
//	  256 (225)  |   225             |   200 < 225: skip  |   -    -> 55296
//	  512 (112)  |   112             |   200 > 112: take  |   128  -> 55424
//	 1024 (56)   |    56             |    88 > 56: take   |    64  -> 55488
//	 2048 (28)   |    28             |    32 > 28: take   |    32  -> 55520
//	 4096 (14)   |    14             |     4 < 14: skip   |   -    -> 55520
//	 8192 (7)    |     7             |     4 < 7: skip    |   -    -> 55520
//	16384 (3)    |     3             |     4 > 3: take    |     4  -> 55524
//	32768 (1)    |     1             |     1 not > 1: exit|   -    -> 55524
//
// Final sdm = 55524 = 0xD8E4.
func TestComputeSDM1090MHz(t *testing.T) {
	t.Parallel()

	const want uint16 = 0xD8E4

	got := computeSDM(48_800, 57_600)
	if got != want {
		t.Errorf("computeSDM(48800, 57600) = %#x, want %#x", got, want)
	}
}

func TestComputeSDMZeroFractional(t *testing.T) {
	t.Parallel()

	// Integer-N tuning point: zero fractional residual yields sdm=0,
	// which the orchestrator translates into the pwSDM bit.
	if got := computeSDM(0, 57_600); got != 0 {
		t.Errorf("computeSDM(0, 57600) = %#x, want 0", got)
	}
}

func TestPickMixDivLands1090MHzAtMixDiv2(t *testing.T) {
	t.Parallel()

	mixDiv, divNum, ok := pickMixDiv(1_090_000_000)
	if !ok {
		t.Fatal("pickMixDiv(1.09 GHz) failed; should land at mixDiv=2")
	}

	if mixDiv != 2 {
		t.Errorf("mixDiv = %d, want 2 (1.09 GHz × 2 = 2.18 GHz, in [1.77, 3.54] GHz)", mixDiv)
	}

	if divNum != 0 {
		t.Errorf("divNum = %d, want 0 (log2(2) - 1)", divNum)
	}
}

func TestPickMixDivCoversFullRange(t *testing.T) {
	t.Parallel()

	// One representative frequency per mixDiv tier. Each is chosen
	// to land mid-band so VCO is well clear of the [1.77, 3.54] GHz
	// boundaries.
	tests := []struct {
		name       string
		rfHz       uint32
		wantMixDiv uint8
		wantDivNum uint8
	}{
		{"1.5 GHz", 1_500_000_000, 2, 0},
		{"700 MHz", 700_000_000, 4, 1},
		{"300 MHz", 300_000_000, 8, 2},
		{"150 MHz", 150_000_000, 16, 3},
		{"75 MHz", 75_000_000, 32, 4},
		{"32 MHz", 32_000_000, 64, 5},
	}

	for _, band := range tests {
		t.Run(band.name, func(t *testing.T) {
			t.Parallel()

			mixDiv, divNum, ok := pickMixDiv(band.rfHz)
			if !ok {
				t.Fatalf("pickMixDiv(%d) failed", band.rfHz)
			}

			if mixDiv != band.wantMixDiv {
				t.Errorf("mixDiv = %d, want %d", mixDiv, band.wantMixDiv)
			}

			if divNum != band.wantDivNum {
				t.Errorf("divNum = %d, want %d", divNum, band.wantDivNum)
			}
		})
	}
}

func TestPickMixDivRejectsTooHigh(t *testing.T) {
	t.Parallel()

	// Above 1.77 GHz the lowest mixDiv (2) drives VCO ≥ 3.54 GHz,
	// which exceeds the lock range. No higher mixDiv helps.
	if _, _, ok := pickMixDiv(2_000_000_000); ok {
		t.Error("pickMixDiv(2 GHz) succeeded; expected failure (VCO out of range)")
	}
}

func TestPickMixDivRejectsTooLow(t *testing.T) {
	t.Parallel()

	// Below ~28 MHz, even mixDiv=64 leaves VCO under 1.77 GHz.
	if _, _, ok := pickMixDiv(20_000_000); ok {
		t.Error("pickMixDiv(20 MHz) succeeded; expected failure (VCO under min)")
	}
}

func TestComputePLLSettings1090MHzNominal(t *testing.T) {
	t.Parallel()

	settings, err := computePLLSettings(1_090_000_000, xtalHzDefault, r860VCOPowerRef)
	if err != nil {
		t.Fatalf("computePLLSettings: %v", err)
	}

	// Hand-derived for rfHz=1.09 GHz, xtal=28.8 MHz, vcoFineTune=2:
	//   vcoFreq = 2_180_000_000
	//   2*xtal  = 57_600_000
	//   nint    = 37
	//   ni      = (37 - 13) / 4 = 6
	//   si      = 37 - 24 - 13  = 0
	//   sdm     = 0xD8E4 (see TestComputeSDM1090MHz)
	want := pllSettings{
		mixDiv: 2,
		divNum: 0,
		nint:   37,
		ni:     6,
		si:     0,
		sdm:    0xD8E4,
	}

	if settings != want {
		t.Errorf("settings = %+v\nwant      %+v", settings, want)
	}
}

func TestComputePLLSettingsVCOFineTuneAdjustsDivNum(t *testing.T) {
	t.Parallel()

	// At mixDiv=4 (e.g. 700 MHz) the divNum starts at 1; a "hot"
	// vcoFineTune (>r860VCOPowerRef) should bump it down to 0.
	const rfHz uint32 = 700_000_000

	hot, err := computePLLSettings(rfHz, xtalHzDefault, r860VCOPowerRef+1)
	if err != nil {
		t.Fatalf("hot: %v", err)
	}

	cold, err := computePLLSettings(rfHz, xtalHzDefault, r860VCOPowerRef-1)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}

	nominal, err := computePLLSettings(rfHz, xtalHzDefault, r860VCOPowerRef)
	if err != nil {
		t.Fatalf("nominal: %v", err)
	}

	if hot.divNum+1 != nominal.divNum {
		t.Errorf("hot.divNum=%d nominal.divNum=%d; want hot = nominal-1",
			hot.divNum, nominal.divNum)
	}

	if cold.divNum != nominal.divNum+1 {
		t.Errorf("cold.divNum=%d nominal.divNum=%d; want cold = nominal+1",
			cold.divNum, nominal.divNum)
	}
}

// TestComputePLLSettingsAcceptsAlternateXtal exercises the function
// with a non-default crystal so xtalHz cannot be marked unparam.
// R828D dongles run a 16 MHz xtal; we only verify the call
// succeeds — derived register values are out of scope here.
func TestComputePLLSettingsAcceptsAlternateXtal(t *testing.T) {
	t.Parallel()

	if _, err := computePLLSettings(1_090_000_000, 16_000_000, r860VCOPowerRef); err != nil {
		t.Errorf("with 16 MHz xtal: %v", err)
	}
}

func TestComputePLLSettingsRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	// 3 GHz is way outside the R860 range; no mixDiv works.
	_, err := computePLLSettings(3_000_000_000, xtalHzDefault, r860VCOPowerRef)
	if !errors.Is(err, errR860FreqOutOfRange) {
		t.Errorf("err = %v, want wrapping errR860FreqOutOfRange", err)
	}
}

// TestComputePLLSettingsRejectsNintOutOfRange exercises the
// post-pickMixDiv nint range check. With a tiny synthetic xtal
// the integer-N value blows past r860NintMax even though the VCO
// itself sits inside [1.77, 3.54] GHz, so pickMixDiv succeeds and
// the second guard fires.
func TestComputePLLSettingsRejectsNintOutOfRange(t *testing.T) {
	t.Parallel()

	// 1.77 GHz / (2 * 1 MHz) = 885, far above r860NintMax (76).
	const tinyXtal uint32 = 1_000_000

	_, err := computePLLSettings(1_090_000_000, tinyXtal, r860VCOPowerRef)
	if !errors.Is(err, errR860FreqOutOfRange) {
		t.Errorf("err = %v, want wrapping errR860FreqOutOfRange (nint > max)", err)
	}
}

func TestComputePLLSettingsRejectsVCOFineTuneUnderflow(t *testing.T) {
	t.Parallel()

	// At 1.09 GHz, divNum lands at 0; a "hot" vcoFineTune would
	// decrement it below zero, which the function must reject
	// rather than silently wrap.
	_, err := computePLLSettings(1_090_000_000, xtalHzDefault, r860VCOPowerRef+1)
	if !errors.Is(err, errR860FreqOutOfRange) {
		t.Errorf("err = %v, want wrapping errR860FreqOutOfRange (divNum underflow)", err)
	}
}

// TestComputePLLSettingsRoundTrip checks the closed-loop accuracy:
// reconstructing the actual VCO frequency from the computed
// (mixDiv, nint, sdm) should be within one SDM step of the input.
//
// Equation (recovered RF):
//
//	vco_recovered = 2*xtal*nint + (sdm * 2*xtal) / 65536
//	rf_recovered  = vco_recovered / mixDiv
//
// The SDM truncation gives a quantisation error bounded by
// 2*xtal/65536 ≈ 879 Hz per VCO step, divided by mixDiv at the RF
// output. We allow a generous 2 kHz slack to absorb the integer
// arithmetic in computeSDM.
func TestComputePLLSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	const (
		rfHz = uint32(1_090_000_000)
	)

	settings, err := computePLLSettings(rfHz, xtalHzDefault, r860VCOPowerRef)
	if err != nil {
		t.Fatalf("computePLLSettings: %v", err)
	}

	twoXtal := uint64(xtalHzDefault) * 2
	vcoRec := twoXtal*uint64(settings.nint) +
		(uint64(settings.sdm)*twoXtal)/uint64(r860SDMResolution)

	rfRec := vcoRec / uint64(settings.mixDiv)

	const slackHz = 2_000

	// G115: both values are RF frequencies in the 10s of MHz to GHz
	// range, well under MaxInt64; the casts cannot overflow.
	delta := int64(rfRec) - int64(rfHz) //nolint:gosec
	if delta < -slackHz || delta > slackHz {
		t.Errorf("recovered rf = %d, want within %d of %d (delta = %d)",
			rfRec, slackHz, rfHz, delta)
	}
}
