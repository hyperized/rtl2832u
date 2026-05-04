package rtl2832u

import (
	"errors"
	"fmt"
)

// --- PLL synthesis constants ---
//
// The R820T/R860 PLL synthesises a VCO frequency in
// [1770 MHz, 3540 MHz] and divides it down by mixDiv ∈ {2, 4, 8, 16,
// 32, 64} to produce the local-oscillator frequency that mixes the
// requested RF down to baseband. We pick the smallest mixDiv that
// puts VCO in range; that maximises the mixer's effective Q.
//
// Source: osmocom librtlsdr's tuner_r82xx.c (BSD-2). The bit-by-bit
// register semantics aren't documented in any public datasheet —
// these constants are the only way to reach a working tuner.
const (
	r860VCOMinHz uint64 = 1_770_000_000
	r860VCOMaxHz uint64 = 2 * r860VCOMinHz // 3_540_000_000

	r860MixDivMin uint8 = 2
	r860MixDivMax uint8 = 64

	// Integer-N range. nint = (rfHz * mixDiv) / (2 * xtalHz). The
	// register packs ni (4 bits, low) and si (2 bits, high) where
	// nint = 4*ni + si + 13. Hence nint ∈ [13, 4*15+3+13] = [13, 76].
	r860NintBias = 13
	r860NintMin  = r860NintBias            // 13
	r860NintMax  = 4*15 + 3 + r860NintBias // 76

	// 16-bit sigma-delta modulator. The chip stores the fractional
	// part of the PLL division as a 16-bit value that approximates
	// (vco_fra * 65536) / (2 * xtalHz).
	r860SDMResolution uint32 = 1 << 16

	// r860VCOPowerRef is the calibration reference we compare the
	// chip's vco_fine_tune (read from register 0x04 bits [5:4])
	// against. Higher means the VCO is running cool — bump divNum
	// down by one; lower means hot — bump up.
	r860VCOPowerRef uint8 = 2
)

// niStep is the divisor used to decompose nint into ni + si:
//
//	nint = 4*ni + si + 13
//
// where ni occupies register 0x14 bits [3:0] and si bits [7:6].
// The "4" is the cardinality of si (a 2-bit field): every
// 4-increment of nint increments ni; the 0..3 leftover is si.
const niStep uint8 = 4

// pllSettings holds the register-level values SetFreq writes after
// computePLLSettings runs. Each field maps to one or more bits of a
// specific R860 register; the orchestrator (SetFreq) is responsible
// for the read-modify-write per register.
type pllSettings struct {
	mixDiv uint8  // 2/4/8/16/32/64; not written directly, drives divNum
	divNum uint8  // 3-bit field in register 0x10 [7:5]
	nint   uint8  // composed from ni and si below
	ni     uint8  // register 0x14 [3:0]
	si     uint8  // register 0x14 [7:6]
	sdm    uint16 // register 0x15 (low byte) + register 0x16 (high byte)
}

// errR860FreqOutOfRange is the static sentinel for an RF frequency
// the R860 family cannot synthesise. Either no mixDiv lands the VCO
// in [1.77, 3.54] GHz, or the resulting nint falls outside the
// register's 6-bit window.
var errR860FreqOutOfRange = errors.New(
	"r860: frequency out of range; the R820T/R860 family covers " +
		"approximately 28 MHz to 1.766 GHz",
)

// computePLLSettings returns the register settings the R860 needs
// to lock to rfHz with the given crystal reference. vcoFineTune is
// read from the tuner's register 0x04 bits [5:4] before calling.
//
// The function is pure: no I/O, no chip state. SetFreq orchestrates
// the actual reads and writes.
func computePLLSettings(rfHz, xtalHz uint32, vcoFineTune uint8) (pllSettings, error) {
	var settings pllSettings

	mixDiv, divNum, ok := pickMixDiv(rfHz)
	if !ok {
		return settings, fmt.Errorf("%w: %d Hz cannot lock VCO with any mixDiv in {2..64}",
			errR860FreqOutOfRange, rfHz)
	}

	// vco_fine_tune is the chip's own opinion of where the VCO is
	// running on its temperature curve. We trim divNum by ±1 to
	// keep the VCO in the centre of its sweet spot.
	switch {
	case vcoFineTune > r860VCOPowerRef:
		if divNum == 0 {
			return settings, fmt.Errorf("%w: vcoFineTune=%d would underflow divNum at mixDiv=%d",
				errR860FreqOutOfRange, vcoFineTune, mixDiv)
		}

		divNum--
	case vcoFineTune < r860VCOPowerRef:
		divNum++
	default:
		// vcoFineTune matches the reference; divNum stays as picked.
	}

	vcoFreqHz := uint64(rfHz) * uint64(mixDiv)
	twoXtalHz := uint64(xtalHz) * 2
	nint64 := vcoFreqHz / twoXtalHz

	if nint64 < r860NintMin || nint64 > r860NintMax {
		return settings, fmt.Errorf("%w: nint=%d outside register range [%d, %d] for rfHz=%d",
			errR860FreqOutOfRange, nint64, r860NintMin, r860NintMax, rfHz)
	}

	nint := uint8(nint64) //nolint:gosec // bounded above by r860NintMax check.

	// vco_fra is the residual after subtracting the integer-N portion;
	// the SDM algorithm operates in kHz so both inputs are kHz too.
	const hzPerKHz = 1000

	vcoFraHz := vcoFreqHz - twoXtalHz*nint64
	vcoFraKHz := vcoFraHz / hzPerKHz
	twoXtalKHz := twoXtalHz / hzPerKHz

	settings.mixDiv = mixDiv
	settings.divNum = divNum
	settings.nint = nint
	settings.ni = (nint - r860NintBias) / niStep
	settings.si = nint - niStep*settings.ni - r860NintBias
	settings.sdm = computeSDM(vcoFraKHz, twoXtalKHz)

	return settings, nil
}

// pickMixDiv scans the {2, 4, 8, 16, 32, 64} ladder for the
// smallest mixDiv that lands the VCO in lock range. Returns
// (mixDiv, divNum, ok) where divNum = log2(mixDiv) - 1 because the
// chip stores log2 form, not mixDiv directly.
func pickMixDiv(rfHz uint32) (uint8, uint8, bool) {
	for mixDiv := r860MixDivMin; mixDiv <= r860MixDivMax; mixDiv <<= 1 {
		vco := uint64(rfHz) * uint64(mixDiv)
		if vco < r860VCOMinHz || vco >= r860VCOMaxHz {
			continue
		}

		// divNum = log2(mixDiv) - 1. Computed by halving mixDiv
		// until it reaches the floor; matches librtlsdr's div_buf
		// loop without depending on math/bits.
		var divNum uint8
		for shifted := mixDiv; shifted > r860MixDivMin; shifted >>= 1 {
			divNum++
		}

		return mixDiv, divNum, true
	}

	return 0, 0, false
}

// computeSDM is librtlsdr's iterative sigma-delta modulator
// approximation: it builds a 16-bit fractional value such that
//
//	sdm / 65536 ≈ fractionalKHz / twoXtalKHz
//
// using a power-of-two-denominator refinement that does not need
// any 64-bit divides. The algorithm walks nSDM = 2, 4, 8, ...; on
// each step that the residual fraction can absorb, it adds the
// corresponding mantissa bit and subtracts the contribution.
//
// The chip stores SDM as 16 bits, so the loop stops at nSDM = 0x8000
// (the next iteration would write past the LSB).
func computeSDM(fractionalKHz, twoXtalKHz uint64) uint16 {
	const sdmStopBit uint32 = 0x8000

	var sdm uint32

	nSDM := uint32(2)
	residual := fractionalKHz

	for residual > 1 {
		step := twoXtalKHz / uint64(nSDM)
		if residual > step {
			sdm += r860SDMResolution / nSDM
			residual -= step

			if nSDM >= sdmStopBit {
				break
			}
		}

		nSDM <<= 1
	}

	return uint16(sdm) //nolint:gosec // bounded by SDM ladder sum < 65536.
}
