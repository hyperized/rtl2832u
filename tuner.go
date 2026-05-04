package rtl2832u

// Tuner is the front-end RF chip attached to the RTL2832U over its
// I2C bus. The interface stays minimal so adding implementations
// (R820T, R860, E4000, ...) is a single file.
//
// The chip driver calls Tuner during SetCenterFreq; tuner
// implementations talk back to the chip via the chip's I2C
// repeater. That plumbing lives in the tuner files themselves so
// the rtl2832u type stays unaware of which silicon it's tuning.
type Tuner interface {
	// Name returns a short human-readable identifier — "R820T",
	// "R860", "E4000". Used in log lines and error messages.
	Name() string

	// SetFreq retunes to the given centre frequency in Hz. The chip's
	// IF is normally Zero-IF (en_bbin=1 from Init), so the tuner is
	// programmed to mix the requested RF down to baseband. Range
	// and step depend on the implementation; most R820T-family
	// tuners cover roughly 24 MHz to 1.766 GHz.
	SetFreq(hz uint32) error

	// SetLNAGain, SetMixerGain, and SetVGAGain configure the tuner's
	// three independent gain stages. A GainStage with Auto=true hands
	// the stage to the chip's AGC; otherwise the stage is pinned to
	// the supplied 4-bit Step. Stages that don't apply to a given
	// silicon (e.g. tuners without a separate post-mixer amp) may be
	// implemented as no-ops or return ErrUnsupported.
	SetLNAGain(stage GainStage) error
	SetMixerGain(stage GainStage) error
	SetVGAGain(stage GainStage) error

	// SetIFBandwidth programs the tuner's channel-filter bandwidth.
	// coarse and fine are raw register step indices (0-3 and 0-15
	// on R820T-family silicon, mapping to relative widths the
	// public datasheet does not document in absolute Hz). Tuners
	// without a programmable channel filter may return ErrUnsupported.
	SetIFBandwidth(coarse, fine uint8) error

	// SetIFHighPass programs the channel filter's high-pass
	// corner. code is a 4-bit field whose values map to documented
	// (corner, attenuation) tuples on R820T-family silicon —
	// callers should use the R860HPF* constants rather than raw
	// numbers.
	SetIFHighPass(code uint8) error

	// SetFilterExt enables or disables the chip's "filter extension"
	// for weak-signal conditions. The exact mechanism is not
	// documented in the public datasheet; empirically toggling this
	// on a marginal chain may help.
	SetFilterExt(enable bool) error
}

// GainStage describes how one tuner gain stage operates.
//
// Auto=true delegates the stage to the chip's automatic gain
// control loop (driven by the on-die power detectors for LNA/Mixer,
// or by the demod's IF_AGC voltage for the VGA on R820T-family
// tuners). When Auto is false, Step pins the stage to the
// corresponding 4-bit gain code; valid values are 0..15.
type GainStage struct {
	Auto bool
	Step uint8
}

// AutoGain is the GainStage value that hands a stage back to the
// tuner's AGC. Equivalent to GainStage{Auto: true} but reads
// better at call sites.
//
//nolint:gochecknoglobals // immutable sentinel value.
var AutoGain = GainStage{Auto: true}

// ManualGainStep returns a GainStage pinning the stage to the given
// 4-bit code. Values outside [0, 15] clamp to the boundaries — a
// caller-supplied 99 is more useful as max-gain than as an error.
func ManualGainStep(step uint8) GainStage {
	const maxStep uint8 = 15

	if step > maxStep {
		step = maxStep
	}

	return GainStage{Step: step}
}

// i2cTransport is the minimum chip-side surface a Tuner needs. The
// rtl2832u satisfies it; tuner implementations and their tests are
// wired against this interface so the tuner side can be tested
// against a small mock without booting the full chip.
//
// enable/disable are split (rather than a single toggle taking
// bool) so call sites read like prose: enable, batch of ops,
// disable. Bundling minimises repeater chatter during demod
// activity.
type i2cTransport interface {
	enableI2CRepeater() error
	disableI2CRepeater() error
	i2cWrite(addr uint8, data []byte) error
	i2cRead(addr uint8, dst []byte) error
}
