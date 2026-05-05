package rtl2832u

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultCenterFreqHz(t *testing.T) {
	t.Parallel()

	if DefaultCenterFreqHz != 1_090_000_000 {
		t.Fatalf("DefaultCenterFreqHz = %d, want 1_090_000_000", DefaultCenterFreqHz)
	}
}

func TestDefaultSampleRateHz(t *testing.T) {
	t.Parallel()

	if DefaultSampleRateHz != 2_400_000 {
		t.Fatalf("DefaultSampleRateHz = %d, want 2_400_000", DefaultSampleRateHz)
	}
}

func TestGainAGCSentinel(t *testing.T) {
	t.Parallel()

	if GainAGC >= 0 {
		t.Fatalf("GainAGC = %d, want negative sentinel", GainAGC)
	}
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	if cfg.centerFreqHz != DefaultCenterFreqHz {
		t.Errorf("centerFreqHz = %d, want %d", cfg.centerFreqHz, DefaultCenterFreqHz)
	}

	if cfg.sampleRateHz != DefaultSampleRateHz {
		t.Errorf("sampleRateHz = %d, want %d", cfg.sampleRateHz, DefaultSampleRateHz)
	}

	if cfg.lnaGain != AutoGain || cfg.mixerGain != AutoGain || cfg.vgaGain != AutoGain {
		t.Errorf("default gain stages = (%+v, %+v, %+v), want all AutoGain",
			cfg.lnaGain, cfg.mixerGain, cfg.vgaGain)
	}

	if cfg.deviceIndex != 0 {
		t.Errorf("deviceIndex = %d, want 0", cfg.deviceIndex)
	}
}

func TestWithCenterFreq(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithCenterFreq(978_000_000)(&cfg)

	if cfg.centerFreqHz != 978_000_000 {
		t.Fatalf("centerFreqHz = %d, want 978_000_000", cfg.centerFreqHz)
	}
}

func TestWithLoggerStoresLogger(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	if cfg.logger == nil {
		t.Fatal("default logger is nil; want a discard logger")
	}

	custom := slog.New(slog.DiscardHandler)
	WithLogger(custom)(&cfg)

	if cfg.logger != custom {
		t.Error("WithLogger did not install the supplied logger")
	}
}

func TestWithLoggerNilFallsBackToDiscard(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	custom := slog.New(slog.DiscardHandler)

	// First install a custom logger, then pass nil — the nil
	// path must replace the custom logger with a discard one
	// rather than leave the custom one in place.
	WithLogger(custom)(&cfg)
	WithLogger(nil)(&cfg)

	if cfg.logger == custom {
		t.Error("WithLogger(nil) did not replace the custom logger")
	}

	if cfg.logger == nil {
		t.Error("WithLogger(nil) left logger as nil; want a discard logger")
	}
}

func TestWithFrequencyCorrectionDefault(t *testing.T) {
	t.Parallel()

	if cfg := defaultConfig(); cfg.freqCorrectionPPM != 0 {
		t.Errorf("default freqCorrectionPPM = %d, want 0", cfg.freqCorrectionPPM)
	}
}

func TestWithFrequencyCorrectionStoresValue(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithFrequencyCorrection(-37)(&cfg)

	if cfg.freqCorrectionPPM != -37 {
		t.Errorf("freqCorrectionPPM = %d, want -37", cfg.freqCorrectionPPM)
	}
}

func TestWithFrequencyCorrectionClampsHighAndLow(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		in   int
		want int32
	}{
		{name: "above_max_clamps_to_max", in: 5_000, want: FrequencyCorrectionPPMMax},
		{name: "below_min_clamps_to_min", in: -5_000, want: -FrequencyCorrectionPPMMax},
		{name: "exact_max_passes_through", in: FrequencyCorrectionPPMMax, want: FrequencyCorrectionPPMMax},
		{name: "exact_min_passes_through", in: -FrequencyCorrectionPPMMax, want: -FrequencyCorrectionPPMMax},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := defaultConfig()
			WithFrequencyCorrection(testCase.in)(&cfg)

			if cfg.freqCorrectionPPM != testCase.want {
				t.Errorf("WithFrequencyCorrection(%d) → %d, want %d",
					testCase.in, cfg.freqCorrectionPPM, testCase.want)
			}
		})
	}
}

func TestWithSampleRate(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithSampleRate(2_000_000)(&cfg)

	if cfg.sampleRateHz != 2_000_000 {
		t.Fatalf("sampleRateHz = %d, want 2_000_000", cfg.sampleRateHz)
	}
}

func TestWithGainAGCResetsAllStages(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	cfg.lnaGain = ManualGainStep(15)
	cfg.mixerGain = ManualGainStep(15)
	cfg.vgaGain = ManualGainStep(15)

	WithGain(GainAGC)(&cfg)

	if cfg.lnaGain != AutoGain || cfg.mixerGain != AutoGain || cfg.vgaGain != AutoGain {
		t.Errorf("AGC sentinel did not flip stages back to AutoGain: %+v %+v %+v",
			cfg.lnaGain, cfg.mixerGain, cfg.vgaGain)
	}
}

func TestWithGainResolvesTableEntry(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithGain(420)(&cfg) // 42.0 dB target — within the librtlsdr ladder

	if cfg.lnaGain.Auto || cfg.mixerGain.Auto {
		t.Fatalf("WithGain(420) left a stage on Auto: lna=%+v mixer=%+v",
			cfg.lnaGain, cfg.mixerGain)
	}

	if cfg.vgaGain.Auto || cfg.vgaGain.Step != librtlsdrManualVGAStep {
		t.Errorf("VGA = %+v, want manual step %d", cfg.vgaGain, librtlsdrManualVGAStep)
	}

	if cfg.lnaGain.Step == 0 && cfg.mixerGain.Step == 0 {
		t.Error("WithGain(420) resolved to (0, 0) — table walk likely no-op'd")
	}
}

func TestFilterAndBiasTeeOptions(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()

	WithIFBandwidth(2, 11)(&cfg)
	WithIFHighPass(R860HPF1MHz)(&cfg)
	WithFilterExt(true)(&cfg)
	WithBiasTee(true)(&cfg)

	if !cfg.ifBandwidth.applied || cfg.ifBandwidth.coarse != 2 || cfg.ifBandwidth.fine != 11 {
		t.Errorf("ifBandwidth = %+v, want {2, 11, true}", cfg.ifBandwidth)
	}

	if !cfg.ifHighPass.applied || cfg.ifHighPass.code != R860HPF1MHz {
		t.Errorf("ifHighPass = %+v, want {R860HPF1MHz, true}", cfg.ifHighPass)
	}

	if !cfg.filterExt.applied || !cfg.filterExt.enable {
		t.Errorf("filterExt = %+v, want {true, true}", cfg.filterExt)
	}

	if !cfg.biasTee.applied || cfg.biasTee.gpio != defaultBiasTeeGPIO || !cfg.biasTee.enable {
		t.Errorf("biasTee = %+v, want {0, true, true}", cfg.biasTee)
	}
}

func TestWithBiasTeeGPIOOverridesPin(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithBiasTeeGPIO(5, true)(&cfg)

	if !cfg.biasTee.applied || cfg.biasTee.gpio != 5 || !cfg.biasTee.enable {
		t.Errorf("biasTee = %+v, want {5, true, true}", cfg.biasTee)
	}
}

func TestWithAutoGainEnablesFlag(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()

	// Pre-set per-stage manual values so we can confirm WithAutoGain
	// resets them.
	cfg.lnaGain = ManualGainStep(5)
	cfg.mixerGain = ManualGainStep(5)
	cfg.vgaGain = ManualGainStep(5)

	WithAutoGain()(&cfg)

	if !cfg.autoGain {
		t.Error("autoGain = false, want true")
	}

	if cfg.lnaGain != AutoGain || cfg.mixerGain != AutoGain || cfg.vgaGain != AutoGain {
		t.Errorf("stages not reset: lna=%+v mixer=%+v vga=%+v",
			cfg.lnaGain, cfg.mixerGain, cfg.vgaGain)
	}
}

func TestPerStageOptionsOverrideWithGain(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()

	// WithGain first, then a per-stage override — last write wins.
	WithGain(420)(&cfg)
	WithLNAGain(AutoGain)(&cfg)
	WithMixerGain(ManualGainStep(3))(&cfg)
	WithVGAGain(VGAStepForCentiDB(2000))(&cfg) // +20.0 dB target

	if !cfg.lnaGain.Auto {
		t.Errorf("LNA = %+v, want AutoGain after override", cfg.lnaGain)
	}

	if cfg.mixerGain.Auto || cfg.mixerGain.Step != 3 {
		t.Errorf("Mixer = %+v, want manual step 3", cfg.mixerGain)
	}

	// +20.0 dB is centi-dB 2000; vgaStepForCentiDB((2000) - (-1200)) / 350 = 9.
	if cfg.vgaGain.Auto || cfg.vgaGain.Step != 9 {
		t.Errorf("VGA = %+v, want manual step 9 (~+19.5 dB)", cfg.vgaGain)
	}
}

func TestWithDevice(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithDevice(2)(&cfg)

	if cfg.deviceIndex != 2 {
		t.Fatalf("deviceIndex = %d, want 2", cfg.deviceIndex)
	}
}

func TestPackUSBID(t *testing.T) {
	t.Parallel()

	got := packUSBID(0x0bda, 0x2838)

	const want uint32 = 0x0bda2838
	if got != want {
		t.Fatalf("packUSBID = %#x, want %#x", got, want)
	}
}

// sysfs attribute filenames. Hoisted so tests that build partial
// fixtures (missing busnum, devnum, etc.) reference the same
// strings as fakeDevice's layout.
const (
	sysfsIDVendor  = "idVendor"
	sysfsIDProduct = "idProduct"
	sysfsBusnum    = "busnum"
	sysfsDevnum    = "devnum"
)

// sysfs file *contents* used by the partial-entry / symlink
// fixtures. Hoisted to defeat goconst (the same string body shows
// up in three test bodies).
const (
	vidRealtekContent = "0bda\n"
	pidRTL2838Content = "2838\n"
	busnumOneContent  = "1\n"
	devnumFourContent = "4\n"
)

// fakeDevice writes the four sysfs attribute files that readUSBDevice
// requires under root/<name>. Helper centralises the file layout so
// individual tests can focus on the behaviour they exercise. The bus
// number is hardcoded — no current test needs to vary it, and varying
// would just add noise to the call sites.
func fakeDevice(t *testing.T, root, name, vid, pid, dev string) {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	files := map[string]string{
		sysfsIDVendor:  vid,
		sysfsIDProduct: pid,
		sysfsBusnum:    "1",
		sysfsDevnum:    dev,
	}

	for f, content := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
}

func TestEnumerateFindsKnownRTLSDR(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fakeDevice(t, root, "1-2", "0bda", "2838", "5")

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 1 {
		t.Fatalf("found %d devices, want 1", len(devs))
	}

	got := devs[0]
	if got.vendorID != 0x0bda || got.productID != 0x2838 {
		t.Errorf("VID:PID = %04x:%04x, want 0bda:2838", got.vendorID, got.productID)
	}

	if got.busNum != 1 || got.devNum != 5 {
		t.Errorf("bus:dev = %d:%d, want 1:5", got.busNum, got.devNum)
	}

	const wantNode = "/dev/bus/usb/001/005"
	if got.devNode != wantNode {
		t.Errorf("devNode = %q, want %q", got.devNode, wantNode)
	}
}

func TestEnumerateSkipsUnknownVendor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fakeDevice(t, root, "1-3", "1234", "abcd", "6")

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Fatalf("found %d devices, want 0 (unknown VID:PID)", len(devs))
	}
}

func TestEnumerateSkipsInterfaceDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// "1-0:1.0"-style directories are USB *interface* nodes, not devices,
	// and lack the idVendor/idProduct quartet.
	if err := os.MkdirAll(filepath.Join(root, "1-0:1.0"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Fatalf("found %d devices, want 0", len(devs))
	}
}

func TestEnumerateSkipsPartialEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Directory exists but lacks idVendor — simulates a hotplug race.
	if err := os.MkdirAll(filepath.Join(root, "1-4"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Fatalf("found %d devices, want 0", len(devs))
	}
}

func TestEnumerateRejectsMissingRoot(t *testing.T) {
	t.Parallel()

	_, err := enumerate(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing root, got nil")
	}
}

// TestEnumerateFollowsSymlinks mirrors how /sys/bus/usb/devices is
// laid out on real Linux: every entry is a symlink pointing into
// /sys/devices/... rather than a real directory. Without this test
// the enumerator passed CI on synthetic fixtures while finding zero
// dongles on real hardware (DirEntry.IsDir() reports false for
// symlinks).
func TestEnumerateFollowsSymlinks(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	// "Real" device dir lives outside the enumerator's root, just
	// like /sys/devices/platform/... does on a real box.
	realDev := filepath.Join(tmp, "real", "1-1.3")
	if err := os.MkdirAll(realDev, 0o750); err != nil {
		t.Fatalf("mkdir realDev: %v", err)
	}

	for name, content := range map[string]string{
		sysfsIDVendor:  vidRealtekContent,
		sysfsIDProduct: pidRTL2838Content,
		sysfsBusnum:    busnumOneContent,
		sysfsDevnum:    devnumFourContent,
	} {
		if err := os.WriteFile(filepath.Join(realDev, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// The enumerator's root contains only a symlink — the layout
	// /sys/bus/usb/devices actually has.
	root := filepath.Join(tmp, "bus")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	if err := os.Symlink(realDev, filepath.Join(root, "1-1.3")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 1 {
		t.Fatalf("found %d devices via symlink, want 1", len(devs))
	}

	got := devs[0]
	if got.vendorID != 0x0bda || got.productID != 0x2838 {
		t.Errorf("VID:PID = %04x:%04x, want 0bda:2838", got.vendorID, got.productID)
	}
}

// TestEnumerateSkipsDanglingSymlinks covers the os.Stat-error
// branch in the symlink-aware filter. A dangling symlink (target
// missing) must not abort the whole walk.
func TestEnumerateSkipsDanglingSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "1-1.9")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Errorf("found %d devices through dangling symlink, want 0", len(devs))
	}
}

// TestEnumerateSkipsKnownVendorUnknownProduct exercises the
// isKnownRTLSDR fall-through: matching VID, unfamiliar PID still
// fails the device filter.
func TestEnumerateSkipsKnownVendorUnknownProduct(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Realtek VID, but a product ID we don't recognise (RTL8821AU
	// or similar non-DVB-T silicon).
	fakeDevice(t, root, "1-5", "0bda", "8812", "7")

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Errorf("found %d devices; matching VID with unknown PID must not enumerate", len(devs))
	}
}

// TestReadUSBDeviceMissingIDProduct hits readUSBDevice's
// idProduct read-error branch (idVendor exists, idProduct
// doesn't); enumerate's outer loop swallows the per-device error
// and the directory is skipped.
func TestReadUSBDeviceMissingIDProduct(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	dir := filepath.Join(root, "1-6")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, sysfsIDVendor), []byte(vidRealtekContent), 0o600); err != nil {
		t.Fatalf("write idVendor: %v", err)
	}
	// Skip idProduct, busnum, devnum entirely.

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Errorf("found %d devices; partial entries must not enumerate", len(devs))
	}
}

// TestReadUSBDeviceMissingBusnum hits readUSBDevice's busnum read
// error branch: idVendor + idProduct exist, busnum does not.
func TestReadUSBDeviceMissingBusnum(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	dir := filepath.Join(root, "1-7")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for name, content := range map[string]string{
		sysfsIDVendor:  vidRealtekContent,
		sysfsIDProduct: pidRTL2838Content,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Errorf("found %d devices; entry without busnum must not enumerate", len(devs))
	}
}

// TestReadUSBDeviceMissingDevnum hits readUSBDevice's devnum read
// error branch: idVendor + idProduct + busnum exist, devnum does
// not.
func TestReadUSBDeviceMissingDevnum(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	dir := filepath.Join(root, "1-8")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for name, content := range map[string]string{
		sysfsIDVendor:  vidRealtekContent,
		sysfsIDProduct: pidRTL2838Content,
		sysfsBusnum:    busnumOneContent,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	devs, err := enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(devs) != 0 {
		t.Errorf("found %d devices; entry without devnum must not enumerate", len(devs))
	}
}

func TestReadSizedUintRejectsBadHex(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "idVendor")

	if err := os.WriteFile(path, []byte("not-hex\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := readHexU16(path); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestReadSizedUintRejectsMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := readDecU16(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected read error, got nil")
	}
}

// errFakeRead and errFakeClose are static so tests can assert via
// errors.Is. err113 forbids ad-hoc errors.New inside test bodies because
// equality checks against ad-hoc errors are fragile; static sentinels
// give us identity that survives wrapping.
var (
	errFakeRead  = errors.New("fake backend read failure")
	errFakeClose = errors.New("fake backend close failure")
)

// fakeBackend lets tests exercise the Receiver pass-through behaviour
// without a real USB device. It captures whether Close has been called
// so individual cases can assert that Close propagates without skipping
// the inner backend, and stubs DroppedSampleChunks / SignalStats so the
// type satisfies the backend interface.
type fakeBackend struct {
	readErr     error
	readN       int
	closeErr    error
	closeCalled bool
	dropped     uint64
	stats       SignalStats
	statsErr    error
	tuneResult  AutoTuneResult
	tuneErr     error
}

func (f *fakeBackend) Read(_ context.Context, _ []byte) (int, error) {
	return f.readN, f.readErr
}

func (f *fakeBackend) Close() error {
	f.closeCalled = true

	return f.closeErr
}

func (f *fakeBackend) DroppedSampleChunks() uint64 {
	return f.dropped
}

func (f *fakeBackend) SignalStats() (SignalStats, error) {
	return f.stats, f.statsErr
}

func (f *fakeBackend) AutoTuneGain(_ context.Context, _ AutoTuneOptions) (AutoTuneResult, error) {
	return f.tuneResult, f.tuneErr
}

func TestReceiverReadDelegatesToBackend(t *testing.T) {
	t.Parallel()

	fake := &fakeBackend{readN: 7, readErr: errFakeRead}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	n, err := rcv.Read(context.Background(), make([]byte, 16))
	if n != 7 {
		t.Errorf("n = %d, want 7", n)
	}

	if !errors.Is(err, errFakeRead) {
		t.Errorf("err = %v, want errFakeRead", err)
	}
}

func TestReceiverReadSuccessPath(t *testing.T) {
	t.Parallel()

	// fake returns no error — covers Receiver.Read's no-error
	// return; the error-wrap path is in TestReceiverReadDelegatesToBackend.
	fake := &fakeBackend{readN: 12}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	count, err := rcv.Read(context.Background(), make([]byte, 16))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if count != 12 {
		t.Errorf("count = %d, want 12", count)
	}
}

func TestReceiverCloseDelegatesToBackend(t *testing.T) {
	t.Parallel()

	fake := &fakeBackend{closeErr: errFakeClose}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	if err := rcv.Close(); !errors.Is(err, errFakeClose) {
		t.Errorf("err = %v, want errFakeClose", err)
	}

	if !fake.closeCalled {
		t.Error("backend Close was not called")
	}
}

func TestReceiverCloseSuccessPath(t *testing.T) {
	t.Parallel()

	// Backend close returns nil — covers Receiver.Close's
	// no-error path.
	fake := &fakeBackend{}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	if err := rcv.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	if !fake.closeCalled {
		t.Error("backend Close was not called")
	}
}

func TestReceiverSignalStatsDelegates(t *testing.T) {
	t.Parallel()

	want := SignalStats{RFAGCValue: 1234, IFAGCValue: -567, AAGCLocked: true}

	fake := &fakeBackend{stats: want}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	got, err := rcv.SignalStats()
	if err != nil {
		t.Fatalf("SignalStats: %v", err)
	}

	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReceiverSignalStatsWrapsBackendError(t *testing.T) {
	t.Parallel()

	fake := &fakeBackend{statsErr: errFakeRead}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	if _, err := rcv.SignalStats(); !errors.Is(err, errFakeRead) {
		t.Errorf("err = %v, want wrapping errFakeRead", err)
	}
}

func TestReceiverAutoTuneGainDelegates(t *testing.T) {
	t.Parallel()

	want := AutoTuneResult{
		LNA:        ManualGainStep(12),
		Mixer:      ManualGainStep(15),
		VGA:        ManualGainStep(15),
		FinalIFAGC: -1500,
		Iterations: 4,
	}

	fake := &fakeBackend{tuneResult: want}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	got, err := rcv.AutoTuneGain(t.Context(), AutoTuneOptions{})
	if err != nil {
		t.Fatalf("AutoTuneGain: %v", err)
	}

	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReceiverAutoTuneGainWrapsBackendError(t *testing.T) {
	t.Parallel()

	fake := &fakeBackend{tuneErr: errFakeRead}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	if _, err := rcv.AutoTuneGain(t.Context(), AutoTuneOptions{}); !errors.Is(err, errFakeRead) {
		t.Errorf("err = %v, want wrapping errFakeRead", err)
	}
}

func TestReceiverDroppedSampleChunksDelegates(t *testing.T) {
	t.Parallel()

	fake := &fakeBackend{dropped: 1234}
	rcv := &Receiver{cfg: defaultConfig(), backend: fake}

	if got := rcv.DroppedSampleChunks(); got != 1234 {
		t.Errorf("DroppedSampleChunks = %d, want 1234", got)
	}
}
