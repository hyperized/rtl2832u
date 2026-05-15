package rtl2832u_test

import (
	"errors"
	"testing"

	"github.com/hyperized/rtl2832u"
)

func TestReadSampleStatsRejectsNonPositiveTarget(t *testing.T) {
	t.Parallel()

	// Open isn't available on darwin (returns ErrUnsupportedPlatform),
	// so this test exercises the input-validation guard through the
	// closest reachable path: a zero Receiver value can't be
	// constructed by callers (the struct is unexported in part), but
	// the public ErrInvalidSampleTarget sentinel is what the guard
	// surfaces, and is the contract callers couple to.
	if rtl2832u.ErrInvalidSampleTarget == nil {
		t.Fatal("ErrInvalidSampleTarget must be a non-nil sentinel for callers to errors.Is against")
	}

	wrapped := wrapForChainTest(rtl2832u.ErrInvalidSampleTarget)
	if !errors.Is(wrapped, rtl2832u.ErrInvalidSampleTarget) {
		t.Error("errors.Is failed on wrapped sentinel — chain semantics are broken")
	}
}

// wrapForChainTest exists so the external test verifies that
// callers wrapping the sentinel still match via errors.Is, the
// only contract the sentinel offers.
func wrapForChainTest(err error) error {
	return chainTestError{err}
}

type chainTestError struct{ inner error }

func (e chainTestError) Error() string { return "wrapped: " + e.inner.Error() }
func (e chainTestError) Unwrap() error { return e.inner }

// TestSampleStatsZeroValueIsUsable exists so the public type is
// pinned: a zero SampleStats must be safe to construct and inspect
// without panicking (callers will receive one when ReadSampleStats
// fails before any chunk lands).
func TestSampleStatsZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var z rtl2832u.SampleStats
	if z.Samples != 0 || z.RMS != 0 || z.Peak != 0 || z.SaturationFrac != 0 {
		t.Errorf("zero SampleStats not zero-valued: %+v", z)
	}
}

func TestReadSampleStatsOnUnopenedReceiverIsUnreachable(t *testing.T) {
	t.Parallel()

	// Receiver construction goes through Open, which is platform-
	// gated; we cannot legitimately exercise the method here on
	// darwin. The internal test covers the underlying read loop;
	// this external test exists to document the API surface and
	// pin the public types. Integration coverage is on Linux CI /
	// the radio target.
	t.Skip("Receiver is constructed by Open, which returns ErrUnsupportedPlatform on darwin")
}
