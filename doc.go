// Package rtl2832u is a pure-Go driver for the Realtek RTL2832U
// software-defined-radio demodulator paired with a Rafael Micro
// R820T / R860 tuner. The driver speaks Linux usbfs directly via
// golang.org/x/sys/unix ioctls — no CGo on the deploy target, no
// libusb, no librtlsdr.
//
// # Public surface
//
// The package exposes a Receiver opened through Open with
// functional options:
//
//	rcv, err := rtl2832u.Open(
//	    rtl2832u.WithCenterFreq(1_090_000_000),
//	    rtl2832u.WithSampleRate(2_400_000),
//	    rtl2832u.WithAutoGain(),
//	)
//	if err != nil { return err }
//	defer rcv.Close()
//
//	buf := make([]byte, 32*1024)
//	n, err := rcv.Read(ctx, buf)
//	// buf[:n] is interleaved unsigned-8-bit IQ: I, Q, I, Q, ...
//	// (DC offset at 127). Feed to a demodulator.
//
// Configuration is set only at Open time; there are deliberately
// no setter methods on Receiver so the open device's parameters
// cannot drift mid-stream. Runtime adjustments — re-running the
// gain auto-tune after a band change, sampling the AGC state for
// diagnostics — are exposed as named methods (Receiver.AutoTuneGain,
// Receiver.SignalStats) rather than implicit Option re-application.
//
// # Platform support
//
// Linux is the only target with a real USB backend. On non-Linux
// builds (including darwin dev hosts) Open returns
// ErrUnsupportedPlatform so callers can compile and run the unit
// test suite without a dongle attached.
//
// # Hardware target
//
// The driver is hardware-validated on the Realtek RTL2838DUB
// (USB ID 0x0bda:0x2838) with a Rafael Micro R820T2 tuner. The
// chip-ID gate is strict against the R860 datasheet's fixed
// value (R0 must read 0x96 post-bitrev per §6 Read Mode); other
// tuners return ErrTunerNotPresent.
package rtl2832u

// DefaultCenterFreqHz is the ADS-B 1090 MHz Extended Squitter
// centre frequency. It's the default this package was developed
// against; reuse the package for other narrowband targets by
// passing WithCenterFreq.
const DefaultCenterFreqHz uint32 = 1_090_000_000

// DefaultSampleRateHz is the sample rate FlightAware dump1090
// uses — 2.4 samples per bit at the Mode S 1 µs bit period.
const DefaultSampleRateHz uint32 = 2_400_000

// GainAGC is the sentinel value that selects automatic gain
// control instead of a fixed tuner gain. -1 falls outside the
// chip's 0..15 step range so it cannot collide with a real value.
const GainAGC int = -1
