package rtl2832u

import "fmt"

// Sample-rate-driven IF filter selection
// =======================================
//
// librtlsdr ties two things to a single rtlsdr_set_sample_rate call:
// the demod's resampler ratio and the R820T2's IF-filter shape.
// The latter is r82xx_set_bandwidth, called from rtlsdr_set_sample_rate
// with bw = max(dev->bw, dev->rate). For an unforced bandwidth and a
// 2.4 MS/s sample rate, that picks a Mode-S-compatible filter that
// passes the 2.4 MHz baseband cleanly. Skipping the call leaves the
// filter at whatever the seed table programmed (the librtlsdr
// "FILT_BW=narrow, FILT_CODE=6" defaults), which clips most of the
// Mode S envelope and decodes every bit as noise.
//
// SetBandwidthForSampleRate ports r82xx_set_bandwidth and returns
// the demod-side IF frequency the tuner now produces. Callers that
// care about the demod's DDC alignment (rtlsdr_set_if_freq) can use
// the returned value; envelope-based PPM decoding tolerates the
// residual offset, so the chip's downsample FIR catches the signal
// regardless.

// FILT_HP_BW1 / FILT_HP_BW2 are the high-pass corner frequencies
// the bandwidth heuristic walks through. Names mirror upstream
// (tuner_r82xx.c) so a porter can grep both projects against the
// same identifier.
const (
	r82xxFILTHPBw1 = 350_000
	r82xxFILTHPBw2 = 380_000
)

// r82xxIFLowPassBwTable enumerates the low-pass corners FILT_CODE
// can reach, in descending order. Index becomes the FILT_CODE
// slot; r82xx_set_bandwidth picks the largest corner ≤ the
// remaining bandwidth budget.
//
//nolint:gochecknoglobals,mnd // verbatim port of upstream constant table.
var r82xxIFLowPassBwTable = []int{
	1_700_000, 1_600_000, 1_550_000, 1_450_000, 1_200_000,
	900_000, 700_000, 550_000, 450_000, 350_000,
}

// SetBandwidthForSampleRate programs R0x0a and R0x0b on the R820T2
// to the IF-filter shape librtlsdr's rtlsdr_set_sample_rate would
// for the given baseband bandwidth. Returns the IF frequency the
// tuner mixes RF down to — the caller must reprogram the demod's
// DDC to that same frequency (see (*rtl2832u).SetIFFrequency)
// otherwise the digital and analogue IFs disagree and the signal
// lands off-tune in the decimating FIR.
//
// The argument matches the rtl-sdr-blog librtlsdr fork's
// rtlsdr_set_sample_rate convention: bw = sample_rate. (The older
// keenerd fork passed 2 × sample_rate, which selects a wider
// 6 MHz analogue filter at intFreq = 3.57 MHz; the newer fork
// passes the raw sample rate and lands in the default branch with
// intFreq ≈ 1.815 MHz. readsb uses the rtl-sdr-blog fork; we
// match that exactly because it's the configuration we know
// produces clean Mode S frames at 2.4 MS/s.)
//
// Caller must hold the chip's I²C repeater open; SetIFBandwidth is
// the equivalent locked entry point but takes coarse/fine values
// directly. This method exists for the rate-driven path.
//
//nolint:cyclop,funlen,mnd // verbatim port of r82xx_set_bandwidth.
func (t *R860) SetBandwidthForSampleRate(bwHz uint32) (uint32, error) {
	// Drop into a signed int locally so the subtractions below can
	// transiently go negative without uint wraparound. Bandwidth
	// always fits in int on every target architecture (max value
	// is ~7 MHz).
	bwBudget := int(bwHz) //nolint:gosec // bwHz ≤ ~tens of MHz, fits in int on 32/64-bit.

	var (
		reg0a   uint8
		reg0b   uint8
		intFreq int
		realBw  int
	)

	switch {
	case bwBudget > 7_000_000:
		reg0a, reg0b = 0x10, 0x0b
		intFreq = 4_570_000
	case bwBudget > 6_000_000:
		reg0a, reg0b = 0x10, 0x2a
		intFreq = 4_570_000
	case bwBudget > r82xxIFLowPassBwTable[0]+r82xxFILTHPBw1+r82xxFILTHPBw2:
		reg0a, reg0b = 0x10, 0x6b
		intFreq = 3_570_000
	default:
		reg0a, reg0b = 0x00, 0x80
		intFreq = 2_300_000

		if bwBudget > r82xxIFLowPassBwTable[0]+r82xxFILTHPBw1 {
			bwBudget -= r82xxFILTHPBw2
			intFreq += r82xxFILTHPBw2
			realBw += r82xxFILTHPBw2
		} else {
			reg0b |= 0x20
		}

		if bwBudget > r82xxIFLowPassBwTable[0] {
			bwBudget -= r82xxFILTHPBw1
			intFreq += r82xxFILTHPBw1
			realBw += r82xxFILTHPBw1
		} else {
			reg0b |= 0x40
		}

		// Find the largest low-pass corner that still fits the
		// remaining bandwidth budget. Match upstream's "first
		// table[i] strictly less than bw, then back up one"
		// behaviour exactly.
		idx := 0
		for ; idx < len(r82xxIFLowPassBwTable); idx++ {
			if bwBudget > r82xxIFLowPassBwTable[idx] {
				break
			}
		}

		idx--
		// Upstream r82xx_set_bandwidth carries an `if (idx < 0) idx = 0;`
		// clamp here. It is structurally unreachable in this port: by
		// the time the loop runs, the preceding if/else chain has
		// clipped bwBudget to at most r82xxIFLowPassBwTable[0] (1.7 MHz),
		// so the very first iteration always finds table[0] ≥ bwBudget
		// and exits the loop with idx=0 (never breaking out earlier
		// at idx=0, which would require bwBudget > table[0]). Omitted
		// to keep coverage honest; revisit if a future caller routes
		// around the if/else chain.

		const lowPassMaxIdx = 15

		reg0b |= uint8(lowPassMaxIdx - idx) //nolint:gosec // idx ∈ [0, 15] by construction.

		realBw += r82xxIFLowPassBwTable[idx]
		intFreq -= realBw / 2
	}

	if err := t.writeRegisterMasked(0x0a, reg0a, 0x10); err != nil {
		return 0, fmt.Errorf("r860: set bandwidth R0x0a (%#02x): %w", reg0a, err)
	}

	if err := t.writeRegisterMasked(0x0b, reg0b, 0xef); err != nil {
		return 0, fmt.Errorf("r860: set bandwidth R0x0b (%#02x): %w", reg0b, err)
	}

	// Record the IF frequency the tuner will produce so SetFreq
	// can offset the LO by the same amount. Without this, SetFreq
	// would tune the LO to rfHz exactly (Zero-IF) while the
	// IF filter is centred on `intFreq`, attenuating the actual
	// signal and starving the demod's DDC.
	intFreqHz := uint32(intFreq) //nolint:gosec // intFreq ≤ 4.57 MHz fits in uint32.
	t.intFreqHz = intFreqHz

	return intFreqHz, nil
}

// InitializeForSampleRate implements Tuner.InitializeForSampleRate.
// It opens the chip's I²C repeater, runs the rate-driven IF-filter
// configuration (matching librtlsdr's rtlsdr_set_sample_rate ordering
// for the R820T path), and returns the intermediate frequency the
// demod's DDC must mix down to baseband.
//
// Separating the I²C-bracketed wrapper from SetBandwidthForSampleRate
// lets the orchestrator stay polymorphic (Tuner interface) while
// keeping the bandwidth primitive callable independently in tests
// that hold the bridge open themselves.
func (t *R860) InitializeForSampleRate(rateHz uint32) (uint32, error) {
	var intFreqHz uint32

	err := t.withRepeater(func() error {
		freq, bwErr := t.SetBandwidthForSampleRate(rateHz)
		intFreqHz = freq

		return bwErr
	})
	if err != nil {
		return 0, err
	}

	return intFreqHz, nil
}
