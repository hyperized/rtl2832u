package rtl2832u

import (
	"errors"
	"fmt"
)

// R860 gain control
// =================
//
// The R860 has three independently-programmable gain stages along
// the signal path (datasheet §1 block diagram + §6 table 6-3):
//
//   LNA → Mixer (post-mixer amp) → VGA (variable-gain amp)
//
// Each stage has an "auto" mode (driven by an internal AGC loop
// fed by the chip's power detectors) and a "manual" mode (driven
// by a 4-bit gain code in I²C registers). Switching to manual
// disables the corresponding AGC loop and pins the gain to the
// programmed code.
//
// Register layout (datasheet §6 table 6-2 / 6-3):
//
//   R5  (0x05) bit [4]   LNA_GAIN_MODE   0=auto, 1=manual
//   R5  (0x05) bits [3:0] LNA_GAIN[3:0]  0000=min .. 1111=max
//   R7  (0x07) bit [4]   MIXGAIN_MODE    0=manual, 1=auto  ← inverse polarity
//   R7  (0x07) bits [3:0] MIX_GAIN[3:0]  0000=min .. 1111=max
//   R12 (0x0C) bit [4]   VGA_MODE        0=VGA_CODE controls (I²C),
//                                         1=IF_AGC pin voltage controls
//   R12 (0x0C) bits [3:0] VGA_CODE[3:0]  0000=-12.0 dB .. 1111=+40.5 dB
//                                         (3.5 dB per step, 16 steps)
//
// LNA_GAIN_MODE and VGA_MODE/MIXGAIN_MODE have inverse polarity in
// the datasheet; the helpers below normalise that so the public
// surface reads consistently regardless of stage.
//
// Datasheet uses "VGA controlled by IF_AGC pin" as the default — the
// RTL2832U drives that pin from its DAGC. Switching VGA_MODE to 0
// hands gain control entirely to the host via VGA_CODE.

// Register addresses, field bits, and step counts. Names match the
// datasheet's symbol column so cross-referencing stays trivial.
const (
	regR860LNAGain   uint8 = 0x05 // R5: LNA_GAIN_MODE + LNA_GAIN[3:0]
	regR860MixerGain uint8 = 0x07 // R7: MIXGAIN_MODE + MIX_GAIN[3:0]
	regR860VGAGain   uint8 = 0x0c // R12: VGA_MODE + VGA_CODE[3:0]

	// Mode-bit masks for each stage. The bit position is the same
	// across all three (bit 4) but the polarity differs — see the
	// helpers for the normalisation logic.
	maskR860LNAGainMode   uint8 = 1 << 4
	maskR860MixerGainMode uint8 = 1 << 4
	maskR860VGAMode       uint8 = 1 << 4

	// 4-bit gain codes occupy bits [3:0] of each stage's register.
	maskR860GainCode uint8 = 0x0f

	// r860GainStepCount is the cardinality of every stage's 4-bit
	// gain code: 16 levels per stage (datasheet §6 table 6-3).
	r860GainStepCount uint8 = 16

	// r860VGAStepCenti is the VGA's per-step gain in hundredths of
	// a dB (3.5 dB → 350). VGA_CODE 0000 corresponds to r860VGABaseCenti.
	// Only the VGA has a documented dB scale; LNA and Mixer expose
	// raw step indices because their dB tables are not in the public
	// R860 datasheet.
	r860VGAStepCenti int = 350
	r860VGABaseCenti int = -1200
)

// ErrR860GainStepRange is the static sentinel for a gain-step index
// outside [0, r860GainStepCount).
var ErrR860GainStepRange = errors.New("r860: gain step index out of range [0, 16)")

// setLNAGainManual switches the LNA into manual mode and pins its
// gain code to step. step must be in [0, 16); 0 is the lowest gain,
// 15 is the highest.
//
// Caller must hold the chip's I2C repeater open (withRepeater).
func (t *R860) setLNAGainManual(step uint8) error {
	if step >= r860GainStepCount {
		return fmt.Errorf("%w: LNA step=%d", ErrR860GainStepRange, step)
	}

	// LNA_GAIN_MODE=1 (manual) | LNA_GAIN[3:0] = step.
	value := maskR860LNAGainMode | (step & maskR860GainCode)
	mask := maskR860LNAGainMode | maskR860GainCode

	if err := t.writeRegisterMasked(regR860LNAGain, value, mask); err != nil {
		return fmt.Errorf("r860: set LNA manual gain step=%d: %w", step, err)
	}

	return nil
}

// setLNAGainAuto returns the LNA to AGC-driven gain control by
// clearing LNA_GAIN_MODE. The LNA gain code in the lower nibble is
// left untouched — the chip's AGC loop overrides it while the mode
// bit is auto.
func (t *R860) setLNAGainAuto() error {
	if err := t.writeRegisterMasked(regR860LNAGain, 0, maskR860LNAGainMode); err != nil {
		return fmt.Errorf("r860: set LNA auto gain: %w", err)
	}

	return nil
}

// setMixerGainManual switches the mixer into manual mode and pins
// its gain code to step. step must be in [0, 16). The MIXGAIN_MODE
// bit polarity is inverse to the LNA's: 0 = manual.
func (t *R860) setMixerGainManual(step uint8) error {
	if step >= r860GainStepCount {
		return fmt.Errorf("%w: mixer step=%d", ErrR860GainStepRange, step)
	}

	// MIXGAIN_MODE=0 (manual) | MIX_GAIN[3:0] = step.
	value := step & maskR860GainCode
	mask := maskR860MixerGainMode | maskR860GainCode

	if err := t.writeRegisterMasked(regR860MixerGain, value, mask); err != nil {
		return fmt.Errorf("r860: set mixer manual gain step=%d: %w", step, err)
	}

	return nil
}

// setMixerGainAuto returns the mixer to AGC-driven control by
// setting MIXGAIN_MODE = 1 (auto polarity is opposite to the LNA's).
func (t *R860) setMixerGainAuto() error {
	if err := t.writeRegisterMasked(regR860MixerGain, maskR860MixerGainMode, maskR860MixerGainMode); err != nil {
		return fmt.Errorf("r860: set mixer auto gain: %w", err)
	}

	return nil
}

// setVGAGainManual switches the VGA from IF_AGC pin control to I²C
// register control and pins VGA_CODE to step. step must be in
// [0, 16); the resulting VGA gain in centi-dB is
// r860VGABaseCenti + step·r860VGAStepCenti.
func (t *R860) setVGAGainManual(step uint8) error {
	if step >= r860GainStepCount {
		return fmt.Errorf("%w: VGA step=%d", ErrR860GainStepRange, step)
	}

	// VGA_MODE=0 (I²C controls) | VGA_CODE[3:0] = step.
	value := step & maskR860GainCode
	mask := maskR860VGAMode | maskR860GainCode

	if err := t.writeRegisterMasked(regR860VGAGain, value, mask); err != nil {
		return fmt.Errorf("r860: set VGA manual gain step=%d: %w", step, err)
	}

	return nil
}

// setVGAGainAuto hands the VGA back to the IF_AGC pin (the
// RTL2832U's DAGC drives it, exactly as librtlsdr leaves it by
// default).
func (t *R860) setVGAGainAuto() error {
	if err := t.writeRegisterMasked(regR860VGAGain, maskR860VGAMode, maskR860VGAMode); err != nil {
		return fmt.Errorf("r860: set VGA auto gain: %w", err)
	}

	return nil
}

// vgaGainCentiForStep returns the VGA's gain in centi-dB for a
// given VGA_CODE step. Exposed for tests and for any future
// gain-translation logic that needs to know the dB of a chosen
// step. step must be in [0, 16).
func vgaGainCentiForStep(step uint8) int {
	return r860VGABaseCenti + int(step)*r860VGAStepCenti
}

// VGAStepForCentiDB returns a manual GainStage that pins the VGA
// to the step whose programmed gain is closest to centiDB without
// exceeding it (floor-toward-negative-infinity quantisation). The
// VGA's documented range per R860 datasheet table 6-3 is
// -12.00 dB to +40.50 dB in 3.5 dB increments; values outside
// clamp to the boundary steps.
//
// Convenient when the user thinks in dB rather than in raw step
// indices. LNA and Mixer have no equivalent helper because their
// dB-per-step scale is not in the public datasheet.
func VGAStepForCentiDB(centiDB int) GainStage {
	return GainStage{Step: vgaStepForCentiDBClamped(centiDB)}
}

// vgaStepForCentiDBClamped is the package-internal worker that
// converts a centi-dB target to the corresponding VGA_CODE step,
// with the boundary clamps documented on VGAStepForCentiDB.
func vgaStepForCentiDBClamped(centiDB int) uint8 {
	if centiDB <= r860VGABaseCenti {
		return 0
	}

	step := (centiDB - r860VGABaseCenti) / r860VGAStepCenti
	if step >= int(r860GainStepCount) {
		return r860GainStepCount - 1
	}

	return uint8(step) //nolint:gosec // step bounded by the if above.
}

// SetLNAGain implements Tuner.SetLNAGain. Auto=true delegates the
// stage to the chip's LNA AGC loop (driven by the Det1 power
// detector, datasheet §5 Figure 5-2). Otherwise pins LNA_GAIN[3:0]
// to GainStage.Step.
func (t *R860) SetLNAGain(stage GainStage) error {
	return t.withRepeater(func() error {
		if stage.Auto {
			return t.setLNAGainAuto()
		}

		return t.setLNAGainManual(stage.Step)
	})
}

// SetMixerGain implements Tuner.SetMixerGain. The mixer's
// MIXGAIN_MODE polarity is inverse to the LNA's (datasheet table
// 6-3): 1 = auto, 0 = manual. The setMixerGain* primitives handle
// the inversion so this wrapper stays parallel to SetLNAGain.
func (t *R860) SetMixerGain(stage GainStage) error {
	return t.withRepeater(func() error {
		if stage.Auto {
			return t.setMixerGainAuto()
		}

		return t.setMixerGainManual(stage.Step)
	})
}

// SetVGAGain implements Tuner.SetVGAGain. Auto=true switches
// VGA_MODE so that the IF_AGC pin voltage (driven by the RTL2832U's
// digital AGC) controls VGA gain — the librtlsdr default and the
// chip's lowest-friction streaming mode. Manual pins VGA_CODE
// directly via I²C; combine with VGAStepForCentiDB to set in dB.
func (t *R860) SetVGAGain(stage GainStage) error {
	return t.withRepeater(func() error {
		if stage.Auto {
			return t.setVGAGainAuto()
		}

		return t.setVGAGainManual(stage.Step)
	})
}
