// Package rtl2832u is a pure-Go driver for the Realtek RTL2832U
// software-defined radio demodulator paired with a Rafael Micro
// R820T / R860 tuner. The driver speaks usbfs directly via
// golang.org/x/sys/unix ioctls — no CGo, no libusb, no librtlsdr.
//
// The public API exposes a Receiver type opened through Open with
// functional options (WithCenterFreq, WithSampleRate, WithGain, …).
// Receiver.Read returns interleaved unsigned-8-bit IQ samples that
// downstream demodulators can process directly.
//
// Linux is the only platform with a real backend; non-Linux builds
// return ErrUnsupportedPlatform from Open so callers can compile
// and test on dev machines without a dongle attached.
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
