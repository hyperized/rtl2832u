//go:build linux

package rtl2832u

import (
	"os"
	"testing"
	"unsafe"
)

// TestUSBDevFsURBSize backs up the compile-time const-assertion
// with a runtime check, so a future Go layout change shows up as a
// readable test failure rather than a cryptic build-time underflow.
func TestUSBDevFsURBSize(t *testing.T) {
	t.Parallel()

	const want = 56

	if got := unsafe.Sizeof(usbdevfsURB{}); got != want {
		t.Errorf("sizeof(usbdevfsURB) = %d, want %d", got, want)
	}
}

// TestUSBDevFsURBOffsets pins the field offsets the kernel reads.
// Re-ordering the struct fields would corrupt the URB descriptor
// and produce silent decoding bugs at runtime; an explicit
// per-field test makes that breakage loud.
func TestUSBDevFsURBOffsets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"urbType", unsafe.Offsetof(usbdevfsURB{}.urbType), 0},
		{"endpoint", unsafe.Offsetof(usbdevfsURB{}.endpoint), 1},
		{"status", unsafe.Offsetof(usbdevfsURB{}.status), 4},
		{"flags", unsafe.Offsetof(usbdevfsURB{}.flags), 8},
		{"buffer", unsafe.Offsetof(usbdevfsURB{}.buffer), 16},
		{"bufferLength", unsafe.Offsetof(usbdevfsURB{}.bufferLength), 24},
		{"actualLength", unsafe.Offsetof(usbdevfsURB{}.actualLength), 28},
		{"startFrame", unsafe.Offsetof(usbdevfsURB{}.startFrame), 32},
		{"streamID", unsafe.Offsetof(usbdevfsURB{}.streamID), 36},
		{"errorCount", unsafe.Offsetof(usbdevfsURB{}.errorCount), 40},
		{"signr", unsafe.Offsetof(usbdevfsURB{}.signr), 44},
		{"userContext", unsafe.Offsetof(usbdevfsURB{}.userContext), 48},
	}

	for _, field := range tests {
		t.Run(field.name, func(t *testing.T) {
			t.Parallel()

			if field.offset != field.want {
				t.Errorf("offsetof(%s) = %d, want %d", field.name, field.offset, field.want)
			}
		})
	}
}

// linuxBackendOnPipe builds a backend whose dev is one end of a
// pipe. ioctls against it fail with ENOTTY (or similar), which is
// exactly what the submit/reap/discard helpers need to exercise
// their error-wrapping paths without real USB hardware.
func linuxBackendOnPipe(t *testing.T) *linuxBackend {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	return &linuxBackend{dev: writer}
}

func TestSubmitURBSurfacesIoctlError(t *testing.T) {
	t.Parallel()

	back := linuxBackendOnPipe(t)

	urb := &usbdevfsURB{urbType: usbdevfsURBTypeBulk, endpoint: usbBulkInEndpoint}
	if err := back.submitURB(urb); err == nil {
		t.Error("expected SUBMITURB on a pipe to fail, got nil")
	}
}

func TestReapURBSurfacesIoctlError(t *testing.T) {
	t.Parallel()

	back := linuxBackendOnPipe(t)

	if _, err := back.reapURB(); err == nil {
		t.Error("expected REAPURB on a pipe to fail, got nil")
	}
}

func TestDrainNextURBSurfacesIoctlError(t *testing.T) {
	t.Parallel()

	back := linuxBackendOnPipe(t)

	// REAPURBNDELAY on a pipe yields ENOTTY (not EAGAIN), so the
	// caller sees a wrapped error rather than the empty "no URB
	// completed yet" return.
	if _, err := back.drainNextURB(); err == nil {
		t.Error("expected REAPURBNDELAY on a pipe to surface an error")
	}
}

func TestDiscardURBSurfacesIoctlError(t *testing.T) {
	t.Parallel()

	back := linuxBackendOnPipe(t)

	urb := &usbdevfsURB{urbType: usbdevfsURBTypeBulk, endpoint: usbBulkInEndpoint}
	// DISCARDURB swallows EINVAL (URB already completed); a pipe
	// surfaces a different errno (ENOTTY), so the wrapper should
	// propagate.
	if err := back.discardURB(urb); err == nil {
		t.Error("expected DISCARDURB on a pipe to surface an error, got nil")
	}
}
