# rtl2832u

[![Go Reference](https://pkg.go.dev/badge/github.com/hyperized/rtl2832u.svg)](https://pkg.go.dev/github.com/hyperized/rtl2832u)

A pure-Go driver for the Realtek RTL2832U SDR demodulator paired with a Rafael Micro R820T / R860 tuner. The driver speaks Linux usbfs directly through `golang.org/x/sys/unix` ioctls — no CGo on the deploy target, no `libusb`, no `librtlsdr`. One module, drop it into any application that needs RTL-SDR IQ samples.

## Why pure Go?

- **Container-friendly**: the deploy artefact is a single static binary. No `apt-get install librtlsdr0` step in your Dockerfile, no shared-library version drift between dev and prod, no glibc surprises in distroless images.
- **Cross-compilable from anywhere**: `GOOS=linux GOARCH=arm64 go build` produces a uConsole-ready binary from a Mac without configuring a toolchain.
- **Auditable**: every register write, every USB transfer, every gain table is visible Go code with citations to the RTL2832U / R860 datasheets and (where transliterated) `osmocom/rtl-sdr` (BSD-2). No black box.

## Status

Hardware-validated end-to-end on a Realtek RTL2838 + R820T2 dongle on a Raspberry Pi CM4 (uConsole, Linux/arm64). The flow that already works on real silicon:

```
sysfs enumerate → claim usbfs interface → RTL2832U baseband init →
sample-rate divider → R860 detect (chip-ID 0x96 per datasheet §6) →
seed register table over I²C → PLL lock → per-stage gain →
IF channel-filter → optional bias-tee → optional auto-gain search →
URB-ring bulk read → IQ samples on the wire
```

Coverage: 99.7 % of statements (`make cover`); lint clean against `golangci-lint v2.12` with `default: all` and `revive` enable-all-rules at line-length 120.

## Quickstart

```go
package main

import (
	"context"
	"fmt"

	"github.com/hyperized/rtl2832u"
)

func main() {
	rcv, err := rtl2832u.Open(
		rtl2832u.WithCenterFreq(1_090_000_000), // 1090 MHz Mode S (the default; shown for clarity)
		rtl2832u.WithSampleRate(2_400_000),     // 2.4 MS/s, the FlightAware dump1090 default
		rtl2832u.WithAutoGain(),                // closed-loop gain search at Open time
	)
	if err != nil {
		panic(err)
	}
	defer rcv.Close()

	buf := make([]byte, 32*1024)
	n, err := rcv.Read(context.Background(), buf)
	if err != nil {
		panic(err)
	}

	// buf[:n] is interleaved unsigned-8-bit IQ: I, Q, I, Q, ...
	// (DC offset at 127). Feed it to your demodulator of choice.
	fmt.Printf("got %d IQ bytes\n", n)
}
```

## Public API tour

The driver exposes a single `Receiver` opened through `Open` with functional options. Once open, `Read` blocks until the requested IQ chunk is available and `Close` releases the device.

### Open / Read / Close

| | |
|---|---|
| `Open(opts ...Option) (*Receiver, error)` | Enumerates dongles via sysfs, claims interface 0, runs the chip + tuner init dance. Returns `ErrNoDevice` if nothing matched, `ErrUnsupportedPlatform` on non-Linux. |
| `Receiver.Read(ctx, p) (int, error)` | Fills `p` with interleaved `I, Q, I, Q, ...` bytes. Buffer sizes between 16 KiB and 256 KiB match the URB sizes used by `librtlsdr` / `dump1090`. Cancel via `ctx`. |
| `Receiver.Close() error` | Releases the USB interface and closes the device handle. Idempotent. |

### Tuning options

| Option | Default | Notes |
|---|---|---|
| `WithCenterFreq(hz uint32)` | `DefaultCenterFreqHz` (1090 MHz) | Override for non-ADS-B targets within the tuner's 24 MHz – 1.766 GHz range. |
| `WithSampleRate(hz uint32)` | `DefaultSampleRateHz` (2.4 MS/s) | Valid sub-ranges: (225 kHz, 300 kHz] ∪ (900 kHz, 3.2 MHz]. The chip's gap between 300 kHz and 900 kHz produces nonsense; the validator rejects with a hint. |
| `WithDevice(index int)` | `0` (first enumerated) | Stable per boot, not across reboots — pin by serial number once an EEPROM reader lands. |
| `WithFrequencyCorrection(ppm int)` | `0` | Trims the chip's effective reference crystal so both `rsamp_ratio` and the R860 PLL compensate for a drifty TCXO. Clamped to ±`FrequencyCorrectionPPMMax` (1000 ppm), matching `librtlsdr`'s `rtlsdr_set_freq_correction` range. |

### Gain control

Three stages on the R820T / R860 tuner: LNA, post-mixer amp, VGA. Pin individually, hand to the chip's AGC, or let auto-tune pick.

| Option | Notes |
|---|---|
| `WithGain(tenthsDB int)` | librtlsdr-compatible single-knob ladder. Walks the chip's empirically-calibrated LNA + Mixer step pairs to land closest to the requested target; pins VGA at the librtlsdr default (+16 dB). Pass `GainAGC` to hand all three stages back to AGC. |
| `WithLNAGain(stage GainStage)` | Per-stage override. `AutoGain` for AGC, `ManualGainStep(0..15)` to pin. Last write wins. |
| `WithMixerGain(stage GainStage)` | Same shape; controls the post-mixer amplifier. |
| `WithVGAGain(stage GainStage)` | VGA scale is documented (3.5 dB/step from -12.0 dB per R860 datasheet table 6-3). Use `VGAStepForCentiDB(centi)` if a centi-dB target reads better than a step index. |
| `WithAutoGain()` | Closed-loop search at Open time: pin Mixer + VGA at max, walk LNA from step 15 downward until the chip's `if_agc_val` mean (RTL2832U §8.1.5) climbs above the over-gained threshold. Converges in 1–3 iterations on most chains; logs the result via stdlib `log`. |

`Receiver.AutoTuneGain(ctx, opts)` runs the same algorithm at runtime — useful after a band change or thermal event.

### IF channel filter (R860)

| Option | Register | Notes |
|---|---|---|
| `WithIFBandwidth(coarse, fine uint8)` | FILT_BW (0..3) + FILT_CODE (0..15) | Defaults to the chip's init seed (`FILT_BW=3 narrow, FILT_CODE=6 mid` — librtlsdr's). Lower numbers = wider, higher = narrower. |
| `WithIFHighPass(code uint8)` | R11 HPF[3:0] | Use the package's `R860HPF*` constants for the documented (corner, attenuation) tuples per code. |
| `WithFilterExt(enable bool)` | R30[6] | Datasheet labels this "filter extension for weak signal conditions"; the mechanism is undocumented. Toggle empirically. |

### Diagnostics + auxiliary

| | |
|---|---|
| `Receiver.SignalStats() (SignalStats, error)` | Point-in-time read of the chip's `if_agc_val` / `rf_agc_val` / `aagc_lock` (RTL2832U §8.1.5). Drives auto-tune and is useful as a chain-quality probe. |
| `Receiver.DroppedSampleChunks() uint64` | Cumulative count of sample chunks the URB ring had to discard because the consumer fell behind. A non-zero value over a long-running session means the demod is slower than the configured sample rate. |
| `WithBiasTee(enable bool)` | Toggles GPIO0 to switch the dongle's 4.5 V bias-tee output. Powers external active LNAs and SAW filters from the antenna coax on V3-class dongles. |
| `WithBiasTeeGPIO(gpio uint8, enable bool)` | Escape hatch for clones with non-standard bias-tee wiring. |

## Architecture

The package splits naturally along the two physical chips:

```
                      Receiver (sdr.go)                        ← public surface
                       │
                       ▼
                openBackend(cfg)                                ← platform-conditional
                       │
        ┌──────────────┴──────────────┐
        │                             │
   linuxBackend (usbfs_linux.go)   stub_other.go
        │                             │
   ┌────┴────┐                  returns ErrUnsupportedPlatform
   │         │
   ▼         ▼
 chip      tuner
 (rtl2832u.go,                           (tuner_r860*.go)
  init_chip.go,                          ─ chip-ID gate (0x96)
  sample_rate.go,                        ─ seed register table (chunked I²C)
  signal_stats.go,                       ─ PLL synthesis (integer-N + 16-bit Σ-Δ)
  bias_tee.go,                           ─ per-band setMux
  i2c.go,                                ─ IF channel filter
  center_freq.go)                        ─ per-stage gain (LNA / Mixer / VGA)
        │                                        ▲
        └────────────── Tuner interface ─────────┘  ← swap-in for E4000, FC0012, …
```

The chip driver (`rtl2832u`) only knows about baseband. Any front-end mixer that implements `Tuner` — `SetFreq`, the three `SetXGain` methods, the IF-filter trio — slots in without changes elsewhere. Today only R820T / R860 ships; the abstraction makes adding more straightforward.

The chip exposes its I²C bus to the tuner through a `repeater` register, encapsulated as `i2cTransport`. Tuners are written against that interface so they can be unit-tested with a small mock instead of booting the whole stack.

## Hardware target

Tested on:

- **Realtek RTL2838DUB** (USB ID `0x0bda:0x2838`) with a Rafael Micro R820T2 (datasheet-equivalent to R860).
- **HackerGadgets All-In-One** board on a uConsole + Raspberry Pi CM4 (Linux/arm64).
- 28.8 MHz reference crystal — the value baked into `referenceClockHz`. Boards with a different TCXO need either `WithFrequencyCorrection(ppm)` (small drift) or, eventually, an EEPROM reader for boards with a 16 MHz reference.

The chip-ID gate is strict against the datasheet's fixed value (R0 must read `0x96` post-bitrev per R860 §6 Read Mode); dongles with anything else return `ErrTunerNotPresent` so the failure mode is "wrong tuner / dead silicon" instead of a cascade of register-write errors.

## Build & test

```sh
make           # fmt + vet + test (race + cover)
make lint      # golangci-lint run ./...
make cover     # produces coverage.html
```

Cross-compile to the deploy target:

```sh
GOOS=linux GOARCH=arm64 go build ./...
```

`go test ./...` runs the offline test suite (controller mocks; no USB needed) on every supported platform. Integration tests that touch real hardware live behind a build tag and are not run by default; see `integration_linux_test.go`.

## License

Business Source License 1.1. See `LICENSE`. Free for non-commercial use; commercial integration requires a paid license. Converts to Apache-2.0 on the change date (2036-05-04).

For commercial licensing, contact the licensor (see `LICENSE` for details).

## References

- [RTL2832U datasheet](https://www.realtek.com/) (Realtek; sections cited inline as `§N.N` or `R<addr>`).
- [R860 datasheet](https://www.rafaelmicro.com/) (Rafael Micro; "table 6-3", etc.).
- [`osmocom/rtl-sdr`](https://github.com/osmocom/rtl-sdr) — BSD-2 reference implementation; transliterated tables (gain ladder, FIR coefficients) are attributed inline.
