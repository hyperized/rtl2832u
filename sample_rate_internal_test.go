package rtl2832u

import (
	"errors"
	"reflect"
	"testing"
)

func TestComputeRSampRatio2400000(t *testing.T) {
	t.Parallel()

	// 28_800_000 << 22 = 1.207e14; / 2_400_000 = 50_331_648 = 0x03000000.
	// Bottom two bits already zero, so the mask is a no-op.
	const want uint32 = 0x0300_0000

	got := computeRSampRatio(2_400_000, referenceClockHz)
	if got != want {
		t.Errorf("computeRSampRatio(2_400_000) = %#x, want %#x", got, want)
	}
}

func TestComputeRSampRatio2048000(t *testing.T) {
	t.Parallel()

	// (28_800_000 << 22) / 2_048_000 = 14.0625 << 22 = 58_982_400 =
	// 0x0384_0000. Bottom bits already zero, so the mask is a no-op.
	const want uint32 = 0x0384_0000

	got := computeRSampRatio(2_048_000, referenceClockHz)
	if got != want {
		t.Errorf("computeRSampRatio(2_048_000) = %#x, want %#x", got, want)
	}
}

func TestComputeActualSampleRate2400000Exact(t *testing.T) {
	t.Parallel()

	got := computeActualSampleRate(0x0300_0000, referenceClockHz)
	if got != 2_400_000 {
		t.Errorf("computeActualSampleRate(0x03000000) = %d, want 2_400_000", got)
	}
}

func TestComputeActualSampleRateZeroIsSafe(t *testing.T) {
	t.Parallel()

	// Defensive: a zero rsampRatio would cause a divide-by-zero in the
	// real-rate computation. The function returns 0 instead of panicking.
	if got := computeActualSampleRate(0, referenceClockHz); got != 0 {
		t.Errorf("computeActualSampleRate(0) = %d, want 0", got)
	}
}

func TestEffectiveXtalHzZeroIsIdentity(t *testing.T) {
	t.Parallel()

	if got := effectiveXtalHz(referenceClockHz, 0); got != referenceClockHz {
		t.Errorf("effectiveXtalHz(%d, 0) = %d, want %d (identity)",
			referenceClockHz, got, referenceClockHz)
	}
}

func TestEffectiveXtalHzPositivePPMShiftsUp(t *testing.T) {
	t.Parallel()

	// +100 ppm of 28.8 MHz = 2880 Hz. Crystal "runs fast" so the
	// effective xtal is higher than nominal.
	const want uint32 = 28_802_880

	if got := effectiveXtalHz(referenceClockHz, 100); got != want {
		t.Errorf("effectiveXtalHz(%d, +100) = %d, want %d",
			referenceClockHz, got, want)
	}
}

func TestEffectiveXtalHzNegativePPMShiftsDown(t *testing.T) {
	t.Parallel()

	// -50 ppm of 28.8 MHz = -1440 Hz.
	const want uint32 = 28_798_560

	if got := effectiveXtalHz(referenceClockHz, -50); got != want {
		t.Errorf("effectiveXtalHz(%d, -50) = %d, want %d",
			referenceClockHz, got, want)
	}
}

func TestComputeRSampRatioCompensatesPositivePPM(t *testing.T) {
	t.Parallel()

	// At +100 ppm, the chip's clock is 28_802_880 Hz instead of
	// 28_800_000. To still produce 2.4 MS/s on the wire we have
	// to ask for a slightly larger divider — the rsamp_ratio
	// must come out larger than the nominal 0x0300_0000.
	const targetRate uint32 = 2_400_000

	xtalCorrected := effectiveXtalHz(referenceClockHz, 100)

	nominal := computeRSampRatio(targetRate, referenceClockHz)
	corrected := computeRSampRatio(targetRate, xtalCorrected)

	if corrected <= nominal {
		t.Errorf("corrected rsamp_ratio = %#x, want > nominal %#x (positive ppm should grow the divider)",
			corrected, nominal)
	}

	// Sanity-check the effective rate the chip will produce: at
	// +100 ppm with the corrected divider it should reproduce the
	// requested 2.4 MS/s on the wire (within rounding) — that's
	// the whole point of the correction.
	produced := computeActualSampleRate(corrected, xtalCorrected)
	if delta := int64(produced) - int64(targetRate); delta > 100 || delta < -100 {
		t.Errorf("produced rate %d differs from target %d by %d Hz (>100 Hz rounding budget)",
			produced, targetRate, delta)
	}
}

func TestValidateSampleRateAcceptsValidLowAndHighRanges(t *testing.T) {
	t.Parallel()

	for _, rate := range []uint32{225_001, 250_000, 300_000, 900_001, 2_048_000, 2_400_000, 3_200_000} {
		if err := validateSampleRate(rate); err != nil {
			t.Errorf("validateSampleRate(%d) returned error %v, want nil", rate, err)
		}
	}
}

func TestValidateSampleRateRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	for _, rate := range []uint32{0, 100_000, 225_000, 3_200_001, 4_000_000} {
		err := validateSampleRate(rate)
		if !errors.Is(err, ErrSampleRateOutOfRange) {
			t.Errorf("validateSampleRate(%d) err = %v, want ErrSampleRateOutOfRange", rate, err)
		}
	}
}

func TestValidateSampleRateRejectsGap(t *testing.T) {
	t.Parallel()

	for _, rate := range []uint32{300_001, 500_000, 600_000, 900_000} {
		err := validateSampleRate(rate)
		if !errors.Is(err, ErrSampleRateInGap) {
			t.Errorf("validateSampleRate(%d) err = %v, want ErrSampleRateInGap", rate, err)
		}
	}
}

func TestSetSampleRate2400000Writes(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	actual, err := chip.SetSampleRate(2_400_000, referenceClockHz)
	if err != nil {
		t.Fatalf("SetSampleRate: %v", err)
	}

	if actual != 2_400_000 {
		t.Errorf("actual rate = %d, want 2_400_000 (exact for this ratio)", actual)
	}

	got := writesOnly(mock.calls)

	pg1 := demodIdx(demodPage1)
	want := []capturedCall{
		// rsamp_ratio = 0x0300_0000: hi half 0x0300, lo half 0x0000.
		wantWrite(encodeDemodAddr(regDemodRSampRatioHi), pg1, 0x03, 0x00),
		wantWrite(encodeDemodAddr(regDemodRSampRatioLo), pg1, 0x00, 0x00),
		// reset pulse (matches the resetDemod phase shape).
		wantWrite(encodeDemodAddr(0x01), pg1, 0x14),
		wantWrite(encodeDemodAddr(0x01), pg1, 0x10),
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %#v\nwant %#v", got, want)
	}
}

func TestSetSampleRate2048000Writes(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if _, err := chip.SetSampleRate(2_048_000, referenceClockHz); err != nil {
		t.Fatalf("SetSampleRate: %v", err)
	}

	got := writesOnly(mock.calls)

	pg1 := demodIdx(demodPage1)
	want := []capturedCall{
		// rsamp_ratio = 0x0384_0000: hi half 0x0384, lo half 0x0000.
		wantWrite(encodeDemodAddr(regDemodRSampRatioHi), pg1, 0x03, 0x84),
		wantWrite(encodeDemodAddr(regDemodRSampRatioLo), pg1, 0x00, 0x00),
		wantWrite(encodeDemodAddr(0x01), pg1, 0x14),
		wantWrite(encodeDemodAddr(0x01), pg1, 0x10),
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %#v\nwant %#v", got, want)
	}
}

func TestSetSampleRateRejectsInvalid(t *testing.T) {
	t.Parallel()

	mock := &mockController{inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	// 600 kHz lies in the (300 kHz, 900 kHz] gap.
	if _, err := chip.SetSampleRate(600_000, referenceClockHz); !errors.Is(err, ErrSampleRateInGap) {
		t.Errorf("err = %v, want ErrSampleRateInGap", err)
	}

	// And nothing should have hit the wire.
	if len(mock.calls) != 0 {
		t.Errorf("expected no controller calls on rejection, got %d", len(mock.calls))
	}
}

func TestSetSampleRateWrapsControllerError(t *testing.T) {
	t.Parallel()

	mock := &mockController{outErr: errFakeControlOut, inDefault: flushReadOK}
	chip := newRTL2832U(mock)

	if _, err := chip.SetSampleRate(2_400_000, referenceClockHz); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}

// TestSetSampleRateResetFailureSurfaces hits the error wrap on the
// post-write soft-reset pulse: the rsamp_ratio writes succeed, but
// the soft-reset bytes that follow them fail. SetSampleRate must
// surface that error rather than silently returning the actual
// rate.
func TestSetSampleRateResetFailureSurfaces(t *testing.T) {
	t.Parallel()

	const failAfterWrites = 2 // 2 ratio writes succeed; 3rd onwards fail (the reset pulse)

	mock := &countingController{
		mockController: &mockController{inDefault: flushReadOK},
		failAfter:      failAfterWrites,
	}
	chip := newRTL2832U(mock)

	if _, err := chip.SetSampleRate(2_400_000, referenceClockHz); !errors.Is(err, errFakeControlOut) {
		t.Errorf("err = %v, want wrapping errFakeControlOut", err)
	}
}
