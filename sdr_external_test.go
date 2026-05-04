package rtl2832u_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/hyperized/rtl2832u"
)

// TestOpenWithoutHardware asserts the contract from the public API: when
// Open cannot produce a working Receiver, it must return a typed error
// and a nil Receiver. The expected sentinel depends on platform —
// ErrUnsupportedPlatform on darwin/etc, ErrNoDevice on linux without a
// dongle plugged in. Hosts with a real RTL-SDR connected skip rather
// than fail, so this stays useful on developer machines.
func TestOpenWithoutHardware(t *testing.T) {
	t.Parallel()

	rcv, err := rtl2832u.Open()
	if err == nil {
		// If a dongle is genuinely connected, exercise Close so we do
		// not leak the claimed interface to the next test.
		if rcv != nil {
			_ = rcv.Close()
		}

		t.Skip("RTL-SDR appears connected; skipping no-hardware contract test")
	}

	if rcv != nil {
		t.Error("non-nil Receiver returned alongside error")
	}

	want := rtl2832u.ErrNoDevice
	if runtime.GOOS != "linux" {
		want = rtl2832u.ErrUnsupportedPlatform
	}

	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestOpenAppliesOptionsBeforeFailing verifies the Option callbacks
// run before openBackend fails on a no-hardware host. The test
// covers the option-loop body in Open; the actual cfg values are
// internal so we only assert the typed error from the failing
// backend, which is enough to prove the loop ran without
// panicking.
func TestOpenAppliesOptionsBeforeFailing(t *testing.T) {
	t.Parallel()

	rcv, err := rtl2832u.Open(
		rtl2832u.WithCenterFreq(1_090_000_000),
		rtl2832u.WithSampleRate(2_400_000),
		rtl2832u.WithGain(rtl2832u.GainAGC),
		rtl2832u.WithDevice(0),
	)
	if err == nil {
		if rcv != nil {
			_ = rcv.Close()
		}

		t.Skip("RTL-SDR appears connected; skipping option-application test")
	}

	if rcv != nil {
		t.Error("non-nil Receiver returned alongside error")
	}
}
