package rtl2832u

import "fmt"

// Post-seed-table init for the R820T/R860 tuner
// ===============================================
//
// Writing the 0x05..0x1f seed table (init()) is necessary but not
// sufficient. librtlsdr's r82xx_init() follows the seed write with
// two more passes that program the actual operating mode:
//
//   1. r82xx_set_tv_standard(bw=3, type=DIGITAL_TV, delsys=0)
//      - Programs the IF filter Q, HP corner, image-rejection sign,
//        filter gain, channel-filter extension, and — critically —
//        clears bit [7] of R0x05 to disable the loop-through path.
//      - Without this, the seed value 0x83 leaves loop-through ON,
//        which routes the IF signal to a tuner pin instead of (or
//        in addition to) the internal baseband, killing the Q
//        channel at the chip's I/Q output.
//
//   2. r82xx_sysfreq_sel(freq=0, type=DIGITAL_TV, delsys=SYS_DVBT)
//      - Sets mixer-top, LNA-top, charge-pump current, mixer/LNA
//        threshold voltages, AGC clock rate, and discharge mode.
//
// Both passes are needed for the chip to produce a clean baseband
// IQ stream. We mirror them here as a single applyPostInit step
// invoked from NewR860.
//
// Filter self-calibration (the inner two-pass loop in
// r82xx_set_tv_standard that retunes the PLL to 56 MHz, pulses
// R0x0b bit [4], and reads back R0x00) is intentionally omitted —
// it requires running the PLL and reading sample status before
// the user has even committed to a centre frequency, and the
// chip's seed fil_cal_code (0) is acceptable for ADS-B reception
// per librtlsdr commentary. Add later if SNR ever shows it
// matters.

// applyPostInit runs the librtlsdr-equivalent of r82xx_set_tv_standard
// followed by r82xx_sysfreq_sel for the SDR / digital-TV / DVB-T
// combination that matches our use case. Caller must hold the
// chip's I2C repeater open.
func (t *R860) applyPostInit() error {
	if err := t.applySetTVStandard(); err != nil {
		return fmt.Errorf("r860: set TV standard: %w", err)
	}

	return t.applySysFreqSel()
}

// applySetTVStandard mirrors librtlsdr's r82xx_set_tv_standard for
// (bw=3, type=DIGITAL_TV, delsys=0) — the SDR-relevant case. Filter
// self-calibration is skipped; we accept the seed fil_cal_code = 0.
//
// Constant values are quoted from librtlsdr verbatim with their
// original comments preserved alongside. Register addresses are
// inline literals because each table row already carries the
// `why` field naming what the address controls; pulling them into
// named constants would only duplicate the comments.
//
//nolint:mnd // register addresses; documented via the per-row `why` field.
func (t *R860) applySetTVStandard() error {
	const (
		filtGain   uint8 = 0x10 // +3 dB filter gain, 6 MHz on
		imgR       uint8 = 0x00 // image negative
		filtQ      uint8 = 0x10 // R10[4]: low Q
		hpCor      uint8 = 0x6b // 1.7 MHz disable, +2 cap, 1.0 MHz
		extEnable  uint8 = 0x60 // R30[6]=1 ext enable; R30[5]=1 ext at LNA max-1
		ltOff      uint8 = 0x00 // bit [7] of R0x05 cleared → loop-through OFF
		ltAttEn    uint8 = 0x00 // R31[7] cleared → LT attenuation enable
		fltExtWide uint8 = 0x00 // R15[7] cleared → flt_ext_wide off
		polyfilCur uint8 = 0x60 // R25[6:5] = min
		filCalCode uint8 = 0x00 // we skip self-calibration; seed value
	)

	steps := []struct {
		reg, val, mask uint8
		why            string
	}{
		// Init flag / xtal-check result (clears VGA-gain init bits).
		{reg: 0x0c, val: 0x00, mask: 0x0f, why: "init flag bits[3:0]"},

		// Filter Q + (skipped) calibration code.
		{reg: 0x0a, val: filtQ | filCalCode, mask: 0x1f, why: "filt_q + fil_cal_code"},

		// Bandwidth, filter gain, HP corner.
		{reg: 0x0b, val: hpCor, mask: 0xef, why: "BW + filter gain + HP corner"},

		// Image rejection sign.
		{reg: 0x07, val: imgR, mask: 0x80, why: "img_r"},

		// 6 MHz filter on, +3 dB gain.
		{reg: 0x06, val: filtGain, mask: 0x30, why: "filt_3dB / V6MHz"},

		// Channel filter extension.
		{reg: 0x1e, val: extEnable, mask: 0x60, why: "channel filter extension"},

		// Loop-through OFF — the critical step missed by our previous
		// init. With seed bit [7]=1, the chip routes signal to the
		// LT pin and the IQ output collapses (Q-dead).
		{reg: 0x05, val: ltOff, mask: 0x80, why: "loop-through off"},

		// Loop-through attenuation enable.
		{reg: 0x1f, val: ltAttEn, mask: 0x80, why: "lt_att enable"},

		// Filter-extension-widest off.
		{reg: 0x0f, val: fltExtWide, mask: 0x80, why: "flt_ext_widest"},

		// RF poly-filter current = min.
		{reg: 0x19, val: polyfilCur, mask: 0x60, why: "polyfil_cur"},
	}

	for _, step := range steps {
		if err := t.writeRegisterMasked(step.reg, step.val, step.mask); err != nil {
			return fmt.Errorf("r860: %s (reg %#x, val %#x, mask %#x): %w",
				step.why, step.reg, step.val, step.mask, err)
		}
	}

	return nil
}

// applySysFreqSel mirrors librtlsdr's r82xx_sysfreq_sel for
// (freq=0, type=DIGITAL_TV, delsys=SYS_DVBT). Programs mixer/LNA
// top, threshold voltages, charge-pump current, divider buffer
// current, AGC clock, and discharge mode. The "freq != 506/666/818
// MHz" branch is taken for ADS-B's 1090 MHz.
//
//nolint:mnd // register addresses; documented via the per-row `why` field.
func (t *R860) applySysFreqSel() error {
	const (
		mixerTop     uint8 = 0x24 // mixer top:13, top-1, low-discharge
		lnaTop       uint8 = 0xe5 // detect bw 3, lna top:4, predet top:2
		cpCur        uint8 = 0x38 // 111, auto
		divBufCur    uint8 = 0x30 // 11, 150 µA
		lnaVthL      uint8 = 0x53 // LNA vth 0.84, vtl 0.64
		mixerVthL    uint8 = 0x75 // mixer vth 1.04, vtl 0.84
		airCable1In  uint8 = 0x00 // air input
		cable2In     uint8 = 0x00
		filterCur    uint8 = 0x40 // 10, low
		lnaDischarge uint8 = 14
	)

	commonSteps := []struct {
		reg, val, mask uint8
		why            string
	}{
		// Notes: pre_dect handling lives behind a config flag in
		// librtlsdr; we never set use_predetect, so skip the early
		// pre_dect write that's gated on it.

		{reg: 0x1d, val: lnaTop, mask: 0xc7, why: "LNA top initial"},
		{reg: 0x1c, val: mixerTop, mask: 0xf8, why: "mixer top"},
		{reg: 0x0d, val: lnaVthL, mask: 0xff, why: "LNA Vth/Vtl"},
		{reg: 0x0e, val: mixerVthL, mask: 0xff, why: "mixer Vth/Vtl"},
		{reg: 0x05, val: airCable1In, mask: 0x60, why: "air/cable1 input"},
		{reg: 0x06, val: cable2In, mask: 0x08, why: "cable2 input"},
		{reg: 0x11, val: cpCur, mask: 0x38, why: "charge-pump current"},
		{reg: 0x17, val: divBufCur, mask: 0x30, why: "divider buffer current"},
		{reg: 0x0a, val: filterCur, mask: 0x60, why: "filter current"},
	}

	for _, step := range commonSteps {
		if err := t.writeRegisterMasked(step.reg, step.val, step.mask); err != nil {
			return fmt.Errorf("r860: %s (reg %#x): %w", step.why, step.reg, err)
		}
	}

	// LNA section: digital-TV path (type != ANALOG_TV in librtlsdr).
	digitalTVSteps := []struct {
		reg, val, mask uint8
		why            string
	}{
		{reg: 0x1d, val: 0x00, mask: 0x38, why: "LNA top: lowest"},
		{reg: 0x1c, val: 0x00, mask: 0x04, why: "normal mode"},
		{reg: 0x06, val: 0x00, mask: 0x40, why: "PRE_DECT off"},
		{reg: 0x1a, val: 0x30, mask: 0x30, why: "AGC clk 250 Hz"},
		{reg: 0x1d, val: 0x18, mask: 0x38, why: "LNA top = 3"},
		// Upstream-verbatim: librtlsdr writes this with mask 0x04
		// even though tuner_r82xx.c's own comment flags the mask
		// as "IMHO wrong, but matches the original driver."
		// Mirroring upstream — diverging earned us "Q dead",
		// not a fix.
		{reg: 0x1c, val: mixerTop, mask: 0x04, why: "mixer discharge mode"},
		{reg: 0x1e, val: lnaDischarge, mask: 0x1f, why: "LNA discharge current"},
		{reg: 0x1a, val: 0x20, mask: 0x30, why: "AGC clk 60 Hz"},
	}

	for _, step := range digitalTVSteps {
		if err := t.writeRegisterMasked(step.reg, step.val, step.mask); err != nil {
			return fmt.Errorf("r860: %s (reg %#x): %w", step.why, step.reg, err)
		}
	}

	return nil
}
