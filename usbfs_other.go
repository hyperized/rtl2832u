//go:build !linux

package rtl2832u

// openBackend is the non-Linux fallback. We deliberately do not gate on
// CGo or attempt to use libusb here: the deploy target is Linux on
// aarch64, and darwin support is for editing and unit tests only.
//
//nolint:ireturn // backend is the polymorphic seam between platform builds.
func openBackend(_ config) (backend, error) {
	return nil, ErrUnsupportedPlatform
}
