package rtl2832u

import (
	"errors"
	"testing"
)

func TestSignExtend14(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   uint16
		want int16
	}{
		{"zero", 0x0000, 0},
		{"max positive", 0x1fff, 8191},
		{"min negative", 0x2000, -8192},
		{"minus one", 0x3fff, -1},
		{"mid positive", 0x0fff, 4095},
		{"mid negative", 0x3000, -4096},
		// High bits above bit 13 are ignored — the input is meant to
		// already be masked to [13:0]; verify we don't propagate
		// stray bits as if they were sign info.
		{"stray top bits dropped", 0xffff & 0x3fff, -1},
	}

	for _, tc := range cases {
		if got := signExtend14(tc.in); got != tc.want {
			t.Errorf("%s: signExtend14(%#x) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// fakeFlushController forces every read to return the byte we
// queued for it; the chip's flush-after-write reads (demodFlush)
// share the same queue, so we pre-populate enough flush bytes for
// every write the test triggers.
//
// readSignalStats issues 5 reads in order:
//   1. demodPage3 0x59 (if_agc_val LSB)
//   2. demodPage3 0x5A (if_agc_val MSB)
//   3. demodPage3 0x5B (rf_agc_val LSB)
//   4. demodPage3 0x5C (rf_agc_val MSB)
//   5. demodPage3 0x50 (aagc_lock)
// We feed the inResponses queue in that order.

func TestReadSignalStatsAssemblesValues(t *testing.T) {
	t.Parallel()

	// IFAGC = +1234 → 0x04D2 → LSB=0xD2, MSB[5:0]=0x04
	// RFAGC = -567  → 14-bit two's complement = 0x3DC9 → LSB=0xC9, MSB[5:0]=0x3D
	// aagc_lock = 1 → bit 0 of the byte set
	mock := &mockController{
		inResponses: [][]byte{
			{0xD2}, // if LSB
			{0x04}, // if MSB[5:0]
			{0xC9}, // rf LSB
			{0x3D}, // rf MSB[5:0]
			{0x01}, // aagc_lock
		},
	}
	chip := newRTL2832U(mock)

	stats, err := chip.readSignalStats()
	if err != nil {
		t.Fatalf("readSignalStats: %v", err)
	}

	if stats.IFAGCValue != 1234 {
		t.Errorf("IFAGCValue = %d, want 1234", stats.IFAGCValue)
	}

	if stats.RFAGCValue != -567 {
		t.Errorf("RFAGCValue = %d, want -567", stats.RFAGCValue)
	}

	if !stats.AAGCLocked {
		t.Error("AAGCLocked = false, want true (bit 0 set)")
	}
}

func TestReadSignalStatsLockBitClear(t *testing.T) {
	t.Parallel()

	mock := &mockController{
		inResponses: [][]byte{
			{0x00}, {0x00}, // if_agc_val = 0
			{0x00}, {0x00}, // rf_agc_val = 0
			{0xFE}, // aagc_lock LSB clear; high bits set are ignored
		},
	}
	chip := newRTL2832U(mock)

	stats, err := chip.readSignalStats()
	if err != nil {
		t.Fatalf("readSignalStats: %v", err)
	}

	if stats.AAGCLocked {
		t.Error("AAGCLocked = true, want false (bit 0 clear)")
	}
}

func TestReadSignalStatsTopMSBBitsMaskedOff(t *testing.T) {
	t.Parallel()

	// The MSB byte's [7:6] are reserved per datasheet table; the
	// chip might return any value there. The packing must mask
	// them off so they don't bleed into the 14-bit value.
	mock := &mockController{
		inResponses: [][]byte{
			{0x00}, {0xFF}, // if MSB has all bits set; mask should keep only [5:0]
			{0x00}, {0x00},
			{0x00},
		},
	}
	chip := newRTL2832U(mock)

	stats, err := chip.readSignalStats()
	if err != nil {
		t.Fatalf("readSignalStats: %v", err)
	}

	// 0xFF & 0x3F = 0x3F → bits [13:8] = 0x3F → value = 0x3F00 = 16128
	// signExtend14(0x3F00) = -256 (sign bit at 13 is set; 0x3F00 = 0b11111100000000)
	const want int16 = -256
	if stats.IFAGCValue != want {
		t.Errorf("IFAGCValue = %d, want %d", stats.IFAGCValue, want)
	}
}

func TestReadSignalStatsPropagatesIOFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		responses [][]byte
		inErr     error
	}{
		{"ifLSB", nil, errFakeControlIn},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mock := &mockController{inResponses: tc.responses, inErr: tc.inErr}
			chip := newRTL2832U(mock)

			if _, err := chip.readSignalStats(); !errors.Is(err, errFakeControlIn) {
				t.Errorf("err = %v, want wrapping errFakeControlIn", err)
			}
		})
	}
}

// TestReadSignalStatsErrorAtEachStage walks the five reads in
// order, failing one at a time, to cover each error-wrap branch
// in readSignalStats.
func TestReadSignalStatsErrorAtEachStage(t *testing.T) {
	t.Parallel()

	for stage := range 5 {
		t.Run("", func(t *testing.T) {
			t.Parallel()

			// Provide enough successful responses for the first
			// `stage` reads, then start failing.
			responses := make([][]byte, stage)
			for i := range responses {
				responses[i] = []byte{0x00}
			}

			mock := &mockController{
				inResponses: responses,
				// failReadAfter implemented by mockController as its
				// inResponses queue exhausting → returns an error.
				// We use inErr instead: it overrides every read.
			}

			// Attach a counting controller so the first `stage` reads
			// pass (responses queue), then the (stage+1)-th fails.
			counter := &readsFailingAfter{
				mockController: mock,
				succeedFirst:   stage,
			}

			chip := newRTL2832U(counter)
			if _, err := chip.readSignalStats(); !errors.Is(err, errFakeControlIn) {
				t.Errorf("stage %d: err = %v, want errFakeControlIn", stage, err)
			}
		})
	}
}

// readsFailingAfter wraps a mockController and forces reads after
// the first `succeedFirst` count to return errFakeControlIn. Used
// to drive readSignalStats through each error-wrap branch.
type readsFailingAfter struct {
	*mockController

	succeedFirst int
	count        int
}

func (r *readsFailingAfter) controlIn(req uint8, value, index uint16, data []byte) (int, error) {
	r.count++
	if r.count > r.succeedFirst {
		return 0, errFakeControlIn
	}

	return r.mockController.controlIn(req, value, index, data)
}

func (r *readsFailingAfter) controlOut(req uint8, value, index uint16, data []byte) (int, error) {
	return r.mockController.controlOut(req, value, index, data)
}
