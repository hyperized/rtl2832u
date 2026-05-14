//go:build linux

package rtl2832u

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// USB bulk transfers carry the RTL-SDR's IQ sample stream. Each
// transfer ferries up to ~16 KiB from the chip's bulk IN endpoint
// (0x81) to a host-side buffer; we keep a ring of these in flight
// so the chip never sees a back-pressure stall on its FIFO.
//
// Async usbfs API
// ---------------
// The Linux kernel exposes async USB I/O through three ioctls:
//
//	USBDEVFS_SUBMITURB     — hand a URB descriptor to the kernel,
//	                         which queues the transfer.
//	USBDEVFS_REAPURB       — block until any URB completes; the
//	                         kernel writes the address of the
//	                         completed URB into the caller's
//	                         pointer.
//	USBDEVFS_REAPURBNDELAY — non-blocking variant; returns -EAGAIN
//	                         when no URB has completed yet.
//	USBDEVFS_DISCARDURB    — cancel an in-flight URB.
//
// The reaped URB carries the kernel's status (success / errno) and
// the actual bytes transferred. Callers typically resubmit the URB
// immediately to keep the ring full.
//
// We hardcode the bulk IN endpoint as 0x81 — that's where the
// RTL2832U streams its IQ samples in SDR mode. librtlsdr does the
// same; it's not configurable from the host without modifying
// kernel-internal RTL2832U state.
const (
	usbBulkInEndpoint uint8 = 0x81

	usbdevfsURBTypeBulk uint8 = 3
)

// usbdevfsURB mirrors the kernel's struct usbdevfs_urb from
// include/uapi/linux/usbdevice_fs.h:
//
//	struct usbdevfs_urb {
//	    unsigned char type;
//	    unsigned char endpoint;
//	    int           status;
//	    unsigned int  flags;
//	    void __user  *buffer;
//	    int           buffer_length;
//	    int           actual_length;
//	    int           start_frame;
//	    union {
//	        int          number_of_packets;
//	        unsigned int stream_id;
//	    };
//	    int           error_count;
//	    unsigned int  signr;
//	    void __user  *usercontext;
//	    struct usbdevfs_iso_packet_desc iso_frame_desc[0];
//	};
//
// On linux/amd64 and linux/arm64 the natural Go alignment matches
// the C compiler's: 56 bytes total. The compile-time assertion at
// the bottom of this file guards against drift.
type usbdevfsURB struct {
	urbType      uint8
	endpoint     uint8
	_            [2]byte // pad to 4-byte boundary; matches C alignment
	status       int32
	flags        uint32
	buffer       uintptr // void __user *buffer
	bufferLength int32
	actualLength int32
	startFrame   int32
	streamID     uint32 // union with number_of_packets
	errorCount   int32
	signr        uint32
	userContext  uintptr // void __user *usercontext
}

// USBDEVFS bulk-transfer ioctl request codes, computed at startup
// from the matching kernel macros. Keeping these as runtime
// constants (rather than hard-coded hex) means the code stays
// correct on hypothetical 32-bit builds where struct alignment
// would shift the encoded size.
//
//	USBDEVFS_SUBMITURB     _IOR ('U', 10, struct usbdevfs_urb)
//	USBDEVFS_DISCARDURB    _IO  ('U', 11)
//	USBDEVFS_REAPURB       _IOW ('U', 12, void *)
//	USBDEVFS_REAPURBNDELAY _IOW ('U', 13, void *)
const (
	usbdevfsNRSubmitURB     = 10
	usbdevfsNRDiscardURB    = 11
	usbdevfsNRReapURB       = 12
	usbdevfsNRReapURBNDelay = 13
)

//nolint:gochecknoglobals // initialised once from constants; immutable thereafter.
var (
	usbdevfsSubmitURB     = ioctlRequest(ioctlDirRead, usbdevfsNRSubmitURB, unsafe.Sizeof(usbdevfsURB{}))
	usbdevfsDiscardURB    = ioctlRequest(ioctlDirNone, usbdevfsNRDiscardURB, 0)
	usbdevfsReapURB       = ioctlRequest(ioctlDirWrite, usbdevfsNRReapURB, unsafe.Sizeof(uintptr(0)))
	usbdevfsReapURBNDelay = ioctlRequest(ioctlDirWrite, usbdevfsNRReapURBNDelay, unsafe.Sizeof(uintptr(0)))
)

// submitURB hands the URB to the kernel for processing. The URB
// pointer must outlive the kernel queueing — typically callers keep
// the URB struct in a fixed slice for the lifetime of the stream.
//
// SUBMITURB is a non-blocking call: the URB is placed on the
// kernel's queue and the call returns immediately. Reaping retrieves
// the completion later.
func (b *linuxBackend) submitURB(urb *usbdevfsURB) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		b.dev.Fd(),
		uintptr(usbdevfsSubmitURB),
		uintptr(unsafe.Pointer(urb)), //nolint:gosec // urb pinned by caller for lifetime of stream.
	)
	if errno != 0 {
		return fmt.Errorf("ioctl USBDEVFS_SUBMITURB ep=%#x: %w", urb.endpoint, errno)
	}

	return nil
}

// reapURB blocks until any URB on the device completes and returns
// a pointer to the completed URB struct (the same pointer that was
// passed to submitURB). The kernel writes status and actualLength
// into the URB; callers inspect those before deciding whether to
// resubmit.
//
// A signal delivered to the process while we sleep in this ioctl
// surfaces as EINTR; the standard Linux convention is to retry the
// syscall — EINTR is not a stream-level error, just a wake-up.
// The Go runtime delivers SIGURG to goroutines for asynchronous
// preemption, which would otherwise translate every preemption
// into a fake "stream failed" event.
func (b *linuxBackend) reapURB() (*usbdevfsURB, error) {
	var completed *usbdevfsURB

	for {
		_, _, errno := unix.Syscall(
			unix.SYS_IOCTL,
			b.dev.Fd(),
			uintptr(usbdevfsReapURB),
			uintptr(unsafe.Pointer(&completed)), //nolint:gosec // kernel writes completed-URB address into our pointer.
		)
		if errno == 0 {
			return completed, nil
		}

		if errors.Is(errno, syscall.EINTR) {
			continue
		}

		return nil, fmt.Errorf("ioctl USBDEVFS_REAPURB: %w", errno)
	}
}

// drainNextURB attempts to reap a completed URB without blocking.
// The bool is false (with err == nil) when no URB has completed
// yet (kernel reports EAGAIN); used by the stream shutdown path
// to drain any URBs the kernel still holds without hanging on a
// stall. Callers don't need the URB pointer — the kernel has
// already written status into our backing slice.
func (b *linuxBackend) drainNextURB() (bool, error) {
	var completed *usbdevfsURB

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		b.dev.Fd(),
		uintptr(usbdevfsReapURBNDelay),
		uintptr(unsafe.Pointer(&completed)), //nolint:gosec // kernel writes completed-URB address into our pointer.
	)
	if errno != 0 {
		if errors.Is(errno, syscall.EAGAIN) {
			return false, nil
		}

		return false, fmt.Errorf("ioctl USBDEVFS_REAPURBNDELAY: %w", errno)
	}

	return completed != nil, nil
}

// discardURB cancels an in-flight URB. The kernel will eventually
// reap it with status -ECONNRESET; callers shut down by discarding
// every URB they submitted, then draining via reapURBNoBlock until
// it returns (nil, nil).
//
// DISCARDURB takes the URB pointer directly as the ioctl argument
// (not a pointer-to-pointer like REAPURB). EINVAL is returned if
// the URB has already completed; callers can ignore that.
func (b *linuxBackend) discardURB(urb *usbdevfsURB) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		b.dev.Fd(),
		uintptr(usbdevfsDiscardURB),
		uintptr(unsafe.Pointer(urb)), //nolint:gosec // same lifetime invariant as submitURB.
	)
	if errno != 0 && !errors.Is(errno, syscall.EINVAL) {
		return fmt.Errorf("ioctl USBDEVFS_DISCARDURB ep=%#x: %w", urb.endpoint, errno)
	}

	return nil
}

// Compile-time assertion: the URB struct must be exactly 56 bytes
// on the platforms we ship (linux/amd64 and linux/arm64). If a
// future Go version or a 32-bit build changes the layout, this
// expression underflows uintptr and the build fails — louder than
// a runtime mis-decode.
const _ uintptr = 56 - unsafe.Sizeof(usbdevfsURB{})

// --- Bulk stream parameters ---
//
// streamRingURBs and streamURBLen size the URB ring. 32 × 16 KiB =
// 512 KiB total, which is what librtlsdr defaults to and what real
// dongles tolerate well: enough to absorb scheduler jitter on a
// busy host, small enough that the buffers fit comfortably in
// data cache.
const (
	streamRingURBs = 32
	streamURBLen   = 16 * 1024
	streamChanCap  = streamRingURBs
)

// errStreamShortReap is the static sentinel for a reapURB call
// that returned no URB at all. The kernel should always hand back
// a pointer when the syscall succeeds; a nil return is a sign of
// driver corruption rather than expected behaviour.
var errStreamShortReap = errors.New("sdr: bulk reap returned no URB despite success")

// errStreamURBStatus wraps the kernel's reported errno on a
// completed URB. Errno values are platform-defined small integers
// (e.g. -EOVERFLOW, -EPROTO); the wrap surfaces the raw number so
// users can grep kernel logs without a translation table.
var errStreamURBStatus = errors.New("sdr: bulk URB completed with non-zero status")

// ensureStream lazily starts the streaming goroutine on the first
// Read call. sync.Once guarantees the URB ring is allocated and
// submitted exactly once even under concurrent Reads; subsequent
// calls see streamErr (which may be nil for success) without
// re-running the body.
func (b *linuxBackend) ensureStream() error {
	b.streamOnce.Do(func() {
		b.streamErr = b.startStream()
	})

	return b.streamErr
}

// startStream allocates the URB ring, submits every URB, and
// launches the reaper goroutine. On any submit failure it discards
// the URBs already submitted and drains via reapURBNoBlock so the
// kernel doesn't keep a dangling reference.
func (b *linuxBackend) startStream() error {
	b.urbBufs = make([][]byte, streamRingURBs)
	b.urbs = make([]usbdevfsURB, streamRingURBs)

	for idx := range b.urbs {
		b.urbBufs[idx] = make([]byte, streamURBLen)
		b.urbs[idx] = usbdevfsURB{
			urbType:  usbdevfsURBTypeBulk,
			endpoint: usbBulkInEndpoint,
			//nolint:gosec // urb buffer pinned for stream lifetime in b.urbBufs.
			buffer: uintptr(unsafe.Pointer(&b.urbBufs[idx][0])),
			//nolint:gosec // streamURBLen is a small constant fitting in int32.
			bufferLength: int32(streamURBLen),
			//nolint:gosec // ring index, bounded by streamRingURBs.
			userContext: uintptr(idx),
		}

		if err := b.submitURB(&b.urbs[idx]); err != nil {
			b.unwindSubmittedURBs(idx)

			return fmt.Errorf("sdr: stream submit URB %d/%d: %w", idx, streamRingURBs, err)
		}
	}

	b.streamCh = make(chan []byte, streamChanCap)
	b.streamDone = make(chan struct{})

	go b.runStreamReaper()

	return nil
}

// unwindSubmittedURBs cancels the URBs we managed to submit before
// hitting an error during startup, then drains the kernel's queue
// via the non-blocking reap so the device file descriptor is in a
// clean state for Close.
func (b *linuxBackend) unwindSubmittedURBs(submitted int) {
	for idx := range submitted {
		_ = b.discardURB(&b.urbs[idx])
	}

	for {
		hasURB, err := b.drainNextURB()
		if err != nil || !hasURB {
			return
		}
	}
}

// runStreamReaper is the body of the streaming goroutine. It reaps
// URBs as they complete, copies the data into a fresh slice (so
// the URB buffer can be reused immediately), pushes to streamCh
// with drop-oldest backpressure, and resubmits the URB.
//
// On shutdown (b.closed flips true after Close discards every URB)
// the reaper stops resubmitting and counts the cancelled URBs back
// from the kernel; once every URB is accounted for it closes
// streamDone and exits.
func (b *linuxBackend) runStreamReaper() {
	defer close(b.streamDone)

	pending := uint32(streamRingURBs)

	for pending > 0 {
		urb, err := b.reapURB()
		if err != nil {
			b.streamErr = err

			return
		}

		if urb == nil {
			b.streamErr = errStreamShortReap

			return
		}

		if b.closed.Load() {
			pending--

			continue
		}

		if urb.status != 0 {
			b.streamErr = fmt.Errorf("%w: status=%d ep=%#x", errStreamURBStatus, urb.status, urb.endpoint)

			return
		}

		b.dispatchChunk(urb)

		if err := b.submitURB(urb); err != nil {
			b.streamErr = err

			return
		}
	}
}

// dispatchChunk copies the just-reaped bytes into a fresh slice
// (the URB buffer is reused immediately on resubmit) and hands the
// chunk to the consumer channel. Drop-oldest backpressure: if the
// channel is full we evict the head and bump droppedURBs so a
// stalled consumer cannot stall the chip.
func (b *linuxBackend) dispatchChunk(urb *usbdevfsURB) {
	idx := int(urb.userContext)
	actual := int(urb.actualLength)

	chunk := make([]byte, actual)
	copy(chunk, b.urbBufs[idx][:actual])

	select {
	case b.streamCh <- chunk:
		return
	default:
	}

	// Channel full — drain the oldest chunk to make room.
	select {
	case <-b.streamCh:
		b.droppedURBs.Add(1)
	default:
	}

	select {
	case b.streamCh <- chunk:
	default:
		// Still full (consumer racing the producer); drop the new chunk.
		b.droppedURBs.Add(1)
	}
}
