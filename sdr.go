package rtl2832u

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// ErrUnsupportedPlatform reports that this build target has no USB backend.
// Linux is the only platform that ships a real implementation; darwin and
// other operating systems are buildable for editing/CI but cannot open a
// dongle. We return a typed sentinel rather than a string so callers can
// branch on it with errors.Is in cross-platform integration code.
var ErrUnsupportedPlatform = errors.New(
	"rtl2832u: USB backend is not implemented on this platform " +
		"(Linux is the only deploy target; on darwin use file replay or " +
		"run demod1090 inside a Linux container with /dev/bus/usb mounted)",
)

// ErrNoDevice reports that no RTL-SDR dongle was found at the requested
// device index. The Linux backend matches dongles by USB VID:PID against
// a known-good list (see sysfs.go); unknown clones must be added there
// before they will enumerate.
var ErrNoDevice = errors.New("rtl2832u: no RTL-SDR device found")

// Option configures the Receiver returned by Open. Configuration is set
// only at Open time; there are deliberately no setter methods on Receiver
// so that the open device's parameters cannot drift mid-stream.
type Option func(*config)

// config holds the resolved option values that Open hands to the backend.
// It is unexported because callers should never construct a Receiver
// without going through Open.
//
// Gain stages are stored individually so per-stage overrides (WithLNAGain
// / WithMixerGain / WithVGAGain) can layer cleanly on top of the
// convenience WithGain knob. Functional options apply in the order the
// caller passed them — last write to a stage wins.
//
// autoGain, if true, replaces the per-stage values at Open time by
// running the auto-tune search. The per-stage fields still capture
// the post-tune state so callers can read them back via SignalStats
// + tuner getters in a future iteration.
type config struct {
	centerFreqHz uint32
	sampleRateHz uint32
	deviceIndex  int

	lnaGain   GainStage
	mixerGain GainStage
	vgaGain   GainStage

	autoGain bool

	// IF filter overrides. The zero value of `*FilterSet` is "leave
	// the chip at its init-seed value"; only the explicit Options
	// flip the field to applied=true.
	ifBandwidth ifBandwidthSetting
	ifHighPass  ifHighPassSetting
	filterExt   filterExtSetting

	// Bias-tee. The zero value is "do not touch the chip's
	// bias-tee state"; WithBiasTee flips applied=true.
	biasTee biasTeeSetting

	// freqCorrectionPPM shifts the chip's effective reference
	// clock by ±ppm parts per million. Zero (the default) means
	// no correction. Both rsamp_ratio (sample rate) and the R860
	// PLL math (centre frequency) pick up the correction at
	// Open time via effectiveXtalHz.
	freqCorrectionPPM int32

	// logger receives the package's diagnostic output (currently
	// just the auto-tune converged-config line). Defaults to a
	// discard logger so the package is silent unless the caller
	// explicitly opts in via WithLogger. TUI consumers in
	// particular should keep the default — stdlib `log` writes
	// to stderr, which most TUI frameworks render onto the draw
	// surface.
	logger *slog.Logger

	// warnings accumulates messages emitted by options that
	// silently sanitised user input (e.g. clamping an out-of-range
	// frequency correction). Open flushes the slice through
	// cfg.logger.Warn after every option has run, so the warnings
	// appear before any I/O begins.
	warnings []string
}

// biasTeeSetting holds an optional bias-tee override. The chip's
// bias-tee defaults to off at boot on every dongle we've seen,
// but we still distinguish "user did not pass WithBiasTee" from
// "user passed WithBiasTee(false)" so an explicit `--bias-tee=false`
// is a deterministic disable rather than a no-op.
type biasTeeSetting struct {
	gpio    uint8
	enable  bool
	applied bool
}

// ifBandwidthSetting holds an optional override for FILT_BW
// (coarse) and FILT_CODE (fine). The applied flag distinguishes
// "user did not pass WithIFBandwidth" from "user passed
// WithIFBandwidth(0, 0)" so the open path can leave the chip at
// its seed values when the user hasn't asked for an override.
type ifBandwidthSetting struct {
	coarse  uint8
	fine    uint8
	applied bool
}

type ifHighPassSetting struct {
	code    uint8
	applied bool
}

type filterExtSetting struct {
	enable  bool
	applied bool
}

func defaultConfig() config {
	return config{
		centerFreqHz: DefaultCenterFreqHz,
		sampleRateHz: DefaultSampleRateHz,
		deviceIndex:  0,
		lnaGain:      AutoGain,
		mixerGain:    AutoGain,
		vgaGain:      AutoGain,
		logger:       slog.New(slog.DiscardHandler),
	}
}

// WithLogger directs the package's diagnostic output (currently
// the auto-tune converged-config line) to the given slog.Logger.
// Pass nil to silence — the same as the default. Useful for
// callers that want to tee the log into their structured-logging
// pipeline, and essential for TUI consumers that would otherwise
// have stdlib log output drawn over their UI.
func WithLogger(logger *slog.Logger) Option {
	return func(cfg *config) {
		if logger == nil {
			cfg.logger = slog.New(slog.DiscardHandler)

			return
		}

		cfg.logger = logger
	}
}

// WithCenterFreq overrides the centre frequency. The default targets
// 1090 MHz Mode S Extended Squitter; override only when reusing this
// receiver for other narrowband RF tasks.
func WithCenterFreq(hz uint32) Option {
	return func(c *config) { c.centerFreqHz = hz }
}

// WithSampleRate overrides the sample rate. The default of 2.4 MS/s gives
// 2.4 samples per Mode S bit, matching FlightAware dump1090. Lower rates
// reduce CPU but also degrade preamble selectivity and frame yield.
func WithSampleRate(hz uint32) Option {
	return func(c *config) { c.sampleRateHz = hz }
}

// WithGain is the librtlsdr-compatible single-knob gain control:
// it walks an empirically-calibrated table of LNA + Mixer step
// pairs to land as close to the requested tenths-of-a-dB target
// as the silicon allows, then pins the VGA at a fixed mid-band
// step (matches librtlsdr's r82xx_set_gain default).
//
// Pass GainAGC to hand all three stages back to the chip's AGC
// loops (the configured default).
//
// For finer control, use WithLNAGain / WithMixerGain / WithVGAGain
// after WithGain — per-stage options override the table lookup
// for the corresponding stage.
func WithGain(tenthsDB int) Option {
	return func(cfg *config) {
		if tenthsDB == GainAGC {
			cfg.lnaGain = AutoGain
			cfg.mixerGain = AutoGain
			cfg.vgaGain = AutoGain

			return
		}

		lnaStep, mixerStep := librtlsdrGainSteps(tenthsDB)
		cfg.lnaGain = ManualGainStep(lnaStep)
		cfg.mixerGain = ManualGainStep(mixerStep)
		cfg.vgaGain = ManualGainStep(librtlsdrManualVGAStep)
	}
}

// WithLNAGain overrides the LNA gain stage. Pass AutoGain for AGC,
// or ManualGainStep(0..15) to pin. Per-stage options layer on top
// of WithGain; a later option wins.
//
// ManualGainStep silently clamps out-of-range values; prefer
// WithManualLNAGain when the step comes from user input — that
// helper accumulates a Warn-level log entry when the value
// exceeds the chip's 4-bit code range, so the operator sees the
// substitution.
func WithLNAGain(stage GainStage) Option {
	return func(c *config) { c.lnaGain = stage }
}

// WithMixerGain overrides the post-mixer-amplifier gain stage.
// Same conventions as WithLNAGain; see WithManualMixerGain for
// the user-input-friendly variant that surfaces clamping.
func WithMixerGain(stage GainStage) Option {
	return func(c *config) { c.mixerGain = stage }
}

// WithVGAGain overrides the VGA stage. Pass AutoGain to track
// librtlsdr's r82xx_set_gain auto branch (VGA_CODE pinned at the
// 0x0b "AGC entry point" while the LNA + Mixer AGC loops ride
// above it — the IF_AGC pin path is not used because the demod's
// RF/IF AGC loop is disabled on R820T silicon to keep it from
// fighting the tuner's own loops). Or pass ManualGainStep(0..15)
// / VGAStepForCentiDB(centi) to pin. VGA_CODE 0..15 maps to
// -12.0 dB through +40.5 dB in 3.5 dB increments per R860
// datasheet table 6-3. See WithManualVGAGain for the variant
// that surfaces clamping.
func WithVGAGain(stage GainStage) Option {
	return func(c *config) { c.vgaGain = stage }
}

// gainStepMax is the upper bound of the R860's 4-bit gain code
// shared by every manual stage. Kept here (and re-asserted against
// r860GainStepCount in the tuner package) so the option layer
// never imports tuner-internal constants.
const gainStepMax uint8 = 15

// clampGainStep validates step against the R860's 4-bit code range
// and, if out of range, appends a Warn-level message to cfg.warnings
// describing the substitution. Returns the clamped step. The stage
// parameter is the human-readable name used in the warning ("LNA",
// "Mixer", "VGA") — keep it short, it ends up in operator logs.
func clampGainStep(cfg *config, stage string, step uint8) uint8 {
	if step <= gainStepMax {
		return step
	}

	cfg.warnings = append(cfg.warnings, fmt.Sprintf(
		"WithManual%sGain: step=%d clamped to %d (max)",
		stage, step, gainStepMax))

	return gainStepMax
}

// WithManualLNAGain pins the LNA to a 4-bit gain code, surfacing a
// Warn-level log entry through cfg.logger when step exceeds 15.
// Prefer this over WithLNAGain(ManualGainStep(...)) when step
// originates from user input (CLI flag, config file): the silent
// clamp inside ManualGainStep is hard to debug from logs.
func WithManualLNAGain(step uint8) Option {
	return func(cfg *config) {
		cfg.lnaGain = GainStage{Step: clampGainStep(cfg, "LNA", step)}
	}
}

// WithManualMixerGain pins the post-mixer-amplifier to a 4-bit gain
// code with the same range-warning behaviour as WithManualLNAGain.
func WithManualMixerGain(step uint8) Option {
	return func(cfg *config) {
		cfg.mixerGain = GainStage{Step: clampGainStep(cfg, "Mixer", step)}
	}
}

// WithManualVGAGain pins the VGA to a 4-bit VGA_CODE with the same
// range-warning behaviour as WithManualLNAGain. For dB-based input
// use VGAStepForCentiDB plus WithVGAGain — that helper does its
// own boundary clamp on dB rather than on the raw code.
func WithManualVGAGain(step uint8) Option {
	return func(cfg *config) {
		cfg.vgaGain = GainStage{Step: clampGainStep(cfg, "VGA", step)}
	}
}

// WithIFBandwidth overrides the R860's channel-filter
// bandwidth. coarse selects FILT_BW (0-3, 0=widest, 3=narrowest);
// fine selects FILT_CODE (0-15, 0=widest, 15=narrowest). The
// public datasheet does not document absolute Hz for these; the
// chip ships init-seed values that match librtlsdr's default
// (FILT_BW=3 narrow, FILT_CODE=6 mid). Override only after
// measuring frame yield against the seed values.
func WithIFBandwidth(coarse, fine uint8) Option {
	return func(c *config) {
		c.ifBandwidth = ifBandwidthSetting{coarse: coarse, fine: fine, applied: true}
	}
}

// WithIFHighPass overrides the R860's channel-filter high-pass
// corner. Use the R860HPF* constants from tuner_r860_filter.go
// (R860HPF5MHz down to R860HPF500kHz, plus the per-attenuation
// variants the datasheet table 6-3 documents).
func WithIFHighPass(code uint8) Option {
	return func(c *config) {
		c.ifHighPass = ifHighPassSetting{code: code, applied: true}
	}
}

// WithBiasTee toggles the dongle's bias-tee output on its
// conventional GPIO0 pin. Powers an external active LNA / filter
// from the antenna coax on V3-class dongles. No-op on dongles
// without a bias-tee circuit (the GPIO drives a high-impedance
// pin) — but harmless either way.
func WithBiasTee(enable bool) Option {
	return func(c *config) {
		c.biasTee = biasTeeSetting{
			gpio:    defaultBiasTeeGPIO,
			enable:  enable,
			applied: true,
		}
	}
}

// WithBiasTeeGPIO is the escape hatch for clones that wire the
// bias-tee to a non-default GPIO. Pass the GPIO index (0..7) plus
// the desired enable state.
func WithBiasTeeGPIO(gpio uint8, enable bool) Option {
	return func(c *config) {
		c.biasTee = biasTeeSetting{
			gpio:    gpio,
			enable:  enable,
			applied: true,
		}
	}
}

// WithFilterExt enables or disables the R860's "filter extension
// for weak signal conditions" (datasheet R30 bit [6]). The
// internal mechanism is undocumented; toggle it empirically.
func WithFilterExt(enable bool) Option {
	return func(c *config) {
		c.filterExt = filterExtSetting{enable: enable, applied: true}
	}
}

// WithAutoGain runs the auto-tune algorithm at Open time. The
// algorithm pins Mixer and VGA at maximum, then walks the LNA
// gain step downward until the chip's IF AGC signals it is no
// longer severely over-gained (datasheet §8.1.5). Converges in
// 1–3 iterations on most antenna chains.
//
// Layering: WithAutoGain takes precedence over any per-stage
// option set earlier in the option list, since the search needs
// a clean starting point. Per-stage options set *after*
// WithAutoGain in the option list still apply (last-wins) and
// disable auto-tune for those stages.
func WithAutoGain() Option {
	return func(c *config) {
		c.autoGain = true
		c.lnaGain = AutoGain
		c.mixerGain = AutoGain
		c.vgaGain = AutoGain
	}
}

// WithDevice selects a receiver by zero-based enumeration index. The order
// is the sysfs directory listing, which is stable per boot but not across
// reboots — pin by serial number once the EEPROM reader lands.
func WithDevice(index int) Option {
	return func(c *config) { c.deviceIndex = index }
}

// FrequencyCorrectionPPMMax is the magnitude clamp applied by
// WithFrequencyCorrection. ±1000 ppm matches librtlsdr's
// rtlsdr_set_freq_correction range — beyond that the rsamp_ratio
// math starts to round visibly and the tuner's PLL has to retune
// further than its lock tolerance comfortably allows.
const FrequencyCorrectionPPMMax = 1000

// WithFrequencyCorrection trims the chip's *effective* reference
// crystal by ppm parts per million. Both the demodulator's
// rsamp_ratio (sample rate) and the R860 tuner's PLL math (centre
// frequency) pick the correction up via effectiveXtalHz, so a
// single value compensates a drifty TCXO across the entire chain.
//
// Sign convention: positive ppm = crystal runs *fast* (an external
// reference reads the chip's clock above nominal). Pass the value
// you would write into librtlsdr's rtlsdr_set_freq_correction.
//
// Values outside ±FrequencyCorrectionPPMMax are clamped to the
// boundary; functional options can't return errors and the
// alternative (panicking) is a poor library citizen. Clamping is
// surfaced via the cfg.logger as a Warn-level entry from Open, so
// the operator sees the substitution even though no error is
// returned. Zero is the default and is a no-op.
func WithFrequencyCorrection(ppm int) Option {
	return func(cfg *config) {
		switch {
		case ppm > FrequencyCorrectionPPMMax:
			cfg.freqCorrectionPPM = FrequencyCorrectionPPMMax
			cfg.warnings = append(cfg.warnings, fmt.Sprintf(
				"WithFrequencyCorrection: ppm=%d clamped to +%d (max)",
				ppm, FrequencyCorrectionPPMMax))
		case ppm < -FrequencyCorrectionPPMMax:
			cfg.freqCorrectionPPM = -FrequencyCorrectionPPMMax
			cfg.warnings = append(cfg.warnings, fmt.Sprintf(
				"WithFrequencyCorrection: ppm=%d clamped to %d (min)",
				ppm, -FrequencyCorrectionPPMMax))
		default:
			cfg.freqCorrectionPPM = int32(ppm)
		}
	}
}

// Receiver represents an open RTL-SDR device. Read is single-producer:
// USB bulk endpoints serialise transfers, so a Receiver must not be
// shared between goroutines for reading. Close is safe to call from any
// goroutine and is idempotent.
type Receiver struct {
	cfg     config
	backend backend
}

// backend is the OS-specific transport. usbfs_linux.go provides the real
// implementation; usbfs_other.go returns ErrUnsupportedPlatform from
// openBackend so that go test ./... still works on darwin dev hosts.
//
// DroppedSampleChunks reports how many sample chunks the streaming
// path had to discard because the consumer fell behind. Exposed
// through the interface (rather than via type assertion) so the
// non-linux fallback can return a stable zero without callers
// needing platform-conditional code.
//
// SignalStats reads the chip's AGC state on demand. Exposed
// through the interface for the same reason as DroppedSampleChunks:
// keeps callers free of platform-conditional code.
//
// AutoTuneGain runs the gain auto-tune algorithm against the
// open device. Same rationale as the rest: per-platform
// implementations supply the chip+tuner refs, callers see one
// stable surface.
type backend interface {
	Read(ctx context.Context, p []byte) (int, error)
	Close() error
	DroppedSampleChunks() uint64
	SignalStats() (SignalStats, error)
	AutoTuneGain(ctx context.Context, opts AutoTuneOptions) (AutoTuneResult, error)
	SetLNAGain(step uint8) error
	SetMixerGain(step uint8) error
	SetVGAGain(step uint8) error
	SetBiasTee(enable bool) error
}

// Open enumerates RTL-SDR devices and opens the one at the configured
// index. The returned Receiver has a claimed USB interface and a
// chip-initialised demodulator. Tuning to a centre frequency requires
// a Tuner; bulk reads land once the URB ring is in place. Always
// Close the Receiver when done.
//
// Open flushes any warnings options accumulated (e.g. clamped
// frequency correction) through cfg.logger.Warn before touching
// hardware, so the operator sees substitutions even when no error
// is returned.
func Open(opts ...Option) (*Receiver, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	for _, warning := range cfg.warnings {
		cfg.logger.Warn(warning)
	}

	b, err := openBackend(cfg)
	if err != nil {
		return nil, err
	}

	return &Receiver{cfg: cfg, backend: b}, nil
}

// Read returns up to len(p) bytes of interleaved unsigned 8-bit
// IQ samples (I, Q, I, Q, ...) from the next completed URB. It
// blocks until a chunk arrives, the stream ends, or ctx is
// cancelled. A single Read maps to a single URB completion;
// callers needing fill-to-buffer semantics across multiple URBs
// must wrap with io.ReadFull. Buffer sizes between 16 KiB and
// 256 KiB match the URB sizes used by librtlsdr and dump1090.
//
// Receiver is single-producer: usbfs serialises bulk transfers,
// so concurrent calls to Read from multiple goroutines are not
// supported.
func (r *Receiver) Read(ctx context.Context, p []byte) (int, error) {
	n, err := r.backend.Read(ctx, p)
	if err != nil {
		return n, fmt.Errorf("sdr: read: %w", err)
	}

	return n, nil
}

// Close releases the USB interface and closes the device handle.
// Subsequent calls return the first close's error.
func (r *Receiver) Close() error {
	if err := r.backend.Close(); err != nil {
		return fmt.Errorf("sdr: close: %w", err)
	}

	return nil
}

// DroppedSampleChunks returns the cumulative count of sample
// chunks the streaming path had to discard because the consumer
// fell behind. A non-zero value over a long-running session
// indicates the demodulator is slower than 2.4 MS/s and frames
// are being missed; consider profiling the Process call or
// reducing chunk-size pressure.
func (r *Receiver) DroppedSampleChunks() uint64 {
	return r.backend.DroppedSampleChunks()
}

// SignalStats reports the chip's analogue AGC state at the moment
// of the call (RTL2832U datasheet §8.1.5). Useful for diagnostics
// and as input to gain-tuning logic; see the SignalStats type for
// scale and meaning.
//
// Sample stale and lock-volatile: the read is point-in-time, and
// the chip's AGC takes a few hundred milliseconds to settle after
// any gain or frequency change. Check AAGCLocked before drawing
// conclusions, or average over a window via AutoTuneGain when the
// signal source is bursty (ADS-B).
func (r *Receiver) SignalStats() (SignalStats, error) {
	stats, err := r.backend.SignalStats()
	if err != nil {
		return SignalStats{}, fmt.Errorf("rtl2832u: signal stats: %w", err)
	}

	return stats, nil
}

// AutoTuneGain runs the gain auto-tune algorithm: pin Mixer and
// VGA at maximum, walk the LNA gain step downward until the chip
// stops signalling severe over-gain. Returns the converged
// configuration and the IF AGC mean it observed there.
//
// Pass AutoTuneOptions{} for sensible defaults; override
// individual fields for tighter or looser control. The call
// blocks for as long as the algorithm runs (typically 1–3
// seconds, max ~16 seconds if the LNA has to walk all the way to
// zero).
func (r *Receiver) AutoTuneGain(ctx context.Context, opts AutoTuneOptions) (AutoTuneResult, error) {
	result, err := r.backend.AutoTuneGain(ctx, opts)
	if err != nil {
		return AutoTuneResult{}, fmt.Errorf("rtl2832u: auto-tune gain: %w", err)
	}

	return result, nil
}

// SetLNAGain reconfigures the tuner's LNA gain stage at runtime
// to the supplied step (0..15, R860 ladder). Safe to call while
// sample streaming is in progress — the write goes through a USB
// control transfer on the I²C path, separate from the bulk
// endpoint the URB ring uses. Values outside 0..15 are clamped
// by the tuner.
//
// Useful for interactive tuning: an operator UI can rebind keys
// to walk the LNA step up/down and observe the SampleStats /
// spectrum response live. The change takes effect on the next
// I/Q chunk that arrives; on the R820T2 the tuner's own settle
// time is well under one URB period.
func (r *Receiver) SetLNAGain(step uint8) error {
	if err := r.backend.SetLNAGain(step); err != nil {
		return fmt.Errorf("rtl2832u: set LNA gain: %w", err)
	}

	return nil
}

// SetMixerGain reconfigures the tuner's Mixer gain stage at
// runtime. Same safety / clamping notes as SetLNAGain.
func (r *Receiver) SetMixerGain(step uint8) error {
	if err := r.backend.SetMixerGain(step); err != nil {
		return fmt.Errorf("rtl2832u: set Mixer gain: %w", err)
	}

	return nil
}

// SetVGAGain reconfigures the tuner's VGA gain stage at runtime.
// Same safety / clamping notes as SetLNAGain.
func (r *Receiver) SetVGAGain(step uint8) error {
	if err := r.backend.SetVGAGain(step); err != nil {
		return fmt.Errorf("rtl2832u: set VGA gain: %w", err)
	}

	return nil
}

// SetBiasTee toggles the dongle's bias-tee GPIO at runtime,
// reusing the GPIO line selected at Open time (default 0). Safe
// to call while sample streaming is in progress.
func (r *Receiver) SetBiasTee(enable bool) error {
	if err := r.backend.SetBiasTee(enable); err != nil {
		return fmt.Errorf("rtl2832u: set bias-tee: %w", err)
	}

	return nil
}
