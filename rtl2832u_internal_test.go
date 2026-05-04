package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

// errFakeControlIn and errFakeControlOut let tests assert that errors
// from the controller layer are wrapped (not replaced) by the chip
// driver. err113 forbids ad-hoc errors.New inside test bodies.
var (
	errFakeControlIn  = errors.New("fake controller IN failure")
	errFakeControlOut = errors.New("fake controller OUT failure")
)

// dirIn / dirOut name the two valid values of capturedCall.direction.
// Hoisting them avoids goconst hits and lets the assertions read more
// like English ("calls[0].direction was an out").
const (
	dirIn  = "in"
	dirOut = "out"
)

// capturedCall is the wire-level record of one controller invocation.
// Tests assert against the captured slice so the chip driver's
// encoding can be verified end-to-end without real USB hardware.
type capturedCall struct {
	direction string // dirIn or dirOut
	request   uint8
	value     uint16
	index     uint16
	data      []byte
}

// mockController fakes the USB control transport. inResponses is
// consumed FIFO for controlIn calls; once it drains, inDefault is
// used for every subsequent IN — useful for the long demod-flush
// chains that init phases generate. Per-direction errors let
// individual tests exercise the error-wrapping paths in rtl2832u.
type mockController struct {
	calls       []capturedCall
	inResponses [][]byte
	inDefault   []byte
	inErr       error
	outErr      error
}

func (m *mockController) controlIn(req uint8, value, index uint16, data []byte) (int, error) {
	if m.inErr != nil {
		return 0, m.inErr
	}

	var resp []byte

	switch {
	case len(m.inResponses) > 0:
		resp = m.inResponses[0]
		m.inResponses = m.inResponses[1:]
	default:
		resp = m.inDefault
	}

	count := copy(data, resp)

	m.calls = append(m.calls, capturedCall{
		direction: dirIn,
		request:   req,
		value:     value,
		index:     index,
		data:      append([]byte(nil), data...),
	})

	return count, nil
}

func (m *mockController) controlOut(req uint8, value, index uint16, data []byte) (int, error) {
	if m.outErr != nil {
		return 0, m.outErr
	}

	m.calls = append(m.calls, capturedCall{
		direction: dirOut,
		request:   req,
		value:     value,
		index:     index,
		data:      append([]byte(nil), data...),
	})

	return len(data), nil
}

func TestEncodeBlockIndex(t *testing.T) {
	t.Parallel()

	got := encodeBlockIndex(chipBlockUSB, chipDirWrite)

	const want uint16 = 0x0110 // block 1 in high byte, write flag 0x10 in low
	if got != want {
		t.Errorf("encodeBlockIndex(USB, write) = %#x, want %#x", got, want)
	}
}

func TestEncodeDemodAddr(t *testing.T) {
	t.Parallel()

	got := encodeDemodAddr(0x14)

	const want uint16 = 0x1420 // addr<<8 | demodAddrFlag
	if got != want {
		t.Errorf("encodeDemodAddr(0x14) = %#x, want %#x", got, want)
	}
}

func TestRTL2832UWriteByte(t *testing.T) {
	t.Parallel()

	mock := &mockController{}
	chip := newRTL2832U(mock)

	if err := chip.writeByte(chipBlockUSB, regUSBSysCtl, 0x09); err != nil {
		t.Fatalf("writeByte: %v", err)
	}

	want := []capturedCall{{
		direction: dirOut,
		request:   0,
		value:     regUSBSysCtl,
		index:     uint16(chipBlockUSB)<<8 | uint16(chipDirWrite),
		data:      []byte{0x09},
	}}

	if !reflect.DeepEqual(mock.calls, want) {
		t.Errorf("calls = %+v, want %+v", mock.calls, want)
	}
}

func TestRTL2832UWriteWordBigEndian(t *testing.T) {
	t.Parallel()

	// Use chipBlockSYS rather than chipBlockUSB so the writeWord
	// primitive has callers across more than one block (the init
	// flow uses chipBlockUSB; this test plus the demod tests cover
	// the others), which keeps the unparam linter satisfied.
	mock := &mockController{}
	chip := newRTL2832U(mock)

	if err := chip.writeWord(chipBlockSYS, 0x3010, 0x1002); err != nil {
		t.Fatalf("writeWord: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(mock.calls))
	}

	got := mock.calls[0].data

	want := []byte{0x10, 0x02} // 0x1002 in big-endian
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %#v, want %#v (big-endian)", got, want)
	}
}

func TestRTL2832UReadByteReturnsResponse(t *testing.T) {
	t.Parallel()

	mock := &mockController{inResponses: [][]byte{{0xAB}}}
	chip := newRTL2832U(mock)

	got, err := chip.readByte(chipBlockUSB, regUSBStat)
	if err != nil {
		t.Fatalf("readByte: %v", err)
	}

	if got != 0xAB {
		t.Errorf("got %#x, want 0xAB", got)
	}
}

func TestRTL2832UReadByteShortReadFails(t *testing.T) {
	t.Parallel()

	// Empty inResponses queue makes the mock return n=0; the chip
	// driver must surface that as an error rather than returning a
	// zero-valued byte that the caller might mistake for real data.
	mock := &mockController{}
	chip := newRTL2832U(mock)

	if _, err := chip.readByte(chipBlockUSB, regUSBStat); err == nil {
		t.Fatal("expected short-read error, got nil")
	}
}

func TestRTL2832UReadWordBigEndian(t *testing.T) {
	t.Parallel()

	mock := &mockController{inResponses: [][]byte{{0x12, 0x34}}}
	chip := newRTL2832U(mock)

	got, err := chip.readWord(chipBlockUSB, regUSBEPACtl)
	if err != nil {
		t.Fatalf("readWord: %v", err)
	}

	if got != 0x1234 {
		t.Errorf("got %#x, want 0x1234", got)
	}
}

func TestRTL2832UReadWordShortReadFails(t *testing.T) {
	t.Parallel()

	// One-byte response is shorter than the two bytes readWord
	// expects; the chip driver must reject it rather than returning
	// a partially-decoded uint16.
	mock := &mockController{inResponses: [][]byte{{0x12}}}
	chip := newRTL2832U(mock)

	if _, err := chip.readWord(chipBlockUSB, regUSBEPACtl); err == nil {
		t.Fatal("expected short-read error, got nil")
	}
}

func TestRTL2832UDemodWriteByteFlushes(t *testing.T) {
	t.Parallel()

	// inResponses serves the flush-read that demodWriteByte issues
	// internally; without it the flush would short-read and the
	// write would surface as an error.
	mock := &mockController{inResponses: [][]byte{{0x00}}}
	chip := newRTL2832U(mock)

	if err := chip.demodWriteByte(1, 0x14, 0x01); err != nil {
		t.Fatalf("demodWriteByte: %v", err)
	}

	if len(mock.calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2 (write + flush)", len(mock.calls))
	}

	write := mock.calls[0]
	if write.direction != dirOut {
		t.Errorf("calls[0].direction = %q, want %q", write.direction, dirOut)
	}

	if write.value != encodeDemodAddr(0x14) {
		t.Errorf("write.value = %#x, want %#x", write.value, encodeDemodAddr(0x14))
	}

	wantWriteIndex := uint16(1) | uint16(chipDirWrite)
	if write.index != wantWriteIndex {
		t.Errorf("write.index = %#x, want %#x (page=1 | write flag)", write.index, wantWriteIndex)
	}

	flush := mock.calls[1]
	if flush.direction != dirIn {
		t.Errorf("calls[1].direction = %q, want %q", flush.direction, dirIn)
	}

	if flush.value != encodeDemodAddr(demodFlushAddr) {
		t.Errorf("flush.value = %#x, want %#x", flush.value, encodeDemodAddr(demodFlushAddr))
	}

	if flush.index != uint16(demodFlushPage) {
		t.Errorf("flush.index = %#x, want %#x", flush.index, demodFlushPage)
	}
}

func TestRTL2832UDemodWriteWordBigEndianAndFlushes(t *testing.T) {
	t.Parallel()

	mock := &mockController{inResponses: [][]byte{{0x00}}}
	chip := newRTL2832U(mock)

	if err := chip.demodWriteWord(0, 0x16, 0xABCD); err != nil {
		t.Fatalf("demodWriteWord: %v", err)
	}

	if len(mock.calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2 (write + flush)", len(mock.calls))
	}

	got := mock.calls[0].data

	want := []byte{0xAB, 0xCD}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %#v, want %#v (big-endian)", got, want)
	}
}

func TestRTL2832UDemodReadByteReturnsResponse(t *testing.T) {
	t.Parallel()

	mock := &mockController{inResponses: [][]byte{{0x42}}}
	chip := newRTL2832U(mock)

	got, err := chip.demodReadByte(2, 0x10)
	if err != nil {
		t.Fatalf("demodReadByte: %v", err)
	}

	if got != 0x42 {
		t.Errorf("got %#x, want 0x42", got)
	}
}

func TestRTL2832UDemodReadByteShortReadFails(t *testing.T) {
	t.Parallel()

	mock := &mockController{}
	chip := newRTL2832U(mock)

	if _, err := chip.demodReadByte(2, 0x10); err == nil {
		t.Fatal("expected short-read error, got nil")
	}
}

func TestRTL2832UWriteByteWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	err := chip.writeByte(chipBlockUSB, regUSBSysCtl, 0x09)
	if !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestRTL2832UReadByteWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{inErr: errFakeControlIn}
	chip := newRTL2832U(mock)

	_, err := chip.readByte(chipBlockUSB, regUSBStat)
	if !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

func TestRTL2832UWriteWordWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	if err := chip.writeWord(chipBlockUSB, regUSBEPACtl, 0x1002); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestRTL2832UReadWordWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{inErr: errFakeControlIn}
	chip := newRTL2832U(mock)

	if _, err := chip.readWord(chipBlockUSB, regUSBEPACtl); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

func TestRTL2832UDemodWriteByteWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	if err := chip.demodWriteByte(1, 0x14, 0x01); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestRTL2832UDemodWriteWordWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	if err := chip.demodWriteWord(0, 0x16, 0xABCD); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

func TestRTL2832UDemodReadByteWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{inErr: errFakeControlIn}
	chip := newRTL2832U(mock)

	if _, err := chip.demodReadByte(2, 0x10); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

func TestRTL2832UDemodWriteFlushFailurePropagates(t *testing.T) {
	t.Parallel()

	// Empty inResponses queue causes demodFlush's read to short-read,
	// which must propagate as an error from demodWriteByte rather
	// than being silently swallowed.
	mock := &mockController{}
	chip := newRTL2832U(mock)

	if err := chip.demodWriteByte(1, 0x14, 0x01); err == nil {
		t.Fatal("expected flush failure to propagate, got nil")
	}
}
