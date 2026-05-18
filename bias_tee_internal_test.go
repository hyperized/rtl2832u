package rtl2832u

import (
	"errors"
	"testing"
)

// biasTeeMockController gives the bias-tee tests a deterministic
// view of the SYS-block GPIO registers. The chip-side
// configureGPIOOutput / writeGPIOBit primitives perform six
// register touches (read+write × three regs); the mock returns a
// queued initial value for each read and captures the writes.
type biasTeeMockController struct {
	*mockController

	// readQueue is the FIFO of values returned by successive
	// readByte calls. Tests pre-load it with the expected
	// register-current values.
	readQueue [][]byte
	readIdx   int
}

func (c *biasTeeMockController) controlIn(req uint8, value, index uint16, data []byte) (int, error) {
	if c.readIdx >= len(c.readQueue) {
		return c.mockController.controlIn(req, value, index, data)
	}

	resp := c.readQueue[c.readIdx]
	c.readIdx++
	count := copy(data, resp)

	c.calls = append(c.calls, capturedCall{
		direction: dirIn,
		request:   req,
		value:     value,
		index:     index,
		data:      append([]byte(nil), data...),
	})

	return count, nil
}

func (c *biasTeeMockController) controlOut(req uint8, value, index uint16, data []byte) (int, error) {
	return c.mockController.controlOut(req, value, index, data)
}

func TestSetBiasTeeEnableSequence(t *testing.T) {
	t.Parallel()

	// Pre-load reads: GPD=0xFF (all-input), GPOE=0x00 (all-disabled),
	// GPO=0x00 (all-low). The chip resets to roughly this.
	mock := &biasTeeMockController{
		mockController: &mockController{},
		readQueue: [][]byte{
			{0xff}, // first GPD read (configure direction)
			{0x00}, // GPOE read (enable output driver)
			{0x00}, // GPO read (drive bit)
		},
	}
	chip := newRTL2832U(mock)

	if err := chip.setBiasTee(0, true); err != nil {
		t.Fatalf("setBiasTee: %v", err)
	}

	// Six calls: read+write × three SYS regs in order GPD, GPOE, GPO.
	// Mismatching addresses here would catch a regression of the
	// 0x0001/0x0002/0x0003-vs-0x3001/0x3003/0x3004 bug.
	if got := len(mock.calls); got != 6 {
		t.Fatalf("call count = %d, want 6", got)
	}

	wantSequence := []struct {
		dir     string
		addr    uint16
		comment string
	}{
		{dirIn, regSYSGPD, "GPD read"},
		{dirOut, regSYSGPD, "GPD write"},
		{dirIn, regSYSGPOE, "GPOE read"},
		{dirOut, regSYSGPOE, "GPOE write"},
		{dirIn, regSYSGPO, "GPO read"},
		{dirOut, regSYSGPO, "GPO write"},
	}

	for i, want := range wantSequence {
		got := mock.calls[i]

		if got.direction != want.dir || got.value != want.addr {
			t.Errorf("calls[%d] = (%q, %#x), want (%q, %#x) — %s",
				i, got.direction, got.value, want.dir, want.addr, want.comment)
		}
	}

	if got := mock.calls[5].data[0] & 0x01; got != 0x01 {
		t.Errorf("GPO write payload bit 0 = %#x, want 0x01 (drive high)", got)
	}
}

func TestSetBiasTeeDisableClearsBit(t *testing.T) {
	t.Parallel()

	// Pre-load reads: chip already in bias-tee-on state
	// (GPD bit 0 cleared, GPOE bit 0 set, GPO bit 0 set).
	mock := &biasTeeMockController{
		mockController: &mockController{},
		readQueue: [][]byte{
			{0xfe}, // GPD: bit 0 clear (already output)
			{0x01}, // GPOE: bit 0 set (already enabled)
			{0x01}, // GPO: bit 0 set (currently driving high)
		},
	}
	chip := newRTL2832U(mock)

	if err := chip.setBiasTee(0, false); err != nil {
		t.Fatalf("setBiasTee: %v", err)
	}

	gpoWrite := mock.calls[5]
	if got := gpoWrite.data[0] & 0x01; got != 0 {
		t.Errorf("GPO write payload bit 0 = %#x, want 0 (drive low)", got)
	}
}

func TestSetBiasTeeRejectsOutOfRangeGPIO(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{mockController: &mockController{}}
	chip := newRTL2832U(mock)

	if err := chip.setBiasTee(8, true); !errors.Is(err, ErrInvalidGPIO) {
		t.Errorf("err = %v, want wrapping ErrInvalidGPIO", err)
	}
}

func TestSetBiasTeePreservesOtherGPIOs(t *testing.T) {
	t.Parallel()

	// Other GPIOs (1..7) are configured; only GPIO0 should change.
	mock := &biasTeeMockController{
		mockController: &mockController{},
		readQueue: [][]byte{
			{0xff}, // GPD all-1: every pin is input
			{0xa0}, // GPOE: bits 5,7 set (some other GPIOs already enabled)
			{0xa0}, // GPO: bits 5,7 currently driving
		},
	}
	chip := newRTL2832U(mock)

	if err := chip.setBiasTee(0, true); err != nil {
		t.Fatalf("setBiasTee: %v", err)
	}

	// GPD write: should clear only bit 0, keep others (was 0xff → 0xfe).
	if got := mock.calls[1].data[0]; got != 0xfe {
		t.Errorf("GPD write = %#x, want 0xfe", got)
	}

	// GPOE write: should set bit 0, preserve bits 5,7 (was 0xa0 → 0xa1).
	if got := mock.calls[3].data[0]; got != 0xa1 {
		t.Errorf("GPOE write = %#x, want 0xa1", got)
	}

	// GPO write: should set bit 0, preserve bits 5,7 (was 0xa0 → 0xa1).
	if got := mock.calls[5].data[0]; got != 0xa1 {
		t.Errorf("GPO write = %#x, want 0xa1", got)
	}
}

func TestSetBiasTeeWrapsReadFailure(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{
		mockController: &mockController{inErr: errFakeControlIn},
	}
	chip := newRTL2832U(mock)

	if err := chip.setBiasTee(0, true); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}

func TestSetBiasTeeWrapsWriteFailure(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{
		mockController: &mockController{outErr: errFakeControlOut},
		readQueue:      [][]byte{{0xff}},
	}
	chip := newRTL2832U(mock)

	if err := chip.setBiasTee(0, true); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestSetBiasTeeWrapsGPOEWriteFailure exercises the
// configureGPIOOutput second-write branch: GPD read+write succeed,
// GPOE read returns OK, GPOE write fails. Without this case the
// "set output enable" error wrap stays uncovered.
func TestSetBiasTeeWrapsGPOEWriteFailure(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{
		mockController: &mockController{},
		readQueue: [][]byte{
			{0xff}, // GPD read
			{0x00}, // GPOE read
		},
	}
	// Wrap a counter that lets the GPD write succeed but fails the
	// GPOE write.
	counter := &countingController{
		mockController: mock.mockController,
		failAfter:      1, // one OUT goes through (GPD); the next (GPOE) fails
	}
	mock.mockController = counter.mockController
	chip := &rtl2832u{ctrl: &biasTeeFailGPOE{biasTeeMockController: mock, counter: counter}}

	if err := chip.setBiasTee(0, true); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut on GPOE write", err)
	}
}

// biasTeeFailGPOE is a thin shim that routes controlIn through the
// biasTeeMockController's read-queue logic and controlOut through
// the failAfter-equipped countingController.
type biasTeeFailGPOE struct {
	*biasTeeMockController

	counter *countingController
}

func (b *biasTeeFailGPOE) controlOut(req uint8, value, index uint16, data []byte) (int, error) {
	return b.counter.controlOut(req, value, index, data)
}

// TestSetBiasTeeWrapsGPOWriteFailure exercises the writeGPIOBit
// error branch: GPD + GPOE configure succeed, then GPO read OK but
// GPO write fails.
func TestSetBiasTeeWrapsGPOWriteFailure(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{
		mockController: &mockController{},
		readQueue: [][]byte{
			{0xff}, // GPD read
			{0x00}, // GPOE read
			{0x00}, // GPO read
		},
	}
	counter := &countingController{
		mockController: mock.mockController,
		failAfter:      2, // GPD+GPOE writes succeed, GPO write fails
	}
	chip := &rtl2832u{ctrl: &biasTeeFailGPOE{biasTeeMockController: mock, counter: counter}}

	if err := chip.setBiasTee(0, true); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut on GPO write", err)
	}
}

// TestGetBiasTeeReadsLiveBit verifies the chip-side getBiasTee
// reads regSYSGPO (the output latch) and reports the bit for the
// requested GPIO. The bias-tee pin is configured as output, so
// GPO is the documented read-back surface; reading GPI on an
// output-mode pin is undefined per datasheet §10.2.2.
func TestGetBiasTeeReadsLiveBit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		gpoValue byte
		gpio     uint8
		want     bool
	}{
		{name: "bit 0 low", gpoValue: 0x00, gpio: 0, want: false},
		{name: "bit 0 high", gpoValue: 0x01, gpio: 0, want: true},
		{name: "bit 0 high, others set too", gpoValue: 0xa1, gpio: 0, want: true},
		{name: "bit 0 low, others set", gpoValue: 0xa0, gpio: 0, want: false},
		{name: "bit 3 high", gpoValue: 0x08, gpio: 3, want: true},
		{name: "bit 7 high", gpoValue: 0x80, gpio: 7, want: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mock := &biasTeeMockController{
				mockController: &mockController{},
				readQueue:      [][]byte{{testCase.gpoValue}},
			}
			chip := newRTL2832U(mock)

			got, err := chip.getBiasTee(testCase.gpio)
			if err != nil {
				t.Fatalf("getBiasTee: %v", err)
			}

			if got != testCase.want {
				t.Errorf("getBiasTee enabled = %v, want %v", got, testCase.want)
			}

			if len(mock.calls) != 1 {
				t.Fatalf("calls = %d, want 1 (single GPO read)", len(mock.calls))
			}

			if call := mock.calls[0]; call.direction != dirIn || call.value != regSYSGPO {
				t.Errorf("calls[0] = (%q, %#x), want (%q, %#x) — GPO read",
					call.direction, call.value, dirIn, regSYSGPO)
			}
		})
	}
}

func TestGetBiasTeeRejectsOutOfRangeGPIO(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{mockController: &mockController{}}
	chip := newRTL2832U(mock)

	if _, err := chip.getBiasTee(8); !errors.Is(err, ErrInvalidGPIO) {
		t.Errorf("err = %v, want wrapping ErrInvalidGPIO", err)
	}
}

func TestGetBiasTeeWrapsReadFailure(t *testing.T) {
	t.Parallel()

	mock := &biasTeeMockController{
		mockController: &mockController{inErr: errFakeControlIn},
	}
	chip := newRTL2832U(mock)

	if _, err := chip.getBiasTee(0); !errors.Is(err, errFakeControlIn) {
		t.Errorf("err = %v, want wrapping errFakeControlIn", err)
	}
}
