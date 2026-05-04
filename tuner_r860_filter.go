package rtl2832u

import (
	"errors"
	"fmt"
)

// R860 IF / channel filter
// ========================
//
// The R860 has a programmable channel filter sitting after the
// mixer and before the VGA. Its purpose is to band-limit the
// down-converted signal so the ADC and demod don't waste dynamic
// range on out-of-band noise. Per datasheet table 6-3:
//
//   R10 (0x0A) bit  [7]   PWD_FILT       0=filter off, 1=on
//   R10 (0x0A) bits [6:5] PW_FILT[1:0]   filter power: 00=highest power, 11=lowest
//   R10 (0x0A) bits [3:0] FILT_CODE[3:0] LPF fine tune: 0000=widest, 1111=narrowest
//   R11 (0x0B) bits [6:5] FILT_BW[1:0]   LPF coarse: 00=wide, 01/10=middle, 11=narrow
//   R11 (0x0B) bits [3:0] HPF[3:0]       high-pass corner; the datasheet documents
//                                          16 specific (corner, attenuation) tuples
//                                          ranging from 5 MHz down to 0.5 MHz
//   R30 (0x1E) bit  [6]   FILTER_EXT     filter extension for weak-signal conditions
//
// The public R860 datasheet lists the relative widths but no
// absolute Hz for the FILT_BW × FILT_CODE matrix; LNA / Mixer dB
// scales are similarly not in the public release. The HPF table
// is documented in absolute Hz, so we expose it via named
// constants.
//
// For Mode S at 2.4 MS/s the useful signal bandwidth is roughly
// 2 MHz; the librtlsdr default ships R10/R11 at FILT_BW=narrow,
// FILT_CODE=6 — which the chip's seed register table preserves.
// Narrowing FILT_CODE further (toward 1111) drops noise floor at
// the cost of cutting into the Mode S preamble's edges; that's an
// empirical trade-off best made by sweeping yield rather than
// asserting a value at compile time.

// IF filter register addresses, field masks, and HPF code
// mappings. Names mirror the datasheet column for cross-reference.
const (
	regR860Filter1   uint8 = 0x0a // R10: PWD_FILT, PW_FILT[1:0], FILT_CODE[3:0]
	regR860Filter2   uint8 = 0x0b // R11: FILT_BW[1:0], HPF[3:0]
	regR860FilterExt uint8 = 0x1e // R30: FILTER_EXT in bit [6]

	maskR860FILTBW    uint8 = 0x60   // R11 bits [6:5]
	maskR860FILTCode  uint8 = 0x0f   // R10 bits [3:0]
	maskR860HPF       uint8 = 0x0f   // R11 bits [3:0]
	maskR860FilterExt uint8 = 1 << 6 // R30 bit [6]

	// FILT_BW field shift: bits [6:5] in the register byte.
	r860FILTBWShift = 5

	// r860FilterStepCount is the cardinality of FILT_CODE and
	// HPF (each occupies 4 bits = 16 values).
	r860FilterStepCount uint8 = 16

	// r860FILTBWCount is the cardinality of FILT_BW (2 bits = 4
	// possible values, though the datasheet only assigns three:
	// 00=wide, 01/10=middle, 11=narrow).
	r860FILTBWCount uint8 = 4
)

// HPF code constants mirror the datasheet's documented corner
// frequencies. Names use the form `R860HPF<MHz>` for the
// canonical-value entries; the "<-N>dBat<F>MHz" entries cover the
// same corner with different attenuation profiles.
//
// The values are exposed publicly so callers can pass them to
// SetIFHighPass without re-encoding the datasheet table at every
// call site.
const (
	R860HPF5MHz          uint8 = 0b0000
	R860HPF4MHz          uint8 = 0b0001
	R860HPF12dBAt2_25MHz uint8 = 0b0010
	R860HPF8dBAt2_25MHz  uint8 = 0b0011
	R860HPF4dBAt2_25MHz  uint8 = 0b0100
	R860HPF12dBAt1_75MHz uint8 = 0b0101
	R860HPF8dBAt1_75MHz  uint8 = 0b0110
	R860HPF4dBAt1_75MHz  uint8 = 0b0111
	R860HPF12dBAt1_25MHz uint8 = 0b1000
	R860HPF8dBAt1_25MHz  uint8 = 0b1001
	R860HPF4dBAt1_25MHz  uint8 = 0b1010
	R860HPF1MHz          uint8 = 0b1011
	R860HPF800kHz        uint8 = 0b1100
	R860HPF700kHz        uint8 = 0b1101
	R860HPF600kHz        uint8 = 0b1110
	R860HPF500kHz        uint8 = 0b1111
)

// ErrR860FilterRange is the static sentinel for an out-of-range
// IF-filter step index.
var ErrR860FilterRange = errors.New("r860: IF filter step out of range")

// applyIFBandwidth pins the channel filter's coarse (FILT_BW) and
// fine (FILT_CODE) bandwidth. coarse must be in [0, 4); fine in
// [0, 16). A "0" coarse is the widest setting; 3 is the narrowest.
//
// Caller must hold the chip's I2C repeater open.
func (t *R860) applyIFBandwidth(coarse, fine uint8) error {
	if coarse >= r860FILTBWCount {
		return fmt.Errorf("%w: FILT_BW coarse=%d", ErrR860FilterRange, coarse)
	}

	if fine >= r860FilterStepCount {
		return fmt.Errorf("%w: FILT_CODE fine=%d", ErrR860FilterRange, fine)
	}

	coarseBits := (coarse << r860FILTBWShift) & maskR860FILTBW
	if err := t.writeRegisterMasked(regR860Filter2, coarseBits, maskR860FILTBW); err != nil {
		return fmt.Errorf("r860: set FILT_BW coarse=%d: %w", coarse, err)
	}

	if err := t.writeRegisterMasked(regR860Filter1, fine&maskR860FILTCode, maskR860FILTCode); err != nil {
		return fmt.Errorf("r860: set FILT_CODE fine=%d: %w", fine, err)
	}

	return nil
}

// applyIFHighPass programs the channel filter's high-pass corner.
// code must be in [0, 16); use the R860HPF* constants for the
// documented (corner, attenuation) tuples per datasheet table 6-3.
func (t *R860) applyIFHighPass(code uint8) error {
	if code >= r860FilterStepCount {
		return fmt.Errorf("%w: HPF code=%d", ErrR860FilterRange, code)
	}

	if err := t.writeRegisterMasked(regR860Filter2, code&maskR860HPF, maskR860HPF); err != nil {
		return fmt.Errorf("r860: set HPF code=%d: %w", code, err)
	}

	return nil
}

// applyFilterExt enables or disables the chip's filter extension
// for weak-signal conditions (datasheet R30 bit [6]). The
// datasheet doesn't say what the extension does internally, only
// that it is "for weak signal conditions" — empirically toggling
// it on a marginal chain may help; on a strong chain it is
// neutral or counterproductive.
//
//nolint:revive // this is a feature toggle, not a control-flow flag.
func (t *R860) applyFilterExt(enable bool) error {
	value := uint8(0)
	if enable {
		value = maskR860FilterExt
	}

	if err := t.writeRegisterMasked(regR860FilterExt, value, maskR860FilterExt); err != nil {
		return fmt.Errorf("r860: set FILTER_EXT enable=%t: %w", enable, err)
	}

	return nil
}

// SetIFBandwidth implements the Tuner contract. Wraps the
// internal primitive in the chip's I2C repeater so callers don't
// have to manage that themselves.
func (t *R860) SetIFBandwidth(coarse, fine uint8) error {
	return t.withRepeater(func() error {
		return t.applyIFBandwidth(coarse, fine)
	})
}

// SetIFHighPass implements the Tuner contract. See applyIFHighPass
// for the value mapping; the R860HPF* constants in this file
// match the datasheet's documented (corner, attenuation) tuples.
func (t *R860) SetIFHighPass(code uint8) error {
	return t.withRepeater(func() error {
		return t.applyIFHighPass(code)
	})
}

// SetFilterExt implements the Tuner contract.
func (t *R860) SetFilterExt(enable bool) error {
	return t.withRepeater(func() error {
		return t.applyFilterExt(enable)
	})
}
