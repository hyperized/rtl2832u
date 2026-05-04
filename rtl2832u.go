package rtl2832u

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RTL2832U primer
// ===============
//
// The RTL2832U is a Realtek COFDM demodulator chip originally
// designed for European DVB-T digital television. The reason it
// matters for SDR is that an undocumented hidden mode lets the
// chip stream its 8-bit ADC samples (interleaved I and Q) over
// USB instead of decoding them into a transport stream — turning
// every cheap DVB-T USB stick into a 24 MHz to 1.7 GHz
// software-defined receiver. Antti Palosaari first published the
// trick in 2012; osmocom librtlsdr made it portable.
//
// The chip is not a tuner. It only handles baseband: it expects a
// front-end (the R820T/R860 in our hardware) to mix the requested
// RF down before the chip's ADC samples it. The chip drives the
// tuner over its built-in I2C bus — see i2c.go for that primer.
//
// Register layout
// ---------------
// Configuration is split across seven register groups, each
// addressed via the upper byte of the USB control transfer's
// wIndex field (chipBlock below):
//
//	0  Demod        — demodulator core (paged; see demod registers)
//	1  USB          — USB endpoint controller, FIFO config
//	2  SYS          — system control, GPIO, demod power rails
//	3  Tuner        — tuner I2C bus (we use chipBlockI2C in practice)
//	4  ROM          — boot ROM (untouched in SDR mode)
//	5  IR           — IR receiver (unused)
//	6  I2C          — generic I2C; how we reach the tuner
//
// The lower byte of wIndex carries a direction flag (0x10 = WRITE,
// 0x00 = READ). The chip enforces this at the silicon level — a
// USB control transfer with the wrong direction flag is silently
// discarded.
//
// Demod registers use a separate addressing scheme: the address
// goes in wValue's high byte with a 0x20 suffix (see encodeDemodAddr)
// and the page number lives in wIndex's low byte. The chip's demod
// register file is buffered, so every demod write must be followed
// by a flush read on any demod page — we do this automatically
// (see demodFlush below).
//
// We talk to all of this via USB control transfers (USBDEVFS_CONTROL
// ioctl on Linux, see control_linux.go).

// controller is the USB control-transfer abstraction used by the
// RTL2832U chip driver and (later) the tuner drivers. The Linux
// backend in control_linux.go implements it via the USBDEVFS_CONTROL
// ioctl; tests inject a mock that captures the wire-level call
// sequence and asserts the encoded wValue / wIndex / payload.
//
// Direction is split into two methods rather than a single Control
// method with a direction flag: callers always know statically
// whether they're reading or writing, so static-typing the direction
// makes the register helper bodies one line shorter and prevents the
// "I forgot the IN flag" class of bug.
type controller interface {
	controlIn(req uint8, value, index uint16, data []byte) (int, error)
	controlOut(req uint8, value, index uint16, data []byte) (int, error)
}

// chipBlock identifies one of the seven RTL2832U register groups. The
// numeric values are protocol facts (the chip uses them as the upper
// byte of the USB control transfer wIndex), so this is a typed alias
// over uint8 rather than a free-form constant set — it keeps callers
// from passing arbitrary numbers.
//
// Source: osmocom rtl-sdr include/rtl-sdr.h (`enum blocks`), BSD-2.
// Refresh by re-reading the upstream header at
// https://github.com/osmocom/rtl-sdr/blob/master/include/rtl-sdr.h.
type chipBlock uint8

const (
	chipBlockDemod chipBlock = 0
	chipBlockUSB   chipBlock = 1
	chipBlockSYS   chipBlock = 2
	chipBlockTuner chipBlock = 3
	chipBlockROM   chipBlock = 4
	chipBlockIR    chipBlock = 5
	chipBlockI2C   chipBlock = 6
)

// chipDirection is the read/write flag the chip expects in the low
// byte of the USB control transfer wIndex. The flag is a chip-level
// detail, not a USB-level one — control transfers already carry
// direction in bmRequestType, but the RTL2832U requires this extra
// bit for the registers to land in the right place.
type chipDirection uint8

const (
	chipDirRead  chipDirection = 0x00
	chipDirWrite chipDirection = 0x10
)

// USB block (chipBlockUSB) register addresses. Names match the
// `enum usb_reg` in osmocom librtlsdr's include/rtl-sdr.h. Listed
// here instead of dropped inline so call sites read like English
// ("write USBSysCtl") rather than chains of magic numbers.
const (
	regUSBSysCtl     uint16 = 0x2000
	regUSBCtrl       uint16 = 0x2010
	regUSBStat       uint16 = 0x2014
	regUSBEPACfg     uint16 = 0x2144
	regUSBEPACtl     uint16 = 0x2148
	regUSBEPAMaxPkt  uint16 = 0x2158
	regUSBEPAMaxPkt2 uint16 = 0x215a
	regUSBEPAFifoCfg uint16 = 0x2160
)

// SYS block (chipBlockSYS) register addresses live in init_chip.go
// alongside the phases that use them; declaring registers next to
// their use site keeps unused-symbol lints quiet and helps readers
// find the specific phase that touches a given register.

// errShortRead is the static sentinel for "the kernel reported fewer
// bytes than the operation expected". err113 forbids ad-hoc errors
// from fmt.Errorf, so the short-read paths wrap this with %w and
// attach the actual byte counts in the message.
var errShortRead = errors.New("rtl2832u: short read")

// rtl2832u is the chip-level driver. It owns no state of its own;
// methods are pure protocol operations against the underlying
// controller. The type is unexported because the chip is an internal
// implementation detail of Open() — public callers interact with
// Receiver, never the chip directly.
type rtl2832u struct {
	ctrl controller
}

func newRTL2832U(ctrl controller) *rtl2832u { return &rtl2832u{ctrl: ctrl} }

// encodeBlockIndex packs a chip block id and direction flag into the
// uint16 the chip expects as wIndex. The encoding is a chip-level
// fact, so we keep it in one place rather than repeating the
// (block << 8) | dir expression at every call site.
func encodeBlockIndex(block chipBlock, dir chipDirection) uint16 {
	return uint16(block)<<8 | uint16(dir)
}

// writeByte writes one byte to register addr in the given block.
//
// Separating writeByte and writeWord (instead of taking a length
// parameter as librtlsdr does) trades two extra method names for
// strong typing on the value: the compiler refuses to silently
// truncate a uint16 down to a uint8, which has been the source of
// real bugs in the C source that we want to avoid carrying over.
func (r *rtl2832u) writeByte(block chipBlock, addr uint16, val uint8) error {
	if _, err := r.ctrl.controlOut(0, addr, encodeBlockIndex(block, chipDirWrite), []byte{val}); err != nil {
		return fmt.Errorf("rtl2832u: writeByte block=%d addr=%#x: %w", block, addr, err)
	}

	return nil
}

// writeWord writes a 16-bit value (big-endian) to register addr.
// The chip expects MSB-first regardless of host byte order.
func (r *rtl2832u) writeWord(block chipBlock, addr uint16, val uint16) error {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], val)

	if _, err := r.ctrl.controlOut(0, addr, encodeBlockIndex(block, chipDirWrite), buf[:]); err != nil {
		return fmt.Errorf("rtl2832u: writeWord block=%d addr=%#x: %w", block, addr, err)
	}

	return nil
}

// readByte reads one byte from register addr in the given block.
func (r *rtl2832u) readByte(block chipBlock, addr uint16) (uint8, error) {
	var buf [1]byte

	count, err := r.ctrl.controlIn(0, addr, encodeBlockIndex(block, chipDirRead), buf[:])
	if err != nil {
		return 0, fmt.Errorf("rtl2832u: readByte block=%d addr=%#x: %w", block, addr, err)
	}

	if count != len(buf) {
		return 0, fmt.Errorf("rtl2832u: readByte block=%d addr=%#x: %w (got %d, want %d)",
			block, addr, errShortRead, count, len(buf))
	}

	return buf[0], nil
}

// readWord reads a 16-bit big-endian value from register addr.
func (r *rtl2832u) readWord(block chipBlock, addr uint16) (uint16, error) {
	var buf [2]byte

	count, err := r.ctrl.controlIn(0, addr, encodeBlockIndex(block, chipDirRead), buf[:])
	if err != nil {
		return 0, fmt.Errorf("rtl2832u: readWord block=%d addr=%#x: %w", block, addr, err)
	}

	if count != len(buf) {
		return 0, fmt.Errorf("rtl2832u: readWord block=%d addr=%#x: %w (got %d, want %d)",
			block, addr, errShortRead, count, len(buf))
	}

	return binary.BigEndian.Uint16(buf[:]), nil
}

// Demodulator-side registers use a different control-transfer
// encoding than the generic block registers: the address goes into
// the high byte of wValue with a 0x20 suffix, and the page number
// goes into wIndex (with the WRITE flag for writes, no flag for
// reads).
const demodAddrFlag = 0x20

func encodeDemodAddr(addr uint16) uint16 { return addr<<8 | demodAddrFlag }

// demodWriteByte writes one byte to a demodulator register on the
// given page. Every demod write is followed by an internal flush
// read — see demodFlush — so callers do not have to think about it.
func (r *rtl2832u) demodWriteByte(page uint8, addr uint16, val uint8) error {
	wIndex := uint16(page) | uint16(chipDirWrite)
	if _, err := r.ctrl.controlOut(0, encodeDemodAddr(addr), wIndex, []byte{val}); err != nil {
		return fmt.Errorf("rtl2832u: demodWriteByte page=%d addr=%#x: %w", page, addr, err)
	}

	return r.demodFlush()
}

// demodWriteWord writes a 16-bit big-endian value to a demodulator
// register on the given page, followed by a flush read.
func (r *rtl2832u) demodWriteWord(page uint8, addr uint16, val uint16) error {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], val)

	wIndex := uint16(page) | uint16(chipDirWrite)
	if _, err := r.ctrl.controlOut(0, encodeDemodAddr(addr), wIndex, buf[:]); err != nil {
		return fmt.Errorf("rtl2832u: demodWriteWord page=%d addr=%#x: %w", page, addr, err)
	}

	return r.demodFlush()
}

// demodReadByte reads one byte from a demodulator register on the
// given page. Demod reads use the same address encoding as writes
// but no direction flag in wIndex.
func (r *rtl2832u) demodReadByte(page uint8, addr uint16) (uint8, error) {
	var buf [1]byte

	count, err := r.ctrl.controlIn(0, encodeDemodAddr(addr), uint16(page), buf[:])
	if err != nil {
		return 0, fmt.Errorf("rtl2832u: demodReadByte page=%d addr=%#x: %w", page, addr, err)
	}

	if count != len(buf) {
		return 0, fmt.Errorf("rtl2832u: demodReadByte page=%d addr=%#x: %w (got %d, want %d)",
			page, addr, errShortRead, count, len(buf))
	}

	return buf[0], nil
}

// demodFlushPage and demodFlushAddr are the page/addr librtlsdr
// reads after every demod write. The chip's demod register file is
// buffered and only commits to silicon on the next read; without
// this dummy read, a back-to-back write can clobber the pending
// state of the previous one. The exact (page, addr) pair is not
// load-bearing — any read on the demod side flushes — but matching
// librtlsdr makes upstream tickets easier to compare.
const (
	demodFlushPage uint8  = 0x0a
	demodFlushAddr uint16 = 0x01
)

func (r *rtl2832u) demodFlush() error {
	if _, err := r.demodReadByte(demodFlushPage, demodFlushAddr); err != nil {
		return fmt.Errorf("rtl2832u: demod flush: %w", err)
	}

	return nil
}
