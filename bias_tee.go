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
// Three SYS registers participate:
//
//   GPO  (0x0001)  per-bit GPIO output value
//   GPOE (0x0002)  per-bit output-enable; 1 = drive, 0 = high-Z
//   GPD  (0x0003)  per-bit direction; 0 = output, 1 = input
//
// Toggle sequence per librtlsdr's rtlsdr_set_bias_tee_gpio:
//
//   1. clear the bit in GPD (configure as output)
//   2. set   the bit in GPOE (enable the output driver)
//   3. set/clear the bit in GPO (drive high / low)
//
// We implement the three steps as read-modify-writes against the
// chip's SYS block so we don't disturb other GPIO bits the user
// might have configured. Call frequency is low (twice per session
// at most) so the extra USB round-trips are immaterial.

const (
	// regSYSGPO holds the GPIO output values (per-bit).
	regSYSGPO uint16 = 0x0001
	// regSYSGPOE holds the GPIO output-enable bits.
	regSYSGPOE uint16 = 0x0002
	// regSYSGPD holds the GPIO direction (0 = output, 1 = input).
	regSYSGPD uint16 = 0x0003

	// defaultBiasTeeGPIO is the GPIO pin librtlsdr drives for
	// bias-tee on every common RTL-SDR dongle. Clones with
	// non-standard wiring need to override via SetBiasTeeGPIO.
	//
	//nolint:unused // surfaced via WithBiasTee in a follow-up commit.
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
