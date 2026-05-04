package rtl2832u

import "fmt"

// AGC value registers
// ===================
//
// Per RTL2832U datasheet §8.1.5 (Register Name: if_agc_val /
// rf_agc_val), the chip exposes its current analogue-AGC output
// levels as read-only signed 14-bit values on demod page 3:
//
//   if_agc_val  page 3, {0x59 LSB, 0x5A MSB[5:0]}  s(14)
//   rf_agc_val  page 3, {0x5B LSB, 0x5C MSB[5:0]}  s(14)
//   aagc_lock   page 3, 0x50 bit [0]                R
//
// The values are scaled to the AGC pin output: +8191 = pin output
// at maximum control voltage (i.e. AGC is demanding maximum gain
// from the tuner), −8192 = minimum (AGC is fully attenuating).
//
// In demod1090's stack:
//
//   * rf_agc_val tracks the RTL2832U's RF_AGC pin. With an R820T/
//     R860 tuner, the LNA and Mixer have their own internal AGC
//     loops fed by the tuner's Det1/Det2 pins, so the RTL2832U
//     RF_AGC pin is unused on the wire — but the demod chip still
//     runs its own RF AGC loop and exposes its intent via this
//     register. Saturation here means the chip wants more
//     front-end gain than its closed loop can extract.
//
//   * if_agc_val drives the IF_AGC pin → the R860's IF_AGC input
//     → the R860's VGA gain. While the VGA is on AGC mode
//     (VGA_MODE=1, the default), this register is the chip's
//     real-time recommendation: saturated positive means the IF
//     AGC has hit the VGA ceiling and still wants more, so the
//     LNA/Mixer manual gains are too low.
//
// Useful for diagnostics ("is my chain over- or under-gained?")
// and as the metric input to an auto-gain tuner.

// signalStatsRegisters lays out the demod page-3 reads
// SignalStats needs. Hoisted to a struct so a future iteration can
// pack them into a single multi-byte read once we add a multi-byte
// demod-read primitive; for now, five per-byte controlIn calls is
// fine (~5 × 2 ms USB control-transfer round-trip = ~10 ms total,
// well within the 50 ms autotune-window budget).
const (
	regAGCIFValueLSB = 0x59 // if_agc_val[7:0]
	regAGCIFValueMSB = 0x5a // if_agc_val[13:8]
	regAGCRFValueLSB = 0x5b // rf_agc_val[7:0]
	regAGCRFValueMSB = 0x5c // rf_agc_val[13:8]
	regAAGCLock      = 0x50 // bit 0 = aagc_lock

	// agcValueMSBMask isolates the six high bits of a 14-bit AGC
	// value packed into the MSB byte (top two bits are reserved
	// per the datasheet's [13:0] notation).
	agcValueMSBMask uint8 = 0x3f
)

// SignalStats reports the RTL2832U's analogue AGC state. Useful
// for diagnostics and as the input to an auto-tune algorithm.
//
// RFAGCValue and IFAGCValue are scaled to the chip's RF_AGC and
// IF_AGC pin output voltages: −8192 = pin at minimum drive, 0 ≈
// midpoint, +8191 = pin at maximum drive (AGC demanding maximum
// tuner gain). Saturation in either direction means the AGC has
// hit a ceiling or floor and still wants more — typically a sign
// that the front-end gain configuration is sub-optimal.
//
// AAGCLocked reflects the chip's `aagc_lock` bit: true once the
// analogue AGC loop has settled. Always sample SignalStats with
// AAGCLocked=true for a meaningful reading; a freshly-reset chip
// or a recently-changed gain config takes a few hundred ms to
// re-lock.
type SignalStats struct {
	RFAGCValue int16
	IFAGCValue int16
	AAGCLocked bool
}

// readSignalStats fetches the AGC state from demod page 3 in the
// order documented by §8.1.5 of the RTL2832U datasheet.
func (r *rtl2832u) readSignalStats() (SignalStats, error) {
	ifLSB, err := r.demodReadByte(demodPage3, regAGCIFValueLSB)
	if err != nil {
		return SignalStats{}, fmt.Errorf("rtl2832u: read if_agc_val LSB: %w", err)
	}

	ifMSB, err := r.demodReadByte(demodPage3, regAGCIFValueMSB)
	if err != nil {
		return SignalStats{}, fmt.Errorf("rtl2832u: read if_agc_val MSB: %w", err)
	}

	rfLSB, err := r.demodReadByte(demodPage3, regAGCRFValueLSB)
	if err != nil {
		return SignalStats{}, fmt.Errorf("rtl2832u: read rf_agc_val LSB: %w", err)
	}

	rfMSB, err := r.demodReadByte(demodPage3, regAGCRFValueMSB)
	if err != nil {
		return SignalStats{}, fmt.Errorf("rtl2832u: read rf_agc_val MSB: %w", err)
	}

	lockReg, err := r.demodReadByte(demodPage3, regAAGCLock)
	if err != nil {
		return SignalStats{}, fmt.Errorf("rtl2832u: read aagc_lock: %w", err)
	}

	return SignalStats{
		RFAGCValue: signExtend14(uint16(rfMSB&agcValueMSBMask)<<bitsPerByte | uint16(rfLSB)),
		IFAGCValue: signExtend14(uint16(ifMSB&agcValueMSBMask)<<bitsPerByte | uint16(ifLSB)),
		AAGCLocked: lockReg&0x01 != 0,
	}, nil
}

// bitsPerByte names the constant 8 so the LSB/MSB packing reads
// without a magic number.
const bitsPerByte = 8

// signExtend14 promotes a 14-bit two's-complement value packed
// into the low 14 bits of a uint16 to a sign-extended int16. The
// shift-left/shift-right idiom relies on Go's arithmetic right
// shift on signed integers (>> on int16 sign-extends).
func signExtend14(value uint16) int16 {
	// 16 - 14: align bit 13 with bit 15 so int16's arithmetic
	// shift can replicate it back down.
	const shiftToTopBit = 2

	//nolint:gosec // bit-pattern reinterpret; canonical sign-extend.
	return int16(value<<shiftToTopBit) >> shiftToTopBit
}
