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
configure demod for R820T real-IF (I-only ADC, spectrum inversion) →
R860 detect (chip-ID 0x96 per datasheet §6) → seed register table over I²C →
rate-driven IF filter (Tuner.InitializeForSampleRate) →
align demod DDC with tuner IF → sample-rate divider → PLL lock →
per-stage gain → IF channel-filter overrides → optional bias-tee →
optional auto-gain search → URB-ring bulk read → IQ samples on the wire
```

Coverage: ≥ 98 % of statements across the library and the `rtl-probe` operator tool (`make cover`); lint clean against `golangci-lint v2.12` with `default: all` and `revive` enable-all-rules at line-length 120.

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
| `Receiver.Read(ctx, p) (int, error)` | Returns up to `len(p)` bytes of interleaved `I, Q, I, Q, ...` from the next completed URB. One Read maps to one URB; wrap with `io.ReadFull` for fill-to-buffer semantics. Buffer sizes between 16 KiB and 256 KiB match the URB sizes used by `librtlsdr` / `dump1090`. Cancel via `ctx`. |
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
| `WithLNAGain(stage GainStage)` | Per-stage escape hatch for callers already holding a `GainStage` (e.g. from `VGAStepForCentiDB`). `AutoGain` for AGC, `ManualGainStep(0..15)` to pin. Last write wins. |
| `WithMixerGain(stage GainStage)` | Same shape; controls the post-mixer amplifier. |
| `WithVGAGain(stage GainStage)` | VGA scale is documented (3.5 dB/step from -12.0 dB per R860 datasheet table 6-3). Use `VGAStepForCentiDB(centi)` if a centi-dB target reads better than a step index. |
| `WithManualLNAGain(step uint8)` | User-input-friendly variant: pins the LNA to a 4-bit code (0..15) and surfaces a `Warn`-level log entry through `WithLogger` if `step` exceeds 15 (`ManualGainStep` clamps silently — debug-hostile). Same for `WithManualMixerGain` / `WithManualVGAGain`. |
| `WithAutoGain()` | Closed-loop search at Open time: pin Mixer + VGA at max, walk LNA from step 15 downward until the chip's `if_agc_val` mean (RTL2832U §8.1.5) climbs above the over-gained threshold. Converges in 1–3 iterations on most chains; logs the result via the configured `slog.Logger`. |

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
| `Receiver.ReadSampleStats(ctx, targetSamples) (SampleStats, error)` | Reads `targetSamples` I/Q pairs and returns host-side magnitude statistics: RMS, peak, saturation fraction, DC offsets, and a 64-bucket magnitude histogram. Primary chain-quality probe. Works on all tuners — computed from the bulk endpoint, independent of the chip's AGC state. |
| `ComputeSampleStats(raw []byte) SampleStats` | Pure-function variant: when the caller already has the raw I/Q bytes in hand (replay mode, shared buffers feeding an FFT, etc.) and wants the stats without going through a Read pass. |
| `Receiver.SignalStats() (SignalStats, error)` | Point-in-time read of the chip's `if_agc_val` / `rf_agc_val` / `aagc_lock` (RTL2832U §8.1.5). Note: on the R820T2 path the demod-side RF/IF AGC loops are intentionally disabled to avoid a feedback fight with the tuner's VGA, so these register values are effectively static — use `ReadSampleStats` instead for diagnostics on that tuner. |
| `Receiver.AutoTuneGain(ctx, opts) (AutoTuneResult, error)` | Closed-loop gain search at Open time. Pins Mixer + VGA at max, walks LNA down until `SampleStats.SaturationFrac` drops below the threshold (default 2 %, sized to ADS-B's burst statistics). |
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

## Operator tool: `rtl-probe`

`cmd/rtl-probe` is a small CLI that ships alongside the library. Three modes, all routed through the same opener so the flag plumbing is shared:

```sh
# one-shot stats line (legacy, fast)
rtl-probe --probe-bytes 65536

# IQ capture (rtl_sdr / dump1090 --ifile compatible)
rtl-probe --capture cap.iq --capture-bytes $((2*1024*1024))

# interactive TUI: live histogram + strip chart + spectrum
rtl-probe --tui
```

The `--tui` mode opens a tview UI driven by a sampling goroutine that pulls raw I/Q chunks (~5 Hz) and derives both `SampleStats` and an FFT spectrum from the same buffer:

```
┌─ header: live samples / RMS / peak / sat % / DC values ─────────────────┐
├─ status: smoothed GOOD / MARGINAL / BAD with offender labels ───────────┤
├─ advice: actionable hints ("reduce gain", "DC offset large", …) ────────┤
├──────────────────────────────────┬──────────────────────────────────────┤
│ ┌─ magnitude histogram ────────┐ │ ┌─ strip chart (last ~30 s) ───────┐ │
│ │ colour by X-axis position:   │ │ │ rms / sat / peak / dcI / dcQ as  │ │
│ │  red < 12 mag (under-gained) │ │ │ block-char traces; each cell     │ │
│ │  green 25–130 (healthy)      │ │ │ colour-coded by per-series grade │ │
│ │  red > 160 mag (clipping)    │ │ └──────────────────────────────────┘ │
│ │ y-axis: % of max bucket      │ │ ┌─ spectrum (~54 ms Welch average) ┐ │
│ │ x-axis: 0..181 |I+jQ|        │ │ │ row-coloured (VU-meter style),   │ │
│ └──────────────────────────────┘ │ │ baseline ╌╌ at long-term floor,  │ │
│                                  │ │ │ at carrier, ▲ on the axis     │ │
│                                  │ └──────────────────────────────────┘ │
├─────────────────────────────────────────────────────────────────────────┤
└─ footer: LNA=N Mix=N VGA=N bias=on/off · keybinds · errors / auto-tune ─┘
```

Live controls (footer keybinds):

- **`l` / `L`** — step LNA up / down
- **`m` / `M`** — step Mixer up / down
- **`v` / `V`** — step VGA up / down
- **`b`** — toggle bias-tee
- **`a`** — run TUI-driven gain auto-tune (pins Mixer + VGA at 15, walks LNA down until `SaturationFrac` ≤ 5 % or LNA hits 0). Press `a` again while it's running to cancel. The footer shows live progress (`probing LNA=N (step k/16)`) and, after a run completes, a green summary (`auto-tune: LNA=N sat=X.XX% in Y steps`) that stays visible so you can compare against manual exploration.
- **`s`** — run TUI-driven 3D sweep across LNA × Mixer × VGA on a stride-3 grid (`{0,3,6,9,12,15}` per axis = 216 cells, ~1.5–2.5 min). Picks the cell with the highest RMS where `SaturationFrac` ≤ 5 %; falls back to the lowest-saturation cell if none meet the threshold. Footer shows `cell N/216 · probing LNA=L Mix=M VGA=V · best LNA=A Mix=B VGA=C (sat=X.XX% rms=Y.Y)` live; on completion the walker applies the winning cell and the footer shows the sticky summary. Press `s` again to cancel. Mutually exclusive with `a`.
- **`q`** / **`Esc`** — quit

Manual gain keys are suppressed while either walker is running so the operator's keystrokes don't race the walker's `Set*` calls. The footer's completion summary always shows whichever of auto-tune / sweep finished most recently (`completedAt` timestamp comparison).

Key design choices:

- **Welch averaging in the spectrum**: each frame runs ~256 short FFTs over the 128 KiB sample window (50 % overlap, Hann window) and averages in the power domain before converting to dB. Stable noise floor; bursts don't dominate single frames.
- **Slow-decay scale tracker**: the spectrum's Y-axis top snaps up to new peaks immediately but decays at 5 dB/s — the chart doesn't constantly rescale on transient signals.
- **Long-term baseline tracker**: a 30 s EMA of the 25th-percentile bin, drawn as a horizontal dashed line. Anything sticking up above the line is signal worth attention.
- **Carrier marker `│`**: a vertical guide through the chart at the tuned-frequency column, plus a `▲` on the X-axis. Visual anchor for "is the peak where I tuned?".
- **Status / advice debounce**: both banners read from a 20-frame trailing average so they don't flicker on bursty traffic.
- **TUI-side auto-tune**: the walker drives `Receiver.SetLNAGain` directly and reads `SaturationFrac` off the live sampler rather than calling `Receiver.AutoTuneGain` — the library's variant issues its own `Read` calls, and the receiver is single-producer on the bulk endpoint. This way the strip chart visibly updates as auto-tune steps so the operator can see the convergence.

The TUI is built on `github.com/rivo/tview` + `github.com/gdamore/tcell` — pulled in only by `cmd/rtl-probe`, so library consumers (e.g. downstream demodulators) don't transitively depend on them.

## Hardware target

Tested on:

- **Realtek RTL2838DUB** (USB ID `0x0bda:0x2838`) with a Rafael Micro R820T2 (datasheet-equivalent to R860).
- **HackerGadgets All-In-One** board on a uConsole + Raspberry Pi CM4 (Linux/arm64).
- 28.8 MHz reference crystal — the value baked into `referenceClockHz`. Boards with a different TCXO need either `WithFrequencyCorrection(ppm)` (small drift) or, eventually, an EEPROM reader for boards with a 16 MHz reference.

The chip-ID gate is strict against the datasheet's fixed value (R0 must read `0x96` post-bitrev per R860 §6 Read Mode); dongles with anything else return `ErrTunerNotPresent` so the failure mode is "wrong tuner / dead silicon" instead of a cascade of register-write errors.

## Build & test

```sh
make                  # fmt + vet + test (race + cover)
make lint             # golangci-lint run ./...
make cover            # produces coverage.html
make test-integration # go test -race -tags=integration ./...
```

Cross-compile to the deploy target:

```sh
GOOS=linux GOARCH=arm64 go build ./...
```

`go test ./...` runs the offline test suite (controller mocks; no USB needed) on every supported platform. Integration tests that touch real hardware live behind the `integration` build tag and are not run by default; see `integration_linux_test.go` and use `make test-integration` on a host with a dongle attached.

## Roadmap

- **Push the remaining tuner-specific demod writes behind the `Tuner` interface.**
  The rate-driven IF-filter selection is already there — every `Tuner`
  implements `InitializeForSampleRate(rateHz) (intFreqHz, error)` and
  the orchestrator pipes the returned IF into the demod's DDC. What is
  *not* yet behind the interface: `configureForR820T` (the real-IF /
  I-only-ADC / spectrum-inversion register dance) is still called
  unconditionally in the bring-up path because R820T is the only
  silicon that ships today. The next tuner (R828D, future Rafael parts)
  would want this generalised — likely an `Init(chip) error` method on
  the `Tuner` interface so each silicon owns its demod-side prerequisites.

## License

Business Source License 1.1. See `LICENSE`. Free for non-commercial use; commercial integration requires a paid license. Converts to Apache-2.0 on the change date (2036-05-04).

For commercial licensing, contact the licensor (see `LICENSE` for details).

## References

- [RTL2832U datasheet](https://www.realtek.com/) (Realtek; sections cited inline as `§N.N` or `R<addr>`).
- [R860 datasheet](https://www.rafaelmicro.com/) (Rafael Micro; "table 6-3", etc.).
- [`osmocom/rtl-sdr`](https://github.com/osmocom/rtl-sdr) — BSD-2 reference implementation; transliterated tables (gain ladder, FIR coefficients) are attributed inline.
