# rtl2832u

Pure Go driver for the Realtek RTL2832U SDR demodulator and the Rafael Micro R820T / R860 tuner. No CGo, no `librtlsdr`, no `libusb` — talks to the dongle directly through Linux usbfs ioctls.

## Status

Scaffolding only. Feature work lands commit-by-commit; see the git log for the iteration shape.

## Scope

- USB control + bulk transfer over Linux usbfs (`USBDEVFS_*` ioctls via `golang.org/x/sys/unix`).
- RTL2832U baseband init: FIR coefficients, AGC, Zero-IF, sample-rate divider, demod sync.
- R820T / R860 tuner: chip-ID gate, register seed table, PLL synthesis, per-band tracking-filter / mux, IF channel-filter (FILT_BW / FILT_CODE / HPF / FILTER_EXT), per-stage gain (LNA / Mixer / VGA in both manual and AGC modes), and the librtlsdr-compatible single-knob gain ladder.
- Async-equivalent bulk read via a URB ring with EINTR-safe REAPURB.
- Diagnostic + tuning helpers: `SignalStats` (chip AGC readback per RTL2832U §8.1.5), closed-loop `AutoTuneGain`, bias-tee toggle, TCXO ppm correction.

The driver targets one RTL2832U + R820T/R860 dongle on Linux; darwin and other platforms ship a stub that returns `ErrUnsupportedPlatform` so `go test ./...` still works on dev machines.

## Build & test

```sh
make           # fmt + vet + test (race + cover)
make lint      # golangci-lint run ./...
```

## License

Business Source License 1.1. See `LICENSE`. Free for non-commercial use; commercial integration requires a paid license. Converts to Apache-2.0 on the change date.
