package rtl2832u

import (
	"errors"
	"fmt"
)

// Bias-tee control
// ================
//
// Many V3-class RTL-SDR dongles (rtl-sdr.com V3/V4, NESDR SMArt
// XTR, the ClockworkPi HackerGadgets AIO board, etc.) wire a
// 4.5 V bias-tee circuit between the antenna SMA / SMB connector
// and one of the RTL2832U's GPIO pins. Driving that GPIO high
// powers the bias-tee, which lets users feed a remote LNA or
// filter from the same coax — material Mode S sensitivity boost
// when the antenna is far from the receiver.
//
// The chip's GPIO subsystem lives in the SYS block (datasheet pin
// table assigns GPIO[0..7] to specific pins). librtlsdr drives
// the bias-tee on GPIO0 by convention; almost every dongle that
// implements bias-tee follows that convention. Clones with
// alternate wiring need to override the GPIO via SetBiasTeeGPIO.
//
// Four SYS registers participate. Per the RTL2832U datasheet §10
// ("address is defined by offset value with base address 0x3000"),
// the absolute addresses are the table's offsets plus 0x3000:
//
//   GPO  (0x3001)  RW  per-bit GPIO output latch
//   GPI  (0x3002)  R   per-bit GPIO input level (live pin voltage)
//   GPOE (0x3003)  RW  per-bit output enable; 1 = drive, 0 = high-Z
//   GPD  (0x3004)  RW  per-bit direction; 0 = output, 1 = input
//
// Toggle sequence per librtlsdr's rtlsdr_set_bias_tee_gpio:
//
//   1. clear the bit in GPD (configure as output)
//   2. set   the bit in GPOE (enable the output driver)
//   3. set/clear the bit in GPO (drive high / low)
//
// We implement the three writes as read-modify-writes against the
// SYS block so we don't disturb other GPIO bits the user might
// have configured. Read-back uses GPO (not GPI): the bias-tee pin
// is configured as an OUTPUT, and per datasheet §10.2.2 "Input
// Value of GPIO N. Valid only when GPIO N is defined as input
// pin." — so GPI is undefined for output pins. GPO holds the
// latched output value, mirroring whatever was last written, and
// librtlsdr's set_gpio_bit uses GPO for its own RMW. We keep
// regSYSGPI defined for completeness / future input-mode use but
// the bias-tee path itself never reads it.
//
// Call frequency is low (twice per session at most for sets, ~1 Hz
// for reads) so the USB round-trips are immaterial.
//
// Cross-references: Linux kernel drivers/media/usb/dvb-usb-v2/
// rtl28xxu.h SYS_GPIO_* defines, osmocom librtlsdr enum sys_reg.
// All three agree on the 0x300x addressing.

const (
	// regSYSGPO holds the per-bit GPIO output latch (RW). Used
	// by setBiasTee for the drive write and by getBiasTee for
	// read-back of the latched output value.
	regSYSGPO uint16 = 0x3001
	// regSYSGPI holds the per-bit GPIO input pin level (R-only).
	// Not consumed by the bias-tee path — the bias-tee pin is
	// configured as an output and per RTL2832U datasheet §10.2.2
	// "Input Value of GPIO N. Valid only when GPIO N is defined
	// as input pin." Declared here so the SYS-block GPIO triplet
	// reads complete in code and future input-mode callers can
	// pick it up without re-discovering the address.
	//
	//nolint:unused // documentation; consumed by future input-mode callers.
	regSYSGPI uint16 = 0x3002
	// regSYSGPOE holds the per-bit GPIO output-enable.
	regSYSGPOE uint16 = 0x3003
	// regSYSGPD holds the per-bit GPIO direction (0 = output).
	regSYSGPD uint16 = 0x3004

	// defaultBiasTeeGPIO is the GPIO pin librtlsdr drives for
	// bias-tee on every common RTL-SDR dongle. Clones with
	// non-standard wiring need to override via SetBiasTeeGPIO.
	defaultBiasTeeGPIO uint8 = 0

	// biasTeeMaxGPIO is the largest GPIO index the chip exposes
	// (GPIO[0..7] per datasheet pin table); higher values would
	// shift outside the byte width of the GPIO registers.
	biasTeeMaxGPIO uint8 = 7
)

// ErrInvalidGPIO is the static sentinel for an out-of-range GPIO
// index passed to SetBiasTeeGPIO.
var ErrInvalidGPIO = errors.New("rtl2832u: GPIO index out of range [0, 7]")

// configureGPIOOutput points the chip's GPIO subsystem at the
// requested pin: clear GPD bit (output direction), set GPOE bit
// (drive enable). Caller still has to write the value via
// writeGPIOBit afterwards.
func (r *rtl2832u) configureGPIOOutput(gpio uint8) error {
	mask := uint8(1 << gpio)

	if err := r.writeSysRegMask(regSYSGPD, 0, mask); err != nil {
		return fmt.Errorf("rtl2832u: configure GPIO%d direction: %w", gpio, err)
	}

	if err := r.writeSysRegMask(regSYSGPOE, mask, mask); err != nil {
		return fmt.Errorf("rtl2832u: configure GPIO%d output enable: %w", gpio, err)
	}

	return nil
}

// writeGPIOBit drives a previously-configured GPIO output high or
// low. The bit must already be configured via configureGPIOOutput.
//
//nolint:revive // `high` is the new pin level, not a control-flow flag.
func (r *rtl2832u) writeGPIOBit(gpio uint8, high bool) error {
	mask := uint8(1 << gpio)

	value := uint8(0)
	if high {
		value = mask
	}

	if err := r.writeSysRegMask(regSYSGPO, value, mask); err != nil {
		return fmt.Errorf("rtl2832u: write GPIO%d=%t: %w", gpio, high, err)
	}

	return nil
}

// writeSysRegMask is the read-modify-write primitive the GPIO
// helpers need. The SYS block's GPIO registers each pack 8
// independent bits, so masking is the only way to touch one
// without clobbering the other seven.
func (r *rtl2832u) writeSysRegMask(addr uint16, value, mask uint8) error {
	current, err := r.readByte(chipBlockSYS, addr)
	if err != nil {
		return fmt.Errorf("rtl2832u: read SYS reg %#x: %w", addr, err)
	}

	updated := (current &^ mask) | (value & mask)

	if err := r.writeByte(chipBlockSYS, addr, updated); err != nil {
		return fmt.Errorf("rtl2832u: write SYS reg %#x: %w", addr, err)
	}

	return nil
}

// setBiasTee toggles the chip's bias-tee output on the GPIO pin
// the dongle wires to its 4.5 V bias-tee circuit. Performs the
// configure-output + drive-bit sequence librtlsdr's
// rtlsdr_set_bias_tee_gpio implements.
//
//nolint:revive // `enable` is a feature toggle, not a control-flow flag.
func (r *rtl2832u) setBiasTee(gpio uint8, enable bool) error {
	if gpio > biasTeeMaxGPIO {
		return fmt.Errorf("%w: gpio=%d", ErrInvalidGPIO, gpio)
	}

	if err := r.configureGPIOOutput(gpio); err != nil {
		return err
	}

	return r.writeGPIOBit(gpio, enable)
}

// getBiasTee reads the chip's GPO (output latch) register and
// reports whether the bias-tee bit for the configured GPIO is
// driven high. GPO returns the latched output value — exactly
// what we last wrote with SetBiasTee — so a re-read after a write
// is the canonical state for an output-mode pin.
//
// Why GPO and not GPI: per datasheet §10.2.2, "Input Value of
// GPIO N. Valid only when GPIO N is defined as input pin." The
// bias-tee GPIO is configured as an OUTPUT (configureGPIOOutput
// clears GPD bit, sets GPOE bit), so GPI is undefined for it.
// GPO is read-modify-written by librtlsdr's set_gpio_bit and is
// the documented surface for read-back of an output pin.
//
// Caveat: external tools that flip the bit through their own
// rtlsdr handle (rtl_biast, another process) will also update
// GPO via the same code path, so we still see those changes —
// the chip-level latch is the source of truth.
func (r *rtl2832u) getBiasTee(gpio uint8) (bool, error) {
	if gpio > biasTeeMaxGPIO {
		return false, fmt.Errorf("%w: gpio=%d", ErrInvalidGPIO, gpio)
	}

	value, err := r.readByte(chipBlockSYS, regSYSGPO)
	if err != nil {
		return false, fmt.Errorf("rtl2832u: read GPIO%d state: %w", gpio, err)
	}

	return value&(1<<gpio) != 0, nil
}
