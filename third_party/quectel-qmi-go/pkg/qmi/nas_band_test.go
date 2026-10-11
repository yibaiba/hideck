package qmi

import (
	"encoding/binary"
	"fmt"
	"testing"
)

func TestLTEBandNumberUsesQMIEnum(t *testing.T) {
	for _, tc := range []struct {
		raw, number uint16
		duplex      string
	}{
		{127, 8, "FDD"}, {141, 39, "TDD"}, {149, 41, "TDD"},
		{161, 66, "FDD"}, {145, 20, "FDD"}, {168, 71, "FDD"},
		{125, 6, "FDD"}, {128, 9, "FDD"}, {129, 10, "FDD"},
		{130, 11, "FDD"}, {146, 21, "FDD"}, {152, 23, "FDD"}, {147, 24, "FDD"},
		{159, 29, ""}, {154, 32, ""},
		{8, 0, ""}, {41, 0, ""}, {999, 0, ""},
	} {
		t.Run(fmt.Sprint(tc.raw), func(t *testing.T) {
			entry := RFBandInfoEntry{RadioInterface: 0x08, ActiveBandClass: tc.raw}
			number, known := entry.LTEBandNumber()
			if number != tc.number || known != (tc.number != 0) {
				t.Fatalf("number=%d known=%v want=%d", number, known, tc.number)
			}
			if got := GetLTEDuplexModeFromBandInfo(&RFBandInfo{Bands: []RFBandInfoEntry{entry}}); got != tc.duplex {
				t.Fatalf("duplex=%q want=%q", got, tc.duplex)
			}
			entry.RadioInterface = 0x04
			if _, known := entry.LTEBandNumber(); known {
				t.Fatal("non-LTE RAT must not be interpreted as LTE")
			}
		})
	}
	if got := GetLTEDuplexModeFromBandInfo(nil); got != "" {
		t.Fatalf("nil info duplex=%q", got)
	}
}

func TestParseRFBandInfoPreservesEnumAndChannel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tlvType uint8
		channel uint32
	}{
		{"standard B39", 0x01, 38400},
		{"extended B39", 0x11, 38400},
		{"extended channel width", 0x11, 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := []byte{1, 0x08, 141, 0, 0, 0, 0, 0}
			binary.LittleEndian.PutUint32(value[4:], tc.channel)
			if tc.tlvType == 0x01 {
				value = value[:6]
			}
			info, err := parseRFBandInfoResponse(&Packet{TLVs: []TLV{
				successResultTLV(), {Type: tc.tlvType, Value: value},
			}})
			if err != nil || len(info.Bands) != 1 {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			entry := info.Bands[0]
			number, known := entry.LTEBandNumber()
			if entry.ActiveBandClass != 141 || entry.ActiveChannel != tc.channel || number != 39 || !known {
				t.Fatalf("entry=%+v number=%d known=%v", entry, number, known)
			}
			if got := GetLTEDuplexModeFromBandInfo(info); got != "TDD" {
				t.Fatalf("duplex=%q", got)
			}
		})
	}
}
