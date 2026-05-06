package rtl2832u

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// flushReadOK is the single-byte response used to satisfy every
// internal flush read that demodWrite* triggers. Long phases (FIR,
// clearDDCAndIF) issue many flushes; setting it once on the mock is
// shorter than queueing identical inResponses.
//
//nolint:gochecknoglobals // test-only immutable fixture.
var flushReadOK = []byte{0x00}

// writesOnly returns just the OUT calls from a captured sequence.
// Init phases interleave writes with demod-flush reads; assertions
// only care about the wire-level register writes.
func writesOnly(calls []capturedCall) []capturedCall {
	out := make([]capturedCall, 0, len(calls))

	for _, call := range calls {
		if call.direction == dirOut {
			out = append(out, call)
		}
	}

	return out
}

// wantWrite is a brevity helper for declaring expected register
// writes in test tables. Tests pass payload bytes directly because
// Go's variadic-slice ergonomics make that more readable than
// constructing []byte literals at every site.
func wantWrite(value, index uint16, data ...byte) capturedCall {
	return capturedCall{
		direction: dirOut,
		request:   0,
		value:     value,
		index:     index,
		data:      data,
	}
}

// blockIdx and demodIdx are call-side conveniences so test tables
// don't restate the block/page encoding boilerplate at every row.
func blockIdx(b chipBlock) uint16 { return encodeBlockIndex(b, chipDirWrite) }
func demodIdx(page uint8) uint16  { return uint16(page) | uint16(chipDirWrite) }

// expectedDefaultFIRBytes is the canonical packed form of
// defaultFIRCoefficients. Hardcoding the bytes (rather than calling
// packFIRCoefficients(defaultFIRCoefficients) at test time) keeps
// the test honest: a regression in the packer surfaces immediately,
// not via test-vs-production using the same buggy code.
//
// Derivation:
//   - top 8 (signed int8): -54 -36 -41 -40 -32 -14 14 53
//     => 0xCA 0xDC 0xD7 0xD8 0xE0 0xF2 0x0E 0x35
//   - bottom 8 packed pairs (12-bit signed, x then y -> 3 bytes):
//     (101,156)=(0x065,0x09c) => 0x06 0x50 0x9C
//     (215,273)=(0x0d7,0x111) => 0x0D 0x71 0x11
//     (327,372)=(0x147,0x174) => 0x14 0x71 0x74
//     (404,421)=(0x194,0x1a5) => 0x19 0x41 0xA5
//
//nolint:gochecknoglobals // test-time fixture; immutable.
var expectedDefaultFIRBytes = [firTotalByteCount]byte{
	0xCA, 0xDC, 0xD7, 0xD8, 0xE0, 0xF2, 0x0E, 0x35,
	0x06, 0x50, 0x9C,
	0x0D, 0x71, 0x11,
	0x14, 0x71, 0x74,
	0x19, 0x41, 0xA5,
}

func TestPackFIRCoefficientsDefault(t *testing.T) {
	t.Parallel()

	got, err := packFIRCoefficients(defaultFIRCoefficients)
	if err != nil {
		t.Fatalf("packFIRCoefficients: %v", err)
	}

	if got != expectedDefaultFIRBytes {
		t.Errorf("got %#v\nwant %#v", got, expectedDefaultFIRBytes)
	}
}

func TestPackFIRCoefficientsTopOutOfRange(t *testing.T) {
	t.Parallel()

	taps := defaultFIRCoefficients
	taps[0] = 200 // outside int8 range

	_, err := packFIRCoefficients(taps)
	if !errors.Is(err, errFIRTopOutOfRange) {
		t.Fatalf("err = %v, want wrapping errFIRTopOutOfRange", err)
	}
}

func TestPackFIRCoefficientsBottomOutOfRange(t *testing.T) {
	t.Parallel()

	taps := defaultFIRCoefficients
	taps[firTopTapCount] = 9000 // outside int12 range

	_, err := packFIRCoefficients(taps)
	if !errors.Is(err, errFIRBottomOutOfRange) {
		t.Fatalf("err = %v, want wrapping errFIRBottomOutOfRange", err)
	}
}

func TestPackFIRCoefficientsNegativeBottomTaps(t *testing.T) {
	t.Parallel()

	// Verifies the int12 sign-extension path: negative taps must
	// pack with the high nibble of byte0 carrying the sign-extended
	// twelve-bit pattern, not zero.
	var taps [firTapCount]int16

	taps[firTopTapCount+0] = -1   // 0xfff
	taps[firTopTapCount+1] = -100 // 0xf9c

	got, err := packFIRCoefficients(taps)
	if err != nil {
		t.Fatalf("packFIRCoefficients: %v", err)
	}

	// (-1, -100) -> bytes:
	//   byte0 = 0xfff >> 4 = 0xff
	//   byte1 = (0xfff & 0xf) << 4 | (0xf9c >> 8) = 0xf0 | 0x0f = 0xff
	//   byte2 = 0xf9c & 0xff = 0x9c
	if got[firTopByteCount+0] != 0xff || got[firTopByteCount+1] != 0xff || got[firTopByteCount+2] != 0x9c {
		t.Errorf("packed pair = %02x/%02x/%02x, want 0xff/0xff/0x9c",
			got[firTopByteCount+0], got[firTopByteCount+1], got[firTopByteCount+2])
	}
}

// TestMustPackFIRCoefficientsPanicsOnBadInput exercises the
// programming-error guard mustPackFIRCoefficients exposes for the
// package-init path. The success path is already exercised every
// time the package loads.
func TestMustPackFIRCoefficientsPanicsOnBadInput(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()

	taps := defaultFIRCoefficients
	taps[0] = 200 // outside int8 range — must trigger the panic guard

	mustPackFIRCoefficients(taps)
}

// initPhaseCase is the table row for TestInitPhases. Hoisted to a
// type so the table can live as a package var (keeping the test
// function's body short enough for revive's function-length).
type initPhaseCase struct {
	name string
	run  func(*rtl2832u) error
	want []capturedCall
}

// initPhaseCases enumerates every Init() phase with its expected
// register writes. Hoisting to a package var keeps TestInitPhases
// itself short; the data is immutable test fixture.
//
//nolint:gochecknoglobals // test fixture, never mutated.
var initPhaseCases = func() []initPhaseCase {
	usbIdx := blockIdx(chipBlockUSB)
	sysIdx := blockIdx(chipBlockSYS)
	pg0 := demodIdx(demodPage0)
	pg1 := demodIdx(demodPage1)

	return []initPhaseCase{
		{"initUSB", (*rtl2832u).initUSB, []capturedCall{
			wantWrite(regUSBSysCtl, usbIdx, 0x09),
			wantWrite(regUSBEPAMaxPkt, usbIdx, 0x00, 0x02),
			wantWrite(regUSBEPACtl, usbIdx, 0x10, 0x02),
		}},
		{"powerOnDemod", (*rtl2832u).powerOnDemod, []capturedCall{
			wantWrite(regSYSDemodCtl1, sysIdx, 0x22),
			wantWrite(regSYSDemodCtl, sysIdx, 0xe8),
		}},
		{"resetDemod", (*rtl2832u).resetDemod, []capturedCall{
			wantWrite(encodeDemodAddr(0x01), pg1, 0x14),
			wantWrite(encodeDemodAddr(0x01), pg1, 0x10),
		}},
		{"configureSpectrum", (*rtl2832u).configureSpectrum, []capturedCall{
			wantWrite(encodeDemodAddr(0x15), pg1, 0x00),
			wantWrite(encodeDemodAddr(0x16), pg1, 0x00, 0x00),
		}},
		{"clearDDCAndIF", (*rtl2832u).clearDDCAndIF, []capturedCall{
			wantWrite(encodeDemodAddr(0x16), pg1, 0x00),
			wantWrite(encodeDemodAddr(0x17), pg1, 0x00),
			wantWrite(encodeDemodAddr(0x18), pg1, 0x00),
			wantWrite(encodeDemodAddr(0x19), pg1, 0x00),
			wantWrite(encodeDemodAddr(0x1a), pg1, 0x00),
			wantWrite(encodeDemodAddr(0x1b), pg1, 0x00),
		}},
		{"configureSDRMode", (*rtl2832u).configureSDRMode, []capturedCall{
			wantWrite(encodeDemodAddr(0x19), pg0, 0x05),
		}},
		{"initFSMState", (*rtl2832u).initFSMState, []capturedCall{
			wantWrite(encodeDemodAddr(0x93), pg1, 0xf0),
			wantWrite(encodeDemodAddr(0x94), pg1, 0x0f),
		}},
		{"disableDemodAGC", (*rtl2832u).disableDemodAGC, []capturedCall{
			wantWrite(encodeDemodAddr(0x11), pg1, 0x00),
		}},
		{"disableRFIFAGC", (*rtl2832u).disableRFIFAGC, []capturedCall{
			wantWrite(encodeDemodAddr(0x04), pg1, 0x00),
		}},
		{"disablePIDFilter", (*rtl2832u).disablePIDFilter, []capturedCall{
			wantWrite(encodeDemodAddr(0x61), pg0, 0x60),
		}},
		{"configureADCDatapath", (*rtl2832u).configureADCDatapath, []capturedCall{
			wantWrite(encodeDemodAddr(0x06), pg0, 0x80),
		}},
		{"enableZeroIF", (*rtl2832u).enableZeroIF, []capturedCall{
			wantWrite(encodeDemodAddr(0xb1), pg1, 0x1b),
		}},
		{"disableClockOutput", (*rtl2832u).disableClockOutput, []capturedCall{
			wantWrite(encodeDemodAddr(0x0d), pg0, 0x83),
		}},
	}
}()

// TestInitPhases runs each phase in isolation against a fresh mock
// and asserts the exact ordered sequence of register writes. The
// flush reads that demodWrite* generates are filtered out by
// writesOnly; the chip-side primitives' tests already cover the
// flush path itself.
func TestInitPhases(t *testing.T) {
	t.Parallel()

	for _, phase := range initPhaseCases {
		t.Run(phase.name, func(t *testing.T) {
			t.Parallel()

			mock := &mockController{inDefault: flushReadOK}
			chip := newRTL2832U(mock)

			if err := phase.run(chip); err != nil {
				t.Fatalf("%s: %v", phase.name, err)
			}

			got := writesOnly(mock.calls)
			if !reflect.DeepEqual(got, phase.want) {
				t.Errorf("writes = %#v\nwant %#v", got, phase.want)
			}
		})
	}
}

func TestWriteDefaultFIR(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if err := chip.writeDefaultFIR(); err != nil {
		t.Fatalf("writeDefaultFIR: %v", err)
	}

	writes := writesOnly(mock.calls)
	if len(writes) != firTotalByteCount {
		t.Fatalf("got %d writes, want %d", len(writes), firTotalByteCount)
	}

	for idx, want := range expectedDefaultFIRBytes {
		got := writes[idx]

		wantValue := encodeDemodAddr(firBaseAddr + uint16(idx))
		if got.value != wantValue {
			t.Errorf("write[%d].value = %#x, want %#x", idx, got.value, wantValue)
		}

		if len(got.data) != 1 || got.data[0] != want {
			t.Errorf("write[%d].data = %#v, want [%#x]", idx, got.data, want)
		}
	}
}

func TestRTL2832UInitProducesExpectedTotalWrites(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if err := chip.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Phase write counts (USB and SYS phases write to non-demod
	// blocks; everything else is demod): 3 + 2 + 2 + 2 + 6 + 20 +
	// 1 + 2 + 1 + 1 + 1 + 1 + 1 + 1 = 44.
	const wantTotalWrites = 44

	writes := writesOnly(mock.calls)
	if len(writes) != wantTotalWrites {
		t.Errorf("got %d writes, want %d", len(writes), wantTotalWrites)
	}
}

func TestRTL2832UInitErrorPropagates(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut, inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	err := chip.Init()
	if !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestInitPhasesPropagateError exercises every init phase's error
// path by injecting a controller failure on every OUT transfer.
// Each phase wraps its own writes; calling them with a failing
// controller hits the per-phase error-propagation branch that the
// happy-path TestInitPhases doesn't reach.
func TestInitPhasesPropagateError(t *testing.T) {
	t.Parallel()

	for _, phase := range initPhaseCases {
		t.Run(phase.name, func(t *testing.T) {
			t.Parallel()

			mock := &mockController{outErr: errFakeControlOut, inDefault: flushReadOK}
			chip := newRTL2832U(mock)

			err := phase.run(chip)
			if !errors.Is(err, errFakeControlOut) {
				t.Errorf("err = %v, want wrapping errFakeControlOut", err)
			}
		})
	}
}

// TestInitUSBLaterWriteFailures covers initUSB's second and third
// writes: the first writeByte succeeds, then the next writeWord
// fails (stage 1) or the second writeWord fails (stage 2).
// Phase-level "fail every write" tests only hit the first.
func TestInitUSBLaterWriteFailures(t *testing.T) {
	t.Parallel()

	for stage := 1; stage <= 2; stage++ {
		t.Run(fmt.Sprintf("after_%d", stage), func(t *testing.T) {
			t.Parallel()

			mock := &countingController{
				mockController: &mockController{inDefault: flushReadOK},
				failAfter:      stage,
			}
			chip := newRTL2832U(mock)

			if err := chip.initUSB(); !errors.Is(err, errFakeControlOut) {
				t.Errorf("stage %d: err = %v, want wrapping errFakeControlOut", stage, err)
			}
		})
	}
}

// TestWriteDefaultFIRMidLoopFailure covers the loop-iteration
// error path inside writeDefaultFIR: the first few register
// writes succeed; the failure lands somewhere in the middle of
// the 20-byte FIR table.
func TestWriteDefaultFIRMidLoopFailure(t *testing.T) {
	t.Parallel()

	mock := &countingController{
		mockController: &mockController{inDefault: flushReadOK},
		failAfter:      5, // 5 FIR bytes go through; the 6th fails
	}
	chip := newRTL2832U(mock)

	if err := chip.writeDefaultFIR(); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestResetSampleBufferIssuesHaltRunAndSyncTrigger pins down the
// librtlsdr-equivalent stream-arm sequence: EPA_CTL halt + run on
// the USB block, followed by the page-0 demod soft-reset pulse
// that triggers sync mode and flushes the demod's sample FIFO.
func TestResetSampleBufferIssuesHaltRunAndSyncTrigger(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if err := chip.ResetSampleBuffer(); err != nil {
		t.Fatalf("ResetSampleBuffer: %v", err)
	}

	got := writesOnly(mock.calls)
	usbIdx := blockIdx(chipBlockUSB)
	pg0 := demodIdx(demodPage0)
	want := []capturedCall{
		wantWrite(regUSBEPACtl, usbIdx, 0x10, 0x02),
		wantWrite(regUSBEPACtl, usbIdx, 0x00, 0x00),
		wantWrite(encodeDemodAddr(regDemodSoftReset), pg0, softResetAsserted),
		wantWrite(encodeDemodAddr(regDemodSoftReset), pg0, softResetReleased),
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("writes =\n%#v\nwant\n%#v", got, want)
	}
}

// TestResetSampleBufferHaltFailureSurfaces covers the first-write
// error branch.
func TestResetSampleBufferHaltFailureSurfaces(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut}
	chip := newRTL2832U(mock)

	if err := chip.ResetSampleBuffer(); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestResetSampleBufferLaterFailures pins each non-first write of
// ResetSampleBuffer's stream-arm sequence: run, sync-assert, sync-
// release. failAfter=N lets the first N writes through, then the
// (N+1)th fails — covering each subsequent-write error branch.
func TestResetSampleBufferLaterFailures(t *testing.T) {
	t.Parallel()

	for stage := 1; stage <= 3; stage++ {
		t.Run(fmt.Sprintf("after_%d", stage), func(t *testing.T) {
			t.Parallel()

			mock := &countingController{
				mockController: &mockController{inDefault: flushReadOK},
				failAfter:      stage,
			}
			chip := newRTL2832U(mock)

			if err := chip.ResetSampleBuffer(); !errors.Is(err, errFakeControlOut) {
				t.Errorf("stage %d: err = %v, want wrapping errFakeControlOut", stage, err)
			}
		})
	}
}
