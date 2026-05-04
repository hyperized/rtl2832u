package rtl2832u

// --- setMux register addresses, bit fields, and band table ---
//
// setMux configures the R860's analogue front end (RF input filter,
// open-drain control, tracking-filter coefficients, plus a few
// IMR-memory clears) for a given RF band. The chip has discrete
// front-end paths optimised for different segments of the
// 24 MHz..1.8 GHz tuning range; choosing the wrong path produces
// usable but degraded reception.
//
// The path is selected from a step table indexed by the RF's
// integer-MHz value: every row gives the open-drain, RF-mux/polyphase,
// and tracking-filter byte that applies from row.minMHz upward (until
// the next row supersedes it). Values transcribed from osmocom
// librtlsdr's tuner_r82xx.c freq_ranges table (BSD-2).

const (
	// regR860OpenDrain (0x17) bits [3] — front-end open-drain mode.
	regR860OpenDrain uint8 = 0x17

	// regR860RFMux (0x1a) bits [7:6] (LPF select) + bits [1:0]
	// (polymux). Shares the physical register with regR860Autotune,
	// which lives in bits [3:2]. The masks do not overlap, so both
	// callers can safely read-modify-write.
	regR860RFMux uint8 = 0x1a

	// regR860TrackingFilt (0x1b) is a full-byte write that selects
	// among the on-chip tracking-filter coefficient tables.
	regR860TrackingFilt uint8 = 0x1b

	// regR860XtalCap (0x10) — bits [3:0,1] hold xtal-cap selection.
	// The basic init clears them; we follow.
	regR860XtalCap uint8 = 0x10

	// regR860IMRMem1 (0x08) and regR860IMRMem2 (0x09) hold the
	// chip's image-rejection memory. setMux clears the lower six
	// bits to defeat any band-tracked correction left over from a
	// prior tune.
	regR860IMRMem1 uint8 = 0x08
	regR860IMRMem2 uint8 = 0x09
)

const (
	maskR860OpenDrain uint8 = 0x08
	maskR860RFMux     uint8 = 0xc3
	maskR860XtalCap   uint8 = 0x0b
	maskR860IMRMem    uint8 = 0x3f

	// muxClearedBits is the value-bits common to several setMux
	// writes: the chip wants the masked bits zeroed for a clean
	// retune from any prior band.
	muxClearedBits uint8 = 0x00
)

// r860FreqRange is one row of the front-end band table. minMHz is
// the inclusive lower bound where this row applies; the row
// extends upward until the next row's minMHz.
type r860FreqRange struct {
	minMHz       uint32
	openDrain    uint8 // value for regR860OpenDrain (mask maskR860OpenDrain)
	rfMuxPloy    uint8 // value for regR860RFMux (mask maskR860RFMux)
	trackingFilt uint8 // value for regR860TrackingFilt (full byte)
}

// r860FreqRanges is the band-selection table. Source: osmocom
// librtlsdr's tuner_r82xx.c (BSD-2). The rows are ordered by
// ascending minMHz; bandRangeForFreq picks the highest row whose
// minMHz does not exceed the requested RF.
//
//nolint:gochecknoglobals // immutable lookup table.
var r860FreqRanges = []r860FreqRange{
	{0, 0x08, 0x02, 0xdf},
	{50, 0x08, 0x02, 0xbe},
	{55, 0x08, 0x02, 0x8b},
	{60, 0x08, 0x02, 0x7b},
	{65, 0x08, 0x02, 0x69},
	{70, 0x08, 0x02, 0x58},
	{75, 0x00, 0x02, 0x44},
	{80, 0x00, 0x02, 0x44},
	{90, 0x00, 0x02, 0x34},
	{100, 0x00, 0x02, 0x34},
	{110, 0x00, 0x02, 0x24},
	{120, 0x00, 0x02, 0x24},
	{140, 0x00, 0x02, 0x14},
	{180, 0x00, 0x02, 0x13},
	{220, 0x00, 0x02, 0x13},
	{250, 0x00, 0x08, 0x11},
	{280, 0x00, 0x02, 0x00},
	{310, 0x00, 0x41, 0x00},
	{588, 0x00, 0x40, 0x00},
}

// bandRangeForFreq returns the freq_ranges row that applies to
// rfHz. Walks the table forward and remembers the highest row
// whose minMHz still fits; row 0's minMHz is zero so idx is always
// assigned at least once and there is no unreachable fallback.
func bandRangeForFreq(rfHz uint32) r860FreqRange {
	const hzPerMHz = 1_000_000

	rfMHz := rfHz / hzPerMHz

	idx := 0

	for i, band := range r860FreqRanges {
		if band.minMHz > rfMHz {
			break
		}

		idx = i
	}

	return r860FreqRanges[idx]
}

// setMux programs the R860's analogue front end for the band that
// contains rfHz. Always issues all six writes — even when the new
// band's values match the shadow — because read-modify-write is
// safe to repeat and the explicit writes guard against a drifted
// shadow.
//
// Caller must hold the chip's I2C repeater open. SetFreq does this
// via withRepeater before calling setMux + the PLL synthesis path.
func (t *R860) setMux(rfHz uint32) error {
	band := bandRangeForFreq(rfHz)

	if err := t.writeRegisterMasked(regR860OpenDrain, band.openDrain, maskR860OpenDrain); err != nil {
		return err
	}

	if err := t.writeRegisterMasked(regR860RFMux, band.rfMuxPloy, maskR860RFMux); err != nil {
		return err
	}

	if err := t.writeRegister(regR860TrackingFilt, band.trackingFilt); err != nil {
		return err
	}

	if err := t.writeRegisterMasked(regR860XtalCap, muxClearedBits, maskR860XtalCap); err != nil {
		return err
	}

	if err := t.writeRegisterMasked(regR860IMRMem1, muxClearedBits, maskR860IMRMem); err != nil {
		return err
	}

	return t.writeRegisterMasked(regR860IMRMem2, muxClearedBits, maskR860IMRMem)
}
