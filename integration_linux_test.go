//go:build linux && integration

package sdr_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperized/rtl2832u"
)

// Integration tests live behind the `integration` build tag because
// they require a real RTL-SDR dongle plugged into the test host.
// Run them explicitly:
//
//	go test -tags=integration -run TestIntegration ./sdr
//
// CI does not run them — none of the runners have an SDR connected.
//
// Each test takes exclusive ownership of the dongle (only one
// process can claim its USB interface at a time), so they are not
// t.Parallel(). The paralleltest lint suppressions below cite this
// hardware-exclusivity reason explicitly.

// TestIntegrationOpenCloseAtADSB exercises the full open path on
// real silicon: enumerate, claim, chip init, sample-rate program,
// tuner detect+init, centre-frequency lock, then close. Without
// bulk reads in place we cannot assert IQ flow yet; the test
// passes if every stage succeeds and Close returns cleanly.
//
// Failure modes a fresh checkout will hit before this passes:
//
//   - permission denied on /dev/bus/usb/* — install the udev rule
//     described in wrapOpenError;
//   - device busy — unbind the dvb_usb_rtl28xxu kernel driver via
//     /sys/bus/usb/drivers/dvb_usb_rtl28xxu/unbind;
//   - tuner not present — confirm the dongle ships an R820T/R860,
//     not an E4000 / FC0012 (some clones still do).
//
//nolint:paralleltest // exclusive USB device access; cannot run in parallel with other integration tests.
func TestIntegrationOpenCloseAtADSB(t *testing.T) {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithCenterFreq(rtl2832u.DefaultCenterFreqHz),
		rtl2832u.WithSampleRate(rtl2832u.DefaultSampleRateHz),
	)
	if err != nil {
		if errors.Is(err, rtl2832u.ErrNoDevice) {
			t.Skip("no RTL-SDR detected on this host; integration test needs real hardware")
		}

		t.Fatalf("rtl2832u.Open: %v", err)
	}

	if err := rcv.Close(); err != nil {
		t.Errorf("Receiver.Close: %v", err)
	}
}

// TestIntegrationOpenCloseRoundTrip retunes through several common
// SDR frequencies to exercise the full mux-band table. Skipped if
// no hardware is present.
//
//nolint:paralleltest // exclusive USB device access; cannot run in parallel with other integration tests.
func TestIntegrationOpenCloseRoundTrip(t *testing.T) {
	frequencies := []uint32{
		88_500_000,                   // FM broadcast band
		137_100_000,                  // NOAA / weather satellites
		446_000_000,                  // PMR446
		rtl2832u.DefaultCenterFreqHz, // ADS-B
	}

	for _, freq := range frequencies {
		rcv, err := rtl2832u.Open(rtl2832u.WithCenterFreq(freq))
		if err != nil {
			if errors.Is(err, rtl2832u.ErrNoDevice) {
				t.Skip("no RTL-SDR detected on this host; integration test needs real hardware")
			}

			t.Fatalf("Open at %d Hz: %v", freq, err)
		}

		if err := rcv.Close(); err != nil {
			t.Errorf("Close after %d Hz: %v", freq, err)
		}
	}
}

// TestIntegrationCaptureIQToFile is the end-to-end smoke test for
// the bulk-read path. It opens a dongle at the ADS-B defaults,
// reads ~64 KiB of interleaved IQ samples, writes them to a temp
// file, and asserts the data has non-trivial variance — a constant
// pattern would mean the chip's ADC is muted or the URB ring is
// returning stale buffers.
//
// Run via:
//
//	go test -tags=integration -run TestIntegrationCaptureIQToFile ./sdr
//
// The captured file is preserved under t.TempDir() so the
// developer can inspect or replay it. The test suite cleans it up
// on completion.
//
//nolint:paralleltest // exclusive USB device access; cannot run in parallel with other integration tests.
func TestIntegrationCaptureIQToFile(t *testing.T) {
	const (
		captureBytes  = 64 * 1024
		readDeadline  = 5 * time.Second
		minSampleSpan = 16 // require at least this many distinct byte values
	)

	rcv, err := rtl2832u.Open(
		rtl2832u.WithCenterFreq(rtl2832u.DefaultCenterFreqHz),
		rtl2832u.WithSampleRate(rtl2832u.DefaultSampleRateHz),
	)
	if err != nil {
		if errors.Is(err, rtl2832u.ErrNoDevice) {
			t.Skip("no RTL-SDR detected on this host; integration test needs real hardware")
		}

		t.Fatalf("rtl2832u.Open: %v", err)
	}

	defer func() {
		if cerr := rcv.Close(); cerr != nil {
			t.Errorf("Receiver.Close: %v", cerr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), readDeadline)
	defer cancel()

	buf := make([]byte, captureBytes)

	count, err := rcv.Read(ctx, buf)
	if err != nil {
		t.Fatalf("Receiver.Read: %v (got %d bytes before failure)", err, count)
	}

	if count != captureBytes {
		t.Fatalf("Read returned %d bytes, want %d", count, captureBytes)
	}

	// Write to a temp file the user can inspect after the test runs.
	path := filepath.Join(t.TempDir(), "adsb_capture.iq")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}

	t.Logf("captured %d bytes to %s", count, path)

	// Variance check: a real RTL-SDR's ADC should produce noise that
	// covers a substantial chunk of the 0..255 byte range. A muted
	// chip or a stale-buffer bug would cluster at one or two values.
	seen := make(map[byte]struct{})
	for _, b := range buf {
		seen[b] = struct{}{}
	}

	if len(seen) < minSampleSpan {
		t.Errorf("capture has only %d distinct byte values (< %d); chip likely muted or stream bug",
			len(seen), minSampleSpan)
	}
}
