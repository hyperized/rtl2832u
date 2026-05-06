package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

// op kind names used by fakeI2C.ops. Hoisted to constants so
// goconst stays satisfied and so tests cannot drift on the
// spelling.
const (
	opEnable  = "enable"
	opDisable = "disable"
	opWrite   = "write"
	opRead    = "read"
)

// expectedR860ShadowAfterInit returns the byte-level shadow state
// the tuner ends up in after NewR860 has run (seed table +
// applyPostInit). Each entry was hand-derived from the librtlsdr
// r82xx_set_tv_standard / r82xx_sysfreq_sel sequences for the
// (bw=3, type=DIGITAL_TV, delsys=SYS_DVBT, freq=0) configuration
// our init mirrors. Updating applyPostInit requires updating this
// table; the divergence is what the assertion catches.
//
// Index = R820T2 register address (0x05..0x1f).
//
//nolint:mnd // every entry is a documented register value, not a magic number.
func expectedR860ShadowAfterInit() map[int]uint8 {
	return map[int]uint8{
		0x05: 0x03, // seed 0x83 with bit[7] cleared (loop-through OFF)
		0x06: 0x12, // seed 0x32 with bits[5:4] = filt_gain (+3 dB, 6 MHz on)
		0x07: 0x75, // seed unchanged (img_r already 0)
		0x08: 0xc0, // seed unchanged
		0x09: 0x40, // seed unchanged
		0x0a: 0xd0, // seed 0xd6 with bits[4:0] = filt_q | fil_cal_code(=0)
		0x0b: 0x6b, // seed 0x6c with mask 0xef → hp_cor 0x6b
		0x0c: 0xf0, // seed 0xf5 with bits[3:0] cleared (init flag)
		0x0d: 0x53, // lna_vth_l (full byte)
		0x0e: 0x75, // mixer_vth_l (full byte; matches seed)
		0x0f: 0x68, // seed unchanged (flt_ext_widest already 0)
		0x10: 0x6c, // seed unchanged
		0x11: 0xbb, // seed 0x83 | cp_cur 0x38 → 0xbb
		0x12: 0x80, // seed unchanged
		0x13: 0x00, // seed unchanged
		0x14: 0x0f, // seed unchanged
		0x15: 0x00, // seed unchanged
		0x16: 0xc0, // seed unchanged
		0x17: 0x30, // seed unchanged (div_buf_cur same as seed)
		0x18: 0x48, // seed unchanged
		0x19: 0xec, // seed 0xcc | polyfil_cur 0x60 → 0xec
		0x1a: 0x60, // 0x60 → 0x70 (AGC clk 250 Hz) → 0x60 (AGC clk 60 Hz)
		0x1b: 0x00, // seed unchanged
		0x1c: 0x24, // mixer_top + discharge mode (mixer_top & 0x04 = 0x04)
		0x1d: 0xdd, // seed 0xae → 0xed (LNA top initial) → 0xc5 (clear) → 0xdd (LNA top = 3)
		0x1e: 0x6e, // ext_enable + lna_discharge
		0x1f: 0x40, // seed 0xc0 with bit[7] cleared (lt_att enable)
	}
}

// fakeI2C implements i2cTransport for tuner-side tests. It records
// every operation in order so assertions can verify the bracketed
// open / write / close discipline tuner code is supposed to follow.
type fakeI2C struct {
	ops []fakeI2COp

	// readResponses is a FIFO queue: the next i2cRead pops one and
	// copies its bytes into dst. Empty queue → empty response (the
	// caller's short-read handling kicks in).
	readResponses [][]byte

	// Per-operation error injection. A non-nil entry causes the
	// corresponding op to fail before any state is captured.
	enableErr  error
	disableErr error
	writeErr   error
	readErr    error

	// failWritesEnabled gates failWritesAfter so its zero value
	// doesn't accidentally fail every write. When true, the Nth
	// write attempt onwards (counting from 1) returns
	// errFakeControlOut, where N = failWritesAfter + 1.
	failWritesEnabled bool
	failWritesAfter   int
	writeCount        int
}

type fakeI2COp struct {
	kind string // "enable", "disable", "write", "read"
	addr uint8  // for write/read
	data []byte // for write: payload; for read: the bytes returned
}

func (f *fakeI2C) enableI2CRepeater() error {
	if f.enableErr != nil {
		return f.enableErr
	}

	f.ops = append(f.ops, fakeI2COp{kind: opEnable})

	return nil
}

func (f *fakeI2C) disableI2CRepeater() error {
	if f.disableErr != nil {
		return f.disableErr
	}

	f.ops = append(f.ops, fakeI2COp{kind: opDisable})

	return nil
}

func (f *fakeI2C) i2cWrite(addr uint8, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}

	f.writeCount++

	if f.failWritesEnabled && f.writeCount > f.failWritesAfter {
		return errFakeControlOut
	}

	f.ops = append(f.ops, fakeI2COp{kind: opWrite, addr: addr, data: append([]byte(nil), data...)})

	return nil
}

func (f *fakeI2C) i2cRead(addr uint8, dst []byte) error {
	if f.readErr != nil {
		return f.readErr
	}

	var resp []byte
	if len(f.readResponses) > 0 {
		resp = f.readResponses[0]
		f.readResponses = f.readResponses[1:]
	}

	count := copy(dst, resp)
	if count != len(dst) {
		// Caller handles short-read; mock just copies what it has.
		_ = count
	}

	f.ops = append(f.ops, fakeI2COp{kind: opRead, addr: addr, data: append([]byte(nil), dst...)})

	return nil
}

// chipIDOK gives detect a passing reading. R860 datasheet §6
// states reads transmit LSB-first, so the wire byte must be the
// bit-reversed form of r860ChipIDValue (0x96): 0x69 on the wire
// → 0x96 after readRegister applies r860BitRev → matches the
// fixed reference value.
func chipIDOK() *fakeI2C {
	return &fakeI2C{readResponses: [][]byte{{r860BitRev(r860ChipIDValue)}}}
}

// r860TestXtalHz is the reference clock used in every R860 test.
// 28.8 MHz matches the chip-level constant and the dongles
// targeted by this project.
const r860TestXtalHz uint32 = 28_800_000

func TestNewR860DetectsAndInits(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	if tuner.Name() != "R860" {
		t.Errorf("Name = %q, want R860", tuner.Name())
	}

	// Expected I2C op sequence:
	//   detect:           enable, write [chipIDReg], read 1 byte, disable
	//   init bracket:     enable, N chunked seed-table writes,
	//                     postinit single-byte writes, disable
	// 27 init bytes / (maxR860I2CMsgLen-1 = 7) = 4 chunks.
	// applyPostInit issues one single-byte write per
	// writeRegisterMasked call inside the same bracket, so they
	// add to the bus.ops sequence after the chunked seed writes
	// without an extra enable/disable pair.
	const (
		initChunkCount    = (r860InitWriteCount + (maxR860I2CMsgLen - 2)) / (maxR860I2CMsgLen - 1)
		postInitWriteOps  = 27 // 10 set-TV-standard + 9 sysfreq common + 8 sysfreq digital-TV
		detectOps         = 4
		initBracketBookOp = 2
	)

	wantOps := detectOps + initBracketBookOp + initChunkCount + postInitWriteOps
	if got := len(bus.ops); got != wantOps {
		t.Fatalf("ops count = %d, want %d (detect %d + init bracket %d + chunks %d + postinit %d)",
			got, wantOps, detectOps, initBracketBookOp, initChunkCount, postInitWriteOps)
	}

	// First five ops are fixed: detect (enable/write/read/disable)
	// then init bracket open (enable). The last op closes the init
	// bracket (disable). Everything between is writes — verified by
	// the chunked-payload reassembly below.
	if got := []string{
		bus.ops[0].kind,
		bus.ops[1].kind,
		bus.ops[2].kind,
		bus.ops[3].kind,
		bus.ops[4].kind,
		bus.ops[len(bus.ops)-1].kind,
	}; !reflect.DeepEqual(got, []string{opEnable, opWrite, opRead, opDisable, opEnable, opDisable}) {
		t.Errorf("bracket op kinds = %#v, want enable/write/read/disable/enable/.../disable", got)
	}

	for i := 5; i < len(bus.ops)-1; i++ {
		if bus.ops[i].kind != opWrite {
			t.Errorf("ops[%d].kind = %q, want write", i, bus.ops[i].kind)
		}
	}

	// Detect's read-pointer write must be a single-byte transaction
	// addressing the chip-ID register.
	if got := bus.ops[1].data; !reflect.DeepEqual(got, []byte{r860ChipIDReg}) {
		t.Errorf("detect read-pointer payload = %#v, want [%#x]", got, r860ChipIDReg)
	}

	// Reassemble the chunked init writes and compare against the
	// original seed table — order, contents, and the auto-incremented
	// register pointer per chunk must all line up. Postinit writes
	// (single-byte, mixed addresses) live after the seed chunks; they
	// are validated by TestR860ShadowMatchesInit, not here.
	reassembled := make([]byte, 0, r860InitWriteCount)

	for chunk := range initChunkCount {
		chunkOp := bus.ops[5+chunk]
		expectedStart := r860InitBaseReg + uint8(chunk*(maxR860I2CMsgLen-1))

		if chunkOp.data[0] != expectedStart {
			t.Errorf("chunk %d start = %#x, want %#x", chunk, chunkOp.data[0], expectedStart)
		}

		reassembled = append(reassembled, chunkOp.data[1:]...)
	}

	if !reflect.DeepEqual(reassembled, r860InitValues[:]) {
		t.Errorf("reassembled init payload mismatch:\n got %#v\nwant %#v", reassembled, r860InitValues[:])
	}
}

// TestNewR860RejectsWrongChipID verifies the strict chip-ID gate:
// any wire byte that does not bit-reverse to r860ChipIDValue (0x96
// per datasheet §6) must fail detection with ErrTunerNotPresent.
// 0xAA is a value that bit-reverses to 0x55 — no Rafael Micro
// tuner returns it.
func TestNewR860RejectsWrongChipID(t *testing.T) {
	t.Parallel()

	bus := &fakeI2C{readResponses: [][]byte{{0xAA}}}

	_, err := NewR860(bus, r860TestXtalHz)
	if !errors.Is(err, ErrTunerNotPresent) {
		t.Fatalf("err = %v, want ErrTunerNotPresent", err)
	}

	// Detection still ran enable + write + read + disable; the
	// withRepeater deferred close must always fire.
	if got, want := len(bus.ops), 4; got != want {
		t.Errorf("ops count = %d, want %d (enable + write + read + disable)", got, want)
	}

	if last := bus.ops[len(bus.ops)-1].kind; last != opDisable {
		t.Errorf("last op = %q, want %q (deferred close did not fire)", last, opDisable)
	}
}

func TestNewR860DetectReadFailureSurfaces(t *testing.T) {
	t.Parallel()

	bus := &fakeI2C{readErr: errFakeControlIn}

	if _, err := NewR860(bus, r860TestXtalHz); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

func TestNewR860InitWriteFailureSurfaces(t *testing.T) {
	t.Parallel()

	// Detection succeeds (the chip-ID read works); the subsequent
	// init write fails.
	bus := &fakeI2C{
		readResponses: [][]byte{{r860BitRev(r860ChipIDValue)}},
	}

	tuner := &R860{i2c: bus, xtalHz: r860TestXtalHz}
	if err := tuner.detect(); err != nil {
		t.Fatalf("detect: %v", err)
	}

	bus.writeErr = errFakeControlOut

	err := tuner.init()
	if !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestNewR860InitFailureSurfacesThroughConstructor exercises the
// init-error branch in NewR860 itself: detect succeeds (one i2cWrite
// to set the read pointer), but the next write (init's first
// register seed) fails.
func TestNewR860InitFailureSurfacesThroughConstructor(t *testing.T) {
	t.Parallel()

	bus := &fakeI2C{
		readResponses:     [][]byte{{r860BitRev(r860ChipIDValue)}},
		failWritesEnabled: true,
		failWritesAfter:   1, // detect's read-pointer write goes through; init's first write fails
	}

	if _, err := NewR860(bus, r860TestXtalHz); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut (init failure)", err)
	}
}

func TestR860ShadowMatchesInit(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	// Every init register's shadow value should match what we
	// programmed after the seed write + applyPostInit; this is the
	// source of truth for later read-modify-write operations.
	for addr, want := range expectedR860ShadowAfterInit() {
		if got := tuner.shadow[addr]; got != want {
			t.Errorf("shadow[%#x] = %#x, want %#x", addr, got, want)
		}
	}

	// Lower-five (read-only chip ID + status) shadow entries are
	// untouched and stay zero.
	for addr := range int(r860InitBaseReg) {
		if tuner.shadow[addr] != 0 {
			t.Errorf("shadow[%#x] = %#x, want 0 (read-only registers untouched)",
				addr, tuner.shadow[addr])
		}
	}
}

func TestR860SatisfiesTunerInterface(t *testing.T) {
	t.Parallel()

	// Compile-time assertion via dynamic check; the var declaration
	// would force the test binary to fail to link, but checking via
	// a runtime cast surfaces the failure as a test diagnostic.
	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	var _ Tuner = tuner
}

func TestR860WriteRegistersRangeCheck(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	// 0x1f + 2 bytes would walk past the register file end (0x20).
	err = tuner.writeRegisters(0x1f, []uint8{0xAA, 0xBB})
	if !errors.Is(err, errR860RegRangeOutOfBounds) {
		t.Errorf("err = %v, want errR860RegRangeOutOfBounds", err)
	}
}

func TestR860ReadRegistersRangeCheck(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	if err := tuner.readRegisters(0x1f, make([]byte, 2)); !errors.Is(err, errR860RegRangeOutOfBounds) {
		t.Errorf("err = %v, want errR860RegRangeOutOfBounds", err)
	}
}

// TestR860ReadRegistersWriteFailure exercises the write-pointer
// error path inside readRegisters.
func TestR860ReadRegistersWriteFailure(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	bus.writeErr = errFakeControlOut

	if err := tuner.readRegisters(0x05, make([]byte, 1)); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestR860WithRepeaterCloseFailureSurfaces hits the deferred
// close-error path: the body succeeds, but disableI2CRepeater
// fails. The withRepeater helper must surface that error rather
// than swallowing it.
func TestR860WithRepeaterCloseFailureSurfaces(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	bus.disableErr = errFakeControlOut

	wrapErr := tuner.withRepeater(func() error { return nil })
	if !errors.Is(wrapErr, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut (deferred close)", wrapErr)
	}
}

// TestR860WithRepeaterOpenFailureSurfaces hits the open-error
// path before body runs.
func TestR860WithRepeaterOpenFailureSurfaces(t *testing.T) {
	t.Parallel()

	bus := chipIDOK()

	tuner, err := NewR860(bus, r860TestXtalHz)
	if err != nil {
		t.Fatalf("NewR860: %v", err)
	}

	bus.enableErr = errFakeControlOut

	bodyRan := false

	wrapErr := tuner.withRepeater(func() error {
		bodyRan = true

		return nil
	})

	if !errors.Is(wrapErr, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut (open)", wrapErr)
	}

	if bodyRan {
		t.Error("body must not run when repeater open fails")
	}
}
