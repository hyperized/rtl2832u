package rtl2832u

import (
	"errors"
	"fmt"
)

// R820T / R860 primer
// ===================
//
// The R820T (and its rebranded successor the R820T2 / R860) is a
// silicon-tuned superheterodyne receiver chip from Rafael Micro.
// It is the front-end RF stage in most modern RTL-SDR dongles:
// roughly 28 MHz to 1.766 GHz, with on-chip LNA, mixer, IF filter,
// and PLL-driven local oscillator.
//
// The chip lives on the RTL2832U's I2C bus. The 7-bit slave address
// is 0x1a; the 8-bit form (with the R/W bit appended) is 0x34, and
// that's the form the demod's I2C engine wants in the USB control
// transfer's wValue. It is not addressable from the host directly —
// every write or read passes through the demodulator's I2C repeater
// (see i2c.go).
//
// Why it's complicated
// --------------------
//   - No public datasheet. All knowledge of the register map comes
//     from osmocom's reverse-engineered driver. Bit semantics that
//     look arbitrary often are: we transcribe them and trust.
//   - Most registers are write-only. The chip exposes only its
//     ID and a few status bits via reads. Anything else needs a
//     shadow register array on the host to do read-modify-write
//     without losing state.
//   - Tuning is non-trivial. SetFreq is not a single register
//     write — it picks a VCO band, computes integer-N and a 16-bit
//     sigma-delta fractional, then writes ~7 PLL-related
//     registers in the right order. The PLL math lives separately
//     in tuner_r860_pll.go.
//
// What this driver does
// ---------------------
//   - Detects the chip by reading register 0x00, expecting 0x69
//     (the Rafael Micro family marker — same on R820T, R820T2,
//     R860, R828D).
//   - Initialises the chip with a 27-byte seed register table
//     (registers 0x05..0x1f) — the values Realtek's Windows DAB/FM
//     driver uses as a sane baseline.
//   - Tunes via SetFreq (PLL synthesis) and, when the band tables
//     land, configures the right RF/IF/LNA paths via setMux.
//
// Source for register map and init values: osmocom rtl-sdr's
// tuner_r82xx.c (BSD-2). Refresh by re-reading the upstream file:
// https://github.com/osmocom/rtl-sdr/blob/master/src/tuner_r82xx.c

// --- R860 / R820T silicon constants ---
//
// Pulled from osmocom librtlsdr's tuner_r82xx.c plus its header.
// Refresh via the URL above whenever the upstream driver changes.
const (
	// r860I2CAddr is the value written into wValue on every USB
	// control transfer that crosses the chip's I2C repeater to the
	// tuner. It is the 8-bit form (7-bit slave address 0x1a shifted
	// up by one to leave room for the R/W bit) — librtlsdr's
	// R820T_I2C_ADDR. The chip's I2C engine ignores the LSB because
	// the direction comes from the USB transfer itself; what matters
	// is that the upper 7 bits land at 0x1a. Putting the un-shifted
	// 0x1a here addresses 0x0d on the bus and stalls the endpoint.
	r860I2CAddr uint8 = 0x34

	// r860RegCount is the size of the tuner's register file
	// (addresses 0x00..0x1f). The lower five are read-only chip-ID
	// and status; the rest are programmable.
	r860RegCount = 0x20

	// r860ChipIDReg is the chip's read-only ID/version register
	// (register R0 in the R860 datasheet). Per datasheet §6, R0
	// returns a fixed reference value of 0x96 — its sole purpose is
	// to confirm the I2C transport is reachable and the chip is in
	// the Rafael Micro tuner family.
	//
	// r860ChipIDValue is that fixed reference, used post-bitrev:
	// the chip transmits I2C reads LSB-first (datasheet §6 Read
	// Mode), so the wire byte 0x69 reverses to 0x96 once readRegisters
	// applies r860BitRev. A mismatch means the chip is silent, on the
	// wrong I2C address, or not an R820T-family tuner at all.
	r860ChipIDReg   uint8 = 0x00
	r860ChipIDValue uint8 = 0x96

	// r860InitBaseReg is the lowest writable register. The init
	// seed table writes a contiguous block from here to register
	// 0x1f.
	r860InitBaseReg uint8 = 0x05
)

// r860InitWriteCount is the number of registers written during init
// (0x05..0x1f inclusive).
const r860InitWriteCount = 0x1f - int(r860InitBaseReg) + 1

// r860InitValues mirrors tuner_r82xx_init_array in osmocom
// librtlsdr's tuner_r82xx.c. Each entry programs the corresponding
// register starting at r860InitBaseReg.
//
// The bit-by-bit semantics aren't documented in any public datasheet,
// so the only safe way to refresh this is to re-read the upstream C
// source if it ever changes.
//
//nolint:gochecknoglobals // immutable register table.
var r860InitValues = [r860InitWriteCount]uint8{
	/* 0x05 */ 0x83,
	/* 0x06 */ 0x32,
	/* 0x07 */ 0x75,
	/* 0x08 */ 0xc0,
	/* 0x09 */ 0x40,
	/* 0x0a */ 0xd6,
	/* 0x0b */ 0x6c,
	/* 0x0c */ 0xf5,
	/* 0x0d */ 0x63,
	/* 0x0e */ 0x75,
	/* 0x0f */ 0x68,
	/* 0x10 */ 0x6c,
	/* 0x11 */ 0x83,
	/* 0x12 */ 0x80,
	/* 0x13 */ 0x00,
	/* 0x14 */ 0x0f,
	/* 0x15 */ 0x00,
	/* 0x16 */ 0xc0,
	/* 0x17 */ 0x30,
	/* 0x18 */ 0x48,
	/* 0x19 */ 0xcc,
	/* 0x1a */ 0x60,
	/* 0x1b */ 0x00,
	/* 0x1c */ 0x54,
	/* 0x1d */ 0xae,
	/* 0x1e */ 0x4a,
	/* 0x1f */ 0xc0,
}

// maxR860I2CMsgLen is the per-transaction byte cap for I2C writes
// crossing the chip's repeater to the R820T/R860. It includes the
// leading register-pointer byte, so the maximum number of register
// values per transaction is maxR860I2CMsgLen - 1.
//
// 8 is the value librtlsdr's r82xx_config carries for the RTL-SDR
// stack. Real silicon stalls (EPIPE) when a single I2C write
// exceeds this — we hit that during the 27-byte init seed before
// adding chunking. Smaller values would also work but waste USB
// round-trips; 8 is the documented sweet spot.
const maxR860I2CMsgLen = 8

// ErrTunerNotPresent is returned when the chip-ID register does not
// match the datasheet's fixed reference value (R0 = 0x96 per
// datasheet §6, post-bitrev). Either the I2C transport is silent,
// the slave address is wrong, or the dongle ships a different
// tuner family (E4000 / FC0012 / FC0013) that needs its own driver.
var ErrTunerNotPresent = errors.New(
	"tuner: no Rafael Micro R820T/R860 detected on the chip's I2C bus " +
		"(run `lsusb -d 0bda:` and check the dongle's marketing material — " +
		"some clones ship E4000/FC0012 tuners, which need a different driver)",
)

// errR860RegRangeOutOfBounds is the static sentinel for a writeRegisters
// call that would walk past the tuner's 32-byte register file. Reaching
// this is a programming error inside the driver — the I2C engine itself
// would silently wrap and clobber registers we do not own.
var errR860RegRangeOutOfBounds = errors.New("r860: register range exceeds device register file")

// R860 drives a Rafael Micro R820T / R820T2 / R860 tuner over the
// chip's I2C bridge. Construct one with NewR860 and pass it to
// rtl2832u.SetCenterFreq.
//
// xtalHz is the host's PLL reference (the dongle's TCXO frequency
// — typically 28.8 MHz). It's stored on the tuner because PLL
// synthesis depends on it; threading it through the chip would
// duplicate state without buying clarity.
//
// The shadow register array caches every byte we have written so
// later read-modify-write operations (the PLL bit-banging in
// SetFreq in particular) can update individual bits without
// re-fetching state — most R860 registers are write-only on real
// silicon.
type R860 struct {
	i2c    i2cTransport
	xtalHz uint32
	shadow [r860RegCount]uint8
}

// NewR860 detects an R820T/R860 on the chip's I2C bus and writes
// the seed register table. The returned tuner is ready for SetFreq.
//
// xtalHz is the dongle's reference clock (28.8 MHz on every board
// this driver currently targets). Pass it explicitly so a
// hypothetical 16 MHz R828D variant can be supported by the same
// code path without per-construction guessing.
//
// We treat the entire detect+init flow as a single construction
// step so callers cannot end up holding a half-initialised tuner;
// any error rolls the whole operation back.
func NewR860(transport i2cTransport, xtalHz uint32) (*R860, error) {
	tuner := &R860{i2c: transport, xtalHz: xtalHz}

	if err := tuner.detect(); err != nil {
		return nil, err
	}

	if err := tuner.init(); err != nil {
		return nil, err
	}

	return tuner, nil
}

// Name implements Tuner. The string is used in log lines and error
// messages, so it stays a stable, human-readable label rather than
// something derived from a chip-ID register.
func (*R860) Name() string { return "R860" }

// detect verifies the tuner is a Rafael Micro R820T/R860 family
// chip by reading the fixed reference value at R0 (0x96 per
// datasheet §6). The transport-level bitrev is already applied by
// readRegister, so a healthy chip returns 0x96 directly. Any other
// value means either the I2C bus didn't reach the chip or this is
// a different tuner family (E4000, FC0012, etc.).
//
// withRepeater opens the chip's I2C bridge only for the time the
// read needs, then closes it again.
func (t *R860) detect() error {
	return t.withRepeater(func() error {
		got, err := t.readRegister(r860ChipIDReg)
		if err != nil {
			return err
		}

		if got != r860ChipIDValue {
			return fmt.Errorf("%w: register %#x = %#x, want %#x",
				ErrTunerNotPresent, r860ChipIDReg, got, r860ChipIDValue)
		}

		return nil
	})
}

// init writes the seed register table and then runs the post-seed
// configuration librtlsdr applies via r82xx_set_tv_standard +
// r82xx_sysfreq_sel. The seed table alone leaves loop-through
// enabled (R0x05 bit [7] = 1), which routes IF energy to a tuner
// pin instead of the internal baseband — observable as a dead Q
// channel at the host's IQ stream. applyPostInit clears the bit
// and programs the rest of the SDR-mode operating state.
//
// One I2C bridge bracket covers both phases so the chip's
// repeater opens once per init rather than twice.
func (t *R860) init() error {
	return t.withRepeater(func() error {
		if err := t.writeRegisters(r860InitBaseReg, r860InitValues[:]); err != nil {
			return err
		}

		return t.applyPostInit()
	})
}

// withRepeater opens the chip's I2C bridge, runs body, and closes it
// again. Closing happens via defer so the bridge always shuts even
// if body panics; the deferred close error only surfaces if body
// otherwise succeeded.
func (t *R860) withRepeater(body func() error) (err error) {
	if openErr := t.i2c.enableI2CRepeater(); openErr != nil {
		return fmt.Errorf("r860: open I2C: %w", openErr)
	}

	defer func() {
		if closeErr := t.i2c.disableI2CRepeater(); err == nil && closeErr != nil {
			err = fmt.Errorf("r860: close I2C: %w", closeErr)
		}
	}()

	return body()
}

// readRegister fetches one byte from the tuner.
func (t *R860) readRegister(reg uint8) (uint8, error) {
	var buf [1]byte
	if err := t.readRegisters(reg, buf[:]); err != nil {
		return 0, err
	}

	return buf[0], nil
}

// readRegisters fetches len(dst) bytes from the tuner starting at
// `start`. The chip's I2C engine auto-increments its read pointer,
// so a single (write-pointer, read-N) transaction covers the
// whole range.
//
// Returned bytes are bit-reversed by the R820T/R860's I2C read
// engine — every byte's bit order is flipped 0↔7, 1↔6, etc. We
// undo that here so callers see register values in their natural
// orientation. librtlsdr's r82xx_read does the same trick.
func (t *R860) readRegisters(start uint8, dst []byte) error {
	if int(start)+len(dst) > r860RegCount {
		return fmt.Errorf("%w: start=%#x len=%d, file size %d",
			errR860RegRangeOutOfBounds, start, len(dst), r860RegCount)
	}

	if err := t.i2c.i2cWrite(r860I2CAddr, []byte{start}); err != nil {
		return fmt.Errorf("r860: write read-pointer reg=%#x: %w", start, err)
	}

	if err := t.i2c.i2cRead(r860I2CAddr, dst); err != nil {
		return fmt.Errorf("r860: read regs starting at %#x: %w", start, err)
	}

	for i := range dst {
		dst[i] = r860BitRev(dst[i])
	}

	return nil
}

// writeRegister programs one register and updates the shadow copy
// atomically. Use this for full-byte writes; for partial bit
// updates, use writeRegisterMasked instead.
func (t *R860) writeRegister(reg, val uint8) error {
	if err := t.i2c.i2cWrite(r860I2CAddr, []byte{reg, val}); err != nil {
		return fmt.Errorf("r860: write reg=%#x: %w", reg, err)
	}

	t.shadow[reg] = val

	return nil
}

// writeRegisterMasked is the read-modify-write primitive: it
// computes the new register value by clearing the bits in `mask`
// from the shadow and OR-ing in the masked bits of `value`, then
// writes the result. Most R860 registers pack several unrelated
// fields into eight bits; the chip is silently wrong if any
// field's bits are clobbered.
//
// Always writes (even if the new value matches the shadow). This
// matches librtlsdr and avoids hidden bugs when the shadow has
// drifted from real hardware state.
func (t *R860) writeRegisterMasked(reg, value, mask uint8) error {
	current := t.shadow[reg]
	updated := (current &^ mask) | (value & mask)

	return t.writeRegister(reg, updated)
}

// writeRegisters programs a contiguous range of registers, splitting
// the payload across as many I2C transactions as the chip's per-
// transaction byte cap requires (see maxR860I2CMsgLen). The R860's
// auto-incrementing register pointer means we just bump the start
// register for each chunk and the chip resumes where it left off.
//
// The shadow copy is updated chunk-by-chunk so a partial failure
// still leaves the host's view consistent with whatever wire writes
// did succeed.
func (t *R860) writeRegisters(start uint8, values []uint8) error {
	if int(start)+len(values) > r860RegCount {
		return fmt.Errorf("%w: start=%#x len=%d, file size %d",
			errR860RegRangeOutOfBounds, start, len(values), r860RegCount)
	}

	const maxDataPerChunk = maxR860I2CMsgLen - 1 // one byte goes to the register pointer

	for offset := 0; offset < len(values); offset += maxDataPerChunk {
		chunkLen := min(len(values)-offset, maxDataPerChunk)
		chunkStart := start + uint8(offset) //nolint:gosec // offset bounded by r860RegCount (32) above.

		payload := make([]byte, 1+chunkLen)
		payload[0] = chunkStart
		copy(payload[1:], values[offset:offset+chunkLen])

		if err := t.i2c.i2cWrite(r860I2CAddr, payload); err != nil {
			return fmt.Errorf("r860: write %d regs starting at %#x: %w", chunkLen, chunkStart, err)
		}

		copy(t.shadow[int(chunkStart):int(chunkStart)+chunkLen], values[offset:offset+chunkLen])
	}

	return nil
}

// r860BitRevLUT maps a 4-bit nibble to its bit-reversed form. Two
// LUT lookups (one per nibble) reverse a full byte without a loop.
// LUT contents follow librtlsdr's r82xx_bitrev_lut.
//
//nolint:gochecknoglobals // immutable lookup table.
var r860BitRevLUT = [16]uint8{
	0x0, 0x8, 0x4, 0xc, 0x2, 0xa, 0x6, 0xe,
	0x1, 0x9, 0x5, 0xd, 0x3, 0xb, 0x7, 0xf,
}

// r860BitRev reverses the bit order of a byte. The R820T/R860's
// I2C read engine returns bytes in this bit-reversed form;
// readRegisters applies r860BitRev to undo it before exposing
// the value to callers.
func r860BitRev(b uint8) uint8 {
	const nibbleShift = 4

	return (r860BitRevLUT[b&0x0f] << nibbleShift) | r860BitRevLUT[b>>nibbleShift]
}
