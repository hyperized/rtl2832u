//go:build linux

package rtl2832u

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// TestUSBDevFSDisconnectClaimStructSize asserts the Go struct's
// memory layout matches the kernel's struct usbdevfs_disconnect_claim:
//
//	struct usbdevfs_disconnect_claim {
//	    unsigned int interface;
//	    unsigned int flags;
//	    char         driver[USBDEVFS_MAXDRIVERNAME + 1];
//	};
//
// USBDEVFS_MAXDRIVERNAME is 255 in include/uapi/linux/usbdevice_fs.h,
// so the array is 256 bytes. Total: 4 + 4 + 256 = 264 bytes on any
// arch where uint is 32 bits (every Linux target we ship to).
func TestUSBDevFSDisconnectClaimStructSize(t *testing.T) {
	t.Parallel()

	const wantBytes = 264
	if got := unsafe.Sizeof(usbdevfsDisconnectClaim{}); got != wantBytes {
		t.Errorf("sizeof(usbdevfsDisconnectClaim) = %d, want %d", got, wantBytes)
	}
}

// TestUSBDevFSDisconnectClaimRequestEncoding pins the encoded
// ioctl number so a future change to ioctlRequest or the struct
// can't silently shift the wire value. Computed:
//
//	_IOR('U', 27, struct usbdevfs_disconnect_claim)
//	= (2<<30) | (264<<16) | ('U'<<8) | 27
//	= 0x8108551b
func TestUSBDevFSDisconnectClaimRequestEncoding(t *testing.T) {
	t.Parallel()

	const wantRequest uint = 0x8108551b
	if usbdevfsDisconnectClaimRequest != wantRequest {
		t.Errorf("usbdevfsDisconnectClaimRequest = %#x, want %#x",
			usbdevfsDisconnectClaimRequest, wantRequest)
	}
}

// TestDisconnectKernelAndClaimENOTTY drives the syscall against a
// regular file that the kernel will reject with ENOTTY (regular
// fds don't accept USBDEVFS ioctls). The function must translate
// that into errKernelDetachUnsupported so callers can fall back.
func TestDisconnectKernelAndClaimENOTTY(t *testing.T) {
	t.Parallel()

	regularFile, err := os.CreateTemp(t.TempDir(), "non-usb-fd-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}

	defer func() { _ = regularFile.Close() }()

	err = disconnectKernelAndClaim(regularFile, 0)
	if !errors.Is(err, errKernelDetachUnsupported) {
		t.Errorf("disconnectKernelAndClaim on tempfile = %v, want errKernelDetachUnsupported", err)
	}
}

// TestDisconnectKernelAndClaimEBADF drives the syscall against a
// closed fd to exercise the "errno that is neither ENOTTY nor 0"
// branch — the function must return that error wrapped, not
// silently swallowed.
func TestDisconnectKernelAndClaimEBADF(t *testing.T) {
	t.Parallel()

	regularFile, err := os.CreateTemp(t.TempDir(), "closed-fd-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}

	if err := regularFile.Close(); err != nil {
		t.Fatalf("Close temp: %v", err)
	}

	err = disconnectKernelAndClaim(regularFile, 0)
	if err == nil {
		t.Fatal("expected error from closed fd; got nil")
	}

	if errors.Is(err, errKernelDetachUnsupported) {
		t.Errorf("EBADF should not collapse to errKernelDetachUnsupported; got %v", err)
	}

	if !errors.Is(err, syscall.EBADF) {
		t.Errorf("expected EBADF wrap; got %v", err)
	}
}
