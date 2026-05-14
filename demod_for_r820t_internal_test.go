package rtl2832u

import (
	"reflect"
	"testing"
)

// TestConfigureForR820TWriteSequence pins down the exact set of
// demod register writes configureForR820T performs. The contract
// is a verbatim mirror of librtlsdr's R820T-branch in
// rtlsdr_open / rtlsdr_set_center_freq:
//
//   - Disable Zero-IF mode (bbn_in cleared, IQ comp/est preserved).
//   - Switch the ADC routing to I-only (0x4d).
//   - Program the demod's DDC IF frequency to -3.57 MHz, encoded
//     across 0x19/0x1a/0x1b page 1.
//   - Enable spectrum inversion (0x15 page 1 = 0x01).
//
// A regression in any of these — particularly the en_bbin clear or
// the spectrum-inversion enable — silently kills the Q channel at
// the host's IQ stream. The test enumerates the wire-level writes
// rather than checking shadow state because the chip has no shadow
// for demod registers.
func TestConfigureForR820TWriteSequence(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := &rtl2832u{ctrl: mock}

	const xtalHz uint32 = 28_800_000

	if err := chip.configureForR820T(xtalHz); err != nil {
		t.Fatalf("configureForR820T: %v", err)
	}

	// At 28.8 MHz xtal and 3.57 MHz IF the signed encoded value
	// is -519918 (= int64(-3570000 << 22) / 28800000). 519918 in
	// binary is 0x7EEEE; two's-complement for 32 bits is
	// 0xFFF81112, masked to 22 bits = 0x381112.
	const (
		wantIFHi  byte = 0x38
		wantIFMid byte = 0x11
		wantIFLo  byte = 0x12
	)

	wantWrites := []capturedCall{
		// Disable Zero-IF (regDemodZeroIF, page 1, value 0x1a).
		wantWrite(encodeDemodAddr(regDemodZeroIF), demodIdx(demodPage1), zeroIFDisabled),

		// I-only ADC input (regDemodADCInput, page 0, value 0x4d).
		wantWrite(encodeDemodAddr(regDemodADCInput), demodIdx(demodPage0), adcInputIOnly),

		// IF freq high byte.
		wantWrite(encodeDemodAddr(regDemodIFFreqHi), demodIdx(demodPage1), wantIFHi),

		// IF freq mid byte.
		wantWrite(encodeDemodAddr(regDemodIFFreqMid), demodIdx(demodPage1), wantIFMid),

		// IF freq low byte.
		wantWrite(encodeDemodAddr(regDemodIFFreqLo), demodIdx(demodPage1), wantIFLo),

		// Spectrum inversion ON.
		wantWrite(encodeDemodAddr(regDemodSpectrumInv), demodIdx(demodPage1), spectrumInvOn),
	}

	if got := writesOnly(mock.calls); !reflect.DeepEqual(got, wantWrites) {
		t.Errorf("write sequence mismatch:\n got %#v\nwant %#v", got, wantWrites)
	}
}

// TestWriteDemodIFFreqRoundsToTwosComplement covers the
// fixed-point encoding's edge: a positive freqHz must produce a
// *negative* (two's-complement) digital IF, since the DDC mixes
// by negating its programmed value. Regression here would put the
// signal on the wrong sideband — easy to confuse with a working
// chip when the input happens to look symmetric.
func TestWriteDemodIFFreqRoundsToTwosComplement(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := &rtl2832u{ctrl: mock}

	if err := chip.writeDemodIFFreq(r820tIFFreqHz, 28_800_000); err != nil {
		t.Fatalf("writeDemodIFFreq: %v", err)
	}

	writes := writesOnly(mock.calls)

	if len(writes) != 3 {
		t.Fatalf("got %d writes, want 3 (hi/mid/lo)", len(writes))
	}

	// hi byte's top two bits must be zero (the register slot is
	// 6 bits wide). 0x3818c0 truncated → 0x38.
	if got := writes[0].data[0]; got&0xc0 != 0 {
		t.Errorf("IF-freq hi-byte top bits leak into the register slot: got %#x", got)
	}

	// Reassembled 22-bit value must match the encoded -3.57 MHz.
	got22 := uint32(writes[0].data[0])<<16 |
		uint32(writes[1].data[0])<<8 |
		uint32(writes[2].data[0])
	got22 &= ifFreqRegMask

	const wantEncoded uint32 = 0x381112
	if got22 != wantEncoded {
		t.Errorf("reassembled IF-freq encoding = %#x, want %#x", got22, wantEncoded)
	}
}

// TestSetIFFrequencyDelegatesToWriter pins the exported wrapper to
// the same three-byte demod-page-1 write sequence that
// writeDemodIFFreq emits. The unexported helper is already covered;
// this test exists purely to keep the exported entry point from
// silently diverging (e.g. if a future refactor wires the wrapper
// to a different writer by accident).
func TestSetIFFrequencyDelegatesToWriter(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := &rtl2832u{ctrl: mock}

	if err := chip.SetIFFrequency(r820tIFFreqHz, 28_800_000); err != nil {
		t.Fatalf("SetIFFrequency: %v", err)
	}

	writes := writesOnly(mock.calls)

	if len(writes) != 3 {
		t.Fatalf("got %d writes, want 3 (hi/mid/lo)", len(writes))
	}

	// Reassembled 22-bit value must match the encoded -3.57 MHz —
	// same invariant TestWriteDemodIFFreqRoundsToTwosComplement
	// pins on the unexported path.
	got22 := uint32(writes[0].data[0])<<16 |
		uint32(writes[1].data[0])<<8 |
		uint32(writes[2].data[0])
	got22 &= ifFreqRegMask

	const wantEncoded uint32 = 0x381112
	if got22 != wantEncoded {
		t.Errorf("SetIFFrequency encoding = %#x, want %#x (delegation regressed)", got22, wantEncoded)
	}
}
