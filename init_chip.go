package rtl2832u

import (
	"errors"
	"fmt"
	"math"
)

// All silicon-imposed values used by Init() are declared in this
// section. Two reasons for grouping them here rather than scattering
// inside the phase methods:
//
//   - Maintenance: a future maintainer auditing the chip's register
//     map can read the entire init contract without jumping between
//     functions.
//   - Datasheet citation: the chip's register layout is the
//     authoritative source. We mirror that in one block so the
//     citation has one place to live.
//
// All values transcribed from osmocom librtlsdr's rtlsdr_init_baseband
// (BSD-2). Refresh by re-reading
// https://github.com/osmocom/rtl-sdr/blob/master/src/librtlsdr.c.

// --- Demod page numbers ---
//
// The RTL2832U exposes its demodulator state across pages addressed
// via the low byte of wIndex. Init touches pages 0 and 1; the AGC
// readback registers (datasheet §8.1.5) live on page 3.
const (
	demodPage0 uint8 = 0
	demodPage1 uint8 = 1
	demodPage3 uint8 = 3
)

// --- SYS-block register addresses (chipBlockSYS) ---
//
// Names match `enum sys_reg` in osmocom librtlsdr's include/rtl-sdr.h.
// Only the registers Init() touches are listed; the set can grow as
// later phases need new addresses.
const (
	regSYSDemodCtl  uint16 = 0x3000
	regSYSDemodCtl1 uint16 = 0x300b
)

// --- Demod-page register addresses ---
//
// librtlsdr leaves most of these unnamed; the descriptive names below
// come from the `/* ... */` comments in rtlsdr_init_baseband. Where
// the comment is silent we use a name derived from the bit role.
const (
	regDemodSoftReset   uint16 = 0x01 // page 1
	regDemodAGCLoop     uint16 = 0x04 // page 1; RF/IF AGC loop control
	regDemodADCDatapath uint16 = 0x06 // page 0; opt_adc_iq routing
	regDemodClockOutput uint16 = 0x0d // page 0; 4.096 MHz clock pin
	regDemodAGCEnable   uint16 = 0x11 // page 1; en_dagc bit
	regDemodSpectrumInv uint16 = 0x15 // page 1
	regDemodACRBase     uint16 = 0x16 // page 1; ACR register, also DDC+IF base
	regDemodSDRMode     uint16 = 0x19 // page 0; SDR vs DVB-T datapath
	regDemodPIDFilter   uint16 = 0x61 // page 0; PID filter enable
	regDemodFSM1        uint16 = 0x93 // page 1; FSM state holding register 1
	regDemodFSM2        uint16 = 0x94 // page 1; FSM state holding register 2
	regDemodZeroIF      uint16 = 0xb1 // page 1; en_bbin/en_dc_est/en_iq_*
)

// --- USB endpoint phase values ---
//
// SYSCTL turns on the USB block, EPA_MAXPKT sets the max packet
// size, EPA_CTL is the EP-A FIFO control register. usbEPACtlReset
// (0x1002) halts the FIFO and clears any partial transfer; the
// follow-up usbEPACtlRun (0x0000) resumes streaming. librtlsdr's
// rtlsdr_reset_buffer pulses the pair before every stream start;
// the chip leaves EPA_CTL in the "halt" state after init so the
// run write is required or bulk reads get EPIPE.
const (
	usbSysCtlOn        uint8  = 0x09
	usbEPAMaxPktConfig uint16 = 0x0002
	usbEPACtlReset     uint16 = 0x1002
	usbEPACtlRun       uint16 = 0x0000
)

// --- Power-on demod phase values (SYS block) ---
//
// Both writes go to the SYS block to bring the demod power rails up;
// the demod side itself is still asleep at this point.
const (
	sysDemodCtl1PowerOn uint8 = 0x22
	sysDemodCtlPowerOn  uint8 = 0xe8
)

// --- Soft reset phase values ---
//
// Bit 3 of regDemodSoftReset is soft_rst. The chip latches state on
// the rising-then-falling transition, so the phase writes the bit
// asserted, then released.
const (
	softResetAsserted uint8 = 0x14
	softResetReleased uint8 = 0x10
)

// --- Spectrum / ACR phase values ---
//
// Spectrum inversion (bit 0 of regDemodSpectrumInv) and adjacent
// channel rejection (regDemodACRBase) are both disabled by writing
// zero — DVB-T artefacts irrelevant to SDR mode.
const (
	spectrumInvOff uint8  = 0x00
	acrCleared     uint16 = 0x0000
)

// --- DDC+IF clear phase values ---
//
// Six consecutive bytes starting at regDemodACRBase hold the DDC
// shift and IF settings; we zero them so SetCenterFreq starts from a
// known baseline.
const (
	clearedByte   uint8  = 0x00
	ddcIFRegCount uint16 = 6
)

// --- SDR mode phase values ---
//
// 0x05 enables the SDR datapath and clears bit 5 (DAGC). The
// bit-by-bit semantics aren't documented in any public datasheet —
// we follow librtlsdr.
const sdrModeOn uint8 = 0x05

// --- FSM state seed values ---
//
// Hard-coded librtlsdr defaults; the chip won't track signal
// correctly without these holding-register seeds.
const (
	fsmReg1Init uint8 = 0xf0
	fsmReg2Init uint8 = 0x0f
)

// --- AGC disable phase values ---
//
// Both registers go to zero. They have separate named constants
// (rather than reusing clearedByte) so the call sites name what
// they're disabling, not just "write a zero here".
//
// Why both AGC subsystems are disabled
// ------------------------------------
// An earlier iteration of this driver wrote 0xc8 to regDemodAGCLoop
// (en_if_agc + en_rf_agc + loop_gain2 = 4) so SignalStats had a
// running RF/IF AGC loop to report. That made the chip drive the
// IF_AGC pin while the R820T2's VGA was simultaneously reading
// IF_AGC in its auto mode (VGA_MODE = 1). The two systems fed each
// other's outputs into each other's inputs, the VGA gain wandered,
// and the host-side decoder reported zero clean Mode S frames at
// any tuner gain — a 15× yield drop compared to disabling the loop.
//
// librtlsdr writes 0 here for the R820T path; we now do the same.
// SignalStats's RFAGCValue / IFAGCValue still read whatever the
// chip's hardware leaves in the value registers, but the values are
// effectively static and the AAGCLocked bit no longer flips during
// an auto-tune sweep — a diagnostic regression we accept in
// exchange for the chain actually decoding signal.
const (
	dagcDisabled uint8 = 0x00

	// rfifAGCDisabled writes the demod-page-1-register-0x04 byte
	// for the AGC subsystem with both en_if_agc and en_rf_agc
	// cleared. Mirrors librtlsdr's R820T configuration.
	rfifAGCDisabled uint8 = 0x00
)

// --- PID filter phase values ---
//
// 0x60 is librtlsdr's "PID filter off" value; the bit pattern is
// undocumented but matches the Realtek Windows DAB/FM driver.
const pidFilterDisabled uint8 = 0x60

// --- ADC datapath phase values ---
//
// 0x80 selects the default ADC_I/ADC_Q routing (opt_adc_iq=0).
const adcDatapathDefault uint8 = 0x80

// --- Zero-IF phase values ---
//
// 0x1b = 0b00011011 enables en_bbin (Zero-IF), en_dc_est (DC
// cancellation), en_iq_comp and en_iq_est (IQ compensation +
// estimation).
const zeroIFEnabled uint8 = 0x1b

// --- Clock output phase values ---
//
// 0x83 silences the 4.096 MHz reference clock the chip can emit on
// pin 4. We never wire it externally; leaving it on emits unwanted
// EMI near the antenna.
const clockOutputDisabled uint8 = 0x83

// --- FIR coefficient layout ---
//
// The downsampling FIR has 16 taps: top 8 are int8 (one byte each),
// bottom 8 are int12 packed two-into-three bytes. 8 + 12 = 20 bytes
// on the wire.
const (
	firTapCount        = 16
	firTopTapCount     = 8
	firBottomTapCount  = firTapCount - firTopTapCount
	firTopByteCount    = firTopTapCount // 1 byte per tap
	firBottomByteCount = firBottomTapCount * 3 / 2
	firTotalByteCount  = firTopByteCount + firBottomByteCount

	// firBaseAddr is the page-1 address at which the chip expects the
	// first FIR coefficient byte. The remaining 19 bytes occupy the
	// next 19 sequential addresses.
	firBaseAddr uint16 = 0x1c

	// firTopMin/Max bound the int8-storable taps; firBottomMin/Max
	// bound the int12-storable taps. Named so the bounds checks read
	// like English and so mnd stays quiet.
	firTopMin    int16 = math.MinInt8
	firTopMax    int16 = math.MaxInt8
	firBottomMin int16 = -2048
	firBottomMax int16 = 2047
)

// defaultFIRCoefficients are the 16 filter taps used by the Realtek
// Windows DAB/FM driver, also the librtlsdr default. Chosen to keep
// roughly 1 MHz of usable signal bandwidth at sample rates up to
// 2.4 MS/s — wide enough for ADS-B's preamble selectivity, narrow
// enough to suppress out-of-band noise.
//
// Stored as int16 because the upper 8 are int8 (-128..127) and the
// lower 8 are int12 (-2048..2047); int16 covers both ranges without
// per-half sign juggling.
//
//nolint:gochecknoglobals // immutable default register table; safe as a package-level value.
var defaultFIRCoefficients = [firTapCount]int16{
	-54, -36, -41, -40, -32, -14, 14, 53, // top 8 (8-bit signed)
	101, 156, 215, 273, 327, 372, 404, 421, // bottom 8 (12-bit signed)
}

// errFIRTopOutOfRange and errFIRBottomOutOfRange are the static
// sentinels for FIR taps that don't fit their half's bit width.
// Static so packing failures can be detected with errors.Is.
var (
	errFIRTopOutOfRange    = errors.New("rtl2832u: FIR top tap out of int8 range")
	errFIRBottomOutOfRange = errors.New("rtl2832u: FIR bottom tap out of int12 range")
)

// packFIRCoefficients converts 16 signed integer taps into the 20-byte
// wire format the RTL2832U expects.
//
// Top 8 taps go raw as int8. Bottom 8 taps are packed in pairs: every
// (x, y) pair becomes three bytes:
//
//	byte0 = x[11:4]               (top 8 of x's 12 bits)
//	byte1 = x[3:0] << 4 | y[11:8] (bottom 4 of x, top 4 of y)
//	byte2 = y[7:0]                (bottom 8 of y)
//
// Negative taps are sign-extended into a uint16 mask via & 0x0fff so
// the packer is correct for the full int12 range, even though the
// default table only uses positive bottom taps.
func packFIRCoefficients(taps [firTapCount]int16) ([firTotalByteCount]byte, error) {
	var out [firTotalByteCount]byte

	for i := range firTopTapCount {
		if taps[i] < firTopMin || taps[i] > firTopMax {
			return out, fmt.Errorf("%w: tap[%d] = %d", errFIRTopOutOfRange, i, taps[i])
		}

		out[i] = byte(int8(taps[i])) //nolint:gosec // bounded above by firTopMin/Max check.
	}

	const (
		twelveBitMask  uint16 = 0x0fff
		nibbleHighMask uint16 = 0xf0
		nibbleLowMask  uint16 = 0x0f
		nibbleShift           = 4
		byteMask       uint16 = 0xff
		highByteShift         = 8
	)

	for pair := range firBottomTapCount / 2 {
		xIdx := firTopTapCount + pair*2
		yIdx := xIdx + 1

		xTap := taps[xIdx]
		yTap := taps[yIdx]

		if xTap < firBottomMin || xTap > firBottomMax || yTap < firBottomMin || yTap > firBottomMax {
			return out, fmt.Errorf("%w: tap[%d]=%d, tap[%d]=%d",
				errFIRBottomOutOfRange, xIdx, xTap, yIdx, yTap)
		}

		// Bit-twiddling section: every byte() conversion below is
		// bounded by twelveBitMask + further masks/shifts; gosec's
		// G115 cannot see those bounds, hence the per-line nolints.
		xBits := uint16(xTap) & twelveBitMask //nolint:gosec
		yBits := uint16(yTap) & twelveBitMask //nolint:gosec

		base := firTopByteCount + pair*3
		out[base+0] = byte(xBits >> nibbleShift)                 //nolint:gosec
		out[base+1] = byte((xBits<<nibbleShift)&nibbleHighMask | //nolint:gosec
			(yBits>>highByteShift)&nibbleLowMask)
		out[base+2] = byte(yBits & byteMask) //nolint:gosec
	}

	return out, nil
}

// Init brings the RTL2832U into a state ready for SetSampleRate +
// SetCenterFreq. The order matches osmocom librtlsdr's
// rtlsdr_init_baseband — the silicon expects this exact sequence;
// reordering can break later stages even though no individual write
// errors out. Each phase wraps its own error context, so the outer
// wrap from Init is intentionally thin.
func (r *rtl2832u) Init() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"USB endpoint setup", r.initUSB},
		{"power-on demod", r.powerOnDemod},
		{"reset demod", r.resetDemod},
		{"configure spectrum", r.configureSpectrum},
		{"clear DDC and IF", r.clearDDCAndIF},
		{"write default FIR", r.writeDefaultFIR},
		{"configure SDR mode", r.configureSDRMode},
		{"init FSM state", r.initFSMState},
		{"disable demod AGC", r.disableDemodAGC},
		{"disable RF/IF AGC", r.disableRFIFAGC},
		{"disable PID filter", r.disablePIDFilter},
		{"configure ADC datapath", r.configureADCDatapath},
		{"enable Zero-IF mode", r.enableZeroIF},
		{"disable 4.096 MHz clock output", r.disableClockOutput},
	}

	for _, step := range steps {
		if err := step.fn(); err != nil {
			return fmt.Errorf("rtl2832u: init: %s: %w", step.name, err)
		}
	}

	return nil
}

// initUSB programs the USB endpoint controller for bulk transfers:
// SYSCTL turns on the USB block, EPA_MAXPKT sets the max packet size,
// EPA_CTL halts the EP-A FIFO. After this point the chip is ready
// for SetSampleRate / SetCenterFreq, but bulk reads will EPIPE
// until ResetSampleBuffer runs to flip EPA_CTL back to "run".
func (r *rtl2832u) initUSB() error {
	if err := r.writeByte(chipBlockUSB, regUSBSysCtl, usbSysCtlOn); err != nil {
		return err
	}

	if err := r.writeWord(chipBlockUSB, regUSBEPAMaxPkt, usbEPAMaxPktConfig); err != nil {
		return err
	}

	return r.writeWord(chipBlockUSB, regUSBEPACtl, usbEPACtlReset)
}

// ResetSampleBuffer prepares the chip to stream samples after Open
// or any time bulk reads need to be re-armed. It performs two
// distinct pulses, both of which librtlsdr issues at the start of
// rtlsdr_read_async:
//
//   - EPA_CTL halt (0x1002) → run (0x0000): flushes the EP-A FIFO
//     between the demod and the USB block. Without this the bulk
//     endpoint is stalled and the kernel returns EPIPE on the
//     first REAPURB.
//   - Demod soft-reset on page 1, register 0x01: 0x14 (asserted) →
//     0x10 (released). This is librtlsdr's "trigger sync mode (also
//     flushes the FIFO)" pulse — the demod's sample FIFO needs a
//     resync after PLL re-locks or any sample-rate / centre-freq
//     change, otherwise no URBs ever complete.
//
// Earlier revisions of this file targeted page 0 with the
// speculative claim that the chip mirrors soft-reset bits across
// pages; neither the RTL2832U datasheet nor osmocom librtlsdr
// support that, and the page-1 register is the only documented
// soft-reset surface (used by rtlsdr_set_sample_rate and chip
// init). Page-1 makes the intent unambiguous and matches the
// upstream reference.
func (r *rtl2832u) ResetSampleBuffer() error {
	if err := r.writeWord(chipBlockUSB, regUSBEPACtl, usbEPACtlReset); err != nil {
		return fmt.Errorf("rtl2832u: reset sample buffer (halt): %w", err)
	}

	if err := r.writeWord(chipBlockUSB, regUSBEPACtl, usbEPACtlRun); err != nil {
		return fmt.Errorf("rtl2832u: reset sample buffer (run): %w", err)
	}

	if err := r.demodWriteByte(demodPage1, regDemodSoftReset, softResetAsserted); err != nil {
		return fmt.Errorf("rtl2832u: trigger sync mode (assert): %w", err)
	}

	if err := r.demodWriteByte(demodPage1, regDemodSoftReset, softResetReleased); err != nil {
		return fmt.Errorf("rtl2832u: trigger sync mode (release): %w", err)
	}

	return nil
}

// powerOnDemod toggles the system-side demodulator power rails. Both
// writes go to the SYS block, never the demod block — the demod
// itself is still asleep at this point.
func (r *rtl2832u) powerOnDemod() error {
	if err := r.writeByte(chipBlockSYS, regSYSDemodCtl1, sysDemodCtl1PowerOn); err != nil {
		return err
	}

	return r.writeByte(chipBlockSYS, regSYSDemodCtl, sysDemodCtlPowerOn)
}

// resetDemod pulses the soft_rst bit (bit 3 of regDemodSoftReset on
// page 1): set it high, then clear it. The chip latches state on the
// rising-then-falling transition, not on either edge alone.
func (r *rtl2832u) resetDemod() error {
	if err := r.demodWriteByte(demodPage1, regDemodSoftReset, softResetAsserted); err != nil {
		return err
	}

	return r.demodWriteByte(demodPage1, regDemodSoftReset, softResetReleased)
}

// configureSpectrum disables spectrum inversion (regDemodSpectrumInv)
// and adjacent-channel rejection (regDemodACRBase as a 16-bit clear).
// regDemodACRBase is then re-cleared by clearDDCAndIF; the redundancy
// matches librtlsdr.
func (r *rtl2832u) configureSpectrum() error {
	if err := r.demodWriteByte(demodPage1, regDemodSpectrumInv, spectrumInvOff); err != nil {
		return err
	}

	return r.demodWriteWord(demodPage1, regDemodACRBase, acrCleared)
}

// clearDDCAndIF zeros the six consecutive registers starting at
// regDemodACRBase that hold the DDC shift and IF settings. Zeroing
// them here ensures SetCenterFreq starts from a known baseline rather
// than whatever stale values survived the soft reset.
func (r *rtl2832u) clearDDCAndIF() error {
	for offset := range ddcIFRegCount {
		if err := r.demodWriteByte(demodPage1, regDemodACRBase+offset, clearedByte); err != nil {
			return err
		}
	}

	return nil
}

// defaultFIRPacked is the pre-packed wire form of
// defaultFIRCoefficients. Computed once at package init so the hot
// path in writeDefaultFIR is a pure register loop without an
// unreachable error branch.
//
//nolint:gochecknoglobals // immutable register-table cache.
var defaultFIRPacked = mustPackFIRCoefficients(defaultFIRCoefficients)

// mustPackFIRCoefficients wraps packFIRCoefficients for the
// package-init path: a packing failure here means defaultFIRCoefficients
// was edited into an invalid range, which is a programming error to
// fail fast on, not a runtime condition.
func mustPackFIRCoefficients(taps [firTapCount]int16) [firTotalByteCount]byte {
	packed, err := packFIRCoefficients(taps)
	if err != nil {
		panic(err)
	}

	return packed
}

// writeDefaultFIR programs the default 16-tap FIR into the chip.
// The FIR determines the chip's anti-alias filter at the downsample
// stage; the default keeps ~1 MHz signal bandwidth usable, which is
// enough for ADS-B Mode S at 2.4 MS/s.
func (r *rtl2832u) writeDefaultFIR() error {
	for i, val := range defaultFIRPacked {
		addr := firBaseAddr + uint16(i)
		if err := r.demodWriteByte(demodPage1, addr, val); err != nil {
			return err
		}
	}

	return nil
}

// configureSDRMode enables the SDR datapath and disables the digital
// AGC bit on regDemodSDRMode (page 0). Bit layout follows librtlsdr's
// comment "enable SDR mode, disable DAGC (bit 5)"; the chip
// datasheet is not public so the bit-by-bit semantics are inferred
// from the upstream driver rather than authoritative.
func (r *rtl2832u) configureSDRMode() error {
	return r.demodWriteByte(demodPage0, regDemodSDRMode, sdrModeOn)
}

// initFSMState seeds the demod's finite-state-machine holding
// registers (page 1). Values are librtlsdr's hard-coded defaults;
// the chip won't track signal correctly without them.
func (r *rtl2832u) initFSMState() error {
	if err := r.demodWriteByte(demodPage1, regDemodFSM1, fsmReg1Init); err != nil {
		return err
	}

	return r.demodWriteByte(demodPage1, regDemodFSM2, fsmReg2Init)
}

// disableDemodAGC clears en_dagc (bit 0 of regDemodAGCEnable). We
// disable because tuner-side AGC (configured later by the tuner
// driver) gives better dynamic range for narrow-band signals like
// Mode S than the demod's coarser digital AGC.
func (r *rtl2832u) disableDemodAGC() error {
	return r.demodWriteByte(demodPage1, regDemodAGCEnable, dagcDisabled)
}

// disableRFIFAGC turns OFF the demod's RF and IF AGC loops. With
// the R820T2 attached the loops do more harm than good: the IF
// loop drives the chip's IF_AGC pin while the R820T2's VGA in
// auto mode reads it. Both ends try to converge against each
// other and the VGA gain wanders — host-side decoder yield
// collapses to ~zero clean Mode S frames. librtlsdr disables both
// loops on the R820T path; we mirror that.
//
// SignalStats consequence: with the loops off the chip's
// if_agc_val / rf_agc_val registers stop tracking a live AGC
// loop, and the AAGCLocked flag rarely flips. Diagnostics that
// rely on those values become uninformative; the upside is the
// radio actually decodes signal.
func (r *rtl2832u) disableRFIFAGC() error {
	return r.demodWriteByte(demodPage1, regDemodAGCLoop, rfifAGCDisabled)
}

// disablePIDFilter sets PID filter off (enable_PID = 0). PID
// filtering is a DVB-T artifact; SDR mode bypasses it.
func (r *rtl2832u) disablePIDFilter() error {
	return r.demodWriteByte(demodPage0, regDemodPIDFilter, pidFilterDisabled)
}

// configureADCDatapath sets opt_adc_iq=0 (default ADC_I/ADC_Q
// routing).
func (r *rtl2832u) configureADCDatapath() error {
	return r.demodWriteByte(demodPage0, regDemodADCDatapath, adcDatapathDefault)
}

// enableZeroIF enables Zero-IF mode (en_bbin), DC cancellation
// (en_dc_est), and IQ estimation/compensation (en_iq_comp,
// en_iq_est) on regDemodZeroIF.
func (r *rtl2832u) enableZeroIF() error {
	return r.demodWriteByte(demodPage1, regDemodZeroIF, zeroIFEnabled)
}

// disableClockOutput silences the 4.096 MHz reference clock the chip
// can emit on pin 4. We never wire it externally, and leaving it on
// emits unwanted EMI near the antenna.
func (r *rtl2832u) disableClockOutput() error {
	return r.demodWriteByte(demodPage0, regDemodClockOutput, clockOutputDisabled)
}
