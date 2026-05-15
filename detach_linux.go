//go:build linux

package rtl2832u

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// USBDEVFS_DISCONNECT_CLAIM atomically evicts whatever in-kernel
// driver currently owns the interface (typically
// dvb_usb_rtl28xxu for RTL-SDR dongles) and claims the interface
// for the calling fd, in a single ioctl. Equivalent to libusb's
// libusb_detach_kernel_driver + libusb_claim_interface pair, but
// race-free with respect to a parallel hotplug rebind.
//
// Available since Linux 3.18 (2014). Older kernels return ENOTTY,
// in which case the caller is expected to unbind the kernel
// driver manually (e.g. via /sys/bus/usb/drivers/.../unbind).
//
// Kernel header:
//
//	#define USBDEVFS_DISCONNECT_CLAIM  _IOR('U', 27, struct usbdevfs_disconnect_claim)
//
//	struct usbdevfs_disconnect_claim {
//	    unsigned int interface;
//	    unsigned int flags;
//	    char         driver[USBDEVFS_MAXDRIVERNAME + 1];
//	};

const (
	// usbdevfsNRDisconnectClaim is the per-type sequence number
	// for USBDEVFS_DISCONNECT_CLAIM.
	usbdevfsNRDisconnectClaim = 27

	// usbdevfsMaxDriverName matches the kernel constant of the
	// same name. The driver field is one byte longer for the NUL
	// terminator.
	usbdevfsMaxDriverName = 255

	// disconnectClaimFlagsAny is the flags value for "evict
	// whatever driver is currently bound, no name match required."
	// flags=1 would constrain to a named driver; flags=2 would
	// invert (evict unless named). Neither is useful here.
	disconnectClaimFlagsAny uint32 = 0
)

// usbdevfsDisconnectClaim mirrors the kernel's struct
// usbdevfs_disconnect_claim. interface is renamed Iface because
// `interface` is a reserved word in Go.
type usbdevfsDisconnectClaim struct {
	Iface  uint32
	Flags  uint32
	Driver [usbdevfsMaxDriverName + 1]byte
}

//nolint:gochecknoglobals // initialised once from a constant Sizeof; immutable thereafter.
var usbdevfsDisconnectClaimRequest = ioctlRequest(
	ioctlDirRead,
	usbdevfsNRDisconnectClaim,
	unsafe.Sizeof(usbdevfsDisconnectClaim{}),
)

// errKernelDetachUnsupported is the sentinel callers can branch on
// when the running kernel predates USBDEVFS_DISCONNECT_CLAIM and
// the caller wants to fall back to the legacy unbind-via-sysfs
// path. wrapClaimError uses it to tailor the error message.
var errKernelDetachUnsupported = errors.New("rtl2832u: USBDEVFS_DISCONNECT_CLAIM unsupported (kernel < 3.18)")

// disconnectKernelAndClaim atomically evicts the in-kernel driver
// bound to iface (if any) and claims the interface for dev's fd.
// One syscall, no race with a hotplug rebind.
//
// ENOTTY → errKernelDetachUnsupported (kernel < 3.18; caller may
// fall back). Every other errno is returned wrapped verbatim.
func disconnectKernelAndClaim(dev *os.File, iface uint32) error {
	cmd := usbdevfsDisconnectClaim{
		Iface: iface,
		Flags: disconnectClaimFlagsAny,
	}

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		dev.Fd(),
		uintptr(usbdevfsDisconnectClaimRequest),
		uintptr(unsafe.Pointer(&cmd)), //nolint:gosec // cmd is stack-resident for the duration of the syscall.
	)
	if errno == 0 {
		return nil
	}

	if errors.Is(errno, syscall.ENOTTY) {
		return errKernelDetachUnsupported
	}

	return fmt.Errorf("ioctl USBDEVFS_DISCONNECT_CLAIM iface=%d: %w", iface, errno)
}
