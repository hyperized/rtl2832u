//go:build linux

package rtl2832u

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// defaultSysfsRoot is the canonical sysfs USB root on Linux. Returned
// from a function (rather than held in a package-level var) so that
// gochecknoglobals stays satisfied; tests inject their own root by
// calling openBackendWithSysfs directly with a t.TempDir() path.
func defaultSysfsRoot() string { return "/sys/bus/usb/devices" }

// USBDEVFS ioctl request codes, transcribed from the Linux kernel header
// include/uapi/linux/usbdevice_fs.h:
//
//	#define USBDEVFS_CLAIMINTERFACE   _IOR('U', 15, unsigned int)
//	#define USBDEVFS_RELEASEINTERFACE _IOR('U', 16, unsigned int)
//
// golang.org/x/sys/unix v0.43.0 does not export these. Hardcoding the
// encoded values is portable across linux/amd64 and linux/arm64 because
// _IOR is computed from architecture-independent bit positions — only
// the kernel header's symbol table varies.
//
// To verify or refresh these values on any Linux host:
//
//   - Read the macro definitions:
//     grep 'USBDEVFS_\(CLAIM\|RELEASE\)INTERFACE' /usr/include/linux/usbdevice_fs.h
//
//   - Resolve the macros to numeric values by compiling a tiny program:
//
//     cat > /tmp/u.c <<'C'
//     #include <linux/usbdevice_fs.h>
//     #include <stdio.h>
//     int main(void) {
//     printf("claim=0x%x\nrelease=0x%x\n",
//     USBDEVFS_CLAIMINTERFACE, USBDEVFS_RELEASEINTERFACE);
//     }
//     C
//     cc /tmp/u.c -o /tmp/u && /tmp/u
//
//   - Reference manpage: man 2 ioctl_usbfs
//
//   - Upstream source (mirrored on bootlin):
//     https://elixir.bootlin.com/linux/latest/source/include/uapi/linux/usbdevice_fs.h
//
// The _IOC encoding scheme that produces the actual hex values lives in
// include/uapi/asm-generic/ioctl.h on the same kernel tree.
const (
	usbdevfsClaimInterface   uint = 0x8004550f
	usbdevfsReleaseInterface uint = 0x80045510
)

// linuxBackend owns a usbfs file descriptor with one claimed
// interface and (lazily, on first Read) a goroutine that streams
// IQ samples from the chip's bulk IN endpoint via a ring of
// USBDEVFS URBs.
//
// Concurrency
// -----------
//   - mu / closed guard the lifecycle bits the public API touches
//     (Close idempotency, ensuring the stream is started exactly
//     once).
//   - The stream goroutine is the single producer for streamCh;
//     Receiver.Read is the single consumer. The channel itself
//     handles synchronisation.
//   - droppedURBs is bumped from the producer when the channel is
//     full and the oldest chunk is overwritten — atomic so external
//     diagnostics readers can sample it without a lock.
//   - urbs/urbBufs are owned by the stream goroutine after
//     startStream returns. The kernel writes into urbBufs while
//     URBs are in flight, so they must outlive every URB submitted
//     against them; we reuse the same backing slices for the
//     stream's lifetime.
type linuxBackend struct {
	dev   *os.File
	iface uint32

	// biasTeeGPIO remembers which GPIO line the Open-time bias-tee
	// config used (or the default 0 when the operator never
	// passed --bias-tee). Receiver.SetBiasTee at runtime reuses
	// this so callers don't have to re-specify the GPIO each
	// time they toggle.
	biasTeeGPIO uint8

	// chip is retained so post-Open methods (SignalStats,
	// AutoTuneGain, runtime register reads) can drive the same
	// controller that opened the device. The chip's `ctrl` field
	// is this backend; the back-reference is intentional.
	chip *rtl2832u

	// tuner is retained for the same reason: AutoTuneGain calls
	// SetLNAGain/SetMixerGain/SetVGAGain on it during the
	// gradient-descent loop, so runtime gain re-tuning needs to
	// reach the same instance Open built.
	tuner Tuner

	// logger is the caller-supplied destination for diagnostic
	// output (currently just the auto-tune converged-config
	// line). Set from cfg.logger by openBackendWithSysfs;
	// defaults to slog.DiscardHandler so the package is silent.
	logger *slog.Logger

	// mu serialises Close's idempotency check. closed is a separate
	// atomic so the URB reaper's hot path can sample it without
	// trapping into the mutex on every completion.
	mu     sync.Mutex
	closed atomic.Bool

	streamOnce sync.Once
	streamErr  error
	streamCh   chan []byte
	streamDone chan struct{}

	urbs    []usbdevfsURB
	urbBufs [][]byte

	droppedURBs atomic.Uint64
}

//nolint:ireturn // backend is the polymorphic seam between platform builds.
func openBackend(cfg config) (backend, error) {
	return openBackendWithSysfs(cfg, defaultSysfsRoot())
}

// openBackendWithSysfs is the testable form of openBackend: the sysfs
// root is a parameter, so test cases can each use their own t.TempDir()
// and run with t.Parallel(). Production paths reach this through
// openBackend with the real /sys/bus/usb/devices root.
//
//nolint:ireturn // backend is the polymorphic seam between platform builds.
func openBackendWithSysfs(cfg config, root string) (backend, error) {
	usb, err := selectDevice(cfg, root)
	if err != nil {
		return nil, err
	}

	device, err := claimUSBDevice(usb)
	if err != nil {
		return nil, err
	}

	back := &linuxBackend{
		dev:         device,
		iface:       0,
		biasTeeGPIO: cfg.biasTee.gpio,
		logger:      cfg.logger,
	}
	back.chip = newRTL2832U(back)

	if err := configureChipAndTuner(cfg, usb, back); err != nil {
		_ = back.Close()

		return nil, err
	}

	if cfg.autoGain {
		if err := runAutoTuneAtOpen(back); err != nil {
			_ = back.Close()

			return nil, err
		}
	}

	return back, nil
}

// runAutoTuneAtOpen executes the auto-tune algorithm during the
// open flow and emits the converged configuration via the
// configured slog logger (default: discard). Extracted to keep
// openBackendWithSysfs's cyclomatic complexity within revive's
// threshold.
//
// Open does not currently take a context, so the auto-tune runs
// against a background context — meaning the caller cannot
// cancel a tune mid-flight. The algorithm self-bounds at 16
// iterations (~16 seconds), so the worst case is bounded.
func runAutoTuneAtOpen(back *linuxBackend) error {
	//nolint:contextcheck // Open API has no ctx; algorithm self-bounds at ~16s.
	result, err := back.AutoTuneGain(context.Background(), AutoTuneOptions{})
	if err != nil {
		return fmt.Errorf("rtl2832u: auto-tune gain: %w", err)
	}

	back.logger.Info("rtl2832u: auto-tune converged",
		slog.Int("lna_step", int(result.LNA.Step)),
		slog.Int("mixer_step", int(result.Mixer.Step)),
		slog.Int("vga_step", int(result.VGA.Step)),
		slog.Float64("saturation_frac", result.FinalStats.SaturationFrac),
		slog.Float64("rms", result.FinalStats.RMS),
		slog.Float64("peak", result.FinalStats.Peak),
		slog.Int("iterations", result.Iterations),
	)

	return nil
}

// selectDevice resolves the requested device index against the
// sysfs enumeration, returning the matching usbDevice or a
// helpful error if the index is invalid or no dongle was found.
func selectDevice(cfg config, root string) (usbDevice, error) {
	devs, err := enumerate(root)
	if err != nil {
		// Some container or chroot environments lack /sys; surfacing that
		// as ErrNoDevice keeps callers' error handling uniform with the
		// "no dongle plugged in" case.
		if errors.Is(err, fs.ErrNotExist) {
			return usbDevice{}, fmt.Errorf(
				"%w: sysfs %q missing (mount /sys, or run the container with --privileged): %w",
				ErrNoDevice, root, err)
		}

		return usbDevice{}, err
	}

	if len(devs) == 0 {
		return usbDevice{}, fmt.Errorf(
			"%w: no matching dongles in %s "+
				"(run `lsusb -d 0bda:` to confirm the dongle is connected; "+
				"unrecognised clones need an entry added to rtlsdrUSBIDs)",
			ErrNoDevice, root)
	}

	if cfg.deviceIndex < 0 || cfg.deviceIndex >= len(devs) {
		return usbDevice{}, fmt.Errorf(
			"%w: index=%d but only %d dongle(s) enumerated; valid range is 0..%d",
			ErrNoDevice, cfg.deviceIndex, len(devs), len(devs)-1)
	}

	return devs[cfg.deviceIndex], nil
}

// claimUSBDevice opens the dongle's /dev/bus/usb/... node and
// claims its interface 0. On claim failure the file descriptor is
// closed so the kernel doesn't keep the device pinned.
//
// First attempt is USBDEVFS_DISCONNECT_CLAIM (kernel ≥ 3.18),
// which atomically evicts any in-kernel driver (typically
// dvb_usb_rtl28xxu, which auto-binds to RTL-SDR dongles) and
// claims the interface for our fd in a single ioctl. Falls back
// to bare USBDEVFS_CLAIMINTERFACE on ancient kernels — there the
// operator still has to unbind the kernel driver by hand.
func claimUSBDevice(usb usbDevice) (*os.File, error) {
	device, err := os.OpenFile(usb.devNode, os.O_RDWR, 0)
	if err != nil {
		return nil, wrapOpenError(usb.devNode, err)
	}

	err = disconnectKernelAndClaim(device, 0)
	if errors.Is(err, errKernelDetachUnsupported) {
		err = claimInterface(device, 0)
	}

	if err != nil {
		// Roll back the open: leaving the fd around would leak a kernel
		// handle if the kernel rejected the claim.
		_ = device.Close()

		return nil, wrapClaimError(usb, err)
	}

	return device, nil
}

// configureChipAndTuner runs the post-claim init sequence: chip
// init, sample-rate, tuner attach + centre-freq lock, gain config,
// and EP-A FIFO arm. Each error wraps with a contextual message
// pointing at the failing step.
func configureChipAndTuner(cfg config, usb usbDevice, back *linuxBackend) error {
	chip := back.chip
	if err := chip.Init(); err != nil {
		// The chip rejected baseband init. Most often the dongle is
		// stuck in DVB-T mode from a prior session; closing the
		// handle and reopening usually clears it.
		return fmt.Errorf("sdr: %w (try unplug+replug, or `usbreset %s`)", err, usb.devNode)
	}

	xtalHz := effectiveXtalHz(referenceClockHz, cfg.freqCorrectionPPM)

	// Order matches librtlsdr's rtlsdr_open + first-time
	// set_sample_rate / set_center_freq:
	//
	//   1. Init (generic baseband)
	//   2. R820T-specific demod writes (ZeroIF off, I-only ADC,
	//      IF freq, spectrum inversion) — must precede tuner init
	//      so the chip's ADC routing is right when the tuner
	//      configures itself.
	//   3. Tuner init (seed table + r82xx_set_tv_standard +
	//      r82xx_sysfreq_sel).
	//   4. set_bandwidth (IF filter shape for sample rate) +
	//      write resampler ratio + soft-reset pulse.
	//   5. set_center_freq.
	//   6. Gain / IF filter overrides / bias-tee.
	//   7. Reset sample buffer.
	if err := chip.configureForR820T(xtalHz); err != nil {
		return fmt.Errorf("sdr: configure demod for R820T tuner: %w", err)
	}

	tuner, err := NewR860(chip, xtalHz)
	if err != nil {
		return fmt.Errorf("sdr: attach tuner: %w", err)
	}

	back.tuner = tuner

	// librtlsdr's rtlsdr_set_sample_rate runs r82xx_set_bandwidth
	// before writing the resampler ratio. The bandwidth call programs
	// R0x0a / R0x0b — the IF filter shape — and chooses the IF
	// centre frequency the tuner will produce. We then have to
	// reprogram both the demod's DDC frequency (so it mixes that
	// same IF down to baseband) and the tuner's intFreqHz field
	// (so SetCenterFreq offsets the LO by it). Without those two
	// the filter sits at its seed values, the demod still expects
	// the chip-init 3.57 MHz IF, and the LO targets the wrong
	// frequency — every CRC fails. InitializeForSampleRate brackets
	// the I²C repeater itself; we propagate its returned IF to the
	// demod here.
	tunerIntFreqHz, err := tuner.InitializeForSampleRate(cfg.sampleRateHz)
	if err != nil {
		return fmt.Errorf("sdr: configure tuner IF filter for sample rate: %w", err)
	}

	if err := chip.SetIFFrequency(tunerIntFreqHz, xtalHz); err != nil {
		return fmt.Errorf("sdr: align demod DDC with tuner IF (%d Hz): %w",
			tunerIntFreqHz, err)
	}

	if _, err := chip.SetSampleRate(cfg.sampleRateHz, xtalHz); err != nil {
		return fmt.Errorf("sdr: configure sample rate: %w", err)
	}

	if err := chip.SetCenterFreq(cfg.centerFreqHz, tuner); err != nil {
		return fmt.Errorf(
			"sdr: configure centre frequency to %d Hz: %w "+
				"(check the requested RF is within the tuner's 28 MHz..1.766 GHz range, "+
				"and that antenna/cable losses leave enough signal for PLL lock)",
			cfg.centerFreqHz, err)
	}

	if err := applyTunerGain(tuner, cfg); err != nil {
		return fmt.Errorf("sdr: configure tuner gain: %w", err)
	}

	if err := applyTunerFilter(tuner, cfg); err != nil {
		return fmt.Errorf("sdr: configure tuner filter: %w", err)
	}

	if cfg.biasTee.applied {
		if err := chip.setBiasTee(cfg.biasTee.gpio, cfg.biasTee.enable); err != nil {
			return fmt.Errorf("sdr: configure bias-tee: %w", err)
		}
	}

	// initUSB left EPA_CTL in the halt state; flip it to run before
	// the first bulk read so the kernel doesn't get EPIPE.
	if err := chip.ResetSampleBuffer(); err != nil {
		return fmt.Errorf("sdr: reset sample buffer: %w", err)
	}

	return nil
}

// applyTunerGain pushes the resolved per-stage gain configuration
// down to the Tuner. Each stage is programmed independently so a
// later iteration that exposes per-stage retuning at runtime can
// reuse the same Tuner methods.
func applyTunerGain(tuner Tuner, cfg config) error {
	if err := tuner.SetLNAGain(cfg.lnaGain); err != nil {
		return fmt.Errorf("LNA: %w", err)
	}

	if err := tuner.SetMixerGain(cfg.mixerGain); err != nil {
		return fmt.Errorf("mixer: %w", err)
	}

	if err := tuner.SetVGAGain(cfg.vgaGain); err != nil {
		return fmt.Errorf("VGA: %w", err)
	}

	return nil
}

// applyTunerFilter pushes IF-filter overrides down to the Tuner.
// Each setting checks its applied flag so the chip stays at its
// init-seed value when the user hasn't asked for an override.
func applyTunerFilter(tuner Tuner, cfg config) error {
	if cfg.ifBandwidth.applied {
		if err := tuner.SetIFBandwidth(cfg.ifBandwidth.coarse, cfg.ifBandwidth.fine); err != nil {
			return fmt.Errorf("IF bandwidth: %w", err)
		}
	}

	if cfg.ifHighPass.applied {
		if err := tuner.SetIFHighPass(cfg.ifHighPass.code); err != nil {
			return fmt.Errorf("IF highpass: %w", err)
		}
	}

	if cfg.filterExt.applied {
		if err := tuner.SetFilterExt(cfg.filterExt.enable); err != nil {
			return fmt.Errorf("filter ext: %w", err)
		}
	}

	return nil
}

// wrapOpenError translates an os.OpenFile failure on /dev/bus/usb/...
// into a message that names the most likely fix. EACCES is the
// dominant cause on stock Linux because the device node is root:root
// 0664 by default.
func wrapOpenError(devNode string, err error) error {
	switch {
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf(
			"sdr: open %s: permission denied "+
				`(install a udev rule like SUBSYSTEM=="usb", ATTRS{idVendor}=="0bda", `+
				`MODE="0660", GROUP="plugdev" and add your user to plugdev; `+
				"or run with sudo for a one-off): %w",
			devNode, err)
	case errors.Is(err, syscall.ENOENT):
		return fmt.Errorf(
			"sdr: open %s: device node missing "+
				"(dongle was likely unplugged between enumerate() and OpenFile()): %w",
			devNode, err)
	default:
		return fmt.Errorf("sdr: open %s: %w", devNode, err)
	}
}

// wrapClaimError translates a USBDEVFS_DISCONNECT_CLAIM (or
// fallback CLAIMINTERFACE) failure into a message that names the
// most likely fix. EBUSY post-auto-detach usually means another
// userspace process already has the interface claimed (a stale
// readsb / dump1090 / rtl_tcp); on pre-3.18 kernels the fallback
// path can still surface the legacy "kernel driver holds the
// interface" cause, and the unbind hint stays useful there.
func wrapClaimError(usb usbDevice, err error) error {
	if errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf(
			"sdr: claim interface 0 on %s: device busy "+
				"(another userspace process likely has it — `lsof %s` / `fuser %s`; "+
				"on kernels < 3.18 also unbind dvb_usb_rtl28xxu: "+
				"`echo %d-%d | sudo tee /sys/bus/usb/drivers/dvb_usb_rtl28xxu/unbind`): %w",
			usb.devNode, usb.devNode, usb.devNode, usb.busNum, usb.devNum, err)
	}

	return fmt.Errorf("sdr: claim interface 0 on %s: %w", usb.devNode, err)
}

// claimInterface and releaseInterface wrap the USBDEVFS_*INTERFACE ioctls.
// Both take a pointer to an unsigned int holding the interface number;
// IoctlSetPointerInt handles the indirection on our behalf.
func claimInterface(dev *os.File, iface uint32) error {
	if err := unix.IoctlSetPointerInt(int(dev.Fd()), usbdevfsClaimInterface, int(iface)); err != nil {
		return fmt.Errorf("ioctl USBDEVFS_CLAIMINTERFACE: %w", err)
	}

	return nil
}

func releaseInterface(dev *os.File, iface uint32) error {
	if err := unix.IoctlSetPointerInt(int(dev.Fd()), usbdevfsReleaseInterface, int(iface)); err != nil {
		return fmt.Errorf("ioctl USBDEVFS_RELEASEINTERFACE: %w", err)
	}

	return nil
}

// Read returns up to len(dst) bytes from the next completed URB.
// It blocks until a chunk arrives, the stream ends, or ctx is
// cancelled. A single Read maps to a single URB completion:
// callers needing fill-to-buffer semantics across multiple URBs
// wrap with io.ReadFull.
//
// One-URB-per-Read is the simpler contract: it eliminates a
// per-backend "tail" buffer that was mutated without a lock,
// which silently corrupted state under any accidental concurrent
// Reader. The single-producer contract still applies — usbfs
// bulk endpoints serialise transfers — but the new shape is also
// safe under misuse: each Read either gets a chunk or doesn't.
func (b *linuxBackend) Read(ctx context.Context, dst []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("sdr: read cancelled: %w", err)
	}

	if err := b.ensureStream(); err != nil {
		return 0, err
	}

	select {
	case chunk, ok := <-b.streamCh:
		if !ok {
			return 0, b.streamErr
		}

		return copy(dst, chunk), nil
	case <-ctx.Done():
		return 0, fmt.Errorf("sdr: read cancelled: %w", ctx.Err())
	case <-b.streamDone:
		return 0, b.streamErr
	}
}

// Close releases the USB interface and closes the file descriptor.
// If the stream goroutine is running, Close discards every URB it
// holds and waits for the goroutine to drain before tearing the
// device down — the kernel needs every URB accounted for or it
// keeps the descriptor pinned.
//
// Releasing the interface must precede closing the fd: closing
// the fd alone leaves the kernel's claim record pinned until the
// device is physically unplugged, which would block subsequent
// demod1090 invocations.
func (b *linuxBackend) Close() error {
	// mu serialises the "first-Close wins" check; the atomic store
	// publishes the closed state to the reaper's lock-free read.
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()

		return nil
	}

	b.closed.Store(true)
	b.mu.Unlock()

	if b.streamDone != nil {
		for i := range b.urbs {
			_ = b.discardURB(&b.urbs[i])
		}

		<-b.streamDone

		// If runStreamReaper exited early (e.g. on a non-zero URB
		// status from the kernel) some URBs may still be queued
		// without anyone to reap them. The kernel keeps the device
		// fd pinned until every URB it knows about has been reaped,
		// so we drain non-blockingly here. Cap at the ring size: a
		// real URB can only complete once, and the reaper already
		// consumed whatever it could before exiting. drainNextURB
		// returns (false, nil) when the kernel has nothing more to
		// hand back.
		b.drainOrphanedURBs()
	}

	var firstErr error
	if err := releaseInterface(b.dev, b.iface); err != nil {
		firstErr = fmt.Errorf("sdr: release interface: %w", err)
	}

	if err := b.dev.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("sdr: close device: %w", err)
	}

	return firstErr
}

// drainOrphanedURBs reaps any URBs the kernel still holds after the
// stream goroutine has exited. Bounded by the ring size so a
// runaway kernel state cannot stall Close indefinitely; any reap
// error is logged at warn level and ignored — the goal is best-
// effort cleanup before we release the interface and close the fd.
// Warn (not Debug) because a reap error here indicates the kernel
// is in an unexpected state and is the kind of signal a user
// looking at logs after a "device wedged on Close" report will
// want to see without flipping verbosity.
func (b *linuxBackend) drainOrphanedURBs() {
	for range b.urbs {
		hasURB, err := b.drainNextURB()
		if err != nil {
			b.logger.Warn("rtl2832u: drain orphaned URB",
				slog.String("error", err.Error()))

			return
		}

		if !hasURB {
			return
		}
	}
}

// DroppedSampleChunks satisfies the backend interface; see the
// method's doc on the public Receiver type for behaviour.
func (b *linuxBackend) DroppedSampleChunks() uint64 {
	return b.droppedURBs.Load()
}

// SignalStats satisfies the backend interface; see Receiver.SignalStats
// for documentation. The page-3 reads are USB control transfers, not
// I/Os against the streaming bulk endpoint, so they are safe to
// issue concurrently with active sample streaming.
func (b *linuxBackend) SignalStats() (SignalStats, error) {
	return b.chip.readSignalStats()
}

// AutoTuneGain satisfies the backend interface. Runs the
// gradient-descent search defined in autotune.go against the
// retained tuner and the bulk-endpoint sample stream.
func (b *linuxBackend) AutoTuneGain(ctx context.Context, opts AutoTuneOptions) (AutoTuneResult, error) {
	return autoTuneGain(ctx, b.tuner, b, opts)
}

// SetLNAGain reconfigures the tuner's LNA stage at runtime. Safe
// to call while bulk reads are in flight — the write is a USB
// control transfer on the I²C side, separate from the bulk
// endpoint the URB ring drives. step is clamped by the tuner's
// own validation.
func (b *linuxBackend) SetLNAGain(step uint8) error {
	if err := b.tuner.SetLNAGain(ManualGainStep(step)); err != nil {
		return fmt.Errorf("rtl2832u: set LNA gain: %w", err)
	}

	return nil
}

// SetMixerGain reconfigures the tuner's Mixer stage at runtime.
// Same safety / clamping notes as SetLNAGain.
func (b *linuxBackend) SetMixerGain(step uint8) error {
	if err := b.tuner.SetMixerGain(ManualGainStep(step)); err != nil {
		return fmt.Errorf("rtl2832u: set Mixer gain: %w", err)
	}

	return nil
}

// SetVGAGain reconfigures the tuner's VGA stage at runtime. Same
// safety / clamping notes as SetLNAGain.
func (b *linuxBackend) SetVGAGain(step uint8) error {
	if err := b.tuner.SetVGAGain(ManualGainStep(step)); err != nil {
		return fmt.Errorf("rtl2832u: set VGA gain: %w", err)
	}

	return nil
}

// SetBiasTee toggles the bias-tee GPIO at runtime, reusing the
// GPIO line the Open-time config selected (default 0).
func (b *linuxBackend) SetBiasTee(enable bool) error {
	if err := b.chip.setBiasTee(b.biasTeeGPIO, enable); err != nil {
		return fmt.Errorf("rtl2832u: set bias-tee: %w", err)
	}

	return nil
}
