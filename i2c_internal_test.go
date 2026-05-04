package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

// fakeI2CDeviceAddr is the wValue used in i2c tests. It matches
// the R820T/R860 family's wire-form address (0x34 — the 7-bit
// slave 0x1a shifted up by one to leave room for the R/W bit) so
// the test expectations line up with what the tuner driver
// produces.
const fakeI2CDeviceAddr uint8 = 0x34

func TestEnableI2CRepeater(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if err := chip.enableI2CRepeater(); err != nil {
		t.Fatalf("enableI2CRepeater: %v", err)
	}

	got := writesOnly(mock.calls)
	want := []capturedCall{
		wantWrite(encodeDemodAddr(regDemodSoftReset), demodIdx(demodPage1), i2cRepeaterEnabled),
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %#v\nwant %#v", got, want)
	}
}

func TestDisableI2CRepeater(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if err := chip.disableI2CRepeater(); err != nil {
		t.Fatalf("disableI2CRepeater: %v", err)
	}

	got := writesOnly(mock.calls)
	want := []capturedCall{
		wantWrite(encodeDemodAddr(regDemodSoftReset), demodIdx(demodPage1), i2cRepeaterDisabled),
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %#v\nwant %#v", got, want)
	}
}

func TestI2CWriteEncoding(t *testing.T) {
	t.Parallel()

	mock := &mockController{}
	chip := newRTL2832U(mock)

	payload := []byte{0x05, 0x42, 0x99}
	if err := chip.i2cWrite(fakeI2CDeviceAddr, payload); err != nil {
		t.Fatalf("i2cWrite: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(mock.calls))
	}

	got := mock.calls[0]
	wantIndex := encodeBlockIndex(chipBlockI2C, chipDirWrite)

	if got.direction != dirOut {
		t.Errorf("direction = %q, want %q", got.direction, dirOut)
	}

	if got.value != uint16(fakeI2CDeviceAddr) {
		t.Errorf("value = %#x, want %#x", got.value, fakeI2CDeviceAddr)
	}

	if got.index != wantIndex {
		t.Errorf("index = %#x, want %#x", got.index, wantIndex)
	}

	if !reflect.DeepEqual(got.data, payload) {
		t.Errorf("payload = %#v, want %#v", got.data, payload)
	}
}

func TestI2CReadEncoding(t *testing.T) {
	t.Parallel()

	mock := &mockController{inResponses: [][]byte{{0xAB, 0xCD}}}
	chip := newRTL2832U(mock)

	dst := make([]byte, 2)
	if err := chip.i2cRead(fakeI2CDeviceAddr, dst); err != nil {
		t.Fatalf("i2cRead: %v", err)
	}

	if !reflect.DeepEqual(dst, []byte{0xAB, 0xCD}) {
		t.Errorf("dst = %#v, want [0xAB 0xCD]", dst)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(mock.calls))
	}

	got := mock.calls[0]
	wantIndex := encodeBlockIndex(chipBlockI2C, chipDirRead)

	if got.direction != dirIn {
		t.Errorf("direction = %q, want %q", got.direction, dirIn)
	}

	if got.value != uint16(fakeI2CDeviceAddr) {
		t.Errorf("value = %#x, want %#x", got.value, fakeI2CDeviceAddr)
	}

	if got.index != wantIndex {
		t.Errorf("index = %#x, want %#x", got.index, wantIndex)
	}
}

func TestI2CReadShortReadFails(t *testing.T) {
	t.Parallel()

	// Mock returns one byte; we ask for two.
	mock := &mockController{inResponses: [][]byte{{0xAB}}}
	chip := newRTL2832U(mock)

	err := chip.i2cRead(fakeI2CDeviceAddr, make([]byte, 2))
	if !errors.Is(err, errShortRead) {
		t.Errorf("err = %v, want wrapping errShortRead", err)
	}
}

func TestI2CWriteRegister(t *testing.T) {
	t.Parallel()

	mock := &mockController{}
	chip := newRTL2832U(mock)

	if err := chip.i2cWriteRegister(fakeI2CDeviceAddr, 0x05, 0x42); err != nil {
		t.Fatalf("i2cWriteRegister: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1 (single i2c transaction)", len(mock.calls))
	}

	want := []byte{0x05, 0x42}
	if !reflect.DeepEqual(mock.calls[0].data, want) {
		t.Errorf("payload = %#v, want %#v (reg, val)", mock.calls[0].data, want)
	}
}

func TestI2CReadRegisterTwoTransactions(t *testing.T) {
	t.Parallel()

	// First transaction (write the reg pointer) returns nothing on
	// the read side — the byte response is consumed by the second
	// (the read transaction itself).
	mock := &mockController{inResponses: [][]byte{{0x42}}}
	chip := newRTL2832U(mock)

	got, err := chip.i2cReadRegister(fakeI2CDeviceAddr, 0x05)
	if err != nil {
		t.Fatalf("i2cReadRegister: %v", err)
	}

	if got != 0x42 {
		t.Errorf("got = %#x, want 0x42", got)
	}

	// Two transactions: one write (the reg pointer), one read.
	if len(mock.calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2 (write reg pointer + read value)", len(mock.calls))
	}

	first := mock.calls[0]
	if first.direction != dirOut {
		t.Errorf("calls[0].direction = %q, want %q", first.direction, dirOut)
	}

	if !reflect.DeepEqual(first.data, []byte{0x05}) {
		t.Errorf("calls[0].data = %#v, want [0x05]", first.data)
	}

	second := mock.calls[1]
	if second.direction != dirIn {
		t.Errorf("calls[1].direction = %q, want %q", second.direction, dirIn)
	}
}

func TestI2CWriteWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	err := chip.i2cWrite(fakeI2CDeviceAddr, []byte{0x05})
	if !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestI2CReadWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{inErr: errFakeControlIn}
	chip := newRTL2832U(mock)

	err := chip.i2cRead(fakeI2CDeviceAddr, make([]byte, 1))
	if !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

func TestI2CRepeaterWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut, inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if err := chip.enableI2CRepeater(); !errors.Is(err, errFakeControlOut) {
		t.Errorf("enable err = %v, want wrapping errFakeControlOut", err)
	}

	if err := chip.disableI2CRepeater(); !errors.Is(err, errFakeControlOut) {
		t.Errorf("disable err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestI2CReadRegisterWriteFailureSurfaces hits the read-pointer
// write error path inside i2cReadRegister.
func TestI2CReadRegisterWriteFailureSurfaces(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	if _, err := chip.i2cReadRegister(fakeI2CDeviceAddr, 0x05); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestI2CReadRegisterReadFailureSurfaces hits the read-byte error
// path: the write of the register pointer succeeds, the read
// fails. Tests that the error path threads through both
// transactions correctly.
func TestI2CReadRegisterReadFailureSurfaces(t *testing.T) {
	t.Parallel()

	mock := &mockController{inErr: errFakeControlIn}
	chip := newRTL2832U(mock)

	if _, err := chip.i2cReadRegister(fakeI2CDeviceAddr, 0x05); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}
