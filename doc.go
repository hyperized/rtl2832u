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
