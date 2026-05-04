package rtl2832u

import "fmt"

// --- SetFreq register addresses, bit fields, and values ---
//
// The PLL synthesis path touches six tuner registers, each with
// its own bit field layout. We name them per librtlsdr's comments
// in r82xx_set_pll; the bit-by-bit semantics aren't datasheet-
// documented, so the names are descriptive but not authoritative.
const (
	// regR860VCOCurrent (page 0x12) packs the VCO drive current in
	// bits [7:5] and the pwSDM (integer-N override) bit in [3].
	regR860VCOCurrent uint8 = 0x12

	// regR860DivNum (page 0x10) holds the mixer divider's log2
	// form in bits [7:5].
	regR860DivNum uint8 = 0x10

	// regR860NiSi (page 0x14) is a full-byte write packing si in
	// bits [7:6] and ni in [3:0]. Bits [5:4] are reserved.
	regR860NiSi uint8 = 0x14

	// regR860SDMHigh / regR860SDMLow are the upper and lower bytes
	// of the 16-bit sigma-delta modulator value.
	regR860SDMHigh uint8 = 0x16
	regR860SDMLow  uint8 = 0x15

	// regR860Autotune (page 0x1a) controls the PLL's autotune step
	// size in bits [3:2]. We toggle to slow (128 kHz) before the
	// other writes and to fast (8 kHz) after, matching librtlsdr.
	regR860Autotune uint8 = 0x1a

	// regR860ProbeStart / regR860ProbeLen drive the initial 5-byte
	// read used to extract vcoFineTune from register 0x04.
	regR860ProbeStart uint8 = 0x00
	regR860ProbeLen   uint8 = 5

	// fineTuneRegOffset is the index inside that 5-byte buffer where
	// register 0x04 lands.
	fineTuneRegOffset uint8 = 4
)

const (
	maskR860DivNum     uint8 = 0xe0 // bits [7:5]
	maskR860VCOCurrent uint8 = 0xe0 // bits [7:5]
	maskR860PwSDM      uint8 = 0x08 // bit [3]
	maskR860Autotune   uint8 = 0x0c // bits [3:2]
	maskR860Autotune8K uint8 = 0x08 // bit [3] alone
	maskR860FineTune   uint8 = 0x30 // bits [5:4] of register 0x04
)

const (
	// r860VCOCurrent100 is the bit pattern for VCO current = 100,
	// per librtlsdr's "set VCO current = 100" comment in
	// r82xx_set_pll. Goes into the upper three bits of register 0x12.
	r860VCOCurrent100 uint8 = 0x80

	// r860Autotune128k clears bits [3:2] of register 0x1a (slow
	// autotune); r860Autotune8k sets bit [3] (fast autotune).
	r860Autotune128k uint8 = 0x00
	r860Autotune8k   uint8 = 0x08

	// r860DivNumShift moves the 3-bit divNum into bits [7:5].
	r860DivNumShift uint8 = 5

	// r860SiShift moves the 2-bit si field into bits [7:6] of
	// register 0x14.
	r860SiShift uint8 = 6

	// r860FineTuneShift extracts vcoFineTune from bits [5:4] of
	// register 0x04 by right-shifting after masking.
	r860FineTuneShift uint8 = 4

	// sdmByteShift / sdmByteMask split the 16-bit SDM into two 8-bit
	// register writes.
	sdmByteShift uint8  = 8
	sdmHighShift        = sdmByteShift
	sdmLowMask   uint16 = 0xff
)

// SetFreq implements Tuner. setMux configures the analogue front
// end for the requested band; the PLL synthesis path then drives
// the LO. Both stages share one withRepeater bracket so the chip's
// I2C bridge opens once per call.
func (t *R860) SetFreq(rfHz uint32) error {
	return t.withRepeater(func() error {
		if err := t.setMux(rfHz); err != nil {
			return err
		}

		return t.setFreqInner(rfHz)
	})
}

// setFreqInner orders the writes to match librtlsdr's r82xx_set_pll
// because the silicon expects them this way:
//
//  1. PLL autotune slow — gives the loop time to settle on big
//     frequency steps.
//  2. VCO current = 100 — drive strength for the next lock.
//  3. Read register 0x04 to extract vcoFineTune (bits [5:4]) and
//     trim divNum by ±1 around the chip's current temperature
//     curve. This read is the only I/O in setFreqInner that isn't
//     a write.
//  4. computePLLSettings produces the divider, integer-N, and SDM.
//  5. Write divNum into register 0x10 [7:5].
//  6. Write the full ni|si byte to register 0x14.
//  7. Toggle the pwSDM bit in register 0x12 — set when the
//     fractional residue is zero (integer-N tuning), cleared when
//     SDM has work to do.
//  8. Write SDM high / low bytes (registers 0x16 and 0x15).
//  9. PLL autotune fast — tight tracking once the loop is locked.
//
// PLL lock readback (a couple of register polls librtlsdr does in
// a small loop) is skipped here; failing to lock surfaces during
// the next sample stream as garbage IQ, and we do not yet have a
// retry policy that would benefit from the early signal.
func (t *R860) setFreqInner(rfHz uint32) error {
	if err := t.writeRegisterMasked(regR860Autotune, r860Autotune128k, maskR860Autotune); err != nil {
		return err
	}

	if err := t.writeRegisterMasked(regR860VCOCurrent, r860VCOCurrent100, maskR860VCOCurrent); err != nil {
		return err
	}

	vcoFineTune, err := t.readVCOFineTune()
	if err != nil {
		return err
	}

	settings, err := computePLLSettings(rfHz, t.xtalHz, vcoFineTune)
	if err != nil {
		return err
	}

	if err := t.writeRegisterMasked(regR860DivNum, settings.divNum<<r860DivNumShift, maskR860DivNum); err != nil {
		return err
	}

	if err := t.writeRegister(regR860NiSi, settings.ni|(settings.si<<r860SiShift)); err != nil {
		return err
	}

	pwSDM := uint8(0)
	if settings.sdm == 0 {
		pwSDM = maskR860PwSDM
	}

	if err := t.writeRegisterMasked(regR860VCOCurrent, pwSDM, maskR860PwSDM); err != nil {
		return err
	}

	// SDM splits across two byte writes. The shift and mask reduce
	// the 16-bit SDM to byte-sized values; gosec G115 cannot see
	// the bound but the math is exact.
	sdmHigh := uint8(settings.sdm >> sdmHighShift) //nolint:gosec
	sdmLow := uint8(settings.sdm & sdmLowMask)     //nolint:gosec

	if err := t.writeRegister(regR860SDMHigh, sdmHigh); err != nil {
		return err
	}

	if err := t.writeRegister(regR860SDMLow, sdmLow); err != nil {
		return err
	}

	return t.writeRegisterMasked(regR860Autotune, r860Autotune8k, maskR860Autotune8K)
}

// readVCOFineTune fetches register 0x04 (via a 5-byte read from
// register 0x00, matching librtlsdr) and extracts the 2-bit
// fine-tune calibration value the PLL math needs.
func (t *R860) readVCOFineTune() (uint8, error) {
	var probe [regR860ProbeLen]byte
	if err := t.readRegisters(regR860ProbeStart, probe[:]); err != nil {
		return 0, fmt.Errorf("r860: read vcoFineTune: %w", err)
	}

	return (probe[fineTuneRegOffset] & maskR860FineTune) >> r860FineTuneShift, nil
}
