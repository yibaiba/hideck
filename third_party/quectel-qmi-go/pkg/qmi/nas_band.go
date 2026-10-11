package qmi

const nasRadioInterfaceLTE uint8 = 0x08

// QMI NAS Active Band is an enum, not a 3GPP band number. In particular,
// EUTRAN 39 is 141, while EUTRAN 41 is 149; there is no common offset.
// Source: libqmi QmiNasActiveBand, qmi-enums-nas.h (commit 73c6d5ee53ad).
var nasLTEBands = map[uint16]uint16{
	120: 1, 121: 2, 122: 3, 123: 4, 124: 5, 125: 6, 126: 7,
	127: 8, 128: 9, 129: 10, 130: 11, 131: 12, 132: 13, 133: 14,
	134: 17, 143: 18, 144: 19, 145: 20, 146: 21, 152: 23,
	147: 24, 148: 25, 153: 26, 164: 27, 158: 28, 159: 29,
	160: 30, 165: 31, 154: 32,
	135: 33, 136: 34, 137: 35, 138: 36, 139: 37, 140: 38,
	141: 39, 142: 40, 149: 41, 150: 42, 151: 43,
	163: 46, 166: 47, 167: 48, 161: 66, 168: 71,
	155: 125, 156: 126, 157: 127, 162: 250,
}

// LTEBandNumber returns the EUTRAN band number without changing the raw enum.
// Unknown enums and entries for other radio technologies are not guessed.
func (entry RFBandInfoEntry) LTEBandNumber() (uint16, bool) {
	if entry.RadioInterface != nasRadioInterfaceLTE {
		return 0, false
	}
	band, known := nasLTEBands[entry.ActiveBandClass]
	return band, known
}
