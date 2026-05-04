package rtl2832u

import (
	"errors"
	"fmt"
)

// ErrNoTuner reports that SetCenterFreq was called without a tuner.
// The RTL2832U cannot reach 1090 MHz on its own — the chip's
// internal datapath only handles baseband; the tuner is what mixes
// the RF down. Without one we have no way to retune, so the call is
// rejected up front.
//
// Direct-sampling mode (chip-only reception ≤ 14 MHz) lands when a
// caller actually needs it; until then SetCenterFreq is a tuner-only
// operation.
var ErrNoTuner = errors.New(
	"sdr: SetCenterFreq requires a Tuner; pass an R860 (or other) " +
		"tuner implementation when one becomes available, or use " +
		"direct-sampling mode for sub-14 MHz reception once that lands",
)

// SetCenterFreq retunes the receiver to hz. With Init having
// configured Zero-IF mode, the chip's IF stays at zero and the
// tuner does all the mixing — we just delegate.
//
// Errors from the tuner are wrapped with the tuner's Name() so
// downstream messages identify the silicon, not just "tuner failed".
func (*rtl2832u) SetCenterFreq(rfHz uint32, tuner Tuner) error {
	if tuner == nil {
		return ErrNoTuner
	}

	if err := tuner.SetFreq(rfHz); err != nil {
		return fmt.Errorf("sdr: tuner %s set freq %d Hz: %w", tuner.Name(), rfHz, err)
	}

	return nil
}
