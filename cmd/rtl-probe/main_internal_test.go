package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hyperized/rtl2832u"
)

// fakeReceiver is the test double for the receiver interface. The
// FIFO mirrors how demod1090 tests sequence Read responses; the
// closeFn hook lets tests assert behaviour around Close errors
// without spawning real silicon.
type fakeReceiver struct {
	reads       []fakeRead
	statsResult rtl2832u.SampleStats
	statsErr    error
	statsCalls  int
	closeFn     func() error
	closed      int
}

type fakeRead struct {
	data []byte
	err  error
}

var (
	errFakeReadDry      = errors.New("fakeReceiver: Read queue exhausted")
	errSyntheticRead    = errors.New("synthetic read failure")
	errSyntheticClose   = errors.New("synthetic close failure")
	errSyntheticWriteIO = errors.New("synthetic write failure")
)

const (
	flagNoProbe       = "-no-probe"
	flagCapture       = "-capture"
	flagCaptureBytes  = "-capture-bytes"
	flagProbeBytes    = "-probe-bytes"
	flagBytesSmall    = "1024"
	captureSinkStdout = "-"
)

func (f *fakeReceiver) Read(_ context.Context, dst []byte) (int, error) {
	if len(f.reads) == 0 {
		return 0, errFakeReadDry
	}

	next := f.reads[0]
	f.reads = f.reads[1:]

	if next.err != nil {
		return 0, next.err
	}

	return copy(dst, next.data), nil
}

func (f *fakeReceiver) ReadSampleStats(_ context.Context, _ int) (rtl2832u.SampleStats, error) {
	f.statsCalls++

	return f.statsResult, f.statsErr
}

func (f *fakeReceiver) Close() error {
	f.closed++

	if f.closeFn != nil {
		return f.closeFn()
	}

	return nil
}

// stubOpener returns an opener that hands out the supplied
// receiver and never errors. Tests inject the receiver pre-loaded
// with the read sequence the path under test needs.
func stubOpener(rcv receiver) opener {
	return func(_ config) (receiver, error) { return rcv, nil }
}

// failingOpener returns an opener that always fails with err.
func failingOpener(err error) opener {
	return func(_ config) (receiver, error) { return nil, err }
}

// noProbeArgs disables the IQ probe and capture so the test only
// exercises the open / close branches.
func noProbeArgs() []string {
	return []string{flagNoProbe}
}

func TestParseConfigDefaults(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer

	cfg, err := parseConfig(nil, &stderr)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.centerFreqHz != rtl2832u.DefaultCenterFreqHz {
		t.Errorf("centerFreqHz = %d, want %d", cfg.centerFreqHz, rtl2832u.DefaultCenterFreqHz)
	}

	if cfg.sampleRateHz != rtl2832u.DefaultSampleRateHz {
		t.Errorf("sampleRateHz = %d, want %d", cfg.sampleRateHz, rtl2832u.DefaultSampleRateHz)
	}

	if cfg.gainTenthsDB != rtl2832u.GainAGC {
		t.Errorf("gainTenthsDB = %d, want %d (AGC)", cfg.gainTenthsDB, rtl2832u.GainAGC)
	}

	if cfg.lnaGainStep != gainStageFlagAGC || cfg.mixerGainStep != gainStageFlagAGC ||
		cfg.vgaGainStep != gainStageFlagAGC {
		t.Errorf("per-stage flags = (%d, %d, %d), want all %d",
			cfg.lnaGainStep, cfg.mixerGainStep, cfg.vgaGainStep, gainStageFlagAGC)
	}

	if cfg.probeBytes != defaultProbeBytes {
		t.Errorf("probeBytes = %d, want %d", cfg.probeBytes, defaultProbeBytes)
	}

	if cfg.captureBytes != defaultCaptureBytes {
		t.Errorf("captureBytes = %d, want %d", cfg.captureBytes, defaultCaptureBytes)
	}

	if cfg.biasTee != biasTeeUnset {
		t.Errorf("biasTee = %d, want %d", cfg.biasTee, biasTeeUnset)
	}
}

func TestParseConfigOverrides(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer

	cfg, err := parseConfig([]string{
		"-center-freq", "868000000",
		"-sample-rate", "1024000",
		"-device", "2",
		"-gain", "496",
		"-ppm", "-37",
		"-bias-tee", "1",
		flagProbeBytes, "65536",
		flagCapture, "out.iq",
		flagCaptureBytes, "131072",
	}, &stderr)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if cfg.centerFreqHz != 868_000_000 {
		t.Errorf("centerFreqHz = %d", cfg.centerFreqHz)
	}

	if cfg.sampleRateHz != 1_024_000 {
		t.Errorf("sampleRateHz = %d", cfg.sampleRateHz)
	}

	if cfg.deviceIndex != 2 {
		t.Errorf("deviceIndex = %d", cfg.deviceIndex)
	}

	if cfg.gainTenthsDB != 496 {
		t.Errorf("gainTenthsDB = %d", cfg.gainTenthsDB)
	}

	if cfg.freqCorrectionPPM != -37 {
		t.Errorf("ppm = %d", cfg.freqCorrectionPPM)
	}

	if cfg.biasTee != 1 {
		t.Errorf("biasTee = %d", cfg.biasTee)
	}

	if cfg.probeBytes != 65536 {
		t.Errorf("probeBytes = %d", cfg.probeBytes)
	}

	if cfg.capturePath != "out.iq" {
		t.Errorf("capturePath = %q", cfg.capturePath)
	}

	if cfg.captureBytes != 131072 {
		t.Errorf("captureBytes = %d", cfg.captureBytes)
	}
}

func TestParseConfigBadFlag(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer

	if _, err := parseConfig([]string{"-nope"}, &stderr); err == nil {
		t.Fatal("parseConfig: want error, got nil")
	}
}

func TestGainOptionsAGCDefaultIsBaseGainOnly(t *testing.T) {
	t.Parallel()

	cfg := config{
		gainTenthsDB:  rtl2832u.GainAGC,
		lnaGainStep:   gainStageFlagAGC,
		mixerGainStep: gainStageFlagAGC,
		vgaGainStep:   gainStageFlagAGC,
	}

	if got := len(gainOptions(cfg)); got != 1 {
		t.Errorf("len(gainOptions) = %d, want 1", got)
	}
}

func TestGainOptionsAutoGainShortCircuits(t *testing.T) {
	t.Parallel()

	cfg := config{
		autoGain:      true,
		gainTenthsDB:  420,
		lnaGainStep:   7,
		mixerGainStep: 3,
		vgaGainStep:   9,
	}

	if got := len(gainOptions(cfg)); got != 1 {
		t.Errorf("len(gainOptions) = %d, want 1 (auto-gain alone)", got)
	}
}

func TestGainOptionsLayersAllStages(t *testing.T) {
	t.Parallel()

	cfg := config{
		gainTenthsDB:  420,
		lnaGainStep:   7,
		mixerGainStep: 3,
		vgaGainStep:   9,
	}

	if got := len(gainOptions(cfg)); got != 4 {
		t.Errorf("len(gainOptions) = %d, want 4 (gain + LNA + Mixer + VGA)", got)
	}
}

func TestBuildOptionsBaseAlwaysHasFour(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	// 4 base + 1 gain (AGC) + 0 bias-tee = 5 minimum.
	if got := len(buildOptions(cfg)); got != 5 {
		t.Errorf("len(buildOptions) = %d, want 5", got)
	}
}

func TestBuildOptionsAddsBiasTeeWhenSet(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig([]string{"-bias-tee", "1"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	// 4 base + 1 gain + 1 bias-tee = 6.
	if got := len(buildOptions(cfg)); got != 6 {
		t.Errorf("len(buildOptions) = %d, want 6 (extra bias-tee)", got)
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), []string{"-version"}, &stdout, &stderr, defaultOpener); code != exitOK {
		t.Fatalf("run = %d, want %d", code, exitOK)
	}

	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Errorf("stdout = %q, want %q", got, version)
	}
}

func TestRunBadFlag(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), []string{"-nope"}, &stdout, &stderr, defaultOpener); code != exitUsage {
		t.Fatalf("run = %d, want %d", code, exitUsage)
	}
}

func TestRunBadCaptureBytes(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), []string{flagCaptureBytes, "0"}, &stdout, &stderr, defaultOpener)
	if code != exitUsage {
		t.Errorf("run = %d, want %d", code, exitUsage)
	}

	if !strings.Contains(stderr.String(), "capture-bytes") {
		t.Errorf("stderr missing diagnostic; got %q", stderr.String())
	}
}

func TestRunBadProbeBytes(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), []string{flagProbeBytes, "-1"}, &stdout, &stderr, defaultOpener)
	if code != exitUsage {
		t.Errorf("run = %d, want %d", code, exitUsage)
	}

	if !strings.Contains(stderr.String(), "probe-bytes") {
		t.Errorf("stderr missing diagnostic; got %q", stderr.String())
	}
}

func TestRunOpenFails(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	open := failingOpener(rtl2832u.ErrNoDevice)
	if code := run(t.Context(), nil, &stdout, &stderr, open); code != exitOpenFailed {
		t.Errorf("run = %d, want %d", code, exitOpenFailed)
	}

	if !strings.Contains(stderr.String(), "rtl-probe:") {
		t.Errorf("stderr missing prefix; got %q", stderr.String())
	}
}

func TestRunNoProbeNoCapture(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{}

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), noProbeArgs(), &stdout, &stderr, stubOpener(rcv)); code != exitOK {
		t.Fatalf("run = %d, want %d; stderr=%q", code, exitOK, stderr.String())
	}

	if rcv.closed != 1 {
		t.Errorf("Close() called %d times, want 1", rcv.closed)
	}

	if !strings.Contains(stderr.String(), "opened device=") {
		t.Errorf("stderr missing 'opened device' line; got %q", stderr.String())
	}
}

// noiseChunk fabricates a chunk of pseudo-noise IQ centred at 127.
// Deterministic so tests can assert exact stat outputs.
func noiseChunk(size int, seed uint32) []byte {
	out := make([]byte, size)

	// Linear-congruential PRNG with byte output centred at 127.
	// Not cryptographic — pure determinism for repeatable stats.
	const (
		mul = 1103515245
		inc = 12345
	)

	state := seed
	for i := range size {
		state = state*mul + inc
		// Spread across [97, 157] (~31 stddev around 127) so the
		// summary line resembles real-world noise.
		out[i] = byte(127 + int32(state>>24)%31 - 15) //nolint:gosec // bounded to a byte by the modulo + uint8 cast.
	}

	return out
}

func TestRunProbeOK(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{
			{data: noiseChunk(captureChunkSize, 1)},
			{data: noiseChunk(captureChunkSize, 2)},
		},
	}

	args := []string{flagProbeBytes, "32768"}

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv)); code != exitOK {
		t.Fatalf("run = %d, want %d; stderr=%q", code, exitOK, stderr.String())
	}

	if !strings.Contains(stderr.String(), "iq_stats bytes=32768") {
		t.Errorf("stderr missing iq_stats line; got %q", stderr.String())
	}

	if !strings.Contains(stderr.String(), "mean_i=") || !strings.Contains(stderr.String(), "std_q=") {
		t.Errorf("stderr missing per-channel fields; got %q", stderr.String())
	}
}

func TestRunProbeReadError(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{{err: errSyntheticRead}},
	}

	args := []string{flagProbeBytes, "32768"}

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv)); code != exitProbeFailed {
		t.Errorf("run = %d, want %d", code, exitProbeFailed)
	}

	if !strings.Contains(stderr.String(), "probe read") {
		t.Errorf("stderr missing diagnostic; got %q", stderr.String())
	}
}

func TestProbeIQContextCanceledMidStream(t *testing.T) {
	t.Parallel()

	// First chunk lands fine; second read returns Canceled. The
	// probe should emit stats for what it did read and exit clean.
	rcv := &fakeReceiver{
		reads: []fakeRead{
			{data: noiseChunk(captureChunkSize, 7)},
			{err: context.Canceled},
		},
	}

	cfg := config{probeBytes: 4 * captureChunkSize}

	var stderr bytes.Buffer

	if rc := probeIQ(t.Context(), rcv, cfg, &stderr); rc != exitOK {
		t.Errorf("probeIQ = %d, want %d", rc, exitOK)
	}

	// Partial read still produces an iq_stats line with the byte
	// count actually folded.
	wantPrefix := "iq_stats bytes=" + strconv.Itoa(captureChunkSize)
	if !strings.Contains(stderr.String(), wantPrefix) {
		t.Errorf("stderr missing partial-stats line %q; got %q", wantPrefix, stderr.String())
	}
}

func TestProbeIQTrimsTrailingChunk(t *testing.T) {
	t.Parallel()

	// probeBytes is 1.5 chunks; the second Read returns a full
	// 32 KiB chunk but only the first half should be folded so
	// the byte count matches the request.
	const wantBytes = captureChunkSize + captureChunkSize/2

	rcv := &fakeReceiver{
		reads: []fakeRead{
			{data: bytes.Repeat([]byte{127}, captureChunkSize)},
			{data: bytes.Repeat([]byte{127}, captureChunkSize)},
		},
	}

	cfg := config{probeBytes: wantBytes}

	var stderr bytes.Buffer

	if rc := probeIQ(t.Context(), rcv, cfg, &stderr); rc != exitOK {
		t.Errorf("probeIQ = %d, want %d", rc, exitOK)
	}

	wantPrefix := "iq_stats bytes=" + strconv.Itoa(wantBytes)
	if !strings.Contains(stderr.String(), wantPrefix) {
		t.Errorf("stderr missing %q; got %q", wantPrefix, stderr.String())
	}
}

func TestProbeIQDisabledIsNoop(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{} // Read queue empty; would error if called.
	cfg := config{probeBytes: 0}

	var stderr bytes.Buffer

	if rc := probeIQ(t.Context(), rcv, cfg, &stderr); rc != exitOK {
		t.Errorf("probeIQ = %d, want %d", rc, exitOK)
	}

	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty; got %q", stderr.String())
	}
}

func TestProbeIQCanceledBeforeFirstRead(t *testing.T) {
	t.Parallel()

	// Read returns Canceled before delivering any data. Probe must
	// not emit an iq_stats line (no samples folded).
	rcv := &fakeReceiver{reads: []fakeRead{{err: context.Canceled}}}
	cfg := config{probeBytes: captureChunkSize}

	var stderr bytes.Buffer

	if rc := probeIQ(t.Context(), rcv, cfg, &stderr); rc != exitOK {
		t.Errorf("probeIQ = %d, want %d", rc, exitOK)
	}

	if strings.Contains(stderr.String(), "iq_stats") {
		t.Errorf("stderr unexpectedly contains iq_stats; got %q", stderr.String())
	}
}

func TestIQStatsConstantSignalProducesZeroStd(t *testing.T) {
	t.Parallel()

	// All-127 buffer: mean should be 127, std should be exactly 0.
	stats := iqStats{}
	stats.fold(bytes.Repeat([]byte{127}, 1024))

	out := stats.summarise()

	const want = 127.0
	if out.MeanI != want || out.MeanQ != want {
		t.Errorf("means = (%.3f, %.3f), want (127, 127)", out.MeanI, out.MeanQ)
	}

	if out.StdI != 0 || out.StdQ != 0 {
		t.Errorf("stds = (%.3f, %.3f), want (0, 0)", out.StdI, out.StdQ)
	}
}

func TestIQStatsAlternatingProducesChannelDifferenceInMeanOnly(t *testing.T) {
	t.Parallel()

	// Even index = 100, odd index = 200. Both channels are constant
	// but at different DC levels; std stays zero on both.
	stats := iqStats{}

	buf := make([]byte, 1024)
	for i := range buf {
		if i&1 == 0 {
			buf[i] = 100
		} else {
			buf[i] = 200
		}
	}

	stats.fold(buf)

	out := stats.summarise()

	if out.MeanI != 100 || out.MeanQ != 200 {
		t.Errorf("means = (%.3f, %.3f), want (100, 200)", out.MeanI, out.MeanQ)
	}

	if out.StdI != 0 || out.StdQ != 0 {
		t.Errorf("stds = (%.3f, %.3f), want (0, 0)", out.StdI, out.StdQ)
	}
}

func TestIQStatsSummariseEmptyIsZero(t *testing.T) {
	t.Parallel()

	out := (&iqStats{}).summarise()

	if out.MeanI != 0 || out.StdI != 0 || out.MeanQ != 0 || out.StdQ != 0 {
		t.Errorf("empty stats = %+v, want zero summary", out)
	}
}

func TestIQStatsRandomNoiseStdRoughlyMatches(t *testing.T) {
	t.Parallel()

	// Within ±15 of 127 → uniform-ish noise on a 31-wide window.
	// Population std of that range is ~9; assert it lands in a
	// loose band so the test isn't fragile against PRNG tweaks.
	stats := iqStats{}
	stats.fold(noiseChunk(8192, 42))

	out := stats.summarise()

	const minStd, maxStd = 5.0, 15.0
	if out.StdI < minStd || out.StdI > maxStd {
		t.Errorf("std_i = %.2f, want in [%.1f, %.1f]", out.StdI, minStd, maxStd)
	}

	if out.StdQ < minStd || out.StdQ > maxStd {
		t.Errorf("std_q = %.2f, want in [%.1f, %.1f]", out.StdQ, minStd, maxStd)
	}
}

func TestIQStatsMaxValuesDoNotOverflow(t *testing.T) {
	t.Parallel()

	// All 255s: mean=255, std=0. Sanity check that the byte
	// accumulators don't get truncated for the upper-bound input.
	stats := iqStats{}
	stats.fold(bytes.Repeat([]byte{255}, 1024))

	out := stats.summarise()

	if out.MeanI != 255 {
		t.Errorf("mean_i = %.3f, want 255", out.MeanI)
	}

	if out.StdI != 0 {
		t.Errorf("std_i = %.3f, want 0", out.StdI)
	}

	// Also assert summarise's variance clamp protects against the
	// floating-point case where E[X²] = (E[X])² but the subtraction
	// returns a tiny negative.
	if math.IsNaN(out.StdI) {
		t.Error("std_i = NaN, want clamp to 0")
	}
}

func TestRunCaptureFile(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{
			{data: bytes.Repeat([]byte{0xAA}, captureChunkSize)},
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "cap.iq")

	args := []string{
		flagNoProbe,
		flagCapture, path,
		flagCaptureBytes, flagBytesSmall,
	}

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv)); code != exitOK {
		t.Fatalf("run = %d, want %d; stderr=%q", code, exitOK, stderr.String())
	}

	got, err := os.ReadFile(path) //nolint:gosec // G304: path is a t.TempDir() value owned by this test.
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	if len(got) != 1024 {
		t.Errorf("captured %d bytes, want 1024", len(got))
	}

	for i, b := range got {
		if b != 0xAA {
			t.Errorf("byte %d = %#x, want 0xAA", i, b)

			break
		}
	}
}

func TestRunCaptureStdout(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{
			{data: bytes.Repeat([]byte{0x55}, captureChunkSize)},
		},
	}

	args := []string{flagNoProbe, flagCapture, captureSinkStdout, flagCaptureBytes, "512"}

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv)); code != exitOK {
		t.Fatalf("run = %d, want %d; stderr=%q", code, exitOK, stderr.String())
	}

	if stdout.Len() != 512 {
		t.Errorf("stdout = %d bytes, want 512", stdout.Len())
	}

	if !strings.Contains(stderr.String(), "captured 512 bytes to stdout") {
		t.Errorf("stderr missing summary line; got %q", stderr.String())
	}
}

func TestRunCaptureCanceledMidStream(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{
			{data: bytes.Repeat([]byte{0x01}, 256)},
			{err: context.Canceled},
		},
	}

	args := []string{flagNoProbe, flagCapture, captureSinkStdout, flagCaptureBytes, "65536"}

	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv)); code != exitOK {
		t.Fatalf("run = %d, want %d; stderr=%q", code, exitOK, stderr.String())
	}

	if stdout.Len() != 256 {
		t.Errorf("stdout = %d bytes, want 256 (cancel after first chunk)", stdout.Len())
	}
}

func TestRunCaptureReadError(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{
			{err: errSyntheticRead},
		},
	}

	args := []string{flagNoProbe, flagCapture, captureSinkStdout, flagCaptureBytes, flagBytesSmall}

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv))
	if code != exitCaptureFailed {
		t.Errorf("run = %d, want %d", code, exitCaptureFailed)
	}
}

// failingWriter satisfies io.Writer and returns an error on the
// first byte, exercising the captureIQ write-failure branch.
type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) { return 0, errSyntheticWriteIO }

func TestCaptureIQSurfaceWriteError(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{
		reads: []fakeRead{{data: bytes.Repeat([]byte{0x00}, captureChunkSize)}},
	}

	cfg := config{capturePath: captureSinkStdout, captureBytes: 256}

	var stderr bytes.Buffer

	if rc := captureIQ(t.Context(), rcv, cfg, failingWriter{}, &stderr); rc != exitCaptureFailed {
		t.Errorf("captureIQ = %d, want %d", rc, exitCaptureFailed)
	}

	if !strings.Contains(stderr.String(), "write capture") {
		t.Errorf("stderr missing write diagnostic; got %q", stderr.String())
	}
}

func TestRunCaptureCreatePathFails(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{}

	args := []string{flagNoProbe, flagCapture, filepath.Join(t.TempDir(), "no", "such", "dir", "out.iq")}

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), args, &stdout, &stderr, stubOpener(rcv))
	if code != exitCaptureFailed {
		t.Errorf("run = %d, want %d", code, exitCaptureFailed)
	}
}

func TestRunCloseError(t *testing.T) {
	t.Parallel()

	rcv := &fakeReceiver{closeFn: func() error { return errSyntheticClose }}

	var stdout, stderr bytes.Buffer

	// Close failure does not change exit code; it is logged on stderr.
	if code := run(t.Context(), noProbeArgs(), &stdout, &stderr, stubOpener(rcv)); code != exitOK {
		t.Errorf("run = %d, want %d", code, exitOK)
	}

	if !strings.Contains(stderr.String(), "close:") {
		t.Errorf("stderr missing close diagnostic; got %q", stderr.String())
	}
}

func TestCaptureSinkLabel(t *testing.T) {
	t.Parallel()

	if got := captureSinkLabel("-"); got != "stdout" {
		t.Errorf("captureSinkLabel(-) = %q, want stdout", got)
	}

	if got := captureSinkLabel("/tmp/cap.iq"); got != "/tmp/cap.iq" {
		t.Errorf("captureSinkLabel returned %q, want passthrough", got)
	}
}

func TestDefaultOpenerSurfacesError(t *testing.T) {
	t.Parallel()

	// On a host without an SDR the opener returns ErrUnsupportedPlatform
	// (darwin) or ErrNoDevice (linux without dongle); both are valid
	// failure modes here. We assert that some error comes back so the
	// production code path is exercised.
	cfg, err := parseConfig(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}

	if _, err := defaultOpener(cfg); err == nil {
		t.Skip("defaultOpener returned nil error; a real RTL-SDR is attached. Skip rather than fail.")
	}
}
