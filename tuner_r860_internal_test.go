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
	//   detect:    enable, write [chipIDReg], read 1 byte, disable
	//   init:      enable, N chunked writes, disable
	// 27 init bytes / (maxR860I2CMsgLen-1 = 7) = 4 chunks.
	const initChunkCount = (r860InitWriteCount + (maxR860I2CMsgLen - 2)) / (maxR860I2CMsgLen - 1)

	// 4 detect ops + (1 enable + N chunks + 1 disable) for init.
	wantOps := 4 + 2 + initChunkCount
	if got := len(bus.ops); got != wantOps {
		t.Fatalf("ops count = %d, want %d (detect 4 + init %d-chunked)", got, wantOps, initChunkCount)
	}

	wantKinds := make([]string, 0, 5+initChunkCount+1)
	wantKinds = append(wantKinds, opEnable, opWrite, opRead, opDisable, opEnable)

	for range initChunkCount {
		wantKinds = append(wantKinds, opWrite)
	}

	wantKinds = append(wantKinds, opDisable)

	for i, kind := range wantKinds {
		if bus.ops[i].kind != kind {
			t.Errorf("ops[%d].kind = %q, want %q", i, bus.ops[i].kind, kind)
		}
	}

	// Detect's read-pointer write must be a single-byte transaction
	// addressing the chip-ID register.
	if got := bus.ops[1].data; !reflect.DeepEqual(got, []byte{r860ChipIDReg}) {
		t.Errorf("detect read-pointer payload = %#v, want [%#x]", got, r860ChipIDReg)
	}

	// Reassemble the chunked init writes and compare against the
	// original seed table — order, contents, and the auto-incremented
	// register pointer per chunk must all line up.
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
	// programmed; this is the source of truth for later
	// read-modify-write operations.
	for offset, want := range r860InitValues {
		addr := int(r860InitBaseReg) + offset
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
