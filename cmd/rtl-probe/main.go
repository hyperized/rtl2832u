// Command rtl-probe is a small operator tool for verifying that a
// Realtek RTL2832U + R820T/R860 dongle is healthy on the host
// running the binary. It speaks pure Go through the rtl2832u
// driver — no librtlsdr dependency — so it cross-compiles to
// arm/arm64 and runs on devices like the uConsole without extra
// system packages.
//
// Three checks, all optional, all on stderr:
//
//   - device info: chip-init succeeds and the receiver opens at
//     the requested centre frequency / sample rate;
//   - IQ probe: stream `--probe-bytes` of interleaved uint8 IQ
//     through the URB ring and report mean/std of the I and Q
//     channels. Centre near 127.5 with std ~30–40 = healthy noise
//     floor, std near 0 = chip muted, std saturating = ADC
//     clipping. Replaces the SignalStats AGC readout used by
//     demod1090 --probe-stats: rtl2832u disables the demod's RF/IF
//     AGC loop on R820T silicon (it fights the tuner's internal
//     loops), so those registers are static-zero by design.
//   - IQ capture: read `--capture-bytes` of interleaved uint8 IQ
//     into the file at `--capture` (use `-` for stdout). The file
//     is rtl_sdr / dump1090 --ifile compatible.
//
// Designed to compose with demod1090: `rtl-probe --capture cap.iq`
// produces a file the demod1090 binary can replay end-to-end.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyperized/rtl2832u"
)

// version is overridden at link time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// errCaptureSecondsNegative is the sentinel for a sub-zero
// --capture-seconds value. Wrapped (not returned bare) so callers
// keep the formatted detail (got X) while errors.Is checks work.
var errCaptureSecondsNegative = errors.New("--capture-seconds must be >= 0")

// gainStageFlagAGC is the magic value meaning "leave this stage on
// AGC". Negative numbers are rejected by R860 step validation, so
// reusing -1 (which is also rtl2832u.GainAGC) is unambiguous.
const gainStageFlagAGC = -1

// captureChunkSize is the read granularity for both probe and
// capture. 32 KiB matches one URB-ring iteration on the linux
// backend, so each Read returns the freshest data without
// coalescing across kernel boundaries.
const captureChunkSize = 32 * 1024

// defaultProbeBytes is the IQ volume the probe streams when
// computing channel statistics. 128 KiB ≈ 27 ms at 2.4 MS/s, more
// than enough samples for a stable mean/std estimate while still
// fast enough that the operator notices a hung dongle within a
// second.
const defaultProbeBytes = 4 * captureChunkSize

// defaultCaptureBytes is conservative: 64 KiB at 2.4 MS/s is ~14 ms
// of IQ — enough to verify the path produces non-trivial samples
// without wasting disk on a casual probe.
const defaultCaptureBytes = 64 * 1024

// config holds the parsed CLI flags. Pulling them into a struct
// keeps run() tractable and lets parseConfig be tested in
// isolation without spinning up a flag parser per test case.
type config struct {
	showVersion bool

	centerFreqHz uint32
	sampleRateHz uint32
	deviceIndex  int

	gainTenthsDB  int
	lnaGainStep   int
	mixerGainStep int
	vgaGainStep   int
	autoGain      bool

	freqCorrectionPPM int
	biasTee           int

	probeBytes int

	capturePath    string
	captureBytes   int
	captureSeconds int

	skipProbe bool
	tui       bool
}

// biasTeeUnset is the sentinel for --bias-tee meaning "leave the
// chip at its boot default". The flag is otherwise 0 (off) or 1
// (on); -1 is safely out of range.
const biasTeeUnset = -1

// parseConfig wires the Go flag package to a config. Returns the
// parsed config plus any flag.Parse error (caller maps to exit
// code 2 — the conventional "usage" exit).
func parseConfig(args []string, stderr io.Writer) (config, error) {
	var cfg config

	flagSet := flag.NewFlagSet("rtl-probe", flag.ContinueOnError)
	flagSet.SetOutput(stderr)

	var (
		center uint64
		rate   uint64
	)

	flagSet.BoolVar(&cfg.showVersion, "version", false, "print version and exit")
	flagSet.Uint64Var(&center, "center-freq", uint64(rtl2832u.DefaultCenterFreqHz), "centre frequency in Hz")
	flagSet.Uint64Var(&rate, "sample-rate", uint64(rtl2832u.DefaultSampleRateHz), "sample rate in Hz")
	flagSet.IntVar(&cfg.deviceIndex, "device", 0, "device index (0 = first dongle)")
	flagSet.IntVar(&cfg.gainTenthsDB, "gain", rtl2832u.GainAGC,
		"librtlsdr-style tuner gain in tenths of dB (e.g. 496 for 49.6 dB); -1 = AGC")
	flagSet.IntVar(&cfg.lnaGainStep, "lna-gain", gainStageFlagAGC,
		"LNA gain step 0..15 (overrides --gain); -1 = AGC")
	flagSet.IntVar(&cfg.mixerGainStep, "mix-gain", gainStageFlagAGC,
		"mixer gain step 0..15 (overrides --gain); -1 = AGC")
	flagSet.IntVar(&cfg.vgaGainStep, "vga-gain", gainStageFlagAGC,
		"VGA gain step 0..15 (overrides --gain); -1 = AGC")
	flagSet.BoolVar(&cfg.autoGain, "auto-gain", false,
		"run rtl2832u's closed-loop gain search at Open. Overrides --gain and the per-stage flags.")
	flagSet.IntVar(&cfg.freqCorrectionPPM, "ppm", 0,
		"TCXO frequency correction in ppm; positive = crystal runs fast. Clamped to ±1000 ppm.")
	flagSet.IntVar(&cfg.biasTee, "bias-tee", biasTeeUnset,
		"power the dongle's bias-tee output on GPIO0: 0=off, 1=on; -1=leave at chip default.")
	flagSet.IntVar(&cfg.probeBytes, "probe-bytes", defaultProbeBytes,
		"IQ bytes to stream when computing channel statistics; "+
			"set 0 to skip the probe entirely (combine with --capture for capture-only runs)")
	flagSet.StringVar(&cfg.capturePath, "capture", "",
		"optional path to write captured IQ to (use '-' for stdout). "+
			"Format: interleaved uint8 IQ, rtl_sdr / dump1090 --ifile compatible.")
	flagSet.IntVar(&cfg.captureBytes, "capture-bytes", defaultCaptureBytes,
		"number of IQ bytes to capture when --capture is set; rounded up to a 32 KiB multiple internally")
	flagSet.IntVar(&cfg.captureSeconds, "capture-seconds", 0,
		"capture duration in seconds; when >0 overrides --capture-bytes "+
			"(bytes = seconds × sample-rate × 2 for UC8 interleaved)")
	flagSet.BoolVar(&cfg.skipProbe, "no-probe", false,
		"skip the AGC SignalStats probe; useful when you only want a capture")
	flagSet.BoolVar(&cfg.tui, "tui", false,
		"open a live magnitude-histogram + strip-chart TUI instead of running the "+
			"one-shot probe/capture. Useful for diagnosing gain regime and chain stability "+
			"interactively. Keys: l/L m/M v/V step LNA/Mixer/VGA, b toggle bias-tee, "+
			"a auto-tune LNA, s 3D gain sweep, q or Esc quits.")

	if err := flagSet.Parse(args); err != nil {
		return cfg, fmt.Errorf("flag parse: %w", err)
	}

	cfg.centerFreqHz = uint32(center) //nolint:gosec // user-supplied Hz; chip max ≤ 1.766 GHz fits uint32.
	cfg.sampleRateHz = uint32(rate)   //nolint:gosec // sample rate ≤ 3.2 MS/s fits uint32.

	if cfg.captureSeconds < 0 {
		_, _ = fmt.Fprintf(stderr, "rtl-probe: --capture-seconds must be >= 0, got %d\n", cfg.captureSeconds)

		return cfg, fmt.Errorf("%w: got %d", errCaptureSecondsNegative, cfg.captureSeconds)
	}

	if cfg.captureSeconds > 0 {
		const bytesPerSample = 2 // UC8: interleaved uint8 I and Q

		cfg.captureBytes = int(int64(cfg.captureSeconds) * int64(cfg.sampleRateHz) * bytesPerSample)
	}

	return cfg, nil
}

// receiver is the slice of *rtl2832u.Receiver rtl-probe needs.
// Hoisted to an interface so run() can be tested with a fake
// without opening real silicon.
type receiver interface {
	Read(ctx context.Context, p []byte) (int, error)
	ReadSampleStats(ctx context.Context, targetSamples int) (rtl2832u.SampleStats, error)
	SetLNAGain(step uint8) error
	SetMixerGain(step uint8) error
	SetVGAGain(step uint8) error
	SetBiasTee(enable bool) error
	Close() error
}

// opener constructs a receiver from a config. The default
// implementation calls rtl2832u.Open; tests inject a fake.
type opener func(cfg config) (receiver, error)

// defaultOpener wraps rtl2832u.Open so the production path keeps
// using the real driver while tests substitute a stub.
//
//nolint:ireturn // factory: returning the interface is the seam tests rely on.
func defaultOpener(cfg config) (receiver, error) {
	rcv, err := rtl2832u.Open(buildOptions(cfg)...)
	if err != nil {
		return nil, fmt.Errorf("rtl-probe: open: %w", err)
	}

	return rcv, nil
}

// buildOptions assembles the full rtl2832u.Option slice from the
// parsed config. Order matches functional-options last-wins
// semantics: base, then gain, then bias-tee.
func buildOptions(cfg config) []rtl2832u.Option {
	const baseCount = 4 // center, rate, device, ppm

	opts := make([]rtl2832u.Option, 0, baseCount)
	opts = append(opts,
		rtl2832u.WithCenterFreq(cfg.centerFreqHz),
		rtl2832u.WithSampleRate(cfg.sampleRateHz),
		rtl2832u.WithDevice(cfg.deviceIndex),
		rtl2832u.WithFrequencyCorrection(cfg.freqCorrectionPPM),
	)

	opts = append(opts, gainOptions(cfg)...)

	if cfg.biasTee != biasTeeUnset {
		opts = append(opts, rtl2832u.WithBiasTee(cfg.biasTee != 0))
	}

	return opts
}

// gainOptions converts the parsed gain flags to rtl2832u.Options.
// --auto-gain shorts the per-stage flags out; otherwise the base
// WithGain ladder is layered with any per-stage overrides.
func gainOptions(cfg config) []rtl2832u.Option {
	if cfg.autoGain {
		return []rtl2832u.Option{rtl2832u.WithAutoGain()}
	}

	const maxOpts = 4

	opts := make([]rtl2832u.Option, 0, maxOpts)
	opts = append(opts, rtl2832u.WithGain(cfg.gainTenthsDB))

	if cfg.lnaGainStep != gainStageFlagAGC {
		stage := rtl2832u.ManualGainStep(uint8(cfg.lnaGainStep)) //nolint:gosec // bounded by ManualGainStep clamp.
		opts = append(opts, rtl2832u.WithLNAGain(stage))
	}

	if cfg.mixerGainStep != gainStageFlagAGC {
		stage := rtl2832u.ManualGainStep(uint8(cfg.mixerGainStep)) //nolint:gosec // bounded by ManualGainStep clamp.
		opts = append(opts, rtl2832u.WithMixerGain(stage))
	}

	if cfg.vgaGainStep != gainStageFlagAGC {
		stage := rtl2832u.ManualGainStep(uint8(cfg.vgaGainStep)) //nolint:gosec // bounded by ManualGainStep clamp.
		opts = append(opts, rtl2832u.WithVGAGain(stage))
	}

	return opts
}

// run is the real entry point — main() just calls os.Exit(run(...))
// so tests can drive it with their own argv and capture stdout/
// stderr without forking a subprocess.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, open opener) int {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		return exitUsage
	}

	if cfg.showVersion {
		_, _ = fmt.Fprintln(stdout, version)

		return exitOK
	}

	if cfg.captureBytes <= 0 {
		_, _ = fmt.Fprintln(stderr, "rtl-probe: --capture-bytes must be positive")

		return exitUsage
	}

	if cfg.probeBytes < 0 {
		_, _ = fmt.Fprintln(stderr, "rtl-probe: --probe-bytes must be >= 0")

		return exitUsage
	}

	rcv, err := open(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "rtl-probe: %v\n", err)

		return exitOpenFailed
	}

	defer func() {
		if cerr := rcv.Close(); cerr != nil {
			_, _ = fmt.Fprintf(stderr, "rtl-probe: close: %v\n", cerr)
		}
	}()

	_, _ = fmt.Fprintf(stderr,
		"rtl-probe: opened device=%d center=%d Hz rate=%d Hz ppm=%d\n",
		cfg.deviceIndex, cfg.centerFreqHz, cfg.sampleRateHz, cfg.freqCorrectionPPM)

	if cfg.tui {
		return runTUI(ctx, rcv, cfg.sampleRateHz, stderr)
	}

	if !cfg.skipProbe {
		if rc := probeIQ(ctx, rcv, cfg, stderr); rc != exitOK {
			return rc
		}
	}

	if cfg.capturePath != "" {
		if rc := captureIQ(ctx, rcv, cfg, stdout, stderr); rc != exitOK {
			return rc
		}
	}

	return exitOK
}

// iqStats is the streaming accumulator probeIQ folds the per-byte
// IQ stream into. Sums and sums-of-squares are kept as uint64;
// for our 8-bit samples and probeBytes ≤ 1 GiB the worst-case
// sumSq stays well under 2^60 (255² × 5×10⁸ ≈ 3.3×10¹³), so no
// overflow risk in any realistic operator-supplied size.
type iqStats struct {
	sumI, sumQ     uint64
	sumSqI, sumSqQ uint64
	countI, countQ uint64
}

// fold ingests a chunk of interleaved unsigned-8-bit IQ. Even
// indices are I, odd are Q (rtl_sdr / dump1090 convention).
func (s *iqStats) fold(buf []byte) {
	for i, sample := range buf {
		val := uint64(sample)
		if i&1 == 0 {
			s.sumI += val
			s.sumSqI += val * val
			s.countI++
		} else {
			s.sumQ += val
			s.sumSqQ += val * val
			s.countQ++
		}
	}
}

// iqSummary is the per-channel mean and population stddev pair
// summarise produces. Returning a struct (rather than four named
// floats) keeps the function signature inside revive's
// function-result-limit budget and makes the call site read
// naturally.
type iqSummary struct {
	MeanI, StdI float64
	MeanQ, StdQ float64
}

// summarise returns mean and population stddev for I and Q. Std
// is clamped at zero to absorb floating-point round-off when the
// signal is constant (variance = E[X²] - (E[X])² is exact in
// reals but can land slightly negative when the two terms agree
// on every bit but the last).
func (s *iqStats) summarise() iqSummary {
	out := iqSummary{}

	if s.countI > 0 {
		out.MeanI = float64(s.sumI) / float64(s.countI)
		varI := float64(s.sumSqI)/float64(s.countI) - out.MeanI*out.MeanI
		out.StdI = math.Sqrt(math.Max(0, varI))
	}

	if s.countQ > 0 {
		out.MeanQ = float64(s.sumQ) / float64(s.countQ)
		varQ := float64(s.sumSqQ)/float64(s.countQ) - out.MeanQ*out.MeanQ
		out.StdQ = math.Sqrt(math.Max(0, varQ))
	}

	return out
}

// probeIQ streams cfg.probeBytes of IQ through the URB ring,
// computes per-channel mean and stddev, and prints one summary
// line on stderr. Centre near 127.5 with std ~30–40 indicates a
// healthy noise floor; std near 0 means the chip is muted; std
// saturating means the front-end is overgained and the ADC is
// clipping.
//
// Returns exitOK on success or context cancellation, exitProbeFailed
// on any non-cancel Read error.
func probeIQ(ctx context.Context, rcv receiver, cfg config, stderr io.Writer) int {
	if cfg.probeBytes <= 0 {
		return exitOK
	}

	buf := make([]byte, captureChunkSize)
	stats := iqStats{}
	streamed := 0

	for streamed < cfg.probeBytes {
		count, err := rcv.Read(ctx, buf)
		if count > 0 {
			toFold := count
			if remaining := cfg.probeBytes - streamed; remaining < toFold {
				toFold = remaining
			}

			stats.fold(buf[:toFold])
			streamed += toFold
		}

		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}

			_, _ = fmt.Fprintf(stderr, "rtl-probe: probe read: %v\n", err)

			return exitProbeFailed
		}
	}

	if stats.countI == 0 {
		// No samples gathered — caller (e.g. ctx canceled before
		// the first read returned data) gets an empty probe but
		// no error.
		return exitOK
	}

	summary := stats.summarise()

	_, _ = fmt.Fprintf(stderr,
		"iq_stats bytes=%d mean_i=%.1f std_i=%.1f mean_q=%.1f std_q=%.1f\n",
		streamed, summary.MeanI, summary.StdI, summary.MeanQ, summary.StdQ)

	return exitOK
}

// captureIQ pulls cfg.captureBytes of IQ from the receiver and
// writes them to cfg.capturePath ("-" = stdout). The buffer is
// rounded up to a 32 KiB multiple to match the URB-ring chunk
// size; the surplus is dropped so the on-disk byte count matches
// the requested length within one chunk's worth of slack.
func captureIQ(ctx context.Context, rcv receiver, cfg config, stdout, stderr io.Writer) int {
	out, closeOut, err := openCaptureSink(cfg.capturePath, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "rtl-probe: %v\n", err)

		return exitCaptureFailed
	}

	defer closeOut()

	buf := make([]byte, captureChunkSize)
	written := 0

	for written < cfg.captureBytes {
		count, rerr := rcv.Read(ctx, buf)
		if count > 0 {
			toWrite := count
			if remaining := cfg.captureBytes - written; remaining < toWrite {
				toWrite = remaining
			}

			if _, werr := out.Write(buf[:toWrite]); werr != nil {
				_, _ = fmt.Fprintf(stderr, "rtl-probe: write capture: %v\n", werr)

				return exitCaptureFailed
			}

			written += toWrite
		}

		if rerr != nil {
			if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
				break
			}

			_, _ = fmt.Fprintf(stderr, "rtl-probe: read: %v\n", rerr)

			return exitCaptureFailed
		}
	}

	_, _ = fmt.Fprintf(stderr, "rtl-probe: captured %d bytes to %s\n", written, captureSinkLabel(cfg.capturePath))

	return exitOK
}

// openCaptureSink resolves "-" to stdout (no-op closer) and any
// other path to a freshly created file. The closer surfaces close
// errors via stderr so caller stays straight-line.
func openCaptureSink(path string, stdout io.Writer) (io.Writer, func(), error) {
	if path == "-" {
		return stdout, func() {}, nil
	}

	const fileMode = 0o600

	//nolint:gosec // G304: capture path is operator-supplied by design (this is a CLI flag).
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode)
	if err != nil {
		return nil, nil, fmt.Errorf("create capture file: %w", err)
	}

	return file, func() { _ = file.Close() }, nil
}

// captureSinkLabel renders the path for the operator log: stdout
// gets a friendly name; otherwise echo the literal path.
func captureSinkLabel(path string) string {
	if path == "-" {
		return "stdout"
	}

	return path
}

// Exit codes. Stable for scripting.
const (
	exitOK            = 0
	exitUsage         = 2
	exitOpenFailed    = 3
	exitProbeFailed   = 4
	exitCaptureFailed = 5
)

func main() {
	os.Exit(realMain())
}

// realMain isolates os.Exit from the deferred cancel: gocritic's
// exitAfterDefer rule fires when os.Exit and defer share a frame,
// because the defer never runs. Splitting the entry point keeps
// the deferred cancel honoured on every return path.
func realMain() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultOpener)
}
