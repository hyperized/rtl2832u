package rtl2832u

import "fmt"

// Tuner-aware demod configuration
// ================================
//
// Init() leaves the chip in zero-IF mode (en_bbin = 1) which only
// makes sense when the tuner attached produces a complex baseband
// stream. The R820T2 / R860 does not — it outputs a real-valued
// signal at a 3.57 MHz IF, expecting the demod's DDC to mix it
// down to baseband and split into I/Q.
//
// Running the chip with en_bbin = 1 against an R820T2 routes the
// tuner's I-only output around the DDC, populates only the I ADC,
// and leaves the Q ADC sampling thermal noise (std ≈ 1.9 LSB at
// the chip's 8-bit output, exactly what we observed in the field
// before this fix). The host-side magnitude sees |I|, the PPM bit
// decisions see noise-shaped garbage, and CRC residual is random
// — for every frame, every aircraft.
//
// configureForR820T undoes the zero-IF default and programs the
// pieces librtlsdr writes for the R820T case in
// rtlsdr_open / rtlsdr_set_center_freq:
//
//   - regDemodZeroIF (0xb1) ← 0x1a   (clear en_bbin; keep
//                                     en_dc_est, en_iq_comp,
//                                     en_iq_est)
//   - regDemodADCInput (0x08) ← 0x4d (only enable I-ADC input;
//                                     Q-ADC stays parked because
//                                     the tuner has no Q output
//                                     to feed it)
//   - DDC IF freq (0x19/0x1a/0x1b page 1) ← encoded -3.57 MHz
//   - regDemodSpectrumInv (0x15) ← 0x01 (the tuner's mixer flips
//                                        spectrum sense; the demod
//                                        flips it back)
//
// Without configureForR820T the rest of the demod chain produces
// plausible-looking preambles from noise but no clean Mode S
// frames, because the DDC is bypassed and the bit-level decisions
// happen against an offset-IF signal masquerading as baseband.

const (
	// regDemodADCInput selects which ADC inputs the chip wires
	// into its DDC. 0xcd enables I+Q (default for direct-sampling
	// or zero-IF tuners); 0x4d enables I only — the right value
	// when an R820T2 / R860 produces a real-valued IF.
	regDemodADCInput uint16 = 0x08 // page 0

	// regDemodIFFreqHi/Mid/Lo carry the 22-bit signed digital IF
	// frequency the DDC mixes by. Stored across three demod
	// page-1 registers, MSB first, with the high register's top
	// two bits unused.
	regDemodIFFreqHi  uint16 = 0x19 // page 1
	regDemodIFFreqMid uint16 = 0x1a // page 1
	regDemodIFFreqLo  uint16 = 0x1b // page 1
)

const (
	// zeroIFDisabled clears en_bbin (bit 0) while keeping
	// DC cancellation + IQ comp/est on; the value librtlsdr
	// writes for the R820T path.
	zeroIFDisabled uint8 = 0x1a

	// adcInputIOnly enables only the I-ADC channel. The R820T2
	// outputs a real IF on a single line; pretending to also
	// feed Q populates Q with thermal noise that downstream
	// demod treats as signal.
	adcInputIOnly uint8 = 0x4d

	// spectrumInvOn flips the demod's spectrum-inversion bit,
	// undoing the inversion the R820T2 mixer introduces. With
	// inversion off and an R820T2 attached, lower-sideband
	// signals end up where the upper-sideband should be and
	// the magnitude lookup smears bit decisions.
	spectrumInvOn uint8 = 0x01

	// r820tIFFreqHz is the IF the R820T2 / R860 produces in
	// 6 MHz DVB-T mode (and the value librtlsdr also uses for
	// SDR). The 8 MHz mode would shift to 4.57 MHz, but Mode S
	// at 2.4 MS/s lives comfortably inside the 6 MHz channel
	// filter so we hardcode the lower IF here.
	r820tIFFreqHz uint32 = 3_570_000

	// ifFreqRegMask bounds the 22-bit signed IF frequency value
	// to its three-byte register layout. The high byte's top
	// two bits are unused.
	ifFreqRegMask uint32 = 0x3fffff

	// ifFreqFracBits is the 22-bit fixed-point scale used for
	// the IF programming: regs encode (-freqHz × 2²² / xtalHz).
	ifFreqFracBits = 22
)

// configureForR820T applies the demod-side register writes that
// librtlsdr does for the R820T tuner path. xtalHz is the
// effective reference clock (referenceClockHz adjusted for any
// ppm correction). Caller must drive this AFTER Init() but BEFORE
// ResetSampleBuffer() — Init() leaves zero-IF on, ResetSampleBuffer
// arms the bulk-IN endpoint with whatever state we last wrote.
func (r *rtl2832u) configureForR820T(xtalHz uint32) error {
	if err := r.demodWriteByte(demodPage1, regDemodZeroIF, zeroIFDisabled); err != nil {
		return fmt.Errorf("rtl2832u: disable Zero-IF for R820T: %w", err)
	}

	if err := r.demodWriteByte(demodPage0, regDemodADCInput, adcInputIOnly); err != nil {
		return fmt.Errorf("rtl2832u: enable I-only ADC input for R820T: %w", err)
	}

	if err := r.writeDemodIFFreq(r820tIFFreqHz, xtalHz); err != nil {
		return fmt.Errorf("rtl2832u: program demod IF freq for R820T: %w", err)
	}

	if err := r.demodWriteByte(demodPage1, regDemodSpectrumInv, spectrumInvOn); err != nil {
		return fmt.Errorf("rtl2832u: enable spectrum inversion for R820T: %w", err)
	}

	return nil
}

// SetIFFrequency reprograms the demod's DDC to mix the given IF
// down to baseband. Call this whenever the tuner's IF output
// changes (i.e. after r82xx_set_bandwidth picks a different
// intFreq for a different sample rate); leaving the demod at the
// init-time 3.57 MHz value while the tuner produces a 1.815 MHz IF
// (the default-branch result for samp_rate ≤ 2.43 MHz) shifts the
// signal by 1.755 MHz at the demod's input — past the decimating
// FIR's pass-band edge for any output ≤ 3.5 MS/s, attenuating the
// Mode S envelope into the noise floor.
//
// Thin wrapper around writeDemodIFFreq exposed for the bring-up
// orchestrator; the package's own bring-up uses the unexported
// helper directly.
func (r *rtl2832u) SetIFFrequency(freqHz, xtalHz uint32) error {
	return r.writeDemodIFFreq(freqHz, xtalHz)
}

// writeDemodIFFreq programs the DDC's digital IF frequency in
// the chip's signed 22-bit two's-complement format. The mix-down
// uses the negation: register encodes -freqHz × 2²² / xtalHz so
// the DDC's complex exponential e^{-jωt} brings a positive-IF
// signal down to baseband.
//
// The chip latches the value across three byte writes;
// downstream sample arrival picks up the new shift on the next
// DDC iteration. No reset pulse needed.
func (r *rtl2832u) writeDemodIFFreq(freqHz, xtalHz uint32) error {
	const (
		highByteShift = 16
		midByteShift  = 8
		lowByteMask   = 0xff

		// hiByteMask isolates the top six bits of the 22-bit
		// register value (bits [21:16] live in 0x19's low six
		// bits; bits [7:6] of the register byte are unused).
		hiByteMask = 0x3f
	)

	// Compute in signed int64 so the negation, shift, and divide
	// all carry the sign correctly. librtlsdr: if_freq =
	// ((freq * 2^22) / rtl_xtal) * -1. The result fits in 22
	// signed bits for any freqHz < xtalHz, which is always true
	// for the R820T2's 3.57 MHz IF against the 28.8 MHz xtal.
	signed := -(int64(freqHz) << ifFreqFracBits) / int64(xtalHz)
	encoded := uint32(signed) & ifFreqRegMask //nolint:gosec // bit-pattern reinterpret; bounded by mask.

	hi := byte((encoded >> highByteShift) & hiByteMask)
	mid := byte((encoded >> midByteShift) & lowByteMask)
	low := byte(encoded & lowByteMask)

	if err := r.demodWriteByte(demodPage1, regDemodIFFreqHi, hi); err != nil {
		return err
	}

	if err := r.demodWriteByte(demodPage1, regDemodIFFreqMid, mid); err != nil {
		return err
	}

	return r.demodWriteByte(demodPage1, regDemodIFFreqLo, low)
}
