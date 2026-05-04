package rtl2832u

import "fmt"

// I2C primer (Inter-Integrated Circuit)
// =====================================
//
// I2C is a two-wire (clock + data) serial bus that lets one master
// chip talk to many slave chips over short distances on the same
// PCB. Each transaction is:
//
//	START | <7-bit address> <R/W bit> | <data bytes...> | STOP
//
// The master drives the clock; the addressed slave acks each byte
// or NACKs to refuse. Reads stay at the device's current internal
// register pointer, so the conventional "read register N" idiom is
// two transactions: write 1 byte (the pointer N), then read M bytes.
// That is why i2cReadRegister below issues two control transfers.
//
// Addressing gotcha: I2C datasheets often quote two forms of the
// same address — the 7-bit slave address (e.g. R820T's 0x1a) and
// the 8-bit form that pads it with a R/W bit (0x34). The chip's
// I2C engine wants the 8-bit form in wValue: it ignores the LSB
// because direction comes from the USB control-transfer direction
// flag, but it requires the slave bits to start at bit 1, not bit
// 0. Putting 0x1a in wValue addresses 0x0d on the bus and the
// endpoint stalls (EPIPE).
//
// How demod1090 uses I2C
// ----------------------
// The RTL2832U is the I2C master; the tuner (R820T/R860, slave
// address 0x1a → wValue 0x34) is its only slave. The host doesn't
// drive the bus directly — it asks the chip to relay transactions
// via USB control transfers addressed to chipBlockI2C. wValue
// carries the 8-bit shifted slave address; wIndex carries the
// chip's block-and-direction encoding; the payload is the I2C byte
// sequence.
//
// Repeater bridge
// ---------------
// The chip gates this relay through a "repeater" register so I2C
// traffic doesn't leak onto the tuner bus while the demod is busy
// streaming samples. The bridge is opened (enableI2CRepeater),
// kept open for a batch of writes/reads, and closed
// (disableI2CRepeater) when done. While closed, control transfers
// to chipBlockI2C are silently dropped — which is the desired
// safety net during sample streaming.
//
// regDemodSoftReset (page 1, addr 0x01) is the same register the
// init phase pulses for soft reset; bit 3 toggles the repeater,
// bit 2 the soft reset. The two values below leave bit 4 (the
// baseline) on so they coexist with the rest of the init state.
const (
	i2cRepeaterEnabled  uint8 = 0x18
	i2cRepeaterDisabled uint8 = 0x10
)

// Compile-time assertion that rtl2832u still satisfies i2cTransport.
// If a future refactor breaks the contract, the build fails here
// rather than at the tuner-construction site.
var _ i2cTransport = (*rtl2832u)(nil)

// enableI2CRepeater opens the chip's I2C bridge to the tuner.
// Control transfers addressed to chipBlockI2C are forwarded to the
// tuner's I2C address while the bridge is open. Pair with
// disableI2CRepeater after a batch of tuner ops.
func (r *rtl2832u) enableI2CRepeater() error {
	if err := r.demodWriteByte(demodPage1, regDemodSoftReset, i2cRepeaterEnabled); err != nil {
		return fmt.Errorf("rtl2832u: enable i2c repeater: %w", err)
	}

	return nil
}

// disableI2CRepeater closes the chip's I2C bridge. Subsequent
// chipBlockI2C transfers are silently dropped, so accidental tuner
// reads during demod activity can't corrupt anything.
func (r *rtl2832u) disableI2CRepeater() error {
	if err := r.demodWriteByte(demodPage1, regDemodSoftReset, i2cRepeaterDisabled); err != nil {
		return fmt.Errorf("rtl2832u: disable i2c repeater: %w", err)
	}

	return nil
}

// i2cWrite sends data to the I2C device at addr via the chip's
// repeater. Caller must hold the repeater open (enableI2CRepeater
// before, disableI2CRepeater after) — i2cWrite does not toggle on
// its own because most tuner operations bundle multiple writes
// between a single open/close pair to minimise repeater chatter.
//
// On the wire: bmRequestType vendor-OUT, wValue=addr, wIndex=block
// chipBlockI2C with the WRITE flag, payload=data.
func (r *rtl2832u) i2cWrite(addr uint8, data []byte) error {
	_, err := r.ctrl.controlOut(0, uint16(addr),
		encodeBlockIndex(chipBlockI2C, chipDirWrite), data)
	if err != nil {
		return fmt.Errorf("rtl2832u: i2c write addr=%#x len=%d: %w", addr, len(data), err)
	}

	return nil
}

// i2cRead reads len(dst) bytes from the I2C device at addr via the
// chip's repeater. The kernel's USB stack returns the actual byte
// count; we treat anything short of len(dst) as an error so the
// caller doesn't silently get partial data.
func (r *rtl2832u) i2cRead(addr uint8, dst []byte) error {
	count, err := r.ctrl.controlIn(0, uint16(addr),
		encodeBlockIndex(chipBlockI2C, chipDirRead), dst)
	if err != nil {
		return fmt.Errorf("rtl2832u: i2c read addr=%#x len=%d: %w", addr, len(dst), err)
	}

	if count != len(dst) {
		return fmt.Errorf("rtl2832u: i2c read addr=%#x: %w (got %d, want %d)",
			addr, errShortRead, count, len(dst))
	}

	return nil
}

// i2cWriteRegister writes a single register on the I2C device:
// `addr` is the device's 7-bit I2C address, `reg` is the on-device
// register pointer, `val` is the byte to write. The chip's I2C
// engine sends both bytes as one transaction.
func (r *rtl2832u) i2cWriteRegister(addr, reg, val uint8) error {
	return r.i2cWrite(addr, []byte{reg, val})
}

// i2cReadRegister reads a single register on the I2C device: it
// first writes the register pointer (a one-byte transfer to set
// the device's read cursor), then reads one byte back. See the
// "I2C primer" at the top of this file for why this needs two
// transactions instead of one.
func (r *rtl2832u) i2cReadRegister(addr, reg uint8) (uint8, error) {
	if err := r.i2cWrite(addr, []byte{reg}); err != nil {
		return 0, err
	}

	var buf [1]byte
	if err := r.i2cRead(addr, buf[:]); err != nil {
		return 0, err
	}

	return buf[0], nil
}
