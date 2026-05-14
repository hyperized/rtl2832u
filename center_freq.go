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
	"rtl2832u: SetCenterFreq requires a Tuner; pass an R860 (or other) " +
		"tuner implementation when one becomes available, or use " +
		"direct-sampling mode for sub-14 MHz reception once that lands",
)

// SetCenterFreq retunes the receiver to rfHz. With configureForR820T
// having put the chip in real-IF mode at intFreqHz, the tuner's
// SetFreq is responsible for both LO programming and offsetting by
// intFreqHz — we just delegate.
//
// Errors from the tuner are wrapped with the tuner's Name() so
// downstream messages identify the silicon, not just "tuner failed".
func (*rtl2832u) SetCenterFreq(rfHz uint32, tuner Tuner) error {
	if tuner == nil {
		return ErrNoTuner
	}

	if err := tuner.SetFreq(rfHz); err != nil {
		return fmt.Errorf("rtl2832u: tuner %s set freq %d Hz: %w", tuner.Name(), rfHz, err)
	}

	return nil
}
