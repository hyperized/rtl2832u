//go:build linux

package rtl2832u

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// usbdevfsCtrlTransfer mirrors the kernel's struct
// usbdevfs_ctrltransfer from include/uapi/linux/usbdevice_fs.h:
//
//	struct usbdevfs_ctrltransfer {
//	    __u8  bRequestType;
//	    __u8  bRequest;
//	    __u16 wValue;
//	    __u16 wIndex;
//	    __u16 wLength;
//	    __u32 timeout;     /* in milliseconds */
//	    void __user *data;
//	};
//
// Field order, names, and widths must match exactly: the kernel reads
// the struct as a flat memory image, so any reordering or padding
// difference yields silent corruption rather than an obvious error.
// Go's natural alignment puts the same padding before `data` as the C
// compiler does on linux/amd64 and linux/arm64 (the only platforms
// this build target supports), so no explicit padding field is needed.
type usbdevfsCtrlTransfer struct {
	bRequestType uint8
	bRequest     uint8
	wValue       uint16
	wIndex       uint16
	wLength      uint16
	timeoutMs    uint32
	data         uintptr
}

// usbControlTimeoutMs is the per-transfer timeout. librtlsdr uses 300
// for every RTL2832U register operation: short enough to fail fast
// when the dongle is unplugged, long enough to not give up on a
// loaded host. We follow the upstream value rather than tuning down
// because the chip's response time isn't documented.
const usbControlTimeoutMs uint32 = 300

// usbControlMaxPayload is the upper bound on data passed to a single
// USB control transfer. wLength in the kernel struct is a __u16, so
// the absolute ceiling is 65535; we keep the constant explicit so the
// bounds check below has a citable value rather than a magic number.
const usbControlMaxPayload = 1<<16 - 1

// errPayloadTooLarge is the static sentinel for a control transfer
// whose data would not fit in the wLength field. Reaching this is a
// programming error inside demod1090 (RTL2832U registers are 1, 2, or
// 4 bytes — never anywhere near 64 KiB), so the message says so.
var errPayloadTooLarge = errors.New(
	"usb control: payload exceeds wLength; this is a programming error " +
		"in demod1090 — please file an issue with the failing call site",
)

// USB control transfer bmRequestType for vendor requests addressed to
// the whole device. RTL2832U register access never strays outside
// this combination, so we hardcode the values rather than computing
// them at every call site:
//
//	bit 7    : direction (0 = host→device / OUT, 1 = device→host / IN)
//	bits 6:5 : type      (01 = vendor)
//	bits 4:0 : recipient (00000 = device)
const (
	usbReqTypeVendorIn  uint8 = 0xC0
	usbReqTypeVendorOut uint8 = 0x40
)

// _IOC encoding from include/uapi/asm-generic/ioctl.h. The 32-bit
// ioctl request code packs four fields:
//
//	bits 31:30 = direction (0 none, 1 W, 2 R, 3 RW)
//	bits 29:16 = size of the argument struct in bytes (14 bits)
//	bits 15:8  = type      (the ioctl "magic", 'U' = 0x55 here)
//	bits 7:0   = nr        (per-type sequence number, 0 for CONTROL)
//
// Computing the request code at run time via unsafe.Sizeof rather
// than hard-coding 0xc0185500 keeps us correct on any architecture
// where struct alignment differs (32-bit Linux, hypothetically) — the
// extra cycles run once, at startup, when the package var initialises.
// ioctl direction codes per the _IOC family. The names follow the
// kernel macros' convention: "read" / "write" describe the transfer
// from the caller's perspective:
//
//	_IO   — no data argument
//	_IOR  — kernel writes into the caller's buffer (caller reads)
//	_IOW  — caller writes into the kernel's buffer (caller writes)
//	_IOWR — both directions, used when a single struct carries
//	         both inputs and outputs (e.g. USBDEVFS_CONTROL).
const (
	ioctlDirNone  = 0
	ioctlDirWrite = 1
	ioctlDirRead  = 2
	ioctlDirRW    = ioctlDirWrite | ioctlDirRead

	ioctlSizeBitShift = 16
	ioctlTypeBitShift = 8
	ioctlDirBitShift  = 30

	// ioctlTypeUSBDevFS is the kernel's "magic" letter for the
	// usbfs ioctl family ('U' = 0x55). Every ioctl this driver
	// issues uses it, so we hardcode the value inside ioctlRequest
	// rather than thread it through every call site.
	ioctlTypeUSBDevFS = 'U'
)

func ioctlRequest(dir, nr uint, size uintptr) uint {
	return dir<<ioctlDirBitShift |
		uint(size)<<ioctlSizeBitShift |
		ioctlTypeUSBDevFS<<ioctlTypeBitShift |
		nr
}

// usbdevfsControl is the ioctl request code for USBDEVFS_CONTROL,
// equivalent to the kernel macro:
//
//	#define USBDEVFS_CONTROL  _IOWR('U', 0, struct usbdevfs_ctrltransfer)
//
//nolint:gochecknoglobals // initialised once from a constant Sizeof; immutable thereafter.
var usbdevfsControl = ioctlRequest(ioctlDirRW, usbdevfsNRControl, unsafe.Sizeof(usbdevfsCtrlTransfer{}))

// usbdevfsNRControl is the per-type sequence number for
// USBDEVFS_CONTROL. Names of the form usbdevfsNR* live next to
// their ioctl request codes; bulk_linux.go declares the bulk-side
// NRs in the same style.
const usbdevfsNRControl = 0

// controlIn satisfies the controller interface: it issues a vendor IN
// control transfer and fills data with the chip's response. Returns
// the number of bytes the kernel reports as transferred.
func (b *linuxBackend) controlIn(req uint8, value, index uint16, data []byte) (int, error) {
	return b.doControl(usbReqTypeVendorIn, req, value, index, data)
}

// controlOut satisfies the controller interface: it issues a vendor
// OUT control transfer with data as the payload.
func (b *linuxBackend) controlOut(req uint8, value, index uint16, data []byte) (int, error) {
	return b.doControl(usbReqTypeVendorOut, req, value, index, data)
}

// doControl is the shared body for controlIn/controlOut. Splitting
// it out keeps the public-shaped methods small and the syscall
// detail in one place, which matters because that detail involves
// unsafe.Pointer arithmetic that we don't want to multiply.
func (b *linuxBackend) doControl(reqType, req uint8, value, index uint16, data []byte) (int, error) {
	if len(data) > usbControlMaxPayload {
		return 0, fmt.Errorf("%w: %d bytes (max %d)", errPayloadTooLarge, len(data), usbControlMaxPayload)
	}

	// G103: taking a Go pointer for a syscall argument is the only
	// way to address a buffer that the kernel will fill or read; the
	// statement-local lifetime ensures the GC does not move the slice
	// before unix.Syscall returns. The standard library does this
	// pattern in syscall/zsyscall_linux_amd64.go and elsewhere.
	var dataPtr uintptr
	if len(data) > 0 {
		dataPtr = uintptr(unsafe.Pointer(&data[0])) //nolint:gosec
	}

	ctrl := usbdevfsCtrlTransfer{
		bRequestType: reqType,
		bRequest:     req,
		wValue:       value,
		wIndex:       index,
		wLength:      uint16(len(data)), //nolint:gosec // bounded by usbControlMaxPayload check above.
		timeoutMs:    usbControlTimeoutMs,
		data:         dataPtr,
	}

	transferred, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		b.dev.Fd(),
		uintptr(usbdevfsControl),
		uintptr(unsafe.Pointer(&ctrl)), //nolint:gosec // see G103 comment above; same lifetime invariant.
	)
	if errno != 0 {
		return 0, wrapControlErrno(errno)
	}

	return int(transferred), nil
}

// wrapControlErrno turns the bare errno returned by USBDEVFS_CONTROL
// into a message that names the most likely cause for the failure
// modes we actually see in practice. Everything else falls through
// to the bare ioctl wrap, so unfamiliar errnos stay visible rather
// than getting buried by a generic message.
func wrapControlErrno(errno syscall.Errno) error {
	switch {
	case errors.Is(errno, syscall.EPIPE):
		return fmt.Errorf(
			"ioctl USBDEVFS_CONTROL: stalled endpoint (the chip rejected "+
				"the request — usually means init was incomplete or the "+
				"register/page is wrong; reopening the device clears the stall): %w",
			errno)
	case errors.Is(errno, syscall.ETIMEDOUT):
		return fmt.Errorf(
			"ioctl USBDEVFS_CONTROL: timed out after %d ms (host is heavily "+
				"loaded, USB cable/hub flaky, or chip is wedged — try a different "+
				"USB port and check `dmesg` for hub errors): %w",
			usbControlTimeoutMs, errno)
	case errors.Is(errno, syscall.ENODEV):
		return fmt.Errorf(
			"ioctl USBDEVFS_CONTROL: no such device (the dongle was "+
				"disconnected mid-operation; reopen with sdr.Open after re-plug): %w",
			errno)
	default:
		return fmt.Errorf("ioctl USBDEVFS_CONTROL: %w", errno)
	}
}
