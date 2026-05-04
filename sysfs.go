package rtl2832u

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// USB identifiers for RTL2832U-based dongles known to ship the R820T/R860
// family of tuners. Naming the values rather than scattering raw hex
// through the code keeps mnd quiet and makes adding a new clone a
// one-line change.
//
// To verify or extend this list:
//
//   - lsusb shows the VID:PID of any connected dongle. Filtering by the
//     Realtek vendor narrows the output to RTL-SDR-class hardware:
//     lsusb -d 0bda:
//
//   - The same numbers live in sysfs (which is what enumerate() reads):
//     cat /sys/bus/usb/devices/*/idVendor
//     cat /sys/bus/usb/devices/*/idProduct
//
//   - udevadm walks every attribute exposed for a given /dev/bus/usb node:
//     udevadm info --attribute-walk -n /dev/bus/usb/001/005
//
//   - Authoritative upstream list of supported dongles is osmocom
//     rtl-sdr's known_devices table in src/librtlsdr.c (BSD-2):
//     https://github.com/osmocom/rtl-sdr/blob/master/src/librtlsdr.c
//
//   - Vendor-ID assignments are managed by USB-IF; the Linux usb.ids
//     database mirrors them on most distros at
//     /var/lib/usbutils/usb.ids (also at http://www.linux-usb.org/usb.ids).
//
// Realtek (vendor 0x0bda) reuses 0x2832 across several DVB-T sticks;
// rebadged dongles often ship their own VID:PID and need an explicit
// entry here before they will enumerate.
const (
	realtekVendorID uint16 = 0x0bda

	rtl2832ProductID uint16 = 0x2832 // generic RTL2832U
	rtl2838ProductID uint16 = 0x2838 // generic RTL2832U + R820T
)

// isKnownRTLSDR reports whether the given (vid, pid) pair belongs to a
// dongle this driver supports. We use a function rather than a global
// lookup table so that gochecknoglobals stays satisfied without the
// extra ceremony of //nolint directives.
func isKnownRTLSDR(vid, pid uint16) bool {
	if vid != realtekVendorID {
		return false
	}

	switch pid {
	case rtl2832ProductID, rtl2838ProductID:
		return true
	}

	return false
}

// packUSBID composes a (vendor, product) pair into a single uint32 key.
// Retained for tests that assert the encoding scheme used in the
// pre-1.0 Receiver protocol; not used at runtime.
func packUSBID(vid, pid uint16) uint32 { return uint32(vid)<<16 | uint32(pid) }

// usbDevice describes one entry under /sys/bus/usb/devices that matched
// our known RTL-SDR identifier set. devNode is precomputed because the
// usbfs path format (%03d/%03d) is awkward to recompute at every callsite.
type usbDevice struct {
	vendorID  uint16
	productID uint16
	busNum    uint16
	devNum    uint16
	sysPath   string
	devNode   string
}

// enumerate scans the given sysfs USB root and returns matching RTL-SDR
// devices in directory-name order. The root path is normally
// "/sys/bus/usb/devices"; tests inject a temp-dir fixture so the parser
// is exercised without real hardware.
//
// Entries whose names contain ':' are USB *interfaces*, not devices, and
// are skipped — they expose attributes per-interface rather than the
// idVendor/idProduct/busnum/devnum quartet that we read.
func enumerate(root string) ([]usbDevice, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("sdr: enumerate sysfs %q: %w", root, err)
	}

	out := make([]usbDevice, 0, len(entries))

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ":") {
			continue
		}

		// Real /sys/bus/usb/devices entries are symlinks pointing into
		// /sys/devices/...; DirEntry.IsDir() reports false for symlinks
		// even when the target is a directory, so the early-exit check
		// has to follow links via os.Stat. Tests using real directories
		// still pass — Stat treats them as directories too.
		entryPath := filepath.Join(root, entry.Name())

		info, err := os.Stat(entryPath)
		if err != nil || !info.IsDir() {
			continue
		}

		dev, err := readUSBDevice(entryPath)
		if err != nil {
			// Hotplug races and partial nodes are common; ignore silently
			// rather than failing the whole walk.
			continue
		}

		if !isKnownRTLSDR(dev.vendorID, dev.productID) {
			continue
		}

		out = append(out, dev)
	}

	return out, nil
}

func readUSBDevice(path string) (usbDevice, error) {
	vid, err := readHexU16(filepath.Join(path, "idVendor"))
	if err != nil {
		return usbDevice{}, err
	}

	pid, err := readHexU16(filepath.Join(path, "idProduct"))
	if err != nil {
		return usbDevice{}, err
	}

	bus, err := readDecU16(filepath.Join(path, "busnum"))
	if err != nil {
		return usbDevice{}, err
	}

	num, err := readDecU16(filepath.Join(path, "devnum"))
	if err != nil {
		return usbDevice{}, err
	}

	return usbDevice{
		vendorID:  vid,
		productID: pid,
		busNum:    bus,
		devNum:    num,
		sysPath:   path,
		devNode:   fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, num),
	}, nil
}

// Numeric bases for the sysfs files we parse: idVendor / idProduct are
// hex, busnum / devnum are decimal. Naming the bases avoids mnd hits and
// makes the call sites read clearly.
const (
	hexBase   = 16
	decBase   = 10
	uint16Bit = 16
)

func readHexU16(path string) (uint16, error) { return readSizedUint(path, hexBase) }
func readDecU16(path string) (uint16, error) { return readSizedUint(path, decBase) }

// readSizedUint reads a whitespace-delimited number from path and parses
// it as an unsigned integer of width 16 bits. The bitsize argument to
// strconv.ParseUint enforces the upper bound, so the uint16 conversion
// below can never truncate.
func readSizedUint(path string, base int) (uint16, error) {
	// G304: the path is composed of a controlled root (sysfsRoot or a
	// t.TempDir()) plus a hardcoded sysfs filename. Within the root the
	// directory comes from os.ReadDir, which only lists existing entries.
	// Writing to /sys/bus/usb/devices requires root, so an attacker who
	// could change these inputs already owns the system.
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return 0, fmt.Errorf("sdr: read %q: %w", path, err)
	}

	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), base, uint16Bit)
	if err != nil {
		return 0, fmt.Errorf("sdr: parse %q: %w", path, err)
	}

	return uint16(n), nil
}
