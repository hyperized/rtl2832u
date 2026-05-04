package rtl2832u

import (
	"errors"
	"fmt"
)

// --- Sample rate constants ---
//
// referenceClockHz is the chip's crystal frequency. Most RTL2832U
// dongles ship with a 28.8 MHz reference, including the HackerGadgets
// AIO board this project targets. If a future board ships with a
// different TCXO this needs to become per-device, sourced from an
// EEPROM read at Open time.
const referenceClockHz uint32 = 28_800_000

// rsampRatioFractionalBits is the fixed-point scale: rsamp_ratio is
// (refClock << 22) / sampleRate. The mask drops the bottom two bits
// because the chip ignores them — keeping them just confuses the
// "exact rate" calculation downstream.
const (
	rsampRatioFractionalBits        = 22
	rsampRatioMask           uint32 = 0x0fff_fffc

	// rsampRatioRealRateBit is the mantissa-doubling bit librtlsdr
	// uses to recover the rate the chip will actually produce after
	// the bottom-bits mask. If bit 27 is set, the effective ratio
	// has bit 28 set too.
	rsampRatioRealRateBit uint32 = 0x0800_0000
)

// regDemodRSampRatioHi / regDemodRSampRatioLo carry the high and low
// 16-bit halves of rsamp_ratio. Names follow the pattern of the init
// register set; the chip stores a 28-bit value across these two
// 16-bit registers.
const (
	regDemodRSampRatioHi uint16 = 0x9f
	regDemodRSampRatioLo uint16 = 0xa1
)

// Sample rate validity bounds, transcribed from librtlsdr's
// rtlsdr_set_sample_rate. The chip's resampler only spans two
// disjoint sub-ranges; rates in the gap silently produce nonsense
// IQ. Rejecting them up front is friendlier than letting the user
// chase ghost frames at 600 kHz.
const (
	sampleRateMinExclusive uint32 = 225_000
	sampleRateMaxInclusive uint32 = 3_200_000
	sampleRateGapStart     uint32 = 300_000 // first invalid hz is gapStart+1
	sampleRateGapEnd       uint32 = 900_000
)

// ErrSampleRateOutOfRange and ErrSampleRateInGap are the static
// sentinels for SetSampleRate validation. Static so callers can
// errors.Is and branch on the kind of failure.
var (
	ErrSampleRateOutOfRange = errors.New("rtl2832u: sample rate out of supported range")
	ErrSampleRateInGap      = errors.New("rtl2832u: sample rate falls in the chip's unsupported gap")
)

// SetSampleRate programs the demodulator's resampler. The rate the
// chip actually produces may differ slightly from hz because the
// 22-bit fractional ratio rounds; the returned actualHz reports the
// precise produced rate so callers can warn or adjust.
//
// xtalHz is the *effective* reference clock — the chip's nominal
// crystal frequency adjusted for any TCXO ppm correction the
// caller wants applied. Pass referenceClockHz for the uncorrected
// path, or effectiveXtalHz(referenceClockHz, ppm) when correcting.
//
// A soft-reset pulse follows the ratio writes. librtlsdr does the
// same: without the pulse the chip can keep emitting samples at the
// previous rate for a few hundred milliseconds, which would corrupt
// preamble detection at the next stage.
func (r *rtl2832u) SetSampleRate(rate, xtalHz uint32) (uint32, error) {
	if err := validateSampleRate(rate); err != nil {
		return 0, err
	}

	rsampRatio := computeRSampRatio(rate, xtalHz)
	actualHz := computeActualSampleRate(rsampRatio, xtalHz)

	if err := r.writeRSampRatio(rsampRatio); err != nil {
		return 0, fmt.Errorf("rtl2832u: write rsamp_ratio for %d Hz: %w", rate, err)
	}

	if err := r.resetDemod(); err != nil {
		return 0, fmt.Errorf("rtl2832u: reset demod after sample rate change: %w", err)
	}

	return actualHz, nil
}

// validateSampleRate rejects rates outside (225 kHz, 300 kHz] ∪
// (900 kHz, 3.2 MHz]. Hints in the message tell the user the valid
// sub-ranges instead of forcing them to read the source.
func validateSampleRate(rate uint32) error {
	if rate <= sampleRateMinExclusive || rate > sampleRateMaxInclusive {
		return fmt.Errorf(
			"%w: %d Hz. Valid sub-ranges are (%d, %d] Hz and (%d, %d] Hz",
			ErrSampleRateOutOfRange, rate,
			sampleRateMinExclusive, sampleRateGapStart,
			sampleRateGapEnd, sampleRateMaxInclusive,
		)
	}

	if rate > sampleRateGapStart && rate <= sampleRateGapEnd {
		return fmt.Errorf(
			"%w: %d Hz lies in (%d, %d] which the resampler cannot reproduce. "+
				"Pick a rate in (%d, %d] (low range) or (%d, %d] (high range — "+
				"2.4 MS/s is the FlightAware dump1090 default for ADS-B)",
			ErrSampleRateInGap, rate,
			sampleRateGapStart, sampleRateGapEnd,
			sampleRateMinExclusive, sampleRateGapStart,
			sampleRateGapEnd, sampleRateMaxInclusive,
		)
	}

	return nil
}

// computeRSampRatio is the chip's fixed-point ratio:
//
//	ratio = (xtalHz * 2^22) / hz, with the bottom two bits cleared.
//
// We compute in uint64 because (xtalHz << 22) overflows uint32
// even at the chip's reference frequency (28.8 MHz × 4 Mi ≈ 1.2e11).
// The result fits in uint32 for any rate in the valid range.
//
// xtalHz is the effective reference clock — pass referenceClockHz
// uncorrected, or effectiveXtalHz(referenceClockHz, ppm) when the
// caller wants TCXO ppm compensation folded into the divider.
func computeRSampRatio(rate, xtalHz uint32) uint32 {
	numerator := uint64(xtalHz) << rsampRatioFractionalBits

	return uint32(numerator/uint64(rate)) & rsampRatioMask //nolint:gosec
}

// computeActualSampleRate undoes the 22-bit fixed-point math to
// recover the rate the chip will actually emit after the
// bottom-two-bits mask. The bit-27 expansion mirrors librtlsdr's
// real_rsamp_ratio computation. xtalHz must match the value
// computeRSampRatio was given when producing the rsampRatio.
func computeActualSampleRate(rsampRatio, xtalHz uint32) uint32 {
	numerator := uint64(xtalHz) << rsampRatioFractionalBits

	expanded := uint64(rsampRatio | ((rsampRatio & rsampRatioRealRateBit) << 1))

	if expanded == 0 {
		return 0
	}

	return uint32(numerator / expanded) //nolint:gosec
}

// effectiveXtalHz returns the effective reference-clock frequency
// after applying a ppm correction. Positive ppm means the crystal
// runs *fast* relative to nominal — i.e. an oscilloscope measures
// (1 + ppm·1e-6) times the ideal frequency — so we model the chip
// as if its reference were that adjusted value, and the rsamp_ratio
// and PLL math then naturally compensate.
//
// ppm is clamped to [-FrequencyCorrectionPPMMax, +FrequencyCorrectionPPMMax]
// at the option layer; this helper itself trusts its input. The
// arithmetic is done in int64 so the multiply doesn't overflow at
// the boundary (28.8 MHz × 1000 ≈ 2.88e10, fits easily).
//
// referenceHz is parameterised even though only referenceClockHz
// ever flows through today: the EEPROM reader (Plan.MD's "EEPROM
// reader" Open TODO) will eventually source per-device crystal
// values, and a unit test can pin a known value without depending
// on the package-level constant.
//
//nolint:unparam // referenceHz parameterised for future EEPROM-driven xtal; intentional.
func effectiveXtalHz(referenceHz uint32, ppm int32) uint32 {
	const ppmDivisor int64 = 1_000_000

	delta := int64(referenceHz) * int64(ppm) / ppmDivisor

	return uint32(int64(referenceHz) + delta) //nolint:gosec // bounded by ppm clamp.
}

// writeRSampRatio splits the 28-bit rsamp_ratio across the two
// 16-bit demod registers the chip exposes. The high register holds
// the upper 16 bits, the low register the lower 16; both transfers
// are big-endian on the wire.
func (r *rtl2832u) writeRSampRatio(rsampRatio uint32) error {
	const (
		halfShift          = 16
		lowHalfMask uint32 = 0xffff
	)

	hiHalf := uint16(rsampRatio >> halfShift)  //nolint:gosec
	loHalf := uint16(rsampRatio & lowHalfMask) //nolint:gosec

	if err := r.demodWriteWord(demodPage1, regDemodRSampRatioHi, hiHalf); err != nil {
		return err
	}

	return r.demodWriteWord(demodPage1, regDemodRSampRatioLo, loHalf)
}
