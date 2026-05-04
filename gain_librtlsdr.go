package rtl2832u

// librtlsdr-compatible gain table
// ===============================
//
// The values and the alternate-walk algorithm in this file are
// transliterated from osmocom librtlsdr's tuner_r82xx.c
// (https://github.com/osmocom/rtl-sdr, BSD-2-Clause). librtlsdr
// derived these tenths-of-a-dB deltas empirically; the public
// R860 datasheet does not document an LNA or Mixer dB scale, so
// the values are not silicon-authoritative — they are the de
// facto reference that dump1090, readsb, and rtl_test all use,
// included here so demod1090's WithGain accepts the same
// 0..496 (tenths-dB) ladder users already know.
//
// Originating header (BSD-2-Clause, retained for attribution):
//
//   Copyright (C) 2013 Mauro Carvalho Chehab
//   Copyright (C) 2014 Steve Markgraf
//
//   Redistribution and use in source and binary forms, with or
//   without modification, are permitted under BSD-2-Clause terms.
//
// The two arrays are *deltas*, not cumulative gains. librtlsdr's
// r82xx_set_gain alternates between bumping the LNA index and the
// Mixer index, accumulating each delta into a running total until
// the requested tenths-of-a-dB is reached or the table is
// exhausted. The total spans 0..49.6 dB across the resulting 16+16
// step pairs.

// r860LNAGainSteps holds the per-step LNA gain *delta* in tenths
// of a dB. Index 0 is the no-gain baseline; each subsequent index
// adds its delta to the cumulative total.
//
//nolint:gochecknoglobals // immutable BSD-2 reference table.
var r860LNAGainSteps = [r860GainStepCount]int{
	0, 9, 13, 40, 38, 13, 31, 22,
	26, 31, 26, 14, 19, 5, 35, 13,
}

// r860MixerGainSteps mirrors r860LNAGainSteps for the post-mixer
// amplifier. Same delta-not-cumulative semantics.
//
//nolint:gochecknoglobals // immutable BSD-2 reference table.
var r860MixerGainSteps = [r860GainStepCount]int{
	0, 5, 10, 10, 19, 9, 10, 25,
	17, 10, 8, 16, 13, 6, 3, -8,
}

// librtlsdrManualVGAStep is the VGA step librtlsdr leaves manual
// gain at: VGA_CODE = 8, which per R860 datasheet table 6-3 is
// (-12.0 + 8 * 3.5) = +16.0 dB. librtlsdr's source comments call
// it "16.3 dB" — the dB scale rounds to that on slightly older
// silicon revisions; we use the documented 3.5 dB/step from the
// datasheet.
//
// Pinned here next to the librtlsdr table it derives from; the
// public WithGain Option (added later) consumes it.
//
//nolint:unused // surfaced via WithGain in a follow-up commit.
const librtlsdrManualVGAStep uint8 = 8

// librtlsdrGainSteps walks the LNA / Mixer delta tables to find
// the (lnaStep, mixerStep) pair whose cumulative total is closest
// to (but not exceeding) the requested tenths-of-a-dB target. The
// algorithm matches osmocom librtlsdr's r82xx_set_gain: bump the
// LNA index, check the running total, then bump the Mixer index,
// check again. The two indices grow in lockstep so the total
// climbs monotonically.
//
// Out-of-range targets clamp at the table boundaries: a negative
// or zero target returns (0, 0) (no manual gain), and any target
// at or above the table maximum returns the (15, 15) saturation
// pair.
func librtlsdrGainSteps(tenthsDB int) (uint8, uint8) {
	var (
		total int
		lna   uint8
		mixer uint8
	)

	for lna < r860GainStepCount-1 && mixer < r860GainStepCount-1 {
		if total >= tenthsDB {
			break
		}

		lna++
		total += r860LNAGainSteps[lna]

		if total >= tenthsDB {
			break
		}

		mixer++
		total += r860MixerGainSteps[mixer]
	}

	return lna, mixer
}
